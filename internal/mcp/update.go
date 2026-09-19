package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"agentmemory/internal/memory/store"
	"agentmemory/internal/memory/write"
)

// handleUpdate реализует memory_update: тонкая обёртка над write.Writer.Update
// (DESIGN.md §12) — вся доменная логика (Get→мутация→Put, нормализация importance)
// живёт в пакете write, здесь только разбор входа и текстовый ответ. Доменные
// ошибки (в т.ч. write.ErrTaskBlocked с перечнем блокеров и
// write.ErrBlockerNotTask, задача 15.4) доходят до агента текстом ошибки
// инструмента как есть — их Error() уже читаем и полон.
func (s *Server) handleUpdate(ctx context.Context, _ *sdkmcp.CallToolRequest, in updateInput) (*sdkmcp.CallToolResult, any, error) {
	id := strings.TrimSpace(in.ID)
	if id == "" {
		return nil, nil, fmt.Errorf("memory_update: id обязателен")
	}

	links, err := normalizeLinks(in.Links)
	if err != nil {
		return nil, nil, fmt.Errorf("memory_update: %w", err)
	}
	unlinks, err := normalizeLinks(in.Unlinks)
	if err != nil {
		return nil, nil, fmt.Errorf("memory_update: unlinks: %w", err)
	}

	if err := s.writer.Update(ctx, write.UpdateInput{
		Id:         id,
		Content:    in.Content,
		Summary:    in.Summary,
		Importance: in.Importance,
		Status:     in.Status,
		Type:       in.Type,
		Branch:     in.Branch,
		Workflow:   in.Workflow,
		Tags:       in.Tags,
		Links:      links,
		Unlinks:    unlinks,
		TTL:        in.TTL,
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, fmt.Errorf("memory_update: запись %q не найдена", id)
		}
		return nil, nil, fmt.Errorf("memory_update: %w", err)
	}
	return textResult(fmt.Sprintf("Обновлено: %s", id)), nil, nil
}
