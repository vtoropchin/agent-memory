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
	"agentmemory/internal/memory/recall"
	"agentmemory/internal/memory/store"
	"agentmemory/internal/memory/write"
)

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

	w := write.New(s)
	r := recall.New(s, cfg.Recall)
	srv := mcp.NewServer(w, r, s)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Printf("memoryd: запуск MCP-сервера (stdio), store=%s", cfg.Store.Path)
	if err := srv.Run(ctx); err != nil {
		return fmt.Errorf("работа сервера: %w", err)
	}
	return nil
}
