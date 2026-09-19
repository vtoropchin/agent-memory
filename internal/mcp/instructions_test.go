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

// TestBuildInstructions_MultiprojectDefaultSharedUnchanged фиксирует нулевую
// регрессию текста для дефолтного конфига (shared = ["user:*"]): совет про save
// общих фактов должен получиться байт-в-байт прежним.
func TestBuildInstructions_MultiprojectDefaultSharedUnchanged(t *testing.T) {
	scope := config.ScopeConfig{Project: "agentmemory", Shared: []string{"user:*"}}

	got := buildInstructions(scope)

	if !strings.Contains(got, "Общие для всех проектов факты сохраняй явно в scope \"user:*\".") {
		t.Errorf("дефолтный вывод совета про общий scope изменился:\n%s", got)
	}
}

// TestBuildInstructions_MultiprojectCustomShared доказывает, что совет про save
// выводится из scope.Shared, а не из захардкоженного "user:*".
func TestBuildInstructions_MultiprojectCustomShared(t *testing.T) {
	scope := config.ScopeConfig{Project: "agentmemory", Shared: []string{"team:backend"}}

	got := buildInstructions(scope)

	if !strings.Contains(got, "\"team:backend\"") {
		t.Errorf("совет про общий scope не содержит кастомный Shared \"team:backend\":\n%s", got)
	}
	if strings.Contains(got, "\"user:*\"") {
		t.Errorf("совет про общий scope не должен содержать захардкоженный \"user:*\":\n%s", got)
	}
}

// TestBuildInstructions_MultiprojectMultipleShared проверяет, что при нескольких
// общих scope перечислены все значения из Shared.
func TestBuildInstructions_MultiprojectMultipleShared(t *testing.T) {
	scope := config.ScopeConfig{Project: "agentmemory", Shared: []string{"user:*", "team:backend"}}

	got := buildInstructions(scope)

	for _, want := range []string{"\"user:*\"", "\"team:backend\""} {
		if !strings.Contains(got, want) {
			t.Errorf("совет про общие scope не содержит %q:\n%s", want, got)
		}
	}
}

// TestBuildInstructions_MultiprojectEmptyShared: при заданном Project и пустом
// Shared у инстанса нет общего слоя — строки-совета про общий scope быть не
// должно, но save/recall-строки присутствуют и функция не паникует.
func TestBuildInstructions_MultiprojectEmptyShared(t *testing.T) {
	scope := config.ScopeConfig{Project: "agentmemory", Shared: nil}

	got := buildInstructions(scope)

	if strings.Contains(got, "Общие") {
		t.Errorf("при пустом Shared не должно быть совета про общий scope:\n%s", got)
	}
	for _, want := range []string{"memory_save без scope сохраняет", "memory_recall без scope/scopes ищет"} {
		if !strings.Contains(got, want) {
			t.Errorf("отсутствует ожидаемая строка %q:\n%s", want, got)
		}
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

// TestBuildInstructions_TaskConventions фиксирует конвенции Этапа 15 в
// серверных инструкциях — главном канале конвенций для любого MCP-клиента:
// все семь стадий доски, типы задач с направлением part_of «дочь → родитель»,
// branch/workflow при переводе в doing, примеры recall по statuses/types.
// Упразднённого статуса blocked быть не должно.
func TestBuildInstructions_TaskConventions(t *testing.T) {
	got := buildInstructions(config.ScopeConfig{Project: "agentmemory", Shared: []string{"user:*"}})

	for _, want := range []string{
		"backlog", "todo", "doing", "review", "testing", "release", "done",
		"epic", "story", "part_of", "дочерней", "родительской",
		"branch", "workflow",
		`statuses:["todo","doing"]`, `types:["epic"]`, `statuses:["done"]`,
		`"rel":"blocked_by"`, "unlinks", "дальше todo", "⛔",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("инструкции не содержат %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{`"blocked"`, "status = todo|doing|blocked", "ждёшь чего-то — blocked"} {
		if strings.Contains(got, bad) {
			t.Errorf("инструкции содержат упразднённый статус blocked (%q):\n%s", bad, got)
		}
	}
}
