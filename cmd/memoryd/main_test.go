package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"agentmemory/internal/config"
	"agentmemory/internal/memory/embed"
	"agentmemory/internal/memory/reason"
)

func TestNewReasoner(t *testing.T) {
	tests := []struct {
		name    string
		cfg     config.ReasonerConfig
		apiKey  string
		wantT   string // "%T" ожидаемого типа Reasoner
		wantErr bool
	}{
		{
			name:  "noop",
			cfg:   config.ReasonerConfig{Provider: "noop", Model: "deepseek-chat"},
			wantT: "*reason.Noop",
		},
		{
			name:   "deepseek без ключа деградирует до noop",
			cfg:    config.ReasonerConfig{Provider: "deepseek", Model: "deepseek-chat"},
			apiKey: "",
			wantT:  "*reason.Noop",
		},
		{
			name:   "deepseek с ключом",
			cfg:    config.ReasonerConfig{Provider: "deepseek", Model: "deepseek-chat"},
			apiKey: "sk-test",
			wantT:  "*reason.DeepSeek",
		},
		{
			name:   "deepseek с ключом, но пустой model деградирует до noop",
			cfg:    config.ReasonerConfig{Provider: "deepseek", Model: ""},
			apiKey: "sk-test",
			wantT:  "*reason.Noop",
		},
		{
			name:    "неизвестный provider — ошибка",
			cfg:     config.ReasonerConfig{Provider: "openai", Model: "gpt-4"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(deepseekAPIKeyEnv, tt.apiKey)

			got, err := newReasoner(tt.cfg)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("newReasoner(%+v) = %v, nil; want error", tt.cfg, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("newReasoner(%+v) вернул ошибку: %v", tt.cfg, err)
			}
			if gotT := fmt.Sprintf("%T", got); gotT != tt.wantT {
				t.Fatalf("newReasoner(%+v) вернул тип %s, хотим %s", tt.cfg, gotT, tt.wantT)
			}
			var _ reason.Reasoner = got
		})
	}
}

func TestNewEmbedder(t *testing.T) {
	// closedSrv — httptest-сервер, немедленно закрытый: имитирует недоступный ollama
	// (тот же приём, что в embed.TestNewOllama_ServerUnavailable).
	closedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedSrv.Close()

	tests := []struct {
		name    string
		cfg     config.EmbedderConfig
		wantT   string
		wantErr bool
	}{
		{
			name:  "noop",
			cfg:   config.EmbedderConfig{Provider: "noop", Model: "bge-m3"},
			wantT: "*embed.Noop",
		},
		{
			name:  "пустой provider — noop",
			cfg:   config.EmbedderConfig{Provider: "", Model: "bge-m3"},
			wantT: "*embed.Noop",
		},
		{
			name:  "ollama недоступен деградирует до noop",
			cfg:   config.EmbedderConfig{Provider: "ollama", BaseURL: closedSrv.URL, Model: "bge-m3"},
			wantT: "*embed.Noop",
		},
		{
			name:    "неизвестный provider — ошибка",
			cfg:     config.EmbedderConfig{Provider: "openai", Model: "bge-m3"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := newEmbedder(context.Background(), tt.cfg)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("newEmbedder(%+v) = %v, nil; want error", tt.cfg, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("newEmbedder(%+v) вернул ошибку: %v", tt.cfg, err)
			}
			if gotT := fmt.Sprintf("%T", got); gotT != tt.wantT {
				t.Fatalf("newEmbedder(%+v) вернул тип %s, хотим %s", tt.cfg, gotT, tt.wantT)
			}
			var _ embed.Embedder = got
		})
	}
}

func TestNewEmbedder_OllamaAvailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"embeddings":[[0.1,0.2,0.3]]}`))
	}))
	defer srv.Close()

	got, err := newEmbedder(context.Background(), config.EmbedderConfig{Provider: "ollama", BaseURL: srv.URL, Model: "bge-m3"})
	if err != nil {
		t.Fatalf("newEmbedder: %v", err)
	}
	if gotT := fmt.Sprintf("%T", got); gotT != "*embed.Ollama" {
		t.Fatalf("newEmbedder вернул тип %s, хочу *embed.Ollama", gotT)
	}
	if got.Dim() != 3 {
		t.Errorf("Dim() = %d, хочу 3", got.Dim())
	}
}
