package reason

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// defaultBaseURL — эндпоинт DeepSeek chat по умолчанию (OpenAI-совместимый API).
const defaultBaseURL = "https://api.deepseek.com"

// defaultTimeout — таймаут HTTP-запроса по умолчанию.
const defaultTimeout = 60 * time.Second

// maxErrBodyLen — сколько байт тела ответа включать в текст ошибки.
const maxErrBodyLen = 1024

// DeepSeek — реализация Reasoner поверх DeepSeek chat (OpenAI-совместимый API).
type DeepSeek struct {
	httpClient *http.Client
	baseURL    string
	model      string
	apiKey     string
}

// Option настраивает DeepSeek при создании через NewDeepSeek.
type Option func(*DeepSeek)

// WithBaseURL переопределяет базовый URL API (используется в тестах с httptest).
func WithBaseURL(baseURL string) Option {
	return func(d *DeepSeek) {
		d.baseURL = baseURL
	}
}

// NewDeepSeek создаёт Reasoner поверх DeepSeek chat. apiKey передаёт вызывающий код
// (значение читается из env DEEPSEEK_API_KEY в cmd/memoryd, не в этом пакете); пустой
// ключ — ошибка конфигурации, а не время выполнения.
func NewDeepSeek(apiKey, model string, opts ...Option) (*DeepSeek, error) {
	if apiKey == "" {
		return nil, errors.New("reason: apiKey пуст")
	}
	if model == "" {
		return nil, errors.New("reason: model пуст")
	}
	d := &DeepSeek{
		httpClient: &http.Client{Timeout: defaultTimeout},
		baseURL:    defaultBaseURL,
		model:      model,
		apiKey:     apiKey,
	}
	for _, opt := range opts {
		opt(d)
	}
	return d, nil
}

// chatRequest — тело запроса к /chat/completions (OpenAI-совместимый формат).
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	Stream      bool          `json:"stream"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatResponse — тело успешного ответа /chat/completions.
type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

// Complete отправляет prompt в DeepSeek chat одним пользовательским сообщением и
// возвращает текст ответа модели.
func (d *DeepSeek) Complete(ctx context.Context, prompt string) (string, error) {
	reqBody, err := json.Marshal(chatRequest{
		Model: d.model,
		Messages: []chatMessage{
			{Role: "user", Content: prompt},
		},
		Temperature: 0.3,
		Stream:      false,
	})
	if err != nil {
		return "", fmt.Errorf("reason: marshal запроса: %w", err)
	}

	url := d.baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("reason: сборка запроса: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+d.apiKey)

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("reason: запрос к DeepSeek: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reason: чтение ответа: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("reason: DeepSeek ответил %d: %s", resp.StatusCode, truncate(body, maxErrBodyLen))
	}

	var chatResp chatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return "", fmt.Errorf("reason: разбор ответа: %w", err)
	}
	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("reason: DeepSeek вернул пустой список choices: %s", truncate(body, maxErrBodyLen))
	}

	return chatResp.Choices[0].Message.Content, nil
}

// truncate обрезает тело ответа до n байт для включения в сообщение об ошибке.
func truncate(body []byte, n int) string {
	if len(body) <= n {
		return string(body)
	}
	return string(body[:n]) + "…"
}

var _ Reasoner = (*DeepSeek)(nil)
