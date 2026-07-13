// cobra_test.go — регрессия задачи 11.1: миграция диспетчера подкоманд на
// cobra не должна была изменить ни маршрутизацию (голый `memoryd -config X`
// без подкоманды по-прежнему поднимает сервер — обратная совместимость с уже
// установленными .mcp.json), ни контракты exit-кода/разделения stdout-stderr
// каждой подкоманды. Тесты идут через newRootCmd(...).Execute() с буферами
// вместо os.Stdin/os.Stdout/os.Stderr и читают *exitCode вместо os.Exit —
// поэтому ничего не завершает процесс теста. Сервер (run()) специально нигде
// не запускается взаправду (он блокирует на stdio) — проверяется только то,
// что root.Find без имени подкоманды резолвится в саму root-команду, т.е. её
// RunE (серверный путь) — это то, что вызовется.
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRootCmd_NoSubcommandRoutesToServer проверяет, что аргументы, которые
// использует уже установленный .mcp.json (`-config <path>`, без подкоманды),
// и полностью пустой вызов маршрутизируются в саму root-команду — то есть её
// RunE (серверный путь run()), а не в одну из подкоманд.
func TestRootCmd_NoSubcommandRoutesToServer(t *testing.T) {
	exitCode := 0
	root := newRootCmd(strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}, &exitCode)

	tests := [][]string{
		{"-config", "/some/agentmemory.toml"},
		{},
		{"-db", "/some/memory.db"},
	}
	for _, args := range tests {
		cmd, _, err := root.Find(args)
		if err != nil {
			t.Fatalf("root.Find(%v): %v", args, err)
		}
		if cmd != root {
			t.Errorf("root.Find(%v) = %q, хочу саму root-команду (серверный путь)", args, cmd.Name())
		}
	}
}

// TestRootCmd_SubcommandsRouteByName проверяет, что имя подкоманды находит
// соответствующую cobra.Command, а не остаётся на root.
func TestRootCmd_SubcommandsRouteByName(t *testing.T) {
	exitCode := 0
	root := newRootCmd(strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}, &exitCode)

	for _, name := range []string{"init", "web", "recall", "save", "hook", "doctor", "export", "import"} {
		cmd, _, err := root.Find([]string{name, "-config", "x"})
		if err != nil {
			t.Fatalf("root.Find([%s ...]): %v", name, err)
		}
		if cmd.Name() != name {
			t.Errorf("root.Find([%s ...]) = %q, хочу %q", name, cmd.Name(), name)
		}
	}
}

// TestRootCmd_TopLevelHelp проверяет, что "-h"/"--help"/"help" в качестве
// единственного аргумента верхнего уровня печатают полную справку (writeUsage)
// в stdout и не трогают exitCode — тот же путь, что был у isHelpArg-проверки в
// main() до миграции на cobra.
func TestRootCmd_TopLevelHelp(t *testing.T) {
	for _, arg := range []string{"-h", "-help", "--help", "help"} {
		exitCode := 0
		var stdout bytes.Buffer
		root := newRootCmd(strings.NewReader(""), &stdout, &bytes.Buffer{}, &exitCode)
		root.SetArgs([]string{arg})
		if err := root.Execute(); err != nil {
			t.Fatalf("Execute(%q): %v", arg, err)
		}
		if exitCode != 0 {
			t.Errorf("Execute(%q) exitCode = %d, хочу 0", arg, exitCode)
		}
		if !strings.Contains(stdout.String(), "memoryd init") || !strings.Contains(stdout.String(), "memoryd doctor") {
			t.Errorf("Execute(%q) stdout не похож на writeUsage(): %s", arg, stdout.String())
		}
	}
}

// TestCobraHook_ErrorYieldsEmptyStdoutAndZeroExit — регрессия ЖЕЛЕЗНОГО
// инварианта hook.go: неизвестное -event (или любая другая ошибка runHook)
// должно давать пустой stdout и exitCode 0, а не 1 — cobra не должна была
// сломать это через свою обычную "ошибка -> ненулевой код".
func TestCobraHook_ErrorYieldsEmptyStdoutAndZeroExit(t *testing.T) {
	exitCode := 0
	var stdout bytes.Buffer
	root := newRootCmd(strings.NewReader(""), &stdout, &bytes.Buffer{}, &exitCode)
	root.SetArgs([]string{"hook", "-event", "bogus-event"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("exitCode = %d, хочу 0 (hook никогда не ломает сессию)", exitCode)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, хочу пусто", stdout.String())
	}
}

// TestCobraRecall_EmptyQueryExitsOneStdoutClean проверяет контракт recall:
// ошибка (пустой запрос) -> exitCode 1, stdout остаётся чистым (без usage
// cobra), ошибка идёт в stderr с префиксом "memoryd recall:".
func TestCobraRecall_EmptyQueryExitsOneStdoutClean(t *testing.T) {
	exitCode := 0
	var stdout, stderr bytes.Buffer
	root := newRootCmd(strings.NewReader(""), &stdout, &stderr, &exitCode)
	root.SetArgs([]string{"recall"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if exitCode != 1 {
		t.Errorf("exitCode = %d, хочу 1", exitCode)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, хочу пусто (ошибка не должна попадать в stdout)", stdout.String())
	}
	if !strings.Contains(stderr.String(), "memoryd recall:") {
		t.Errorf("stderr = %q, хочу префикс \"memoryd recall:\"", stderr.String())
	}
}

// TestCobraRecall_SuccessQuietEmptyBrief проверяет успешный путь recall с
// -quiet на свежей (пустой) БД: exitCode 0, stdout пуст (Count==0 + -quiet),
// без служебного вывода cobra.
func TestCobraRecall_SuccessQuietEmptyBrief(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "memory.db")

	exitCode := 0
	var stdout, stderr bytes.Buffer
	root := newRootCmd(strings.NewReader(""), &stdout, &stderr, &exitCode)
	root.SetArgs([]string{"recall", "-db", dbPath, "-quiet", "запрос, для которого ничего не найдётся"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("exitCode = %d, хочу 0; stderr=%s", exitCode, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, хочу пусто (-quiet, пустой бриф)", stdout.String())
	}
}

// TestCobraSave_SuccessPrintsSavedID проверяет успешный путь save: exitCode 0,
// stdout содержит "saved <id>" и ничего больше служебного от cobra.
func TestCobraSave_SuccessPrintsSavedID(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeTestConfig(t, dir, "alpha", []string{"user:*"}, "")
	dbPath := filepath.Join(dir, "memory.db")

	exitCode := 0
	var stdout, stderr bytes.Buffer
	root := newRootCmd(strings.NewReader(""), &stdout, &stderr, &exitCode)
	root.SetArgs([]string{"save", "-config", cfgPath, "-db", dbPath, "-kind", "semantic", "-content", "факт для cobra_test"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("exitCode = %d, хочу 0; stderr=%s", exitCode, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "saved ") {
		t.Errorf("stdout = %q, хочу префикс \"saved \"", stdout.String())
	}
}

// TestCobraDoctor_ExitCodeMatchesReport проверяет, что exitCode берётся из
// отчёта doctor, а не из простого "err != nil": несуществующий файл БД —
// секция «БД» валится (❌), что даёт ExitCode=1 БЕЗ ошибки runDoctor (файл
// конфига/флаги валидны, отчёт строится и печатается штатно).
func TestCobraDoctor_ExitCodeMatchesReport(t *testing.T) {
	dir := t.TempDir()
	missingDB := filepath.Join(dir, "does-not-exist.db")

	exitCode := 0
	var stdout, stderr bytes.Buffer
	root := newRootCmd(strings.NewReader(""), &stdout, &stderr, &exitCode)
	root.SetArgs([]string{"doctor", "-db", missingDB})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if exitCode != 1 {
		t.Errorf("exitCode = %d, хочу 1 (отчёт: БД не найдена -> ❌)", exitCode)
	}
	if !strings.Contains(stdout.String(), "БД") {
		t.Errorf("stdout не содержит секцию БД: %s", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, хочу пусто (это отчёт, а не ошибка построения отчёта)", stderr.String())
	}
}

// TestCobraDoctor_ConfigErrorExitsOneViaErr проверяет вторую ветку doctor:
// битый TOML-конфиг не даёт построить отчёт вовсе — runDoctor возвращает err,
// exitCode=1 идёт через ветку ошибки, а не отчёта, и сообщение попадает в
// stderr с префиксом "memoryd doctor:".
func TestCobraDoctor_ConfigErrorExitsOneViaErr(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "broken.toml")
	if err := os.WriteFile(cfgPath, []byte("[store\npath = broken"), 0o644); err != nil {
		t.Fatalf("запись битого TOML: %v", err)
	}

	exitCode := 0
	var stdout, stderr bytes.Buffer
	root := newRootCmd(strings.NewReader(""), &stdout, &stderr, &exitCode)
	root.SetArgs([]string{"doctor", "-config", cfgPath})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if exitCode != 1 {
		t.Errorf("exitCode = %d, хочу 1", exitCode)
	}
	if !strings.Contains(stderr.String(), "memoryd doctor:") {
		t.Errorf("stderr = %q, хочу префикс \"memoryd doctor:\"", stderr.String())
	}
}
