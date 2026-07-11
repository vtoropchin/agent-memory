package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentmemory/internal/config"
)

// TestDoInit_WritesBothFiles проверяет, что init в чистый временный каталог
// пишет оба файла: .mcp.json — валидный JSON с сервером agentmemory,
// agentmemory.toml — конфиг, из которого config.Load восстанавливает нужный
// scope.project и провайдеров (round-trip сгенерированного конфига).
func TestDoInit_WritesBothFiles(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer

	if err := doInit("myproj", dir, false, &out); err != nil {
		t.Fatalf("doInit: %v", err)
	}

	mcpPath := filepath.Join(dir, ".mcp.json")
	mcpRaw, err := os.ReadFile(mcpPath)
	if err != nil {
		t.Fatalf("чтение %s: %v", mcpPath, err)
	}
	var mcpCfg mcpConfigFile
	if err := json.Unmarshal(mcpRaw, &mcpCfg); err != nil {
		t.Fatalf(".mcp.json невалиден: %v\n%s", err, mcpRaw)
	}
	srv, ok := mcpCfg.MCPServers["agentmemory"]
	if !ok {
		t.Fatalf(".mcp.json не содержит сервер agentmemory: %s", mcpRaw)
	}
	if srv.Command == "" {
		t.Errorf("mcpServers.agentmemory.command пуст")
	}
	wantArgs := []string{"-config", "${CLAUDE_PROJECT_DIR:-.}/agentmemory.toml"}
	if len(srv.Args) != len(wantArgs) || srv.Args[0] != wantArgs[0] || srv.Args[1] != wantArgs[1] {
		t.Errorf("mcpServers.agentmemory.args = %v, хочу %v", srv.Args, wantArgs)
	}
	if srv.Env["DEEPSEEK_API_KEY"] != "${DEEPSEEK_API_KEY:-}" {
		t.Errorf("mcpServers.agentmemory.env.DEEPSEEK_API_KEY = %q", srv.Env["DEEPSEEK_API_KEY"])
	}

	tomlPath := filepath.Join(dir, "agentmemory.toml")
	cfg, err := config.Load(tomlPath)
	if err != nil {
		t.Fatalf("config.Load(%s): %v", tomlPath, err)
	}
	if cfg.Scope.Project != "myproj" {
		t.Errorf("Scope.Project = %q, хочу %q", cfg.Scope.Project, "myproj")
	}
	if len(cfg.Scope.Shared) != 1 || cfg.Scope.Shared[0] != "user:*" {
		t.Errorf("Scope.Shared = %v, хочу [user:*]", cfg.Scope.Shared)
	}
	if cfg.Embedder.Provider != "ollama" {
		t.Errorf("Embedder.Provider = %q, хочу ollama", cfg.Embedder.Provider)
	}
	if cfg.Reasoner.Provider != "deepseek" {
		t.Errorf("Reasoner.Provider = %q, хочу deepseek", cfg.Reasoner.Provider)
	}
	if cfg.Store.Path != defaultStorePath {
		t.Errorf("Store.Path = %q, хочу %q", cfg.Store.Path, defaultStorePath)
	}

	if !strings.Contains(out.String(), "myproj") {
		t.Errorf("вывод не упоминает имя проекта: %s", out.String())
	}
}

// TestDoInit_DefaultProjectNameFromDir проверяет, что при пустом -project имя
// проекта берётся из базового имени -dir, а явный -project его переопределяет.
func TestDoInit_DefaultProjectNameFromDir(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "cool-app")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	var out bytes.Buffer
	if err := doInit("", dir, false, &out); err != nil {
		t.Fatalf("doInit: %v", err)
	}
	cfg, err := config.Load(filepath.Join(dir, "agentmemory.toml"))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Scope.Project != "cool-app" {
		t.Errorf("Scope.Project = %q, хочу %q (из имени каталога)", cfg.Scope.Project, "cool-app")
	}

	var out2 bytes.Buffer
	if err := doInit("explicit-name", dir, true, &out2); err != nil {
		t.Fatalf("doInit c -force: %v", err)
	}
	cfg2, err := config.Load(filepath.Join(dir, "agentmemory.toml"))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg2.Scope.Project != "explicit-name" {
		t.Errorf("Scope.Project = %q, хочу %q (явный -project)", cfg2.Scope.Project, "explicit-name")
	}
}

// TestDoInit_ExistingFileSkippedWithoutForce проверяет, что существующий файл
// без -force пропускается (содержимое не меняется, повтор идемпотентен), а с
// -force — перезаписывается.
func TestDoInit_ExistingFileSkippedWithoutForce(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "agentmemory.toml")
	const sentinel = "# уже существующий конфиг, не трогать\n"
	if err := os.WriteFile(tomlPath, []byte(sentinel), 0o644); err != nil {
		t.Fatalf("запись стартового файла: %v", err)
	}

	var out bytes.Buffer
	if err := doInit("myproj", dir, false, &out); err != nil {
		t.Fatalf("doInit: %v", err)
	}
	got, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatalf("чтение: %v", err)
	}
	if string(got) != sentinel {
		t.Errorf("файл изменился без -force: %s", got)
	}
	if !strings.Contains(out.String(), "пропущено") {
		t.Errorf("вывод не сообщает о пропуске: %s", out.String())
	}

	var out2 bytes.Buffer
	if err := doInit("myproj", dir, true, &out2); err != nil {
		t.Fatalf("doInit с -force: %v", err)
	}
	got2, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatalf("чтение: %v", err)
	}
	if string(got2) == sentinel {
		t.Errorf("файл не перезаписан с -force")
	}
	if !strings.Contains(string(got2), "myproj") {
		t.Errorf("перезаписанный файл не содержит project: %s", got2)
	}
}

// TestResolveProjectName_InvalidName проверяет, что пустое/невалидное имя
// проекта (корневой каталог без явного -project) — ошибка.
func TestResolveProjectName_InvalidName(t *testing.T) {
	if _, err := resolveProjectName("", string(filepath.Separator)); err == nil {
		t.Errorf("resolveProjectName(\"\", %q) = nil error, хочу ошибку", string(filepath.Separator))
	}
	got, err := resolveProjectName("explicit", string(filepath.Separator))
	if err != nil {
		t.Errorf("resolveProjectName с явным -project вернул ошибку: %v", err)
	}
	if got != "explicit" {
		t.Errorf("resolveProjectName = %q, хочу explicit", got)
	}

	// Имя с управляющим символом (перевод строки) — ошибка: иначе сырой '\n'
	// попал бы в basic-строку сгенерированного agentmemory.toml и сломал его.
	if _, err := resolveProjectName("a\nb", "."); err == nil {
		t.Errorf("resolveProjectName(%q) = nil error, хочу ошибку на управляющий символ", "a\nb")
	}
}
