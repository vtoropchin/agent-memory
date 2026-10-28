package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"agentmemory/internal/config"
	"agentmemory/internal/memory/store"
)

// TestStartSyncLoop_DegradesOnUnreachableHub: фоновый цикл прогоняет цель с
// недоступной общей базой — ошибка в статусе цели и строка в логе, цикл
// останавливается по stop.
func TestStartSyncLoop_DegradesOnUnreachableHub(t *testing.T) {
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "memory.db")
	s, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer s.Close()
	l := s.SyncLocal()
	ctx := context.Background()
	if _, err := l.PutTarget(ctx, store.SyncTarget{Project: "alpha", Host: "127.0.0.1", Port: 1, User: "memo", DBName: "memo", Push: true}); err != nil {
		t.Fatalf("PutTarget: %v", err)
	}
	if err := l.SetProfile(ctx, store.SyncProfile{AuthorName: "alice"}); err != nil {
		t.Fatalf("SetProfile: %v", err)
	}

	stop := startSyncLoop(ctx, s, config.StoreConfig{Driver: "sqlite", Path: dbPath})
	deadline := time.Now().Add(15 * time.Second)
	var lastErr string
	for time.Now().Before(deadline) {
		st, err := l.State(ctx, "alpha")
		if err != nil {
			t.Fatalf("State: %v", err)
		}
		if lastErr = st.LastError; lastErr != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	stop()

	if lastErr == "" {
		t.Fatal("ошибка недоступной общей базы не попала в статус цели")
	}
	if !strings.Contains(logs.String(), "sync: alpha:") {
		t.Errorf("в логе нет строки об ошибке цели:\n%s", logs.String())
	}
}

// TestStartSyncLoop_NotLocalStore: хранилище не SQLite (driver = "postgres") —
// цикл не запускается, stop — пустой.
func TestStartSyncLoop_NotLocalStore(t *testing.T) {
	stop := startSyncLoop(context.Background(), struct{ store.Store }{}, config.StoreConfig{Driver: "postgres"})
	stop()
}

// TestSyncLoopE2E_MCPServerPushes — приёмка 19.6 через живой бинарь: MCP-сервер
// с настроенной целью сам отправляет новую запись в общую базу за интервал,
// stdout остаётся чистым JSON-RPC. Общая база — временная схема живого сервера
// (MEMO_TEST_POSTGRES_DSN): бинарь попадает в неё через PGOPTIONS search_path,
// public не трогается. Без DSN — пропуск.
func TestSyncLoopE2E_MCPServerPushes(t *testing.T) {
	dsn := os.Getenv("MEMO_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("MEMO_TEST_POSTGRES_DSN не задан — пропуск e2e фоновой синхронизации")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("разбор DSN: %v", err)
	}
	ctx := context.Background()

	// Временная схема общей базы со схемой синхронизации.
	schema := "memo_test_" + strings.ToLower(ulid.Make().String())
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("подключение: %v", err)
	}
	t.Cleanup(func() { admin.Close() })
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("CREATE SCHEMA: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("DROP SCHEMA: %v", err)
		}
	})
	t.Setenv("PGOPTIONS", "-c search_path="+schema)
	port, _ := strconv.Atoi(u.Port())
	pw, _ := u.User.Password()
	hubCfg := config.StoreConfig{Driver: "postgres", Host: u.Hostname(), Port: port, User: u.User.Username(),
		DBName: strings.TrimPrefix(u.Path, "/"), SSLMode: u.Query().Get("sslmode"), Password: pw}
	hub, err := store.OpenHub(ctx, hubCfg)
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	if err := hub.ApplySchema(ctx); err != nil {
		t.Fatalf("ApplySchema: %v", err)
	}
	hub.Close()

	// Локальная база с целью e2e (push) и интервалом 1 с, пароль — в .env каталога БД.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "memory.db")
	s, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	l := s.SyncLocal()
	target, err := l.PutTarget(ctx, store.SyncTarget{Project: "e2e", Host: hubCfg.Host, Port: hubCfg.Port, User: hubCfg.User,
		DBName: hubCfg.DBName, SSLMode: hubCfg.SSLMode, Push: true})
	if err != nil {
		t.Fatalf("PutTarget: %v", err)
	}
	if err := l.SetProfile(ctx, store.SyncProfile{AuthorName: "e2e-dev", IntervalSec: 1}); err != nil {
		t.Fatalf("SetProfile: %v", err)
	}
	s.Close()
	if err := saveSyncPassword(dir, target.PasswordEnv, pw); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "agentmemory.toml")
	toml := fmt.Sprintf("[store]\npath = %q\n\n[scope]\nproject = \"e2e\"\n\n[embedder]\nprovider = \"noop\"\n\n[reasoner]\nprovider = \"noop\"\n", dbPath)
	if err := os.WriteFile(cfgPath, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(t.TempDir(), "memo")
	build := exec.Command("go", "build", "-tags", "sqlite_fts5", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("сборка бинаря: %v\n%s", err, out)
	}

	cmd := exec.Command(bin, "-config", cfgPath)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+filepath.Join(dir, "gitconfig"))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("запуск бинаря: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	var out []string
	await := func(id int) string {
		t.Helper()
		timeout := time.After(15 * time.Second)
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					t.Fatalf("stdout закрыт до ответа %d; stderr:\n%s", id, stderr.String())
				}
				out = append(out, line)
				var msg struct {
					ID int `json:"id"`
				}
				if json.Unmarshal([]byte(line), &msg) == nil && msg.ID == id {
					return line
				}
			case <-timeout:
				t.Fatalf("нет ответа %d; stderr:\n%s", id, stderr.String())
			}
		}
	}
	send := func(msg string) {
		t.Helper()
		if _, err := io.WriteString(stdin, msg+"\n"); err != nil {
			t.Fatalf("запись в stdin: %v", err)
		}
	}

	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e","version":"0"}}}`)
	await(1)
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	content := "e2e фоновой синхронизации " + ulid.Make().String()
	save, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": "memory_save", "arguments": map[string]any{"kind": "semantic", "content": content, "scope": "project:e2e"}}})
	send(string(save))
	if resp := await(2); strings.Contains(resp, `"isError":true`) {
		t.Fatalf("memory_save: %s", resp)
	}

	// Запись появляется в общей базе сама — без команды синхронизации.
	var author string
	deadline := time.Now().Add(20 * time.Second)
	for {
		err := admin.QueryRowContext(ctx, "SELECT author FROM "+schema+".memories WHERE content = $1", content).Scan(&author)
		if err == nil {
			break
		}
		if err != sql.ErrNoRows {
			t.Fatalf("чтение общей базы: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("запись не появилась в общей базе за 20 с; stdout:\n%s\nstderr:\n%s", strings.Join(out, "\n"), stderr.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
	if author != "e2e-dev" {
		t.Errorf("author в общей базе = %q, хочу e2e-dev", author)
	}

	stdin.Close()
	for line := range lines {
		out = append(out, line)
	}
	if err := cmd.Wait(); err != nil {
		t.Errorf("завершение бинаря: %v; stderr:\n%s", err, stderr.String())
	}
	for _, line := range out {
		var msg struct {
			JSONRPC string `json:"jsonrpc"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil || msg.JSONRPC != "2.0" {
			t.Errorf("в stdout не JSON-RPC: %q", line)
		}
	}
}
