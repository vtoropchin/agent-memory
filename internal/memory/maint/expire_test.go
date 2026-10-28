package maint

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"agentmemory/internal/memory"
	"agentmemory/internal/memory/embed"
	"agentmemory/internal/memory/store"
	"agentmemory/internal/memory/write"
)

// putMemoryWithTTL вставляет живую semantic-запись напрямую в Store (в обход
// write path) с явным ExpiresAt — Expirer реагирует именно на это поле.
// expiresAt<=0 означает вечную запись (как и Memory.ExpiresAt в целом).
func putMemoryWithTTL(t *testing.T, s store.Store, scope, content string, expiresAt int64) memory.Memory {
	t.Helper()
	now := time.Now().Unix()
	m := memory.Memory{
		Id:         ulid.Make().String(),
		Kind:       memory.KindSemantic,
		Scope:      scope,
		Content:    content,
		Importance: 3,
		CreatedAt:  now,
		UpdatedAt:  now,
		ExpiresAt:  expiresAt,
	}
	if err := s.Put(context.Background(), m); err != nil {
		t.Fatalf("putMemoryWithTTL: %v", err)
	}
	return m
}

// TestExpireSweepArchivesOnlyExpired проверяет, что ExpireSweep архивирует
// (self-supersede-тумстоун) только записи с истёкшим TTL, оставляя вечные
// (ExpiresAt=0) и ещё не истёкшие нетронутыми.
func TestExpireSweepArchivesOnlyExpired(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	e := NewExpirer(s, write.New(s, embed.NewNoop()))

	now := time.Now().Unix()
	expired := putMemoryWithTTL(t, s, "project:x", "просроченный факт", now-10)
	eternal := putMemoryWithTTL(t, s, "project:x", "вечный факт", 0)
	future := putMemoryWithTTL(t, s, "project:x", "факт с запасом времени", now+3600)

	result, err := e.ExpireSweep(ctx, 0)
	if err != nil {
		t.Fatalf("ExpireSweep: %v", err)
	}
	if result.Scanned != 1 {
		t.Errorf("Scanned = %d, want 1", result.Scanned)
	}
	if result.Archived != 1 {
		t.Errorf("Archived = %d, want 1", result.Archived)
	}

	gotExpired, err := s.Get(ctx, expired.Id)
	if err != nil {
		t.Fatalf("Get expired: %v", err)
	}
	if gotExpired.SupersededBy == nil || *gotExpired.SupersededBy != expired.Id {
		t.Errorf("expired.SupersededBy = %v, want self-supersede %q", gotExpired.SupersededBy, expired.Id)
	}

	gotEternal, err := s.Get(ctx, eternal.Id)
	if err != nil {
		t.Fatalf("Get eternal: %v", err)
	}
	if gotEternal.SupersededBy != nil {
		t.Errorf("eternal.SupersededBy = %v, want nil (без TTL sweep не трогает)", gotEternal.SupersededBy)
	}

	gotFuture, err := s.Get(ctx, future.Id)
	if err != nil {
		t.Fatalf("Get future: %v", err)
	}
	if gotFuture.SupersededBy != nil {
		t.Errorf("future.SupersededBy = %v, want nil (TTL ещё не истёк)", gotFuture.SupersededBy)
	}
}

// TestExpireSweepIdempotent проверяет, что повторный прогон ExpireSweep не видит
// уже заархивированные записи (superseded_by исключает их из ListExpired).
func TestExpireSweepIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	e := NewExpirer(s, write.New(s, embed.NewNoop()))

	now := time.Now().Unix()
	putMemoryWithTTL(t, s, "project:x", "просроченный факт", now-10)

	result, err := e.ExpireSweep(ctx, 0)
	if err != nil {
		t.Fatalf("ExpireSweep: %v", err)
	}
	if result.Scanned != 1 || result.Archived != 1 {
		t.Fatalf("первый прогон = %+v, want Scanned=1 Archived=1", result)
	}

	result2, err := e.ExpireSweep(ctx, 0)
	if err != nil {
		t.Fatalf("ExpireSweep (повторный): %v", err)
	}
	if result2.Scanned != 0 || result2.Archived != 0 {
		t.Errorf("повторный ExpireSweep = %+v, want все нули", result2)
	}
}
