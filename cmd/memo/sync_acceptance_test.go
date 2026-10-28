package main

// sync_acceptance_test.go — приёмка Этапа 19 (задача 19.10, DESIGN.md §4.1, §7.1, §12):
// два разработчика («Аня» и «Борис») со своими SQLite и живыми бинарями memo (web, MCP,
// CLI) и одна живая общая PostgreSQL. Общая база — временная схема сервера
// MEMO_TEST_POSTGRES_DSN, бинари попадают в неё через PGOPTIONS search_path, public не
// трогается; схема удаляется в конце. Без DSN — пропуск. Сценарии идут подтестами
// по порядку и опираются на состояние предыдущих.

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"agentmemory/internal/memory/store"
)

// accProject / accOther — проект синхронизации и соседний, который утекать не должен.
const (
	accProject = "teamx"
	accOther   = "teamy"
	accWait    = 30 * time.Second
)

// accHub — параметры общей базы из DSN и служебное подключение к временной схеме.
type accHub struct {
	host, user, dbname, sslmode, password string
	port                                  int
	schema                                string
	db                                    *sql.DB
}

// accDev — разработчик: свой каталог (БД, конфиг, .env с паролем цели, gitconfig,
// HOME), свои процессы memo web и MCP.
type accDev struct {
	t      *testing.T
	name   string
	dir    string
	cfg    string
	bin    string
	env    []string
	web    *exec.Cmd
	webLog *bytes.Buffer
	base   string
	mcp    *accMCP
}

func TestSyncAcceptanceE2E(t *testing.T) {
	dsn := os.Getenv("MEMO_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("MEMO_TEST_POSTGRES_DSN не задан — пропуск приёмки синхронизации")
	}
	hub := newAccHub(t, dsn)
	bin := buildAccBinary(t)
	ollama := ollamaHasModel("bge-m3")

	anya := newAccDev(t, "Аня", bin, false)
	boris := newAccDev(t, "Борис", bin, ollama)

	ids := map[string]string{}
	target := func(port int, push, pull bool, password bool) map[string]any {
		body := map[string]any{"host": hub.host, "port": port, "user": hub.user, "dbname": hub.dbname,
			"sslmode": hub.sslmode, "push": push, "pull": pull}
		if password {
			body["password"] = hub.password
		}
		return body
	}
	closedPort := freePort(t) // порт, на котором никто не слушает: «общая база недоступна»
	// Сценарии опираются на состояние предыдущих: первое падение останавливает приёмку.
	step := func(name string, f func(t *testing.T)) {
		if !t.Run(name, f) {
			t.FailNow()
		}
	}

	step("1_Аня_настраивает_и_отправляет", func(t *testing.T) {
		// Записи до настройки синхронизации: имени ещё нет (git user.name пуст).
		ids["semantic"] = anya.save(t, "-kind", "semantic", "-scope", "project:"+accProject, "-content", "Архитектура синхронизации: курсор pull — горизонт снимка xmin")
		ids["procedural"] = anya.save(t, "-kind", "procedural", "-scope", "project:"+accProject, "-content", "Рецепт: применить схему общей базы кнопкой с подтверждением")
		ids["debt"] = anya.save(t, "-kind", "debt", "-scope", "project:"+accProject, "-content", "Долг: ложные конфликты из окна перечитывания xmin..xmax")
		ids["task"] = anya.save(t, "-kind", "task", "-scope", "project:"+accProject, "-summary", "Задача команды", "-content", "Сделать общую доску задач")
		ids["episodic"] = anya.save(t, "-kind", "episodic", "-scope", "project:"+accProject, "-content", "Эпизод: Аня обсуждала курсор с Борисом")
		ids["user"] = anya.save(t, "-kind", "semantic", "-scope", "user:*", "-content", "Личное: Аня любит тёмную тему")
		ids["other"] = anya.save(t, "-kind", "semantic", "-scope", "project:"+accOther, "-content", "Чужой проект teamy: секретный план")

		anya.startWeb(t)
		if m := anya.get(t, ids["semantic"]); m.Author != "" {
			t.Fatalf("до настройки author = %q, хочу пусто", m.Author)
		}
		anya.api(t, "PUT", "/api/sync/profile", map[string]any{"name": "Аня", "intervalSec": 60}, http.StatusOK, nil)
		// Связь факта проекта на эпизод — локальная, в общую базу не уходит (сценарий 6).
		anya.api(t, "PATCH", "/api/memories/"+ids["semantic"], map[string]any{"links": []map[string]string{{"id": ids["episodic"], "rel": "related"}}}, http.StatusOK, nil)

		var st syncResponse
		anya.api(t, "GET", "/api/sync", nil, http.StatusOK, &st)
		if !st.Available || !contains(st.Projects, accProject) {
			t.Fatalf("GET /api/sync: available=%v projects=%v", st.Available, st.Projects)
		}
		anya.api(t, "PUT", "/api/sync/targets/"+accProject, target(hub.port, false, false, true), http.StatusOK, nil)

		var chk syncCheckResponse
		anya.api(t, "POST", "/api/sync/targets/"+accProject+"/check", nil, http.StatusOK, &chk)
		t.Logf("проверка до схемы: %+v schema=%+v", chk, chk.Schema)
		if !chk.Reachable || chk.Schema == nil || chk.Schema.State != "empty" {
			t.Fatalf("проверка пустой базы: %+v", chk)
		}
		anya.api(t, "POST", "/api/sync/targets/"+accProject+"/schema", map[string]any{}, http.StatusBadRequest, nil)
		anya.api(t, "POST", "/api/sync/targets/"+accProject+"/schema", map[string]any{"confirm": true}, http.StatusOK, &chk)
		if chk.Schema == nil || chk.Schema.State != "ok" {
			t.Fatalf("после применения схемы: %+v", chk)
		}
		t.Logf("после применения схемы: schema=%+v", chk.Schema)

		anya.api(t, "PUT", "/api/sync/targets/"+accProject, target(hub.port, true, false, false), http.StatusOK, nil)
		res := anya.runNow(t) // «Синхронизировать сейчас»
		t.Logf("прогон Ани: %+v", res)
		waitFor(t, "записи Ани в общей базе", func() (bool, string) {
			n := hub.count(t, "SELECT count(*) FROM memories WHERE scope = $1", "project:"+accProject)
			return n == 4, fmt.Sprintf("записей project:%s в общей базе: %d", accProject, n)
		})
		for _, k := range []string{"semantic", "procedural", "debt", "task"} {
			author, updatedBy := hub.authors(t, ids[k])
			if author != "Аня" {
				t.Errorf("%s: author в общей базе = %q, хочу Аня", k, author)
			}
			t.Logf("общая база: %s author=%q updated_by=%q", k, author, updatedBy)
			if m := anya.get(t, ids[k]); m.Author != "Аня" {
				t.Errorf("%s: локальный author у Ани = %q, хочу Аня (штамп старых записей)", k, m.Author)
			}
		}
		// Дальше Аня и загружает: сценарии 3–5 ждут правок Бориса.
		anya.api(t, "PUT", "/api/sync/targets/"+accProject, target(hub.port, true, true, false), http.StatusOK, nil)
		anya.fastInterval(t)
	})

	step("2_Борис_загружает_проект", func(t *testing.T) {
		boris.startWeb(t)
		var st syncResponse
		boris.api(t, "GET", "/api/sync", nil, http.StatusOK, &st)
		if contains(st.Projects, accProject) {
			t.Fatalf("у Бориса проект %s уже есть локально: %v", accProject, st.Projects)
		}
		boris.api(t, "PUT", "/api/sync/targets/"+accProject, target(hub.port, false, true, true), http.StatusOK, nil)
		res := boris.runNow(t)
		t.Logf("прогон Бориса: %+v", res)
		waitFor(t, "записи проекта у Бориса", func() (bool, string) {
			got := 0
			for _, k := range []string{"semantic", "procedural", "debt", "task"} {
				if code, _ := boris.try("GET", "/api/memories/"+ids[k], nil); code == http.StatusOK {
					got++
				}
			}
			return got == 4, fmt.Sprintf("получено %d из 4", got)
		})
		if m := boris.get(t, ids["task"]); m.Author != "Аня" || m.Summary == nil || *m.Summary != "Задача команды" {
			t.Errorf("задача у Бориса: author=%q summary=%v", m.Author, m.Summary)
		}

		if ollama {
			// Векторы не синхронизируются: их досчитывает свой maint-прогон, MCP-сервер
			// Бориса делает его на старте.
			if boris.embedded(t, ids["semantic"]) {
				t.Fatal("вектор у Бориса есть сразу после pull — ожидался досчёт локально")
			}
			boris.startMCP(t)
			waitFor(t, "досчёт векторов у Бориса", func() (bool, string) {
				missing := 0
				for _, k := range []string{"semantic", "procedural", "debt", "task"} {
					if !boris.embedded(t, ids[k]) {
						missing++
					}
				}
				return missing == 0 && boris.pendingVectors(t) == 0, fmt.Sprintf("без вектора: %d, очередь %d", missing, boris.pendingVectors(t))
			})
		} else {
			t.Log("ollama с bge-m3 недоступна — досчёт векторов не проверяется, recall на FTS5")
			boris.startMCP(t)
		}
		out := boris.cli(t, "recall", "-scopes", "project:"+accProject, "курсор горизонт снимка")
		if !strings.Contains(out, "горизонт снимка xmin") {
			t.Fatalf("recall у Бориса не нашёл факт %s:\n%s", ids["semantic"], out)
		}
		t.Logf("recall у Бориса:\n%s", out)
		boris.stopMCP(t)
		boris.fastInterval(t)
	})

	step("3_Борис_двигает_задачу", func(t *testing.T) {
		// Имя вводится в той же морде, где Борис тут же двигает карточку, — без перезапуска.
		boris.api(t, "PUT", "/api/sync/profile", map[string]any{"name": "Борис", "intervalSec": 60}, http.StatusOK, nil)
		boris.api(t, "PUT", "/api/sync/targets/"+accProject, target(hub.port, true, true, false), http.StatusOK, nil)
		boris.api(t, "PATCH", "/api/memories/"+ids["task"], map[string]any{"status": "doing"}, http.StatusOK, nil)
		res := boris.runNow(t)
		t.Logf("прогон Бориса: %+v", res)
		waitFor(t, "стадия doing у Ани (её фоновый интервал)", func() (bool, string) {
			m := anya.get(t, ids["task"])
			return m.Status == "doing" && m.UpdatedBy == "Борис", fmt.Sprintf("у Ани status=%q updated_by=%q", m.Status, m.UpdatedBy)
		})
		boris.fastInterval(t) // PUT профиля вернул интервал 60 с
	})

	step("4_конфликт_офлайн_правок", func(t *testing.T) {
		before := boris.state(t).ConflictsTotal + anya.state(t).ConflictsTotal
		anya.api(t, "PUT", "/api/sync/targets/"+accProject, target(closedPort, true, true, false), http.StatusOK, nil)
		boris.api(t, "PUT", "/api/sync/targets/"+accProject, target(closedPort, true, true, false), http.StatusOK, nil)
		waitFor(t, "обе стороны офлайн", func() (bool, string) {
			a, b := anya.state(t), boris.state(t)
			return a.LastError != "" && b.LastError != "", fmt.Sprintf("ошибки: Аня %q, Борис %q", a.LastError, b.LastError)
		})
		anyaText := "Архитектура синхронизации: версия Ани (офлайн)"
		borisText := "Архитектура синхронизации: версия Бориса (офлайн, позже)"
		anya.api(t, "PATCH", "/api/memories/"+ids["semantic"], map[string]any{"content": anyaText}, http.StatusOK, nil)
		time.Sleep(1100 * time.Millisecond) // разные машины: в ту же секунду была бы ничья в пользу общей базы
		boris.api(t, "PATCH", "/api/memories/"+ids["semantic"], map[string]any{"content": borisText}, http.StatusOK, nil)

		anya.api(t, "PUT", "/api/sync/targets/"+accProject, target(hub.port, true, true, false), http.StatusOK, nil)
		waitFor(t, "версия Ани в общей базе", func() (bool, string) {
			c := hub.content(t, ids["semantic"])
			return c == anyaText, fmt.Sprintf("в общей базе %q", c)
		})
		boris.api(t, "PUT", "/api/sync/targets/"+accProject, target(hub.port, true, true, false), http.StatusOK, nil)
		waitFor(t, "сходимость к версии Бориса", func() (bool, string) {
			a, b, h := anya.get(t, ids["semantic"]).Content, boris.get(t, ids["semantic"]).Content, hub.content(t, ids["semantic"])
			return a == borisText && b == borisText && h == borisText, fmt.Sprintf("Аня %q, Борис %q, общая %q", a, b, h)
		})
		after := boris.state(t).ConflictsTotal + anya.state(t).ConflictsTotal
		t.Logf("конфликтов всего: Аня %d, Борис %d", anya.state(t).ConflictsTotal, boris.state(t).ConflictsTotal)
		if after <= before {
			t.Errorf("счётчик конфликтов не вырос: было %d, стало %d", before, after)
		}
		am, bm := anya.get(t, ids["semantic"]), boris.get(t, ids["semantic"])
		if am.UpdatedAt != bm.UpdatedAt || am.UpdatedBy != "Борис" || bm.UpdatedBy != "Борис" {
			t.Errorf("версии разошлись: Аня updated_at=%d by=%q, Борис updated_at=%d by=%q", am.UpdatedAt, am.UpdatedBy, bm.UpdatedAt, bm.UpdatedBy)
		}
	})

	step("5_forget_доходит_тумстоуном", func(t *testing.T) {
		anya.api(t, "DELETE", "/api/memories/"+ids["procedural"], nil, http.StatusOK, nil)
		waitFor(t, "тумстоун у Бориса", func() (bool, string) {
			m := boris.get(t, ids["procedural"])
			return m.SupersededBy != nil && *m.SupersededBy != "", fmt.Sprintf("superseded_by у Бориса = %v", m.SupersededBy)
		})
	})

	step("6_несинхронизируемое_не_утекает", func(t *testing.T) {
		for _, q := range []struct {
			desc, sql string
			args      []any
		}{
			{"episodic", "SELECT count(*) FROM memories WHERE kind = 'episodic'", nil},
			{"user:*", "SELECT count(*) FROM memories WHERE scope = 'user:*'", nil},
			{"project:" + accOther, "SELECT count(*) FROM memories WHERE scope = $1", []any{"project:" + accOther}},
			{"связь на эпизод", "SELECT count(*) FROM links WHERE dst = $1", []any{ids["episodic"]}},
		} {
			if n := hub.count(t, q.sql, q.args...); n != 0 {
				t.Errorf("в общей базе %s: %d, хочу 0", q.desc, n)
			}
		}
		for _, k := range []string{"episodic", "user", "other"} {
			if code, _ := boris.try("GET", "/api/memories/"+ids[k], nil); code != http.StatusNotFound {
				t.Errorf("%s у Бориса: код %d, хочу 404", k, code)
			}
		}
		// Факт пережил применение общей версии (сценарий 4), локальная связь на эпизод — на месте.
		m := anya.get(t, ids["semantic"])
		found := false
		for _, l := range m.Links {
			found = found || l.Dst == ids["episodic"]
		}
		if !found {
			t.Errorf("у Ани потерялась связь %s → эпизод %s: %+v", ids["semantic"], ids["episodic"], m.Links)
		}
	})

	step("7_общая_база_недоступна", func(t *testing.T) {
		anya.startMCP(t)
		anya.api(t, "PUT", "/api/sync/targets/"+accProject, target(closedPort, true, true, false), http.StatusOK, nil)
		waitFor(t, "ошибка в статусе", func() (bool, string) {
			s := anya.state(t)
			return s.LastError != "", fmt.Sprintf("last_error = %q", s.LastError)
		})
		t.Logf("статус Ани при недоступной базе: %q", anya.state(t).LastError)
		content := "Сохранено офлайн через MCP " + ulid.Make().String()
		resp := anya.mcp.call(t, "memory_save", map[string]any{"kind": "semantic", "content": content, "scope": "project:" + accProject})
		if strings.Contains(resp, `"isError":true`) {
			t.Fatalf("memory_save при недоступной общей базе: %s", resp)
		}
		if resp := anya.mcp.call(t, "memory_recall", map[string]any{"query": "сохранено офлайн", "scopes": []string{"project:" + accProject}}); !strings.Contains(resp, "офлайн") {
			t.Fatalf("memory_recall при недоступной общей базе: %s", resp)
		}
		anya.api(t, "PUT", "/api/sync/targets/"+accProject, target(hub.port, true, true, false), http.StatusOK, nil)
		waitFor(t, "догоняет после восстановления", func() (bool, string) {
			n := hub.count(t, "SELECT count(*) FROM memories WHERE content = $1", content)
			return n == 1 && anya.state(t).LastError == "", fmt.Sprintf("в общей базе %d, ошибка %q", n, anya.state(t).LastError)
		})
		anya.stopMCP(t)
	})

	step("8_схема_новее_бинаря", func(t *testing.T) {
		hub.exec(t, "UPDATE schema_version SET version = version + 1 WHERE id = 1")
		waitFor(t, "синхронизация остановлена", func() (bool, string) {
			s := anya.state(t)
			return strings.Contains(s.LastError, "обновите memo"), fmt.Sprintf("last_error = %q", s.LastError)
		})
		var chk syncCheckResponse
		anya.api(t, "POST", "/api/sync/targets/"+accProject+"/check", nil, http.StatusOK, &chk)
		if chk.Schema == nil || chk.Schema.State != "newer" {
			t.Errorf("проверка при схеме новее: %+v", chk)
		}
		anya.api(t, "POST", "/api/sync/targets/"+accProject+"/schema", map[string]any{"confirm": true}, http.StatusConflict, nil)
		t.Logf("статус при схеме новее: %q", anya.state(t).LastError)
		hub.exec(t, "UPDATE schema_version SET version = version - 1 WHERE id = 1")
		waitFor(t, "синхронизация возобновилась", func() (bool, string) {
			s := anya.state(t)
			return s.LastError == "", fmt.Sprintf("last_error = %q", s.LastError)
		})
	})

	step("9_CLI_и_doctor", func(t *testing.T) {
		run := anya.cli(t, "sync", "-project", accProject)
		status := anya.cli(t, "sync", "status")
		doctor := anya.cliAllowFail(t, "doctor")
		t.Logf("memo sync:\n%s\nmemo sync status:\n%s\nmemo doctor:\n%s", run, status, doctor)
		api := anya.target(t)
		for _, s := range []struct{ out, want string }{
			{run, accProject},
			{status, accProject},
			{status, strconv.Itoa(api.State.ConflictsTotal)},
			{doctor, accProject},
		} {
			if !strings.Contains(s.out, s.want) {
				t.Errorf("в выводе нет %q:\n%s", s.want, s.out)
			}
		}
		if strings.Contains(status+doctor+run, hub.password) && hub.password != hub.user && hub.password != hub.dbname {
			t.Error("пароль общей базы попал в вывод CLI")
		}
	})

	step("10_driver_postgres_видит_те_же_данные", func(t *testing.T) {
		pg := newAccPGInstance(t, hub, bin)
		out := pg.cli(t, "recall", "-scopes", "project:"+accProject, "-statuses", "doing", "Задача команды")
		if !strings.Contains(out, "[doing] Задача команды") {
			t.Fatalf("driver=postgres не видит задачу %s:\n%s", ids["task"], out)
		}
		pg.startWeb(t)
		m := pg.get(t, ids["semantic"])
		bm := boris.get(t, ids["semantic"])
		if m.Content != bm.Content || m.UpdatedAt != bm.UpdatedAt || m.UpdatedBy != bm.UpdatedBy || m.Author != bm.Author {
			t.Errorf("общая база напрямую: %q/%d/%q/%q, у Бориса %q/%d/%q/%q", m.Content, m.UpdatedAt, m.UpdatedBy, m.Author, bm.Content, bm.UpdatedAt, bm.UpdatedBy, bm.Author)
		}
		var st syncResponse
		pg.api(t, "GET", "/api/sync", nil, http.StatusOK, &st)
		if st.Available {
			t.Errorf("driver=postgres: раздел синхронизации доступен, хочу «хранилище уже общее»")
		}
		t.Logf("driver=postgres /api/sync: available=%v reason=%q", st.Available, st.Reason)
		// Прямой писатель: правка через driver=postgres доходит до Ани синхронизацией.
		pg.api(t, "PATCH", "/api/memories/"+ids["debt"], map[string]any{"importance": 5}, http.StatusOK, nil)
		waitFor(t, "правка прямого писателя у Ани", func() (bool, string) {
			m := anya.get(t, ids["debt"])
			return m.Importance == 5, fmt.Sprintf("importance у Ани = %d", m.Importance)
		})
	})
}

// --- общая база ---

func newAccHub(t *testing.T, dsn string) *accHub {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("разбор DSN: %v", err)
	}
	h := &accHub{host: u.Hostname(), user: u.User.Username(), dbname: strings.TrimPrefix(u.Path, "/"),
		sslmode: u.Query().Get("sslmode"), schema: "memo_test_" + strings.ToLower(ulid.Make().String())}
	h.password, _ = u.User.Password()
	h.port, _ = strconv.Atoi(u.Port())
	if h.port == 0 {
		h.port = 5432
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("подключение: %v", err)
	}
	t.Cleanup(func() { admin.Close() })
	if _, err := admin.Exec("CREATE SCHEMA " + h.schema); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec("DROP SCHEMA " + h.schema + " CASCADE"); err != nil {
			t.Errorf("DROP SCHEMA: %v", err)
		}
	})
	admin.SetMaxOpenConns(1)
	if _, err := admin.Exec("SET search_path = " + h.schema); err != nil {
		t.Fatalf("search_path: %v", err)
	}
	h.db = admin
	t.Setenv("PGOPTIONS", "-c search_path="+h.schema)
	t.Logf("общая база: временная схема %s на %s:%d/%s", h.schema, h.host, h.port, h.dbname)
	return h
}

func (h *accHub) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := h.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("общая база %q: %v", q, err)
	}
	return n
}

func (h *accHub) authors(t *testing.T, id string) (author, updatedBy string) {
	t.Helper()
	var a, u sql.NullString
	if err := h.db.QueryRow("SELECT author, updated_by FROM memories WHERE id = $1", id).Scan(&a, &u); err != nil {
		t.Fatalf("общая база, автор %s: %v", id, err)
	}
	return a.String, u.String
}

func (h *accHub) content(t *testing.T, id string) string {
	t.Helper()
	var c string
	if err := h.db.QueryRow("SELECT content FROM memories WHERE id = $1", id).Scan(&c); err != nil {
		t.Fatalf("общая база, content %s: %v", id, err)
	}
	return c
}

func (h *accHub) exec(t *testing.T, q string) {
	t.Helper()
	if _, err := h.db.Exec(q); err != nil {
		t.Fatalf("общая база %q: %v", q, err)
	}
}

// --- разработчик ---

func newAccDev(t *testing.T, name, bin string, ollama bool) *accDev {
	t.Helper()
	dir := t.TempDir()
	embedder := "[embedder]\nprovider = \"noop\"\n"
	if ollama {
		embedder = "[embedder]\nprovider = \"ollama\"\nbase_url = \"http://localhost:11434\"\nmodel = \"bge-m3:latest\"\n"
	}
	cfg := filepath.Join(dir, "agentmemory.toml")
	toml := fmt.Sprintf("[store]\npath = %q\n\n[scope]\nproject = %q\n\n%s\n[reasoner]\nprovider = \"noop\"\n",
		filepath.Join(dir, "memory.db"), accProject, embedder)
	if err := os.WriteFile(cfg, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "HOME="+home, "GIT_CONFIG_GLOBAL="+filepath.Join(dir, "gitconfig"), "MEMO_POSTGRES_PASSWORD=")
	d := &accDev{t: t, name: name, dir: dir, cfg: cfg, bin: bin, env: env}
	t.Cleanup(d.stopAll)
	return d
}

func (d *accDev) command(args ...string) *exec.Cmd {
	cmd := exec.Command(d.bin, args...)
	cmd.Env = d.env
	cmd.Dir = d.dir
	return cmd
}

// cli — одноразовая подкоманда: аргументы после имени, -config подставляется.
func (d *accDev) cli(t *testing.T, sub string, args ...string) string {
	t.Helper()
	out, err := d.runCLI(sub, args...)
	if err != nil {
		t.Fatalf("%s: memo %s %v: %v\n%s", d.name, sub, args, err, out)
	}
	return out
}

func (d *accDev) cliAllowFail(t *testing.T, sub string, args ...string) string {
	t.Helper()
	out, _ := d.runCLI(sub, args...)
	return out
}

func (d *accDev) runCLI(sub string, args ...string) (string, error) {
	full := append([]string{sub, "-config", d.cfg}, args...)
	// recall принимает запрос последним позиционным аргументом — после флагов.
	out, err := d.command(full...).CombinedOutput()
	return string(out), err
}

func (d *accDev) save(t *testing.T, args ...string) string {
	t.Helper()
	out := d.cli(t, "save", args...)
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "saved" {
			return f[1]
		}
	}
	t.Fatalf("%s: memo save %v — нет id:\n%s", d.name, args, out)
	return ""
}

func (d *accDev) startWeb(t *testing.T) {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	d.webLog = &bytes.Buffer{}
	d.web = d.command("web", "-config", d.cfg, "-addr", addr)
	d.web.Stdout, d.web.Stderr = d.webLog, d.webLog
	if err := d.web.Start(); err != nil {
		t.Fatalf("%s: запуск memo web: %v", d.name, err)
	}
	d.base = "http://" + addr
	waitFor(t, d.name+": memo web поднялся", func() (bool, string) {
		code, _ := d.try("GET", "/api/stats", nil)
		return code == http.StatusOK, "код " + strconv.Itoa(code)
	})
}

func (d *accDev) stopWeb(t *testing.T) {
	t.Helper()
	_ = d.web.Process.Signal(os.Interrupt)
	if err := d.web.Wait(); err != nil {
		t.Logf("%s: memo web завершился: %v", d.name, err)
	}
	d.web = nil
}

func (d *accDev) startMCP(t *testing.T) {
	t.Helper()
	d.mcp = startAccMCP(t, d.command("-config", d.cfg))
}

func (d *accDev) stopMCP(t *testing.T) {
	t.Helper()
	if d.mcp != nil {
		d.mcp.close(t)
		d.mcp = nil
	}
}

func (d *accDev) stopAll() {
	if d.mcp != nil {
		_ = d.mcp.cmd.Process.Kill()
		_ = d.mcp.cmd.Wait()
	}
	if d.web != nil {
		_ = d.web.Process.Kill()
		_ = d.web.Wait()
		if d.t.Failed() {
			d.t.Logf("%s: лог memo web:\n%s", d.name, d.webLog.String())
		}
	}
}

// fastInterval — интервал фона 1 с прямо в профиль (API пускает от 60 с, а
// приёмке нужны быстрые фоновые прогоны — тот же приём, что в e2e 19.6) и перезапуск морды.
func (d *accDev) fastInterval(t *testing.T) {
	t.Helper()
	s, err := store.OpenSQLite(filepath.Join(d.dir, "memory.db"))
	if err != nil {
		t.Fatalf("%s: OpenSQLite: %v", d.name, err)
	}
	defer s.Close()
	l := s.SyncLocal()
	p, err := l.Profile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p.IntervalSec = 1
	if err := l.SetProfile(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if d.web != nil { // спящий планировщик перечитает интервал только через минуту
		d.stopWeb(t)
		d.startWeb(t)
	}
}

func (d *accDev) try(method, path string, body any) (int, []byte) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, d.base+path, rd)
	if err != nil {
		return 0, nil
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (d *accDev) api(t *testing.T, method, path string, body any, want int, out any) {
	t.Helper()
	code, b := d.try(method, path, body)
	if code != want {
		t.Fatalf("%s: %s %s: код %d, хочу %d: %s", d.name, method, path, code, want, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s: %s %s: разбор ответа: %v: %s", d.name, method, path, err, b)
		}
	}
}

// runNow — кнопка «Синхронизировать сейчас»; 409 (идёт фоновый прогон) — штатный
// ответ, кнопку жмут ещё раз.
func (d *accDev) runNow(t *testing.T) syncRunResponse {
	t.Helper()
	var res syncRunResponse
	waitFor(t, d.name+": прогон «сейчас»", func() (bool, string) {
		code, b := d.try("POST", "/api/sync/targets/"+accProject+"/run", nil)
		if code == http.StatusOK {
			return json.Unmarshal(b, &res) == nil, string(b)
		}
		return false, fmt.Sprintf("код %d: %s", code, b)
	})
	return res
}

func (d *accDev) get(t *testing.T, id string) memoryDTO {
	t.Helper()
	var card memoryCardResponse
	d.api(t, "GET", "/api/memories/"+id, nil, http.StatusOK, &card)
	return card.Memory
}

func (d *accDev) embedded(t *testing.T, id string) bool {
	t.Helper()
	var card memoryCardResponse
	d.api(t, "GET", "/api/memories/"+id, nil, http.StatusOK, &card)
	return card.Embedding.Embedded
}

func (d *accDev) target(t *testing.T) syncTargetDTO {
	t.Helper()
	var st syncResponse
	d.api(t, "GET", "/api/sync", nil, http.StatusOK, &st)
	for _, tg := range st.Targets {
		if tg.Project == accProject {
			return tg
		}
	}
	t.Fatalf("%s: цели %s нет в /api/sync", d.name, accProject)
	return syncTargetDTO{}
}

func (d *accDev) state(t *testing.T) syncStateDTO {
	t.Helper()
	return d.target(t).State
}

func (d *accDev) pendingVectors(t *testing.T) int {
	t.Helper()
	var st statsResponse
	d.api(t, "GET", "/api/stats", nil, http.StatusOK, &st)
	return st.Providers.Embedder.Pending
}

// newAccPGInstance — третий участник: memo с driver = "postgres" прямо на общей базе
// (та же временная схема через PGOPTIONS), пароль — из .env рядом с конфигом.
func newAccPGInstance(t *testing.T, h *accHub, bin string) *accDev {
	t.Helper()
	d := newAccDev(t, "driver=postgres", bin, false)
	toml := fmt.Sprintf("[store]\ndriver = \"postgres\"\nhost = %q\nport = %d\nuser = %q\ndbname = %q\nsslmode = %q\n\n[scope]\nproject = %q\n\n[embedder]\nprovider = \"noop\"\n\n[reasoner]\nprovider = \"noop\"\n",
		h.host, h.port, h.user, h.dbname, h.sslmode, accProject)
	if err := os.WriteFile(d.cfg, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeEnvFile(d.dir, memoPostgresPasswordEnv+"="+h.password+"\n"); err != nil {
		t.Fatal(err)
	}
	return d
}

// --- MCP по stdio ---

type accMCP struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan string
	stderr *bytes.Buffer
	mu     sync.Mutex
	next   int
}

func startAccMCP(t *testing.T, cmd *exec.Cmd) *accMCP {
	t.Helper()
	m := &accMCP{cmd: cmd, lines: make(chan string, 64), stderr: &bytes.Buffer{}, next: 1}
	var err error
	if m.stdin, err = cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = m.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("запуск MCP: %v", err)
	}
	go func() {
		defer close(m.lines)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			m.lines <- sc.Text()
		}
	}()
	m.request(t, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "acceptance", "version": "0"}})
	m.send(t, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	return m
}

func (m *accMCP) send(t *testing.T, msg any) {
	t.Helper()
	b, _ := json.Marshal(msg)
	if _, err := m.stdin.Write(append(b, '\n')); err != nil {
		t.Fatalf("MCP stdin: %v", err)
	}
}

func (m *accMCP) request(t *testing.T, method string, params any) string {
	t.Helper()
	m.mu.Lock()
	id := m.next
	m.next++
	m.mu.Unlock()
	m.send(t, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	timeout := time.After(accWait)
	for {
		select {
		case line, ok := <-m.lines:
			if !ok {
				t.Fatalf("MCP закрыл stdout до ответа %d; stderr:\n%s", id, m.stderr.String())
			}
			var msg struct {
				ID int `json:"id"`
			}
			if json.Unmarshal([]byte(line), &msg) == nil && msg.ID == id {
				return line
			}
		case <-timeout:
			t.Fatalf("MCP: нет ответа %d; stderr:\n%s", id, m.stderr.String())
		}
	}
}

func (m *accMCP) call(t *testing.T, tool string, args map[string]any) string {
	t.Helper()
	return m.request(t, "tools/call", map[string]any{"name": tool, "arguments": args})
}

func (m *accMCP) close(t *testing.T) {
	t.Helper()
	m.stdin.Close()
	for range m.lines {
	}
	if err := m.cmd.Wait(); err != nil {
		t.Errorf("завершение MCP: %v; stderr:\n%s", err, m.stderr.String())
	}
}

// --- общее ---

func buildAccBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "memo")
	build := exec.Command("go", "build", "-tags", "sqlite_fts5", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("сборка бинаря: %v\n%s", err, out)
	}
	return bin
}

func ollamaHasModel(model string) bool {
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://localhost:11434/api/tags")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return strings.Contains(string(b), `"`+model)
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitFor(t *testing.T, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(accWait)
	for {
		ok, last := cond()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: не дождались за %s; последнее: %s", what, accWait, last)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
