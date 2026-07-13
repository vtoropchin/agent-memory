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

// openaiEmbedHandler строит httptest-хендлер `/v1/embeddings`, возвращающий по одному
// детерминированному вектору dim-элементов на каждый вход (в порядке запроса), и
// считает число вызовов.
func openaiEmbedHandler(t *testing.T, dim int, calls *int32) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("метод = %s, хочу POST", r.Method)
		}
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("путь = %s, хочу /v1/embeddings", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, хочу %q", got, "Bearer test-key")
		}

		var req openAIEmbedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("декодирование запроса: %v", err)
		}
		atomic.AddInt32(calls, 1)

		var resp openAIEmbedResponse
		for i := range req.Input {
			v := make([]float32, dim)
			for j := range v {
				v[j] = float32(i + j)
			}
			resp.Data = append(resp.Data, struct {
				Embedding []float32 `json:"embedding"`
				Index     int       `json:"index"`
			}{Embedding: v, Index: i})
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func TestNewOpenAI_ProbeDeterminesDim(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(openaiEmbedHandler(t, 5, &calls))
	defer srv.Close()

	o, err := NewOpenAI(context.Background(), srv.URL, "text-embedding-3-small", "test-key")
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}
	if o.Dim() != 5 {
		t.Errorf("Dim() = %d, хочу 5", o.Dim())
	}
	if o.Model() != "text-embedding-3-small" {
		t.Errorf("Model() = %q, хочу %q", o.Model(), "text-embedding-3-small")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("вызовов к серверу = %d, хочу 1 (пробный эмбеддинг)", got)
	}
}

func TestNewOpenAI_EmptyBaseURL(t *testing.T) {
	_, err := NewOpenAI(context.Background(), "", "text-embedding-3-small", "test-key")
	if err == nil {
		t.Fatal("ожидал ошибку при пустом baseURL")
	}
}

func TestNewOpenAI_EmptyModel(t *testing.T) {
	_, err := NewOpenAI(context.Background(), "https://api.openai.com", "", "test-key")
	if err == nil {
		t.Fatal("ожидал ошибку при пустом model")
	}
}

func TestNewOpenAI_EmptyAPIKey(t *testing.T) {
	_, err := NewOpenAI(context.Background(), "https://api.openai.com", "text-embedding-3-small", "")
	if err == nil {
		t.Fatal("ожидал ошибку при пустом apiKey")
	}
}

func TestNewOpenAI_ServerUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	_, err := NewOpenAI(context.Background(), srv.URL, "text-embedding-3-small", "test-key")
	if err == nil {
		t.Fatal("ожидал ошибку при недоступном сервере")
	}
}

// TestOpenAI_Embed_ReordersByIndex — ключевой тест на отличие от ollama: сервер
// возвращает data в порядке, обратном входу; Embed обязан вернуть векторы в порядке
// texts, ориентируясь на поле index, а не на позицию в массиве data.
func TestOpenAI_Embed_ReordersByIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openAIEmbedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("декодирование запроса: %v", err)
		}

		if len(req.Input) == 1 && req.Input[0] == probeText {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"embedding":[1,2],"index":0}]}`))
			return
		}

		// Перепутанный порядок: data[0] соответствует последнему входу (index len-1),
		// data[len-1] — первому (index 0).
		var resp openAIEmbedResponse
		for i := len(req.Input) - 1; i >= 0; i-- {
			resp.Data = append(resp.Data, struct {
				Embedding []float32 `json:"embedding"`
				Index     int       `json:"index"`
			}{Embedding: []float32{float32(i), float32(i) + 100}, Index: i})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	o, err := NewOpenAI(context.Background(), srv.URL, "text-embedding-3-small", "test-key")
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}

	vecs, err := o.Embed(context.Background(), []string{"первый", "второй", "третий"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 3 {
		t.Fatalf("len(vecs) = %d, хочу 3", len(vecs))
	}
	for i, v := range vecs {
		want := []float32{float32(i), float32(i) + 100}
		if v[0] != want[0] || v[1] != want[1] {
			t.Errorf("vecs[%d] = %v, хочу %v (порядок должен идти по index, не по позиции в data)", i, v, want)
		}
	}
}

func TestOpenAI_Embed_EmptyTexts(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(openaiEmbedHandler(t, 3, &calls))
	defer srv.Close()

	o, err := NewOpenAI(context.Background(), srv.URL, "text-embedding-3-small", "test-key")
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
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

func TestOpenAI_Embed_CacheHit(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(openaiEmbedHandler(t, 3, &calls))
	defer srv.Close()

	o, err := NewOpenAI(context.Background(), srv.URL, "text-embedding-3-small", "test-key")
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
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

func TestOpenAI_Embed_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openAIEmbedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Input) == 1 && req.Input[0] == probeText {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"embedding":[1,2],"index":0}]}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	}))
	defer srv.Close()

	o, err := NewOpenAI(context.Background(), srv.URL, "text-embedding-3-small", "test-key")
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}

	_, err = o.Embed(context.Background(), []string{"новый текст"})
	if err == nil {
		t.Fatal("ожидал ошибку при не-2xx статусе")
	}
}

func TestOpenAI_Embed_CountMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openAIEmbedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Input) == 1 && req.Input[0] == probeText {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"embedding":[1,2],"index":0}]}`))
			return
		}
		// Возвращаем меньше векторов, чем текстов.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[1,2],"index":0}]}`))
	}))
	defer srv.Close()

	o, err := NewOpenAI(context.Background(), srv.URL, "text-embedding-3-small", "test-key")
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}

	_, err = o.Embed(context.Background(), []string{"текст1", "текст2"})
	if err == nil {
		t.Fatal("ожидал ошибку при несовпадении числа векторов")
	}
}

// TestOpenAI_Embed_DuplicateIndex — дубль index при пропуске другого: количество
// элементов data совпадает с числом текстов и оба index в допустимом диапазоне, но
// один слот (index 1) не заполнен, а другой (index 0) записан дважды. Без явной
// проверки заполненности это прошло бы обе предыдущие проверки и тихо вернуло бы
// nil-вектор для второго текста — ТЗ требует явную ошибку вместо тихой порчи.
func TestOpenAI_Embed_DuplicateIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openAIEmbedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Input) == 1 && req.Input[0] == probeText {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"embedding":[1,2],"index":0}]}`))
			return
		}
		// Два элемента с index:0, index:1 (второй текст) не встречается вовсе.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[1,2],"index":0},{"embedding":[3,4],"index":0}]}`))
	}))
	defer srv.Close()

	o, err := NewOpenAI(context.Background(), srv.URL, "text-embedding-3-small", "test-key")
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}

	vecs, err := o.Embed(context.Background(), []string{"a", "b"})
	if err == nil {
		t.Fatalf("ожидал ошибку при дублирующемся index, получил vecs=%+v", vecs)
	}
}

// TestOpenAI_Embed_IndexOutOfRange — index за пределами [0, len(texts)) должен
// приводить к ошибке, а не к панике или молчаливой потере вектора.
func TestOpenAI_Embed_IndexOutOfRange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openAIEmbedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Input) == 1 && req.Input[0] == probeText {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"embedding":[1,2],"index":0}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[1,2],"index":0},{"embedding":[3,4],"index":5}]}`))
	}))
	defer srv.Close()

	o, err := NewOpenAI(context.Background(), srv.URL, "text-embedding-3-small", "test-key")
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}

	_, err = o.Embed(context.Background(), []string{"a", "b"})
	if err == nil {
		t.Fatal("ожидал ошибку при index вне диапазона")
	}
}

func TestOpenAI_WithHTTPClient(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(openaiEmbedHandler(t, 2, &calls))
	defer srv.Close()

	custom := &http.Client{Timeout: 5 * time.Second}
	o, err := NewOpenAI(context.Background(), srv.URL, "text-embedding-3-small", "test-key", WithOpenAIHTTPClient(custom))
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}
	if o.httpClient != custom {
		t.Error("WithOpenAIHTTPClient не применился")
	}
}
