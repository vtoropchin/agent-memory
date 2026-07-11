package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
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

func TestMaintConfigDurationsValid(t *testing.T) {
	m := MaintConfig{DecayInterval: "24h", ConsolidateAfter: "168h"}
	decay, consolidate, err := m.Durations()
	if err != nil {
		t.Fatalf("Durations: %v", err)
	}
	if decay != 24*time.Hour {
		t.Errorf("decayInterval = %v, want 24h", decay)
	}
	if consolidate != 168*time.Hour {
		t.Errorf("consolidateAfter = %v, want 168h", consolidate)
	}
}

func TestMaintConfigDurationsInvalidDecayInterval(t *testing.T) {
	m := MaintConfig{DecayInterval: "не-длительность", ConsolidateAfter: "168h"}
	if _, _, err := m.Durations(); err == nil {
		t.Errorf("Durations() err = nil, want ошибку для невалидного decay_interval")
	}
}

func TestMaintConfigDurationsInvalidConsolidateAfter(t *testing.T) {
	m := MaintConfig{DecayInterval: "24h", ConsolidateAfter: "не-длительность"}
	if _, _, err := m.Durations(); err == nil {
		t.Errorf("Durations() err = nil, want ошибку для невалидного consolidate_after")
	}
}

// TestMaintConfigDurationsNonPositiveRejected — time.ParseDuration успешно
// разбирает и "0s", и отрицательные значения вроде "-1h" без ошибки; Durations()
// обязан отвергать такие значения отдельно, иначе непозитивный decay_interval
// доходит до time.NewTicker в Scheduler.Run и роняет процесс паникой.
func TestMaintConfigDurationsNonPositiveRejected(t *testing.T) {
	cases := []struct {
		name string
		m    MaintConfig
	}{
		{"decay_interval = 0s", MaintConfig{DecayInterval: "0s", ConsolidateAfter: "168h"}},
		{"decay_interval = -1h", MaintConfig{DecayInterval: "-1h", ConsolidateAfter: "168h"}},
		{"consolidate_after = 0s", MaintConfig{DecayInterval: "24h", ConsolidateAfter: "0s"}},
		{"consolidate_after = -1h", MaintConfig{DecayInterval: "24h", ConsolidateAfter: "-1h"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := tc.m.Durations(); err == nil {
				t.Errorf("Durations() err = nil, want ошибку для непозитивной длительности")
			}
		})
	}
}

// TestLoadRejectsNonPositiveMaintDuration — тот же случай на уровне Load: TOML с
// decay_interval = "0s" должен фейлить загрузку конфига, а не проходить как
// валидный (что уронило бы memoryd на старте через time.NewTicker).
func TestLoadRejectsNonPositiveMaintDuration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	toml := `
[maint]
decay_interval = "0s"
`
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Errorf("Load() err = nil, want ошибку из-за непозитивного maint.decay_interval")
	}
}

func TestDefaultMaintDurationsAreValid(t *testing.T) {
	if _, _, err := Default().Maint.Durations(); err != nil {
		t.Errorf("Default().Maint.Durations(): %v, дефолты должны быть валидны", err)
	}
}

func TestLoadRejectsInvalidMaintDuration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	toml := `
[maint]
decay_interval = "не-длительность"
`
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Errorf("Load() err = nil, want ошибку из-за невалидного maint.decay_interval")
	}
}

func TestLoadAcceptsValidMaintDuration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	toml := `
[maint]
decay_interval = "1h"
consolidate_after = "72h"
`
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	decay, consolidate, err := cfg.Maint.Durations()
	if err != nil {
		t.Fatalf("Durations: %v", err)
	}
	if decay != time.Hour {
		t.Errorf("decayInterval = %v, want 1h", decay)
	}
	if consolidate != 72*time.Hour {
		t.Errorf("consolidateAfter = %v, want 72h", consolidate)
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
