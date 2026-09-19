package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"agentmemory/internal/memory"
)

// TestTaskSchemaAcceptsKindAndStatus: схемы инструментов принимают task/status/
// statuses — без них агент не смог бы завести задачу через строгий валидатор.
func TestTaskSchemaAcceptsKindAndStatus(t *testing.T) {
	saveResolved, err := saveInputSchema().Resolve(nil)
	if err != nil {
		t.Fatalf("saveInputSchema().Resolve: %v", err)
	}
	var saveArgs any
	if err := json.Unmarshal([]byte(`{"kind":"task","content":"текст задачи","scope":"s","summary":"Заголовок","status":"doing"}`), &saveArgs); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if err := saveResolved.Validate(saveArgs); err != nil {
		t.Errorf("memory_save Validate(kind=task, status=doing): %v, want nil", err)
	}

	var badStatus any
	if err := json.Unmarshal([]byte(`{"kind":"task","content":"c","scope":"s","summary":"h","status":"in-progress"}`), &badStatus); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if err := saveResolved.Validate(badStatus); err == nil {
		t.Errorf("memory_save Validate(status=in-progress) = nil, want ошибку (значения вне memory.Statuses)")
	}

	recallResolved, err := recallInputSchema().Resolve(nil)
	if err != nil {
		t.Fatalf("recallInputSchema().Resolve: %v", err)
	}
	var recallArgs any
	if err := json.Unmarshal([]byte(`{"query":"q","kinds":["task"],"statuses":["todo","doing"]}`), &recallArgs); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if err := recallResolved.Validate(recallArgs); err != nil {
		t.Errorf("memory_recall Validate(kinds=[task], statuses=[todo,doing]): %v, want nil", err)
	}

	updateResolved, err := updateInputSchema().Resolve(nil)
	if err != nil {
		t.Fatalf("updateInputSchema().Resolve: %v", err)
	}
	var updateArgs any
	if err := json.Unmarshal([]byte(`{"id":"01J8Z000000000000000000A","status":"done"}`), &updateArgs); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if err := updateResolved.Validate(updateArgs); err != nil {
		t.Errorf("memory_update Validate(status=done): %v, want nil", err)
	}
}

// TestTaskSchemaStage15Fields: схемы принимают type/branch/workflow (save и
// update) и types (recall), отвергают упразднённый status "blocked" и type
// вне перечня memory.TaskTypes — строгий валидатор SDK ловит опечатку до
// обработчика.
func TestTaskSchemaStage15Fields(t *testing.T) {
	validate := func(t *testing.T, name string, s *jsonschema.Schema, raw string) error {
		t.Helper()
		resolved, err := s.Resolve(nil)
		if err != nil {
			t.Fatalf("%s: Resolve: %v", name, err)
		}
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatalf("%s: json.Unmarshal(%s): %v", name, raw, err)
		}
		return resolved.Validate(v)
	}

	saveSchema, updateSchema, recallSchema := saveInputSchema(), updateInputSchema(), recallInputSchema()

	okCases := []struct {
		name string
		s    *jsonschema.Schema
		raw  string
	}{
		{"save epic", saveSchema, `{"kind":"task","content":"c","scope":"s","summary":"h","type":"epic"}`},
		{"save с branch/workflow", saveSchema, `{"kind":"task","content":"c","scope":"s","summary":"h","status":"doing","branch":"feat/x","workflow":"dev"}`},
		{"save status review", saveSchema, `{"kind":"task","content":"c","scope":"s","summary":"h","status":"review"}`},
		{"update type/branch/workflow", updateSchema, `{"id":"01J8Z000000000000000000A","type":"story","branch":"","workflow":"ultracode"}`},
		{"recall types", recallSchema, `{"query":"q","types":["epic","story"]}`},
		{"recall statuses без blocked", recallSchema, `{"query":"q","statuses":["backlog","todo","doing","review","testing","release","done"]}`},
	}
	for _, c := range okCases {
		if err := validate(t, c.name, c.s, c.raw); err != nil {
			t.Errorf("%s: Validate(%s) = %v, want nil", c.name, c.raw, err)
		}
	}

	badCases := []struct {
		name string
		s    *jsonschema.Schema
		raw  string
	}{
		{"save status blocked", saveSchema, `{"kind":"task","content":"c","scope":"s","summary":"h","status":"blocked"}`},
		{"save type bug", saveSchema, `{"kind":"task","content":"c","scope":"s","summary":"h","type":"bug"}`},
		{"update status blocked", updateSchema, `{"id":"01J8Z000000000000000000A","status":"blocked"}`},
		{"update type bug", updateSchema, `{"id":"01J8Z000000000000000000A","type":"bug"}`},
		{"recall statuses blocked", recallSchema, `{"query":"q","statuses":["blocked"]}`},
		{"recall types bug", recallSchema, `{"query":"q","types":["bug"]}`},
	}
	for _, c := range badCases {
		if err := validate(t, c.name, c.s, c.raw); err == nil {
			t.Errorf("%s: Validate(%s) = nil, want ошибку", c.name, c.raw)
		}
	}
}

// TestTaskStage15Cycle — полный цикл Этапа 15 через инструменты: save эпика
// (type=epic) → save истории с part_of → recall types=[epic] показывает только
// эпик → update истории (doing + branch + workflow) → memory_get показывает все
// четыре task-поля. Плюс ошибки write path для type у не-задачи и неизвестного
// type — читаемым текстом инструмента.
func TestTaskStage15Cycle(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)
	const scope = "project:agentmemory"

	epicRes, _, err := srv.handleSave(ctx, nil, saveInput{
		Kind: string(memory.KindTask), Content: "Kanban-доска задач целиком.", Scope: scope,
		Summary: "Эпик доски задач", Type: string(memory.TaskTypeEpic),
	})
	if err != nil {
		t.Fatalf("handleSave(epic): %v", err)
	}
	epicID := extractID(t, textOf(epicRes))

	storyRes, _, err := srv.handleSave(ctx, nil, saveInput{
		Kind: string(memory.KindTask), Content: "Как агент я вижу поля задачи в схемах инструментов.", Scope: scope,
		Summary: "История поверхности агента", Type: string(memory.TaskTypeStory),
		Links: []linkEntry{{Dst: epicID, Rel: memory.RelPartOf}},
	})
	if err != nil {
		t.Fatalf("handleSave(story): %v", err)
	}
	storyID := extractID(t, textOf(storyRes))

	epics, _, err := srv.handleRecall(ctx, nil, recallInput{Query: "доска задач агент", Scope: scope, Types: []string{string(memory.TaskTypeEpic)}})
	if err != nil {
		t.Fatalf("handleRecall(types=[epic]): %v", err)
	}
	if md := textOf(epics); !strings.Contains(md, "Эпик доски задач") || strings.Contains(md, "История поверхности агента") {
		t.Errorf("recall types=[epic] должен показать только эпик:\n%s", md)
	}

	doing, branch, workflow := string(memory.StatusDoing), "feat/15.2-task-agent-surface", "dev"
	if _, _, err := srv.handleUpdate(ctx, nil, updateInput{ID: storyID, Status: &doing, Branch: &branch, Workflow: &workflow}); err != nil {
		t.Fatalf("handleUpdate(doing+branch+workflow): %v", err)
	}

	getRes, _, err := srv.handleGet(ctx, nil, getInput{ID: storyID})
	if err != nil {
		t.Fatalf("handleGet: %v", err)
	}
	full := textOf(getRes)
	for _, want := range []string{"status: doing", "- type: story", "- branch: feat/15.2-task-agent-surface", "- workflow: dev", epicID + " (part_of)"} {
		if !strings.Contains(full, want) {
			t.Errorf("memory_get не содержит %q:\n%s", want, full)
		}
	}

	// Очистка branch пустой строкой — поле исчезает из вывода memory_get.
	empty := ""
	if _, _, err := srv.handleUpdate(ctx, nil, updateInput{ID: storyID, Branch: &empty}); err != nil {
		t.Fatalf("handleUpdate(branch=\"\"): %v", err)
	}
	getRes, _, err = srv.handleGet(ctx, nil, getInput{ID: storyID})
	if err != nil {
		t.Fatalf("handleGet после очистки: %v", err)
	}
	if strings.Contains(textOf(getRes), "- branch:") {
		t.Errorf("после очистки branch не должен выводиться:\n%s", textOf(getRes))
	}

	// Ошибки write path доходят до агента читаемым текстом инструмента.
	_, _, err = srv.handleSave(ctx, nil, saveInput{Kind: string(memory.KindSemantic), Content: "факт", Scope: scope, Type: "task"})
	if err == nil || !strings.Contains(err.Error(), "kind=task") {
		t.Errorf("type у не-задачи: err = %v, want упоминание «kind=task»", err)
	}
	_, _, err = srv.handleSave(ctx, nil, saveInput{Kind: string(memory.KindTask), Content: "c", Scope: scope, Summary: "h", Type: "bug"})
	if err == nil || !strings.Contains(err.Error(), "epic, story, task") {
		t.Errorf("неизвестный type: err = %v, want перечень допустимых", err)
	}
}

// TestTaskLifecycleThroughTools проходит полный цикл задачи через инструменты:
// save(task) -> recall (задача видна) -> get (полный текст) -> update(done) ->
// recall (не видна) -> recall(statuses=[done]) (видна снова).
func TestTaskLifecycleThroughTools(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)

	const body = "Перевести хранение задач из файлов в память.\n\nГраницы: только ядро и MCP.\nКритерий приёмки: тесты зелёные."
	saveRes, _, err := srv.handleSave(ctx, nil, saveInput{
		Kind:    string(memory.KindTask),
		Content: body,
		Scope:   "project:agentmemory",
		Summary: "Задачи в памяти вместо файлов",
	})
	if err != nil {
		t.Fatalf("handleSave: %v", err)
	}
	id := extractID(t, textOf(saveRes))

	brief, _, err := srv.handleRecall(ctx, nil, recallInput{Query: "хранение задач память", Scope: "project:agentmemory"})
	if err != nil {
		t.Fatalf("handleRecall: %v", err)
	}
	md := textOf(brief)
	if !strings.Contains(md, "### Tasks (task)") || !strings.Contains(md, "[todo] Задачи в памяти вместо файлов") {
		t.Errorf("бриф должен показать задачу в секции Tasks со статусом todo:\n%s", md)
	}
	if strings.Contains(md, "Критерий приёмки") {
		t.Errorf("полный текст задачи не должен попадать в бриф:\n%s", md)
	}

	getRes, _, err := srv.handleGet(ctx, nil, getInput{ID: id})
	if err != nil {
		t.Fatalf("handleGet: %v", err)
	}
	full := textOf(getRes)
	if !strings.Contains(full, "Критерий приёмки: тесты зелёные.") {
		t.Errorf("memory_get обязан вернуть полный текст задачи:\n%s", full)
	}
	if !strings.Contains(full, "status: todo") {
		t.Errorf("memory_get должен показать состояние задачи:\n%s", full)
	}

	done := string(memory.StatusDone)
	if _, _, err := srv.handleUpdate(ctx, nil, updateInput{ID: id, Status: &done}); err != nil {
		t.Fatalf("handleUpdate(done): %v", err)
	}

	after, _, err := srv.handleRecall(ctx, nil, recallInput{Query: "хранение задач память", Scope: "project:agentmemory"})
	if err != nil {
		t.Fatalf("handleRecall после done: %v", err)
	}
	if strings.Contains(textOf(after), "Задачи в памяти вместо файлов") {
		t.Errorf("выполненная задача не должна попадать в дефолтный бриф:\n%s", textOf(after))
	}

	explicit, _, err := srv.handleRecall(ctx, nil, recallInput{
		Query: "хранение задач память", Scope: "project:agentmemory",
		Statuses: []string{done},
	})
	if err != nil {
		t.Fatalf("handleRecall(statuses=[done]): %v", err)
	}
	if !strings.Contains(textOf(explicit), "Задачи в памяти вместо файлов") {
		t.Errorf("явный фильтр statuses=[done] обязан показать выполненную задачу:\n%s", textOf(explicit))
	}
}

// TestGetNotFound: memory_get на несуществующем id даёт понятную ошибку, а не
// пустой ответ.
func TestGetNotFound(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)

	if _, _, err := srv.handleGet(ctx, nil, getInput{ID: "01J8Z000000000000000000A"}); err == nil {
		t.Fatalf("handleGet(несуществующий id) = nil, want ошибку")
	} else if !strings.Contains(err.Error(), "не найдена") {
		t.Errorf("ошибка = %v, want упоминание «не найдена»", err)
	}
}
