package write

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agentmemory/internal/memory"
)

// taskInput — валидный вход задачи для тестов (задача 14.1): у задачи
// обязателен summary-заголовок.
func taskInput(content, summary string) SaveInput {
	return SaveInput{
		Kind:    memory.KindTask,
		Scope:   "project:tasks",
		Content: content,
		Summary: summary,
	}
}

func TestSaveTaskDefaultsToTodo(t *testing.T) {
	ctx := context.Background()
	w, st := newTestWriter(t)

	id, merged, err := w.Save(ctx, taskInput("Перевести хранение задач из файлов в память", "Задачи в memo"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if merged {
		t.Fatalf("новая задача не должна сливаться с существующей")
	}

	m, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if m.Status != memory.StatusTodo {
		t.Errorf("Status = %q, want %q (дефолт при пустом status)", m.Status, memory.StatusTodo)
	}
	if m.Summary == nil || *m.Summary != "Задачи в memo" {
		t.Errorf("Summary = %v, want заголовок задачи", m.Summary)
	}
}

func TestSaveTaskExplicitStatus(t *testing.T) {
	ctx := context.Background()
	w, st := newTestWriter(t)

	in := taskInput("Починить дедуп задач", "Дедуп задач")
	in.Status = string(memory.StatusReview)
	id, _, err := w.Save(ctx, in)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	m, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if m.Status != memory.StatusReview {
		t.Errorf("Status = %q, want %q", m.Status, memory.StatusReview)
	}
}

// TestSaveTaskAcceptsEveryStage: каждая из стадий доски (Этап 15) принимается
// Save и переживает round-trip; бывший статус blocked — больше не стадия и
// отвергается как неизвестный (блокировка — свойство из связей, задача 15.4).
func TestSaveTaskAcceptsEveryStage(t *testing.T) {
	ctx := context.Background()
	w, st := newTestWriter(t)

	if len(memory.Statuses) != 7 {
		t.Fatalf("memory.Statuses = %v, want 7 стадий доски", memory.Statuses)
	}
	for _, stage := range memory.Statuses {
		in := taskInput("Задача на стадии "+string(stage), "Стадия "+string(stage))
		in.Status = string(stage)
		id, _, err := w.Save(ctx, in)
		if err != nil {
			t.Fatalf("Save(%s): %v", stage, err)
		}
		m, err := st.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get(%s): %v", stage, err)
		}
		if m.Status != stage {
			t.Errorf("Status = %q, want %q", m.Status, stage)
		}
	}

	in := taskInput("Задача со старым статусом", "Старый статус")
	in.Status = "blocked"
	if _, _, err := w.Save(ctx, in); !errors.Is(err, ErrUnknownStatus) {
		t.Errorf("Save со status=blocked: err = %v, want ErrUnknownStatus (стадия упразднена)", err)
	}
}

// TestSaveTaskTypeDefaultsAndValidation: дефолт type=task; epic/story
// принимаются; значение вне перечня — ErrUnknownTaskType с подсказкой.
func TestSaveTaskTypeDefaultsAndValidation(t *testing.T) {
	ctx := context.Background()
	w, st := newTestWriter(t)

	id, _, err := w.Save(ctx, taskInput("Задача без явного типа", "Без типа"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	m, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if m.Type != memory.TaskTypeTask {
		t.Errorf("Type = %q, want %q (дефолт)", m.Type, memory.TaskTypeTask)
	}

	for _, typ := range memory.TaskTypes {
		in := taskInput("Задача типа "+string(typ), "Тип "+string(typ))
		in.Type = string(typ)
		id, _, err := w.Save(ctx, in)
		if err != nil {
			t.Fatalf("Save(type=%s): %v", typ, err)
		}
		m, err := st.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if m.Type != typ {
			t.Errorf("Type = %q, want %q", m.Type, typ)
		}
	}

	in := taskInput("Задача с неизвестным типом", "Неизвестный тип")
	in.Type = "bug"
	_, _, err = w.Save(ctx, in)
	if !errors.Is(err, ErrUnknownTaskType) {
		t.Fatalf("Save с type=bug: err = %v, want ErrUnknownTaskType", err)
	}
	if !strings.Contains(err.Error(), "story") {
		t.Errorf("ошибка должна перечислять допустимые типы, got %v", err)
	}
}

// TestSaveTaskBranchWorkflowRoundTrip: branch/workflow — свободные строки, без
// валидации, обрезаются по пробелам и переживают round-trip.
func TestSaveTaskBranchWorkflowRoundTrip(t *testing.T) {
	ctx := context.Background()
	w, st := newTestWriter(t)

	in := taskInput("Задача с веткой и процессом", "Ветка и процесс")
	in.Branch = "  feat/kanban-board "
	in.Workflow = "ultracode"
	id, _, err := w.Save(ctx, in)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	m, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if m.Branch != "feat/kanban-board" {
		t.Errorf("Branch = %q, want %q", m.Branch, "feat/kanban-board")
	}
	if m.Workflow != "ultracode" {
		t.Errorf("Workflow = %q, want %q", m.Workflow, "ultracode")
	}
}

// TestSaveTaskFieldsRejectedForNonTask: type/branch/workflow у записи kind !=
// task — ErrTaskFieldNotApplicable с именем поля в тексте (fail-fast, не
// молчаливое отбрасывание).
func TestSaveTaskFieldsRejectedForNonTask(t *testing.T) {
	ctx := context.Background()
	w, _ := newTestWriter(t)

	cases := []struct {
		field string
		in    SaveInput
	}{
		{"type", SaveInput{Kind: memory.KindSemantic, Scope: "project:tasks", Content: "Факт с типом", Type: "epic"}},
		{"branch", SaveInput{Kind: memory.KindSemantic, Scope: "project:tasks", Content: "Факт с веткой", Branch: "main"}},
		{"workflow", SaveInput{Kind: memory.KindSemantic, Scope: "project:tasks", Content: "Факт с процессом", Workflow: "dev"}},
	}
	for _, c := range cases {
		_, _, err := w.Save(ctx, c.in)
		if !errors.Is(err, ErrTaskFieldNotApplicable) {
			t.Errorf("Save с %s у semantic: err = %v, want ErrTaskFieldNotApplicable", c.field, err)
			continue
		}
		if !strings.Contains(err.Error(), c.field) {
			t.Errorf("ошибка должна называть поле %q, got %v", c.field, err)
		}
	}

	// Пустые значения у не-задачи — не ошибка (поля просто не заданы).
	if _, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "project:tasks", Content: "Факт без task-полей"}); err != nil {
		t.Errorf("Save semantic без task-полей: %v", err)
	}
}

// TestUpdateTaskFields: Update меняет type/branch/workflow; пустая строка у
// Branch/Workflow очищает поле, у Type — возвращает дефолт task; nil — не
// трогает; у записи kind != task — ErrTaskFieldNotApplicable.
func TestUpdateTaskFields(t *testing.T) {
	ctx := context.Background()
	w, st := newTestWriter(t)

	in := taskInput("Задача для правки task-полей", "Правка task-полей")
	in.Type, in.Branch, in.Workflow = "story", "feat/x", "dev"
	id, _, err := w.Save(ctx, in)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	epic, branch := "epic", "feat/y"
	if err := w.Update(ctx, UpdateInput{Id: id, Type: &epic, Branch: &branch}); err != nil {
		t.Fatalf("Update(type, branch): %v", err)
	}
	m, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if m.Type != memory.TaskTypeEpic || m.Branch != "feat/y" || m.Workflow != "dev" {
		t.Errorf("после Update: type=%q branch=%q workflow=%q, want epic/feat/y/dev (workflow не тронут)", m.Type, m.Branch, m.Workflow)
	}

	empty := ""
	if err := w.Update(ctx, UpdateInput{Id: id, Type: &empty, Branch: &empty, Workflow: &empty}); err != nil {
		t.Fatalf("Update(пустые): %v", err)
	}
	m, err = st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if m.Type != memory.TaskTypeTask {
		t.Errorf("Type после пустого Update = %q, want %q (дефолт)", m.Type, memory.TaskTypeTask)
	}
	if m.Branch != "" || m.Workflow != "" {
		t.Errorf("Branch/Workflow после пустого Update = %q/%q, want очищены", m.Branch, m.Workflow)
	}

	bad := "bug"
	if err := w.Update(ctx, UpdateInput{Id: id, Type: &bad}); !errors.Is(err, ErrUnknownTaskType) {
		t.Errorf("Update(type=bug): err = %v, want ErrUnknownTaskType", err)
	}

	factID, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "project:tasks", Content: "Факт для правки task-полей"})
	if err != nil {
		t.Fatalf("Save semantic: %v", err)
	}
	if err := w.Update(ctx, UpdateInput{Id: factID, Branch: &branch}); !errors.Is(err, ErrTaskFieldNotApplicable) {
		t.Errorf("Update(branch) у semantic: err = %v, want ErrTaskFieldNotApplicable", err)
	}
}

func TestSaveTaskRequiresSummary(t *testing.T) {
	ctx := context.Background()
	w, _ := newTestWriter(t)

	in := taskInput("Текст задачи без заголовка", "   ")
	if _, _, err := w.Save(ctx, in); !errors.Is(err, ErrTaskSummaryRequired) {
		t.Errorf("Save без summary: err = %v, want ErrTaskSummaryRequired", err)
	}
}

func TestSaveUnknownStatusRejected(t *testing.T) {
	ctx := context.Background()
	w, _ := newTestWriter(t)

	in := taskInput("Задача с опечаткой в статусе", "Опечатка")
	in.Status = "in-progress"
	_, _, err := w.Save(ctx, in)
	if !errors.Is(err, ErrUnknownStatus) {
		t.Fatalf("Save с неизвестным status: err = %v, want ErrUnknownStatus", err)
	}
	// Ошибка перечисляет допустимые значения — агент узнаёт правильную форму сразу.
	if !strings.Contains(err.Error(), "doing") {
		t.Errorf("ошибка должна перечислять допустимые статусы, got %v", err)
	}
}

func TestSaveStatusRejectedForNonTask(t *testing.T) {
	ctx := context.Background()
	w, _ := newTestWriter(t)

	in := SaveInput{
		Kind:    memory.KindSemantic,
		Scope:   "project:tasks",
		Content: "Обычный факт со статусом",
		Status:  string(memory.StatusTodo),
	}
	if _, _, err := w.Save(ctx, in); !errors.Is(err, ErrStatusNotApplicable) {
		t.Errorf("Save со status у semantic: err = %v, want ErrStatusNotApplicable", err)
	}
}

// TestSaveTasksAreNeverDeduplicated: две задачи с ОДИНАКОВЫМ текстом — две
// записи, а не усиление одной (см. write.SkipDedup). Контроль в конце теста:
// тот же текст, сохранённый как semantic, дедупом сливается — значит задачи
// разводит именно исключение из дедупа, а не непопадание в порог сходства.
func TestSaveTasksAreNeverDeduplicated(t *testing.T) {
	ctx := context.Background()
	w, st := newTestWriter(t)

	const first = "Перенести хранение конфигурации в отдельный пакет, критерий приёмки — тесты зелёные"
	const second = first

	id1, merged1, err := w.Save(ctx, taskInput(first, "Конфиг в отдельный пакет"))
	if err != nil {
		t.Fatalf("Save 1: %v", err)
	}
	id2, merged2, err := w.Save(ctx, taskInput(second, "Конфиг в отдельный пакет, шаг 2"))
	if err != nil {
		t.Fatalf("Save 2: %v", err)
	}
	if merged1 || merged2 {
		t.Fatalf("задачи не должны сливаться дедупом: merged1=%v merged2=%v", merged1, merged2)
	}
	if id1 == id2 {
		t.Fatalf("две задачи должны получить разные id, got %q", id1)
	}

	// Тот же текст как semantic — сливается: показываем, что порог SimHash
	// действительно счёл бы эти тексты дублем, и разводит их именно SkipDedup.
	semIn := SaveInput{Kind: memory.KindSemantic, Scope: "project:tasks", Content: first}
	semID, _, err := w.Save(ctx, semIn)
	if err != nil {
		t.Fatalf("Save semantic 1: %v", err)
	}
	semIn2 := SaveInput{Kind: memory.KindSemantic, Scope: "project:tasks", Content: second}
	semID2, mergedSem, err := w.Save(ctx, semIn2)
	if err != nil {
		t.Fatalf("Save semantic 2: %v", err)
	}
	if !mergedSem || semID2 != semID {
		t.Errorf("тот же текст как semantic обязан слиться дедупом: merged=%v id=%q want %q", mergedSem, semID2, semID)
	}

	if _, err := st.Get(ctx, id2); err != nil {
		t.Errorf("вторая задача должна существовать: %v", err)
	}
}

func TestUpdateTaskStatus(t *testing.T) {
	ctx := context.Background()
	w, st := newTestWriter(t)

	id, _, err := w.Save(ctx, taskInput("Задача, которую доведём до конца", "Доведение до конца"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	doing := string(memory.StatusDoing)
	if err := w.Update(ctx, UpdateInput{Id: id, Status: &doing}); err != nil {
		t.Fatalf("Update(doing): %v", err)
	}
	m, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if m.Status != memory.StatusDoing {
		t.Errorf("Status = %q, want %q", m.Status, memory.StatusDoing)
	}

	done := string(memory.StatusDone)
	if err := w.Update(ctx, UpdateInput{Id: id, Status: &done}); err != nil {
		t.Fatalf("Update(done): %v", err)
	}
	m, err = st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if m.Status != memory.StatusDone {
		t.Errorf("Status = %q, want %q", m.Status, memory.StatusDone)
	}
	if m.SupersededBy != nil {
		t.Errorf("выполненная задача остаётся живой записью (историей), got superseded_by=%v", *m.SupersededBy)
	}
}

func TestUpdateStatusValidation(t *testing.T) {
	ctx := context.Background()
	w, _ := newTestWriter(t)

	taskID, _, err := w.Save(ctx, taskInput("Задача для проверки валидации статуса", "Валидация статуса"))
	if err != nil {
		t.Fatalf("Save task: %v", err)
	}
	factID, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "project:tasks", Content: "Факт без статуса"})
	if err != nil {
		t.Fatalf("Save semantic: %v", err)
	}

	bad := "готово"
	if err := w.Update(ctx, UpdateInput{Id: taskID, Status: &bad}); !errors.Is(err, ErrUnknownStatus) {
		t.Errorf("неизвестный status: err = %v, want ErrUnknownStatus", err)
	}
	empty := ""
	if err := w.Update(ctx, UpdateInput{Id: taskID, Status: &empty}); !errors.Is(err, ErrUnknownStatus) {
		t.Errorf("пустой status при правке: err = %v, want ErrUnknownStatus (не молчаливый откат в todo)", err)
	}
	doing := string(memory.StatusDoing)
	if err := w.Update(ctx, UpdateInput{Id: factID, Status: &doing}); !errors.Is(err, ErrStatusNotApplicable) {
		t.Errorf("status у semantic: err = %v, want ErrStatusNotApplicable", err)
	}
}
