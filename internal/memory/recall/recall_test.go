package recall

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"agentmemory/internal/config"
	"agentmemory/internal/memory"
	"agentmemory/internal/memory/store"
)

func newTestStore(t *testing.T) store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "memory.db"))
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

	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()},
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
	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()},
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

	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()},
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

	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()},
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

	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()},
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

	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()},
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

	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()},
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

	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()},
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

	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()},
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
