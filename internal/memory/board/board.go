// Package board — вариант использования «kanban-доска задач» (DESIGN.md §14
// Этап 15, задача 15.5): раскладывает живые задачи по колонкам-стадиям и
// вычисляет для каждой карточки блокировку, ближайшего родителя и прогресс
// детей. Не знает ни про HTTP, ни про JSON, ни про драйверы БД: единственная
// зависимость ядра — agentmemory/internal/memory; хранилище приходит через
// узкий порт Source, который store.Store удовлетворяет структурно. Потому
// пакет тестируется на in-memory Source без SQLite, а тот же Build доступен
// отложенным потребителям (memo task в CLI, MCP «покажи доску») без правок
// web-морды.
package board

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"agentmemory/internal/memory"
)

// Source — порт доски: ровно то, что нужно Build. Объявлен здесь, у
// потребителя (идиома Go), а не в пакете store — иначе board унаследовал бы
// базовую линию нарушений правила зависимостей вместе с драйверами БД.
type Source interface {
	// ListTasks — все живые задачи scopes (пусто — все scope) с Links.
	ListTasks(ctx context.Context, scopes []string) ([]memory.Memory, error)
	// GetMany — записи по id (в т.ч. тумстоуны, отсутствующие пропущены) —
	// догрузка блокеров вне scopes доски.
	GetMany(ctx context.Context, ids []string) ([]memory.Memory, error)
}

// defaultDoneLimit — сколько самых свежих карточек колонки done отдаётся по
// умолчанию (Filter.DoneLimit <= 0); остальные колонки — целиком.
const defaultDoneLimit = 30

// Filter — параметры доски. Все фильтры, кроме Scopes, применяются ПОСЛЕ
// вычисления прогресса родителей: прогресс эпика не зависит от того, что
// сейчас показано.
type Filter struct {
	Scopes    []string          // scope задач; пусто — все
	Types     []memory.TaskType // только эти типы; пусто — все
	ParentID  string            // только прямые дети этой записи (memory.ParentOf); пусто — без фильтра
	Branch    string            // точное совпадение Memory.Branch; пусто — без фильтра
	Workflow  string            // точное совпадение Memory.Workflow; пусто — без фильтра
	Tag       string            // задача обязана иметь этот тег; пусто — без фильтра
	DoneLimit int               // усечение колонки done; <= 0 → defaultDoneLimit
}

// Ref — краткая ссылка на задачу в карточке (блокер, родитель, ребёнок):
// ровно то, что нужно строке списка и переходу на карточку. Summary пуст,
// если запись недоступна (родитель вне scopes доски — известен только id).
type Ref struct {
	ID      string
	Summary string
	Status  memory.Status
	Type    memory.TaskType
}

// Progress — прогресс родителя по живым детям (memory.Progress).
type Progress struct {
	Done, Total int
}

// Card — карточка доски: задача плюс вычисленные свойства.
type Card struct {
	Memory    memory.Memory
	Blocked   bool      // есть хотя бы один живой блокер (memory.LiveBlockers)
	BlockedBy []Ref     // живые блокеры, в порядке Links
	Parent    *Ref      // ближайший родитель по part_of (memory.ParentOf); nil — корневая
	Progress  *Progress // только у родителей с детьми (epic/story с part_of на них); иначе nil
}

// Column — колонка-стадия. Total — число карточек до усечения: отличается от
// len(Cards) только у done (Filter.DoneLimit).
type Column struct {
	Status memory.Status
	Cards  []Card
	Total  int
}

// Board — доска: колонки в порядке memory.Statuses, всегда все семь (пустые
// тоже — фронту не нужно догадываться, какой стадии «не было»).
type Board struct {
	Columns []Column
}

// Build собирает доску: ListTasks(scopes) → индекс по id → дети и прогресс
// каждого родителя (по ВСЕМ живым задачам scopes, до фильтров) → блокеры
// (memory.BlockerIDs; отсутствующие в индексе — кросс-scope — догружаются
// GetMany) → memory.LiveBlockers → Filter → колонки по memory.Statuses →
// сортировка (memory.SortByStage: внутри колонки importance ↓, updated_at ↓,
// id) → усечение done до DoneLimit самых свежих по updated_at. Задача с
// неизвестной стадией (данные чужого, более нового бинаря) в колонки не
// попадает.
func Build(ctx context.Context, src Source, f Filter) (Board, error) {
	tasks, err := src.ListTasks(ctx, f.Scopes)
	if err != nil {
		return Board{}, fmt.Errorf("board: задачи: %w", err)
	}

	byID := make(map[string]memory.Memory, len(tasks))
	for _, t := range tasks {
		byID[t.Id] = t
	}
	children := make(map[string][]memory.Memory)
	for _, t := range tasks {
		if parent, ok := memory.ParentOf(t); ok {
			children[parent] = append(children[parent], t)
		}
	}

	blockersByID, err := loadBlockers(ctx, src, tasks, byID)
	if err != nil {
		return Board{}, err
	}

	limit := f.DoneLimit
	if limit <= 0 {
		limit = defaultDoneLimit
	}
	columns := make([]Column, len(memory.Statuses))
	index := make(map[memory.Status]int, len(memory.Statuses))
	for i, st := range memory.Statuses {
		columns[i].Status = st
		columns[i].Cards = []Card{}
		index[st] = i
	}
	for _, t := range tasks {
		if !f.matches(t) {
			continue
		}
		i, ok := index[t.Status]
		if !ok {
			continue
		}
		columns[i].Cards = append(columns[i].Cards, card(t, byID, blockersByID, children[t.Id]))
	}
	for i := range columns {
		col := &columns[i]
		col.Total = len(col.Cards)
		if col.Status == memory.StatusDone && len(col.Cards) > limit {
			sort.SliceStable(col.Cards, func(a, b int) bool {
				return col.Cards[a].Memory.UpdatedAt > col.Cards[b].Memory.UpdatedAt
			})
			col.Cards = col.Cards[:limit]
		}
		sortCards(col.Cards)
	}
	return Board{Columns: columns}, nil
}

// matches применяет фильтры Filter (кроме Scopes — они ушли в ListTasks).
func (f Filter) matches(t memory.Memory) bool {
	if len(f.Types) > 0 && !slices.Contains(f.Types, t.Type) {
		return false
	}
	if f.ParentID != "" {
		if parent, ok := memory.ParentOf(t); !ok || parent != f.ParentID {
			return false
		}
	}
	if f.Branch != "" && t.Branch != f.Branch {
		return false
	}
	if f.Workflow != "" && t.Workflow != f.Workflow {
		return false
	}
	if f.Tag != "" && !slices.Contains(t.Tags, f.Tag) {
		return false
	}
	return true
}

// loadBlockers собирает карту всех блокеров задач: из индекса живых задач
// scopes плюс один GetMany по id, которых в индексе нет (блокер из другого
// scope, done-задача другого scope или тумстоун — LiveBlockers отфильтрует).
func loadBlockers(ctx context.Context, src Source, tasks []memory.Memory, byID map[string]memory.Memory) (map[string]memory.Memory, error) {
	blockers := make(map[string]memory.Memory)
	var missing []string
	seen := map[string]bool{}
	for _, t := range tasks {
		for _, id := range memory.BlockerIDs(t) {
			if seen[id] {
				continue
			}
			seen[id] = true
			if b, ok := byID[id]; ok {
				blockers[id] = b
			} else {
				missing = append(missing, id)
			}
		}
	}
	if len(missing) > 0 {
		loaded, err := src.GetMany(ctx, missing)
		if err != nil {
			return nil, fmt.Errorf("board: блокеры: %w", err)
		}
		for _, b := range loaded {
			blockers[b.Id] = b
		}
	}
	return blockers, nil
}

// card собирает карточку задачи t: живые блокеры, родитель (из индекса; вне
// scopes — только id) и прогресс по детям.
func card(t memory.Memory, byID, blockersByID map[string]memory.Memory, kids []memory.Memory) Card {
	c := Card{Memory: t}
	for _, b := range memory.LiveBlockers(t, blockersByID) {
		c.BlockedBy = append(c.BlockedBy, refOf(b))
	}
	c.Blocked = len(c.BlockedBy) > 0
	if parentID, ok := memory.ParentOf(t); ok {
		ref := Ref{ID: parentID}
		if p, ok := byID[parentID]; ok {
			ref = refOf(p)
		}
		c.Parent = &ref
	}
	if len(kids) > 0 {
		done, total := memory.Progress(kids)
		c.Progress = &Progress{Done: done, Total: total}
	}
	return c
}

// sortCards упорядочивает карточки колонки правилом memory.LessByStage (стадия
// у всех одна, поэтому фактически importance ↓, updated_at ↓, id).
func sortCards(cards []Card) {
	sort.SliceStable(cards, func(i, j int) bool { return memory.LessByStage(cards[i].Memory, cards[j].Memory) })
}

// refOf — Ref по записи; Summary — заголовок задачи (пусто, если не задан).
func refOf(m memory.Memory) Ref {
	r := Ref{ID: m.Id, Status: m.Status, Type: m.Type}
	if m.Summary != nil {
		r.Summary = *m.Summary
	}
	return r
}
