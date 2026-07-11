// Подкоманда `memoryd init` — скаффолдинг подключения memoryd к проекту
// (DESIGN.md §14 Этап 5, задача 5.3). Раскладывает в целевой каталог .mcp.json
// (MCP-сервер agentmemory поверх stdio) и agentmemory.toml (полный конфиг с
// project = имя каталога) — самодостаточный проект, никакого глобального
// конфига. Не трогает загрузку конфига (internal/config) и серверный путь.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// defaultStorePath — путь к файлу SQLite в сгенерированном конфиге (совпадает с
// config.Default().Store.Path и корневым agentmemory.toml).
const defaultStorePath = "~/.agentmemory/memory.db"

// configTomlTemplate — шаблон agentmemory.toml по образцу корневого
// agentmemory.toml/config.example.toml (DESIGN.md §13). Константа-строка, а не
// toml.Marshal — так сохраняются комментарии. %[1]s — экранированное имя
// проекта, %[2]s — путь к файлу БД.
const configTomlTemplate = `# Конфиг AgentMemory для проекта %[1]s (DESIGN.md §13).
#
# Claude Code поднимает MCP-сервер agentmemory из .mcp.json в этом каталоге и
# передаёт ему этот файл флагом -config.

[store]
# Общий файл БД на все проекты (DESIGN.md §2.1: один файл, изоляция через scope).
# Каталог создаётся автоматически при первом старте (store.Open).
path = "%[2]s"

[embedder]
# Локальный ollama + bge-m3 (dim определяется автоматически пробным эмбеддингом).
# Если ollama недоступен — memoryd стартует, recall падает на чистый FTS5 (§15).
provider = "ollama"
base_url = "http://localhost:11434"
model    = "bge-m3"

[reasoner]
# DeepSeek chat: ключ берётся из env DEEPSEEK_API_KEY (пробрасывается через
# .mcp.json), а не из этого файла. Пустой ключ — деградация до noop (§15):
# recall без суммаризации, консолидация недоступна — но память работает.
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

// mcpServerArgs — аргументы, с которыми Claude Code запускает memoryd как
// MCP-сервер agentmemory.
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

// runInit разбирает флаги подкоманды `init` и выполняет скаффолдинг. Вынесена
// из диспетчера в main.go для тестируемости.
func runInit(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	project := fs.String("project", "", "имя проекта (дефолт — базовое имя каталога -dir)")
	dir := fs.String("dir", ".", "каталог, в который скаффолдить .mcp.json и agentmemory.toml")
	force := fs.Bool("force", false, "перезаписывать существующие файлы")
	_ = fs.Parse(args) // flag.ExitOnError сам завершает процесс при ошибке разбора

	return doInit(*project, *dir, *force, out)
}

// doInit — собственно логика init, отделённая от разбора флагов для тестов.
func doInit(project, dir string, force bool, out io.Writer) error {
	project, err := resolveProjectName(project, dir)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("создание каталога %q: %w", dir, err)
	}

	command := "memoryd" // фолбэк на PATH, если os.Executable() недоступен
	if exe, err := os.Executable(); err == nil {
		command = exe
	} else {
		fmt.Fprintf(out, "предупреждение: os.Executable() недоступен (%v), в .mcp.json подставлена команда \"memoryd\" (расчёт на PATH)\n", err)
	}

	mcpJSON, err := renderMCPJSON(command)
	if err != nil {
		return err
	}
	tomlContent := fmt.Sprintf(configTomlTemplate, escapeTOMLString(project), defaultStorePath)

	if err := writeScaffoldFile(filepath.Join(dir, ".mcp.json"), mcpJSON, force, out); err != nil {
		return err
	}
	if err := writeScaffoldFile(filepath.Join(dir, "agentmemory.toml"), tomlContent, force, out); err != nil {
		return err
	}

	fmt.Fprintf(out, "проект: %s\n", project)
	fmt.Fprintf(out, "путь БД: %s\n", defaultStorePath)
	fmt.Fprintln(out, "дальше: перезапусти Claude Code в этом каталоге и одобри сервер `agentmemory`; убедись, что memoryd доступен — `go install` кладёт его на PATH (см. `make install`).")
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
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", fmt.Errorf("резолв каталога %q: %w", dir, err)
		}
		base := filepath.Base(abs)
		if base == "" || base == "." || base == string(filepath.Separator) {
			return "", fmt.Errorf("не удалось определить имя проекта из каталога %q — укажи явно -project", dir)
		}
		project = base
	}

	if strings.IndexFunc(project, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("имя проекта %q содержит управляющий символ — укажи корректное -project", project)
	}
	return project, nil
}

// escapeTOMLString экранирует project для подстановки в TOML-строку в двойных
// кавычках (обратный слэш и кавычка) — имя каталога, как правило, безопасно,
// но подстановка не должна ломать синтаксис файла в редких случаях.
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

// writeScaffoldFile пишет content в path. Если файл уже существует и force
// не задан — пропускает его с предупреждением в out (не ошибка): повторный
// `memoryd init` идемпотентен и безопасен.
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
