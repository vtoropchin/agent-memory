// Command memoryd — точка входа AgentMemory: MCP-сервер долговременной памяти
// поверх SQLite, обслуживающий инструменты save/recall/update/forget по stdio
// (DESIGN.md §3, §12).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

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

func main() {
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
	dotenvDir := "."
	if *configPath != "" {
		dotenvDir = filepath.Dir(*configPath)
	}
	dotenvPath := filepath.Join(dotenvDir, ".env")
	if loaded, err := loadDotEnv(dotenvPath); err != nil {
		log.Printf("memoryd: загрузка %s: %v", dotenvPath, err)
	} else if loaded {
		log.Printf("memoryd: переменные окружения подхвачены из %s", dotenvPath)
	}

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
			log.Printf("memoryd: reasoner.provider=deepseek, но %s пуст — работаю без Reasoner (Noop)", deepseekAPIKeyEnv)
			return reason.NewNoop(), nil
		}
		if cfg.Model == "" {
			log.Printf("memoryd: reasoner.provider=deepseek, но reasoner.model пуст — работаю без Reasoner (Noop)")
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
// доступность сервера и модели. Эта проверка — не про конфигурацию, а про внешний
// сервис: сервер обязан стартовать в любом случае (DESIGN.md §15), поэтому ошибка
// здесь — предупреждение в stderr и откат на Noop, а не отказ в старте. Неизвестный
// provider — ошибка при старте, это опечатка в конфиге, а не runtime-деградация.
func newEmbedder(ctx context.Context, cfg config.EmbedderConfig) (embed.Embedder, error) {
	switch cfg.Provider {
	case "", "noop":
		return embed.NewNoop(), nil
	case "ollama":
		e, err := embed.NewOllama(ctx, cfg.BaseURL, cfg.Model)
		if err != nil {
			log.Printf("memoryd: embedder.provider=ollama, но сервер недоступен или модель не подтянута (%v) — работаю без Embedder (Noop, recall на FTS5)", err)
			return embed.NewNoop(), nil
		}
		return e, nil
	default:
		return nil, fmt.Errorf("неизвестный embedder.provider %q", cfg.Provider)
	}
}
