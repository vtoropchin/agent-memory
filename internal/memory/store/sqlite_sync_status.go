package store

import (
	"context"
	"fmt"

	"agentmemory/internal/memory"
)

// PendingCount — число неотправленных записей scope видов kinds (пусто — любых)
// для статуса и doctor. В отличие от Dirty только читает: пометки лишних видов
// не снимает.
func (l *SyncLocal) PendingCount(ctx context.Context, scope string, kinds []memory.Kind) (int, error) {
	where := "m.scope = ?"
	args := []any{scope}
	if clause, kargs := kindPredicate(kindStrings(kinds)); clause != "" {
		where += " AND " + clause
		args = append(args, kargs...)
	}
	var n int
	if err := l.s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sync_dirty d JOIN memories m ON m.id = d.memory_id WHERE `+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("подсчёт неотправленного %q: %w", scope, err)
	}
	return n, nil
}
