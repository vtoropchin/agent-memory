package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agentmemory/internal/memory/store"
)

// useGitGlobalConfig подставляет git глобальный конфиг из content (пусто —
// пустой файл), чтобы тест не зависел от настроек машины.
func useGitGlobalConfig(t *testing.T, content string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git не установлен")
	}
	path := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", path)
}

// setProfileName записывает имя разработчика в профиль синхронизации БД dbPath.
func setProfileName(t *testing.T, dbPath, name string) {
	t.Helper()
	s, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer s.Close()
	if err := s.SyncLocal().SetProfile(context.Background(), store.SyncProfile{AuthorName: name}); err != nil {
		t.Fatalf("SetProfile: %v", err)
	}
}

func TestAuthorName(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "memory.db")
	s, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer s.Close()

	useGitGlobalConfig(t, "[user]\n\tname = Пётр Git\n")
	if got := authorName(ctx, s); got != "Пётр Git" {
		t.Errorf("пустой профиль: authorName = %q, хочу имя из git", got)
	}

	if err := s.SyncLocal().SetProfile(ctx, store.SyncProfile{AuthorName: "Иван"}); err != nil {
		t.Fatalf("SetProfile: %v", err)
	}
	if got := authorName(ctx, s); got != "Иван" {
		t.Errorf("профиль задан: authorName = %q, хочу Иван", got)
	}

	if err := s.SyncLocal().SetProfile(ctx, store.SyncProfile{}); err != nil {
		t.Fatalf("SetProfile: %v", err)
	}
	useGitGlobalConfig(t, "")
	if got := authorName(ctx, s); got != "" {
		t.Errorf("ни профиля, ни git: authorName = %q, хочу пусто", got)
	}
}

// TestCLISave_AuthorFromProfile — e2e приёмки 19.3: memo с профилем «Иван»
// сохраняет запись, и у неё автор и последний редактор — Иван.
func TestCLISave_AuthorFromProfile(t *testing.T) {
	useGitGlobalConfig(t, "[user]\n\tname = Пётр Git\n")
	dir := t.TempDir()
	cfgPath := writeTestConfig(t, dir, "alpha", []string{"user:*"}, "")
	dbPath := filepath.Join(dir, "memory.db")
	setProfileName(t, dbPath, "Иван")

	var out bytes.Buffer
	args := []string{"-config", cfgPath, "-db", dbPath, "-kind", "semantic", "-content", "факт с автором из профиля"}
	if err := runSave(args, strings.NewReader(""), &out); err != nil {
		t.Fatalf("runSave: %v", err)
	}
	id := strings.TrimSpace(strings.TrimPrefix(out.String(), "saved "))
	assertAuthor(t, dbPath, id, "Иван", "Иван")
}
