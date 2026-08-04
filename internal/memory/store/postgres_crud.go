package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/oklog/ulid/v2"

	"agentmemory/internal/memory"
)

// Put создаёт или полностью обновляет запись памяти (upsert по id) вместе с её тегами
// и связями в одной транзакции — поведенческий паритет с sqliteStore.Put. search_vec
// не входит в список колонок ни INSERT, ни ON CONFLICT DO UPDATE SET — его
// пересчитывает триггер memories_search_vec_trg (postgres.go) до записи строки.
func (s *pgStore) Put(ctx context.Context, m memory.Memory) error {
	if m.Id == "" {
		return fmt.Errorf("memory: Id обязателен")
	}
	if _, err := ulid.Parse(m.Id); err != nil {
		return fmt.Errorf("memory: Id %q не является ULID: %w", m.Id, err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("начало транзакции: %w", err)
	}
	defer tx.Rollback()

	// expires_at: 0 (Memory.ExpiresAt "вечная") хранится как SQL NULL, не литеральный
	// 0 — иначе metaFiltersPG (m.expires_at IS NULL OR m.expires_at > ?now) счёл бы
	// такую запись просроченной ещё в 1970 году (тот же довод, что в sqliteStore.Put).
	var expiresAt any
	if m.ExpiresAt > 0 {
		expiresAt = m.ExpiresAt
	}
	// *string/*int64 поля передаются как any с явным разыменованием (не напрямую
	// указателем) — не полагаемся на то, умеет ли конкретный database/sql-драйвер
	// разворачивать указатели через DefaultParameterConverter; nil остаётся nil -> SQL NULL.
	var summary, superseded, source any
	if m.Summary != nil {
		summary = *m.Summary
	}
	if m.SupersededBy != nil {
		superseded = *m.SupersededBy
	}
	if m.Source != nil {
		source = *m.Source
	}
	var accessedAt any
	if m.AccessedAt != nil {
		accessedAt = *m.AccessedAt
	}
	// status: пусто -> SQL NULL (тот же довод, что в sqliteStore.Put).
	var status any
	if m.Status != "" {
		status = string(m.Status)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO memories (id, kind, scope, content, summary, importance, created_at, updated_at, accessed_at, access_count, superseded_by, source, expires_at, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		ON CONFLICT (id) DO UPDATE SET
			kind=excluded.kind, scope=excluded.scope, content=excluded.content, summary=excluded.summary,
			importance=excluded.importance, created_at=excluded.created_at, updated_at=excluded.updated_at,
			accessed_at=excluded.accessed_at, access_count=excluded.access_count,
			superseded_by=excluded.superseded_by, source=excluded.source, expires_at=excluded.expires_at,
			status=excluded.status
	`, m.Id, string(m.Kind), m.Scope, m.Content, summary, m.Importance, m.CreatedAt, m.UpdatedAt, accessedAt, m.AccessCount, superseded, source, expiresAt, status)
	if err != nil {
		return fmt.Errorf("upsert memories: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM tags WHERE memory_id = $1`, m.Id); err != nil {
		return fmt.Errorf("очистка tags: %w", err)
	}
	for _, tag := range m.Tags {
		if _, err := tx.ExecContext(ctx, `INSERT INTO tags (memory_id, tag) VALUES ($1, $2)`, m.Id, tag); err != nil {
			return fmt.Errorf("вставка tag %q: %w", tag, err)
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM links WHERE src = $1`, m.Id); err != nil {
		return fmt.Errorf("очистка links: %w", err)
	}
	for _, link := range m.Links {
		rel := link.Rel
		if rel == "" {
			rel = memory.RelRelated
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO links (src, dst, rel) VALUES ($1, $2, $3)`, m.Id, link.Dst, rel); err != nil {
			return fmt.Errorf("вставка link -> %q: %w", link.Dst, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Get возвращает запись по id вместе с её тегами и исходящими связями.
func (s *pgStore) Get(ctx context.Context, id string) (memory.Memory, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, kind, scope, content, summary, importance, created_at, updated_at, accessed_at, access_count, superseded_by, source, expires_at, status
		FROM memories WHERE id = $1
	`, id)

	m, err := scanMemory(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return memory.Memory{}, fmt.Errorf("get %q: %w", id, ErrNotFound)
		}
		return memory.Memory{}, fmt.Errorf("get %q: %w", id, err)
	}

	if m.Tags, err = s.loadTags(ctx, id); err != nil {
		return memory.Memory{}, err
	}
	if m.Links, err = s.loadLinks(ctx, id); err != nil {
		return memory.Memory{}, err
	}
	return m, nil
}

func (s *pgStore) loadTags(ctx context.Context, id string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT tag FROM tags WHERE memory_id = $1 ORDER BY tag`, id)
	if err != nil {
		return nil, fmt.Errorf("чтение tags %q: %w", id, err)
	}
	defer rows.Close()

	var tags []string
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			return nil, fmt.Errorf("сканирование tag %q: %w", id, err)
		}
		tags = append(tags, tag)
	}
	return tags, rows.Err()
}

// dbQueryer — общий интерфейс *sql.DB и *sql.Tx, которым достаточно loadTagsBatch/
// loadLinksBatch: обеим функциям всё равно, участвуют ли они в чужой транзакции
// (List зовёт их из tx ради консистентной пары с total) или читают вне её (Neighbors/
// AllForExport/AllForReindex — там отдельной транзакционной консистентности не было и
// раньше, построчные loadTags/loadLinks тоже читали через s.db напрямую).
type dbQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// loadTagsBatch — аналог sqliteStore.loadTagsBatch: грузит теги сразу для набора id
// одним запросом (memory_id IN (...)), вместо построчных loadTags в цикле (N+1).
// Возвращает map с пустым срезом отсутствующим — вызывающая сторона получает nil для
// записей без тегов.
func (s *pgStore) loadTagsBatch(ctx context.Context, q dbQueryer, ids []string) (map[string][]string, error) {
	result := make(map[string][]string, len(ids))
	if len(ids) == 0 {
		return result, nil
	}

	a := &argList{}
	placeholders := make([]string, len(ids))
	for i, id := range ids {
		placeholders[i] = a.add(id)
	}

	rows, err := q.QueryContext(ctx, `
		SELECT memory_id, tag FROM tags WHERE memory_id IN (`+strings.Join(placeholders, ",")+`)
		ORDER BY memory_id, tag
	`, a.args...)
	if err != nil {
		return nil, fmt.Errorf("батч-чтение tags: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id, tag string
		if err := rows.Scan(&id, &tag); err != nil {
			return nil, fmt.Errorf("сканирование tag (батч): %w", err)
		}
		result[id] = append(result[id], tag)
	}
	return result, rows.Err()
}

// loadLinksBatch — симметричный loadTagsBatch аналог для исходящих связей: грузит
// links сразу для набора id одним запросом (src IN (...)), вместо построчных
// loadLinks в цикле. Используется там, где записей может быть много (AllForExport,
// AllForReindex) — на каждый КЛИЕНТ-СЕРВЕР round-trip тратится сетевое время (в
// отличие от sqlite.go, где то же самое — обращение к локальному файлу), и на общей
// базе в десятки тысяч записей построчная загрузка превращается в десятки тысяч
// лишних round-trip'ов на один вызов. Возвращает map с отсутствующим ключом для
// записей без исходящих связей — вызывающая сторона получает nil (тот же контракт,
// что у loadTagsBatch).
func (s *pgStore) loadLinksBatch(ctx context.Context, q dbQueryer, ids []string) (map[string][]memory.Link, error) {
	result := make(map[string][]memory.Link, len(ids))
	if len(ids) == 0 {
		return result, nil
	}

	a := &argList{}
	placeholders := make([]string, len(ids))
	for i, id := range ids {
		placeholders[i] = a.add(id)
	}

	rows, err := q.QueryContext(ctx, `
		SELECT src, dst, rel FROM links WHERE src IN (`+strings.Join(placeholders, ",")+`)
		ORDER BY src, dst, rel
	`, a.args...)
	if err != nil {
		return nil, fmt.Errorf("батч-чтение links: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var src string
		var l memory.Link
		if err := rows.Scan(&src, &l.Dst, &l.Rel); err != nil {
			return nil, fmt.Errorf("сканирование link (батч): %w", err)
		}
		result[src] = append(result[src], l)
	}
	return result, rows.Err()
}

func (s *pgStore) loadLinks(ctx context.Context, id string) ([]memory.Link, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT dst, rel FROM links WHERE src = $1 ORDER BY dst, rel`, id)
	if err != nil {
		return nil, fmt.Errorf("чтение links %q: %w", id, err)
	}
	defer rows.Close()

	var links []memory.Link
	for rows.Next() {
		var l memory.Link
		if err := rows.Scan(&l.Dst, &l.Rel); err != nil {
			return nil, fmt.Errorf("сканирование link %q: %w", id, err)
		}
		links = append(links, l)
	}
	return links, rows.Err()
}

// Neighbors возвращает записи, связанные с id рёбрами links (в любом направлении),
// исключая мягко забытые. $1 используется дважды (обе стороны ребра) — id один и тот
// же, поэтому одного параметра достаточно (в отличие от sqlite.go, где ? позиционные
// и id передаётся аргументом дважды). Tags подгружаются одним батч-запросом
// (loadTagsBatch) на всех соседей сразу, а не построчно в цикле — та же логика, что и
// у List (Links здесь исторически не грузятся вовсе, это сохранённое поведение, не
// упущение).
func (s *pgStore) Neighbors(ctx context.Context, id string) ([]memory.Memory, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT m.id, m.kind, m.scope, m.content, m.summary, m.importance, m.created_at, m.updated_at, m.accessed_at, m.access_count, m.superseded_by, m.source, m.expires_at, m.status
		FROM memories m
		JOIN links l ON (l.dst = m.id AND l.src = $1) OR (l.src = m.id AND l.dst = $1)
		WHERE m.superseded_by IS NULL
	`, id)
	if err != nil {
		return nil, fmt.Errorf("обход соседей %q: %w", id, err)
	}
	defer rows.Close()

	var neighbors []memory.Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, fmt.Errorf("сканирование соседа %q: %w", id, err)
		}
		neighbors = append(neighbors, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("чтение соседей %q: %w", id, err)
	}

	ids := make([]string, len(neighbors))
	for i, m := range neighbors {
		ids[i] = m.Id
	}
	tagsByID, err := s.loadTagsBatch(ctx, s.db, ids)
	if err != nil {
		return nil, err
	}
	for i := range neighbors {
		neighbors[i].Tags = tagsByID[neighbors[i].Id]
	}
	return neighbors, nil
}

// Supersede помечает oldID как заменённый записью newID (мягкое забывание).
func (s *pgStore) Supersede(ctx context.Context, oldID, newID string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE memories SET superseded_by = $1, updated_at = $2 WHERE id = $3`, newID, s.now(), oldID)
	if err != nil {
		return fmt.Errorf("supersede %q -> %q: %w", oldID, newID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("supersede %q -> %q: %w", oldID, newID, err)
	}
	if n == 0 {
		return fmt.Errorf("supersede %q -> %q: %w", oldID, newID, ErrNotFound)
	}
	return nil
}

// Touch обновляет accessed_at на текущее время и увеличивает access_count на 1.
func (s *pgStore) Touch(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE memories SET accessed_at = $1, access_count = access_count + 1 WHERE id = $2`, s.now(), id)
	if err != nil {
		return fmt.Errorf("touch %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("touch %q: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("touch %q: %w", id, ErrNotFound)
	}
	return nil
}

// List возвращает страницу записей под фильтр f (browse-морда) и total — общее число
// записей под тот же фильтр без учёта Limit/Offset, в одной транзакции (консистентная
// пара под конкурентной записью), с батч-подгруженными Tags. Паритет с sqliteStore.List.
func (s *pgStore) List(ctx context.Context, f BrowseFilter) ([]memory.Memory, int, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = listDefaultLimit
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	a := &argList{}
	var conds []string
	if !f.IncludeSuperseded {
		conds = append(conds, "m.superseded_by IS NULL")
	}
	if clause := scopePredicatePG(a, f.Scopes, ""); clause != "" {
		conds = append(conds, clause)
	}
	if clause := kindPredicatePG(a, f.Kinds); clause != "" {
		conds = append(conds, clause)
	}

	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("начало транзакции: %w", err)
	}
	defer tx.Rollback()

	var total int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM memories m"+where, a.args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("подсчёт total для List: %w", err)
	}

	limitPh := a.add(limit)
	offsetPh := a.add(offset)
	query := `
		SELECT m.id, m.kind, m.scope, m.content, m.summary, m.importance, m.created_at, m.updated_at, m.accessed_at, m.access_count, m.superseded_by, m.source, m.expires_at, m.status
		FROM memories m` + where + `
		ORDER BY m.created_at DESC, m.id DESC
		LIMIT ` + limitPh + ` OFFSET ` + offsetPh

	rows, err := tx.QueryContext(ctx, query, a.args...)
	if err != nil {
		return nil, 0, fmt.Errorf("выборка List: %w", err)
	}
	defer rows.Close()

	var items []memory.Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("сканирование List: %w", err)
		}
		items = append(items, m)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("чтение List: %w", err)
	}

	ids := make([]string, len(items))
	for i, m := range items {
		ids[i] = m.Id
	}
	tagsByID, err := s.loadTagsBatch(ctx, tx, ids)
	if err != nil {
		return nil, 0, err
	}
	for i := range items {
		items[i].Tags = tagsByID[items[i].Id]
	}

	if err := tx.Commit(); err != nil {
		return nil, 0, fmt.Errorf("commit: %w", err)
	}
	return items, total, nil
}

// Stats возвращает сводные счётчики памяти: всего живых и архивных записей, разбивки
// живых записей по scope и kind. Паритет с sqliteStore.Stats — сам запрос общий,
// statsFromDB (common.go).
func (s *pgStore) Stats(ctx context.Context) (Stats, error) {
	return statsFromDB(ctx, s.db)
}

// loadTagsAndLinksBatch подгружает Tags и Links для all батчем (loadTagsBatch +
// loadLinksBatch, по одному round-trip'у на весь срез каждая — не по одному на
// запись) и раскладывает результат обратно по all[i]. Общая хвостовая часть
// AllForExport и AllForReindex — единственное, чем они отличаются от sqliteStore.
// AllForExport/AllForReindex (там та же надстройка построчная, loadTags/loadLinks в
// цикле — дёшево для локального файла, дорого для сетевого postgres, см. постатейный
// комментарий у loadLinksBatch).
func (s *pgStore) loadTagsAndLinksBatch(ctx context.Context, all []memory.Memory) error {
	ids := make([]string, len(all))
	for i, m := range all {
		ids[i] = m.Id
	}

	tagsByID, err := s.loadTagsBatch(ctx, s.db, ids)
	if err != nil {
		return err
	}
	linksByID, err := s.loadLinksBatch(ctx, s.db, ids)
	if err != nil {
		return err
	}
	for i := range all {
		all[i].Tags = tagsByID[all[i].Id]
		all[i].Links = linksByID[all[i].Id]
	}
	return nil
}

// AllForExport возвращает ВСЕ записи под фильтром scopes — включая тумстоуны — вместе
// с tags/links, отсортированные по id ASC (детерминированный порядок, DESIGN.md §12,
// задача 9.2). Паритет с sqliteStore.AllForExport.
func (s *pgStore) AllForExport(ctx context.Context, scopes []string) ([]memory.Memory, error) {
	a := &argList{}
	where := ""
	if clause := scopePredicatePG(a, scopes, ""); clause != "" {
		where = " WHERE " + clause
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, kind, scope, content, summary, importance, created_at, updated_at, accessed_at, access_count, superseded_by, source, expires_at, status
		FROM memories m`+where+`
		ORDER BY m.id ASC
	`, a.args...)
	if err != nil {
		return nil, fmt.Errorf("чтение записей для export: %w", err)
	}
	defer rows.Close()

	var all []memory.Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, fmt.Errorf("сканирование записи для export: %w", err)
		}
		all = append(all, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("чтение записей для export: %w", err)
	}

	if err := s.loadTagsAndLinksBatch(ctx, all); err != nil {
		return nil, err
	}
	return all, nil
}

// AllForReindex возвращает все не устаревшие записи вместе с tags/links — источник
// для фонового reindex-джоба при смене модели Embedder. Базовый SELECT — общий со
// sqliteStore.AllForReindex код (liveMemoriesFromDB, common.go), Tags/Links здесь
// достраиваются батчем (loadTagsAndLinksBatch) — это самый горячий из трёх мест,
// переведённых на батч-загрузку: вызывается на весь набор живых записей при каждом
// фоновом reindex-джобе (смена модели эмбеддера).
func (s *pgStore) AllForReindex(ctx context.Context) ([]memory.Memory, error) {
	all, err := liveMemoriesFromDB(ctx, s.db)
	if err != nil {
		return nil, err
	}
	if err := s.loadTagsAndLinksBatch(ctx, all); err != nil {
		return nil, err
	}
	return all, nil
}
