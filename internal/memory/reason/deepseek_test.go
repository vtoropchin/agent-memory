package reason

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDeepSeek_Complete_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("метод = %s, хочу POST", r.Method)
		}
		if r.URL.Path != "/chat/completions" {
			t.Errorf("путь = %s, хочу /chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}

		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("декодирование запроса: %v", err)
		}
		if req.Model != "deepseek-chat" {
			t.Errorf("model = %q", req.Model)
		}
		if len(req.Messages) != 1 || req.Messages[0].Role != "user" || req.Messages[0].Content != "привет" {
			t.Errorf("messages = %+v", req.Messages)
		}
		if req.Stream {
			t.Error("stream должен быть false")
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ответ модели"}}]}`))
	}))
	defer srv.Close()

	d, err := NewDeepSeek("test-key", "deepseek-chat", WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewDeepSeek: %v", err)
	}

	got, err := d.Complete(context.Background(), "привет")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got != "ответ модели" {
		t.Errorf("Complete() = %q, хочу %q", got, "ответ модели")
	}
}

func TestDeepSeek_Complete_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	}))
	defer srv.Close()

	d, err := NewDeepSeek("bad-key", "deepseek-chat", WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewDeepSeek: %v", err)
	}

	_, err = d.Complete(context.Background(), "привет")
	if err == nil {
		t.Fatal("ожидал ошибку при не-2xx статусе")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "invalid api key") {
		t.Errorf("текст ошибки не содержит статус/тело: %v", err)
	}
}

func TestDeepSeek_Complete_EmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()

	d, err := NewDeepSeek("test-key", "deepseek-chat", WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewDeepSeek: %v", err)
	}

	_, err = d.Complete(context.Background(), "привет")
	if err == nil {
		t.Fatal("ожидал ошибку при пустых choices")
	}
	if !strings.Contains(err.Error(), "пустой список choices") {
		t.Errorf("текст ошибки: %v", err)
	}
}

func TestDeepSeek_Complete_ContextCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"поздно"}}]}`))
	}))
	defer srv.Close()

	d, err := NewDeepSeek("test-key", "deepseek-chat", WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewDeepSeek: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = d.Complete(ctx, "привет")
	if err == nil {
		t.Fatal("ожидал ошибку при отменённом контексте")
	}
}

func TestNewDeepSeek_EmptyAPIKey(t *testing.T) {
	_, err := NewDeepSeek("", "deepseek-chat")
	if err == nil {
		t.Fatal("ожидал ошибку при пустом apiKey")
	}
}

func TestNewDeepSeek_EmptyModel(t *testing.T) {
	_, err := NewDeepSeek("test-key", "")
	if err == nil {
		t.Fatal("ожидал ошибку при пустом model")
	}
}
