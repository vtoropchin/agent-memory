package store

import (
	"context"
	"database/sql"
	"testing"

	"agentmemory/internal/memory"
)

// TestPutGetStatusRoundTrip: статус задачи переживает Put -> Get, а у записей
// прочих kind остаётся пустым (в БД — NULL).
func TestPutGetStatusRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	taskID, factID := newID(t), newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: taskID, Kind: memory.KindTask, Scope: "project:agentmemory",
		Content: "Текст задачи", Importance: 3, CreatedAt: 1, UpdatedAt: 1,
		Status: memory.StatusDoing,
	}); err != nil {
		t.Fatalf("Put task: %v", err)
	}
	if err := s.Put(ctx, memory.Memory{
		Id: factID, Kind: memory.KindSemantic, Scope: "project:agentmemory",
		Content: "Обычный факт", Importance: 3, CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("Put semantic: %v", err)
	}

	task, err := s.Get(ctx, taskID)
	if err != nil {
		t.Fatalf("Get task: %v", err)
	}
	if task.Status != memory.StatusDoing {
		t.Errorf("Status = %q, want %q", task.Status, memory.StatusDoing)
	}
	fact, err := s.Get(ctx, factID)
	if err != nil {
		t.Fatalf("Get semantic: %v", err)
	}
	if fact.Status != "" {
		t.Errorf("Status записи не-task = %q, want пусто", fact.Status)
	}

	// Пустой статус хранится как NULL, а не пустой строкой (см. годок Put).
	var isNull bool
	if err := s.db.QueryRowContext(ctx, `SELECT status IS NULL FROM memories WHERE id = ?`, factID).Scan(&isNull); err != nil {
		t.Fatalf("проверка NULL: %v", err)
	}
	if !isNull {
		t.Errorf("status записи без состояния должен храниться как NULL")
	}
}

// TestSearchLexicalHidesDoneTasks: без Query.Statuses выполненные задачи не
// попадают в кандидаты, всё остальное попадает; явный фильтр отбирает по статусу.
func TestSearchLexicalHidesDoneTasks(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:agentmemory"

	doneID, todoID, factID := newID(t), newID(t), newID(t)
	for _, m := range []memory.Memory{
		{Id: doneID, Kind: memory.KindTask, Scope: scope, Content: "общий текст кандидат", Importance: 3, CreatedAt: 1, UpdatedAt: 1, Status: memory.StatusDone},
		{Id: todoID, Kind: memory.KindTask, Scope: scope, Content: "общий текст кандидат", Importance: 3, CreatedAt: 2, UpdatedAt: 2, Status: memory.StatusTodo},
		{Id: factID, Kind: memory.KindSemantic, Scope: scope, Content: "общий текст кандидат", Importance: 3, CreatedAt: 3, UpdatedAt: 3},
	} {
		if err := s.Put(ctx, m); err != nil {
			t.Fatalf("Put %s: %v", m.Id, err)
		}
	}

	ids := func(cands []memory.Candidate) map[string]bool {
		out := map[string]bool{}
		for _, c := range cands {
			out[c.Memory.Id] = true
		}
		return out
	}

	got, err := s.SearchLexical(ctx, memory.Query{Text: "текст кандидат", Scope: scope, Limit: 10})
	if err != nil {
		t.Fatalf("SearchLexical: %v", err)
	}
	found := ids(got)
	if found[doneID] {
		t.Errorf("выполненная задача не должна попадать в кандидаты без явного фильтра")
	}
	if !found[todoID] || !found[factID] {
		t.Errorf("открытая задача и обычный факт обязаны остаться: %v", found)
	}

	got, err = s.SearchLexical(ctx, memory.Query{Text: "текст кандидат", Scope: scope, Limit: 10, Statuses: []string{string(memory.StatusDone)}})
	if err != nil {
		t.Fatalf("SearchLexical(statuses=done): %v", err)
	}
	found = ids(got)
	if !found[doneID] {
		t.Errorf("явный фильтр statuses=[done] обязан вернуть выполненную задачу")
	}
	if found[todoID] || found[factID] {
		t.Errorf("явный фильтр по статусу отбирает только совпавшие задачи: %v", found)
	}
}

// legacyStatusSchema — DDL memories ДО задачи 14.1 (schemaVersion=2: есть
// content_stemmed и expires_at, но нет status). Воспроизводит реальный файл
// боевой БД предыдущей версии для теста миграции 2 -> 3.
const legacyStatusSchema = `
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
    expires_at    INTEGER
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
PRAGMA user_version = 2;
`

// TestMigrateStatusPreservesExistingData: Open мигрирует БД версии 2 на версию 3
// без потери данных — старые записи остаются живыми и без статуса, поиск по ним
// работает, а новые задачи со статусом пишутся и читаются как обычно.
func TestMigrateStatusPreservesExistingData(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/legacy_v2.db"

	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(legacyStatusSchema); err != nil {
		t.Fatalf("применение legacy-схемы (v2): %v", err)
	}
	oldID := newID(t)
	if _, err := raw.Exec(`INSERT INTO memories (id, kind, scope, content, importance, created_at, updated_at) VALUES (?, 'semantic', 'project:agentmemory', 'боевая запись до задач', 3, 1, 1)`, oldID); err != nil {
		t.Fatalf("insert legacy memory: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("Open (миграция 2->3): %v", err)
	}
	t.Cleanup(func() { s.Close() })

	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("чтение user_version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("user_version = %d, want %d (schemaVersion)", version, schemaVersion)
	}

	old, err := s.Get(ctx, oldID)
	if err != nil {
		t.Fatalf("Get старой записи: %v", err)
	}
	if old.Status != "" {
		t.Errorf("старая запись получила Status = %q, want пусто", old.Status)
	}
	if old.SupersededBy != nil {
		t.Errorf("миграция не должна архивировать старые записи")
	}
	cands, err := s.SearchLexical(ctx, memory.Query{Text: "боевая", Scope: "project:agentmemory", Limit: 10})
	if err != nil {
		t.Fatalf("SearchLexical после миграции: %v", err)
	}
	if len(cands) != 1 {
		t.Errorf("SearchLexical после миграции = %d кандидатов, want 1", len(cands))
	}

	taskID := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: taskID, Kind: memory.KindTask, Scope: "project:agentmemory",
		Content: "задача после миграции", Importance: 3, CreatedAt: 2, UpdatedAt: 2,
		Status: memory.StatusReview,
	}); err != nil {
		t.Fatalf("Put задачи после миграции: %v", err)
	}
	got, err := s.Get(ctx, taskID)
	if err != nil {
		t.Fatalf("Get задачи: %v", err)
	}
	if got.Status != memory.StatusReview {
		t.Errorf("Status = %q, want %q", got.Status, memory.StatusReview)
	}
}

// TestPutGetTaskFieldsRoundTrip: task_type/branch/workflow (Этап 15) переживают
// Put -> Get и Put -> List, а у записей прочих kind остаются пустыми и хранятся
// как NULL.
func TestPutGetTaskFieldsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:agentmemory"

	taskID, factID := newID(t), newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: taskID, Kind: memory.KindTask, Scope: scope,
		Content: "Текст эпика", Importance: 3, CreatedAt: 1, UpdatedAt: 1,
		Status: memory.StatusDoing, Type: memory.TaskTypeEpic, Branch: "feat/kanban", Workflow: "dev",
	}); err != nil {
		t.Fatalf("Put task: %v", err)
	}
	if err := s.Put(ctx, memory.Memory{
		Id: factID, Kind: memory.KindSemantic, Scope: scope,
		Content: "Обычный факт", Importance: 3, CreatedAt: 2, UpdatedAt: 2,
	}); err != nil {
		t.Fatalf("Put semantic: %v", err)
	}

	check := func(where string, m memory.Memory) {
		t.Helper()
		if m.Type != memory.TaskTypeEpic || m.Branch != "feat/kanban" || m.Workflow != "dev" {
			t.Errorf("%s: type=%q branch=%q workflow=%q, want epic/feat/kanban/dev", where, m.Type, m.Branch, m.Workflow)
		}
	}
	task, err := s.Get(ctx, taskID)
	if err != nil {
		t.Fatalf("Get task: %v", err)
	}
	check("Get", task)
	fact, err := s.Get(ctx, factID)
	if err != nil {
		t.Fatalf("Get semantic: %v", err)
	}
	if fact.Type != "" || fact.Branch != "" || fact.Workflow != "" {
		t.Errorf("task-поля записи не-task = %q/%q/%q, want пусто", fact.Type, fact.Branch, fact.Workflow)
	}

	items, _, err := s.List(ctx, BrowseFilter{Scopes: []string{scope}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, m := range items {
		if m.Id == taskID {
			check("List", m)
		}
	}

	var nulls int
	if err := s.db.QueryRowContext(ctx, `SELECT (task_type IS NULL) + (branch IS NULL) + (workflow IS NULL) FROM memories WHERE id = ?`, factID).Scan(&nulls); err != nil {
		t.Fatalf("проверка NULL: %v", err)
	}
	if nulls != 3 {
		t.Errorf("task-поля записи без них должны храниться как NULL, got %d NULL из 3", nulls)
	}
}

// TestSearchLexicalFilterByTypes: Query.Types отбирает задачи по типу; записи
// прочих kind (task_type = NULL) под явный фильтр не попадают; пустой Types —
// без фильтра.
func TestSearchLexicalFilterByTypes(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:agentmemory"

	epicID, storyID, taskID, factID := newID(t), newID(t), newID(t), newID(t)
	for _, m := range []memory.Memory{
		{Id: epicID, Kind: memory.KindTask, Scope: scope, Content: "общий текст фильтра типов", Importance: 3, CreatedAt: 1, UpdatedAt: 1, Status: memory.StatusTodo, Type: memory.TaskTypeEpic},
		{Id: storyID, Kind: memory.KindTask, Scope: scope, Content: "общий текст фильтра типов", Importance: 3, CreatedAt: 2, UpdatedAt: 2, Status: memory.StatusTodo, Type: memory.TaskTypeStory},
		{Id: taskID, Kind: memory.KindTask, Scope: scope, Content: "общий текст фильтра типов", Importance: 3, CreatedAt: 3, UpdatedAt: 3, Status: memory.StatusTodo, Type: memory.TaskTypeTask},
		{Id: factID, Kind: memory.KindSemantic, Scope: scope, Content: "общий текст фильтра типов", Importance: 3, CreatedAt: 4, UpdatedAt: 4},
	} {
		if err := s.Put(ctx, m); err != nil {
			t.Fatalf("Put %s: %v", m.Id, err)
		}
	}
	ids := func(cands []memory.Candidate) map[string]bool {
		out := map[string]bool{}
		for _, c := range cands {
			out[c.Memory.Id] = true
		}
		return out
	}

	got, err := s.SearchLexical(ctx, memory.Query{Text: "фильтра типов", Scope: scope, Limit: 10, Types: []string{"epic", "story"}})
	if err != nil {
		t.Fatalf("SearchLexical(types): %v", err)
	}
	found := ids(got)
	if !found[epicID] || !found[storyID] {
		t.Errorf("фильтр types=[epic,story] обязан вернуть эпик и историю: %v", found)
	}
	if found[taskID] || found[factID] {
		t.Errorf("фильтр types не должен пропускать обычную задачу и факт: %v", found)
	}

	got, err = s.SearchLexical(ctx, memory.Query{Text: "фильтра типов", Scope: scope, Limit: 10})
	if err != nil {
		t.Fatalf("SearchLexical: %v", err)
	}
	if found = ids(got); len(found) != 4 {
		t.Errorf("без Types все четыре записи должны найтись: %v", found)
	}
}

// legacyTaskSchema — DDL memories версии 3 (задача 14.1: есть status, но нет
// task_type/branch/workflow). Воспроизводит боевую БД до Этапа 15 для теста
// миграции 3 -> 4.
const legacyTaskSchema = `
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
    status        TEXT
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
PRAGMA user_version = 3;
`

// TestMigrateTaskModel: Open мигрирует БД версии 3 на версию 4 (Этап 15):
// задача со status='blocked' стала todo; задача с тегом "эпик" получила
// task_type=epic, с тегом "История" — story, прочие задачи — task; теги целы;
// факт не-task остался без task-полей; поиск работает; повторное открытие
// (идемпотентность) данных не меняет.
func TestMigrateTaskModel(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/legacy_v3.db"

	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(legacyTaskSchema); err != nil {
		t.Fatalf("применение legacy-схемы (v3): %v", err)
	}
	blockedID, epicID, storyID, plainID, doneID, factID := newID(t), newID(t), newID(t), newID(t), newID(t), newID(t)
	insert := func(id, kind, content string, status any) {
		t.Helper()
		if _, err := raw.Exec(`INSERT INTO memories (id, kind, scope, content, importance, created_at, updated_at, status) VALUES (?, ?, 'project:agentmemory', ?, 3, 1, 1, ?)`, id, kind, content, status); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	insert(blockedID, "task", "заблокированная задача до этапа", "blocked")
	insert(epicID, "task", "эпик по тегу", "todo")
	insert(storyID, "task", "история по тегу", "doing")
	insert(plainID, "task", "обычная задача без тегов типа", "todo")
	insert(doneID, "task", "выполненная задача с тегом epic", "done")
	insert(factID, "semantic", "факт с тегом epic, но не задача", nil)
	for _, tg := range [][2]string{{epicID, "эпик"}, {epicID, "kanban"}, {storyID, "История"}, {doneID, "epic"}, {factID, "epic"}} {
		if _, err := raw.Exec(`INSERT INTO tags (memory_id, tag) VALUES (?, ?)`, tg[0], tg[1]); err != nil {
			t.Fatalf("insert tag %v: %v", tg, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("Open (миграция 3->4): %v", err)
	}
	t.Cleanup(func() { s.Close() })

	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("чтение user_version: %v", err)
	}
	if version != 4 {
		t.Errorf("user_version = %d, want 4", version)
	}

	want := map[string]struct {
		status memory.Status
		typ    memory.TaskType
		tags   int
	}{
		blockedID: {memory.StatusTodo, memory.TaskTypeTask, 0},
		epicID:    {memory.StatusTodo, memory.TaskTypeEpic, 2},
		storyID:   {memory.StatusDoing, memory.TaskTypeStory, 1},
		plainID:   {memory.StatusTodo, memory.TaskTypeTask, 0},
		doneID:    {memory.StatusDone, memory.TaskTypeEpic, 1},
		factID:    {"", "", 1},
	}
	verify := func(pass string) {
		t.Helper()
		for id, w := range want {
			m, err := s.Get(ctx, id)
			if err != nil {
				t.Fatalf("%s: Get %s: %v", pass, id, err)
			}
			if m.Status != w.status || m.Type != w.typ {
				t.Errorf("%s: %s (%s): status=%q type=%q, want %q/%q", pass, id, m.Content, m.Status, m.Type, w.status, w.typ)
			}
			if len(m.Tags) != w.tags {
				t.Errorf("%s: %s: теги должны остаться нетронутыми: %v, want %d", pass, id, m.Tags, w.tags)
			}
			if m.Branch != "" || m.Workflow != "" {
				t.Errorf("%s: %s: branch/workflow после миграции должны быть пусты", pass, id)
			}
		}
	}
	verify("первое открытие")

	cands, err := s.SearchLexical(ctx, memory.Query{Text: "заблокированная", Scope: "project:agentmemory", Limit: 10, Statuses: []string{"todo"}})
	if err != nil {
		t.Fatalf("SearchLexical после миграции: %v", err)
	}
	if len(cands) != 1 || cands[0].Memory.Id != blockedID {
		t.Errorf("бывшая blocked-задача должна находиться фильтром statuses=[todo]: %v", cands)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s, err = OpenSQLite(path)
	if err != nil {
		t.Fatalf("повторный Open: %v", err)
	}
	verify("повторное открытие")
}

// TestSearchLexicalIncludeDone: Query.IncludeDone снимает умолчание «скрывать
// выполненные задачи», не превращая выдачу в «только задачи» (что сделал бы
// явный перечень всех статусов — у записей прочих kind status = NULL).
func TestSearchLexicalIncludeDone(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:agentmemory"

	doneID, factID := newID(t), newID(t)
	for _, m := range []memory.Memory{
		{Id: doneID, Kind: memory.KindTask, Scope: scope, Content: "обзорный текст записи", Importance: 3, CreatedAt: 1, UpdatedAt: 1, Status: memory.StatusDone},
		{Id: factID, Kind: memory.KindSemantic, Scope: scope, Content: "обзорный текст записи", Importance: 3, CreatedAt: 2, UpdatedAt: 2},
	} {
		if err := s.Put(ctx, m); err != nil {
			t.Fatalf("Put %s: %v", m.Id, err)
		}
	}

	got, err := s.SearchLexical(ctx, memory.Query{Text: "обзорный текст", Scope: scope, Limit: 10, IncludeDone: true})
	if err != nil {
		t.Fatalf("SearchLexical(IncludeDone): %v", err)
	}
	found := map[string]bool{}
	for _, c := range got {
		found[c.Memory.Id] = true
	}
	if !found[doneID] || !found[factID] {
		t.Errorf("IncludeDone обязан вернуть и выполненную задачу, и обычный факт: %v", found)
	}
}
