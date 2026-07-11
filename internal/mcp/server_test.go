package mcp

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"agentmemory/internal/config"
	"agentmemory/internal/memory/reason"
	"agentmemory/internal/memory/recall"
	"agentmemory/internal/memory/store"
	"agentmemory/internal/memory/write"
)

// newTestServer собирает Server поверх свежего файлового SQLite-хранилища —
// без реального stdio-транспорта, ровно то, что нужно для проверки роутинга
// инструментов на реальном стеке write/recall/store. scope пуст (дефолт
// config.Default().Scope: мультипроектность выключена) — используй
// newTestServerWithScope, если тесту нужен настроенный project.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	return newTestServerWithScope(t, config.Default().Scope)
}

// newTestServerWithScope — как newTestServer, но с явно заданным ScopeConfig, для
// проверки разрешения дефолтов save/recall по DESIGN.md §2.1.
func newTestServerWithScope(t *testing.T, scope config.ScopeConfig) *Server {
	t.Helper()
	return newServerOnDB(t, filepath.Join(t.TempDir(), "m.db"), scope)
}

// newServerOnDB собирает Server поверх БД по заданному пути — позволяет поднять
// несколько инстансов над одним файлом (проверка изоляции проектов, DESIGN.md §2.1).
func newServerOnDB(t *testing.T, dbPath string, scope config.ScopeConfig) *Server {
	t.Helper()
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	w := write.New(s)
	r := recall.New(s, config.Default().Recall, reason.NewNoop())
	return NewServer(w, r, s, scope)
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

// TestResolveRecallScopes проверяет разрешение набора scope для memory_recall
// (DESIGN.md §2.1): приоритет явного scopes/scope из вызова над дефолтом
// инстанса, и adhoc-режим (без фильтра) при незаданном project.
func TestResolveRecallScopes(t *testing.T) {
	alpha := newTestServerWithScope(t, config.ScopeConfig{Project: "alpha", Shared: []string{"user:*"}})
	adhoc := newTestServerWithScope(t, config.ScopeConfig{}) // project не задан

	t.Run("дефолт инстанса при пустых scope/scopes", func(t *testing.T) {
		got := alpha.resolveRecallScopes(recallInput{Query: "x"})
		want := []string{"project:alpha", "user:*"}
		if !slices.Equal(got, want) {
			t.Fatalf("resolveRecallScopes() = %v, want %v", got, want)
		}
	})

	t.Run("явный scopes перекрывает дефолт", func(t *testing.T) {
		got := alpha.resolveRecallScopes(recallInput{Query: "x", Scopes: []string{"project:beta"}})
		want := []string{"project:beta"}
		if !slices.Equal(got, want) {
			t.Fatalf("resolveRecallScopes() = %v, want %v", got, want)
		}
	})

	t.Run("явный одиночный scope даёт набор из одного элемента", func(t *testing.T) {
		got := alpha.resolveRecallScopes(recallInput{Query: "x", Scope: "agent:self"})
		want := []string{"agent:self"}
		if !slices.Equal(got, want) {
			t.Fatalf("resolveRecallScopes() = %v, want %v", got, want)
		}
	})

	t.Run("scopes перекрывает и scope, если оба заданы", func(t *testing.T) {
		got := alpha.resolveRecallScopes(recallInput{Query: "x", Scope: "agent:self", Scopes: []string{"project:beta", "user:*"}})
		want := []string{"project:beta", "user:*"}
		if !slices.Equal(got, want) {
			t.Fatalf("resolveRecallScopes() = %v, want %v", got, want)
		}
	})

	t.Run("adhoc-режим без project — без фильтра", func(t *testing.T) {
		got := adhoc.resolveRecallScopes(recallInput{Query: "x"})
		if len(got) != 0 {
			t.Fatalf("resolveRecallScopes() = %v, want пусто (без фильтра)", got)
		}
	})
}

// TestSaveDefaultScopeFromInstance проверяет, что memory_save без явного scope
// пишет запись в "домашний" scope инстанса (project:<name>), а при выключенной
// мультипроектности (project не задан) возвращает write.ErrEmptyScope.
func TestSaveDefaultScopeFromInstance(t *testing.T) {
	ctx := context.Background()

	t.Run("save без scope пишет в project:<name>", func(t *testing.T) {
		srv := newTestServerWithScope(t, config.ScopeConfig{Project: "alpha", Shared: []string{"user:*"}})

		res, _, err := srv.handleSave(ctx, nil, saveInput{Kind: "semantic", Content: "факт без явного scope"})
		if err != nil {
			t.Fatalf("handleSave: %v", err)
		}
		id := extractID(t, textOf(res))

		m, err := srv.store.Get(ctx, id)
		if err != nil {
			t.Fatalf("store.Get: %v", err)
		}
		if m.Scope != "project:alpha" {
			t.Fatalf("scope = %q, want %q", m.Scope, "project:alpha")
		}
	})

	t.Run("явный scope не переопределяется дефолтом", func(t *testing.T) {
		srv := newTestServerWithScope(t, config.ScopeConfig{Project: "alpha", Shared: []string{"user:*"}})

		res, _, err := srv.handleSave(ctx, nil, saveInput{Kind: "semantic", Content: "факт с явным scope", Scope: "user:*"})
		if err != nil {
			t.Fatalf("handleSave: %v", err)
		}
		id := extractID(t, textOf(res))

		m, err := srv.store.Get(ctx, id)
		if err != nil {
			t.Fatalf("store.Get: %v", err)
		}
		if m.Scope != "user:*" {
			t.Fatalf("scope = %q, want %q", m.Scope, "user:*")
		}
	})

	t.Run("save без scope при выключенной мультипроектности — ошибка", func(t *testing.T) {
		srv := newTestServerWithScope(t, config.ScopeConfig{}) // project не задан

		_, _, err := srv.handleSave(ctx, nil, saveInput{Kind: "semantic", Content: "факт без scope, без дефолта"})
		if !errors.Is(err, write.ErrEmptyScope) {
			t.Fatalf("err = %v, want errors.Is(err, write.ErrEmptyScope)", err)
		}
	})
}

// TestScopeIsolationE2E — сквозная проверка критерия приёмки MP-2 (DESIGN.md §2.1):
// два инстанса memoryd с разными project над ОДНИМ файлом БД. Каждый видит через
// memory_recall свой project:<name> и общий user:*, но не чужой project:*. Прогоняет
// весь read-path целиком (handleRecall → Recaller.Recall → store), а не только
// разрешение scope в изоляции.
func TestScopeIsolationE2E(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "shared.db")

	alpha := newServerOnDB(t, dbPath, config.ScopeConfig{Project: "alpha", Shared: []string{"user:*"}})
	beta := newServerOnDB(t, dbPath, config.ScopeConfig{Project: "beta", Shared: []string{"user:*"}})

	// Наполняем общий файл: по факту в каждый проектный scope + один общий user:*.
	// Контент с уникальными маркерами, чтобы однозначно опознать записи в брифе.
	saveVia := func(srv *Server, in saveInput) {
		t.Helper()
		if _, _, err := srv.handleSave(ctx, nil, in); err != nil {
			t.Fatalf("handleSave(%q): %v", in.Content, err)
		}
	}
	saveVia(alpha, saveInput{Kind: "semantic", Content: "МАРКЕРАЛЬФА проектный факт alpha"})              // → project:alpha
	saveVia(beta, saveInput{Kind: "semantic", Content: "МАРКЕРБЕТА проектный факт beta"})                 // → project:beta
	saveVia(alpha, saveInput{Kind: "semantic", Content: "МАРКЕРЮЗЕР общий факт", Scope: "user:*"})        // → user:*

	recallVia := func(srv *Server) string {
		t.Helper()
		res, _, err := srv.handleRecall(ctx, nil, recallInput{Query: "МАРКЕРАЛЬФА МАРКЕРБЕТА МАРКЕРЮЗЕР факт"})
		if err != nil {
			t.Fatalf("handleRecall: %v", err)
		}
		return textOf(res)
	}

	alphaBrief := recallVia(alpha)
	if !strings.Contains(alphaBrief, "МАРКЕРАЛЬФА") {
		t.Errorf("recall alpha не увидел свой project:alpha; бриф:\n%s", alphaBrief)
	}
	if !strings.Contains(alphaBrief, "МАРКЕРЮЗЕР") {
		t.Errorf("recall alpha не увидел общий user:*; бриф:\n%s", alphaBrief)
	}
	if strings.Contains(alphaBrief, "МАРКЕРБЕТА") {
		t.Errorf("recall alpha увидел чужой project:beta (утечка изоляции); бриф:\n%s", alphaBrief)
	}

	betaBrief := recallVia(beta)
	if !strings.Contains(betaBrief, "МАРКЕРБЕТА") {
		t.Errorf("recall beta не увидел свой project:beta; бриф:\n%s", betaBrief)
	}
	if !strings.Contains(betaBrief, "МАРКЕРЮЗЕР") {
		t.Errorf("recall beta не увидел общий user:*; бриф:\n%s", betaBrief)
	}
	if strings.Contains(betaBrief, "МАРКЕРАЛЬФА") {
		t.Errorf("recall beta увидел чужой project:alpha (утечка изоляции); бриф:\n%s", betaBrief)
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
