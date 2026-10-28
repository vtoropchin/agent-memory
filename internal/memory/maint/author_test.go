package maint

import (
	"context"
	"testing"
	"time"

	"agentmemory/internal/memory/embed"
	"agentmemory/internal/memory/write"
)

// TestMaintJobsStampEditor: maint-джобы, собранные с именованным Writer
// (Этап 19), отрабатывают как прежде и ставят updated_by тронутым записям —
// и при понижении importance (Update), и при тумстоуне (Forget); author не
// меняют.
func TestMaintJobsStampEditor(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	w := write.New(s, embed.NewNoop(), write.WithAuthor("Анна"))

	now := time.Now().Unix()
	longAgo := time.Now().Add(-30 * 24 * time.Hour).Unix()
	demote := putEpisodicWith(t, s, "project:x", "давно не трогали, понизить", longAgo, 3, nil)
	archive := putEpisodicWith(t, s, "project:x", "давно не трогали, в архив", longAgo, 1, nil)
	loser := putMemoryWith(t, s, "project:y", near1, 2, now)
	winner := putMemoryWith(t, s, "project:y", near2, 4, now)
	expired := putMemoryWithTTL(t, s, "project:z", "просроченный факт", now-10)

	decay, err := NewDecayer(s, w).Decay(ctx, time.Hour, 24*time.Hour, 0)
	if err != nil {
		t.Fatalf("Decay: %v", err)
	}
	if decay.Demoted != 1 || decay.Archived != 1 {
		t.Errorf("Decay: %+v, want Demoted=1 Archived=1", decay)
	}
	dedup, err := NewDedupSweeper(s, w).Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if dedup.Superseded != 1 {
		t.Errorf("Sweep: %+v, want Superseded=1", dedup)
	}
	exp, err := NewExpirer(s, w).ExpireSweep(ctx, 0)
	if err != nil {
		t.Fatalf("ExpireSweep: %v", err)
	}
	if exp.Archived != 1 {
		t.Errorf("ExpireSweep: %+v, want Archived=1", exp)
	}

	for _, tc := range []struct {
		name, id, updatedBy string
		tombstone           bool
	}{
		{"decay понизил", demote.Id, "Анна", false},
		{"decay архивировал", archive.Id, "Анна", true},
		{"dedup проигравший", loser.Id, "Анна", true},
		{"dedup победитель не тронут", winner.Id, "", false},
		{"expire архивировал", expired.Id, "Анна", true},
	} {
		got, err := s.Get(ctx, tc.id)
		if err != nil {
			t.Fatalf("%s: Get: %v", tc.name, err)
		}
		if got.UpdatedBy != tc.updatedBy || got.Author != "" {
			t.Errorf("%s: author=%q updated_by=%q, want \"\"/%q", tc.name, got.Author, got.UpdatedBy, tc.updatedBy)
		}
		if (got.SupersededBy != nil) != tc.tombstone {
			t.Errorf("%s: SupersededBy = %v, тумстоун ожидается: %v", tc.name, got.SupersededBy, tc.tombstone)
		}
	}
}
