package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestParseAgentList проверяет разбор значения флага -agent: один ключ,
// список через запятую, пробелы, пустые элементы и повторы.
func TestParseAgentList(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"claude", []string{"claude"}},
		{"claude,opencode", []string{"claude", "opencode"}},
		{" opencode , dsh ,", []string{"opencode", "dsh"}},
		{"dsh,claude,dsh", []string{"dsh", "claude"}},
		{",,", nil},
	}
	for _, c := range cases {
		if got := parseAgentList(c.in); !slices.Equal(got, c.want) {
			t.Errorf("parseAgentList(%q) = %q, хочу %q", c.in, got, c.want)
		}
	}
}

// TestDoInit_MultipleAgents проверяет, что при нескольких выбранных агентах
// каждый получает свой MCP-конфиг и свой набор скиллов, а сводка перечисляет
// всех.
func TestDoInit_MultipleAgents(t *testing.T) {
	t.Setenv("DSH_HOME", t.TempDir()) // init читает patch-слои dsh — не настоящий ~/.dsh
	dir := t.TempDir()
	var out strings.Builder
	cfg := initConfig{Project: "myproj", Dir: dir, Agents: []string{"claude", "opencode", "dsh"}, Hooks: hooksSessionStart}
	if err := doInit(cfg, &out); err != nil {
		t.Fatalf("doInit: %v", err)
	}

	for _, rel := range []string{
		".mcp.json",
		filepath.Join(".claude", "settings.json"),
		filepath.Join(".claude", "skills", "memo", "SKILL.md"),
		"opencode.json",
		filepath.Join(".opencode", "skills", "memo-bootstrap", "SKILL.md"),
		filepath.Join(".dsh", "memo.cordis.yml"),
		filepath.Join(".dsh", dshHooksName),
		filepath.Join(".agents", "skills", "memo", "SKILL.md"),
		"agentmemory.toml",
	} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("%s не создан: %v", rel, err)
		}
	}
	if !strings.Contains(out.String(), "агенты: Claude Code, opencode, DeepSeek Harness") {
		t.Errorf("сводка не перечисляет всех агентов:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "одобри проектный MCP-сервер") {
		t.Errorf("потеряна подсказка Claude Code при нескольких агентах:\n%s", out.String())
	}
}

// TestDoInit_UnknownAgentInList проверяет, что опечатка в любом агенте списка
// отклоняется до записи файлов — проект не остаётся настроенным наполовину.
func TestDoInit_UnknownAgentInList(t *testing.T) {
	dir := t.TempDir()
	err := doInit(initConfig{Project: "myproj", Dir: dir, Agents: []string{"claude", "opencod"}}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "opencod") {
		t.Fatalf("doInit с опечаткой во втором агенте: err = %v, хочу ошибку с её упоминанием", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("после ошибки в каталоге остались файлы: %v", entries)
	}
}

// TestRunInit_AgentListFlag проверяет, что -agent принимает список через
// запятую в неинтерактивном прогоне.
func TestRunInit_AgentListFlag(t *testing.T) {
	dir := t.TempDir()
	args := []string{"-dir", dir, "-project", "myproj", "-agent", "claude,opencode", "-no-input", "-reasoner-provider", "noop", "-embedder-provider", "noop"}
	if err := runInit(args, &strings.Builder{}); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	for _, rel := range []string{".mcp.json", "opencode.json"} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("%s не создан: %v", rel, err)
		}
	}
}

// TestHooksLabels проверяет, что вопрос про хуки собирается только из
// приложений, которые их умеют.
func TestHooksLabels(t *testing.T) {
	if got := hooksLabels([]string{"opencode"}); len(got) != 0 {
		t.Errorf("hooksLabels(opencode) = %q, хочу пусто", got)
	}
	got := hooksLabels([]string{"claude", "opencode", "dsh"})
	want := []string{agentRegistry["claude"].hooksLabel, agentRegistry["dsh"].hooksLabel}
	if !slices.Equal(got, want) {
		t.Errorf("hooksLabels = %q, хочу %q", got, want)
	}
}
