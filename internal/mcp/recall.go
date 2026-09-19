package mcp

import (
	"context"
	"fmt"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"agentmemory/internal/memory"
)

// handleRecall реализует memory_recall: главный инструмент, возвращает готовый
// Markdown-бриф из Recaller.Recall как текстовый контент (DESIGN.md §8, §12) — не
// JSON-простыню.
func (s *Server) handleRecall(ctx context.Context, _ *sdkmcp.CallToolRequest, in recallInput) (*sdkmcp.CallToolResult, any, error) {
	now := time.Now()
	createdAfter, err := parseRecallDuration(in.CreatedAfter, now)
	if err != nil {
		return nil, nil, fmt.Errorf("memory_recall: created_after: %w", err)
	}
	createdBefore, err := parseRecallDuration(in.CreatedBefore, now)
	if err != nil {
		return nil, nil, fmt.Errorf("memory_recall: created_before: %w", err)
	}

	brief, err := s.recaller.Recall(ctx, memory.Query{
		Text:          in.Query,
		Scopes:        s.resolveRecallScopes(in),
		Kinds:         in.Kinds,
		Statuses:      in.Statuses,
		Types:         in.Types,
		Tags:          in.Tags,
		CreatedAfter:  createdAfter,
		CreatedBefore: createdBefore,
		TokenBudget:   in.TokenBudget,
		Summarize:     in.Summarize,
		Explain:       in.Explain,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("memory_recall: %w", err)
	}
	return textResult(brief.Markdown), nil, nil
}

// parseRecallDuration переводит относительную длительность recallInput.CreatedAfter/
// CreatedBefore (задача 8.2, формат time.ParseDuration, напр. "168h" = неделя) в unix
// ts — момент now минус эта длительность. Пустая строка означает «фильтр не задан»
// (0, nil) — так и Query.CreatedAfter/CreatedBefore=0 отключают фильтр в metaFilters.
// now передаётся параметром (а не берётся из time.Now() внутри) — вызывающая сторона
// (handleRecall) фиксирует один момент на оба поля, а тесты получают детерминированный
// расчёт без подмены глобальных часов.
func parseRecallDuration(s string, now time.Time) (int64, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("не удалось разобрать длительность %q (ожидается формат time.ParseDuration, напр. \"168h\"): %w", s, err)
	}
	return now.Add(-d).Unix(), nil
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
