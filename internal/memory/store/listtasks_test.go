package store

import (
	"context"
	"testing"

	"agentmemory/internal/memory"
)

// parityListTasks: ListTasks отдаёт только живые задачи (kind=task, без
// тумстоунов) указанных scopes, с Tags и Links, по updated_at DESC; пустые
// scopes — все scope (задача 15.5). Общий сценарий sqlite/postgres.
func parityListTasks(t *testing.T, s Store) {
	ctx := context.Background()
	put := func(m memory.Memory) {
		t.Helper()
		if err := s.Put(ctx, m); err != nil {
			t.Fatalf("Put %s: %v", m.Id, err)
		}
	}
	older, newer, other, tomb, fact := newID(t), newID(t), newID(t), newID(t), newID(t)
	put(memory.Memory{Id: older, Kind: memory.KindTask, Scope: "project:a", Content: "старая", Status: memory.StatusTodo, Type: memory.TaskTypeTask, Tags: []string{"t1"}, CreatedAt: 1, UpdatedAt: 10})
	put(memory.Memory{Id: newer, Kind: memory.KindTask, Scope: "project:a", Content: "новая", Status: memory.StatusDoing, Type: memory.TaskTypeStory, CreatedAt: 2, UpdatedAt: 20,
		Links: []memory.Link{{Dst: older, Rel: memory.RelBlockedBy}}})
	put(memory.Memory{Id: other, Kind: memory.KindTask, Scope: "project:b", Content: "чужая", Status: memory.StatusDone, Type: memory.TaskTypeTask, CreatedAt: 3, UpdatedAt: 30})
	put(memory.Memory{Id: tomb, Kind: memory.KindTask, Scope: "project:a", Content: "отменённая", Status: memory.StatusTodo, Type: memory.TaskTypeTask, CreatedAt: 4, UpdatedAt: 40})
	put(memory.Memory{Id: fact, Kind: memory.KindSemantic, Scope: "project:a", Content: "факт", CreatedAt: 5, UpdatedAt: 50})
	if err := s.Supersede(ctx, tomb, tomb); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	got, err := s.ListTasks(ctx, []string{"project:a"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(got) != 2 || got[0].Id != newer || got[1].Id != older {
		t.Fatalf("ListTasks(project:a) = %v, want [%s %s] (updated_at DESC, без тумстоуна/факта/чужого scope)", idsOf(got), newer, older)
	}
	if len(got[0].Links) != 1 || got[0].Links[0].Dst != older || got[0].Links[0].Rel != memory.RelBlockedBy {
		t.Errorf("Links новой задачи = %+v, want [{%s blocked_by}]", got[0].Links, older)
	}
	if len(got[1].Tags) != 1 || got[1].Tags[0] != "t1" {
		t.Errorf("Tags старой задачи = %v, want [t1]", got[1].Tags)
	}

	all, err := s.ListTasks(ctx, nil)
	if err != nil {
		t.Fatalf("ListTasks(nil): %v", err)
	}
	if len(all) != 3 || all[0].Id != other {
		t.Errorf("ListTasks(все scope) = %v, want 3 задачи, первая — самая свежая %s", idsOf(all), other)
	}

	none, err := s.ListTasks(ctx, []string{"project:none"})
	if err != nil || len(none) != 0 {
		t.Errorf("ListTasks(пустой scope) = %v, %v; want пусто", idsOf(none), err)
	}
}

func idsOf(ms []memory.Memory) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Id
	}
	return out
}

func TestListTasks(t *testing.T) {
	parityListTasks(t, newTestStore(t))
}
