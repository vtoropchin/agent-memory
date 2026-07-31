package store

import (
	"context"
	"math"
	"path/filepath"
	"testing"

	"github.com/oklog/ulid/v2"

	"agentmemory/internal/memory"
)

func newTestStore(t *testing.T) *sqliteStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "memory.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newID(t *testing.T) string {
	t.Helper()
	return ulid.Make().String()
}

// putTestMemory кладёт в st одну живую запись памяти с разумными дефолтами
// (по образцу putDoctorMemory в cmd/memo/doctor_test.go) — общий хелпер для
// тестов, которым нужна лишь произвольная запись, а не конкретное её
// содержимое. Возвращает Id заведённой записи для последующей проверки (Get,
// SQLiteHasMemoriesContext и т.п.).
func putTestMemory(t *testing.T, st Store, mutate func(*memory.Memory)) string {
	t.Helper()
	m := memory.Memory{
		Id: newID(t), Kind: memory.KindSemantic, Scope: "project:x",
		Content: "запись", Importance: 3, CreatedAt: 1, UpdatedAt: 1,
	}
	if mutate != nil {
		mutate(&m)
	}
	if err := st.Put(context.Background(), m); err != nil {
		t.Fatalf("Put: %v", err)
	}
	return m.Id
}

func strPtr(s string) *string { return &s }

func int64Ptr(v int64) *int64 { return &v }

func TestPutGetRoundtrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	id := newID(t)
	m := memory.Memory{
		Id:         id,
		Kind:       memory.KindSemantic,
		Scope:      "project:agentmemory",
		Content:    "Проект на Go, SQLite через mattn/go-sqlite3",
		Summary:    strPtr("Go + SQLite"),
		Importance: 4,
		CreatedAt:  1000,
		UpdatedAt:  1000,
		Source:     strPtr("session-1"),
		Tags:       []string{"go", "stack"},
	}

	otherID := newID(t)
	other := memory.Memory{
		Id: otherID, Kind: memory.KindSemantic, Scope: "project:agentmemory",
		Content: "другая запись", Importance: 3, CreatedAt: 999, UpdatedAt: 999,
	}
	if err := s.Put(ctx, other); err != nil {
		t.Fatalf("Put other: %v", err)
	}
	m.Links = []memory.Link{{Dst: otherID, Rel: memory.RelRelated}}

	if err := s.Put(ctx, m); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.Content != m.Content {
		t.Errorf("Content = %q, want %q", got.Content, m.Content)
	}
	if got.Summary == nil || *got.Summary != *m.Summary {
		t.Errorf("Summary = %v, want %v", got.Summary, m.Summary)
	}
	if got.Source == nil || *got.Source != *m.Source {
		t.Errorf("Source = %v, want %v", got.Source, m.Source)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "go" || got.Tags[1] != "stack" {
		t.Errorf("Tags = %v, want [go stack]", got.Tags)
	}
	if len(got.Links) != 1 || got.Links[0].Dst != otherID || got.Links[0].Rel != memory.RelRelated {
		t.Errorf("Links = %v", got.Links)
	}

	// Upsert: обновляем content, ожидаем что старые tags/links полностью заменяются.
	m.Content = "обновлённый факт"
	m.Tags = []string{"updated"}
	m.Links = nil
	if err := s.Put(ctx, m); err != nil {
		t.Fatalf("Put update: %v", err)
	}
	got2, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if got2.Content != "обновлённый факт" {
		t.Errorf("Content после update = %q", got2.Content)
	}
	if len(got2.Tags) != 1 || got2.Tags[0] != "updated" {
		t.Errorf("Tags после update = %v, want [updated]", got2.Tags)
	}
	if len(got2.Links) != 0 {
		t.Errorf("Links после update = %v, want empty", got2.Links)
	}
}

func TestPutRejectsEmptyOrInvalidId(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.Put(ctx, memory.Memory{Kind: memory.KindSemantic, Scope: "x", Content: "c"}); err == nil {
		t.Error("Put с пустым Id должен вернуть ошибку")
	}
	if err := s.Put(ctx, memory.Memory{Id: "not-a-ulid", Kind: memory.KindSemantic, Scope: "x", Content: "c"}); err == nil {
		t.Error("Put с некорректным ULID должен вернуть ошибку")
	}
}

func TestGetNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.Get(ctx, newID(t)); err == nil {
		t.Error("Get несуществующей записи должен вернуть ошибку")
	}
}

func TestSearchLexical(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	semID := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: semID, Kind: memory.KindSemantic, Scope: "project:agentmemory",
		Content: "Проект использует SQLite и FTS5 для поиска", Importance: 4,
		CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("Put sem: %v", err)
	}

	procID := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: procID, Kind: memory.KindProcedural, Scope: "project:agentmemory",
		Content: "Сборка через make build, тесты через make test", Importance: 3,
		CreatedAt: 2, UpdatedAt: 2,
	}); err != nil {
		t.Fatalf("Put proc: %v", err)
	}

	otherScopeID := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: otherScopeID, Kind: memory.KindSemantic, Scope: "project:other",
		Content: "SQLite в другом проекте", Importance: 5,
		CreatedAt: 3, UpdatedAt: 3,
	}); err != nil {
		t.Fatalf("Put other scope: %v", err)
	}

	supersededID := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: supersededID, Kind: memory.KindSemantic, Scope: "project:agentmemory",
		Content: "SQLite устаревший факт", Importance: 3,
		CreatedAt: 4, UpdatedAt: 4,
	}); err != nil {
		t.Fatalf("Put superseded: %v", err)
	}
	if err := s.Supersede(ctx, supersededID, semID); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	t.Run("находит по content и фильтрует по scope", func(t *testing.T) {
		cands, err := s.SearchLexical(ctx, memory.Query{Text: "SQLite", Scope: "project:agentmemory", Limit: 10})
		if err != nil {
			t.Fatalf("SearchLexical: %v", err)
		}
		if len(cands) != 1 {
			t.Fatalf("candidates = %d, want 1 (нашли: %+v)", len(cands), cands)
		}
		if cands[0].Memory.Id != semID {
			t.Errorf("candidate.Id = %q, want %q", cands[0].Memory.Id, semID)
		}
		if cands[0].Source != "lexical" {
			t.Errorf("Source = %q, want lexical", cands[0].Source)
		}
	})

	t.Run("фильтрует по kind", func(t *testing.T) {
		cands, err := s.SearchLexical(ctx, memory.Query{
			Text: "make", Scope: "project:agentmemory", Kinds: []string{"procedural"}, Limit: 10,
		})
		if err != nil {
			t.Fatalf("SearchLexical: %v", err)
		}
		if len(cands) != 1 || cands[0].Memory.Id != procID {
			t.Fatalf("candidates = %+v, want procID", cands)
		}
	})

	t.Run("исключает superseded", func(t *testing.T) {
		cands, err := s.SearchLexical(ctx, memory.Query{Text: "устаревший", Scope: "project:agentmemory", Limit: 10})
		if err != nil {
			t.Fatalf("SearchLexical: %v", err)
		}
		if len(cands) != 0 {
			t.Fatalf("candidates = %+v, want пусто (запись superseded)", cands)
		}
	})

	t.Run("кривой запрос не паникует", func(t *testing.T) {
		weird := []string{
			`"unterminated`,
			`col: OR NOT AND (`,
			`-- ; DROP TABLE memories`,
			`""""`,
			"   ",
			"",
		}
		for _, q := range weird {
			if _, err := s.SearchLexical(ctx, memory.Query{Text: q, Scope: "project:agentmemory", Limit: 10}); err != nil {
				t.Errorf("SearchLexical(%q) вернул ошибку: %v", q, err)
			}
		}
	})
}

func TestSearchLexicalScopes(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	aID := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: aID, Kind: memory.KindSemantic, Scope: "project:a",
		Content: "заметка про golang в проекте a", Importance: 3,
		CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("Put a: %v", err)
	}

	userID := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: userID, Kind: memory.KindSemantic, Scope: "user:*",
		Content: "заметка про golang общая", Importance: 3,
		CreatedAt: 2, UpdatedAt: 2,
	}); err != nil {
		t.Fatalf("Put user: %v", err)
	}

	bID := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: bID, Kind: memory.KindSemantic, Scope: "project:b",
		Content: "заметка про golang в проекте b", Importance: 3,
		CreatedAt: 3, UpdatedAt: 3,
	}); err != nil {
		t.Fatalf("Put b: %v", err)
	}

	t.Run("набор scopes возвращает записи из всех перечисленных scope и не возвращает прочие", func(t *testing.T) {
		cands, err := s.SearchLexical(ctx, memory.Query{
			Text: "golang", Scopes: []string{"project:a", "user:*"}, Limit: 10,
		})
		if err != nil {
			t.Fatalf("SearchLexical: %v", err)
		}
		got := map[string]bool{}
		for _, c := range cands {
			got[c.Memory.Id] = true
		}
		if len(cands) != 2 || !got[aID] || !got[userID] {
			t.Fatalf("candidates = %+v, want [a user]", cands)
		}
		if got[bID] {
			t.Errorf("candidates содержат запись из project:b, не входящего в Scopes")
		}
	})

	t.Run("одиночный Scope работает по-прежнему (регрессия)", func(t *testing.T) {
		cands, err := s.SearchLexical(ctx, memory.Query{Text: "golang", Scope: "project:a", Limit: 10})
		if err != nil {
			t.Fatalf("SearchLexical: %v", err)
		}
		if len(cands) != 1 || cands[0].Memory.Id != aID {
			t.Fatalf("candidates = %+v, want [a]", cands)
		}
	})

	t.Run("без scope — без фильтра", func(t *testing.T) {
		cands, err := s.SearchLexical(ctx, memory.Query{Text: "golang", Limit: 10})
		if err != nil {
			t.Fatalf("SearchLexical: %v", err)
		}
		if len(cands) != 3 {
			t.Fatalf("candidates = %+v, want 3 (без фильтра по scope)", cands)
		}
	})

	t.Run("Scopes имеет приоритет над Scope", func(t *testing.T) {
		cands, err := s.SearchLexical(ctx, memory.Query{
			Text: "golang", Scope: "project:b", Scopes: []string{"project:a"}, Limit: 10,
		})
		if err != nil {
			t.Fatalf("SearchLexical: %v", err)
		}
		if len(cands) != 1 || cands[0].Memory.Id != aID {
			t.Fatalf("candidates = %+v, want [a] (Scopes приоритетнее Scope)", cands)
		}
	})
}

// TestSearchLexicalTagsFilter проверяет OR-семантику Query.Tags (задача 8.2):
// запись проходит, если имеет хотя бы один из перечисленных тегов.
func TestSearchLexicalTagsFilter(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	stackID, goID, bothID, noneID := newID(t), newID(t), newID(t), newID(t)
	for _, tc := range []struct {
		id   string
		tags []string
	}{
		{stackID, []string{"стек"}},
		{goID, []string{"go"}},
		{bothID, []string{"стек", "go"}},
		{noneID, nil},
	} {
		if err := s.Put(ctx, memory.Memory{
			Id: tc.id, Kind: memory.KindSemantic, Scope: "project:agentmemory",
			Content: "заметка про инструменты проекта", Importance: 3,
			CreatedAt: 1, UpdatedAt: 1, Tags: tc.tags,
		}); err != nil {
			t.Fatalf("Put %s: %v", tc.id, err)
		}
	}

	t.Run("один тег", func(t *testing.T) {
		cands, err := s.SearchLexical(ctx, memory.Query{Text: "заметка", Tags: []string{"стек"}, Limit: 10})
		if err != nil {
			t.Fatalf("SearchLexical: %v", err)
		}
		got := map[string]bool{}
		for _, c := range cands {
			got[c.Memory.Id] = true
		}
		if len(got) != 2 || !got[stackID] || !got[bothID] {
			t.Fatalf("candidates = %+v, want [stackID bothID]", cands)
		}
	})

	t.Run("несколько тегов — OR", func(t *testing.T) {
		cands, err := s.SearchLexical(ctx, memory.Query{Text: "заметка", Tags: []string{"стек", "go"}, Limit: 10})
		if err != nil {
			t.Fatalf("SearchLexical: %v", err)
		}
		got := map[string]bool{}
		for _, c := range cands {
			got[c.Memory.Id] = true
		}
		if len(got) != 3 || !got[stackID] || !got[goID] || !got[bothID] {
			t.Fatalf("candidates = %+v, want [stackID goID bothID]", cands)
		}
	})

	t.Run("несуществующий тег — пусто", func(t *testing.T) {
		cands, err := s.SearchLexical(ctx, memory.Query{Text: "заметка", Tags: []string{"нет-такого-тега"}, Limit: 10})
		if err != nil {
			t.Fatalf("SearchLexical: %v", err)
		}
		if len(cands) != 0 {
			t.Fatalf("candidates = %+v, want пусто", cands)
		}
	})
}

// TestSearchLexicalCreatedAtFilter проверяет, что CreatedAfter/CreatedBefore
// (задача 8.2) отсекают записи по границам, включая саму границу (>=/<=).
func TestSearchLexicalCreatedAtFilter(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	old, mid, recent := newID(t), newID(t), newID(t)
	for _, tc := range []struct {
		id string
		ts int64
	}{
		{old, 100},
		{mid, 200},
		{recent, 300},
	} {
		if err := s.Put(ctx, memory.Memory{
			Id: tc.id, Kind: memory.KindSemantic, Scope: "project:agentmemory",
			Content: "хронологическая заметка", Importance: 3,
			CreatedAt: tc.ts, UpdatedAt: tc.ts,
		}); err != nil {
			t.Fatalf("Put %s: %v", tc.id, err)
		}
	}

	t.Run("CreatedAfter отсекает более старые, включая границу", func(t *testing.T) {
		cands, err := s.SearchLexical(ctx, memory.Query{Text: "хронологическая", CreatedAfter: 200, Limit: 10})
		if err != nil {
			t.Fatalf("SearchLexical: %v", err)
		}
		got := map[string]bool{}
		for _, c := range cands {
			got[c.Memory.Id] = true
		}
		if len(got) != 2 || !got[mid] || !got[recent] {
			t.Fatalf("candidates = %+v, want [mid recent] (CreatedAfter=200 включает границу)", cands)
		}
	})

	t.Run("CreatedBefore отсекает более свежие, включая границу", func(t *testing.T) {
		cands, err := s.SearchLexical(ctx, memory.Query{Text: "хронологическая", CreatedBefore: 200, Limit: 10})
		if err != nil {
			t.Fatalf("SearchLexical: %v", err)
		}
		got := map[string]bool{}
		for _, c := range cands {
			got[c.Memory.Id] = true
		}
		if len(got) != 2 || !got[old] || !got[mid] {
			t.Fatalf("candidates = %+v, want [old mid] (CreatedBefore=200 включает границу)", cands)
		}
	})

	t.Run("CreatedAfter и CreatedBefore вместе — диапазон", func(t *testing.T) {
		cands, err := s.SearchLexical(ctx, memory.Query{Text: "хронологическая", CreatedAfter: 200, CreatedBefore: 200, Limit: 10})
		if err != nil {
			t.Fatalf("SearchLexical: %v", err)
		}
		if len(cands) != 1 || cands[0].Memory.Id != mid {
			t.Fatalf("candidates = %+v, want [mid]", cands)
		}
	})
}

// TestSearchLexicalCombinedFilters проверяет, что scope+kind+tag+даты работают
// одновременно (AND между разными видами фильтров, задача 8.2) — запись должна
// пройти ВСЕ условия сразу, а не хотя бы одно.
func TestSearchLexicalCombinedFilters(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	match := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: match, Kind: memory.KindProcedural, Scope: "project:agentmemory",
		Content: "комбинированный рецепт сборки", Importance: 3,
		CreatedAt: 200, UpdatedAt: 200, Tags: []string{"стек"},
	}); err != nil {
		t.Fatalf("Put match: %v", err)
	}

	// Не совпадает по kind.
	wrongKind := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: wrongKind, Kind: memory.KindSemantic, Scope: "project:agentmemory",
		Content: "комбинированный факт", Importance: 3,
		CreatedAt: 200, UpdatedAt: 200, Tags: []string{"стек"},
	}); err != nil {
		t.Fatalf("Put wrongKind: %v", err)
	}
	// Не совпадает по scope.
	wrongScope := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: wrongScope, Kind: memory.KindProcedural, Scope: "project:other",
		Content: "комбинированный рецепт в другом проекте", Importance: 3,
		CreatedAt: 200, UpdatedAt: 200, Tags: []string{"стек"},
	}); err != nil {
		t.Fatalf("Put wrongScope: %v", err)
	}
	// Не совпадает по тегу.
	wrongTag := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: wrongTag, Kind: memory.KindProcedural, Scope: "project:agentmemory",
		Content: "комбинированный рецепт без нужного тега", Importance: 3,
		CreatedAt: 200, UpdatedAt: 200, Tags: []string{"другое"},
	}); err != nil {
		t.Fatalf("Put wrongTag: %v", err)
	}
	// Не совпадает по дате (слишком старая).
	wrongDate := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: wrongDate, Kind: memory.KindProcedural, Scope: "project:agentmemory",
		Content: "комбинированный рецепт устаревший", Importance: 3,
		CreatedAt: 50, UpdatedAt: 50, Tags: []string{"стек"},
	}); err != nil {
		t.Fatalf("Put wrongDate: %v", err)
	}

	cands, err := s.SearchLexical(ctx, memory.Query{
		Text: "комбинированный", Scope: "project:agentmemory",
		Kinds: []string{"procedural"}, Tags: []string{"стек"},
		CreatedAfter: 100, Limit: 10,
	})
	if err != nil {
		t.Fatalf("SearchLexical: %v", err)
	}
	if len(cands) != 1 || cands[0].Memory.Id != match {
		t.Fatalf("candidates = %+v, want [match] (только запись, прошедшая все фильтры сразу)", cands)
	}
}

func TestSupersede(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	oldID, newIDVal := newID(t), newID(t)
	if err := s.Put(ctx, memory.Memory{Id: oldID, Kind: memory.KindSemantic, Scope: "s", Content: "старое", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatalf("Put old: %v", err)
	}
	if err := s.Put(ctx, memory.Memory{Id: newIDVal, Kind: memory.KindSemantic, Scope: "s", Content: "новое", CreatedAt: 2, UpdatedAt: 2}); err != nil {
		t.Fatalf("Put new: %v", err)
	}

	s.now = func() int64 { return 12345 }
	if err := s.Supersede(ctx, oldID, newIDVal); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	got, err := s.Get(ctx, oldID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.SupersededBy == nil || *got.SupersededBy != newIDVal {
		t.Errorf("SupersededBy = %v, want %q", got.SupersededBy, newIDVal)
	}
	if got.UpdatedAt != 12345 {
		t.Errorf("UpdatedAt = %d, want 12345", got.UpdatedAt)
	}

	if err := s.Supersede(ctx, newID(t), newIDVal); err == nil {
		t.Error("Supersede несуществующего oldID должен вернуть ошибку")
	}
}

func TestTouch(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	id := newID(t)
	if err := s.Put(ctx, memory.Memory{Id: id, Kind: memory.KindEpisodic, Scope: "s", Content: "событие", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	s.now = func() int64 { return 42 }
	if err := s.Touch(ctx, id); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	got, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AccessedAt == nil || *got.AccessedAt != 42 {
		t.Errorf("AccessedAt = %v, want 42", got.AccessedAt)
	}
	if got.AccessCount != 1 {
		t.Errorf("AccessCount = %d, want 1", got.AccessCount)
	}

	s.now = func() int64 { return 43 }
	if err := s.Touch(ctx, id); err != nil {
		t.Fatalf("Touch 2: %v", err)
	}
	got, err = s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get 2: %v", err)
	}
	if got.AccessCount != 2 {
		t.Errorf("AccessCount = %d, want 2", got.AccessCount)
	}
	if got.AccessedAt == nil || *got.AccessedAt != 43 {
		t.Errorf("AccessedAt = %v, want 43", got.AccessedAt)
	}
}

func TestNeighbors(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	a, b, c := newID(t), newID(t), newID(t)
	if err := s.Put(ctx, memory.Memory{Id: a, Kind: memory.KindSemantic, Scope: "s", Content: "A", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatalf("Put a: %v", err)
	}
	if err := s.Put(ctx, memory.Memory{Id: b, Kind: memory.KindSemantic, Scope: "s", Content: "B", CreatedAt: 2, UpdatedAt: 2}); err != nil {
		t.Fatalf("Put b: %v", err)
	}
	if err := s.Put(ctx, memory.Memory{
		Id: c, Kind: memory.KindSemantic, Scope: "s", Content: "C", CreatedAt: 3, UpdatedAt: 3,
		Links: []memory.Link{{Dst: a, Rel: memory.RelRelated}},
	}); err != nil {
		t.Fatalf("Put c: %v", err)
	}
	// b -> a
	if err := s.Put(ctx, memory.Memory{
		Id: b, Kind: memory.KindSemantic, Scope: "s", Content: "B", CreatedAt: 2, UpdatedAt: 2,
		Links: []memory.Link{{Dst: a, Rel: memory.RelRelated}},
	}); err != nil {
		t.Fatalf("Put b с link: %v", err)
	}

	neighbors, err := s.Neighbors(ctx, a)
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}
	if len(neighbors) != 2 {
		t.Fatalf("neighbors = %+v, want 2 (b и c, оба ссылаются на a)", neighbors)
	}
	ids := map[string]bool{}
	for _, n := range neighbors {
		ids[n.Id] = true
	}
	if !ids[b] || !ids[c] {
		t.Errorf("neighbors ids = %v, want содержать %q и %q", ids, b, c)
	}
}

func TestNeighborsExcludesSuperseded(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	a, b := newID(t), newID(t)
	if err := s.Put(ctx, memory.Memory{Id: a, Kind: memory.KindSemantic, Scope: "s", Content: "A", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatalf("Put a: %v", err)
	}
	if err := s.Put(ctx, memory.Memory{
		Id: b, Kind: memory.KindSemantic, Scope: "s", Content: "B", CreatedAt: 2, UpdatedAt: 2,
		Links: []memory.Link{{Dst: a, Rel: memory.RelRelated}},
	}); err != nil {
		t.Fatalf("Put b: %v", err)
	}

	neighbors, err := s.Neighbors(ctx, a)
	if err != nil {
		t.Fatalf("Neighbors до supersede: %v", err)
	}
	if len(neighbors) != 1 || neighbors[0].Id != b {
		t.Fatalf("neighbors до supersede = %+v, want [b]", neighbors)
	}

	if err := s.Supersede(ctx, b, a); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	neighbors, err = s.Neighbors(ctx, a)
	if err != nil {
		t.Fatalf("Neighbors после supersede: %v", err)
	}
	if len(neighbors) != 0 {
		t.Fatalf("neighbors после supersede b = %+v, want пусто (b — тумстоун)", neighbors)
	}
}

func TestListEpisodicForConsolidation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	const threshold = int64(1000)

	// старые episodic, scope "a" (два, для проверки сортировки внутри scope по created_at).
	oldA1, oldA2 := newID(t), newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: oldA1, Kind: memory.KindEpisodic, Scope: "a", Content: "старое A1",
		CreatedAt: 100, UpdatedAt: 100, Tags: []string{"t1"},
	}); err != nil {
		t.Fatalf("Put oldA1: %v", err)
	}
	if err := s.Put(ctx, memory.Memory{
		Id: oldA2, Kind: memory.KindEpisodic, Scope: "a", Content: "старое A2",
		CreatedAt: 200, UpdatedAt: 200,
	}); err != nil {
		t.Fatalf("Put oldA2: %v", err)
	}

	// старая episodic, scope "b" (лексикографически раньше "a" быть не должна, "a" < "b").
	oldBID := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: oldBID, Kind: memory.KindEpisodic, Scope: "b", Content: "старое B",
		CreatedAt: 150, UpdatedAt: 150,
	}); err != nil {
		t.Fatalf("Put oldB: %v", err)
	}

	// свежая episodic — не должна попасть.
	freshID := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: freshID, Kind: memory.KindEpisodic, Scope: "a", Content: "свежее",
		CreatedAt: threshold, UpdatedAt: threshold,
	}); err != nil {
		t.Fatalf("Put fresh: %v", err)
	}

	// semantic (даже старая) — не должна попасть.
	semID := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: semID, Kind: memory.KindSemantic, Scope: "a", Content: "старый факт",
		CreatedAt: 50, UpdatedAt: 50,
	}); err != nil {
		t.Fatalf("Put semantic: %v", err)
	}

	// superseded episodic (даже старая) — не должна попасть.
	supersededID := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: supersededID, Kind: memory.KindEpisodic, Scope: "a", Content: "заменённое",
		CreatedAt: 60, UpdatedAt: 60,
	}); err != nil {
		t.Fatalf("Put superseded source: %v", err)
	}
	if err := s.Supersede(ctx, supersededID, oldA1); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	t.Run("выбирает старые episodic, сортирует scope→created_at", func(t *testing.T) {
		got, err := s.ListEpisodicForConsolidation(ctx, threshold, 0)
		if err != nil {
			t.Fatalf("ListEpisodicForConsolidation: %v", err)
		}
		wantIDs := []string{oldA1, oldA2, oldBID}
		if len(got) != len(wantIDs) {
			t.Fatalf("len = %d, want %d (got: %+v)", len(got), len(wantIDs), got)
		}
		for i, id := range wantIDs {
			if got[i].Id != id {
				t.Errorf("got[%d].Id = %q, want %q", i, got[i].Id, id)
			}
		}
		// ListEpisodicForConsolidation намеренно не подгружает Tags/Links (не N+1) —
		// oldA1 сохранён с тегом, но здесь он не должен вернуться.
		if got[0].Tags != nil {
			t.Errorf("got[0].Tags = %v, want nil (Tags не загружаются этим методом)", got[0].Tags)
		}
	})

	t.Run("limit ограничивает выборку", func(t *testing.T) {
		got, err := s.ListEpisodicForConsolidation(ctx, threshold, 2)
		if err != nil {
			t.Fatalf("ListEpisodicForConsolidation: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2", len(got))
		}
		if got[0].Id != oldA1 || got[1].Id != oldA2 {
			t.Errorf("got = %+v, want [oldA1 oldA2]", got)
		}
	})

	t.Run("слишком ранний порог даёт пустой результат", func(t *testing.T) {
		got, err := s.ListEpisodicForConsolidation(ctx, 1, 0)
		if err != nil {
			t.Fatalf("ListEpisodicForConsolidation: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("got = %+v, want пусто", got)
		}
	})
}

func TestListEpisodicForDecay(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	const threshold = int64(1000)

	// давно не трогали, обращений не было — попадает, дата сравнения = created_at.
	staleNoAccess := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: staleNoAccess, Kind: memory.KindEpisodic, Scope: "a", Content: "давно не трогали",
		CreatedAt: 100, UpdatedAt: 100,
	}); err != nil {
		t.Fatalf("Put staleNoAccess: %v", err)
	}

	// последнее обращение раньше threshold, хотя создана позже — попадает, дата
	// сравнения = accessed_at, а не created_at.
	staleAccessed := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: staleAccessed, Kind: memory.KindEpisodic, Scope: "a", Content: "давно доступалось",
		CreatedAt: 300, UpdatedAt: 300, AccessedAt: int64Ptr(200),
	}); err != nil {
		t.Fatalf("Put staleAccessed: %v", err)
	}

	// недавно обращались (Touch) — не должна попасть, хотя создана давно.
	recentlyTouched := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: recentlyTouched, Kind: memory.KindEpisodic, Scope: "a", Content: "недавно трогали",
		CreatedAt: 50, UpdatedAt: 50, AccessedAt: int64Ptr(threshold),
	}); err != nil {
		t.Fatalf("Put recentlyTouched: %v", err)
	}

	// semantic (даже старая, без обращений) — не должна попасть (§2: не устаревает).
	semID := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: semID, Kind: memory.KindSemantic, Scope: "a", Content: "старый факт",
		CreatedAt: 50, UpdatedAt: 50,
	}); err != nil {
		t.Fatalf("Put semantic: %v", err)
	}

	// superseded episodic (даже старая) — не должна попасть.
	supersededID := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: supersededID, Kind: memory.KindEpisodic, Scope: "a", Content: "заменённое",
		CreatedAt: 60, UpdatedAt: 60,
	}); err != nil {
		t.Fatalf("Put superseded source: %v", err)
	}
	if err := s.Supersede(ctx, supersededID, staleNoAccess); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	t.Run("выбирает записи без недавних обращений, сортирует по дате по возрастанию", func(t *testing.T) {
		got, err := s.ListEpisodicForDecay(ctx, threshold, 0)
		if err != nil {
			t.Fatalf("ListEpisodicForDecay: %v", err)
		}
		wantIDs := []string{staleNoAccess, staleAccessed}
		if len(got) != len(wantIDs) {
			t.Fatalf("len = %d, want %d (got: %+v)", len(got), len(wantIDs), got)
		}
		for i, id := range wantIDs {
			if got[i].Id != id {
				t.Errorf("got[%d].Id = %q, want %q", i, got[i].Id, id)
			}
		}
		// Tags/Links намеренно не подгружаются этим методом.
		if got[0].Tags != nil {
			t.Errorf("got[0].Tags = %v, want nil (Tags не загружаются этим методом)", got[0].Tags)
		}
	})

	t.Run("limit ограничивает выборку", func(t *testing.T) {
		got, err := s.ListEpisodicForDecay(ctx, threshold, 1)
		if err != nil {
			t.Fatalf("ListEpisodicForDecay: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("len = %d, want 1", len(got))
		}
		if got[0].Id != staleNoAccess {
			t.Errorf("got[0].Id = %q, want %q", got[0].Id, staleNoAccess)
		}
	})

	t.Run("слишком ранний порог даёт пустой результат", func(t *testing.T) {
		got, err := s.ListEpisodicForDecay(ctx, 1, 0)
		if err != nil {
			t.Fatalf("ListEpisodicForDecay: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("got = %+v, want пусто", got)
		}
	})
}

func TestResetVectorIndexRecreatesTable(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.ResetVectorIndex(ctx, "bge-m3", 3); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}
	exists, err := s.hasVecTable(ctx)
	if err != nil {
		t.Fatalf("hasVecTable: %v", err)
	}
	if !exists {
		t.Fatal("memories_vec не создана после ResetVectorIndex")
	}
	model, dim, err := s.EmbedMeta(ctx)
	if err != nil {
		t.Fatalf("EmbedMeta: %v", err)
	}
	if model != "bge-m3" || dim != 3 {
		t.Fatalf("EmbedMeta = (%q, %d), want (bge-m3, 3)", model, dim)
	}

	id := newID(t)
	if err := s.Put(ctx, memory.Memory{Id: id, Kind: memory.KindSemantic, Scope: "s", Content: "c", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.PutVector(ctx, id, []float32{1, 0, 0}); err != nil {
		t.Fatalf("PutVector: %v", err)
	}

	// Пересоздание с другой размерностью: старые векторы должны исчезнуть, dim в
	// embed_meta — обновиться.
	if err := s.ResetVectorIndex(ctx, "other-model", 4); err != nil {
		t.Fatalf("ResetVectorIndex (2): %v", err)
	}
	model, dim, err = s.EmbedMeta(ctx)
	if err != nil {
		t.Fatalf("EmbedMeta (2): %v", err)
	}
	if model != "other-model" || dim != 4 {
		t.Fatalf("EmbedMeta (2) = (%q, %d), want (other-model, 4)", model, dim)
	}

	cands, err := s.SearchVector(ctx, []float32{1, 0, 0, 0}, memory.Query{Limit: 10})
	if err != nil {
		t.Fatalf("SearchVector после пересоздания: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("candidates = %+v, want пусто (старые векторы должны были исчезнуть)", cands)
	}
}

func TestSearchVectorOrdersByDistance(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.ResetVectorIndex(ctx, "test-model", 3); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}

	near, mid, far := newID(t), newID(t), newID(t)
	for _, tc := range []struct {
		id  string
		vec []float32
		ts  int64
	}{
		{near, []float32{1, 0, 0}, 1},
		{mid, []float32{1, 1, 0}, 2},
		{far, []float32{0, 1, 0}, 3},
	} {
		if err := s.Put(ctx, memory.Memory{
			Id: tc.id, Kind: memory.KindSemantic, Scope: "project:agentmemory",
			Content: "запись " + tc.id, CreatedAt: tc.ts, UpdatedAt: tc.ts,
		}); err != nil {
			t.Fatalf("Put %s: %v", tc.id, err)
		}
		if err := s.PutVector(ctx, tc.id, tc.vec); err != nil {
			t.Fatalf("PutVector %s: %v", tc.id, err)
		}
	}

	cands, err := s.SearchVector(ctx, []float32{1, 0, 0}, memory.Query{Scope: "project:agentmemory", Limit: 10})
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	if len(cands) != 3 {
		t.Fatalf("candidates = %+v, want 3", cands)
	}
	wantOrder := []string{near, mid, far}
	for i, id := range wantOrder {
		if cands[i].Memory.Id != id {
			t.Errorf("cands[%d].Id = %q, want %q (порядок: %+v)", i, cands[i].Memory.Id, id, cands)
		}
		if cands[i].Source != "vector" {
			t.Errorf("cands[%d].Source = %q, want vector", i, cands[i].Source)
		}
	}
	// score убывает по мере удаления (1/(1+distance) монотонно убывает с distance).
	for i := 1; i < len(cands); i++ {
		if cands[i].Score > cands[i-1].Score {
			t.Errorf("score не убывает по расстоянию: %+v", cands)
		}
	}
}

func TestSearchVectorScopesAndSuperseded(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.ResetVectorIndex(ctx, "test-model", 2); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}

	mine, other, supersededID, replacement := newID(t), newID(t), newID(t), newID(t)
	for _, tc := range []struct {
		id, scope string
		vec       []float32
		ts        int64
	}{
		{mine, "project:agentmemory", []float32{1, 0}, 1},
		{other, "project:other", []float32{1, 0}, 2},
		{supersededID, "project:agentmemory", []float32{1, 0}, 3},
		{replacement, "project:agentmemory", []float32{0, 1}, 4},
	} {
		if err := s.Put(ctx, memory.Memory{
			Id: tc.id, Kind: memory.KindSemantic, Scope: tc.scope,
			Content: "c", CreatedAt: tc.ts, UpdatedAt: tc.ts,
		}); err != nil {
			t.Fatalf("Put %s: %v", tc.id, err)
		}
		if err := s.PutVector(ctx, tc.id, tc.vec); err != nil {
			t.Fatalf("PutVector %s: %v", tc.id, err)
		}
	}
	if err := s.Supersede(ctx, supersededID, replacement); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	t.Run("набор scopes видит свой проект и user:*, не видит чужой", func(t *testing.T) {
		cands, err := s.SearchVector(ctx, []float32{1, 0}, memory.Query{
			Scopes: []string{"project:agentmemory", "user:*"}, Limit: 10,
		})
		if err != nil {
			t.Fatalf("SearchVector: %v", err)
		}
		got := map[string]bool{}
		for _, c := range cands {
			got[c.Memory.Id] = true
		}
		if got[other] {
			t.Errorf("candidates содержат запись чужого project:other: %+v", cands)
		}
		if got[supersededID] {
			t.Errorf("candidates содержат superseded запись: %+v", cands)
		}
		if !got[mine] || !got[replacement] {
			t.Errorf("candidates = %+v, want содержать %q и %q", cands, mine, replacement)
		}
	})
}

// TestSearchVectorTagsFilter проверяет, что SearchVector уважает Query.Tags так же,
// как SearchLexical (задача 8.2, DESIGN.md §8: оба источника фильтруют одинаково через
// общий metaFilters), поверх живого sqlite-vec. Все три записи одинаково близки к
// вектору запроса, чтобы единственной переменной был тег.
func TestSearchVectorTagsFilter(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.ResetVectorIndex(ctx, "test-model", 2); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}

	stackID, goID, bothID := newID(t), newID(t), newID(t)
	for _, tc := range []struct {
		id   string
		tags []string
	}{
		{stackID, []string{"стек"}},
		{goID, []string{"go"}},
		{bothID, []string{"стек", "go"}},
	} {
		if err := s.Put(ctx, memory.Memory{
			Id: tc.id, Kind: memory.KindSemantic, Scope: "project:agentmemory",
			Content: "c", CreatedAt: 1, UpdatedAt: 1, Tags: tc.tags,
		}); err != nil {
			t.Fatalf("Put %s: %v", tc.id, err)
		}
		if err := s.PutVector(ctx, tc.id, []float32{1, 0}); err != nil {
			t.Fatalf("PutVector %s: %v", tc.id, err)
		}
	}

	t.Run("один тег", func(t *testing.T) {
		cands, err := s.SearchVector(ctx, []float32{1, 0}, memory.Query{Tags: []string{"стек"}, Limit: 10})
		if err != nil {
			t.Fatalf("SearchVector: %v", err)
		}
		got := map[string]bool{}
		for _, c := range cands {
			got[c.Memory.Id] = true
		}
		if len(got) != 2 || !got[stackID] || !got[bothID] {
			t.Fatalf("candidates = %+v, want [stackID bothID]", cands)
		}
	})

	t.Run("несуществующий тег — пусто", func(t *testing.T) {
		cands, err := s.SearchVector(ctx, []float32{1, 0}, memory.Query{Tags: []string{"нет-такого-тега"}, Limit: 10})
		if err != nil {
			t.Fatalf("SearchVector: %v", err)
		}
		if len(cands) != 0 {
			t.Fatalf("candidates = %+v, want пусто", cands)
		}
	})
}

// TestSearchVectorTagsFilterKBeforeFilterPad проверяет решение по k-before-filter
// (задача 8.2, см. vectorTagFilterPad): при активном Tags-фильтре и Limit меньше
// числа тегированных записей SearchVector всё равно должен вернуть все подходящие по
// тегу записи в пределах Limit, а не недобрать их из-за узкого k=Limit в KNN. Кладём
// много "шумовых" записей БЕЗ нужного тега ближе по дистанции, чем тегированные —
// без запаса k=Limit в KNN тегированные записи не попали бы даже в кандидаты.
func TestSearchVectorTagsFilterKBeforeFilterPad(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.ResetVectorIndex(ctx, "test-model", 2); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}

	const limit = 2
	// Шум: ближе к вектору запроса (меньшая дистанция), без искомого тега — их
	// больше, чем limit, поэтому голый k=limit в KNN целиком вытеснил бы тегированные.
	for i := 0; i < limit+1; i++ {
		id := newID(t)
		if err := s.Put(ctx, memory.Memory{
			Id: id, Kind: memory.KindSemantic, Scope: "project:agentmemory",
			Content: "шум", CreatedAt: int64(i), UpdatedAt: int64(i),
		}); err != nil {
			t.Fatalf("Put шум %s: %v", id, err)
		}
		if err := s.PutVector(ctx, id, []float32{1, 0}); err != nil {
			t.Fatalf("PutVector шум %s: %v", id, err)
		}
	}

	tagged := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: tagged, Kind: memory.KindSemantic, Scope: "project:agentmemory",
		Content: "нужная запись", CreatedAt: 100, UpdatedAt: 100, Tags: []string{"стек"},
	}); err != nil {
		t.Fatalf("Put tagged: %v", err)
	}
	// Дальше по дистанции, чем весь шум (дальше от [1,0]).
	if err := s.PutVector(ctx, tagged, []float32{0, 1}); err != nil {
		t.Fatalf("PutVector tagged: %v", err)
	}

	cands, err := s.SearchVector(ctx, []float32{1, 0}, memory.Query{Tags: []string{"стек"}, Limit: limit})
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	if len(cands) != 1 || cands[0].Memory.Id != tagged {
		t.Fatalf("candidates = %+v, want [tagged] (запас k должен был подтянуть тегированную запись из-за шумовых кандидатов ближе неё)", cands)
	}
}

// TestSearchVectorCreatedAtFilter проверяет, что SearchVector уважает
// CreatedAfter/CreatedBefore так же, как SearchLexical (задача 8.2).
func TestSearchVectorCreatedAtFilter(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.ResetVectorIndex(ctx, "test-model", 2); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}

	old, recent := newID(t), newID(t)
	for _, tc := range []struct {
		id string
		ts int64
	}{
		{old, 100},
		{recent, 300},
	} {
		if err := s.Put(ctx, memory.Memory{
			Id: tc.id, Kind: memory.KindSemantic, Scope: "project:agentmemory",
			Content: "c", CreatedAt: tc.ts, UpdatedAt: tc.ts,
		}); err != nil {
			t.Fatalf("Put %s: %v", tc.id, err)
		}
		if err := s.PutVector(ctx, tc.id, []float32{1, 0}); err != nil {
			t.Fatalf("PutVector %s: %v", tc.id, err)
		}
	}

	cands, err := s.SearchVector(ctx, []float32{1, 0}, memory.Query{CreatedAfter: 200, Limit: 10})
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	if len(cands) != 1 || cands[0].Memory.Id != recent {
		t.Fatalf("candidates = %+v, want [recent]", cands)
	}
}

func TestSearchVectorWithoutVecTable(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	cands, err := s.SearchVector(ctx, []float32{1, 0, 0}, memory.Query{Limit: 10})
	if err != nil {
		t.Fatalf("SearchVector без vec-таблицы должен вернуть nil,nil: %v", err)
	}
	if cands != nil {
		t.Fatalf("candidates = %+v, want nil", cands)
	}
}

func TestSearchVectorEmptyQueryVector(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Индекс существует, но вектор запроса пуст (recall передаёт nil до Этапа 3.4):
	// деградация до пустого результата без ошибки, а не dimension mismatch в MATCH.
	if err := s.ResetVectorIndex(ctx, "test-model", 3); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}
	id := newID(t)
	if err := s.Put(ctx, memory.Memory{Id: id, Kind: memory.KindSemantic, Scope: "s", Content: "c", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.PutVector(ctx, id, []float32{1, 0, 0}); err != nil {
		t.Fatalf("PutVector: %v", err)
	}

	for _, vec := range [][]float32{nil, {}} {
		cands, err := s.SearchVector(ctx, vec, memory.Query{Limit: 10})
		if err != nil {
			t.Fatalf("SearchVector с пустым вектором (%v) должен вернуть nil,nil: %v", vec, err)
		}
		if cands != nil {
			t.Fatalf("candidates = %+v, want nil", cands)
		}
	}
}

func TestPutVectorWithoutVecTableFails(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	id := newID(t)
	if err := s.Put(ctx, memory.Memory{Id: id, Kind: memory.KindSemantic, Scope: "s", Content: "c", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.PutVector(ctx, id, []float32{1, 0, 0}); err == nil {
		t.Error("PutVector без ResetVectorIndex должен вернуть ошибку")
	}
}

func TestAllForReindex(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	kept, superseded, replacement := newID(t), newID(t), newID(t)
	if err := s.Put(ctx, memory.Memory{Id: replacement, Kind: memory.KindSemantic, Scope: "s", Content: "замена", CreatedAt: 2, UpdatedAt: 2}); err != nil {
		t.Fatalf("Put replacement: %v", err)
	}
	if err := s.Put(ctx, memory.Memory{
		Id: kept, Kind: memory.KindSemantic, Scope: "s", Content: "живая запись",
		CreatedAt: 1, UpdatedAt: 1, Tags: []string{"go"},
		Links: []memory.Link{{Dst: replacement, Rel: memory.RelRelated}},
	}); err != nil {
		t.Fatalf("Put kept: %v", err)
	}
	if err := s.Put(ctx, memory.Memory{Id: superseded, Kind: memory.KindSemantic, Scope: "s", Content: "устаревшая", CreatedAt: 3, UpdatedAt: 3}); err != nil {
		t.Fatalf("Put superseded: %v", err)
	}
	if err := s.Supersede(ctx, superseded, replacement); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	all, err := s.AllForReindex(ctx)
	if err != nil {
		t.Fatalf("AllForReindex: %v", err)
	}
	got := map[string]memory.Memory{}
	for _, m := range all {
		got[m.Id] = m
	}
	if len(all) != 2 {
		t.Fatalf("AllForReindex = %+v, want 2 записи (kept, replacement)", all)
	}
	if _, ok := got[superseded]; ok {
		t.Errorf("AllForReindex вернул superseded запись: %+v", all)
	}
	keptGot, ok := got[kept]
	if !ok {
		t.Fatalf("AllForReindex не содержит %q: %+v", kept, all)
	}
	if len(keptGot.Tags) != 1 || keptGot.Tags[0] != "go" {
		t.Errorf("AllForReindex не подгрузил Tags: %+v", keptGot)
	}
	if len(keptGot.Links) != 1 || keptGot.Links[0].Dst != replacement {
		t.Errorf("AllForReindex не подгрузил Links: %+v", keptGot)
	}
}

func TestListMissingVectorsWithoutVecTable(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	id := newID(t)
	if err := s.Put(ctx, memory.Memory{Id: id, Kind: memory.KindSemantic, Scope: "s", Content: "c", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	missing, err := s.ListMissingVectors(ctx, 0)
	if err != nil {
		t.Fatalf("ListMissingVectors без vec-таблицы должен вернуть nil,nil: %v", err)
	}
	if missing != nil {
		t.Fatalf("missing = %+v, want nil", missing)
	}
}

func TestListMissingVectors(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.ResetVectorIndex(ctx, "test-model", 3); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}

	withVec, withoutVec, superseded := newID(t), newID(t), newID(t)
	if err := s.Put(ctx, memory.Memory{Id: withVec, Kind: memory.KindSemantic, Scope: "s", Content: "с вектором", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatalf("Put withVec: %v", err)
	}
	if err := s.PutVector(ctx, withVec, []float32{1, 0, 0}); err != nil {
		t.Fatalf("PutVector: %v", err)
	}
	if err := s.Put(ctx, memory.Memory{Id: withoutVec, Kind: memory.KindSemantic, Scope: "s", Content: "без вектора", CreatedAt: 2, UpdatedAt: 2}); err != nil {
		t.Fatalf("Put withoutVec: %v", err)
	}
	if err := s.Put(ctx, memory.Memory{Id: superseded, Kind: memory.KindSemantic, Scope: "s", Content: "устаревшая без вектора", CreatedAt: 3, UpdatedAt: 3}); err != nil {
		t.Fatalf("Put superseded: %v", err)
	}
	if err := s.Supersede(ctx, superseded, withVec); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	missing, err := s.ListMissingVectors(ctx, 0)
	if err != nil {
		t.Fatalf("ListMissingVectors: %v", err)
	}
	if len(missing) != 1 || missing[0].Id != withoutVec {
		t.Fatalf("ListMissingVectors = %+v, want ровно [%q]", missing, withoutVec)
	}
}

func TestListPaginationAndOrder(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	var ids []string
	for i := 0; i < 5; i++ {
		id := newID(t)
		ids = append(ids, id)
		if err := s.Put(ctx, memory.Memory{
			Id: id, Kind: memory.KindSemantic, Scope: "s", Content: "запись",
			CreatedAt: int64(i), UpdatedAt: int64(i), Tags: []string{"t" + id[:1]},
		}); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	// ожидаемый порядок: created_at DESC, т.е. обратный ids.
	wantOrder := make([]string, len(ids))
	for i, id := range ids {
		wantOrder[len(ids)-1-i] = id
	}

	t.Run("страница 1: Limit=2 Offset=0, total не зависит от страницы", func(t *testing.T) {
		items, total, err := s.List(ctx, BrowseFilter{Limit: 2, Offset: 0})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if total != 5 {
			t.Errorf("total = %d, want 5", total)
		}
		if len(items) != 2 || items[0].Id != wantOrder[0] || items[1].Id != wantOrder[1] {
			t.Fatalf("items = %+v, want [%q %q]", items, wantOrder[0], wantOrder[1])
		}
		if len(items[0].Tags) != 1 {
			t.Errorf("items[0].Tags = %v, want заполнены", items[0].Tags)
		}
		if items[0].Links != nil {
			t.Errorf("items[0].Links = %v, want nil (List не грузит Links)", items[0].Links)
		}
	})

	t.Run("страница 2: Limit=2 Offset=2", func(t *testing.T) {
		items, total, err := s.List(ctx, BrowseFilter{Limit: 2, Offset: 2})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if total != 5 {
			t.Errorf("total = %d, want 5", total)
		}
		if len(items) != 2 || items[0].Id != wantOrder[2] || items[1].Id != wantOrder[3] {
			t.Fatalf("items = %+v, want [%q %q]", items, wantOrder[2], wantOrder[3])
		}
	})
}

func TestListFiltersScopeAndKind(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	aSem, aProc, bSem := newID(t), newID(t), newID(t)
	if err := s.Put(ctx, memory.Memory{Id: aSem, Kind: memory.KindSemantic, Scope: "project:a", Content: "c", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatalf("Put aSem: %v", err)
	}
	if err := s.Put(ctx, memory.Memory{Id: aProc, Kind: memory.KindProcedural, Scope: "project:a", Content: "c", CreatedAt: 2, UpdatedAt: 2}); err != nil {
		t.Fatalf("Put aProc: %v", err)
	}
	if err := s.Put(ctx, memory.Memory{Id: bSem, Kind: memory.KindSemantic, Scope: "project:b", Content: "c", CreatedAt: 3, UpdatedAt: 3}); err != nil {
		t.Fatalf("Put bSem: %v", err)
	}

	t.Run("фильтр по scope", func(t *testing.T) {
		items, total, err := s.List(ctx, BrowseFilter{Scopes: []string{"project:a"}, Limit: 10})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if total != 2 || len(items) != 2 {
			t.Fatalf("items/total = %+v/%d, want 2/2", items, total)
		}
		for _, m := range items {
			if m.Scope != "project:a" {
				t.Errorf("m.Scope = %q, want project:a", m.Scope)
			}
		}
	})

	t.Run("фильтр по kind", func(t *testing.T) {
		items, total, err := s.List(ctx, BrowseFilter{Kinds: []string{"procedural"}, Limit: 10})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if total != 1 || len(items) != 1 || items[0].Id != aProc {
			t.Fatalf("items/total = %+v/%d, want [aProc]/1", items, total)
		}
	})

	t.Run("комбинация scope+kind", func(t *testing.T) {
		items, total, err := s.List(ctx, BrowseFilter{Scopes: []string{"project:a"}, Kinds: []string{"semantic"}, Limit: 10})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if total != 1 || len(items) != 1 || items[0].Id != aSem {
			t.Fatalf("items/total = %+v/%d, want [aSem]/1", items, total)
		}
	})
}

func TestListSupersededVisibility(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	live, superseded, replacement := newID(t), newID(t), newID(t)
	if err := s.Put(ctx, memory.Memory{Id: replacement, Kind: memory.KindSemantic, Scope: "s", Content: "замена", CreatedAt: 3, UpdatedAt: 3}); err != nil {
		t.Fatalf("Put replacement: %v", err)
	}
	if err := s.Put(ctx, memory.Memory{Id: live, Kind: memory.KindSemantic, Scope: "s", Content: "живая", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatalf("Put live: %v", err)
	}
	if err := s.Put(ctx, memory.Memory{Id: superseded, Kind: memory.KindSemantic, Scope: "s", Content: "устаревшая", CreatedAt: 2, UpdatedAt: 2}); err != nil {
		t.Fatalf("Put superseded: %v", err)
	}
	if err := s.Supersede(ctx, superseded, replacement); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	t.Run("по умолчанию superseded исключены", func(t *testing.T) {
		items, total, err := s.List(ctx, BrowseFilter{Limit: 10})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if total != 2 {
			t.Fatalf("total = %d, want 2", total)
		}
		for _, m := range items {
			if m.Id == superseded {
				t.Errorf("items содержат superseded запись: %+v", items)
			}
		}
	})

	t.Run("IncludeSuperseded=true включает архивные, total растёт", func(t *testing.T) {
		items, total, err := s.List(ctx, BrowseFilter{Limit: 10, IncludeSuperseded: true})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if total != 3 || len(items) != 3 {
			t.Fatalf("items/total = %+v/%d, want 3/3", items, total)
		}
	})
}

func TestListBoundaries(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	for i := 0; i < 3; i++ {
		id := newID(t)
		if err := s.Put(ctx, memory.Memory{Id: id, Kind: memory.KindSemantic, Scope: "s", Content: "c", CreatedAt: int64(i), UpdatedAt: int64(i)}); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}

	t.Run("Limit<=0 использует дефолт", func(t *testing.T) {
		items, total, err := s.List(ctx, BrowseFilter{Limit: 0})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if total != 3 || len(items) != 3 {
			t.Fatalf("items/total = %+v/%d, want 3/3 (дефолтный лимит покрывает все 3 записи)", items, total)
		}
	})

	t.Run("Offset за пределами total даёт пустой items и корректный total", func(t *testing.T) {
		items, total, err := s.List(ctx, BrowseFilter{Limit: 10, Offset: 100})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if total != 3 {
			t.Errorf("total = %d, want 3", total)
		}
		if len(items) != 0 {
			t.Errorf("items = %+v, want пусто", items)
		}
	})

	t.Run("отрицательный Offset трактуется как 0", func(t *testing.T) {
		items, _, err := s.List(ctx, BrowseFilter{Limit: 10, Offset: -5})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(items) != 3 {
			t.Errorf("items = %+v, want 3 (Offset<0 -> 0)", items)
		}
	})
}

func TestStats(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	t.Run("пустая БД: нули и пустые карты", func(t *testing.T) {
		stats, err := s.Stats(ctx)
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
		if stats.Total != 0 || stats.Superseded != 0 {
			t.Errorf("stats = %+v, want нули", stats)
		}
		if stats.ByScope == nil || len(stats.ByScope) != 0 {
			t.Errorf("ByScope = %v, want пустая непустая карта", stats.ByScope)
		}
		if stats.ByKind == nil || len(stats.ByKind) != 0 {
			t.Errorf("ByKind = %v, want пустая непустая карта", stats.ByKind)
		}
	})

	aSem, aProc, bSem, superseded := newID(t), newID(t), newID(t), newID(t)
	if err := s.Put(ctx, memory.Memory{Id: aSem, Kind: memory.KindSemantic, Scope: "project:a", Content: "c", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatalf("Put aSem: %v", err)
	}
	if err := s.Put(ctx, memory.Memory{Id: aProc, Kind: memory.KindProcedural, Scope: "project:a", Content: "c", CreatedAt: 2, UpdatedAt: 2}); err != nil {
		t.Fatalf("Put aProc: %v", err)
	}
	if err := s.Put(ctx, memory.Memory{Id: bSem, Kind: memory.KindSemantic, Scope: "project:b", Content: "c", CreatedAt: 3, UpdatedAt: 3}); err != nil {
		t.Fatalf("Put bSem: %v", err)
	}
	if err := s.Put(ctx, memory.Memory{Id: superseded, Kind: memory.KindSemantic, Scope: "project:a", Content: "c", CreatedAt: 4, UpdatedAt: 4}); err != nil {
		t.Fatalf("Put superseded: %v", err)
	}

	t.Run("до supersede: 4 живых, разбивки по scope/kind", func(t *testing.T) {
		stats, err := s.Stats(ctx)
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
		if stats.Total != 4 || stats.Superseded != 0 {
			t.Fatalf("stats = %+v, want Total=4 Superseded=0", stats)
		}
		if stats.ByScope["project:a"] != 3 || stats.ByScope["project:b"] != 1 {
			t.Errorf("ByScope = %v, want project:a=3 project:b=1", stats.ByScope)
		}
		if stats.ByKind["semantic"] != 3 || stats.ByKind["procedural"] != 1 {
			t.Errorf("ByKind = %v, want semantic=3 procedural=1", stats.ByKind)
		}
	})

	if err := s.Supersede(ctx, superseded, aSem); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	t.Run("после supersede: живой счётчик падает, Superseded растёт", func(t *testing.T) {
		stats, err := s.Stats(ctx)
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
		if stats.Total != 3 {
			t.Errorf("Total = %d, want 3", stats.Total)
		}
		if stats.Superseded != 1 {
			t.Errorf("Superseded = %d, want 1", stats.Superseded)
		}
		if stats.ByScope["project:a"] != 2 {
			t.Errorf("ByScope[project:a] = %d, want 2 (superseded запись больше не живая)", stats.ByScope["project:a"])
		}
	})
}

func TestEmbedMetaEmptyByDefault(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	model, dim, err := s.EmbedMeta(ctx)
	if err != nil {
		t.Fatalf("EmbedMeta: %v", err)
	}
	if model != "" || dim != 0 {
		t.Errorf("EmbedMeta = (%q, %d), want (\"\", 0)", model, dim)
	}

	if err := s.ResetVectorIndex(ctx, "bge-m3", 1024); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}
	model, dim, err = s.EmbedMeta(ctx)
	if err != nil {
		t.Fatalf("EmbedMeta после ResetVectorIndex: %v", err)
	}
	if model != "bge-m3" || dim != 1024 {
		t.Errorf("EmbedMeta = (%q, %d), want (bge-m3, 1024)", model, dim)
	}
}

func TestVectorStatusWithoutVecTable(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	id := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: id, Kind: memory.KindSemantic, Scope: "project:agentmemory",
		Content: "c", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	status, err := s.VectorStatus(ctx, id)
	if err != nil {
		t.Fatalf("VectorStatus без vec-таблицы: %v", err)
	}
	if status != (VectorStatus{}) {
		t.Errorf("VectorStatus = %+v, want нулевое значение", status)
	}
}

func TestVectorStatusNotEmbedded(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.ResetVectorIndex(ctx, "bge-m3", 3); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}

	id := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: id, Kind: memory.KindSemantic, Scope: "project:agentmemory",
		Content: "c", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	status, err := s.VectorStatus(ctx, id)
	if err != nil {
		t.Fatalf("VectorStatus: %v", err)
	}
	want := VectorStatus{Embedded: false, Model: "bge-m3", Dim: 3}
	if status != want {
		t.Errorf("VectorStatus = %+v, want %+v", status, want)
	}
}

func TestVectorStatusEmbedded(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.ResetVectorIndex(ctx, "bge-m3", 3); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}

	id := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: id, Kind: memory.KindSemantic, Scope: "project:agentmemory",
		Content: "c", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.PutVector(ctx, id, []float32{1, 0, 0}); err != nil {
		t.Fatalf("PutVector: %v", err)
	}

	status, err := s.VectorStatus(ctx, id)
	if err != nil {
		t.Fatalf("VectorStatus: %v", err)
	}
	want := VectorStatus{Embedded: true, Model: "bge-m3", Dim: 3}
	if status != want {
		t.Errorf("VectorStatus = %+v, want %+v", status, want)
	}
}

func TestSemanticNeighborsWithoutVecTable(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	id := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: id, Kind: memory.KindSemantic, Scope: "project:agentmemory",
		Content: "c", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	cands, err := s.SemanticNeighbors(ctx, id, 3)
	if err != nil {
		t.Fatalf("SemanticNeighbors без vec-таблицы: %v", err)
	}
	if cands != nil {
		t.Fatalf("candidates = %+v, want nil", cands)
	}
}

func TestSemanticNeighborsAnchorWithoutVector(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.ResetVectorIndex(ctx, "test-model", 3); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}

	anchor, other := newID(t), newID(t)
	for _, id := range []string{anchor, other} {
		if err := s.Put(ctx, memory.Memory{
			Id: id, Kind: memory.KindSemantic, Scope: "project:agentmemory",
			Content: "c", CreatedAt: 1, UpdatedAt: 1,
		}); err != nil {
			t.Fatalf("Put %s: %v", id, err)
		}
	}
	// у anchor вектора нет; у other — есть.
	if err := s.PutVector(ctx, other, []float32{1, 0, 0}); err != nil {
		t.Fatalf("PutVector: %v", err)
	}

	cands, err := s.SemanticNeighbors(ctx, anchor, 3)
	if err != nil {
		t.Fatalf("SemanticNeighbors без вектора якоря: %v", err)
	}
	if cands != nil {
		t.Fatalf("candidates = %+v, want nil", cands)
	}
}

func TestSemanticNeighborsExcludesSelfAndSuperseded(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.ResetVectorIndex(ctx, "test-model", 3); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}

	anchor, near, far, supersededID, replacement := newID(t), newID(t), newID(t), newID(t), newID(t)
	for i, tc := range []struct {
		id     string
		vec    []float32
		hasVec bool
		ts     int64
	}{
		{anchor, []float32{1, 0, 0}, true, 1},
		{near, []float32{1, 0.1, 0}, true, 2},
		{far, []float32{0, 1, 0}, true, 3},
		{supersededID, []float32{1, 0.05, 0}, true, 4}, // ближе near, но будет superseded
		{replacement, nil, false, 5},                   // без вектора — не должен попадать в выдачу
	} {
		if err := s.Put(ctx, memory.Memory{
			Id: tc.id, Kind: memory.KindSemantic, Scope: "project:agentmemory",
			Content: "запись " + tc.id, CreatedAt: tc.ts, UpdatedAt: tc.ts,
		}); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
		if tc.hasVec {
			if err := s.PutVector(ctx, tc.id, tc.vec); err != nil {
				t.Fatalf("PutVector %d: %v", i, err)
			}
		}
	}
	if err := s.Supersede(ctx, supersededID, replacement); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	cands, err := s.SemanticNeighbors(ctx, anchor, 3)
	if err != nil {
		t.Fatalf("SemanticNeighbors: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("candidates = %+v, want 2 (near, far; anchor и superseded исключены)", cands)
	}
	wantOrder := []string{near, far}
	for i, id := range wantOrder {
		if cands[i].Memory.Id != id {
			t.Errorf("cands[%d].Id = %q, want %q (порядок: %+v)", i, cands[i].Memory.Id, id, cands)
		}
		if cands[i].Source != "vector" {
			t.Errorf("cands[%d].Source = %q, want vector", i, cands[i].Source)
		}
	}
	// Score — честная дистанция, возрастает с удалением.
	for i := 1; i < len(cands); i++ {
		if cands[i].Score < cands[i-1].Score {
			t.Errorf("Score (дистанция) не возрастает: %+v", cands)
		}
	}
	for _, c := range cands {
		if c.Memory.Id == anchor {
			t.Errorf("candidates содержат саму запись-якорь: %+v", cands)
		}
		if c.Memory.Id == supersededID {
			t.Errorf("candidates содержат superseded запись: %+v", cands)
		}
	}
}

// TestSemanticNeighborsKTruncation проверяет, что запас (semanticNeighborsPad) над
// k в MATCH-запросе достаточен, чтобы якорь (дистанция 0, всегда попадающий в
// top-k выборки vec0) не вытеснял настоящих соседей: при 10 реальных соседях и
// k=3 должны вернуться ровно 3 ближайшие записи, а не меньше.
func TestSemanticNeighborsKTruncation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.ResetVectorIndex(ctx, "test-model", 2); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}

	anchor := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: anchor, Kind: memory.KindSemantic, Scope: "project:agentmemory",
		Content: "anchor", CreatedAt: 0, UpdatedAt: 0,
	}); err != nil {
		t.Fatalf("Put anchor: %v", err)
	}
	if err := s.PutVector(ctx, anchor, []float32{1, 0}); err != nil {
		t.Fatalf("PutVector anchor: %v", err)
	}

	const n = 10
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		id := newID(t)
		ids[i] = id
		// углы возрастают вместе с i => cosine-дистанция до anchor([1,0]) тоже
		// монотонно возрастает вместе с i.
		angle := float64(i+1) * math.Pi / (2 * (n + 1))
		vec := []float32{float32(math.Cos(angle)), float32(math.Sin(angle))}
		if err := s.Put(ctx, memory.Memory{
			Id: id, Kind: memory.KindSemantic, Scope: "project:agentmemory",
			Content: "запись " + id, CreatedAt: int64(i + 1), UpdatedAt: int64(i + 1),
		}); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
		if err := s.PutVector(ctx, id, vec); err != nil {
			t.Fatalf("PutVector %d: %v", i, err)
		}
	}

	const k = 3
	cands, err := s.SemanticNeighbors(ctx, anchor, k)
	if err != nil {
		t.Fatalf("SemanticNeighbors: %v", err)
	}
	if len(cands) != k {
		t.Fatalf("candidates = %+v (len %d), want %d", cands, len(cands), k)
	}
	wantOrder := ids[:k]
	for i, id := range wantOrder {
		if cands[i].Memory.Id != id {
			t.Errorf("cands[%d].Id = %q, want %q (порядок: %+v)", i, cands[i].Memory.Id, id, cands)
		}
	}
}
