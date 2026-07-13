// Command memimport — разовый импортёр файловой памяти проекта (.claude/memory/*.md)
// в SQLite-хранилище memo (DESIGN.md §14, Этап 5 «Догфудинг», задача 5.1).
// Каждый файл — один атомарный факт, проходящий через тот же write path (дедуп,
// FTS5-индексация), что и обычный memory_save; [[wiki-ссылки]] между файлами
// восстанавливаются как links (rel=related) вторым проходом, когда id всех
// записей уже известны. Реальный Embedder не нужен: вектор для импортированных
// записей досчитает фоновый reindex/backfill в memo (DESIGN.md §7.4, §9).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"

	"agentmemory/internal/config"
	"agentmemory/internal/memory/embed"
	"agentmemory/internal/memory/store"
	"agentmemory/internal/memory/write"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		log.Fatalf("memimport: %v", err)
	}
}

// run разбирает флаги и выполняет импорт. Вынесен из main для тестируемости:
// main_test.go гоняет его напрямую с произвольными args и перехватом out.
func run(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("memimport", flag.ContinueOnError)
	dirFlag := fs.String("dir", ".claude/memory", "директория с файлами памяти (frontmatter + Markdown)")
	dbFlag := fs.String("db", "", "путь к файлу SQLite, переопределяет store.path из конфига (по умолчанию — из конфига)")
	scopeFlag := fs.String("scope", "project:agentmemory", "scope, в который импортируются записи")
	dryRun := fs.Bool("dry-run", false, "только напечатать, что было бы сделано, не писать в БД")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// dry-run не открывает Store вовсе — гарантия того, что он ничего не пишет
	// в БД, а не соглашение, которое можно случайно нарушить внутри Run.
	if *dryRun {
		sum, err := Run(context.Background(), nil, *dirFlag, *scopeFlag, true, out)
		if err != nil {
			return err
		}
		printSummary(out, sum)
		return nil
	}

	dbPath := *dbFlag
	if dbPath == "" {
		cfg, err := config.Load("")
		if err != nil {
			return fmt.Errorf("загрузка конфига: %w", err)
		}
		dbPath = cfg.Store.Path
	}

	s, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("открытие хранилища %q: %w", dbPath, err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			log.Printf("memimport: закрытие хранилища: %v", err)
		}
	}()

	w := write.New(s, embed.NewNoop())
	sum, err := Run(context.Background(), w, *dirFlag, *scopeFlag, false, out)
	if err != nil {
		return err
	}
	printSummary(out, sum)
	return nil
}

// printSummary печатает итог прогона в формате, требуемом задачей 5.1:
// "imported N, updated-with-links M, skipped-as-duplicate K", затем по одному
// предупреждению на строку.
func printSummary(out io.Writer, sum Summary) {
	fmt.Fprintf(out, "imported %d, updated-with-links %d, skipped-as-duplicate %d\n",
		sum.Imported, sum.UpdatedWithLinks, sum.SkippedDuplicate)
	for _, w := range sum.Warnings {
		fmt.Fprintf(out, "warning: %s\n", w)
	}
}
