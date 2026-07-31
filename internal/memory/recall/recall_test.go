package recall

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"agentmemory/internal/config"
	"agentmemory/internal/memory"
	"agentmemory/internal/memory/embed"
	"agentmemory/internal/memory/reason"
	"agentmemory/internal/memory/store"
)

func newTestStore(t *testing.T) store.Store {
	t.Helper()
	s, err := store.OpenSQLite(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newID(t *testing.T) string {
	t.Helper()
	return ulid.Make().String()
}

func strPtr(s string) *string { return &s }

func putMemory(t *testing.T, ctx context.Context, s store.Store, m memory.Memory) {
	t.Helper()
	if err := s.Put(ctx, m); err != nil {
		t.Fatalf("Put %q: %v", m.Id, err)
	}
}

func defaultWeights() config.RecallWeights {
	return config.RecallWeights{Relevance: 1.0, Importance: 0.5, Recency: 0.4, Access: 0.2}
}

// fakeReasoner — тестовая реализация reason.Reasoner: возвращает заданный результат/
// ошибку и запоминает, был ли вызван Complete (для проверки Summarize=false).
type fakeReasoner struct {
	result string
	err    error
	called bool
}

func (f *fakeReasoner) Complete(_ context.Context, _ string) (string, error) {
	f.called = true
	return f.result, f.err
}

var _ reason.Reasoner = (*fakeReasoner)(nil)

// Секция брифа между заголовком "### <title>" и следующим "###" или концом строки.
func sectionOf(t *testing.T, md, title string) string {
	t.Helper()
	marker := "### " + title
	idx := strings.Index(md, marker)
	if idx == -1 {
		return ""
	}
	rest := md[idx+len(marker):]
	if next := strings.Index(rest, "\n### "); next != -1 {
		rest = rest[:next]
	}
	if next := strings.Index(rest, "\n_Свёрнуто"); next != -1 {
		rest = rest[:next]
	}
	return rest
}

func TestRecall_RanksByImportanceWhenRelevanceEqual(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:agentmemory"

	lowID, highID := newID(t), newID(t)
	// Одинаковый Content -> одинаковая лексическая релевантность (bm25).
	// Различаются только Summary (что показывается в брифе) и Importance.
	putMemory(t, ctx, s, memory.Memory{
		Id: lowID, Kind: memory.KindSemantic, Scope: scope,
		Content: "Проект хранит атомарные факты в SQLite базе данных",
		Summary: strPtr("Низкая важность"), Importance: 1,
		CreatedAt: 1000, UpdatedAt: 1000,
	})
	putMemory(t, ctx, s, memory.Memory{
		Id: highID, Kind: memory.KindSemantic, Scope: scope,
		Content: "Проект хранит атомарные факты в SQLite базе данных",
		Summary: strPtr("Высокая важность"), Importance: 5,
		CreatedAt: 1000, UpdatedAt: 1000,
	})

	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()}, reason.NewNoop(), embed.NewNoop(),
		WithNow(func() int64 { return 1000 }))

	brief, err := r.Recall(ctx, memory.Query{Text: "факты SQLite базе", Scope: scope})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	idxHigh := strings.Index(brief.Markdown, "Высокая важность")
	idxLow := strings.Index(brief.Markdown, "Низкая важность")
	if idxHigh == -1 || idxLow == -1 {
		t.Fatalf("обе записи должны быть в брифе, got:\n%s", brief.Markdown)
	}
	if idxHigh > idxLow {
		t.Errorf("запись с большей importance должна идти раньше в брифе:\n%s", brief.Markdown)
	}
}

func TestRecall_RanksByRecencyWhenRelevanceAndImportanceEqual(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:agentmemory"

	oldID, freshID := newID(t), newID(t)
	putMemory(t, ctx, s, memory.Memory{
		Id: oldID, Kind: memory.KindSemantic, Scope: scope,
		Content: "Проект хранит атомарные факты в SQLite базе данных",
		Summary: strPtr("Старая запись"), Importance: 3,
		CreatedAt: 0, UpdatedAt: 0,
	})
	putMemory(t, ctx, s, memory.Memory{
		Id: freshID, Kind: memory.KindSemantic, Scope: scope,
		Content: "Проект хранит атомарные факты в SQLite базе данных",
		Summary: strPtr("Свежая запись"), Importance: 3,
		CreatedAt: 1_000_000, UpdatedAt: 1_000_000,
	})

	// now намного дальше периода полураспада от oldID и совпадает с freshID.
	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()}, reason.NewNoop(), embed.NewNoop(),
		WithNow(func() int64 { return 1_000_000 }))

	brief, err := r.Recall(ctx, memory.Query{Text: "факты SQLite базе", Scope: scope})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	idxFresh := strings.Index(brief.Markdown, "Свежая запись")
	idxOld := strings.Index(brief.Markdown, "Старая запись")
	if idxFresh == -1 || idxOld == -1 {
		t.Fatalf("обе записи должны быть в брифе, got:\n%s", brief.Markdown)
	}
	if idxFresh > idxOld {
		t.Errorf("более свежая запись должна идти раньше в брифе:\n%s", brief.Markdown)
	}
}

func TestRecall_BudgetCollapsesTail(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:budget"

	const n = 8
	for i := 0; i < n; i++ {
		putMemory(t, ctx, s, memory.Memory{
			Id: newID(t), Kind: memory.KindSemantic, Scope: scope,
			Content:    strings.Repeat("слово ", 20) + "уникальныйтег" + string(rune('a'+i)),
			Summary:    strPtr("Факт номер " + string(rune('a'+i))),
			Importance: 3, CreatedAt: int64(1000 + i), UpdatedAt: int64(1000 + i),
		})
	}

	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()}, reason.NewNoop(), embed.NewNoop(),
		WithNow(func() int64 { return 2000 }))

	t.Run("маленький бюджет режет хвост", func(t *testing.T) {
		brief, err := r.Recall(ctx, memory.Query{Text: "слово факт", Scope: scope, TokenBudget: 5})
		if err != nil {
			t.Fatalf("Recall: %v", err)
		}
		if brief.Collapsed == 0 {
			t.Fatalf("ожидали Collapsed > 0 при маленьком бюджете, brief=%+v", brief)
		}
		if brief.Count+brief.Collapsed != n {
			t.Errorf("Count(%d)+Collapsed(%d) != %d", brief.Count, brief.Collapsed, n)
		}
		wantTail := "Свёрнуто ещё "
		if !strings.Contains(brief.Markdown, wantTail) {
			t.Errorf("markdown должен содержать строку про свёртку:\n%s", brief.Markdown)
		}
	})

	t.Run("большой бюджет не режет", func(t *testing.T) {
		brief, err := r.Recall(ctx, memory.Query{Text: "слово факт", Scope: scope, TokenBudget: 100000})
		if err != nil {
			t.Fatalf("Recall: %v", err)
		}
		if brief.Collapsed != 0 {
			t.Errorf("Collapsed = %d, want 0 при большом бюджете", brief.Collapsed)
		}
		if brief.Count != n {
			t.Errorf("Count = %d, want %d", brief.Count, n)
		}
		if strings.Contains(brief.Markdown, "Свёрнуто") {
			t.Errorf("markdown не должен упоминать свёртку при Collapsed=0:\n%s", brief.Markdown)
		}
	})
}

func TestRecall_BudgetAccountsForMarkdownWrapping(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:budget-wrap"

	const n = 10
	for i := 0; i < n; i++ {
		putMemory(t, ctx, s, memory.Memory{
			Id: newID(t), Kind: memory.KindSemantic, Scope: scope,
			Content:    strings.Repeat("слово ", 20) + "уникальныйтег" + string(rune('a'+i)),
			Summary:    strPtr("Факт номер " + string(rune('a'+i))),
			Importance: 3, CreatedAt: int64(1000 + i), UpdatedAt: int64(1000 + i),
		})
	}

	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()}, reason.NewNoop(), embed.NewNoop(),
		WithNow(func() int64 { return 2000 }))

	// Бюджет заведомо меньше того, что нужно на все n записей + обвязку (заголовок,
	// заголовок секции, разделители, строка о свёртке) — должна сработать реальная
	// обрезка хвоста, а не только построчная оценка без учёта обвязки.
	const budget = 60
	brief, err := r.Recall(ctx, memory.Query{Text: "слово факт", Scope: scope, TokenBudget: budget})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	// Та же эвристика, что и estimateTokens в recall.go — фактический размер
	// собранного Markdown не должен систематически превышать заявленный бюджет
	// (с учётом заголовка, заголовков секций и разделителей).
	actual := len([]rune(brief.Markdown)) / 4
	if actual > budget {
		t.Errorf("фактический размер брифа (~%d токенов) превышает бюджет %d:\n%s", actual, budget, brief.Markdown)
	}
	if brief.ApproxTokens != actual {
		t.Errorf("ApproxTokens = %d, want %d (честная оценка по фактическому Markdown)", brief.ApproxTokens, actual)
	}
	if brief.Count+brief.Collapsed != n {
		t.Errorf("Count(%d)+Collapsed(%d) != %d", brief.Count, brief.Collapsed, n)
	}
	if brief.Collapsed == 0 {
		t.Fatalf("ожидали реальную обрезку хвоста (Collapsed>0) при бюджете %d на %d записей, brief=%+v", budget, n, brief)
	}
}

func TestRecall_TinyBudgetStillReturnsOneRecord(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:tiny-budget"

	const n = 4
	for i := 0; i < n; i++ {
		putMemory(t, ctx, s, memory.Memory{
			Id: newID(t), Kind: memory.KindSemantic, Scope: scope,
			Content:    "тиникрошечныйбюджет тест уникальныймаркер " + string(rune('a'+i)),
			Summary:    strPtr("Факт номер " + string(rune('a'+i))),
			Importance: 3, CreatedAt: int64(1000 + i), UpdatedAt: int64(1000 + i),
		})
	}

	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()}, reason.NewNoop(), embed.NewNoop(),
		WithNow(func() int64 { return 2000 }))

	brief, err := r.Recall(ctx, memory.Query{
		Text: "тиникрошечныйбюджет тест уникальныймаркер", Scope: scope, TokenBudget: 3,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if brief.Count != 1 {
		t.Fatalf("Count = %d, want 1 (минимум одна запись при непустых кандидатах, даже если бюджет крошечный)", brief.Count)
	}
	if brief.Collapsed != n-1 {
		t.Errorf("Collapsed = %d, want %d", brief.Collapsed, n-1)
	}
	if strings.Contains(brief.Markdown, "(0 записей") {
		t.Errorf("бриф не должен сообщать 0 записей, когда есть релевантные кандидаты:\n%s", brief.Markdown)
	}
}

func TestRecall_GroupsByKindInOrder(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:grouping"

	semID, procID, epiID := newID(t), newID(t), newID(t)
	putMemory(t, ctx, s, memory.Memory{
		Id: semID, Kind: memory.KindSemantic, Scope: scope,
		Content: "общий термин факт про группировку", Summary: strPtr("Семантический факт"),
		Importance: 3, CreatedAt: 1, UpdatedAt: 1,
	})
	putMemory(t, ctx, s, memory.Memory{
		Id: procID, Kind: memory.KindProcedural, Scope: scope,
		Content: "общий термин факт про группировку", Summary: strPtr("Процедурный рецепт"),
		Importance: 3, CreatedAt: 2, UpdatedAt: 2,
	})
	putMemory(t, ctx, s, memory.Memory{
		Id: epiID, Kind: memory.KindEpisodic, Scope: scope,
		Content: "общий термин факт про группировку", Summary: strPtr("Эпизодическое событие"),
		Importance: 3, CreatedAt: 3, UpdatedAt: 3,
	})

	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()}, reason.NewNoop(), embed.NewNoop(),
		WithNow(func() int64 { return 1000 }))

	brief, err := r.Recall(ctx, memory.Query{Text: "термин факт группировку", Scope: scope})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	idxFacts := strings.Index(brief.Markdown, "### Facts (semantic)")
	idxHowTo := strings.Index(brief.Markdown, "### How-to (procedural)")
	idxRecent := strings.Index(brief.Markdown, "### Recent context (episodic)")
	if idxFacts == -1 || idxHowTo == -1 || idxRecent == -1 {
		t.Fatalf("все три секции должны присутствовать:\n%s", brief.Markdown)
	}
	if !(idxFacts < idxHowTo && idxHowTo < idxRecent) {
		t.Errorf("порядок секций должен быть Facts -> How-to -> Recent context:\n%s", brief.Markdown)
	}

	if !strings.Contains(sectionOf(t, brief.Markdown, "Facts (semantic)"), "Семантический факт") {
		t.Errorf("секция Facts должна содержать семантическую запись:\n%s", brief.Markdown)
	}
	if !strings.Contains(sectionOf(t, brief.Markdown, "How-to (procedural)"), "Процедурный рецепт") {
		t.Errorf("секция How-to должна содержать процедурную запись:\n%s", brief.Markdown)
	}
	if !strings.Contains(sectionOf(t, brief.Markdown, "Recent context (episodic)"), "Эпизодическое событие") {
		t.Errorf("секция Recent context должна содержать эпизодическую запись:\n%s", brief.Markdown)
	}
}

func TestRecall_FiltersByScope(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	mineID, otherID := newID(t), newID(t)
	putMemory(t, ctx, s, memory.Memory{
		Id: mineID, Kind: memory.KindSemantic, Scope: "project:mine",
		Content: "уникальный маркер scope тест", Summary: strPtr("Моя запись"),
		Importance: 3, CreatedAt: 1, UpdatedAt: 1,
	})
	putMemory(t, ctx, s, memory.Memory{
		Id: otherID, Kind: memory.KindSemantic, Scope: "project:other",
		Content: "уникальный маркер scope тест", Summary: strPtr("Чужая запись"),
		Importance: 5, CreatedAt: 1, UpdatedAt: 1,
	})

	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()}, reason.NewNoop(), embed.NewNoop(),
		WithNow(func() int64 { return 1000 }))

	brief, err := r.Recall(ctx, memory.Query{Text: "уникальный маркер scope", Scope: "project:mine"})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if brief.Count != 1 {
		t.Fatalf("Count = %d, want 1 (только свой scope)", brief.Count)
	}
	if !strings.Contains(brief.Markdown, "Моя запись") {
		t.Errorf("бриф должен содержать свою запись:\n%s", brief.Markdown)
	}
	if strings.Contains(brief.Markdown, "Чужая запись") {
		t.Errorf("бриф не должен содержать запись из чужого scope:\n%s", brief.Markdown)
	}
}

func TestRecall_EmptyResultIsFriendly(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()}, reason.NewNoop(), embed.NewNoop(),
		WithNow(func() int64 { return 1000 }))

	brief, err := r.Recall(ctx, memory.Query{Text: "ничего не найдётся", Scope: "project:empty"})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if brief.Count != 0 || brief.Collapsed != 0 {
		t.Errorf("brief = %+v, want Count=0 Collapsed=0", brief)
	}
	if !strings.Contains(brief.Markdown, "не найдена") {
		t.Errorf("markdown должен быть дружелюбным при пустом результате:\n%s", brief.Markdown)
	}
	if strings.Contains(brief.Markdown, "###") {
		t.Errorf("пустой бриф не должен содержать секции:\n%s", brief.Markdown)
	}
}

func TestRecall_TouchesIncludedMemories(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:touch"

	id := newID(t)
	putMemory(t, ctx, s, memory.Memory{
		Id: id, Kind: memory.KindSemantic, Scope: scope,
		Content:    "запись для проверки touch после recall",
		Importance: 3, CreatedAt: 1, UpdatedAt: 1,
	})

	before, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get before: %v", err)
	}
	if before.AccessCount != 0 {
		t.Fatalf("AccessCount до recall = %d, want 0", before.AccessCount)
	}

	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()}, reason.NewNoop(), embed.NewNoop(),
		WithNow(func() int64 { return 1000 }))
	brief, err := r.Recall(ctx, memory.Query{Text: "запись touch recall", Scope: scope})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if brief.Count != 1 {
		t.Fatalf("Count = %d, want 1", brief.Count)
	}

	after, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after: %v", err)
	}
	if after.AccessCount != 1 {
		t.Errorf("AccessCount после recall = %d, want 1", after.AccessCount)
	}
	if after.AccessedAt == nil {
		t.Errorf("AccessedAt после recall не проставлен")
	}
}

func TestRecall_SummarizeSuccess_AddsSummarySection(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:summarize-ok"

	putMemory(t, ctx, s, memory.Memory{
		Id: newID(t), Kind: memory.KindSemantic, Scope: scope,
		Content: "факт для проверки суммаризации брифа", Summary: strPtr("Факт про суммаризацию"),
		Importance: 3, CreatedAt: 1, UpdatedAt: 1,
	})

	fr := &fakeReasoner{result: "Короткая сводка о фактах проекта."}
	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()}, fr, embed.NewNoop(),
		WithNow(func() int64 { return 1000 }))

	brief, err := r.Recall(ctx, memory.Query{Text: "факт суммаризация брифа", Scope: scope, Summarize: true})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if !fr.called {
		t.Fatalf("Reasoner.Complete не был вызван при Summarize=true")
	}
	if !strings.HasPrefix(brief.Markdown, "### Сводка") {
		t.Fatalf("бриф должен начинаться с секции ### Сводка:\n%s", brief.Markdown)
	}
	if !strings.Contains(brief.Markdown, "Короткая сводка о фактах проекта.") {
		t.Errorf("бриф должен содержать текст сводки:\n%s", brief.Markdown)
	}
	if !strings.Contains(brief.Markdown, "Факт про суммаризацию") {
		t.Errorf("сводка не должна вытеснять сами записи из брифа:\n%s", brief.Markdown)
	}
}

func TestRecall_SummarizeError_DegradesWithoutFailingRecall(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:summarize-err"

	putMemory(t, ctx, s, memory.Memory{
		Id: newID(t), Kind: memory.KindSemantic, Scope: scope,
		Content: "факт для проверки деградации при ошибке суммаризации", Summary: strPtr("Факт про деградацию"),
		Importance: 3, CreatedAt: 1, UpdatedAt: 1,
	})

	fr := &fakeReasoner{err: fmt.Errorf("deepseek: недоступен")}
	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()}, fr, embed.NewNoop(),
		WithNow(func() int64 { return 1000 }))

	brief, err := r.Recall(ctx, memory.Query{Text: "факт деградация суммаризации", Scope: scope, Summarize: true})
	if err != nil {
		t.Fatalf("Recall не должен возвращать ошибку при сбое Reasoner: %v", err)
	}
	if !fr.called {
		t.Fatalf("Reasoner.Complete не был вызван при Summarize=true")
	}
	if strings.Contains(brief.Markdown, "### Сводка") {
		t.Errorf("бриф не должен содержать секцию сводки при ошибке Reasoner:\n%s", brief.Markdown)
	}
	if !strings.Contains(brief.Markdown, "Факт про деградацию") {
		t.Errorf("бриф должен остаться со своими записями при ошибке Reasoner:\n%s", brief.Markdown)
	}
}

func TestRecall_SummarizeFalse_DoesNotCallReasoner(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:summarize-off"

	putMemory(t, ctx, s, memory.Memory{
		Id: newID(t), Kind: memory.KindSemantic, Scope: scope,
		Content: "факт без запроса на суммаризацию", Summary: strPtr("Факт без сводки"),
		Importance: 3, CreatedAt: 1, UpdatedAt: 1,
	})

	fr := &fakeReasoner{result: "эта сводка не должна попасть в бриф"}
	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()}, fr, embed.NewNoop(),
		WithNow(func() int64 { return 1000 }))

	brief, err := r.Recall(ctx, memory.Query{Text: "факт без сводки", Scope: scope})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if fr.called {
		t.Fatalf("Reasoner.Complete не должен вызываться при Summarize=false")
	}
	if strings.Contains(brief.Markdown, "### Сводка") {
		t.Errorf("бриф не должен содержать секцию сводки при Summarize=false:\n%s", brief.Markdown)
	}
}

func TestRecall_SummarizeTrue_EmptyResultSkipsReasoner(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	fr := &fakeReasoner{result: "эта сводка не должна вызываться вовсе"}
	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()}, fr, embed.NewNoop(),
		WithNow(func() int64 { return 1000 }))

	brief, err := r.Recall(ctx, memory.Query{Text: "ничего не найдётся", Scope: "project:summarize-empty", Summarize: true})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if fr.called {
		t.Fatalf("Reasoner.Complete не должен вызываться на пустом результате (платный вызов на 'ничего не найдено')")
	}
	if brief.Count != 0 || brief.Collapsed != 0 {
		t.Errorf("brief = %+v, want Count=0 Collapsed=0", brief)
	}
	if !strings.Contains(brief.Markdown, "не найдена") {
		t.Errorf("markdown должен быть стандартным 'не найдено':\n%s", brief.Markdown)
	}
	if strings.Contains(brief.Markdown, "### Сводка") {
		t.Errorf("пустой бриф не должен содержать секцию сводки:\n%s", brief.Markdown)
	}
}

func TestRecall_SummarizeSuccess_ApproxTokensReflectsFinalMarkdown(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:summarize-tokens"

	putMemory(t, ctx, s, memory.Memory{
		Id: newID(t), Kind: memory.KindSemantic, Scope: scope,
		Content: "факт для проверки пересчёта ApproxTokens после сводки", Summary: strPtr("Факт про токены"),
		Importance: 3, CreatedAt: 1, UpdatedAt: 1,
	})

	fr := &fakeReasoner{result: "Довольно длинная сводка, которая заметно увеличивает размер итогового Markdown-брифа."}
	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()}, fr, embed.NewNoop(),
		WithNow(func() int64 { return 1000 }))

	brief, err := r.Recall(ctx, memory.Query{Text: "факт токены сводка", Scope: scope, Summarize: true})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if !fr.called {
		t.Fatalf("Reasoner.Complete не был вызван при Summarize=true")
	}

	wantTokens := len([]rune(brief.Markdown)) / 4
	if brief.ApproxTokens != wantTokens {
		t.Errorf("ApproxTokens = %d, want %d (пересчёт по итоговому Markdown со сводкой)", brief.ApproxTokens, wantTokens)
	}

	// ApproxTokens после сводки должен быть больше, чем оценка брифа без неё
	// (те же записи, без секции "### Сводка").
	withoutSummary, err := r.Recall(ctx, memory.Query{Text: "факт токены сводка", Scope: scope})
	if err != nil {
		t.Fatalf("Recall (без Summarize): %v", err)
	}
	if brief.ApproxTokens <= withoutSummary.ApproxTokens {
		t.Errorf("ApproxTokens со сводкой (%d) должен быть больше, чем без неё (%d)",
			brief.ApproxTokens, withoutSummary.ApproxTokens)
	}
}

func TestRecall_NoopReasoner_LeavesBriefUnchanged(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:summarize-noop"

	putMemory(t, ctx, s, memory.Memory{
		Id: newID(t), Kind: memory.KindSemantic, Scope: scope,
		Content: "факт для проверки noop reasoner", Summary: strPtr("Факт без реального reasoner"),
		Importance: 3, CreatedAt: 1, UpdatedAt: 1,
	})

	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()}, reason.NewNoop(), embed.NewNoop(),
		WithNow(func() int64 { return 1000 }))

	brief, err := r.Recall(ctx, memory.Query{Text: "факт noop reasoner", Scope: scope, Summarize: true})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if strings.Contains(brief.Markdown, "### Сводка") {
		t.Errorf("Noop Reasoner возвращает пустую строку — бриф не должен получить секцию сводки:\n%s", brief.Markdown)
	}
	if !strings.Contains(brief.Markdown, "Факт без реального reasoner") {
		t.Errorf("бриф должен остаться неизменным при Noop reasoner:\n%s", brief.Markdown)
	}
}

// fakeEmbedder — тестовая реализация embed.Embedder: возвращает фиксированный вектор
// (одинаковый для любого текста запроса) заданной размерности, считает вызовы Embed
// и может вернуть заданную ошибку вместо вектора.
type fakeEmbedder struct {
	dim        int
	vec        []float32
	err        error
	embedCalls int
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.embedCalls++
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = f.vec
	}
	return out, nil
}

func (f *fakeEmbedder) Model() string { return "fake" }
func (f *fakeEmbedder) Dim() int      { return f.dim }

var _ embed.Embedder = (*fakeEmbedder)(nil)

// TestRecall_FoundOnlyByVector демонстрирует снятие ограничения FTS5 на русскую
// морфологию (.claude/memory/mvp-notes.md): запрос "собирать/тестировать" не матчит
// лексически сохранённое "Сборка/тесты" (нет общих токенов вообще), но запись всё
// равно попадает в бриф — через векторный источник.
func TestRecall_FoundOnlyByVector(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:vector-only"

	if err := s.ResetVectorIndex(ctx, "fake-model", 3); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}

	id := newID(t)
	putMemory(t, ctx, s, memory.Memory{
		Id: id, Kind: memory.KindSemantic, Scope: scope,
		Content: "Сборка тесты проекта запускаются через Makefile",
		Summary: strPtr("Про сборку и тесты"), Importance: 3, CreatedAt: 1, UpdatedAt: 1,
	})
	if err := s.PutVector(ctx, id, []float32{1, 0, 0}); err != nil {
		t.Fatalf("PutVector: %v", err)
	}

	fe := &fakeEmbedder{dim: 3, vec: []float32{1, 0, 0}}
	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()}, reason.NewNoop(), fe,
		WithNow(func() int64 { return 1000 }))

	// Перефразировка без единого общего токена с сохранённым Content/Summary.
	brief, err := r.Recall(ctx, memory.Query{Text: "собирать тестировать репозиторий", Scope: scope})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if fe.embedCalls != 1 {
		t.Errorf("embedCalls = %d, want 1", fe.embedCalls)
	}
	if brief.Count != 1 {
		t.Fatalf("Count = %d, want 1 (запись должна найтись только вектором): brief=%+v", brief.Count, brief)
	}
	if !strings.Contains(brief.Markdown, "Про сборку и тесты") {
		t.Errorf("бриф должен содержать запись, найденную вектором:\n%s", brief.Markdown)
	}
}

// TestRecall_FoundByBothSourcesRanksHigher проверяет свойство RRF: запись, найденная
// и лексическим, и векторным источником, должна ранжироваться выше записи, найденной
// только одним из них (сумма 1/(k+rank) по двум источникам больше, чем по одному).
func TestRecall_FoundByBothSourcesRanksHigher(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:rrf-both"

	if err := s.ResetVectorIndex(ctx, "fake-model", 3); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}

	bothID, lexicalOnlyID := newID(t), newID(t)
	// Одинаковые важность/свежесть у обеих записей — единственная разница в
	// финальном скоринге должна идти от вклада источников в RRF.
	putMemory(t, ctx, s, memory.Memory{
		Id: bothID, Kind: memory.KindSemantic, Scope: scope,
		Content: "уникальныймаркерrrf общий термин поиска", Summary: strPtr("Найдена обоими источниками"),
		Importance: 3, CreatedAt: 1000, UpdatedAt: 1000,
	})
	putMemory(t, ctx, s, memory.Memory{
		Id: lexicalOnlyID, Kind: memory.KindSemantic, Scope: scope,
		Content: "уникальныймаркерrrf общий термин поиска", Summary: strPtr("Найдена только лексикой"),
		Importance: 3, CreatedAt: 1000, UpdatedAt: 1000,
	})
	// Вектор кладём только для bothID — lexicalOnlyID останется вне векторного индекса.
	if err := s.PutVector(ctx, bothID, []float32{1, 0, 0}); err != nil {
		t.Fatalf("PutVector: %v", err)
	}

	fe := &fakeEmbedder{dim: 3, vec: []float32{1, 0, 0}}
	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()}, reason.NewNoop(), fe,
		WithNow(func() int64 { return 1000 }))

	brief, err := r.Recall(ctx, memory.Query{Text: "уникальныймаркерrrf общий термин поиска", Scope: scope})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if brief.Count != 2 {
		t.Fatalf("Count = %d, want 2 (обе записи должны попасть в бриф): brief=%+v", brief.Count, brief)
	}
	idxBoth := strings.Index(brief.Markdown, "Найдена обоими источниками")
	idxLexOnly := strings.Index(brief.Markdown, "Найдена только лексикой")
	if idxBoth == -1 || idxLexOnly == -1 {
		t.Fatalf("обе записи должны быть в брифе:\n%s", brief.Markdown)
	}
	if idxBoth > idxLexOnly {
		t.Errorf("запись, найденная обоими источниками, должна ранжироваться выше:\n%s", brief.Markdown)
	}
}

// TestRecall_EmbedErrorDegradesToLexical проверяет деградацию §15: ошибка Embed не
// должна ронять Recall — бриф собирается из FTS без ошибки наружу.
func TestRecall_EmbedErrorDegradesToLexical(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:embed-error"

	putMemory(t, ctx, s, memory.Memory{
		Id: newID(t), Kind: memory.KindSemantic, Scope: scope,
		Content: "запись для проверки деградации при ошибке эмбеддинга", Summary: strPtr("Найдена лексикой при сбое Embed"),
		Importance: 3, CreatedAt: 1, UpdatedAt: 1,
	})

	fe := &fakeEmbedder{dim: 3, err: fmt.Errorf("ollama: недоступен")}
	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()}, reason.NewNoop(), fe,
		WithNow(func() int64 { return 1000 }))

	brief, err := r.Recall(ctx, memory.Query{Text: "запись деградация ошибка эмбеддинга", Scope: scope})
	if err != nil {
		t.Fatalf("Recall не должен возвращать ошибку при сбое Embed: %v", err)
	}
	if fe.embedCalls != 1 {
		t.Errorf("embedCalls = %d, want 1", fe.embedCalls)
	}
	if brief.Count != 1 {
		t.Fatalf("Count = %d, want 1 (бриф должен собраться из FTS): brief=%+v", brief.Count, brief)
	}
	if !strings.Contains(brief.Markdown, "Найдена лексикой при сбое Embed") {
		t.Errorf("бриф должен содержать запись, найденную лексическим поиском:\n%s", brief.Markdown)
	}
}

// TestRecall_NoopEmbedderNeverCallsEmbed проверяет, что при выключенном векторном
// слое (Dim()==0) Embed вообще не вызывается — векторный источник кандидатов не
// подключается, а не молча выполняет бесполезную работу.
func TestRecall_NoopEmbedderNeverCallsEmbed(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:embed-noop"

	putMemory(t, ctx, s, memory.Memory{
		Id: newID(t), Kind: memory.KindSemantic, Scope: scope,
		Content: "запись для проверки noop embedder", Summary: strPtr("Факт при noop embedder"),
		Importance: 3, CreatedAt: 1, UpdatedAt: 1,
	})

	fe := &fakeEmbedder{dim: 0}
	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()}, reason.NewNoop(), fe,
		WithNow(func() int64 { return 1000 }))

	brief, err := r.Recall(ctx, memory.Query{Text: "запись noop embedder", Scope: scope})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if fe.embedCalls != 0 {
		t.Errorf("embedCalls = %d, want 0 (Noop Dim()==0 не должен вызывать Embed)", fe.embedCalls)
	}
	if brief.Count != 1 {
		t.Errorf("Count = %d, want 1", brief.Count)
	}
}

// explainLineRe разбирает explain-довесок, собранный formatExplainLine (задача
// 8.1): "  · via <источники> · rrf <r> · rel <a> + imp <b> + rec <c> + acc <d> = <f>".
var explainLineRe = regexp.MustCompile(
	`· via (\S+) · rrf (-?[\d.]+) · rel (-?[\d.]+) \+ imp (-?[\d.]+) \+ rec (-?[\d.]+) \+ acc (-?[\d.]+) = (-?[\d.]+)`)

// explainLineFor находит explain-строку, относящуюся к записи, чей маркер
// summary/content встречается в md первым после neederMarker, и разбирает её поля.
func explainLineFor(t *testing.T, md, marker string) (sources string, rrf, rel, imp, rec, acc, final float64) {
	t.Helper()
	idx := strings.Index(md, marker)
	if idx == -1 {
		t.Fatalf("маркер %q не найден в брифе:\n%s", marker, md)
	}
	rest := md[idx:]
	m := explainLineRe.FindStringSubmatch(rest)
	if m == nil {
		t.Fatalf("explain-строка не найдена после маркера %q в брифе:\n%s", marker, md)
	}
	parse := func(s string) float64 {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			t.Fatalf("разбор числа %q: %v", s, err)
		}
		return v
	}
	return m[1], parse(m[2]), parse(m[3]), parse(m[4]), parse(m[5]), parse(m[6]), parse(m[7])
}

// TestRecall_ExplainFalse_BriefByteIdentical проверяет требование задачи 8.1:
// Query.Explain=false не должен ни на байт менять бриф (никакого регресса формата
// для существующих клиентов) — сравниваем бриф с явным Explain=false и без него
// вовсе (нулевое значение) для одного и того же запроса.
func TestRecall_ExplainFalse_BriefByteIdentical(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:explain-off"

	putMemory(t, ctx, s, memory.Memory{
		Id: newID(t), Kind: memory.KindSemantic, Scope: scope,
		Content: "запись без явного explain в запросе", Summary: strPtr("Факт без explain"),
		Importance: 3, CreatedAt: 1, UpdatedAt: 1,
	})

	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()}, reason.NewNoop(), embed.NewNoop(),
		WithNow(func() int64 { return 1000 }))

	withoutField, err := r.Recall(ctx, memory.Query{Text: "запись explain", Scope: scope})
	if err != nil {
		t.Fatalf("Recall (без Explain): %v", err)
	}
	explicitFalse, err := r.Recall(ctx, memory.Query{Text: "запись explain", Scope: scope, Explain: false})
	if err != nil {
		t.Fatalf("Recall (Explain: false): %v", err)
	}
	if withoutField.Markdown != explicitFalse.Markdown {
		t.Fatalf("бриф с Explain=false должен быть байт-в-байт идентичен:\n%q\nvs\n%q",
			withoutField.Markdown, explicitFalse.Markdown)
	}
	if strings.Contains(withoutField.Markdown, "· via") || strings.Contains(withoutField.Markdown, "(explain)") {
		t.Errorf("бриф при Explain=false не должен содержать explain-довески:\n%s", withoutField.Markdown)
	}
}

// TestRecall_ExplainTrue_ShowsSourcesAndSumMatchesFinal проверяет саму задачу 8.1:
// заголовок брифа помечен "(explain)", explain-строка под каждой записью содержит
// корректные источники (запись, найденная и лексикой, и вектором -> "lexical+vector";
// запись, найденная только лексикой -> "lexical") и сумма слагаемых Relevance+
// Importance+Recency+Access сходится с Final (допуск на округление печати).
func TestRecall_ExplainTrue_ShowsSourcesAndSumMatchesFinal(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:explain-on"

	if err := s.ResetVectorIndex(ctx, "fake-model", 3); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}

	bothID, lexicalOnlyID := newID(t), newID(t)
	putMemory(t, ctx, s, memory.Memory{
		Id: bothID, Kind: memory.KindSemantic, Scope: scope,
		Content: "уникальныймаркерexplain общий термин поиска", Summary: strPtr("Найдена обоими источниками explain"),
		Importance: 4, CreatedAt: 1000, UpdatedAt: 1000,
	})
	putMemory(t, ctx, s, memory.Memory{
		Id: lexicalOnlyID, Kind: memory.KindSemantic, Scope: scope,
		Content: "уникальныймаркерexplain общий термин поиска", Summary: strPtr("Найдена только лексикой explain"),
		Importance: 2, CreatedAt: 900, UpdatedAt: 900,
	})
	if err := s.PutVector(ctx, bothID, []float32{1, 0, 0}); err != nil {
		t.Fatalf("PutVector: %v", err)
	}

	fe := &fakeEmbedder{dim: 3, vec: []float32{1, 0, 0}}
	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()}, reason.NewNoop(), fe,
		WithNow(func() int64 { return 1000 }))

	brief, err := r.Recall(ctx, memory.Query{
		Text: "уникальныймаркерexplain общий термин поиска", Scope: scope, Explain: true,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if brief.Count != 2 {
		t.Fatalf("Count = %d, want 2: brief=%+v", brief.Count, brief)
	}
	if !strings.HasPrefix(brief.Markdown, "## Релевантная память") || !strings.Contains(strings.SplitN(brief.Markdown, "\n", 2)[0], "(explain)") {
		t.Fatalf("заголовок брифа должен быть помечен (explain):\n%s", brief.Markdown)
	}

	sourcesBoth, rrfBoth, relBoth, impBoth, recBoth, accBoth, finalBoth :=
		explainLineFor(t, brief.Markdown, "Найдена обоими источниками explain")
	if sourcesBoth != "lexical+vector" {
		t.Errorf("sources для записи, найденной обоими источниками = %q, want lexical+vector", sourcesBoth)
	}
	if sum := relBoth + impBoth + recBoth + accBoth; math.Abs(sum-finalBoth) > 0.02 {
		t.Errorf("сумма слагаемых %v (=%.3f) не сходится с final %.3f", []float64{relBoth, impBoth, recBoth, accBoth}, sum, finalBoth)
	}
	if rrfBoth <= 0 {
		t.Errorf("rrf для найденной записи должен быть положительным, got %v", rrfBoth)
	}

	sourcesLex, _, relLex, impLex, recLex, accLex, finalLex :=
		explainLineFor(t, brief.Markdown, "Найдена только лексикой explain")
	if sourcesLex != "lexical" {
		t.Errorf("sources для записи, найденной только лексикой = %q, want lexical", sourcesLex)
	}
	if sum := relLex + impLex + recLex + accLex; math.Abs(sum-finalLex) > 0.02 {
		t.Errorf("сумма слагаемых %v (=%.3f) не сходится с final %.3f", []float64{relLex, impLex, recLex, accLex}, sum, finalLex)
	}
}

// TestCandidates_MatchesRecallOrderAndSet — связующий тест экстракции (задача 10.1):
// публичный Candidates должен отдавать тот же набор записей в том же порядке, что и
// виден в брифе Recall на идентичном запросе (обе точки входа теперь строятся над
// общим scoredCandidates, поведение Recall при этом не изменилось — см. остальные
// тесты файла, прошедшие без правок ожиданий). Три записи одного kind (без деления на
// секции брифа) с разными источниками — лексика, вектор, оба сразу — так порядок строк
// в Markdown совпадает с глобальным порядком по final-score, который и проверяем.
func TestCandidates_MatchesRecallOrderAndSet(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:candidates-extraction"

	if err := s.ResetVectorIndex(ctx, "fake-model", 3); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}

	bothID, lexOnlyID, vecOnlyID := newID(t), newID(t), newID(t)
	putMemory(t, ctx, s, memory.Memory{
		Id: bothID, Kind: memory.KindSemantic, Scope: scope,
		Content: "уникальныймаркерcand общий термин поиска", Summary: strPtr("Both source item"),
		Importance: 5, CreatedAt: 1000, UpdatedAt: 1000,
	})
	putMemory(t, ctx, s, memory.Memory{
		Id: lexOnlyID, Kind: memory.KindSemantic, Scope: scope,
		Content: "уникальныймаркерcand общий термин поиска", Summary: strPtr("Lexical only item"),
		Importance: 3, CreatedAt: 1000, UpdatedAt: 1000,
	})
	// Слова без общего корня со словами запроса (никакой стеммер не должен свести их
	// к тому же токену) — эта запись обязана найтись только вектором, не лексикой.
	putMemory(t, ctx, s, memory.Memory{
		Id: vecOnlyID, Kind: memory.KindSemantic, Scope: scope,
		Content: "жираф слон черепаха облако", Summary: strPtr("Vector only item"),
		Importance: 3, CreatedAt: 1000, UpdatedAt: 1000,
	})
	// Вектор запроса и обеих "векторных" записей совпадает; lexOnlyID остаётся вне
	// векторного индекса — найдётся только лексикой.
	if err := s.PutVector(ctx, bothID, []float32{1, 0, 0}); err != nil {
		t.Fatalf("PutVector both: %v", err)
	}
	if err := s.PutVector(ctx, vecOnlyID, []float32{1, 0, 0}); err != nil {
		t.Fatalf("PutVector vecOnly: %v", err)
	}

	fe := &fakeEmbedder{dim: 3, vec: []float32{1, 0, 0}}
	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()}, reason.NewNoop(), fe,
		WithNow(func() int64 { return 1000 }))

	query := memory.Query{Text: "уникальныймаркерcand общий термин поиска", Scope: scope}

	brief, err := r.Recall(ctx, query)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if brief.Count != 3 {
		t.Fatalf("Count = %d, want 3 (бюджет достаточен для всех трёх записей): brief=%+v", brief.Count, brief)
	}

	candidates, err := r.Candidates(ctx, query)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(candidates) != 3 {
		t.Fatalf("len(candidates) = %d, want 3", len(candidates))
	}

	// Порядок в брифе (единственная секция kind=semantic, значит порядок строк
	// совпадает с глобальным порядком по final) должен совпадать с порядком
	// Candidates — сам числовой порядок (both выше единственного источника, а между
	// lexical-only/vector-only решает случайный порядок в исходных срезах SearchLexical/
	// SearchVector до RRF) не входит в контракт этого теста, важно только совпадение
	// Recall и Candidates между собой на одном и том же запросе.
	var wantOrder []string
	for _, c := range candidates {
		switch c.Memory.Id {
		case bothID:
			wantOrder = append(wantOrder, "Both source item")
		case lexOnlyID:
			wantOrder = append(wantOrder, "Lexical only item")
		case vecOnlyID:
			wantOrder = append(wantOrder, "Vector only item")
		}
	}
	lastIdx := -1
	for _, marker := range wantOrder {
		idx := strings.Index(brief.Markdown, marker)
		if idx == -1 {
			t.Fatalf("бриф должен содержать %q:\n%s", marker, brief.Markdown)
		}
		if idx <= lastIdx {
			t.Fatalf("бриф должен перечислять записи в том же порядке, что Candidates (%v):\n%s", wantOrder, brief.Markdown)
		}
		lastIdx = idx
	}

	wantSources := map[string]string{bothID: "lexical+vector", lexOnlyID: "lexical", vecOnlyID: "vector"}
	for i, c := range candidates {
		if want := wantSources[c.Memory.Id]; c.Source != want {
			t.Errorf("candidates[%d] (id=%s).Source = %q, want %q", i, c.Memory.Id, c.Source, want)
		}
	}
}
