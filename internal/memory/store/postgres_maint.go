package store

import (
	"context"
	"fmt"

	"agentmemory/internal/memory"
)

// ListEpisodicForConsolidation возвращает episodic-записи старше olderThan без
// superseded_by, отсортированные по scope, затем created_at (DESIGN.md §11). Tags/Links
// не подгружаются (потребитель — maint.Consolidator — использует только
// Id/Content/Scope). Паритет с sqliteStore.ListEpisodicForConsolidation.
func (s *pgStore) ListEpisodicForConsolidation(ctx context.Context, olderThan int64, limit int) ([]memory.Memory, error) {
	a := &argList{}
	kindPh := a.add(string(memory.KindEpisodic))
	olderPh := a.add(olderThan)
	query := `
		SELECT ` + memoryColumns + `
		FROM memories m
		WHERE kind = ` + kindPh + ` AND superseded_by IS NULL AND created_at < ` + olderPh + `
		ORDER BY scope, created_at
	`
	if limit > 0 {
		query += " LIMIT " + a.add(limit)
	}

	rows, err := s.db.QueryContext(ctx, query, a.args...)
	if err != nil {
		return nil, fmt.Errorf("выборка episodic для консолидации: %w", err)
	}
	defer rows.Close()

	var episodes []memory.Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, fmt.Errorf("сканирование episodic для консолидации: %w", err)
		}
		episodes = append(episodes, m)
	}
	return episodes, rows.Err()
}

// ListEpisodicForDecay возвращает episodic-записи без superseded_by, у которых
// COALESCE(accessed_at, created_at) < notAccessedSince, отсортированные по той же дате
// по возрастанию (DESIGN.md §11). Tags/Links не подгружаются. Паритет с
// sqliteStore.ListEpisodicForDecay.
func (s *pgStore) ListEpisodicForDecay(ctx context.Context, notAccessedSince int64, limit int) ([]memory.Memory, error) {
	a := &argList{}
	kindPh := a.add(string(memory.KindEpisodic))
	sincePh := a.add(notAccessedSince)
	query := `
		SELECT ` + memoryColumns + `
		FROM memories m
		WHERE kind = ` + kindPh + ` AND superseded_by IS NULL AND COALESCE(accessed_at, created_at) < ` + sincePh + `
		ORDER BY COALESCE(accessed_at, created_at)
	`
	if limit > 0 {
		query += " LIMIT " + a.add(limit)
	}

	rows, err := s.db.QueryContext(ctx, query, a.args...)
	if err != nil {
		return nil, fmt.Errorf("выборка episodic для decay: %w", err)
	}
	defer rows.Close()

	var episodes []memory.Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, fmt.Errorf("сканирование episodic для decay: %w", err)
		}
		episodes = append(episodes, m)
	}
	return episodes, rows.Err()
}

// ListExpired возвращает живые записи с истёкшим явным TTL: 0 < expires_at < now
// (DESIGN.md §4, §11, задача 9.4), отсортированные по expires_at по возрастанию.
// Tags/Links не подгружаются. Паритет с sqliteStore.ListExpired (та же семантика
// строгого "<", не "<=" — см. комментарий там).
func (s *pgStore) ListExpired(ctx context.Context, now int64, limit int) ([]memory.Memory, error) {
	a := &argList{}
	nowPh := a.add(now)
	query := `
		SELECT ` + memoryColumns + `
		FROM memories m
		WHERE superseded_by IS NULL AND expires_at IS NOT NULL AND expires_at > 0 AND expires_at < ` + nowPh + `
		ORDER BY expires_at
	`
	if limit > 0 {
		query += " LIMIT " + a.add(limit)
	}

	rows, err := s.db.QueryContext(ctx, query, a.args...)
	if err != nil {
		return nil, fmt.Errorf("выборка просроченных записей: %w", err)
	}
	defer rows.Close()

	var expired []memory.Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, fmt.Errorf("сканирование просроченной записи: %w", err)
		}
		expired = append(expired, m)
	}
	return expired, rows.Err()
}
