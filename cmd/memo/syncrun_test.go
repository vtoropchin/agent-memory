package main

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"agentmemory/internal/memory/store"
)

// newSyncLocal открывает SQLite во временном каталоге и заводит цель alpha с
// параметрами подключения t; возвращает локальное состояние, каталог БД и путь к файлу БД.
func newSyncLocal(t *testing.T, target store.SyncTarget) (*store.SyncLocal, string, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "memory.db")
	s, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	l := s.SyncLocal()
	target.Project, target.Push, target.Pull = "alpha", true, true
	if _, err := l.PutTarget(context.Background(), target); err != nil {
		t.Fatalf("PutTarget: %v", err)
	}
	return l, dir, dbPath
}

// TestSyncPassword: значение из .env каталога БД приоритетнее окружения
// процесса; окружение — только если в файле ключа нет; нигде нет — пусто.
func TestSyncPassword(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MEMO_SYNC_PASSWORD_A", "из-окружения")
	t.Setenv("MEMO_SYNC_PASSWORD_B", "из-окружения-b")
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("MEMO_SYNC_PASSWORD_A=из-файла\nMEMO_SYNC_PASSWORD_C=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"MEMO_SYNC_PASSWORD_A": "из-файла",
		"MEMO_SYNC_PASSWORD_B": "из-окружения-b",
		"MEMO_SYNC_PASSWORD_C": "",
		"MEMO_SYNC_PASSWORD_D": "",
	} {
		if got, err := syncPassword(dir, name); err != nil || got != want {
			t.Errorf("syncPassword(%s) = %q, %v; хочу %q", name, got, err, want)
		}
	}
	if got, err := syncPassword(t.TempDir(), "MEMO_SYNC_PASSWORD_B"); err != nil || got != "из-окружения-b" {
		t.Errorf("без .env: %q, %v", got, err)
	}
}

// TestSaveSyncPassword: слияние с прочими ключами, замена на месте, права 0600,
// каталог 0700 при создании, пароль с пробелами, кавычками, '=' и '#' читается
// обратно как был, временных файлов не остаётся, перевод строки отвергается.
func TestSaveSyncPassword(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agentmemory")
	if err := saveSyncPassword(dir, "MEMO_SYNC_PASSWORD_A", "первый"); err != nil {
		t.Fatalf("saveSyncPassword: %v", err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("каталог: %v, %v; хочу 0700", info.Mode().Perm(), err)
	}
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, append(mustRead(t, path), []byte("# комментарий\nDEEPSEEK_API_KEY=ключ\n")...), 0o600); err != nil {
		t.Fatal(err)
	}

	tricky := ` 'пароль = "с # кавычками" ' `
	if err := saveSyncPassword(dir, "MEMO_SYNC_PASSWORD_A", tricky); err != nil {
		t.Fatalf("saveSyncPassword: %v", err)
	}
	if err := saveSyncPassword(dir, "MEMO_SYNC_PASSWORD_B", "второй"); err != nil {
		t.Fatalf("saveSyncPassword: %v", err)
	}
	if got, _ := syncPassword(dir, "MEMO_SYNC_PASSWORD_A"); got != tricky {
		t.Errorf("пароль прочитан как %q, хочу %q", got, tricky)
	}
	if got, _ := syncPassword(dir, "MEMO_SYNC_PASSWORD_B"); got != "второй" {
		t.Errorf("второй пароль = %q", got)
	}
	content := string(mustRead(t, path))
	if strings.Count(content, "MEMO_SYNC_PASSWORD_A=") != 1 || !strings.Contains(content, "DEEPSEEK_API_KEY=ключ") || !strings.Contains(content, "# комментарий") {
		t.Errorf(".env после слияния:\n%s", content)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("права .env = %v, хочу 0600", info.Mode().Perm())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("в каталоге остались лишние файлы: %v", entries)
	}

	err := saveSyncPassword(dir, "MEMO_SYNC_PASSWORD_A", "секрет\nвторая строка")
	if err == nil || strings.Contains(err.Error(), "секрет") {
		t.Errorf("пароль с переводом строки: %v, хочу ошибку без значения", err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestRedactSecret: пароль в любой форме вырезается из текста ошибки.
func TestRedactSecret(t *testing.T) {
	const pw = "p@ss w/rd"
	err := redactSecret(errors.New("dsn postgres://u:"+url.UserPassword("u", pw).String()[2:]+"@h, raw "+pw+", query "+url.QueryEscape(pw)), pw)
	if strings.Contains(err.Error(), pw) || strings.Contains(err.Error(), url.QueryEscape(pw)) || strings.Contains(err.Error(), "p%40ss") {
		t.Errorf("пароль остался в ошибке: %v", err)
	}
	plain := errors.New("без секрета")
	if redactSecret(plain, pw) != plain || redactSecret(plain, "") != plain {
		t.Errorf("ошибка без секрета должна возвращаться как есть")
	}
}

// TestRunSync_UnreachableNoLeak: недоступная общая база — ошибка в итоге и в
// sync_state, без пароля; ни в файле БД, ни в ошибке пароля нет. Без пароля —
// подсказка, где его задать.
func TestRunSync_UnreachableNoLeak(t *testing.T) {
	ctx := context.Background()
	l, dir, dbPath := newSyncLocal(t, store.SyncTarget{Host: "127.0.0.1", Port: 1, User: "memo", DBName: "memo", SSLMode: "disable"})

	_, err := runSync(ctx, l, dir, "alpha", "alice")
	if err == nil || !strings.Contains(err.Error(), "пароль не задан: MEMO_SYNC_PASSWORD_ALPHA") {
		t.Fatalf("runSync без пароля: %v, хочу подсказку про пароль", err)
	}

	const pw = "очень-секретный-пароль"
	if err := saveSyncPassword(dir, "MEMO_SYNC_PASSWORD_ALPHA", pw); err != nil {
		t.Fatal(err)
	}
	_, err = runSync(ctx, l, dir, "alpha", "alice")
	if err == nil || strings.Contains(err.Error(), pw) || strings.Contains(err.Error(), "пароль не задан") {
		t.Fatalf("runSync с паролем: %v, хочу ошибку подключения без пароля и без подсказки", err)
	}
	st, err := l.State(ctx, "alpha")
	if err != nil || st.LastError == "" || strings.Contains(st.LastError, pw) {
		t.Fatalf("sync_state: %+v, %v; хочу ошибку без пароля", st, err)
	}
	for _, f := range []string{dbPath, dbPath + "-wal"} {
		if b, err := os.ReadFile(f); err == nil && strings.Contains(string(b), pw) {
			t.Errorf("пароль попал в %s", f)
		}
	}
}

// TestRunSync_Busy: цель держит другой процесс — прогон не идёт и итог не пишет.
func TestRunSync_Busy(t *testing.T) {
	ctx := context.Background()
	l, dir, _ := newSyncLocal(t, store.SyncTarget{Host: "127.0.0.1", Port: 1, User: "memo", DBName: "memo"})
	if ok, err := l.Lease(ctx, "alpha", "другой процесс", 1<<40); err != nil || !ok {
		t.Fatalf("Lease: %v, %v", ok, err)
	}
	if _, err := runSync(ctx, l, dir, "alpha", "alice"); !errors.Is(err, errSyncBusy) {
		t.Fatalf("runSync = %v, хочу errSyncBusy", err)
	}
	if st, _ := l.State(ctx, "alpha"); st.LastRunAt != 0 {
		t.Errorf("занятая цель записала итог: %+v", st)
	}
	if _, err := runSync(ctx, l, dir, "beta", "alice"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("runSync без цели = %v, хочу ErrNotFound", err)
	}
}

// TestHubDialer_LivePG: Dial по цели с паролем из .env каталога БД подключается к
// живой общей базе и читает её состояние без DDL; неверный пароль — ошибка без
// пароля. Без MEMO_TEST_POSTGRES_DSN — пропуск.
func TestHubDialer_LivePG(t *testing.T) {
	dsn := os.Getenv("MEMO_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("MEMO_TEST_POSTGRES_DSN не задан — пропуск проверки подключения к общей базе")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("разбор DSN: %v", err)
	}
	port, _ := strconv.Atoi(u.Port())
	pw, _ := u.User.Password()
	l, dir, _ := newSyncLocal(t, store.SyncTarget{Host: u.Hostname(), Port: port, User: u.User.Username(),
		DBName: strings.TrimPrefix(u.Path, "/"), SSLMode: u.Query().Get("sslmode")})
	ctx := context.Background()
	d := hubDialer{local: l, envDir: dir}

	if err := saveSyncPassword(dir, "MEMO_SYNC_PASSWORD_ALPHA", pw); err != nil {
		t.Fatal(err)
	}
	sh, err := d.Dial(ctx, "alpha")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer sh.Close()
	if b, err := sh.Base(ctx); err != nil || b.XMax == 0 {
		t.Fatalf("Base = %+v, %v", b, err)
	}

	const wrong = "заведомо-неверный-пароль"
	if err := saveSyncPassword(dir, "MEMO_SYNC_PASSWORD_ALPHA", wrong); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Dial(ctx, "alpha"); err == nil || strings.Contains(err.Error(), wrong) {
		t.Fatalf("Dial с неверным паролем: %v, хочу ошибку без пароля", err)
	}
}

// TestRunSync_SameProcessBusy: lease, взятый этим же процессом под другим
// прогоном, тоже занят — владелец свой на каждый прогон, а не на процесс.
func TestRunSync_SameProcessBusy(t *testing.T) {
	ctx := context.Background()
	l, dir, _ := newSyncLocal(t, store.SyncTarget{Host: "127.0.0.1", Port: 1, User: "memo", DBName: "memo"})
	if ok, err := l.Lease(ctx, "alpha", syncRunHolder(), 1<<40); err != nil || !ok {
		t.Fatalf("Lease: %v, %v", ok, err)
	}
	if _, err := runSync(ctx, l, dir, "alpha", "alice"); !errors.Is(err, errSyncBusy) {
		t.Fatalf("runSync при прогоне этого же процесса = %v, хочу errSyncBusy", err)
	}
}

// TestDeleteSyncPassword: удаляется только своя строка, прочие остаются; значение,
// влитое из этого же файла в окружение, снимается; нет файла — no-op.
func TestDeleteSyncPassword(t *testing.T) {
	dir := t.TempDir()
	if err := deleteSyncPassword(dir, "MEMO_SYNC_PASSWORD_A"); err != nil {
		t.Fatalf("без .env: %v", err)
	}
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("# комментарий\nMEMO_SYNC_PASSWORD_A=секрет\nDEEPSEEK_API_KEY=ключ\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEMO_SYNC_PASSWORD_A", "секрет")
	if err := deleteSyncPassword(dir, "MEMO_SYNC_PASSWORD_A"); err != nil {
		t.Fatalf("deleteSyncPassword: %v", err)
	}
	if got := string(mustRead(t, path)); got != "# комментарий\nDEEPSEEK_API_KEY=ключ\n" {
		t.Errorf(".env после удаления:\n%s", got)
	}
	if got, _ := syncPassword(dir, "MEMO_SYNC_PASSWORD_A"); got != "" {
		t.Errorf("пароль после удаления = %q, хочу пусто (окружение тоже снято)", got)
	}
}
