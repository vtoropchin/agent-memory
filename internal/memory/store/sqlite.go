package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"context"

	_ "github.com/mattn/go-sqlite3"
	"github.com/oklog/ulid/v2"

	"agentmemory/internal/memory"
)

// ErrNotFound возвращается, когда запись с указанным id не найдена.
var ErrNotFound = errors.New("memory: запись не найдена")

// schema — DDL хранилища (DESIGN.md §4). Идемпотентен: все объекты создаются
// через IF NOT EXISTS, поэтому Open можно вызывать многократно на одном файле.
//
// Отклонение от иллюстративной схемы в DESIGN.md: memories_fts объявлена как
// самодостаточная (не external-content, без `content='memories'`). Причина: колонка
// `tags` в fts — денормализованный агрегат из таблицы `tags`, а не колонка `memories`,
// поэтому корректная синхронизация через специальные 'delete'-команды external-content
// режима потребовала бы либо дублирования агрегата тегов в самой `memories` (лишняя
// колонка вне схемы §4), либо риска рассинхронизации индекса при неточном совпадении
// удаляемых значений. Самодостаточная fts5-таблица поддерживает обычные UPDATE/DELETE
// по rowid, что делает триггеры синхронизации простыми и заведомо корректными;
// накладные расходы на дублирование текста незначительны на масштабах MVP (см. §1
// «не-цели»: не промышленный масштаб).
const schema = `
CREATE TABLE IF NOT EXISTS memories (
    id            TEXT PRIMARY KEY,
    kind          TEXT NOT NULL,
    scope         TEXT NOT NULL,
    content       TEXT NOT NULL,
    summary       TEXT,
    importance    INTEGER NOT NULL DEFAULT 3,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    accessed_at   INTEGER,
    access_count  INTEGER NOT NULL DEFAULT 0,
    superseded_by TEXT,
    source        TEXT
);

CREATE TABLE IF NOT EXISTS tags (
    memory_id TEXT NOT NULL REFERENCES memories(id),
    tag       TEXT NOT NULL,
    PRIMARY KEY (memory_id, tag)
);

CREATE TABLE IF NOT EXISTS links (
    src TEXT NOT NULL REFERENCES memories(id),
    dst TEXT NOT NULL REFERENCES memories(id),
    rel TEXT NOT NULL DEFAULT 'related',
    PRIMARY KEY (src, dst, rel)
);

CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
    content, summary, tags
);

CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN
    INSERT INTO memories_fts(rowid, content, summary, tags)
    VALUES (new.rowid, new.content, coalesce(new.summary, ''), '');
END;

CREATE TRIGGER IF NOT EXISTS memories_au AFTER UPDATE ON memories BEGIN
    UPDATE memories_fts SET content = new.content, summary = coalesce(new.summary, '')
    WHERE rowid = new.rowid;
END;

CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN
    DELETE FROM memories_fts WHERE rowid = old.rowid;
END;

CREATE TRIGGER IF NOT EXISTS tags_ai AFTER INSERT ON tags BEGIN
    UPDATE memories_fts SET tags = (
        SELECT coalesce(group_concat(tag, ' '), '') FROM tags WHERE memory_id = new.memory_id
    ) WHERE rowid = (SELECT rowid FROM memories WHERE id = new.memory_id);
END;

CREATE TRIGGER IF NOT EXISTS tags_ad AFTER DELETE ON tags BEGIN
    UPDATE memories_fts SET tags = (
        SELECT coalesce(group_concat(tag, ' '), '') FROM tags WHERE memory_id = old.memory_id
    ) WHERE rowid = (SELECT rowid FROM memories WHERE id = old.memory_id);
END;

-- Векторный индекс (Этап 3) — создаётся, когда включён Embedder; dim берётся из модели.
-- CREATE VIRTUAL TABLE memories_vec USING vec0(
--     memory_id TEXT PRIMARY KEY,
--     embedding float[<DIM>] distance_metric=cosine
-- );

CREATE TABLE IF NOT EXISTS embed_meta (
    id    INTEGER PRIMARY KEY CHECK (id = 1),
    model TEXT NOT NULL,
    dim   INTEGER NOT NULL
);
`

// sqliteStore — реализация Store поверх SQLite (метаданные + FTS5). Векторный слой
// (SearchVector/PutVector) — заглушка до Этапа 3.
type sqliteStore struct {
	db  *sql.DB
	now func() int64 // источник текущего unix ts; подменяется в тестах
}

var _ Store = (*sqliteStore)(nil)

func defaultNow() int64 {
	return time.Now().Unix()
}

// Open открывает (или создаёт) файл SQLite по указанному пути, применяет схему и
// возвращает готовый Store. Путь может начинаться с `~` — раскрывается в домашний
// каталог пользователя. Родительские директории создаются при необходимости.
func Open(path string) (*sqliteStore, error) {
	expanded, err := expandHome(path)
	if err != nil {
		return nil, fmt.Errorf("раскрытие пути %q: %w", path, err)
	}

	if dir := filepath.Dir(expanded); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("создание директории %q: %w", dir, err)
		}
	}

	dsn := fmt.Sprintf("file:%s?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000", expanded)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("открытие sqlite %q: %w", expanded, err)
	}

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("проверка соединения sqlite %q: %w", expanded, err)
	}

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("применение схемы: %w", err)
	}

	return &sqliteStore{db: db, now: defaultNow}, nil
}

func expandHome(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("получение домашней директории: %w", err)
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~")), nil
	}
	return path, nil
}

// Close закрывает соединение с БД.
func (s *sqliteStore) Close() error {
	return s.db.Close()
}

// Put создаёт или полностью обновляет запись памяти (upsert по id) вместе с её
// тегами и связями в одной транзакции.
func (s *sqliteStore) Put(ctx context.Context, m memory.Memory) error {
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

	_, err = tx.ExecContext(ctx, `
		INSERT INTO memories (id, kind, scope, content, summary, importance, created_at, updated_at, accessed_at, access_count, superseded_by, source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			kind=excluded.kind, scope=excluded.scope, content=excluded.content, summary=excluded.summary,
			importance=excluded.importance, created_at=excluded.created_at, updated_at=excluded.updated_at,
			accessed_at=excluded.accessed_at, access_count=excluded.access_count,
			superseded_by=excluded.superseded_by, source=excluded.source
	`, m.Id, string(m.Kind), m.Scope, m.Content, m.Summary, m.Importance, m.CreatedAt, m.UpdatedAt, m.AccessedAt, m.AccessCount, m.SupersededBy, m.Source)
	if err != nil {
		return fmt.Errorf("upsert memories: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM tags WHERE memory_id = ?`, m.Id); err != nil {
		return fmt.Errorf("очистка tags: %w", err)
	}
	for _, tag := range m.Tags {
		if _, err := tx.ExecContext(ctx, `INSERT INTO tags (memory_id, tag) VALUES (?, ?)`, m.Id, tag); err != nil {
			return fmt.Errorf("вставка tag %q: %w", tag, err)
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM links WHERE src = ?`, m.Id); err != nil {
		return fmt.Errorf("очистка links: %w", err)
	}
	for _, link := range m.Links {
		rel := link.Rel
		if rel == "" {
			rel = memory.RelRelated
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO links (src, dst, rel) VALUES (?, ?, ?)`, m.Id, link.Dst, rel); err != nil {
			return fmt.Errorf("вставка link -> %q: %w", link.Dst, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Get возвращает запись по id вместе с её тегами и исходящими связями.
func (s *sqliteStore) Get(ctx context.Context, id string) (memory.Memory, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, kind, scope, content, summary, importance, created_at, updated_at, accessed_at, access_count, superseded_by, source
		FROM memories WHERE id = ?
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

func (s *sqliteStore) loadTags(ctx context.Context, id string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT tag FROM tags WHERE memory_id = ? ORDER BY tag`, id)
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

func (s *sqliteStore) loadLinks(ctx context.Context, id string) ([]memory.Link, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT dst, rel FROM links WHERE src = ? ORDER BY dst, rel`, id)
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

// rowScanner — общий интерфейс *sql.Row и *sql.Rows, используемый scanMemory.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanMemory(row rowScanner) (memory.Memory, error) {
	var (
		m          memory.Memory
		kind       string
		summary    sql.NullString
		accessedAt sql.NullInt64
		superseded sql.NullString
		source     sql.NullString
	)

	if err := row.Scan(&m.Id, &kind, &m.Scope, &m.Content, &summary, &m.Importance,
		&m.CreatedAt, &m.UpdatedAt, &accessedAt, &m.AccessCount, &superseded, &source); err != nil {
		return memory.Memory{}, err
	}

	m.Kind = memory.Kind(kind)
	if summary.Valid {
		m.Summary = &summary.String
	}
	if accessedAt.Valid {
		m.AccessedAt = &accessedAt.Int64
	}
	if superseded.Valid {
		m.SupersededBy = &superseded.String
	}
	if source.Valid {
		m.Source = &source.String
	}
	return m, nil
}

// SearchLexical ищет по FTS5-индексу (content/summary/tags), фильтрует по scope и
// kind, исключает записи с проставленным superseded_by. Score — инверсия bm25
// (больше значит релевантнее).
//
// Tags кандидатов НЕ заполняются (Memory.Tags остаётся nil) — это read-hot-path
// (recall дергает его на каждый запрос), а ни recall (см. rank/formatLine), ни
// write-дедуп (сравнивает только Content через SimHash) теги кандидатов не читают;
// при усилении дубля write.reinforce берёт полную запись отдельным Store.Get.
// Загрузка тегов построчно для каждого из до Limit кандидатов стоила N+1 запросов
// на recall без какой-либо пользы.
func (s *sqliteStore) SearchLexical(ctx context.Context, q memory.Query) ([]memory.Candidate, error) {
	var (
		conds []string
		args  []any
	)
	conds = append(conds, "m.superseded_by IS NULL")

	matchQuery := sanitizeFTSQuery(q.Text)
	useMatch := matchQuery != ""

	query := "SELECT m.id, m.kind, m.scope, m.content, m.summary, m.importance, m.created_at, m.updated_at, m.accessed_at, m.access_count, m.superseded_by, m.source"
	if useMatch {
		query += ", -bm25(memories_fts) AS score FROM memories_fts JOIN memories m ON m.rowid = memories_fts.rowid"
		conds = append(conds, "memories_fts MATCH ?")
		args = append(args, matchQuery)
	} else {
		query += ", 0.0 AS score FROM memories m"
	}

	if q.Scope != "" {
		conds = append(conds, "m.scope = ?")
		args = append(args, q.Scope)
	}
	if len(q.Kinds) > 0 {
		placeholders := make([]string, len(q.Kinds))
		for i, k := range q.Kinds {
			placeholders[i] = "?"
			args = append(args, k)
		}
		conds = append(conds, "m.kind IN ("+strings.Join(placeholders, ",")+")")
	}
	if q.SinceTs > 0 {
		conds = append(conds, "m.created_at >= ?")
		args = append(args, q.SinceTs)
	}

	query += " WHERE " + strings.Join(conds, " AND ")
	if useMatch {
		query += " ORDER BY score DESC"
	} else {
		query += " ORDER BY m.importance DESC, m.created_at DESC"
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	query += " LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("лексический поиск: %w", err)
	}
	defer rows.Close()

	var candidates []memory.Candidate
	for rows.Next() {
		var score float64
		m, err := scanMemory(rowWithExtra{row: rows, extra: []any{&score}})
		if err != nil {
			return nil, fmt.Errorf("сканирование кандидата: %w", err)
		}
		candidates = append(candidates, memory.Candidate{Memory: m, Score: score, Source: "lexical"})
	}
	return candidates, rows.Err()
}

// rowWithExtra адаптирует rowScanner так, чтобы scanMemory (рассчитанный на ровно
// 12 колонок memories) можно было переиспользовать для запросов, добавляющих
// дополнительные колонки после стандартных 12 (например, score в SearchLexical):
// dest от scanMemory дополняется extra перед вызовом настоящего Scan.
type rowWithExtra struct {
	row   rowScanner
	extra []any
}

func (r rowWithExtra) Scan(dest ...any) error {
	return r.row.Scan(append(dest, r.extra...)...)
}

// sanitizeFTSQuery превращает произвольный пользовательский текст в безопасное
// FTS5 MATCH-выражение: каждое слово оборачивается в кавычки (двойные кавычки
// внутри экранируются удвоением), что нейтрализует операторы FTS5 (AND/OR/NOT,
// колонковые фильтры, скобки, дефисы) — они становятся частью буквальной фразы.
// Слова соединяются через OR (найти запись, содержащую хотя бы одно слово).
func sanitizeFTSQuery(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		escaped := strings.ReplaceAll(f, `"`, `""`)
		parts = append(parts, `"`+escaped+`"`)
	}
	return strings.Join(parts, " OR ")
}

// SearchVector — заглушка Этапа 1: векторный слой не подключён (Embedder=noop),
// поиск деградирует до SearchLexical на уровне recall.
func (s *sqliteStore) SearchVector(_ context.Context, _ []float32, _ memory.Query) ([]memory.Candidate, error) {
	return nil, nil
}

// Neighbors возвращает записи, связанные с id рёбрами links (в любом направлении),
// исключая мягко забытые (superseded_by IS NOT NULL) — как и SearchLexical, соседи
// не должны включать тумстоуны.
func (s *sqliteStore) Neighbors(ctx context.Context, id string) ([]memory.Memory, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT m.id, m.kind, m.scope, m.content, m.summary, m.importance, m.created_at, m.updated_at, m.accessed_at, m.access_count, m.superseded_by, m.source
		FROM memories m
		JOIN links l ON (l.dst = m.id AND l.src = ?) OR (l.src = m.id AND l.dst = ?)
		WHERE m.superseded_by IS NULL
	`, id, id)
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
		if m.Tags, err = s.loadTags(ctx, m.Id); err != nil {
			return nil, err
		}
		neighbors = append(neighbors, m)
	}
	return neighbors, rows.Err()
}

// Supersede помечает oldID как заменённый записью newID (мягкое забывание).
func (s *sqliteStore) Supersede(ctx context.Context, oldID, newID string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE memories SET superseded_by = ?, updated_at = ? WHERE id = ?`, newID, s.now(), oldID)
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
func (s *sqliteStore) Touch(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE memories SET accessed_at = ?, access_count = access_count + 1 WHERE id = ?`, s.now(), id)
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

// EmbedMeta возвращает модель и размерность вектора, зафиксированные последним
// reindex. Если reindex ещё не запускался, возвращает ("", 0, nil).
func (s *sqliteStore) EmbedMeta(ctx context.Context) (string, int, error) {
	var model string
	var dim int
	err := s.db.QueryRowContext(ctx, `SELECT model, dim FROM embed_meta WHERE id = 1`).Scan(&model, &dim)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("чтение embed_meta: %w", err)
	}
	return model, dim, nil
}

// ResetVectorIndex фиксирует новую модель/размерность эмбеддинга. Пересоздание
// самой vec-таблицы — Этап 3; в MVP метод только обновляет embed_meta.
func (s *sqliteStore) ResetVectorIndex(ctx context.Context, model string, dim int) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO embed_meta (id, model, dim) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET model = excluded.model, dim = excluded.dim
	`, model, dim)
	if err != nil {
		return fmt.Errorf("сброс embed_meta: %w", err)
	}
	return nil
}

// AllForReindex возвращает все записи для пересчёта эмбеддингов фоновым джобом.
func (s *sqliteStore) AllForReindex(ctx context.Context) ([]memory.Memory, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, kind, scope, content, summary, importance, created_at, updated_at, accessed_at, access_count, superseded_by, source
		FROM memories
	`)
	if err != nil {
		return nil, fmt.Errorf("чтение всех записей для reindex: %w", err)
	}
	defer rows.Close()

	var all []memory.Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, fmt.Errorf("сканирование записи для reindex: %w", err)
		}
		all = append(all, m)
	}
	return all, rows.Err()
}

// PutVector — заглушка Этапа 1: векторный индекс не создан, запись игнорируется.
func (s *sqliteStore) PutVector(_ context.Context, _ string, _ []float32) error {
	return nil
}
