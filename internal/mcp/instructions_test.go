package mcp

import (
	"strings"
	"testing"

	"agentmemory/internal/config"
)

func TestBuildInstructions_Multiproject(t *testing.T) {
	scope := config.ScopeConfig{Project: "agentmemory", Shared: []string{"user:*"}}

	got := buildInstructions(scope)

	if got == "" {
		t.Fatal("buildInstructions вернул пустую строку")
	}
	for _, want := range []string{"project:agentmemory", "user:*", "атомарный факт", "recall"} {
		if !strings.Contains(got, want) {
			t.Errorf("buildInstructions не содержит %q:\n%s", want, got)
		}
	}
}

func TestBuildInstructions_Adhoc(t *testing.T) {
	scope := config.ScopeConfig{Project: ""}

	got := buildInstructions(scope)

	if got == "" {
		t.Fatal("buildInstructions вернул пустую строку")
	}
	if strings.Contains(got, "project:") {
		t.Errorf("adhoc-режим не должен упоминать дефолт \"project:\":\n%s", got)
	}
	if !strings.Contains(got, "явный scope") {
		t.Errorf("adhoc-режим должен требовать явный scope у save:\n%s", got)
	}
	if !strings.Contains(got, "без фильтра") {
		t.Errorf("adhoc-режим должен упоминать recall без фильтра:\n%s", got)
	}
}

// TestBuildInstructions_AdhocEmptyShared проверяет, что пустой Shared не паникует.
func TestBuildInstructions_AdhocEmptyShared(t *testing.T) {
	scope := config.ScopeConfig{Project: "", Shared: nil}

	if got := buildInstructions(scope); got == "" {
		t.Fatal("buildInstructions вернул пустую строку")
	}
}

// TestNewServer_PublishesInstructions — smoke: NewServer с настроенным scope не
// паникует и конструируется независимо от Run (DESIGN.md §12).
func TestNewServer_PublishesInstructions(t *testing.T) {
	scope := config.ScopeConfig{Project: "agentmemory", Shared: []string{"user:*"}}
	srv := newTestServerWithScope(t, scope)

	if srv == nil {
		t.Fatal("NewServer вернул nil")
	}
}
