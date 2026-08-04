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
	in.Status = string(memory.StatusBlocked)
	id, _, err := w.Save(ctx, in)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	m, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if m.Status != memory.StatusBlocked {
		t.Errorf("Status = %q, want %q", m.Status, memory.StatusBlocked)
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
