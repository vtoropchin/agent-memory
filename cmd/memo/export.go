// Подкоманда `memo export` — полный/частичный экспорт памяти в переносимый
// JSONL-бэкап (DESIGN.md §12, задача 9.2): первая строка — заголовок с
// метаданными формата, далее одна запись памяти на строку (id, kind, scope,
// content, summary, importance, все таймстампы, superseded_by, source,
// expires_at, tags, links). Векторы НЕ экспортируются — производные данные,
// которые досчитает Reindexer при следующем запуске memo (пустая embed_meta
// новой БД → полный reindex, DESIGN.md §9, §11). Тумстоуны (superseded)
// экспортируются как есть — история переносится вместе с фактами. Формат
// стримится построчно (JSONL) — диффабелен и grep'абелен; порядок строк
// детерминирован (по id, задаёт Store.AllForExport).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"agentmemory/internal/config"
	"agentmemory/internal/memory"
)

// exportFormat/exportVersion — идентификатор и текущая версия формата экспорта
// (заголовочная строка JSONL, задача 9.2). `memo import` отказывает на
// незнакомых format/version — см. runImport.
const (
	exportFormat  = "agentmemory-export"
	exportVersion = 1
)

// exportHeader — первая строка файла экспорта: метаданные формата, а не
// запись памяти (отличается набором полей — import.go разбирает первую
// строку отдельно от остальных).
type exportHeader struct {
	Format     string `json:"format"`
	Version    int    `json:"version"`
	ExportedAt int64  `json:"exported_at"`
	DB         string `json:"db"`
}

// exportHeaderFor строит заголовок экспорта для хранилища cfg. DB — короткое
// человекочитаемое описание хранилища через cfg.Target() (путь к файлу для
// sqlite, host:port/dbname для postgres, без пароля) — не cfg.Path напрямую:
// Path заполняется только для sqlite, а при driver=postgres пуст, и поле "db"
// в заголовке JSONL-бэкапа молча становилось бы пустой строкой (та же
// абстракция уже применяется по всему cmd/memo — web.go, migrate.go).
// Вынесена из runExport отдельной функцией, чтобы её можно было проверить
// юнит-тестом без сетевого открытия postgres-хранилища.
func exportHeaderFor(cfg config.StoreConfig) exportHeader {
	return exportHeader{
		Format:     exportFormat,
		Version:    exportVersion,
		ExportedAt: time.Now().Unix(),
		DB:         cfg.Target(),
	}
}

// exportLink — сериализуемая форма memory.Link.
type exportLink struct {
	Dst string `json:"dst"`
	Rel string `json:"rel"`
}

// exportRecord — одна строка тела экспорта: memory.Memory плюс Tags/Links,
// без вектора (DESIGN.md §9 — производные данные, их досчитает Reindexer).
// Порядок полей соответствует примеру формата в задаче 9.2.
type exportRecord struct {
	Id           string  `json:"id"`
	Kind         string  `json:"kind"`
	Scope        string  `json:"scope"`
	Content      string  `json:"content"`
	Summary      *string `json:"summary"`
	Importance   int     `json:"importance"`
	CreatedAt    int64   `json:"created_at"`
	UpdatedAt    int64   `json:"updated_at"`
	AccessedAt   *int64  `json:"accessed_at"`
	AccessCount  int     `json:"access_count"`
	SupersededBy *string `json:"superseded_by"`
	Source       *string `json:"source"`
	// ExpiresAt — явный TTL записи (unix ts, задача 9.4). omitempty: 0 (вечная,
	// подавляющее большинство записей) не засоряет JSONL полем-нулём; отсутствие
	// поля при импорте (recordToMemory, старые бэкапы без TTL включительно)
	// читается как нулевое значение Go — те же 0/вечная, обратная совместимость
	// без инкремента exportVersion (поле аддитивное и опциональное).
	ExpiresAt int64 `json:"expires_at,omitempty"`
	// Status — состояние задачи (kind=task, задача 14.1). omitempty по тем же
	// причинам, что и у ExpiresAt: у записей прочих kind статуса нет, и поле не
	// должно засорять бэкап пустой строкой; отсутствие поля в старых бэкапах
	// читается как пустой статус — аддитивно, exportVersion не меняется.
	Status string `json:"status,omitempty"`
	// Type/Branch/Workflow — тип задачи, git-ветка и процесс (kind=task, Этап
	// 15). Аддитивны и omitempty по тем же причинам, что и Status: у прочих
	// kind пусты, в старых бэкапах отсутствуют и читаются как пустые
	// (recordToMemory); exportVersion не меняется.
	Type     string       `json:"type,omitempty"`
	Branch   string       `json:"branch,omitempty"`
	Workflow string       `json:"workflow,omitempty"`
	Tags     []string     `json:"tags"`
	Links    []exportLink `json:"links"`
}

// memoryToExportRecord переводит доменную Memory в сериализуемую запись.
// Tags/Links всегда непустые срезы (не nil) — экспорт всегда печатает [], а не
// null, для записи без тегов/ссылок: store.loadTags/loadLinks (см. Get,
// AllForExport) точно так же дают nil на пустом наборе, поэтому повторный
// экспорт после импорта нормализуется тем же правилом — необходимое условие
// побайтового совпадения round-trip (задача 9.2).
func memoryToExportRecord(m memory.Memory) exportRecord {
	tags := m.Tags
	if tags == nil {
		tags = []string{}
	}
	links := make([]exportLink, 0, len(m.Links))
	for _, l := range m.Links {
		links = append(links, exportLink{Dst: l.Dst, Rel: l.Rel})
	}
	return exportRecord{
		Id:           m.Id,
		Kind:         string(m.Kind),
		Scope:        m.Scope,
		Content:      m.Content,
		Summary:      m.Summary,
		Importance:   m.Importance,
		CreatedAt:    m.CreatedAt,
		UpdatedAt:    m.UpdatedAt,
		AccessedAt:   m.AccessedAt,
		AccessCount:  m.AccessCount,
		SupersededBy: m.SupersededBy,
		Source:       m.Source,
		ExpiresAt:    m.ExpiresAt,
		Status:       string(m.Status),
		Type:         string(m.Type),
		Branch:       m.Branch,
		Workflow:     m.Workflow,
		Tags:         tags,
		Links:        links,
	}
}

// runExport разбирает флаги подкоманды `export` и пишет JSONL-бэкап в out (или
// в файл -o). Вынесена из диспетчера для тестируемости (по образцу остальных
// подкоманд).
func runExport(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fmt.Fprintln(out, "полный или частичный экспорт памяти в переносимый JSONL-бэкап (без векторов).")
		fmt.Fprintln(out, "Использование: memo export [флаги]")
		fs.PrintDefaults()
	}
	configPath := fs.String("config", "", "путь к TOML-конфигу или каталогу проекта с agentmemory.toml (опционально; если задан — обязан существовать)")
	dbPath := fs.String("db", "", "путь к файлу SQLite, переопределяет store.path из конфига (опционально)")
	oFlag := fs.String("o", "", "путь к выходному файлу (пусто — stdout)")
	scopesFlag := fs.String("scopes", "", "пространства через запятую для частичного экспорта (пусто — вся БД)")
	_ = fs.Parse(args) // flag.ExitOnError сам завершает процесс при ошибке разбора/-h

	// .env — наравне с остальными подкомандами: при driver="postgres" там лежит
	// MEMO_POSTGRES_PASSWORD, без которого не открыть хранилище (см. коммент к
	// runWeb о том, почему раньше загрузку здесь пропускали).
	loadDotEnvForConfig(*configPath, "memo export")

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("загрузка конфига: %w", err)
	}
	if *dbPath != "" {
		cfg.Store.Path = *dbPath
	}

	s, err := openStore(cfg.Store)
	if err != nil {
		return fmt.Errorf("открытие хранилища %q: %w", cfg.Store.Target(), err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			log.Printf("memo export: закрытие хранилища: %v", err)
		}
	}()

	ctx := context.Background()
	records, err := s.AllForExport(ctx, splitCSV(*scopesFlag))
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}

	dest := out
	if *oFlag != "" {
		f, err := os.Create(*oFlag)
		if err != nil {
			return fmt.Errorf("export: создание файла %q: %w", *oFlag, err)
		}
		defer f.Close()
		dest = f
	}

	// SetEscapeHTML(false) — content факта может содержать <, >, & (код,
	// markdown-разметку и т.п.); экранирование под HTML-контекст здесь не нужно
	// и портило бы читаемость/grep'абельность бэкапа.
	enc := json.NewEncoder(dest)
	enc.SetEscapeHTML(false)

	header := exportHeaderFor(cfg.Store)
	if err := enc.Encode(header); err != nil {
		return fmt.Errorf("export: запись заголовка: %w", err)
	}
	for _, m := range records {
		if err := enc.Encode(memoryToExportRecord(m)); err != nil {
			return fmt.Errorf("export: запись строки id=%s: %w", m.Id, err)
		}
	}
	return nil
}
