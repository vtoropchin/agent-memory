package main

import (
	"context"
	"log"
	"os/exec"
	"strings"
	"time"

	"agentmemory/internal/memory/store"
)

// gitUserNameTimeout ограничивает вызов git при старте процесса: имя автора —
// не повод задерживать запуск MCP-сервера.
const gitUserNameTimeout = 2 * time.Second

// authorName — имя разработчика для полей авторства записей (Этап 19, DESIGN.md
// §4), передаётся Writer через write.WithAuthor: имя из профиля синхронизации
// локальной БД, иначе git config --global user.name, иначе пусто — записи без
// автора, не ошибка. При driver = "postgres" профиля нет — сразу git. Читается
// один раз при старте процесса.
func authorName(ctx context.Context, s store.Store) string {
	if l, ok := store.SyncLocalOf(s); ok {
		p, err := l.Profile(ctx)
		if err != nil {
			log.Printf("memo: чтение профиля синхронизации: %v — имя автора из git", err)
		} else if p.AuthorName != "" {
			return p.AuthorName
		}
	}
	return gitUserName(ctx)
}

// gitUserName возвращает git config --global user.name; нет git, нет настройки
// или ошибка — пустая строка.
func gitUserName(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, gitUserNameTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "config", "--global", "user.name").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
