// wizard.go — интерактивный TUI-промпт `memo init` (задача 11.1): select
// стрелками/Enter + скрытый ввод для ключей, поверх github.com/charmbracelet/huh
// (спайк-решение — см. "Решение" в tasks/11.1-init-cobra-provider-setup.md).
// Вызывается ТОЛЬКО когда isInteractiveStdin() уже подтвердил, что stdin —
// реальный терминал (runInit, init.go); это единственный уровень "graceful
// фолбэка при не-TTY" — huh/bubbletea сами по себе не вызываются в пайпе или
// под -no-input, поэтому их поведение без TTY здесь не имеет значения. Каждое
// поле — тонкая обёртка над уже протестированными без TUI функциями
// (fetchOllamaModels/fetchOpenAIEmbeddingModels — providers.go;
// resolveOllamaEmbedState/classifyOllamaEmbedModels — ollama.go); сам виджет
// не юнит-тестируется (библиотека), тестируется только плумбинг под ним.
package main

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strconv"
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

	// Петля вокруг выбора провайдера эмбеддинга + его настройки: ollama-ветка
	// (promptOllamaEmbedder) может попросить "выбрать другую настройку"
	// (reselect=true) при недоступности/отсутствии ollama — тогда сбрасываем
	// выбор провайдера и модель и возвращаемся к select провайдера (задача
	// 11.1, цикл исправлений). openai/noop не запрашивают reselect —
	// однократный проход.
	for {
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

		reselect := false
		switch cfg.EmbedderProvider {
		case "ollama":
			r, err := promptOllamaEmbedder(ctx, out, &cfg)
			if err != nil {
				return cfg, err
			}
			reselect = r
		case "openai":
			if err := promptOpenAIEmbedder(ctx, out, &cfg); err != nil {
				return cfg, err
			}
		}

		if !reselect {
			break
		}
		cfg.EmbedderProvider = ""
		cfg.EmbedderModel = ""
		cfg.EmbedderBaseURL = ""
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

	if cfg.ReasonerProvider == "deepseek" {
		if cfg.DeepseekKey == "" {
			key := ""
			if err := huh.NewInput().
				Title("DeepSeek API key (Enter — пропустить, указать позже в .env)").
				Password(true).
				Value(&key).Run(); err != nil {
				return cfg, err
			}
			cfg.DeepseekKey = key
		}
		if err := promptDeepSeekReasoner(ctx, out, &cfg); err != nil {
			return cfg, err
		}
	}

	// Петля вокруг выбора бэкенда хранилища + его настройки — по образцу петли
	// эмбеддера выше: postgres-ветка (promptPostgresStore) может попросить
	// "выбрать другую настройку" (reselect=true) при недоступности сервера,
	// тогда сбрасываем драйвер вместе со всеми postgres-полями и возвращаемся к
	// select бэкенда (в том числе чтобы можно было уйти на sqlite). sqlite —
	// однократный проход, ровно тот же вопрос про путь к файлу, что и раньше.
	for {
		if cfg.StoreDriver == "" {
			driver := storeDriverSQLite
			if err := huh.NewSelect[string]().
				Title("Хранилище памяти").
				Options(providerSelectOptions(storeDrivers)...).
				Value(&driver).Run(); err != nil {
				return cfg, err
			}
			cfg.StoreDriver = driver
		}

		if cfg.StoreDriver != storeDriverPostgres {
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
			break
		}

		reselect, err := promptPostgresStore(out, &cfg)
		if err != nil {
			return cfg, err
		}
		if !reselect {
			break
		}
		cfg.StoreDriver = ""
		clearPostgresFields(&cfg)
	}

	// Ambient-режим через хуки харнесса (задача 7.2) — спрашиваем только у
	// харнессов, которые их умеют (agentTarget.hooksLabel: Claude Code и dsh —
	// второй через мост dsh-hooks-claude-code); у opencode хуков нет, его
	// scaffold сам честно сообщит об этом в выводе.
	if target, ok := agentRegistry[cfg.Agent]; ok && target.hooksLabel != "" && cfg.Hooks == "" {
		hooks := hooksSessionStart
		if err := huh.NewSelect[string]().
			Title(fmt.Sprintf("Подключить автоматический recall через %s (ambient-режим)", target.hooksLabel)).
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

// promptOllamaEmbedder спрашивает base URL ollama (если не задан), затем в
// петле диагностирует окружение (resolveOllamaEmbedState, ollama.go) и
// различает 4 состояния (задача 11.1, цикл исправлений):
//   - happy — показывает select ТОЛЬКО из распознанных моделей эмбеддинга
//     (рекомендованные первыми, первая преселекчена), пишет выбор в
//     cfg.EmbedderModel и возвращает (false, nil);
//   - no-models/not-running/not-installed — печатает в out рекомендации/
//     инструкцию под текущую ОС, затем меню из 2 пунктов: "Готово,
//     продолжаем" повторяет пробу (следующая итерация петли), "Выбрать
//     другую настройку" возвращает (true, nil) — сигнал promptInitTUI
//     сбросить провайдера эмбеддинга и вернуться к его select.
//
// Если cfg.EmbedderModel уже задан (флагом) — модель повторно не
// спрашивается, функция возвращает (false, nil) сразу после base URL.
func promptOllamaEmbedder(ctx context.Context, out io.Writer, cfg *initConfig) (reselect bool, err error) {
	if cfg.EmbedderBaseURL == "" {
		base := defaultOllamaBaseURL
		if err := huh.NewInput().Title("ollama base URL").Value(&base).Run(); err != nil {
			return false, err
		}
		if base == "" {
			base = defaultOllamaBaseURL
		}
		cfg.EmbedderBaseURL = base
	}
	if cfg.EmbedderModel != "" {
		return false, nil
	}

	for {
		state, models, fetchErr := resolveOllamaEmbedState(ctx, cfg.EmbedderBaseURL, exec.LookPath)
		switch state {
		case ollamaStateHappy:
			model := models[0]
			if err := huh.NewSelect[string]().
				Title("модель эмбеддингов ollama (установленные)").
				Options(toModelOptions(models)...).
				Value(&model).Run(); err != nil {
				return false, err
			}
			cfg.EmbedderModel = model
			return false, nil
		case ollamaStateNoModels:
			printOllamaNoModelsHelp(out, models)
		case ollamaStateNotRunning:
			printOllamaNotRunningHelp(out, fetchErr, runtime.GOOS)
		default: // ollamaStateNotInstalled
			printOllamaNotInstalledHelp(out, fetchErr, runtime.GOOS)
		}

		const (
			menuRetry    = "retry"
			menuReselect = "reselect"
		)
		action := menuRetry
		if err := huh.NewSelect[string]().
			Title("Что дальше?").
			Options(
				huh.NewOption("Готово, продолжаем", menuRetry),
				huh.NewOption("Выбрать другую настройку", menuReselect),
			).
			Value(&action).Run(); err != nil {
			return false, err
		}
		if action == menuReselect {
			return true, nil
		}
		// menuRetry — назад к пере-пробе resolveOllamaEmbedState.
	}
}

// promptPostgresStore спрашивает параметры подключения к общей базе команды
// (promptPostgresFields) и тут же проверяет их пробой — по образцу
// promptOllamaEmbedder: молча сгенерировать конфиг, с которым memo заведомо не
// стартует, визард не должен. Проба — read-only (probeStoreConnection, init.go:
// store.OpenPostgresReadOnly, а не store.Open — почему именно так, см. её
// комментарий): `memo init` не имеет права накатить прод-схему памяти в общую
// командную базу просто потому, что кто-то проверял доступность сервера.
//
// Неудача — причина в out и меню из 2 пунктов: "Ввести параметры заново"
// (сбрасывает postgres-поля, следующая итерация спросит их с нуля) и "Выбрать
// другую настройку" — возврат (true, nil), по которому promptInitTUI вернётся к
// select бэкенда (в том числе на sqlite).
//
// Предупреждения о готовности БД (первый элемент возврата probeStoreConnection
// — pgvector/право CREATE, задача 12.1), в отличие от ошибки доступности,
// здесь намеренно не печатаются: doInit (init.go), вызываемый после визарда с
// теми же параметрами, пробует подключение заново и печатает их сам — это
// единственная точка их печати что в интерактивном, что в -no-input прогоне,
// что и защищает от дублирования одного и того же предупреждения дважды в
// рамках одного интерактивного прогона.
func promptPostgresStore(out io.Writer, cfg *initConfig) (reselect bool, err error) {
	reask := false
	for {
		if err := promptPostgresFields(cfg, reask); err != nil {
			return false, err
		}

		storeCfg := storeConfigFromInit(*cfg)
		_, probeErr := probeStoreConnection(storeCfg)
		if probeErr == nil {
			fmt.Fprintf(out, "подключение к %s проверено\n", storeCfg.Target())
			return false, nil
		}

		fmt.Fprintf(out, "не удалось подключиться к %s (%v).\n", storeCfg.Target(), probeErr)
		fmt.Fprintln(out, "проверь, что сервер доступен по сети (listen_addresses/pg_hba.conf), что база и роль созданы и что пароль верный — см. раздел «Общая база команды» в README.")

		const (
			menuRetry    = "retry"
			menuReselect = "reselect"
		)
		action := menuRetry
		if err := huh.NewSelect[string]().
			Title("Что дальше?").
			Options(
				huh.NewOption("Ввести параметры подключения заново", menuRetry),
				huh.NewOption("Выбрать другую настройку", menuReselect),
			).
			Value(&action).Run(); err != nil {
			return false, err
		}
		if action == menuReselect {
			return true, nil
		}
		// Повторный проход спрашивает ВСЕ поля заново, но с уже введёнными
		// значениями в качестве дефолтов: ошибся в одном пароле — не должен
		// перенабирать host/user/dbname. Поэтому сбрасывается сам факт «поле
		// уже заполнено, спрашивать не надо», а не значения.
		reask = true
	}
}

// promptPostgresFields спрашивает поля подключения к PostgreSQL. Уже заданные
// (флагом либо предыдущей итерацией) при reask=false не переспрашиваются — тот
// же принцип, что во всём визарде; reask=true (повтор после неудачной пробы)
// спрашивает всё, подставляя ранее введённое как дефолт промпта. host/user/dbname
// обязаны быть непусты (то же правило, что проверит config.StoreConfig.Validate в
// doInit), поэтому у host стоит Validate на непустоту, а user/dbname/port/sslmode
// приходят с дефолтами. Пароль — отдельный вопрос со скрытым вводом
// (Password(true)), как ключи провайдеров; пустой пароль допустим
// (trust-аутентификация) и просто не попадёт в .env.
func promptPostgresFields(cfg *initConfig, reask bool) error {
	if reask || cfg.PostgresHost == "" {
		host := cfg.PostgresHost
		if err := huh.NewInput().
			Title("Адрес сервера PostgreSQL (напр. postgres.lan)").
			Validate(func(s string) error {
				if strings.TrimSpace(s) == "" {
					return fmt.Errorf("адрес сервера не может быть пустым")
				}
				return nil
			}).
			Value(&host).Run(); err != nil {
			return err
		}
		cfg.PostgresHost = strings.TrimSpace(host)
	}

	if reask || cfg.PostgresPort == 0 {
		port := strconv.Itoa(defaultPostgresPort)
		if cfg.PostgresPort != 0 {
			port = strconv.Itoa(cfg.PostgresPort)
		}
		if err := huh.NewInput().
			Title("Порт PostgreSQL").
			Validate(func(s string) error {
				_, err := parsePostgresPortInput(s)
				return err
			}).
			Value(&port).Run(); err != nil {
			return err
		}
		// Тот же парсер, что стоит в Validate выше — второй ветки разбора (а с
		// ней и недостижимой обработки ошибки) в коде нет.
		parsed, err := parsePostgresPortInput(port)
		if err != nil {
			return err
		}
		cfg.PostgresPort = parsed
	}

	if reask || cfg.PostgresUser == "" {
		user := cmp.Or(cfg.PostgresUser, defaultPostgresUser)
		if err := huh.NewInput().Title("Роль PostgreSQL").Value(&user).Run(); err != nil {
			return err
		}
		cfg.PostgresUser = cmp.Or(strings.TrimSpace(user), defaultPostgresUser)
	}

	if reask || cfg.PostgresDBName == "" {
		dbname := cmp.Or(cfg.PostgresDBName, defaultPostgresDBName)
		if err := huh.NewInput().Title("Имя базы PostgreSQL").Value(&dbname).Run(); err != nil {
			return err
		}
		cfg.PostgresDBName = cmp.Or(strings.TrimSpace(dbname), defaultPostgresDBName)
	}

	if reask || cfg.PostgresSSLMode == "" {
		sslmode := cmp.Or(cfg.PostgresSSLMode, defaultPostgresSSLMode)
		if err := huh.NewInput().
			Title("Режим TLS (sslmode: disable, require, verify-full, ...)").
			Value(&sslmode).Run(); err != nil {
			return err
		}
		cfg.PostgresSSLMode = cmp.Or(strings.TrimSpace(sslmode), defaultPostgresSSLMode)
	}

	if reask || cfg.PostgresPassword == "" {
		password := cfg.PostgresPassword
		if err := huh.NewInput().
			Title("Пароль PostgreSQL (Enter — пропустить: trust-аутентификация или указать позже в .env)").
			Password(true).
			Value(&password).Run(); err != nil {
			return err
		}
		cfg.PostgresPassword = password
	}

	return nil
}

// clearPostgresFields сбрасывает все поля подключения к PostgreSQL, включая
// пароль — чтобы они не утекли в конфиг при уходе на другой бэкенд (пункт меню
// "Выбрать другую настройку"). Повтор ввода после неудачной пробы значения НЕ
// стирает — см. reask в promptPostgresStore/promptPostgresFields.
func clearPostgresFields(cfg *initConfig) {
	cfg.PostgresHost = ""
	cfg.PostgresPort = 0
	cfg.PostgresUser = ""
	cfg.PostgresDBName = ""
	cfg.PostgresSSLMode = ""
	cfg.PostgresPassword = ""
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
		return promptModelManual(&cfg.EmbedderModel, "", "модель эмбеддингов OpenAI")
	}

	fetchCtx, cancel := context.WithTimeout(ctx, fetchModelsTimeout)
	models, err := fetchOpenAIEmbeddingModels(fetchCtx, cfg.EmbedderBaseURL, cfg.OpenAIKey)
	cancel()
	if err != nil {
		fmt.Fprintf(out, "не удалось получить список моделей OpenAI (%v) — введи имя модели вручную\n", err)
		return promptModelManual(&cfg.EmbedderModel, "", "модель эмбеддингов OpenAI")
	}
	if len(models) == 0 {
		fmt.Fprintln(out, "OpenAI не вернул ни одной модели — введи имя модели вручную")
		return promptModelManual(&cfg.EmbedderModel, "", "модель эмбеддингов OpenAI")
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

// promptDeepSeekReasoner спрашивает модель reasoner DeepSeek — по образцу
// promptOpenAIEmbedder, но отвечает ТОЛЬКО за выбор модели: ключ (и, в отличие
// от OpenAI-эмбеддера, base URL) запрашивать не нужно — ключ уже взят в
// promptInitTUI, а base URL у reasoner не настраивается (см. defaultDeepSeekBaseURL
// в init.go). Логика выбора: если модель уже задана (флагом) — не переспрашивает;
// если ключ не введён (пропущен) — сеть не дёргаем, сразу ручной ввод с
// дефолтом defaultDeepSeekModel; иначе запрашивает список через
// fetchDeepSeekModels (таймаут fetchModelsTimeout) — ошибка запроса или
// пустой список тоже откатываются на ручной ввод с тем же дефолтом, успех —
// select из полученных моделей (первая преселекчена).
func promptDeepSeekReasoner(ctx context.Context, out io.Writer, cfg *initConfig) error {
	if cfg.ReasonerModel != "" {
		return nil
	}
	if cfg.DeepseekKey == "" {
		fmt.Fprintln(out, "ключ не введён — список моделей запросить нечем, введи имя модели вручную")
		return promptModelManual(&cfg.ReasonerModel, defaultDeepSeekModel, "модель reasoner (DeepSeek)")
	}

	fetchCtx, cancel := context.WithTimeout(ctx, fetchModelsTimeout)
	models, err := fetchDeepSeekModels(fetchCtx, defaultDeepSeekBaseURL, cfg.DeepseekKey)
	cancel()
	if err != nil {
		fmt.Fprintf(out, "не удалось получить список моделей DeepSeek (%v) — введи имя модели вручную\n", err)
		return promptModelManual(&cfg.ReasonerModel, defaultDeepSeekModel, "модель reasoner (DeepSeek)")
	}
	if len(models) == 0 {
		fmt.Fprintln(out, "DeepSeek не вернул ни одной модели — введи имя модели вручную")
		return promptModelManual(&cfg.ReasonerModel, defaultDeepSeekModel, "модель reasoner (DeepSeek)")
	}

	model := models[0]
	if err := huh.NewSelect[string]().
		Title("модель reasoner DeepSeek").
		Options(toModelOptions(models)...).
		Value(&model).Run(); err != nil {
		return err
	}
	cfg.ReasonerModel = model
	return nil
}

// promptModelManual — ручной ввод имени модели в *dst с дефолтом def и
// заголовком title промпта. Пустой def (нет естественного дефолта, напр.
// OpenAI без списка) требует непустого ввода — иначе agentmemory.toml
// получил бы модель "".
func promptModelManual(dst *string, def, title string) error {
	val := def
	input := huh.NewInput().Title(title)
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
