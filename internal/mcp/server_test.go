package mcp

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"agentmemory/internal/config"
	"agentmemory/internal/memory/recall"
	"agentmemory/internal/memory/store"
	"agentmemory/internal/memory/write"
)

// newTestServer собирает Server поверх свежего файлового SQLite-хранилища —
// без реального stdio-транспорта, ровно то, что нужно для проверки роутинга
// инструментов на реальном стеке write/recall/store.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "m.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	w := write.New(s)
	r := recall.New(s, config.Default().Recall)
	return NewServer(w, r, s)
}

func TestSaveThenRecall(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)

	res1, _, err := srv.handleSave(ctx, nil, saveInput{
		Kind:    "semantic",
		Content: "Проект AgentMemory написан на Go 1.26.",
		Scope:   "project:agentmemory",
	})
	if err != nil {
		t.Fatalf("handleSave #1: %v", err)
	}
	if !strings.Contains(textOf(res1), "Сохранено:") {
		t.Fatalf("handleSave #1: неожиданный результат %q", textOf(res1))
	}

	res2, _, err := srv.handleSave(ctx, nil, saveInput{
		Kind:    "procedural",
		Content: "Сборка проекта: make build; тесты: make test.",
		Scope:   "project:agentmemory",
	})
	if err != nil {
		t.Fatalf("handleSave #2: %v", err)
	}
	if !strings.Contains(textOf(res2), "Сохранено:") {
		t.Fatalf("handleSave #2: неожиданный результат %q", textOf(res2))
	}

	recallRes, _, err := srv.handleRecall(ctx, nil, recallInput{
		Query: "Go проект сборка",
		Scope: "project:agentmemory",
	})
	if err != nil {
		t.Fatalf("handleRecall: %v", err)
	}
	md := textOf(recallRes)
	if !strings.Contains(md, "Go 1.26") {
		t.Errorf("бриф не содержит сохранённый semantic-факт:\n%s", md)
	}
	if !strings.Contains(md, "make build") {
		t.Errorf("бриф не содержит сохранённый procedural-факт:\n%s", md)
	}
}

func TestForgetRemovesFromRecall(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)

	res, _, err := srv.handleSave(ctx, nil, saveInput{
		Kind:    "episodic",
		Content: "В сессии X чинили гонку в кэше SimHash.",
		Scope:   "project:agentmemory",
	})
	if err != nil {
		t.Fatalf("handleSave: %v", err)
	}
	id := extractID(t, textOf(res))

	before, _, err := srv.handleRecall(ctx, nil, recallInput{Query: "гонка кэш SimHash", Scope: "project:agentmemory"})
	if err != nil {
		t.Fatalf("handleRecall (до forget): %v", err)
	}
	if !strings.Contains(textOf(before), "гонку в кэше") {
		t.Fatalf("запись должна быть видна до forget:\n%s", textOf(before))
	}

	if _, _, err := srv.handleForget(ctx, nil, forgetInput{ID: id}); err != nil {
		t.Fatalf("handleForget: %v", err)
	}

	after, _, err := srv.handleRecall(ctx, nil, recallInput{Query: "гонка кэш SimHash", Scope: "project:agentmemory"})
	if err != nil {
		t.Fatalf("handleRecall (после forget): %v", err)
	}
	if strings.Contains(textOf(after), "гонку в кэше") {
		t.Fatalf("запись должна пропасть из recall после forget:\n%s", textOf(after))
	}
}

func TestUpdateImportancePersists(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)

	res, _, err := srv.handleSave(ctx, nil, saveInput{
		Kind:       "semantic",
		Content:    "API-ключ DeepSeek читается из env DEEPSEEK_API_KEY.",
		Scope:      "project:agentmemory",
		Importance: 2,
	})
	if err != nil {
		t.Fatalf("handleSave: %v", err)
	}
	id := extractID(t, textOf(res))

	newImportance := 5
	if _, _, err := srv.handleUpdate(ctx, nil, updateInput{ID: id, Importance: &newImportance}); err != nil {
		t.Fatalf("handleUpdate: %v", err)
	}

	m, err := srv.store.Get(ctx, id)
	if err != nil {
		t.Fatalf("store.Get после update: %v", err)
	}
	if m.Importance != 5 {
		t.Fatalf("importance после update = %d, ожидалось 5", m.Importance)
	}
}

// TestSaveAndUpdateImportanceClampConsistently — регресс на рассинхрон: раньше
// memory_save клампил importance вне 1..5 через clampImportance, а memory_update
// падал ошибкой на том же значении (валидировал отдельно). После выноса нормализации
// в write.Writer (Save и Update оба используют clampImportance) оба инструмента
// должны клампить одинаково.
func TestSaveAndUpdateImportanceClampConsistently(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)

	res, _, err := srv.handleSave(ctx, nil, saveInput{
		Kind:       "semantic",
		Content:    "факт с зашкаливающей важностью при сохранении",
		Scope:      "project:agentmemory",
		Importance: 9,
	})
	if err != nil {
		t.Fatalf("handleSave: %v", err)
	}
	savedID := extractID(t, textOf(res))
	saved, err := srv.store.Get(ctx, savedID)
	if err != nil {
		t.Fatalf("store.Get после save: %v", err)
	}
	if saved.Importance != 5 {
		t.Fatalf("memory_save importance=9 -> %d, want 5 (clamp)", saved.Importance)
	}

	otherRes, _, err := srv.handleSave(ctx, nil, saveInput{
		Kind:    "semantic",
		Content: "отдельный факт для проверки memory_update",
		Scope:   "project:agentmemory",
	})
	if err != nil {
		t.Fatalf("handleSave (отдельная запись): %v", err)
	}
	updateID := extractID(t, textOf(otherRes))

	overImportance := 9
	if _, _, err := srv.handleUpdate(ctx, nil, updateInput{ID: updateID, Importance: &overImportance}); err != nil {
		t.Fatalf("handleUpdate: %v (memory_update не должен падать на importance=9, как и memory_save)", err)
	}
	updated, err := srv.store.Get(ctx, updateID)
	if err != nil {
		t.Fatalf("store.Get после update: %v", err)
	}
	if updated.Importance != 5 {
		t.Fatalf("memory_update importance=9 -> %d, want 5 (clamp, как и memory_save)", updated.Importance)
	}
}

func TestSaveValidationErrorNotPanic(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)

	_, _, err := srv.handleSave(ctx, nil, saveInput{Kind: "semantic", Content: "   ", Scope: "project:agentmemory"})
	if err == nil {
		t.Fatal("ожидалась ошибка на пустой content, получили nil")
	}
}

func TestUpdateUnknownIDNotPanic(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)

	_, _, err := srv.handleUpdate(ctx, nil, updateInput{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV"})
	if err == nil {
		t.Fatal("ожидалась ошибка на неизвестный id, получили nil")
	}
}

func TestForgetUnknownIDNotPanic(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)

	_, _, err := srv.handleForget(ctx, nil, forgetInput{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV"})
	if err == nil {
		t.Fatal("ожидалась ошибка на неизвестный id, получили nil")
	}
}

// --- вспомогательные функции ---

// textOf извлекает текст первого текстового блока результата инструмента —
// вся выдача наших хендлеров состоит из одного TextContent (см. textResult).
func textOf(res *sdkmcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	for _, c := range res.Content {
		if tc, ok := c.(*sdkmcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}

func extractID(t *testing.T, msg string) string {
	t.Helper()
	fields := strings.Fields(msg)
	for i, f := range fields {
		if strings.HasSuffix(f, ":") && i+1 < len(fields) {
			return strings.TrimSuffix(fields[i+1], ",")
		}
	}
	t.Fatalf("не удалось извлечь id из сообщения: %q", msg)
	return ""
}
