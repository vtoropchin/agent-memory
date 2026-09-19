package main

import (
	"strings"
	"testing"
)

// TestRenderSkillMD_TaskConventions: генерируемый `memo init` скил memo несёт
// конвенции Этапа 15 (стадии доски, type + part_of «дочь → родитель»,
// branch/workflow при взятии в работу, примеры recall по statuses/types) и не
// советует упразднённую стадию blocked. Скил — второй (после серверных
// инструкций) канал конвенций для агента, расхождение между ними — дефект.
func TestRenderSkillMD_TaskConventions(t *testing.T) {
	got := renderSkillMD("alpha")

	if !strings.HasPrefix(got, "---\nname: memo\n") {
		t.Fatalf("SKILL.md должен начинаться с YAML-фронтматтера name: memo:\n%.80s", got)
	}
	for _, want := range []string{
		"## Задачи (kanban-доска)",
		"`backlog`", "`todo`", "`doing`", "`review`", "`testing`", "`release`", "`done`",
		"`epic`", "`story`", "`task`", "`rel: part_of`", "**от дочерней к родительской**",
		"**Блокировка**", `rel: "blocked_by"`, "unlinks", "дальше `todo`", "⛔",
		"`branch`", "`workflow`",
		`memory_recall(statuses:["todo","doing"])`, `memory_recall(types:["epic"])`, `memory_recall(statuses:["done"])`,
		"project:alpha",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("скил memo не содержит %q", want)
		}
	}
	for _, bad := range []string{`"blocked"`, "`todo`/`doing`/`blocked`/`done`"} {
		if strings.Contains(got, bad) {
			t.Errorf("скил memo содержит упразднённую стадию blocked: %q", bad)
		}
	}
}

// TestRenderBootstrapSkillMD_TaskConventions: разовый bootstrap-скил
// перечисляет стадии и типы задач без blocked и с направлением part_of.
func TestRenderBootstrapSkillMD_TaskConventions(t *testing.T) {
	got := renderBootstrapSkillMD("alpha")

	if !strings.HasPrefix(got, "---\nname: memo-bootstrap\n") {
		t.Fatalf("SKILL.md должен начинаться с YAML-фронтматтера name: memo-bootstrap:\n%.80s", got)
	}
	for _, want := range []string{
		"backlog/todo/doing/review/testing/release/done",
		"epic/story/task",
		"`rel: part_of`", "от дочерней к родительской",
		"`rel: blocked_by`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("bootstrap-скил не содержит %q", want)
		}
	}
	for _, bad := range []string{"`blocked`", "/blocked/", `"blocked"`} {
		if strings.Contains(got, bad) {
			t.Errorf("bootstrap-скил содержит упразднённую стадию blocked: %q", bad)
		}
	}
}
