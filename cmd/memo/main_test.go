package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentmemory/internal/config"
	"agentmemory/internal/memory/embed"
	"agentmemory/internal/memory/reason"
)

func TestWriteUsage(t *testing.T) {
	var buf bytes.Buffer
	writeUsage(&buf)
	out := buf.String()

	wantSubstrings := []string{
		"memo init",
		"memo web",
		"-config",
		"-db",
		"DEEPSEEK_API_KEY",
		"stdio",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(out, want) {
			t.Errorf("writeUsage() вывод не содержит %q; вывод:\n%s", want, out)
		}
	}
}

func TestIsHelpArg(t *testing.T) {
	tests := []struct {
		arg  string
		want bool
	}{
		{"-h", true},
		{"-help", true},
		{"--help", true},
		{"help", true},
		{"init", false},
		{"web", false},
		{"-config", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isHelpArg(tt.arg); got != tt.want {
			t.Errorf("isHelpArg(%q) = %v, хочу %v", tt.arg, got, tt.want)
		}
	}
}

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
		apiKey  string
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
			cfg:     config.EmbedderConfig{Provider: "bogus", Model: "bge-m3"},
			wantErr: true,
		},
		{
			name:   "openai без ключа деградирует до noop",
			cfg:    config.EmbedderConfig{Provider: "openai", Model: "text-embedding-3-small"},
			apiKey: "",
			wantT:  "*embed.Noop",
		},
		{
			name:   "openai недоступен деградирует до noop",
			cfg:    config.EmbedderConfig{Provider: "openai", BaseURL: closedSrv.URL, Model: "text-embedding-3-small"},
			apiKey: "sk-test",
			wantT:  "*embed.Noop",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(openaiAPIKeyEnv, tt.apiKey)
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

func TestNewEmbedder_OpenAIAvailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization = %q, хочу %q", got, "Bearer sk-test")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3],"index":0}]}`))
	}))
	defer srv.Close()

	t.Setenv(openaiAPIKeyEnv, "sk-test")

	got, err := newEmbedder(context.Background(), config.EmbedderConfig{Provider: "openai", BaseURL: srv.URL, Model: "text-embedding-3-small"})
	if err != nil {
		t.Fatalf("newEmbedder: %v", err)
	}
	if gotT := fmt.Sprintf("%T", got); gotT != "*embed.OpenAI" {
		t.Fatalf("newEmbedder вернул тип %s, хочу *embed.OpenAI", gotT)
	}
	if got.Dim() != 3 {
		t.Errorf("Dim() = %d, хочу 3", got.Dim())
	}
}
