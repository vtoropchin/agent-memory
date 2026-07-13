// Command memoryd — точка входа AgentMemory: MCP-сервер долговременной памяти
// поверх SQLite, обслуживающий инструменты save/recall/update/forget по stdio
// (DESIGN.md §3, §12).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"

	"agentmemory/internal/config"
	"agentmemory/internal/mcp"
	"agentmemory/internal/memory/embed"
	"agentmemory/internal/memory/maint"
	"agentmemory/internal/memory/reason"
	"agentmemory/internal/memory/recall"
	"agentmemory/internal/memory/store"
	"agentmemory/internal/memory/write"
)

// deepseekAPIKeyEnv — переменная окружения с ключом DeepSeek (DESIGN.md §10). Ключ
// никогда не читается из конфиг-файла и не логируется.
const deepseekAPIKeyEnv = "DEEPSEEK_API_KEY"

// openaiAPIKeyEnv — переменная окружения с ключом OpenAI-совместимого эмбеддера
// (DESIGN.md §13, то же правило, что для DEEPSEEK_API_KEY). Ключ никогда не
// читается из конфиг-файла и не логируется.
const openaiAPIKeyEnv = "OPENAI_API_KEY"

// openaiDefaultBaseURL — базовый URL, подставляемый, когда embedder.base_url пуст
// при provider="openai" (DESIGN.md §9: openai — это не только api.openai.com, но
// любой /v1/embeddings-совместимый сервер; дефолт — сам OpenAI).
const openaiDefaultBaseURL = "https://api.openai.com"

// ollamaDefaultBaseURL — базовый URL, подставляемый, когда embedder.base_url пуст
// при provider="ollama". Дефолт провайдер-специфичен, поэтому не хранится в
// config.Default() (там EmbedderConfig.BaseURL всегда "") — иначе он остался бы
// значением по умолчанию и при provider="openai" без явного base_url в TOML.
const ollamaDefaultBaseURL = "http://localhost:11434"

func main() {
	// Справка верхнего уровня — перехватывается до диспетчеризации init/web и
	// до log.SetOutput(os.Stderr): сервер ещё не стартовал, поэтому печать в
	// stdout здесь безопасна и не портит протокол MCP.
	if len(os.Args) >= 2 && isHelpArg(os.Args[1]) {
		writeUsage(os.Stdout)
		return
	}

	// `memoryd init` — скаффолдинг .mcp.json + agentmemory.toml (задача 5.3),
	// диспетчеризуется до разбора серверных флагов и работы с конфигом/store,
	// поэтому `memoryd`, `memoryd -config ...`, `memoryd -db ...` не затронуты.
	if len(os.Args) >= 2 && os.Args[1] == "init" {
		if err := runInit(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "memoryd init: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// `memoryd web` — read-only HTTP-морда просмотра памяти (задача 6.2),
	// диспетчеризуется симметрично `init`, до разбора серверных флагов и
	// работы с reasoner/embedder: морда только читает store лексически.
	if len(os.Args) >= 2 && os.Args[1] == "web" {
		if err := runWeb(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "memoryd web: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// `memoryd recall` / `memoryd save` — one-shot CLI поверх read/write path
	// (задача 7.1), диспетчеризуются симметрично init/web, до разбора серверных
	// флагов. В отличие от web, им нужны reasoner/embedder (newReasoner/newEmbedder
	// из этого файла) — как и серверному режиму, деградация обоих до Noop уходит
	// предупреждением в stderr (log.Printf пишет в os.Stderr по умолчанию, здесь
	// это ещё не переопределялось), а не в stdout, куда пишется только результат.
	if len(os.Args) >= 2 && os.Args[1] == "recall" {
		if err := runRecall(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "memoryd recall: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) >= 2 && os.Args[1] == "save" {
		if err := runSave(os.Args[2:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "memoryd save: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// `memoryd doctor` — read-only диагностика режима работы памяти (задача
	// 9.1), диспетчеризуется симметрично recall/save, до разбора серверных
	// флагов. Код выхода задаётся самим отчётом (0 здоров/деградирован, 1
	// сломан), а не универсальным os.Exit(1) остальных подкоманд — поэтому
	// runDoctor возвращает exitCode отдельно от err (err — только ошибки,
	// из-за которых отчёт вообще нельзя построить: битые флаги/конфиг).
	if len(os.Args) >= 2 && os.Args[1] == "doctor" {
		code, err := runDoctor(os.Args[2:], os.Stdout)
		if err != nil {
			fmt.Fprintf(os.Stderr, "memoryd doctor: %v\n", err)
			os.Exit(1)
		}
		os.Exit(code)
	}

	// `memoryd hook` — обёртка над recall-путём для хуков Claude Code
	// (SessionStart/UserPromptSubmit, задача 7.2). В отличие от recall/save,
	// ошибка НЕ приводит к os.Exit(1) — только к логу в stderr: хук обязан
	// завершаться нулевым кодом при любой ошибке (нет конфига/БД, битый
	// stdin), иначе он мог бы сломать старт или ход сессии агента.
	if len(os.Args) >= 2 && os.Args[1] == "hook" {
		if err := runHook(os.Args[2:], os.Stdin, os.Stdout); err != nil {
			log.Printf("memoryd hook: %v", err)
		}
		return
	}

	// Транспорт MCP использует stdout для протокола — всё логирование только в
	// stderr, иначе лог-строки повредят JSON-RPC поток.
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags)

	if err := run(); err != nil {
		log.Fatalf("memoryd: %v", err)
	}
}

func run() error {
	configPath := flag.String("config", "", "путь к TOML-конфигу (опционально)")
	dbPath := flag.String("db", "", "путь к файлу SQLite, переопределяет store.path из конфига (опционально)")
	flag.Parse()

	// .env рядом с конфигом (или в текущем каталоге, если -config не задан) —
	// раскладывается `memoryd init` (задача 5.4). Загрузить ДО newReasoner,
	// иначе DEEPSEEK_API_KEY туда не долетит. Значение ключа в лог не попадает.
	loadDotEnvForConfig(*configPath, "memoryd")

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("загрузка конфига: %w", err)
	}
	if *dbPath != "" {
		cfg.Store.Path = *dbPath
	}

	s, err := store.Open(cfg.Store.Path)
	if err != nil {
		return fmt.Errorf("открытие хранилища %q: %w", cfg.Store.Path, err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			log.Printf("memoryd: закрытие хранилища: %v", err)
		}
	}()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	reasoner, err := newReasoner(cfg.Reasoner)
	if err != nil {
		return fmt.Errorf("создание reasoner: %w", err)
	}

	embedder, err := newEmbedder(ctx, cfg.Embedder)
	if err != nil {
		return fmt.Errorf("создание embedder: %w", err)
	}

	w := write.New(s, embedder)
	r := recall.New(s, cfg.Recall, reasoner, embedder)
	srv := mcp.NewServer(w, r, s, cfg.Scope)

	// decay/consolidate/dedup-sweep/reindex по расписанию (DESIGN.md §11, §13):
	// интервал и возраст консолидации уже провалидированы в config.Load, поэтому
	// Durations() здесь не может вернуть ошибку — но всё равно проверяем её, а не
	// игнорируем возврат. Scheduler.Run вызывает первый прогон немедленно и сам
	// достаточен для досчёта векторов на старте (последний шаг RunOnce —
	// CheckAndReindex, который no-op при Noop-embedder) — отдельной стартовой
	// reindex-горутины не нужно.
	decayInterval, consolidateAfter, err := cfg.Maint.Durations()
	if err != nil {
		return fmt.Errorf("разбор maint-таймингов: %w", err)
	}
	scheduler := maint.NewScheduler(
		maint.NewDecayer(s, w),
		maint.NewDedupSweeper(s),
		maint.NewConsolidator(s, reasoner, w),
		maint.NewReindexer(s, embedder),
		decayInterval, consolidateAfter,
	)
	go scheduler.Run(ctx)

	log.Printf("memoryd: запуск MCP-сервера (stdio), store=%s", cfg.Store.Path)
	if err := srv.Run(ctx); err != nil {
		return fmt.Errorf("работа сервера: %w", err)
	}
	return nil
}

// newReasoner создаёт Reasoner по конфигу. "" и "noop" отключают LLM-слой.
// "deepseek" без ключа в env или с пустой model деградирует до Noop (сервер
// должен стартовать — DESIGN.md §15): оба случая означают «LLM-слой не готов»,
// а не ошибку конфигурации. С ключом и model — создаёт DeepSeek-клиент.
// Неизвестный provider — ошибка при старте, это опечатка в конфиге, а не
// runtime-деградация.
func newReasoner(cfg config.ReasonerConfig) (reason.Reasoner, error) {
	switch cfg.Provider {
	case "", "noop":
		return reason.NewNoop(), nil
	case "deepseek":
		key := os.Getenv(deepseekAPIKeyEnv)
		if key == "" {
			log.Printf("memoryd: reasoner.provider=deepseek, но %s пуст — суммаризация отключена", deepseekAPIKeyEnv)
			return reason.NewNoop(), nil
		}
		if cfg.Model == "" {
			log.Printf("memoryd: reasoner.provider=deepseek, но reasoner.model пуст — суммаризация отключена")
			return reason.NewNoop(), nil
		}
		return reason.NewDeepSeek(key, cfg.Model)
	default:
		return nil, fmt.Errorf("неизвестный reasoner.provider %q", cfg.Provider)
	}
}

// newEmbedder создаёт Embedder по конфигу (симметрично newReasoner). "" и "noop"
// отключают векторный слой — recall работает на чистом FTS5. "ollama" создаёт
// клиент к серверу ollama; конструктор делает пробный эмбеддинг, заодно проверяя
// доступность сервера и модели; пустой base_url в конфиге замещается на
// ollamaDefaultBaseURL. "openai" — то же самое поверх любого
// /v1/embeddings-совместимого сервера (OpenAI, OpenRouter, LM Studio, vLLM —
// DESIGN.md §9); ключ читается только из env OPENAI_API_KEY (симметрично
// DEEPSEEK_API_KEY, никогда не из TOML), пустой base_url в конфиге замещается на
// openaiDefaultBaseURL (api.openai.com). Оба дефолта провайдер-специфичны и живут
// здесь, а не в config.Default() — иначе один из них (обязательно ollama-специфичный
// localhost:11434) утёк бы во второй провайдер при пустом base_url в TOML. Ошибка
// на этапе создания клиента — не про конфигурацию, а про внешний сервис: сервер
// обязан стартовать в любом случае (DESIGN.md §15), поэтому ошибка здесь —
// предупреждение в stderr и откат на Noop, а не отказ в старте. Неизвестный
// provider — ошибка при старте, это опечатка в конфиге, а не runtime-деградация.
func newEmbedder(ctx context.Context, cfg config.EmbedderConfig) (embed.Embedder, error) {
	switch cfg.Provider {
	case "", "noop":
		return embed.NewNoop(), nil
	case "ollama":
		baseURL := cfg.BaseURL
		if baseURL == "" {
			baseURL = ollamaDefaultBaseURL
		}
		e, err := embed.NewOllama(ctx, baseURL, cfg.Model)
		if err != nil {
			log.Printf("memoryd: embedder.provider=ollama, но ollama недоступна или модель не загружена (%v) — векторный поиск отключён, recall на полнотекстовом", err)
			return embed.NewNoop(), nil
		}
		return e, nil
	case "openai":
		key := os.Getenv(openaiAPIKeyEnv)
		if key == "" {
			log.Printf("memoryd: embedder.provider=openai, но %s пуст — векторный поиск отключён, recall на полнотекстовом", openaiAPIKeyEnv)
			return embed.NewNoop(), nil
		}
		baseURL := cfg.BaseURL
		if baseURL == "" {
			baseURL = openaiDefaultBaseURL
		}
		e, err := embed.NewOpenAI(ctx, baseURL, cfg.Model, key)
		if err != nil {
			log.Printf("memoryd: embedder.provider=openai, но сервер недоступен или ключ/модель невалидны (%v) — векторный поиск отключён, recall на полнотекстовом", err)
			return embed.NewNoop(), nil
		}
		return e, nil
	default:
		return nil, fmt.Errorf("неизвестный embedder.provider %q", cfg.Provider)
	}
}

// isHelpArg проверяет, что arg — один из стандартных способов запросить
// справку верхнего уровня.
func isHelpArg(arg string) bool {
	switch arg {
	case "-h", "-help", "--help", "help":
		return true
	default:
		return false
	}
}

// writeUsage печатает справку верхнего уровня memoryd: что это, три формы
// запуска, флаги серверного режима и подкоманды. Вынесена из main для
// тестируемости.
func writeUsage(w io.Writer) {
	fmt.Fprintln(w, "memoryd — сервер долговременной памяти для ИИ-агентов (MCP поверх SQLite).")
	fmt.Fprintln(w)

	fmt.Fprintln(w, "Использование:")
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  memoryd [-config <path>] [-db <path>]\tзапуск MCP-сервера (режим по умолчанию, транспорт stdio)")
	fmt.Fprintln(tw, "  memoryd init [флаги]\tустановить память в проект (генерирует MCP-конфиг и скил)")
	fmt.Fprintln(tw, "  memoryd web [флаги]\tHTTP-морда для просмотра памяти в браузере")
	fmt.Fprintln(tw, "  memoryd recall [флаги] \"<запрос>\"\tразовый поиск по памяти (Markdown-бриф в stdout)")
	fmt.Fprintln(tw, "  memoryd save [флаги]\tразовое сохранение факта в память")
	fmt.Fprintln(tw, "  memoryd hook -event <name> [флаги]\tхук Claude Code (session-start|prompt-submit), JSON события на stdin")
	fmt.Fprintln(tw, "  memoryd doctor [флаги]\tread-only диагностика: в каком режиме реально работает память")
	tw.Flush()
	fmt.Fprintln(w)

	fmt.Fprintln(w, "Режим сервера (по умолчанию):")
	tw = tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  -config <path>\tпуть к TOML-конфигу (опционально)")
	fmt.Fprintln(tw, "  -db <path>\tпуть к файлу SQLite, переопределяет store.path из конфига")
	tw.Flush()
	fmt.Fprintln(w, "  ключ DeepSeek берётся из переменной окружения DEEPSEEK_API_KEY или из .env")
	fmt.Fprintln(w, "  рядом с конфигом; вектор (ollama) опционален, при недоступности recall")
	fmt.Fprintln(w, "  работает на полнотекстовом поиске.")
	fmt.Fprintln(w)

	fmt.Fprintln(w, "Подкоманды:")
	tw = tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  init\tустановщик памяти в проект (агентское приложение, ollama, DeepSeek-ключ)")
	fmt.Fprintln(tw, "  web\tHTTP-морда для просмотра и поиска памяти")
	fmt.Fprintln(tw, "  recall\tодноразовый recall из shell/скрипта/хука агента (флаг -quiet — для хуков)")
	fmt.Fprintln(tw, "  save\tодноразовый save из shell/скрипта; -content - читает факт из stdin")
	fmt.Fprintln(tw, "  hook\tхук-режим для .claude/settings.json (см. `memoryd init`): любая ошибка → пустой stdout, exit 0")
	fmt.Fprintln(tw, "  doctor\tдиагностика: конфиг/БД/записи/вектор/embedder/reasoner одной командой, ничего не пишет")
	tw.Flush()
	fmt.Fprintln(w, "  memoryd init -h, memoryd web -h, memoryd recall -h, memoryd save -h, memoryd hook -h, memoryd doctor -h — подробности по флагам каждой.")
}
