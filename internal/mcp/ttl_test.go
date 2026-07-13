package mcp

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestHandleSaveTTLSetsExpiresAt проверяет, что memory_save с полем ttl (задача
// 9.4) доходит до write.Writer.Save и записывает Memory.ExpiresAt.
func TestHandleSaveTTLSetsExpiresAt(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)

	res, _, err := srv.handleSave(ctx, nil, saveInput{
		Kind: "semantic", Content: "факт с известным сроком годности",
		Scope: "project:agentmemory", TTL: "24h",
	})
	if err != nil {
		t.Fatalf("handleSave: %v", err)
	}
	id := extractID(t, textOf(res))

	got, err := srv.store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ExpiresAt == 0 {
		t.Errorf("ExpiresAt = 0, want непусто (ttl=24h должен был установить срок)")
	}
}

// TestHandleSaveInvalidTTLReturnsError проверяет, что невалидный ttl возвращает
// ошибку инструмента, а не паникует и не вставляет запись.
func TestHandleSaveInvalidTTLReturnsError(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)

	_, _, err := srv.handleSave(ctx, nil, saveInput{
		Kind: "semantic", Content: "факт с плохим сроком",
		Scope: "project:agentmemory", TTL: "не длительность",
	})
	if err == nil {
		t.Fatal("handleSave с невалидным ttl должен вернуть ошибку")
	}
}

// TestHandleUpdateTTLExtendsAndClears проверяет оба режима memory_update.ttl
// (задача 9.4): продление срока произвольной длительностью и снятие срока
// сентинелом "0" (запись становится вечной).
func TestHandleUpdateTTLExtendsAndClears(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)

	res, _, err := srv.handleSave(ctx, nil, saveInput{
		Kind: "semantic", Content: "факт для продления срока",
		Scope: "project:agentmemory", TTL: "1h",
	})
	if err != nil {
		t.Fatalf("handleSave: %v", err)
	}
	id := extractID(t, textOf(res))

	before, err := srv.store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get before: %v", err)
	}

	extended := "168h"
	if _, _, err := srv.handleUpdate(ctx, nil, updateInput{ID: id, TTL: &extended}); err != nil {
		t.Fatalf("handleUpdate (продление): %v", err)
	}
	afterExtend, err := srv.store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after extend: %v", err)
	}
	if afterExtend.ExpiresAt <= before.ExpiresAt {
		t.Errorf("ExpiresAt после продления = %d, want > %d", afterExtend.ExpiresAt, before.ExpiresAt)
	}

	cleared := "0"
	if _, _, err := srv.handleUpdate(ctx, nil, updateInput{ID: id, TTL: &cleared}); err != nil {
		t.Fatalf("handleUpdate (снятие): %v", err)
	}
	afterClear, err := srv.store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after clear: %v", err)
	}
	if afterClear.ExpiresAt != 0 {
		t.Errorf("ExpiresAt после ttl=\"0\" = %d, want 0 (вечная)", afterClear.ExpiresAt)
	}
}

// TestHandleRecallHidesExpiredBeforeSweep — двойная защита задачи 9.4: запись с
// TTL в прошлом не выдаётся memory_recall НЕМЕДЛЕННО (metaFilters), даже когда её
// ещё никто не заархивировал фоновым maint.Expirer (SupersededBy остаётся nil —
// sweep в этом тесте вообще не запускается). Дополняет TestExpireSweep*
// (internal/memory/maint), которые проверяют вторую половину двойной защиты —
// само архивирование.
func TestHandleRecallHidesExpiredBeforeSweep(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)

	res, _, err := srv.handleSave(ctx, nil, saveInput{
		Kind: "semantic", Content: "МАРКЕРПРОСРОЧЕНО факт с истёкшим сроком годности",
		Scope: "project:agentmemory",
	})
	if err != nil {
		t.Fatalf("handleSave: %v", err)
	}
	id := extractID(t, textOf(res))

	// Записываем срок в прошлое напрямую через Store (в обход write.Writer,
	// который принимает только положительные будущие TTL, см. write.parseTTL) —
	// имитирует запись, чей заранее заданный TTL уже истёк к моменту recall.
	m, err := srv.store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	m.ExpiresAt = time.Now().Unix() - 10
	if err := srv.store.Put(ctx, m); err != nil {
		t.Fatalf("Put (истёкший срок): %v", err)
	}

	brief, _, err := srv.handleRecall(ctx, nil, recallInput{Query: "МАРКЕРПРОСРОЧЕНО", Scope: "project:agentmemory"})
	if err != nil {
		t.Fatalf("handleRecall: %v", err)
	}
	if strings.Contains(textOf(brief), "МАРКЕРПРОСРОЧЕНО") {
		t.Fatalf("recall вернул запись с истёкшим TTL до прогона sweep:\n%s", textOf(brief))
	}

	// Sweep ещё не запускался (Scheduler не поднят в этом тесте) — запись
	// скрыта recall фильтром, но пока НЕ тумстоун.
	stillLive, err := srv.store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get после recall: %v", err)
	}
	if stillLive.SupersededBy != nil {
		t.Errorf("SupersededBy = %v, want nil (sweep не запускался, только recall-фильтр)", stillLive.SupersededBy)
	}
}
