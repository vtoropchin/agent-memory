package embed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// defaultTimeout — таймаут HTTP-запроса к ollama по умолчанию.
const defaultTimeout = 30 * time.Second

// maxErrBodyLen — сколько байт тела ответа включать в текст ошибки.
const maxErrBodyLen = 1024

// maxCacheEntries — порог размера кэша эмбеддингов, после которого он очищается
// целиком (простая стратегия без LRU: промах на кэше не критичен, просто лишний
// запрос к ollama).
const maxCacheEntries = 10000

// probeText — короткий текст пробного эмбеддинга в конструкторе: заодно проверяет,
// что ollama жив и модель подтянута, и определяет dim.
const probeText = "probe"

// Ollama — реализация Embedder поверх ollama `/api/embed` (DESIGN.md §9). Дефолт
// проекта — модель bge-m3 (1024 dim, мультиязычная), но модель конфигурируется
// свободно — dim не хардкодится, а определяется пробным эмбеддингом при старте.
type Ollama struct {
	httpClient *http.Client
	baseURL    string
	model      string
	dim        int

	mu    sync.RWMutex
	cache map[string][]float32
}

// OllamaOption настраивает Ollama при создании через NewOllama.
type OllamaOption func(*Ollama)

// WithHTTPClient переопределяет HTTP-клиент (используется в тестах с httptest,
// а также для кастомных таймаутов/транспортов).
func WithHTTPClient(c *http.Client) OllamaOption {
	return func(o *Ollama) {
		o.httpClient = c
	}
}

// NewOllama создаёт Embedder поверх ollama. baseURL и model обязательны. Конструктор
// делает пробный эмбеддинг одного короткого текста: это заодно проверка, что ollama
// жив и модель подтянута, а dim берётся из ответа (не хардкодится). Ошибка на этом
// шаге — ошибка конструктора; откат на Noop делает вызывающий код (задача 3.6).
func NewOllama(ctx context.Context, baseURL, model string, opts ...OllamaOption) (*Ollama, error) {
	if baseURL == "" {
		return nil, errors.New("embed: baseURL пуст")
	}
	if model == "" {
		return nil, errors.New("embed: model пуст")
	}
	o := &Ollama{
		httpClient: &http.Client{Timeout: defaultTimeout},
		baseURL:    baseURL,
		model:      model,
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

// embedRequest — тело запроса к ollama `/api/embed`.
type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// embedResponse — тело успешного ответа `/api/embed`.
type embedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

// Embed возвращает векторы для текстов, используя кэш по (model, text) и обращаясь
// к ollama только за промахами. Пустой texts не делает запроса.
func (o *Ollama) Embed(ctx context.Context, texts []string) ([][]float32, error) {
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
		// text), холодный промах просто стоит одного лишнего запроса к ollama.
		o.cache = make(map[string][]float32)
	}
	for j, idx := range missIdx {
		result[idx] = vecs[j]
		o.cache[keys[idx]] = vecs[j]
	}
	o.mu.Unlock()

	return result, nil
}

// embedRemote выполняет запрос к ollama `/api/embed` без обращения к кэшу.
func (o *Ollama) embedRemote(ctx context.Context, texts []string) ([][]float32, error) {
	reqBody, err := json.Marshal(embedRequest{
		Model: o.model,
		Input: texts,
	})
	if err != nil {
		return nil, fmt.Errorf("embed: marshal запроса: %w", err)
	}

	url := o.baseURL + "/api/embed"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("embed: сборка запроса: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: запрос к ollama: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("embed: чтение ответа: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("embed: ollama ответила %d: %s", resp.StatusCode, truncate(body, maxErrBodyLen))
	}

	var embResp embedResponse
	if err := json.Unmarshal(body, &embResp); err != nil {
		return nil, fmt.Errorf("embed: разбор ответа: %w", err)
	}
	if len(embResp.Embeddings) != len(texts) {
		return nil, fmt.Errorf("embed: ollama вернула %d векторов на %d текстов", len(embResp.Embeddings), len(texts))
	}

	return embResp.Embeddings, nil
}

// cacheKey строит ключ кэша эмбеддингов как sha256(model + "\x00" + text) в hex.
func cacheKey(model, text string) string {
	sum := sha256.Sum256([]byte(model + "\x00" + text))
	return hex.EncodeToString(sum[:])
}

// truncate обрезает тело ответа до n байт для включения в сообщение об ошибке.
func truncate(body []byte, n int) string {
	if len(body) <= n {
		return string(body)
	}
	return string(body[:n]) + "…"
}

// Model возвращает имя модели ollama, заданное при создании.
func (o *Ollama) Model() string {
	return o.model
}

// Dim возвращает размерность вектора, определённую пробным эмбеддингом при старте.
func (o *Ollama) Dim() int {
	return o.dim
}

var _ Embedder = (*Ollama)(nil)
