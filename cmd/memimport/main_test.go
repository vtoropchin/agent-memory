package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentmemory/internal/memory"
	"agentmemory/internal/memory/embed"
	"agentmemory/internal/memory/store"
	"agentmemory/internal/memory/write"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", name, err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestParseFrontMatterWithTagsList(t *testing.T) {
	raw := "---\n" +
		"name: stack-and-storage\n" +
		"kind: decision\n" +
		"importance: 5\n" +
		"tags: [stack, storage, sqlite]\n" +
		"---\n" +
		"\n" +
		"Тело записи.\n" +
		"\n" +
		"Связано: [[embedder-ollama]]\n"

	fm, body, err := parseFrontMatter(raw)
	if err != nil {
		t.Fatalf("parseFrontMatter: %v", err)
	}
	if fm.Name != "stack-and-storage" {
		t.Errorf("Name = %q, want stack-and-storage", fm.Name)
	}
	// Строка `kind: decision` во входе — намеренно: kind не хранится (см. frontMatter),
	// парсер должен молча её проигнорировать, не сломав разбор остальных полей.
	if fm.Importance != 5 {
		t.Errorf("Importance = %d, want 5", fm.Importance)
	}
	want := []string{"stack", "storage", "sqlite"}
	if !equalStrings(fm.Tags, want) {
		t.Errorf("Tags = %v, want %v", fm.Tags, want)
	}
	if !strings.Contains(body, "Тело записи.") {
		t.Errorf("body = %q, не содержит контент", body)
	}
}

func TestParseFrontMatterWithoutTags(t *testing.T) {
	raw := "---\nname: progress\nkind: project\nimportance: 3\n---\n\nПрогресс.\n"

	fm, _, err := parseFrontMatter(raw)
	if err != nil {
		t.Fatalf("parseFrontMatter: %v", err)
	}
	if fm.Tags != nil {
		t.Errorf("Tags = %v, want nil (поле tags отсутствует)", fm.Tags)
	}
	if fm.Name != "progress" {
		t.Errorf("Name = %q, want progress", fm.Name)
	}
}

func TestParseFrontMatterMissingDelimiter(t *testing.T) {
	if _, _, err := parseFrontMatter("просто текст без frontmatter\n"); err == nil {
		t.Error("err = nil, want ошибку про отсутствующий разделитель ---")
	}
}

func TestParseFrontMatterUnclosedDelimiter(t *testing.T) {
	if _, _, err := parseFrontMatter("---\nname: x\n"); err == nil {
		t.Error("err = nil, want ошибку про незакрытый frontmatter")
	}
}

func TestParseRecordMapsNameToTagAndKindToSemantic(t *testing.T) {
	raw := "---\n" +
		"name: my-slug\n" +
		"kind: reference\n" +
		"importance: 0\n" +
		"tags: [foo]\n" +
		"---\n" +
		"\n" +
		"Контент записи с [[other-slug]] и [[my-slug]] (само-ссылка тоже допустима).\n"

	rec, err := parseRecord(raw)
	if err != nil {
		t.Fatalf("parseRecord: %v", err)
	}
	if rec.Slug != "my-slug" {
		t.Errorf("Slug = %q, want my-slug", rec.Slug)
	}
	if rec.Kind != memory.KindSemantic {
		t.Errorf("Kind = %q, want %q (файловый kind reference маппится в semantic)", rec.Kind, memory.KindSemantic)
	}
	if rec.Importance != 3 {
		t.Errorf("Importance = %d, want дефолт 3 (0 в frontmatter)", rec.Importance)
	}
	wantTags := []string{"foo", "name:my-slug"}
	if !equalStrings(rec.Tags, wantTags) {
		t.Errorf("Tags = %v, want %v", rec.Tags, wantTags)
	}
	wantLinks := []string{"other-slug", "my-slug"}
	if !equalStrings(rec.LinkSlugs, wantLinks) {
		t.Errorf("LinkSlugs = %v, want %v", rec.LinkSlugs, wantLinks)
	}
}

func TestParseRecordClampsImportance(t *testing.T) {
	raw := "---\nname: hot\nimportance: 9\n---\n\nконтент\n"
	rec, err := parseRecord(raw)
	if err != nil {
		t.Fatalf("parseRecord: %v", err)
	}
	if rec.Importance != 5 {
		t.Errorf("Importance = %d, want 5 (clamp)", rec.Importance)
	}
}

func TestParseRecordMissingNameIsError(t *testing.T) {
	raw := "---\nkind: project\n---\n\nконтент без имени\n"
	if _, err := parseRecord(raw); err == nil {
		t.Error("err = nil, want ошибку про отсутствующий name")
	}
}

func TestListMemoryFilesSkipsReadmeAndMemoryIndex(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "MEMORY.md", "индекс")
	writeFile(t, dir, "README.md", "readme")
	writeFile(t, dir, "fact-one.md", "---\nname: fact-one\n---\n\nконтент")
	writeFile(t, dir, "not-markdown.txt", "игнорируется")

	files, err := listMemoryFiles(dir)
	if err != nil {
		t.Fatalf("listMemoryFiles: %v", err)
	}
	if len(files) != 1 || filepath.Base(files[0]) != "fact-one.md" {
		t.Errorf("files = %v, want [fact-one.md]", files)
	}
}

func newTestWriter(t *testing.T) *write.Writer {
	t.Helper()
	s, err := store.OpenSQLite(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return write.New(s, embed.NewNoop())
}

func TestRunTwoPassLinking(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.md", "---\nname: a\nimportance: 4\ntags: [x]\n---\n\nЗапись A про первую тему ссылается на [[b]].\n")
	writeFile(t, dir, "b.md", "---\nname: b\n---\n\nЗапись B про вторую тему, без ссылок.\n")
	writeFile(t, dir, "c.md", "---\nname: c\n---\n\nЗапись C про третью тему ссылается на несуществующий [[ghost]].\n")

	w := newTestWriter(t)
	var out strings.Builder
	sum, err := Run(context.Background(), w, dir, "project:test", false, &out)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Imported != 3 {
		t.Errorf("Imported = %d, want 3", sum.Imported)
	}
	if sum.SkippedDuplicate != 0 {
		t.Errorf("SkippedDuplicate = %d, want 0", sum.SkippedDuplicate)
	}
	if sum.UpdatedWithLinks != 1 {
		t.Errorf("UpdatedWithLinks = %d, want 1 (только a.md ссылается на существующий b)", sum.UpdatedWithLinks)
	}
	if len(sum.Warnings) != 1 || !strings.Contains(sum.Warnings[0], "ghost") {
		t.Errorf("Warnings = %v, want ровно одно предупреждение про [[ghost]]", sum.Warnings)
	}
}

func TestRunSkipsSelfLink(t *testing.T) {
	dir := t.TempDir()
	// Запись ссылается на собственный slug — ребро self->self писать не нужно.
	writeFile(t, dir, "solo.md", "---\nname: solo\n---\n\nЗапись про саму себя ссылается на [[solo]].\n")

	s, err := store.OpenSQLite(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	w := write.New(s, embed.NewNoop())

	var out strings.Builder
	sum, err := Run(context.Background(), w, dir, "project:test", false, &out)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.UpdatedWithLinks != 0 {
		t.Errorf("UpdatedWithLinks = %d, want 0 (self-ссылка не создаёт ребро)", sum.UpdatedWithLinks)
	}

	got, err := s.SearchLexical(context.Background(), memory.Query{Text: "сама себя", Scope: "project:test", Limit: 10})
	if err != nil {
		t.Fatalf("SearchLexical: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("запись solo не найдена")
	}
	// SearchLexical не грузит Links (read-hot-path) — подтягиваем полную запись через Get.
	full, err := s.Get(context.Background(), got[0].Memory.Id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(full.Links) != 0 {
		t.Errorf("Links = %v, want пусто (self-ребро не должно записаться)", full.Links)
	}
}

func TestRunDryRunReportsWithoutWriter(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.md", "---\nname: a\n---\n\nЗапись A про dry-run ссылается на [[b]].\n")
	writeFile(t, dir, "b.md", "---\nname: b\n---\n\nЗапись B про dry-run.\n")

	var out strings.Builder
	// w=nil — dry-run не должен вызывать ни одного метода Writer.
	sum, err := Run(context.Background(), nil, dir, "project:test", true, &out)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Imported != 2 {
		t.Errorf("Imported = %d, want 2", sum.Imported)
	}
	if sum.UpdatedWithLinks != 1 {
		t.Errorf("UpdatedWithLinks = %d, want 1", sum.UpdatedWithLinks)
	}
	if !strings.Contains(out.String(), "would import a") || !strings.Contains(out.String(), "would link a -> [b]") {
		t.Errorf("stdout = %q, want отчёт о планируемых import/link", out.String())
	}
}

// TestCLIDryRunDoesNotCreateDB — сквозной прогон через run() (флаги + wiring):
// -dry-run не должен даже открывать файл БД, не то что писать в него.
func TestCLIDryRunDoesNotCreateDB(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.md", "---\nname: a\n---\n\nЗапись A для CLI dry-run.\n")
	dbPath := filepath.Join(t.TempDir(), "sub", "memory.db")

	var out strings.Builder
	args := []string{"-dir", dir, "-db", dbPath, "-scope", "project:test", "-dry-run"}
	if err := run(args, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "imported 1, updated-with-links 0, skipped-as-duplicate 0") {
		t.Errorf("stdout = %q, want итоговую строку imported 1, ...", out.String())
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Errorf("-dry-run создал файл БД %q (err = %v)", dbPath, err)
	}
}

// TestCLIRealRunThenRerunDoesNotDuplicate — сквозной прогон через run(): реальный
// импорт кладёт запись в БД, повторный прогон над тем же файлом не плодит дубль
// (дедуп срабатывает штатно через write.Writer.Save).
func TestCLIRealRunThenRerunDoesNotDuplicate(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.md", "---\nname: a\nimportance: 4\n---\n\nЗапись A про базы данных для CLI.\n")
	dbPath := filepath.Join(t.TempDir(), "memory.db")

	var out1 strings.Builder
	args := []string{"-dir", dir, "-db", dbPath, "-scope", "project:test"}
	if err := run(args, &out1); err != nil {
		t.Fatalf("run (первый прогон): %v", err)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("реальный прогон должен создать файл БД: %v", err)
	}
	if !strings.Contains(out1.String(), "imported 1, updated-with-links 0, skipped-as-duplicate 0") {
		t.Errorf("stdout (1) = %q, want imported 1, ...", out1.String())
	}

	var out2 strings.Builder
	if err := run(args, &out2); err != nil {
		t.Fatalf("run (второй прогон): %v", err)
	}
	if !strings.Contains(out2.String(), "imported 0, updated-with-links 0, skipped-as-duplicate 1") {
		t.Errorf("stdout (2) = %q, want imported 0, ..., skipped-as-duplicate 1 (дедуп)", out2.String())
	}

	s, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("store.Open для проверки: %v", err)
	}
	defer s.Close()
	got, err := s.SearchLexical(context.Background(), memory.Query{Text: "базы данных", Scope: "project:test", Limit: 10})
	if err != nil {
		t.Fatalf("SearchLexical: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("записей в БД после двух прогонов = %d, want 1 (без дублей)", len(got))
	}
}
