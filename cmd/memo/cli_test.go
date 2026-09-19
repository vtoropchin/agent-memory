package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentmemory/internal/memory"
	"agentmemory/internal/memory/store"
	"agentmemory/internal/memory/write"
)

// writeTestConfig создаёт минимальный agentmemory.toml с заданным [scope] (и,
// опционально, embedder.base_url — для теста деградации) и возвращает путь к
// нему. Остальные поля наследуют config.Default() (Load декодирует TOML поверх
// дефолтов), путь БД в файле не задаётся — тесты передают его отдельно флагом -db.
func writeTestConfig(t *testing.T, dir, project string, shared []string, embedderURL string) string {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "[scope]\nproject = %q\n", project)
	fmt.Fprint(&b, "shared = [")
	for i, s := range shared {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q", s)
	}
	b.WriteString("]\n")
	if embedderURL != "" {
		fmt.Fprintf(&b, "\n[embedder]\nprovider = \"ollama\"\nbase_url = %q\nmodel = \"bge-m3\"\n", embedderURL)
	}
	path := filepath.Join(dir, "agentmemory.toml")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
	return path
}

// TestCLISaveThenRecallRoundTrip проверяет базовый сценарий задачи 7.1: save
// кладёт факт в домашний scope проекта (дефолт из [scope]), recall с тем же
// конфигом находит его по смыслу текста.
func TestCLISaveThenRecallRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeTestConfig(t, dir, "alpha", []string{"user:*"}, "")
	dbPath := filepath.Join(dir, "memory.db")

	var saveOut bytes.Buffer
	saveArgs := []string{"-config", cfgPath, "-db", dbPath, "-kind", "semantic", "-content", "уникальный факт про round trip CLI 7.1"}
	if err := runSave(saveArgs, strings.NewReader(""), &saveOut); err != nil {
		t.Fatalf("runSave: %v", err)
	}
	if !strings.HasPrefix(saveOut.String(), "saved ") {
		t.Fatalf("runSave stdout = %q, want префикс \"saved \"", saveOut.String())
	}

	var recallOut bytes.Buffer
	recallArgs := []string{"-config", cfgPath, "-db", dbPath, "round trip CLI 7.1"}
	if err := runRecall(recallArgs, &recallOut); err != nil {
		t.Fatalf("runRecall: %v", err)
	}
	if !strings.Contains(recallOut.String(), "round trip CLI 7.1") {
		t.Errorf("runRecall stdout = %q, не содержит сохранённый факт", recallOut.String())
	}
}

// TestCLIRecallScopesFiltersOtherProject проверяет, что явный -scopes сужает
// выдачу: факт, сохранённый в домашний scope проекта alpha, не находится при
// recall с -scopes, указывающим на чужой project.
func TestCLIRecallScopesFiltersOtherProject(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeTestConfig(t, dir, "alpha", []string{"user:*"}, "")
	dbPath := filepath.Join(dir, "memory.db")

	var saveOut bytes.Buffer
	saveArgs := []string{"-config", cfgPath, "-db", dbPath, "-kind", "semantic", "-content", "факт только для проекта alpha про изоляцию scope"}
	if err := runSave(saveArgs, strings.NewReader(""), &saveOut); err != nil {
		t.Fatalf("runSave: %v", err)
	}

	var recallOut bytes.Buffer
	recallArgs := []string{"-config", cfgPath, "-db", dbPath, "-scopes", "project:beta", "-quiet", "изоляцию scope"}
	if err := runRecall(recallArgs, &recallOut); err != nil {
		t.Fatalf("runRecall: %v", err)
	}
	if recallOut.String() != "" {
		t.Errorf("runRecall с -scopes=project:beta и -quiet вернул %q, want пустой stdout (чужой project не виден)", recallOut.String())
	}
}

// TestCLISaveContentFromStdin проверяет, что -content - читает текст факта из
// stdin, а не трактует "-" как буквальное содержимое.
func TestCLISaveContentFromStdin(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeTestConfig(t, dir, "alpha", []string{"user:*"}, "")
	dbPath := filepath.Join(dir, "memory.db")

	var saveOut bytes.Buffer
	saveArgs := []string{"-config", cfgPath, "-db", dbPath, "-kind", "episodic", "-content", "-"}
	stdin := strings.NewReader("факт, пришедший через стандартный ввод для CLI")
	if err := runSave(saveArgs, stdin, &saveOut); err != nil {
		t.Fatalf("runSave: %v", err)
	}
	if !strings.HasPrefix(saveOut.String(), "saved ") {
		t.Fatalf("runSave stdout = %q, want префикс \"saved \"", saveOut.String())
	}

	var recallOut bytes.Buffer
	recallArgs := []string{"-config", cfgPath, "-db", dbPath, "стандартный ввод для CLI"}
	if err := runRecall(recallArgs, &recallOut); err != nil {
		t.Fatalf("runRecall: %v", err)
	}
	if !strings.Contains(recallOut.String(), "стандартный ввод") {
		t.Errorf("runRecall stdout = %q, не содержит факт, сохранённый из stdin", recallOut.String())
	}
}

// TestCLISaveDuplicateMerges проверяет дедуп: повторный save того же факта в
// тот же scope/kind не вставляет новую запись, а сливается с существующей
// (вывод "merged <id>" вместо "saved <id>").
func TestCLISaveDuplicateMerges(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeTestConfig(t, dir, "alpha", []string{"user:*"}, "")
	dbPath := filepath.Join(dir, "memory.db")
	args := []string{"-config", cfgPath, "-db", dbPath, "-kind", "semantic", "-content", "повторяющийся факт про дедупликацию CLI"}

	var out1 bytes.Buffer
	if err := runSave(args, strings.NewReader(""), &out1); err != nil {
		t.Fatalf("runSave (1): %v", err)
	}
	if !strings.HasPrefix(out1.String(), "saved ") {
		t.Fatalf("runSave (1) stdout = %q, want \"saved \"", out1.String())
	}

	var out2 bytes.Buffer
	if err := runSave(args, strings.NewReader(""), &out2); err != nil {
		t.Fatalf("runSave (2): %v", err)
	}
	if !strings.HasPrefix(out2.String(), "merged ") {
		t.Errorf("runSave (2) stdout = %q, want \"merged \" (дедуп)", out2.String())
	}
}

// TestCLISaveEmptyScopeErrors проверяет, что save без явного -scope и при
// пустом [scope] (мультипроектность выключена) возвращает ошибку
// write.ErrEmptyScope, а не молча пишет в пустой scope.
func TestCLISaveEmptyScopeErrors(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeTestConfig(t, dir, "", nil, "")
	dbPath := filepath.Join(dir, "memory.db")

	args := []string{"-config", cfgPath, "-db", dbPath, "-kind", "semantic", "-content", "факт без scope"}
	err := runSave(args, strings.NewReader(""), &bytes.Buffer{})
	if !errors.Is(err, write.ErrEmptyScope) {
		t.Fatalf("runSave err = %v, want обёрнутый write.ErrEmptyScope", err)
	}
}

// TestCLIRecallStdoutHasNoLogs проверяет, что предупреждение о деградации
// embedder (ollama недоступна) уходит в stderr (через стандартный log.Printf),
// а не примешивается в stdout recall — единственное, что должно быть в out,
// это Markdown-бриф.
func TestCLIRecallStdoutHasNoLogs(t *testing.T) {
	dir := t.TempDir()
	// Заведомо недоступный адрес ollama — newEmbedder должен деградировать до
	// Noop с предупреждением в лог, а не завершить recall ошибкой.
	cfgPath := writeTestConfig(t, dir, "alpha", []string{"user:*"}, "http://127.0.0.1:1")
	dbPath := filepath.Join(dir, "memory.db")

	var saveOut bytes.Buffer
	// Save в этом же тесте выполняется с тем же конфигом (ollama тоже
	// недоступна для него) — это часть проверки: деградация не должна портить
	// вывод ни одной из подкоманд.
	saveArgs := []string{"-config", cfgPath, "-db", dbPath, "-kind", "semantic", "-content", "факт для проверки чистоты stdout"}
	if err := runSave(saveArgs, strings.NewReader(""), &saveOut); err != nil {
		t.Fatalf("runSave: %v", err)
	}

	var logBuf bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(prevOut) })

	var recallOut bytes.Buffer
	recallArgs := []string{"-config", cfgPath, "-db", dbPath, "проверки чистоты stdout"}
	if err := runRecall(recallArgs, &recallOut); err != nil {
		t.Fatalf("runRecall: %v", err)
	}

	if strings.Contains(recallOut.String(), "ollama") || strings.Contains(recallOut.String(), "недоступн") {
		t.Errorf("recall stdout = %q, содержит лог деградации embedder — должен уйти в stderr", recallOut.String())
	}
	if !strings.Contains(recallOut.String(), "проверки чистоты stdout") {
		t.Errorf("recall stdout = %q, не содержит ожидаемый факт", recallOut.String())
	}
	if !strings.Contains(logBuf.String(), "ollama") {
		t.Errorf("log-вывод = %q, ожидал предупреждение о недоступности ollama (значит warning потерялся, а не просто ушёл не в stdout)", logBuf.String())
	}
}

// TestCLIRecallEmptyQueryErrors проверяет, что пустой запрос (без позиционных
// аргументов) — ошибка валидации, а не пустой успешный recall.
func TestCLIRecallEmptyQueryErrors(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeTestConfig(t, dir, "alpha", []string{"user:*"}, "")
	dbPath := filepath.Join(dir, "memory.db")

	err := runRecall([]string{"-config", cfgPath, "-db", dbPath}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("runRecall с пустым запросом = nil, want ошибку")
	}
}

// TestSplitCSV проверяет разбор списков через запятую для флагов
// -scopes/-kinds/-tags: обрезка пробелов, отбрасывание пустых элементов, nil
// на пустом входе.
func TestSplitCSV(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"a", []string{"a"}},
		{"a,b,c", []string{"a", "b", "c"}},
		{" a , b ,, c ", []string{"a", "b", "c"}},
	}
	for _, tt := range tests {
		got := splitCSV(tt.in)
		if len(got) != len(tt.want) {
			t.Errorf("splitCSV(%q) = %v, want %v", tt.in, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("splitCSV(%q) = %v, want %v", tt.in, got, tt.want)
				break
			}
		}
	}
}

// TestCLISaveTaskFieldsAndRecallTypes (Этап 15, задача 15.2): `memo save`
// пробрасывает -type/-branch/-workflow в write path один в один (проверяется
// чтением записи из хранилища), а `memo recall -types epic` показывает только
// эпик — фильтр Query.Types доходит из CSV-флага.
func TestCLISaveTaskFieldsAndRecallTypes(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeTestConfig(t, dir, "alpha", []string{"user:*"}, "")
	dbPath := filepath.Join(dir, "memory.db")

	var epicOut bytes.Buffer
	epicArgs := []string{"-config", cfgPath, "-db", dbPath, "-kind", "task", "-summary", "Эпик доски задач CLI",
		"-type", "epic", "-content", "крупная цель: kanban-доска задач через CLI"}
	if err := runSave(epicArgs, strings.NewReader(""), &epicOut); err != nil {
		t.Fatalf("runSave(epic): %v", err)
	}

	var storyOut bytes.Buffer
	storyArgs := []string{"-config", cfgPath, "-db", dbPath, "-kind", "task", "-summary", "История поверхности CLI",
		"-type", "story", "-status", "doing", "-branch", "feat/15.2", "-workflow", "dev",
		"-content", "история: агент задаёт поля задачи через флаги kanban-доски CLI"}
	if err := runSave(storyArgs, strings.NewReader(""), &storyOut); err != nil {
		t.Fatalf("runSave(story): %v", err)
	}
	storyID := strings.TrimSpace(strings.TrimPrefix(storyOut.String(), "saved "))

	s, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer s.Close()
	got, err := s.Get(context.Background(), storyID)
	if err != nil {
		t.Fatalf("Get(%s): %v", storyID, err)
	}
	if got.Type != memory.TaskTypeStory || got.Status != memory.StatusDoing || got.Branch != "feat/15.2" || got.Workflow != "dev" {
		t.Errorf("сохранённая задача = type=%q status=%q branch=%q workflow=%q, want story/doing/feat/15.2/dev", got.Type, got.Status, got.Branch, got.Workflow)
	}

	var recallOut bytes.Buffer
	recallArgs := []string{"-config", cfgPath, "-db", dbPath, "-types", "epic", "kanban-доска задач CLI"}
	if err := runRecall(recallArgs, &recallOut); err != nil {
		t.Fatalf("runRecall(-types epic): %v", err)
	}
	if md := recallOut.String(); !strings.Contains(md, "Эпик доски задач CLI") || strings.Contains(md, "История поверхности CLI") {
		t.Errorf("runRecall -types epic должен показать только эпик, got:\n%s", md)
	}
}

// TestCLISaveTaskFieldsRejectedForNonTask: -type у не-задачи — ошибка write
// path (write.ErrTaskFieldNotApplicable) как есть, без молчаливого отбрасывания;
// неизвестный -type — write.ErrUnknownTaskType.
func TestCLISaveTaskFieldsRejectedForNonTask(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeTestConfig(t, dir, "alpha", []string{"user:*"}, "")
	dbPath := filepath.Join(dir, "memory.db")

	args := []string{"-config", cfgPath, "-db", dbPath, "-kind", "semantic", "-branch", "feat/x", "-content", "факт с веткой"}
	if err := runSave(args, strings.NewReader(""), &bytes.Buffer{}); !errors.Is(err, write.ErrTaskFieldNotApplicable) {
		t.Errorf("runSave(-kind semantic -branch) err = %v, want обёрнутый write.ErrTaskFieldNotApplicable", err)
	}

	args = []string{"-config", cfgPath, "-db", dbPath, "-kind", "task", "-summary", "h", "-type", "bug", "-content", "задача с неизвестным типом"}
	if err := runSave(args, strings.NewReader(""), &bytes.Buffer{}); !errors.Is(err, write.ErrUnknownTaskType) {
		t.Errorf("runSave(-type bug) err = %v, want обёрнутый write.ErrUnknownTaskType", err)
	}
}
