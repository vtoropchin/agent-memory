package store

import (
	"context"
	"database/sql"
	"testing"

	"agentmemory/internal/memory"
)

// TestParityAuthor: author/updated_by (Этап 19) проходят Put и все пути чтения
// записи (Get, GetMany, ListTasks, List, AllForExport) на обоих драйверах;
// пустые хранятся как NULL и читаются пустыми; Supersede с именем ставит
// updated_by, без имени — не трогает, author не меняет; Touch не трогает ни то,
// ни другое.
func TestParityAuthor(t *testing.T) {
	runParity(t, "Author", func(t *testing.T, s Store) {
		ctx := context.Background()
		scope := "project:authors"

		taskID := putTestMemory(t, s, func(m *memory.Memory) {
			m.Kind, m.Scope, m.Status, m.Type = memory.KindTask, scope, memory.StatusTodo, memory.TaskTypeTask
			m.Author, m.UpdatedBy = "Анна", "Борис"
		})
		anonID := putTestMemory(t, s, func(m *memory.Memory) { m.Scope = scope })

		check := func(path string, got memory.Memory, author, updatedBy string) {
			t.Helper()
			if got.Author != author || got.UpdatedBy != updatedBy {
				t.Errorf("%s %s: author=%q updated_by=%q, want %q/%q", path, got.Id, got.Author, got.UpdatedBy, author, updatedBy)
			}
		}
		want := map[string][2]string{taskID: {"Анна", "Борис"}, anonID: {"", ""}}
		checkAll := func(path string, ms []memory.Memory) {
			t.Helper()
			for _, m := range ms {
				if w, ok := want[m.Id]; ok {
					check(path, m, w[0], w[1])
				}
			}
		}

		for id, w := range want {
			got, err := s.Get(ctx, id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			check("Get", got, w[0], w[1])
		}
		many, err := s.GetMany(ctx, []string{taskID, anonID})
		if err != nil {
			t.Fatalf("GetMany: %v", err)
		}
		checkAll("GetMany", many)
		tasks, err := s.ListTasks(ctx, []string{scope})
		if err != nil {
			t.Fatalf("ListTasks: %v", err)
		}
		checkAll("ListTasks", tasks)
		listed, _, err := s.List(ctx, BrowseFilter{Scopes: []string{scope}})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		checkAll("List", listed)
		exported, err := s.AllForExport(ctx, []string{scope})
		if err != nil {
			t.Fatalf("AllForExport: %v", err)
		}
		if len(exported) != 2 {
			t.Fatalf("AllForExport: %d записей, want 2", len(exported))
		}
		checkAll("AllForExport", exported)

		if err := s.Touch(ctx, taskID); err != nil {
			t.Fatalf("Touch: %v", err)
		}
		got, err := s.Get(ctx, taskID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		check("после Touch", got, "Анна", "Борис")

		if err := s.Supersede(ctx, taskID, taskID, ""); err != nil {
			t.Fatalf("Supersede без имени: %v", err)
		}
		got, err = s.Get(ctx, taskID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		check("после Supersede без имени", got, "Анна", "Борис")

		if err := s.Supersede(ctx, anonID, anonID, "Вера"); err != nil {
			t.Fatalf("Supersede с именем: %v", err)
		}
		got, err = s.Get(ctx, anonID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		check("после Supersede с именем", got, "", "Вера")
	})
}

// legacyAuthorSchema — DDL БД версии 4 (после Этапа 15, до Этапа 19): memories
// с task-полями, но без author/updated_by. Источник для проверки миграции 4 -> 5.
const legacyAuthorSchema = `
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
    source        TEXT,
    expires_at    INTEGER,
    status        TEXT,
    task_type     TEXT,
    branch        TEXT,
    workflow      TEXT
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
CREATE TABLE embed_meta (
    id    INTEGER PRIMARY KEY CHECK (id = 1),
    model TEXT NOT NULL,
    dim   INTEGER NOT NULL
);
PRAGMA user_version = 4;
`

// TestMigrateAuthor: Open мигрирует БД версии 4 на 5 (Этап 19) без потерь —
// записи, теги и связи на месте, author/updated_by пусты; после миграции поля
// пишутся и читаются; повторное открытие идемпотентно.
func TestMigrateAuthor(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/legacy_v4.db"

	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(legacyAuthorSchema); err != nil {
		t.Fatalf("применение legacy-схемы (v4): %v", err)
	}
	taskID, factID := newID(t), newID(t)
	if _, err := raw.Exec(`INSERT INTO memories (id, kind, scope, content, importance, created_at, updated_at, status, task_type, branch, workflow)
		VALUES (?, 'task', 'project:x', 'задача до этапа 19', 4, 1, 2, 'doing', 'story', 'feat/x', 'dev')`, taskID); err != nil {
		t.Fatalf("insert task: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO memories (id, kind, scope, content, importance, created_at, updated_at) VALUES (?, 'semantic', 'project:x', 'факт до этапа 19', 3, 1, 1)`, factID); err != nil {
		t.Fatalf("insert fact: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO tags (memory_id, tag) VALUES (?, 'sync')`, factID); err != nil {
		t.Fatalf("insert tag: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO links (src, dst, rel) VALUES (?, ?, 'part_of')`, factID, taskID); err != nil {
		t.Fatalf("insert link: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("Open (миграция 4->5): %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if v, err := s.SchemaVersion(ctx); err != nil || v != 5 {
		t.Fatalf("SchemaVersion = %d (%v), want 5", v, err)
	}
	verify := func(pass string) {
		t.Helper()
		task, err := s.Get(ctx, taskID)
		if err != nil {
			t.Fatalf("%s: Get task: %v", pass, err)
		}
		if task.Status != memory.StatusDoing || task.Type != memory.TaskTypeStory || task.Branch != "feat/x" || task.Workflow != "dev" || task.UpdatedAt != 2 {
			t.Errorf("%s: задача изменилась при миграции: %+v", pass, task)
		}
		fact, err := s.Get(ctx, factID)
		if err != nil {
			t.Fatalf("%s: Get fact: %v", pass, err)
		}
		if len(fact.Tags) != 1 || len(fact.Links) != 1 || fact.Links[0].Dst != taskID {
			t.Errorf("%s: теги/связи потеряны: tags=%v links=%v", pass, fact.Tags, fact.Links)
		}
		for _, m := range []memory.Memory{task, fact} {
			if m.Author != "" || m.UpdatedBy != "" {
				t.Errorf("%s: %s: у записи до Этапа 19 автор неизвестен, got %q/%q", pass, m.Id, m.Author, m.UpdatedBy)
			}
		}
	}
	verify("первое открытие")

	cands, err := s.SearchLexical(ctx, memory.Query{Text: "факт", Scope: "project:x", Limit: 10})
	if err != nil || len(cands) != 1 {
		t.Errorf("поиск после миграции: %v (%v), want 1 кандидат", cands, err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s, err = OpenSQLite(path)
	if err != nil {
		t.Fatalf("повторный Open: %v", err)
	}
	verify("повторное открытие")

	fact, err := s.Get(ctx, factID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	fact.Author, fact.UpdatedBy = "Анна", "Анна"
	if err := s.Put(ctx, fact); err != nil {
		t.Fatalf("Put с автором: %v", err)
	}
	if got, err := s.Get(ctx, factID); err != nil || got.Author != "Анна" || got.UpdatedBy != "Анна" {
		t.Errorf("после миграции поля не пишутся: %+v (%v)", got, err)
	}
}

// TestMigrateAuthorPG: схема PostgreSQL версии 1 (до Этапа 19, без author/
// updated_by) приводится init к текущей версии без потери записей.
func TestMigrateAuthorPG(t *testing.T) {
	ctx := context.Background()
	s := newTestPgStore(t)

	// Откат свежей схемы к версии 1: колонок авторства ещё нет.
	for _, stmt := range []string{
		`ALTER TABLE memories DROP COLUMN author, DROP COLUMN updated_by`,
		`UPDATE schema_version SET version = 1 WHERE id = 1`,
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("подготовка схемы v1 (%s): %v", stmt, err)
		}
	}
	id := newID(t)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO memories (id, kind, scope, content, importance, created_at, updated_at, status, task_type)
		VALUES ($1, 'task', 'project:x', 'задача до этапа 19', 4, 1, 2, 'todo', 'task')`, id); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if err := s.init(ctx); err != nil {
		t.Fatalf("init (миграция 1->2): %v", err)
	}
	if v, err := s.SchemaVersion(ctx); err != nil || v != pgSchemaVersion {
		t.Fatalf("SchemaVersion = %d (%v), want %d", v, err, pgSchemaVersion)
	}
	got, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Content != "задача до этапа 19" || got.Status != memory.StatusTodo || got.UpdatedAt != 2 || got.Author != "" || got.UpdatedBy != "" {
		t.Errorf("запись после миграции: %+v", got)
	}
	got.Author, got.UpdatedBy = "Анна", "Борис"
	if err := s.Put(ctx, got); err != nil {
		t.Fatalf("Put с автором: %v", err)
	}
	if got, err := s.Get(ctx, id); err != nil || got.Author != "Анна" || got.UpdatedBy != "Борис" {
		t.Errorf("после миграции поля не пишутся: %+v (%v)", got, err)
	}
}
