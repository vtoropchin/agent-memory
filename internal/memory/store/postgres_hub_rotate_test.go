package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestHub_RotateBase: новая идентичность видна в Base; на базе без схемы —
// errNoSyncMeta, ничего не создаётся; на схеме чужой версии — отказ.
func TestHub_RotateBase(t *testing.T) {
	ctx := context.Background()

	empty := newTestHub(t)
	if _, err := empty.RotateBase(ctx); !errors.Is(err, errNoSyncMeta) {
		t.Fatalf("RotateBase без схемы = %v, хочу errNoSyncMeta", err)
	}
	if st, err := empty.Schema(ctx); err != nil || !st.Empty {
		t.Fatalf("RotateBase создал схему: %+v, %v", st, err)
	}

	h := newTestHubApplied(t)
	if _, err := h.pg.db.ExecContext(ctx, `UPDATE schema_version SET version = version + 1 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.RotateBase(ctx); err == nil || !strings.Contains(err.Error(), "только на актуальной схеме") {
		t.Fatalf("RotateBase на схеме новее бинаря = %v, хочу отказ", err)
	}
	if _, err := h.pg.db.ExecContext(ctx, `UPDATE schema_version SET version = version - 1 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	before, err := h.Base(ctx)
	if err != nil || before.ID == "" {
		t.Fatalf("Base до ротации = %+v, %v", before, err)
	}
	id, err := h.RotateBase(ctx)
	if err != nil || id == "" || id == before.ID {
		t.Fatalf("RotateBase = %q, %v; хочу новый base_id, был %q", id, err, before.ID)
	}
	if after, err := h.Base(ctx); err != nil || after.ID != id {
		t.Fatalf("Base после ротации = %+v, %v; хочу ID %q", after, err, id)
	}
}
