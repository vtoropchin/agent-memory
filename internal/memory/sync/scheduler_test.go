package sync

import (
	"bytes"
	"context"
	"errors"
	"log"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakePlan — цели, интервал и итоги прогонов локальной базы.
type fakePlan struct {
	targets   []Target
	interval  time.Duration
	states    map[string]State
	stateErr  map[string]error
	targetErr error
	panicky   bool // Targets паникует
}

func (p *fakePlan) Targets(context.Context) ([]Target, error) {
	if p.panicky {
		panic("сбой плана")
	}
	return p.targets, p.targetErr
}

func (p *fakePlan) Interval(context.Context) (time.Duration, error) { return p.interval, nil }

func (p *fakePlan) State(_ context.Context, project string) (State, error) {
	if err := p.stateErr[project]; err != nil {
		return State{}, err
	}
	return p.states[project], nil
}

// schedFixture — планировщик с часами clock; прогон записывает LastRunAt, как
// Syncer, если для проекта не задан сбой в fail (ошибка) или panics.
type schedFixture struct {
	s      *Scheduler
	plan   *fakePlan
	clock  int64
	ran    []string
	fail   map[string]error
	panics map[string]bool
}

func newSchedFixture(targets ...Target) *schedFixture {
	f := &schedFixture{
		plan:   &fakePlan{targets: targets, interval: 300 * time.Second, states: map[string]State{}, stateErr: map[string]error{}},
		clock:  1000,
		fail:   map[string]error{},
		panics: map[string]bool{},
	}
	f.s = NewScheduler(f.plan, func(_ context.Context, project string) (Result, error) {
		f.ran = append(f.ran, project)
		if f.panics[project] {
			panic("сбой прогона")
		}
		if err := f.fail[project]; err != nil {
			if !errors.Is(err, ErrBusy) {
				f.plan.states[project] = State{LastRunAt: f.clock, LastError: err.Error()}
			}
			return Result{Project: project}, err
		}
		f.plan.states[project] = State{LastRunAt: f.clock, LastOKAt: f.clock}
		return Result{Project: project, Pushed: 1}, nil
	})
	f.s.now = func() int64 { return f.clock }
	return f
}

// tick — проход в момент at; возвращает запущенные цели и паузу.
func (f *schedFixture) tick(at int64) ([]string, time.Duration) {
	f.clock = at
	f.ran = nil
	wait := f.s.tickRecovered(context.Background())
	return f.ran, wait
}

// captureLog перенаправляет стандартный логгер в буфер на время теста.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })
	return &buf
}

func TestScheduler_SkipsTargetsWithTogglesOff(t *testing.T) {
	captureLog(t)
	f := newSchedFixture(
		Target{Project: "off"},
		Target{Project: "push", Push: true},
		Target{Project: "pull", Pull: true},
	)
	ran, _ := f.tick(1000)
	if !slices.Equal(ran, []string{"push", "pull"}) {
		t.Fatalf("запущены %v, хочу [push pull]", ran)
	}
}

func TestScheduler_RespectsInterval(t *testing.T) {
	captureLog(t)
	f := newSchedFixture(Target{Project: "a", Push: true})
	f.plan.interval = 50 * time.Second

	if ran, wait := f.tick(1000); len(ran) != 1 || wait != 50*time.Second {
		t.Fatalf("первый проход: %v, пауза %v; хочу [a] и 50s", ran, wait)
	}
	if ran, wait := f.tick(1020); len(ran) != 0 || wait != 30*time.Second {
		t.Fatalf("до интервала: %v, пауза %v; хочу [] и 30s", ran, wait)
	}
	if ran, _ := f.tick(1050); len(ran) != 1 {
		t.Fatalf("интервал прошёл: %v, хочу [a]", ran)
	}

	// Прогон другого процесса машины сдвигает срок: LastRunAt общий.
	f.plan.states["a"] = State{LastRunAt: 1080}
	if ran, wait := f.tick(1110); len(ran) != 0 || wait != 20*time.Second {
		t.Fatalf("после чужого прогона: %v, пауза %v; хочу [] и 20s", ran, wait)
	}

	// Интервал перечитывается на каждом проходе.
	f.plan.interval = 30 * time.Second
	if ran, _ := f.tick(1110); len(ran) != 1 {
		t.Fatalf("после сокращения интервала: %v, хочу [a]", ran)
	}

	// Длинный интервал: пауза не дольше maxWait — цели и интервал перечитываются.
	f.plan.interval = time.Hour
	if _, wait := f.tick(1120); wait != maxWait {
		t.Fatalf("длинный интервал: пауза %v, хочу %v", wait, maxWait)
	}
}

func TestScheduler_WaitBounds(t *testing.T) {
	f := newSchedFixture()
	if _, wait := f.tick(1000); wait != maxWait {
		t.Errorf("без целей пауза %v, хочу %v", wait, maxWait)
	}
	f = newSchedFixture(Target{Project: "a", Push: true})
	f.plan.interval = 0 // неположительный — дефолт
	f.plan.states["a"] = State{LastRunAt: 1000}
	if ran, wait := f.tick(1000 + int64(defaultInterval/time.Second) - 1); len(ran) != 0 || wait != minWait {
		t.Errorf("за секунду до срока: %v, пауза %v; хочу [] и %v", ran, wait, minWait)
	}
}

func TestScheduler_BusyLeaseSkipped(t *testing.T) {
	logs := captureLog(t)
	f := newSchedFixture(Target{Project: "a", Push: true})
	f.fail["a"] = ErrBusy

	if ran, _ := f.tick(1000); len(ran) != 1 {
		t.Fatalf("первый проход: %v, хочу попытку [a]", ran)
	}
	if logs.Len() != 0 {
		t.Errorf("занятый lease залогирован как ошибка: %q", logs)
	}
	// Итог не записан (его пишет владелец lease), но попытка не повторяется
	// на каждом проходе — только через интервал.
	if ran, _ := f.tick(1010); len(ran) != 0 {
		t.Errorf("повтор до интервала: %v", ran)
	}
	delete(f.fail, "a")
	if ran, _ := f.tick(1300); len(ran) != 1 {
		t.Errorf("после интервала: %v, хочу [a]", ran)
	}
}

func TestScheduler_ErrorOfOneTargetDoesNotStopOthers(t *testing.T) {
	logs := captureLog(t)
	f := newSchedFixture(
		Target{Project: "down", Push: true},
		Target{Project: "nostate", Push: true},
		Target{Project: "ok", Pull: true},
	)
	f.fail["down"] = errors.New("общая база недоступна")
	f.plan.stateErr["nostate"] = errors.New("диск")

	if ran, _ := f.tick(1000); !slices.Equal(ran, []string{"down", "ok"}) {
		t.Fatalf("запущены %v, хочу [down ok]", ran)
	}
	if st := f.plan.states["down"]; st.LastError == "" {
		t.Errorf("ошибка не в статусе цели: %+v", st)
	}
	for _, want := range []string{"sync: down: общая база недоступна", "sync: nostate: состояние синхронизации: диск", "sync: ok: отправлено 1"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("в логе нет %q:\n%s", want, logs)
		}
	}
}

func TestScheduler_PanicRecovered(t *testing.T) {
	captureLog(t)
	f := newSchedFixture(Target{Project: "boom", Push: true}, Target{Project: "ok", Push: true})
	f.panics["boom"] = true

	if ran, _ := f.tick(1000); !slices.Equal(ran, []string{"boom", "ok"}) {
		t.Fatalf("запущены %v, хочу [boom ok]", ran)
	}
	// Паника не записала итог, но цель не гоняется на каждом проходе.
	if ran, _ := f.tick(1010); len(ran) != 0 {
		t.Errorf("повтор паникующей цели до интервала: %v", ran)
	}

	f.plan.panicky = true
	if _, wait := f.tick(1020); wait != maxWait {
		t.Errorf("паника плана: пауза %v, хочу %v", wait, maxWait)
	}
}

func TestScheduler_TargetsErrorRetriesLater(t *testing.T) {
	captureLog(t)
	f := newSchedFixture(Target{Project: "a", Push: true})
	f.plan.targetErr = errors.New("база занята")
	if ran, wait := f.tick(1000); len(ran) != 0 || wait != maxWait {
		t.Fatalf("ошибка чтения целей: %v, пауза %v", ran, wait)
	}
}

// TestScheduler_Run: первый проход сразу, следующий — по сигналу ожидания с
// паузой из прохода; отмена ctx завершает цикл.
func TestScheduler_Run(t *testing.T) {
	captureLog(t)
	f := newSchedFixture(Target{Project: "a", Push: true})
	f.plan.interval = 10 * time.Second
	waits := make(chan time.Duration)
	fire := make(chan time.Time)
	f.s.after = func(d time.Duration) <-chan time.Time {
		waits <- d
		return fire
	}
	var runs int
	run := f.s.run
	f.s.run = func(ctx context.Context, p string) (Result, error) {
		runs++
		return run(ctx, p)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.s.Run(ctx); close(done) }()

	if d := <-waits; d != 10*time.Second || runs != 1 {
		t.Fatalf("после первого прохода: пауза %v, прогонов %d", d, runs)
	}
	f.clock += 10
	fire <- time.Time{}
	if d := <-waits; d != 10*time.Second || runs != 2 {
		t.Fatalf("после второго прохода: пауза %v, прогонов %d", d, runs)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run не завершился после отмены ctx")
	}
}
