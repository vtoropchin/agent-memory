package mcp

import (
	"context"
	"fmt"
	"strings"

	"agentmemory/internal/memory"
	"agentmemory/internal/memory/board"
)

// taskSections рендерит для ответа memory_get секции задачи (DESIGN.md §14
// Этап 15): «Родитель» и «Дети (N, готово M)» — иерархия (задача 15.3),
// «Заблокирована (ждёт)» и «Блокирует» — блокировка (задача 15.4). Сама
// политика (что считать родителем/ребёнком/блокером, порядок, тумстоуны) —
// board.Describe, общая с карточкой web-морды (задача 15.5 закрыла дубль);
// здесь только Markdown. Для записей прочих kind Describe отдаёт пустой Detail
// — секций нет. Пустые секции не выводятся.
func (s *Server) taskSections(ctx context.Context, m memory.Memory) (string, error) {
	d, err := board.Describe(ctx, s.store, m)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if d.Parent != nil {
		b.WriteString("Родитель:\n")
		b.WriteString(taskLine(*d.Parent))
		b.WriteString("\n")
	}
	if len(d.Children) > 0 {
		fmt.Fprintf(&b, "Дети (%d, готово %d):\n", d.Progress.Total, d.Progress.Done)
		writeRefs(&b, d.Children)
	}
	if len(d.BlockedBy) > 0 {
		b.WriteString("Заблокирована (ждёт):\n")
		writeRefs(&b, d.BlockedBy)
	}
	if len(d.Blocks) > 0 {
		b.WriteString("Блокирует:\n")
		writeRefs(&b, d.Blocks)
	}
	return b.String(), nil
}

func writeRefs(b *strings.Builder, refs []board.Ref) {
	for _, r := range refs {
		b.WriteString(taskLine(r))
	}
	b.WriteString("\n")
}

// taskLine — строка задачи в секциях: `- [status] (type) Заголовок (id)`.
// Заголовок — Summary (у задачи обязателен), при его отсутствии — id.
func taskLine(r board.Ref) string {
	title := r.Summary
	if strings.TrimSpace(title) == "" {
		title = r.ID
	}
	return fmt.Sprintf("- [%s] (%s) %s (%s)\n", r.Status, r.Type, title, r.ID)
}
