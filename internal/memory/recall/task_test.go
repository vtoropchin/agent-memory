package recall

import (
	"context"
	"strings"
	"testing"

	"agentmemory/internal/config"
	"agentmemory/internal/memory"
	"agentmemory/internal/memory/embed"
	"agentmemory/internal/memory/reason"
)

// TestRecall_TaskSectionInOrder: задачи выводятся отдельной секцией между
// How-to и Tech debt (DESIGN.md §8).
func TestRecall_TaskSectionInOrder(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:task-section"

	procID, taskID, debtID := newID(t), newID(t), newID(t)
	putMemory(t, ctx, s, memory.Memory{
		Id: procID, Kind: memory.KindProcedural, Scope: scope,
		Content: "общий термин про задачи секцию", Summary: strPtr("Процедурный рецепт"),
		Importance: 3, CreatedAt: 1, UpdatedAt: 1,
	})
	putMemory(t, ctx, s, memory.Memory{
		Id: taskID, Kind: memory.KindTask, Scope: scope, Status: memory.StatusDoing,
		Content: "общий термин про задачи секцию", Summary: strPtr("Починить экспорт"),
		Importance: 3, CreatedAt: 2, UpdatedAt: 2,
	})
	putMemory(t, ctx, s, memory.Memory{
		Id: debtID, Kind: memory.KindDebt, Scope: scope,
		Content: "общий термин про задачи секцию", Summary: strPtr("Известная проблема"),
		Importance: 3, CreatedAt: 3, UpdatedAt: 3,
	})

	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()}, reason.NewNoop(), embed.NewNoop(),
		WithNow(func() int64 { return 1000 }))

	brief, err := r.Recall(ctx, memory.Query{Text: "термин задачи секцию", Scope: scope})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	idxHowTo := strings.Index(brief.Markdown, "### How-to (procedural)")
	idxTask := strings.Index(brief.Markdown, "### Tasks (task)")
	idxDebt := strings.Index(brief.Markdown, "### Tech debt (debt)")
	if idxHowTo == -1 || idxTask == -1 || idxDebt == -1 {
		t.Fatalf("все секции должны присутствовать:\n%s", brief.Markdown)
	}
	if !(idxHowTo < idxTask && idxTask < idxDebt) {
		t.Errorf("порядок секций должен быть How-to -> Tasks -> Tech debt:\n%s", brief.Markdown)
	}
}

// TestRecall_TaskLineShowsStatus: строка задачи начинается с маркера состояния,
// а сам текст берётся из Summary (заголовка), а не из многострочного content.
func TestRecall_TaskLineShowsStatus(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:task-line"

	putMemory(t, ctx, s, memory.Memory{
		Id: newID(t), Kind: memory.KindTask, Scope: scope, Status: memory.StatusDoing,
		Content: "Полный текст задачи: сделать импорт задач, границы такие-то, критерий приёмки такой-то",
		Summary: strPtr("Импорт задач"), Importance: 4, CreatedAt: 1, UpdatedAt: 1,
	})

	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()}, reason.NewNoop(), embed.NewNoop(),
		WithNow(func() int64 { return 1000 }))

	brief, err := r.Recall(ctx, memory.Query{Text: "импорт задач", Scope: scope})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if !strings.Contains(brief.Markdown, "- [doing] Импорт задач [★4]") {
		t.Errorf("строка задачи должна нести статус и заголовок, got:\n%s", brief.Markdown)
	}
	if strings.Contains(brief.Markdown, "критерий приёмки") {
		t.Errorf("полный текст задачи не должен попадать в бриф:\n%s", brief.Markdown)
	}
}

// TestRecall_DoneTaskHiddenByDefault: выполненная задача не попадает в выдачу
// без явного фильтра, но находится при statuses=["done"]; незавершённые задачи
// и записи прочих kind фильтр не затрагивает.
func TestRecall_DoneTaskHiddenByDefault(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:task-done"

	putMemory(t, ctx, s, memory.Memory{
		Id: newID(t), Kind: memory.KindTask, Scope: scope, Status: memory.StatusDone,
		Content: "общий текст про миграцию хранилища", Summary: strPtr("Сделанная задача"),
		Importance: 3, CreatedAt: 1, UpdatedAt: 1,
	})
	putMemory(t, ctx, s, memory.Memory{
		Id: newID(t), Kind: memory.KindTask, Scope: scope, Status: memory.StatusTodo,
		Content: "общий текст про миграцию хранилища", Summary: strPtr("Открытая задача"),
		Importance: 3, CreatedAt: 2, UpdatedAt: 2,
	})
	putMemory(t, ctx, s, memory.Memory{
		Id: newID(t), Kind: memory.KindSemantic, Scope: scope,
		Content: "общий текст про миграцию хранилища", Summary: strPtr("Обычный факт"),
		Importance: 3, CreatedAt: 3, UpdatedAt: 3,
	})

	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()}, reason.NewNoop(), embed.NewNoop(),
		WithNow(func() int64 { return 1000 }))

	brief, err := r.Recall(ctx, memory.Query{Text: "текст про миграцию хранилища", Scope: scope})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if strings.Contains(brief.Markdown, "Сделанная задача") {
		t.Errorf("выполненная задача не должна попадать в дефолтную выдачу:\n%s", brief.Markdown)
	}
	if !strings.Contains(brief.Markdown, "Открытая задача") || !strings.Contains(brief.Markdown, "Обычный факт") {
		t.Errorf("открытая задача и обычный факт обязаны остаться в выдаче:\n%s", brief.Markdown)
	}

	done, err := r.Recall(ctx, memory.Query{Text: "текст про миграцию хранилища", Scope: scope, Statuses: []string{string(memory.StatusDone)}})
	if err != nil {
		t.Fatalf("Recall(statuses=done): %v", err)
	}
	if !strings.Contains(done.Markdown, "Сделанная задача") {
		t.Errorf("явный фильтр statuses=[done] обязан показать выполненную задачу:\n%s", done.Markdown)
	}
	if strings.Contains(done.Markdown, "Обычный факт") {
		t.Errorf("явный фильтр по статусу отбирает только задачи, факт не должен попасть:\n%s", done.Markdown)
	}
}
