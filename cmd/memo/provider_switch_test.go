package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"agentmemory/internal/config"
	"agentmemory/internal/memory"
	"agentmemory/internal/memory/embed"
	"agentmemory/internal/memory/maint"
	"agentmemory/internal/memory/store"
)

// TestProviderSwitchOllamaToOpenAITriggersReindex — демонстрирует задачу 8.4, п.4
// (e2e без живого ключа OpenAI): нет доступа ни к реальной ollama, ни к реальному
// OpenAI, поэтому оба провайдера подняты как httptest-серверы, но через реальные
// клиенты newEmbedder(...) создаёт embed.Ollama и embed.OpenAI — не заглушки. Смена
// embedder.provider в конфиге с "ollama" на "openai" (разные model/dim) обязана
// заставить maint.Reindexer пересоздать векторный индекс и перегнать существующие
// записи новой моделью — это уже общий путь смены модели (DESIGN.md §9), здесь
// только подтверждается, что оба реальных клиента (не только тестовые заглушки)
// заводятся через него одинаково.
func TestProviderSwitchOllamaToOpenAITriggersReindex(t *testing.T) {
	ctx := context.Background()

	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		vecs := make([][]float32, len(req.Input))
		for i := range req.Input {
			vecs[i] = []float32{1, 1, 1} // dim=3
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Embeddings [][]float32 `json:"embeddings"`
		}{Embeddings: vecs})
	}))
	defer ollamaSrv.Close()

	openaiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		var data []item
		for i := range req.Input {
			data = append(data, item{Embedding: []float32{2, 2, 2, 2}, Index: i}) // dim=4
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Data []item `json:"data"`
		}{Data: data})
	}))
	defer openaiSrv.Close()

	s, err := store.OpenSQLite(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()

	now := time.Now().Unix()
	m := memory.Memory{
		Id:        ulid.Make().String(),
		Kind:      memory.KindSemantic,
		Scope:     "project:x",
		Content:   "факт, записанный до смены провайдера",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.Put(ctx, m); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Шаг 1: embedder.provider = "ollama" — реальный embed.Ollama через фабрику.
	t.Setenv(openaiAPIKeyEnv, "")
	ollamaEmbedder, err := newEmbedder(ctx, config.EmbedderConfig{
		Provider: "ollama",
		BaseURL:  ollamaSrv.URL,
		Model:    "bge-m3",
	})
	if err != nil {
		t.Fatalf("newEmbedder(ollama): %v", err)
	}
	if _, ok := ollamaEmbedder.(*embed.Ollama); !ok {
		t.Fatalf("newEmbedder(ollama) вернул %T, хочу *embed.Ollama", ollamaEmbedder)
	}

	r1 := maint.NewReindexer(s, ollamaEmbedder)
	res1, err := r1.CheckAndReindex(ctx)
	if err != nil {
		t.Fatalf("CheckAndReindex (ollama): %v", err)
	}
	if !res1.Reindexed || res1.Embedded != 1 {
		t.Fatalf("CheckAndReindex (ollama) = %+v, хочу Reindexed=true Embedded=1 (первый прогон — пустой embed_meta)", res1)
	}

	model, dim, err := s.EmbedMeta(ctx)
	if err != nil {
		t.Fatalf("EmbedMeta после ollama: %v", err)
	}
	if model != ollamaEmbedder.Model() || dim != 3 {
		t.Fatalf("EmbedMeta после ollama = (%q, %d), хочу (%q, 3)", model, dim, ollamaEmbedder.Model())
	}

	// Шаг 2: конфиг переключается на embedder.provider = "openai" — та же фабрика
	// newEmbedder, реальный embed.OpenAI с другой model/dim.
	t.Setenv(openaiAPIKeyEnv, "sk-test")
	openaiEmbedder, err := newEmbedder(ctx, config.EmbedderConfig{
		Provider: "openai",
		BaseURL:  openaiSrv.URL,
		Model:    "text-embedding-3-small",
	})
	if err != nil {
		t.Fatalf("newEmbedder(openai): %v", err)
	}
	if _, ok := openaiEmbedder.(*embed.OpenAI); !ok {
		t.Fatalf("newEmbedder(openai) вернул %T, хочу *embed.OpenAI", openaiEmbedder)
	}

	r2 := maint.NewReindexer(s, openaiEmbedder)
	res2, err := r2.CheckAndReindex(ctx)
	if err != nil {
		t.Fatalf("CheckAndReindex (openai): %v", err)
	}
	if !res2.Reindexed {
		t.Fatalf("CheckAndReindex (openai) = %+v, хочу Reindexed=true (model/dim сменились ollama→openai)", res2)
	}
	if res2.Embedded != 1 {
		t.Fatalf("CheckAndReindex (openai) Embedded = %d, хочу 1 (существующая запись перегнана новой моделью)", res2.Embedded)
	}

	model, dim, err = s.EmbedMeta(ctx)
	if err != nil {
		t.Fatalf("EmbedMeta после openai: %v", err)
	}
	if model != openaiEmbedder.Model() || dim != 4 {
		t.Fatalf("EmbedMeta после openai = (%q, %d), хочу (%q, 4)", model, dim, openaiEmbedder.Model())
	}

	// recall вектором работает новой моделью: SearchVector находит перегнанную запись.
	cands, err := s.SearchVector(ctx, []float32{2, 2, 2, 2}, memory.Query{Limit: 10})
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	if len(cands) != 1 || cands[0].Memory.Id != m.Id {
		t.Fatalf("SearchVector = %+v, хочу единственную запись %q", cands, m.Id)
	}
}
