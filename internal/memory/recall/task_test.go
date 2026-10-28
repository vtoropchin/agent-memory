package recall

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agentmemory/internal/config"
	"agentmemory/internal/memory"
	"agentmemory/internal/memory/embed"
	"agentmemory/internal/memory/reason"
	"agentmemory/internal/memory/store"
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

// TestRecall_TaskLineShowsTypeAndBranch: тип задачи (Этап 15) печатается в
// скобках только для epic/story, ветка — хвостом строки; обычная задача (task)
// без ветки выглядит как раньше.
func TestRecall_TaskLineShowsTypeAndBranch(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:task-type-line"

	putMemory(t, ctx, s, memory.Memory{
		Id: newID(t), Kind: memory.KindTask, Scope: scope, Status: memory.StatusDoing,
		Type: memory.TaskTypeEpic, Branch: "feat/kanban-board",
		Content: "Полный текст эпика про доску задач", Summary: strPtr("Kanban-доска"),
		Importance: 4, CreatedAt: 1, UpdatedAt: 1,
	})
	putMemory(t, ctx, s, memory.Memory{
		Id: newID(t), Kind: memory.KindTask, Scope: scope, Status: memory.StatusTodo,
		Type:    memory.TaskTypeTask,
		Content: "Полный текст обычной задачи про доску", Summary: strPtr("Колонка done"),
		Importance: 3, CreatedAt: 2, UpdatedAt: 2,
	})

	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()}, reason.NewNoop(), embed.NewNoop(),
		WithNow(func() int64 { return 1000 }))

	brief, err := r.Recall(ctx, memory.Query{Text: "доска задач", Scope: scope})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if !strings.Contains(brief.Markdown, "- [doing] (epic) Kanban-доска [★4] · ветка feat/kanban-board") {
		t.Errorf("строка эпика должна нести тип и ветку, got:\n%s", brief.Markdown)
	}
	// Обычная задача (тип task — дефолт) без ветки печатается как раньше: ни
	// "(task)", ни хвоста с веткой (проверка по строке целиком, а не по
	// вхождению — заголовок секции "### Tasks (task)" содержит те же скобки).
	if !strings.Contains("\n"+brief.Markdown+"\n", "\n- [todo] Колонка done [★3]\n") {
		t.Errorf("обычная задача без ветки печатается как раньше, got:\n%s", brief.Markdown)
	}
}

// TestRecall_FilterByTypes: Query.Types пробрасывается в store — в бриф
// попадают только задачи запрошенных типов.
func TestRecall_FilterByTypes(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:task-types-filter"

	putMemory(t, ctx, s, memory.Memory{
		Id: newID(t), Kind: memory.KindTask, Scope: scope, Status: memory.StatusTodo, Type: memory.TaskTypeEpic,
		Content: "общий текст про фильтр типов", Summary: strPtr("Эпик фильтра"), Importance: 3, CreatedAt: 1, UpdatedAt: 1,
	})
	putMemory(t, ctx, s, memory.Memory{
		Id: newID(t), Kind: memory.KindTask, Scope: scope, Status: memory.StatusTodo, Type: memory.TaskTypeTask,
		Content: "общий текст про фильтр типов", Summary: strPtr("Задача фильтра"), Importance: 3, CreatedAt: 2, UpdatedAt: 2,
	})
	putMemory(t, ctx, s, memory.Memory{
		Id: newID(t), Kind: memory.KindSemantic, Scope: scope,
		Content: "общий текст про фильтр типов", Summary: strPtr("Факт фильтра"), Importance: 3, CreatedAt: 3, UpdatedAt: 3,
	})

	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()}, reason.NewNoop(), embed.NewNoop(),
		WithNow(func() int64 { return 1000 }))

	brief, err := r.Recall(ctx, memory.Query{Text: "фильтр типов", Scope: scope, Types: []string{"epic"}})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if !strings.Contains(brief.Markdown, "Эпик фильтра") {
		t.Errorf("эпик обязан попасть в бриф при Types=[epic]:\n%s", brief.Markdown)
	}
	if strings.Contains(brief.Markdown, "Задача фильтра") || strings.Contains(brief.Markdown, "Факт фильтра") {
		t.Errorf("Types=[epic] не должен пропускать обычную задачу и факт:\n%s", brief.Markdown)
	}
}

// putBlockedTask — задача scope со статусом status и рёбрами blocked_by на
// blockers; content общий, чтобы все записи находились одним запросом.
func putBlockedTask(t *testing.T, s store.Store, scope, id, title string, status memory.Status, blockers ...string) {
	t.Helper()
	links := make([]memory.Link, len(blockers))
	for i, b := range blockers {
		links[i] = memory.Link{Dst: b, Rel: memory.RelBlockedBy}
	}
	putMemory(t, context.Background(), s, memory.Memory{
		Id: id, Kind: memory.KindTask, Scope: scope, Status: status, Type: memory.TaskTypeTask,
		Content: "общий текст про блокировку задач", Summary: strPtr(title),
		Importance: 3, CreatedAt: 1, UpdatedAt: 1, Links: links,
	})
}

// TestRecall_BlockedTaskMarker: заблокированная задача помечается ⛔ с
// заголовком блокера (первые два по имени, дальше +n); блокер из другого scope
// учитывается; после done блокера маркера нет (задача 15.4).
func TestRecall_BlockedTaskMarker(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:blocked"

	b1, b2, b3, task := newID(t), newID(t), newID(t), newID(t)
	putBlockedTask(t, s, scope, b1, "Первый блокер", memory.StatusTodo)
	putBlockedTask(t, s, "project:other", b2, "Чужой блокер", memory.StatusDoing) // кросс-scope
	putBlockedTask(t, s, scope, b3, "Третий блокер", memory.StatusTodo)
	putBlockedTask(t, s, scope, task, "Зависимая задача", memory.StatusTodo, b1, b2, b3)

	r := New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()}, reason.NewNoop(), embed.NewNoop(),
		WithNow(func() int64 { return 1000 }))
	brief, err := r.Recall(ctx, memory.Query{Text: "блокировку задач", Scope: scope})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	want := "- [todo] ⛔ Зависимая задача (ждёт: Первый блокер, Чужой блокер +1) [★3]"
	if !strings.Contains(brief.Markdown, want) {
		t.Errorf("ожидал строку %q:\n%s", want, brief.Markdown)
	}
	if strings.Contains(brief.Markdown, "⛔ Первый блокер") {
		t.Errorf("сам блокер не должен помечаться ⛔:\n%s", brief.Markdown)
	}

	// Блокеры закрыты/отменены — маркер исчезает без правки задачи.
	tomb := b3
	for _, id := range []string{b1, b2} {
		m, err := s.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		m.Status = memory.StatusDone
		putMemory(t, ctx, s, m)
	}
	if err := s.Supersede(ctx, b3, tomb, ""); err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	brief, err = r.Recall(ctx, memory.Query{Text: "блокировку задач", Scope: scope})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if strings.Contains(brief.Markdown, "⛔") || !strings.Contains(brief.Markdown, "- [todo] Зависимая задача [★3]") {
		t.Errorf("после done/тумстоуна блокеров маркера быть не должно:\n%s", brief.Markdown)
	}
}

// getManyFailingStore — Store, у которого GetMany всегда падает: имитирует
// сбой догрузки блокеров.
type getManyFailingStore struct {
	store.Store
}

func (getManyFailingStore) GetMany(context.Context, []string) ([]memory.Memory, error) {
	return nil, errors.New("сбой батч-чтения")
}

// TestRecall_BlockerLoadErrorDegrades: ошибка GetMany — бриф собирается без
// маркера, Recall не падает (DESIGN.md §15).
func TestRecall_BlockerLoadErrorDegrades(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	scope := "project:blocked-degrade"
	b, task := newID(t), newID(t)
	putBlockedTask(t, s, scope, b, "Блокер", memory.StatusTodo)
	putBlockedTask(t, s, scope, task, "Зависимая задача", memory.StatusTodo, b)

	r := New(getManyFailingStore{s}, config.RecallConfig{DefaultTokenBudget: 800, Weights: defaultWeights()}, reason.NewNoop(), embed.NewNoop(),
		WithNow(func() int64 { return 1000 }))
	brief, err := r.Recall(ctx, memory.Query{Text: "блокировку задач", Scope: scope})
	if err != nil {
		t.Fatalf("Recall при сбое GetMany должен деградировать, а не падать: %v", err)
	}
	if strings.Contains(brief.Markdown, "⛔") || !strings.Contains(brief.Markdown, "- [todo] Зависимая задача [★3]") {
		t.Errorf("бриф без маркера, но с задачей:\n%s", brief.Markdown)
	}
}
