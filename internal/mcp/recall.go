package mcp

import (
	"context"
	"fmt"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"agentmemory/internal/memory"
)

// handleRecall реализует memory_recall: главный инструмент, возвращает готовый
// Markdown-бриф из Recaller.Recall как текстовый контент (DESIGN.md §8, §12) — не
// JSON-простыню.
func (s *Server) handleRecall(ctx context.Context, _ *sdkmcp.CallToolRequest, in recallInput) (*sdkmcp.CallToolResult, any, error) {
	brief, err := s.recaller.Recall(ctx, memory.Query{
		Text:        in.Query,
		Scope:       in.Scope,
		Kinds:       in.Kinds,
		TokenBudget: in.TokenBudget,
		Summarize:   in.Summarize,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("memory_recall: %w", err)
	}
	return textResult(brief.Markdown), nil, nil
}
