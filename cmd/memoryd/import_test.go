package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"agentmemory/internal/memory"
	"agentmemory/internal/memory/store"
)

// headerLine сериализует стандартный заголовок формата экспорта (версия/имя
// формата фиксированы — эту же функцию используют тесты, которым нужен
// заведомо валидный заголовок; тесты header-валидации собирают заголовок
// вручную).
func headerLine(t *testing.T) string {
	t.Helper()
	b, err := json.Marshal(exportHeader{Format: exportFormat, Version: exportVersion, ExportedAt: 1700000000, DB: "test.db"})
	if err != nil {
		t.Fatalf("Marshal header: %v", err)
	}
	return string(b)
}

func recordLine(t *testing.T, rec exportRecord) string {
	t.Helper()
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("Marshal record: %v", err)
	}
	return string(b)
}

// writeExportFileRaw пишет строки (уже сериализованные JSON, по одной на
// строку) во временный файл экспорта — даёт тестам полный контроль над
// содержимым файла (включая заведомо битые строки), в отличие от прогона
// через runExport.
func writeExportFileRaw(t *testing.T, dir string, lines []string) string {
	t.Helper()
	path := filepath.Join(dir, "export.jsonl")
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// minimalRecord строит exportRecord с обязательными полями и пустыми (не
// nil) Tags/Links — совпадает с тем, что печатает runExport для записи без
// тегов/ссылок (memoryToExportRecord).
func minimalRecord(id, scope, content string) exportRecord {
	return exportRecord{
		Id: id, Kind: "semantic", Scope: scope, Content: content,
		Importance: 3, CreatedAt: 1000, UpdatedAt: 1000,
		Tags: []string{}, Links: []exportLink{},
	}
}

// TestRunImport_ConflictSkipByDefault проверяет: повторный импорт того же
// файла в ту же БД по умолчанию пропускает конфликтующий id и не меняет
// существующую запись.
func TestRunImport_ConflictSkipByDefault(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "memory.db")
	id := ulid.Make().String()
	path := writeExportFileRaw(t, dir, []string{headerLine(t), recordLine(t, minimalRecord(id, "project:agentmemory", "оригинал"))})

	var out1 bytes.Buffer
	if err := runImport([]string{"-db", dbPath, path}, &out1); err != nil {
		t.Fatalf("первый import: %v", err)
	}
	if !strings.Contains(out1.String(), "imported 1, skipped 0, links 0 (dropped 0)") {
		t.Errorf("первый import вывод = %q, want imported 1, skipped 0", out1.String())
	}

	var out2 bytes.Buffer
	if err := runImport([]string{"-db", dbPath, path}, &out2); err != nil {
		t.Fatalf("второй import: %v", err)
	}
	if !strings.Contains(out2.String(), "imported 0, skipped 1, links 0 (dropped 0)") {
		t.Errorf("второй import вывод = %q, want imported 0, skipped 1 (конфликт id)", out2.String())
	}

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()
	got, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Content != "оригинал" {
		t.Errorf("Content = %q, want %q (skip не должен был менять запись)", got.Content, "оригинал")
	}
}

// TestRunImport_Overwrite проверяет флаг -overwrite: конфликтующий id
// заменяется содержимым файла.
func TestRunImport_Overwrite(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "memory.db")
	id := ulid.Make().String()

	path1 := writeExportFileRaw(t, dir, []string{headerLine(t), recordLine(t, minimalRecord(id, "project:agentmemory", "первая версия"))})
	var out1 bytes.Buffer
	if err := runImport([]string{"-db", dbPath, path1}, &out1); err != nil {
		t.Fatalf("первый import: %v", err)
	}

	path2 := filepath.Join(dir, "export2.jsonl")
	if err := os.WriteFile(path2, []byte(headerLine(t)+"\n"+recordLine(t, minimalRecord(id, "project:agentmemory", "вторая версия"))+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var out2 bytes.Buffer
	if err := runImport([]string{"-db", dbPath, "-overwrite", path2}, &out2); err != nil {
		t.Fatalf("второй import -overwrite: %v", err)
	}
	if !strings.Contains(out2.String(), "imported 1, skipped 0, links 0 (dropped 0)") {
		t.Errorf("import -overwrite вывод = %q, want imported 1, skipped 0", out2.String())
	}

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()
	got, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Content != "вторая версия" {
		t.Errorf("Content = %q, want %q (-overwrite должен был заменить запись)", got.Content, "вторая версия")
	}
}

// TestRunImport_Rescope проверяет, что -rescope переписывает только scope,
// остальные поля записи остаются как в файле.
func TestRunImport_Rescope(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "memory.db")
	id := ulid.Make().String()
	path := writeExportFileRaw(t, dir, []string{headerLine(t), recordLine(t, minimalRecord(id, "project:old", "контент без изменений"))})

	var out bytes.Buffer
	if err := runImport([]string{"-db", dbPath, "-rescope", "project:old=project:new", path}, &out); err != nil {
		t.Fatalf("import -rescope: %v", err)
	}

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()
	got, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Scope != "project:new" {
		t.Errorf("Scope = %q, want %q", got.Scope, "project:new")
	}
	if got.Content != "контент без изменений" {
		t.Errorf("Content = %q, изменился хотя -rescope должен трогать только scope", got.Content)
	}
}

// TestRunImport_MalformedLineErrorWithLineNumber проверяет, что битая строка
// JSONL прерывает импорт с ошибкой, содержащей номер строки, и не создаёт БД
// (весь файл разбирается до открытия хранилища).
func TestRunImport_MalformedLineErrorWithLineNumber(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "memory.db")
	path := writeExportFileRaw(t, dir, []string{
		headerLine(t),
		recordLine(t, minimalRecord(ulid.Make().String(), "project:agentmemory", "валидная запись")),
		"{это не json",
	})

	var out bytes.Buffer
	err := runImport([]string{"-db", dbPath, path}, &out)
	if err == nil {
		t.Fatal("runImport вернул nil, want ошибку про битую строку 3")
	}
	if !strings.Contains(err.Error(), "3") {
		t.Errorf("ошибка = %q, want упоминание номера строки 3", err.Error())
	}
	if _, statErr := os.Stat(dbPath); !os.IsNotExist(statErr) {
		t.Errorf("файл БД %s создан, хотя импорт должен был упасть до открытия хранилища", dbPath)
	}
}

// TestRunImport_DryRunDoesNotCreateDB проверяет архитектурную гарантию:
// -dry-run вообще не открывает Store, поэтому файл БД не создаётся.
func TestRunImport_DryRunDoesNotCreateDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "memory.db")
	path := writeExportFileRaw(t, dir, []string{headerLine(t), recordLine(t, minimalRecord(ulid.Make().String(), "project:agentmemory", "x"))})

	var out bytes.Buffer
	if err := runImport([]string{"-db", dbPath, "-dry-run", path}, &out); err != nil {
		t.Fatalf("runImport -dry-run: %v", err)
	}
	if !strings.Contains(out.String(), "imported 1, skipped 0, links 0 (dropped 0)") {
		t.Errorf("вывод dry-run = %q, want imported 1, skipped 0", out.String())
	}
	if _, statErr := os.Stat(dbPath); !os.IsNotExist(statErr) {
		t.Errorf("файл БД %s создан под -dry-run, ожидалось отсутствие файла", dbPath)
	}
}

// TestRunImport_DanglingLinkWarning проверяет: ссылка на отсутствующий id —
// предупреждение и пропуск ребра, остальной импорт продолжается.
func TestRunImport_DanglingLinkWarning(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "memory.db")
	targetID := ulid.Make().String()
	srcID := ulid.Make().String()
	missingID := ulid.Make().String()

	target := minimalRecord(targetID, "project:agentmemory", "цель ссылки")
	src := minimalRecord(srcID, "project:agentmemory", "источник ссылки")
	src.Links = []exportLink{{Dst: targetID, Rel: "related"}, {Dst: missingID, Rel: "related"}}

	path := writeExportFileRaw(t, dir, []string{headerLine(t), recordLine(t, target), recordLine(t, src)})

	var out bytes.Buffer
	if err := runImport([]string{"-db", dbPath, path}, &out); err != nil {
		t.Fatalf("runImport: %v", err)
	}
	if !strings.Contains(out.String(), "imported 2, skipped 0, links 1 (dropped 1)") {
		t.Errorf("вывод = %q, want imported 2, links 1 (dropped 1)", out.String())
	}
	if !strings.Contains(out.String(), missingID) {
		t.Errorf("вывод не содержит предупреждение про отсутствующий id %s:\n%s", missingID, out.String())
	}

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()
	got, err := s.Get(context.Background(), srcID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Links) != 1 || got.Links[0].Dst != targetID {
		t.Errorf("Links = %+v, want одно ребро -> %s (висячее пропущено)", got.Links, targetID)
	}
}

// TestRunImport_UnknownVersionRejected проверяет, что неизвестная версия
// формата — ошибка, а не молчаливый разбор.
func TestRunImport_UnknownVersionRejected(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "memory.db")
	badHeader, err := json.Marshal(exportHeader{Format: exportFormat, Version: 999, ExportedAt: 1, DB: "x"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	path := writeExportFileRaw(t, dir, []string{string(badHeader), recordLine(t, minimalRecord(ulid.Make().String(), "project:agentmemory", "x"))})

	err = runImport([]string{"-db", dbPath, path}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("runImport вернул nil для version=999, want ошибку")
	}
	if !strings.Contains(err.Error(), "999") {
		t.Errorf("ошибка = %q, want упоминание version 999", err.Error())
	}
}

// TestRunImport_RoundTripByteForByte — главный инвариант задачи 9.2:
// export -> import в пустую scratch-БД -> повторный export побайтово равен
// первому (кроме заголовка — exported_at/db в нём заведомо различаются между
// двумя независимыми прогонами export). Покрывает id, все таймстампы,
// access_count, tags, links, тумстоуны за один прогон.
func TestRunImport_RoundTripByteForByte(t *testing.T) {
	srcDBPath, srcStore := newExportTestStore(t)

	idB := ulid.Make().String() // цель ссылки — вставляем первой, чтобы не зависеть от FK-порядка
	idA := ulid.Make().String()
	idC := ulid.Make().String() // тумстоун, superseded_by -> idA

	mustPut(t, srcStore, memoryRec{
		id: idB, scope: "project:agentmemory", content: "запись B — цель ссылки",
		importance: 2, createdAt: 900, updatedAt: 900,
	})
	mustPut(t, srcStore, memoryRec{
		id: idA, scope: "project:agentmemory", content: "запись A — полный набор полей",
		importance: 5, createdAt: 1000, updatedAt: 1200,
		summary: strPtr("A summary"), accessedAt: int64Ptr(1300), accessCount: 7,
		source: strPtr("session-42"), tags: []string{"b-tag", "a-tag"},
		links: []exportLink{{Dst: idB, Rel: "related"}},
		// expiresAt (задача 9.4) — регресс на кросс-задачную потерю TTL при
		// export->import: recordToMemory/memoryToExportRecord обязаны
		// пробрасывать expires_at так же честно, как остальные поля.
		expiresAt: 999999999,
	})
	mustPut(t, srcStore, memoryRec{
		id: idC, scope: "project:agentmemory", content: "запись C — тумстоун",
		importance: 3, createdAt: 800, updatedAt: 850,
		supersededBy: strPtr(idA), links: []exportLink{{Dst: idA, Rel: "supersedes"}},
	})

	var firstExport bytes.Buffer
	if err := runExport([]string{"-db", srcDBPath}, &firstExport); err != nil {
		t.Fatalf("первый export: %v", err)
	}

	dir := t.TempDir()
	backupPath := filepath.Join(dir, "backup.jsonl")
	if err := os.WriteFile(backupPath, firstExport.Bytes(), 0o644); err != nil {
		t.Fatalf("WriteFile backup: %v", err)
	}

	scratchDBPath := filepath.Join(dir, "scratch.db")
	var importOut bytes.Buffer
	if err := runImport([]string{"-db", scratchDBPath, backupPath}, &importOut); err != nil {
		t.Fatalf("import: %v", err)
	}
	if !strings.Contains(importOut.String(), "imported 3, skipped 0, links 2 (dropped 0)") {
		t.Errorf("вывод import = %q, want imported 3, links 2 (dropped 0)", importOut.String())
	}

	// Регресс-проверка (задача 9.4): expires_at должен реально попасть в JSONL
	// первого экспорта, а не просто совпасть между первым/вторым экспортом по
	// какой-то другой причине (например, если бы поле всегда терялось молча на
	// обоих прогонах). ExpiresAt=0 (вечная, идёт с omitempty) для idB/idC ниже
	// подтверждает, что поле не появляется в JSONL для не-TTL записей.
	if !strings.Contains(firstExport.String(), `"expires_at":999999999`) {
		t.Fatalf("первый export не содержит expires_at записи A:\n%s", firstExport.String())
	}

	var secondExport bytes.Buffer
	if err := runExport([]string{"-db", scratchDBPath}, &secondExport); err != nil {
		t.Fatalf("второй export: %v", err)
	}

	firstLines := strings.Split(strings.TrimRight(firstExport.String(), "\n"), "\n")
	secondLines := strings.Split(strings.TrimRight(secondExport.String(), "\n"), "\n")
	if len(firstLines) != len(secondLines) {
		t.Fatalf("число строк первого экспорта (%d) != второго (%d)", len(firstLines), len(secondLines))
	}
	// Строка 0 — заголовок: exported_at/db заведомо различаются между двумя
	// независимыми прогонами export (разные пути БД, возможно разная секунда
	// wall-clock) — round-trip-инвариант касается ТЕЛА экспорта, не служебных
	// метаданных заголовка.
	for i := 1; i < len(firstLines); i++ {
		if firstLines[i] != secondLines[i] {
			t.Errorf("строка %d различается:\nбыло:  %s\nстало: %s", i, firstLines[i], secondLines[i])
		}
	}
}

// memoryRec — плоский набор полей для mustPut (тестовый хелпер), избавляет
// TestRunImport_RoundTripByteForByte от повторения memory.Memory{...} с
// указателями на каждое поле.
type memoryRec struct {
	id, scope, content            string
	importance                    int
	createdAt, updatedAt          int64
	summary, source, supersededBy *string
	accessedAt                    *int64
	accessCount                   int
	expiresAt                     int64 // TTL (задача 9.4), 0 = вечная
	tags                          []string
	links                         []exportLink
}

// mustPut вставляет r в s через recordToMemory — тот же путь конвертации
// exportRecord -> memory.Memory, что использует сам import.go, поэтому
// исходные данные округляются по тем же правилам (Links сразу целиком,
// FK-порядок здесь не важен: idB уже вставлен раньше idA/idC в вызывающем
// тесте).
func mustPut(t *testing.T, s store.Store, r memoryRec) {
	t.Helper()
	m := recordToMemory(exportRecord{
		Id: r.id, Kind: "semantic", Scope: r.scope, Content: r.content,
		Summary: r.summary, Importance: r.importance, CreatedAt: r.createdAt, UpdatedAt: r.updatedAt,
		AccessedAt: r.accessedAt, AccessCount: r.accessCount, SupersededBy: r.supersededBy,
		Source: r.source, ExpiresAt: r.expiresAt, Tags: r.tags,
	})
	for _, l := range r.links {
		m.Links = append(m.Links, memory.Link{Dst: l.Dst, Rel: l.Rel})
	}
	if err := s.Put(context.Background(), m); err != nil {
		t.Fatalf("Put %s: %v", r.id, err)
	}
}
