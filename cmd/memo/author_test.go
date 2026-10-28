package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"agentmemory/internal/memory"
	"agentmemory/internal/memory/store"
)

// putAuthored кладёт в st запись с полями авторства (Этап 19) и запись без них;
// возвращает их id.
func putAuthored(t *testing.T, st store.Store) (authoredID, plainID string) {
	t.Helper()
	ctx := context.Background()
	authoredID, plainID = ulid.Make().String(), ulid.Make().String()
	for _, m := range []memory.Memory{
		{Id: authoredID, Kind: memory.KindSemantic, Scope: "project:agentmemory", Content: "факт с автором",
			Importance: 3, CreatedAt: 1000, UpdatedAt: 1001, Author: "Анна", UpdatedBy: "Борис"},
		{Id: plainID, Kind: memory.KindSemantic, Scope: "project:agentmemory", Content: "факт без автора",
			Importance: 3, CreatedAt: 1002, UpdatedAt: 1002},
	} {
		if err := st.Put(ctx, m); err != nil {
			t.Fatalf("Put %s: %v", m.Id, err)
		}
	}
	return authoredID, plainID
}

// assertAuthor проверяет поля авторства записи id в БД по пути dbPath.
func assertAuthor(t *testing.T, dbPath, id, author, updatedBy string) {
	t.Helper()
	s, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()
	got, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get %s: %v", id, err)
	}
	if got.Author != author || got.UpdatedBy != updatedBy {
		t.Errorf("%s: author=%q updated_by=%q, want %q/%q", id, got.Author, got.UpdatedBy, author, updatedBy)
	}
}

// TestExportImport_AuthorRoundTrip: export печатает author/updated_by только у
// записей, где они есть (omitempty), import восстанавливает их как есть.
func TestExportImport_AuthorRoundTrip(t *testing.T) {
	srcDBPath, src := newExportTestStore(t)
	authoredID, plainID := putAuthored(t, src)

	var exported bytes.Buffer
	if err := runExport([]string{"-db", srcDBPath}, &exported); err != nil {
		t.Fatalf("export: %v", err)
	}
	for _, line := range strings.Split(exported.String(), "\n") {
		if strings.Contains(line, authoredID) && !strings.Contains(line, `"author":"Анна","updated_by":"Борис"`) {
			t.Errorf("у записи с автором должны печататься author/updated_by: %s", line)
		}
		if strings.Contains(line, plainID) && (strings.Contains(line, `"author"`) || strings.Contains(line, `"updated_by"`)) {
			t.Errorf("у записи без автора полей быть не должно (omitempty): %s", line)
		}
	}

	dir := t.TempDir()
	exportPath := filepath.Join(dir, "export.jsonl")
	if err := os.WriteFile(exportPath, exported.Bytes(), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	dstDBPath := filepath.Join(dir, "dst.db")
	var out bytes.Buffer
	if err := runImport([]string{"-db", dstDBPath, exportPath}, &out); err != nil {
		t.Fatalf("import: %v", err)
	}
	assertAuthor(t, dstDBPath, authoredID, "Анна", "Борис")
	assertAuthor(t, dstDBPath, plainID, "", "")
}

// TestRunImport_LegacyFileWithoutAuthor: бэкап до Этапа 19 (строка записи без
// author/updated_by) импортируется, поля пусты.
func TestRunImport_LegacyFileWithoutAuthor(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "memory.db")
	id := ulid.Make().String()
	legacy := `{"id":"` + id + `","kind":"semantic","scope":"project:agentmemory","content":"факт из старого бэкапа","summary":null,"importance":3,"created_at":1000,"updated_at":1000,"accessed_at":null,"access_count":0,"superseded_by":null,"source":null,"tags":[],"links":[]}`
	path := writeExportFileRaw(t, dir, []string{headerLine(t), legacy})

	var out bytes.Buffer
	if err := runImport([]string{"-db", dbPath, path}, &out); err != nil {
		t.Fatalf("import: %v", err)
	}
	assertAuthor(t, dbPath, id, "", "")
}

// TestRunMigrate_CarriesAuthor: перенос sqlite->sqlite сохраняет поля
// авторства.
func TestRunMigrate_CarriesAuthor(t *testing.T) {
	srcPath, src := newExportTestStore(t)
	authoredID, plainID := putAuthored(t, src)
	src.Close()

	dstPath := filepath.Join(t.TempDir(), "dest.db")
	var out bytes.Buffer
	if err := runMigrate([]string{"sqlite:" + srcPath, "sqlite:" + dstPath}, &out); err != nil {
		t.Fatalf("runMigrate: %v\nвывод:\n%s", err, out.String())
	}
	assertAuthor(t, dstPath, authoredID, "Анна", "Борис")
	assertAuthor(t, dstPath, plainID, "", "")
}

// TestHandleGetMemory_Author: карточка записи в морде несёт author/updatedBy
// (панель записи и карточка задачи, задача 19.9); у записи без авторства —
// пустые строки, фронт тогда строк не показывает.
func TestHandleGetMemory_Author(t *testing.T) {
	s := newTestWebStore(t)
	authoredID, plainID := putAuthored(t, s)
	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	for _, tc := range []struct{ id, author, updatedBy string }{
		{authoredID, "Анна", "Борис"},
		{plainID, "", ""},
	} {
		var card struct {
			Memory map[string]any `json:"memory"`
		}
		if resp := getJSON(t, srv.URL+"/api/memories/"+tc.id, &card); resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: статус %d", tc.id, resp.StatusCode)
		}
		if card.Memory["author"] != tc.author || card.Memory["updatedBy"] != tc.updatedBy {
			t.Errorf("%s: author=%v updatedBy=%v, хочу %q/%q", tc.id, card.Memory["author"], card.Memory["updatedBy"], tc.author, tc.updatedBy)
		}
	}
}
