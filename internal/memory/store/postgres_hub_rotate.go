package store

import (
	"context"
	"errors"
	"fmt"
)

// errNoSyncMeta — в общей базе нет схемы синхронизации, ротировать нечего.
var errNoSyncMeta = errors.New("в общей базе нет схемы синхронизации — сначала примените схему")

// RotateBase выдаёт общей базе новую идентичность sync_meta.base_id после
// восстановления из копии (DESIGN.md §4.1): у всех клиентов курсор pull
// сбросится и набор сверится целиком. Возвращает новый base_id. Только на схеме
// версии бинаря: старее — sync_meta может не быть, новее — не этому memo её
// менять. Явная операция с подтверждением — как ApplySchema.
func (h *Hub) RotateBase(ctx context.Context) (string, error) {
	st, err := h.Schema(ctx)
	if err != nil {
		return "", err
	}
	switch {
	case st.Empty:
		return "", errNoSyncMeta
	case !st.Current():
		return "", fmt.Errorf("схема общей базы версии %d, а этот memo понимает %d — ротация только на актуальной схеме", st.Version, st.Supported)
	}
	var id string
	err = h.pg.db.QueryRowContext(ctx, `
		INSERT INTO sync_meta (id, base_id) VALUES (1, gen_random_uuid()::text)
		ON CONFLICT (id) DO UPDATE SET base_id = excluded.base_id
		RETURNING base_id`).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("ротация base_id общей базы: %w", err)
	}
	return id, nil
}
