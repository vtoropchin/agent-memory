package board

import (
	"context"
	"fmt"

	"agentmemory/internal/memory"
)

// DetailSource — порт карточки одной задачи (Describe): соседи по графу и
// догрузка по id. store.Store удовлетворяет структурно; тест — in-memory.
type DetailSource interface {
	// Neighbors — живые записи, связанные с id в любом направлении, с Links.
	Neighbors(ctx context.Context, id string) ([]memory.Memory, error)
	GetMany(ctx context.Context, ids []string) ([]memory.Memory, error)
}

// Detail — контекст задачи для её карточки (memory_get, GET /api/memories/
// {id}): родитель, дети с прогрессом, живые блокеры и «что блокирует».
// Единственное место политики карточки — раньше её дублировали два адаптера
// (mcp и web), см. DESIGN.md §14 Этап 15.
type Detail struct {
	Parent    *Ref      // ближайший ЖИВОЙ родитель по part_of; тумстоун или отсутствующая запись — nil
	Children  []Ref     // живые задачи с part_of на эту, в порядке доски (memory.SortByStage)
	Progress  *Progress // по Children; nil у записей прочих kind (у задачи без детей — {0, 0})
	BlockedBy []Ref     // живые блокеры (memory.LiveBlockers), в порядке Links
	Blocks    []Ref     // живые задачи с blocked_by на эту (memory.BlockedBy), в порядке доски
}

// Describe собирает Detail для m: один Neighbors (дети и блокируемые — обратный
// обход по Links соседей, memory.ChildrenOf/BlockedBy) и один GetMany (родитель
// и блокеры — по id, без фильтра scope). Для записей прочих kind возвращает
// пустой Detail без обращения к Source: у них part_of — тематическая связь,
// а не иерархия, и блокировка не определена. Родитель-тумстоун опускается —
// это отличие от прежнего Store.Get в адаптерах, который тумстоуны не
// фильтровал (закрытый техдолг 15.3).
func Describe(ctx context.Context, src DetailSource, m memory.Memory) (Detail, error) {
	var d Detail
	if m.Kind != memory.KindTask {
		return d, nil
	}

	neighbors, err := src.Neighbors(ctx, m.Id)
	if err != nil {
		return Detail{}, fmt.Errorf("board: соседи %q: %w", m.Id, err)
	}
	kids := memory.ChildrenOf(m.Id, neighbors)
	memory.SortByStage(kids)
	d.Children = refsOf(kids)
	done, total := memory.Progress(kids)
	d.Progress = &Progress{Done: done, Total: total}
	blocks := memory.BlockedBy(m.Id, neighbors)
	memory.SortByStage(blocks)
	d.Blocks = refsOf(blocks)

	ids := memory.BlockerIDs(m)
	parentID, hasParent := memory.ParentOf(m)
	if hasParent {
		ids = append(ids, parentID)
	}
	if len(ids) == 0 {
		return d, nil
	}
	loaded, err := src.GetMany(ctx, ids)
	if err != nil {
		return Detail{}, fmt.Errorf("board: родитель/блокеры %q: %w", m.Id, err)
	}
	byID := make(map[string]memory.Memory, len(loaded))
	for _, r := range loaded {
		byID[r.Id] = r
	}
	if p, ok := byID[parentID]; hasParent && ok && p.SupersededBy == nil {
		ref := refOf(p)
		d.Parent = &ref
	}
	d.BlockedBy = refsOf(memory.LiveBlockers(m, byID))
	return d, nil
}

func refsOf(ms []memory.Memory) []Ref {
	refs := make([]Ref, len(ms))
	for i, m := range ms {
		refs[i] = refOf(m)
	}
	return refs
}
