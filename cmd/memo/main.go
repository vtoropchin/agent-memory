// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Vladimir Toropchin
//
// Command memo — точка входа AgentMemory: MCP-сервер долговременной памяти
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
	"strings"
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

// memoPostgresPasswordEnv — переменная окружения с паролем PostgreSQL-хранилища
// (DESIGN.md §13, то же правило, что для DEEPSEEK_API_KEY/OPENAI_API_KEY). Пароль
// никогда не читается из конфиг-файла и не логируется.
const memoPostgresPasswordEnv = "MEMO_POSTGRES_PASSWORD"

// Дефолтные base URL эмбеддера (defaultOllamaBaseURL/defaultOpenAIBaseURL) —
// единый источник истины в init.go: там же, где их пишет `memo init`, чтобы
// init-дефолт и рантайм-дефолт newEmbedder не разъехались.

// routeDecision — куда идут args верхнего уровня ДО того, как они попадают в
// cobra (см. decideRoute). Отдельный enum, а не просто bool "сервер/подкоманда",
// потому что help — третий путь, обрабатываемый напрямую (writeUsage), минуя
// и сервер, и cobra.
type routeDecision int

const (
	routeCobra  routeDecision = iota // подкоманда по имени (или неизвестное имя -> root -> сервер, как раньше)
	routeServer                      // первый арг похож на флаг сервера -> run(args) напрямую, cobra не участвует
	routeHelp                        // -h/-help/--help/help первым аргументом -> writeUsage
)

// decideRoute решает маршрут ПЕРВЫМ аргументом args, байт-в-байт повторяя
// правило старого диспетчера (до миграции на cobra в задаче 11.1): в
// подкоманду роутит только точное совпадение имени, поэтому "-db recall" или
// "-config web" уходили в сервер, а не в подкоманду recall/web. Цикл
// исправлений после 11.1: миграция на cobra сломала это байт-в-байт
// поведение — cobra.Command.Find (используемый внутри root.Execute) сам
// решает, что считать подкомандой, через свою эвристику stripFlags, которая
// при "-db recall" отбрасывает САМ токен "-db" (посчитав, что это флаг с
// значением) и трактует следующий токен "recall" как имя подкоманды — в
// результате `memo -db recall` уходил в подкоманду recall, чей отдельный
// flag.ExitOnError-FlagSet видел "-db" без значения и завершал процесс.
// decideRoute вызывается ДО того, как args вообще попадают в cobra — поэтому
// для аргументов, похожих на флаг (routeServer), Find/stripFlags уже не
// участвуют вовсе, и разойтись с историческим поведением нечем.
//
// Порядок проверок: help-токен первым аргументом -> routeHelp; иначе первый
// арг начинается с "-" (это флаг сервера, не имя подкоманды) -> routeServer;
// иначе -> routeCobra (там уже cobra резолвит по точному имени; неизвестное
// имя, как и раньше, падает на root -> серверный путь).
func decideRoute(args []string) routeDecision {
	if len(args) >= 1 && isHelpArg(args[0]) {
		return routeHelp
	}
	if len(args) >= 1 && strings.HasPrefix(args[0], "-") {
		return routeServer
	}
	return routeCobra
}

// runMain — тестируемое тело main(): принимает stdin/stdout/stderr и args
// параметрами (вместо os.Stdin/os.Stdout/os.Args) и возвращает код выхода
// вместо os.Exit — тот же приём, что уже применялся для дерева cobra
// (cobra.go), теперь распространённый и на guard верхнего уровня decideRoute,
// чтобы БАГ 2 (маршрутизация `-db recall`/`-config web`) был покрыт тестом без
// реального завершения процесса.
func runMain(stdin io.Reader, stdout, stderr io.Writer, args []string) int {
	switch decideRoute(args) {
	case routeHelp:
		writeUsage(stdout)
		return 0
	case routeServer:
		if err := run(args); err != nil {
			fmt.Fprintf(stderr, "memo: %v\n", err)
			return 1
		}
		return 0
	default: // routeCobra
		// Дерево подкоманд (задача 11.1) построено на cobra (newRootCmd,
		// cobra.go): каждая подкоманда — тонкая обёртка над уже существующими
		// runInit/runWeb/runRecall/runSave/runHook/runDoctor/runExport/
		// runImport и серверным run(), сама бизнес-логика этой миграцией не
		// затронута. exitCode передаётся как указатель, а не через возврат
		// ошибки Execute(), потому что у подкоманд разные контракты выхода
		// (hook — всегда 0, doctor — код из отчёта, остальные — 0/1) и это
		// нельзя выразить одним правилом "ошибка -> ненулевой код", которое
		// применяет cobra по умолчанию.
		exitCode := 0
		root := newRootCmd(stdin, stdout, stderr, &exitCode)
		root.SetArgs(args)
		root.SetOut(stdout)
		root.SetErr(stderr)
		if err := root.Execute(); err != nil {
			// Не должно случаться: на каждой команде DisableFlagParsing=true и
			// RunE сама обрабатывает свои ошибки (см. cobra.go) — но не
			// проглатываем непредвиденную ошибку cobra, если она всё же возникла.
			fmt.Fprintln(stderr, err)
			return 1
		}
		return exitCode
	}
}

// main — точка входа memo. Вся логика — в runMain, main() лишь подключает
// реальные os.Std*/os.Args и транслирует код выхода в os.Exit (без этого
// runMain нельзя было бы тестировать без завершения процесса теста).
func main() {
	os.Exit(runMain(os.Stdin, os.Stdout, os.Stderr, os.Args[1:]))
}

// run — серверный режим (MCP поверх stdio), вызывается корневой командой без
// подкоманды (в т.ч. `memo -config <path>`, как зовёт уже установленный
// .mcp.json — обратная совместимость, задача 11.1). args — то, что осталось
// после имени команды (пусто для голого `memo`); разбираются собственным
// FlagSet, а не глобальным flag.CommandLine — тот же приём, что у остальных
// подкоманд (cli.go и др.), нужен, чтобы cobra могла передавать args явно, не
// трогая os.Args.
func run(args []string) error {
	// Транспорт MCP использует stdout для протокола — всё логирование только в
	// stderr, иначе лог-строки повредят JSON-RPC поток.
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags)

	fs := flag.NewFlagSet("memo", flag.ExitOnError)
	configPath := fs.String("config", "", "путь к TOML-конфигу (опционально)")
	dbPath := fs.String("db", "", "путь к файлу SQLite, переопределяет store.path из конфига (опционально)")
	_ = fs.Parse(args) // flag.ExitOnError сам завершает процесс при ошибке разбора/-h

	// .env рядом с конфигом (или в текущем каталоге, если -config не задан) —
	// раскладывается `memo init` (задача 5.4). Загрузить ДО newReasoner,
	// иначе DEEPSEEK_API_KEY туда не долетит. Значение ключа в лог не попадает.
	loadDotEnvForConfig(*configPath, "memo")

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("загрузка конфига: %w", err)
	}
	if *dbPath != "" {
		cfg.Store.Path = *dbPath
	}

	s, err := openStore(cfg.Store)
	if err != nil {
		return fmt.Errorf("открытие хранилища %q: %w", cfg.Store.Target(), err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			log.Printf("memo: закрытие хранилища: %v", err)
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
		maint.NewExpirer(s),
		maint.NewDecayer(s, w),
		maint.NewDedupSweeper(s),
		maint.NewConsolidator(s, reasoner, w),
		maint.NewReindexer(s, embedder),
		decayInterval, consolidateAfter,
	)
	go scheduler.Run(ctx)

	log.Printf("memo: запуск MCP-сервера (stdio), store=%s", cfg.Store.Target())
	if err := srv.Run(ctx); err != nil {
		return fmt.Errorf("работа сервера: %w", err)
	}
	return nil
}

// resolvePostgresPassword заполняет cfg.Password из env MEMO_POSTGRES_PASSWORD, если
// store.driver=postgres и пароль ещё не задан явно. Пустой пароль для postgres — НЕ
// фатальная ошибка на этом этапе: сервер может использовать trust-аутентификацию
// (DESIGN.md §15, деградация вместо падения) — только предупреждение в stderr,
// попытка подключения делается всё равно; если пароль всё же был нужен, дальнейшее
// открытие (store.Open или store.OpenPostgresReadOnly) вернёт содержательную ошибку
// аутентификации. Вынесена из openStore в отдельную функцию, чтобы этим же путём мог
// воспользоваться read-only путь диагностики (doctor.go postgresDatabaseSection),
// который открывает Store не через общую фабрику store.Open, а напрямую через
// store.OpenPostgresReadOnly (см. её комментарий, зачем).
func resolvePostgresPassword(cfg config.StoreConfig) config.StoreConfig {
	if cfg.Driver == "postgres" && cfg.Password == "" {
		if pw := os.Getenv(memoPostgresPasswordEnv); pw != "" {
			cfg.Password = pw
		} else {
			log.Printf("memo: store.driver=postgres, но %s пуст (env/.env) — подключение будет попытано без пароля", memoPostgresPasswordEnv)
		}
	}
	return cfg
}

// openStore — общая точка входа для всех мест cmd/memo, открывающих Store через
// схему приложения целиком (server-режим, recall/save/web/export/import) —
// симметрично newReasoner/newEmbedder. НЕ используется doctor'ом для postgres
// (см. store.OpenPostgresReadOnly и её вызов в doctor.go): store.Open накатывает
// схему как часть открытия, что для read-only диагностики недопустимо.
func openStore(cfg config.StoreConfig) (store.Store, error) {
	return store.Open(resolvePostgresPassword(cfg))
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
			log.Printf("memo: reasoner.provider=deepseek, но %s пуст — суммаризация отключена", deepseekAPIKeyEnv)
			return reason.NewNoop(), nil
		}
		if cfg.Model == "" {
			log.Printf("memo: reasoner.provider=deepseek, но reasoner.model пуст — суммаризация отключена")
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
// defaultOllamaBaseURL. "openai" — то же самое поверх любого
// /v1/embeddings-совместимого сервера (OpenAI, OpenRouter, LM Studio, vLLM —
// DESIGN.md §9); ключ читается только из env OPENAI_API_KEY (симметрично
// DEEPSEEK_API_KEY, никогда не из TOML), пустой base_url в конфиге замещается на
// defaultOpenAIBaseURL (api.openai.com). Оба дефолта провайдер-специфичны и живут
// в init.go (единый источник истины с `memo init`), а не в config.Default() —
// иначе один из них (обязательно ollama-специфичный localhost:11434) утёк бы во
// второй провайдер при пустом base_url в TOML. Ошибка
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
			baseURL = defaultOllamaBaseURL
		}
		e, err := embed.NewOllama(ctx, baseURL, cfg.Model)
		if err != nil {
			log.Printf("memo: embedder.provider=ollama, но ollama недоступна или модель не загружена (%v) — векторный поиск отключён, recall на полнотекстовом", err)
			return embed.NewNoop(), nil
		}
		return e, nil
	case "openai":
		key := os.Getenv(openaiAPIKeyEnv)
		if key == "" {
			log.Printf("memo: embedder.provider=openai, но %s пуст — векторный поиск отключён, recall на полнотекстовом", openaiAPIKeyEnv)
			return embed.NewNoop(), nil
		}
		baseURL := cfg.BaseURL
		if baseURL == "" {
			baseURL = defaultOpenAIBaseURL
		}
		e, err := embed.NewOpenAI(ctx, baseURL, cfg.Model, key)
		if err != nil {
			log.Printf("memo: embedder.provider=openai, но сервер недоступен или ключ/модель невалидны (%v) — векторный поиск отключён, recall на полнотекстовом", err)
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

// writeUsage печатает справку верхнего уровня memo: что это, три формы
// запуска, флаги серверного режима и подкоманды. Вынесена из main для
// тестируемости.
func writeUsage(w io.Writer) {
	fmt.Fprintln(w, "memo — сервер долговременной памяти для ИИ-агентов (MCP поверх SQLite или PostgreSQL).")
	fmt.Fprintln(w)

	fmt.Fprintln(w, "Использование:")
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  memo [-config <path>] [-db <path>]\tзапуск MCP-сервера (режим по умолчанию, транспорт stdio)")
	fmt.Fprintln(tw, "  memo init [флаги]\tустановить память в проект (генерирует MCP-конфиг и скил)")
	fmt.Fprintln(tw, "  memo web [флаги]\tHTTP-морда для просмотра памяти в браузере")
	fmt.Fprintln(tw, "  memo recall [флаги] \"<запрос>\"\tразовый поиск по памяти (Markdown-бриф в stdout)")
	fmt.Fprintln(tw, "  memo save [флаги]\tразовое сохранение факта в память")
	fmt.Fprintln(tw, "  memo hook -event <name> [флаги]\tхук Claude Code (session-start|prompt-submit), JSON события на stdin")
	fmt.Fprintln(tw, "  memo doctor [флаги]\tread-only диагностика: в каком режиме реально работает память")
	fmt.Fprintln(tw, "  memo export [флаги]\tэкспорт памяти в переносимый JSONL-бэкап (без векторов)")
	fmt.Fprintln(tw, "  memo import [флаги] file.jsonl\tимпорт JSONL-бэкапа с сохранением id и таймстампов")
	fmt.Fprintln(tw, "  memo migrate [флаги] <источник> <приёмник>\tперенос памяти между бэкендами (sqlite <-> postgres)")
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
	fmt.Fprintln(tw, "  hook\tхук-режим для .claude/settings.json (см. `memo init`): любая ошибка → пустой stdout, exit 0")
	fmt.Fprintln(tw, "  doctor\tдиагностика: конфиг/БД/записи/вектор/embedder/reasoner одной командой, ничего не пишет")
	fmt.Fprintln(tw, "  export\tбэкап/перенос памяти: вся БД или -scopes в переносимый JSONL (векторы не экспортируются)")
	fmt.Fprintln(tw, "  import\tвосстановление из JSONL: сохраняет id/таймстампы; -dry-run/-overwrite/-rescope")
	fmt.Fprintln(tw, "  migrate\tперенос между бэкендами: sqlite:<путь> <-> postgres://user@host:port/dbname; -dry-run/-overwrite/-scopes")
	tw.Flush()
	fmt.Fprintln(w, "  memo init -h, memo web -h, memo recall -h, memo save -h, memo hook -h, memo doctor -h, memo export -h, memo import -h, memo migrate -h — подробности по флагам каждой.")
}
