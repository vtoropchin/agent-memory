package embed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// embedHandler строит httptest-хендлер `/api/embed`, возвращающий по одному
// детерминированному вектору dim-элементов на каждый вход, и считает число вызовов.
func embedHandler(t *testing.T, dim int, calls *int32) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("метод = %s, хочу POST", r.Method)
		}
		if r.URL.Path != "/api/embed" {
			t.Errorf("путь = %s, хочу /api/embed", r.URL.Path)
		}

		var req embedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("декодирование запроса: %v", err)
		}
		atomic.AddInt32(calls, 1)

		vecs := make([][]float32, len(req.Input))
		for i := range req.Input {
			v := make([]float32, dim)
			for j := range v {
				v[j] = float32(i + j)
			}
			vecs[i] = v
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(embedResponse{Embeddings: vecs})
	}
}

func TestNewOllama_ProbeDeterminesDim(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(embedHandler(t, 4, &calls))
	defer srv.Close()

	o, err := NewOllama(context.Background(), srv.URL, "bge-m3")
	if err != nil {
		t.Fatalf("NewOllama: %v", err)
	}
	if o.Dim() != 4 {
		t.Errorf("Dim() = %d, хочу 4", o.Dim())
	}
	if o.Model() != "bge-m3" {
		t.Errorf("Model() = %q, хочу %q", o.Model(), "bge-m3")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("вызовов к серверу = %d, хочу 1 (пробный эмбеддинг)", got)
	}
}

func TestNewOllama_EmptyBaseURL(t *testing.T) {
	_, err := NewOllama(context.Background(), "", "bge-m3")
	if err == nil {
		t.Fatal("ожидал ошибку при пустом baseURL")
	}
}

func TestNewOllama_EmptyModel(t *testing.T) {
	_, err := NewOllama(context.Background(), "http://localhost:11434", "")
	if err == nil {
		t.Fatal("ожидал ошибку при пустом model")
	}
}

func TestNewOllama_ServerUnavailable(t *testing.T) {
	// Порт, на котором заведомо никто не слушает (сервер сразу закрыт).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	_, err := NewOllama(context.Background(), srv.URL, "bge-m3")
	if err == nil {
		t.Fatal("ожидал ошибку при недоступном сервере")
	}
}

func TestOllama_Embed_Batch(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(embedHandler(t, 3, &calls))
	defer srv.Close()

	o, err := NewOllama(context.Background(), srv.URL, "bge-m3")
	if err != nil {
		t.Fatalf("NewOllama: %v", err)
	}
	atomic.StoreInt32(&calls, 0)

	vecs, err := o.Embed(context.Background(), []string{"один", "два"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 2 {
		t.Fatalf("len(vecs) = %d, хочу 2", len(vecs))
	}
	if len(vecs[0]) != 3 || len(vecs[1]) != 3 {
		t.Errorf("векторы не той размерности: %+v", vecs)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("вызовов к серверу = %d, хочу 1", got)
	}
}

func TestOllama_Embed_EmptyTexts(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(embedHandler(t, 3, &calls))
	defer srv.Close()

	o, err := NewOllama(context.Background(), srv.URL, "bge-m3")
	if err != nil {
		t.Fatalf("NewOllama: %v", err)
	}
	atomic.StoreInt32(&calls, 0)

	vecs, err := o.Embed(context.Background(), nil)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 0 {
		t.Errorf("len(vecs) = %d, хочу 0", len(vecs))
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("вызовов к серверу = %d, хочу 0 (запрос не должен уходить)", got)
	}
}

func TestOllama_Embed_CacheHit(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(embedHandler(t, 3, &calls))
	defer srv.Close()

	o, err := NewOllama(context.Background(), srv.URL, "bge-m3")
	if err != nil {
		t.Fatalf("NewOllama: %v", err)
	}
	atomic.StoreInt32(&calls, 0)

	first, err := o.Embed(context.Background(), []string{"повтор"})
	if err != nil {
		t.Fatalf("Embed (1): %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("вызовов после 1-го Embed = %d, хочу 1", got)
	}

	second, err := o.Embed(context.Background(), []string{"повтор"})
	if err != nil {
		t.Fatalf("Embed (2): %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("вызовов после 2-го Embed = %d, хочу 1 (кэш-хит, сеть не трогается)", got)
	}
	if len(second) != 1 || len(second[0]) != len(first[0]) {
		t.Errorf("second = %+v, хочу вектор той же размерности, что first = %+v", second, first)
	}
	for i := range first[0] {
		if first[0][i] != second[0][i] {
			t.Errorf("second[0][%d] = %v, хочу %v (кэшированное значение)", i, second[0][i], first[0][i])
		}
	}
}

func TestOllama_Embed_MixedHitAndMiss(t *testing.T) {
	var calls int32
	var lastInput []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("декодирование запроса: %v", err)
		}
		lastInput = req.Input
		atomic.AddInt32(&calls, 1)

		vecs := make([][]float32, len(req.Input))
		for i := range req.Input {
			vecs[i] = []float32{float32(i), float32(i + 1), float32(i + 2)}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(embedResponse{Embeddings: vecs})
	}))
	defer srv.Close()

	o, err := NewOllama(context.Background(), srv.URL, "bge-m3")
	if err != nil {
		t.Fatalf("NewOllama: %v", err)
	}

	// "known" уже в кэше, "unknown" — нет.
	if _, err := o.Embed(context.Background(), []string{"known"}); err != nil {
		t.Fatalf("прогрев кэша: %v", err)
	}
	atomic.StoreInt32(&calls, 0)

	vecs, err := o.Embed(context.Background(), []string{"known", "unknown"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 2 {
		t.Fatalf("len(vecs) = %d, хочу 2", len(vecs))
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("вызовов к серверу = %d, хочу 1", got)
	}
	if len(lastInput) != 1 || lastInput[0] != "unknown" {
		t.Errorf("в ollama ушло %+v, хочу только промах [\"unknown\"]", lastInput)
	}
}

func TestOllama_Embed_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/embed" {
			var req embedRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if len(req.Input) == 1 && req.Input[0] == probeText {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(embedResponse{Embeddings: [][]float32{{1, 2}}})
				return
			}
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"model not found"}`))
	}))
	defer srv.Close()

	o, err := NewOllama(context.Background(), srv.URL, "bge-m3")
	if err != nil {
		t.Fatalf("NewOllama: %v", err)
	}

	_, err = o.Embed(context.Background(), []string{"новый текст"})
	if err == nil {
		t.Fatal("ожидал ошибку при не-2xx статусе")
	}
}

func TestOllama_Embed_CountMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Input) == 1 && req.Input[0] == probeText {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(embedResponse{Embeddings: [][]float32{{1, 2}}})
			return
		}
		// Возвращаем меньше векторов, чем текстов.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(embedResponse{Embeddings: [][]float32{{1, 2}}})
	}))
	defer srv.Close()

	o, err := NewOllama(context.Background(), srv.URL, "bge-m3")
	if err != nil {
		t.Fatalf("NewOllama: %v", err)
	}

	_, err = o.Embed(context.Background(), []string{"текст1", "текст2"})
	if err == nil {
		t.Fatal("ожидал ошибку при несовпадении числа векторов")
	}
}

func TestOllama_Embed_ContextCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Input) == 1 && req.Input[0] == probeText {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(embedResponse{Embeddings: [][]float32{{1, 2}}})
			return
		}
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(embedResponse{Embeddings: [][]float32{{1, 2}}})
	}))
	defer srv.Close()

	o, err := NewOllama(context.Background(), srv.URL, "bge-m3")
	if err != nil {
		t.Fatalf("NewOllama: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = o.Embed(ctx, []string{"новый текст, не в кэше"})
	if err == nil {
		t.Fatal("ожидал ошибку при отменённом контексте")
	}
}

func TestOllama_WithHTTPClient(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(embedHandler(t, 2, &calls))
	defer srv.Close()

	custom := &http.Client{Timeout: 5 * time.Second}
	o, err := NewOllama(context.Background(), srv.URL, "bge-m3", WithHTTPClient(custom))
	if err != nil {
		t.Fatalf("NewOllama: %v", err)
	}
	if o.httpClient != custom {
		t.Error("WithHTTPClient не применился")
	}
}
