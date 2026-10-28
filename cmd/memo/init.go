// Подкоманда `memo init` — интерактивный установщик памяти в проект под
// конкретное агентское приложение (DESIGN.md §14 Этап 5, задачи 5.3/5.4;
// выбор провайдеров и TUI — задача 11.1). Раскладывает в целевой каталог
// MCP-конфиг, скил-инструкцию «Протокол работы с памятью», agentmemory.toml
// (с введённым провайдером эмбеддинга/reasoner, store, project) и, если
// введены ключи выбранных провайдеров, .env (+ строку .env в .gitignore). Не
// трогает загрузку конфига (internal/config) и серверный путь.
//
// Повторный `memo init` в уже инициализированном проекте ведёт себя по-разному
// в зависимости от файла: agentmemory.toml всегда перезаписывается целиком
// (overwriteFile) — он полностью выводится из параметров текущего прогона, и
// -force на него не влияет; MCP-конфиг (.mcp.json/opencode.json) мержится
// (mergeMCPConfig) — запись сервера memo (пере)генерируется из параметров
// текущего прогона, но чужие серверы и прочие top-level ключи файла
// сохраняются, тоже независимо от -force; .env мержится (mergeEnvFile) —
// ключи выбранных провайдеров дописываются/обновляются, остальные строки
// сохраняются, тоже независимо от -force; скиллы SKILL.md — единственные,
// кто уважает -force в исходном смысле "пропустить существующий файл"
// (writeScaffoldFile).
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
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/huh"
	"github.com/mattn/go-isatty"

	"agentmemory/internal/config"
	"agentmemory/internal/memory/store"
)

// Дефолты полей initConfig, не заданных ни флагом, ни интерактивным вводом.
//
// defaultOllamaBaseURL/defaultOpenAIBaseURL — единый источник истины для
// провайдер-специфичного дефолтного base URL эмбеддера во всём пакете cmd/memo:
// их пишет `memo init` в agentmemory.toml (applyInitDefaults/wizard) И на них же
// падает рантайм-фабрика newEmbedder и проба `memo doctor`, когда embedder.base_url
// в конфиге пуст (main.go, doctor.go). Дефолт провайдер-специфичен и намеренно НЕ
// живёт в config.Default() (там EmbedderConfig.BaseURL всегда "") — иначе
// ollama-специфичный localhost:11434 утёк бы во второй провайдер при пустом
// base_url в TOML. Держать одну пару констант обязательно: разъезд init- и
// рантайм-дефолтов дал бы молчаливую FTS5-деградацию (init записал один адрес,
// проба ходит на другой).
const (
	defaultAgentKey         = "claude"
	defaultEmbedderProvider = "ollama"
	defaultOllamaBaseURL    = "http://localhost:11434"
	defaultOllamaModel      = "bge-m3"
	defaultOpenAIBaseURL    = "https://api.openai.com"
	defaultOpenAIModel      = "text-embedding-3-small"
	defaultReasonerProvider = "deepseek"
	// Дефолтные base URL OpenAI-совместимых reasoner'ов — по тому же правилу
	// «одна константа на init и рантайм», что у эмбеддера: на них ссылается
	// реестр reasonerProviders, из которого берут адрес и newReasoner, и проба
	// doctor'а, и запрос списка моделей в мастере. У DeepSeek API нет префикса
	// /v1, у CLIProxyAPI он есть (порт 8317 — дефолт его config.yaml).
	defaultDeepSeekBaseURL = "https://api.deepseek.com"
	defaultDeepSeekModel   = "deepseek-chat"
	defaultCLIProxyBaseURL = "http://127.0.0.1:8317/v1"
	defaultStorePath       = "~/.agentmemory/memory.db"

	// Дефолты полей подключения к PostgreSQL (DESIGN.md §13, §14 Этап 12).
	// Хоста среди них нет намеренно: адрес общего сервера команды —
	// единственный параметр, у которого не бывает осмысленного дефолта (у
	// port/sslmode он есть в config.Default(), user/dbname по конвенции из
	// README «Общая база команды» — роль и база называются memo), поэтому
	// host остаётся пустым и обязателен: интерактивно — валидацией непустоты
	// в промпте, неинтерактивно — через config.StoreConfig.Validate в doInit.
	defaultPostgresPort    = 5432
	defaultPostgresUser    = "memo"
	defaultPostgresDBName  = "memo"
	defaultPostgresSSLMode = "disable"

	// fetchModelsTimeout — таймаут запроса списка моделей у ollama/OpenAI в
	// интерактивном промпте: недоступный/зависший сервер не должен вешать
	// установку навсегда (задача 11.1 — "не блокировать установку").
	fetchModelsTimeout = 5 * time.Second

	// probeStoreTimeout — таймаут пробы подключения к PostgreSQL
	// (probeStoreConnectionLive): тот же инвариант, что у fetchModelsTimeout —
	// установка не должна блокироваться недоступным сервисом ни интерактивно,
	// ни в CI под -no-input. Значение больше, чем у списка моделей: проба
	// делает полноценное соединение (DNS + TCP + стартовый хендшейк pgx, при
	// sslmode != disable ещё и TLS) по, возможно, не локальной сети. Без
	// дедлайна вовсе процесс висел бы до таймаута ОС на хосте, который дропает
	// пакеты (закрытый firewall, опечатка в адресе).
	probeStoreTimeout = 10 * time.Second

	// sqliteMemoryCountTimeout — таймаут read-only подсчёта записей в
	// локальном файле SQLite (findExistingSqliteMemory -> nonEmptySQLiteFile
	// -> store.SQLiteMemoryCountContext, задача 12.1 доработок): тот же
	// инвариант "установка не виснет", что у probeStoreTimeout, только для
	// локального диска, а не сети. Отдельная, более короткая константа, а не
	// переиспользование probeStoreTimeout: природа операции другая (файловый
	// ввод-вывод обычно на порядки быстрее сетевого хендшейка) и не должна
	// незаметно связывать два таймаута друг с другом при будущей правке
	// одного из них; значения хватает с запасом на обычный локальный диск, но
	// не даёт зависнуть, если путь ведёт на подвисший сетевой маунт (NFS/SMB).
	sqliteMemoryCountTimeout = 3 * time.Second
)

// providerSpec описывает один провайдер эмбеддинга/reasoner: значение флага
// -embedder-provider/-reasoner-provider (key), человекочитаемый label для
// help-текста и TUI-select (wizard.go), и env-переменную с API-ключом (envKey
// — пусто, если провайдеру ключ не нужен, напр. ollama/noop). Единый источник
// метаданных провайдера — по образцу agentRegistry: из embedderProviders/
// reasonerProviders деривятся список опций help/select, валидация флага и
// выбор env-ключа в buildMCPServerEntry/buildOpencodeServerEntry/doInit (задача 11.1,
// цикл исправлений — раньше эти четыре вещи были рассинхронизируемыми копиями
// в разных функциях).
type providerSpec struct {
	key    string
	label  string
	envKey string
	// baseURL/defaultModel — дефолтные корень API и модель провайдера reasoner
	// (пусто у noop; у cliproxy нет дефолтной модели — набор зависит от аккаунтов
	// прокси). У эмбеддеров дефолты пока живут в switch'ах applyInitDefaults/newEmbedder.
	baseURL      string
	defaultModel string
}

// embedderProviders/reasonerProviders — реестры провайдеров в порядке показа
// в select-промпте (первый — дефолт).
var embedderProviders = []providerSpec{
	{key: "ollama", label: "ollama (локальный сервер)"},
	{key: "openai", label: "openai (облако или совместимый сервер)", envKey: openaiAPIKeyEnv},
	{key: "noop", label: "noop (без вектора, только полнотекстовый поиск)"},
}

var reasonerProviders = []providerSpec{
	{key: "deepseek", label: "deepseek", envKey: deepseekAPIKeyEnv, baseURL: defaultDeepSeekBaseURL, defaultModel: defaultDeepSeekModel},
	{key: "cliproxy", label: "cliproxy (локальный CLIProxyAPI: Gemini, Claude, Codex по подписке)", envKey: cliproxyAPIKeyEnv, baseURL: defaultCLIProxyBaseURL},
	{key: "noop", label: "noop (без LLM)"},
}

// storeDriverSQLite/storeDriverPostgres — значения флага -store-driver и поля
// `driver` секции [store] генерируемого agentmemory.toml (DESIGN.md §13, §14
// Этап 12). Пустое значение эквивалентно sqlite — это контракт
// config.StoreConfig.Validate, а не локальное соглашение init.
const (
	storeDriverSQLite   = "sqlite"
	storeDriverPostgres = "postgres"
)

// storeDrivers — бэкенды хранилища в порядке показа в select-промпте (первый —
// дефолт), по образцу embedderProviders/reasonerProviders: один источник и для
// help-текста -store-driver, и для вариантов TUI (wizard.go). Тип переиспользован
// (providerSpec) ради общих providerKeys/providerSelectOptions; поле envKey
// намеренно пусто — секрет бэкенда (MEMO_POSTGRES_PASSWORD) привязан к драйверу
// жёстко и кладётся в .env не через реестр провайдеров, а отдельной веткой в
// doInit (см. там же, почему).
var storeDrivers = []providerSpec{
	{key: storeDriverSQLite, label: "sqlite (локальный файл — дефолт)"},
	{key: storeDriverPostgres, label: "postgres (общая база команды: PostgreSQL + pgvector)"},
}

var storeDriverOptions = providerKeys(storeDrivers)

// validateStoreDriverKey проверяет значение флага -store-driver по реестру
// storeDrivers (пусто — валидно, это дефолтный sqlite). Сообщение — в стиле
// валидации провайдеров в doInit. Итоговую валидацию конфигурации хранилища
// всё равно делает config.StoreConfig.Validate (в doInit, вместе с проверкой
// обязательных полей postgres) — здесь ранняя проверка нужна только затем,
// чтобы опечатка не увела интерактивный мастер по ветке другого бэкенда.
func validateStoreDriverKey(driver string) error {
	if driver == "" {
		return nil
	}
	if _, ok := providerByKey(storeDrivers, driver); !ok {
		return fmt.Errorf("неизвестный бэкенд хранилища %q — поддерживаются: %s", driver, strings.Join(storeDriverOptions, ", "))
	}
	return nil
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
// провайдера (DeepseekKey для DEEPSEEK_API_KEY, CLIProxyKey для CLIPROXY_API_KEY,
// OpenAIKey для OPENAI_API_KEY),
// либо nil, если провайдер (envKey=="") ключа не требует. Единственное место,
// связывающее провайдера с полем initConfig, хранящим его секрет.
func (p providerSpec) keyField(cfg *initConfig) *string {
	switch p.envKey {
	case deepseekAPIKeyEnv:
		return &cfg.DeepseekKey
	case cliproxyAPIKeyEnv:
		return &cfg.CLIProxyKey
	case openaiAPIKeyEnv:
		return &cfg.OpenAIKey
	default:
		return nil
	}
}

// selectedProviders возвращает записи реестров, соответствующие выбранным в
// cfg провайдерам эмбеддинга и reasoner (в этом порядке) — общий источник для
// buildMCPServerEntry/buildOpencodeServerEntry (какие env-плейсхолдеры класть
// в .mcp.json/opencode.json) и doInit (какие ключи класть в .env). cfg должен быть уже
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

// Значения initConfig.Hooks — какие хуки харнесса подключить для
// ambient-режима (авто-recall, DESIGN.md §12, задача 7.2); у Claude Code они
// свои, у dsh — через мост dsh-hooks-claude-code (см. cmd/memo/dsh.go), но
// набор событий и их имена общие (формат Claude Code). hooksOptions — порядок
// вариантов в интерактивном промпте (первый — дефолт: session-start один
// покрывает основной сценарий «recall в начале задачи» дешевле всего;
// prompt-submit добавляет цену на каждое сообщение, поэтому не дефолт).
const (
	hooksSessionStart = "session-start"
	hooksBoth         = "session-start+prompt-submit"
	hooksNone         = "none"
)

var hooksOptions = []string{hooksSessionStart, hooksBoth, hooksNone}

// hookEventMapping — одно событие Claude Code (ключ секции `hooks` в
// settings.json) и соответствующее значение флага `-event` подкоманды
// `memo hook`.
type hookEventMapping struct {
	ClaudeEvent string // "SessionStart" | "UserPromptSubmit"
	MemoEvent   string // "session-start" | "prompt-submit"
}

// hookEventsByChoice — какие события подключает каждое значение
// initConfig.Hooks. hooksNone намеренно отсутствует — для него подключать
// нечего.
var hookEventsByChoice = map[string][]hookEventMapping{
	hooksSessionStart: {
		{ClaudeEvent: "SessionStart", MemoEvent: "session-start"},
	},
	hooksBoth: {
		{ClaudeEvent: "SessionStart", MemoEvent: "session-start"},
		{ClaudeEvent: "UserPromptSubmit", MemoEvent: "prompt-submit"},
	},
}

// configTomlTemplate — шаблон agentmemory.toml по образцу корневого
// agentmemory.toml/config.example.toml (DESIGN.md §13). Константа-строка, а не
// toml.Marshal — так сохраняются комментарии. %[1]s — экранированное имя
// проекта, %[2]s — тело секции [store] (renderStoreTomlBlock), %[3]s — тело
// секции [embedder] (renderEmbedderTomlBlock), %[4]s — тело секции [reasoner]
// (renderReasonerTomlBlock) — все три зависят от выбранных в этом прогоне
// бэкенда хранилища и провайдеров.
const configTomlTemplate = `# Конфиг AgentMemory для проекта %[1]s (DESIGN.md §13).
#
# Агентское приложение поднимает MCP-сервер memo из сгенерированного
# рядом MCP-конфига и передаёт ему этот файл флагом -config.

[store]
%[2]s
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

// renderStoreTomlBlock рендерит тело секции [store] под выбранный
// cfg.StoreDriver — единственное место, определяющее, какие поля и комментарии
// попадают в agentmemory.toml для каждого бэкенда (DESIGN.md §13, §14 Этап 12),
// по образцу renderEmbedderTomlBlock/renderReasonerTomlBlock.
//
// Для sqlite поле driver НЕ пишется намеренно: пустое значение по контракту
// config.StoreConfig.Validate и так означает sqlite, а дефолтный вывод
// `memo init` обязан остаться побайтово тем же, что до появления второго
// бэкенда (старые конфиги и ожидания существующих тестов не должны
// разъезжаться из-за новой строки).
//
// Пароль postgres здесь не появляется ни при каких значениях cfg — это
// инвариант, а не забывчивость: секрет живёт только в env/.env
// (MEMO_POSTGRES_PASSWORD, DESIGN.md §13), ровно как ключи провайдеров.
func renderStoreTomlBlock(cfg initConfig) string {
	switch cfg.StoreDriver {
	case storeDriverPostgres:
		return fmt.Sprintf(`# Общая база команды: PostgreSQL + pgvector (DESIGN.md §14 Этап 12) — одна база
# на нескольких разработчиков вместо локального файла. Схему таблиц memo
# накатывает сам при первом старте, база и роль должны существовать заранее.
# Пароль в этот файл НЕ пишется — читается из env %[1]s (или .env
# рядом с этим конфигом), то же правило, что для DEEPSEEK_API_KEY/OPENAI_API_KEY.
# Пустой пароль — не ошибка (сервер может использовать trust-аутентификацию).
driver  = "postgres"
host    = "%[2]s"
port    = %[3]d
user    = "%[4]s"
dbname  = "%[5]s"
sslmode = "%[6]s"
`, memoPostgresPasswordEnv, escapeTOMLString(cfg.PostgresHost), cfg.PostgresPort,
			escapeTOMLString(cfg.PostgresUser), escapeTOMLString(cfg.PostgresDBName),
			escapeTOMLString(cfg.PostgresSSLMode))
	default: // "" | "sqlite"
		return fmt.Sprintf(`# Общий файл БД на все проекты (DESIGN.md §2.1: один файл, изоляция через scope).
# Каталог создаётся автоматически при первом старте (store.Open).
path = "%s"
`, escapeTOMLString(cfg.StorePath))
	}
}

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
# Если ollama недоступен — memo стартует, recall падает на чистый FTS5 (§15).
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
	case "cliproxy":
		return fmt.Sprintf(`# Локальный CLIProxyAPI (OpenAI-совместимый прокси к Gemini/Claude/Codex по
# подписке). Ключ — один из api-keys его конфига — берётся из env CLIPROXY_API_KEY
# (или .env рядом с этим конфигом), в TOML не пишется (DESIGN.md §13). Пустой
# ключ или неподнятый прокси — деградация до noop (§15), память работает.
provider = "cliproxy"
base_url = "%s"
model    = "%s"
`, escapeTOMLString(cfg.ReasonerBaseURL), escapeTOMLString(cfg.ReasonerModel))
	case "deepseek":
		var baseURL string
		if cfg.ReasonerBaseURL != "" {
			baseURL = fmt.Sprintf("base_url = \"%s\"\n", escapeTOMLString(cfg.ReasonerBaseURL))
		}
		return fmt.Sprintf(`# DeepSeek chat: ключ берётся из env DEEPSEEK_API_KEY. Если в shell-env его нет,
# memo сам подхватывает его из .env рядом с этим конфигом при старте (см.
# loadDotEnv в main.go) — .env раскладывается этим же "memo init". Пустой
# ключ — деградация до noop (§15): recall без суммаризации, консолидация
# недоступна, но память работает.
provider = "deepseek"
%smodel    = "%s"
`, baseURL, escapeTOMLString(cfg.ReasonerModel))
	default:
		// Недостижимо: doInit пропускает только ключи reasonerProviders, а
		// TestRenderReasonerTomlBlock_CoversRegistry ловит провайдера, добавленного
		// в реестр без своей ветки здесь (иначе init молча записал бы чужой provider).
		panic(fmt.Sprintf("renderReasonerTomlBlock: провайдер %q не описан", cfg.ReasonerProvider))
	}
}

// mcpServerArgs — аргументы, с которыми агентское приложение запускает
// бинарь memo как MCP-сервер memo.
var mcpServerArgs = []string{"-config", "${CLAUDE_PROJECT_DIR:-.}/agentmemory.toml"}

// mcpConfigFile / mcpServerSpec — форма .mcp.json (см. корневой .mcp.json).
// mcpServerSpec также используется buildMCPServerEntry для сборки записи
// сервера memo, мержимой в существующий файл (mergeMCPConfig).
type mcpConfigFile struct {
	MCPServers map[string]mcpServerSpec `json:"mcpServers"`
}

type mcpServerSpec struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

// skillSpec описывает один скил, раскладываемый `memo init` в
// `<skillsDir>/<dirName>/SKILL.md`: dirName — подкаталог под
// agentTarget.skillsDir, render — рендерер тела SKILL.md под конкретный
// проект.
type skillSpec struct {
	dirName string
	render  func(project string) string
}

// memorySkills — набор скиллов, которые `memo init` раскладывает КАЖДОМУ
// поддерживаемому агентскому приложению (одинаковый для всех записей
// agentRegistry — различается только корень skillsDir): "memo" — протокол
// работы с памятью (skill.go), "memo-bootstrap" — разовая инициализация
// памяти проекта (bootstrap_skill.go). Добавить следующий скил — одна запись
// в этом слайсе.
var memorySkills = []skillSpec{
	{dirName: "memo", render: renderSkillMD},
	{dirName: "memo-bootstrap", render: renderBootstrapSkillMD},
}

// agentTarget описывает одно агентское приложение и то, как оно раскладывает
// артефакты AgentMemory в проекте: запись MCP-сервера memo (и, если харнесс
// умеет хуки, конфиг ambient-режима) и скилы. В agentRegistry — claude (Claude
// Code), opencode и dsh (DeepSeek Harness); структура рассчитана на то, чтобы
// добавить следующее приложение ещё одной записью.
type agentTarget struct {
	label string // человекочитаемое имя для промптов/вывода

	// scaffold раскладывает всё, что специфично для харнесса: MCP-запись
	// сервера memo и (если харнесс умеет хуки и они выбраны) конфиг
	// ambient-режима. У Claude Code и opencode это мерж JSON-записи в
	// существующий файл (mergeMCPConfig), у dsh — генерация проектного
	// YAML-оверлея Cordis целиком (scaffoldDsh): шов один, потому что у dsh
	// запись сервера и строка моста хуков живут в одном файле, а у Claude Code
	// хуки — вообще другой файл (setupClaudeHooks). Возвращает подсказку о
	// шагах, которые пользователь делает сам (одобрение сервера, подключение
	// оверлея): doInit печатает её в конце вывода, после сводки, чтобы она не
	// терялась среди строк «записано: …».
	scaffold func(dir, project, command string, cfg initConfig, out io.Writer) (string, error)

	// hooksLabel — как называть механику хуков в вопросе визарда об
	// ambient-режиме; пусто — хуков у харнесса нет: промпт не показывается, а
	// scaffold сам сообщает в выводе, что авто-recall недоступен.
	hooksLabel string

	skillsDir func(dir string) string // корень каталога скиллов агента
	skills    []skillSpec
}

// agentRegistry — реестр поддерживаемых агентских приложений, ключ — значение
// флага -agent.
var agentRegistry = map[string]agentTarget{
	"claude": {
		label:      "Claude Code",
		scaffold:   scaffoldClaude,
		hooksLabel: "хуки Claude Code",
		skillsDir: func(dir string) string {
			return filepath.Join(dir, ".claude", "skills")
		},
		skills: memorySkills,
	},
	"opencode": {
		label:    "opencode",
		scaffold: scaffoldOpencode,
		// hooksLabel пуст: у opencode хуков нет.
		skillsDir: func(dir string) string {
			return filepath.Join(dir, ".opencode", "skills")
		},
		skills: memorySkills,
	},
	"dsh": {
		label:    "DeepSeek Harness",
		scaffold: scaffoldDsh,
		// Механику хуков (мост dsh-hooks-claude-code, конфиг формата Claude
		// Code) в формулировке вопроса не называем — это деталь реализации,
		// которую пользователю знать не обязательно; она описана в DESIGN §12
		// и в подсказках scaffold.
		hooksLabel: "хуки DeepSeek Harness",
		// Проектный корень скиллов конвенции agents (в dsh — rank 200; первый
		// по приоритету, .dsh/skills, занимать не стали: в проектах пользователя
		// скилы уже лежат здесь, и этот же корень читают другие агентские
		// приложения).
		skillsDir: func(dir string) string {
			return filepath.Join(dir, ".agents", "skills")
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

// initConfig — параметры одного прогона `memo init`: заданы флагом, либо (в
// интерактивном TTY-режиме) дозапрошены promptInitTUI, либо остаются пустыми
// и получают дефолт в doInit/applyInitDefaults.
type initConfig struct {
	Project          string
	Dir              string
	Agent            string
	EmbedderProvider string // ollama | openai | noop
	EmbedderBaseURL  string // ollama/openai: base URL; noop — не используется
	EmbedderModel    string // ollama/openai: модель эмбеддинга; noop — не используется
	ReasonerProvider string // deepseek | cliproxy | noop
	ReasonerBaseURL  string // cliproxy: base URL; deepseek — дефолт провайдера, в TOML не пишется; noop — не используется
	ReasonerModel    string // deepseek/cliproxy: модель reasoner; noop — не используется
	StoreDriver      string // "" (== sqlite) | sqlite | postgres
	StorePath        string // только если StoreDriver sqlite: путь к файлу БД
	PostgresHost     string // только если StoreDriver == "postgres"
	PostgresPort     int    // только если StoreDriver == "postgres"; 0 — дефолт defaultPostgresPort
	PostgresUser     string // только если StoreDriver == "postgres"
	PostgresDBName   string // только если StoreDriver == "postgres"
	PostgresSSLMode  string // только если StoreDriver == "postgres"
	PostgresPassword string // только если StoreDriver == "postgres"; в TOML не попадает никогда — только в .env
	OpenAIKey        string // только если EmbedderProvider == "openai"
	DeepseekKey      string // только если ReasonerProvider == "deepseek"
	CLIProxyKey      string // только если ReasonerProvider == "cliproxy"
	Hooks            string // "" | session-start | session-start+prompt-submit | none (см. hooksOptions)
	Force            bool
}

// storeConfigFromInit собирает config.StoreConfig из параметров прогона init —
// единственный мост между initConfig и настоящей конфигурацией хранилища.
// Через него идут ВСЕ решения о хранилище, которым нужна логика рантайма:
// валидация (StoreConfig.Validate — не копия её правил в init), проба
// подключения (probeStoreConnection -> store.OpenPostgresReadOnly, которой
// нужен DSN) и человекочитаемое имя цели в выводе/ошибках (StoreConfig.Target).
// Так `memo init` не может разойтись с тем, что сочтёт валидным и куда пойдёт
// сам memo при старте с этим же конфигом.
//
// cfg должен быть уже пропущен через applyInitDefaults — иначе порт/sslmode
// уедут в нули и Target/DSN покажут не то, что реально попадёт в TOML.
func storeConfigFromInit(cfg initConfig) config.StoreConfig {
	return config.StoreConfig{
		Driver:   cfg.StoreDriver,
		Path:     cfg.StorePath,
		Host:     cfg.PostgresHost,
		Port:     cfg.PostgresPort,
		User:     cfg.PostgresUser,
		DBName:   cfg.PostgresDBName,
		SSLMode:  cfg.PostgresSSLMode,
		Password: cfg.PostgresPassword,
	}
}

// probeStoreConnection — проба доступности хранилища перед генерацией конфига
// (интерактивно — wizard.go promptPostgresStore, неинтерактивно — doInit).
// Пакетная переменная, а не прямой вызов: проба — единственная часть init,
// которая ходит в сеть, а doInit покрыт большим набором юнит-тестов, гоняющих
// его на временных каталогах без всякого PostgreSQL. Тест подменяет её стабом
// (с восстановлением через t.Cleanup) — тот же приём инъекции внешнего эффекта,
// что у resolveOllamaEmbedState с параметром lookPath, только здесь точка
// подмены пакетная: менять сигнатуру doInit(cfg, out) нельзя, на ней завязаны
// все существующие тесты init.
//
// Возвращает не только error (доступность — см. probeStoreConnectionLive), но и
// []string предупреждений о ГОТОВНОСТИ (задача 12.1: pgvector/право CREATE) —
// они не валят пробу (деградация, а не отказ, DESIGN.md §15), поэтому не могут
// быть частью error. wizard.go эти предупреждения игнорирует намеренно (см.
// комментарий у promptPostgresStore) — единственная точка их печати doInit,
// который в интерактивном прогоне вызывается ПОСЛЕ визарда с теми же
// параметрами и печатает их ровно один раз.
var probeStoreConnection = probeStoreConnectionLive

// probeStoreConnectionLive — боевая проба: открывает соединение через
// store.OpenPostgresReadOnly, дёргает Health и, поверх того же соединения,
// диагностику готовности (store.PostgresReadinessChecker).
//
// Почему read-only, а не общая фабрика store.Open: Open накатывает схему памяти
// как побочный эффект открытия (CREATE TABLE/INDEX, триггеры — см. openPostgres),
// то есть `memo init` на этапе «проверим, доступен ли сервер» молча создал бы
// прод-схему в общей командной базе — при опечатке в имени базы/хоста ещё и не
// в той. Проверка доступности обязана оставаться проверкой: OpenPostgresReadOnly
// делает только Ping + current_schema(), ровно как это уже сделано в `memo doctor`
// (doctor.go, postgresDatabaseSection). Схему накатит первый штатный старт memo.
//
// Отсутствие схемы памяти на сервере пробу НЕ валит: Health читает только
// SELECT 1 / server_version / pg_is_in_recovery — «сервер доступен, база
// открывается» и есть тот факт, который init проверяет. Диагностика готовности
// (pgvector, право CREATE) идёт тем же соединением ПОСЛЕ Health и тоже не может
// её завалить — её собственная неудача (напр. нет доступа к pg_extension) тоже
// превращается в предупреждение, а не в ошибку пробы (см. storeReadinessWarnings).
//
// Проба целиком (соединение + Health + диагностика готовности) ограничена
// probeStoreTimeout: висящая установка недопустима ни интерактивно, ни под
// -no-input в CI, поэтому один и тот же ctx прокидывается во все три шага
// (store.OpenPostgresReadOnlyContext — ctx-форма специально ради этого случая).
func probeStoreConnectionLive(cfg config.StoreConfig) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeStoreTimeout)
	defer cancel()

	st, err := store.OpenPostgresReadOnlyContext(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer st.Close()

	if _, err := st.Health(ctx); err != nil {
		return nil, err
	}
	return storeReadinessWarningsFor(ctx, st), nil
}

// storeReadinessWarningsFor делает type-assertion st в
// store.PostgresReadinessChecker и, если удалось, зовёт storeReadinessWarnings
// — общая точка для probeStoreConnectionLive (выше) и postgresDatabaseSection
// (doctor.go), которые раньше независимо повторяли одинаковую проверку и
// одинаковый комментарий про неё. Диагностика готовности умышленно не часть
// интерфейса store.Store (см. её комментарий в postgres_readiness.go) — отсюда
// type-assertion, а не прямой вызов метода: st, не реализующий
// store.PostgresReadinessChecker, просто не диагностируется (nil, без
// ошибки) — тот же принцип деградации, что и у самой диагностики внутри
// storeReadinessWarnings. Store, открытый store.OpenPostgresReadOnly(Context),
// всегда реализует этот интерфейс (это *pgStore); ok==false сюда дойти не
// должно, но обе вызывающие стороны не паникуют на этот случай.
func storeReadinessWarningsFor(ctx context.Context, st store.Store) []string {
	checker, ok := st.(store.PostgresReadinessChecker)
	if !ok {
		return nil
	}
	return storeReadinessWarnings(ctx, checker)
}

// storeReadinessWarnings превращает store.PostgresReadiness (или неудачу самой
// диагностики) в человекочитаемые предупреждения для вывода `memo init`.
// Принимает узкий интерфейс store.PostgresReadinessChecker, а не store.Store
// целиком — держит логику форматирования отделённой от типа-ассерции в
// probeStoreConnectionLive и юнит-тестируемой без реализации всех ~20 методов
// store.Store (см. init_readiness_test.go: fakeReadinessChecker).
func storeReadinessWarnings(ctx context.Context, checker store.PostgresReadinessChecker) []string {
	readiness, err := checker.CheckPostgresReadiness(ctx)
	if err != nil {
		// Таймаут/отмена самого диагностического запроса — отдельная
		// формулировка, а не общее "не удалось проверить": Health (и, для
		// probeStoreConnectionLive, сама проба доступности) к этому моменту
		// уже прошли успешно (это ЕЩЁ один запрос поверх того же соединения),
		// и текст "не удалось проверить готовность БД" уводит пользователя к
		// правам/расширениям — хотя настоящая причина в том, что сервер не
		// успел ответить именно на этот запрос за отведённое время. Без
		// конкретного значения дедлайна в тексте намеренно: у вызывающих
		// сторон он разный (probeStoreTimeout в probeStoreConnectionLive,
		// probeTimeout в doctor.go), а из ctx.Err() узнать, какой именно
		// сработал, нельзя — печатать здесь один из двух наугад означало бы
		// иногда врать про длительность.
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return []string{"сервер не ответил вовремя — диагностика готовности (pgvector/право CREATE) пропущена, сама проба подключения при этом успешна"}
		}
		return []string{fmt.Sprintf("не удалось проверить готовность БД к первому старту memo: %v", err)}
	}
	var warnings []string
	if !readiness.VectorExtension {
		warnings = append(warnings, "тип PostgreSQL 'vector' (pgvector) не резолвится по search_path целевой схемы — memo попробует создать расширение сам при первом ResetVectorIndex (CREATE EXTENSION IF NOT EXISTS vector); если у роли нет прав на это, векторный слой не поднимется и recall деградирует до чистого лексического поиска (DESIGN.md §15)")
	}
	if !readiness.CanCreate {
		warnings = append(warnings, "у роли нет права CREATE в целевой схеме — первый штатный старт memo не сможет накатить схему памяти; попроси администратора выдать право (GRANT CREATE ON SCHEMA ... TO ...) или создать схему заранее")
	}
	return warnings
}

// dotEnvPostgresPassword достаёт MEMO_POSTGRES_PASSWORD из <dir>/.env — того
// самого файла, который `memo init` сам же и написал в прошлый прогон. Нужна
// для повторного init в уже настроенном на postgres проекте (сменить агента,
// перезалить скилы -force, добавить хуки): без неё проба уходила бы на сервер
// без пароля и падала на "password authentication failed", хотя рабочий пароль
// лежит в двух шагах.
//
// Читается ИМЕННО файл проекта (parseDotEnv), а не окружение процесса, и это
// принципиально: переменная окружения шелла источником пароля для init не
// является (см. runInit — секрет попадает в .env только осознанным действием
// пользователя), а <dir>/.env — артефакт самого проекта, который init создал и
// который лежит рядом с генерируемым конфигом. Поэтому и loadDotEnv здесь не
// годится: она по контракту уступает непустому shell-env и вернула бы значение
// из окружения. Нечитаемый/битый .env — не ошибка init: возвращаем пусто и идём
// в пробу как есть (её ошибка будет содержательнее).
func dotEnvPostgresPassword(dir string) string {
	vars, loaded, err := parseDotEnv(filepath.Join(dir, ".env"))
	if err != nil || !loaded {
		return ""
	}
	return vars[memoPostgresPasswordEnv]
}

// resolveConfigRelativePath делает относительный path абсолютным относительно
// configDir — каталога, где лежит agentmemory.toml, ЧЕЙ путь она читает
// (findExistingSqliteMemory, кандидат 1). config.Load сам этого не делает
// (её контракт этой правкой намеренно не трогается — см. границы задачи
// 12.1 доработок): относительный store.path в TOML читается "как есть" и
// резолвится позже, каждым потребителем по своим правилам (store.OpenSQLite
// резолвит его от cwd процесса memo при старте). Для findExistingSqliteMemory
// это не годится: она вызывается из `memo init`, чей cwd — это не обязательно
// каталог проекта (-dir может быть любым, в т.ч. относительным к другому
// cwd) — без этой правки "path = \"./memory.db\"\"" в чужом agentmemory.toml
// резолвился бы от cwd init, а не от каталога, где сам этот TOML лежит, и
// подсказка `memo migrate` либо не находила бы реальный файл, либо (что
// хуже) указывала на случайный посторонний файл с тем же относительным
// именем в текущем каталоге. path гарантированно непуст — единственный вызов
// уже под охраной непустого oldCfg.Store.Path.
//
// "~/..." (и голый "~") раскрывается через expandHomeLocal, а не второй
// копией её же предиката "начинается с ~": пробуем раскрыть path и смотрим,
// получился ли абсолютный путь — раскрытый "~/..." всегда абсолютен, уже
// абсолютный path тоже (expandHomeLocal его не трогает и возвращает как
// есть), а обычный относительный — нет, ему и нужен Join с configDir.
func resolveConfigRelativePath(configDir, path string) string {
	if expanded, err := expandHomeLocal(path); err == nil && filepath.IsAbs(expanded) {
		return expanded
	}
	return filepath.Join(configDir, path)
}

// findExistingSqliteMemory ищет непустой локальный файл SQLite с накопленной
// памятью, перенос которой в PostgreSQL имеет смысл подсказать через `memo
// migrate` (DESIGN.md §14 Этап 12, задача 12.1). Кандидаты, в порядке
// проверки:
//  1. путь, который записал в <dir>/agentmemory.toml предыдущий прогон `memo
//     init` (если проект раньше был настроен на sqlite — самый прямой
//     источник истины про то, каким файлом проект реально пользовался, точнее
//     любого дефолта);
//  2. штатный дефолт `~/.agentmemory/memory.db` (defaultStorePath) — общий
//     файл на все проекты (DESIGN.md §2.1), которым можно было пользоваться и
//     без собственного agentmemory.toml.
//
// Должна вызываться ДО overwriteFile(agentmemory.toml, ...) в doInit: после
// перезаписи старый путь (кандидат 1) уже потерян — новый agentmemory.toml
// содержит postgres, а renderStoreTomlBlock для postgres поле path не пишет.
//
// Возвращает "" (без подсказки) первым значением, если ни один кандидат не
// найден или найденные — пустые/недоступные/битые; не создаёт отсутствующих
// файлов и не накатывает схему ни при каких обстоятельствах (см.
// store.SQLiteHasMemoriesContext). Второе значение — предупреждение, если хотя
// бы одну из проверок кандидатов не удалось довести до конца (nonEmptySQLiteFile
// вернула checkWarning) и при этом ни один более поздний кандидат не нашёлся:
// симметрично тому, как диагностика готовности PostgreSQL в этой же задаче не
// молчит при таймауте, а печатает "диагностика пропущена" — см. её комментарий.
// Если хоть один кандидат найден, предыдущие неудачные проверки не репортятся:
// подсказка про migrate напечатана, беспокоиться не о чем.
func findExistingSqliteMemory(dir string) (path string, checkWarning string) {
	var candidates []string
	// config.Load("<dir>/agentmemory.toml") при отсутствующем или битом
	// файле возвращает ошибку — тогда просто не используем кандидата 1, до
	// второго кандидата это не мешает дойти.
	if oldCfg, err := config.Load(filepath.Join(dir, "agentmemory.toml")); err == nil {
		if (oldCfg.Store.Driver == "" || oldCfg.Store.Driver == storeDriverSQLite) && oldCfg.Store.Path != "" {
			candidates = append(candidates, resolveConfigRelativePath(dir, oldCfg.Store.Path))
		}
	}
	candidates = append(candidates, defaultStorePath)

	seen := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		expanded, err := expandHomeLocal(c)
		if err != nil || seen[expanded] {
			continue
		}
		seen[expanded] = true
		found, warn := nonEmptySQLiteFile(expanded)
		if found {
			return expanded, ""
		}
		if warn != "" && checkWarning == "" {
			checkWarning = warn
		}
	}
	return "", checkWarning
}

// nonEmptySQLiteFile сообщает, есть ли в файле SQLite path хотя бы одна
// запись памяти — точная read-only проверка (store.SQLiteHasMemoriesContext), а
// не эвристика по размеру файла: прежняя версия судила "непустоту" по
// суммарному размеру файла + WAL-журнала относительно калиброванного порога,
// что было хрупко к любой будущей миграции схемы (новая таблица/индекс
// поднимает базовый размер пустой БД и порог начинает ложно срабатывать).
//
// Ограничена sqliteMemoryCountTimeout — тот же инвариант "установка не
// виснет", что у пробы postgres (probeStoreTimeout): findExistingSqliteMemory
// проверяет до двух кандидатов, каждый этой функцией.
//
// Не создаёт отсутствующий файл и не накатывает схему (см. комментарий у
// store.SQLiteHasMemoriesContext — та же логика, что у read-only пробы postgres
// выше). Второй результат — непустая строка, если сама проверка не завершилась
// (файл поврежён, недоступен, таймаут на подвисшем сетевом маунте) — в отличие
// от "файла нет" (типичный случай, не ошибка), это стоит показать пользователю:
// findExistingSqliteMemory пробрасывает её как предупреждение, чтобы не
// молчать о том, что накопленная память могла остаться незамеченной.
func nonEmptySQLiteFile(path string) (found bool, checkWarning string) {
	ctx, cancel := context.WithTimeout(context.Background(), sqliteMemoryCountTimeout)
	defer cancel()
	has, err := store.SQLiteHasMemoriesContext(ctx, path)
	if err != nil {
		return false, fmt.Sprintf("не удалось проверить файл SQLite %s на накопленную память: %v — если там есть данные, подсказка про `memo migrate` может не появиться", path, err)
	}
	return has, ""
}

// validatePostgresPort — единственное место с правилом допустимого порта в
// cmd/memo (config.StoreConfig.Validate его намеренно не проверяет — её дело
// driver и обязательные поля). Через него идут оба пути: интерактивный ввод
// (parsePostgresPortInput в wizard.go) и флаг -postgres-port (doInit).
func validatePostgresPort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("порт %d вне диапазона 1..65535", port)
	}
	return nil
}

// parsePostgresPortInput разбирает введённый в промпте порт: пустая строка —
// дефолт defaultPostgresPort, иначе целое, проверенное validatePostgresPort.
// Один парсер и для huh-валидатора поля, и для чтения результата — иначе в
// коде появлялась бы вторая, недостижимая (а потому непроверяемая) ветка
// разбора.
func parsePostgresPortInput(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return defaultPostgresPort, nil
	}
	port, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("порт должен быть числом")
	}
	if err := validatePostgresPort(port); err != nil {
		return 0, err
	}
	return port, nil
}

// runInit разбирает флаги подкоманды `init` и выполняет скаффолдинг. Вынесена
// из диспетчера в main.go для тестируемости.
func runInit(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fmt.Fprintln(out, "устанавливает память в проект: генерирует MCP-конфиг, скил-инструкцию и agentmemory.toml.")
		fmt.Fprintln(out, "Использование: memo init [флаги]")
		fs.PrintDefaults()
	}
	project := fs.String("project", "", "имя проекта (дефолт — базовое имя каталога -dir)")
	dir := fs.String("dir", ".", "каталог, в который скаффолдить артефакты")
	force := fs.Bool("force", false, "перезаписывать существующие файлы скиллов SKILL.md (agentmemory.toml/mcp-конфиг всегда синхронизируются, .env всегда мержится — -force на них не влияет)")
	agent := fs.String("agent", "", fmt.Sprintf("агентское приложение (%s); дефолт %s", strings.Join(agentKeys(), ", "), defaultAgentKey))
	embedderProvider := fs.String("embedder-provider", "", fmt.Sprintf("провайдер эмбеддинга (%s); дефолт %s", strings.Join(embedderProviderOptions, ", "), defaultEmbedderProvider))
	embedderURL := fs.String("embedder-url", "", "base URL эмбеддера (дефолт зависит от провайдера)")
	embedderModel := fs.String("embedder-model", "", "модель эмбеддинга (дефолт зависит от провайдера)")
	ollamaURL := fs.String("ollama-url", "", "алиас -embedder-url (для embedder-provider=ollama, дефолт "+defaultOllamaBaseURL+")")
	ollamaModel := fs.String("ollama-model", "", "алиас -embedder-model (для embedder-provider=ollama, дефолт "+defaultOllamaModel+")")
	reasonerProvider := fs.String("reasoner-provider", "", fmt.Sprintf("провайдер reasoner (%s); дефолт %s", strings.Join(reasonerProviderOptions, ", "), defaultReasonerProvider))
	reasonerURL := fs.String("reasoner-url", "", "base URL reasoner (для reasoner-provider=cliproxy, дефолт "+defaultCLIProxyBaseURL+")")
	reasonerModel := fs.String("reasoner-model", "", "модель reasoner (для deepseek дефолт "+defaultDeepSeekModel+"; для cliproxy обязательна без интерактивного ввода — список: GET <base URL>/models)")
	storeDriver := fs.String("store-driver", "", fmt.Sprintf("бэкенд хранилища (%s); дефолт %s", strings.Join(storeDriverOptions, ", "), storeDriverSQLite))
	storePath := fs.String("store", "", "путь к файлу SQLite — только при -store-driver="+storeDriverSQLite+" (дефолт "+defaultStorePath+")")
	postgresHost := fs.String("postgres-host", "", "адрес сервера PostgreSQL (обязателен при -store-driver="+storeDriverPostgres+")")
	postgresPort := fs.Int("postgres-port", 0, fmt.Sprintf("порт PostgreSQL (для -store-driver=%s, дефолт %d)", storeDriverPostgres, defaultPostgresPort))
	postgresUser := fs.String("postgres-user", "", "роль PostgreSQL (для -store-driver="+storeDriverPostgres+", дефолт "+defaultPostgresUser+")")
	postgresDBName := fs.String("postgres-dbname", "", "имя базы PostgreSQL (для -store-driver="+storeDriverPostgres+", дефолт "+defaultPostgresDBName+")")
	postgresSSLMode := fs.String("postgres-sslmode", "", "режим TLS PostgreSQL (для -store-driver="+storeDriverPostgres+", дефолт "+defaultPostgresSSLMode+")")
	postgresPassword := fs.String("postgres-password", "", "пароль PostgreSQL — пишется в <dir>/.env как "+memoPostgresPasswordEnv+", в TOML не попадает никогда; пустой пароль допустим (trust-аутентификация). Осторожно: попадёт в историю shell и будет виден в `ps` на время работы команды — надёжнее заранее положить строку "+memoPostgresPasswordEnv+"=... в <dir>/.env")
	cliproxyKey := fs.String("cliproxy-key", "", "ключ CLIProxyAPI (один из api-keys его конфига) — пишется в <dir>/.env как "+cliproxyAPIKeyEnv+", только если reasoner-provider=cliproxy (осторожно: попадёт в историю shell)")
	deepseekKey := fs.String("deepseek-key", "", "ключ DeepSeek API — пишется в <dir>/.env, только если reasoner-provider=deepseek (осторожно: попадёт в историю shell)")
	openaiKey := fs.String("openai-key", "", "ключ OpenAI API (или env OPENAI_API_KEY) — пишется в <dir>/.env, только если embedder-provider=openai")
	hooks := fs.String("hooks", "", fmt.Sprintf("хуки харнесса для ambient-режима (%s); поддерживают Claude Code и dsh (второй — через мост dsh-hooks-claude-code), у opencode хуков нет. Дефолт %s в интерактивном режиме, %s без него", strings.Join(hooksOptions, ", "), hooksSessionStart, hooksNone))
	noInput := fs.Bool("no-input", false, "не спрашивать интерактивно — использовать только флаги и дефолты (без сетевых запросов к провайдерам; при -store-driver="+storeDriverPostgres+" проба подключения к БД выполняется всё равно)")
	_ = fs.Parse(args) // flag.ExitOnError сам завершает процесс при ошибке разбора

	openaiKeyVal := *openaiKey
	if openaiKeyVal == "" {
		openaiKeyVal = os.Getenv(openaiAPIKeyEnv)
	}

	// Пароль postgres, в отличие от -openai-key выше, НЕ подхватывается из
	// одноимённой env-переменной (MEMO_POSTGRES_PASSWORD). Отличие осознанное:
	// записать секрет в файл (<dir>/.env) init должен только по явному действию
	// пользователя — флагом или интерактивным вводом в этом прогоне, — а не
	// потому, что переменная случайно оказалась в окружении: пользователь,
	// запускающий init, при таком подхвате нигде не увидит, что и откуда было
	// сохранено на диск. Повторные прогоны от этого не страдают: пароль для
	// пробы восстанавливается из уже лежащего рядом <dir>/.env
	// (dotEnvPostgresPassword в doInit) — это файл самого проекта, а не
	// окружение.
	cfg := initConfig{
		Project:          *project,
		Dir:              *dir,
		Force:            *force,
		Agent:            *agent,
		EmbedderProvider: *embedderProvider,
		EmbedderBaseURL:  cmp.Or(*embedderURL, *ollamaURL),
		EmbedderModel:    cmp.Or(*embedderModel, *ollamaModel),
		ReasonerProvider: *reasonerProvider,
		ReasonerBaseURL:  *reasonerURL,
		ReasonerModel:    *reasonerModel,
		StoreDriver:      *storeDriver,
		StorePath:        *storePath,
		PostgresHost:     *postgresHost,
		PostgresPort:     *postgresPort,
		PostgresUser:     *postgresUser,
		PostgresDBName:   *postgresDBName,
		PostgresSSLMode:  *postgresSSLMode,
		PostgresPassword: *postgresPassword,
		DeepseekKey:      *deepseekKey,
		CLIProxyKey:      *cliproxyKey,
		OpenAIKey:        openaiKeyVal,
		Hooks:            *hooks,
	}

	// Драйвер хранилища проверяется ДО интерактивного промпта (в отличие от
	// провайдеров, чью валидацию делает doInit в самом конце): от него зависит,
	// какие вопросы задаст мастер, поэтому опечатка вроде -store-driver=postgre
	// иначе молча увела бы пользователя в sqlite-ветку — он ответил бы на все
	// вопросы и только потом получил ошибку.
	if err := validateStoreDriverKey(cfg.StoreDriver); err != nil {
		return err
	}

	// -store без -store-driver означает sqlite: до появления второго бэкенда
	// этот флаг был единственным способом сказать «про хранилище не спрашивай»,
	// и скрипты/мануалы, написанные под него, не должны вдруг упираться в новый
	// вопрос мастера.
	if cfg.StoreDriver == "" && cfg.StorePath != "" {
		cfg.StoreDriver = storeDriverSQLite
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
// смысл только в этом случае, иначе `memo init` должен остаться
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
	switch cfg.ReasonerProvider {
	case "deepseek":
		// base_url DeepSeek пишется в TOML, только если задан явно (-reasoner-url):
		// иначе рантайм берёт дефолт реестра, а дефолтный конфиг остаётся
		// побайтово прежним.
		if cfg.ReasonerModel == "" {
			cfg.ReasonerModel = defaultDeepSeekModel
		}
	case "cliproxy":
		// Дефолтной модели нет: набор зависит от аккаунтов, подключённых к
		// прокси, — пустую модель отвергает doInit.
		if cfg.ReasonerBaseURL == "" {
			cfg.ReasonerBaseURL = defaultCLIProxyBaseURL
		}
	default: // "noop" — base_url/model reasoner'у не нужны
		cfg.ReasonerBaseURL = ""
		cfg.ReasonerModel = ""
	}
	clearUnselectedProviderKeys(&cfg, embedderProviders, cfg.EmbedderProvider)
	clearUnselectedProviderKeys(&cfg, reasonerProviders, cfg.ReasonerProvider)
	// Дефолты хранилища и обнуление полей НЕвыбранного бэкенда — та же защита
	// от рассинхрона флагов, что clearUnselectedProviderKeys даёт ключам
	// провайдеров: -postgres-* при sqlite (и наоборот -store при postgres) не
	// должны ни попасть в TOML, ни утечь паролем в .env. StoreDriver == "" не
	// нормализуется в "sqlite" намеренно — от этого зависит побайтовая
	// неизменность дефолтного конфига (см. renderStoreTomlBlock).
	switch cfg.StoreDriver {
	case storeDriverPostgres:
		cfg.StorePath = ""
		if cfg.PostgresPort == 0 {
			cfg.PostgresPort = defaultPostgresPort
		}
		if cfg.PostgresUser == "" {
			cfg.PostgresUser = defaultPostgresUser
		}
		if cfg.PostgresDBName == "" {
			cfg.PostgresDBName = defaultPostgresDBName
		}
		if cfg.PostgresSSLMode == "" {
			cfg.PostgresSSLMode = defaultPostgresSSLMode
		}
	default: // "" | "sqlite" | неизвестный драйвер (его отклонит Validate в doInit)
		if cfg.StorePath == "" {
			cfg.StorePath = defaultStorePath
		}
		cfg.PostgresHost = ""
		cfg.PostgresPort = 0
		cfg.PostgresUser = ""
		cfg.PostgresDBName = ""
		cfg.PostgresSSLMode = ""
		cfg.PostgresPassword = ""
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
	if cfg.ReasonerProvider == "cliproxy" && cfg.ReasonerModel == "" {
		return fmt.Errorf("для reasoner-provider=cliproxy задай -reasoner-model — список моделей прокси: GET %s/models", cfg.ReasonerBaseURL)
	}

	// Валидация хранилища — ПОСЛЕ applyInitDefaults, в отличие от провайдеров
	// выше: правила берутся у config.StoreConfig.Validate (та же проверка, что
	// отклонит невалидный конфиг при старте memo — своей копии правил здесь
	// нет), а ей нужно уже разрешённое значение port/sslmode, иначе она
	// проверяла бы не то, что попадёт в TOML. Дефолт скрыть опечатку не может:
	// неизвестный driver Validate отклоняет при любых дефолтах, а host/user/
	// dbname обязательны и дефолтом хоста не подменяются (см. defaultPostgres*).
	storeCfg := storeConfigFromInit(cfg)
	if err := storeCfg.Validate(); err != nil {
		return fmt.Errorf("настройки хранилища: %w", err)
	}
	if cfg.StoreDriver == storeDriverPostgres {
		// Порт из флага -postgres-port проверяется тем же правилом, что и порт
		// из интерактивного промпта (validatePostgresPort) — иначе -postgres-port=-5
		// молча уехал бы в TOML.
		if err := validatePostgresPort(cfg.PostgresPort); err != nil {
			return fmt.Errorf("настройки хранилища: %w", err)
		}
	}

	target, ok := agentRegistry[cfg.Agent]
	if !ok {
		return fmt.Errorf("неизвестное агентское приложение %q — поддерживаются: %s", cfg.Agent, strings.Join(agentKeys(), ", "))
	}

	project, err := resolveProjectName(cfg.Project, cfg.Dir)
	if err != nil {
		return err
	}

	// Проба доступности PostgreSQL — после всех дешёвых локальных проверок
	// (валидация конфигурации, агент, имя проекта: нет смысла ходить в сеть,
	// если прогон и так обречён), но ДО записи любых файлов и даже до создания
	// каталога: молча сгенерировать конфиг, с которым memo заведомо не
	// стартует, хуже, чем упасть здесь с внятной причиной, а неудачный init не
	// должен оставлять после себя следов.
	//
	// В интерактивном прогоне эту же пробу уже сделал визард (wizard.go,
	// promptPostgresStore) и дал меню повторить ввод — повтор здесь (второе
	// read-only соединение) осознан: doInit обязан оставаться самодостаточным,
	// он же единственная точка входа для -no-input, прогонов без TTY (CI) и
	// всех тестов.
	if cfg.StoreDriver == storeDriverPostgres {
		if storeCfg.Password == "" {
			// Повторный init в уже настроенном проекте: пароль лежит в
			// <dir>/.env с прошлого прогона — берём его ТОЛЬКО для пробы, в
			// cfg (и, значит, в перезапись .env ниже) он не возвращается.
			storeCfg.Password = dotEnvPostgresPassword(cfg.Dir)
		}
		warnings, err := probeStoreConnection(storeCfg)
		if err != nil {
			return fmt.Errorf("хранилище %s недоступно: %w (проверь адрес, что база и роль созданы и что пароль передан -postgres-password или лежит в %s/.env)", storeCfg.Target(), err, cfg.Dir)
		}
		// Единственная точка печати предупреждений о ГОТОВНОСТИ (в отличие от
		// ошибки доступности выше, они не мешают init продолжить, DESIGN.md
		// §15) — интерактивный визард (wizard.go promptPostgresStore) делает
		// свою пробу молча по этой части и печатает их не сам: он вызывается
		// ДО doInit с теми же параметрами, doInit пробует снова (см.
		// комментарий у него самого) и печатает warnings ровно один раз,
		// независимо от того, интерактивный это прогон или -no-input.
		for _, w := range warnings {
			fmt.Fprintf(out, "warning: %s\n", w)
		}
	}

	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return fmt.Errorf("создание каталога %q: %w", cfg.Dir, err)
	}

	command := "memo" // фолбэк на PATH, если os.Executable() недоступен
	if exe, err := os.Executable(); err == nil {
		command = exe
	} else {
		fmt.Fprintf(out, "не удалось определить путь к memo (%v) — в MCP-конфиг записана команда \"memo\", проверь, что она доступна в PATH\n", err)
	}

	tomlContent := fmt.Sprintf(configTomlTemplate,
		escapeTOMLString(project), renderStoreTomlBlock(cfg), renderEmbedderTomlBlock(cfg), renderReasonerTomlBlock(cfg))

	// Подсказка про `memo migrate` (задача 12.1) ищет локальный SQLite СЕЙЧАС,
	// до overwriteFile(agentmemory.toml, ...) ниже: тот перезапишет секцию
	// [store] на postgres, и путь, которым проект пользовался раньше (если
	// был настроен на sqlite), станет недоступен для findExistingSqliteMemory.
	// Сам перенос init не делает (задача прямо это оговаривает) — только
	// печатает готовую команду в конце (см. ниже).
	var sqliteMigrationHint string
	if cfg.StoreDriver == storeDriverPostgres {
		var warn string
		sqliteMigrationHint, warn = findExistingSqliteMemory(cfg.Dir)
		if warn != "" {
			fmt.Fprintf(out, "warning: %s\n", warn)
		}
	}

	// Запись сервера memo в конфиг харнесса: у Claude Code и opencode это мерж
	// в существующий файл (.mcp.json/opencode.json — чужие серверы и прочие
	// top-level ключи сохраняются), у dsh — генерация проектного оверлея
	// Cordis целиком, вместе со строкой моста хуков. Всё, что различается по
	// харнессам, закрыто agentTarget.scaffold; -force на запись сервера не
	// влияет нигде (источник истины — параметры текущего прогона init).
	harnessHint, err := target.scaffold(cfg.Dir, project, command, cfg, out)
	if err != nil {
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

	// agentmemory.toml — единственный артефакт, который всё ещё перезаписывается
	// целиком (overwriteFile): весь его источник истины — параметры текущего
	// прогона init, -force на него не влияет.
	if err := overwriteFile(filepath.Join(cfg.Dir, "agentmemory.toml"), tomlContent, out); err != nil {
		return err
	}

	// .env — только ключи выбранных провайдеров (DESIGN.md §13, задача 11.1):
	// applyInitDefaults уже обнулил ключи невыбранных провайдеров выше, здесь
	// достаточно проверить, что оставшееся непусто. selectedProviders — тот же
	// реестр, из которого buildMCPServerEntry/buildOpencodeServerEntry берут
	// env-плейсхолдеры. mergeEnvFile дописывает/обновляет только эти ключи в
	// существующем .env, не трогая остальные строки — независимо от cfg.Force
	// (в отличие от agentmemory.toml выше, .env никогда не перезаписывается
	// целиком: секреты, введённые в прошлых прогонах init, не должны теряться).
	envVars := map[string]string{}
	for _, p := range selectedProviders(cfg) {
		if f := p.keyField(&cfg); f != nil && *f != "" {
			envVars[p.envKey] = *f
		}
	}
	// Пароль PostgreSQL идёт тем же путём, что ключи провайдеров (DESIGN.md §13:
	// секреты только в env/.env, в TOML — никогда), но не через реестр
	// selectedProviders: у бэкенда хранилища нет провайдерской пары
	// "провайдер -> поле с ключом", секрет ровно один и привязан к driver.
	// Пустой пароль — не ошибка (trust-аутентификация, см. StoreConfig.Validate):
	// просто ничего не дописываем, .env из-за postgres не создаётся.
	if cfg.StoreDriver == storeDriverPostgres && cfg.PostgresPassword != "" {
		envVars[memoPostgresPasswordEnv] = cfg.PostgresPassword
	}
	if len(envVars) > 0 {
		if err := mergeEnvFile(filepath.Join(cfg.Dir, ".env"), envVars, out); err != nil {
			return err
		}
	}
	// Строка .env в .gitignore нужна не только когда секрет записан прямо
	// сейчас: при driver=postgres пустой пароль — штатный сценарий (промпт сам
	// предлагает «указать позже в .env», trust-аутентификация тоже возможна), и
	// пользователь создаст этот файл руками — он не должен оказаться в
	// репозитории незаигнориренным.
	if len(envVars) > 0 || cfg.StoreDriver == storeDriverPostgres {
		if err := ensureGitignoreEntry(filepath.Join(cfg.Dir, ".gitignore"), ".env", out); err != nil {
			return err
		}
	}

	fmt.Fprintf(out, "проект: %s\n", project)
	fmt.Fprintf(out, "агент: %s\n", target.label)
	fmt.Fprintf(out, "эмбеддинг: %s\n", cfg.EmbedderProvider)
	fmt.Fprintf(out, "reasoner: %s\n", cfg.ReasonerProvider)
	// Хранилище печатается через StoreConfig.Target() — один формат с логом
	// старта memo и секцией «БД» в `memo doctor`: путь к файлу для sqlite,
	// postgres://host:port/dbname для postgres (без пароля).
	fmt.Fprintf(out, "хранилище: %s\n", storeCfg.Target())
	if sqliteMigrationHint != "" {
		// formatMigrateEndpoint (migrate.go) — обратная к её же parseMigrateEndpoint:
		// один источник синтаксиса "sqlite:<путь>"/"postgres://user@host:port/dbname?..."
		// для парсера и для этой подсказки, вместо третьего независимого способа
		// собрать ту же строку. Пароль в напечатанную команду не попадает никогда
		// (DESIGN.md §13, то же правило, что у agentmemory.toml) — formatMigrateEndpoint
		// обнуляет его сама, даже если в storeCfg к этому моменту лежит пароль для
		// пробы выше (в т.ч. восстановленный из <dir>/.env).
		srcCfg := config.StoreConfig{Driver: storeDriverSQLite, Path: sqliteMigrationHint}
		fmt.Fprintf(out, "обнаружена локальная память %s — перенос в общую базу init не делает сам, команда для переноса:\n", sqliteMigrationHint)
		// shellQuoteSingle, а не %q: %q — Go-синтаксис двойных кавычек, он не
		// экранирует $, обратные кавычки и ! — путь вроде "/home/u/$PROJ/memory.db",
		// напечатанный так и скопированный в sh/bash/zsh, подставил бы значение
		// переменной $PROJ вместо буквального символа (см. её комментарий).
		fmt.Fprintf(out, "  memo migrate %s %s\n", shellQuoteSingle(formatMigrateEndpoint(srcCfg)), shellQuoteSingle(formatMigrateEndpoint(storeCfg)))
		// Пароль команда migrate возьмёт сама — из окружения (MEMO_POSTGRES_PASSWORD)
		// или из .env, куда его положил этот же прогон init (или предыдущий,
		// см. dotEnvPostgresPassword) — та же логика, что подхватывает пароль у
		// остальных подкоманд memo (loadDotEnvForConfig, задача 12.1 доработок).
		//
		// Про «запусти из этого каталога» сказано не для красоты: у migrate нет
		// -config, поэтому loadDotEnvForConfig ищет .env в ТЕКУЩЕМ каталоге
		// процесса (см. runMigrate), а не рядом с конфигом проекта. Скопированная
		// в другой каталог команда пароль из <dir>/.env не увидит и упадёт на
		// аутентификации — проверено живым прогоном.
		fmt.Fprintf(out, "  (запусти её из %s — пароль команда возьмёт сама из переменной окружения %s или из лежащего там .env)\n", cfg.Dir, memoPostgresPasswordEnv)
	}
	// Финальная подсказка — общая для всех харнессов, за ней — специфичное для
	// конкретного харнесса (одобрение сервера у Claude Code, ручное подключение
	// оверлея у dsh), которое вернул его scaffold.
	fmt.Fprintln(out, "дальше: убедись, что memo доступен — `go install` кладёт его на PATH (см. `make install`), и перезапусти агентское приложение в этом каталоге.")
	fmt.Fprint(out, harnessHint)
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

// shellQuoteSingle оборачивает s в одинарные кавычки — единственная
// переносимая (POSIX sh/bash/zsh) форма, внутри которой ни один спецсимвол
// шелла ($, обратная кавычка, !, обратный слэш, двойная кавычка и т.д.) не
// интерполируется вовсе. Единственный символ, который сам по себе ломает
// такую строку — одинарная кавычка; стандартный приём её экранирования:
// закрыть строку, вставить экранированную кавычку снаружи неё, открыть строку
// заново — код ниже отдельным блоком, чтобы форматтер doc-комментариев не
// принял пример за обычную прозу и не переформатировал кавычки в нём:
//
//	вход:  foo'bar
//	выход: 'foo'\''bar'
//
// Используется для печати аргументов подсказки `memo migrate` (doInit) — путь
// к файлу или DSN пользователь копирует прямо в шелл, и он может содержать что
// угодно, включая $ в имени переменной окружения ($HOME) или в самом пути.
// Go-кавычки (%q) для этого не годятся: это синтаксис Go/C-строк, шелл видит в
// них обычные двойные кавычки — $, обратная кавычка и ! внутри них по-прежнему
// интерполируются шеллом (в bash/zsh ещё и history expansion на "!").
func shellQuoteSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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

// buildMCPServerEntry собирает JSON-запись сервера memo для .mcp.json (форма
// mcpServerSpec, stdio) — вставляется mergeMCPConfig в существующий или новый
// файл под ключом "memo" секции mcpServers. env содержит ТОЛЬКО плейсхолдеры
// выбранных провайдеров (shell-синтаксис "${VAR:-}").
func buildMCPServerEntry(command string, cfg initConfig) (json.RawMessage, error) {
	env := buildProviderEnv(cfg, func(envKey string) string { return "${" + envKey + ":-}" })
	spec := mcpServerSpec{
		Command: command,
		Args:    mcpServerArgs,
		Env:     env,
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("сборка записи сервера memo для .mcp.json: %w", err)
	}
	return b, nil
}

// opencodeServerSpec — форма записи сервера в секции "mcp" opencode.json для
// local MCP-сервера (opencode-документация). В отличие от mcpServerSpec у
// Claude Code:
//   - command — единый массив [команда, аргументы...], а не раздельные
//     command+args;
//   - интерполяция env — синтаксис "{env:VAR}", а не shell-style "${VAR:-}";
//   - поле окружения называется environment, а не env.
type opencodeServerSpec struct {
	Type        string            `json:"type"`
	Command     []string          `json:"command"`
	Enabled     bool              `json:"enabled"`
	Environment map[string]string `json:"environment"`
}

// buildOpencodeServerEntry собирает JSON-запись сервера memo для opencode.json
// (local MCP-сервер) — вставляется mergeMCPConfig под ключом "memo" секции
// mcp. environment — те же правила выбора ключей, что у buildMCPServerEntry.
// Путь к agentmemory.toml — относительный: opencode запускает local
// MCP-процесс с рабочим каталогом = корень workspace (в его исходниках cwd
// незаданного mcp.cwd резолвится в baseDir — корень инстанса), откуда и
// подхватывается "agentmemory.toml". Абсолютный указатель уровня
// ${CLAUDE_PROJECT_DIR} у opencode не нужен — своего аналога он не задаёт.
func buildOpencodeServerEntry(command string, cfg initConfig) (json.RawMessage, error) {
	env := buildProviderEnv(cfg, func(envKey string) string { return "{env:" + envKey + "}" })
	spec := opencodeServerSpec{
		Type:        "local",
		Command:     []string{command, "-config", "agentmemory.toml"},
		Enabled:     true,
		Environment: env,
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("сборка записи сервера memo для opencode.json: %w", err)
	}
	return b, nil
}

// ensureOpencodeSchema — mcpConfigMerge.ensureTopLevel для opencode.json:
// проставляет top-level "$schema", если в файле его ещё нет (новый файл, либо
// существующий, где его почему-то не было) — не трогает уже присутствующее
// значение, даже если оно отличается от дефолтного.
func ensureOpencodeSchema(raw map[string]json.RawMessage) error {
	if _, ok := raw["$schema"]; ok {
		return nil
	}
	b, err := json.Marshal("https://opencode.ai/config.json")
	if err != nil {
		return fmt.Errorf("сборка $schema для opencode.json: %w", err)
	}
	raw["$schema"] = b
	return nil
}

// mcpConfigMerge параметризует mergeMCPConfig под конкретный харнесс:
// sectionKey — ключ верхнего уровня, под которым лежат записи серверов
// ("mcpServers" у .mcp.json, "mcp" у opencode.json), buildEntry — сборка
// JSON-записи сервера memo под параметры текущего прогона init,
// ensureTopLevel — точечная правка прочих top-level ключей (nil, если таких
// нет, как у .mcp.json; у opencode.json — проставляет "$schema").
type mcpConfigMerge struct {
	sectionKey     string
	buildEntry     func(command string, cfg initConfig) (json.RawMessage, error)
	ensureTopLevel func(raw map[string]json.RawMessage) error
}

// scaffoldClaude раскладывает артефакты Claude Code: MCP-запись сервера memo в
// <dir>/.mcp.json (mergeMCPConfig — чужие серверы и top-level ключи файла
// сохраняются) и, если в этом прогоне выбраны хуки, команды `memo hook` в
// <dir>/.claude/settings.json (setupClaudeHooks).
func scaffoldClaude(dir, _ string, command string, cfg initConfig, out io.Writer) (string, error) {
	if err := mergeMCPConfig(filepath.Join(dir, ".mcp.json"), command, cfg, mcpConfigMerge{
		sectionKey: "mcpServers",
		buildEntry: buildMCPServerEntry,
	}, out); err != nil {
		return "", err
	}
	if mappings, ok := hookEventsByChoice[cfg.Hooks]; ok {
		if err := setupClaudeHooks(dir, command, mappings, out); err != nil {
			return "", err
		}
	}
	// Одобрение проектного MCP-сервера — специфика именно Claude Code (у
	// opencode сервер включается полем enabled, у dsh одобрения нет вовсе),
	// поэтому подсказка живёт здесь, а не в общем финальном выводе doInit.
	return "при первом запуске Claude Code одобри проектный MCP-сервер `memo`\n", nil
}

// scaffoldOpencode раскладывает артефакты opencode: MCP-запись сервера memo в
// <dir>/opencode.json (mergeMCPConfig, плюс ensureOpencodeSchema для
// top-level "$schema"). Хуков у opencode нет, поэтому ambient-режим здесь
// недоступен в принципе — scaffold честно сообщает об этом в выводе, а не
// молчит (задача 7.2).
func scaffoldOpencode(dir, _ string, command string, cfg initConfig, out io.Writer) (string, error) {
	if err := mergeMCPConfig(filepath.Join(dir, "opencode.json"), command, cfg, mcpConfigMerge{
		sectionKey:     "mcp",
		buildEntry:     buildOpencodeServerEntry,
		ensureTopLevel: ensureOpencodeSchema,
	}, out); err != nil {
		return "", err
	}
	fmt.Fprintf(out, "ambient-режим (авто-recall через хуки) у opencode недоступен — хуков, как у Claude Code, у него нет\n")
	return "", nil
}

// mergeMCPConfig мержит запись сервера "memo" в MCP-конфиг по path
// (.mcp.json/opencode.json), не теряя ни другие серверы в секции
// m.sectionKey, ни прочие top-level ключи файла — по образцу mergeClaudeHooks:
// существующий файл парсится в map[string]json.RawMessage, точечно
// правится только своя секция и (если задано) m.ensureTopLevel, остальное
// проходит байт-в-байт. Запись "memo" внутри секции безусловно
// (пере)генерируется из command/cfg — независимо от того, что там было
// раньше и от -force (та же семантика, что была у полной перезаписи файла).
// Файл отсутствует/пуст — эквивалентно мержу в пустой объект, т.е. создаётся
// с нуля. Битый существующий JSON — ошибка, файл не создаётся и не
// перезаписывается.
func mergeMCPConfig(path, command string, cfg initConfig, m mcpConfigMerge, out io.Writer) error {
	raw := map[string]json.RawMessage{}
	existing, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(existing, &raw); err != nil {
			return fmt.Errorf("разбор %q: %w", path, err)
		}
	case os.IsNotExist(err):
		// файла нет — начинаем с пустого объекта верхнего уровня
	default:
		return fmt.Errorf("чтение %q: %w", path, err)
	}

	section := map[string]json.RawMessage{}
	if rawSection, ok := raw[m.sectionKey]; ok {
		if err := json.Unmarshal(rawSection, &section); err != nil {
			return fmt.Errorf("разбор секции %q в %q: %w", m.sectionKey, path, err)
		}
	}

	entry, err := m.buildEntry(command, cfg)
	if err != nil {
		return err
	}
	section["memo"] = entry

	sectionRaw, err := json.Marshal(section)
	if err != nil {
		return fmt.Errorf("сборка секции %q: %w", m.sectionKey, err)
	}
	raw[m.sectionKey] = sectionRaw

	if m.ensureTopLevel != nil {
		if err := m.ensureTopLevel(raw); err != nil {
			return err
		}
	}

	b, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("сборка %q: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("создание каталога %q: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("запись %q: %w", path, err)
	}
	fmt.Fprintf(out, "записано: %s\n", path)
	return nil
}

// writeScaffoldFile пишет content в path с правами 0o644. Если файл уже
// существует и force не задан — пропускает его с предупреждением в out (не
// ошибка): повторный `memo init` идемпотентен и безопасен. Используется для
// артефактов, где потеря ручных правок при повторном init нежелательна
// (скиллы SKILL.md) — в отличие от agentmemory.toml (см. overwriteFile),
// mcp-конфига (см. mergeMCPConfig) и .env (см. mergeEnvFile), которые
// сознательно ведут себя иначе.
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

// overwriteFile пишет content в path с правами 0o644, безусловно перезаписывая
// существующий файл. Используется для agentmemory.toml: его единственный
// источник истины — параметры текущего прогона `memo init`, поэтому -force на
// него не влияет и повторный init всегда синхронизирует его с введёнными
// параметрами — осознанный tradeoff (ручные правки этого файла не переживают
// повторный init), в отличие от writeScaffoldFile (скиллы), mergeEnvFile
// (.env) и mergeMCPConfig (.mcp.json/opencode.json) — те мержат, а не
// перезаписывают целиком.
func overwriteFile(path, content string, out io.Writer) error {
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("запись %q: %w", path, err)
	}
	fmt.Fprintf(out, "записано: %s\n", path)
	return nil
}

// mergeEnvFile делает upsert vars (KEY=VALUE) в существующий .env по path, не
// теряя остальные строки (чужие переменные, комментарии, пустые строки):
// если ключ из vars уже есть в файле — его значение обновляется на месте
// (без дублирования строки), иначе строка KEY=VALUE дописывается в конец
// (новые ключи — в отсортированном порядке, для детерминированного вывода).
// Мержится ТОЛЬКО набор ключей из vars — vars собирает doInit из выбранных в
// этом прогоне провайдеров (DESIGN.md §13, задача 11.1); переменные с другими
// именами (напр. OPENAI_API_KEY, оставшийся от прошлого прогона с
// embedder=openai, при текущем reasoner=deepseek) не трогаются. В отличие от
// writeScaffoldFile/overwriteFile мержится безусловно, независимо от -force:
// ключ, введённый повторным `memo init` в уже существующий .env, обязан туда
// долететь, а не молча потеряться. Разбор существующих строк — по формату
// loadDotEnv (dotenv.go): `KEY=VALUE`, `#`-комментарии, пустые строки — но
// здесь, в отличие от loadDotEnv, все нераспознанные/незатронутые строки
// сохраняются как есть байт-в-байт. Права результата всегда 0o600 (секрет) —
// проставляются явно поверх os.WriteFile, т.к. WriteFile не меняет права уже
// существующего файла.
func mergeEnvFile(path string, vars map[string]string, out io.Writer) error {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("чтение %q: %w", path, err)
	}

	content := mergeEnvContent(existing, vars)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("запись %q: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("установка прав 0600 %q: %w", path, err)
	}

	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintf(out, "записано: %s (ключи: %s, права 0600)\n", path, strings.Join(keys, ", "))
	return nil
}

// mergeEnvContent — содержимое .env existing с ключами vars: строка с тем же
// ключом заменяется на месте, новые ключи дописываются в конец по алфавиту,
// прочие строки (в т.ч. комментарии) не трогаются.
func mergeEnvContent(existing []byte, vars map[string]string) string {
	remaining := make(map[string]string, len(vars))
	for k, v := range vars {
		remaining[k] = v
	}

	var lines []string
	if len(existing) > 0 {
		lines = strings.Split(strings.TrimRight(string(existing), "\n"), "\n")
	}
	merged := make([]string, 0, len(lines)+len(vars))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if key, _, ok := strings.Cut(trimmed, "="); ok && trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			key = strings.TrimSpace(key)
			if newVal, ok := remaining[key]; ok {
				merged = append(merged, key+"="+newVal)
				delete(remaining, key)
				continue
			}
		}
		merged = append(merged, line)
	}

	newKeys := make([]string, 0, len(remaining))
	for k := range remaining {
		newKeys = append(newKeys, k)
	}
	sort.Strings(newKeys)
	for _, k := range newKeys {
		merged = append(merged, k+"="+remaining[k])
	}

	content := strings.Join(merged, "\n")
	if content != "" {
		content += "\n"
	}
	return content
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
// абсолютный путь к бинарю memo (memoPath — тот же, что уже разрешён в
// doInit для .mcp.json, см. os.Executable()/фолбэк на "memo" из PATH) +
// подкоманда `hook` с нужным -event и -config. memoPath обёрнут в кавычки
// (%q) — путь может содержать пробелы.
func buildHookCommand(memoPath, event string) string {
	return fmt.Sprintf("%q hook -event %s -config %s", memoPath, event, hookConfigAnchor)
}

// setupClaudeHooks мержит в <dir>/.claude/settings.json команды `memo hook`
// для событий из mappings (DESIGN.md §12, задача 7.2) и печатает в out, что
// именно обновлено.
func setupClaudeHooks(dir, memoPath string, mappings []hookEventMapping, out io.Writer) error {
	eventCommands := make(map[string]string, len(mappings))
	claudeEvents := make([]string, 0, len(mappings))
	for _, m := range mappings {
		eventCommands[m.ClaudeEvent] = buildHookCommand(memoPath, m.MemoEvent)
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
