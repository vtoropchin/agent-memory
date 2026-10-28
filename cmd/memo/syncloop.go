// syncloop.go — фоновая синхронизация в долгоживущих процессах memo (Этап 19,
// DESIGN.md §11): MCP-сервер и `memo web` запускают memsync.Scheduler поверх
// локального хранилища; каждый прогон — runSync (lease, Syncer). При
// driver = "postgres" хранилище уже общее — цикл не запускается.
package main

import (
	"context"
	"log"
	"time"

	"agentmemory/internal/config"
	"agentmemory/internal/memory/store"
	memsync "agentmemory/internal/memory/sync"
)

// startSyncLoop запускает фоновую синхронизацию всех целей локальной БД s и
// возвращает stop: он останавливает цикл и ждёт конца текущего прогона —
// вызвать до закрытия хранилища, чтобы прогон успел записать итог и снять
// lease. Ошибки планировщика и прогонов — только в stderr (log), stdout
// MCP-сервера остаётся протоколом.
func startSyncLoop(ctx context.Context, s store.Store, cfg config.StoreConfig) (stop func()) {
	local, ok := store.SyncLocalOf(s)
	if !ok {
		return func() {}
	}
	envDir, err := syncEnvDir(cfg)
	if err != nil {
		log.Printf("memo: фоновая синхронизация выключена: %v", err)
		return func() {}
	}
	sched := memsync.NewScheduler(syncPlan{local}, func(ctx context.Context, project string) (memsync.Result, error) {
		// Имя автора — на каждый прогон: правка профиля в морде действует без перезапуска.
		return runSync(ctx, local, envDir, project, authorName(ctx, s))
	})

	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		sched.Run(ctx)
	}()
	return func() {
		cancel()
		<-done
	}
}

// syncPlan — порт memsync.Plan поверх локального состояния синхронизации:
// цели и интервал из sync_targets/sync_profile, итог прогонов из sync_state.
type syncPlan struct {
	local *store.SyncLocal
}

var _ memsync.Plan = syncPlan{}

func (p syncPlan) Targets(ctx context.Context) ([]memsync.Target, error) {
	ts, err := p.local.Targets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]memsync.Target, len(ts))
	for i, t := range ts {
		out[i] = memsync.Target{Project: t.Project, Push: t.Push, Pull: t.Pull}
	}
	return out, nil
}

func (p syncPlan) Interval(ctx context.Context) (time.Duration, error) {
	prof, err := p.local.Profile(ctx)
	if err != nil {
		return 0, err
	}
	return time.Duration(prof.IntervalSec) * time.Second, nil
}

func (p syncPlan) State(ctx context.Context, project string) (memsync.State, error) {
	return p.local.State(ctx, project)
}
