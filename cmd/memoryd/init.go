// Подкоманда `memoryd init` — интерактивный установщик памяти в проект под
// конкретное агентское приложение (DESIGN.md §14 Этап 5, задачи 5.3/5.4;
// выбор провайдеров и TUI — задача 11.1). Раскладывает в целевой каталог
// MCP-конфиг, скил-инструкцию «Протокол работы с памятью», agentmemory.toml
// (с введённым провайдером эмбеддинга/reasoner, store, project) и, если
// введены ключи выбранных провайдеров, .env (+ строку .env в .gitignore). Не
// трогает загрузку конфига (internal/config) и серверный путь.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/huh"
	"github.com/mattn/go-isatty"
)

// Дефолты полей initConfig, не заданных ни флагом, ни интерактивным вводом.
const (
	defaultAgentKey         = "claude"
	defaultEmbedderProvider = "ollama"
	defaultOllamaBaseURL    = "http://localhost:11434"
	defaultOllamaModel      = "bge-m3"
	defaultOpenAIBaseURL    = "https://api.openai.com"
	defaultOpenAIModel      = "text-embedding-3-small"
	defaultReasonerProvider = "deepseek"
	defaultStorePath        = "~/.agentmemory/memory.db"

	// fetchModelsTimeout — таймаут запроса списка моделей у ollama/OpenAI в
	// интерактивном промпте: недоступный/зависший сервер не должен вешать
	// установку навсегда (задача 11.1 — "не блокировать установку").
	fetchModelsTimeout = 5 * time.Second
)

// providerSpec описывает один провайдер эмбеддинга/reasoner: значение флага
// -embedder-provider/-reasoner-provider (key), человекочитаемый label для
// help-текста и TUI-select (wizard.go), и env-переменную с API-ключом (envKey
// — пусто, если провайдеру ключ не нужен, напр. ollama/noop). Единый источник
// метаданных провайдера — по образцу agentRegistry: из embedderProviders/
// reasonerProviders деривятся список опций help/select, валидация флага и
// выбор env-ключа в renderMCPJSON/renderOpencodeJSON/doInit (задача 11.1,
// цикл исправлений — раньше эти четыре вещи были рассинхронизируемыми копиями
// в разных функциях).
type providerSpec struct {
	key    string
	label  string
	envKey string
}

// embedderProviders/reasonerProviders — реестры провайдеров в порядке показа
// в select-промпте (первый — дефолт).
var embedderProviders = []providerSpec{
	{key: "ollama", label: "ollama (локальный сервер)"},
	{key: "openai", label: "openai (облако или совместимый сервер)", envKey: openaiAPIKeyEnv},
	{key: "noop", label: "noop (без вектора, только полнотекстовый поиск)"},
}

var reasonerProviders = []providerSpec{
	{key: "deepseek", label: "deepseek", envKey: deepseekAPIKeyEnv},
	{key: "noop", label: "noop (без LLM)"},
}

// embedderProviderOptions/reasonerProviderOptions — ключи провайдеров в
// порядке select-промпта/help-текста (первый — дефолт), деривированы из
// embedderProviders/reasonerProviders.
var embedderProviderOptions = providerKeys(embedderProviders)
var reasonerProviderOptions = providerKeys(reasonerProviders)

// providerKeys возвращает ключи -provider всех записей providers в заданном
// порядке — для help-текста флагов и сообщений об ошибке валидации.
func providerKeys(providers []providerSpec) []string {
	keys := make([]string, 0, len(providers))
	for _, p := range providers {
		keys = append(keys, p.key)
	}
	return keys
}

// providerByKey ищет запись providers по значению флага -*-provider.
func providerByKey(providers []providerSpec, key string) (providerSpec, bool) {
	for _, p := range providers {
		if p.key == key {
			return p, true
		}
	}
	return providerSpec{}, false
}

// keyField возвращает указатель на поле initConfig, хранящее API-ключ этого
// провайдера (DeepseekKey для DEEPSEEK_API_KEY, OpenAIKey для OPENAI_API_KEY),
// либо nil, если провайдер (envKey=="") ключа не требует. Единственное место,
// связывающее провайдера с полем initConfig, хранящим его секрет.
func (p providerSpec) keyField(cfg *initConfig) *string {
	switch p.envKey {
	case deepseekAPIKeyEnv:
		return &cfg.DeepseekKey
	case openaiAPIKeyEnv:
		return &cfg.OpenAIKey
	default:
		return nil
	}
}

// selectedProviders возвращает записи реестров, соответствующие выбранным в
// cfg провайдерам эмбеддинга и reasoner (в этом порядке) — общий источник для
// renderMCPJSON/renderOpencodeJSON (какие env-плейсхолдеры класть в .mcp.json/
// opencode.json) и doInit (какие ключи класть в .env). cfg должен быть уже
// пропущен через applyInitDefaults (Provider непуст) — при пустом/неизвестном
// значении запись просто не находится и пропускается.
func selectedProviders(cfg initConfig) []providerSpec {
	var out []providerSpec
	if p, ok := providerByKey(embedderProviders, cfg.EmbedderProvider); ok {
		out = append(out, p)
	}
	if p, ok := providerByKey(reasonerProviders, cfg.ReasonerProvider); ok {
		out = append(out, p)
	}
	return out
}

// clearUnselectedProviderKeys обнуляет в cfg ключи провайдеров из providers,
// которые НЕ являются выбранным selected — защита от рассинхрона флагов
// (напр. -deepseek-key при -reasoner-provider=noop), задача 11.1.
func clearUnselectedProviderKeys(cfg *initConfig, providers []providerSpec, selected string) {
	for _, p := range providers {
		if p.key == selected || p.envKey == "" {
			continue
		}
		if f := p.keyField(cfg); f != nil {
			*f = ""
		}
	}
}

// Значения initConfig.Hooks — какие хуки Claude Code подключить для
// ambient-режима (авто-recall, DESIGN.md §12, задача 7.2). hooksOptions —
// порядок вариантов в интерактивном промпте (первый — дефолт: session-start
// один покрывает основной сценарий «recall в начале задачи» дешевле всего;
// prompt-submit добавляет цену на каждое сообщение, поэтому не дефолт).
const (
	hooksSessionStart = "session-start"
	hooksBoth         = "session-start+prompt-submit"
	hooksNone         = "none"
)

var hooksOptions = []string{hooksSessionStart, hooksBoth, hooksNone}

// hookEventMapping — одно событие Claude Code (ключ секции `hooks` в
// settings.json) и соответствующее значение флага `-event` подкоманды
// `memoryd hook`.
type hookEventMapping struct {
	ClaudeEvent  string // "SessionStart" | "UserPromptSubmit"
	MemorydEvent string // "session-start" | "prompt-submit"
}

// hookEventsByChoice — какие события подключает каждое значение
// initConfig.Hooks. hooksNone намеренно отсутствует — для него подключать
// нечего.
var hookEventsByChoice = map[string][]hookEventMapping{
	hooksSessionStart: {
		{ClaudeEvent: "SessionStart", MemorydEvent: "session-start"},
	},
	hooksBoth: {
		{ClaudeEvent: "SessionStart", MemorydEvent: "session-start"},
		{ClaudeEvent: "UserPromptSubmit", MemorydEvent: "prompt-submit"},
	},
}

// configTomlTemplate — шаблон agentmemory.toml по образцу корневого
// agentmemory.toml/config.example.toml (DESIGN.md §13). Константа-строка, а не
// toml.Marshal — так сохраняются комментарии. %[1]s — экранированное имя
// проекта, %[2]s — путь к файлу БД, %[3]s — тело секции [embedder]
// (renderEmbedderTomlBlock), %[4]s — тело секции [reasoner]
// (renderReasonerTomlBlock) — обе зависят от выбранного провайдера.
const configTomlTemplate = `# Конфиг AgentMemory для проекта %[1]s (DESIGN.md §13).
#
# Агентское приложение поднимает MCP-сервер agentmemory из сгенерированного
# рядом MCP-конфига и передаёт ему этот файл флагом -config.

[store]
# Общий файл БД на все проекты (DESIGN.md §2.1: один файл, изоляция через scope).
# Каталог создаётся автоматически при первом старте (store.Open).
path = "%[2]s"

[embedder]
%[3]s
[reasoner]
%[4]s
[recall]
default_token_budget = 800
weights = { relevance = 1.0, importance = 0.5, recency = 0.4, access = 0.2 }

[maint]
decay_interval    = "24h"
consolidate_after = "168h"                 # неделя episodic → semantic

[scope]
# Домашнее пространство project:%[1]s, поверх него в recall всегда добавляется
# общий слой user:*. memory_save без явного scope пишет в project:%[1]s;
# общие привычки — явно scope="user:*".
project = "%[1]s"
shared  = ["user:*"]
`

// renderEmbedderTomlBlock рендерит тело секции [embedder] под выбранный
// cfg.EmbedderProvider — единственное место, определяющее, какие поля и
// комментарии попадают в agentmemory.toml для каждого провайдера (DESIGN.md
// §13: ключи в TOML никогда не пишутся).
func renderEmbedderTomlBlock(cfg initConfig) string {
	switch cfg.EmbedderProvider {
	case "openai":
		return fmt.Sprintf(`# OpenAI-совместимый /v1/embeddings (OpenAI, OpenRouter, LM Studio, vLLM и т.п.).
# Ключ берётся из env OPENAI_API_KEY (или .env рядом с этим конфигом) — в TOML
# не пишется (DESIGN.md §13).
provider = "openai"
base_url = "%s"
model    = "%s"
`, escapeTOMLString(cfg.EmbedderBaseURL), escapeTOMLString(cfg.EmbedderModel))
	case "noop":
		return "# Без вектора — recall работает на чистом FTS5 (DESIGN.md §15).\nprovider = \"noop\"\n"
	default: // "ollama"
		return fmt.Sprintf(`# Локальный ollama (dim определяется автоматически пробным эмбеддингом).
# Если ollama недоступен — memoryd стартует, recall падает на чистый FTS5 (§15).
provider = "ollama"
base_url = "%s"
model    = "%s"
`, escapeTOMLString(cfg.EmbedderBaseURL), escapeTOMLString(cfg.EmbedderModel))
	}
}

// renderReasonerTomlBlock — то же самое для секции [reasoner].
func renderReasonerTomlBlock(cfg initConfig) string {
	switch cfg.ReasonerProvider {
	case "noop":
		return "# Без LLM-суммаризации/консолидации (DESIGN.md §15).\nprovider = \"noop\"\n"
	default: // "deepseek"
		return `# DeepSeek chat: ключ берётся из env DEEPSEEK_API_KEY. Если в shell-env его нет,
# memoryd сам подхватывает его из .env рядом с этим конфигом при старте (см.
# loadDotEnv в main.go) — .env раскладывается этим же "memoryd init". Пустой
# ключ — деградация до noop (§15): recall без суммаризации, консолидация
# недоступна, но память работает.
provider = "deepseek"
model    = "deepseek-chat"
`
	}
}

// mcpServerArgs — аргументы, с которыми агентское приложение запускает
// memoryd как MCP-сервер agentmemory.
var mcpServerArgs = []string{"-config", "${CLAUDE_PROJECT_DIR:-.}/agentmemory.toml"}

// mcpConfigFile / mcpServerSpec — форма .mcp.json (см. корневой .mcp.json).
type mcpConfigFile struct {
	MCPServers map[string]mcpServerSpec `json:"mcpServers"`
}

type mcpServerSpec struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

// skillSpec описывает один скил, раскладываемый `memoryd init` в
// `<skillsDir>/<dirName>/SKILL.md`: dirName — подкаталог под
// agentTarget.skillsDir, render — рендерер тела SKILL.md под конкретный
// проект.
type skillSpec struct {
	dirName string
	render  func(project string) string
}

// memorySkills — набор скиллов, которые `memoryd init` раскладывает КАЖДОМУ
// поддерживаемому агентскому приложению (одинаковый для всех записей
// agentRegistry — различается только корень skillsDir): "memoryd" — протокол
// работы с памятью (skill.go), "memoryd-bootstrap" — разовая инициализация
// памяти проекта (bootstrap_skill.go). Добавить следующий скил — одна запись
// в этом слайсе.
var memorySkills = []skillSpec{
	{dirName: "memoryd", render: renderSkillMD},
	{dirName: "memoryd-bootstrap", render: renderBootstrapSkillMD},
}

// agentTarget описывает одно агентское приложение и то, как оно раскладывает
// артефакты AgentMemory в проекте: MCP-конфиг и скилы. В agentRegistry —
// claude (Claude Code) и opencode; структура рассчитана на то, чтобы добавить
// следующее приложение (например codex) ещё одной записью.
type agentTarget struct {
	label string // человекочитаемое имя для промптов/вывода

	mcpConfigPath   func(dir string) string
	renderMCPConfig func(command string, cfg initConfig) (string, error)

	skillsDir func(dir string) string // корень каталога скиллов агента
	skills    []skillSpec
}

// agentRegistry — реестр поддерживаемых агентских приложений, ключ — значение
// флага -agent.
var agentRegistry = map[string]agentTarget{
	"claude": {
		label:           "Claude Code",
		mcpConfigPath:   func(dir string) string { return filepath.Join(dir, ".mcp.json") },
		renderMCPConfig: renderMCPJSON,
		skillsDir: func(dir string) string {
			return filepath.Join(dir, ".claude", "skills")
		},
		skills: memorySkills,
	},
	"opencode": {
		label:           "opencode",
		mcpConfigPath:   func(dir string) string { return filepath.Join(dir, "opencode.json") },
		renderMCPConfig: renderOpencodeJSON,
		skillsDir: func(dir string) string {
			return filepath.Join(dir, ".opencode", "skills")
		},
		skills: memorySkills,
	},
}

// agentKeys возвращает отсортированные ключи agentRegistry — для текста
// промптов и сообщений об ошибке.
func agentKeys() []string {
	keys := make([]string, 0, len(agentRegistry))
	for k := range agentRegistry {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// initConfig — параметры одного прогона `memoryd init`: заданы флагом, либо (в
// интерактивном TTY-режиме) дозапрошены promptInitTUI, либо остаются пустыми
// и получают дефолт в doInit/applyInitDefaults.
type initConfig struct {
	Project          string
	Dir              string
	Agent            string
	EmbedderProvider string // ollama | openai | noop
	EmbedderBaseURL  string // ollama/openai: base URL; noop — не используется
	EmbedderModel    string // ollama/openai: модель эмбеддинга; noop — не используется
	ReasonerProvider string // deepseek | noop
	StorePath        string
	OpenAIKey        string // только если EmbedderProvider == "openai"
	DeepseekKey      string // только если ReasonerProvider == "deepseek"
	Hooks            string // "" | session-start | session-start+prompt-submit | none (см. hooksOptions)
	Force            bool
}

// runInit разбирает флаги подкоманды `init` и выполняет скаффолдинг. Вынесена
// из диспетчера в main.go для тестируемости.
func runInit(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fmt.Fprintln(out, "устанавливает память в проект: генерирует MCP-конфиг, скил-инструкцию и agentmemory.toml.")
		fmt.Fprintln(out, "Использование: memoryd init [флаги]")
		fs.PrintDefaults()
	}
	project := fs.String("project", "", "имя проекта (дефолт — базовое имя каталога -dir)")
	dir := fs.String("dir", ".", "каталог, в который скаффолдить артефакты")
	force := fs.Bool("force", false, "перезаписывать существующие файлы")
	agent := fs.String("agent", "", fmt.Sprintf("агентское приложение (%s); дефолт %s", strings.Join(agentKeys(), ", "), defaultAgentKey))
	embedderProvider := fs.String("embedder-provider", "", fmt.Sprintf("провайдер эмбеддинга (%s); дефолт %s", strings.Join(embedderProviderOptions, ", "), defaultEmbedderProvider))
	embedderURL := fs.String("embedder-url", "", "base URL эмбеддера (дефолт зависит от провайдера)")
	embedderModel := fs.String("embedder-model", "", "модель эмбеддинга (дефолт зависит от провайдера)")
	ollamaURL := fs.String("ollama-url", "", "алиас -embedder-url (для embedder-provider=ollama, дефолт "+defaultOllamaBaseURL+")")
	ollamaModel := fs.String("ollama-model", "", "алиас -embedder-model (для embedder-provider=ollama, дефолт "+defaultOllamaModel+")")
	reasonerProvider := fs.String("reasoner-provider", "", fmt.Sprintf("провайдер reasoner (%s); дефолт %s", strings.Join(reasonerProviderOptions, ", "), defaultReasonerProvider))
	store := fs.String("store", "", "путь к файлу SQLite (дефолт "+defaultStorePath+")")
	deepseekKey := fs.String("deepseek-key", "", "ключ DeepSeek API — пишется в <dir>/.env, только если reasoner-provider=deepseek (осторожно: попадёт в историю shell)")
	openaiKey := fs.String("openai-key", "", "ключ OpenAI API (или env OPENAI_API_KEY) — пишется в <dir>/.env, только если embedder-provider=openai")
	hooks := fs.String("hooks", "", fmt.Sprintf("хуки Claude Code для ambient-режима (%s); дефолт %s в интерактивном режиме, %s без него", strings.Join(hooksOptions, ", "), hooksSessionStart, hooksNone))
	noInput := fs.Bool("no-input", false, "не спрашивать интерактивно — использовать только флаги и дефолты (без сетевых запросов)")
	_ = fs.Parse(args) // flag.ExitOnError сам завершает процесс при ошибке разбора

	openaiKeyVal := *openaiKey
	if openaiKeyVal == "" {
		openaiKeyVal = os.Getenv(openaiAPIKeyEnv)
	}

	cfg := initConfig{
		Project:          *project,
		Dir:              *dir,
		Force:            *force,
		Agent:            *agent,
		EmbedderProvider: *embedderProvider,
		EmbedderBaseURL:  cmp.Or(*embedderURL, *ollamaURL),
		EmbedderModel:    cmp.Or(*embedderModel, *ollamaModel),
		ReasonerProvider: *reasonerProvider,
		StorePath:        *store,
		DeepseekKey:      *deepseekKey,
		OpenAIKey:        openaiKeyVal,
		Hooks:            *hooks,
	}

	if !*noInput && isInteractiveStdin() {
		updated, err := promptInitTUI(context.Background(), out, cfg)
		if err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				return fmt.Errorf("init: отменено пользователем")
			}
			return fmt.Errorf("init: интерактивный ввод: %w", err)
		}
		cfg = updated
	}
	// Дефолты незаполненных полей проставляет doInit (applyInitDefaults) —
	// он же единая точка входа для тестов, поэтому здесь второй раз не нужно.
	return doInit(cfg, out)
}

// isInteractiveStdin проверяет, что stdin — реальный терминал (а не пайп/
// файл/перенаправление/redirect с /dev/null): интерактивный TUI-промпт имеет
// смысл только в этом случае, иначе `memoryd init` должен остаться
// скриптуемым и не блокироваться на рендере bubbletea, у которого нет TTY.
// os.ModeCharDevice здесь недостаточно: /dev/null — тоже символьное
// устройство (частый redirect stdin в CI/тестовом окружении), поэтому
// используется настоящая ioctl-проверка терминала (isatty), та же, что
// применяют сами bubbletea/huh при определении, поднимать ли полноэкранный
// рендер — иначе huh попытается открыть /dev/tty и упадёт там, где раньше
// promptLine/promptChoice тихо получали EOF и откатывались на дефолты.
func isInteractiveStdin() bool {
	fd := os.Stdin.Fd()
	return isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)
}

// applyInitDefaults заполняет дефолтами поля, оставшиеся пустыми после флагов
// и (опционального) интерактивного промпта, и обнуляет ключи провайдеров, не
// выбранных пользователем (задача 11.1: "ключи только у выбранных
// провайдеров" — защита от рассинхрона флагов, напр. -deepseek-key при
// -reasoner-provider=noop). Project намеренно не трогается — его резолвит
// resolveProjectName из dir, если явное имя не задано.
func applyInitDefaults(cfg initConfig) initConfig {
	if cfg.Dir == "" {
		cfg.Dir = "."
	}
	if cfg.Agent == "" {
		cfg.Agent = defaultAgentKey
	}
	if cfg.EmbedderProvider == "" {
		cfg.EmbedderProvider = defaultEmbedderProvider
	}
	switch cfg.EmbedderProvider {
	case "ollama":
		if cfg.EmbedderBaseURL == "" {
			cfg.EmbedderBaseURL = defaultOllamaBaseURL
		}
		if cfg.EmbedderModel == "" {
			cfg.EmbedderModel = defaultOllamaModel
		}
	case "openai":
		if cfg.EmbedderBaseURL == "" {
			cfg.EmbedderBaseURL = defaultOpenAIBaseURL
		}
		if cfg.EmbedderModel == "" {
			cfg.EmbedderModel = defaultOpenAIModel
		}
	default: // "noop" — base_url/model эмбеддеру не нужны
		cfg.EmbedderBaseURL = ""
		cfg.EmbedderModel = ""
	}
	if cfg.ReasonerProvider == "" {
		cfg.ReasonerProvider = defaultReasonerProvider
	}
	clearUnselectedProviderKeys(&cfg, embedderProviders, cfg.EmbedderProvider)
	clearUnselectedProviderKeys(&cfg, reasonerProviders, cfg.ReasonerProvider)
	if cfg.StorePath == "" {
		cfg.StorePath = defaultStorePath
	}
	return cfg
}

// dirBaseName — базовое имя абсолютного пути dir как кандидат в имя проекта,
// либо "" если из каталога нельзя вывести осмысленное имя (ошибка резолва, или
// корневой/безымянный каталог "."/"/"). Общий хелпер для промпта (дефолт имени
// проекта — при "" промпт покажется без дефолта) и resolveProjectName (при "" —
// внятная ошибка).
func dirBaseName(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	base := filepath.Base(abs)
	if base == "" || base == "." || base == string(filepath.Separator) {
		return ""
	}
	return base
}

// doInit — собственно логика init, отделённая от разбора флагов/промптов для
// тестов.
func doInit(cfg initConfig, out io.Writer) error {
	// Валидация провайдеров — ДО applyInitDefaults (которая иначе заполнила бы
	// пустое значение дефолтом и опечатка перестала бы отличаться от "не
	// задано"): пустое значение по-прежнему валидно (дефолт провайдера),
	// неизвестное — внятная ошибка, а не тихий разъезд default-веток рендера
	// TOML/.env (задача 11.1, цикл исправлений — раньше renderEmbedderTomlBlock/
	// renderReasonerTomlBlock/applyInitDefaults расходились по default-кейсу
	// switch при опечатке в -embedder-provider/-reasoner-provider).
	if cfg.EmbedderProvider != "" {
		if _, ok := providerByKey(embedderProviders, cfg.EmbedderProvider); !ok {
			return fmt.Errorf("неизвестный провайдер эмбеддинга %q — поддерживаются: %s", cfg.EmbedderProvider, strings.Join(embedderProviderOptions, ", "))
		}
	}
	if cfg.ReasonerProvider != "" {
		if _, ok := providerByKey(reasonerProviders, cfg.ReasonerProvider); !ok {
			return fmt.Errorf("неизвестный провайдер reasoner %q — поддерживаются: %s", cfg.ReasonerProvider, strings.Join(reasonerProviderOptions, ", "))
		}
	}

	cfg = applyInitDefaults(cfg)

	target, ok := agentRegistry[cfg.Agent]
	if !ok {
		return fmt.Errorf("неизвестное агентское приложение %q — поддерживаются: %s", cfg.Agent, strings.Join(agentKeys(), ", "))
	}

	project, err := resolveProjectName(cfg.Project, cfg.Dir)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return fmt.Errorf("создание каталога %q: %w", cfg.Dir, err)
	}

	command := "memoryd" // фолбэк на PATH, если os.Executable() недоступен
	if exe, err := os.Executable(); err == nil {
		command = exe
	} else {
		fmt.Fprintf(out, "не удалось определить путь к memoryd (%v) — в MCP-конфиг записана команда \"memoryd\", проверь, что она доступна в PATH\n", err)
	}

	mcpContent, err := target.renderMCPConfig(command, cfg)
	if err != nil {
		return err
	}
	tomlContent := fmt.Sprintf(configTomlTemplate,
		escapeTOMLString(project), escapeTOMLString(cfg.StorePath), renderEmbedderTomlBlock(cfg), renderReasonerTomlBlock(cfg))

	if err := writeScaffoldFile(target.mcpConfigPath(cfg.Dir), mcpContent, cfg.Force, out); err != nil {
		return err
	}

	for _, spec := range target.skills {
		skillPath := filepath.Join(target.skillsDir(cfg.Dir), spec.dirName, "SKILL.md")
		if err := os.MkdirAll(filepath.Dir(skillPath), 0o755); err != nil {
			return fmt.Errorf("создание каталога %q: %w", filepath.Dir(skillPath), err)
		}
		if err := writeScaffoldFile(skillPath, spec.render(project), cfg.Force, out); err != nil {
			return err
		}
	}

	if err := writeScaffoldFile(filepath.Join(cfg.Dir, "agentmemory.toml"), tomlContent, cfg.Force, out); err != nil {
		return err
	}

	// .env — только ключи выбранных провайдеров (DESIGN.md §13, задача 11.1):
	// applyInitDefaults уже обнулил ключи невыбранных провайдеров выше, здесь
	// достаточно проверить, что оставшееся непусто. selectedProviders — тот же
	// реестр, из которого renderMCPJSON/renderOpencodeJSON берут env-плейсхолдеры.
	envVars := map[string]string{}
	for _, p := range selectedProviders(cfg) {
		if f := p.keyField(&cfg); f != nil && *f != "" {
			envVars[p.envKey] = *f
		}
	}
	if len(envVars) > 0 {
		if err := writeEnvFile(filepath.Join(cfg.Dir, ".env"), envVars, cfg.Force, out); err != nil {
			return err
		}
		if err := ensureGitignoreEntry(filepath.Join(cfg.Dir, ".gitignore"), ".env", out); err != nil {
			return err
		}
	}

	if cfg.Agent == "claude" {
		if mappings, ok := hookEventsByChoice[cfg.Hooks]; ok {
			if err := setupClaudeHooks(cfg.Dir, command, mappings, out); err != nil {
				return err
			}
		}
	} else {
		// Ambient-режим (авто-recall через хуки) — механика харнесса Claude
		// Code; у второго поддерживаемого приложения (opencode) аналога нет —
		// честно сообщаем об этом, а не молчим (задача 7.2).
		fmt.Fprintf(out, "ambient-режим (авто-recall через хуки) поддерживает только Claude Code — для %s недоступен\n", target.label)
	}

	fmt.Fprintf(out, "проект: %s\n", project)
	fmt.Fprintf(out, "агент: %s\n", target.label)
	fmt.Fprintf(out, "эмбеддинг: %s\n", cfg.EmbedderProvider)
	fmt.Fprintf(out, "reasoner: %s\n", cfg.ReasonerProvider)
	fmt.Fprintf(out, "путь БД: %s\n", cfg.StorePath)
	fmt.Fprintln(out, "дальше: перезапусти агентское приложение в этом каталоге и одобри сервер `agentmemory`; убедись, что memoryd доступен — `go install` кладёт его на PATH (см. `make install`).")
	return nil
}

// resolveProjectName возвращает имя проекта: явно заданное project, либо (если
// пусто) базовое имя абсолютного пути dir. Ошибка — если итоговое имя пусто,
// "." или "/" (корневой/безымянный каталог), т.к. из такого имени нельзя
// вывести осмысленный project, а также если имя содержит управляющий символ
// (перевод строки и т.п.): такое имя сломало бы синтаксис basic-строки в
// сгенерированном agentmemory.toml (escapeTOMLString их не покрывает) и в любом
// случае не годится как пространство имён scope project:<имя>.
func resolveProjectName(project, dir string) (string, error) {
	if project == "" {
		project = dirBaseName(dir)
		if project == "" {
			return "", fmt.Errorf("не удалось определить имя проекта из каталога %q — укажи явно -project", dir)
		}
	}

	if strings.IndexFunc(project, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("имя проекта %q содержит управляющий символ — укажи корректное -project", project)
	}
	return project, nil
}

// escapeTOMLString экранирует значение для подстановки в TOML-строку в двойных
// кавычках (обратный слэш и кавычка) — имя каталога/URL/путь, как правило,
// безопасны, но подстановка не должна ломать синтаксис файла в редких случаях.
func escapeTOMLString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// buildProviderEnv собирает карту env-плейсхолдеров ТОЛЬКО для выбранных
// провайдеров (DESIGN.md §13, задача 11.1): выбор — из selectedProviders (единый
// реестр с applyInitDefaults/doInit), значение плейсхолдера формирует placeholder
// по имени переменной (у .mcp.json и opencode.json разный синтаксис интерполяции).
func buildProviderEnv(cfg initConfig, placeholder func(envKey string) string) map[string]string {
	env := map[string]string{}
	for _, p := range selectedProviders(cfg) {
		if p.envKey != "" {
			env[p.envKey] = placeholder(p.envKey)
		}
	}
	return env
}

// renderMCPJSON собирает .mcp.json (сервер agentmemory, stdio) через
// encoding/json — гарантированно валидный JSON с отступом 2 пробела. env
// содержит ТОЛЬКО плейсхолдеры выбранных провайдеров (shell-синтаксис "${VAR:-}").
func renderMCPJSON(command string, cfg initConfig) (string, error) {
	env := buildProviderEnv(cfg, func(envKey string) string { return "${" + envKey + ":-}" })
	mcpCfg := mcpConfigFile{
		MCPServers: map[string]mcpServerSpec{
			"agentmemory": {
				Command: command,
				Args:    mcpServerArgs,
				Env:     env,
			},
		},
	}
	b, err := json.MarshalIndent(mcpCfg, "", "  ")
	if err != nil {
		return "", fmt.Errorf("сборка .mcp.json: %w", err)
	}
	return string(b) + "\n", nil
}

// opencodeConfigFile / opencodeServerSpec — форма opencode.json для local MCP-
// сервера (opencode-документация). В отличие от .mcp.json у Claude Code:
//   - command — единый массив [команда, аргументы...], а не раздельные
//     command+args;
//   - интерполяция env — синтаксис "{env:VAR}", а не shell-style "${VAR:-}";
//   - поле окружения называется environment, а не env.
type opencodeConfigFile struct {
	Schema string                        `json:"$schema"`
	MCP    map[string]opencodeServerSpec `json:"mcp"`
}

type opencodeServerSpec struct {
	Type        string            `json:"type"`
	Command     []string          `json:"command"`
	Enabled     bool              `json:"enabled"`
	Environment map[string]string `json:"environment"`
}

// renderOpencodeJSON собирает opencode.json (local MCP-сервер agentmemory)
// через encoding/json — гарантированно валидный JSON с отступом 2 пробела.
// environment — те же правила выбора ключей, что у renderMCPJSON. Путь к
// agentmemory.toml — относительный: opencode запускает local MCP-процесс с
// рабочим каталогом = корень workspace (в его исходниках cwd незаданного
// mcp.cwd резолвится в baseDir — корень инстанса), откуда и подхватывается
// "agentmemory.toml". Абсолютный указатель уровня ${CLAUDE_PROJECT_DIR} у
// opencode не нужен — своего аналога он не задаёт.
func renderOpencodeJSON(command string, cfg initConfig) (string, error) {
	env := buildProviderEnv(cfg, func(envKey string) string { return "{env:" + envKey + "}" })
	ocCfg := opencodeConfigFile{
		Schema: "https://opencode.ai/config.json",
		MCP: map[string]opencodeServerSpec{
			"agentmemory": {
				Type:        "local",
				Command:     []string{command, "-config", "agentmemory.toml"},
				Enabled:     true,
				Environment: env,
			},
		},
	}
	b, err := json.MarshalIndent(ocCfg, "", "  ")
	if err != nil {
		return "", fmt.Errorf("сборка opencode.json: %w", err)
	}
	return string(b) + "\n", nil
}

// writeScaffoldFile пишет content в path с правами 0o644. Если файл уже
// существует и force не задан — пропускает его с предупреждением в out (не
// ошибка): повторный `memoryd init` идемпотентен и безопасен.
func writeScaffoldFile(path, content string, force bool, out io.Writer) error {
	if !force {
		if _, err := os.Stat(path); err == nil {
			fmt.Fprintf(out, "пропущено (уже существует): %s\n", path)
			return nil
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("проверка %q: %w", path, err)
		}
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("запись %q: %w", path, err)
	}
	fmt.Fprintf(out, "записано: %s\n", path)
	return nil
}

// writeEnvFile пишет vars (KEY=VALUE построчно, отсортировано по ключу для
// детерминированного вывода) в path с правами 0o600 (секрет). Пишутся только
// ключи выбранных провайдеров — vars собирает doInit. Идемпотентность и
// -force семантика — как у writeScaffoldFile.
func writeEnvFile(path string, vars map[string]string, force bool, out io.Writer) error {
	if !force {
		if _, err := os.Stat(path); err == nil {
			fmt.Fprintf(out, "пропущено (уже существует): %s\n", path)
			return nil
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("проверка %q: %w", path, err)
		}
	}

	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&sb, "%s=%s\n", k, vars[k])
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		return fmt.Errorf("запись %q: %w", path, err)
	}
	fmt.Fprintf(out, "записано: %s (ключи: %s, права 0600)\n", path, strings.Join(keys, ", "))
	return nil
}

// ensureGitignoreEntry дописывает entry отдельной строкой в конец path (создаёт
// файл, если его нет), если такой строки там ещё нет — без дублей.
func ensureGitignoreEntry(path, entry string, out io.Writer) error {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("чтение %q: %w", path, err)
	}
	for _, line := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(line) == entry {
			return nil // уже есть — не дублируем
		}
	}

	content := string(existing)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += entry + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("запись %q: %w", path, err)
	}
	fmt.Fprintf(out, "обновлено: %s (добавлена строка %q)\n", path, entry)
	return nil
}

// hookConfigAnchor — путь к agentmemory.toml, который подставляется в команду
// хука: ${CLAUDE_PROJECT_DIR} — тот же приём, что уже используется для
// mcpServerArgs (.mcp.json) — Claude Code подставляет его на абсолютный корень
// проекта; ":-." — фолбэк на текущий каталог, если переменная почему-то не
// задана (симметрично mcpServerArgs).
const hookConfigAnchor = "${CLAUDE_PROJECT_DIR:-.}/agentmemory.toml"

// buildHookCommand собирает shell-команду одной matcher-записи хука:
// абсолютный путь к бинарю memoryd (memorydPath — тот же, что уже разрешён в
// doInit для .mcp.json, см. os.Executable()/фолбэк на "memoryd" из PATH) +
// подкоманда `hook` с нужным -event и -config. memorydPath обёрнут в кавычки
// (%q) — путь может содержать пробелы.
func buildHookCommand(memorydPath, event string) string {
	return fmt.Sprintf("%q hook -event %s -config %s", memorydPath, event, hookConfigAnchor)
}

// setupClaudeHooks мержит в <dir>/.claude/settings.json команды `memoryd hook`
// для событий из mappings (DESIGN.md §12, задача 7.2) и печатает в out, что
// именно обновлено.
func setupClaudeHooks(dir, memorydPath string, mappings []hookEventMapping, out io.Writer) error {
	eventCommands := make(map[string]string, len(mappings))
	claudeEvents := make([]string, 0, len(mappings))
	for _, m := range mappings {
		eventCommands[m.ClaudeEvent] = buildHookCommand(memorydPath, m.MemorydEvent)
		claudeEvents = append(claudeEvents, m.ClaudeEvent)
	}

	settingsPath := filepath.Join(dir, ".claude", "settings.json")
	if err := mergeClaudeHooks(settingsPath, eventCommands); err != nil {
		return err
	}
	fmt.Fprintf(out, "обновлено: %s (хуки: %s)\n", settingsPath, strings.Join(claudeEvents, ", "))
	return nil
}

// hookMatcherEntry — одна matcher-запись в массиве события секции `hooks`
// settings.json Claude Code: matcher (пустая строка — срабатывает всегда) +
// список команд. Сама команда описана как map[string]any, а не отдельным
// типом с полями type/command — так merge не теряет чужие ключи вроде
// "timeout" у уже существующих записей при пере-сериализации.
type hookMatcherEntry struct {
	Matcher string           `json:"matcher"`
	Hooks   []map[string]any `json:"hooks"`
}

// mergeClaudeHooks дописывает в settingsPath команды из eventCommands (ключ —
// имя события Claude Code, значение — shell-команда), не трогая остальные
// ключи файла верхнего уровня и остальные события/matcher-записи секции
// `hooks` (DESIGN.md §12, задача 7.2). Существующий settingsPath парсится как
// map[string]json.RawMessage — все ключи, кроме "hooks", остаются как есть
// байт-в-байт; сама секция "hooks" разбирается точечно только на глубину
// событие -> []hookMatcherEntry, поэтому события, которые эта функция не
// трогает (Stop, PreToolUse и т.п.), проходят через неё без потерь.
// Идемпотентно: если для события уже есть matcher-запись с точно такой же
// командой — новая не добавляется. Битый существующий JSON — ошибка, файл не
// создаётся и не перезаписывается.
func mergeClaudeHooks(settingsPath string, eventCommands map[string]string) error {
	raw := map[string]json.RawMessage{}
	existing, err := os.ReadFile(settingsPath)
	switch {
	case err == nil:
		if err := json.Unmarshal(existing, &raw); err != nil {
			return fmt.Errorf("разбор %q: %w", settingsPath, err)
		}
	case os.IsNotExist(err):
		// файла нет — начинаем с пустого объекта верхнего уровня
	default:
		return fmt.Errorf("чтение %q: %w", settingsPath, err)
	}

	hooksSection := map[string][]hookMatcherEntry{}
	if rawHooks, ok := raw["hooks"]; ok {
		if err := json.Unmarshal(rawHooks, &hooksSection); err != nil {
			return fmt.Errorf("разбор секции hooks в %q: %w", settingsPath, err)
		}
	}

	// Сортированный порядок событий — детерминированный вывод/тесты.
	events := make([]string, 0, len(eventCommands))
	for ev := range eventCommands {
		events = append(events, ev)
	}
	sort.Strings(events)

	for _, event := range events {
		command := eventCommands[event]
		if hasHookCommand(hooksSection[event], command) {
			continue // идемпотентность: команда уже вписана
		}
		hooksSection[event] = append(hooksSection[event], hookMatcherEntry{
			Matcher: "",
			Hooks:   []map[string]any{{"type": "command", "command": command}},
		})
	}

	hooksRaw, err := json.Marshal(hooksSection)
	if err != nil {
		return fmt.Errorf("сборка секции hooks: %w", err)
	}
	raw["hooks"] = hooksRaw

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("сборка %q: %w", settingsPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		return fmt.Errorf("создание каталога %q: %w", filepath.Dir(settingsPath), err)
	}
	if err := os.WriteFile(settingsPath, append(out, '\n'), 0o644); err != nil {
		return fmt.Errorf("запись %q: %w", settingsPath, err)
	}
	return nil
}

// hasHookCommand проверяет, есть ли среди matcher-записей entries хотя бы
// одна команда, точно равная command.
func hasHookCommand(entries []hookMatcherEntry, command string) bool {
	for _, e := range entries {
		for _, h := range e.Hooks {
			if cmd, _ := h["command"].(string); cmd == command {
				return true
			}
		}
	}
	return false
}
