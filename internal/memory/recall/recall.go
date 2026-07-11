// Package recall реализует read path AgentMemory (DESIGN.md §8): гибридный поиск
// кандидатов (лексический + векторный), слияние через Reciprocal Rank Fusion,
// ранжирование по бизнес-сигналам и сборку Markdown-брифа под бюджет токенов.
package recall

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"agentmemory/internal/config"
	"agentmemory/internal/memory"
	"agentmemory/internal/memory/reason"
	"agentmemory/internal/memory/store"
)

const (
	// rrfK — константа Reciprocal Rank Fusion (DESIGN.md §8): score(doc) = Σ 1/(k+rank).
	// k=60 — общепринятое значение из литературы по RRF, сглаживает влияние
	// единичного высокого ранга в одном источнике.
	rrfK = 60

	// defaultCandidateLimit — сколько кандидатов запрашивать у каждого источника
	// поиска, если Query.Limit не задан явно.
	defaultCandidateLimit = 50

	// recencyHalfLife — период полураспада признака свежести: запись, созданная
	// (или в последний раз затронутая) этим временем назад, вносит в recency_decay
	// вдвое меньший вклад, чем свежесозданная. 14 дней — компромисс между долгоживущими
	// semantic/procedural (которым не нужно быстро "остывать") и episodic (которые по
	// DESIGN.md §2 устаревают "средне"): месячные факты всё ещё заметно всплывают,
	// но события недельной давности уже ощутимо уступают вчерашним.
	recencyHalfLife = 14 * 24 * time.Hour

	// tokenCharsPerToken — эвристика оценки токенов: len(runes)/4. Грубое, но
	// стандартное для GPT-подобных токенизаторов приближение (~4 символа на токен
	// для латиницы; для кириллицы токенов на символ обычно больше, так что оценка
	// консервативно занижает бюджет использования — не превышает его).
	tokenCharsPerToken = 4
)

// summarizePrompt — инструкция для Reasoner при Query.Summarize (DESIGN.md §8):
// просим короткую сводку брифа, без выдумывания фактов и со сменой языка исходных записей.
const summarizePrompt = "Сожми выжимку памяти в один короткий абзац-сводку (3–5 предложений), " +
	"сохрани язык исходных записей, не выдумывай фактов."

// Option настраивает Recaller при создании.
type Option func(*Recaller)

// WithNow подменяет источник текущего времени (для детерминированных тестов recency).
func WithNow(now func() int64) Option {
	return func(r *Recaller) { r.now = now }
}

// Recaller реализует Recall — сборку recall-брифа поверх Store.
type Recaller struct {
	store         store.Store
	weights       config.RecallWeights
	defaultBudget int
	now           func() int64
	reasoner      reason.Reasoner
}

// New создаёт Recaller поверх заданного Store с весами и дефолтным бюджетом токенов
// из конфига (DESIGN.md §13). reasoner используется опционально (Query.Summarize) —
// на Этапе 1 передаётся reason.NewNoop().
func New(s store.Store, cfg config.RecallConfig, reasoner reason.Reasoner, opts ...Option) *Recaller {
	budget := cfg.DefaultTokenBudget
	if budget <= 0 {
		budget = 800
	}
	r := &Recaller{
		store:         s,
		weights:       cfg.Weights,
		defaultBudget: budget,
		now:           func() int64 { return time.Now().Unix() },
		reasoner:      reasoner,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// section — одна секция брифа: заголовок + kind, который она отображает. Порядок
// среза задаёт порядок секций в брифе (DESIGN.md §8).
type section struct {
	kind  memory.Kind
	title string
}

var sections = []section{
	{memory.KindSemantic, "Facts (semantic)"},
	{memory.KindProcedural, "How-to (procedural)"},
	{memory.KindEpisodic, "Recent context (episodic)"},
}

// rrfEntry — запись, слитая из одного или нескольких источников, с накопленным RRF-score.
type rrfEntry struct {
	mem memory.Memory
	rrf float64
}

// scoredCandidate — запись с итоговым final-score после наложения бизнес-сигналов.
type scoredCandidate struct {
	mem   memory.Memory
	final float64
}

// Recall выполняет read path целиком: кандидаты -> RRF-слияние -> ранжирование ->
// сборка брифа под token_budget -> усиление доступа (Touch) для вошедших записей
// (DESIGN.md §8).
func (r *Recaller) Recall(ctx context.Context, q memory.Query) (memory.Brief, error) {
	budget := q.TokenBudget
	if budget <= 0 {
		budget = r.defaultBudget
	}

	candQuery := q
	if candQuery.Limit <= 0 {
		candQuery.Limit = defaultCandidateLimit
	}

	lexical, err := r.store.SearchLexical(ctx, candQuery)
	if err != nil {
		return memory.Brief{}, fmt.Errorf("recall: лексический поиск: %w", err)
	}
	// SearchVector пока заглушка (Embedder=noop, Этап 1), но вызываем её всегда,
	// чтобы Этап 3 подключил векторный источник без правок этого пути.
	vector, err := r.store.SearchVector(ctx, nil, candQuery)
	if err != nil {
		return memory.Brief{}, fmt.Errorf("recall: векторный поиск: %w", err)
	}

	merged := fuseRRF(lexical, vector)
	if len(merged) == 0 {
		// Нечего суммировать и незачем платить за вызов Reasoner на тексте
		// "ничего не найдено" — ранний выход до withSummary.
		return emptyBrief(), nil
	}

	scored := r.rank(merged)
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].final != scored[j].final {
			return scored[i].final > scored[j].final
		}
		// Детерминированный тай-брейк: свежее — первым.
		return scored[i].mem.CreatedAt > scored[j].mem.CreatedAt
	})

	included, collapsed := selectByBudget(scored, budget)

	for _, c := range included {
		// Touch — best-effort: ошибка усиления доступа не должна ронять recall.
		_ = r.store.Touch(ctx, c.mem.Id)
	}

	brief := buildBrief(included, collapsed)
	if q.Summarize {
		brief = r.withSummary(ctx, brief)
	}
	return brief, nil
}

// withSummary просит Reasoner сжать уже собранный бриф в короткую сводку и, если
// получилось, добавляет секцию "### Сводка" перед остальным телом брифа. Сводка не
// участвует в token_budget-отборе ЗАПИСЕЙ (селектится уже после сборки Markdown) —
// Count/Collapsed остаются в терминах самих записей. Но ApproxTokens пересчитывается
// по итоговому Markdown (с добавленной секцией) той же эвристикой estimateTokens, что
// и при сборке брифа — иначе поле лжёт о фактическом размере выдачи (DESIGN.md §15:
// честность выдачи). Любая ошибка Reasoner или пустой ответ деградируют до брифа без
// сводки — ошибка наружу не отдаётся, только в лог.
func (r *Recaller) withSummary(ctx context.Context, brief memory.Brief) memory.Brief {
	summary, err := r.reasoner.Complete(ctx, summarizePrompt+"\n\n"+brief.Markdown)
	if err != nil {
		log.Printf("recall: суммаризация брифа: %v", err)
		return brief
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return brief
	}
	brief.Markdown = "### Сводка\n\n" + summary + "\n\n" + brief.Markdown
	brief.ApproxTokens = estimateTokens(brief.Markdown)
	return brief
}

// fuseRRF сливает кандидатов от нескольких источников поиска через Reciprocal Rank
// Fusion (DESIGN.md §8): каждый источник ранжируется независимо по своему Score,
// вклады 1/(k+rank) суммируются по Memory.Id. Работает корректно при 1..N источниках:
// при единственном источнике RRF вырождается в монотонную функцию ранга.
func fuseRRF(sources ...[]memory.Candidate) []rrfEntry {
	byID := map[string]*rrfEntry{}
	var order []string

	for _, src := range sources {
		if len(src) == 0 {
			continue
		}
		ranked := append([]memory.Candidate(nil), src...)
		sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].Score > ranked[j].Score })

		for i, c := range ranked {
			rank := i + 1
			e, ok := byID[c.Memory.Id]
			if !ok {
				e = &rrfEntry{mem: c.Memory}
				byID[c.Memory.Id] = e
				order = append(order, c.Memory.Id)
			}
			e.rrf += 1.0 / float64(rrfK+rank)
		}
	}

	result := make([]rrfEntry, 0, len(order))
	for _, id := range order {
		result = append(result, *byID[id])
	}
	return result
}

// rank накладывает бизнес-сигналы поверх RRF (DESIGN.md §8 п.3):
// final = w_rel·rrf_norm + w_imp·(importance/5) + w_rec·recency_decay + w_acc·log(1+access_count).
// rrf_norm нормирует rrf в [0,1] по максимуму в текущей выдаче.
func (r *Recaller) rank(entries []rrfEntry) []scoredCandidate {
	maxRRF := 0.0
	for _, e := range entries {
		if e.rrf > maxRRF {
			maxRRF = e.rrf
		}
	}

	now := r.now()
	result := make([]scoredCandidate, 0, len(entries))
	for _, e := range entries {
		rrfNorm := 0.0
		if maxRRF > 0 {
			rrfNorm = e.rrf / maxRRF
		}
		importanceNorm := float64(e.mem.Importance) / 5.0
		recency := recencyDecay(e.mem, now)
		accessBoost := math.Log1p(float64(e.mem.AccessCount))

		final := r.weights.Relevance*rrfNorm +
			r.weights.Importance*importanceNorm +
			r.weights.Recency*recency +
			r.weights.Access*accessBoost

		result = append(result, scoredCandidate{mem: e.mem, final: final})
	}
	return result
}

// recencyDecay — экспоненциальное затухание по возрасту записи: используется
// AccessedAt, если он проставлен (последнее обращение освежает запись), иначе
// CreatedAt. Период полураспада — recencyHalfLife.
func recencyDecay(m memory.Memory, now int64) float64 {
	ts := m.CreatedAt
	if m.AccessedAt != nil {
		ts = *m.AccessedAt
	}
	age := now - ts
	if age < 0 {
		age = 0
	}
	halfLifeSeconds := recencyHalfLife.Seconds()
	return math.Exp(-math.Ln2 * float64(age) / halfLifeSeconds)
}

// briefItem — запись со строкой брифа, уже отформатированной formatLine. Строка
// считается ровно один раз (в selectByBudget) и переиспользуется в composeBody —
// так оценка бюджета и финальный текст брифа гарантированно не расходятся.
type briefItem struct {
	mem  memory.Memory
	line string
}

// selectByBudget отбирает кандидатов в порядке убывания final под token_budget.
// Бюджет должен покрывать не только сами строки, но и Markdown-обвязку финального
// брифа — верхний заголовок, заголовки секций попавших kind'ов, разделители между
// блоками и строку "свёрнуто ещё N" (иначе бриф систематически превышает
// token_budget на размер этой обвязки — DESIGN.md §15: честность выдачи). Алгоритм:
//  1. Жадно набираем строки без учёта обвязки — верхняя граница набора, тот же
//     порядок отбора, что и раньше (останавливаемся на первой не влезшей строке,
//     без случайных пропусков середины списка).
//  2. Гарантируем минимум одну запись, если кандидаты вообще есть: крошечный
//     бюджет не должен превращать непустую выдачу в «0 записей» — честнее отдать
//     самую релевантную запись с превышением бюджета, чем ничего.
//  3. Пока фактическая стоимость собранного брифа (composeMarkdown, т.е. реальный
//     Markdown с обвязкой) не укладывается в бюджет — обрезаем с хвоста наименее
//     релевантные записи, но не ниже одной.
func selectByBudget(scored []scoredCandidate, budget int) ([]briefItem, int) {
	if len(scored) == 0 {
		return nil, 0
	}

	items := make([]briefItem, len(scored))
	for i, c := range scored {
		items[i] = briefItem{mem: c.mem, line: formatLine(c.mem)}
	}

	var included []briefItem
	used := 0
	for _, it := range items {
		cost := estimateTokens(it.line)
		if used+cost > budget {
			break
		}
		included = append(included, it)
		used += cost
	}
	if len(included) == 0 {
		included = items[:1]
	}

	for len(included) > 1 {
		collapsed := len(items) - len(included)
		if _, cost := composeMarkdown(included, collapsed); cost <= budget {
			break
		}
		included = included[:len(included)-1]
	}

	return included, len(items) - len(included)
}

// estimateTokens — грубая оценка числа токенов: len(runes)/4 (см. tokenCharsPerToken).
func estimateTokens(s string) int {
	return utf8.RuneCountInString(s) / tokenCharsPerToken
}

// formatLine форматирует одну строку брифа: краткая форма (Summary), если задана,
// иначе Content, плюс маркер важности.
func formatLine(m memory.Memory) string {
	text := m.Content
	if m.Summary != nil && strings.TrimSpace(*m.Summary) != "" {
		text = *m.Summary
	}
	return fmt.Sprintf("- %s [★%d]", text, m.Importance)
}

// composeBody собирает тело брифа: секции по kind в фиксированном порядке
// (DESIGN.md §8), плюс строка про свёрнутый хвост, если он есть. Общая точка
// правды для selectByBudget (оценка стоимости обвязки) и buildBrief (финальный
// вывод) — обе используют уже отформатированные it.line, ничего не форматируют
// заново.
func composeBody(included []briefItem, collapsed int) string {
	var bodyBlocks []string
	for _, sec := range sections {
		var lines []string
		for _, it := range included {
			if it.mem.Kind == sec.kind {
				lines = append(lines, it.line)
			}
		}
		if len(lines) > 0 {
			bodyBlocks = append(bodyBlocks, "### "+sec.title+"\n"+strings.Join(lines, "\n"))
		}
	}
	if collapsed > 0 {
		bodyBlocks = append(bodyBlocks, fmt.Sprintf(
			"_Свёрнуто ещё %d менее релевантных записей. Уточни запрос при необходимости._", collapsed))
	}
	return strings.Join(bodyBlocks, "\n\n")
}

// composeMarkdown собирает полный текст брифа (заголовок + тело) и честную оценку
// числа токенов по ЭТОМУ ЖЕ фактическому тексту, а не по сумме частей порознь —
// именно так этот текст выйдет пользователю. Заголовок самореферентен (сам
// сообщает число токенов всего брифа), поэтому оценка считается в два прохода:
// сперва по черновику с заголовком-плейсхолдером (число токенов в самом заголовке
// не разъезжается больше чем на пару цифр между черновиком и финалом — при эвристике
// runes/4 это меняет итог не более чем на доли токена), затем в заголовок
// подставляется посчитанное число. Используется и для измерения стоимости в
// selectByBudget, и для финальной сборки в buildBrief — единая точка правды.
func composeMarkdown(included []briefItem, collapsed int) (string, int) {
	body := composeBody(included, collapsed)

	draftHeader := fmt.Sprintf("## Релевантная память (%d записей, ~%d токенов)", len(included), 0)
	draft := draftHeader
	if body != "" {
		draft = draftHeader + "\n\n" + body
	}
	approxTokens := estimateTokens(draft)

	header := fmt.Sprintf("## Релевантная память (%d записей, ~%d токенов)", len(included), approxTokens)
	md := header
	if body != "" {
		md = header + "\n\n" + body
	}
	return md, approxTokens
}

// buildBrief собирает итоговый memory.Brief из отобранных записей (DESIGN.md §8).
func buildBrief(included []briefItem, collapsed int) memory.Brief {
	md, approxTokens := composeMarkdown(included, collapsed)
	return memory.Brief{
		Markdown:     md,
		Count:        len(included),
		Collapsed:    collapsed,
		ApproxTokens: approxTokens,
	}
}

// emptyBrief — дружелюбный ответ, когда кандидатов не нашлось вовсе (не путать со
// случаем, когда кандидаты есть, но не влезли в бюджет — там бриф остаётся честным
// про Collapsed).
func emptyBrief() memory.Brief {
	return memory.Brief{
		Markdown: "## Релевантная память не найдена\n\n" +
			"Подходящих записей по этому запросу и scope пока нет. Уточни запрос или сохрани новый факт.",
	}
}
