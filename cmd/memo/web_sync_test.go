package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentmemory/internal/config"
	"agentmemory/internal/memory"
	"agentmemory/internal/memory/embed"
	"agentmemory/internal/memory/reason"
	"agentmemory/internal/memory/recall"
	"agentmemory/internal/memory/store"
	"agentmemory/internal/memory/write"
)

// Тесты JSON API раздела «Синхронизация» (web_sync.go) — без общей базы:
// недоступный сервер — 127.0.0.1:1 (соединение отвергается сразу).

const syncTestPassword = "пароль-который-нельзя-показывать"

// syncWebUnreachableBody — тело PUT цели на недоступный сервер.
const syncWebUnreachableBody = `{"host":"127.0.0.1","port":1,"user":"memo","dbname":"memo","sslmode":"disable","push":true,"pull":true`

// newSyncWebServer поднимает морду над SQLite во временном каталоге; envDir —
// каталог БД (там .env с паролями). Глобальный git-конфиг подменён: имя по
// умолчанию — «Гит Тестов».
func newSyncWebServer(t *testing.T, s store.Store, envDir string) *httptest.Server {
	t.Helper()
	useGitGlobalConfig(t, "[user]\n\tname = Гит Тестов\n")
	w := write.New(s, embed.NewNoop())
	rc := recall.New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: testRecallWeights()}, reason.NewNoop(), embed.NewNoop())
	srv := httptest.NewServer(newWebHandler(s, w, rc, embed.NewNoop(), webInstance{SyncEnvDir: envDir}))
	t.Cleanup(srv.Close)
	return srv
}

// newSyncWebSQLite — SQLite-хранилище во временном каталоге и сам каталог.
func newSyncWebSQLite(t *testing.T) (store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.OpenSQLite(filepath.Join(dir, "memory.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}

// syncDo выполняет запрос и возвращает код и сырое тело: по сырому телу
// проверяется, что пароля в ответе нет ни в каком поле.
func syncDo(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("чтение ответа: %v", err)
	}
	if strings.Contains(string(raw), syncTestPassword) {
		t.Fatalf("%s %s: пароль в ответе: %s", method, url, raw)
	}
	return resp.StatusCode, string(raw)
}

// syncDoJSON — syncDo с ожидаемым кодом и разбором тела в out.
func syncDoJSON(t *testing.T, method, url, body string, wantStatus int, out any) {
	t.Helper()
	code, raw := syncDo(t, method, url, body)
	if code != wantStatus {
		t.Fatalf("%s %s = %d (%s), хочу %d", method, url, code, raw, wantStatus)
	}
	if out != nil {
		if err := json.Unmarshal([]byte(raw), out); err != nil {
			t.Fatalf("разбор %s: %v", raw, err)
		}
	}
}

func TestSyncAPI_GetDefaults(t *testing.T) {
	s, dir := newSyncWebSQLite(t)
	putTestMemory(t, s, func(m *memory.Memory) { m.Scope = "project:beta" })
	putTestMemory(t, s, func(m *memory.Memory) { m.Scope = "project:alpha" })
	putTestMemory(t, s, func(m *memory.Memory) { m.Scope = "user:*" })
	srv := newSyncWebServer(t, s, dir)

	var got syncResponse
	syncDoJSON(t, http.MethodGet, srv.URL+"/api/sync", "", http.StatusOK, &got)
	if !got.Available || got.Profile == nil {
		t.Fatalf("GET /api/sync = %+v, хочу доступный раздел с профилем", got)
	}
	if got.Profile.Name != "" || got.Profile.NameDefault != "Гит Тестов" || got.Profile.IntervalSec != 300 {
		t.Errorf("профиль = %+v, хочу пустое имя, дефолт из git и 300 с", *got.Profile)
	}
	if strings.Join(got.Projects, ",") != "alpha,beta" || got.Targets == nil || len(got.Targets) != 0 {
		t.Errorf("projects = %v, targets = %v; хочу alpha,beta и пустой список целей", got.Projects, got.Targets)
	}
}

func TestSyncAPI_Profile(t *testing.T) {
	s, dir := newSyncWebSQLite(t)
	srv := newSyncWebServer(t, s, dir)

	var p syncProfileDTO
	syncDoJSON(t, http.MethodPut, srv.URL+"/api/sync/profile", `{"name":"  Алиса  ","intervalSec":120}`, http.StatusOK, &p)
	if p.Name != "Алиса" || p.IntervalSec != 120 || p.NameDefault != "Гит Тестов" {
		t.Fatalf("PUT profile = %+v", p)
	}
	syncDoJSON(t, http.MethodPut, srv.URL+"/api/sync/profile", `{"name":"","intervalSec":0}`, http.StatusOK, &p)
	if p.Name != "" || p.IntervalSec != 300 {
		t.Fatalf("PUT profile с дефолтами = %+v, хочу пустое имя и 300 с", p)
	}

	for _, body := range []string{
		`{"name":"x","intervalSec":30}`,
		`{"name":"x","intervalSec":100000}`,
		`{"name":"a\u0000b"}`,
		`{"name":"x","extra":1}`,
		`не json`,
		`{"name":"` + syncTestPassword + `\x"}`,
	} {
		if code, raw := syncDo(t, http.MethodPut, srv.URL+"/api/sync/profile", body); code != http.StatusBadRequest {
			t.Errorf("PUT profile %s = %d (%s), хочу 400", body, code, raw)
		}
	}
}

// TestSyncAPI_TargetPassword: цель заводится на проект без локальных записей;
// пароль только на запись — лежит в .env каталога БД, наружу лишь passwordSet;
// без поля password пароль не трогается, пустая строка — удаляет.
func TestSyncAPI_TargetPassword(t *testing.T) {
	s, dir := newSyncWebSQLite(t)
	srv := newSyncWebServer(t, s, dir)
	url := srv.URL + "/api/sync/targets/новичок"
	envPath := filepath.Join(dir, ".env")

	var tg syncTargetDTO
	syncDoJSON(t, http.MethodPut, url, `{"host":"pg.lan","user":"memo","dbname":"team","push":true,"password":"`+syncTestPassword+`"}`, http.StatusOK, &tg)
	if tg.Project != "новичок" || tg.Port != 5432 || tg.SSLMode != "prefer" || !tg.Push || tg.Pull || !tg.PasswordSet || tg.Warning != "" {
		t.Fatalf("PUT цели = %+v", tg)
	}
	saved, err := mustSyncLocal(t, s).Target(context.Background(), "новичок")
	if err != nil {
		t.Fatalf("Target: %v", err)
	}
	if got, _ := syncPassword(dir, saved.PasswordEnv); got != syncTestPassword {
		t.Fatalf("пароль в .env = %q, хочу сохранённый (файл: %s)", got, mustRead(t, envPath))
	}

	// Без поля password — пароль на месте, остальное меняется.
	syncDoJSON(t, http.MethodPut, url, `{"host":"pg2.lan","port":6432,"user":"memo","dbname":"team","sslmode":"require","pull":true}`, http.StatusOK, &tg)
	if tg.Host != "pg2.lan" || tg.Port != 6432 || tg.SSLMode != "require" || tg.Push || !tg.Pull || !tg.PasswordSet {
		t.Fatalf("PUT без пароля = %+v", tg)
	}

	var got syncResponse
	syncDoJSON(t, http.MethodGet, srv.URL+"/api/sync", "", http.StatusOK, &got)
	if len(got.Targets) != 1 || got.Targets[0].Project != "новичок" || !got.Targets[0].PasswordSet || len(got.Projects) != 0 {
		t.Fatalf("GET после PUT = %+v", got)
	}

	// Пустая строка — удалить пароль.
	syncDoJSON(t, http.MethodPut, url, `{"host":"pg2.lan","user":"memo","dbname":"team","password":""}`, http.StatusOK, &tg)
	if tg.PasswordSet || strings.Contains(string(mustRead(t, envPath)), "MEMO_SYNC_PASSWORD") {
		t.Fatalf("после password=\"\": passwordSet = %v, .env = %s", tg.PasswordSet, mustRead(t, envPath))
	}
}

func TestSyncAPI_TargetValidation(t *testing.T) {
	s, dir := newSyncWebSQLite(t)
	srv := newSyncWebServer(t, s, dir)
	ok := `{"host":"pg.lan","user":"memo","dbname":"team"}`
	for _, c := range []struct{ project, body string }{
		{"a%20b", ok},
		{"a:b", ok},
		{"alpha", `{"user":"memo","dbname":"team"}`},
		{"alpha", `{"host":"pg.lan/evil","user":"memo","dbname":"team"}`},
		{"alpha", `{"host":"pg lan","user":"memo","dbname":"team"}`},
		{"alpha", `{"host":"pg.lan","user":"memo"}`},
		{"alpha", `{"host":"pg.lan","user":"memo","dbname":"a?b"}`},
		{"alpha", `{"host":"pg.lan","port":70000,"user":"memo","dbname":"team"}`},
		{"alpha", `{"host":"pg.lan","user":"memo","dbname":"team","sslmode":"bogus"}`},
		{"alpha", `{"host":"pg.lan","user":"memo","dbname":"team","password":"a\nb"}`},
		{"alpha", `{"host":"pg.lan","user":"memo","dbname":"team","unknown":true}`},
	} {
		if code, raw := syncDo(t, http.MethodPut, srv.URL+"/api/sync/targets/"+c.project, c.body); code != http.StatusBadRequest {
			t.Errorf("PUT %s %s = %d (%s), хочу 400", c.project, c.body, code, raw)
		}
	}
	if targets, _ := mustSyncLocal(t, s).Targets(context.Background()); len(targets) != 0 {
		t.Errorf("невалидный ввод завёл цель: %+v", targets)
	}
}

// TestSyncAPI_PasswordWarningInRepo: пароль лёг в .env внутри git-репозитория —
// ответ предупреждает (DESIGN.md §13).
func TestSyncAPI_PasswordWarningInRepo(t *testing.T) {
	s, _ := newSyncWebSQLite(t)
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	srv := newSyncWebServer(t, s, filepath.Join(repo, "data"))
	var tg syncTargetDTO
	syncDoJSON(t, http.MethodPut, srv.URL+"/api/sync/targets/alpha", `{"host":"h","user":"u","dbname":"d","password":"`+syncTestPassword+`"}`, http.StatusOK, &tg)
	if !strings.Contains(tg.Warning, ".gitignore") {
		t.Fatalf("warning = %q, хочу предупреждение про .gitignore", tg.Warning)
	}
}

func TestSyncAPI_DeleteTarget(t *testing.T) {
	s, dir := newSyncWebSQLite(t)
	srv := newSyncWebServer(t, s, dir)
	m := putTestMemory(t, s, func(m *memory.Memory) { m.Scope = "project:alpha" })
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("DEEPSEEK_API_KEY=ключ\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	syncDoJSON(t, http.MethodPut, srv.URL+"/api/sync/targets/alpha", syncWebUnreachableBody+`,"password":"`+syncTestPassword+`"}`, http.StatusOK, nil)

	syncDoJSON(t, http.MethodDelete, srv.URL+"/api/sync/targets/alpha", "", http.StatusOK, nil)
	var got syncResponse
	syncDoJSON(t, http.MethodGet, srv.URL+"/api/sync", "", http.StatusOK, &got)
	if len(got.Targets) != 0 {
		t.Fatalf("цель осталась: %+v", got.Targets)
	}
	if env := string(mustRead(t, filepath.Join(dir, ".env"))); strings.Contains(env, "MEMO_SYNC_PASSWORD") || !strings.Contains(env, "DEEPSEEK_API_KEY=ключ") {
		t.Fatalf(".env после удаления цели: %s", env)
	}
	if _, err := s.Get(context.Background(), m.Id); err != nil {
		t.Fatalf("удаление цели тронуло локальные записи: %v", err)
	}
	if code, _ := syncDo(t, http.MethodDelete, srv.URL+"/api/sync/targets/alpha", ""); code != http.StatusNotFound {
		t.Errorf("повторный DELETE = %d, хочу 404", code)
	}
}

// TestSyncAPI_HubOpsUnreachable: проверка, схема, ротация и прогон на
// недоступной базе — ошибка без пароля; схема и ротация без confirm — 400 до
// подключения; нет цели — 404.
func TestSyncAPI_HubOpsUnreachable(t *testing.T) {
	s, dir := newSyncWebSQLite(t)
	srv := newSyncWebServer(t, s, dir)
	syncDoJSON(t, http.MethodPut, srv.URL+"/api/sync/targets/alpha", syncWebUnreachableBody+`,"password":"`+syncTestPassword+`"}`, http.StatusOK, nil)
	// Запись после появления цели помечают триггеры; эпизод в число неотправленного не входит.
	putTestMemory(t, s, func(m *memory.Memory) { m.Scope = "project:alpha"; m.Kind = memory.KindSemantic })
	putTestMemory(t, s, func(m *memory.Memory) { m.Scope = "project:alpha"; m.Kind = memory.KindEpisodic })
	base := srv.URL + "/api/sync/targets/alpha"

	var check syncCheckResponse
	syncDoJSON(t, http.MethodPost, base+"/check", "", http.StatusOK, &check)
	if check.Reachable || check.Error == "" || check.Schema != nil {
		t.Fatalf("check = %+v, хочу reachable:false с ошибкой", check)
	}

	for _, op := range []string{"schema", "rotate"} {
		for _, body := range []string{`{}`, `{"confirm":false}`} {
			if code, raw := syncDo(t, http.MethodPost, base+"/"+op, body); code != http.StatusBadRequest || !strings.Contains(raw, "confirm") {
				t.Errorf("%s %s = %d (%s), хочу 400 про confirm", op, body, code, raw)
			}
		}
		if code, raw := syncDo(t, http.MethodPost, base+"/"+op, `{"confirm":true}`); code != http.StatusBadGateway {
			t.Errorf("%s на недоступной базе = %d (%s), хочу 502", op, code, raw)
		}
		if code, _ := syncDo(t, http.MethodPost, srv.URL+"/api/sync/targets/beta/"+op, `{"confirm":true}`); code != http.StatusNotFound {
			t.Errorf("%s без цели = %d, хочу 404", op, code)
		}
	}

	var run syncRunResponse
	syncDoJSON(t, http.MethodPost, base+"/run", "", http.StatusOK, &run)
	if run.Project != "alpha" || run.Error == "" {
		t.Fatalf("run = %+v, хочу ошибку прогона", run)
	}
	var got syncResponse
	syncDoJSON(t, http.MethodGet, srv.URL+"/api/sync", "", http.StatusOK, &got)
	st := got.Targets[0].State
	if st.LastRunAt == 0 || st.LastError == "" || st.Pending != 1 {
		t.Fatalf("state после прогона = %+v, хочу время, ошибку и 1 неотправленную", st)
	}

	if code, _ := syncDo(t, http.MethodPost, srv.URL+"/api/sync/targets/beta/check", ""); code != http.StatusNotFound {
		t.Errorf("check без цели = %d, хочу 404", code)
	}
	if code, _ := syncDo(t, http.MethodPost, srv.URL+"/api/sync/targets/beta/run", ""); code != http.StatusNotFound {
		t.Errorf("run без цели = %d, хочу 404", code)
	}

	// Цель держит другой процесс — 409.
	if ok, err := mustSyncLocal(t, s).Lease(context.Background(), "alpha", "другой процесс", 1<<40); err != nil || !ok {
		t.Fatalf("Lease: %v, %v", ok, err)
	}
	if code, raw := syncDo(t, http.MethodPost, base+"/run", ""); code != http.StatusConflict || !strings.Contains(raw, "уже идёт") {
		t.Errorf("run при занятом lease = %d (%s), хочу 409", code, raw)
	}
	if code, raw := syncDo(t, http.MethodDelete, base, ""); code != http.StatusConflict {
		t.Errorf("DELETE при занятом lease = %d (%s), хочу 409", code, raw)
	}
}

// sharedStore прячет SyncLocal SQLite-хранилища — так выглядит хранилище с
// driver = "postgres" для раздела синхронизации.
type sharedStore struct{ store.Store }

func TestSyncAPI_PostgresMode(t *testing.T) {
	s, dir := newSyncWebSQLite(t)
	srv := newSyncWebServer(t, sharedStore{s}, dir)

	var got syncResponse
	syncDoJSON(t, http.MethodGet, srv.URL+"/api/sync", "", http.StatusOK, &got)
	if got.Available || got.Reason == "" || got.Profile != nil || got.Projects == nil || got.Targets == nil {
		t.Fatalf("GET в режиме postgres = %+v", got)
	}
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPut, "/api/sync/profile", `{"name":"x"}`},
		{http.MethodPut, "/api/sync/targets/alpha", `{"host":"h","user":"u","dbname":"d"}`},
		{http.MethodDelete, "/api/sync/targets/alpha", ""},
		{http.MethodPost, "/api/sync/targets/alpha/check", ""},
		{http.MethodPost, "/api/sync/targets/alpha/schema", `{"confirm":true}`},
		{http.MethodPost, "/api/sync/targets/alpha/rotate", `{"confirm":true}`},
		{http.MethodPost, "/api/sync/targets/alpha/run", ""},
	} {
		if code, raw := syncDo(t, c.method, srv.URL+c.path, c.body); code != http.StatusConflict {
			t.Errorf("%s %s = %d (%s), хочу 409", c.method, c.path, code, raw)
		}
	}
}

func mustSyncLocal(t *testing.T, s store.Store) *store.SyncLocal {
	t.Helper()
	l, ok := store.SyncLocalOf(s)
	if !ok {
		t.Fatal("хранилище не локальное")
	}
	return l
}
