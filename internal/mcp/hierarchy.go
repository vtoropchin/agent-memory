package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"agentmemory/internal/memory"
	"agentmemory/internal/memory/store"
)

// hierarchySections собирает для ответа memory_get секции иерархии задачи
// (DESIGN.md §14 Этап 15, задача 15.3): «Родитель» — цель первого ребра
// part_of (memory.ParentOf + Store.Get), «Дети» — соседи, объявившие себя
// частью m (memory.ChildrenOf поверх Store.Neighbors, который отдаёт соседей с
// Links). Для записей прочих kind возвращает пустую строку: у них part_of —
// тематическая связь, а не иерархия, и секции только зашумили бы ответ.
// Пустые секции не выводятся.
//
// Обратный обход (дети) намеренно строится из Get+Neighbors, а не отдельным
// «входящим» запросом к хранилищу: множества соседей малы (см. Store.Neighbors).
func (s *Server) hierarchySections(ctx context.Context, m memory.Memory) (string, error) {
	if m.Kind != memory.KindTask {
		return "", nil
	}
	var b strings.Builder

	if parentID, ok := memory.ParentOf(m); ok {
		parent, err := s.store.Get(ctx, parentID)
		switch {
		case err == nil:
			b.WriteString("Родитель:\n")
			b.WriteString(taskLine(parent))
			b.WriteString("\n")
		case errors.Is(err, store.ErrNotFound):
			// Висячее ребро — не ошибка чтения записи, секция просто
			// опускается. Через store такое не возникает (FOREIGN KEY на links
			// в обеих схемах) — ветка защитная, на случай внешней правки БД.
		default:
			return "", fmt.Errorf("родитель %q: %w", parentID, err)
		}
	}

	neighbors, err := s.store.Neighbors(ctx, m.Id)
	if err != nil {
		return "", fmt.Errorf("дети %q: %w", m.Id, err)
	}
	children := memory.ChildrenOf(m.Id, neighbors)
	if len(children) > 0 {
		memory.SortByStage(children)
		done, total := memory.Progress(children)
		fmt.Fprintf(&b, "Дети (%d, готово %d):\n", total, done)
		for _, c := range children {
			b.WriteString(taskLine(c))
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

// taskLine — строка задачи в секциях иерархии: `- [status] (type) Заголовок
// (id)`. Заголовок — Summary (у задачи обязателен), при его отсутствии — id.
func taskLine(m memory.Memory) string {
	title := m.Id
	if m.Summary != nil && strings.TrimSpace(*m.Summary) != "" {
		title = *m.Summary
	}
	return fmt.Sprintf("- [%s] (%s) %s (%s)\n", m.Status, m.Type, title, m.Id)
}
