package maint

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"agentmemory/internal/memory"
	"agentmemory/internal/memory/store"
)

// putMemoryWith вставляет запись напрямую в Store (в обход write path) с явными
// importance/createdAt — dedup sweep реагирует именно на эти поля при выборе
// победителя пары (см. pickDedupWinner).
func putMemoryWith(t *testing.T, s store.Store, scope, content string, importance int, createdAt int64) memory.Memory {
	t.Helper()
	m := memory.Memory{
		Id:         ulid.Make().String(),
		Kind:       memory.KindSemantic,
		Scope:      scope,
		Content:    content,
		Importance: importance,
		CreatedAt:  createdAt,
		UpdatedAt:  createdAt,
	}
	if err := s.Put(context.Background(), m); err != nil {
		t.Fatalf("putMemoryWith: %v", err)
	}
	return m
}

// putMemoryWithKind — как putMemoryWith, но с явным Kind: нужен тесту, что
// dedup sweep не считает записи разного Kind дублями, даже при похожем content
// (см. TestSweepDoesNotDedupAcrossKinds).
func putMemoryWithKind(t *testing.T, s store.Store, scope string, kind memory.Kind, content string, importance int, createdAt int64) memory.Memory {
	t.Helper()
	m := memory.Memory{
		Id:         ulid.Make().String(),
		Kind:       kind,
		Scope:      scope,
		Content:    content,
		Importance: importance,
		CreatedAt:  createdAt,
		UpdatedAt:  createdAt,
	}
	if err := s.Put(context.Background(), m); err != nil {
		t.Fatalf("putMemoryWithKind: %v", err)
	}
	return m
}

// Три взаимно похожих (SimHash-расстояние <= 8 для каждой пары) и одна явно
// непохожая формулировка — проверено отдельно (scratch-прогон при разработке):
// distance(near1,near2)=5, distance(near1,near3)=6, distance(near2,near3)=5,
// distance(near1,farAway) и т.п. — все >= 26.
const (
	near1   = "Пользователь предпочитает тёмную тему интерфейса и компактный режим отображения списков во всех приложениях"
	near2   = "Пользователь предпочитает тёмную тему интерфейса и компактный режим отображения списков почти во всех приложениях"
	near3   = "Пользователь предпочитает тёмную тему интерфейса и компактный режим отображения списков во всех своих приложениях"
	farAway = "Пользователь любит совершенно другую светлую тему и крупный шрифт для чтения книг по вечерам"
)

func TestSweepSupersedesMoreImportantSurvives(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	sweeper := NewDedupSweeper(s)

	base := time.Now().Unix()
	loEnd := putMemoryWith(t, s, "project:x", near1, 2, base)
	hiEnd := putMemoryWith(t, s, "project:x", near2, 4, base)

	result, err := sweeper.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if result.Scanned != 2 {
		t.Errorf("Scanned = %d, want 2", result.Scanned)
	}
	if result.Superseded != 1 {
		t.Errorf("Superseded = %d, want 1", result.Superseded)
	}

	gotLo, err := s.Get(ctx, loEnd.Id)
	if err != nil {
		t.Fatalf("Get loEnd: %v", err)
	}
	if gotLo.SupersededBy == nil || *gotLo.SupersededBy != hiEnd.Id {
		t.Errorf("loEnd.SupersededBy = %v, want %q (более важная запись выживает)", gotLo.SupersededBy, hiEnd.Id)
	}

	gotHi, err := s.Get(ctx, hiEnd.Id)
	if err != nil {
		t.Fatalf("Get hiEnd: %v", err)
	}
	if gotHi.SupersededBy != nil {
		t.Errorf("hiEnd.SupersededBy = %v, want nil (выжила)", gotHi.SupersededBy)
	}
}

func TestSweepTieBreaksOnNewerCreatedAt(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	sweeper := NewDedupSweeper(s)

	base := time.Now().Unix()
	older := putMemoryWith(t, s, "project:x", near1, 3, base)
	newer := putMemoryWith(t, s, "project:x", near2, 3, base+100)

	result, err := sweeper.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if result.Superseded != 1 {
		t.Errorf("Superseded = %d, want 1", result.Superseded)
	}

	gotOlder, err := s.Get(ctx, older.Id)
	if err != nil {
		t.Fatalf("Get older: %v", err)
	}
	if gotOlder.SupersededBy == nil || *gotOlder.SupersededBy != newer.Id {
		t.Errorf("older.SupersededBy = %v, want %q (более новая запись выживает при равном importance)", gotOlder.SupersededBy, newer.Id)
	}
}

func TestSweepDoesNotDedupAcrossScopes(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	sweeper := NewDedupSweeper(s)

	base := time.Now().Unix()
	a := putMemoryWith(t, s, "project:x", near1, 3, base)
	b := putMemoryWith(t, s, "project:y", near1, 3, base)

	result, err := sweeper.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if result.Scanned != 2 {
		t.Errorf("Scanned = %d, want 2", result.Scanned)
	}
	if result.Superseded != 0 {
		t.Errorf("Superseded = %d, want 0 (одинаковый текст в разных scope не дедуплицируется)", result.Superseded)
	}

	for _, m := range []memory.Memory{a, b} {
		got, err := s.Get(ctx, m.Id)
		if err != nil {
			t.Fatalf("Get %q: %v", m.Id, err)
		}
		if got.SupersededBy != nil {
			t.Errorf("Get %q: SupersededBy = %v, want nil", m.Id, got.SupersededBy)
		}
	}
}

// TestSweepDoesNotDedupAcrossKinds — регресс на расхождение с write path
// (см. арх-ревью задачи 4.2): write.dedup фильтрует кандидатов по Scope И Kind
// (internal/memory/write/write.go, dedup: SearchLexical с Kinds:
// []string{string(kind)}), поэтому запись одного Kind никогда не считается
// дублем записи другого Kind — даже при похожем content и одном scope
// (episodic-наблюдение и semantic-факт легитимно разные сущности). Sweep обязан
// вести себя так же.
func TestSweepDoesNotDedupAcrossKinds(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	sweeper := NewDedupSweeper(s)

	base := time.Now().Unix()
	episodic := putMemoryWithKind(t, s, "project:x", memory.KindEpisodic, near1, 3, base)
	semantic := putMemoryWithKind(t, s, "project:x", memory.KindSemantic, near2, 3, base+1)

	result, err := sweeper.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if result.Scanned != 2 {
		t.Errorf("Scanned = %d, want 2", result.Scanned)
	}
	if result.Superseded != 0 {
		t.Errorf("Superseded = %d, want 0 (разный Kind в одном scope не дедуплицируется)", result.Superseded)
	}

	for _, m := range []memory.Memory{episodic, semantic} {
		got, err := s.Get(ctx, m.Id)
		if err != nil {
			t.Fatalf("Get %q: %v", m.Id, err)
		}
		if got.SupersededBy != nil {
			t.Errorf("Get %q: SupersededBy = %v, want nil", m.Id, got.SupersededBy)
		}
	}
}

func TestSweepChainOfThreeCollapsesToOneSurvivor(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	sweeper := NewDedupSweeper(s)

	base := time.Now().Unix()
	putMemoryWith(t, s, "project:x", near1, 3, base)
	putMemoryWith(t, s, "project:x", near2, 3, base+1)
	best := putMemoryWith(t, s, "project:x", near3, 5, base+2) // самая важная — должна выжить

	result, err := sweeper.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if result.Scanned != 3 {
		t.Errorf("Scanned = %d, want 3", result.Scanned)
	}
	if result.Superseded != 2 {
		t.Errorf("Superseded = %d, want 2 (выживает ровно одна запись из тройки)", result.Superseded)
	}

	all, err := s.AllForReindex(ctx)
	if err != nil {
		t.Fatalf("AllForReindex: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("живых записей после Sweep = %d, want 1", len(all))
	}
	if all[0].Id != best.Id {
		t.Errorf("выжившая запись = %q, want %q (наибольший importance)", all[0].Id, best.Id)
	}
}

func TestSweepLeavesDissimilarRecordsUntouched(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	sweeper := NewDedupSweeper(s)

	base := time.Now().Unix()
	a := putMemoryWith(t, s, "project:x", near1, 3, base)
	b := putMemoryWith(t, s, "project:x", farAway, 3, base+1)

	result, err := sweeper.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if result.Superseded != 0 {
		t.Errorf("Superseded = %d, want 0 (записи не похожи)", result.Superseded)
	}

	for _, m := range []memory.Memory{a, b} {
		got, err := s.Get(ctx, m.Id)
		if err != nil {
			t.Fatalf("Get %q: %v", m.Id, err)
		}
		if got.SupersededBy != nil {
			t.Errorf("Get %q: SupersededBy = %v, want nil", m.Id, got.SupersededBy)
		}
	}
}

func TestSweepIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	sweeper := NewDedupSweeper(s)

	base := time.Now().Unix()
	putMemoryWith(t, s, "project:x", near1, 2, base)
	putMemoryWith(t, s, "project:x", near2, 4, base+1)
	putMemoryWith(t, s, "project:x", near3, 3, base+2)

	first, err := sweeper.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep #1: %v", err)
	}
	if first.Superseded != 2 {
		t.Fatalf("Sweep #1: Superseded = %d, want 2", first.Superseded)
	}

	second, err := sweeper.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep #2: %v", err)
	}
	if second.Superseded != 0 {
		t.Errorf("Sweep #2: Superseded = %d, want 0 (идемпотентность — дубли уже устранены)", second.Superseded)
	}
	if second.Scanned != 1 {
		t.Errorf("Sweep #2: Scanned = %d, want 1 (после первого прохода жива только одна запись)", second.Scanned)
	}
}

// TestSweepSkipsTasks: задачи не участвуют в ретроактивном дедупе (write.SkipDedup,
// задача 14.1) — две задачи с идентичным content остаются двумя живыми записями,
// тогда как та же пара semantic-записей схлопывается в одну.
func TestSweepSkipsTasks(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:tasks-sweep"

	t1 := putMemoryWithKind(t, s, scope, memory.KindTask, near1, 3, 100)
	t2 := putMemoryWithKind(t, s, scope, memory.KindTask, near1, 3, 200)
	f1 := putMemoryWithKind(t, s, scope, memory.KindSemantic, near1, 3, 100)
	f2 := putMemoryWithKind(t, s, scope, memory.KindSemantic, near1, 3, 200)

	res, err := NewDedupSweeper(s).Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.Superseded != 1 {
		t.Errorf("Superseded = %d, want 1 (схлопнуться должна только пара semantic)", res.Superseded)
	}

	for _, id := range []string{t1.Id, t2.Id} {
		m, err := s.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get %s: %v", id, err)
		}
		if m.SupersededBy != nil {
			t.Errorf("задача %s не должна быть схлопнута дедупом", id)
		}
	}
	loser, err := s.Get(ctx, f1.Id)
	if err != nil {
		t.Fatalf("Get %s: %v", f1.Id, err)
	}
	if loser.SupersededBy == nil || *loser.SupersededBy != f2.Id {
		t.Errorf("пара semantic обязана схлопнуться в пользу более новой записи, got %v", loser.SupersededBy)
	}
}
