package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"agentmemory/internal/memory"
	"agentmemory/internal/memory/store"
)

// handleGet реализует memory_get: возвращает ОДНУ запись целиком — полный
// content плюс метаданные (DESIGN.md §12, задача 14.1).
//
// Зачем отдельный инструмент рядом с memory_recall: бриф recall намеренно
// компактен — он выводит краткую форму записи (Summary, если задана) и режет
// выдачу под token_budget. Для факта в 1–3 предложения этого достаточно, а
// задача (kind=task) — документ, чей заголовок в бриф попадает, а текст нет.
// Без memory_get задачу можно было бы найти, но не прочитать. Инструмент не
// специфичен для задач: он одинаково отдаёт любую запись по её id.
//
// В отличие от recall, здесь НЕТ ни фильтра scope, ни скрытия выполненных задач:
// id — точный адрес, агент уже знает, что именно запрашивает. Store.Touch не
// вызывается: чтение по прямому id — не сигнал релевантности поиска, а работа с
// уже найденной записью (иначе access_count задачи рос бы от каждого её
// перечитывания и искажал ранжирование остальных).
func (s *Server) handleGet(ctx context.Context, _ *sdkmcp.CallToolRequest, in getInput) (*sdkmcp.CallToolResult, any, error) {
	id := strings.TrimSpace(in.ID)
	if id == "" {
		return nil, nil, fmt.Errorf("memory_get: id обязателен")
	}

	m, err := s.store.Get(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, fmt.Errorf("memory_get: запись %q не найдена", id)
		}
		return nil, nil, fmt.Errorf("memory_get: %w", err)
	}
	hierarchy, err := s.hierarchySections(ctx, m)
	if err != nil {
		return nil, nil, fmt.Errorf("memory_get: %w", err)
	}
	return textResult(formatMemory(m, hierarchy)), nil, nil
}

// formatMemory рендерит запись в Markdown для ответа memory_get: сперва шапка с
// метаданными (по одной строке на поле, пустые поля опускаются — шум в ответе
// инструмента дороже полноты), затем полный текст записи. Тот же принцип, что у
// recall-брифа: агенту отдаётся читаемый Markdown, а не JSON-простыня.
// sections — уже отрендеренные дополнительные секции (иерархия задачи,
// hierarchySections), вставляются между метаданными и текстом; пусто — ничего.
func formatMemory(m memory.Memory, sections string) string {
	var b strings.Builder

	title := m.Id
	if m.Summary != nil && strings.TrimSpace(*m.Summary) != "" {
		title = *m.Summary
	}
	fmt.Fprintf(&b, "## %s\n\n", title)
	fmt.Fprintf(&b, "- id: `%s`\n", m.Id)
	fmt.Fprintf(&b, "- kind: %s", m.Kind)
	if m.Status != "" {
		fmt.Fprintf(&b, " (status: %s)", m.Status)
	}
	b.WriteString("\n")
	// Task-only поля Этапа 15 — отдельными строками, пустые опускаются (у
	// записей kind != task их нет вовсе; у задачи type всегда заполнен).
	if m.Type != "" {
		fmt.Fprintf(&b, "- type: %s\n", m.Type)
	}
	if m.Branch != "" {
		fmt.Fprintf(&b, "- branch: %s\n", m.Branch)
	}
	if m.Workflow != "" {
		fmt.Fprintf(&b, "- workflow: %s\n", m.Workflow)
	}
	fmt.Fprintf(&b, "- scope: %s\n", m.Scope)
	fmt.Fprintf(&b, "- importance: %d\n", m.Importance)
	if len(m.Tags) > 0 {
		fmt.Fprintf(&b, "- tags: %s\n", strings.Join(m.Tags, ", "))
	}
	if len(m.Links) > 0 {
		refs := make([]string, len(m.Links))
		for i, l := range m.Links {
			rel := l.Rel
			if rel == "" {
				rel = memory.RelRelated
			}
			refs[i] = fmt.Sprintf("%s (%s)", l.Dst, rel)
		}
		fmt.Fprintf(&b, "- links: %s\n", strings.Join(refs, ", "))
	}
	if m.SupersededBy != nil {
		// Запись — тумстоун: в recall она уже не участвует, и агент, пришедший
		// по сохранённому id, должен об этом узнать, а не считать её актуальной.
		if *m.SupersededBy == m.Id {
			b.WriteString("- статус записи: архивирована (без замены)\n")
		} else {
			fmt.Fprintf(&b, "- статус записи: заменена на `%s`\n", *m.SupersededBy)
		}
	}

	b.WriteString("\n")
	if sections != "" {
		b.WriteString(sections)
	}
	b.WriteString(m.Content)
	b.WriteString("\n")
	return b.String()
}
