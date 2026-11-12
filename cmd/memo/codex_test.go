package main

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestScaffoldCodexPreservesProjectConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "project with spaces")
	path := filepath.Join(dir, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	const existing = "# project settings\n[mcp_servers.other]\ncommand = 'other' # keep\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"/path/to/memo", `/new "quoted" path/memo`, `/new "quoted" path/memo`} {
		cfg := initConfig{ReasonerProvider: "deepseek", StoreDriver: storeDriverPostgres}
		if _, err := scaffoldCodex(dir, "myproj", command, cfg, io.Discard); err != nil {
			t.Fatal(err)
		}
		var config struct {
			Servers map[string]struct {
				Command string
				Args    []string
				Cwd     string
				EnvVars []string `toml:"env_vars"`
			} `toml:"mcp_servers"`
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(content), existing) || strings.Count(string(content), codexMemoStart) != 1 {
			t.Fatalf("unexpected project config:\n%s", content)
		}
		if _, err := toml.Decode(string(content), &config); err != nil {
			t.Fatal(err)
		}
		memo := config.Servers["memo"]
		if memo.Command != command || memo.Cwd != dir ||
			!reflect.DeepEqual(memo.Args, []string{"-config", "agentmemory.toml"}) ||
			!reflect.DeepEqual(memo.EnvVars, []string{deepseekAPIKeyEnv, memoPostgresPasswordEnv}) {
			t.Fatalf("unexpected memo configuration: %+v", memo)
		}
	}
}

func TestScaffoldCodexRejectsExistingConfigConflict(t *testing.T) {
	for _, existing := range []string{
		"[mcp_servers.memo]\nurl = 'https://example.com/mcp'\n",
		"mcp_servers = { memo = { command = 'custom-memo' } }\n",
		"[invalid\n",
	} {
		if _, err := mergeCodexMemoConfig(existing, ""); err == nil {
			t.Errorf("expected an error for %q", existing)
		}
	}
}
