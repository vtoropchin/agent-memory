package maint

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"agentmemory/internal/memory"
	"agentmemory/internal/memory/embed"
	"agentmemory/internal/memory/store"
	"agentmemory/internal/memory/write"
)

// putEpisodicWith вставляет episodic-запись напрямую в Store (в обход write path) с
// явными importance/accessedAt — decay реагирует именно на эти поля, а putEpisodic
// (см. consolidate_test.go) их не выставляет.
func putEpisodicWith(t *testing.T, s store.Store, scope, content string, createdAt int64, importance int, accessedAt *int64) memory.Memory {
	t.Helper()
	m := memory.Memory{
		Id:         ulid.Make().String(),
		Kind:       memory.KindEpisodic,
		Scope:      scope,
		Content:    content,
		Importance: importance,
		CreatedAt:  createdAt,
		UpdatedAt:  createdAt,
		AccessedAt: accessedAt,
	}
	if err := s.Put(context.Background(), m); err != nil {
		t.Fatalf("putEpisodicWith: %v", err)
	}
	return m
}

func TestDecayDemotesStaleEpisodic(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	w := write.New(s, embed.NewNoop())
	d := NewDecayer(s, w)

	longAgo := time.Now().Add(-30 * 24 * time.Hour).Unix()
	m := putEpisodicWith(t, s, "project:x", "давно не трогали", longAgo, 3, nil)

	// archiveAfter заведомо огромный — запись не должна архивироваться, только
	// понизиться по importance.
	result, err := d.Decay(ctx, time.Hour, 365*24*time.Hour, 0)
	if err != nil {
		t.Fatalf("Decay: %v", err)
	}
	if result.Scanned != 1 {
		t.Errorf("Scanned = %d, want 1", result.Scanned)
	}
	if result.Demoted != 1 {
		t.Errorf("Demoted = %d, want 1", result.Demoted)
	}
	if result.Archived != 0 {
		t.Errorf("Archived = %d, want 0", result.Archived)
	}

	got, err := s.Get(ctx, m.Id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Importance != 2 {
		t.Errorf("Importance = %d, want 2", got.Importance)
	}
	if got.SupersededBy != nil {
		t.Errorf("SupersededBy = %v, want nil", got.SupersededBy)
	}
}

func TestDecayArchivesAtImportanceFloor(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	w := write.New(s, embed.NewNoop())
	d := NewDecayer(s, w)

	longAgo := time.Now().Add(-1000 * 24 * time.Hour).Unix()
	m := putEpisodicWith(t, s, "project:x", "совсем старое", longAgo, 1, nil)

	result, err := d.Decay(ctx, time.Hour, 48*time.Hour, 0)
	if err != nil {
		t.Fatalf("Decay: %v", err)
	}
	if result.Scanned != 1 {
		t.Errorf("Scanned = %d, want 1", result.Scanned)
	}
	if result.Archived != 1 {
		t.Errorf("Archived = %d, want 1", result.Archived)
	}
	if result.Demoted != 0 {
		t.Errorf("Demoted = %d, want 0", result.Demoted)
	}

	got, err := s.Get(ctx, m.Id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.SupersededBy == nil || *got.SupersededBy != m.Id {
		t.Errorf("SupersededBy = %v, want self-supersede %q", got.SupersededBy, m.Id)
	}

	cands, err := s.SearchLexical(ctx, memory.Query{Text: "старое", Scope: "project:x", Kinds: []string{"episodic"}, Limit: 10})
	if err != nil {
		t.Fatalf("SearchLexical: %v", err)
	}
	if len(cands) != 0 {
		t.Errorf("SearchLexical после архивации = %+v, want пусто", cands)
	}

	// повторный Decay идемпотентен по архиву: запись уже superseded, значит
	// ListEpisodicForDecay больше её не возвращает — второй прогон её не видит.
	result2, err := d.Decay(ctx, time.Hour, 48*time.Hour, 0)
	if err != nil {
		t.Fatalf("Decay (повторный): %v", err)
	}
	if result2.Scanned != 0 || result2.Archived != 0 || result2.Demoted != 0 {
		t.Errorf("повторный Decay = %+v, want все нули", result2)
	}
}

// TestDecaySkipsNonEpisodic — регрессионная страховка (в т.ч. для нового
// kind=debt, техдолг): Decayer.Decay работает только с episodic (см. годок
// Decayer), значит запись любого другого kind, сколь угодно старая, не должна
// ни понижаться по importance, ни архивироваться. Табличный тест вместо
// отдельных TestDecaySkipsSemantic/TestDecaySkipsDebt с идентичным телом —
// заодно накрывает procedural, для которого раньше не было своего кейса.
func TestDecaySkipsNonEpisodic(t *testing.T) {
	kinds := []memory.Kind{memory.KindSemantic, memory.KindProcedural, memory.KindDebt}
	for _, kind := range kinds {
		t.Run(string(kind), func(t *testing.T) {
			ctx := context.Background()
			s := newTestStore(t)
			w := write.New(s, embed.NewNoop())
			d := NewDecayer(s, w)

			longAgo := time.Now().Add(-1000 * 24 * time.Hour).Unix()
			m := putMemoryWithKind(t, s, "project:x", kind, "устойчивая запись", 3, longAgo)

			result, err := d.Decay(ctx, time.Hour, 48*time.Hour, 0)
			if err != nil {
				t.Fatalf("Decay: %v", err)
			}
			if result.Scanned != 0 {
				t.Errorf("Scanned = %d, want 0 (%s decay не подлежит)", result.Scanned, kind)
			}

			got, err := s.Get(ctx, m.Id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Importance != 3 {
				t.Errorf("Importance = %d, want 3 (не тронуто)", got.Importance)
			}
			if got.SupersededBy != nil {
				t.Errorf("SupersededBy = %v, want nil (не архивировано)", got.SupersededBy)
			}
		})
	}
}

func TestDecaySkipsRecentlyAccessed(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	w := write.New(s, embed.NewNoop())
	d := NewDecayer(s, w)

	longAgo := time.Now().Add(-30 * 24 * time.Hour).Unix()
	m := putEpisodicWith(t, s, "project:x", "старое, но недавно смотрели", longAgo, 3, nil)
	if err := s.Touch(ctx, m.Id); err != nil {
		t.Fatalf("Touch: %v", err)
	}

	result, err := d.Decay(ctx, time.Hour, 48*time.Hour, 0)
	if err != nil {
		t.Fatalf("Decay: %v", err)
	}
	if result.Scanned != 0 {
		t.Errorf("Scanned = %d, want 0 (недавно accessed)", result.Scanned)
	}

	got, err := s.Get(ctx, m.Id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Importance != 3 {
		t.Errorf("Importance = %d, want 3 (не тронуто)", got.Importance)
	}
}
