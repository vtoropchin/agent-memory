// Подкоманда `memo import` — восстановление переносимого JSONL-бэкапа (см.
// export.go) в SQLite-хранилище с сохранением идентичности записей: id (ULID)
// и все таймстампы переносятся как есть, а не пересчитываются. В отличие от
// `memimport`/write.Writer (легаси-путь для .claude/memory/*.md), импорт идёт
// напрямую через store.Put — это точное восстановление бэкапа, а не приём
// нового факта, поэтому дедупликация/классификация write-пути здесь были бы
// искажением, а не помощью (DESIGN.md §7, §15).
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"agentmemory/internal/config"
	"agentmemory/internal/memory"
	"agentmemory/internal/memory/store"
)

// lineRecord — разобранная строка тела экспорта вместе с номером строки в
// исходном файле (для сообщений об ошибках/предупреждений с привязкой к
// строке, задача 9.2).
type lineRecord struct {
	line int
	rec  exportRecord
}

// parseExportFile разбирает JSONL-файл экспорта: первая строка — заголовок
// (проверка format/version — на совести вызывающей стороны, runImport),
// остальные — по одной записи на строку. Любая непарсящаяся строка (битый
// JSON) прерывает разбор целиком с указанием номера строки — весь файл
// разбирается ДО того, как runImport хоть раз откроет хранилище на запись,
// поэтому битый файл не может привести к частичному импорту.
func parseExportFile(r io.Reader) (exportHeader, []lineRecord, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 10*1024*1024) // записи могут быть длиннее дефолтных 64KiB на строку

	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return exportHeader{}, nil, fmt.Errorf("import: чтение заголовка: %w", err)
		}
		return exportHeader{}, nil, fmt.Errorf("import: пустой файл, заголовок не найден")
	}
	var header exportHeader
	if err := json.Unmarshal(sc.Bytes(), &header); err != nil {
		return exportHeader{}, nil, fmt.Errorf("import: строка 1: разбор заголовка: %w", err)
	}

	var records []lineRecord
	lineNum := 1
	for sc.Scan() {
		lineNum++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue // пустая строка (обычно хвост файла) — не запись, не ошибка
		}
		var rec exportRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return exportHeader{}, nil, fmt.Errorf("import: строка %d: %w", lineNum, err)
		}
		records = append(records, lineRecord{line: lineNum, rec: rec})
	}
	if err := sc.Err(); err != nil {
		return exportHeader{}, nil, fmt.Errorf("import: чтение файла: %w", err)
	}
	return header, records, nil
}

// recordToMemory переводит разобранную строку экспорта в доменную Memory
// (обратное к memoryToExportRecord). Links сюда сознательно не попадают —
// importRecords заполняет их отдельным вторым проходом: FK на links.dst
// (DESIGN.md §4, `_foreign_keys=on` в DSN store.Open) требует, чтобы запись-
// цель уже существовала в memories, а при прямом (форвардном) порядке ссылок
// в файле цель может идти позже источника.
func recordToMemory(rec exportRecord) memory.Memory {
	return memory.Memory{
		Id:           rec.Id,
		Kind:         memory.Kind(rec.Kind),
		Scope:        rec.Scope,
		Content:      rec.Content,
		Summary:      rec.Summary,
		Importance:   rec.Importance,
		CreatedAt:    rec.CreatedAt,
		UpdatedAt:    rec.UpdatedAt,
		AccessedAt:   rec.AccessedAt,
		AccessCount:  rec.AccessCount,
		SupersededBy: rec.SupersededBy,
		Source:       rec.Source,
		ExpiresAt:    rec.ExpiresAt,
		Tags:         rec.Tags,
	}
}

// importSummary — итог одного прогона `memo import` (см. printImportSummary).
// LinksOK/LinksDropped считают рёбра (не записи): одна запись с тремя links,
// из которых один повис, даёт LinksOK+=2, LinksDropped+=1.
type importSummary struct {
	Imported     int
	Skipped      int
	LinksOK      int
	LinksDropped int
	Warnings     []string
}

// importRecords восстанавливает records в s. Для каждой записи: конфликт id
// (запись уже есть) — по умолчанию skip и счётчик; overwrite=true — Put
// заменяет содержимое. Put используется напрямую (не write.Writer) и сохраняет
// id/все таймстампы ровно такими, какие пришли из файла (store.Put не
// пересчитывает created_at/updated_at/accessed_at/access_count — см. отчёт
// задачи 9.2) — восстановление бэкапа получается точным.
//
// Links проставляются вторым проходом и только для записей, реально
// записанных в этом прогоне (imported или overwritten, не skipped): у
// пропущенной записи уже есть собственное состояние в хранилище, трогать его
// не нужно. dst, отсутствующий в хранилище (ни в этом файле, ни ранее в БД) —
// предупреждение и пропуск ребра (DESIGN.md §15 — деградация, а не падение).
func importRecords(ctx context.Context, s store.Store, records []lineRecord, overwrite bool) (importSummary, error) {
	var sum importSummary
	written := make([]lineRecord, 0, len(records))

	for _, lr := range records {
		m := recordToMemory(lr.rec)

		_, err := s.Get(ctx, m.Id)
		exists := err == nil
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return sum, fmt.Errorf("строка %d: проверка id %q: %w", lr.line, m.Id, err)
		}

		if exists && !overwrite {
			sum.Skipped++
			continue
		}

		if err := s.Put(ctx, m); err != nil {
			return sum, fmt.Errorf("строка %d: запись %q: %w", lr.line, m.Id, err)
		}
		sum.Imported++
		written = append(written, lr)
	}

	for _, lr := range written {
		if len(lr.rec.Links) == 0 {
			continue
		}
		var resolved []memory.Link
		for _, l := range lr.rec.Links {
			if _, err := s.Get(ctx, l.Dst); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					sum.Warnings = append(sum.Warnings, fmt.Sprintf("строка %d: %s: ссылка на отсутствующий id %s пропущена", lr.line, lr.rec.Id, l.Dst))
					sum.LinksDropped++
					continue
				}
				return sum, fmt.Errorf("строка %d: проверка ссылки %s -> %s: %w", lr.line, lr.rec.Id, l.Dst, err)
			}
			resolved = append(resolved, memory.Link{Dst: l.Dst, Rel: l.Rel})
		}
		if len(resolved) == 0 {
			continue
		}
		m := recordToMemory(lr.rec)
		m.Links = resolved
		if err := s.Put(ctx, m); err != nil {
			return sum, fmt.Errorf("строка %d: запись ссылок %q: %w", lr.line, m.Id, err)
		}
		sum.LinksOK += len(resolved)
	}

	return sum, nil
}

// dryRunSummary считает ту же сводку, что importRecords, но БЕЗ обращения к
// Store — dry-run не открывает хранилище вовсе (архитектурная гарантия
// задачи 9.2, как у memimport: `Run(..., dryRun=true, ...)` вызывается с
// w=nil). Конфликт id поэтому не проверяется (проверять не с чем — реальной
// БД нет), Skipped всегда 0. Ссылки проверяются только на самосогласованность
// внутри самого файла (dst встречается среди id этого же файла): dst,
// которого в файле нет, может как отсутствовать вообще, так и существовать в
// целевой БД (это не проверить без открытия хранилища) — поэтому в dry-run
// такая ссылка не считается окончательно пропущенной, а помечается
// примечанием, что реальный import перепроверит её по целевой БД.
func dryRunSummary(records []lineRecord) importSummary {
	known := make(map[string]bool, len(records))
	for _, lr := range records {
		known[lr.rec.Id] = true
	}

	var sum importSummary
	for _, lr := range records {
		sum.Imported++
		for _, l := range lr.rec.Links {
			if known[l.Dst] {
				sum.LinksOK++
				continue
			}
			sum.LinksDropped++
			sum.Warnings = append(sum.Warnings, fmt.Sprintf(
				"строка %d: %s: ссылка на id %s не найдена среди записей файла (dry-run не открывает БД — реальный import перепроверит по целевому хранилищу)",
				lr.line, lr.rec.Id, l.Dst))
		}
	}
	return sum
}

// parseRescope разбирает флаг -rescope "project:old=project:new" в пару
// (from, to). Пустая строка означает «rescope не запрошен» — (from="", to="", err=nil).
func parseRescope(flagVal string) (from, to string, err error) {
	if flagVal == "" {
		return "", "", nil
	}
	from, to, ok := strings.Cut(flagVal, "=")
	if !ok || from == "" || to == "" {
		return "", "", fmt.Errorf("import: -rescope должен быть вида old=new, получено %q", flagVal)
	}
	return from, to, nil
}

// printImportSummary печатает итоговую строку в формате задачи 9.2
// (`imported N, skipped M, links K (dropped L)`) и предупреждения построчно
// (по образцу printSummary из cmd/memimport).
func printImportSummary(out io.Writer, sum importSummary) {
	fmt.Fprintf(out, "imported %d, skipped %d, links %d (dropped %d)\n", sum.Imported, sum.Skipped, sum.LinksOK, sum.LinksDropped)
	for _, w := range sum.Warnings {
		fmt.Fprintf(out, "warning: %s\n", w)
	}
}

// runImport разбирает флаги подкоманды `import` и восстанавливает записи из
// file.jsonl. Вынесена из диспетчера для тестируемости (по образцу остальных
// подкоманд).
func runImport(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fmt.Fprintln(out, "импорт JSONL-бэкапа (см. `memo export`) с сохранением id и всех таймстампов.")
		fmt.Fprintln(out, "Использование: memo import [флаги] file.jsonl")
		fs.PrintDefaults()
	}
	configPath := fs.String("config", "", "путь к TOML-конфигу (опционально)")
	dbPath := fs.String("db", "", "путь к файлу SQLite, переопределяет store.path из конфига (опционально)")
	dryRun := fs.Bool("dry-run", false, "только разобрать файл и напечатать отчёт; БД не открывается вовсе")
	overwrite := fs.Bool("overwrite", false, "заменять записи с конфликтующим id (по умолчанию — пропуск)")
	rescopeFlag := fs.String("rescope", "", "перенос scope при импорте: old=new (напр. project:a=project:b); переписывает только scope")
	_ = fs.Parse(args) // flag.ExitOnError сам завершает процесс при ошибке разбора/-h

	if fs.NArg() < 1 {
		return fmt.Errorf("import: не указан файл экспорта — memo import [флаги] file.jsonl")
	}
	path := fs.Arg(0)

	rescopeFrom, rescopeTo, err := parseRescope(*rescopeFlag)
	if err != nil {
		return err
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("import: открытие файла %q: %w", path, err)
	}
	defer f.Close()

	header, records, err := parseExportFile(f)
	if err != nil {
		return err
	}
	if header.Format != exportFormat {
		return fmt.Errorf("import: неизвестный формат файла %q (ожидался %q)", header.Format, exportFormat)
	}
	if header.Version != exportVersion {
		return fmt.Errorf("import: неизвестная version %d (эта memo поддерживает %d)", header.Version, exportVersion)
	}

	if rescopeFrom != "" {
		for i := range records {
			if records[i].rec.Scope == rescopeFrom {
				records[i].rec.Scope = rescopeTo
			}
		}
	}

	if *dryRun {
		printImportSummary(out, dryRunSummary(records))
		fmt.Fprintln(out, "dry-run: БД не открывалась — конфликты id и ссылки на записи вне файла не проверялись")
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("загрузка конфига: %w", err)
	}
	if *dbPath != "" {
		cfg.Store.Path = *dbPath
	}

	s, err := store.Open(cfg.Store.Path)
	if err != nil {
		return fmt.Errorf("открытие хранилища %q: %w", cfg.Store.Path, err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			log.Printf("memo import: закрытие хранилища: %v", err)
		}
	}()

	ctx := context.Background()
	sum, err := importRecords(ctx, s, records, *overwrite)
	if err != nil {
		return fmt.Errorf("import: %w", err)
	}
	printImportSummary(out, sum)
	fmt.Fprintln(out, "векторы будут досчитаны фоновым reindex при следующем запуске memo (DESIGN.md §9, §11)")
	return nil
}
