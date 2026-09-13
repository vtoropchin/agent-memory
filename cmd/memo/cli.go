// Подкоманды `memo recall` / `memo save` — одноразовые CLI-обёртки над
// read/write path памяти (DESIGN.md §12, задача 7.1). В отличие от MCP-режима
// (стартует раз на сессию агента и живёт демоном) и `memo web` (read-only
// HTTP), это one-shot вызовы из терминала/скрипта — прежде всего для хуков
// агентского харнесса (SessionStart/UserPromptSubmit, задача 7.2), которым
// нужно дёрнуть память одной командой без поднятия MCP-транспорта. Один
// прогон = открыть Store → выполнить → Close; фоновый maintenance-планировщик
// (maint.Scheduler) здесь не запускается — это не демон.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"strings"

	"agentmemory/internal/config"
	"agentmemory/internal/memory"
	"agentmemory/internal/memory/recall"
	"agentmemory/internal/memory/write"
)

// runRecall разбирает флаги подкоманды `recall` и выполняет один Recall,
// печатая готовый бриф в out. Вынесена из диспетчера в main.go для
// тестируемости (по образцу runInit/runWeb). out — единственное, куда пишется
// результат (Markdown или JSON); предупреждения о деградации reasoner/embedder
// (newReasoner/newEmbedder) уходят через log.Printf в stderr по умолчанию.
func runRecall(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("recall", flag.ExitOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fmt.Fprintln(out, "разовый поиск по памяти: печатает готовый Markdown-бриф в stdout.")
		fmt.Fprintln(out, "Использование: memo recall [флаги] \"<запрос>\"")
		fs.PrintDefaults()
	}
	configPath := fs.String("config", "", "путь к TOML-конфигу или каталогу проекта с agentmemory.toml (опционально; если задан — обязан существовать)")
	dbPath := fs.String("db", "", "путь к файлу SQLite, переопределяет store.path из конфига (опционально)")
	scopesFlag := fs.String("scopes", "", "пространства через запятую (пусто — дефолт инстанса из [scope])")
	kindsFlag := fs.String("kinds", "", "типы через запятую: semantic,episodic,procedural,debt,task (пусто — все)")
	statusesFlag := fs.String("statuses", "", "состояния задач через запятую: todo,doing,blocked,done (пусто — все записи, кроме выполненных задач)")
	budget := fs.Int("budget", 0, "бюджет токенов брифа (0 — дефолт из [recall])")
	jsonOut := fs.Bool("json", false, "печатать сырой JSON вместо Markdown")
	quiet := fs.Bool("quiet", false, "при пустой выдаче — пустой stdout и exit 0 (для хуков)")
	summarize := fs.Bool("summarize", false, "суммировать бриф через Reasoner (по умолчанию выключено — латентность хука)")
	_ = fs.Parse(args) // flag.ExitOnError сам завершает процесс при ошибке разбора/-h

	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		return fmt.Errorf("recall: запрос не задан — memo recall \"<запрос>\"")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("загрузка конфига: %w", err)
	}
	if *dbPath != "" {
		cfg.Store.Path = *dbPath
	}
	loadDotEnvForConfig(*configPath, "memo recall")

	ctx := context.Background()

	// Разрешение scope — той же логикой, что resolveRecallScopes в internal/mcp
	// (приоритет явного набора над дефолтом инстанса): здесь единственный явный
	// источник — флаг -scopes, поэтому дублировать back-compat-ветку с
	// одиночным полем Scope (актуальную только для входа MCP-инструмента) не
	// нужно — сам дефолт всё равно берётся из общего config.ScopeConfig.RecallScopes().
	scopes := splitCSV(*scopesFlag)
	if len(scopes) == 0 {
		scopes = cfg.Scope.RecallScopes()
	}

	brief, err := doRecall(ctx, cfg, "memo recall", memory.Query{
		Text:        query,
		Scopes:      scopes,
		Kinds:       splitCSV(*kindsFlag),
		Statuses:    splitCSV(*statusesFlag),
		TokenBudget: *budget,
		Summarize:   *summarize,
	})
	if err != nil {
		return fmt.Errorf("recall: %w", err)
	}

	if *quiet && brief.Count == 0 {
		return nil
	}

	if *jsonOut {
		enc := json.NewEncoder(out)
		if err := enc.Encode(brief); err != nil {
			return fmt.Errorf("recall: сериализация JSON: %w", err)
		}
		return nil
	}
	fmt.Fprintln(out, brief.Markdown)
	return nil
}

// doRecall выполняет один Recall над уже загруженным cfg: открывает
// хранилище, создаёт reasoner/embedder через newReasoner/newEmbedder (та же
// деградация до Noop, что у серверного режима — предупреждения только в
// stderr), выполняет q и закрывает хранилище перед возвратом. logPrefix —
// префикс сообщения в лог о закрытии хранилища (у recall/hook он разный).
// Общий приватный хелпер для `memo recall` (runRecall выше) и `memo
// hook` (runHook, задача 7.2, hook.go) — единственная точка, открывающая
// store+reasoner+embedder для recall-пути в CLI-режиме; caller сам решает,
// что делать с cfg.Store.Path (напр. runRecall применяет поверх него -db) и с
// Query.Scopes (recall — флаг -scopes, hook — дефолт инстанса напрямую).
// maint-планировщик здесь не запускается — это не демон.
func doRecall(ctx context.Context, cfg config.Config, logPrefix string, q memory.Query) (memory.Brief, error) {
	s, err := openStore(cfg.Store)
	if err != nil {
		return memory.Brief{}, fmt.Errorf("открытие хранилища %q: %w", cfg.Store.Target(), err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			log.Printf("%s: закрытие хранилища: %v", logPrefix, err)
		}
	}()

	reasoner, err := newReasoner(cfg.Reasoner)
	if err != nil {
		return memory.Brief{}, fmt.Errorf("создание reasoner: %w", err)
	}
	embedder, err := newEmbedder(ctx, cfg.Embedder)
	if err != nil {
		return memory.Brief{}, fmt.Errorf("создание embedder: %w", err)
	}

	r := recall.New(s, cfg.Recall, reasoner, embedder)
	return r.Recall(ctx, q)
}

// runSave разбирает флаги подкоманды `save` и выполняет один Save, печатая
// одну строку результата в out (`saved <id>` либо `merged <id>` — по образцу
// вывода memimport). Вынесена из диспетчера в main.go для тестируемости.
// stdin читается только когда -content равен "-".
func runSave(args []string, stdin io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("save", flag.ExitOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fmt.Fprintln(out, "разовое сохранение факта в память.")
		fmt.Fprintln(out, "Использование: memo save -kind <kind> -content <текст|-> [флаги]")
		fs.PrintDefaults()
	}
	configPath := fs.String("config", "", "путь к TOML-конфигу или каталогу проекта с agentmemory.toml (опционально; если задан — обязан существовать)")
	dbPath := fs.String("db", "", "путь к файлу SQLite, переопределяет store.path из конфига (опционально)")
	kindFlag := fs.String("kind", "", "тип факта: semantic|episodic|procedural|debt|task (обязателен)")
	contentFlag := fs.String("content", "", "текст факта (обязателен); \"-\" — читать из stdin")
	summaryFlag := fs.String("summary", "", "краткая форма для брифа; для -kind task это заголовок задачи и он обязателен")
	statusFlag := fs.String("status", "", "состояние задачи: todo|doing|blocked|done (только для -kind task; пусто — todo)")
	importance := fs.Int("importance", 0, "важность 1..5 (0 — дефолт 3)")
	tagsFlag := fs.String("tags", "", "теги через запятую")
	scopeFlag := fs.String("scope", "", "пространство записи (пусто — домашний scope инстанса из [scope])")
	sourceFlag := fs.String("source", "", "источник факта (пусто — \"cli\")")
	ttlFlag := fs.String("ttl", "", "явный срок жизни факта: Go-длительность от сейчас (\"72h\"); пусто — вечно (задача 9.4)")
	_ = fs.Parse(args) // flag.ExitOnError сам завершает процесс при ошибке разбора/-h

	content := *contentFlag
	if content == "-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return fmt.Errorf("save: чтение содержимого из stdin: %w", err)
		}
		content = string(b)
	}

	source := *sourceFlag
	if source == "" {
		source = "cli"
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("загрузка конфига: %w", err)
	}
	if *dbPath != "" {
		cfg.Store.Path = *dbPath
	}
	loadDotEnvForConfig(*configPath, "memo save")

	s, err := openStore(cfg.Store)
	if err != nil {
		return fmt.Errorf("открытие хранилища %q: %w", cfg.Store.Target(), err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			log.Printf("memo save: закрытие хранилища: %v", err)
		}
	}()

	ctx := context.Background()
	// Embedder — реальный из фабрики (симметрично серверному режиму): вектор
	// пишется сразу, если провайдер доступен; при недоступной ollama — штатная
	// деградация write path (Writer.Save просто откладывает вектор для
	// фонового досчёта, DESIGN.md §7.4, §9). Reasoner здесь не нужен — save не
	// суммирует и не консолидирует.
	embedder, err := newEmbedder(ctx, cfg.Embedder)
	if err != nil {
		return fmt.Errorf("создание embedder: %w", err)
	}

	scope := *scopeFlag
	if scope == "" {
		// Явный scope не передан — подставляем "домашний" scope инстанса. Если
		// мультипроектность выключена (project не задан), SaveScope() вернёт ""
		// и Writer.Save сам вернёт write.ErrEmptyScope.
		scope = cfg.Scope.SaveScope()
	}

	w := write.New(s, embedder)
	id, merged, err := w.Save(ctx, write.SaveInput{
		Kind:       memory.Kind(*kindFlag),
		Scope:      scope,
		Content:    content,
		Summary:    *summaryFlag,
		Status:     *statusFlag,
		Importance: *importance,
		Tags:       splitCSV(*tagsFlag),
		Source:     source,
		TTL:        *ttlFlag,
	})
	if err != nil {
		return fmt.Errorf("save: %w", err)
	}

	if merged {
		fmt.Fprintf(out, "merged %s\n", id)
	} else {
		fmt.Fprintf(out, "saved %s\n", id)
	}
	return nil
}

// splitCSV разбирает список через запятую в срез: обрезает пробелы вокруг
// каждого элемента и отбрасывает пустые (в т.ч. на пустом входе — тогда
// возвращает nil, а не срез из одной пустой строки). Общий разбор для флагов
// -scopes/-kinds/-tags.
func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
