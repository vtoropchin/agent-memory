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

	"agentmemory/internal/config"
	"agentmemory/internal/memory"
	"agentmemory/internal/memory/store"
)

// newExportTestStore открывает scratch-хранилище во временном каталоге теста
// (по образцу newDoctorTestStore из doctor_test.go) и возвращает путь вместе с
// открытым Store для наполнения тестовыми записями.
func newExportTestStore(t *testing.T) (path string, st store.Store) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "memory.db")
	s, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return path, s
}

func strPtr(s string) *string { return &s }
func int64Ptr(v int64) *int64 { return &v }

// TestRunExport_HeaderAndOrder проверяет, что первая строка — корректный
// заголовок формата, а тело отсортировано по id (детерминированность,
// диффабельность экспортов, задача 9.2).
func TestRunExport_HeaderAndOrder(t *testing.T) {
	dbPath, st := newExportTestStore(t)
	ctx := context.Background()

	idA := ulid.Make().String()
	idB := ulid.Make().String()
	// Вставляем в обратном (не по id) порядке — проверяем, что export
	// сортирует сам, а не полагается на порядок вставки.
	if err := st.Put(ctx, memory.Memory{Id: idB, Kind: memory.KindSemantic, Scope: "project:agentmemory", Content: "B", Importance: 3, CreatedAt: 2000, UpdatedAt: 2000}); err != nil {
		t.Fatalf("Put B: %v", err)
	}
	if err := st.Put(ctx, memory.Memory{Id: idA, Kind: memory.KindSemantic, Scope: "project:agentmemory", Content: "A", Importance: 3, CreatedAt: 1000, UpdatedAt: 1000}); err != nil {
		t.Fatalf("Put A: %v", err)
	}

	var out bytes.Buffer
	if err := runExport([]string{"-db", dbPath}, &out); err != nil {
		t.Fatalf("runExport: %v", err)
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("ожидалось 3 строки (заголовок + 2 записи), получено %d:\n%s", len(lines), out.String())
	}

	var header exportHeader
	if err := json.Unmarshal([]byte(lines[0]), &header); err != nil {
		t.Fatalf("разбор заголовка: %v", err)
	}
	if header.Format != exportFormat || header.Version != exportVersion {
		t.Errorf("header = %+v, want format=%q version=%d", header, exportFormat, exportVersion)
	}
	if header.DB != dbPath {
		t.Errorf("header.DB = %q, want %q", header.DB, dbPath)
	}

	var recA, recB exportRecord
	if err := json.Unmarshal([]byte(lines[1]), &recA); err != nil {
		t.Fatalf("разбор строки 1: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[2]), &recB); err != nil {
		t.Fatalf("разбор строки 2: %v", err)
	}
	if recA.Id != idA || recB.Id != idB {
		t.Errorf("порядок записей = (%s, %s), want (%s, %s) — по возрастанию id", recA.Id, recB.Id, idA, idB)
	}
}

// TestRunExport_IncludesTombstonesTagsLinks проверяет, что тумстоуны
// (superseded_by) экспортируются как есть, а Tags/Links попадают в вывод —
// экспорт не через AllForReindex/List (те либо режут superseded, либо не
// грузят Links).
func TestRunExport_IncludesTombstonesTagsLinks(t *testing.T) {
	dbPath, st := newExportTestStore(t)
	ctx := context.Background()

	oldID := ulid.Make().String()
	newID := ulid.Make().String()
	if err := st.Put(ctx, memory.Memory{
		Id: newID, Kind: memory.KindSemantic, Scope: "project:agentmemory",
		Content: "новая версия факта", Importance: 4, CreatedAt: 2000, UpdatedAt: 2000,
		Tags: []string{"a", "b"},
	}); err != nil {
		t.Fatalf("Put newID: %v", err)
	}
	if err := st.Put(ctx, memory.Memory{
		Id: oldID, Kind: memory.KindSemantic, Scope: "project:agentmemory",
		Content: "старая версия факта", Importance: 3, CreatedAt: 1000, UpdatedAt: 1500,
		SupersededBy: strPtr(newID),
		Links:        []memory.Link{{Dst: newID, Rel: memory.RelSupersedes}},
	}); err != nil {
		t.Fatalf("Put oldID: %v", err)
	}

	var out bytes.Buffer
	if err := runExport([]string{"-db", dbPath}, &out); err != nil {
		t.Fatalf("runExport: %v", err)
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	records := map[string]exportRecord{}
	for _, line := range lines[1:] {
		var rec exportRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("разбор строки %q: %v", line, err)
		}
		records[rec.Id] = rec
	}

	old, ok := records[oldID]
	if !ok {
		t.Fatalf("тумстоун %s отсутствует в экспорте", oldID)
	}
	if old.SupersededBy == nil || *old.SupersededBy != newID {
		t.Errorf("old.SupersededBy = %v, want %s", old.SupersededBy, newID)
	}
	if len(old.Links) != 1 || old.Links[0].Dst != newID || old.Links[0].Rel != memory.RelSupersedes {
		t.Errorf("old.Links = %+v, want одно ребро -> %s (rel=%s)", old.Links, newID, memory.RelSupersedes)
	}

	newRec, ok := records[newID]
	if !ok {
		t.Fatalf("живая запись %s отсутствует в экспорте", newID)
	}
	if len(newRec.Tags) != 2 || newRec.Tags[0] != "a" || newRec.Tags[1] != "b" {
		t.Errorf("newRec.Tags = %v, want [a b]", newRec.Tags)
	}
}

// TestRunExport_ScopesFilter проверяет частичный экспорт: -scopes ограничивает
// вывод указанным пространством, записи другого scope не попадают в файл.
func TestRunExport_ScopesFilter(t *testing.T) {
	dbPath, st := newExportTestStore(t)
	ctx := context.Background()

	idAlpha := ulid.Make().String()
	idBeta := ulid.Make().String()
	if err := st.Put(ctx, memory.Memory{Id: idAlpha, Kind: memory.KindSemantic, Scope: "project:alpha", Content: "alpha", Importance: 3, CreatedAt: 1000, UpdatedAt: 1000}); err != nil {
		t.Fatalf("Put alpha: %v", err)
	}
	if err := st.Put(ctx, memory.Memory{Id: idBeta, Kind: memory.KindSemantic, Scope: "project:beta", Content: "beta", Importance: 3, CreatedAt: 1000, UpdatedAt: 1000}); err != nil {
		t.Fatalf("Put beta: %v", err)
	}

	var out bytes.Buffer
	if err := runExport([]string{"-db", dbPath, "-scopes", "project:alpha"}, &out); err != nil {
		t.Fatalf("runExport: %v", err)
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("ожидалась 1 запись (заголовок + 1), получено %d строк:\n%s", len(lines), out.String())
	}
	var rec exportRecord
	if err := json.Unmarshal([]byte(lines[1]), &rec); err != nil {
		t.Fatalf("разбор записи: %v", err)
	}
	if rec.Id != idAlpha {
		t.Errorf("экспортированная запись id=%s, want %s (beta не должен попасть)", rec.Id, idAlpha)
	}
}

// TestExportHeaderFor_PostgresUsesTarget проверяет регресс код-ревью:
// заголовок экспорта для driver=postgres берёт "db" из cfg.Target()
// (host:port/dbname), а не из cfg.Path — Path для postgres всегда пуст, и до
// этой правки поле "db" в заголовке JSONL-бэкапа молча становилось пустой
// строкой.
func TestExportHeaderFor_PostgresUsesTarget(t *testing.T) {
	cfg := config.StoreConfig{Driver: "postgres", Host: "postgres.lan", Port: 5432, DBName: "memo", SSLMode: "disable"}
	header := exportHeaderFor(cfg)
	want := "postgres://postgres.lan:5432/memo"
	if header.DB != want {
		t.Errorf("header.DB = %q, want %q", header.DB, want)
	}
	if header.Format != exportFormat || header.Version != exportVersion {
		t.Errorf("header = %+v, want format=%q version=%d", header, exportFormat, exportVersion)
	}
}

// TestExportHeaderFor_SQLiteUsesPath проверяет, что для sqlite (driver="" —
// дефолт) "db" остаётся путём к файлу, как и раньше (cfg.Target() для sqlite
// возвращает cfg.Path — см. config.StoreConfig.Target).
func TestExportHeaderFor_SQLiteUsesPath(t *testing.T) {
	cfg := config.StoreConfig{Path: "/tmp/memory.db"}
	header := exportHeaderFor(cfg)
	if header.DB != "/tmp/memory.db" {
		t.Errorf("header.DB = %q, want %q", header.DB, "/tmp/memory.db")
	}
}

// TestRunExport_OutputFile проверяет флаг -o: вывод пишется в файл, а не в out.
func TestRunExport_OutputFile(t *testing.T) {
	dbPath, st := newExportTestStore(t)
	ctx := context.Background()
	if err := st.Put(ctx, memory.Memory{Id: ulid.Make().String(), Kind: memory.KindSemantic, Scope: "project:agentmemory", Content: "x", Importance: 3, CreatedAt: 1000, UpdatedAt: 1000}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	outFile := filepath.Join(t.TempDir(), "backup.jsonl")
	var out bytes.Buffer
	if err := runExport([]string{"-db", dbPath, "-o", outFile}, &out); err != nil {
		t.Fatalf("runExport: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want пусто (вывод должен уйти в -o файл)", out.String())
	}
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("чтение -o файла: %v", err)
	}
	if !strings.Contains(string(data), exportFormat) {
		t.Errorf("файл -o не содержит заголовок формата: %s", data)
	}
}
