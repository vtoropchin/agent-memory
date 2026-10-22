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
	all, err := fetchModelIDs(ctx, strings.TrimRight(baseURL, "/")+"/v1/models", key)
	if err != nil {
		return nil, err
	}
	var embeddingOnly []string
	for _, id := range all {
		if strings.Contains(id, "embedding") {
			embeddingOnly = append(embeddingOnly, id)
		}
	}
	if len(embeddingOnly) > 0 {
		return embeddingOnly, nil
	}
	return all, nil
}

// fetchModelIDs запрашивает у OpenAI-совместимого сервера список моделей
// (`GET <url>`, `Authorization: Bearer <key>`) и возвращает все непустые
// `.data[].id` в порядке ответа. Общий транспорт эмбеддинг- и chat-вариантов
// (fetchOpenAIEmbeddingModels/fetchChatModels) — они различаются только
// путём эндпоинта и пост-фильтром, а не HTTP-обвязкой/разбором. Неверный ключ/
// недоступный сервер — non-2xx статус, возвращается как ошибка; пустой список
// без ошибки — валидный результат (вызывающий сам решает, что показать). Ollama
// сюда не складывается: другой эндпоинт (/api/tags), без auth и с формой
// .models[].name — см. отдельную fetchOllamaModels.
func fetchModelIDs(ctx context.Context, url, key string) ([]string, error) {
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

	ids := make([]string, 0, len(parsed.Data))
	for _, d := range parsed.Data {
		if d.ID != "" {
			ids = append(ids, d.ID)
		}
	}
	return ids, nil
}

// fetchChatModels запрашивает у OpenAI-совместимого chat-API (`GET {baseURL}/models`)
// список доступных моделей через fetchModelIDs и возвращает их без фильтрации (в
// отличие от fetchOpenAIEmbeddingModels: это chat-модели reasoner'а, фильтровать по
// имени нечего). baseURL — тот же корень, что у reason.OpenAI: у DeepSeek без /v1
// (https://api.deepseek.com → /models), у CLIProxyAPI с ним (…:8317/v1 → /v1/models).
func fetchChatModels(ctx context.Context, baseURL, key string) ([]string, error) {
	return fetchModelIDs(ctx, strings.TrimRight(baseURL, "/")+"/models", key)
}
