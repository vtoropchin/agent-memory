package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// OpenAI — реализация Embedder поверх OpenAI-совместимого `/v1/embeddings` (DESIGN.md
// §9). Один клиент покрывает не только OpenAI, но и любой сервер с тем же контрактом
// (OpenRouter, LM Studio, vLLM и т.п.) — отсюда конфигурируемый baseURL.
type OpenAI struct {
	httpClient *http.Client
	baseURL    string
	model      string
	apiKey     string
	dim        int

	mu    sync.RWMutex
	cache map[string][]float32
}

// OpenAIOption настраивает OpenAI при создании через NewOpenAI.
type OpenAIOption func(*OpenAI)

// WithOpenAIHTTPClient переопределяет HTTP-клиент (используется в тестах с httptest,
// а также для кастомных таймаутов/транспортов).
func WithOpenAIHTTPClient(c *http.Client) OpenAIOption {
	return func(o *OpenAI) {
		o.httpClient = c
	}
}

// NewOpenAI создаёт Embedder поверх OpenAI-совместимого `/v1/embeddings`. baseURL,
// model и apiKey обязательны. Конструктор делает пробный эмбеддинг одного короткого
// текста: это заодно проверка, что ключ и модель валидны, а dim берётся из ответа
// (не хардкодится — работает для любой совместимой модели, не только
// text-embedding-3-small). Ошибка на этом шаге — ошибка конструктора; откат на Noop
// делает вызывающий код (cmd/memoryd, симметрично ollama).
func NewOpenAI(ctx context.Context, baseURL, model, apiKey string, opts ...OpenAIOption) (*OpenAI, error) {
	if baseURL == "" {
		return nil, errors.New("embed: baseURL пуст")
	}
	if model == "" {
		return nil, errors.New("embed: model пуст")
	}
	if apiKey == "" {
		return nil, errors.New("embed: apiKey пуст")
	}
	o := &OpenAI{
		httpClient: &http.Client{Timeout: defaultTimeout},
		baseURL:    baseURL,
		model:      model,
		apiKey:     apiKey,
		cache:      make(map[string][]float32),
	}
	for _, opt := range opts {
		opt(o)
	}

	vecs, err := o.embedRemote(ctx, []string{probeText})
	if err != nil {
		return nil, fmt.Errorf("embed: пробный эмбеддинг: %w", err)
	}
	o.dim = len(vecs[0])

	return o, nil
}

// openAIEmbedRequest — тело запроса к `/v1/embeddings`.
type openAIEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// openAIEmbedResponse — тело успешного ответа `/v1/embeddings`. Порядок элементов
// data не гарантирован спецификацией (в отличие от ollama) — соответствие входу
// восстанавливается по полю Index.
type openAIEmbedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
}

// Embed возвращает векторы для текстов, используя кэш по (model, text) и обращаясь
// к серверу только за промахами. Пустой texts не делает запроса.
func (o *OpenAI) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	result := make([][]float32, len(texts))
	keys := make([]string, len(texts))
	var missIdx []int
	var missTexts []string

	o.mu.RLock()
	for i, t := range texts {
		key := cacheKey(o.model, t)
		keys[i] = key
		if v, ok := o.cache[key]; ok {
			result[i] = v
		} else {
			missIdx = append(missIdx, i)
			missTexts = append(missTexts, t)
		}
	}
	o.mu.RUnlock()

	if len(missTexts) == 0 {
		return result, nil
	}

	vecs, err := o.embedRemote(ctx, missTexts)
	if err != nil {
		return nil, err
	}

	o.mu.Lock()
	if len(o.cache) > maxCacheEntries {
		// Простая стратегия сброса вместо LRU: кэш эмбеддингов — чистый по (model,
		// text), холодный промах просто стоит одного лишнего запроса.
		o.cache = make(map[string][]float32)
	}
	for j, idx := range missIdx {
		result[idx] = vecs[j]
		o.cache[keys[idx]] = vecs[j]
	}
	o.mu.Unlock()

	return result, nil
}

// embedRemote выполняет запрос к `/v1/embeddings` без обращения к кэшу. Векторы в
// ответе пересобираются по полю index — порядок data в ответе не гарантирован.
func (o *OpenAI) embedRemote(ctx context.Context, texts []string) ([][]float32, error) {
	reqBody, err := json.Marshal(openAIEmbedRequest{
		Model: o.model,
		Input: texts,
	})
	if err != nil {
		return nil, fmt.Errorf("embed: marshal запроса: %w", err)
	}

	url := o.baseURL + "/v1/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("embed: сборка запроса: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.apiKey)

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: запрос к OpenAI: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("embed: чтение ответа: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("embed: OpenAI ответил %d: %s", resp.StatusCode, truncate(body, maxErrBodyLen))
	}

	var embResp openAIEmbedResponse
	if err := json.Unmarshal(body, &embResp); err != nil {
		return nil, fmt.Errorf("embed: разбор ответа: %w", err)
	}
	if len(embResp.Data) != len(texts) {
		return nil, fmt.Errorf("embed: OpenAI вернул %d векторов на %d текстов", len(embResp.Data), len(texts))
	}

	// Пересборка по index: спецификация OpenAI не гарантирует порядок data. Помимо
	// диапазона и общего количества, отдельно отслеживаем заполненность каждого
	// слота: дубль index при пропуске другого проходит обе предыдущие проверки
	// (совпадает и количество, и диапазон), но тихо портит один из векторов —
	// такое не должно проходить молча.
	vecs := make([][]float32, len(texts))
	filled := make([]bool, len(texts))
	for _, d := range embResp.Data {
		if d.Index < 0 || d.Index >= len(texts) {
			return nil, fmt.Errorf("embed: OpenAI вернул некорректный index %d на %d текстов", d.Index, len(texts))
		}
		if filled[d.Index] {
			return nil, fmt.Errorf("embed: OpenAI вернул повторяющийся index %d на %d текстов", d.Index, len(texts))
		}
		vecs[d.Index] = d.Embedding
		filled[d.Index] = true
	}
	for i, ok := range filled {
		if !ok {
			return nil, fmt.Errorf("embed: OpenAI не вернул вектор для index %d на %d текстов", i, len(texts))
		}
	}

	return vecs, nil
}

// Model возвращает имя модели, заданное при создании.
func (o *OpenAI) Model() string {
	return o.model
}

// Dim возвращает размерность вектора, определённую пробным эмбеддингом при старте.
func (o *OpenAI) Dim() int {
	return o.dim
}

var _ Embedder = (*OpenAI)(nil)
