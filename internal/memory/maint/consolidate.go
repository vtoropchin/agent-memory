// Package maint реализует фоновые джобы обслуживания памяти (DESIGN.md §11):
// decay, consolidation, dedup-sweep, reindex. Сам планировщик (тикеры/расписание) —
// предмет Этапа 4; здесь — только вызываемые доменные операции.
package maint

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"agentmemory/internal/memory"
	"agentmemory/internal/memory/reason"
	"agentmemory/internal/memory/store"
	"agentmemory/internal/memory/write"
)

// defaultBatchLimit — сколько episodic-записей рассматриваем за один вызов
// Consolidate, если вызывающий код не задал свой лимит (batchLimit <= 0).
const defaultBatchLimit = 50

// consolidationSource — значение SaveInput.Source для фактов, рождённых консолидацией.
const consolidationSource = "consolidation"

// ConsolidateResult — статистика одного прогона Consolidate.
type ConsolidateResult struct {
	Scanned    int // сколько episodic-записей реально взято в обработку (см. годок Consolidate про batchLimit)
	Created    int // сколько НОВЫХ semantic-фактов вставлено (Writer.Save вернул merged=false)
	Merged     int // сколько консолидированных фактов слито с уже существующим semantic-дублем (Writer.Save вернул merged=true) вместо вставки новой записи
	Superseded int // сколько уникальных исходных episodic помечено superseded_by (повторный source_id в пределах группы считается один раз)
	Skipped    int // сколько scope-групп пропущено целиком или частично (ошибка/невалидный ответ Reasoner, либо ошибка Store при сохранении/supersede внутри группы — см. годок Consolidate)
}

// Consolidator сводит накопившиеся episodic-записи в устойчивые semantic-факты
// через Reasoner (DESIGN.md §11): кластер похожих/связанных эпизодов одного scope
// обобщается в факт, исходные эпизоды помечаются superseded_by.
type Consolidator struct {
	store    store.Store
	reasoner reason.Reasoner
	writer   *write.Writer
}

// NewConsolidator создаёт Consolidator поверх Store, Reasoner и Writer (запись новых
// фактов идёт через обычный write path — дедуп и генерация ULID достаются бесплатно).
func NewConsolidator(s store.Store, r reason.Reasoner, w *write.Writer) *Consolidator {
	return &Consolidator{store: s, reasoner: r, writer: w}
}

// consolidatedFact — один элемент JSON-ответа Reasoner на запрос консолидации.
type consolidatedFact struct {
	Content   string   `json:"content"`
	Summary   string   `json:"summary"`
	Tags      []string `json:"tags"`
	SourceIDs []string `json:"source_ids"`
}

// Consolidate группирует episodic-записи старше olderThan по scope и для каждой
// группы просит Reasoner обобщить повторяющиеся/связанные эпизоды в устойчивые
// semantic-факты. Каждый полученный факт сохраняется через Writer.Save, а
// перечисленные в нём source_ids (только те, что реально входили в группу, без
// повторов) помечаются superseded_by.
//
// batchLimit — мягкая граница на суммарное число обрабатываемых за вызов episodic-
// записей (<=0 → defaultBatchLimit). Группы (по scope) берутся в работу ЦЕЛИКОМ и
// строго по порядку, полученному от Store: как только накопленное число эпизодов из
// уже взятых групп достигает batchLimit, новые группы больше не берутся — но
// последняя взятая группа обрабатывается полностью, даже если из-за этого суммарное
// число обработанных эпизодов превысит batchLimit. Граница никогда не разрывает
// scope-группу внутри себя. Result.Scanned — сумма размеров реально обработанных
// групп, а не общее число, найденное Store.
//
// Деградация (DESIGN.md §15): любая ошибка на уровне группы — ошибка/пустой ответ
// (в т.ч. Noop) Reasoner.Complete, невалидный JSON, либо ошибка Store при
// Writer.Save/Store.Supersede внутри группы — приводит к пропуску/частичному
// пропуску только этой группы (Skipped++, с логом в stderr) и не прерывает
// обработку остальных групп. Единственная ошибка, которую Consolidate возвращает
// вызывающему коду, — ошибка самой выборки ListEpisodicForConsolidation.
func (c *Consolidator) Consolidate(ctx context.Context, olderThan time.Duration, batchLimit int) (ConsolidateResult, error) {
	if batchLimit <= 0 {
		batchLimit = defaultBatchLimit
	}
	threshold := time.Now().Add(-olderThan).Unix()

	// limit=0 — выборка без ограничения: резать её на уровне SQL нельзя, не разрушив
	// scope-группы (см. godoc выше); ограничение применяется ниже, по целым группам.
	episodes, err := c.store.ListEpisodicForConsolidation(ctx, threshold, 0)
	if err != nil {
		return ConsolidateResult{}, fmt.Errorf("maint: выборка episodic для консолидации: %w", err)
	}
	if len(episodes) == 0 {
		return ConsolidateResult{}, nil
	}

	var result ConsolidateResult
	for _, group := range groupByScope(episodes) {
		if result.Scanned >= batchLimit {
			break
		}
		result.Scanned += len(group.members)
		c.consolidateGroup(ctx, group, &result)
	}
	return result, nil
}

// scopeGroup — episodic-записи одного scope, в порядке, полученном от Store
// (ListEpisodicForConsolidation сортирует по scope, затем created_at).
type scopeGroup struct {
	scope   string
	members []memory.Memory
}

// groupByScope разбивает episodes на группы по Scope линейным сканом: episodes уже
// отсортированы Store'ом по scope (затем created_at), поэтому все записи одного
// scope идут подряд — достаточно сравнить с предыдущим scope, без map. Если порядок
// выборки когда-нибудь перестанет гарантировать сортировку по scope, эта функция
// перестанет корректно группировать.
func groupByScope(episodes []memory.Memory) []scopeGroup {
	var groups []scopeGroup
	for _, m := range episodes {
		if len(groups) == 0 || groups[len(groups)-1].scope != m.Scope {
			groups = append(groups, scopeGroup{scope: m.Scope})
		}
		last := &groups[len(groups)-1]
		last.members = append(last.members, m)
	}
	return groups
}

// consolidateGroup обрабатывает одну scope-группу: запрашивает у Reasoner обобщение,
// парсит ответ и сохраняет валидные факты. Ошибка Reasoner/парсинга — пропуск всей
// группы (result.Skipped++). Ошибка Store (Writer.Save или Store.Supersede) на
// отдельном факте/source_id логируется в stderr, группа тоже считается Skipped
// (частично применённая), но обработка остальных фактов группы продолжается —
// один сбойный факт не должен откатывать уже сохранённые соседние.
func (c *Consolidator) consolidateGroup(ctx context.Context, group scopeGroup, result *ConsolidateResult) {
	resp, err := c.reasoner.Complete(ctx, buildConsolidationPrompt(group.members))
	if err != nil {
		log.Printf("maint: consolidate reasoner (scope %q): %v", group.scope, err)
		result.Skipped++
		return
	}
	resp = strings.TrimSpace(resp)
	if resp == "" { // в т.ч. reason.Noop
		result.Skipped++
		return
	}

	facts, err := parseConsolidatedFacts(resp)
	if err != nil {
		log.Printf("maint: consolidate разбор ответа Reasoner (scope %q): %v", group.scope, err)
		result.Skipped++
		return
	}

	allowedSources := make(map[string]bool, len(group.members))
	for _, m := range group.members {
		allowedSources[m.Id] = true
	}
	// superseded — источники, уже помеченные superseded_by в рамках ЭТОЙ группы:
	// один и тот же source_id может повториться внутри одного факта или встретиться
	// в нескольких фактах группы — Supersede на него должен уйти ровно один раз.
	superseded := make(map[string]bool, len(group.members))

	groupHadError := false
	for _, fact := range facts {
		content := strings.TrimSpace(fact.Content)
		if content == "" {
			continue
		}

		id, merged, err := c.writer.Save(ctx, write.SaveInput{
			Kind:       memory.KindSemantic,
			Scope:      group.scope,
			Content:    content,
			Summary:    fact.Summary,
			Importance: 0, // 0 → дефолт 3 через write.clampImportance
			Tags:       fact.Tags,
			Source:     consolidationSource,
		})
		if err != nil {
			log.Printf("maint: сохранение консолидированного факта (scope %q): %v", group.scope, err)
			groupHadError = true
			continue
		}
		if merged {
			result.Merged++
		} else {
			result.Created++
		}

		// Supersede выполняется по одному per source_id, каждый — autocommit-транзакция
		// Store: Store не выставляет BeginTx/батч-supersede в интерфейсе (DESIGN.md §6),
		// а расширять интерфейс ради этого джоба — больше площади, чем стоит задача.
		// Цикл уже сгруппирован после успешного Save: Supersede для факта начинается
		// только когда id этого факта подтверждён записанным.
		for _, sourceID := range fact.SourceIDs {
			sourceID = strings.TrimSpace(sourceID)
			if sourceID == "" || !allowedSources[sourceID] || superseded[sourceID] {
				continue // чужой/пустой/уже обработанный id — молча пропускаем (см. godoc Consolidate)
			}
			if err := c.store.Supersede(ctx, sourceID, id); err != nil {
				log.Printf("maint: supersede %q -> %q: %v", sourceID, id, err)
				groupHadError = true
				continue
			}
			superseded[sourceID] = true
			result.Superseded++
		}
	}
	if groupHadError {
		result.Skipped++
	}
}

// buildConsolidationPrompt формирует запрос к Reasoner: пронумерованный список
// эпизодов (id + content) группы одного scope и инструкцию вернуть строгий JSON.
func buildConsolidationPrompt(members []memory.Memory) string {
	var sb strings.Builder
	sb.WriteString("Ты помогаешь консолидировать долговременную память ИИ-агента. " +
		"Ниже — список эпизодических записей (episodic) одного scope. Обобщи повторяющиеся " +
		"или тематически связанные эпизоды в устойчивые долгосрочные факты (semantic). " +
		"Эпизоды, которые не образуют обобщения (единичные, не связанные с другими), " +
		"не включай ни в один факт.\n\n")
	sb.WriteString("Верни СТРОГО JSON-массив, без markdown-обёртки (без ```), в формате:\n")
	sb.WriteString(`[{"content": "...", "summary": "...", "tags": ["..."], "source_ids": ["..."]}]`)
	sb.WriteString("\n\n")
	sb.WriteString("Правила:\n" +
		"- content — атомарный факт (1-3 предложения), на том же языке, что исходные записи.\n" +
		"- summary — ультра-короткая форма факта (несколько слов).\n" +
		"- tags — список ключевых слов факта (может быть пустым).\n" +
		"- source_ids — id эпизодов из списка ниже, легших в основу факта.\n" +
		"- Если ни один эпизод не образует устойчивого обобщения, верни пустой массив [].\n\n")
	sb.WriteString("Записи:\n")
	for i, m := range members {
		fmt.Fprintf(&sb, "%d. [%s] %s\n", i+1, m.Id, m.Content)
	}
	return sb.String()
}

// parseConsolidatedFacts разбирает ответ Reasoner в список фактов, срезая
// возможные ```-ограждения markdown вокруг JSON-массива.
func parseConsolidatedFacts(resp string) ([]consolidatedFact, error) {
	resp = stripCodeFence(resp)
	var facts []consolidatedFact
	if err := json.Unmarshal([]byte(resp), &facts); err != nil {
		return nil, fmt.Errorf("maint: разбор ответа Reasoner: %w", err)
	}
	return facts, nil
}

// stripCodeFence срезает обрамляющий markdown code fence (```` ```json ... ``` ````
// или просто ```` ``` ... ``` ````), если он есть; иначе возвращает текст как есть.
func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```")
	if nl := strings.IndexByte(s, '\n'); nl != -1 {
		firstLine := strings.TrimSpace(s[:nl])
		// срезаем маркер языка (например "json"), если он есть на первой строке.
		if firstLine == "" || !strings.ContainsAny(firstLine, " \t{[") {
			s = s[nl+1:]
		}
	}
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	return strings.TrimSpace(s)
}
