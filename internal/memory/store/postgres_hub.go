// postgres_hub.go — общая база синхронизации (Этап 19, DESIGN.md §4.1): та же схема
// postgres-бэкенда, но открытая БЕЗ автомиграции, и инкрементальная выборка изменений
// по серверному номеру изменения (memories.change_xid, шаг migrateChangeXIDPG).
//
// store.Open с driver = "postgres" по-прежнему приводит схему к целевой при каждом
// открытии — это режим «своё хранилище». Hub DDL не выполняет вовсе: схему создаёт и
// обновляет только явный ApplySchema (кнопка с подтверждением в морде, `memo sync
// -apply-schema`), а синхронизация на схеме чужой версии не идёт — это правило
// интерактора, Hub лишь сообщает состояние (HubSchema).
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"agentmemory/internal/config"
	"agentmemory/internal/memory"
)

// Hub — общая база одного проекта, открытая для синхронизации.
type Hub struct {
	pg *pgStore
}

// HubSchema — состояние схемы общей базы относительно бинаря.
type HubSchema struct {
	Empty     bool // нет таблицы schema_version — схема в базе не создавалась
	Version   int  // schema_version.version; 0 при Empty
	Supported int  // версия схемы, которую понимает этот бинарь (pgSchemaVersion)
}

// Current — версия схемы равна поддерживаемой бинарём: синхронизировать можно.
func (h HubSchema) Current() bool { return !h.Empty && h.Version == h.Supported }

// Ahead — схема новее бинаря («обновите memo»): ни синхронизировать, ни применять
// схему этим бинарём нельзя.
func (h HubSchema) Ahead() bool { return h.Version > h.Supported }

// HubCursor — позиция pull в общей базе (DESIGN.md §4.1): пара (xid, id) в порядке
// (change_xid, id). ID == "" — «все строки с change_xid >= XID»; нулевой курсор — с
// начала.
type HubCursor struct {
	XID uint64
	ID  string
}

// less — строгий порядок курсоров: по XID, затем по ID.
func (c HubCursor) less(o HubCursor) bool {
	if c.XID != o.XID {
		return c.XID < o.XID
	}
	return c.ID < o.ID
}

// HubChange — запись общей базы (с Tags и Links) и её номер изменения.
type HubChange struct {
	Memory memory.Memory
	XID    uint64
}

// HubPage — страница изменений и безопасный курсор, который можно сохранить после
// применения страницы: следующий pull от него не пропустит ни одной строки.
type HubPage struct {
	Records []HubChange
	Next    HubCursor
}

// OpenHub подключается к общей базе по cfg (ping, без DDL и без чтения схемы).
// Состояние схемы — Schema, создание и обновление — ApplySchema.
func OpenHub(ctx context.Context, cfg config.StoreConfig) (*Hub, error) {
	db, err := dialPostgres(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return newHub(db), nil
}

func newHub(db *sql.DB) *Hub {
	return &Hub{pg: &pgStore{db: db, now: defaultNow}}
}

// Close закрывает пул соединений.
func (h *Hub) Close() error {
	return h.pg.db.Close()
}

// Schema сообщает состояние схемы, ничего не создавая: to_regclass не падает на
// отсутствующей таблице, в отличие от прямого SELECT.
func (h *Hub) Schema(ctx context.Context) (HubSchema, error) {
	st := HubSchema{Supported: pgSchemaVersion}
	var exists bool
	if err := h.pg.db.QueryRowContext(ctx, `SELECT to_regclass('schema_version') IS NOT NULL`).Scan(&exists); err != nil {
		return HubSchema{}, fmt.Errorf("проверка схемы общей базы: %w", err)
	}
	if !exists {
		st.Empty = true
		return st, nil
	}
	v, err := h.pg.SchemaVersion(ctx)
	if err != nil {
		return HubSchema{}, err
	}
	st.Version = v
	return st, nil
}

// ApplySchema создаёт или обновляет схему общей базы до версии бинаря — тот же init,
// что у store.Open, под тем же advisory lock. Схему новее бинаря не трогает: старый
// бинарь переприменил бы поверх неё свою базовую схему и триггеры.
func (h *Hub) ApplySchema(ctx context.Context) error {
	st, err := h.Schema(ctx)
	if err != nil {
		return err
	}
	if st.Ahead() {
		return fmt.Errorf("схема общей базы (версия %d) новее, чем понимает этот memo (%d) — обновите memo", st.Version, st.Supported)
	}
	if err := h.pg.init(ctx); err != nil {
		return fmt.Errorf("применение схемы общей базы: %w", err)
	}
	return nil
}

// Changes отдаёт изменения записей scope (kinds пусто — все виды) после from
// страницами не больше limit записей: в порядке (change_xid, id), с тегами и
// исходящими связями, вместе с тумстоунами. Все страницы читаются в одной транзакции
// REPEATABLE READ READ ONLY — один снимок; xid читателю не выдаётся. Ошибка page
// прерывает выборку и возвращается как есть.
//
// Курсор — горизонт снимка, а не последний прочитанный номер (DESIGN.md §4.1):
// xmin = pg_snapshot_xmin снимка, все транзакции с меньшим xid к снимку завершены и
// видны. Страница, последняя строка которой (L, I) при L < xmin, отдаёт Next = (L, I);
// иначе и после последней страницы — (xmin, ""), строки окна [xmin, xmax) следующий
// pull перечитает. Next не меньше from. Нет изменений — одна пустая страница с
// продвинутым курсором.
func (h *Hub) Changes(ctx context.Context, scope string, kinds []memory.Kind, from HubCursor, limit int, page func(HubPage) error) error {
	if limit <= 0 {
		return fmt.Errorf("выборка изменений: limit должен быть положительным, получено %d", limit)
	}

	tx, err := h.pg.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return fmt.Errorf("начало транзакции выборки изменений: %w", err)
	}
	defer tx.Rollback()

	// Первый запрос транзакции фиксирует её снимок, поэтому xmin берётся именно им.
	var xminText string
	if err := tx.QueryRowContext(ctx, `SELECT pg_snapshot_xmin(pg_current_snapshot())::text`).Scan(&xminText); err != nil {
		return fmt.Errorf("чтение горизонта снимка: %w", err)
	}
	xmin, err := strconv.ParseUint(xminText, 10, 64)
	if err != nil {
		return fmt.Errorf("разбор горизонта снимка %q: %w", xminText, err)
	}

	horizon := HubCursor{XID: xmin}
	if horizon.less(from) {
		horizon = from
	}

	pos := from
	for {
		// limit+1 — чтобы узнать, последняя ли это страница, без лишнего пустого запроса.
		recs, err := h.changesAfter(ctx, tx, scope, kinds, pos, limit+1)
		if err != nil {
			return err
		}
		last := len(recs) <= limit
		next := horizon
		if !last {
			recs = recs[:limit]
			l := recs[limit-1]
			pos = HubCursor{XID: l.XID, ID: l.Memory.Id}
			// Транзакция L завершилась до снимка: все её строки видны и идут по id, а
			// строки с меньшим xid уже отданы. pos > from — keyset строго растёт.
			if l.XID < xmin {
				next = pos
			}
		}
		if err := page(HubPage{Records: recs, Next: next}); err != nil {
			return err
		}
		if last {
			return nil
		}
	}
}

// changesAfter — одна keyset-страница Changes: строки с (change_xid, id) > pos.
// При pos.ID == "" условие равносильно change_xid >= pos.XID (id не пуст).
func (h *Hub) changesAfter(ctx context.Context, tx *sql.Tx, scope string, kinds []memory.Kind, pos HubCursor, limit int) ([]HubChange, error) {
	a := &argList{}
	conds := []string{"(m.change_xid, m.id) > (" + a.add(strconv.FormatUint(pos.XID, 10)) + "::xid8, " + a.add(pos.ID) + ")"}
	if c := scopePredicatePG(a, nil, scope); c != "" {
		conds = append(conds, c)
	}
	kindNames := make([]string, len(kinds))
	for i, k := range kinds {
		kindNames[i] = string(k)
	}
	if c := kindPredicatePG(a, kindNames); c != "" {
		conds = append(conds, c)
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT `+memoryColumns+`, m.change_xid::text
		FROM memories m
		WHERE `+strings.Join(conds, " AND ")+`
		ORDER BY m.change_xid, m.id
		LIMIT `+a.add(limit), a.args...)
	if err != nil {
		return nil, fmt.Errorf("выборка изменений: %w", err)
	}
	defer rows.Close()

	var recs []HubChange
	for rows.Next() {
		var xidText string
		m, err := scanMemory(rowWithExtra{row: rows, extra: []any{&xidText}})
		if err != nil {
			return nil, fmt.Errorf("сканирование изменения: %w", err)
		}
		xid, err := strconv.ParseUint(xidText, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("разбор change_xid %q записи %s: %w", xidText, m.Id, err)
		}
		recs = append(recs, HubChange{Memory: m, XID: xid})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("выборка изменений: %w", err)
	}

	ids := make([]string, len(recs))
	for i, r := range recs {
		ids[i] = r.Memory.Id
	}
	tagsByID, err := h.pg.loadTagsBatch(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	linksByID, err := h.pg.loadLinksBatch(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	for i := range recs {
		recs[i].Memory.Tags = tagsByID[recs[i].Memory.Id]
		recs[i].Memory.Links = linksByID[recs[i].Memory.Id]
	}
	return recs, nil
}
