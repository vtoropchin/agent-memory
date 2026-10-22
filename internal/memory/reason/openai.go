package reason

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// defaultTimeout — таймаут HTTP-запроса по умолчанию.
const defaultTimeout = 60 * time.Second

// maxErrBodyLen — сколько байт тела ответа включать в текст ошибки.
const maxErrBodyLen = 1024

// OpenAI — реализация Reasoner поверх любого OpenAI-совместимого chat completions
// (DeepSeek, локальный CLIProxyAPI и т.п.): провайдеры различаются только base URL,
// ключом и моделью, которые выбирает composition root (cmd/memo, DESIGN.md §10).
type OpenAI struct {
	httpClient *http.Client
	baseURL    string
	model      string
	apiKey     string
}

// NewOpenAI создаёт Reasoner поверх OpenAI-совместимого chat. baseURL — корень API,
// к которому дописывается /chat/completions (у DeepSeek без /v1, у CLIProxyAPI —
// с /v1). apiKey передаёт вызывающий код (значение читается из env в cmd/memo, не в
// этом пакете); пустые baseURL, apiKey или model — ошибка конфигурации, а не время
// выполнения.
func NewOpenAI(baseURL, model, apiKey string) (*OpenAI, error) {
	if baseURL == "" {
		return nil, errors.New("reason: baseURL пуст")
	}
	if apiKey == "" {
		return nil, errors.New("reason: apiKey пуст")
	}
	if model == "" {
		return nil, errors.New("reason: model пуст")
	}
	return &OpenAI{
		httpClient: &http.Client{Timeout: defaultTimeout},
		baseURL:    strings.TrimRight(baseURL, "/"),
		model:      model,
		apiKey:     apiKey,
	}, nil
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

// Complete отправляет prompt в chat одним пользовательским сообщением и возвращает
// текст ответа модели.
func (d *OpenAI) Complete(ctx context.Context, prompt string) (string, error) {
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
		return "", fmt.Errorf("reason: запрос к %s: %w", d.baseURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reason: чтение ответа: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("reason: %s ответил %d: %s", d.baseURL, resp.StatusCode, truncate(body, maxErrBodyLen))
	}

	var chatResp chatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return "", fmt.Errorf("reason: разбор ответа: %w", err)
	}
	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("reason: %s вернул пустой список choices: %s", d.baseURL, truncate(body, maxErrBodyLen))
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

var _ Reasoner = (*OpenAI)(nil)
