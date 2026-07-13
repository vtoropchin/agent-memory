package maint

import (
	"context"
	"errors"
	"testing"
	"time"

	"agentmemory/internal/memory"
	"agentmemory/internal/memory/embed"
	"agentmemory/internal/memory/reason"
	"agentmemory/internal/memory/store"
	"agentmemory/internal/memory/write"
)

// newTestScheduler собирает Scheduler поверх переданного Store s и
// reasoner/embedder — те же фейки, что используют остальные тесты пакета
// (echoReasoner/fakeReasoner из consolidate_test.go, reindexFakeEmbedder из
// reindex_test.go).
func newTestScheduler(s store.Store, r reason.Reasoner, e embed.Embedder) *Scheduler {
	w := write.New(s, e)
	return NewScheduler(
		NewExpirer(s),
		NewDecayer(s, w),
		NewDedupSweeper(s),
		NewConsolidator(s, r, w),
		NewReindexer(s, e),
		time.Hour, // interval — RunOnce его не использует напрямую
		time.Hour, // consolidateAfter
	)
}

// TestSchedulerRunOnceCallsAllJobs проверяет, что один прогон RunOnce реально
// затрагивает все пять джобов: expire sweep (запись с истёкшим TTL становится
// тумстоуном), consolidate (эпизод старше consolidateAfter консолидируется через
// echoReasoner), dedup sweep (near-duplicate пара схлопывается) и reindex (у всех
// живых записей появляется вектор).
func TestSchedulerRunOnceCallsAllJobs(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// эпизод старше consolidateAfter (veryOld) — должен быть взят Consolidate.
	// importance=3 (> 1) намеренно: decay (запускается перед consolidate в
	// RunOnce) идле этой записи заведомо превышает и demoteAfter, и
	// archiveAfter, поэтому при importance<=1 decay архивировал бы (self-
	// supersede) запись раньше, чем до неё дошёл бы consolidate — importance=3
	// гарантирует, что decay в этом прогоне только демоутит её, а не архивирует.
	old := putEpisodicWith(t, s, "project:x", "старый эпизод про Go", veryOld, 3, nil)
	// near-duplicate пара semantic-фактов — должна быть схлопнута dedup sweep.
	base := time.Now().Unix()
	putMemoryWith(t, s, "project:x", near1, 2, base)
	putMemoryWith(t, s, "project:x", near2, 4, base)
	// запись с истёкшим TTL (задача 9.4) — должна стать тумстоуном ExpireSweep,
	// первым джобом RunOnce, до того как до неё вообще дошли бы decay/consolidate.
	expired := putMemoryWithTTL(t, s, "project:x", "факт с истёкшим сроком годности", base-10)

	rs := &echoReasoner{}
	fe := &reindexFakeEmbedder{model: "fake-model", dim: 3}
	sched := newTestScheduler(s, rs, fe)

	sched.RunOnce(ctx)

	gotExpired, err := s.Get(ctx, expired.Id)
	if err != nil {
		t.Fatalf("Get expired: %v", err)
	}
	if gotExpired.SupersededBy == nil {
		t.Errorf("expired.SupersededBy = nil, want непусто (expire sweep должен был архивировать запись с истёкшим TTL)")
	}

	if rs.calls == 0 {
		t.Errorf("Reasoner.Complete вызовов = 0, want > 0 (consolidate должен был отработать)")
	}
	gotOld, err := s.Get(ctx, old.Id)
	if err != nil {
		t.Fatalf("Get old: %v", err)
	}
	if gotOld.SupersededBy == nil {
		t.Errorf("old.SupersededBy = nil, want непусто (consolidate должен был его заменить)")
	}

	all, err := s.AllForReindex(ctx)
	if err != nil {
		t.Fatalf("AllForReindex: %v", err)
	}
	// dedup sweep должен был оставить живой только одну из двух near-duplicate
	// записей — если бы sweep не отработал, среди живых были бы обе.
	liveNear := 0
	for _, m := range all {
		if m.Content == near1 || m.Content == near2 {
			liveNear++
		}
	}
	if liveNear != 1 {
		t.Errorf("живых near-duplicate записей = %d, want 1 (dedup sweep должен был схлопнуть пару)", liveNear)
	}

	if fe.calls == 0 {
		t.Errorf("Embed вызовов = 0, want > 0 (reindex должен был отработать)")
	}
	missing, err := s.ListMissingVectors(ctx, 0)
	if err != nil {
		t.Fatalf("ListMissingVectors: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("ListMissingVectors = %+v, want пусто после reindex", missing)
	}
}

// failingDecayListStore оборачивает store.Store, проваливая
// ListEpisodicForDecay — единственную ошибку, которую Decay.Decay
// возвращает наружу вызывающему коду (см. decay.go). Остальные методы не
// затронуты — Consolidate/Sweep/CheckAndReindex должны отработать как обычно.
type failingDecayListStore struct {
	store.Store
}

func (f *failingDecayListStore) ListEpisodicForDecay(ctx context.Context, notAccessedSince int64, limit int) ([]memory.Memory, error) {
	return nil, errors.New("boom: decay listing недоступен")
}

// TestSchedulerRunOnceOneJobErrorDoesNotBlockOthers проверяет, что ошибка
// одного джоба (decay) не мешает остальным трём отработать: consolidate,
// dedup sweep и reindex по-прежнему видны в результате, а RunOnce не паникует
// и не возвращает ошибку наружу (сигнатура RunOnce её и не предполагает).
func TestSchedulerRunOnceOneJobErrorDoesNotBlockOthers(t *testing.T) {
	ctx := context.Background()
	real := newTestStore(t)
	failing := &failingDecayListStore{Store: real}

	old := putEpisodicWith(t, failing, "project:x", "старый эпизод", veryOld, 3, nil)
	base := time.Now().Unix()
	putMemoryWith(t, failing, "project:x", near1, 2, base)
	putMemoryWith(t, failing, "project:x", near2, 4, base)

	rs := &echoReasoner{}
	fe := &reindexFakeEmbedder{model: "fake-model", dim: 3}
	sched := newTestScheduler(failing, rs, fe)

	// не должно паниковать/зависать несмотря на ошибку в decay.
	sched.RunOnce(ctx)

	if rs.calls == 0 {
		t.Errorf("Reasoner.Complete вызовов = 0, want > 0 (consolidate должен был отработать несмотря на ошибку decay)")
	}
	gotOld, err := real.Get(ctx, old.Id)
	if err != nil {
		t.Fatalf("Get old: %v", err)
	}
	if gotOld.SupersededBy == nil {
		t.Errorf("old.SupersededBy = nil, want непусто (consolidate должен был отработать)")
	}

	all, err := real.AllForReindex(ctx)
	if err != nil {
		t.Fatalf("AllForReindex: %v", err)
	}
	liveNear := 0
	for _, m := range all {
		if m.Content == near1 || m.Content == near2 {
			liveNear++
		}
	}
	if liveNear != 1 {
		t.Errorf("живых near-duplicate записей = %d, want 1 (dedup sweep должен был отработать несмотря на ошибку decay)", liveNear)
	}

	if fe.calls == 0 {
		t.Errorf("Embed вызовов = 0, want > 0 (reindex должен был отработать несмотря на ошибку decay)")
	}
}

// TestSchedulerRunRecoversFromPanic проверяет, что паника внутри прогона
// джобов (здесь — искусственная, из подменённого Store) не убивает горутину
// Run: ctx отменяется сразу после первого (панического) прогона, Run должен
// корректно вернуться, а не зависнуть/упасть.
type panicStore struct {
	store.Store
}

func (p *panicStore) ListEpisodicForDecay(ctx context.Context, notAccessedSince int64, limit int) ([]memory.Memory, error) {
	panic("boom: искусственная паника в decay listing")
}

func TestSchedulerRunRecoversFromPanic(t *testing.T) {
	real := newTestStore(t)
	panicking := &panicStore{Store: real}

	sched := newTestScheduler(panicking, reason.NewNoop(), embed.NewNoop())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Run должен сделать ровно один (панический) прогон и сразу выйти по ctx.Done()

	done := make(chan struct{})
	go func() {
		sched.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run не вернулся вовремя после паники в первом прогоне")
	}
}
