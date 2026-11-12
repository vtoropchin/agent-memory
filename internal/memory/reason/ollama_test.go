package reason

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOllamaComplete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/chat" || r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected request: %s %s (authorization present: %t)", r.Method, r.URL.Path, r.Header.Get("Authorization") != "")
		}
		var request struct {
			Model    string        `json:"model"`
			Messages []chatMessage `json:"messages"`
			Think    *bool         `json:"think"`
			Stream   *bool         `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if request.Model != "qwen3.5:9b" || len(request.Messages) != 1 ||
			request.Messages[0] != (chatMessage{Role: "user", Content: "Сделай сводку"}) {
			t.Errorf("unexpected model or messages: %+v", request)
		}
		if request.Think == nil || *request.Think || request.Stream == nil || *request.Stream {
			t.Errorf("want explicit think:false and stream:false: %+v", request)
		}
		_, _ = w.Write([]byte(`{"message":{"content":"Готово","thinking":"скрытый текст"},"done":true}`))
	}))
	defer server.Close()

	client, err := NewOllama(server.URL+"/", "qwen3.5:9b")
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.Complete(context.Background(), "Сделай сводку")
	if err != nil || got != "Готово" {
		t.Fatalf("Complete() = %q, %v", got, err)
	}
}

func TestOllamaCompleteErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
		want string
	}{
		{"missing model", 404, `{"error":"model not found"}`, "404"},
		{"empty answer", 200, `{"message":{"thinking":"только рассуждения"},"done":true}`, "пустой"},
		{"streamed answer", 200, `{"message":{"content":"часть ответа"},"done":false}`, "незавершённый"},
		{"truncated answer", 200, `{"message":{"content":"незаконченный ответ"},"done":true,"done_reason":"length"}`, "length"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			client, err := NewOllama(server.URL, "test")
			if err != nil {
				t.Fatal(err)
			}
			got, err := client.Complete(context.Background(), "test")
			if got != "" || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Complete() = %q, %v; want error containing %q", got, err, tc.want)
			}
		})
	}
}

func TestOllamaDoesNotFollowRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Error("followed redirect and replayed the prompt")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, "/another-endpoint", http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	client, err := NewOllama(server.URL, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Complete(context.Background(), "test"); err == nil || !strings.Contains(err.Error(), "307") {
		t.Fatalf("Complete() error = %v, want HTTP 307", err)
	}
}
