package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestClassifyOllamaEmbedModels(t *testing.T) {
	tests := []struct {
		name      string
		installed []string
		want      []string
	}{
		{
			name:      "пусто",
			installed: nil,
			want:      nil,
		},
		{
			name:      "только чат-модели отсеиваются",
			installed: []string{"llama3", "mistral:7b"},
			want:      nil,
		},
		{
			name:      "распознаёт без тега",
			installed: []string{"bge-m3"},
			want:      []string{"bge-m3"},
		},
		{
			name:      "распознаёт с тегом",
			installed: []string{"bge-m3:latest", "nomic-embed-text:v1.5"},
			want:      []string{"bge-m3:latest", "nomic-embed-text:v1.5"},
		},
		{
			name:      "префикс snowflake-arctic-embed2",
			installed: []string{"snowflake-arctic-embed2:latest"},
			want:      []string{"snowflake-arctic-embed2:latest"},
		},
		{
			name:      "рекомендованные первыми, независимо от порядка installed",
			installed: []string{"mxbai-embed-large", "llama3", "bge-m3", "nomic-embed-text"},
			want:      []string{"bge-m3", "nomic-embed-text", "mxbai-embed-large"},
		},
		{
			name:      "нерекомендованные распознанные — следом, в порядке installed",
			installed: []string{"granite-embedding", "bge-m3", "all-minilm"},
			want:      []string{"bge-m3", "granite-embedding", "all-minilm"},
		},
		{
			name:      "смесь чат- и embedding-моделей",
			installed: []string{"llama3:8b", "bge-large", "mistral", "qwen3-embedding"},
			want:      []string{"bge-large", "qwen3-embedding"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyOllamaEmbedModels(tt.installed)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("classifyOllamaEmbedModels(%v) = %v, хочу %v", tt.installed, got, tt.want)
			}
		})
	}
}

func TestOllamaInstallInstructions(t *testing.T) {
	tests := []struct {
		goos      string
		substring string
	}{
		{"darwin", "brew install ollama"},
		{"linux", "curl -fsSL https://ollama.com/install.sh"},
		{"windows", "ollama.com/download/windows"},
		{"plan9", "ollama.com/download"},
	}
	for _, tt := range tests {
		t.Run(tt.goos, func(t *testing.T) {
			got := ollamaInstallInstructions(tt.goos)
			if !strings.Contains(got, tt.substring) {
				t.Errorf("ollamaInstallInstructions(%q) = %q, хочу подстроку %q", tt.goos, got, tt.substring)
			}
		})
	}
}

func TestOllamaStartInstructions(t *testing.T) {
	tests := []struct {
		goos      string
		substring string
	}{
		{"darwin", "ollama serve"},
		{"windows", "ollama serve"},
		{"linux", "systemctl"},
		{"plan9", "ollama serve"},
	}
	for _, tt := range tests {
		t.Run(tt.goos, func(t *testing.T) {
			got := ollamaStartInstructions(tt.goos)
			if !strings.Contains(got, tt.substring) {
				t.Errorf("ollamaStartInstructions(%q) = %q, хочу подстроку %q", tt.goos, got, tt.substring)
			}
		})
	}
}

func TestResolveOllamaEmbedState(t *testing.T) {
	lookPathFound := func(string) (string, error) { return "/usr/local/bin/ollama", nil }
	lookPathNotFound := func(string) (string, error) { return "", exec.ErrNotFound }

	t.Run("happy — есть распознанная embedding-модель", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"llama3"},{"name":"bge-m3"}]}`))
		}))
		defer srv.Close()

		state, models, err := resolveOllamaEmbedState(context.Background(), srv.URL, lookPathNotFound)
		if err != nil {
			t.Fatalf("err = %v, хочу nil", err)
		}
		if state != ollamaStateHappy {
			t.Fatalf("state = %v, хочу ollamaStateHappy", state)
		}
		if !reflect.DeepEqual(models, []string{"bge-m3"}) {
			t.Errorf("models = %v, хочу [bge-m3]", models)
		}
	})

	t.Run("no-models — сервер отвечает, только чат-модели", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"llama3"}]}`))
		}))
		defer srv.Close()

		state, models, err := resolveOllamaEmbedState(context.Background(), srv.URL, lookPathFound)
		if err != nil {
			t.Fatalf("err = %v, хочу nil", err)
		}
		if state != ollamaStateNoModels {
			t.Fatalf("state = %v, хочу ollamaStateNoModels", state)
		}
		if !reflect.DeepEqual(models, []string{"llama3"}) {
			t.Errorf("models = %v, хочу [llama3]", models)
		}
	})

	t.Run("no-models — сервер отвечает, моделей нет вовсе", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[]}`))
		}))
		defer srv.Close()

		state, models, err := resolveOllamaEmbedState(context.Background(), srv.URL, lookPathFound)
		if err != nil {
			t.Fatalf("err = %v, хочу nil", err)
		}
		if state != ollamaStateNoModels {
			t.Fatalf("state = %v, хочу ollamaStateNoModels", state)
		}
		if len(models) != 0 {
			t.Errorf("models = %v, хочу пусто", models)
		}
	})

	t.Run("not-running — сервер недоступен, бинарь найден", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		srv.Close() // сразу закрыт — соединение не устанавливается

		state, models, err := resolveOllamaEmbedState(context.Background(), srv.URL, lookPathFound)
		if err == nil {
			t.Fatal("err = nil, хочу ошибку соединения")
		}
		if state != ollamaStateNotRunning {
			t.Fatalf("state = %v, хочу ollamaStateNotRunning", state)
		}
		if models != nil {
			t.Errorf("models = %v, хочу nil", models)
		}
	})

	t.Run("not-installed — сервер недоступен, бинаря нет", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		srv.Close()

		state, models, err := resolveOllamaEmbedState(context.Background(), srv.URL, lookPathNotFound)
		if err == nil {
			t.Fatal("err = nil, хочу ошибку соединения")
		}
		if state != ollamaStateNotInstalled {
			t.Fatalf("state = %v, хочу ollamaStateNotInstalled", state)
		}
		if models != nil {
			t.Errorf("models = %v, хочу nil", models)
		}
	})
}
