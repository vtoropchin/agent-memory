// Подкоманда `memoryd init` — интерактивный установщик памяти в проект под
// конкретное агентское приложение (DESIGN.md §14 Этап 5, задачи 5.3/5.4).
// Раскладывает в целевой каталог MCP-конфиг, скил-инструкцию «Протокол работы
// с памятью», agentmemory.toml (с введёнными ollama/store/project) и, если
// введён ключ DeepSeek, .env (+ строку .env в .gitignore). Не трогает загрузку
// конфига (internal/config) и серверный путь.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// Дефолты полей initConfig, не заданных ни флагом, ни интерактивным вводом —
// те же значения, что раньше были захардкожены в configTomlTemplate.
const (
	defaultAgentKey      = "claude"
	defaultOllamaBaseURL = "http://localhost:11434"
	defaultOllamaModel   = "bge-m3"
	defaultStorePath     = "~/.agentmemory/memory.db"
)

// configTomlTemplate — шаблон agentmemory.toml по образцу корневого
// agentmemory.toml/config.example.toml (DESIGN.md §13). Константа-строка, а не
// toml.Marshal — так сохраняются комментарии. %[1]s — экранированное имя
// проекта, %[2]s — путь к файлу БД, %[3]s — base URL ollama, %[4]s — модель
// эмбеддингов ollama.
const configTomlTemplate = `# Конфиг AgentMemory для проекта %[1]s (DESIGN.md §13).
#
# Агентское приложение поднимает MCP-сервер agentmemory из сгенерированного
# рядом MCP-конфига и передаёт ему этот файл флагом -config.

[store]
# Общий файл БД на все проекты (DESIGN.md §2.1: один файл, изоляция через scope).
# Каталог создаётся автоматически при первом старте (store.Open).
path = "%[2]s"

[embedder]
# Локальный ollama (dim определяется автоматически пробным эмбеддингом).
# Если ollama недоступен — memoryd стартует, recall падает на чистый FTS5 (§15).
provider = "ollama"
base_url = "%[3]s"
model    = "%[4]s"

[reasoner]
# DeepSeek chat: ключ берётся из env DEEPSEEK_API_KEY. Если в shell-env его нет,
# memoryd сам подхватывает его из .env рядом с этим конфигом при старте (см.
# loadDotEnv в main.go) — .env раскладывается этим же "memoryd init". Пустой
# ключ — деградация до noop (§15): recall без суммаризации, консолидация
# недоступна, но память работает.
provider = "deepseek"
model    = "deepseek-chat"

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

// agentTarget описывает одно агентское приложение и то, как оно раскладывает
// артефакты AgentMemory в проекте: MCP-конфиг и скил-инструкцию. Сейчас в
// agentRegistry только claude (Claude Code), но структура рассчитана на то,
// чтобы добавить opencode/codex одной записью — их форматы MCP-конфига и
// расположение скилов пока не реализованы.
type agentTarget struct {
	label string // человекочитаемое имя для промптов/вывода

	mcpConfigPath   func(dir string) string
	renderMCPConfig func(command string) (string, error)

	skillPath   func(dir string) string
	renderSkill func(project string) string
}

// agentRegistry — реестр поддерживаемых агентских приложений, ключ — значение
// флага -agent.
var agentRegistry = map[string]agentTarget{
	"claude": {
		label:           "Claude Code",
		mcpConfigPath:   func(dir string) string { return filepath.Join(dir, ".mcp.json") },
		renderMCPConfig: renderMCPJSON,
		skillPath: func(dir string) string {
			return filepath.Join(dir, ".claude", "skills", "memoryd", "SKILL.md")
		},
		renderSkill: renderClaudeSkill,
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
// интерактивном TTY-режиме) дозапрошены promptInit, либо остаются пустыми и
// получают дефолт в doInit/applyInitDefaults.
type initConfig struct {
	Project       string
	Dir           string
	Agent         string
	OllamaBaseURL string
	OllamaModel   string
	StorePath     string
	DeepseekKey   string
	Force         bool
}

// runInit разбирает флаги подкоманды `init` и выполняет скаффолдинг. Вынесена
// из диспетчера в main.go для тестируемости.
func runInit(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	project := fs.String("project", "", "имя проекта (дефолт — базовое имя каталога -dir)")
	dir := fs.String("dir", ".", "каталог, в который скаффолдить артефакты")
	force := fs.Bool("force", false, "перезаписывать существующие файлы")
	agent := fs.String("agent", "", fmt.Sprintf("агентское приложение (%s); дефолт %s", strings.Join(agentKeys(), ", "), defaultAgentKey))
	ollamaURL := fs.String("ollama-url", "", "base URL ollama-сервера (дефолт "+defaultOllamaBaseURL+")")
	ollamaModel := fs.String("ollama-model", "", "модель эмбеддингов ollama (дефолт "+defaultOllamaModel+")")
	store := fs.String("store", "", "путь к файлу SQLite (дефолт "+defaultStorePath+")")
	deepseekKey := fs.String("deepseek-key", "", "ключ DeepSeek API — пишется в <dir>/.env (осторожно: попадёт в историю shell)")
	noInput := fs.Bool("no-input", false, "не спрашивать интерактивно — использовать только флаги и дефолты")
	_ = fs.Parse(args) // flag.ExitOnError сам завершает процесс при ошибке разбора

	cfg := initConfig{
		Project:       *project,
		Dir:           *dir,
		Force:         *force,
		Agent:         *agent,
		OllamaBaseURL: *ollamaURL,
		OllamaModel:   *ollamaModel,
		StorePath:     *store,
		DeepseekKey:   *deepseekKey,
	}

	if !*noInput && isInteractiveStdin() {
		cfg = promptInit(os.Stdin, out, cfg)
	}
	// Дефолты незаполненных полей проставляет doInit (applyInitDefaults) —
	// он же единая точка входа для тестов, поэтому здесь второй раз не нужно.
	return doInit(cfg, out)
}

// isInteractiveStdin проверяет, что stdin — терминал (а не пайп/файл/
// перенаправление): интерактивные промпты имеют смысл только в этом случае,
// иначе `memoryd init` должен остаться скриптуемым и не блокироваться на чтении.
func isInteractiveStdin() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// applyInitDefaults заполняет дефолтами поля, оставшиеся пустыми после флагов
// и (опционального) интерактивного промпта. Project намеренно не трогается —
// его резолвит resolveProjectName из dir, если явное имя не задано.
func applyInitDefaults(cfg initConfig) initConfig {
	if cfg.Dir == "" {
		cfg.Dir = "."
	}
	if cfg.Agent == "" {
		cfg.Agent = defaultAgentKey
	}
	if cfg.OllamaBaseURL == "" {
		cfg.OllamaBaseURL = defaultOllamaBaseURL
	}
	if cfg.OllamaModel == "" {
		cfg.OllamaModel = defaultOllamaModel
	}
	if cfg.StorePath == "" {
		cfg.StorePath = defaultStorePath
	}
	return cfg
}

// promptInit дозапрашивает через in (обычно os.Stdin) поля cfg, оставшиеся
// пустыми (флаг = преответ — если поле уже задано, промпт по нему
// пропускается). Пустой ввод = дефолт, показанный в скобках.
func promptInit(in io.Reader, out io.Writer, cfg initConfig) initConfig {
	sc := bufio.NewScanner(in)

	if cfg.Agent == "" {
		fmt.Fprintf(out, "агентские приложения: %s\n", strings.Join(agentKeys(), ", "))
		cfg.Agent = promptLine(sc, out, "агентское приложение", defaultAgentKey)
	}
	if cfg.Project == "" {
		cfg.Project = promptLine(sc, out, "имя проекта", dirBaseName(cfg.Dir))
	}
	if cfg.OllamaBaseURL == "" {
		cfg.OllamaBaseURL = promptLine(sc, out, "ollama base URL", defaultOllamaBaseURL)
	}
	if cfg.OllamaModel == "" {
		cfg.OllamaModel = promptLine(sc, out, "ollama модель эмбеддингов", defaultOllamaModel)
	}
	if cfg.StorePath == "" {
		cfg.StorePath = promptLine(sc, out, "путь к файлу БД", defaultStorePath)
	}
	if cfg.DeepseekKey == "" {
		fmt.Fprintln(out, "ключ DeepSeek API эхается в терминал при вводе (маскировка требует x/term, вне stdlib) — можно пропустить и задать позже через .env")
		cfg.DeepseekKey = promptLine(sc, out, "DeepSeek API key (Enter — пропустить)", "")
	}
	return cfg
}

// promptLine печатает question (с def в скобках, если def непуст) и читает
// одну строку из sc. Пустой ввод или EOF — возвращают def.
func promptLine(sc *bufio.Scanner, out io.Writer, question, def string) string {
	if def != "" {
		fmt.Fprintf(out, "%s [%s]: ", question, def)
	} else {
		fmt.Fprintf(out, "%s: ", question)
	}
	if !sc.Scan() {
		return def
	}
	line := strings.TrimSpace(sc.Text())
	if line == "" {
		return def
	}
	return line
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
		fmt.Fprintf(out, "предупреждение: os.Executable() недоступен (%v), в MCP-конфиге подставлена команда \"memoryd\" (расчёт на PATH)\n", err)
	}

	mcpContent, err := target.renderMCPConfig(command)
	if err != nil {
		return err
	}
	skillContent := target.renderSkill(project)
	tomlContent := fmt.Sprintf(configTomlTemplate,
		escapeTOMLString(project), escapeTOMLString(cfg.StorePath), escapeTOMLString(cfg.OllamaBaseURL), escapeTOMLString(cfg.OllamaModel))

	if err := writeScaffoldFile(target.mcpConfigPath(cfg.Dir), mcpContent, cfg.Force, out); err != nil {
		return err
	}

	skillPath := target.skillPath(cfg.Dir)
	if err := os.MkdirAll(filepath.Dir(skillPath), 0o755); err != nil {
		return fmt.Errorf("создание каталога %q: %w", filepath.Dir(skillPath), err)
	}
	if err := writeScaffoldFile(skillPath, skillContent, cfg.Force, out); err != nil {
		return err
	}

	if err := writeScaffoldFile(filepath.Join(cfg.Dir, "agentmemory.toml"), tomlContent, cfg.Force, out); err != nil {
		return err
	}

	if cfg.DeepseekKey != "" {
		if err := writeEnvFile(filepath.Join(cfg.Dir, ".env"), cfg.DeepseekKey, cfg.Force, out); err != nil {
			return err
		}
		if err := ensureGitignoreEntry(filepath.Join(cfg.Dir, ".gitignore"), ".env", out); err != nil {
			return err
		}
	}

	fmt.Fprintf(out, "проект: %s\n", project)
	fmt.Fprintf(out, "агент: %s\n", target.label)
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

// renderMCPJSON собирает .mcp.json (сервер agentmemory, stdio) через
// encoding/json — гарантированно валидный JSON с отступом 2 пробела.
func renderMCPJSON(command string) (string, error) {
	cfg := mcpConfigFile{
		MCPServers: map[string]mcpServerSpec{
			"agentmemory": {
				Command: command,
				Args:    mcpServerArgs,
				Env:     map[string]string{"DEEPSEEK_API_KEY": "${DEEPSEEK_API_KEY:-}"},
			},
		},
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", fmt.Errorf("сборка .mcp.json: %w", err)
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

// writeEnvFile пишет DEEPSEEK_API_KEY=<key> в path с правами 0o600 (секрет).
// Идемпотентность и -force семантика — как у writeScaffoldFile.
func writeEnvFile(path, key string, force bool, out io.Writer) error {
	if !force {
		if _, err := os.Stat(path); err == nil {
			fmt.Fprintf(out, "пропущено (уже существует): %s\n", path)
			return nil
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("проверка %q: %w", path, err)
		}
	}
	content := fmt.Sprintf("DEEPSEEK_API_KEY=%s\n", key)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("запись %q: %w", path, err)
	}
	fmt.Fprintf(out, "записано: %s (ключ DeepSeek, права 0600)\n", path)
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
