// providers.go — получение списка доступных моделей у ollama/OpenAI-
// совместимого сервера для интерактивного `memo init` (задача 11.1).
// Живут в cmd/memo, а не в internal/memory/embed: это специфика
// инсталлятора (удобный выбор модели стрелками), а не рантайма — доменный
// интерфейс Embedder эти функции не используют и не должны. Тонкая обёртка
// поверх stdlib net/http — без зависимости от TUI-библиотеки, поэтому
// тестируется httptest без реального ollama/OpenAI (см. providers_test.go).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// fetchOllamaModels запрашивает у ollama-сервера baseURL список реально
// установленных моделей (`GET /api/tags`, поле `.models[].name`). Пустой
// список без ошибки — валидный результат (ollama жива, моделей не подтянуто);
// вызывающий (промпт init) сам решает откатиться на ручной ввод.
func fetchOllamaModels(ctx context.Context, baseURL string) ([]string, error) {
	url := strings.TrimRight(baseURL, "/") + "/api/tags"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("сборка запроса %s: %w", url, err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("запрос %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("%s: статус %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("разбор ответа %s: %w", url, err)
	}

	models := make([]string, 0, len(parsed.Models))
	for _, m := range parsed.Models {
		if m.Name != "" {
			models = append(models, m.Name)
		}
	}
	return models, nil
}

// fetchOpenAIEmbeddingModels запрашивает у OpenAI-совместимого сервера
// baseURL (`GET /v1/models`, `Authorization: Bearer <key>`) список моделей и
// отфильтровывает id, содержащие "embedding" (де-факто соглашение именования
// у OpenAI и совместимых серверов — text-embedding-3-small и т.п.). Если после
// фильтра список пуст, но запрос успешен — возвращает ВСЕ id: вызывающий
// промпт может показать полный список вместо пустого (задача 11.1: "при
// пустом фильтре можно показать все .data[].id"). Неверный ключ/недоступный
// сервер — non-2xx статус, возвращается как ошибка.
func fetchOpenAIEmbeddingModels(ctx context.Context, baseURL, key string) ([]string, error) {
	url := strings.TrimRight(baseURL, "/") + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("сборка запроса %s: %w", url, err)
	}
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("запрос %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("%s: статус %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("разбор ответа %s: %w", url, err)
	}

	all := make([]string, 0, len(parsed.Data))
	var embeddingOnly []string
	for _, d := range parsed.Data {
		if d.ID == "" {
			continue
		}
		all = append(all, d.ID)
		if strings.Contains(d.ID, "embedding") {
			embeddingOnly = append(embeddingOnly, d.ID)
		}
	}
	if len(embeddingOnly) > 0 {
		return embeddingOnly, nil
	}
	return all, nil
}
