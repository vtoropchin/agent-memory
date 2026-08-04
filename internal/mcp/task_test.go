package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

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
