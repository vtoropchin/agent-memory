package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"agentmemory/internal/config"
	"agentmemory/internal/memory"
	"agentmemory/internal/memory/embed"
	"agentmemory/internal/memory/reason"
	"agentmemory/internal/memory/recall"
	"agentmemory/internal/memory/store"
	"agentmemory/internal/memory/write"
)

// Тесты контракта API новой web-морды (DESIGN.md §14 Этап 16, задача 16.1):
// providers/jobs в /api/stats, expiresBefore в /api/memories, правка
// content/summary через PATCH.

// newTestWebHandlerInstance — handler с явным описанием инстанса (провайдеры,
// домашний scope) и заданным Embedder; Writer собирается с тем же Embedder,
// что и в runWeb — правка content переэмбеддит запись.
func newTestWebHandlerInstance(s store.Store, embedder embed.Embedder, inst webInstance) http.Handler {
	w := write.New(s, embedder)
	rc := recall.New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: testRecallWeights()}, reason.NewNoop(), embedder)
	return newWebHandler(s, w, rc, embedder, inst)
}

func testInstance() webInstance {
	return webInstance{
		HomeScope:         "project:home",
		StoreDriver:       "sqlite",
		StoreTarget:       "/tmp/memory.db",
		EmbedderProvider:  "ollama",
		EmbedderModel:     "bge-m3",
		ReasonerProvider:  "deepseek",
		ReasonerModel:     "deepseek-chat",
		ReasonerAvailable: true,
	}
}

// TestHandleStats_ProvidersAndJobs — панель «Сервисы»: имена провайдеров из
// инстанса, доступность эмбеддера — по Dim() собранного экземпляра, очередь на
// эмбеддинг и версия схемы — из Store, отметка прогона джобов — null, пока
// планировщик не отметился.
func TestHandleStats_ProvidersAndJobs(t *testing.T) {
	ctx := context.Background()
	s := newTestWebStore(t)
	inst := testInstance()

	t.Run("noop-эмбеддер: недоступен, очередь пуста, jobs null", func(t *testing.T) {
		srv := httptest.NewServer(newTestWebHandlerInstance(s, embed.NewNoop(), inst))
		defer srv.Close()

		var got statsResponse
		getJSON(t, srv.URL+"/api/stats", &got)
		p := got.Providers
		if p.Store.Driver != "sqlite" || p.Store.Target != "/tmp/memory.db" || p.Store.Schema <= 0 {
			t.Errorf("providers.store = %+v, want sqlite, /tmp/memory.db, schema > 0", p.Store)
		}
		if p.Embedder.Provider != "ollama" || p.Embedder.Model != "bge-m3" || p.Embedder.Available || p.Embedder.Dim != 0 || p.Embedder.Pending != 0 {
			t.Errorf("providers.embedder = %+v, want ollama/bge-m3, available=false, dim=0, pending=0", p.Embedder)
		}
		if p.Reasoner.Provider != "deepseek" || p.Reasoner.Model != "deepseek-chat" || !p.Reasoner.Available {
			t.Errorf("providers.reasoner = %+v, want deepseek/deepseek-chat available", p.Reasoner)
		}
		if got.Jobs.LastRunAt != nil {
			t.Errorf("jobs.lastRunAt = %v, want null (прогонов не было)", *got.Jobs.LastRunAt)
		}
		if got.HomeScope != "project:home" {
			t.Errorf("homeScope = %q, want project:home", got.HomeScope)
		}
	})

	// Индекс есть, одна запись без вектора — очередь 1; планировщик отметился.
	if err := s.ResetVectorIndex(ctx, "fake", 3); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}
	putTestMemory(t, s, nil)
	if err := s.SetLastMaintRun(ctx, 777); err != nil {
		t.Fatalf("SetLastMaintRun: %v", err)
	}

	t.Run("живой эмбеддер: доступен, dim, очередь и отметка прогона", func(t *testing.T) {
		srv := httptest.NewServer(newTestWebHandlerInstance(s, &fakeEmbedder{dim: 3, vec: []float32{1, 0, 0}}, inst))
		defer srv.Close()

		var got statsResponse
		getJSON(t, srv.URL+"/api/stats", &got)
		if !got.Providers.Embedder.Available || got.Providers.Embedder.Dim != 3 {
			t.Errorf("embedder = %+v, want available dim=3", got.Providers.Embedder)
		}
		if got.Providers.Embedder.Pending != 1 {
			t.Errorf("embedder.pending = %d, want 1", got.Providers.Embedder.Pending)
		}
		if got.Jobs.LastRunAt == nil || *got.Jobs.LastRunAt != 777 {
			t.Errorf("jobs.lastRunAt = %v, want 777", got.Jobs.LastRunAt)
		}
	})
}

// TestHandleListMemories_ExpiresBefore — «Истекает скоро»: фильтр по сроку,
// ближайшие первыми; мусор в параметре — 400.
func TestHandleListMemories_ExpiresBefore(t *testing.T) {
	s := newTestWebStore(t)
	putTestMemory(t, s, func(m *memory.Memory) { m.ExpiresAt = 0 })
	later := putTestMemory(t, s, func(m *memory.Memory) { m.ExpiresAt = 300; m.CreatedAt = 2 })
	soon := putTestMemory(t, s, func(m *memory.Memory) { m.ExpiresAt = 100; m.CreatedAt = 1 })
	putTestMemory(t, s, func(m *memory.Memory) { m.ExpiresAt = 900 })

	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	var got memoriesResponse
	getJSON(t, srv.URL+"/api/memories?expiresBefore=500", &got)
	if got.Total != 2 || len(got.Items) != 2 || got.Items[0].Id != soon.Id || got.Items[1].Id != later.Id {
		t.Fatalf("expiresBefore=500: total=%d items=%+v; want [soon, later]", got.Total, got.Items)
	}

	resp := getJSON(t, srv.URL+"/api/memories?expiresBefore=abc", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expiresBefore=abc: status = %d, want 400", resp.StatusCode)
	}
}

// TestHandlePatchMemory_ContentAndSummary — инвариант §12 снят: content и
// summary правятся из браузера через тот же Writer.Update; пустой summary
// снимает заголовок, пустой content — 400; при живом эмбеддере запись
// переэмбеддится (вектор появляется в индексе).
func TestHandlePatchMemory_ContentAndSummary(t *testing.T) {
	ctx := context.Background()
	s := newTestWebStore(t)
	if err := s.ResetVectorIndex(ctx, "fake", 3); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}
	m := putTestMemory(t, s, func(m *memory.Memory) { s := "старый заголовок"; m.Summary = &s })

	srv := httptest.NewServer(newTestWebHandlerInstance(s, &fakeEmbedder{dim: 3, vec: []float32{0, 1, 0}}, testInstance()))
	defer srv.Close()
	url := srv.URL + "/api/memories/" + m.Id

	var got memoryDTO
	resp := patchJSON(t, url, `{"content":"новый текст записи","summary":"новый заголовок"}`, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH content/summary: status = %d, want 200", resp.StatusCode)
	}
	if got.Content != "новый текст записи" || got.Summary == nil || *got.Summary != "новый заголовок" {
		t.Fatalf("после PATCH: content=%q summary=%v", got.Content, got.Summary)
	}
	vs, err := s.VectorStatus(ctx, m.Id)
	if err != nil {
		t.Fatalf("VectorStatus: %v", err)
	}
	if !vs.Embedded {
		t.Errorf("после правки content вектор не пересчитан (Embedded=false) — Writer морды должен эмбеддить")
	}

	resp = patchJSON(t, url, `{"summary":""}`, &got)
	if resp.StatusCode != http.StatusOK || got.Summary != nil {
		t.Fatalf("PATCH summary:\"\": status=%d summary=%v; want 200 и null", resp.StatusCode, got.Summary)
	}
	if got.Content != "новый текст записи" {
		t.Errorf("частичный PATCH summary тронул content: %q", got.Content)
	}

	resp = patchJSON(t, url, `{"content":"   "}`, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("PATCH пустой content: status = %d, want 400", resp.StatusCode)
	}
}

// TestNewWebInstance_NoopHidesModel — у выключенного провайдера нет модели,
// даже если конфиг подставил дефолт; пустой driver — sqlite.
func TestNewWebInstance_NoopHidesModel(t *testing.T) {
	cfg := config.Default()
	cfg.Embedder.Provider = ""
	cfg.Embedder.Model = "bge-m3"
	cfg.Reasoner.Provider = "deepseek"
	cfg.Reasoner.Model = "deepseek-chat"
	cfg.Store.Driver = ""
	cfg.Store.Path = "/x/memory.db"

	inst := newWebInstance(cfg, true)
	if inst.EmbedderProvider != "noop" || inst.EmbedderModel != "" {
		t.Errorf("embedder = %q/%q, want noop/\"\"", inst.EmbedderProvider, inst.EmbedderModel)
	}
	if inst.ReasonerProvider != "deepseek" || inst.ReasonerModel != "deepseek-chat" || !inst.ReasonerAvailable {
		t.Errorf("reasoner = %+v, want deepseek/deepseek-chat available", inst)
	}
	if inst.StoreDriver != "sqlite" || inst.StoreTarget != "/x/memory.db" {
		t.Errorf("store = %q/%q, want sqlite//x/memory.db", inst.StoreDriver, inst.StoreTarget)
	}
}

// TestReasonerAvailable — правило «Noop = недоступен» живёт у фабрики.
func TestReasonerAvailable(t *testing.T) {
	if reasonerAvailable(reason.NewNoop()) {
		t.Error("Noop должен считаться недоступным")
	}
	ds, err := reason.NewDeepSeek("key", "model")
	if err != nil {
		t.Fatalf("NewDeepSeek: %v", err)
	}
	if !reasonerAvailable(ds) {
		t.Error("DeepSeek должен считаться доступным")
	}
}
