package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentmemory/internal/config"
)

// TestDoInit_WritesAllFiles проверяет, что init в чистый временный каталог
// пишет все артефакты: .mcp.json — валидный JSON с сервером memo,
// оба SKILL.md под claude (memo и memo-bootstrap), agentmemory.toml —
// конфиг, из которого config.Load восстанавливает нужный scope.project и
// провайдеров (round-trip сгенерированного конфига). Дефолт без явных
// провайдеров — ollama/deepseek.
func TestDoInit_WritesAllFiles(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder

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
	srv, ok := mcpCfg.MCPServers["memo"]
	if !ok {
		t.Fatalf(".mcp.json не содержит сервер memo: %s", mcpRaw)
	}
	if srv.Command == "" {
		t.Errorf("mcpServers.memo.command пуст")
	}
	wantArgs := []string{"-config", "${CLAUDE_PROJECT_DIR:-.}/agentmemory.toml"}
	if len(srv.Args) != len(wantArgs) || srv.Args[0] != wantArgs[0] || srv.Args[1] != wantArgs[1] {
		t.Errorf("mcpServers.memo.args = %v, хочу %v", srv.Args, wantArgs)
	}
	if srv.Env["DEEPSEEK_API_KEY"] != "${DEEPSEEK_API_KEY:-}" {
		t.Errorf("mcpServers.memo.env.DEEPSEEK_API_KEY = %q", srv.Env["DEEPSEEK_API_KEY"])
	}
	if _, ok := srv.Env["OPENAI_API_KEY"]; ok {
		t.Errorf("mcpServers.memo.env содержит OPENAI_API_KEY при embedder=ollama: %v", srv.Env)
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

	skillPath := filepath.Join(dir, ".claude", "skills", "memo", "SKILL.md")
	skillRaw, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("чтение %s: %v", skillPath, err)
	}
	skill := string(skillRaw)
	if !strings.HasPrefix(skill, "---\nname: memo\ndescription:") {
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

	bootstrapPath := filepath.Join(dir, ".claude", "skills", "memo-bootstrap", "SKILL.md")
	bootstrapRaw, err := os.ReadFile(bootstrapPath)
	if err != nil {
		t.Fatalf("чтение %s: %v", bootstrapPath, err)
	}
	bootstrap := string(bootstrapRaw)
	if !strings.HasPrefix(bootstrap, "---\nname: memo-bootstrap\ndescription:") {
		t.Errorf("SKILL.md bootstrap не начинается с валидного фронтматтера name/description")
	}
	if strings.Count(bootstrap, "---\n") < 2 {
		t.Errorf("SKILL.md bootstrap не содержит закрывающий разделитель фронтматтера: %s", bootstrap)
	}
	if !strings.Contains(bootstrap, "myproj") {
		t.Errorf("SKILL.md bootstrap не упоминает имя проекта: %s", bootstrap)
	}
	if !strings.Contains(bootstrap, "memory_save") {
		t.Errorf("SKILL.md bootstrap не упоминает memory_save: %s", bootstrap)
	}
	if !strings.Contains(bootstrap, "graphify") {
		t.Errorf("SKILL.md bootstrap не упоминает graphify: %s", bootstrap)
	}

	if !strings.Contains(out.String(), "myproj") {
		t.Errorf("вывод не упоминает имя проекта: %s", out.String())
	}

	// Без ключей .env и .gitignore не создаются.
	if _, err := os.Stat(filepath.Join(dir, ".env")); !os.IsNotExist(err) {
		t.Errorf(".env создан без ключей: err=%v", err)
	}
}

// TestDoInit_ProviderCombinations прогоняет все комбинации embedder ×
// reasoner (ollama/openai/noop × deepseek/noop, задача 11.1) и проверяет, что
// agentmemory.toml содержит нужные блоки, .env — только выбранные ключи,
// .mcp.json — только выбранные плейсхолдеры env.
func TestDoInit_ProviderCombinations(t *testing.T) {
	tests := []struct {
		name             string
		embedderProvider string
		reasonerProvider string
		openaiKey        string
		deepseekKey      string
		wantEnvKeys      []string
	}{
		{name: "ollama+deepseek с ключом", embedderProvider: "ollama", reasonerProvider: "deepseek", deepseekKey: "sk-ds", wantEnvKeys: []string{"DEEPSEEK_API_KEY"}},
		{name: "ollama+noop", embedderProvider: "ollama", reasonerProvider: "noop"},
		{name: "openai+deepseek с обоими ключами", embedderProvider: "openai", reasonerProvider: "deepseek", openaiKey: "sk-oa", deepseekKey: "sk-ds", wantEnvKeys: []string{"DEEPSEEK_API_KEY", "OPENAI_API_KEY"}},
		{name: "openai+noop с ключом openai", embedderProvider: "openai", reasonerProvider: "noop", openaiKey: "sk-oa", wantEnvKeys: []string{"OPENAI_API_KEY"}},
		{name: "noop+deepseek", embedderProvider: "noop", reasonerProvider: "deepseek", deepseekKey: "sk-ds", wantEnvKeys: []string{"DEEPSEEK_API_KEY"}},
		{name: "noop+noop без ключей", embedderProvider: "noop", reasonerProvider: "noop"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			var out strings.Builder
			cfg := initConfig{
				Project:          "myproj",
				Dir:              dir,
				EmbedderProvider: tt.embedderProvider,
				ReasonerProvider: tt.reasonerProvider,
				OpenAIKey:        tt.openaiKey,
				DeepseekKey:      tt.deepseekKey,
			}
			if tt.embedderProvider == "openai" {
				cfg.EmbedderModel = "text-embedding-3-small"
			}
			if err := doInit(cfg, &out); err != nil {
				t.Fatalf("doInit: %v", err)
			}

			loaded, err := config.Load(filepath.Join(dir, "agentmemory.toml"))
			if err != nil {
				t.Fatalf("config.Load: %v", err)
			}
			if loaded.Embedder.Provider != tt.embedderProvider {
				t.Errorf("Embedder.Provider = %q, хочу %q", loaded.Embedder.Provider, tt.embedderProvider)
			}
			if loaded.Reasoner.Provider != tt.reasonerProvider {
				t.Errorf("Reasoner.Provider = %q, хочу %q", loaded.Reasoner.Provider, tt.reasonerProvider)
			}

			envPath := filepath.Join(dir, ".env")
			envRaw, err := os.ReadFile(envPath)
			if len(tt.wantEnvKeys) == 0 {
				if err == nil {
					t.Errorf(".env создан без выбранных ключей: %s", envRaw)
				}
			} else {
				if err != nil {
					t.Fatalf("чтение .env: %v", err)
				}
				envContent := string(envRaw)
				for _, k := range tt.wantEnvKeys {
					if !strings.Contains(envContent, k+"=") {
						t.Errorf(".env не содержит %s: %s", k, envContent)
					}
				}
				wantLines := len(tt.wantEnvKeys)
				gotLines := len(strings.Fields(strings.TrimSpace(envContent)))
				if gotLines != wantLines {
					t.Errorf(".env содержит %d строк(и), хочу %d: %q", gotLines, wantLines, envContent)
				}
			}

			mcpRaw, err := os.ReadFile(filepath.Join(dir, ".mcp.json"))
			if err != nil {
				t.Fatalf("чтение .mcp.json: %v", err)
			}
			var mcpCfg mcpConfigFile
			if err := json.Unmarshal(mcpRaw, &mcpCfg); err != nil {
				t.Fatalf(".mcp.json невалиден: %v", err)
			}
			env := mcpCfg.MCPServers["memo"].Env
			_, hasDeepseek := env["DEEPSEEK_API_KEY"]
			_, hasOpenAI := env["OPENAI_API_KEY"]
			wantDeepseek := tt.reasonerProvider == "deepseek"
			wantOpenAI := tt.embedderProvider == "openai"
			if hasDeepseek != wantDeepseek {
				t.Errorf(".mcp.json env DEEPSEEK_API_KEY присутствие = %v, хочу %v (reasoner=%s)", hasDeepseek, wantDeepseek, tt.reasonerProvider)
			}
			if hasOpenAI != wantOpenAI {
				t.Errorf(".mcp.json env OPENAI_API_KEY присутствие = %v, хочу %v (embedder=%s)", hasOpenAI, wantOpenAI, tt.embedderProvider)
			}
		})
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

	var out strings.Builder
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

	var out2 strings.Builder
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

// TestDoInit_OverwritesConfigFiles проверяет, что agentmemory.toml и
// .mcp.json ВСЕГДА перезаписываются целиком при повторном `memo init` —
// независимо от Force (единственный источник истины для них — параметры
// текущего прогона, см. overwriteFile в init.go; ручные правки этих двух
// файлов сознательно не переживают повторный init).
func TestDoInit_OverwritesConfigFiles(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "agentmemory.toml")
	mcpPath := filepath.Join(dir, ".mcp.json")
	const tomlSentinel = "# уже существующий конфиг, не трогать\n"
	const mcpSentinel = `{"mcpServers":{}}`
	if err := os.WriteFile(tomlPath, []byte(tomlSentinel), 0o644); err != nil {
		t.Fatalf("запись стартового agentmemory.toml: %v", err)
	}
	if err := os.WriteFile(mcpPath, []byte(mcpSentinel), 0o644); err != nil {
		t.Fatalf("запись стартового .mcp.json: %v", err)
	}

	var out strings.Builder
	if err := doInit(initConfig{Project: "myproj", Dir: dir}, &out); err != nil {
		t.Fatalf("doInit: %v", err)
	}

	got, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatalf("чтение agentmemory.toml: %v", err)
	}
	if string(got) == tomlSentinel {
		t.Errorf("agentmemory.toml не перезаписан без Force: %s", got)
	}
	if !strings.Contains(string(got), "myproj") {
		t.Errorf("перезаписанный agentmemory.toml не содержит project: %s", got)
	}

	mcpGot, err := os.ReadFile(mcpPath)
	if err != nil {
		t.Fatalf("чтение .mcp.json: %v", err)
	}
	if string(mcpGot) == mcpSentinel {
		t.Errorf(".mcp.json не перезаписан без Force: %s", mcpGot)
	}
	var mcpCfg mcpConfigFile
	if err := json.Unmarshal(mcpGot, &mcpCfg); err != nil {
		t.Fatalf("перезаписанный .mcp.json невалиден: %v\n%s", err, mcpGot)
	}
	if _, ok := mcpCfg.MCPServers["memo"]; !ok {
		t.Errorf("перезаписанный .mcp.json не содержит сервер memo: %s", mcpGot)
	}
}

// TestDoInit_UnknownAgent проверяет, что неизвестное агентское приложение
// даёт внятную ошибку со списком поддерживаемых.
func TestDoInit_UnknownAgent(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder
	err := doInit(initConfig{Project: "myproj", Dir: dir, Agent: "bogus"}, &out)
	if err == nil {
		t.Fatalf("doInit с неизвестным Agent = nil error, хочу ошибку")
	}
	if !strings.Contains(err.Error(), "bogus") || !strings.Contains(err.Error(), "claude") {
		t.Errorf("ошибка не упоминает неизвестный агент/список поддерживаемых: %v", err)
	}
}

// TestDoInit_OpencodeTarget проверяет, что при Agent="opencode" doInit пишет
// opencode.json (local MCP-сервер memo, форма которого отличается от
// .mcp.json Claude Code — единый массив command, environment вместо env) и
// оба SKILL.md по opencode-specific пути .opencode/skills/{memo,memo-bootstrap}/SKILL.md.
func TestDoInit_OpencodeTarget(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder

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
	srv, ok := mcp["memo"].(map[string]any)
	if !ok {
		t.Fatalf("opencode.json.mcp.memo отсутствует или не объект: %s", ocRaw)
	}
	if srv["type"] != "local" {
		t.Errorf("mcp.memo.type = %v, хочу local", srv["type"])
	}
	cmd, ok := srv["command"].([]any)
	if !ok {
		t.Fatalf("mcp.memo.command не массив: %v", srv["command"])
	}
	if len(cmd) == 0 || cmd[0] == "" {
		t.Errorf("mcp.memo.command[0] пуст: %v", cmd)
	}
	var cmdStrs []string
	for _, c := range cmd {
		s, _ := c.(string)
		cmdStrs = append(cmdStrs, s)
	}
	joined := strings.Join(cmdStrs, " ")
	if !strings.Contains(joined, "-config") || !strings.Contains(joined, "agentmemory.toml") {
		t.Errorf("mcp.memo.command не содержит -config agentmemory.toml: %v", cmd)
	}
	if srv["enabled"] != true {
		t.Errorf("mcp.memo.enabled = %v, хочу true", srv["enabled"])
	}
	env, ok := srv["environment"].(map[string]any)
	if !ok {
		t.Fatalf("mcp.memo.environment не объект: %v", srv["environment"])
	}
	if env["DEEPSEEK_API_KEY"] != "{env:DEEPSEEK_API_KEY}" {
		t.Errorf("mcp.memo.environment.DEEPSEEK_API_KEY = %v, хочу {env:DEEPSEEK_API_KEY}", env["DEEPSEEK_API_KEY"])
	}

	skillPath := filepath.Join(dir, ".opencode", "skills", "memo", "SKILL.md")
	skillRaw, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("чтение %s: %v", skillPath, err)
	}
	skill := string(skillRaw)
	if !strings.HasPrefix(skill, "---\nname: memo\ndescription:") {
		t.Errorf("SKILL.md не начинается с валидного фронтматтера name/description")
	}
	if !strings.Contains(skill, "myproj") {
		t.Errorf("SKILL.md не упоминает имя проекта: %s", skill)
	}
	if !strings.Contains(skill, "memory_recall") {
		t.Errorf("SKILL.md не упоминает memory_recall: %s", skill)
	}

	bootstrapPath := filepath.Join(dir, ".opencode", "skills", "memo-bootstrap", "SKILL.md")
	bootstrapRaw, err := os.ReadFile(bootstrapPath)
	if err != nil {
		t.Fatalf("чтение %s: %v", bootstrapPath, err)
	}
	bootstrap := string(bootstrapRaw)
	if !strings.HasPrefix(bootstrap, "---\nname: memo-bootstrap\ndescription:") {
		t.Errorf("SKILL.md bootstrap не начинается с валидного фронтматтера name/description")
	}
	if !strings.Contains(bootstrap, "myproj") {
		t.Errorf("SKILL.md bootstrap не упоминает имя проекта: %s", bootstrap)
	}
	if !strings.Contains(bootstrap, "memory_save") {
		t.Errorf("SKILL.md bootstrap не упоминает memory_save: %s", bootstrap)
	}
}

// TestDoInit_WritesEnvAndGitignore проверяет запись .env (0600, с ключом) и
// дополнение .gitignore строкой .env без дублей при повторном запуске.
func TestDoInit_WritesEnvAndGitignore(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder

	if err := doInit(initConfig{Project: "myproj", Dir: dir, ReasonerProvider: "deepseek", DeepseekKey: "sk-test-123"}, &out); err != nil {
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

	// Повторный запуск с Force не дублирует строку .env в .gitignore и
	// обновляет ключ в .env (merge, а не skip).
	var out2 strings.Builder
	if err := doInit(initConfig{Project: "myproj", Dir: dir, ReasonerProvider: "deepseek", DeepseekKey: "sk-test-456", Force: true}, &out2); err != nil {
		t.Fatalf("doInit повторно: %v", err)
	}
	gitignoreContent2, err := os.ReadFile(gitignorePath)
	if err != nil {
		t.Fatalf("чтение .gitignore: %v", err)
	}
	if strings.Count(string(gitignoreContent2), ".env") != 1 {
		t.Errorf(".gitignore задублировал строку .env: %q", gitignoreContent2)
	}
	envContent2, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("чтение .env повторно: %v", err)
	}
	if strings.TrimSpace(string(envContent2)) != "DEEPSEEK_API_KEY=sk-test-456" {
		t.Errorf(".env после повторного init с Force = %q, хочу обновлённый ключ sk-test-456", envContent2)
	}

	// Повторный запуск БЕЗ Force тоже обновляет .env (проблема из репорта
	// пользователя: раньше writeEnvFile без -force молча пропускал уже
	// существующий файл, новый ключ никогда не долетал до .env).
	var out3 strings.Builder
	if err := doInit(initConfig{Project: "myproj", Dir: dir, ReasonerProvider: "deepseek", DeepseekKey: "sk-test-789"}, &out3); err != nil {
		t.Fatalf("doInit повторно без Force: %v", err)
	}
	envContent3, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("чтение .env повторно без Force: %v", err)
	}
	if strings.TrimSpace(string(envContent3)) != "DEEPSEEK_API_KEY=sk-test-789" {
		t.Errorf(".env после повторного init без Force = %q, хочу обновлённый ключ sk-test-789 (не пропуск)", envContent3)
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

	var out strings.Builder
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

// TestMergeEnvFile_CreatesWhenMissing проверяет, что mergeEnvFile в
// отсутствующий файл пишет несколько ключей отсортированно и с правами 0600
// (задача 11.1: обобщение на набор ключей выбранных провайдеров) — merge в
// отсутствующий файл эквивалентен созданию с нуля.
func TestMergeEnvFile_CreatesWhenMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	var out strings.Builder

	err := mergeEnvFile(path, map[string]string{
		"OPENAI_API_KEY":   "sk-oa",
		"DEEPSEEK_API_KEY": "sk-ds",
	}, &out)
	if err != nil {
		t.Fatalf("mergeEnvFile: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf(".env права = %o, хочу 0600", perm)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("чтение: %v", err)
	}
	want := "DEEPSEEK_API_KEY=sk-ds\nOPENAI_API_KEY=sk-oa\n"
	if string(content) != want {
		t.Errorf(".env содержимое = %q, хочу %q (отсортировано по ключу)", content, want)
	}
}

// TestMergeEnvFile_UpsertsExistingKeyPreservesOthers проверяет ключевое
// свойство merge (проблема из репорта пользователя): существующий .env с
// чужой переменной и комментарием не теряет их, а значение уже присутствующего
// ключа обновляется на месте — без дублирования строки.
func TestMergeEnvFile_UpsertsExistingKeyPreservesOthers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	const initial = "# комментарий, не трогать\nEXISTING_VAR=keep\nDEEPSEEK_API_KEY=oldkey\n"
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatalf("запись стартового .env: %v", err)
	}

	var out strings.Builder
	if err := mergeEnvFile(path, map[string]string{"DEEPSEEK_API_KEY": "newkey"}, &out); err != nil {
		t.Fatalf("mergeEnvFile: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf(".env права = %o, хочу 0600", perm)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("чтение: %v", err)
	}
	got := string(content)
	if !strings.Contains(got, "# комментарий, не трогать") {
		t.Errorf("комментарий потерян: %q", got)
	}
	if !strings.Contains(got, "EXISTING_VAR=keep") {
		t.Errorf("чужая переменная потеряна: %q", got)
	}
	if strings.Count(got, "DEEPSEEK_API_KEY=") != 1 {
		t.Errorf("DEEPSEEK_API_KEY задублирован: %q", got)
	}
	if !strings.Contains(got, "DEEPSEEK_API_KEY=newkey") {
		t.Errorf("DEEPSEEK_API_KEY не обновился на newkey: %q", got)
	}
	if strings.Contains(got, "DEEPSEEK_API_KEY=oldkey") {
		t.Errorf("осталось старое значение DEEPSEEK_API_KEY: %q", got)
	}
}

// TestMergeEnvFile_AddsMissingKeyToExistingFile проверяет, что ключ,
// отсутствующий в существующем .env, дописывается в конец, не трогая
// остальное содержимое.
func TestMergeEnvFile_AddsMissingKeyToExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	const initial = "EXISTING_VAR=keep\n"
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatalf("запись стартового .env: %v", err)
	}

	var out strings.Builder
	if err := mergeEnvFile(path, map[string]string{"DEEPSEEK_API_KEY": "sk-new"}, &out); err != nil {
		t.Fatalf("mergeEnvFile: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("чтение: %v", err)
	}
	got := string(content)
	if !strings.Contains(got, "EXISTING_VAR=keep") {
		t.Errorf("существующая строка потеряна: %q", got)
	}
	if !strings.Contains(got, "DEEPSEEK_API_KEY=sk-new") {
		t.Errorf("новый ключ не дописан: %q", got)
	}
}

// TestRunInit_NonInteractive_DoesNotBlock проверяет, что runInit не
// блокируется на чтении stdin в тестовом окружении (go test не подключает
// stdin к терминалу — isInteractiveStdin() должен вернуть false), беря
// значения из флагов/дефолтов, и без сетевых запросов.
func TestRunInit_NonInteractive_DoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder
	err := runInit([]string{"-project", "myproj", "-dir", dir}, &out)
	if err != nil {
		t.Fatalf("runInit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "agentmemory.toml")); err != nil {
		t.Errorf("agentmemory.toml не создан: %v", err)
	}
}

// TestRunInit_NoInputFlag_ProviderFlags проверяет, что -no-input вместе с
// новыми флагами провайдеров (-embedder-provider/-embedder-model/-embedder-url/
// -reasoner-provider/-openai-key) даёт детерминированный результат без сети.
func TestRunInit_NoInputFlag_ProviderFlags(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder
	err := runInit([]string{
		"-no-input",
		"-project", "myproj",
		"-dir", dir,
		"-embedder-provider", "openai",
		"-embedder-model", "text-embedding-3-small",
		"-embedder-url", "https://openrouter.ai/api",
		"-reasoner-provider", "noop",
		"-openai-key", "sk-oa",
	}, &out)
	if err != nil {
		t.Fatalf("runInit: %v", err)
	}

	cfg, err := config.Load(filepath.Join(dir, "agentmemory.toml"))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Embedder.Provider != "openai" {
		t.Errorf("Embedder.Provider = %q, хочу openai", cfg.Embedder.Provider)
	}
	if cfg.Embedder.BaseURL != "https://openrouter.ai/api" {
		t.Errorf("Embedder.BaseURL = %q", cfg.Embedder.BaseURL)
	}
	if cfg.Embedder.Model != "text-embedding-3-small" {
		t.Errorf("Embedder.Model = %q", cfg.Embedder.Model)
	}
	if cfg.Reasoner.Provider != "noop" {
		t.Errorf("Reasoner.Provider = %q, хочу noop", cfg.Reasoner.Provider)
	}

	envRaw, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatalf("чтение .env: %v", err)
	}
	if strings.TrimSpace(string(envRaw)) != "OPENAI_API_KEY=sk-oa" {
		t.Errorf(".env = %q, хочу только OPENAI_API_KEY (reasoner=noop не пишет DEEPSEEK_API_KEY)", envRaw)
	}
}

// TestRunInit_OllamaURLModelAliases проверяет, что -ollama-url/-ollama-model
// работают как алиасы -embedder-url/-embedder-model (обратная совместимость
// с флагами до задачи 11.1).
func TestRunInit_OllamaURLModelAliases(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder
	err := runInit([]string{
		"-no-input",
		"-project", "myproj",
		"-dir", dir,
		"-ollama-url", "http://custom-ollama:11434",
		"-ollama-model", "custom-model",
	}, &out)
	if err != nil {
		t.Fatalf("runInit: %v", err)
	}

	cfg, err := config.Load(filepath.Join(dir, "agentmemory.toml"))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Embedder.Provider != "ollama" {
		t.Errorf("Embedder.Provider = %q, хочу ollama (дефолт)", cfg.Embedder.Provider)
	}
	if cfg.Embedder.BaseURL != "http://custom-ollama:11434" {
		t.Errorf("Embedder.BaseURL = %q, хочу алиас из -ollama-url", cfg.Embedder.BaseURL)
	}
	if cfg.Embedder.Model != "custom-model" {
		t.Errorf("Embedder.Model = %q, хочу алиас из -ollama-model", cfg.Embedder.Model)
	}
}

// TestApplyInitDefaults_ReasonerModel проверяет, что applyInitDefaults
// проставляет defaultDeepSeekModel для reasoner-provider=deepseek без явной
// модели, и обнуляет ReasonerModel для reasoner-provider=noop (модель ему не
// нужна) — симметрично дефолтам EmbedderModel.
func TestApplyInitDefaults_ReasonerModel(t *testing.T) {
	cfg := applyInitDefaults(initConfig{ReasonerProvider: "deepseek"})
	if cfg.ReasonerModel != defaultDeepSeekModel {
		t.Errorf("ReasonerModel = %q, хочу defaultDeepSeekModel = %q", cfg.ReasonerModel, defaultDeepSeekModel)
	}

	cfg = applyInitDefaults(initConfig{ReasonerProvider: "deepseek", ReasonerModel: "deepseek-reasoner"})
	if cfg.ReasonerModel != "deepseek-reasoner" {
		t.Errorf("ReasonerModel = %q, хочу сохранённое явное значение deepseek-reasoner", cfg.ReasonerModel)
	}

	cfg = applyInitDefaults(initConfig{ReasonerProvider: "noop", ReasonerModel: "deepseek-chat"})
	if cfg.ReasonerModel != "" {
		t.Errorf("ReasonerModel = %q, хочу обнулён (reasoner=noop)", cfg.ReasonerModel)
	}
}

// TestRunInit_ReasonerModelFlag проверяет, что флаг -reasoner-model доходит
// до agentmemory.toml (симметрично TestRunInit_OllamaURLModelAliases для
// эмбеддера).
func TestRunInit_ReasonerModelFlag(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder
	err := runInit([]string{
		"-no-input",
		"-project", "myproj",
		"-dir", dir,
		"-reasoner-provider", "deepseek",
		"-reasoner-model", "deepseek-reasoner",
	}, &out)
	if err != nil {
		t.Fatalf("runInit: %v", err)
	}

	cfg, err := config.Load(filepath.Join(dir, "agentmemory.toml"))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Reasoner.Provider != "deepseek" {
		t.Errorf("Reasoner.Provider = %q, хочу deepseek", cfg.Reasoner.Provider)
	}
	if cfg.Reasoner.Model != "deepseek-reasoner" {
		t.Errorf("Reasoner.Model = %q, хочу deepseek-reasoner (из -reasoner-model)", cfg.Reasoner.Model)
	}
}

// TestRenderReasonerTomlBlock_UsesConfiguredModel проверяет, что
// renderReasonerTomlBlock пишет cfg.ReasonerModel в TOML, а не хардкоженное
// имя модели (регрессия задачи: раньше model была захардкожена как
// "deepseek-chat" независимо от cfg).
func TestRenderReasonerTomlBlock_UsesConfiguredModel(t *testing.T) {
	block := renderReasonerTomlBlock(initConfig{ReasonerProvider: "deepseek", ReasonerModel: "deepseek-reasoner"})
	if !strings.Contains(block, `model    = "deepseek-reasoner"`) {
		t.Errorf("renderReasonerTomlBlock не содержит настроенную модель deepseek-reasoner:\n%s", block)
	}
	if strings.Contains(block, `"deepseek-chat"`) {
		t.Errorf("renderReasonerTomlBlock содержит хардкод deepseek-chat вместо cfg.ReasonerModel:\n%s", block)
	}
}

// TestApplyInitDefaults_ClearsUnselectedProviderKeys проверяет защитную
// нормализацию: ключ провайдера, не выбранного пользователем (рассинхрон
// флагов), обнуляется — .env не должен получить лишний ключ.
func TestApplyInitDefaults_ClearsUnselectedProviderKeys(t *testing.T) {
	cfg := applyInitDefaults(initConfig{
		EmbedderProvider: "noop",
		ReasonerProvider: "noop",
		OpenAIKey:        "sk-oa", // embedder=noop — ключ не нужен
		DeepseekKey:      "sk-ds", // reasoner=noop — ключ не нужен
	})
	if cfg.OpenAIKey != "" {
		t.Errorf("OpenAIKey = %q, хочу обнулён (embedder=noop)", cfg.OpenAIKey)
	}
	if cfg.DeepseekKey != "" {
		t.Errorf("DeepseekKey = %q, хочу обнулён (reasoner=noop)", cfg.DeepseekKey)
	}
}

// TestDoInit_HooksSessionStartWritesSettingsJSON проверяет, что при
// Hooks="session-start" doInit пишет секцию hooks в .claude/settings.json с
// командой memo hook -event session-start (задача 7.2), и что повторный
// init не дублирует запись (идемпотентность).
func TestDoInit_HooksSessionStartWritesSettingsJSON(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder
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
	var out2 strings.Builder
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
	var out strings.Builder
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
	var out strings.Builder
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

	var out strings.Builder
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
		t.Errorf("новая matcher-запись memo hook не добавлена: %v", sessionStart)
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

	var out strings.Builder
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
	var out strings.Builder
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

// TestDoInit_UnknownProviderIsError — регрессия БАГ 1 цикла исправлений:
// опечатка в -embedder-provider/-reasoner-provider раньше молча уходила в
// default-ветку switch renderEmbedderTomlBlock/renderReasonerTomlBlock (напр.
// "ollama"/"deepseek"), которая расходилась с default-веткой
// applyInitDefaults — TOML заявлял провайдера, для которого фактически не
// было ни base_url/model, ни .env-ключа. Теперь неизвестное значение — ошибка
// doInit (exit 1 у runInit/runMain), а не тихий битый конфиг.
func TestDoInit_UnknownProviderIsError(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder
	err := doInit(initConfig{Project: "myproj", Dir: dir, EmbedderProvider: "olama"}, &out)
	if err == nil {
		t.Fatalf("doInit с опечаткой в embedder-provider = nil error, хочу ошибку")
	}
	if !strings.Contains(err.Error(), "olama") {
		t.Errorf("ошибка не упоминает переданное значение: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "agentmemory.toml")); !os.IsNotExist(statErr) {
		t.Errorf("agentmemory.toml создан несмотря на ошибку валидации: err=%v", statErr)
	}

	dir2 := t.TempDir()
	var out2 strings.Builder
	err2 := doInit(initConfig{Project: "myproj", Dir: dir2, ReasonerProvider: "deepsek"}, &out2)
	if err2 == nil {
		t.Fatalf("doInit с опечаткой в reasoner-provider = nil error, хочу ошибку")
	}
	if !strings.Contains(err2.Error(), "deepsek") {
		t.Errorf("ошибка не упоминает переданное значение: %v", err2)
	}
}

// TestRunInit_UnknownProviderFlag_ExitsWithError проверяет тот же случай
// через полный флаговый путь runInit (-no-input), как реально вызывает
// пользователь: опечатка в -embedder-provider/-reasoner-provider — ошибка, а
// не тихий дефолт.
func TestRunInit_UnknownProviderFlag_ExitsWithError(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder
	err := runInit([]string{"-no-input", "-project", "myproj", "-dir", dir, "-embedder-provider", "bogus"}, &out)
	if err == nil {
		t.Fatalf("runInit с -embedder-provider bogus = nil error, хочу ошибку")
	}

	dir2 := t.TempDir()
	var out2 strings.Builder
	err2 := runInit([]string{"-no-input", "-project", "myproj", "-dir", dir2, "-reasoner-provider", "bogus"}, &out2)
	if err2 == nil {
		t.Fatalf("runInit с -reasoner-provider bogus = nil error, хочу ошибку")
	}
}

// TestRunInit_OpenAIWithoutModel_DefaultsToNonEmptyModel — регрессия БАГ 1
// цикла исправлений: без -embedder-model в -no-input applyInitDefaults
// дефолтил только base_url для embedder-provider=openai, model оставался ""
// -> agentmemory.toml с неюзабельной пустой model. Теперь дефолт —
// defaultOpenAIModel (text-embedding-3-small, как в config.example.toml).
func TestRunInit_OpenAIWithoutModel_DefaultsToNonEmptyModel(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder
	err := runInit([]string{
		"-no-input",
		"-project", "myproj",
		"-dir", dir,
		"-embedder-provider", "openai",
		"-openai-key", "sk-x",
	}, &out)
	if err != nil {
		t.Fatalf("runInit: %v", err)
	}

	cfg, err := config.Load(filepath.Join(dir, "agentmemory.toml"))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Embedder.Model == "" {
		t.Errorf("Embedder.Model пуст — хочу непустой дефолт (defaultOpenAIModel)")
	}
	if cfg.Embedder.Model != defaultOpenAIModel {
		t.Errorf("Embedder.Model = %q, хочу defaultOpenAIModel = %q", cfg.Embedder.Model, defaultOpenAIModel)
	}
}

// TestProviderRegistries_OptionsAndEnvKeysMatchExpected — тест реестра
// провайдеров (консолидация БАГ 1): список опций (в порядке select-промпта,
// первый — дефолт) и env-ключи деривируются из embedderProviders/
// reasonerProviders единообразно — регрессия на случай, если кто-то добавит
// провайдера в один реестр и забудет про другой потребляющий его код.
func TestProviderRegistries_OptionsAndEnvKeysMatchExpected(t *testing.T) {
	wantEmbedderOptions := []string{"ollama", "openai", "noop"}
	if strings.Join(embedderProviderOptions, ",") != strings.Join(wantEmbedderOptions, ",") {
		t.Errorf("embedderProviderOptions = %v, хочу %v", embedderProviderOptions, wantEmbedderOptions)
	}
	wantReasonerOptions := []string{"deepseek", "noop"}
	if strings.Join(reasonerProviderOptions, ",") != strings.Join(wantReasonerOptions, ",") {
		t.Errorf("reasonerProviderOptions = %v, хочу %v", reasonerProviderOptions, wantReasonerOptions)
	}

	wantEnvKeys := map[string]string{ // provider key -> env var (пусто, если ключ не нужен)
		"ollama": "",
		"openai": openaiAPIKeyEnv,
		"noop":   "",
	}
	for key, wantEnv := range wantEnvKeys {
		p, ok := providerByKey(embedderProviders, key)
		if !ok {
			t.Fatalf("embedderProviders не содержит %q", key)
		}
		if p.envKey != wantEnv {
			t.Errorf("embedderProviders[%q].envKey = %q, хочу %q", key, p.envKey, wantEnv)
		}
	}

	wantReasonerEnvKeys := map[string]string{
		"deepseek": deepseekAPIKeyEnv,
		"noop":     "",
	}
	for key, wantEnv := range wantReasonerEnvKeys {
		p, ok := providerByKey(reasonerProviders, key)
		if !ok {
			t.Fatalf("reasonerProviders не содержит %q", key)
		}
		if p.envKey != wantEnv {
			t.Errorf("reasonerProviders[%q].envKey = %q, хочу %q", key, p.envKey, wantEnv)
		}
	}

	// selectedProviders — общий источник для .env/.mcp.json/opencode.json:
	// проверяем, что для типичной комбинации возвращает обе выбранные записи в
	// порядке embedder, reasoner.
	sel := selectedProviders(initConfig{EmbedderProvider: "openai", ReasonerProvider: "deepseek"})
	if len(sel) != 2 || sel[0].key != "openai" || sel[1].key != "deepseek" {
		t.Errorf("selectedProviders(openai, deepseek) = %+v, хочу [openai, deepseek] в этом порядке", sel)
	}
	if sel[0].envKey != openaiAPIKeyEnv || sel[1].envKey != deepseekAPIKeyEnv {
		t.Errorf("selectedProviders(openai, deepseek) envKey = [%q, %q], хочу [%q, %q]", sel[0].envKey, sel[1].envKey, openaiAPIKeyEnv, deepseekAPIKeyEnv)
	}

	// noop-провайдеры не требуют ключа вовсе — selectedProviders их всё равно
	// находит (для полноты), но envKey у обоих пуст.
	selNoop := selectedProviders(initConfig{EmbedderProvider: "noop", ReasonerProvider: "noop"})
	if len(selNoop) != 2 || selNoop[0].envKey != "" || selNoop[1].envKey != "" {
		t.Errorf("selectedProviders(noop, noop) = %+v, хочу обе записи с пустым envKey", selNoop)
	}
}
