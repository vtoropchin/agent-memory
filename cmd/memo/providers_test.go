package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchOllamaModels(t *testing.T) {
	t.Run("успех", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/tags" {
				t.Errorf("путь = %q, хочу /api/tags", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"bge-m3"},{"name":"nomic-embed-text"}]}`))
		}))
		defer srv.Close()

		got, err := fetchOllamaModels(context.Background(), srv.URL)
		if err != nil {
			t.Fatalf("fetchOllamaModels: %v", err)
		}
		want := []string{"bge-m3", "nomic-embed-text"}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("fetchOllamaModels = %v, хочу %v", got, want)
		}
	})

	t.Run("пустой список", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[]}`))
		}))
		defer srv.Close()

		got, err := fetchOllamaModels(context.Background(), srv.URL)
		if err != nil {
			t.Fatalf("fetchOllamaModels: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("fetchOllamaModels = %v, хочу пусто", got)
		}
	})

	t.Run("сервер недоступен — ошибка", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		srv.Close() // сразу закрыт — соединение не устанавливается

		if _, err := fetchOllamaModels(context.Background(), srv.URL); err == nil {
			t.Fatalf("fetchOllamaModels(недоступный сервер) = nil error, хочу ошибку")
		}
	})

	t.Run("не-200 статус — ошибка", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("boom"))
		}))
		defer srv.Close()

		_, err := fetchOllamaModels(context.Background(), srv.URL)
		if err == nil {
			t.Fatalf("fetchOllamaModels(500) = nil error, хочу ошибку")
		}
		if !strings.Contains(err.Error(), "500") {
			t.Errorf("ошибка не содержит статус-код: %v", err)
		}
	})

	t.Run("битый JSON — ошибка разбора", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("не json"))
		}))
		defer srv.Close()

		if _, err := fetchOllamaModels(context.Background(), srv.URL); err == nil {
			t.Fatalf("fetchOllamaModels(битый JSON) = nil error, хочу ошибку")
		}
	})

	t.Run("модели без name пропускаются", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":""},{"name":"bge-m3"}]}`))
		}))
		defer srv.Close()

		got, err := fetchOllamaModels(context.Background(), srv.URL)
		if err != nil {
			t.Fatalf("fetchOllamaModels: %v", err)
		}
		if len(got) != 1 || got[0] != "bge-m3" {
			t.Errorf("fetchOllamaModels = %v, хочу [bge-m3]", got)
		}
	})
}

func TestFetchOpenAIEmbeddingModels(t *testing.T) {
	t.Run("фильтр по embedding", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/models" {
				t.Errorf("путь = %q, хочу /v1/models", r.URL.Path)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
				t.Errorf("Authorization = %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[
				{"id":"gpt-4o"},
				{"id":"text-embedding-3-small"},
				{"id":"text-embedding-3-large"}
			]}`))
		}))
		defer srv.Close()

		got, err := fetchOpenAIEmbeddingModels(context.Background(), srv.URL, "sk-test")
		if err != nil {
			t.Fatalf("fetchOpenAIEmbeddingModels: %v", err)
		}
		want := []string{"text-embedding-3-small", "text-embedding-3-large"}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("fetchOpenAIEmbeddingModels = %v, хочу %v", got, want)
		}
	})

	t.Run("пустой фильтр — фолбэк на все id", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o"},{"id":"gpt-3.5-turbo"}]}`))
		}))
		defer srv.Close()

		got, err := fetchOpenAIEmbeddingModels(context.Background(), srv.URL, "sk-test")
		if err != nil {
			t.Fatalf("fetchOpenAIEmbeddingModels: %v", err)
		}
		want := []string{"gpt-4o", "gpt-3.5-turbo"}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("fetchOpenAIEmbeddingModels (фолбэк) = %v, хочу %v", got, want)
		}
	})

	t.Run("неверный ключ — ошибка", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
		}))
		defer srv.Close()

		_, err := fetchOpenAIEmbeddingModels(context.Background(), srv.URL, "sk-bogus")
		if err == nil {
			t.Fatalf("fetchOpenAIEmbeddingModels(неверный ключ) = nil error, хочу ошибку")
		}
		if !strings.Contains(err.Error(), "401") {
			t.Errorf("ошибка не содержит статус-код 401: %v", err)
		}
	})

	t.Run("сервер недоступен — ошибка", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		srv.Close()

		if _, err := fetchOpenAIEmbeddingModels(context.Background(), srv.URL, "sk-test"); err == nil {
			t.Fatalf("fetchOpenAIEmbeddingModels(недоступный сервер) = nil error, хочу ошибку")
		}
	})
}
