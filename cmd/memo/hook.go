// Подкоманда `memo hook -event <name> [-config path]` — тонкая обёртка над
// recall-путём 7.1 (doRecall, cli.go) для хуков агентского харнесса Claude
// Code (SessionStart/UserPromptSubmit, DESIGN.md §12, задача 7.2). Харнесс
// передаёт хук-команде на stdin JSON события и вставляет её stdout прямиком в
// контекст сессии (для этих двух событий — как обычный текст, без обёртки в
// JSON), поэтому вывод — либо готовый Markdown-бриф под заголовком, либо
// абсолютно пусто.
//
// ЖЕЛЕЗНЫЙ инвариант: хук никогда не ломает сессию агента. Любая ошибка (нет
// конфига, нет БД, битый stdin, неизвестный -event) должна привести к пустому
// stdout и нулевому коду выхода — это осознанное отступление от fail-fast
// остального `memo` (recall/save падают с exit 1): хук — фоновая роскошь,
// не критический путь запуска сессии. Эту гарантию обеспечивает диспетчер в
// main.go (не вызывает os.Exit при ошибке runHook, только логирует в stderr);
// здесь достаточно не писать в out до того, как есть что печатать.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"agentmemory/internal/config"
	"agentmemory/internal/memory"
)

const (
	// hookBriefHeader — заголовок, под которым бриф авто-recall попадает в
	// контекст: по нему видно происхождение текста (не обычный вывод агента).
	hookBriefHeader = "## Долговременная память проекта (авто-recall)"

	// hookSessionStartQuery — фиксированный запрос recall на старте сессии:
	// не привязан к конкретной задаче (агент её ещё не сформулировал), поэтому
	// нацелен на общий контекст проекта, а не на что-то конкретное.
	hookSessionStartQuery = "состояние проекта, договорённости, следующие шаги, грабли"

	// hookSessionStartBudget / hookPromptSubmitBudget — уменьшенные бюджеты
	// токенов относительно интерактивного recall (default_token_budget в
	// конфиге может быть больше): session-start грузит контекст ОДИН раз за
	// сессию, prompt-submit — на КАЖДОМ сообщении, поэтому его бюджет ещё
	// меньше — нельзя раздувать промпт на каждый ход.
	hookSessionStartBudget = 600
	hookPromptSubmitBudget = 400

	// hookPromptSubmitMinRunes — порог длины prompt в рунах, ниже которого
	// prompt-submit ничего не делает: короткие реплики ("да", "ok", "продолжай")
	// не несут смысловой нагрузки для поиска и только тратят время на recall.
	hookPromptSubmitMinRunes = 15
)

// hookEvent — минимальный набор полей стандартного JSON, который харнесс
// Claude Code передаёт хук-команде на stdin (SessionStart/UserPromptSubmit).
// Реальный payload содержит больше полей (session_id, cwd, hook_event_name,
// source, transcript_path, permission_mode...) — они этому хуку не нужны и
// encoding/json их молча игнорирует (мягкий разбор: неизвестные поля не
// ошибка, событие интересует только своим текстом запроса).
type hookEvent struct {
	Prompt string `json:"prompt"` // UserPromptSubmit: текст, который агент вот-вот увидит
}

// runHook разбирает флаги подкоманды `hook` и печатает в out либо пустую
// строку (нечего добавить в контекст, либо любая ошибка), либо Markdown-бриф
// под hookBriefHeader. Возвращаемая ошибка — только для логирования вызывающей
// стороной (main.go) в stderr; она никогда не сопровождается записью в out.
func runHook(args []string, stdin io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	// Хук не должен ничего печатать в out (stdout) ни при каких обстоятельствах
	// — в отличие от recall/save, где fs.SetOutput(out) осознанно указывает на
	// stdout. usage/ошибки разбора флагов идут в stderr (полезно для ручной
	// отладки `memo hook -h`), но никогда в out — единственный канал наружу
	// для самой ошибки всё равно возвращаемый err, который main.go логирует в
	// stderr ещё раз.
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "хук для .claude/settings.json — печатает Markdown-бриф в stdout, пуст при любой ошибке (exit 0).")
		fmt.Fprintln(os.Stderr, "Использование: memo hook -event <session-start|prompt-submit> [-config path] < event.json")
		fs.PrintDefaults()
	}
	event := fs.String("event", "", "событие хука: session-start | prompt-submit")
	configPath := fs.String("config", "", "путь к TOML-конфигу (опционально)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("hook: разбор флагов: %w", err)
	}

	var ev hookEvent
	if err := json.NewDecoder(stdin).Decode(&ev); err != nil && err != io.EOF {
		// Битый JSON на stdin — деградация, не паника: хук просто ничего не
		// добавляет в контекст. io.EOF (пустой stdin) сюда не попадает — это
		// валидное вырожденное событие без полей, не ошибка.
		return fmt.Errorf("hook: разбор события stdin: %w", err)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("hook: загрузка конфига: %w", err)
	}
	loadDotEnvForConfig(*configPath, "memo hook")

	ctx := context.Background()
	switch *event {
	case "session-start":
		return runHookSessionStart(ctx, cfg, out)
	case "prompt-submit":
		return runHookPromptSubmit(ctx, cfg, ev.Prompt, out)
	default:
		return fmt.Errorf("hook: неизвестное или не заданное -event %q (ожидается session-start | prompt-submit)", *event)
	}
}

// runHookSessionStart выполняет recall фиксированным запросом о состоянии
// проекта по дефолтным scopes инстанса (config.ScopeConfig.RecallScopes) и
// всем kinds — на старте сессии агент ещё не сформулировал задачу, поэтому
// фильтровать по типу факта нечем. Summarize выключен (латентность хука).
// Пустой бриф (Count==0) — пустой out, без заголовка.
func runHookSessionStart(ctx context.Context, cfg config.Config, out io.Writer) error {
	brief, err := doRecall(ctx, cfg, "memo hook", memory.Query{
		Text:        hookSessionStartQuery,
		Scopes:      cfg.Scope.RecallScopes(),
		TokenBudget: hookSessionStartBudget,
		Summarize:   false,
	})
	if err != nil {
		return fmt.Errorf("hook session-start: %w", err)
	}
	return writeHookBrief(out, brief)
}

// runHookPromptSubmit выполняет recall по тексту prompt (взят из stdin-JSON
// события). Короткий prompt (< hookPromptSubmitMinRunes рун после обрезки
// пробелов) — заведомо мусорный запрос ("да", "ok", "погнали") — recall не
// выполняется вовсе, out остаётся пустым.
func runHookPromptSubmit(ctx context.Context, cfg config.Config, prompt string, out io.Writer) error {
	prompt = strings.TrimSpace(prompt)
	if utf8.RuneCountInString(prompt) < hookPromptSubmitMinRunes {
		return nil
	}

	brief, err := doRecall(ctx, cfg, "memo hook", memory.Query{
		Text:        prompt,
		Scopes:      cfg.Scope.RecallScopes(),
		TokenBudget: hookPromptSubmitBudget,
		Summarize:   false,
	})
	if err != nil {
		return fmt.Errorf("hook prompt-submit: %w", err)
	}
	return writeHookBrief(out, brief)
}

// writeHookBrief печатает brief в out под hookBriefHeader — но только если в
// нём есть хоть одна запись; пустая память не должна вставлять в контекст
// заголовок без содержимого.
func writeHookBrief(out io.Writer, brief memory.Brief) error {
	if brief.Count == 0 {
		return nil
	}
	fmt.Fprintln(out, hookBriefHeader)
	fmt.Fprintln(out)
	fmt.Fprint(out, brief.Markdown)
	return nil
}
