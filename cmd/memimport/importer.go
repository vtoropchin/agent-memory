package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"agentmemory/internal/memory"
	"agentmemory/internal/memory/write"
)

// wikiLinkRe находит [[slug]]-ссылки в теле записи (DESIGN.md §14 Этап 5,
// .claude/memory/README.md).
var wikiLinkRe = regexp.MustCompile(`\[\[([\w-]+)\]\]`)

// frontMatter — разобранные поля заголовка файла памяти (между `---`...`---`),
// см. .claude/memory/README.md. Ручной парсер, без сторонних YAML-библиотек —
// формат файлов памяти узкий и целиком описан этим набором полей.
// Поле kind файла (project | decision | reference | ...) сознательно не хранится:
// на Этапе 5 все записи мапятся в единственный доменный Kind — semantic (см.
// parseRecord), поэтому kind из frontmatter при разборе просто игнорируется.
type frontMatter struct {
	Name       string
	Importance int
	Tags       []string
}

// parseFrontMatter разбирает frontmatter файла памяти: `---` на первой значимой
// строке, `key: value` построчно до закрывающего `---`, остаток файла — тело.
// Единственное специальное значение — `tags: [a, b]`, разбираемое в список.
// Неизвестные ключи молча игнорируются (формат файлов памяти неформальный).
func parseFrontMatter(raw string) (frontMatter, string, error) {
	raw = strings.TrimPrefix(raw, "\ufeff") // BOM, если файл им начинается
	lines := strings.Split(raw, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return frontMatter{}, "", fmt.Errorf("memimport: файл не начинается с разделителя frontmatter ---")
	}

	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end == -1 {
		return frontMatter{}, "", fmt.Errorf("memimport: не найден закрывающий разделитель frontmatter ---")
	}

	var fm frontMatter
	for _, line := range lines[1:end] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "name":
			fm.Name = value
		case "importance":
			if n, err := strconv.Atoi(value); err == nil {
				fm.Importance = n
			}
			// нечисловое значение молча игнорируется — importance просто останется 0
			// (дефолт 3 при импорте, как и при отсутствии поля).
		case "tags":
			fm.Tags = parseTagsList(value)
		}
	}

	body := strings.Join(lines[end+1:], "\n")
	return fm, body, nil
}

// parseTagsList разбирает значение поля `tags: [a, b, c]` в список строк.
// Пустой список ([] или отсутствие поля) даёт nil.
func parseTagsList(value string) []string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "[")
	value = strings.TrimSuffix(value, "]")
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	tags := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		tags = append(tags, p)
	}
	return tags
}

// record — один разобранный факт из файла памяти, готовый к передаче в write.Writer.
type record struct {
	Slug       string
	Kind       memory.Kind
	Importance int
	Tags       []string
	Content    string
	LinkSlugs  []string // slugs из [[wiki-ссылок]] тела, порядок первого появления, без повторов
}

// parseRecord превращает содержимое файла памяти в record: `name` становится
// тегом `name:<slug>` (по нему impорт узнаёт запись при повторном прогоне —
// дедуп делает сам write.Writer.Save по content, тег нужен для читаемости и
// будущих запросов), а kind файла (project/decision/reference/...) отображается
// в единственный поддерживаемый на Этапе 5 доменный Kind — semantic: файлы
// памяти проекта являются фактами/решениями, процедур среди них нет (см. задачу
// 5.1). importance нормализуется тем же правилом, что и write.Save (0 → 3,
// clamp 1..5), чтобы то, что печатает -dry-run, совпадало с тем, что реально
// сохранится.
func parseRecord(raw string) (record, error) {
	fm, body, err := parseFrontMatter(raw)
	if err != nil {
		return record{}, err
	}
	if fm.Name == "" {
		return record{}, fmt.Errorf("memimport: frontmatter без поля name")
	}

	nameTag := "name:" + fm.Name
	tags := make([]string, 0, len(fm.Tags)+1)
	seen := make(map[string]bool, len(fm.Tags)+1)
	for _, t := range fm.Tags {
		if seen[t] {
			continue
		}
		seen[t] = true
		tags = append(tags, t)
	}
	if !seen[nameTag] {
		tags = append(tags, nameTag)
	}

	return record{
		Slug:       fm.Name,
		Kind:       memory.KindSemantic,
		// Нормализуем тем же правилом, что и write.Save/Update, чтобы -dry-run
		// печатал ровно то importance, что реально сохранится (Save клампит ещё раз).
		Importance: write.ClampImportance(fm.Importance),
		Tags:       tags,
		Content:    strings.TrimSpace(body),
		LinkSlugs:  extractLinkSlugs(body),
	}, nil
}

// extractLinkSlugs находит все [[slug]]-ссылки в теле записи, в порядке первого
// появления, без повторов.
func extractLinkSlugs(body string) []string {
	matches := wikiLinkRe.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(matches))
	var out []string
	for _, m := range matches {
		slug := m[1]
		if seen[slug] {
			continue
		}
		seen[slug] = true
		out = append(out, slug)
	}
	return out
}

// listMemoryFiles возвращает пути ко всем файлам памяти в dir: `*.md`, кроме
// служебных MEMORY.md (индекс) и README.md (описание формата). Поддиректории
// не рекурсируются — формат .claude/memory/ плоский.
func listMemoryFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("memimport: чтение директории %q: %w", dir, err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if filepath.Ext(name) != ".md" {
			continue
		}
		if name == "MEMORY.md" || name == "README.md" {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}
	return files, nil
}

// Summary — итог одного прогона импорта (см. Run).
type Summary struct {
	Imported         int // новые записи, вставленные Save (merged=false)
	UpdatedWithLinks int // записи, к которым добавлены links вторым проходом
	SkippedDuplicate int // записи, слившиеся с существующим дублем (merged=true)
	Warnings         []string
}

// Run импортирует файлы памяти из dir в scope двумя проходами (DESIGN.md §14
// Этап 5, задача 5.1):
//  1. каждый файл разбирается в record и сохраняется через w.Save — дедуп
//     работает штатно (Save сам решает, вставить новую запись или усилить
//     существующий дубль); slug -> id запоминается для второго прохода;
//  2. [[slug]]-ссылки тела превращаются в links (rel=related) через w.Update;
//     ссылка на slug, отсутствующий среди импортируемых файлов, — предупреждение
//     в Summary.Warnings, сама ссылка пропускается (деградация, а не падение,
//     DESIGN.md §15).
//
// dryRun=true не делает ни одного вызова w (w может быть nil) — Run только
// парсит файлы и печатает в out, что было бы сделано, ничего не записывая в Store.
// Ошибка, прерывающая Run целиком, — только чтение директории dir; ошибки
// отдельных файлов (не читается, битый frontmatter, ошибка Save/Update)
// попадают в Summary.Warnings, и импорт продолжается остальными файлами.
func Run(ctx context.Context, w *write.Writer, dir, scope string, dryRun bool, out io.Writer) (Summary, error) {
	paths, err := listMemoryFiles(dir)
	if err != nil {
		return Summary{}, err
	}

	type parsed struct {
		path string
		rec  record
	}
	records := make([]parsed, 0, len(paths))
	knownSlugs := make(map[string]bool, len(paths))

	var sum Summary
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			sum.Warnings = append(sum.Warnings, fmt.Sprintf("чтение %s: %v", path, err))
			continue
		}
		rec, err := parseRecord(string(raw))
		if err != nil {
			sum.Warnings = append(sum.Warnings, fmt.Sprintf("разбор %s: %v", path, err))
			continue
		}
		records = append(records, parsed{path: path, rec: rec})
		knownSlugs[rec.Slug] = true
	}

	// Первый проход: сохранить все записи, запомнить slug -> id.
	slugToID := make(map[string]string, len(records))
	for _, p := range records {
		if dryRun {
			fmt.Fprintf(out, "would import %s (kind=%s, importance=%d, tags=%v)\n", p.rec.Slug, p.rec.Kind, p.rec.Importance, p.rec.Tags)
			sum.Imported++
			continue
		}
		id, merged, err := w.Save(ctx, write.SaveInput{
			Kind:       p.rec.Kind,
			Scope:      scope,
			Content:    p.rec.Content,
			Importance: p.rec.Importance,
			Tags:       p.rec.Tags,
			Source:     "file-import:" + p.rec.Slug,
		})
		if err != nil {
			sum.Warnings = append(sum.Warnings, fmt.Sprintf("сохранение %s: %v", p.path, err))
			continue
		}
		slugToID[p.rec.Slug] = id
		if merged {
			sum.SkippedDuplicate++
		} else {
			sum.Imported++
		}
	}

	// Второй проход: [[slug]] -> links.
	for _, p := range records {
		if len(p.rec.LinkSlugs) == 0 {
			continue
		}

		if dryRun {
			var targets []string
			for _, slug := range p.rec.LinkSlugs {
				if slug == p.rec.Slug {
					continue // ссылка записи на саму себя — бессмысленное ребро, пропускаем
				}
				if !knownSlugs[slug] {
					sum.Warnings = append(sum.Warnings, fmt.Sprintf("%s: ссылка на неизвестную запись [[%s]] будет пропущена", p.rec.Slug, slug))
					continue
				}
				targets = append(targets, slug)
			}
			if len(targets) > 0 {
				fmt.Fprintf(out, "would link %s -> %v\n", p.rec.Slug, targets)
				sum.UpdatedWithLinks++
			}
			continue
		}

		srcID, ok := slugToID[p.rec.Slug]
		if !ok {
			continue // сама запись не сохранилась (см. warning выше) — линковать нечего
		}

		var targetIDs []string
		for _, slug := range p.rec.LinkSlugs {
			if slug == p.rec.Slug {
				continue // ссылка на свой slug — self-петля, не считаем и не линкуем
			}
			id, ok := slugToID[slug]
			if !ok {
				sum.Warnings = append(sum.Warnings, fmt.Sprintf("%s: ссылка на неизвестную запись [[%s]] пропущена", p.rec.Slug, slug))
				continue
			}
			targetIDs = append(targetIDs, id)
		}
		if len(targetIDs) == 0 {
			continue
		}
		if err := w.Update(ctx, write.UpdateInput{Id: srcID, Links: targetIDs}); err != nil {
			sum.Warnings = append(sum.Warnings, fmt.Sprintf("связывание %s: %v", p.rec.Slug, err))
			continue
		}
		sum.UpdatedWithLinks++
	}

	return sum, nil
}
