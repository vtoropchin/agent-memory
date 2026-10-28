package store

import (
	"context"
	"testing"

	"agentmemory/internal/memory"
)

// Тесты расширений порта для новой web-морды (DESIGN.md §14 Этап 16, задача 16.1):
// Stats.PendingVectors, SchemaVersion, LastMaintRun/SetLastMaintRun и фильтр
// BrowseFilter.ExpiresBefore. Все — паритетные (sqlite + postgres при
// MEMO_TEST_POSTGRES_DSN), кроме точной версии схемы SQLite.

func TestParityStatsPendingVectors(t *testing.T) {
	runParityWithVector(t, "StatsPendingVectors", func(t *testing.T, s Store) {
		ctx := context.Background()

		withVec, withoutVec, superseded := newID(t), newID(t), newID(t)
		for _, id := range []string{withVec, withoutVec, superseded} {
			if err := s.Put(ctx, memory.Memory{Id: id, Kind: memory.KindSemantic, Scope: "s", Content: "c " + id, CreatedAt: 1, UpdatedAt: 1}); err != nil {
				t.Fatalf("Put %s: %v", id, err)
			}
		}

		// Векторного индекса ещё нет — очередь по определению пуста (деградация
		// §15, не ошибка), даже если записи без вектора есть.
		stats, err := s.Stats(ctx)
		if err != nil {
			t.Fatalf("Stats без индекса: %v", err)
		}
		if stats.PendingVectors != 0 {
			t.Fatalf("PendingVectors без индекса = %d, want 0", stats.PendingVectors)
		}

		if err := s.ResetVectorIndex(ctx, "test-model", 3); err != nil {
			t.Fatalf("ResetVectorIndex: %v", err)
		}
		if err := s.PutVector(ctx, withVec, []float32{1, 0, 0}); err != nil {
			t.Fatalf("PutVector: %v", err)
		}
		if err := s.Supersede(ctx, superseded, withVec, ""); err != nil {
			t.Fatalf("Supersede: %v", err)
		}

		stats, err = s.Stats(ctx)
		if err != nil {
			t.Fatalf("Stats с индексом: %v", err)
		}
		// Живая запись без вектора — одна; тумстоун без вектора в очередь не
		// входит (тот же предикат, что у ListMissingVectors).
		if stats.PendingVectors != 1 {
			t.Fatalf("PendingVectors = %d, want 1 (только %q)", stats.PendingVectors, withoutVec)
		}
	})
}

func TestParitySchemaVersion(t *testing.T) {
	runParity(t, "SchemaVersion", func(t *testing.T, s Store) {
		v, err := s.SchemaVersion(context.Background())
		if err != nil {
			t.Fatalf("SchemaVersion: %v", err)
		}
		if v <= 0 {
			t.Fatalf("SchemaVersion = %d, want > 0 (схема накатана при Open)", v)
		}
	})
}

// TestSchemaVersionSQLite — точное значение: PRAGMA user_version после Open равна
// целевой schemaVersion драйвера.
func TestSchemaVersionSQLite(t *testing.T) {
	v, err := newTestStore(t).SchemaVersion(context.Background())
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != schemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", v, schemaVersion)
	}
}

func TestParityMaintRun(t *testing.T) {
	runParity(t, "MaintRun", func(t *testing.T, s Store) {
		ctx := context.Background()

		at, err := s.LastMaintRun(ctx)
		if err != nil {
			t.Fatalf("LastMaintRun на свежей БД: %v", err)
		}
		if at != 0 {
			t.Fatalf("LastMaintRun на свежей БД = %d, want 0 (прогонов не было)", at)
		}

		if err := s.SetLastMaintRun(ctx, 1234); err != nil {
			t.Fatalf("SetLastMaintRun(1234): %v", err)
		}
		if at, err = s.LastMaintRun(ctx); err != nil || at != 1234 {
			t.Fatalf("LastMaintRun = %d, %v; want 1234", at, err)
		}

		// Повторная запись перезаписывает единственную строку, а не добавляет.
		if err := s.SetLastMaintRun(ctx, 5678); err != nil {
			t.Fatalf("SetLastMaintRun(5678): %v", err)
		}
		if at, err = s.LastMaintRun(ctx); err != nil || at != 5678 {
			t.Fatalf("LastMaintRun после перезаписи = %d, %v; want 5678", at, err)
		}
	})
}

func TestParityListExpiresBefore(t *testing.T) {
	runParity(t, "ListExpiresBefore", func(t *testing.T, s Store) {
		ctx := context.Background()

		eternal, soon, later, tooLate, otherScope, archived := newID(t), newID(t), newID(t), newID(t), newID(t), newID(t)
		for _, tc := range []struct {
			id        string
			scope     string
			createdAt int64
			expiresAt int64
		}{
			{eternal, "project:x", 10, 0},
			{later, "project:x", 20, 300}, // создана раньше soon, истекает позже
			{soon, "project:x", 30, 100},
			{tooLate, "project:x", 40, 900}, // за границей expiresBefore
			{otherScope, "project:y", 50, 150},
			{archived, "project:x", 60, 120},
		} {
			if err := s.Put(ctx, memory.Memory{
				Id: tc.id, Kind: memory.KindSemantic, Scope: tc.scope,
				Content: "c", CreatedAt: tc.createdAt, UpdatedAt: tc.createdAt, ExpiresAt: tc.expiresAt,
			}); err != nil {
				t.Fatalf("Put %s: %v", tc.id, err)
			}
		}
		if err := s.Supersede(ctx, archived, archived, ""); err != nil {
			t.Fatalf("Supersede: %v", err)
		}

		items, total, err := s.List(ctx, BrowseFilter{Scopes: []string{"project:x"}, ExpiresBefore: 500})
		if err != nil {
			t.Fatalf("List с ExpiresBefore: %v", err)
		}
		if total != 2 || len(items) != 2 {
			t.Fatalf("List = %d записей, total=%d; want 2/2 (soon, later): %+v", len(items), total, items)
		}
		// Порядок — по сроку, ближайшие первыми, а не по created_at DESC.
		if items[0].Id != soon || items[1].Id != later {
			t.Fatalf("порядок = [%s, %s], want [soon=%s, later=%s]", items[0].Id, items[1].Id, soon, later)
		}

		// Без фильтра — прежнее поведение: все живые scope по created_at DESC.
		items, total, err = s.List(ctx, BrowseFilter{Scopes: []string{"project:x"}})
		if err != nil {
			t.Fatalf("List без ExpiresBefore: %v", err)
		}
		if total != 4 || items[0].Id != tooLate {
			t.Fatalf("List без фильтра: total=%d, first=%s; want 4, %s", total, items[0].Id, tooLate)
		}
	})
}
