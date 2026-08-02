package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"agentmemory/internal/config"
	"agentmemory/internal/memory"
	"agentmemory/internal/memory/embed"
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
	s, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	w := write.New(s, embed.NewNoop())
	r := recall.New(s, config.Default().Recall, reason.NewNoop(), embed.NewNoop())
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

// TestSaveAndRecallDebtKind проверяет открытие kind="debt" на MCP-поверхности
// (сущность техдолга, подзадача B): memory_save принимает kind=debt и запись
// затем видна в memory_recall — тот же путь, что и для остальных трёх Kind.
func TestSaveAndRecallDebtKind(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)

	res, _, err := srv.handleSave(ctx, nil, saveInput{
		Kind:       "debt",
		Content:    "internal/memory/store: SQLite-соединение не переживает конкурентный doctor+migrate — отложено до задачи миграций v2.",
		Scope:      "project:agentmemory",
		Importance: 4,
		Tags:       []string{"pkg:store"},
	})
	if err != nil {
		t.Fatalf("handleSave (kind=debt): %v", err)
	}
	if !strings.Contains(textOf(res), "Сохранено:") {
		t.Fatalf("handleSave (kind=debt): неожиданный результат %q", textOf(res))
	}
	id := extractID(t, textOf(res))

	recallRes, _, err := srv.handleRecall(ctx, nil, recallInput{
		Query: "техдолг SQLite doctor migrate",
		Scope: "project:agentmemory",
		Kinds: []string{"debt"},
	})
	if err != nil {
		t.Fatalf("handleRecall (kinds=[debt]): %v", err)
	}
	if !strings.Contains(textOf(recallRes), "doctor+migrate") {
		t.Fatalf("бриф не содержит сохранённую запись техдолга:\n%s", textOf(recallRes))
	}

	// Закрытие долга — существующим механизмом forget+replaced_by (без новых
	// инструментов/полей): запись о фиксе, затем memory_forget(id, replaced_by).
	fixRes, _, err := srv.handleSave(ctx, nil, saveInput{
		Kind:    "semantic",
		Content: "internal/memory/store: конкурентный doctor+migrate исправлен — соединение открывается заново под мьютексом.",
		Scope:   "project:agentmemory",
	})
	if err != nil {
		t.Fatalf("handleSave (запись-фикс): %v", err)
	}
	fixID := extractID(t, textOf(fixRes))

	if _, _, err := srv.handleForget(ctx, nil, forgetInput{ID: id, ReplacedBy: fixID}); err != nil {
		t.Fatalf("handleForget (закрытие долга replaced_by): %v", err)
	}

	after, _, err := srv.handleRecall(ctx, nil, recallInput{
		Query: "техдолг SQLite doctor migrate",
		Scope: "project:agentmemory",
		Kinds: []string{"debt"},
	})
	if err != nil {
		t.Fatalf("handleRecall (после закрытия долга): %v", err)
	}
	if strings.Contains(textOf(after), "doctor+migrate") {
		t.Fatalf("закрытая запись техдолга не должна быть видна в recall:\n%s", textOf(after))
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
// два инстанса memo с разными project над ОДНИМ файлом БД. Каждый видит через
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
	saveVia(alpha, saveInput{Kind: "semantic", Content: "МАРКЕРАЛЬФА проектный факт alpha"})       // → project:alpha
	saveVia(beta, saveInput{Kind: "semantic", Content: "МАРКЕРБЕТА проектный факт beta"})          // → project:beta
	saveVia(alpha, saveInput{Kind: "semantic", Content: "МАРКЕРЮЗЕР общий факт", Scope: "user:*"}) // → user:*

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

// TestHandleRecall_ExplainReachesQuery проверяет часть B задачи 8.1: поле
// recallInput.Explain доезжает до memory.Query.Explain — handleRecall с
// explain=true возвращает бриф с explain-довеском ("· via ..."), а с explain=false
// (или не переданным вовсе) — обычный бриф без него.
func TestHandleRecall_ExplainReachesQuery(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)

	if _, _, err := srv.handleSave(ctx, nil, saveInput{
		Kind: "semantic", Content: "МАРКЕРEXPLAIN факт для проверки MCP-поля explain", Scope: "project:agentmemory",
	}); err != nil {
		t.Fatalf("handleSave: %v", err)
	}

	withExplain, _, err := srv.handleRecall(ctx, nil, recallInput{
		Query: "МАРКЕРEXPLAIN факт", Scope: "project:agentmemory", Explain: true,
	})
	if err != nil {
		t.Fatalf("handleRecall (explain=true): %v", err)
	}
	mdExplain := textOf(withExplain)
	if !strings.Contains(mdExplain, "· via") {
		t.Errorf("explain=true должен добавить трассу ранжирования в бриф:\n%s", mdExplain)
	}

	withoutExplain, _, err := srv.handleRecall(ctx, nil, recallInput{
		Query: "МАРКЕРEXPLAIN факт", Scope: "project:agentmemory",
	})
	if err != nil {
		t.Fatalf("handleRecall (без explain): %v", err)
	}
	mdNoExplain := textOf(withoutExplain)
	if strings.Contains(mdNoExplain, "· via") {
		t.Errorf("без explain бриф не должен содержать трассу ранжирования:\n%s", mdNoExplain)
	}
}

// TestParseRecallDuration проверяет парсинг recallInput.CreatedAfter/CreatedBefore
// (задача 8.2): пустая строка выключает фильтр, валидная длительность даёт
// now-длительность, невалидный формат — внятную ошибку (а не панику).
func TestParseRecallDuration(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	t.Run("пустая строка — фильтр выключен", func(t *testing.T) {
		got, err := parseRecallDuration("", now)
		if err != nil {
			t.Fatalf("parseRecallDuration(\"\"): %v", err)
		}
		if got != 0 {
			t.Fatalf("parseRecallDuration(\"\") = %d, want 0", got)
		}
	})

	t.Run("валидная длительность — now минус длительность", func(t *testing.T) {
		got, err := parseRecallDuration("168h", now)
		if err != nil {
			t.Fatalf("parseRecallDuration(\"168h\"): %v", err)
		}
		want := now.Add(-168 * time.Hour).Unix()
		if got != want {
			t.Fatalf("parseRecallDuration(\"168h\") = %d, want %d", got, want)
		}
	})

	t.Run("невалидный формат — ошибка, не паника", func(t *testing.T) {
		_, err := parseRecallDuration("вчера", now)
		if err == nil {
			t.Fatal("parseRecallDuration(\"вчера\") должен вернуть ошибку")
		}
	})
}

// TestHandleRecall_InvalidCreatedAfterReturnsError проверяет, что невалидный
// created_after не роняет сервер, а возвращается внятной ошибкой инструмента.
func TestHandleRecall_InvalidCreatedAfterReturnsError(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)

	_, _, err := srv.handleRecall(ctx, nil, recallInput{
		Query: "x", Scope: "project:agentmemory", CreatedAfter: "не-длительность",
	})
	if err == nil {
		t.Fatal("handleRecall с невалидным created_after должен вернуть ошибку, получили nil")
	}
}

// TestHandleRecall_TagsAndDateFiltersReachQuery — часть B задачи 8.2: recallInput.Tags
// и CreatedAfter (относительная длительность) доезжают до memory.Query и реально
// сужают выдачу — запись должна пройти оба фильтра сразу, а не хотя бы один.
func TestHandleRecall_TagsAndDateFiltersReachQuery(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)

	saveWithTag := func(content string, tags []string) string {
		t.Helper()
		res, _, err := srv.handleSave(ctx, nil, saveInput{
			Kind: "semantic", Content: content, Scope: "project:agentmemory", Tags: tags,
		})
		if err != nil {
			t.Fatalf("handleSave(%q): %v", content, err)
		}
		return extractID(t, textOf(res))
	}
	setCreatedAt := func(id string, ts int64) {
		t.Helper()
		m, err := srv.store.Get(ctx, id)
		if err != nil {
			t.Fatalf("store.Get(%q): %v", id, err)
		}
		m.CreatedAt = ts
		if err := srv.store.Put(ctx, m); err != nil {
			t.Fatalf("store.Put(%q): %v", id, err)
		}
	}

	now := time.Now().Unix()
	matchID := saveWithTag("МАРКЕРFILTER подходит по тегу и по дате", []string{"стек"})
	wrongTagID := saveWithTag("МАРКЕРFILTER не подходит по тегу", []string{"другое"})
	wrongDateID := saveWithTag("МАРКЕРFILTER не подходит по дате", []string{"стек"})

	setCreatedAt(matchID, now-60)          // минута назад — свежая
	setCreatedAt(wrongTagID, now-60)       // свежая, но тег не тот
	setCreatedAt(wrongDateID, now-100_000) // больше суток назад

	res, _, err := srv.handleRecall(ctx, nil, recallInput{
		Query: "МАРКЕРFILTER", Scope: "project:agentmemory",
		Tags: []string{"стек"}, CreatedAfter: "1h",
	})
	if err != nil {
		t.Fatalf("handleRecall: %v", err)
	}
	md := textOf(res)
	if !strings.Contains(md, "подходит по тегу и по дате") {
		t.Errorf("бриф должен содержать запись, прошедшую оба фильтра:\n%s", md)
	}
	if strings.Contains(md, "не подходит по тегу") {
		t.Errorf("бриф не должен содержать запись с чужим тегом:\n%s", md)
	}
	if strings.Contains(md, "не подходит по дате") {
		t.Errorf("бриф не должен содержать устаревшую запись:\n%s", md)
	}
}

// TestHandleSaveLinksStringBackCompat проверяет обратную совместимость (задача
// 9.3, часть A): элемент links в виде строки-id ведёт себя так же, как до
// типизированных связей — rel=related. Разбор идёт через настоящий
// json.Unmarshal в saveInput (как это делает MCP SDK с сырыми аргументами
// вызова), а не через прямое конструирование linkEntry в Go.
func TestHandleSaveLinksStringBackCompat(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)

	targetRes, _, err := srv.handleSave(ctx, nil, saveInput{Kind: "semantic", Content: "цель для линка", Scope: "project:agentmemory"})
	if err != nil {
		t.Fatalf("handleSave (target): %v", err)
	}
	target := extractID(t, textOf(targetRes))

	in := unmarshalSaveInput(t, fmt.Sprintf(`{"kind":"semantic","content":"источник со строковым links","scope":"project:agentmemory","links":[%q]}`, target))

	res, _, err := srv.handleSave(ctx, nil, in)
	if err != nil {
		t.Fatalf("handleSave (src): %v", err)
	}
	srcID := extractID(t, textOf(res))

	m, err := srv.store.Get(ctx, srcID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if len(m.Links) != 1 || m.Links[0].Dst != target || m.Links[0].Rel != memory.RelRelated {
		t.Errorf("Links = %+v, want ровно 1 ссылку на %q с rel=related", m.Links, target)
	}
}

// TestHandleSaveLinksObjectFormReachesNeighbors проверяет часть A+B задачи 9.3:
// объектная форма {"id":..,"rel":"causes"} доезжает от сырого JSON аргумента
// memory_save до записи в БД, и типизированное ребро видно через Store.Neighbors
// (граф-обход) — то же, что смотрит web-морда на карточке записи.
func TestHandleSaveLinksObjectFormReachesNeighbors(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)

	targetRes, _, err := srv.handleSave(ctx, nil, saveInput{Kind: "semantic", Content: "первопричина", Scope: "project:agentmemory"})
	if err != nil {
		t.Fatalf("handleSave (target): %v", err)
	}
	target := extractID(t, textOf(targetRes))

	in := unmarshalSaveInput(t, fmt.Sprintf(`{"kind":"semantic","content":"следствие первопричины","scope":"project:agentmemory","links":[{"id":%q,"rel":"causes"}]}`, target))

	res, _, err := srv.handleSave(ctx, nil, in)
	if err != nil {
		t.Fatalf("handleSave (src): %v", err)
	}
	srcID := extractID(t, textOf(res))

	m, err := srv.store.Get(ctx, srcID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if len(m.Links) != 1 || m.Links[0].Dst != target || m.Links[0].Rel != memory.RelCauses {
		t.Fatalf("Links = %+v, want ровно 1 ссылку на %q с rel=causes", m.Links, target)
	}

	neighbors, err := srv.store.Neighbors(ctx, target)
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}
	if len(neighbors) != 1 || neighbors[0].Id != srcID {
		t.Fatalf("Neighbors(%q) = %+v, want содержать только %q", target, neighbors, srcID)
	}
}

// TestHandleSaveLinksMixedArray — один вызов memory_save со смешанным массивом
// links: строка (back-compat) вперемешку с объектом (типизированная форма) —
// оба должны разрешиться и сохраниться со своими rel.
func TestHandleSaveLinksMixedArray(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)

	targetA, _, err := srv.handleSave(ctx, nil, saveInput{Kind: "semantic", Content: "первая цель", Scope: "project:agentmemory"})
	if err != nil {
		t.Fatalf("handleSave (targetA): %v", err)
	}
	a := extractID(t, textOf(targetA))
	targetB, _, err := srv.handleSave(ctx, nil, saveInput{Kind: "semantic", Content: "вторая цель", Scope: "project:agentmemory"})
	if err != nil {
		t.Fatalf("handleSave (targetB): %v", err)
	}
	b := extractID(t, textOf(targetB))

	in := unmarshalSaveInput(t, fmt.Sprintf(`{"kind":"semantic","content":"источник со смешанным links","scope":"project:agentmemory","links":[%q,{"id":%q,"rel":"part_of"}]}`, a, b))

	res, _, err := srv.handleSave(ctx, nil, in)
	if err != nil {
		t.Fatalf("handleSave: %v", err)
	}
	srcID := extractID(t, textOf(res))

	m, err := srv.store.Get(ctx, srcID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if len(m.Links) != 2 {
		t.Fatalf("Links = %+v, want 2 ссылки", m.Links)
	}
	byDst := map[string]string{}
	for _, l := range m.Links {
		byDst[l.Dst] = l.Rel
	}
	if byDst[a] != memory.RelRelated {
		t.Errorf("Links[%q].Rel = %q, want related (строковая форма)", a, byDst[a])
	}
	if byDst[b] != memory.RelPartOf {
		t.Errorf("Links[%q].Rel = %q, want part_of (объектная форма)", b, byDst[b])
	}
}

// TestHandleSaveLinksInvalidRelReturnsError — опечатка в rel не откатывается
// молча в related, а возвращает ошибку инструмента с перечнем допустимых значений
// (fail-fast, DESIGN.md §12, задача 9.3).
func TestHandleSaveLinksInvalidRelReturnsError(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)

	in := unmarshalSaveInput(t, `{"kind":"semantic","content":"факт с опечаткой в rel","scope":"project:agentmemory","links":[{"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","rel":"casue"}]}`)

	_, _, err := srv.handleSave(ctx, nil, in)
	if err == nil {
		t.Fatal("ожидалась ошибка на неизвестный rel, получили nil")
	}
	for _, want := range []string{memory.RelRelated, memory.RelSupersedes, memory.RelCauses, memory.RelPartOf} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, ожидалось упоминание допустимого значения %q", err.Error(), want)
		}
	}
}

// TestHandleUpdateLinksAdditiveAndSelfLinkGuard — часть C задачи 9.3: memory_update
// принимает links в том же формате, что и memory_save, и добавляет их к уже
// существующим связям записи (существующие целы), self-петля отсекается
// существующим guard'ом write-пути.
func TestHandleUpdateLinksAdditiveAndSelfLinkGuard(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)

	oldRes, _, err := srv.handleSave(ctx, nil, saveInput{Kind: "semantic", Content: "старая запись", Scope: "project:agentmemory"})
	if err != nil {
		t.Fatalf("handleSave (old): %v", err)
	}
	oldID := extractID(t, textOf(oldRes))

	firstTarget, _, err := srv.handleSave(ctx, nil, saveInput{Kind: "semantic", Content: "первая связанная запись", Scope: "project:agentmemory"})
	if err != nil {
		t.Fatalf("handleSave (firstTarget): %v", err)
	}
	firstID := extractID(t, textOf(firstTarget))

	// Первый Update — добавляем ссылку rel=causes на firstID.
	firstIn := unmarshalUpdateInput(t, fmt.Sprintf(`{"id":%q,"links":[{"id":%q,"rel":"causes"}]}`, oldID, firstID))
	if _, _, err := srv.handleUpdate(ctx, nil, firstIn); err != nil {
		t.Fatalf("handleUpdate (первая ссылка): %v", err)
	}

	secondTarget, _, err := srv.handleSave(ctx, nil, saveInput{Kind: "semantic", Content: "вторая связанная запись", Scope: "project:agentmemory"})
	if err != nil {
		t.Fatalf("handleSave (secondTarget): %v", err)
	}
	secondID := extractID(t, textOf(secondTarget))

	// Второй Update — добавляем ссылку на secondID + self-петлю на oldID; первая
	// связь должна остаться целой, self-петля должна быть отсечена.
	secondIn := unmarshalUpdateInput(t, fmt.Sprintf(`{"id":%q,"links":[%q,{"id":%q,"rel":"part_of"}]}`, oldID, oldID, secondID))
	if _, _, err := srv.handleUpdate(ctx, nil, secondIn); err != nil {
		t.Fatalf("handleUpdate (вторая ссылка): %v", err)
	}

	m, err := srv.store.Get(ctx, oldID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if len(m.Links) != 2 {
		t.Fatalf("Links = %+v, want 2 ссылки (self-петля отсечена)", m.Links)
	}
	byDst := map[string]string{}
	for _, l := range m.Links {
		byDst[l.Dst] = l.Rel
	}
	if byDst[firstID] != memory.RelCauses {
		t.Errorf("Links[%q].Rel = %q, want causes (сохранено с первого Update)", firstID, byDst[firstID])
	}
	if byDst[secondID] != memory.RelPartOf {
		t.Errorf("Links[%q].Rel = %q, want part_of (добавлено вторым Update)", secondID, byDst[secondID])
	}
	if _, ok := byDst[oldID]; ok {
		t.Errorf("Links = %+v, self-петля на %q не должна была сохраниться", m.Links, oldID)
	}
}

// --- вспомогательные функции ---

// unmarshalSaveInput разбирает сырой JSON аргументов memory_save в saveInput —
// тот же путь, что использует MCP SDK для req.Params.Arguments, важно для links
// (linkEntry.UnmarshalJSON различает строковую и объектную форму по сырым байтам).
func unmarshalSaveInput(t *testing.T, raw string) saveInput {
	t.Helper()
	var in saveInput
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatalf("json.Unmarshal(saveInput) %s: %v", raw, err)
	}
	return in
}

// unmarshalUpdateInput — как unmarshalSaveInput, но для memory_update.
func unmarshalUpdateInput(t *testing.T, raw string) updateInput {
	t.Helper()
	var in updateInput
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatalf("json.Unmarshal(updateInput) %s: %v", raw, err)
	}
	return in
}

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
