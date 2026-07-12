package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeHookConfig — как writeTestConfig в cli_test.go, но явно прописывает
// [store].path: у подкоманды `hook` (в отличие от recall/save) нет флага -db,
// поэтому путь к БД для тестов может прийти только через сам конфиг.
func writeHookConfig(t *testing.T, dir, project string, shared []string, dbPath string) string {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "[store]\npath = %q\n\n", dbPath)
	fmt.Fprintf(&b, "[scope]\nproject = %q\n", project)
	b.WriteString("shared = [")
	for i, s := range shared {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q", s)
	}
	b.WriteString("]\n")
	path := filepath.Join(dir, "agentmemory.toml")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
	return path
}

// TestRunHook_SessionStartFindsSavedFact проверяет основной сценарий
// session-start: факт, сохранённый заранее, попадает в бриф под
// hookBriefHeader — по этому заголовку в контексте видно происхождение блока.
func TestRunHook_SessionStartFindsSavedFact(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "memory.db")
	cfgPath := writeHookConfig(t, dir, "alpha", []string{"user:*"}, dbPath)

	saveArgs := []string{"-config", cfgPath, "-db", dbPath, "-kind", "semantic", "-content", "состояние проекта: этап 7 хуков в разработке"}
	if err := runSave(saveArgs, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatalf("runSave: %v", err)
	}

	var out bytes.Buffer
	stdin := strings.NewReader(`{"session_id":"s1","cwd":"` + dir + `","hook_event_name":"SessionStart","source":"startup"}`)
	if err := runHook([]string{"-event", "session-start", "-config", cfgPath}, stdin, &out); err != nil {
		t.Fatalf("runHook: %v", err)
	}
	if !strings.HasPrefix(out.String(), hookBriefHeader) {
		t.Fatalf("runHook stdout = %q, хочу префикс %q", out.String(), hookBriefHeader)
	}
	if !strings.Contains(out.String(), "этап 7 хуков") {
		t.Errorf("runHook stdout не содержит сохранённый факт: %s", out.String())
	}
}

// TestRunHook_SessionStartEmptyMemoryProducesEmptyOutput проверяет, что при
// пустой памяти (и заодно — при полностью пустом stdin, 0 байт) session-start
// не печатает ни заголовок, ни что-либо ещё: пустая память не должна вставлять
// в контекст пустой блок.
func TestRunHook_SessionStartEmptyMemoryProducesEmptyOutput(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "memory.db")
	cfgPath := writeHookConfig(t, dir, "alpha", []string{"user:*"}, dbPath)

	var out bytes.Buffer
	if err := runHook([]string{"-event", "session-start", "-config", cfgPath}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("runHook: %v", err)
	}
	if out.String() != "" {
		t.Errorf("runHook на пустой памяти = %q, хочу пустой stdout", out.String())
	}
}

// TestRunHook_PromptSubmitUsesStdinPrompt проверяет, что query для recall
// берётся из поля "prompt" JSON-события на stdin.
func TestRunHook_PromptSubmitUsesStdinPrompt(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "memory.db")
	cfgPath := writeHookConfig(t, dir, "alpha", []string{"user:*"}, dbPath)

	saveArgs := []string{"-config", cfgPath, "-db", dbPath, "-kind", "semantic", "-content", "уникальный факт про хук prompt-submit"}
	if err := runSave(saveArgs, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatalf("runSave: %v", err)
	}

	var out bytes.Buffer
	stdin := strings.NewReader(`{"session_id":"s1","prompt":"расскажи про хук prompt-submit подробнее"}`)
	if err := runHook([]string{"-event", "prompt-submit", "-config", cfgPath}, stdin, &out); err != nil {
		t.Fatalf("runHook: %v", err)
	}
	if !strings.HasPrefix(out.String(), hookBriefHeader) {
		t.Fatalf("runHook stdout = %q, хочу префикс %q", out.String(), hookBriefHeader)
	}
	if !strings.Contains(out.String(), "хук prompt-submit") {
		t.Errorf("runHook stdout не содержит сохранённый факт: %s", out.String())
	}
}

// TestRunHook_PromptSubmitShortPromptNoOp проверяет порог длины: короткий
// prompt (< hookPromptSubmitMinRunes) не запускает recall вовсе.
func TestRunHook_PromptSubmitShortPromptNoOp(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "memory.db")
	cfgPath := writeHookConfig(t, dir, "alpha", []string{"user:*"}, dbPath)

	var out bytes.Buffer
	stdin := strings.NewReader(`{"prompt":"ок"}`)
	if err := runHook([]string{"-event", "prompt-submit", "-config", cfgPath}, stdin, &out); err != nil {
		t.Fatalf("runHook: %v", err)
	}
	if out.String() != "" {
		t.Errorf("runHook с коротким prompt = %q, хочу пустой stdout (порог длины)", out.String())
	}
}

// TestRunHook_MissingDBDegradesToEmptyOutput — ЖЕЛЕЗНЫЙ инвариант: недоступная
// БД (Store.Open падает — здесь через каталог, у которого один из компонентов
// пути на самом деле файл, а не каталог) даёт пустой stdout и ошибку только
// для лога вызывающей стороны, не для печати.
func TestRunHook_MissingDBDegradesToEmptyOutput(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	dbPath := filepath.Join(blocker, "sub", "memory.db") // MkdirAll упадёт: blocker — файл, не каталог
	cfgPath := writeHookConfig(t, dir, "alpha", []string{"user:*"}, dbPath)

	var out bytes.Buffer
	err := runHook([]string{"-event", "session-start", "-config", cfgPath}, strings.NewReader(""), &out)
	if err == nil {
		t.Fatalf("runHook = nil error, хочу ошибку открытия хранилища")
	}
	if out.String() != "" {
		t.Errorf("runHook при недоступной БД вывел %q, хочу пустой stdout (ЖЕЛЕЗНЫЙ инвариант)", out.String())
	}
}

// TestRunHook_MalformedStdinDegradesToEmptyOutput — ЖЕЛЕЗНЫЙ инвариант:
// битый JSON на stdin (не путать с пустым stdin — см.
// TestRunHook_SessionStartEmptyMemoryProducesEmptyOutput) даёт пустой stdout.
func TestRunHook_MalformedStdinDegradesToEmptyOutput(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "memory.db")
	cfgPath := writeHookConfig(t, dir, "alpha", []string{"user:*"}, dbPath)

	var out bytes.Buffer
	err := runHook([]string{"-event", "prompt-submit", "-config", cfgPath}, strings.NewReader("{ битый json без закрытия"), &out)
	if err == nil {
		t.Fatalf("runHook = nil error, хочу ошибку разбора stdin")
	}
	if out.String() != "" {
		t.Errorf("runHook при битом stdin вывел %q, хочу пустой stdout (ЖЕЛЕЗНЫЙ инвариант)", out.String())
	}
}

// TestRunHook_MalformedConfigDegradesToEmptyOutput — ЖЕЛЕЗНЫЙ инвариант: битый
// TOML-конфиг даёт пустой stdout, а не панику/крэш.
func TestRunHook_MalformedConfigDegradesToEmptyOutput(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agentmemory.toml")
	if err := os.WriteFile(cfgPath, []byte("key = [1, 2"), 0o644); err != nil { // незакрытый массив — невалидный TOML
		t.Fatalf("WriteFile: %v", err)
	}

	var out bytes.Buffer
	err := runHook([]string{"-event", "session-start", "-config", cfgPath}, strings.NewReader(""), &out)
	if err == nil {
		t.Fatalf("runHook = nil error, хочу ошибку разбора конфига")
	}
	if out.String() != "" {
		t.Errorf("runHook при битом конфиге вывел %q, хочу пустой stdout (ЖЕЛЕЗНЫЙ инвариант)", out.String())
	}
}

// TestRunHook_UnknownEventDegradesToEmptyOutput проверяет, что незнакомое или
// не заданное значение -event тоже деградирует до пустого stdout, а не паники.
func TestRunHook_UnknownEventDegradesToEmptyOutput(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "memory.db")
	cfgPath := writeHookConfig(t, dir, "alpha", []string{"user:*"}, dbPath)

	var out bytes.Buffer
	err := runHook([]string{"-event", "bogus-event", "-config", cfgPath}, strings.NewReader(""), &out)
	if err == nil {
		t.Fatalf("runHook = nil error, хочу ошибку неизвестного -event")
	}
	if out.String() != "" {
		t.Errorf("runHook с неизвестным -event вывел %q, хочу пустой stdout", out.String())
	}
}
