// Общая механика двухпроходной FK-safe записи, которой пользуются `memo
// import` (import.go) и `memo migrate` (migrate.go): обе команды переносят
// записи памяти в чужое хранилище, где связи (links.dst, FK, DESIGN.md §4,
// `_foreign_keys=on` в DSN store.OpenSQLite) могут ссылаться вперёд на
// запись, которая появится в приёмнике позже в том же прогоне. Первый проход
// пишет каждую запись БЕЗ Links (конфликт id — Skipped, если !overwrite,
// иначе Put заменяет содержимое), второй — досыпает Links только для
// записей, реально записанных в этом прогоне (written или overwritten, не
// skipped — у пропущенной уже есть собственное состояние в приёмнике,
// трогать его не нужно). dst, отсутствующий в приёмнике — предупреждение и
// пропуск ребра (DESIGN.md §15 — деградация, а не отказ всей записи).
//
// Тексты вывода/предупреждений у import и migrate осмысленно различаются
// (нумерация строк файла у import, "в приёмнике" у migrate) — это остаётся
// снаружи fkSafeWrite: droppedLinkWarning параметризует ровно эту разницу,
// а не единый шаблон под обе команды.
package main

import (
	"context"
	"errors"
	"fmt"

	"agentmemory/internal/memory"
	"agentmemory/internal/memory/store"
)

// fkSafeResult — счётчики одного прогона fkSafeWrite. import.go/migrate.go
// конвертируют его в собственные importSummary/migrateSummary — разные имена
// полей и итоговый текст (`printImportSummary`/`printMigrateSummary`) эта
// унификация не трогает.
type fkSafeResult struct {
	Written      int
	Skipped      int
	LinksOK      int
	LinksDropped int
	Warnings     []string
}

// fkSafeWrite пишет records в dst в два прохода (см. комментарий файла).
// droppedLinkWarning строит текст предупреждения для ребра srcID -> missingDst,
// dst которого не нашёлся в приёмнике. onWritten, если не nil, вызывается
// синхронно сразу после каждой успешной записи первого прохода с (index,
// total) — index здесь позиция записи в исходном срезе records (0-based, как
// в `for i, m := range records`), а не порядковый номер среди записанных: так
// колбэк прогресса `memo migrate` (единственный потребитель onWritten)
// печатает ровно то же самое условие `(i+1)%migrateProgressEvery == 0`, что
// было в migrateRecords до вынесения этой функции. `memo import` прогресс не
// печатает и передаёт nil.
func fkSafeWrite(ctx context.Context, dst store.Store, records []memory.Memory, overwrite bool, droppedLinkWarning func(srcID, missingDst string) string, onWritten func(index, total int)) (fkSafeResult, error) {
	var res fkSafeResult
	written := make([]memory.Memory, 0, len(records))

	for i, m := range records {
		_, err := dst.Get(ctx, m.Id)
		exists := err == nil
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return fkSafeResult{}, fmt.Errorf("проверка id %q: %w", m.Id, err)
		}

		if exists && !overwrite {
			res.Skipped++
			continue
		}

		wm := m
		wm.Links = nil
		if err := dst.Put(ctx, wm); err != nil {
			return fkSafeResult{}, fmt.Errorf("запись %q: %w", m.Id, err)
		}
		res.Written++
		written = append(written, m)

		if onWritten != nil {
			onWritten(i, len(records))
		}
	}

	for _, m := range written {
		if len(m.Links) == 0 {
			continue
		}
		var resolved []memory.Link
		for _, l := range m.Links {
			if _, err := dst.Get(ctx, l.Dst); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					res.Warnings = append(res.Warnings, droppedLinkWarning(m.Id, l.Dst))
					res.LinksDropped++
					continue
				}
				return fkSafeResult{}, fmt.Errorf("проверка ссылки %s -> %s: %w", m.Id, l.Dst, err)
			}
			resolved = append(resolved, l)
		}
		if len(resolved) == 0 {
			continue
		}
		wm := m
		wm.Links = resolved
		if err := dst.Put(ctx, wm); err != nil {
			return fkSafeResult{}, fmt.Errorf("запись связей %q: %w", m.Id, err)
		}
		res.LinksOK += len(resolved)
	}

	return res, nil
}
