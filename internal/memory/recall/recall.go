// Package recall реализует read path AgentMemory (DESIGN.md §8): гибридный поиск
// кандидатов (лексический + векторный), слияние через Reciprocal Rank Fusion,
// ранжирование по бизнес-сигналам и сборку Markdown-брифа под бюджет токенов.
package recall

import (
	"context"
	"fmt"
	"log"
	"math"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"agentmemory/internal/config"
	"agentmemory/internal/memory"
	"agentmemory/internal/memory/embed"
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
	debtWeight    float64
	now           func() int64
	reasoner      reason.Reasoner
	embedder      embed.Embedder
}

// New создаёт Recaller поверх заданного Store с весами и дефолтным бюджетом токенов
// из конфига (DESIGN.md §13). reasoner используется опционально (Query.Summarize),
// embedder — опционально для векторного источника кандидатов (DESIGN.md §8, §9); на
// Этапе 1/до подключения векторного слоя обоим передаётся соответствующий Noop.
func New(s store.Store, cfg config.RecallConfig, reasoner reason.Reasoner, embedder embed.Embedder, opts ...Option) *Recaller {
	budget := cfg.DefaultTokenBudget
	if budget <= 0 {
		budget = 800
	}
	debtWeight := cfg.DebtWeight
	// Тот же диапазон (0, 1] и та же защита от NaN, что и в
	// config.RecallConfig.Validate (см. её годок) — New не должен полагаться на
	// то, что вызывающий код всегда прошёл через config.Load.
	if !(debtWeight > 0 && debtWeight <= 1) {
		debtWeight = config.DefaultDebtWeight
	}
	r := &Recaller{
		store:         s,
		weights:       cfg.Weights,
		defaultBudget: budget,
		debtWeight:    debtWeight,
		now:           func() int64 { return time.Now().Unix() },
		reasoner:      reasoner,
		embedder:      embedder,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// section — одна секция брифа: заголовок + kind, который она отображает. Порядок
// среза задаёт ПОРЯДОК и ЗАГОЛОВКИ известных секций в брифе (DESIGN.md §8) — не
// фильтр данных: запись, чей kind сюда не входит, всё равно печатается
// (composeBody), просто отдельным блоком после известных секций.
type section struct {
	kind  memory.Kind
	title string
}

var sections = []section{
	{memory.KindSemantic, "Facts (semantic)"},
	{memory.KindProcedural, "How-to (procedural)"},
	{memory.KindTask, "Tasks (task)"},
	{memory.KindDebt, "Tech debt (debt)"},
	{memory.KindEpisodic, "Recent context (episodic)"},
}

// rrfEntry — запись, слитая из одного или нескольких источников, с накопленным
// RRF-score. sources хранит ВСЕ источники, давшие эту запись (в порядке первого
// появления при слиянии в fuseRRF) — нужно для Explain (задача 8.1), само
// ранжирование по sources не зависит.
type rrfEntry struct {
	mem     memory.Memory
	rrf     float64
	sources []string
}

// scoredCandidate — запись с итоговым final-score после наложения бизнес-сигналов.
// explain дублирует уже вычисленные в rank() слагаемые — собирается всегда (это то
// же самое вычисление, что и final, без пересчёта), а рендерится в бриф только при
// Query.Explain (см. selectByBudget).
type scoredCandidate struct {
	mem     memory.Memory
	final   float64
	explain Explain
}

// Explain — трасса ранжирования одной записи брифа (DESIGN.md §8, §12, задача 8.1):
// собирается в rank() для каждой выжившей записи и рендерится в Markdown-бриф под
// каждой строкой, если задан Query.Explain. Все поля — уже вычисленные в rank()
// величины (RRF-слияние + разложение final на слагаемые после весов
// config.RecallWeights), а не отдельный пересчёт: (Relevance+Importance+Recency+Access)
// умноженное на KindFactor равно Final с точностью до округления при печати
// (formatExplainLine).
type Explain struct {
	Sources    []string // источники кандидата, слитые в эту запись: "lexical", "vector" (memory.Candidate.Source)
	RRF        float64  // сырой (ненормированный) RRF-скор слияния — Σ 1/(k+rank) по источникам
	Relevance  float64  // w_relevance * rrf_norm
	Importance float64  // w_importance * (importance/5)
	Recency    float64  // w_recency * recency_decay
	Access     float64  // w_access * log1p(access_count)
	Sum        float64  // Relevance+Importance+Recency+Access, ДО умножения на KindFactor — считается один раз здесь, formatExplainLine его не пересчитывает
	// KindFactor — понижающий множитель по kind (задача с техдолгом): 1.0 для
	// всех kind, кроме debt, для debt — config.RecallConfig.DebtWeight (см.
	// Recaller.debtWeight). Применяется поверх суммы слагаемых выше, а не
	// внутри них — чтобы трасса честно показывала, ЧТО именно понижает скор.
	KindFactor float64
	Final      float64 // итог (Sum * KindFactor), по которому отсортирован бриф
}

// Candidates выполняет гибридный поиск (FTS + вектор при доступном Embedder, RRF,
// бизнес-ранжирование) и возвращает отранжированных кандидатов без сборки брифа
// (задача 10.1: тот же путь, что использует Recall, нужен web-морде для семантического
// поиска в JSON, а не в Markdown). Source кандидата — слитый ("lexical"/"vector"/
// "lexical+vector"), та же величина, что видна в explain-трассе брифа (задача 8.1).
// Пустой результат (нет кандидатов ни от одного источника) — nil, nil, не ошибка.
func (r *Recaller) Candidates(ctx context.Context, q memory.Query) ([]memory.Candidate, error) {
	scored, err := r.scoredCandidates(ctx, q)
	if err != nil {
		return nil, err
	}
	if len(scored) == 0 {
		return nil, nil
	}
	out := make([]memory.Candidate, len(scored))
	for i, c := range scored {
		out[i] = memory.Candidate{
			Memory: c.mem,
			Score:  c.final,
			Source: strings.Join(c.explain.Sources, "+"),
		}
	}
	return out, nil
}

// scoredCandidates — общая точка правды гибридного поиска (DESIGN.md §8 п.1-3):
// FTS5 + вектор (при доступном Embedder) -> RRF-слияние -> бизнес-ранжирование ->
// сортировка по убыванию final. И Recall (сборка Markdown-брифа), и публичный
// Candidates (задача 10.1, JSON для web-морды) строятся поверх этого метода —
// сама логика гибридного поиска существует в коде ровно один раз.
func (r *Recaller) scoredCandidates(ctx context.Context, q memory.Query) ([]scoredCandidate, error) {
	candQuery := q
	if candQuery.Limit <= 0 {
		candQuery.Limit = defaultCandidateLimit
	}

	lexical, err := r.store.SearchLexical(ctx, candQuery)
	if err != nil {
		return nil, fmt.Errorf("recall: лексический поиск: %w", err)
	}
	vector := r.searchVector(ctx, q.Text, candQuery)

	merged := fuseRRF(lexical, vector)
	if len(merged) == 0 {
		return nil, nil
	}

	scored := r.rank(merged)
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].final != scored[j].final {
			return scored[i].final > scored[j].final
		}
		// Детерминированный тай-брейк: свежее — первым.
		return scored[i].mem.CreatedAt > scored[j].mem.CreatedAt
	})
	return scored, nil
}

// Recall выполняет read path целиком: кандидаты -> RRF-слияние -> ранжирование ->
// сборка брифа под token_budget -> усиление доступа (Touch) для вошедших записей
// (DESIGN.md §8). Кандидаты и их ранжирование — через scoredCandidates (тот же путь,
// что и у публичного Candidates), здесь — только специфика брифа (бюджет, Markdown,
// Touch, суммаризация).
func (r *Recaller) Recall(ctx context.Context, q memory.Query) (memory.Brief, error) {
	budget := q.TokenBudget
	if budget <= 0 {
		budget = r.defaultBudget
	}

	scored, err := r.scoredCandidates(ctx, q)
	if err != nil {
		return memory.Brief{}, err
	}
	if len(scored) == 0 {
		// Нечего суммировать и незачем платить за вызов Reasoner на тексте
		// "ничего не найдено" — ранний выход до withSummary.
		return emptyBrief(), nil
	}

	included, collapsed := selectByBudget(scored, budget, q.Explain)

	for _, c := range included {
		// Touch — best-effort: ошибка усиления доступа не должна ронять recall.
		_ = r.store.Touch(ctx, c.mem.Id)
	}

	brief := buildBrief(included, collapsed, q.Explain)
	if q.Summarize {
		brief = r.withSummary(ctx, brief)
	}
	return brief, nil
}

// searchVector — векторный источник кандидатов (DESIGN.md §8, §9): если Embedder
// включён (Dim() > 0) и текст запроса непуст, считает вектор запроса и ищет по нему
// в Store. Векторный слой опционален — любая ошибка (Embed, пустой результат Embed
// или SearchVector) только логируется в stderr, а Recall продолжает работу с одной
// лексикой (DESIGN.md §15: деградация, а не падение). Noop (Dim()==0) — Embed вообще
// не вызывается.
func (r *Recaller) searchVector(ctx context.Context, text string, candQuery memory.Query) []memory.Candidate {
	if r.embedder.Dim() <= 0 || strings.TrimSpace(text) == "" {
		return nil
	}
	vecs, err := r.embedder.Embed(ctx, []string{text})
	if err != nil {
		log.Printf("recall: эмбеддинг запроса: %v (продолжаю только с лексическим поиском)", err)
		return nil
	}
	if len(vecs) < 1 {
		log.Printf("recall: эмбеддинг запроса не вернул вектор (продолжаю только с лексическим поиском)")
		return nil
	}
	vector, err := r.store.SearchVector(ctx, vecs[0], candQuery)
	if err != nil {
		log.Printf("recall: векторный поиск: %v (продолжаю только с лексическим поиском)", err)
		return nil
	}
	return vector
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
			// Explain (задача 8.1): запись сохраняет ВСЕ источники, а не только
			// первый встреченный — порядок внешнего цикла (лексика, затем вектор,
			// см. вызов в Recall) уже даёт детерминированный порядок в срезе.
			if !slices.Contains(e.sources, c.Source) {
				e.sources = append(e.sources, c.Source)
			}
		}
	}

	result := make([]rrfEntry, 0, len(order))
	for _, id := range order {
		result = append(result, *byID[id])
	}
	return result
}

// rank накладывает бизнес-сигналы поверх RRF (DESIGN.md §8 п.3):
// final = (w_rel·rrf_norm + w_imp·(importance/5) + w_rec·recency_decay + w_acc·log(1+access_count)) * kind_factor.
// rrf_norm нормирует rrf в [0,1] по максимуму в текущей выдаче. kind_factor — 1.0
// для всех kind, кроме debt (техдолг): для него применяется r.debtWeight, понижая
// итоговый скор так, чтобы накопленные долги не вытесняли факты из бюджета
// токенов, оставаясь при этом в дефолтной выдаче recall (DESIGN.md §4).
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

		relevanceTerm := r.weights.Relevance * rrfNorm
		importanceTerm := r.weights.Importance * importanceNorm
		recencyTerm := r.weights.Recency * recency
		accessTerm := r.weights.Access * accessBoost
		sum := relevanceTerm + importanceTerm + recencyTerm + accessTerm

		kindFactor := 1.0
		if e.mem.Kind == memory.KindDebt {
			kindFactor = r.debtWeight
		}
		final := sum * kindFactor

		result = append(result, scoredCandidate{
			mem:   e.mem,
			final: final,
			explain: Explain{
				Sources:    e.sources,
				RRF:        e.rrf,
				Relevance:  relevanceTerm,
				Importance: importanceTerm,
				Recency:    recencyTerm,
				Access:     accessTerm,
				Sum:        sum,
				KindFactor: kindFactor,
				Final:      final,
			},
		})
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
// token_budget на размер этой обвязки — DESIGN.md §15: честность выдачи). explain
// добавляет к каждой строке довесок-трассу ранжирования (задача 8.1) ДО оценки
// стоимости — иначе бюджет считался бы нечестно относительно фактического размера
// выдачи. Алгоритм:
//  1. Жадно набираем строки без учёта обвязки — верхняя граница набора, тот же
//     порядок отбора, что и раньше (останавливаемся на первой не влезшей строке,
//     без случайных пропусков середины списка).
//  2. Гарантируем минимум одну запись, если кандидаты вообще есть: крошечный
//     бюджет не должен превращать непустую выдачу в «0 записей» — честнее отдать
//     самую релевантную запись с превышением бюджета, чем ничего.
//  3. Пока фактическая стоимость собранного брифа (composeMarkdown, т.е. реальный
//     Markdown с обвязкой) не укладывается в бюджет — обрезаем с хвоста наименее
//     релевантные записи, но не ниже одной.
func selectByBudget(scored []scoredCandidate, budget int, explain bool) ([]briefItem, int) {
	if len(scored) == 0 {
		return nil, 0
	}

	items := make([]briefItem, len(scored))
	for i, c := range scored {
		line := formatLine(c.mem)
		if explain {
			line += "\n" + formatExplainLine(c.explain)
		}
		items[i] = briefItem{mem: c.mem, line: line}
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
		if _, cost := composeMarkdown(included, collapsed, explain); cost <= budget {
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
// иначе Content, плюс маркер важности. У задач (memory.KindTask) строка
// дополнительно начинается с маркера состояния — `- [doing] Заголовок [★4]`:
// список задач без статусов бесполезен, а сам статус в бриф иначе не попадает
// (в строку выводится только текст записи). Для задачи Summary — её заголовок и
// он обязателен (write.ErrTaskSummaryRequired), поэтому многокилобайтный content
// задачи в бриф не просачивается; полный текст агент забирает по id (memory_get).
func formatLine(m memory.Memory) string {
	text := m.Content
	if m.Summary != nil && strings.TrimSpace(*m.Summary) != "" {
		text = *m.Summary
	}
	if m.Kind == memory.KindTask && m.Status != "" {
		return fmt.Sprintf("- [%s] %s [★%d]", m.Status, text, m.Importance)
	}
	return fmt.Sprintf("- %s [★%d]", text, m.Importance)
}

// formatExplainLine форматирует explain-довесок под строкой записи брифа
// (DESIGN.md §8, §12, задача 8.1): источники кандидата и разложение итогового
// скора на уже взвешенные слагаемые — без пересчёта, те же величины, что явно
// посчитаны в rank() и сохранены в scoredCandidate.explain (включая саму сумму,
// Explain.Sum). Хвост "× kind FACTOR = FINAL" печатается, только если KindFactor
// отличается от 1.0 (для всех kind, кроме debt, он равен 1.0, и Final совпадает
// с Sum — печатать тождество было бы бесполезной тавтологией, вдобавок платной:
// строка удлиняется ДО оценки бюджета в selectByBudget); для debt хвост явно
// показывает, что именно понижает скор.
func formatExplainLine(e Explain) string {
	via := strings.Join(e.Sources, "+")
	line := fmt.Sprintf("  · via %s · rrf %.3f · rel %.2f + imp %.2f + rec %.2f + acc %.2f = %.2f",
		via, e.RRF, e.Relevance, e.Importance, e.Recency, e.Access, e.Sum)
	if e.KindFactor != 1 {
		line += fmt.Sprintf(" × kind %.2f = %.2f", e.KindFactor, e.Final)
	}
	return line
}

// composeBody собирает тело брифа: секции по kind в фиксированном порядке
// (DESIGN.md §8), плюс строка про свёрнутый хвост, если он есть. Общая точка
// правды для selectByBudget (оценка стоимости обвязки) и buildBrief (финальный
// вывод) — обе используют уже отформатированные it.line, ничего не форматируют
// заново.
func composeBody(included []briefItem, collapsed int) string {
	var bodyBlocks []string
	known := make(map[memory.Kind]bool, len(sections))
	for _, sec := range sections {
		known[sec.kind] = true
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
	// Остаток: kind, не входящий ни в одну известную секцию выше — новый kind,
	// которого этот бинарь не знает (напр. более новый клиент на общей БД,
	// DESIGN.md §2.1, либо запись, положенная в обход write.Save). Запись уже
	// прошла selectByBudget и учтена в Count — молча её не показать значило бы
	// разойтись с заголовком брифа (DESIGN.md §15: никаких тихих обрезаний).
	// Порядок внутри блока — тот же, что и у известных секций: included уже
	// отсортирован по убыванию final.
	var otherLines []string
	for _, it := range included {
		if !known[it.mem.Kind] {
			otherLines = append(otherLines, it.line)
		}
	}
	if len(otherLines) > 0 {
		bodyBlocks = append(bodyBlocks, "### Other\n"+strings.Join(otherLines, "\n"))
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
// explain=true добавляет заголовку пометку "(explain)" (задача 8.1) — сами строки
// записей к этому моменту уже несут explain-довесок (см. selectByBudget), здесь
// только маркируется заголовок; explain=false даёт байт-в-байт прежний заголовок.
func composeMarkdown(included []briefItem, collapsed int, explain bool) (string, int) {
	body := composeBody(included, collapsed)
	suffix := ""
	if explain {
		suffix = " (explain)"
	}

	draftHeader := fmt.Sprintf("## Релевантная память (%d записей, ~%d токенов)%s", len(included), 0, suffix)
	draft := draftHeader
	if body != "" {
		draft = draftHeader + "\n\n" + body
	}
	approxTokens := estimateTokens(draft)

	header := fmt.Sprintf("## Релевантная память (%d записей, ~%d токенов)%s", len(included), approxTokens, suffix)
	md := header
	if body != "" {
		md = header + "\n\n" + body
	}
	return md, approxTokens
}

// buildBrief собирает итоговый memory.Brief из отобранных записей (DESIGN.md §8).
func buildBrief(included []briefItem, collapsed int, explain bool) memory.Brief {
	md, approxTokens := composeMarkdown(included, collapsed, explain)
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
