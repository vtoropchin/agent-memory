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
		Scopes:      s.resolveRecallScopes(in),
		Kinds:       in.Kinds,
		TokenBudget: in.TokenBudget,
		Summarize:   in.Summarize,
		Explain:     in.Explain,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("memory_recall: %w", err)
	}
	return textResult(brief.Markdown), nil, nil
}

// resolveRecallScopes разрешает набор scope для recall с приоритетом явного
// значения из вызова над дефолтом инстанса (DESIGN.md §2.1):
//  1. непустой in.Scopes — используется как есть;
//  2. иначе непустой in.Scope (back-compat одиночного поля) — набор из одного scope;
//  3. иначе — дефолт инстанса s.scope.RecallScopes() (project:<name> + Shared);
//     если project не задан, вернёт nil — recall не фильтрует по scope (adhoc-режим).
func (s *Server) resolveRecallScopes(in recallInput) []string {
	if len(in.Scopes) > 0 {
		return in.Scopes
	}
	if in.Scope != "" {
		return []string{in.Scope}
	}
	return s.scope.RecallScopes()
}
