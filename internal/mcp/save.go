package mcp

import (
	"context"
	"fmt"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"agentmemory/internal/memory"
	"agentmemory/internal/memory/write"
)

// handleSave реализует memory_save: роутинг на Writer.Save (DESIGN.md §7, §12).
//
// Writer.Save честно сообщает через возвращаемый merged, был ли факт слит с
// существующим дублем (reinforce) вместо вставки новой записи, — поэтому здесь не
// нужен дополнительный запрос к Store для эвристического определения слияния.
func (s *Server) handleSave(ctx context.Context, _ *sdkmcp.CallToolRequest, in saveInput) (*sdkmcp.CallToolResult, any, error) {
	scope := in.Scope
	if scope == "" {
		// Явный scope не передан — подставляем "домашний" scope инстанса
		// (project:<name>). Если мультипроектность выключена (project не задан),
		// SaveScope() вернёт "" и Writer.Save сам вернёт write.ErrEmptyScope.
		scope = s.scope.SaveScope()
	}

	links, err := normalizeLinks(in.Links)
	if err != nil {
		return nil, nil, fmt.Errorf("memory_save: %w", err)
	}

	id, merged, err := s.writer.Save(ctx, write.SaveInput{
		Kind:       memory.Kind(in.Kind),
		Scope:      scope,
		Content:    in.Content,
		Summary:    in.Summary,
		Status:     in.Status,
		Type:       in.Type,
		Branch:     in.Branch,
		Workflow:   in.Workflow,
		Importance: in.Importance,
		Tags:       in.Tags,
		Links:      links,
		Source:     in.Source,
		TTL:        in.TTL,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("memory_save: %w", err)
	}

	if merged {
		return textResult(fmt.Sprintf("Сохранено: %s (слито с существующей похожей записью)", id)), nil, nil
	}
	return textResult(fmt.Sprintf("Сохранено: %s", id)), nil, nil
}
