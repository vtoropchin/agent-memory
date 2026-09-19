package mcp

import (
	"context"
	"strings"
	"testing"

	"agentmemory/internal/memory"
	"agentmemory/internal/memory/store"
	"agentmemory/internal/memory/write"
)

// saveTask заводит задачу напрямую через Writer (тип задачи в схеме memory_save
// — соседняя задача 15.2, здесь проверяется механика иерархии, а не схема).
func saveTask(t *testing.T, srv *Server, typ memory.TaskType, title, status string, importance int, links ...write.LinkInput) string {
	t.Helper()
	id, _, err := srv.writer.Save(context.Background(), write.SaveInput{
		Kind:       memory.KindTask,
		Type:       string(typ),
		Content:    "Текст задачи «" + title + "»: что сделать, границы, критерий приёмки.",
		Scope:      "project:agentmemory",
		Summary:    title,
		Status:     status,
		Importance: importance,
		Links:      links,
	})
	if err != nil {
		t.Fatalf("Save(%s): %v", title, err)
	}
	return id
}

// TestGetShowsHierarchy: эпик и две истории part_of эпика (одна done) →
// memory_get(эпик) показывает «Дети (2, готово 1)» в порядке доски (открытая
// раньше готовой), memory_get(история) — «Родитель» с эпиком; у истории без
// детей секции «Дети» нет.
func TestGetShowsHierarchy(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)

	epic := saveTask(t, srv, memory.TaskTypeEpic, "Kanban-доска задач", "doing", 5)
	storyDone := saveTask(t, srv, memory.TaskTypeStory, "Модель задачи v2", "done", 4,
		write.LinkInput{Dst: epic, Rel: memory.RelPartOf})
	storyOpen := saveTask(t, srv, memory.TaskTypeStory, "Иерархия epic/story/task", "todo", 3,
		write.LinkInput{Dst: epic, Rel: memory.RelPartOf})
	// Тематическая связь related на эпик — не иерархия: в детях не показывается.
	if _, _, err := srv.writer.Save(ctx, write.SaveInput{
		Kind: memory.KindSemantic, Content: "Факт про доску: колонки в порядке memory.Statuses.",
		Scope: "project:agentmemory", Links: []write.LinkInput{{Dst: epic, Rel: memory.RelRelated}},
	}); err != nil {
		t.Fatalf("Save(факт): %v", err)
	}

	epicRes, _, err := srv.handleGet(ctx, nil, getInput{ID: epic})
	if err != nil {
		t.Fatalf("handleGet(эпик): %v", err)
	}
	out := textOf(epicRes)
	if !strings.Contains(out, "Дети (2, готово 1):") {
		t.Errorf("memory_get(эпик) должен показать «Дети (2, готово 1)»:\n%s", out)
	}
	openLine := "- [todo] (story) Иерархия epic/story/task (" + storyOpen + ")"
	doneLine := "- [done] (story) Модель задачи v2 (" + storyDone + ")"
	iOpen, iDone := strings.Index(out, openLine), strings.Index(out, doneLine)
	if iOpen < 0 || iDone < 0 {
		t.Fatalf("memory_get(эпик) должен перечислить обе истории строками %q и %q:\n%s", openLine, doneLine, out)
	}
	if iOpen > iDone {
		t.Errorf("дети должны идти в порядке стадий доски (todo раньше done):\n%s", out)
	}
	if strings.Contains(out, "Факт про доску") {
		t.Errorf("related-сосед не должен попадать в детей:\n%s", out)
	}
	if strings.Contains(out, "Родитель:") {
		t.Errorf("у эпика без part_of секции «Родитель» быть не должно:\n%s", out)
	}
	// Секции идут после метаданных и до текста записи.
	if iMeta, iBody := strings.Index(out, "- kind: task"), strings.Index(out, "Текст задачи «Kanban-доска задач»"); !(iMeta < iOpen && iOpen < iBody) {
		t.Errorf("секции иерархии должны стоять между метаданными и текстом:\n%s", out)
	}

	storyRes, _, err := srv.handleGet(ctx, nil, getInput{ID: storyOpen})
	if err != nil {
		t.Fatalf("handleGet(история): %v", err)
	}
	out = textOf(storyRes)
	wantParent := "Родитель:\n- [doing] (epic) Kanban-доска задач (" + epic + ")"
	if !strings.Contains(out, wantParent) {
		t.Errorf("memory_get(история) должен показать родителя %q:\n%s", wantParent, out)
	}
	if strings.Contains(out, "Дети (") {
		t.Errorf("у истории без детей секции «Дети» быть не должно:\n%s", out)
	}
}

// TestGetHierarchyOnlyForTasks: у записи прочего kind part_of — тематическая
// связь, секций иерархии нет, даже если на неё ссылаются задачи.
func TestGetHierarchyOnlyForTasks(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)

	factID, _, err := srv.writer.Save(ctx, write.SaveInput{
		Kind: memory.KindSemantic, Content: "Тема: kanban-доска.", Scope: "project:agentmemory",
	})
	if err != nil {
		t.Fatalf("Save(факт): %v", err)
	}
	saveTask(t, srv, memory.TaskTypeTask, "Задача по теме", "todo", 3, write.LinkInput{Dst: factID, Rel: memory.RelPartOf})

	res, _, err := srv.handleGet(ctx, nil, getInput{ID: factID})
	if err != nil {
		t.Fatalf("handleGet(факт): %v", err)
	}
	if out := textOf(res); strings.Contains(out, "Дети (") || strings.Contains(out, "Родитель:") {
		t.Errorf("у не-задачи секций иерархии быть не должно:\n%s", out)
	}
}

// TestGetChildrenExcludeForgotten: забытый (тумстоун) ребёнок не показывается и
// не считается в прогрессе.
func TestGetChildrenExcludeForgotten(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)

	epic := saveTask(t, srv, memory.TaskTypeEpic, "Эпик", "todo", 3)
	kept := saveTask(t, srv, memory.TaskTypeTask, "Живая", "done", 3, write.LinkInput{Dst: epic, Rel: memory.RelPartOf})
	dropped := saveTask(t, srv, memory.TaskTypeTask, "Отменённая", "todo", 3, write.LinkInput{Dst: epic, Rel: memory.RelPartOf})
	if _, _, err := srv.handleForget(ctx, nil, forgetInput{ID: dropped}); err != nil {
		t.Fatalf("handleForget: %v", err)
	}

	res, _, err := srv.handleGet(ctx, nil, getInput{ID: epic})
	if err != nil {
		t.Fatalf("handleGet: %v", err)
	}
	out := textOf(res)
	if !strings.Contains(out, "Дети (1, готово 1):") || !strings.Contains(out, kept) || strings.Contains(out, dropped) {
		t.Errorf("ожидал «Дети (1, готово 1)» только с живым ребёнком:\n%s", out)
	}
}

// parentGoneStore — Store, у которого GetMany не находит один id: имитирует
// висячее ребро part_of (родитель удалён физически). Через сам store такое не
// создать (FOREIGN KEY на links в обеих схемах), поэтому ветка деградации
// проверяется подменой порта.
type parentGoneStore struct {
	store.Store
	gone string
}

func (s parentGoneStore) GetMany(ctx context.Context, ids []string) ([]memory.Memory, error) {
	all, err := s.Store.GetMany(ctx, ids)
	if err != nil {
		return nil, err
	}
	kept := all[:0]
	for _, m := range all {
		if m.Id != s.gone {
			kept = append(kept, m)
		}
	}
	return kept, nil
}

// TestGetDanglingParentSkipped: родитель, которого хранилище не находит, —
// секция «Родитель» опускается, memory_get не падает; дети при этом считаются.
func TestGetDanglingParentSkipped(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)

	epic := saveTask(t, srv, memory.TaskTypeEpic, "Эпик", "todo", 3)
	story := saveTask(t, srv, memory.TaskTypeStory, "История", "todo", 3, write.LinkInput{Dst: epic, Rel: memory.RelPartOf})
	srv.store = parentGoneStore{Store: srv.store, gone: epic}

	res, _, err := srv.handleGet(ctx, nil, getInput{ID: story})
	if err != nil {
		t.Fatalf("handleGet с висячим родителем: %v", err)
	}
	if out := textOf(res); strings.Contains(out, "Родитель:") {
		t.Errorf("недоступный родитель не должен давать секцию «Родитель»:\n%s", out)
	}
}

// TestGetForgottenParentSkipped: забытый (тумстоун) родитель не показывается —
// прежний Store.Get тумстоуны не фильтровал (техдолг 15.3, закрыт board.Describe).
func TestGetForgottenParentSkipped(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)

	epic := saveTask(t, srv, memory.TaskTypeEpic, "Эпик", "todo", 3)
	story := saveTask(t, srv, memory.TaskTypeStory, "История", "todo", 3, write.LinkInput{Dst: epic, Rel: memory.RelPartOf})
	if _, _, err := srv.handleForget(ctx, nil, forgetInput{ID: epic}); err != nil {
		t.Fatalf("handleForget(эпик): %v", err)
	}

	res, _, err := srv.handleGet(ctx, nil, getInput{ID: story})
	if err != nil {
		t.Fatalf("handleGet: %v", err)
	}
	if out := textOf(res); strings.Contains(out, "Родитель:") {
		t.Errorf("забытый родитель не должен давать секцию «Родитель»:\n%s", out)
	}
}
