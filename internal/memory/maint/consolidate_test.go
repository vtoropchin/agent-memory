package maint

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"agentmemory/internal/memory"
	"agentmemory/internal/memory/embed"
	"agentmemory/internal/memory/reason"
	"agentmemory/internal/memory/store"
	"agentmemory/internal/memory/write"
)

// fakeReasoner — управляемая тестами реализация reason.Reasoner: возвращает
// заданный ответ/ошибку и считает число вызовов Complete.
type fakeReasoner struct {
	resp  string
	err   error
	calls int
}

func (f *fakeReasoner) Complete(_ context.Context, _ string) (string, error) {
	f.calls++
	return f.resp, f.err
}

var _ reason.Reasoner = (*fakeReasoner)(nil)

func newTestStore(t *testing.T) store.Store {
	t.Helper()
	s, err := store.OpenSQLite(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// putEpisodic вставляет episodic-запись напрямую в Store (в обход write path — здесь
// не нужны дедуп/классификация, только сырые данные для консолидации) с явным
// created_at, что позволяет детерминированно управлять «возрастом» записи
// относительно реального time.Now(), который использует Consolidate.
func putEpisodic(t *testing.T, s store.Store, scope, content string, createdAt int64) memory.Memory {
	t.Helper()
	m := memory.Memory{
		Id:        ulid.Make().String(),
		Kind:      memory.KindEpisodic,
		Scope:     scope,
		Content:   content,
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
	if err := s.Put(context.Background(), m); err != nil {
		t.Fatalf("putEpisodic: %v", err)
	}
	return m
}

// veryOld — unix ts заведомо в далёком прошлом относительно реального времени теста,
// что бы ни было передано в olderThan (в разумных пределах часов/дней).
const veryOld = 1000

func TestConsolidateSuccess(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	a := putEpisodic(t, s, "project:x", "Пользователь любит Go", veryOld)
	b := putEpisodic(t, s, "project:x", "Пользователь снова пишет на Go", veryOld+1)

	resp := fmt.Sprintf(
		`[{"content": "Пользователь предпочитает Go", "summary": "любит Go", "tags": ["go","pref"], "source_ids": ["%s", "%s"]}]`,
		a.Id, b.Id,
	)
	rs := &fakeReasoner{resp: resp}
	w := write.New(s, embed.NewNoop())
	c := NewConsolidator(s, rs, w)

	result, err := c.Consolidate(ctx, time.Hour, 0)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if result.Scanned != 2 {
		t.Errorf("Scanned = %d, want 2", result.Scanned)
	}
	if result.Created != 1 {
		t.Errorf("Created = %d, want 1", result.Created)
	}
	if result.Superseded != 2 {
		t.Errorf("Superseded = %d, want 2", result.Superseded)
	}
	if result.Skipped != 0 {
		t.Errorf("Skipped = %d, want 0", result.Skipped)
	}
	if rs.calls != 1 {
		t.Errorf("вызовов Reasoner.Complete = %d, want 1", rs.calls)
	}

	// старые эпизоды выпали из recall/SearchLexical (superseded).
	gotA, err := s.Get(ctx, a.Id)
	if err != nil {
		t.Fatalf("Get a: %v", err)
	}
	if gotA.SupersededBy == nil {
		t.Errorf("a.SupersededBy = nil, want непусто")
	}
	cands, err := s.SearchLexical(ctx, memory.Query{Text: "Go", Scope: "project:x", Kinds: []string{"episodic"}, Limit: 10})
	if err != nil {
		t.Fatalf("SearchLexical: %v", err)
	}
	if len(cands) != 0 {
		t.Errorf("SearchLexical episodic после консолидации = %+v, want пусто", cands)
	}

	// новый semantic-факт сохранён с ожидаемым content/summary/tags/source/importance.
	semCands, err := s.SearchLexical(ctx, memory.Query{Text: "предпочитает", Scope: "project:x", Kinds: []string{"semantic"}, Limit: 10})
	if err != nil {
		t.Fatalf("SearchLexical semantic: %v", err)
	}
	if len(semCands) != 1 {
		t.Fatalf("semantic candidates = %+v, want 1", semCands)
	}
	newFact, err := s.Get(ctx, semCands[0].Memory.Id)
	if err != nil {
		t.Fatalf("Get newFact: %v", err)
	}
	if newFact.Content != "Пользователь предпочитает Go" {
		t.Errorf("Content = %q", newFact.Content)
	}
	if newFact.Summary == nil || *newFact.Summary != "любит Go" {
		t.Errorf("Summary = %v, want 'любит Go'", newFact.Summary)
	}
	if newFact.Importance != 3 {
		t.Errorf("Importance = %d, want 3", newFact.Importance)
	}
	if newFact.Source == nil || *newFact.Source != "consolidation" {
		t.Errorf("Source = %v, want consolidation", newFact.Source)
	}
	if len(newFact.Tags) != 2 || newFact.Tags[0] != "go" || newFact.Tags[1] != "pref" {
		t.Errorf("Tags = %v, want [go pref]", newFact.Tags)
	}
}

func TestConsolidateInvalidJSONSkipsGroup(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	putEpisodic(t, s, "project:x", "эпизод 1", veryOld)
	putEpisodic(t, s, "project:x", "эпизод 2", veryOld+1)

	rs := &fakeReasoner{resp: "это не JSON, а свободный текст"}
	w := write.New(s, embed.NewNoop())
	c := NewConsolidator(s, rs, w)

	result, err := c.Consolidate(ctx, time.Hour, 0)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if result.Scanned != 2 {
		t.Errorf("Scanned = %d, want 2", result.Scanned)
	}
	if result.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", result.Skipped)
	}
	if result.Created != 0 || result.Superseded != 0 {
		t.Errorf("Created/Superseded = %d/%d, want 0/0", result.Created, result.Superseded)
	}

	// база не тронута: обе исходные записи всё ещё активны (не superseded).
	cands, err := s.SearchLexical(ctx, memory.Query{Text: "эпизод", Scope: "project:x", Kinds: []string{"episodic"}, Limit: 10})
	if err != nil {
		t.Fatalf("SearchLexical: %v", err)
	}
	if len(cands) != 2 {
		t.Errorf("candidates = %+v, want 2 (записи не тронуты)", cands)
	}
}

func TestConsolidateNoopReasonerSkips(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	putEpisodic(t, s, "project:x", "эпизод", veryOld)

	w := write.New(s, embed.NewNoop())
	c := NewConsolidator(s, reason.NewNoop(), w)

	result, err := c.Consolidate(ctx, time.Hour, 0)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if result.Scanned != 1 {
		t.Errorf("Scanned = %d, want 1", result.Scanned)
	}
	if result.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", result.Skipped)
	}
	if result.Created != 0 {
		t.Errorf("Created = %d, want 0", result.Created)
	}
}

func TestConsolidateReasonerErrorSkipsGroup(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	putEpisodic(t, s, "project:x", "эпизод", veryOld)

	rs := &fakeReasoner{err: errors.New("boom")}
	w := write.New(s, embed.NewNoop())
	c := NewConsolidator(s, rs, w)

	result, err := c.Consolidate(ctx, time.Hour, 0)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if result.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", result.Skipped)
	}
}

func TestConsolidateFreshEpisodesNotScanned(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	putEpisodic(t, s, "project:x", "свежий эпизод", time.Now().Unix())

	rs := &fakeReasoner{resp: "[]"}
	w := write.New(s, embed.NewNoop())
	c := NewConsolidator(s, rs, w)

	result, err := c.Consolidate(ctx, time.Hour, 0)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if result.Scanned != 0 {
		t.Errorf("Scanned = %d, want 0 (запись свежая)", result.Scanned)
	}
	if rs.calls != 0 {
		t.Errorf("вызовов Reasoner.Complete = %d, want 0 (пустая выборка — LLM не звать)", rs.calls)
	}
}

func TestConsolidateForeignSourceIDIgnored(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	a := putEpisodic(t, s, "project:x", "эпизод в группе", veryOld)
	foreign := ulid.Make().String() // валидный ULID, но не входил в группу

	resp := fmt.Sprintf(
		`[{"content": "факт", "summary": "", "tags": [], "source_ids": ["%s", "%s"]}]`,
		a.Id, foreign,
	)
	rs := &fakeReasoner{resp: resp}
	w := write.New(s, embed.NewNoop())
	c := NewConsolidator(s, rs, w)

	result, err := c.Consolidate(ctx, time.Hour, 0)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if result.Created != 1 {
		t.Errorf("Created = %d, want 1", result.Created)
	}
	// superseded только для a — foreign не входил в группу и не существует в базе,
	// вызов Supersede для него привёл бы к ошибке ErrNotFound, если бы не был отфильтрован.
	if result.Superseded != 1 {
		t.Errorf("Superseded = %d, want 1 (foreign id должен быть проигнорирован)", result.Superseded)
	}
	gotA, err := s.Get(ctx, a.Id)
	if err != nil {
		t.Fatalf("Get a: %v", err)
	}
	if gotA.SupersededBy == nil {
		t.Errorf("a.SupersededBy = nil, want непусто")
	}
}

// TestConsolidateMergedFactDoesNotOverwriteSummaryOrCountAsCreated — если
// консолидированный факт слился (Writer dedup) с уже существующим semantic-фактом,
// это не должно: (a) считаться как Created (учитывается отдельно как Merged);
// (b) перезаписывать summary существующей записи значением из ответа Reasoner.
// Supersede исходных episodic на существующий id при этом всё равно применяется.
func TestConsolidateMergedFactDoesNotOverwriteSummaryOrCountAsCreated(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	existingSummary := "старое summary"
	existingID := ulid.Make().String()
	if err := s.Put(ctx, memory.Memory{
		Id: existingID, Kind: memory.KindSemantic, Scope: "project:x",
		Content: "Пользователь предпочитает Go", Summary: &existingSummary,
		Importance: 3, CreatedAt: veryOld, UpdatedAt: veryOld,
	}); err != nil {
		t.Fatalf("Put existing semantic: %v", err)
	}

	a := putEpisodic(t, s, "project:x", "Пользователь любит Go", veryOld)
	b := putEpisodic(t, s, "project:x", "Пользователь снова пишет на Go", veryOld+1)

	resp := fmt.Sprintf(
		`[{"content": "Пользователь предпочитает Go", "summary": "новое summary из Reasoner", "tags": [], "source_ids": ["%s", "%s"]}]`,
		a.Id, b.Id,
	)
	rs := &fakeReasoner{resp: resp}
	w := write.New(s, embed.NewNoop())
	c := NewConsolidator(s, rs, w)

	result, err := c.Consolidate(ctx, time.Hour, 0)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if result.Created != 0 {
		t.Errorf("Created = %d, want 0 (факт слился с существующим дублем)", result.Created)
	}
	if result.Merged != 1 {
		t.Errorf("Merged = %d, want 1", result.Merged)
	}
	if result.Superseded != 2 {
		t.Errorf("Superseded = %d, want 2 (a и b всё равно суперсижены на существующий id)", result.Superseded)
	}
	if result.Skipped != 0 {
		t.Errorf("Skipped = %d, want 0", result.Skipped)
	}

	got, err := s.Get(ctx, existingID)
	if err != nil {
		t.Fatalf("Get existing: %v", err)
	}
	if got.Summary == nil || *got.Summary != existingSummary {
		t.Errorf("Summary = %v, want сохранённое %q (не должно быть перезаписано мерджем)", got.Summary, existingSummary)
	}

	gotA, err := s.Get(ctx, a.Id)
	if err != nil {
		t.Fatalf("Get a: %v", err)
	}
	if gotA.SupersededBy == nil || *gotA.SupersededBy != existingID {
		t.Errorf("a.SupersededBy = %v, want %q", gotA.SupersededBy, existingID)
	}
}

// TestConsolidateDuplicateSourceIDWithinFactSupersededOnce — если Reasoner повторил
// один и тот же source_id дважды внутри одного факта, Supersede должен уйти ровно
// один раз (повторный вызов Store.Supersede на уже заменённую запись — ошибка).
func TestConsolidateDuplicateSourceIDWithinFactSupersededOnce(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	a := putEpisodic(t, s, "project:x", "единственный эпизод группы", veryOld)

	resp := fmt.Sprintf(
		`[{"content": "факт", "summary": "", "tags": [], "source_ids": ["%s", "%s"]}]`,
		a.Id, a.Id,
	)
	rs := &fakeReasoner{resp: resp}
	w := write.New(s, embed.NewNoop())
	c := NewConsolidator(s, rs, w)

	result, err := c.Consolidate(ctx, time.Hour, 0)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if result.Superseded != 1 {
		t.Errorf("Superseded = %d, want 1 (дубль source_id внутри факта считается один раз)", result.Superseded)
	}
	if result.Skipped != 0 {
		t.Errorf("Skipped = %d, want 0", result.Skipped)
	}
}

// TestConsolidateDuplicateSourceIDAcrossFactsSupersededOnce — тот же source_id,
// повторённый в двух РАЗНЫХ фактах одной группы, тоже должен супер­сидиться один раз
// (в пользу первого факта, который его затронул).
func TestConsolidateDuplicateSourceIDAcrossFactsSupersededOnce(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	a := putEpisodic(t, s, "project:x", "единственный эпизод группы", veryOld)

	resp := fmt.Sprintf(
		`[{"content": "первый обобщённый факт про язык Go", "summary": "", "tags": [], "source_ids": ["%s"]},
		  {"content": "второй обобщённый факт про экосистему Python", "summary": "", "tags": [], "source_ids": ["%s"]}]`,
		a.Id, a.Id,
	)
	rs := &fakeReasoner{resp: resp}
	w := write.New(s, embed.NewNoop())
	c := NewConsolidator(s, rs, w)

	result, err := c.Consolidate(ctx, time.Hour, 0)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if result.Created != 2 {
		t.Errorf("Created = %d, want 2 (два разных факта)", result.Created)
	}
	if result.Superseded != 1 {
		t.Errorf("Superseded = %d, want 1 (один и тот же source_id между фактами группы — supersede один раз)", result.Superseded)
	}
}

// TestConsolidateBatchLimitDoesNotSplitGroup — batchLimit ограничивает суммарное
// число обработанных эпизодов, но никогда не разрывает scope-группу: если группа
// пересекает границу лимита, она обрабатывается целиком, а следующие группы уже не
// берутся.
func TestConsolidateBatchLimitDoesNotSplitGroup(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	putEpisodic(t, s, "project:a", "эпизод a1", veryOld)
	putEpisodic(t, s, "project:a", "эпизод a2", veryOld+1)
	putEpisodic(t, s, "project:a", "эпизод a3", veryOld+2)
	putEpisodic(t, s, "project:b", "эпизод b1", veryOld)
	putEpisodic(t, s, "project:b", "эпизод b2", veryOld+1)

	rs := &fakeReasoner{resp: "[]"}
	w := write.New(s, embed.NewNoop())
	c := NewConsolidator(s, rs, w)

	// batchLimit=2, но группа "project:a" (сортируется первой) состоит из 3 записей.
	result, err := c.Consolidate(ctx, time.Hour, 2)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if result.Scanned != 3 {
		t.Errorf("Scanned = %d, want 3 (project:a обработана целиком, несмотря на batchLimit=2)", result.Scanned)
	}
	if rs.calls != 1 {
		t.Errorf("вызовов Reasoner.Complete = %d, want 1 (project:b не взята в работу)", rs.calls)
	}

	cands, err := s.SearchLexical(ctx, memory.Query{Text: "эпизод", Scope: "project:b", Kinds: []string{"episodic"}, Limit: 10})
	if err != nil {
		t.Fatalf("SearchLexical: %v", err)
	}
	if len(cands) != 2 {
		t.Errorf("project:b candidates = %+v, want 2 (группа не тронута)", cands)
	}
}

// echoReasoner — Reasoner для тестов, извлекающий id первой записи промпта
// (формат buildConsolidationPrompt: "1. [ID] content") и возвращающий её же как
// единственный консолидированный факт — так каждая scope-группа детерминированно
// даёт свой, отличимый от других групп, ответ.
type echoReasoner struct{ calls int }

func (e *echoReasoner) Complete(_ context.Context, prompt string) (string, error) {
	e.calls++
	const marker = "1. ["
	start := strings.Index(prompt, marker)
	if start == -1 {
		return "[]", nil
	}
	start += len(marker)
	end := strings.Index(prompt[start:], "]")
	if end == -1 {
		return "[]", nil
	}
	id := prompt[start : start+end]
	return fmt.Sprintf(`[{"content": "факт про %s", "summary": "", "tags": [], "source_ids": ["%s"]}]`, id, id), nil
}

var _ reason.Reasoner = (*echoReasoner)(nil)

// failingSupersedeStore оборачивает store.Store, проваливая Supersede для id из
// failFor — нужен, чтобы проверить, что ошибка Store внутри одной scope-группы не
// мешает обработке остальных групп (DESIGN.md §15: пер-групповая деградация).
type failingSupersedeStore struct {
	store.Store
	failFor map[string]bool
}

func (f *failingSupersedeStore) Supersede(ctx context.Context, oldID, newID string) error {
	if f.failFor[oldID] {
		return errors.New("boom: supersede намеренно проваливается в тесте")
	}
	return f.Store.Supersede(ctx, oldID, newID)
}

func TestConsolidateStoreErrorInOneGroupDoesNotBlockOthers(t *testing.T) {
	ctx := context.Background()
	real := newTestStore(t)

	a := putEpisodic(t, real, "project:a", "эпизод a", veryOld)
	b := putEpisodic(t, real, "project:b", "эпизод b", veryOld)

	fs := &failingSupersedeStore{Store: real, failFor: map[string]bool{a.Id: true}}
	rs := &echoReasoner{}
	w := write.New(fs, embed.NewNoop())
	c := NewConsolidator(fs, rs, w)

	result, err := c.Consolidate(ctx, time.Hour, 0)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if rs.calls != 2 {
		t.Errorf("вызовов Reasoner.Complete = %d, want 2 (обе группы обработаны)", rs.calls)
	}
	if result.Created != 2 {
		t.Errorf("Created = %d, want 2 (оба факта сохранены, ошибка была только в Supersede)", result.Created)
	}
	if result.Superseded != 1 {
		t.Errorf("Superseded = %d, want 1 (успешна только группа project:b)", result.Superseded)
	}
	if result.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1 (группа project:a частично провалилась)", result.Skipped)
	}

	gotA, err := real.Get(ctx, a.Id)
	if err != nil {
		t.Fatalf("Get a: %v", err)
	}
	if gotA.SupersededBy != nil {
		t.Errorf("a.SupersededBy = %v, want nil (Supersede провалился)", gotA.SupersededBy)
	}
	gotB, err := real.Get(ctx, b.Id)
	if err != nil {
		t.Fatalf("Get b: %v", err)
	}
	if gotB.SupersededBy == nil {
		t.Errorf("b.SupersededBy = nil, want непусто (группа project:b обработана успешно)")
	}
}

func TestConsolidateEmptyFactsArrayNoError(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	putEpisodic(t, s, "project:x", "единичный эпизод без пары", veryOld)

	rs := &fakeReasoner{resp: "```json\n[]\n```"}
	w := write.New(s, embed.NewNoop())
	c := NewConsolidator(s, rs, w)

	result, err := c.Consolidate(ctx, time.Hour, 0)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if result.Skipped != 0 {
		t.Errorf("Skipped = %d, want 0 (валидный пустой массив — не ошибка)", result.Skipped)
	}
	if result.Created != 0 {
		t.Errorf("Created = %d, want 0", result.Created)
	}
}
