package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentmemory/internal/config"
)

// TestDoInit_WritesAllFiles проверяет, что init в чистый временный каталог
// пишет все артефакты: .mcp.json — валидный JSON с сервером agentmemory,
// SKILL.md — скил под claude, agentmemory.toml — конфиг, из которого
// config.Load восстанавливает нужный scope.project и провайдеров (round-trip
// сгенерированного конфига).
func TestDoInit_WritesAllFiles(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer

	if err := doInit(initConfig{Project: "myproj", Dir: dir}, &out); err != nil {
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
	if cfg.Embedder.BaseURL != defaultOllamaBaseURL {
		t.Errorf("Embedder.BaseURL = %q, хочу %q", cfg.Embedder.BaseURL, defaultOllamaBaseURL)
	}
	if cfg.Embedder.Model != defaultOllamaModel {
		t.Errorf("Embedder.Model = %q, хочу %q", cfg.Embedder.Model, defaultOllamaModel)
	}
	if cfg.Reasoner.Provider != "deepseek" {
		t.Errorf("Reasoner.Provider = %q, хочу deepseek", cfg.Reasoner.Provider)
	}
	if cfg.Store.Path != defaultStorePath {
		t.Errorf("Store.Path = %q, хочу %q", cfg.Store.Path, defaultStorePath)
	}

	skillPath := filepath.Join(dir, ".claude", "skills", "memoryd", "SKILL.md")
	skillRaw, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("чтение %s: %v", skillPath, err)
	}
	skill := string(skillRaw)
	if !strings.HasPrefix(skill, "---\nname: memoryd\ndescription:") {
		t.Errorf("SKILL.md не начинается с валидного фронтматтера name/description")
	}
	if strings.Count(skill, "---\n") < 2 {
		t.Errorf("SKILL.md не содержит закрывающий разделитель фронтматтера: %s", skill)
	}
	if !strings.Contains(skill, "myproj") {
		t.Errorf("SKILL.md не упоминает имя проекта: %s", skill)
	}
	if !strings.Contains(skill, "memory_recall") || !strings.Contains(skill, "memory_save") {
		t.Errorf("SKILL.md не описывает memory_recall/memory_save: %s", skill)
	}

	if !strings.Contains(out.String(), "myproj") {
		t.Errorf("вывод не упоминает имя проекта: %s", out.String())
	}

	// Без -deepseek-key .env и .gitignore не создаются.
	if _, err := os.Stat(filepath.Join(dir, ".env")); !os.IsNotExist(err) {
		t.Errorf(".env создан без ключа DeepSeek: err=%v", err)
	}
}

// TestDoInit_DefaultProjectNameFromDir проверяет, что при пустом Project имя
// проекта берётся из базового имени Dir, а явный Project его переопределяет.
func TestDoInit_DefaultProjectNameFromDir(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "cool-app")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	var out bytes.Buffer
	if err := doInit(initConfig{Dir: dir}, &out); err != nil {
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
	if err := doInit(initConfig{Project: "explicit-name", Dir: dir, Force: true}, &out2); err != nil {
		t.Fatalf("doInit c Force: %v", err)
	}
	cfg2, err := config.Load(filepath.Join(dir, "agentmemory.toml"))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg2.Scope.Project != "explicit-name" {
		t.Errorf("Scope.Project = %q, хочу %q (явный Project)", cfg2.Scope.Project, "explicit-name")
	}
}

// TestDoInit_ExistingFileSkippedWithoutForce проверяет, что существующий файл
// без Force пропускается (содержимое не меняется, повтор идемпотентен), а с
// Force — перезаписывается.
func TestDoInit_ExistingFileSkippedWithoutForce(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "agentmemory.toml")
	const sentinel = "# уже существующий конфиг, не трогать\n"
	if err := os.WriteFile(tomlPath, []byte(sentinel), 0o644); err != nil {
		t.Fatalf("запись стартового файла: %v", err)
	}

	var out bytes.Buffer
	if err := doInit(initConfig{Project: "myproj", Dir: dir}, &out); err != nil {
		t.Fatalf("doInit: %v", err)
	}
	got, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatalf("чтение: %v", err)
	}
	if string(got) != sentinel {
		t.Errorf("файл изменился без Force: %s", got)
	}
	if !strings.Contains(out.String(), "пропущено") {
		t.Errorf("вывод не сообщает о пропуске: %s", out.String())
	}

	var out2 bytes.Buffer
	if err := doInit(initConfig{Project: "myproj", Dir: dir, Force: true}, &out2); err != nil {
		t.Fatalf("doInit с Force: %v", err)
	}
	got2, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatalf("чтение: %v", err)
	}
	if string(got2) == sentinel {
		t.Errorf("файл не перезаписан с Force")
	}
	if !strings.Contains(string(got2), "myproj") {
		t.Errorf("перезаписанный файл не содержит project: %s", got2)
	}
}

// TestDoInit_UnknownAgent проверяет, что неизвестное агентское приложение
// даёт внятную ошибку со списком поддерживаемых.
func TestDoInit_UnknownAgent(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	err := doInit(initConfig{Project: "myproj", Dir: dir, Agent: "bogus"}, &out)
	if err == nil {
		t.Fatalf("doInit с неизвестным Agent = nil error, хочу ошибку")
	}
	if !strings.Contains(err.Error(), "bogus") || !strings.Contains(err.Error(), "claude") {
		t.Errorf("ошибка не упоминает неизвестный агент/список поддерживаемых: %v", err)
	}
}

// TestDoInit_OpencodeTarget проверяет, что при Agent="opencode" doInit пишет
// opencode.json (local MCP-сервер agentmemory, форма которого отличается от
// .mcp.json Claude Code — единый массив command, environment вместо env) и
// SKILL.md по opencode-specific пути .opencode/skills/memoryd/SKILL.md.
func TestDoInit_OpencodeTarget(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer

	if err := doInit(initConfig{Project: "myproj", Dir: dir, Agent: "opencode"}, &out); err != nil {
		t.Fatalf("doInit: %v", err)
	}

	ocPath := filepath.Join(dir, "opencode.json")
	ocRaw, err := os.ReadFile(ocPath)
	if err != nil {
		t.Fatalf("чтение %s: %v", ocPath, err)
	}
	var raw map[string]any
	if err := json.Unmarshal(ocRaw, &raw); err != nil {
		t.Fatalf("opencode.json невалиден: %v\n%s", err, ocRaw)
	}

	mcp, ok := raw["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("opencode.json.mcp отсутствует или не объект: %s", ocRaw)
	}
	srv, ok := mcp["agentmemory"].(map[string]any)
	if !ok {
		t.Fatalf("opencode.json.mcp.agentmemory отсутствует или не объект: %s", ocRaw)
	}
	if srv["type"] != "local" {
		t.Errorf("mcp.agentmemory.type = %v, хочу local", srv["type"])
	}
	cmd, ok := srv["command"].([]any)
	if !ok {
		t.Fatalf("mcp.agentmemory.command не массив: %v", srv["command"])
	}
	if len(cmd) == 0 || cmd[0] == "" {
		t.Errorf("mcp.agentmemory.command[0] пуст: %v", cmd)
	}
	var cmdStrs []string
	for _, c := range cmd {
		s, _ := c.(string)
		cmdStrs = append(cmdStrs, s)
	}
	joined := strings.Join(cmdStrs, " ")
	if !strings.Contains(joined, "-config") || !strings.Contains(joined, "agentmemory.toml") {
		t.Errorf("mcp.agentmemory.command не содержит -config agentmemory.toml: %v", cmd)
	}
	if srv["enabled"] != true {
		t.Errorf("mcp.agentmemory.enabled = %v, хочу true", srv["enabled"])
	}
	env, ok := srv["environment"].(map[string]any)
	if !ok {
		t.Fatalf("mcp.agentmemory.environment не объект: %v", srv["environment"])
	}
	if env["DEEPSEEK_API_KEY"] != "{env:DEEPSEEK_API_KEY}" {
		t.Errorf("mcp.agentmemory.environment.DEEPSEEK_API_KEY = %v, хочу {env:DEEPSEEK_API_KEY}", env["DEEPSEEK_API_KEY"])
	}

	skillPath := filepath.Join(dir, ".opencode", "skills", "memoryd", "SKILL.md")
	skillRaw, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("чтение %s: %v", skillPath, err)
	}
	skill := string(skillRaw)
	if !strings.HasPrefix(skill, "---\nname: memoryd\ndescription:") {
		t.Errorf("SKILL.md не начинается с валидного фронтматтера name/description")
	}
	if !strings.Contains(skill, "myproj") {
		t.Errorf("SKILL.md не упоминает имя проекта: %s", skill)
	}
	if !strings.Contains(skill, "memory_recall") {
		t.Errorf("SKILL.md не упоминает memory_recall: %s", skill)
	}
}

// TestDoInit_WritesEnvAndGitignore проверяет запись .env (0600, с ключом) и
// дополнение .gitignore строкой .env без дублей при повторном запуске.
func TestDoInit_WritesEnvAndGitignore(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer

	if err := doInit(initConfig{Project: "myproj", Dir: dir, DeepseekKey: "sk-test-123"}, &out); err != nil {
		t.Fatalf("doInit: %v", err)
	}

	envPath := filepath.Join(dir, ".env")
	fi, err := os.Stat(envPath)
	if err != nil {
		t.Fatalf("stat .env: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf(".env права = %o, хочу 0600", perm)
	}
	envContent, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("чтение .env: %v", err)
	}
	if strings.TrimSpace(string(envContent)) != "DEEPSEEK_API_KEY=sk-test-123" {
		t.Errorf(".env содержимое = %q", envContent)
	}

	gitignorePath := filepath.Join(dir, ".gitignore")
	gitignoreContent, err := os.ReadFile(gitignorePath)
	if err != nil {
		t.Fatalf("чтение .gitignore: %v", err)
	}
	if strings.Count(string(gitignoreContent), ".env") != 1 {
		t.Errorf(".gitignore содержит не одну строку .env: %q", gitignoreContent)
	}

	// Повторный запуск с Force не дублирует строку .env в .gitignore.
	var out2 bytes.Buffer
	if err := doInit(initConfig{Project: "myproj", Dir: dir, DeepseekKey: "sk-test-456", Force: true}, &out2); err != nil {
		t.Fatalf("doInit повторно: %v", err)
	}
	gitignoreContent2, err := os.ReadFile(gitignorePath)
	if err != nil {
		t.Fatalf("чтение .gitignore: %v", err)
	}
	if strings.Count(string(gitignoreContent2), ".env") != 1 {
		t.Errorf(".gitignore задублировал строку .env: %q", gitignoreContent2)
	}
}

// TestEnsureGitignoreEntry_AppendsToExisting проверяет, что существующий
// .gitignore с другим содержимым дополняется, а не перезаписывается, и что
// повторный вызов не дублирует запись.
func TestEnsureGitignoreEntry_AppendsToExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".gitignore")
	if err := os.WriteFile(path, []byte("node_modules/\n"), 0o644); err != nil {
		t.Fatalf("запись стартового .gitignore: %v", err)
	}

	var out bytes.Buffer
	if err := ensureGitignoreEntry(path, ".env", &out); err != nil {
		t.Fatalf("ensureGitignoreEntry: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("чтение: %v", err)
	}
	if !strings.Contains(string(got), "node_modules/") {
		t.Errorf("существующее содержимое потеряно: %q", got)
	}
	if !strings.Contains(string(got), ".env") {
		t.Errorf("строка .env не добавлена: %q", got)
	}

	if err := ensureGitignoreEntry(path, ".env", &out); err != nil {
		t.Fatalf("ensureGitignoreEntry повторно: %v", err)
	}
	got2, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("чтение: %v", err)
	}
	if strings.Count(string(got2), ".env") != 1 {
		t.Errorf("повторный вызов задублировал строку: %q", got2)
	}
}

// TestPromptInit_FlagsSkipPrompts проверяет, что уже заполненные (флагом)
// поля initConfig не переспрашиваются: promptInit не должен читать за них из
// stdin. Пустой Reader (0 байт) означает немедленный EOF при первом Scan —
// если promptInit попытается спросить про что-то незаполненное, вернётся его
// дефолт, а не зависание.
func TestPromptInit_FlagsSkipPrompts(t *testing.T) {
	in := strings.NewReader("") // сразу EOF на любой Scan
	var out bytes.Buffer

	cfg := initConfig{
		Agent:         "claude",
		Project:       "preset-project",
		OllamaBaseURL: "http://preset:1234",
		OllamaModel:   "preset-model",
		StorePath:     "/preset/path.db",
		DeepseekKey:   "preset-key",
		Hooks:         "none",
	}
	got := promptInit(in, &out, cfg)
	if got != cfg {
		t.Errorf("promptInit изменил уже заполненные поля: %+v, хочу %+v", got, cfg)
	}
	if out.Len() != 0 {
		t.Errorf("promptInit печатал промпты для уже заполненных полей: %s", out.String())
	}
}

// TestPromptInit_EmptyInputUsesDefaults проверяет, что пустые строки ввода
// (одни Enter) дают дефолтные значения для незаполненных полей.
func TestPromptInit_EmptyInputUsesDefaults(t *testing.T) {
	// agent, project, ollama-url, ollama-model, store, deepseek-key — 6 полей,
	// 6 пустых строк ввода (Enter без значения на каждый вопрос).
	in := strings.NewReader("\n\n\n\n\n\n")
	var out bytes.Buffer

	cfg := promptInit(in, &out, initConfig{Dir: "."})
	if cfg.Agent != defaultAgentKey {
		t.Errorf("Agent = %q, хочу %q", cfg.Agent, defaultAgentKey)
	}
	if cfg.OllamaBaseURL != defaultOllamaBaseURL {
		t.Errorf("OllamaBaseURL = %q, хочу %q", cfg.OllamaBaseURL, defaultOllamaBaseURL)
	}
	if cfg.OllamaModel != defaultOllamaModel {
		t.Errorf("OllamaModel = %q, хочу %q", cfg.OllamaModel, defaultOllamaModel)
	}
	if cfg.StorePath != defaultStorePath {
		t.Errorf("StorePath = %q, хочу %q", cfg.StorePath, defaultStorePath)
	}
	if cfg.DeepseekKey != "" {
		t.Errorf("DeepseekKey = %q, хочу пусто (Enter пропускает)", cfg.DeepseekKey)
	}
}

// TestPromptChoice проверяет выбор опции по номеру и дефолт при пустом вводе.
func TestPromptChoice(t *testing.T) {
	options := []string{"claude", "opencode"}

	var out bytes.Buffer
	sc := bufio.NewScanner(strings.NewReader("2\n"))
	got := promptChoice(sc, &out, "агентское приложение", options, "claude")
	if got != "opencode" {
		t.Errorf("promptChoice(%q) = %q, хочу opencode", "2", got)
	}

	var out2 bytes.Buffer
	sc2 := bufio.NewScanner(strings.NewReader("\n"))
	got2 := promptChoice(sc2, &out2, "агентское приложение", options, "claude")
	if got2 != "claude" {
		t.Errorf("promptChoice(пустой ввод) = %q, хочу дефолт claude", got2)
	}
}

// TestRunInit_NonInteractive_DoesNotBlock проверяет, что runInit не
// блокируется на чтении stdin в тестовом окружении (go test не подключает
// stdin к терминалу — isInteractiveStdin() должен вернуть false), беря
// значения из флагов/дефолтов.
func TestRunInit_NonInteractive_DoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	err := runInit([]string{"-project", "myproj", "-dir", dir}, &out)
	if err != nil {
		t.Fatalf("runInit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "agentmemory.toml")); err != nil {
		t.Errorf("agentmemory.toml не создан: %v", err)
	}
}

// TestDoInit_HooksSessionStartWritesSettingsJSON проверяет, что при
// Hooks="session-start" doInit пишет секцию hooks в .claude/settings.json с
// командой memoryd hook -event session-start (задача 7.2), и что повторный
// init не дублирует запись (идемпотентность).
func TestDoInit_HooksSessionStartWritesSettingsJSON(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := doInit(initConfig{Project: "myproj", Dir: dir, Hooks: hooksSessionStart}, &out); err != nil {
		t.Fatalf("doInit: %v", err)
	}

	settingsPath := filepath.Join(dir, ".claude", "settings.json")
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("чтение %s: %v", settingsPath, err)
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatalf("settings.json невалиден: %v\n%s", err, raw)
	}
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("settings.json.hooks отсутствует или не объект: %s", raw)
	}
	sessionStart, ok := hooks["SessionStart"].([]any)
	if !ok || len(sessionStart) != 1 {
		t.Fatalf("settings.json.hooks.SessionStart = %v, хочу один matcher-элемент", hooks["SessionStart"])
	}
	if _, hasPromptSubmit := hooks["UserPromptSubmit"]; hasPromptSubmit {
		t.Errorf("settings.json.hooks содержит UserPromptSubmit при Hooks=%q, не должен", hooksSessionStart)
	}
	entry, ok := sessionStart[0].(map[string]any)
	if !ok {
		t.Fatalf("SessionStart[0] не объект: %v", sessionStart[0])
	}
	entryHooks, ok := entry["hooks"].([]any)
	if !ok || len(entryHooks) != 1 {
		t.Fatalf("SessionStart[0].hooks = %v, хочу один элемент", entry["hooks"])
	}
	cmdObj, ok := entryHooks[0].(map[string]any)
	if !ok {
		t.Fatalf("SessionStart[0].hooks[0] не объект: %v", entryHooks[0])
	}
	if cmdObj["type"] != "command" {
		t.Errorf("hooks[0].type = %v, хочу command", cmdObj["type"])
	}
	cmd, _ := cmdObj["command"].(string)
	if !strings.Contains(cmd, "hook -event session-start") || !strings.Contains(cmd, "agentmemory.toml") {
		t.Errorf("hooks[0].command = %q, хочу упоминание \"hook -event session-start\" и agentmemory.toml", cmd)
	}

	// Повторный init (идемпотентность) не должен задублировать запись.
	var out2 bytes.Buffer
	if err := doInit(initConfig{Project: "myproj", Dir: dir, Hooks: hooksSessionStart, Force: true}, &out2); err != nil {
		t.Fatalf("doInit повторно: %v", err)
	}
	raw2, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("чтение %s: %v", settingsPath, err)
	}
	var settings2 map[string]any
	if err := json.Unmarshal(raw2, &settings2); err != nil {
		t.Fatalf("settings.json невалиден после повтора: %v\n%s", err, raw2)
	}
	hooks2 := settings2["hooks"].(map[string]any)
	sessionStart2 := hooks2["SessionStart"].([]any)
	if len(sessionStart2) != 1 {
		t.Errorf("повторный init задублировал matcher-записи SessionStart: %v", sessionStart2)
	}
}

// TestDoInit_HooksBothWritesBothEvents проверяет, что
// Hooks="session-start+prompt-submit" пишет обе секции события.
func TestDoInit_HooksBothWritesBothEvents(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := doInit(initConfig{Project: "myproj", Dir: dir, Hooks: hooksBoth}, &out); err != nil {
		t.Fatalf("doInit: %v", err)
	}

	settingsPath := filepath.Join(dir, ".claude", "settings.json")
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("чтение %s: %v", settingsPath, err)
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatalf("settings.json невалиден: %v\n%s", err, raw)
	}
	hooks := settings["hooks"].(map[string]any)
	if _, ok := hooks["SessionStart"]; !ok {
		t.Errorf("settings.json.hooks не содержит SessionStart: %s", raw)
	}
	if _, ok := hooks["UserPromptSubmit"]; !ok {
		t.Errorf("settings.json.hooks не содержит UserPromptSubmit: %s", raw)
	}
}

// TestDoInit_HooksNoneDoesNotTouchSettingsJSON проверяет, что Hooks="none" (и
// пустое значение по умолчанию) не создают .claude/settings.json вовсе.
func TestDoInit_HooksNoneDoesNotTouchSettingsJSON(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := doInit(initConfig{Project: "myproj", Dir: dir, Hooks: hooksNone}, &out); err != nil {
		t.Fatalf("doInit: %v", err)
	}
	settingsPath := filepath.Join(dir, ".claude", "settings.json")
	if _, err := os.Stat(settingsPath); !os.IsNotExist(err) {
		t.Errorf("settings.json создан при Hooks=none: err=%v", err)
	}
}

// TestDoInit_HooksMergePreservesExistingSettings проверяет ключевое свойство
// merge (задача 7.2): существующий settings.json с чужими верхнеуровневыми
// ключами и чужими хуками (другое событие, другой matcher/timeout) не теряет
// их после того, как doInit дописывает свою секцию SessionStart.
func TestDoInit_HooksMergePreservesExistingSettings(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	existing := `{
  "permissions": {"allow": ["Bash(go test:*)"]},
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "echo pretooluse", "timeout": 5}]}
    ],
    "SessionStart": [
      {"matcher": "compact", "hooks": [{"type": "command", "command": "echo custom-compact-hook"}]}
    ]
  }
}`
	if err := os.WriteFile(settingsPath, []byte(existing), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var out bytes.Buffer
	if err := doInit(initConfig{Project: "myproj", Dir: dir, Hooks: hooksSessionStart}, &out); err != nil {
		t.Fatalf("doInit: %v", err)
	}

	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("чтение: %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatalf("settings.json невалиден: %v\n%s", err, raw)
	}

	perms, ok := settings["permissions"].(map[string]any)
	if !ok {
		t.Fatalf("чужой верхнеуровневый ключ permissions потерян: %s", raw)
	}
	if allow, _ := perms["allow"].([]any); len(allow) != 1 || allow[0] != "Bash(go test:*)" {
		t.Errorf("permissions.allow повреждён: %v", perms["allow"])
	}

	hooks := settings["hooks"].(map[string]any)
	preToolUse, ok := hooks["PreToolUse"].([]any)
	if !ok || len(preToolUse) != 1 {
		t.Fatalf("чужое событие PreToolUse потеряно: %v", hooks["PreToolUse"])
	}
	ptEntry := preToolUse[0].(map[string]any)
	if ptEntry["matcher"] != "Bash" {
		t.Errorf("PreToolUse[0].matcher = %v, хочу Bash", ptEntry["matcher"])
	}
	ptHooks := ptEntry["hooks"].([]any)
	ptCmd := ptHooks[0].(map[string]any)
	if ptCmd["command"] != "echo pretooluse" || ptCmd["timeout"] != float64(5) {
		t.Errorf("PreToolUse[0].hooks[0] повреждён: %v", ptCmd)
	}

	sessionStart := hooks["SessionStart"].([]any)
	if len(sessionStart) != 2 {
		t.Fatalf("SessionStart = %v, хочу 2 matcher-записи (чужая compact + новая пустая)", sessionStart)
	}
	var foundCustom, foundOurs bool
	for _, e := range sessionStart {
		entry := e.(map[string]any)
		entryHooks := entry["hooks"].([]any)
		cmd, _ := entryHooks[0].(map[string]any)["command"].(string)
		if entry["matcher"] == "compact" && cmd == "echo custom-compact-hook" {
			foundCustom = true
		}
		if strings.Contains(cmd, "hook -event session-start") {
			foundOurs = true
		}
	}
	if !foundCustom {
		t.Errorf("чужая matcher-запись SessionStart[compact] потеряна: %v", sessionStart)
	}
	if !foundOurs {
		t.Errorf("новая matcher-запись memoryd hook не добавлена: %v", sessionStart)
	}
}

// TestDoInit_HooksMalformedExistingSettingsErrorsWithoutWrite проверяет, что
// битый JSON в существующем settings.json — ошибка, а не перезапись/потеря
// файла.
func TestDoInit_HooksMalformedExistingSettingsErrorsWithoutWrite(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	const broken = "{ не валидный json"
	if err := os.WriteFile(settingsPath, []byte(broken), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var out bytes.Buffer
	err := doInit(initConfig{Project: "myproj", Dir: dir, Hooks: hooksSessionStart}, &out)
	if err == nil {
		t.Fatalf("doInit = nil error, хочу ошибку разбора settings.json")
	}

	raw, readErr := os.ReadFile(settingsPath)
	if readErr != nil {
		t.Fatalf("чтение: %v", readErr)
	}
	if string(raw) != broken {
		t.Errorf("settings.json изменён после ошибки: %q", raw)
	}
}

// TestDoInit_HooksOpencodeNotesUnsupported проверяет, что для opencode init не
// пишет settings.json (у opencode нет аналога хуков), а честно сообщает об
// этом в выводе (задача 7.2).
func TestDoInit_HooksOpencodeNotesUnsupported(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := doInit(initConfig{Project: "myproj", Dir: dir, Agent: "opencode", Hooks: hooksSessionStart}, &out); err != nil {
		t.Fatalf("doInit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Errorf("settings.json создан для opencode: err=%v", err)
	}
	if !strings.Contains(out.String(), "только Claude Code") {
		t.Errorf("вывод не сообщает об отсутствии ambient-режима у opencode: %s", out.String())
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
