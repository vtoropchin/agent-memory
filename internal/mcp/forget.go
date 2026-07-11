package mcp

import (
	"context"
	"fmt"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// handleForget реализует memory_forget: тонкая обёртка над write.Writer.Forget
// (DESIGN.md §12) — тумстоун-логика (Supersede на себя или на replaced_by) живёт
// в пакете write, здесь только разбор входа и текстовый ответ.
func (s *Server) handleForget(ctx context.Context, _ *sdkmcp.CallToolRequest, in forgetInput) (*sdkmcp.CallToolResult, any, error) {
	id := strings.TrimSpace(in.ID)
	if id == "" {
		return nil, nil, fmt.Errorf("memory_forget: id обязателен")
	}
	replacedBy := strings.TrimSpace(in.ReplacedBy)

	if err := s.writer.Forget(ctx, id, replacedBy); err != nil {
		return nil, nil, fmt.Errorf("memory_forget: %w", err)
	}

	if replacedBy != "" {
		return textResult(fmt.Sprintf("Забыто: %s (заменено на %s)", id, replacedBy)), nil, nil
	}
	return textResult(fmt.Sprintf("Забыто: %s", id)), nil, nil
}
