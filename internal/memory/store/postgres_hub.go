// postgres_hub.go — общая база синхронизации (Этап 19, DESIGN.md §4.1): та же схема
// postgres-бэкенда, но открытая БЕЗ автомиграции, и инкрементальная выборка изменений
// по серверному номеру изменения (memories.change_xid, шаг migrateChangeXIDPG).
//
// store.Open с driver = "postgres" по-прежнему приводит схему к целевой при каждом
// открытии — это режим «своё хранилище». Hub DDL не выполняет вовсе: схему создаёт и
// обновляет только явный ApplySchema (кнопка с подтверждением в морде, `memo sync
// -apply-schema`), а синхронизация на схеме чужой версии не идёт — это правило
// интерактора, Hub лишь сообщает состояние (HubSchema).
//
// Hub — адаптер порта memsync.Shared (задача 19.5): механика транзакций, снимков и
// CAS-условий; решающие правила (LWW, состав набора, слияние связей) — в интеракторе.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"agentmemory/internal/config"
	"agentmemory/internal/memory"
	memsync "agentmemory/internal/memory/sync"
)

// Hub — общая база одного проекта, открытая для синхронизации.
type Hub struct {
	pg *pgStore
}

var _ memsync.Shared = (*Hub)(nil)

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

// HubSchemaVersion — версия схемы общей базы, которую понимает этот бинарь:
// синхронизация идёт только при точном совпадении (memsync.New, DESIGN.md §4.1).
const HubSchemaVersion = pgSchemaVersion

// cursorLess — строгий порядок курсоров pull: по XID, затем по ID (пустой ID —
// меньше любого непустого).
func cursorLess(c, o memsync.Cursor) bool {
	if c.XID != o.XID {
		return c.XID < o.XID
	}
	return c.ID < o.ID
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

// Pull отдаёт изменения записей scope (kinds пусто — все виды) после from
// страницами не больше limit записей: в порядке (change_xid, id), с тегами и
// исходящими связями, вместе с тумстоунами. Refs страницы — записи (любого scope и
// вида), на которые ссылаются связи её записей, но которых в странице нет. Все
// страницы читаются в одной транзакции REPEATABLE READ READ ONLY — один снимок; xid
// читателю не выдаётся. Ошибка page прерывает выборку и возвращается как есть.
//
// Курсор — горизонт снимка, а не последний прочитанный номер (DESIGN.md §4.1):
// xmin = pg_snapshot_xmin снимка, все транзакции с меньшим xid к снимку завершены и
// видны. Страница, последняя строка которой (L, I) при L < xmin, отдаёт Next = (L, I);
// иначе и после последней страницы — (xmin, ""), строки окна [xmin, xmax) следующий
// pull перечитает. Next не меньше from. Нет изменений — одна пустая страница с
// продвинутым курсором.
func (h *Hub) Pull(ctx context.Context, scope string, kinds []memory.Kind, from memsync.Cursor, limit int, page func(memsync.Page) error) error {
	if limit <= 0 {
		return fmt.Errorf("выборка изменений: limit должен быть положительным, получено %d", limit)
	}

	tx, err := h.snapshot(ctx)
	if err != nil {
		return fmt.Errorf("начало транзакции выборки изменений: %w", err)
	}
	defer tx.Rollback()

	// Первый запрос транзакции фиксирует её снимок, поэтому xmin берётся именно им.
	xmin, err := scanXID(tx.QueryRowContext(ctx, `SELECT pg_snapshot_xmin(pg_current_snapshot())::text`))
	if err != nil {
		return fmt.Errorf("чтение горизонта снимка: %w", err)
	}

	horizon := memsync.Cursor{XID: xmin}
	if cursorLess(horizon, from) {
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
			pos = memsync.Cursor{XID: l.XID, ID: l.Memory.Id}
			// Транзакция L завершилась до снимка: все её строки видны и идут по id, а
			// строки с меньшим xid уже отданы. pos > from — keyset строго растёт.
			if l.XID < xmin {
				next = pos
			}
		}
		refs, err := h.refs(ctx, tx, recs, nil)
		if err != nil {
			return err
		}
		if err := page(memsync.Page{Records: recs, Refs: refs, Next: next}); err != nil {
			return err
		}
		if last {
			return nil
		}
	}
}

// changesAfter — одна keyset-страница Pull: строки с (change_xid, id) > pos.
// При pos.ID == "" условие равносильно change_xid >= pos.XID (id не пуст).
func (h *Hub) changesAfter(ctx context.Context, tx *sql.Tx, scope string, kinds []memory.Kind, pos memsync.Cursor, limit int) ([]memsync.Remote, error) {
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

	return h.remotes(ctx, tx, `
		SELECT `+memoryColumns+`, m.change_xid::text
		FROM memories m
		WHERE `+strings.Join(conds, " AND ")+`
		ORDER BY m.change_xid, m.id
		LIMIT `+a.add(limit), a.args...)
}

// snapshot открывает транзакцию одного снимка для чтения: REPEATABLE READ READ ONLY,
// xid ей не выдаётся.
func (h *Hub) snapshot(ctx context.Context) (*sql.Tx, error) {
	return h.pg.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
}

// scanXID читает xid8, выбранный как text: беззнаковый 64-битный, в int64 не влезает.
func scanXID(row rowScanner) (uint64, error) {
	var text string
	if err := row.Scan(&text); err != nil {
		return 0, err
	}
	xid, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("разбор xid8 %q: %w", text, err)
	}
	return xid, nil
}

// remotes выполняет query (колонки memoryColumns и change_xid::text) в tx и
// дочитывает теги и исходящие связи найденных записей тем же снимком.
func (h *Hub) remotes(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]memsync.Remote, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("выборка записей общей базы: %w", err)
	}
	defer rows.Close()

	var recs []memsync.Remote
	for rows.Next() {
		var xidText string
		m, err := scanMemory(rowWithExtra{row: rows, extra: []any{&xidText}})
		if err != nil {
			return nil, fmt.Errorf("сканирование записи общей базы: %w", err)
		}
		xid, err := strconv.ParseUint(xidText, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("разбор change_xid %q записи %s: %w", xidText, m.Id, err)
		}
		recs = append(recs, memsync.Remote{Memory: m, XID: xid})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("выборка записей общей базы: %w", err)
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

// byIDs — записи ids (вместе с тумстоунами) тем же снимком tx; отсутствующие пропущены.
func (h *Hub) byIDs(ctx context.Context, tx *sql.Tx, ids []string) ([]memsync.Remote, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	a := &argList{}
	ph := make([]string, len(ids))
	for i, id := range ids {
		ph[i] = a.add(id)
	}
	return h.remotes(ctx, tx, `
		SELECT `+memoryColumns+`, m.change_xid::text
		FROM memories m WHERE m.id IN (`+strings.Join(ph, ",")+`)
		ORDER BY m.id`, a.args...)
}

// refs — адресаты связей recs, которых нет ни среди recs, ни в skip, — тем же
// снимком tx. Общая база держит FK на links.dst, поэтому адресат найдётся всегда.
func (h *Hub) refs(ctx context.Context, tx *sql.Tx, recs []memsync.Remote, skip map[string]bool) ([]memsync.Remote, error) {
	have := map[string]bool{}
	for _, r := range recs {
		have[r.Memory.Id] = true
	}
	var dsts []string
	for _, r := range recs {
		for _, l := range r.Memory.Links {
			if !have[l.Dst] && !skip[l.Dst] {
				have[l.Dst] = true
				dsts = append(dsts, l.Dst)
			}
		}
	}
	return h.byIDs(ctx, tx, dsts)
}

// Base читает состояние общей базы одним снимком (DESIGN.md §4.1): base_id,
// версию схемы (0 — схемы нет), xmax снимка и наибольший change_xid. На схеме
// старее шага change_xid (нет sync_meta) ID и MaxXID пусты — интерактор и так
// остановит прогон по версии.
func (h *Hub) Base(ctx context.Context) (memsync.Base, error) {
	tx, err := h.snapshot(ctx)
	if err != nil {
		return memsync.Base{}, fmt.Errorf("начало транзакции состояния общей базы: %w", err)
	}
	defer tx.Rollback()

	var b memsync.Base
	if b.XMax, err = scanXID(tx.QueryRowContext(ctx, `SELECT pg_snapshot_xmax(pg_current_snapshot())::text`)); err != nil {
		return memsync.Base{}, fmt.Errorf("чтение xmax снимка: %w", err)
	}
	var hasVersion, hasMeta bool
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass('schema_version') IS NOT NULL, to_regclass('sync_meta') IS NOT NULL`).Scan(&hasVersion, &hasMeta); err != nil {
		return memsync.Base{}, fmt.Errorf("проверка схемы общей базы: %w", err)
	}
	if !hasVersion {
		return b, nil
	}
	err = tx.QueryRowContext(ctx, `SELECT version FROM schema_version WHERE id = 1`).Scan(&b.Version)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return memsync.Base{}, fmt.Errorf("чтение schema_version: %w", err)
	}
	if !hasMeta {
		return b, nil
	}
	if err := tx.QueryRowContext(ctx, `SELECT base_id FROM sync_meta WHERE id = 1`).Scan(&b.ID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return memsync.Base{}, fmt.Errorf("чтение sync_meta: %w", err)
	}
	b.MaxXID, err = scanXID(tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT change_xid FROM memories ORDER BY change_xid DESC LIMIT 1), '0'::xid8)::text`))
	if err != nil {
		return memsync.Base{}, fmt.Errorf("чтение наибольшего change_xid: %w", err)
	}
	return b, nil
}

// Versions — версии записей ids (с тегами и связями, вместе с тумстоунами) и Refs —
// адресаты их связей вне ids; один снимок.
func (h *Hub) Versions(ctx context.Context, ids []string) (memsync.Snapshot, error) {
	if len(ids) == 0 {
		return memsync.Snapshot{}, nil
	}
	tx, err := h.snapshot(ctx)
	if err != nil {
		return memsync.Snapshot{}, fmt.Errorf("начало транзакции версий общей базы: %w", err)
	}
	defer tx.Rollback()

	recs, err := h.byIDs(ctx, tx, ids)
	if err != nil {
		return memsync.Snapshot{}, err
	}
	asked := make(map[string]bool, len(ids))
	for _, id := range ids {
		asked[id] = true
	}
	refs, err := h.refs(ctx, tx, recs, asked)
	if err != nil {
		return memsync.Snapshot{}, err
	}
	return memsync.Snapshot{Records: recs, Refs: refs}, nil
}

// pushLockQuery — CAS-блокировка строки в Push: строка с ожидаемым change_xid. FOR NO
// KEY UPDATE, а не FOR UPDATE: id не меняется, а FOR UPDATE конфликтовал бы с FOR KEY
// SHARE, который берёт проверка FK при вставке связи на эту строку. Второй проход
// Push (связи) берёт KEY SHARE на адресаты в произвольном порядке, и два писателя с
// перекрёстными связями X→Y и Y→X взаимно заблокировались бы.
const pushLockQuery = `SELECT content FROM memories WHERE id = $1 AND change_xid = $2::xid8 FOR NO KEY UPDATE`

// Push записывает пачку одной транзакцией READ COMMITTED (DESIGN.md §7.1): записи с
// тегами по CAS в порядке recs (интерактор сортирует по id — единый порядок
// блокировок), затем связи записанных. ExpectXID == 0 — только вставка новой (ON
// CONFLICT DO NOTHING), иначе строка блокируется условием change_xid = ExpectXID
// (pushLockQuery): параллельный писатель успел раньше — под READ COMMITTED
// блокирующий SELECT перепроверяет условие на новой версии строки и не находит её.
// Не прошедшие CAS — stale. accessed_at/access_count существующей записи не
// перезаписываются (у новой — пустые), смена content удаляет строку вектора (вектор
// строится из content), если векторный слой есть.
func (h *Hub) Push(ctx context.Context, recs []memsync.Guarded) (written, stale []string, err error) {
	if len(recs) == 0 {
		return nil, nil, nil
	}
	for _, g := range recs {
		if err := checkMemoryID(g.Memory.Id); err != nil {
			return nil, nil, err
		}
	}
	// Изоляция явно: CAS выше опирается на READ COMMITTED, а дефолт сервера может быть другим.
	tx, err := h.pg.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, nil, fmt.Errorf("начало транзакции отправки: %w", err)
	}
	defer tx.Rollback()

	// memories_vec — ленивая таблица, адресуется схемой, как pgStore.vecTable: голое
	// имя to_regclass нашёл бы и одноимённую таблицу другой схемы search_path.
	var schema string
	if err := tx.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		return nil, nil, fmt.Errorf("текущая схема: %w", err)
	}
	vecTable := (&pgStore{schema: schema}).vecTable()
	var hasVec bool
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, vecTable).Scan(&hasVec); err != nil {
		return nil, nil, fmt.Errorf("проверка memories_vec: %w", err)
	}

	var ok []memory.Memory
	for _, g := range recs {
		m := g.Memory
		m.AccessedAt, m.AccessCount = nil, 0 // значения только для вставки новой записи
		textChanged := false
		upsert := `INSERT INTO memories (` + pgMemoryInsertColumns + `) VALUES (` + pgMemoryInsertValues + `)`
		if g.ExpectXID == 0 {
			upsert += ` ON CONFLICT (id) DO NOTHING`
		} else {
			var content string
			err := tx.QueryRowContext(ctx, pushLockQuery, m.Id, strconv.FormatUint(g.ExpectXID, 10)).Scan(&content)
			if errors.Is(err, sql.ErrNoRows) {
				stale = append(stale, m.Id)
				continue
			}
			if err != nil {
				return nil, nil, fmt.Errorf("блокировка %q: %w", m.Id, err)
			}
			textChanged = content != m.Content
			upsert += ` ON CONFLICT (id) DO UPDATE SET
				kind=excluded.kind, scope=excluded.scope, content=excluded.content, summary=excluded.summary,
				importance=excluded.importance, created_at=excluded.created_at, updated_at=excluded.updated_at,
				superseded_by=excluded.superseded_by, source=excluded.source, expires_at=excluded.expires_at,
				status=excluded.status, task_type=excluded.task_type, branch=excluded.branch, workflow=excluded.workflow,
				author=excluded.author, updated_by=excluded.updated_by`
		}
		res, err := tx.ExecContext(ctx, upsert, pgMemoryArgs(m)...)
		if err != nil {
			return nil, nil, fmt.Errorf("запись %q: %w", m.Id, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return nil, nil, fmt.Errorf("запись %q: %w", m.Id, err)
		} else if n == 0 {
			stale = append(stale, m.Id) // вставка новой, а запись уже есть
			continue
		}
		if err := putTagsPG(ctx, tx, m.Id, m.Tags); err != nil {
			return nil, nil, fmt.Errorf("теги %q: %w", m.Id, err)
		}
		if hasVec && textChanged {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+vecTable+` WHERE memory_id = $1`, m.Id); err != nil {
				return nil, nil, fmt.Errorf("сброс вектора %q: %w", m.Id, err)
			}
		}
		ok = append(ok, m)
	}
	for _, m := range ok {
		if err := putLinksPG(ctx, tx, m.Id, m.Links); err != nil {
			return nil, nil, fmt.Errorf("связи %q: %w", m.Id, err)
		}
		written = append(written, m.Id)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit отправки: %w", err)
	}
	return written, stale, nil
}
