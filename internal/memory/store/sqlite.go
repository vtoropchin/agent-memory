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

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3"
	"github.com/oklog/ulid/v2"

	"agentmemory/internal/memory"
)

// ErrNotFound возвращается, когда запись с указанным id не найдена.
var ErrNotFound = errors.New("memory: запись не найдена")

// listDefaultLimit — размер страницы List по умолчанию, когда BrowseFilter.Limit не
// задан (<=0).
const listDefaultLimit = 50

// semanticNeighborsPad — запас, добавляемый к запрошенному k в MATCH-запросе
// SemanticNeighbors, чтобы компенсировать k-before-filter sqlite-vec (см.
// SemanticNeighbors) — иначе якорь (дистанция 0) вытесняет настоящего соседа.
const semanticNeighborsPad = 5

// vectorTagFilterPad — множитель запаса k, запрашиваемого у sqlite-vec KNN в
// SearchVector, когда активен фильтр Query.Tags (задача 8.2). sqlite-vec обрезает по
// k ДО применения внешних фильтров (тот же k-before-filter, что и в
// SemanticNeighbors выше) — при узком тег-фильтре среди k ближайших по вектору
// могло не оказаться ни одной подходящей по тегу записи, хотя дальше по дистанции
// такие есть. Кратный (не аддитивный, как semanticNeighborsPad) запас — потому что
// здесь k пропорционален произвольному Query.Limit, а не фиксированному k соседей;
// эвристика не даёт точных гарантий покрытия (тег может не встретиться и в
// увеличенном k), но заметно снижает вероятность недобора кандидатов на типичных
// объёмах памяти. Внешний LIMIT ? после WHERE обрезает результат обратно до
// исходного limit.
const vectorTagFilterPad = 5

// baseSchema — DDL хранилища для таблиц метаданных (DESIGN.md §4). Идемпотентен: все
// объекты создаются через IF NOT EXISTS, поэтому Open можно вызывать многократно на
// одном файле. memories_fts сюда НЕ входит — её форма версионируется через механизм
// миграций схемы (см. migrate/migrations ниже, задача 8.3), поскольку она меняется
// между версиями (добавление content_stemmed, а в будущем — возможно другие правки).
const baseSchema = `
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

-- Векторный индекс memories_vec НЕ создаётся здесь: dim (размерность эмбеддинга)
-- неизвестен заранее — она определяется моделью Embedder и подставляется только при
-- первом вызове ResetVectorIndex (см. ниже), который создаёт таблицу через
-- fmt.Sprintf с нужным float[<DIM>]. До первого reindex таблицы memories_vec просто
-- нет — SearchVector/PutVector деградируют соответственно (DESIGN.md §15).

CREATE TABLE IF NOT EXISTS embed_meta (
    id    INTEGER PRIMARY KEY CHECK (id = 1),
    model TEXT NOT NULL,
    dim   INTEGER NOT NULL
);
`

// ftsTriggers синхронизирует memories_fts с memories/tags. Выполняется КАЖДЫЙ Open,
// обязательно ПОСЛЕ migrate() — потому что триггеры ссылаются на memories_fts, а на
// свежей БД её создаёт именно миграция (см. migrateStemmedFTS). Тела триггеров
// синхронизируют content/summary/tags как раньше (задача 8.3 их не меняет) — колонку
// content_stemmed заполняет отдельным UPDATE прикладной код Put (см. ниже): там же,
// где считается normalizeForFTS, естественно и держать её актуальной, не заводя для
// стемминга SQL-функцию.
//
// DROP TRIGGER IF EXISTS + CREATE (а не CREATE TRIGGER IF NOT EXISTS): при миграции
// существующей БД со старой схемой (migrateStemmedFTS переименовывает memories_fts в
// memories_fts_old) SQLite сам переписывает ссылку на таблицу ВНУТРИ уже существующих
// триггеров на новое имя (memories_fts_old) — «просто досоздать при отсутствии» после
// этого оставило бы старые триггеры молча смотрящими на уже удалённую
// memories_fts_old. Безусловный DROP+CREATE каждый Open гарантированно приводит
// триггеры к актуальному определению независимо от того, что с ними сделал rename.
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
const ftsTriggers = `
DROP TRIGGER IF EXISTS memories_ai;
CREATE TRIGGER memories_ai AFTER INSERT ON memories BEGIN
    INSERT INTO memories_fts(rowid, content, summary, tags)
    VALUES (new.rowid, new.content, coalesce(new.summary, ''), '');
END;

DROP TRIGGER IF EXISTS memories_au;
CREATE TRIGGER memories_au AFTER UPDATE ON memories BEGIN
    UPDATE memories_fts SET content = new.content, summary = coalesce(new.summary, '')
    WHERE rowid = new.rowid;
END;

DROP TRIGGER IF EXISTS memories_ad;
CREATE TRIGGER memories_ad AFTER DELETE ON memories BEGIN
    DELETE FROM memories_fts WHERE rowid = old.rowid;
END;

DROP TRIGGER IF EXISTS tags_ai;
CREATE TRIGGER tags_ai AFTER INSERT ON tags BEGIN
    UPDATE memories_fts SET tags = (
        SELECT coalesce(group_concat(tag, ' '), '') FROM tags WHERE memory_id = new.memory_id
    ) WHERE rowid = (SELECT rowid FROM memories WHERE id = new.memory_id);
END;

DROP TRIGGER IF EXISTS tags_ad;
CREATE TRIGGER tags_ad AFTER DELETE ON tags BEGIN
    UPDATE memories_fts SET tags = (
        SELECT coalesce(group_concat(tag, ' '), '') FROM tags WHERE memory_id = old.memory_id
    ) WHERE rowid = (SELECT rowid FROM memories WHERE id = old.memory_id);
END;
`

// createMemoriesFTS — целевая (текущая) форма memories_fts: задача 8.3 добавляет
// content_stemmed к исходным content/summary/tags. Используется и для свежей БД
// (создаёт таблицу напрямую), и как целевая форма при миграции существующей БД со
// старой схемой (см. migrateStemmedFTS).
const createMemoriesFTS = `CREATE VIRTUAL TABLE memories_fts USING fts5(content, summary, tags, content_stemmed)`

// schemaVersion — целевая версия схемы (PRAGMA user_version). Растёт на 1 с каждым
// новым шагом миграции, добавленным в migrations. Задача 8.3 ВВОДИТ сам механизм:
// все существующие на момент введения БД (как и совсем свежие) имеют user_version=0
// (PRAGMA user_version никогда раньше не выставлялась) — поэтому migrations[0]
// обязана идемпотентно привести к целевой форме ОБА случая: и БД, где memories_fts
// ещё вовсе нет (создать сразу в целевом виде), и БД со старой схемой memories_fts
// (content, summary, tags) без content_stemmed (пересобрать и перегнать контент).
// Следующая задача с миграцией (9.4, TTL) добавит migrations[1] и увеличит
// schemaVersion до 2, не трогая этот код.
const schemaVersion = 1

// migrationStep — один шаг миграции схемы: переводит БД с версии i на версию i+1.
// Выполняется целиком в одной транзакции.
type migrationStep func(ctx context.Context, tx *sql.Tx) error

// migrations — упорядоченный список шагов миграции, migrations[i] соответствует
// переходу версии i -> i+1. Длина списка должна совпадать с schemaVersion.
var migrations = []migrationStep{
	migrateStemmedFTS, // 0 -> 1 (задача 8.3): memories_fts + колонка content_stemmed
}

// migrate применяет по порядку шаги migrations, начиная с версии, прочитанной из
// PRAGMA user_version, до schemaVersion. Каждый шаг — в своей транзакции; user_version
// обновляется сразу после успешного шага (а не одной транзакцией на всё) — при обрыве
// между шагами повторный Open продолжит с незавершённого шага, а не переиграет уже
// применённые. Вызывается из Open ПОСЛЕ baseSchema и ДО ftsTriggers.
func (s *sqliteStore) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("чтение user_version: %w", err)
	}

	for version < schemaVersion {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("начало транзакции миграции: %w", err)
		}

		if err := migrations[version](ctx, tx); err != nil {
			tx.Rollback()
			return fmt.Errorf("миграция схемы %d -> %d: %w", version, version+1, err)
		}

		version++
		// PRAGMA не поддерживает bind-параметры в mattn/go-sqlite3 — version посчитан
		// в Go (не из пользовательского ввода), подстановка через Sprintf безопасна.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, version)); err != nil {
			tx.Rollback()
			return fmt.Errorf("обновление user_version до %d: %w", version, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit миграции схемы (версия %d): %w", version, err)
		}
	}
	return nil
}

// ftsTableExists сообщает, создана ли уже виртуальная таблица memories_fts (в этой же
// транзакции — видит незакоммиченные изменения самой миграции).
func ftsTableExists(ctx context.Context, tx *sql.Tx) (bool, error) {
	var name string
	err := tx.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'memories_fts'`).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ftsHasColumn сообщает, объявлена ли у memories_fts колонка column.
func ftsHasColumn(ctx context.Context, tx *sql.Tx, column string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(memories_fts)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, rows.Err()
		}
	}
	return false, rows.Err()
}

// migrateStemmedFTS — миграция 0 -> 1 (задача 8.3). Идемпотентно приводит memories_fts
// к целевой форме fts5(content, summary, tags, content_stemmed):
//   - таблицы ещё нет (свежая БД) — создаёт сразу в целевом виде;
//   - таблица есть, content_stemmed уже есть — ничего не делает (защита от повторного
//     запуска на уже мигрированной БД, если версия схемы почему-то не проставилась);
//   - таблица есть в старой форме (content, summary, tags) — пересобирает: старая
//     таблица переименовывается, создаётся новая, содержимое переносится построчно с
//     доклейкой content_stemmed = normalizeForFTS(content)+" "+normalizeForFTS(summary)
//     (raw content/summary/tags копируются как есть — они не меняются), старая
//     таблица удаляется. Ничего не теряется: количество строк до/после совпадает
//     (проверено тестом на реальном файле старой схемы).
//
// Триггеры (ftsTriggers) не трогаются: их тела не ссылаются на content_stemmed
// (её синхронизацией занимается Put, см. sqliteStore.Put), поэтому их не нужно
// ни менять, ни пересоздавать при миграции.
func migrateStemmedFTS(ctx context.Context, tx *sql.Tx) error {
	exists, err := ftsTableExists(ctx, tx)
	if err != nil {
		return fmt.Errorf("проверка memories_fts: %w", err)
	}
	if !exists {
		if _, err := tx.ExecContext(ctx, createMemoriesFTS); err != nil {
			return fmt.Errorf("создание memories_fts: %w", err)
		}
		return nil
	}

	hasStemmed, err := ftsHasColumn(ctx, tx, "content_stemmed")
	if err != nil {
		return fmt.Errorf("проверка колонки content_stemmed: %w", err)
	}
	if hasStemmed {
		return nil
	}

	if _, err := tx.ExecContext(ctx, `ALTER TABLE memories_fts RENAME TO memories_fts_old`); err != nil {
		return fmt.Errorf("переименование memories_fts: %w", err)
	}
	if _, err := tx.ExecContext(ctx, createMemoriesFTS); err != nil {
		return fmt.Errorf("создание новой memories_fts: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT m.rowid, m.content, m.summary, old.tags
		FROM memories m JOIN memories_fts_old old ON old.rowid = m.rowid
	`)
	if err != nil {
		return fmt.Errorf("чтение старой memories_fts: %w", err)
	}
	type oldRow struct {
		rowid   int64
		content string
		summary sql.NullString
		tags    string
	}
	var oldRows []oldRow
	for rows.Next() {
		var r oldRow
		if err := rows.Scan(&r.rowid, &r.content, &r.summary, &r.tags); err != nil {
			rows.Close()
			return fmt.Errorf("сканирование старой записи fts: %w", err)
		}
		oldRows = append(oldRows, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("чтение старой memories_fts: %w", err)
	}
	rows.Close()

	for _, r := range oldRows {
		stemmed := normalizeForFTS(r.content)
		summary := ""
		if r.summary.Valid {
			summary = r.summary.String
			if s := normalizeForFTS(summary); s != "" {
				stemmed += " " + s
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO memories_fts(rowid, content, summary, tags, content_stemmed) VALUES (?, ?, ?, ?, ?)
		`, r.rowid, r.content, summary, r.tags, stemmed); err != nil {
			return fmt.Errorf("перенос записи fts rowid=%d: %w", r.rowid, err)
		}
	}

	if _, err := tx.ExecContext(ctx, `DROP TABLE memories_fts_old`); err != nil {
		return fmt.Errorf("удаление memories_fts_old: %w", err)
	}
	return nil
}

// sqliteStore — реализация Store поверх SQLite (метаданные + FTS5 + sqlite-vec).
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

	// Регистрирует расширение sqlite-vec (vec0) для всех новых соединений этого
	// процесса. Должно быть вызвано до sql.Open. Идемпотентно.
	sqlite_vec.Auto()

	dsn := fmt.Sprintf("file:%s?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000", expanded)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("открытие sqlite %q: %w", expanded, err)
	}

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("проверка соединения sqlite %q: %w", expanded, err)
	}

	if _, err := db.Exec(baseSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("применение схемы: %w", err)
	}

	store := &sqliteStore{db: db, now: defaultNow}
	if err := store.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("миграция схемы: %w", err)
	}

	if _, err := db.Exec(ftsTriggers); err != nil {
		db.Close()
		return nil, fmt.Errorf("применение триггеров fts: %w", err)
	}

	return store, nil
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

	// content_stemmed (задача 8.3) синхронизируется здесь прикладным кодом, а не
	// триггером: тело триггера memories_ai/au не умеет звать Go-стеммер
	// (потребовалась бы регистрация кастомной SQL-функции через ConnectHook — лишняя
	// сложность ради того, что и так естественно посчитать тут же, рядом с Put).
	// Тем же normalizeForFTS индексируется и content, и summary — оба относятся к
	// одной записи и должны находиться одним и тем же MATCH-запросом.
	stemmed := normalizeForFTS(m.Content)
	if m.Summary != nil {
		if s := normalizeForFTS(*m.Summary); s != "" {
			stemmed += " " + s
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE memories_fts SET content_stemmed = ?
		WHERE rowid = (SELECT rowid FROM memories WHERE id = ?)
	`, stemmed, m.Id); err != nil {
		return fmt.Errorf("обновление content_stemmed: %w", err)
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

// loadTagsBatch грузит теги сразу для набора id одним запросом (memory_id IN (...))
// — в отличие от loadTags (одна запись), нужна там, где потребителю отдаётся
// страница записей и построчная загрузка была бы N+1 (List). Читает внутри той же
// транзакции, что и страница List, — для консистентности пары (items, total).
// Возвращает map с пустым срезом отсутствующим — вызывающая сторона обращается к ней
// по id и получает nil для записей без тегов, что для memory.Memory.Tags эквивалентно.
func (s *sqliteStore) loadTagsBatch(ctx context.Context, tx *sql.Tx, ids []string) (map[string][]string, error) {
	result := make(map[string][]string, len(ids))
	if len(ids) == 0 {
		return result, nil
	}

	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT memory_id, tag FROM tags WHERE memory_id IN (`+strings.Join(placeholders, ",")+`)
		ORDER BY memory_id, tag
	`, args...)
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

// scopePredicate строит условие фильтра по scope (DESIGN.md §2.1) для использования в
// SearchLexical и (в дальнейшем, Этап 3.2) SearchVector — единая точка, чтобы оба
// источника поиска фильтровали scope одинаково.
//
// Приоритет: набор scopes (m.scope IN (...)) > одиночный scope (m.scope = ?) > без
// фильтра (пустой clause), если оба параметра пусты.
func scopePredicate(scopes []string, scope string) (clause string, args []any) {
	if len(scopes) > 0 {
		placeholders := make([]string, len(scopes))
		args = make([]any, len(scopes))
		for i, s := range scopes {
			placeholders[i] = "?"
			args[i] = s
		}
		return "m.scope IN (" + strings.Join(placeholders, ",") + ")", args
	}
	if scope != "" {
		return "m.scope = ?", []any{scope}
	}
	return "", nil
}

// kindPredicate строит условие фильтра по kind (m.kind IN (...)) — единая точка для
// metaFilters (recall) и List (browse-морда), чтобы предикат не дублировался. Пустой
// набор kinds → пустой clause (без фильтра по типу).
func kindPredicate(kinds []string) (clause string, args []any) {
	if len(kinds) == 0 {
		return "", nil
	}
	placeholders := make([]string, len(kinds))
	args = make([]any, len(kinds))
	for i, k := range kinds {
		placeholders[i] = "?"
		args[i] = k
	}
	return "m.kind IN (" + strings.Join(placeholders, ",") + ")", args
}

// tagsPredicate строит условие "хотя бы один из тегов" (OR-семантика, Query.Tags,
// задача 8.2) через EXISTS-подзапрос к таблице tags — единая точка для metaFilters,
// как и scopePredicate/kindPredicate, чтобы SearchLexical и SearchVector фильтровали
// по тегам одинаково (DESIGN.md §8). EXISTS, а не JOIN — запись не должна
// задваиваться, если совпало сразу несколько перечисленных тегов. Пустой набор tags
// → пустой clause (без фильтра).
func tagsPredicate(tags []string) (clause string, args []any) {
	if len(tags) == 0 {
		return "", nil
	}
	placeholders := make([]string, len(tags))
	args = make([]any, len(tags))
	for i, t := range tags {
		placeholders[i] = "?"
		args[i] = t
	}
	return "EXISTS (SELECT 1 FROM tags t WHERE t.memory_id = m.id AND t.tag IN (" + strings.Join(placeholders, ",") + "))", args
}

// metaFilters строит общие для SearchLexical и SearchVector фильтры-предикаты по полям
// Query — кроме полнотекстового/векторного MATCH: мягкое забывание (superseded_by IS
// NULL), scope (через scopePredicate), kind, свежесть (since), теги (OR, задача 8.2),
// дата создания (CreatedAfter/CreatedBefore, задача 8.2). Единая точка гарантирует,
// что лексический и векторный источники recall отбирают кандидатов одинаково (DESIGN.md
// §8) — новый фильтр добавляется в одном месте, а не синхронно в двух.
//
// conds и args возвращаются в lockstep: порядок ? в conds совпадает с порядком args,
// поэтому вызывающая сторона может дописать свои conds/args (MATCH) до или после.
func metaFilters(q memory.Query) (conds []string, args []any) {
	conds = append(conds, "m.superseded_by IS NULL")
	if clause, scopeArgs := scopePredicate(q.Scopes, q.Scope); clause != "" {
		conds = append(conds, clause)
		args = append(args, scopeArgs...)
	}
	if clause, kindArgs := kindPredicate(q.Kinds); clause != "" {
		conds = append(conds, clause)
		args = append(args, kindArgs...)
	}
	if q.SinceTs > 0 {
		conds = append(conds, "m.created_at >= ?")
		args = append(args, q.SinceTs)
	}
	if clause, tagArgs := tagsPredicate(q.Tags); clause != "" {
		conds = append(conds, clause)
		args = append(args, tagArgs...)
	}
	if q.CreatedAfter > 0 {
		conds = append(conds, "m.created_at >= ?")
		args = append(args, q.CreatedAfter)
	}
	if q.CreatedBefore > 0 {
		conds = append(conds, "m.created_at <= ?")
		args = append(args, q.CreatedBefore)
	}
	return conds, args
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
	conds, args := metaFilters(q)

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
//
// Задача 8.3: на каждое слово запроса ДОПОЛНИТЕЛЬНО (тем же OR) добавляется его
// normalizeForFTS-форма — для кириллических слов это стеммированная основа (находит
// content_stemmed при несовпадении точной словоформы с content: "проекта" ->
// добавляется термин "проект", который матчит "проект"/"проекты"/"проектом" и т.д.),
// для остального (латиница, идентификаторы, пути) normalizeForFTS возвращает слово
// без изменений — дубль отбрасывается (дедуп по regarded-как-lower ключу), поведение
// для `sqlite_fts5`/`make build` не меняется. Термины без column-префикса (col:term)
// сознательно не используются — bare MATCH и так ищет по всем колонкам таблицы
// (content/summary/tags/content_stemmed), поэтому лишнее квалифицирование не нужно.
func sanitizeFTSQuery(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	seen := make(map[string]bool, len(fields)*2)
	parts := make([]string, 0, len(fields)*2)
	addTerm := func(term string) {
		if term == "" {
			return
		}
		key := strings.ToLower(term)
		if seen[key] {
			return
		}
		seen[key] = true
		escaped := strings.ReplaceAll(term, `"`, `""`)
		parts = append(parts, `"`+escaped+`"`)
	}
	for _, f := range fields {
		addTerm(f)
		addTerm(normalizeForFTS(f))
	}
	return strings.Join(parts, " OR ")
}

// SearchVector ищет по векторному индексу memories_vec (sqlite-vec) методом KNN и
// применяет те же фильтры, что и SearchLexical: superseded_by IS NULL, scope через
// общий scopePredicate (набор Query.Scopes), kind, since. Score — 1/(1+distance)
// (больше значит ближе), Source = "vector", порядок — по возрастанию distance
// (ближайшие первыми).
//
// vec0 не позволяет джойнить MATCH+k с фильтрами по другой таблице в одном SELECT —
// KNN выполняется в CTE, фильтры по memories накладываются во внешнем запросе.
//
// Если memories_vec ещё не создана (Embedder выключен, ResetVectorIndex ни разу не
// вызывался), возвращает (nil, nil) без ошибки — деградация до чистого FTS5, а не
// падение recall (DESIGN.md §15).
//
// k-before-filter при активном Query.Tags (задача 8.2): в MATCH запрашивается
// limit*vectorTagFilterPad вместо limit (см. константу), а внешний LIMIT ? обрезает
// уже отфильтрованный результат обратно до исходного limit — иначе увеличенный запас
// вернул бы больше кандидатов, чем просил вызывающий код.
//
// Как и SearchLexical, Tags кандидатов не заполняются (read-hot-path, см. коммент там).
func (s *sqliteStore) SearchVector(ctx context.Context, vec []float32, q memory.Query) ([]memory.Candidate, error) {
	// Пустой вектор запроса — не с чем сравнивать: деградируем до пустого векторного
	// источника, а не роняем recall на dimension mismatch в MATCH (DESIGN.md §15).
	// recall передаёт nil, пока векторный запрос не подключён (Этап 3.4); дешёвая
	// проверка до обращения к БД.
	if len(vec) == 0 {
		return nil, nil
	}

	exists, err := s.hasVecTable(ctx)
	if err != nil {
		return nil, fmt.Errorf("проверка memories_vec: %w", err)
	}
	if !exists {
		return nil, nil
	}

	blob, err := sqlite_vec.SerializeFloat32(vec)
	if err != nil {
		return nil, fmt.Errorf("сериализация вектора запроса: %w", err)
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	knnK := limit
	if len(q.Tags) > 0 {
		knnK = limit * vectorTagFilterPad
	}

	conds, args := metaFilters(q)

	query := `
		WITH knn AS (
			SELECT memory_id, distance FROM memories_vec WHERE embedding MATCH ? AND k = ?
		)
		SELECT m.id, m.kind, m.scope, m.content, m.summary, m.importance, m.created_at, m.updated_at, m.accessed_at, m.access_count, m.superseded_by, m.source, knn.distance
		FROM knn JOIN memories m ON m.id = knn.memory_id
		WHERE ` + strings.Join(conds, " AND ") + `
		ORDER BY knn.distance ASC
		LIMIT ?
	`
	queryArgs := append([]any{blob, knnK}, args...)
	queryArgs = append(queryArgs, limit)

	rows, err := s.db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("векторный поиск: %w", err)
	}
	defer rows.Close()

	var candidates []memory.Candidate
	for rows.Next() {
		var distance float64
		m, err := scanMemory(rowWithExtra{row: rows, extra: []any{&distance}})
		if err != nil {
			return nil, fmt.Errorf("сканирование кандидата: %w", err)
		}
		candidates = append(candidates, memory.Candidate{Memory: m, Score: 1.0 / (1.0 + distance), Source: "vector"})
	}
	return candidates, rows.Err()
}

// hasVecTable сообщает, создана ли виртуальная таблица memories_vec. Она не входит
// в статическую schema (dim неизвестен заранее) — появляется только после первого
// ResetVectorIndex.
func (s *sqliteStore) hasVecTable(ctx context.Context) (bool, error) {
	var name string
	err := s.db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'memories_vec'`).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("чтение sqlite_master: %w", err)
	}
	return true, nil
}

// SemanticNeighbors возвращает до k ближайших по cosine-дистанции живых записей к
// записи id (исключая её саму и superseded), отсортированных по возрастанию
// дистанции (DESIGN.md §12, задача 6.4). Candidate.Score = distance (честная
// дистанция — карточке морды нужно показать её как есть, не 1/(1+d)),
// Candidate.Source = "vector". Tags/Links не заполняются, как в SearchVector.
//
// KNN выполняется через CTE, как в SearchVector, но вектор запроса берётся из
// самой записи id (MATCH (SELECT embedding FROM memories_vec WHERE memory_id = ?)).
//
// k-before-filter: vec0 применяет k ДО внешних фильтров (self, superseded) —
// известное ограничение sqlite-vec. Поэтому запрашиваем k+запас (semanticNeighborsPad)
// в MATCH, затем в Go отфильтровываем саму запись и superseded, и обрезаем до k. Без
// запаса якорь (дистанция 0, всегда попадает в top-k) вытеснил бы одного настоящего
// соседа.
//
// Если k<=0, у записи id нет вектора, либо memories_vec ещё не создана — возвращает
// (nil, nil) без ошибки (деградация DESIGN.md §15).
func (s *sqliteStore) SemanticNeighbors(ctx context.Context, id string, k int) ([]memory.Candidate, error) {
	if k <= 0 {
		return nil, nil
	}

	exists, err := s.hasVecTable(ctx)
	if err != nil {
		return nil, fmt.Errorf("проверка memories_vec: %w", err)
	}
	if !exists {
		return nil, nil
	}

	var hasVector bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM memories_vec WHERE memory_id = ?)`, id).Scan(&hasVector); err != nil {
		return nil, fmt.Errorf("проверка вектора %q: %w", id, err)
	}
	if !hasVector {
		return nil, nil
	}

	query := `
		WITH knn AS (
			SELECT memory_id, distance FROM memories_vec
			WHERE embedding MATCH (SELECT embedding FROM memories_vec WHERE memory_id = ?) AND k = ?
		)
		SELECT m.id, m.kind, m.scope, m.content, m.summary, m.importance, m.created_at, m.updated_at, m.accessed_at, m.access_count, m.superseded_by, m.source, knn.distance
		FROM knn JOIN memories m ON m.id = knn.memory_id
		ORDER BY knn.distance ASC
	`
	rows, err := s.db.QueryContext(ctx, query, id, k+semanticNeighborsPad)
	if err != nil {
		return nil, fmt.Errorf("поиск семантических соседей %q: %w", id, err)
	}
	defer rows.Close()

	var candidates []memory.Candidate
	for rows.Next() {
		var distance float64
		m, err := scanMemory(rowWithExtra{row: rows, extra: []any{&distance}})
		if err != nil {
			return nil, fmt.Errorf("сканирование семантического соседа %q: %w", id, err)
		}
		if m.Id == id || m.SupersededBy != nil {
			continue
		}
		candidates = append(candidates, memory.Candidate{Memory: m, Score: distance, Source: "vector"})
		if len(candidates) == k {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("чтение семантических соседей %q: %w", id, err)
	}
	return candidates, nil
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

// List возвращает страницу записей под фильтр f (для browse-морды, DESIGN.md §12) и
// total — общее число записей под тот же фильтр без учёта Limit/Offset. items и
// total читаются в одной транзакции (на одном соединении), чтобы пара оставалась
// консистентной под конкурентной записью. Tags грузятся батчем на всю страницу
// (loadTagsBatch) — не N+1; Links не грузятся (детальная карточка берёт их Get).
func (s *sqliteStore) List(ctx context.Context, f BrowseFilter) ([]memory.Memory, int, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = listDefaultLimit
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	var conds []string
	var args []any
	if !f.IncludeSuperseded {
		conds = append(conds, "m.superseded_by IS NULL")
	}
	if clause, scopeArgs := scopePredicate(f.Scopes, ""); clause != "" {
		conds = append(conds, clause)
		args = append(args, scopeArgs...)
	}
	if clause, kindArgs := kindPredicate(f.Kinds); clause != "" {
		conds = append(conds, clause)
		args = append(args, kindArgs...)
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
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM memories m"+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("подсчёт total для List: %w", err)
	}

	query := `
		SELECT m.id, m.kind, m.scope, m.content, m.summary, m.importance, m.created_at, m.updated_at, m.accessed_at, m.access_count, m.superseded_by, m.source
		FROM memories m` + where + `
		ORDER BY m.created_at DESC, m.id DESC
		LIMIT ? OFFSET ?
	`
	pageArgs := append(append([]any{}, args...), limit, offset)

	rows, err := tx.QueryContext(ctx, query, pageArgs...)
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

// Stats возвращает сводные счётчики памяти (DESIGN.md §12): всего живых и архивных
// записей, разбивки живых записей по scope и kind. Пустые карты инициализируются, а
// не остаются nil — потребителю (морде) не нужно проверять nil перед обходом.
func (s *sqliteStore) Stats(ctx context.Context) (Stats, error) {
	stats := Stats{ByScope: map[string]int{}, ByKind: map[string]int{}}

	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories WHERE superseded_by IS NULL`).Scan(&stats.Total); err != nil {
		return Stats{}, fmt.Errorf("подсчёт живых записей: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories WHERE superseded_by IS NOT NULL`).Scan(&stats.Superseded); err != nil {
		return Stats{}, fmt.Errorf("подсчёт архивных записей: %w", err)
	}

	scopeRows, err := s.db.QueryContext(ctx, `SELECT scope, COUNT(*) FROM memories WHERE superseded_by IS NULL GROUP BY scope`)
	if err != nil {
		return Stats{}, fmt.Errorf("разбивка по scope: %w", err)
	}
	defer scopeRows.Close()
	for scopeRows.Next() {
		var scope string
		var n int
		if err := scopeRows.Scan(&scope, &n); err != nil {
			return Stats{}, fmt.Errorf("сканирование разбивки по scope: %w", err)
		}
		stats.ByScope[scope] = n
	}
	if err := scopeRows.Err(); err != nil {
		return Stats{}, fmt.Errorf("чтение разбивки по scope: %w", err)
	}

	kindRows, err := s.db.QueryContext(ctx, `SELECT kind, COUNT(*) FROM memories WHERE superseded_by IS NULL GROUP BY kind`)
	if err != nil {
		return Stats{}, fmt.Errorf("разбивка по kind: %w", err)
	}
	defer kindRows.Close()
	for kindRows.Next() {
		var kind string
		var n int
		if err := kindRows.Scan(&kind, &n); err != nil {
			return Stats{}, fmt.Errorf("сканирование разбивки по kind: %w", err)
		}
		stats.ByKind[kind] = n
	}
	if err := kindRows.Err(); err != nil {
		return Stats{}, fmt.Errorf("чтение разбивки по kind: %w", err)
	}

	return stats, nil
}

// Health возвращает быстрый read-only снимок целостности БД (DESIGN.md §15,
// задача 9.1 `memoryd doctor`): `PRAGMA quick_check` (быстрая проверка — в
// отличие от `PRAGMA integrity_check`, не сканирует индексы полностью, не
// заявлена дорогой операцией на больших БД) и текущий `PRAGMA journal_mode`.
// Обе PRAGMA — чтение, ничего не пишут в файл БД.
func (s *sqliteStore) Health(ctx context.Context) (HealthInfo, error) {
	var quickCheck string
	if err := s.db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&quickCheck); err != nil {
		return HealthInfo{}, fmt.Errorf("quick_check: %w", err)
	}

	var journalMode string
	if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		return HealthInfo{}, fmt.Errorf("journal_mode: %w", err)
	}

	return HealthInfo{QuickCheck: quickCheck, JournalMode: journalMode}, nil
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

// VectorStatus возвращает состояние вектора записи id (DESIGN.md §12, задача 6.4).
// Если memories_vec ещё не создана (Embedder всегда был noop), возвращает нулевой
// VectorStatus{} без ошибки — деградация, а не падение (DESIGN.md §15). Иначе
// проверяет наличие строки в memories_vec и читает embed_meta (EmbedMeta) для
// модели/dim.
func (s *sqliteStore) VectorStatus(ctx context.Context, id string) (VectorStatus, error) {
	exists, err := s.hasVecTable(ctx)
	if err != nil {
		return VectorStatus{}, fmt.Errorf("проверка memories_vec: %w", err)
	}
	if !exists {
		return VectorStatus{}, nil
	}

	var embedded bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM memories_vec WHERE memory_id = ?)`, id).Scan(&embedded); err != nil {
		return VectorStatus{}, fmt.Errorf("проверка вектора %q: %w", id, err)
	}

	model, dim, err := s.EmbedMeta(ctx)
	if err != nil {
		return VectorStatus{}, fmt.Errorf("чтение embed_meta для статуса %q: %w", id, err)
	}

	return VectorStatus{Embedded: embedded, Model: model, Dim: dim}, nil
}

// ResetVectorIndex пересоздаёт векторный индекс memories_vec под новую модель/dim и
// фиксирует их в embed_meta — источнике истины для reindex (DESIGN.md §9). Вызывается
// при первом включении Embedder или при смене модели (dim/пространство меняются).
// Таблица пересоздаётся с нуля (DROP + CREATE) — старые векторы теряются; вызывающая
// сторона (reindex-джоб) обязана после этого пересчитать эмбеддинги через
// AllForReindex + PutVector. В транзакции.
func (s *sqliteStore) ResetVectorIndex(ctx context.Context, model string, dim int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("начало транзакции: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS memories_vec`); err != nil {
		return fmt.Errorf("удаление memories_vec: %w", err)
	}

	// dim приходит из кода (Embedder.Dim()), не из пользовательского ввода — безопасно
	// подставлять напрямую в DDL, sqlite-vec не поддерживает параметризацию типа колонки.
	createStmt := fmt.Sprintf(
		`CREATE VIRTUAL TABLE memories_vec USING vec0(memory_id TEXT PRIMARY KEY, embedding float[%d] distance_metric=cosine)`,
		dim,
	)
	if _, err := tx.ExecContext(ctx, createStmt); err != nil {
		return fmt.Errorf("создание memories_vec: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO embed_meta (id, model, dim) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET model = excluded.model, dim = excluded.dim
	`, model, dim); err != nil {
		return fmt.Errorf("обновление embed_meta: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// AllForReindex возвращает все не устаревшие (superseded_by IS NULL) записи вместе с
// tags/links — источник для фонового reindex-джоба при смене модели Embedder
// (DESIGN.md §9). Устаревшие записи не нужны новому вектору: они уже вне recall.
func (s *sqliteStore) AllForReindex(ctx context.Context) ([]memory.Memory, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, kind, scope, content, summary, importance, created_at, updated_at, accessed_at, access_count, superseded_by, source
		FROM memories
		WHERE superseded_by IS NULL
	`)
	if err != nil {
		return nil, fmt.Errorf("чтение записей для reindex: %w", err)
	}
	defer rows.Close()

	var all []memory.Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, fmt.Errorf("сканирование записи для reindex: %w", err)
		}
		if m.Tags, err = s.loadTags(ctx, m.Id); err != nil {
			return nil, err
		}
		if m.Links, err = s.loadLinks(ctx, m.Id); err != nil {
			return nil, err
		}
		all = append(all, m)
	}
	return all, rows.Err()
}

// PutVector сохраняет (или заменяет) вектор записи id в memories_vec. Требует, чтобы
// индекс уже был создан ResetVectorIndex — иначе возвращает внятную ошибку (а не
// молча теряет вектор): reindex-джоб обязан вызвать ResetVectorIndex до первого
// PutVector.
//
// Замена реализована как DELETE + INSERT в транзакции, а не `INSERT OR REPLACE`:
// виртуальная таблица vec0 (sqlite-vec) не поддерживает конфликт-резолюцию `OR
// REPLACE` по своему PRIMARY KEY и возвращает "UNIQUE constraint failed" при
// повторной вставке существующего memory_id — это нужно write.Writer.Update,
// который пересчитывает и перезаписывает вектор при изменении content.
func (s *sqliteStore) PutVector(ctx context.Context, id string, vec []float32) error {
	blob, err := sqlite_vec.SerializeFloat32(vec)
	if err != nil {
		return fmt.Errorf("сериализация вектора %q: %w", id, err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("начало транзакции: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM memories_vec WHERE memory_id = ?`, id); err != nil {
		return fmt.Errorf("удаление старого вектора %q: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO memories_vec(memory_id, embedding) VALUES (?, ?)`, id, blob); err != nil {
		return fmt.Errorf("запись вектора %q: %w", id, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ListMissingVectors возвращает не устаревшие (superseded_by IS NULL) записи без
// строки в memories_vec — очередь на досчёт вектора фоновым джобом, когда Save/Update
// отложили эмбеддинг из-за временной недоступности Embedder (DESIGN.md §7.4, §9:
// «очередь» — это просто записи без строки в memories_vec, отдельная таблица не
// нужна). Если memories_vec ещё не создана (Embedder ни разу не включался,
// ResetVectorIndex не вызывался), возвращает пустой список без ошибки — деградация,
// а не падение (DESIGN.md §15).
//
// Как и AllForReindex/ListEpisodicForConsolidation, Tags/Links не подгружаются —
// потребителю (реindex-джобу) для расчёта эмбеддинга нужен только Content.
func (s *sqliteStore) ListMissingVectors(ctx context.Context, limit int) ([]memory.Memory, error) {
	exists, err := s.hasVecTable(ctx)
	if err != nil {
		return nil, fmt.Errorf("проверка memories_vec: %w", err)
	}
	if !exists {
		return nil, nil
	}

	query := `
		SELECT m.id, m.kind, m.scope, m.content, m.summary, m.importance, m.created_at, m.updated_at, m.accessed_at, m.access_count, m.superseded_by, m.source
		FROM memories m
		LEFT JOIN memories_vec v ON v.memory_id = m.id
		WHERE m.superseded_by IS NULL AND v.memory_id IS NULL
		ORDER BY m.created_at
	`
	var args []any
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("выборка записей без вектора: %w", err)
	}
	defer rows.Close()

	var missing []memory.Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, fmt.Errorf("сканирование записи без вектора: %w", err)
		}
		missing = append(missing, m)
	}
	return missing, rows.Err()
}

// ListEpisodicForConsolidation возвращает episodic-записи старше olderThan без
// superseded_by, отсортированные по scope (для группировки consolidation-джобом),
// затем по created_at (DESIGN.md §11). limit <= 0 означает без ограничения.
//
// В отличие от других выборок Store, здесь НЕ подгружаются Tags/Links (они остаются
// nil) — потребитель (maint.Consolidator) использует только Id/Content/Scope, а
// подгрузка тегов/связей на каждую запись была бы N+1 запросом без пользы.
func (s *sqliteStore) ListEpisodicForConsolidation(ctx context.Context, olderThan int64, limit int) ([]memory.Memory, error) {
	query := `
		SELECT id, kind, scope, content, summary, importance, created_at, updated_at, accessed_at, access_count, superseded_by, source
		FROM memories
		WHERE kind = ? AND superseded_by IS NULL AND created_at < ?
		ORDER BY scope, created_at
	`
	args := []any{string(memory.KindEpisodic), olderThan}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("выборка episodic для консолидации: %w", err)
	}
	defer rows.Close()

	var episodes []memory.Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, fmt.Errorf("сканирование episodic для консолидации: %w", err)
		}
		episodes = append(episodes, m)
	}
	return episodes, rows.Err()
}

// ListEpisodicForDecay возвращает episodic-записи без superseded_by, у которых
// COALESCE(accessed_at, created_at) < notAccessedSince — т.е. с момента последнего
// обращения (а если обращений не было — с момента создания) прошло достаточно
// времени (DESIGN.md §11). Отсортированы по той же дате по возрастанию (старые
// первыми), limit <= 0 означает без ограничения.
//
// Как и ListEpisodicForConsolidation, Tags/Links здесь не подгружаются — потребитель
// (maint.Decayer) использует только Id/Importance/CreatedAt/AccessedAt.
func (s *sqliteStore) ListEpisodicForDecay(ctx context.Context, notAccessedSince int64, limit int) ([]memory.Memory, error) {
	query := `
		SELECT id, kind, scope, content, summary, importance, created_at, updated_at, accessed_at, access_count, superseded_by, source
		FROM memories
		WHERE kind = ? AND superseded_by IS NULL AND COALESCE(accessed_at, created_at) < ?
		ORDER BY COALESCE(accessed_at, created_at)
	`
	args := []any{string(memory.KindEpisodic), notAccessedSince}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("выборка episodic для decay: %w", err)
	}
	defer rows.Close()

	var episodes []memory.Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, fmt.Errorf("сканирование episodic для decay: %w", err)
		}
		episodes = append(episodes, m)
	}
	return episodes, rows.Err()
}
