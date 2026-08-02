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

// TestRecall_DebtSectionInOrder проверяет, что kind=debt попадает в дефолтную
// выдачу recall (без явного фильтра kinds) как отдельная секция "Tech debt (debt)",
// расположенная между How-to (procedural) и Recent context (episodic) — см. годок
// пакетной переменной sections.
func TestRecall_DebtSectionInOrder(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:debt-section"

	semID, procID, debtID, epiID := newID(t), newID(t), newID(t), newID(t)
	putMemory(t, ctx, s, memory.Memory{
		Id: semID, Kind: memory.KindSemantic, Scope: scope,
		Content: "общий термин факт про техдолг секцию", Summary: strPtr("Семантический факт"),
		Importance: 3, CreatedAt: 1, UpdatedAt: 1,
	})
	putMemory(t, ctx, s, memory.Memory{
		Id: procID, Kind: memory.KindProcedural, Scope: scope,
		Content: "общий термин факт про техдолг секцию", Summary: strPtr("Процедурный рецепт"),
		Importance: 3, CreatedAt: 2, UpdatedAt: 2,
	})
	putMemory(t, ctx, s, memory.Memory{
		Id: debtID, Kind: memory.KindDebt, Scope: scope,
		Content: "общий термин факт про техдолг секцию", Summary: strPtr("Известная проблема архитектуры"),
		Importance: 3, CreatedAt: 3, UpdatedAt: 3,
	})
	putMemory(t, ctx, s, memory.Memory{
		Id: epiID, Kind: memory.KindEpisodic, Scope: scope,
		Content: "общий термин факт про техдолг секцию", Summary: strPtr("Эпизодическое событие"),
		Importance: 3, CreatedAt: 4, UpdatedAt: 4,
	})

	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()}, reason.NewNoop(), embed.NewNoop(),
		WithNow(func() int64 { return 1000 }))

	brief, err := r.Recall(ctx, memory.Query{Text: "термин факт техдолг секцию", Scope: scope})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	idxHowTo := strings.Index(brief.Markdown, "### How-to (procedural)")
	idxDebt := strings.Index(brief.Markdown, "### Tech debt (debt)")
	idxRecent := strings.Index(brief.Markdown, "### Recent context (episodic)")
	if idxHowTo == -1 || idxDebt == -1 || idxRecent == -1 {
		t.Fatalf("все секции должны присутствовать:\n%s", brief.Markdown)
	}
	if !(idxHowTo < idxDebt && idxDebt < idxRecent) {
		t.Errorf("порядок секций должен быть How-to -> Tech debt -> Recent context:\n%s", brief.Markdown)
	}
	if !strings.Contains(sectionOf(t, brief.Markdown, "Tech debt (debt)"), "Известная проблема архитектуры") {
		t.Errorf("секция Tech debt должна содержать debt-запись:\n%s", brief.Markdown)
	}
}

// TestRecall_UnknownKindStillInBody проверяет DESIGN.md §15 "никаких тихих
// обрезаний": запись с kind, не входящим ни в одну известную секцию (годок
// пакетной переменной sections) — например, более новый клиент на общей
// PostgreSQL-базе (DESIGN.md §2.1) записал kind, которого этот бинарь ещё не
// знает, либо запись положена в обход write.Save (как cmd/memo/import.go) — уже
// проходит selectByBudget (учтена в brief.Count) и обязана присутствовать в теле
// брифа отдельным блоком, а не пропадать молча.
func TestRecall_UnknownKindStillInBody(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:unknown-kind"

	// Store.Put напрямую, в обход write.Save (которая валидирует kind) — тот же
	// путь, что даёт находка: cmd/memo/import.go кладёт memory.Kind(rec.Kind) в
	// store.Put без валидации.
	putMemory(t, ctx, s, memory.Memory{
		Id: newID(t), Kind: memory.Kind("future_kind"), Scope: scope,
		Content: "запись неизвестного будущего kind", Summary: strPtr("Из будущего"),
		Importance: 3, CreatedAt: 1, UpdatedAt: 1,
	})

	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()}, reason.NewNoop(), embed.NewNoop(),
		WithNow(func() int64 { return 1000 }))

	brief, err := r.Recall(ctx, memory.Query{Text: "запись неизвестного будущего kind", Scope: scope})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if brief.Count != 1 {
		t.Fatalf("Count = %d, want 1 (запись учтена в бюджете, значит обязана быть и в теле)", brief.Count)
	}
	if !strings.Contains(brief.Markdown, "Из будущего") {
		t.Errorf("тело брифа должно содержать запись неизвестного kind, а не молча её терять:\n%s", brief.Markdown)
	}
}

// TestRecall_DebtWeightLowersRank проверяет РЕАЛЬНЫЙ эффект понижающего множителя
// config.RecallConfig.DebtWeight — вытеснение записи kind=debt из бюджета токенов
// (не просто перестановку строк в уже целиком вместившемся брифе: composeBody
// группирует записи по секциям в фиксированном порядке (DESIGN.md §8), так что
// позиция строки в Markdown ничего не говорит о ранжировании — только бюджетный
// отбор в selectByBudget его проявляет).
//
// Debt-запись намеренно получает более высокую Importance, чем semantic — запас
// по сигналам ДО применения kindFactor, с большим отрывом, чем любая возможная
// асимметрия RRF-тай-брейка между двумя записями с идентичным содержимым (та
// зависит от порядка, в котором стор возвращает связанные строки, и не должна
// решать исход теста). Token_budget=1 (минимально возможный) по правилу
// selectByBudget "минимум одна запись" гарантированно оставляет в брифе ровно
// самую высокоранговую запись, а вторая уходит в Collapsed:
//   - при DebtWeight=1.0 kindFactor не понижает debt, и она (с преимуществом по
//     importance) остаётся в бюджете, а semantic вытесняется;
//   - при малом DebtWeight (0.1) debt проигрывает semantic-записи, несмотря на
//     исходное преимущество по importance, и вытесняется сама — это и есть эффект
//     множителя, изолированный от прочих сигналов.
func TestRecall_DebtWeightLowersRank(t *testing.T) {
	newFixture := func(t *testing.T) (store.Store, memory.Query) {
		t.Helper()
		s := newTestStore(t)
		ctx := context.Background()
		scope := "project:debt-weight"
		semID, debtID := newID(t), newID(t)
		putMemory(t, ctx, s, memory.Memory{
			Id: semID, Kind: memory.KindSemantic, Scope: scope,
			Content: "уникальныймаркерdebtweight общий термин поиска", Summary: strPtr("Семантический факт про вес"),
			Importance: 1, CreatedAt: 1000, UpdatedAt: 1000,
		})
		putMemory(t, ctx, s, memory.Memory{
			Id: debtID, Kind: memory.KindDebt, Scope: scope,
			Content: "уникальныймаркерdebtweight общий термин поиска", Summary: strPtr("Долг про вес"),
			Importance: 5, CreatedAt: 1000, UpdatedAt: 1000,
		})
		q := memory.Query{Text: "уникальныймаркерdebtweight общий термин поиска", Scope: scope, TokenBudget: 1}
		return s, q
	}

	t.Run("DebtWeight=1.0 — debt (с преимуществом по importance) остаётся в бюджете", func(t *testing.T) {
		ctx := context.Background()
		s, q := newFixture(t)
		r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights(), DebtWeight: 1.0}, reason.NewNoop(), embed.NewNoop(),
			WithNow(func() int64 { return 1000 }))

		brief, err := r.Recall(ctx, q)
		if err != nil {
			t.Fatalf("Recall: %v", err)
		}
		if brief.Count != 1 || brief.Collapsed != 1 {
			t.Fatalf("brief = %+v, want Count=1 Collapsed=1", brief)
		}
		if !strings.Contains(brief.Markdown, "Долг про вес") {
			t.Errorf("при DebtWeight=1.0 debt-запись должна остаться в бюджете:\n%s", brief.Markdown)
		}
		if strings.Contains(brief.Markdown, "Семантический факт про вес") {
			t.Errorf("semantic-запись (ниже по importance) должна быть вытеснена:\n%s", brief.Markdown)
		}
	})

	t.Run("DebtWeight=0.1 — debt вытесняется semantic-записью", func(t *testing.T) {
		ctx := context.Background()
		s, q := newFixture(t)
		r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights(), DebtWeight: 0.1}, reason.NewNoop(), embed.NewNoop(),
			WithNow(func() int64 { return 1000 }))

		brief, err := r.Recall(ctx, q)
		if err != nil {
			t.Fatalf("Recall: %v", err)
		}
		if brief.Count != 1 || brief.Collapsed != 1 {
			t.Fatalf("brief = %+v, want Count=1 Collapsed=1", brief)
		}
		if !strings.Contains(brief.Markdown, "Семантический факт про вес") {
			t.Errorf("при малом DebtWeight semantic-запись должна вытеснить debt из бюджета:\n%s", brief.Markdown)
		}
		if strings.Contains(brief.Markdown, "Долг про вес") {
			t.Errorf("debt-запись (несмотря на более высокий importance) должна быть вытеснена малым DebtWeight:\n%s", brief.Markdown)
		}
	})
}

// TestRecall_DebtExplainMatchesKindFactor проверяет, что explain-трасса debt-записи
// честно отражает применённый множитель (задача 8.1 расширена KindFactor): Final
// равен сумме слагаемых, умноженной на KindFactor, а не самой сумме.
func TestRecall_DebtExplainMatchesKindFactor(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:debt-explain"

	debtID := newID(t)
	putMemory(t, ctx, s, memory.Memory{
		Id: debtID, Kind: memory.KindDebt, Scope: scope,
		Content: "уникальныймаркерdebtexplain термин поиска", Summary: strPtr("Долг с трассой"),
		Importance: 4, CreatedAt: 1000, UpdatedAt: 1000,
	})

	const debtWeight = 0.6
	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights(), DebtWeight: debtWeight}, reason.NewNoop(), embed.NewNoop(),
		WithNow(func() int64 { return 1000 }))

	brief, err := r.Recall(ctx, memory.Query{
		Text: "уникальныймаркерdebtexplain термин поиска", Scope: scope, Explain: true,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if brief.Count != 1 {
		t.Fatalf("Count = %d, want 1: brief=%+v", brief.Count, brief)
	}

	e := explainLineFor(t, brief.Markdown, "Долг с трассой")
	if e.sources != "lexical" {
		t.Errorf("sources = %q, want lexical", e.sources)
	}
	sum := e.rel + e.imp + e.rec + e.acc
	if math.Abs(e.sum-sum) > 0.02 {
		t.Errorf("промежуточная сумма в трассе(%.3f) не сходится с rel+imp+rec+acc(%.3f)", e.sum, sum)
	}
	if math.Abs(e.kindFactor-debtWeight) > 0.001 {
		t.Errorf("KindFactor в трассе = %.3f, want debtWeight %.2f", e.kindFactor, debtWeight)
	}
	wantFinal := sum * debtWeight
	if math.Abs(e.final-wantFinal) > 0.02 {
		t.Errorf("final(%.3f) должен быть суммой слагаемых(%.3f) * debtWeight(%.2f) = %.3f", e.final, sum, debtWeight, wantFinal)
	}
}

// TestNew_DebtWeightFallsBackOnInvalidValues проверяет, что recall.New откатывает
// debtWeight на дефолт config.DefaultDebtWeight для ЛЮБОГО значения вне (0, 1] —
// включая NaN/+Inf/-Inf, которые TOML 1.0 (BurntSushi/toml) допускает как
// литералы. Все сравнения (==, <, >) дают false для NaN, поэтому наивная проверка
// "debtWeight <= 0 || debtWeight > 1" пропускала бы NaN мимо обеих веток и
// оставляла r.debtWeight == NaN, откуда final-score debt-записей в rank()
// становился NaN. config.RecallConfig.Validate отсекает такие значения раньше при
// вызове через config.Load — этот тест проверяет именно New() как отдельную линию
// защиты (для тестов, будущих вызывающих, не прошедших через Load), напрямую по
// приватному полю debtWeight (тест лежит в package recall) — без похода через
// Store/Recall/Markdown ради проверки одного числа.
//
// Кейс {"ноль", 0} заодно покрывает и путь "поле не задано вовсе": в Go нулевое
// значение float64-поля и явное DebtWeight: 0 неразличимы, так что отдельный тест
// на "config.RecallConfig{} без явного DebtWeight" был бы точным дублем этого
// кейса. Мутант "config.DefaultDebtWeight = 1.0" должен красить весь набор.
func TestNew_DebtWeightFallsBackOnInvalidValues(t *testing.T) {
	cases := []struct {
		name string
		in   float64
	}{
		{"NaN", math.NaN()},
		{"+Inf", math.Inf(1)},
		{"-Inf", math.Inf(-1)},
		{"больше_1", 1.5},
		{"ноль", 0},
		{"отрицательное", -0.1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := New(nil, config.RecallConfig{DebtWeight: tc.in}, reason.NewNoop(), embed.NewNoop())
			if math.IsNaN(r.debtWeight) || math.IsInf(r.debtWeight, 0) {
				t.Fatalf("debtWeight не должен быть NaN/Inf при DebtWeight=%v, got %v", tc.in, r.debtWeight)
			}
			if r.debtWeight != config.DefaultDebtWeight {
				t.Errorf("debtWeight = %v, want дефолт %v (DebtWeight=%v вне (0,1])", r.debtWeight, config.DefaultDebtWeight, tc.in)
			}
		})
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
// 8.1, расширена KindFactor задачей с техдолгом): "  · via <источники> · rrf <r>
// · rel <a> + imp <b> + rec <c> + acc <d> = <sum>", за которым, ТОЛЬКО если
// KindFactor != 1, следует необязательный хвост " × kind <factor> = <final>"
// (formatExplainLine не печатает тождественный хвост "× kind 1.00 = <sum>" —
// он же тратил бы token_budget на константу). Группы 8 (factor) и 9 (final)
// пустые, если хвоста нет — explainLineFor трактует это как factor=1, final=sum.
var explainLineRe = regexp.MustCompile(
	`· via (\S+) · rrf (-?[\d.]+) · rel (-?[\d.]+) \+ imp (-?[\d.]+) \+ rec (-?[\d.]+) \+ acc (-?[\d.]+) = (-?[\d.]+)(?: × kind (-?[\d.]+) = (-?[\d.]+))?`)

// explainLine — разобранные поля одной explain-строки (см. explainLineRe), уже
// приведённые к float64, с именованными полями вместо позиционных возвратов.
type explainLine struct {
	sources                 string
	rrf, rel, imp, rec, acc float64
	sum                     float64 // rel+imp+rec+acc, ДО умножения на kindFactor
	kindFactor              float64 // 1.0, если хвост "× kind" не напечатан (формат опустил тождественный множитель)
	final                   float64 // sum*kindFactor — совпадает с sum, если хвоста нет
}

// explainLineFor находит explain-строку, относящуюся к записи, чей маркер
// summary/content встречается в md первым после marker, и разбирает её поля.
func explainLineFor(t *testing.T, md, marker string) explainLine {
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
	e := explainLine{
		sources: m[1],
		rrf:     parse(m[2]),
		rel:     parse(m[3]),
		imp:     parse(m[4]),
		rec:     parse(m[5]),
		acc:     parse(m[6]),
		sum:     parse(m[7]),
	}
	if m[8] == "" {
		// Хвост "× kind ... = ..." не напечатан — formatExplainLine опускает его
		// именно тогда, когда kindFactor == 1 и, следовательно, final == sum.
		e.kindFactor, e.final = 1, e.sum
	} else {
		e.kindFactor, e.final = parse(m[8]), parse(m[9])
	}
	return e
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

// TestRecall_ExplainTrue_ShowsSourcesAndSumMatchesFinal проверяет саму задачу 8.1
// (расширена KindFactor задачей с техдолгом): заголовок брифа помечен "(explain)",
// explain-строка под каждой записью содержит корректные источники (запись,
// найденная и лексикой, и вектором -> "lexical+vector"; запись, найденная только
// лексикой -> "lexical"), сумма слагаемых Relevance+Importance+Recency+Access
// сходится с промежуточной суммой в трассе, KindFactor равен 1.0 для НЕ-debt
// записей (явное требование задачи с техдолгом — kind_factor понижает только
// debt), а настоящий Final равен (сумма слагаемых) * KindFactor, а не самой сумме.
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

	eBoth := explainLineFor(t, brief.Markdown, "Найдена обоими источниками explain")
	if eBoth.sources != "lexical+vector" {
		t.Errorf("sources для записи, найденной обоими источниками = %q, want lexical+vector", eBoth.sources)
	}
	if sum := eBoth.rel + eBoth.imp + eBoth.rec + eBoth.acc; math.Abs(sum-eBoth.sum) > 0.02 {
		t.Errorf("сумма слагаемых %v (=%.3f) не сходится с промежуточной суммой в трассе %.3f",
			[]float64{eBoth.rel, eBoth.imp, eBoth.rec, eBoth.acc}, sum, eBoth.sum)
	}
	if math.Abs(eBoth.kindFactor-1.0) > 0.001 {
		t.Errorf("KindFactor для не-debt (semantic) записи = %.3f, want 1.0", eBoth.kindFactor)
	}
	if wantFinal := eBoth.sum * eBoth.kindFactor; math.Abs(wantFinal-eBoth.final) > 0.02 {
		t.Errorf("final %.3f должен быть (сумма слагаемых %.3f) * KindFactor %.3f = %.3f", eBoth.final, eBoth.sum, eBoth.kindFactor, wantFinal)
	}
	if eBoth.rrf <= 0 {
		t.Errorf("rrf для найденной записи должен быть положительным, got %v", eBoth.rrf)
	}

	eLex := explainLineFor(t, brief.Markdown, "Найдена только лексикой explain")
	if eLex.sources != "lexical" {
		t.Errorf("sources для записи, найденной только лексикой = %q, want lexical", eLex.sources)
	}
	if sum := eLex.rel + eLex.imp + eLex.rec + eLex.acc; math.Abs(sum-eLex.sum) > 0.02 {
		t.Errorf("сумма слагаемых %v (=%.3f) не сходится с промежуточной суммой в трассе %.3f",
			[]float64{eLex.rel, eLex.imp, eLex.rec, eLex.acc}, sum, eLex.sum)
	}
	if math.Abs(eLex.kindFactor-1.0) > 0.001 {
		t.Errorf("KindFactor для не-debt (semantic) записи = %.3f, want 1.0", eLex.kindFactor)
	}
	if wantFinal := eLex.sum * eLex.kindFactor; math.Abs(wantFinal-eLex.final) > 0.02 {
		t.Errorf("final %.3f должен быть (сумма слагаемых %.3f) * KindFactor %.3f = %.3f", eLex.final, eLex.sum, eLex.kindFactor, wantFinal)
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
