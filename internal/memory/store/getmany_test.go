package store

import (
	"context"
	"testing"

	"agentmemory/internal/memory"
)

// parityGetMany: GetMany отдаёт записи по списку id — живые и тумстоуны, с
// Tags и Links, отсутствующие id молча пропускает, пустой список — пустой
// результат (задача 15.4). Общий сценарий для sqlite и postgres (см.
// postgres_test.go TestParityGetMany).
func parityGetMany(t *testing.T, s Store) {
	ctx := context.Background()

	a, b, tomb := newID(t), newID(t), newID(t)
	if err := s.Put(ctx, memory.Memory{Id: a, Kind: memory.KindTask, Scope: "s", Content: "A", Status: memory.StatusTodo, Type: memory.TaskTypeTask, Tags: []string{"x", "y"}, CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatalf("Put a: %v", err)
	}
	if err := s.Put(ctx, memory.Memory{
		Id: b, Kind: memory.KindTask, Scope: "s", Content: "B", Status: memory.StatusDoing, Type: memory.TaskTypeTask, CreatedAt: 2, UpdatedAt: 2,
		Links: []memory.Link{{Dst: a, Rel: memory.RelBlockedBy}},
	}); err != nil {
		t.Fatalf("Put b: %v", err)
	}
	if err := s.Put(ctx, memory.Memory{Id: tomb, Kind: memory.KindSemantic, Scope: "s", Content: "T", CreatedAt: 3, UpdatedAt: 3}); err != nil {
		t.Fatalf("Put tomb: %v", err)
	}
	if err := s.Supersede(ctx, tomb, tomb); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	got, err := s.GetMany(ctx, []string{a, b, tomb, "нет-такого-id"})
	if err != nil {
		t.Fatalf("GetMany: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("GetMany вернул %d записей, want 3 (отсутствующий id пропущен): %+v", len(got), got)
	}
	byID := map[string]memory.Memory{}
	for _, m := range got {
		byID[m.Id] = m
	}
	if ma := byID[a]; len(ma.Tags) != 2 || ma.Tags[0] != "x" || ma.Tags[1] != "y" {
		t.Errorf("Tags[a] = %v, want [x y]", ma.Tags)
	}
	if mb := byID[b]; len(mb.Links) != 1 || mb.Links[0].Dst != a || mb.Links[0].Rel != memory.RelBlockedBy {
		t.Errorf("Links[b] = %+v, want [{%s blocked_by}]", mb.Links, a)
	}
	if mt, ok := byID[tomb]; !ok || mt.SupersededBy == nil {
		t.Errorf("тумстоун %q должен вернуться с SupersededBy: %+v", tomb, mt)
	}

	empty, err := s.GetMany(ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Errorf("GetMany(nil) = %+v, %v; want пусто без ошибки", empty, err)
	}
}

func TestGetMany(t *testing.T) {
	parityGetMany(t, newTestStore(t))
}
