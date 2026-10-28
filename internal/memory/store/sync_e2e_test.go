package store

import (
	"context"
	"errors"
	"testing"

	"github.com/oklog/ulid/v2"

	"agentmemory/internal/memory"
	memsync "agentmemory/internal/memory/sync"
)

// Интеграционный тест адаптеров синхронизации (задача 19.5): две локальные SQLite
// и общая PostgreSQL (живая, как остальные postgres-тесты; без
// MEMO_TEST_POSTGRES_DSN — пропуск), между ними настоящий memsync.Syncer.

// sharedHub отдаёт Syncer одну и ту же общую базу на каждый Dial; Close — no-op,
// пул закрывает уборка теста.
type sharedHub struct{ h *Hub }

type keepOpen struct{ *Hub }

func (keepOpen) Close() error { return nil }

func (d sharedHub) Dial(context.Context, string) (memsync.Shared, error) { return keepOpen{d.h}, nil }

// syncSide — разработчик: своя SQLite, цель alpha с обоими тумблерами, Syncer.
type syncSide struct {
	s     *sqliteStore
	l     *SyncLocal
	sy    *memsync.Syncer
	name  string
	clock int64
}

func newSyncSide(t *testing.T, h *Hub, name string, start int64) *syncSide {
	t.Helper()
	s, l := newSyncTestStore(t)
	putTarget(t, l)
	side := &syncSide{s: s, l: l, name: name, clock: start}
	s.now = func() int64 { return side.clock }
	side.sy = memsync.New(l, sharedHub{h}, pgSchemaVersion, memsync.WithNow(s.now), memsync.WithLimits(2, 2))
	return side
}

func (d *syncSide) run(t *testing.T) memsync.Result {
	t.Helper()
	res, err := d.sy.Run(context.Background(), memsync.Target{Project: "alpha", Push: true, Pull: true}, d.name)
	if err != nil {
		t.Fatalf("Run(%s): %v (итог %+v)", d.name, err, res)
	}
	return res
}

func (d *syncSide) get(t *testing.T, id string) (memory.Memory, bool) {
	t.Helper()
	m, err := d.s.Get(context.Background(), id)
	if errors.Is(err, ErrNotFound) {
		return memory.Memory{}, false
	}
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return m, true
}

func (d *syncSide) put(t *testing.T, m memory.Memory) memory.Memory {
	t.Helper()
	d.clock++
	if m.Id == "" {
		m.Id, m.CreatedAt = ulid.Make().String(), d.clock
	}
	m.UpdatedAt = d.clock
	if err := d.s.Put(context.Background(), m); err != nil {
		t.Fatalf("Put: %v", err)
	}
	return m
}

func TestSyncE2E_TwoDevelopers(t *testing.T) {
	ctx := context.Background()
	h := newTestHubApplied(t)
	alice := newSyncSide(t, h, "alice", 1000)
	bob := newSyncSide(t, h, "bob", 1000)

	// У alice: задача с тегом, связью на свой эпизод (вне набора) и на факт (в
	// наборе); эпизод и запись user:* не синхронизируются.
	episode := alice.put(t, memory.Memory{Kind: memory.KindEpisodic, Scope: "project:alpha", Content: "эпизод", Importance: 3})
	private := alice.put(t, memory.Memory{Kind: memory.KindSemantic, Scope: "user:*", Content: "личное", Importance: 3})
	fact := alice.put(t, memory.Memory{Kind: memory.KindSemantic, Scope: "project:alpha", Content: "факт", Importance: 3})
	task := alice.put(t, memory.Memory{Kind: memory.KindTask, Scope: "project:alpha", Content: "задача", Importance: 4,
		Status: memory.StatusTodo, Tags: []string{"sync"}, Links: []memory.Link{{Dst: episode.Id}, {Dst: fact.Id, Rel: memory.RelPartOf}}})

	if res := alice.run(t); res.Pushed != 2 || res.Stamped != 2 {
		t.Fatalf("первый push alice = %+v, хочу 2 записи с проставленным автором", res)
	}
	if res := bob.run(t); res.Pulled != 2 || res.Pushed != 0 || res.Conflicts != 0 {
		t.Fatalf("первый pull bob = %+v, хочу 2 записи", res)
	}
	got, ok := bob.get(t, task.Id)
	if !ok || got.Content != "задача" || got.Author != "alice" || got.UpdatedBy != "alice" || got.UpdatedAt != task.UpdatedAt ||
		len(got.Tags) != 1 || len(got.Links) != 1 || got.Links[0].Dst != fact.Id {
		t.Fatalf("задача у bob = %+v (есть %v), хочу версию alice без ребра на её эпизод", got, ok)
	}
	for _, id := range []string{episode.Id, private.Id} {
		if _, ok := bob.get(t, id); ok {
			t.Errorf("запись вне набора %s утекла к bob", id)
		}
	}

	// Правка у bob доходит до alice, а её локальное ребро на эпизод остаётся.
	bob.clock = 2000
	got.Content, got.Status, got.UpdatedBy = "задача v2", memory.StatusDoing, "bob"
	bob.put(t, got)
	if res := bob.run(t); res.Pushed != 1 || res.Conflicts != 0 {
		t.Fatalf("push правки bob = %+v", res)
	}
	alice.clock = 2000
	if res := alice.run(t); res.Pulled != 1 || res.Conflicts != 0 || res.Pushed != 0 {
		t.Fatalf("pull правки alice = %+v", res)
	}
	a, _ := alice.get(t, task.Id)
	if a.Content != "задача v2" || a.Status != memory.StatusDoing || a.UpdatedBy != "bob" || a.Author != "alice" || len(a.Links) != 2 {
		t.Fatalf("задача у alice после pull = %+v, хочу правку bob и обе связи (локальное ребро на эпизод цело)", a)
	}

	// forget у alice — тумстоун доходит до bob.
	alice.clock = 3000
	if err := alice.s.Supersede(ctx, fact.Id, "", "alice"); err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	if res := alice.run(t); res.Pushed != 1 {
		t.Fatalf("push тумстоуна = %+v", res)
	}
	bob.clock = 3000
	if res := bob.run(t); res.Pulled != 1 {
		t.Fatalf("pull тумстоуна = %+v", res)
	}
	if f, _ := bob.get(t, fact.Id); f.SupersededBy == nil || f.UpdatedAt != 3000 {
		t.Fatalf("факт у bob = %+v, хочу тумстоун", f)
	}

	// Повторные прогоны без изменений — ничего не делают.
	for _, side := range []*syncSide{alice, bob, alice, bob} {
		if res := side.run(t); res.Pushed != 0 || res.Pulled != 0 || res.Conflicts != 0 || res.Deferred != 0 {
			t.Fatalf("повторный прогон %s = %+v, хочу 0/0/0", side.name, res)
		}
	}
	for _, side := range []*syncSide{alice, bob} {
		if ps, _ := side.l.Dirty(ctx, "project:alpha", nil, 0, 0); len(ps) != 0 {
			t.Errorf("у %s остались пометки после сходимости: %+v", side.name, ps)
		}
	}
}

// TestSyncE2E_SchemaAheadRefuses: схема общей базы новее бинаря — прогон
// отказывается, ни общая база, ни локальные данные не тронуты, ошибка в статусе.
func TestSyncE2E_SchemaAheadRefuses(t *testing.T) {
	ctx := context.Background()
	h := newTestHubApplied(t)
	alice := newSyncSide(t, h, "alice", 1000)
	alice.put(t, memory.Memory{Kind: memory.KindSemantic, Scope: "project:alpha", Content: "факт", Importance: 3})
	if _, err := h.pg.db.ExecContext(ctx, `UPDATE schema_version SET version = $1`, pgSchemaVersion+1); err != nil {
		t.Fatalf("подъём версии схемы: %v", err)
	}

	_, err := alice.sy.Run(ctx, memsync.Target{Project: "alpha", Push: true, Pull: true}, "alice")
	if !errors.Is(err, memsync.ErrSchemaAhead) {
		t.Fatalf("Run = %v, хочу ErrSchemaAhead", err)
	}
	var n int
	if err := h.pg.db.QueryRowContext(ctx, `SELECT count(*) FROM memories`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("в общей базе %d записей (%v), хочу 0", n, err)
	}
	if ps, _ := alice.l.Dirty(ctx, "project:alpha", nil, 0, 0); len(ps) != 1 {
		t.Fatalf("локальная пометка потеряна: %+v", ps)
	}
	if st, _ := alice.l.State(ctx, "alpha"); st.LastError != memsync.ErrSchemaAhead.Error() {
		t.Fatalf("LastError = %q, хочу %q", st.LastError, memsync.ErrSchemaAhead)
	}
}
