package store

import (
	"context"
	"testing"

	"agentmemory/internal/memory"
)

// TestSyncLocal_PendingCount: считаются помеченные записи scope видов kinds,
// пометки других видов не снимаются (чтение без записи), другой scope не в счёт.
func TestSyncLocal_PendingCount(t *testing.T) {
	ctx := context.Background()
	s, l := newSyncTestStore(t)
	putTarget(t, l)
	kinds := []memory.Kind{memory.KindSemantic, memory.KindTask}

	if n, err := l.PendingCount(ctx, "project:alpha", kinds); err != nil || n != 0 {
		t.Fatalf("PendingCount пустой базы = %d, %v; хочу 0", n, err)
	}
	putSyncMem(t, s, "project:alpha", memory.KindSemantic, "факт")
	putSyncMem(t, s, "project:alpha", memory.KindSemantic, "ещё факт")
	putSyncMem(t, s, "project:alpha", memory.KindEpisodic, "эпизод")
	putSyncMem(t, s, "project:beta", memory.KindSemantic, "чужой проект")

	if n, err := l.PendingCount(ctx, "project:alpha", kinds); err != nil || n != 2 {
		t.Fatalf("PendingCount = %d, %v; хочу 2", n, err)
	}
	if n, err := l.PendingCount(ctx, "project:alpha", nil); err != nil || n != 3 {
		t.Fatalf("PendingCount без фильтра видов = %d, %v; хочу 3", n, err)
	}
	if got := dirtyIDs(t, l, "project:alpha"); len(got) != 3 {
		t.Errorf("PendingCount снял пометки: осталось %d, хочу 3", len(got))
	}
}
