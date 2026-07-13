package store

import (
	"context"
	"database/sql"
	"testing"

	"agentmemory/internal/memory"
)

// TestSearchLexicalStemmingFindsRephrasing проверяет ядро задачи 8.3: перефразировка
// другой словоформой (падеж/число) находит запись лексически, embedder=noop (без
// вектора) — таблица пар из спайка (см. tasks/8.3-fts-russian-morphology.md, «Решение»).
func TestSearchLexicalStemmingFindsRephrasing(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	id := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: id, Kind: memory.KindSemantic, Scope: "project:agentmemory",
		Content: "Проект использует хранилище SQLite, тесты гоняются через make test", Importance: 4,
		CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Ни одно из этих слов НЕ встречается в content буквально в такой словоформе —
	// находятся только через content_stemmed.
	for _, q := range []string{"проектом", "хранилища", "тестами"} {
		t.Run(q, func(t *testing.T) {
			cands, err := s.SearchLexical(ctx, memory.Query{Text: q, Scope: "project:agentmemory", Limit: 10})
			if err != nil {
				t.Fatalf("SearchLexical(%q): %v", q, err)
			}
			found := false
			for _, c := range cands {
				if c.Memory.Id == id {
					found = true
				}
			}
			if !found {
				t.Errorf("SearchLexical(%q) не нашёл запись по стеммированной форме (кандидаты: %+v)", q, cands)
			}
		})
	}
}

// TestSearchLexicalStemmingRegression — англ. идентификаторы/точные фразы находятся
// как раньше (задача 8.3 не должна ломать этот путь).
func TestSearchLexicalStemmingRegression(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	id := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: id, Kind: memory.KindProcedural, Scope: "project:agentmemory",
		Content: "CGO_ENABLED=1 go build -tags sqlite_fts5 ./...; make build; SearchLexical живёт в store", Importance: 3,
		CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	for _, q := range []string{"sqlite_fts5", "make build", "SearchLexical"} {
		t.Run(q, func(t *testing.T) {
			cands, err := s.SearchLexical(ctx, memory.Query{Text: q, Scope: "project:agentmemory", Limit: 10})
			if err != nil {
				t.Fatalf("SearchLexical(%q): %v", q, err)
			}
			if len(cands) != 1 || cands[0].Memory.Id != id {
				t.Errorf("SearchLexical(%q) = %+v, want ровно [id] (регрессия точного токена)", q, cands)
			}
		})
	}
}

// TestSearchLexicalStemmingDerivationBoundary — негативный контроль (задача 8.3,
// раздел «Решение»): «собирать»/«сборка» и «компилировать»/«сборка» — разные лексемы
// (словообразование, не словоизменение), лёгкий стеммер их НЕ мостит. Запись содержит
// только «Сборка» (без «собирать»/«компилировать») — запросы этими словами не должны
// её находить. Граница задокументирована явно, а не тихо пропущена.
func TestSearchLexicalStemmingDerivationBoundary(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	id := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: id, Kind: memory.KindProcedural, Scope: "project:agentmemory",
		Content: "Сборка требует CGO и build-тег для FTS5", Importance: 3,
		CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	for _, q := range []string{"собирать", "компилировать"} {
		t.Run(q, func(t *testing.T) {
			cands, err := s.SearchLexical(ctx, memory.Query{Text: q, Scope: "project:agentmemory", Limit: 10})
			if err != nil {
				t.Fatalf("SearchLexical(%q): %v", q, err)
			}
			for _, c := range cands {
				if c.Memory.Id == id {
					t.Errorf("SearchLexical(%q) нашёл запись про «Сборка» — граница задачи 8.3 предполагает, что деривационная пара НЕ мостится", q)
				}
			}
		})
	}
}

// legacyFTSSchema — DDL memories_fts ДО задачи 8.3 (fts5(content, summary, tags), без
// content_stemmed, PRAGMA user_version никогда не выставлялась = 0). Используется
// только тестом миграции — воспроизводит реальный файл со старой схемой.
const legacyFTSSchema = `
CREATE TABLE memories (
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
CREATE TABLE tags (
    memory_id TEXT NOT NULL REFERENCES memories(id),
    tag       TEXT NOT NULL,
    PRIMARY KEY (memory_id, tag)
);
CREATE TABLE links (
    src TEXT NOT NULL REFERENCES memories(id),
    dst TEXT NOT NULL REFERENCES memories(id),
    rel TEXT NOT NULL DEFAULT 'related',
    PRIMARY KEY (src, dst, rel)
);
CREATE VIRTUAL TABLE memories_fts USING fts5(content, summary, tags);
CREATE TRIGGER memories_ai AFTER INSERT ON memories BEGIN
    INSERT INTO memories_fts(rowid, content, summary, tags)
    VALUES (new.rowid, new.content, coalesce(new.summary, ''), '');
END;
CREATE TRIGGER memories_au AFTER UPDATE ON memories BEGIN
    UPDATE memories_fts SET content = new.content, summary = coalesce(new.summary, '')
    WHERE rowid = new.rowid;
END;
CREATE TRIGGER memories_ad AFTER DELETE ON memories BEGIN
    DELETE FROM memories_fts WHERE rowid = old.rowid;
END;
CREATE TABLE embed_meta (
    id    INTEGER PRIMARY KEY CHECK (id = 1),
    model TEXT NOT NULL,
    dim   INTEGER NOT NULL
);
`

// TestMigrateStemmedFTSPreservesExistingData воспроизводит реальный файл со старой
// схемой (до введения content_stemmed/PRAGMA user_version, задача 8.3) и проверяет,
// что Open мигрирует его без потери записей: количество строк memories_fts до/после
// совпадает, content_stemmed заполняется, а старые словоформы продолжают находиться
// точным токеном (регрессия).
func TestMigrateStemmedFTSPreservesExistingData(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/legacy.db"

	// Строим файл СТАРОЙ схемы напрямую через database/sql (не через store.Open —
	// он уже мигрирует), как реальная БД, созданная кодом до задачи 8.3.
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(legacyFTSSchema); err != nil {
		t.Fatalf("применение legacy-схемы: %v", err)
	}
	ids := []string{newID(t), newID(t), newID(t)}
	contents := []string{
		"Проект на Go использует SQLite и FTS5",
		"Сборка через make build, тесты через make test",
		"Третья запись без ничего особенного",
	}
	for i, id := range ids {
		if _, err := raw.Exec(`INSERT INTO memories (id, kind, scope, content, importance, created_at, updated_at) VALUES (?, 'semantic', 'project:agentmemory', ?, 3, 1, 1)`, id, contents[i]); err != nil {
			t.Fatalf("insert legacy memory %d: %v", i, err)
		}
	}
	var beforeCount int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM memories_fts`).Scan(&beforeCount); err != nil {
		t.Fatalf("count legacy memories_fts: %v", err)
	}
	if beforeCount != len(ids) {
		t.Fatalf("beforeCount = %d, want %d", beforeCount, len(ids))
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	// Open — тот же путь реального кода: baseSchema (idempotent) -> migrate -> ftsTriggers.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open (миграция): %v", err)
	}
	t.Cleanup(func() { s.Close() })

	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("чтение user_version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("user_version = %d, want %d (schemaVersion)", version, schemaVersion)
	}

	hasStemmed, err := ftsHasColumnPublic(s.db, ctx)
	if err != nil {
		t.Fatalf("проверка content_stemmed: %v", err)
	}
	if !hasStemmed {
		t.Fatal("content_stemmed отсутствует после миграции")
	}

	var afterCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories_fts`).Scan(&afterCount); err != nil {
		t.Fatalf("count memories_fts после миграции: %v", err)
	}
	if afterCount != beforeCount {
		t.Errorf("count memories_fts после миграции = %d, want %d (ничего не потеряно)", afterCount, beforeCount)
	}

	// Регрессия: точный токен из старого контента продолжает находиться.
	cands, err := s.SearchLexical(ctx, memory.Query{Text: "SQLite", Scope: "project:agentmemory", Limit: 10})
	if err != nil {
		t.Fatalf("SearchLexical после миграции: %v", err)
	}
	if len(cands) != 1 || cands[0].Memory.Id != ids[0] {
		t.Errorf("SearchLexical(SQLite) после миграции = %+v, want [ids[0]]", cands)
	}

	// Новое: словоизменённая перефразировка находится через мигрированный
	// content_stemmed (данные были ДО задачи 8.3, но нормализованы при миграции).
	cands, err = s.SearchLexical(ctx, memory.Query{Text: "тестами", Scope: "project:agentmemory", Limit: 10})
	if err != nil {
		t.Fatalf("SearchLexical(тестами) после миграции: %v", err)
	}
	found := false
	for _, c := range cands {
		if c.Memory.Id == ids[1] {
			found = true
		}
	}
	if !found {
		t.Errorf("SearchLexical(тестами) после миграции не нашёл запись с «тесты» (%+v)", cands)
	}

	// Put на мигрированной БД продолжает синхронизировать content_stemmed как обычно.
	newID := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: newID, Kind: memory.KindSemantic, Scope: "project:agentmemory",
		Content: "Новая запись про миграцию схемы", Importance: 3, CreatedAt: 2, UpdatedAt: 2,
	}); err != nil {
		t.Fatalf("Put после миграции: %v", err)
	}
	cands, err = s.SearchLexical(ctx, memory.Query{Text: "миграций", Scope: "project:agentmemory", Limit: 10})
	if err != nil {
		t.Fatalf("SearchLexical(миграций): %v", err)
	}
	found = false
	for _, c := range cands {
		if c.Memory.Id == newID {
			found = true
		}
	}
	if !found {
		t.Errorf("SearchLexical(миграций) не нашёл новую запись, добавленную после миграции (%+v)", cands)
	}
}

// ftsHasColumnPublic — обёртка над ftsHasColumn для теста (своя транзакция).
func ftsHasColumnPublic(db *sql.DB, ctx context.Context) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	return ftsHasColumn(ctx, tx, "content_stemmed")
}
