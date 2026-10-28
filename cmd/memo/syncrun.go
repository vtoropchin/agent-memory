// syncrun.go — сборка прогона синхронизации проекта с общей базой (Этап 19,
// DESIGN.md §7.1, §13): цель из локальной БД → пароль из .env каталога БД →
// общая база без автомиграции → lease → memsync.Syncer.Run, который сам сверяет
// версию схемы и пишет итог в sync_state. Фон (19.6), CLI (19.7) и морда (19.8)
// запускают прогон через runSync.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"agentmemory/internal/config"
	"agentmemory/internal/memory/store"
	memsync "agentmemory/internal/memory/sync"
)

// Таймаут одного прогона и срок его lease: lease с запасом длиннее прогона,
// чтобы не истечь посреди живого прогона, а цель упавшего процесса
// освобождается не позже него. syncDialTimeout ограничивает установление
// соединения: недоступный сервер — быстрая ошибка в статусе (DESIGN.md §15).
const (
	syncRunTimeout  = 2 * time.Minute
	syncLeaseTTL    = syncRunTimeout + 30*time.Second
	syncDialTimeout = 10 * time.Second
)

// errSyncBusy — цель уже прогоняет другой процесс этой машины (его lease не истёк);
// фоновый планировщик такую цель пропускает без ошибки.
var errSyncBusy = memsync.ErrBusy

// syncHolder — владелец lease от имени этого процесса: pid плюс случайный
// суффикс — pid переиспользуется после перезапуска.
var syncHolder = fmt.Sprintf("pid-%d-%s", os.Getpid(), ulid.Make())

// syncEnvDir — каталог локальной БД: в его .env лежат пароли общих баз целей
// (DESIGN.md §13), а не в проектном .env рядом с agentmemory.toml.
func syncEnvDir(cfg config.StoreConfig) (string, error) {
	path, err := expandHomeLocal(cfg.Path)
	if err != nil {
		return "", fmt.Errorf("каталог локальной БД: %w", err)
	}
	return filepath.Dir(path), nil
}

// runSync — один прогон цели project под lease: name — автор отправляемых
// записей (authorName). Нет цели — store.ErrNotFound; цель занята другим
// процессом — errSyncBusy, итог при этом не пишется (его запишет владелец).
func runSync(ctx context.Context, local *store.SyncLocal, envDir, project, name string) (memsync.Result, error) {
	res := memsync.Result{Project: project}
	t, err := local.Target(ctx, project)
	if err != nil {
		return res, err
	}
	ok, err := local.Lease(ctx, project, syncHolder, time.Now().Add(syncLeaseTTL).Unix())
	if err != nil {
		return res, err
	}
	if !ok {
		return res, errSyncBusy
	}
	defer func() {
		if err := local.Release(context.WithoutCancel(ctx), project, syncHolder); err != nil {
			log.Printf("memo: синхронизация %s: %v", project, err)
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, syncRunTimeout)
	defer cancel()
	s := memsync.New(local, hubDialer{local: local, envDir: envDir}, store.HubSchemaVersion)
	return s.Run(ctx, memsync.Target{Project: t.Project, Push: t.Push, Pull: t.Pull}, name)
}

// hubDialer открывает общую базу проекта по его цели из локальной БД: параметры
// подключения — sync_targets, пароль — syncPassword. Схему не трогает (OpenHub
// без DDL).
type hubDialer struct {
	local  *store.SyncLocal
	envDir string
}

var _ memsync.Dialer = hubDialer{}

func (d hubDialer) Dial(ctx context.Context, project string) (memsync.Shared, error) {
	t, err := d.local.Target(ctx, project)
	if err != nil {
		return nil, err
	}
	password, err := syncPassword(d.envDir, t.PasswordEnv)
	if err != nil {
		return nil, err
	}
	dialCtx, cancel := context.WithTimeout(ctx, syncDialTimeout)
	defer cancel()
	hub, err := store.OpenHub(dialCtx, config.StoreConfig{
		Driver: "postgres", Host: t.Host, Port: t.Port, User: t.User, DBName: t.DBName, SSLMode: t.SSLMode,
		Password: password,
	})
	if err != nil {
		err = redactSecret(err, password)
		if password == "" {
			// Пустой пароль — не ошибка (trust-аутентификация), но при отказе
			// подключения это первая подсказка.
			return nil, fmt.Errorf("%w (пароль не задан: %s в %s)", err, t.PasswordEnv, filepath.Join(d.envDir, ".env"))
		}
		return nil, err
	}
	return hub, nil
}

// redactSecret убирает secret (как есть и в URL-кодировке DSN) из текста err:
// ошибка подключения уходит в sync_state, статус и логи. Драйвер пароль в
// ошибках не печатает — это страховка на случай, если начнёт.
func redactSecret(err error, secret string) error {
	if secret == "" {
		return err
	}
	msg := err.Error()
	redacted := msg
	for _, form := range []string{secret, url.PathEscape(secret), url.QueryEscape(secret), url.UserPassword("", secret).String()[1:]} {
		redacted = strings.ReplaceAll(redacted, form, "***")
	}
	if redacted == msg {
		return err
	}
	return errors.New(redacted)
}

// syncPassword читает пароль общей базы из переменной name: сначала из
// <envDir>/.env, затем из окружения процесса (DESIGN.md §13). Порядок обратный
// loadDotEnv намеренно: файл читается на каждом прогоне, и смена пароля в морде
// действует без перезапуска, а окружение, куда loadDotEnv на старте мог влить
// тот же файл, заморозило бы старый пароль. Нет ни там, ни там — пустой пароль.
func syncPassword(envDir, name string) (string, error) {
	path := filepath.Join(envDir, ".env")
	vars, _, err := parseDotEnv(path)
	if err != nil {
		return "", fmt.Errorf("пароль общей базы: %w", err)
	}
	if v := vars[name]; v != "" {
		return v, nil
	}
	return os.Getenv(name), nil
}

// saveSyncPassword записывает пароль общей базы под именем name в <envDir>/.env:
// слиянием с прочими ключами, через временный файл и rename (читатель не увидит
// недописанный файл), права 0600, каталог — 0700 при создании. Значение
// пишется в одинарных кавычках: parseDotEnv снимает ровно одну пару, поэтому
// пароль с пробелами и кавычками по краям читается как был. Перевод строки в
// .env не записать — такой пароль отвергается. Значение в ошибки не попадает.
func saveSyncPassword(envDir, name, password string) error {
	if strings.ContainsAny(password, "\r\n") {
		return errors.New("пароль общей базы не может содержать перевод строки")
	}
	if err := os.MkdirAll(envDir, 0o700); err != nil {
		return fmt.Errorf("каталог %s: %w", envDir, err)
	}
	path := filepath.Join(envDir, ".env")
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("чтение %s: %w", path, err)
	}
	content := mergeEnvContent(existing, map[string]string{name: "'" + password + "'"})

	tmp, err := os.CreateTemp(envDir, ".env.tmp-*") // CreateTemp создаёт файл с правами 0600
	if err != nil {
		return fmt.Errorf("временный файл для %s: %w", path, err)
	}
	defer os.Remove(tmp.Name()) // после успешного rename файла уже нет — no-op
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return fmt.Errorf("запись %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("запись %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("замена %s: %w", path, err)
	}
	return nil
}
