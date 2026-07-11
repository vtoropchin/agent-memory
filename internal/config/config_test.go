package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestScopeConfigSaveScope(t *testing.T) {
	t.Run("project задан", func(t *testing.T) {
		c := ScopeConfig{Project: "myproject"}
		if got := c.SaveScope(); got != "project:myproject" {
			t.Errorf("SaveScope() = %q, want %q", got, "project:myproject")
		}
	})

	t.Run("project пуст", func(t *testing.T) {
		c := ScopeConfig{Project: ""}
		if got := c.SaveScope(); got != "" {
			t.Errorf("SaveScope() = %q, want пусто", got)
		}
	})
}

func TestScopeConfigRecallScopes(t *testing.T) {
	t.Run("project задан", func(t *testing.T) {
		c := ScopeConfig{Project: "myproject", Shared: []string{"user:*"}}
		want := []string{"project:myproject", "user:*"}
		if got := c.RecallScopes(); !reflect.DeepEqual(got, want) {
			t.Errorf("RecallScopes() = %v, want %v", got, want)
		}
	})

	t.Run("project пуст", func(t *testing.T) {
		c := ScopeConfig{Project: "", Shared: []string{"user:*"}}
		if got := c.RecallScopes(); got != nil {
			t.Errorf("RecallScopes() = %v, want nil", got)
		}
	})
}

func TestDefaultScope(t *testing.T) {
	cfg := Default()
	if cfg.Scope.Project != "" {
		t.Errorf("Default().Scope.Project = %q, want пусто", cfg.Scope.Project)
	}
	want := []string{"user:*"}
	if !reflect.DeepEqual(cfg.Scope.Shared, want) {
		t.Errorf("Default().Scope.Shared = %v, want %v", cfg.Scope.Shared, want)
	}
}

func TestLoadScopeFromTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	toml := `
[scope]
project = "agentmemory"
shared = ["user:*", "team:core"]
`
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Scope.Project != "agentmemory" {
		t.Errorf("Scope.Project = %q, want %q", cfg.Scope.Project, "agentmemory")
	}
	want := []string{"user:*", "team:core"}
	if !reflect.DeepEqual(cfg.Scope.Shared, want) {
		t.Errorf("Scope.Shared = %v, want %v (должен полностью перезаписать дефолт)", cfg.Scope.Shared, want)
	}
	// Остальные секции остаются дефолтными — [scope] не влияет на них.
	if cfg.Store.Path != Default().Store.Path {
		t.Errorf("Store.Path изменился при загрузке только [scope]: %q", cfg.Store.Path)
	}
}

func TestLoadScopeProjectOnlyKeepsDefaultShared(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	toml := `
[scope]
project = "agentmemory"
`
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Scope.Project != "agentmemory" {
		t.Errorf("Scope.Project = %q, want %q", cfg.Scope.Project, "agentmemory")
	}
	want := []string{"user:*"}
	if !reflect.DeepEqual(cfg.Scope.Shared, want) {
		t.Errorf("Scope.Shared = %v, want дефолт %v (shared не указан в TOML)", cfg.Scope.Shared, want)
	}
}
