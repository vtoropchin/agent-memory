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

// Local requests can queue behind inference already running on the same Ollama.
const ollamaTimeout = 120 * time.Second

// Ollama completes prompts through Ollama's native chat API.
type Ollama struct {
	httpClient *http.Client
	baseURL    string
	model      string
}

func NewOllama(baseURL, model string) (*Ollama, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" || strings.TrimSpace(model) == "" {
		return nil, errors.New("reason: для Ollama нужны base URL и модель")
	}
	return &Ollama{
		httpClient: &http.Client{
			Timeout: ollamaTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		baseURL: baseURL,
		model:   model,
	}, nil
}

type ollamaChatRequest struct {
	Model    string             `json:"model"`
	Messages []chatMessage      `json:"messages"`
	Stream   bool               `json:"stream"`
	Think    bool               `json:"think"`
	Options  map[string]float64 `json:"options"`
}

// Complete asks for a direct answer and returns the model's final text.
func (o *Ollama) Complete(ctx context.Context, prompt string) (string, error) {
	body, err := json.Marshal(ollamaChatRequest{
		Model:    o.model,
		Messages: []chatMessage{{Role: "user", Content: prompt}},
		Stream:   false,
		Think:    false,
		Options:  map[string]float64{"temperature": 0.3},
	})
	if err != nil {
		return "", fmt.Errorf("reason: сборка запроса к Ollama: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("reason: сборка запроса к Ollama: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("reason: запрос к Ollama: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBodyLen+1))
		return "", fmt.Errorf("reason: Ollama ответил %d: %s", resp.StatusCode, truncate(detail, maxErrBodyLen))
	}

	var answer struct {
		Message    chatMessage `json:"message"`
		Done       bool        `json:"done"`
		DoneReason string      `json:"done_reason"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		return "", fmt.Errorf("reason: разбор ответа Ollama: %w", err)
	}
	if answer.DoneReason == "length" {
		return "", errors.New("reason: Ollama оборвал ответ по лимиту токенов (length)")
	}
	if !answer.Done {
		return "", errors.New("reason: Ollama вернул незавершённый ответ")
	}
	if strings.TrimSpace(answer.Message.Content) == "" {
		return "", errors.New("reason: Ollama вернул пустой ответ")
	}
	return answer.Message.Content, nil
}

var _ Reasoner = (*Ollama)(nil)
