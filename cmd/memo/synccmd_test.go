package main

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"agentmemory/internal/config"
	"agentmemory/internal/memory"
	"agentmemory/internal/memory/store"
)

// unreachableTarget — цель на заведомо закрытом порту: Dial падает сразу.
var unreachableTarget = store.SyncTarget{Host: "127.0.0.1", Port: 1, User: "memo", DBName: "memo", SSLMode: "disable"}

// syncCmd — runSyncCmd с выводом в буфер.
func syncCmd(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runSyncCmd(args, strings.NewReader(stdin), &out)
	return out.String(), err
}

// TestRunSyncCmd_Flags: неверные сочетания флагов отвергаются до открытия БД.
func TestRunSyncCmd_Flags(t *testing.T) {
	for _, args := range [][]string{
		{"-status", "-apply-schema", "-project", "a"},
		{"-apply-schema", "-rotate-base", "-project", "a"},
		{"-apply-schema"},
		{"-rotate-base"},
		{"лишнее"},
		{"status", "ещё"},
	} {
		if _, err := syncCmd(t, "", args...); err == nil {
			t.Errorf("memo sync %q: хочу ошибку", args)
		}
	}
}

// TestRunSyncCmd_NoTargets: целей нет — подсказка и успех; неизвестная цель — ошибка.
func TestRunSyncCmd_NoTargets(t *testing.T) {
	db := t.TempDir() + "/memory.db"
	for _, args := range [][]string{{"-db", db}, {"-db", db, "status"}} {
		out, err := syncCmd(t, "", args...)
		if err != nil || !strings.Contains(out, "целей синхронизации нет") {
			t.Errorf("memo sync %q = %q, %v; хочу подсказку без ошибки", args, out, err)
		}
	}
	if _, err := syncCmd(t, "", "-db", db, "-project", "нет"); err == nil || !strings.Contains(err.Error(), "цели \"нет\" нет") {
		t.Errorf("неизвестная цель: %v", err)
	}
}

// TestRunSyncCmd_Run: недоступная цель — ❌ и ошибка (exit 1 через cobra), цель с
// выключенными тумблерами и занятая другим процессом — пропуск без ошибки.
func TestRunSyncCmd_Run(t *testing.T) {
	ctx := context.Background()
	l, _, db := newSyncLocal(t, unreachableTarget)
	off := unreachableTarget
	off.Project = "beta"
	if _, err := l.PutTarget(ctx, off); err != nil {
		t.Fatal(err)
	}

	out, err := syncCmd(t, "", "-db", db)
	if err == nil || !strings.Contains(err.Error(), "1 из 2") {
		t.Fatalf("ошибка = %v, хочу «1 из 2»", err)
	}
	if !strings.Contains(out, "❌ alpha: отправлено 0, получено 0, конфликтов 0; ошибка:") || !strings.Contains(out, "⏭ beta: пропущен — «Отправлять» и «Загружать» выключены") {
		t.Errorf("вывод:\n%s", out)
	}

	var stdout, stderr bytes.Buffer
	exitCode := 0
	root := newRootCmd(strings.NewReader(""), &stdout, &stderr, &exitCode)
	root.SetArgs([]string{"sync", "-db", db, "-project", "alpha"})
	if err := root.Execute(); err != nil || exitCode != 1 || !strings.Contains(stderr.String(), "memo sync: синхронизация не удалась") {
		t.Errorf("cobra: err=%v exit=%d stderr=%q; хочу exit 1", err, exitCode, stderr.String())
	}

	if ok, err := l.Lease(ctx, "alpha", "другой процесс", 1<<40); err != nil || !ok {
		t.Fatalf("Lease: %v, %v", ok, err)
	}
	out, err = syncCmd(t, "", "-db", db, "-project", "alpha")
	if err != nil || !strings.Contains(out, "⏭ alpha: пропущен — синхронизация проекта уже идёт") {
		t.Errorf("занятая цель: %q, %v; хочу пропуск без ошибки", out, err)
	}
}

// TestRunSyncCmd_Status: состояние из локальной БД без сети; пароль — только
// «задан/не задан», значения в выводе нет.
func TestRunSyncCmd_Status(t *testing.T) {
	_, dir, db := newSyncLocal(t, unreachableTarget)

	out, err := syncCmd(t, "", "-db", db, "-status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, want := range []string{"alpha: отправлять — да, загружать — да", "memo@127.0.0.1:1/memo (sslmode=disable)",
		"пароль: не задан (MEMO_SYNC_PASSWORD_ALPHA", "прогонов ещё не было", "неотправленных: 0"} {
		if !strings.Contains(out, want) {
			t.Errorf("нет %q в выводе:\n%s", want, out)
		}
	}

	const pw = "секрет-статуса"
	if err := saveSyncPassword(dir, "MEMO_SYNC_PASSWORD_ALPHA", pw); err != nil {
		t.Fatal(err)
	}
	if _, err := syncCmd(t, "", "-db", db); err == nil {
		t.Fatal("прогон недоступной цели без ошибки")
	}
	out, err = syncCmd(t, "", "-db", db, "status")
	if err != nil || !strings.Contains(out, "пароль: задан") || !strings.Contains(out, "— ошибка:") || strings.Contains(out, pw) {
		t.Errorf("status после прогона: %v\n%s", err, out)
	}
}

// TestRunSyncCmd_SchemaUnreachable: операции над схемой недоступной цели —
// ошибка подключения, без вопроса; неизвестная цель — подсказка.
func TestRunSyncCmd_SchemaUnreachable(t *testing.T) {
	_, _, db := newSyncLocal(t, unreachableTarget)
	for _, mode := range []string{"-apply-schema", "-rotate-base"} {
		out, err := syncCmd(t, "y\n", "-db", db, mode, "-project", "alpha")
		if err == nil || strings.Contains(out, "[y/N]") {
			t.Errorf("%s: %q, %v; хочу ошибку без вопроса", mode, out, err)
		}
		if _, err := syncCmd(t, "y\n", "-db", db, mode, "-project", "нет"); err == nil || !strings.Contains(err.Error(), "memo web") {
			t.Errorf("%s неизвестной цели: %v", mode, err)
		}
	}
}

// TestConfirm: «y»/«да» — согласие; прочее и конец ввода — отказ.
func TestConfirm(t *testing.T) {
	for in, want := range map[string]bool{"y\n": true, "Да\n": true, " yes ": true, "n\n": false, "": false, "давай\n": false} {
		var out bytes.Buffer
		if got := confirm(strings.NewReader(in), &out, "Вопрос?"); got != want || !strings.Contains(out.String(), "Вопрос? [y/N]: ") {
			t.Errorf("confirm(%q) = %v, вывод %q; хочу %v", in, got, out.String(), want)
		}
	}
}

// TestSyncDoctor: у цели — своя секция; недоступная общая база — ⚠️, а не ❌,
// пароля в отчёте нет; без целей секций нет.
func TestSyncDoctor(t *testing.T) {
	ctx := context.Background()
	_, dir, db := newSyncLocal(t, unreachableTarget)
	const pw = "секрет-доктора"
	if err := saveSyncPassword(dir, "MEMO_SYNC_PASSWORD_ALPHA", pw); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenSQLite(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	secs := syncSections(ctx, st, config.StoreConfig{Driver: "sqlite", Path: db})
	if len(secs) != 1 {
		t.Fatalf("секций %d, хочу 1: %+v", len(secs), secs)
	}
	s := secs[0]
	if s.Name != "Синхронизация alpha" || s.Status != statusWarn || !strings.Contains(s.Detail, "недоступна") ||
		!strings.Contains(s.Detail, "пароль: задан") || strings.Contains(s.Detail, pw) {
		t.Errorf("секция: %+v", s)
	}

	empty, err := store.OpenSQLite(t.TempDir() + "/memory.db")
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	if secs := syncSections(ctx, empty, config.StoreConfig{Driver: "sqlite"}); len(secs) != 0 {
		t.Errorf("без целей секции: %+v", secs)
	}
	if secs := syncSections(ctx, nil, config.StoreConfig{}); secs != nil {
		t.Errorf("без хранилища секции: %+v", secs)
	}
}

// TestRunSyncCmd_LivePG: полный путь на живой PostgreSQL во временной схеме —
// отказ от подтверждения ничего не меняет, -yes применяет схему, прогон переносит
// запись, ротация меняет base_id, doctor показывает ✅. Без MEMO_TEST_POSTGRES_DSN —
// пропуск.
func TestRunSyncCmd_LivePG(t *testing.T) {
	dsn := os.Getenv("MEMO_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("MEMO_TEST_POSTGRES_DSN не задан — пропуск проверки memo sync на живой общей базе")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("разбор DSN: %v", err)
	}
	newTestPgSchema(t, dsn, "sync_cli_", false) // все новые соединения процесса — во временную схему
	port, _ := strconv.Atoi(u.Port())
	pw, _ := u.User.Password()
	l, dir, db := newSyncLocal(t, store.SyncTarget{Host: u.Hostname(), Port: port, User: u.User.Username(),
		DBName: strings.TrimPrefix(u.Path, "/"), SSLMode: u.Query().Get("sslmode")})
	if err := saveSyncPassword(dir, "MEMO_SYNC_PASSWORD_ALPHA", pw); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	hubSchema := func() store.HubSchema {
		t.Helper()
		hub, err := hubDialer{local: l, envDir: dir}.open(ctx, "alpha")
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer hub.Close()
		s, err := hub.Schema(ctx)
		if err != nil {
			t.Fatalf("Schema: %v", err)
		}
		return s
	}

	if _, err := syncCmd(t, "", "-db", db); err == nil {
		t.Fatal("прогон без схемы общей базы прошёл")
	}
	out, err := syncCmd(t, "n\n", "-db", db, "-apply-schema", "-project", "alpha")
	if !errors.Is(err, errSyncDeclined) || !strings.Contains(out, "схемы нет") || !hubSchema().Empty {
		t.Fatalf("отказ: %q, %v", out, err)
	}
	if out, err = syncCmd(t, "", "-db", db, "-apply-schema", "-project", "alpha", "-yes"); err != nil || !hubSchema().Current() {
		t.Fatalf("-apply-schema -yes: %q, %v", out, err)
	}
	if out, err = syncCmd(t, "", "-db", db, "-apply-schema", "-project", "alpha"); err != nil || !strings.Contains(out, "применять нечего") {
		t.Fatalf("повторное применение: %q, %v", out, err)
	}

	st, err := store.OpenSQLite(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := memory.Memory{Id: ulid.Make().String(), Kind: memory.KindSemantic, Scope: "project:alpha", Content: "факт для общей базы",
		Importance: 3, CreatedAt: 1000, UpdatedAt: 1000}
	if err := st.Put(ctx, m); err != nil {
		t.Fatal(err)
	}
	if out, err = syncCmd(t, "", "-db", db); err != nil || !strings.Contains(out, "✅ alpha: отправлено 1") {
		t.Fatalf("прогон: %q, %v", out, err)
	}

	if out, err = syncCmd(t, "y\n", "-db", db, "-rotate-base", "-project", "alpha"); err != nil || !strings.Contains(out, "base_id общей базы обновлён") {
		t.Fatalf("-rotate-base: %q, %v", out, err)
	}

	secs := syncSections(ctx, st, config.StoreConfig{Driver: "sqlite", Path: db})
	// Утечку пароля здесь не проверить: тестовый пароль может совпасть с частью
	// адреса — её проверяет TestSyncDoctor с заведомо уникальным паролем.
	if len(secs) != 1 || secs[0].Status != statusOK || !strings.Contains(secs[0].Detail, "совпадает") {
		t.Errorf("doctor: %+v", secs)
	}
}
