// wizard.go — интерактивный TUI-промпт `memo init` (задача 11.1): select
// стрелками/Enter + скрытый ввод для ключей, поверх github.com/charmbracelet/huh
// (спайк-решение — см. "Решение" в tasks/11.1-init-cobra-provider-setup.md).
// Вызывается ТОЛЬКО когда isInteractiveStdin() уже подтвердил, что stdin —
// реальный терминал (runInit, init.go); это единственный уровень "graceful
// фолбэка при не-TTY" — huh/bubbletea сами по себе не вызываются в пайпе или
// под -no-input, поэтому их поведение без TTY здесь не имеет значения. Каждое
// поле — тонкая обёртка над уже протестированными без TUI функциями
// (fetchOllamaModels/fetchOpenAIEmbeddingModels, providers.go); сам виджет не
// юнит-тестируется (библиотека), тестируется только плумбинг под ним.
package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/huh"
)

// promptInitTUI дозапрашивает поля cfg, оставшиеся пустыми после флагов, через
// интерактивные select/input-виджеты huh. Уже заполненные (флагом) поля не
// переспрашиваются — тот же принцип, что был у promptInit/promptChoice до
// этой задачи. Возвращает ошибку huh.ErrUserAborted, если пользователь вышел
// (Ctrl+C/Esc) — runInit превращает её в понятное сообщение.
func promptInitTUI(ctx context.Context, out io.Writer, cfg initConfig) (initConfig, error) {
	if cfg.Agent == "" {
		agent := defaultAgentKey
		if err := huh.NewSelect[string]().
			Title("Агентское приложение").
			Options(agentSelectOptions()...).
			Value(&agent).Run(); err != nil {
			return cfg, err
		}
		cfg.Agent = agent
	}

	if cfg.Project == "" {
		def := dirBaseName(cfg.Dir)
		project := def
		if err := huh.NewInput().
			Title("Имя проекта").
			Value(&project).Run(); err != nil {
			return cfg, err
		}
		if project == "" {
			project = def
		}
		cfg.Project = project
	}

	if cfg.EmbedderProvider == "" {
		provider := defaultEmbedderProvider
		if err := huh.NewSelect[string]().
			Title("Провайдер эмбеддинга").
			Options(providerSelectOptions(embedderProviders)...).
			Value(&provider).Run(); err != nil {
			return cfg, err
		}
		cfg.EmbedderProvider = provider
	}

	switch cfg.EmbedderProvider {
	case "ollama":
		if err := promptOllamaEmbedder(ctx, out, &cfg); err != nil {
			return cfg, err
		}
	case "openai":
		if err := promptOpenAIEmbedder(ctx, out, &cfg); err != nil {
			return cfg, err
		}
	}

	if cfg.ReasonerProvider == "" {
		provider := defaultReasonerProvider
		if err := huh.NewSelect[string]().
			Title("Провайдер reasoner (суммаризация recall, консолидация памяти)").
			Options(providerSelectOptions(reasonerProviders)...).
			Value(&provider).Run(); err != nil {
			return cfg, err
		}
		cfg.ReasonerProvider = provider
	}

	if cfg.ReasonerProvider == "deepseek" && cfg.DeepseekKey == "" {
		key := ""
		if err := huh.NewInput().
			Title("DeepSeek API key (Enter — пропустить, указать позже в .env)").
			Password(true).
			Value(&key).Run(); err != nil {
			return cfg, err
		}
		cfg.DeepseekKey = key
	}

	if cfg.StorePath == "" {
		store := defaultStorePath
		if err := huh.NewInput().
			Title("Путь к файлу БД").
			Value(&store).Run(); err != nil {
			return cfg, err
		}
		if store == "" {
			store = defaultStorePath
		}
		cfg.StorePath = store
	}

	// Ambient-режим через хуки харнесса — только для Claude Code (задача 7.2);
	// у opencode аналога хуков нет, doInit сам честно сообщит об этом в выводе.
	if cfg.Agent == "claude" && cfg.Hooks == "" {
		hooks := hooksSessionStart
		if err := huh.NewSelect[string]().
			Title("Подключить автоматический recall через хуки Claude Code (ambient-режим)").
			Options(
				huh.NewOption(hooksSessionStart, hooksSessionStart),
				huh.NewOption(hooksBoth, hooksBoth),
				huh.NewOption(hooksNone, hooksNone),
			).
			Value(&hooks).Run(); err != nil {
			return cfg, err
		}
		cfg.Hooks = hooks
	}

	return cfg, nil
}

// agentSelectOptions строит варианты select-промпта агентского приложения из
// agentRegistry (человекочитаемый label + ключ -agent).
func agentSelectOptions() []huh.Option[string] {
	keys := agentKeys()
	opts := make([]huh.Option[string], 0, len(keys))
	for _, k := range keys {
		opts = append(opts, huh.NewOption(fmt.Sprintf("%s (%s)", agentRegistry[k].label, k), k))
	}
	return opts
}

// providerSelectOptions строит варианты select-промпта провайдера (эмбеддинга
// или reasoner) из providers (init.go) — та же деривация, что agentSelectOptions
// делает для agentRegistry; сохраняет порядок providers (первый — дефолт).
func providerSelectOptions(providers []providerSpec) []huh.Option[string] {
	opts := make([]huh.Option[string], 0, len(providers))
	for _, p := range providers {
		opts = append(opts, huh.NewOption(p.label, p.key))
	}
	return opts
}

// promptOllamaEmbedder спрашивает base URL ollama (если не задан), затем
// пробует получить реально установленные модели (fetchOllamaModels) и
// показать их select-списком; при недоступности сервера/пустом списке —
// печатает причину в out и откатывается на ручной ввод модели (дефолт
// defaultOllamaModel) — не блокирует установку (задача 11.1).
func promptOllamaEmbedder(ctx context.Context, out io.Writer, cfg *initConfig) error {
	if cfg.EmbedderBaseURL == "" {
		base := defaultOllamaBaseURL
		if err := huh.NewInput().Title("ollama base URL").Value(&base).Run(); err != nil {
			return err
		}
		if base == "" {
			base = defaultOllamaBaseURL
		}
		cfg.EmbedderBaseURL = base
	}
	if cfg.EmbedderModel != "" {
		return nil
	}

	fetchCtx, cancel := context.WithTimeout(ctx, fetchModelsTimeout)
	models, err := fetchOllamaModels(fetchCtx, cfg.EmbedderBaseURL)
	cancel()
	if err != nil {
		fmt.Fprintf(out, "не удалось получить список моделей ollama (%v) — введи имя модели вручную\n", err)
		return promptModelManual(&cfg.EmbedderModel, defaultOllamaModel)
	}
	if len(models) == 0 {
		fmt.Fprintln(out, "ollama не сообщила ни одной установленной модели — введи имя модели вручную")
		return promptModelManual(&cfg.EmbedderModel, defaultOllamaModel)
	}

	model := models[0]
	if err := huh.NewSelect[string]().
		Title("модель эмбеддингов ollama (установленные)").
		Options(toModelOptions(models)...).
		Value(&model).Run(); err != nil {
		return err
	}
	cfg.EmbedderModel = model
	return nil
}

// promptOpenAIEmbedder спрашивает токен (скрытый ввод, если не задан), потом
// base URL (дефолт api.openai.com — подходит и для OpenRouter/LM Studio/vLLM),
// затем список моделей эмбеддинга через fetchOpenAIEmbeddingModels; ошибка
// запроса/пустой фильтр — причина в out и ручной ввод (токен повторно не
// спрашивается). Если ключ не введён (пропущен) — сеть не дёргаем вовсе,
// сразу ручной ввод: без ключа запрос заведомо не пройдёт.
func promptOpenAIEmbedder(ctx context.Context, out io.Writer, cfg *initConfig) error {
	if cfg.OpenAIKey == "" {
		key := ""
		if err := huh.NewInput().
			Title("OpenAI API key (Enter — пропустить, указать позже в .env)").
			Password(true).
			Value(&key).Run(); err != nil {
			return err
		}
		cfg.OpenAIKey = key
	}
	if cfg.EmbedderBaseURL == "" {
		base := defaultOpenAIBaseURL
		if err := huh.NewInput().
			Title("base URL (для OpenRouter/LM Studio/vLLM — укажи свой адрес)").
			Value(&base).Run(); err != nil {
			return err
		}
		if base == "" {
			base = defaultOpenAIBaseURL
		}
		cfg.EmbedderBaseURL = base
	}
	if cfg.EmbedderModel != "" {
		return nil
	}
	if cfg.OpenAIKey == "" {
		fmt.Fprintln(out, "ключ не введён — список моделей запросить нечем, введи имя модели вручную")
		return promptModelManual(&cfg.EmbedderModel, "")
	}

	fetchCtx, cancel := context.WithTimeout(ctx, fetchModelsTimeout)
	models, err := fetchOpenAIEmbeddingModels(fetchCtx, cfg.EmbedderBaseURL, cfg.OpenAIKey)
	cancel()
	if err != nil {
		fmt.Fprintf(out, "не удалось получить список моделей OpenAI (%v) — введи имя модели вручную\n", err)
		return promptModelManual(&cfg.EmbedderModel, "")
	}
	if len(models) == 0 {
		fmt.Fprintln(out, "OpenAI не вернул ни одной модели — введи имя модели вручную")
		return promptModelManual(&cfg.EmbedderModel, "")
	}

	model := models[0]
	if err := huh.NewSelect[string]().
		Title("модель эмбеддингов OpenAI").
		Options(toModelOptions(models)...).
		Value(&model).Run(); err != nil {
		return err
	}
	cfg.EmbedderModel = model
	return nil
}

// promptModelManual — ручной ввод имени модели в *dst с дефолтом def. Пустой
// def (нет естественного дефолта, напр. OpenAI без списка) требует
// непустого ввода — иначе agentmemory.toml получил бы модель "".
func promptModelManual(dst *string, def string) error {
	val := def
	input := huh.NewInput().Title("модель эмбеддингов")
	if def == "" {
		input = input.Validate(func(s string) error {
			if strings.TrimSpace(s) == "" {
				return fmt.Errorf("имя модели не может быть пустым")
			}
			return nil
		})
	}
	if err := input.Value(&val).Run(); err != nil {
		return err
	}
	if val == "" {
		val = def
	}
	*dst = val
	return nil
}

// toModelOptions оборачивает список имён моделей в huh.Option (label == value).
func toModelOptions(models []string) []huh.Option[string] {
	opts := make([]huh.Option[string], 0, len(models))
	for _, m := range models {
		opts = append(opts, huh.NewOption(m, m))
	}
	return opts
}
