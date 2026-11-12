package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

const (
	codexMemoStart = "# >>> memo init"
	codexMemoEnd   = "# <<< memo init"
)

func scaffoldCodex(dir, _ string, command string, cfg initConfig, out io.Writer) (string, error) {
	cwd, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("определение пути проекта %q: %w", dir, err)
	}
	path := filepath.Join(cwd, ".codex", "config.toml")
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("чтение %q: %w", path, err)
	}

	server := struct {
		Command string   `toml:"command"`
		Args    []string `toml:"args"`
		Cwd     string   `toml:"cwd"`
		EnvVars []string `toml:"env_vars,omitempty"`
	}{
		Command: command,
		Args:    []string{"-config", "agentmemory.toml"},
		Cwd:     cwd,
	}
	for _, provider := range selectedProviders(cfg) {
		if provider.envKey != "" {
			server.EnvVars = append(server.EnvVars, provider.envKey)
		}
	}
	if cfg.StoreDriver == storeDriverPostgres {
		server.EnvVars = append(server.EnvVars, memoPostgresPasswordEnv)
	}
	var entry bytes.Buffer
	if err := toml.NewEncoder(&entry).Encode(server); err != nil {
		return "", fmt.Errorf("сборка записи memo для %q: %w", path, err)
	}
	block := codexMemoStart + "\n[mcp_servers.memo]\n" + entry.String() + codexMemoEnd + "\n"
	content, err := mergeCodexMemoConfig(string(existing), block)
	if err != nil {
		return "", fmt.Errorf("настройка %q: %w", path, err)
	}
	if content != string(existing) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return "", fmt.Errorf("создание каталога %q: %w", filepath.Dir(path), err)
		}
		if err := overwriteFile(path, content, out); err != nil {
			return "", err
		}
	}
	return "открой новый сеанс Codex в доверенном проекте; для recall используй скил `memo`\n", nil
}

// mergeCodexMemoConfig заменяет только блок, созданный memo init. TOML
// проверяется до и после правки, а ручная запись memo остаётся под контролем
// пользователя. Остальной файл, включая комментарии, сохраняется дословно.
func mergeCodexMemoConfig(existing, block string) (string, error) {
	var config map[string]any
	meta, err := toml.Decode(existing, &config)
	if err != nil {
		return "", fmt.Errorf("разбор TOML: %w", err)
	}
	// Начальный перевод строки позволяет одинаково искать маркер в начале
	// файла и после любой другой секции.
	prefix, rest, managed := strings.Cut("\n"+existing, "\n"+codexMemoStart+"\n")
	var suffix string
	if managed {
		if !meta.IsDefined("mcp_servers", "memo") || !strings.HasPrefix(rest, "[mcp_servers.memo]\n") {
			return "", fmt.Errorf("некорректный блок memo init")
		}
		_, after, closed := strings.Cut(rest, "\n"+codexMemoEnd+"\n")
		if !closed {
			return "", fmt.Errorf("блок memo init без закрывающего маркера")
		}
		suffix = after
		meta, err = toml.Decode(strings.TrimPrefix(prefix, "\n")+"\n"+suffix, &config)
		if err != nil {
			return "", fmt.Errorf("разбор TOML вне блока memo init: %w", err)
		}
	}
	prefix = strings.TrimPrefix(prefix, "\n")
	if prefix != "" && !strings.HasSuffix(prefix, "\n") {
		prefix += "\n"
	}
	if meta.IsDefined("mcp_servers", "memo") {
		return "", fmt.Errorf("ручная запись mcp_servers.memo уже существует; сохрани её отдельно перед повторным memo init")
	}
	content := prefix + block + suffix
	if _, err := toml.Decode(content, &config); err != nil {
		return "", fmt.Errorf("разбор нового TOML: %w", err)
	}
	return content, nil
}
