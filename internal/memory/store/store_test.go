package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/oklog/ulid/v2"

	"agentmemory/internal/memory"
)

func newTestStore(t *testing.T) *sqliteStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "memory.db")
	s, err := Open(path)
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

func strPtr(s string) *string { return &s }

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
