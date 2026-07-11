package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadDotEnv_MissingFile проверяет, что отсутствие .env — тихий no-op:
// loaded=false, ошибки нет.
func TestLoadDotEnv_MissingFile(t *testing.T) {
	loaded, err := loadDotEnv(filepath.Join(t.TempDir(), "nope.env"))
	if err != nil {
		t.Fatalf("loadDotEnv: %v", err)
	}
	if loaded {
		t.Errorf("loaded = true для отсутствующего файла")
	}
}

// TestLoadDotEnv_ParsesCommentsAndQuotes проверяет разбор: пустые строки и
// #-комментарии игнорируются, кавычки (одинарные/двойные) снимаются, пробелы
// вокруг ключа/значения тримятся.
func TestLoadDotEnv_ParsesCommentsAndQuotes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "" +
		"# комментарий\n" +
		"\n" +
		"  DEEPSEEK_API_KEY = \"sk-double-quoted\" \n" +
		"OTHER='single-quoted'\n" +
		"PLAIN=noquotes\n" +
		"malformed-line-without-equals\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("запись .env: %v", err)
	}

	for _, k := range []string{"DEEPSEEK_API_KEY", "OTHER", "PLAIN"} {
		os.Unsetenv(k)
		t.Cleanup(func(k string) func() { return func() { os.Unsetenv(k) } }(k))
	}

	loaded, err := loadDotEnv(path)
	if err != nil {
		t.Fatalf("loadDotEnv: %v", err)
	}
	if !loaded {
		t.Errorf("loaded = false, хочу true")
	}
	if got := os.Getenv("DEEPSEEK_API_KEY"); got != "sk-double-quoted" {
		t.Errorf("DEEPSEEK_API_KEY = %q, хочу sk-double-quoted", got)
	}
	if got := os.Getenv("OTHER"); got != "single-quoted" {
		t.Errorf("OTHER = %q, хочу single-quoted", got)
	}
	if got := os.Getenv("PLAIN"); got != "noquotes" {
		t.Errorf("PLAIN = %q, хочу noquotes", got)
	}
}

// TestLoadDotEnv_ShellEnvTakesPriority проверяет, что уже установленная в
// окружении переменная не перезатирается значением из .env.
func TestLoadDotEnv_ShellEnvTakesPriority(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("DEEPSEEK_API_KEY=from-dotenv\n"), 0o600); err != nil {
		t.Fatalf("запись .env: %v", err)
	}

	t.Setenv("DEEPSEEK_API_KEY", "from-shell")

	if _, err := loadDotEnv(path); err != nil {
		t.Fatalf("loadDotEnv: %v", err)
	}
	if got := os.Getenv("DEEPSEEK_API_KEY"); got != "from-shell" {
		t.Errorf("DEEPSEEK_API_KEY = %q, хочу from-shell (shell-env приоритетнее)", got)
	}
}

// TestLoadDotEnv_EmptyEnvOverriddenByDotEnv — регресс-тест на боевой путь
// через Claude Code: сгенерированный .mcp.json инжектит переменную вида
// ${DEEPSEEK_API_KEY:-}, из-за чего memoryd получает переменную окружения,
// которая ЗАДАНА, но пуста (os.LookupEnv вернёт exists=true). Такое пустое
// значение не должно считаться приоритетнее .env — иначе валидный ключ из
// .env игнорируется и Reasoner деградирует в Noop.
func TestLoadDotEnv_EmptyEnvOverriddenByDotEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("DEEPSEEK_API_KEY=from-dotenv\n"), 0o600); err != nil {
		t.Fatalf("запись .env: %v", err)
	}

	t.Setenv("DEEPSEEK_API_KEY", "")

	if _, err := loadDotEnv(path); err != nil {
		t.Fatalf("loadDotEnv: %v", err)
	}
	if got := os.Getenv("DEEPSEEK_API_KEY"); got != "from-dotenv" {
		t.Errorf("DEEPSEEK_API_KEY = %q, хочу from-dotenv (пустой shell-env перекрывается .env)", got)
	}
}
