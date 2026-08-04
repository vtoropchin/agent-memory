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
// живёт в пакете write, здесь только разбор входа и текстовый ответ.
func (s *Server) handleUpdate(ctx context.Context, _ *sdkmcp.CallToolRequest, in updateInput) (*sdkmcp.CallToolResult, any, error) {
	id := strings.TrimSpace(in.ID)
	if id == "" {
		return nil, nil, fmt.Errorf("memory_update: id обязателен")
	}

	links, err := normalizeLinks(in.Links)
	if err != nil {
		return nil, nil, fmt.Errorf("memory_update: %w", err)
	}

	if err := s.writer.Update(ctx, write.UpdateInput{
		Id:         id,
		Content:    in.Content,
		Summary:    in.Summary,
		Importance: in.Importance,
		Status:     in.Status,
		Tags:       in.Tags,
		Links:      links,
		TTL:        in.TTL,
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, fmt.Errorf("memory_update: запись %q не найдена", id)
		}
		return nil, nil, fmt.Errorf("memory_update: %w", err)
	}
	return textResult(fmt.Sprintf("Обновлено: %s", id)), nil, nil
}
