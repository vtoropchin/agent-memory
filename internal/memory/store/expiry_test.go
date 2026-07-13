package store

import (
	"context"
	"database/sql"
	"testing"

	"agentmemory/internal/memory"
)

// TestSearchLexicalExpiryFilter проверяет, что metaFilters (задача 9.4) скрывает
// из SearchLexical записи с истёкшим TTL, оставляя вечные (ExpiresAt=0) и ещё не
// истёкшие видимыми — и что граница expires_at == now уже трактуется как
// просроченная (m.expires_at > ?now, не >=).
func TestSearchLexicalExpiryFilter(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	s.now = func() int64 { return 1000 }

	eternal := newID(t)
	future := newID(t)
	boundary := newID(t)
	expired := newID(t)
	for _, tc := range []struct {
		id        string
		expiresAt int64
	}{
		{eternal, 0},
		{future, 1001},
		{boundary, 1000}, // == now: уже просрочена (строгое >)
		{expired, 999},
	} {
		if err := s.Put(ctx, memory.Memory{
			Id: tc.id, Kind: memory.KindSemantic, Scope: "project:agentmemory",
			Content: "запись с сроком годности", Importance: 3,
			CreatedAt: 1, UpdatedAt: 1, ExpiresAt: tc.expiresAt,
		}); err != nil {
			t.Fatalf("Put %s: %v", tc.id, err)
		}
	}

	cands, err := s.SearchLexical(ctx, memory.Query{Text: "срока годности", Scope: "project:agentmemory", Limit: 10})
	if err != nil {
		t.Fatalf("SearchLexical: %v", err)
	}
	got := map[string]bool{}
	for _, c := range cands {
		got[c.Memory.Id] = true
	}
	if len(got) != 2 || !got[eternal] || !got[future] {
		t.Fatalf("candidates = %+v, want [eternal future] (boundary/expired должны быть скрыты)", cands)
	}
}

// TestSearchVectorExpiryFilter — то же самое (metaFilters общая точка для обоих
// источников, DESIGN.md §8), но через векторный источник.
func TestSearchVectorExpiryFilter(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	s.now = func() int64 { return 1000 }

	if err := s.ResetVectorIndex(ctx, "test-model", 2); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}

	eternal := newID(t)
	future := newID(t)
	expired := newID(t)
	for _, tc := range []struct {
		id        string
		expiresAt int64
	}{
		{eternal, 0},
		{future, 1001},
		{expired, 999},
	} {
		if err := s.Put(ctx, memory.Memory{
			Id: tc.id, Kind: memory.KindSemantic, Scope: "project:agentmemory",
			Content: "c", CreatedAt: 1, UpdatedAt: 1, ExpiresAt: tc.expiresAt,
		}); err != nil {
			t.Fatalf("Put %s: %v", tc.id, err)
		}
		if err := s.PutVector(ctx, tc.id, []float32{1, 0}); err != nil {
			t.Fatalf("PutVector %s: %v", tc.id, err)
		}
	}

	cands, err := s.SearchVector(ctx, []float32{1, 0}, memory.Query{Limit: 10})
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	got := map[string]bool{}
	for _, c := range cands {
		got[c.Memory.Id] = true
	}
	if len(got) != 2 || !got[eternal] || !got[future] {
		t.Fatalf("candidates = %+v, want [eternal future] (expired должен быть скрыт)", cands)
	}
}

// TestListExpired проверяет выборку Store.ListExpired: только живые записи со
// строго истёкшим TTL (0 < expires_at < now), отсортированные по возрастанию
// expires_at; вечные/будущие/уже superseded не возвращаются.
func TestListExpired(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	eternal := newID(t)
	future := newID(t)
	olderExpired := newID(t)
	newerExpired := newID(t)
	alreadySuperseded := newID(t)
	for _, tc := range []struct {
		id        string
		expiresAt int64
	}{
		{eternal, 0},
		{future, 2000},
		{olderExpired, 500},
		{newerExpired, 900},
		{alreadySuperseded, 100},
	} {
		if err := s.Put(ctx, memory.Memory{
			Id: tc.id, Kind: memory.KindSemantic, Scope: "project:agentmemory",
			Content: "c", CreatedAt: 1, UpdatedAt: 1, ExpiresAt: tc.expiresAt,
		}); err != nil {
			t.Fatalf("Put %s: %v", tc.id, err)
		}
	}
	if err := s.Supersede(ctx, alreadySuperseded, alreadySuperseded); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	expired, err := s.ListExpired(ctx, 1000, 0)
	if err != nil {
		t.Fatalf("ListExpired: %v", err)
	}
	if len(expired) != 2 {
		t.Fatalf("ListExpired = %+v, want 2 записи (olderExpired, newerExpired)", expired)
	}
	if expired[0].Id != olderExpired || expired[1].Id != newerExpired {
		t.Errorf("порядок ListExpired = [%s %s], want [olderExpired newerExpired] (по возрастанию expires_at)", expired[0].Id, expired[1].Id)
	}

	// Граница: expires_at == now НЕ входит (строго "< now", см. годок ListExpired).
	boundary := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: boundary, Kind: memory.KindSemantic, Scope: "project:agentmemory",
		Content: "c", CreatedAt: 1, UpdatedAt: 1, ExpiresAt: 1000,
	}); err != nil {
		t.Fatalf("Put boundary: %v", err)
	}
	expired2, err := s.ListExpired(ctx, 1000, 0)
	if err != nil {
		t.Fatalf("ListExpired (после добавления boundary): %v", err)
	}
	for _, m := range expired2 {
		if m.Id == boundary {
			t.Errorf("ListExpired вернул boundary (expires_at == now), want строго < now")
		}
	}
}

// legacyExpiresAtSchema — DDL memories/memories_fts ДО задачи 9.4 (schemaVersion=1:
// content_stemmed уже есть, задача 8.3, но expires_at ещё нет). Используется
// только тестом миграции — воспроизводит реальный файл со схемой version=1.
const legacyExpiresAtSchema = `
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
CREATE VIRTUAL TABLE memories_fts USING fts5(content, summary, tags, content_stemmed);
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
PRAGMA user_version = 1;
`

// TestMigrateExpiresAtPreservesExistingData воспроизводит реальный файл боевой
// БД на schemaVersion=1 (после задачи 8.3, до 9.4) и проверяет, что Open мигрирует
// его на schemaVersion=2 без потери записей: существующие записи получают
// expires_at=NULL (ExpiresAt=0, вечные — живы и находятся как раньше), PRAGMA
// user_version становится 2, повторный Open (или прямой вызов migrate) идемпотентен.
func TestMigrateExpiresAtPreservesExistingData(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/legacy_v1.db"

	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(legacyExpiresAtSchema); err != nil {
		t.Fatalf("применение legacy-схемы (v1): %v", err)
	}
	ids := []string{newID(t), newID(t), newID(t)}
	contents := []string{
		"Первая боевая запись до TTL",
		"Вторая боевая запись до TTL",
		"Третья боевая запись до TTL",
	}
	for i, id := range ids {
		// memories_ai (AFTER INSERT ON memories) уже синхронизирует memories_fts —
		// отдельная вставка туда не нужна (и конфликтовала бы по rowid).
		if _, err := raw.Exec(`INSERT INTO memories (id, kind, scope, content, importance, created_at, updated_at) VALUES (?, 'semantic', 'project:agentmemory', ?, 3, 1, 1)`, id, contents[i]); err != nil {
			t.Fatalf("insert legacy memory %d: %v", i, err)
		}
	}
	var beforeCount int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&beforeCount); err != nil {
		t.Fatalf("count legacy memories: %v", err)
	}
	if beforeCount != len(ids) {
		t.Fatalf("beforeCount = %d, want %d", beforeCount, len(ids))
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	// Open — тот же путь реального кода: baseSchema (idempotent) -> migrate (1->2) -> ftsTriggers.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open (миграция 1->2): %v", err)
	}
	t.Cleanup(func() { s.Close() })

	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("чтение user_version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("user_version = %d, want %d (schemaVersion)", version, schemaVersion)
	}

	var afterCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories`).Scan(&afterCount); err != nil {
		t.Fatalf("count memories после миграции: %v", err)
	}
	if afterCount != beforeCount {
		t.Errorf("count memories после миграции = %d, want %d (ничего не потеряно)", afterCount, beforeCount)
	}

	// Старые записи живы (не superseded) и получили ExpiresAt=0 (NULL -> вечная).
	for i, id := range ids {
		m, err := s.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get %s после миграции: %v", id, err)
		}
		if m.SupersededBy != nil {
			t.Errorf("запись %d.SupersededBy = %v, want nil (миграция не должна архивировать старые записи)", i, m.SupersededBy)
		}
		if m.ExpiresAt != 0 {
			t.Errorf("запись %d.ExpiresAt = %d, want 0 (старые записи — вечные после миграции)", i, m.ExpiresAt)
		}
		if m.Content != contents[i] {
			t.Errorf("запись %d.Content = %q, want %q", i, m.Content, contents[i])
		}
	}

	// Регрессия: точный лексический поиск продолжает находить старые записи.
	cands, err := s.SearchLexical(ctx, memory.Query{Text: "боевая", Scope: "project:agentmemory", Limit: 10})
	if err != nil {
		t.Fatalf("SearchLexical после миграции: %v", err)
	}
	if len(cands) != len(ids) {
		t.Errorf("SearchLexical(боевая) после миграции = %d кандидатов, want %d", len(cands), len(ids))
	}

	// Put на мигрированной БД продолжает работать как обычно, в т.ч. с TTL.
	newID := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: newID, Kind: memory.KindSemantic, Scope: "project:agentmemory",
		Content: "новая запись после миграции", Importance: 3, CreatedAt: 2, UpdatedAt: 2, ExpiresAt: 12345,
	}); err != nil {
		t.Fatalf("Put после миграции: %v", err)
	}
	got, err := s.Get(ctx, newID)
	if err != nil {
		t.Fatalf("Get новой записи: %v", err)
	}
	if got.ExpiresAt != 12345 {
		t.Errorf("ExpiresAt новой записи = %d, want 12345", got.ExpiresAt)
	}
}
