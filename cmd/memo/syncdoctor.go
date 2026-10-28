// syncdoctor.go — секции «Синхронизация <проект>» в `memo doctor` (Этап 19,
// DESIGN.md §12): по каждой цели — доступность общей базы (проба без DDL с
// таймаутом probeTimeout), версия её схемы, итог последнего прогона, число
// неотправленных и задан ли пароль. Беды общей базы — предупреждение: локальная
// память от них не ломается (DESIGN.md §15).
package main

import (
	"context"
	"fmt"
	"strings"

	"agentmemory/internal/config"
	"agentmemory/internal/memory/store"
)

// syncSections — по секции на цель. Нет локального состояния синхронизации
// (driver = postgres, хранилище не открыто) или целей — секций нет.
func syncSections(ctx context.Context, st store.Store, cfg config.StoreConfig) []section {
	if st == nil {
		return nil
	}
	local, ok := store.SyncLocalOf(st)
	if !ok {
		return nil
	}
	targets, err := local.Targets(ctx)
	if err != nil {
		return []section{{Name: "Синхронизация", Status: statusWarn, Detail: fmt.Sprintf("чтение целей: %v", err)}}
	}
	envDir, err := syncEnvDir(cfg)
	if err != nil {
		return []section{{Name: "Синхронизация", Status: statusWarn, Detail: err.Error()}}
	}
	d := hubDialer{local: local, envDir: envDir}
	out := make([]section, 0, len(targets))
	for _, t := range targets {
		out = append(out, syncTargetSection(ctx, local, d, t))
	}
	return out
}

// syncTargetSection — секция одной цели: всё ✅ только при доступной общей базе,
// совпадающей схеме и успешном последнем прогоне.
func syncTargetSection(ctx context.Context, local *store.SyncLocal, d hubDialer, t store.SyncTarget) section {
	name := "Синхронизация " + t.Project
	ls, err := loadSyncLocalStatus(ctx, local, d.envDir, t)
	if err != nil {
		return section{Name: name, Status: statusWarn, Detail: err.Error()}
	}
	status := statusOK
	parts := []string{
		fmt.Sprintf("отправлять — %s, загружать — %s", yesNo(t.Push), yesNo(t.Pull)),
		"общая база " + syncTargetAddr(t),
		"пароль: " + ls.password,
	}

	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if hub, s, err := openSyncHub(probeCtx, d, t.Project); err != nil {
		status = statusWarn
		parts = append(parts, fmt.Sprintf("недоступна: %v", err))
	} else {
		hub.Close()
		if !s.Current() {
			status = statusWarn
		}
		parts = append(parts, "доступна, схема: "+describeHubSchema(s, t.Project))
	}

	if ls.state.LastError != "" {
		status = statusWarn
	}
	parts = append(parts, ls.lastRun(), fmt.Sprintf("неотправленных: %d", ls.pending))
	return section{Name: name, Status: status, Detail: strings.Join(parts, "; ")}
}
