package sync

import (
	"context"
	"errors"
	"log"
	"time"
)

// Границы ожидания планировщика между проходами: не реже maxWait — цели и
// интервал перечитываются из локальной базы, поэтому правка в морде действует
// без перезапуска процесса; не чаще minWait — защита от холостого цикла.
const (
	maxWait = time.Minute
	minWait = time.Second
)

// defaultInterval — интервал, если Plan вернул неположительный (DESIGN.md §4:
// sync_profile.interval_s по умолчанию 300).
const defaultInterval = 5 * time.Minute

// ErrBusy — цель прогоняет другой процесс memo этой машины (lease не истёк):
// планировщик пропускает её без ошибки.
var ErrBusy = errors.New("синхронизация проекта уже идёт в другом процессе memo")

// Plan — порт планировщика (DESIGN.md §6, §11): цели и интервал из локальной
// базы и итог прошлых прогонов. Интерфейс объявляет потребитель, реализацию
// собирает cmd/memo поверх локального хранилища.
type Plan interface {
	// Targets — все цели синхронизации машины.
	Targets(ctx context.Context) ([]Target, error)
	// Interval — период фоновой синхронизации (общий для всех целей).
	Interval(ctx context.Context) (time.Duration, error)
	// State — итог прогонов цели; нулевой, если прогонов не было.
	State(ctx context.Context, project string) (State, error)
}

// RunFunc — один прогон цели project под lease; ErrBusy — цель занята другим
// процессом. Итог и ошибку прогона в State пишет Syncer.
type RunFunc func(ctx context.Context, project string) (Result, error)

// Scheduler — фоновая синхронизация в долгоживущем процессе memo (DESIGN.md
// §11): прогоняет каждую цель с включённым «Отправлять» или «Загружать», когда
// с её последнего прогона прошёл интервал. Последний прогон — LastRunAt из
// State (его видят все процессы машины), а для прогонов, не записавших итог
// (паника, занятый lease, локальная база новее бинаря), — время попытки этого
// процесса, чтобы не повторять их на каждом проходе. Ошибки и паники
// логируются в stderr, процесс работает дальше (DESIGN.md §15).
type Scheduler struct {
	plan  Plan
	run   RunFunc
	now   func() int64                         // порт времени, unix-секунды (тесты подменяют)
	after func(time.Duration) <-chan time.Time // порт ожидания между проходами (тесты подменяют)
	tried map[string]int64                     // время последней попытки прогона цели этим процессом
}

// NewScheduler собирает планировщик поверх плана и прогона одной цели.
func NewScheduler(plan Plan, run RunFunc) *Scheduler {
	return &Scheduler{
		plan:  plan,
		run:   run,
		now:   func() int64 { return time.Now().Unix() },
		after: time.After,
		tried: map[string]int64{},
	}
}

// Run делает первый проход сразу, затем повторяет их, пока ctx не отменён;
// пауза между проходами — до ближайшей цели, которой пора, но не дольше maxWait.
func (s *Scheduler) Run(ctx context.Context) {
	for {
		wait := s.tickRecovered(ctx)
		select {
		case <-ctx.Done():
			return
		case <-s.after(wait):
		}
	}
}

// tickRecovered — tick под recover: паника плана не останавливает цикл.
func (s *Scheduler) tickRecovered(ctx context.Context) (wait time.Duration) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("sync: scheduler: паника прохода: %v", r)
			wait = maxWait
		}
	}()
	return s.tick(ctx)
}

// tick — один проход: прогоняет цели, которым пора, и возвращает паузу до
// следующего прохода. Ошибка чтения состояния одной цели не мешает другим.
func (s *Scheduler) tick(ctx context.Context) time.Duration {
	targets, err := s.plan.Targets(ctx)
	if err != nil {
		log.Printf("sync: scheduler: цели синхронизации: %v", err)
		return maxWait
	}
	interval, err := s.plan.Interval(ctx)
	if err != nil {
		log.Printf("sync: scheduler: интервал синхронизации: %v", err)
		return maxWait
	}
	if interval <= 0 {
		interval = defaultInterval
	}
	iv := int64(interval / time.Second)

	wait := maxWait
	for _, t := range targets {
		if !t.Push && !t.Pull {
			continue
		}
		if ctx.Err() != nil {
			return wait
		}
		st, err := s.plan.State(ctx, t.Project)
		if err != nil {
			log.Printf("sync: %s: состояние синхронизации: %v", t.Project, err)
			continue
		}
		last := max(st.LastRunAt, s.tried[t.Project])
		if due := last + iv - s.now(); due > 0 {
			wait = min(wait, time.Duration(due)*time.Second)
			continue
		}
		s.tried[t.Project] = s.now()
		s.runTarget(ctx, t.Project)
		wait = min(wait, time.Duration(s.tried[t.Project]+iv-s.now())*time.Second)
	}
	return max(wait, minWait)
}

// runTarget прогоняет одну цель и логирует итог; паника прогона перехвачена.
func (s *Scheduler) runTarget(ctx context.Context, project string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("sync: %s: паника прогона: %v", project, r)
		}
	}()
	res, err := s.run(ctx, project)
	switch {
	case errors.Is(err, ErrBusy), ctx.Err() != nil: // занято или остановка процесса — не сбой
	case err != nil:
		log.Printf("sync: %s: %v", project, err)
	case res.Pushed > 0 || res.Pulled > 0 || res.Conflicts > 0:
		log.Printf("sync: %s: отправлено %d, загружено %d, конфликтов %d", project, res.Pushed, res.Pulled, res.Conflicts)
	}
}
