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
	"syscall"

	"agentmemory/internal/config"
	"agentmemory/internal/mcp"
	"agentmemory/internal/memory/reason"
	"agentmemory/internal/memory/recall"
	"agentmemory/internal/memory/store"
	"agentmemory/internal/memory/write"
)

// deepseekAPIKeyEnv — переменная окружения с ключом DeepSeek (DESIGN.md §10). Ключ
// никогда не читается из конфиг-файла и не логируется.
const deepseekAPIKeyEnv = "DEEPSEEK_API_KEY"

func main() {
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

	reasoner, err := newReasoner(cfg.Reasoner)
	if err != nil {
		return fmt.Errorf("создание reasoner: %w", err)
	}

	w := write.New(s)
	r := recall.New(s, cfg.Recall, reasoner)
	srv := mcp.NewServer(w, r, s, cfg.Scope)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

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
