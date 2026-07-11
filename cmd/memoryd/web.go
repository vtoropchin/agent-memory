// Подкоманда `memoryd web` — read-only HTTP-морда просмотра памяти (DESIGN.md
// §12, Этап 6, задача 6.2). Открывает тот же общий файл БД, что и MCP-сервер
// (WAL + busy_timeout безопасны для параллельных читателей), и отдаёт JSON API
// поверх read-методов Store (List/Stats/Get/Neighbors/SearchLexical) плюс
// статический фронтенд (сам фронтенд — задача 6.3). Никаких write-операций:
// правок памяти из браузера в этой версии нет.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"agentmemory/internal/config"
	"agentmemory/internal/memory"
	"agentmemory/internal/memory/store"
)

// defaultWebAddr — адрес прослушивания `memoryd web` по умолчанию: только
// localhost, т.к. морда показывает всю память и наружу по умолчанию не
// выставляется.
const defaultWebAddr = "127.0.0.1:7070"

// browseLimitCap — потолок limit для /api/memories и /api/search: защита от
// случайного запроса всей БД одним запросом.
const browseLimitCap = 200

// shutdownTimeout — сколько ждём graceful shutdown http.Server после сигнала.
const shutdownTimeout = 5 * time.Second

//go:embed web
var webFS embed.FS

// runWeb разбирает флаги подкоманды `web` и поднимает HTTP-сервер поверх
// Store. Вынесена из диспетчера в main.go для тестируемости (по образцу
// runInit). .env/reasoner/embedder не создаются — морда только читает store
// лексически (без ollama/DeepSeek).
func runWeb(args []string, stdout io.Writer) error {
	flagSet := flag.NewFlagSet("web", flag.ExitOnError)
	addr := flagSet.String("addr", defaultWebAddr, "адрес прослушивания HTTP")
	configPath := flagSet.String("config", "", "путь к TOML-конфигу (опционально)")
	dbPath := flagSet.String("db", "", "путь к файлу SQLite, переопределяет store.path из конфига (опционально)")
	_ = flagSet.Parse(args) // flag.ExitOnError сам завершает процесс при ошибке разбора

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
			log.Printf("memoryd web: закрытие хранилища: %v", err)
		}
	}()

	srv := &http.Server{
		Addr:    *addr,
		Handler: newWebHandler(s),
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	fmt.Fprintf(stdout, "слушаю http://%s (Ctrl+C — стоп)\n", *addr)

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("работа HTTP-сервера: %w", err)
		}
		return nil
	case <-ctx.Done():
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("остановка HTTP-сервера: %w", err)
	}
	return nil
}

// newWebHandler собирает мультиплексор web-морды: JSON API под /api/ (см.
// каждый handler) + статика (embed.FS) на /. API-роуты регистрируются до
// файлового хендлера, чтобы /api/* не перехватывался статикой.
func newWebHandler(s store.Store) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/stats", handleStats(s))
	mux.HandleFunc("GET /api/memories", handleListMemories(s))
	mux.HandleFunc("GET /api/memories/{id}", handleGetMemory(s))
	mux.HandleFunc("GET /api/search", handleSearch(s))
	// Catch-all для неизвестных /api/*-путей: без него они проваливаются в
	// файловый хендлер на "/" и отдают plain-text 404 вместо JSON-ошибки.
	// Менее специфичен, чем method-aware паттерны выше, поэтому известные
	// роуты им не перехватываются (ServeMux матчит самый специфичный паттерн).
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, fmt.Errorf("неизвестный API-путь: %s", r.URL.Path))
	})

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		// webFS собирается из //go:embed web в этом же пакете — подкаталог
		// "web" гарантированно существует на момент сборки, иначе сборка сама
		// не прошла бы; паника здесь недостижима, но fs.Sub возвращает error.
		panic(fmt.Sprintf("memoryd web: подкаталог web не найден в embed.FS: %v", err))
	}
	mux.Handle("/", http.FileServerFS(sub))

	return mux
}

// --- DTO ---

// memoryDTO — представление memory.Memory для JSON API web-морды. Не отдаём
// memory.Memory напрямую: явные camelCase JSON-теги (потребляет фронтенд 6.3)
// и нормализация nullable-полей (*string/*int64 сериализуются в null, когда
// не заданы).
type memoryDTO struct {
	Id           string    `json:"id"`
	Kind         string    `json:"kind"`
	Scope        string    `json:"scope"`
	Content      string    `json:"content"`
	Summary      *string   `json:"summary"`
	Importance   int       `json:"importance"`
	CreatedAt    int64     `json:"createdAt"`
	UpdatedAt    int64     `json:"updatedAt"`
	AccessedAt   *int64    `json:"accessedAt"`
	AccessCount  int       `json:"accessCount"`
	SupersededBy *string   `json:"supersededBy"`
	Source       *string   `json:"source"`
	Tags         []string  `json:"tags"`
	Links        []linkDTO `json:"links"`
}

// linkDTO — представление memory.Link ({dst, rel}) для JSON API.
type linkDTO struct {
	Dst string `json:"dst"`
	Rel string `json:"rel"`
}

// searchItemDTO — элемент выдачи /api/search: memoryDTO + оценка релевантности
// и источник кандидата (сейчас всегда "lexical" — гибридный/векторный поиск
// вне этой версии, см. handleSearch).
type searchItemDTO struct {
	memoryDTO
	Score  float64 `json:"score"`
	Source string  `json:"source"`
}

// toDTOs конвертирует срез memory.Memory в срез memoryDTO (список записей и
// neighbors собираются одинаково).
func toDTOs(ms []memory.Memory) []memoryDTO {
	dtos := make([]memoryDTO, len(ms))
	for i, m := range ms {
		dtos[i] = toDTO(m)
	}
	return dtos
}

// toDTO конвертирует доменную memory.Memory в memoryDTO. Tags/Links при nil
// заменяются на пустой слайс — фронтенд ждёт массив, а не null.
func toDTO(m memory.Memory) memoryDTO {
	tags := m.Tags
	if tags == nil {
		tags = []string{}
	}
	links := make([]linkDTO, len(m.Links))
	for i, l := range m.Links {
		links[i] = linkDTO{Dst: l.Dst, Rel: l.Rel}
	}

	return memoryDTO{
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
		Tags:         tags,
		Links:        links,
	}
}

// --- handlers ---

// statsResponse — тело ответа GET /api/stats.
type statsResponse struct {
	Total      int            `json:"total"`
	Superseded int            `json:"superseded"`
	ByScope    map[string]int `json:"byScope"`
	ByKind     map[string]int `json:"byKind"`
}

func handleStats(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stats, err := s.Stats(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("stats: %w", err))
			return
		}
		writeJSON(w, http.StatusOK, statsResponse{
			Total:      stats.Total,
			Superseded: stats.Superseded,
			ByScope:    stats.ByScope,
			ByKind:     stats.ByKind,
		})
	}
}

// memoriesResponse — тело ответа GET /api/memories.
type memoriesResponse struct {
	Items  []memoryDTO `json:"items"`
	Total  int         `json:"total"`
	Limit  int         `json:"limit"`
	Offset int         `json:"offset"`
}

func handleListMemories(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		limit, err := parseLimit(q.Get("limit"), listDefaultPageSize, browseLimitCap)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		offset, err := parseNonNegativeInt(q.Get("offset"), 0)
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("параметр offset: %w", err))
			return
		}

		f := store.BrowseFilter{
			Scopes:            nonEmpty(q["scope"]),
			Kinds:             nonEmpty(q["kind"]),
			IncludeSuperseded: q.Get("superseded") == "1",
			Limit:             limit,
			Offset:            offset,
		}

		items, total, err := s.List(r.Context(), f)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("list: %w", err))
			return
		}

		writeJSON(w, http.StatusOK, memoriesResponse{
			Items:  toDTOs(items),
			Total:  total,
			Limit:  limit,
			Offset: offset,
		})
	}
}

// memoryCardResponse — тело ответа GET /api/memories/{id}.
type memoryCardResponse struct {
	Memory    memoryDTO   `json:"memory"`
	Neighbors []memoryDTO `json:"neighbors"`
}

func handleGetMemory(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")

		m, err := s.Get(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeErr(w, http.StatusNotFound, fmt.Errorf("запись %q не найдена", id))
				return
			}
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("get %q: %w", id, err))
			return
		}

		neighbors, err := s.Neighbors(r.Context(), id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("neighbors %q: %w", id, err))
			return
		}

		writeJSON(w, http.StatusOK, memoryCardResponse{
			Memory:    toDTO(m),
			Neighbors: toDTOs(neighbors),
		})
	}
}

// searchResponse — тело ответа GET /api/search.
type searchResponse struct {
	Items []searchItemDTO `json:"items"`
}

// handleSearch — лексический поиск (FTS5) поверх Store.SearchLexical.
// Гибридный/векторный поиск (RRF-слияние lexical+vector) — вне этой версии:
// он требует Embedder (ollama), который web-морда намеренно не поднимает
// (только читает store лексически, DESIGN.md §12). Возможное расширение
// будущей задачи — поднимать embedder так же, как это делает run() в main.go.
func handleSearch(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		text := strings.TrimSpace(q.Get("q"))
		if text == "" {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("параметр q обязателен"))
			return
		}

		limit, err := parseLimit(q.Get("limit"), listDefaultPageSize, browseLimitCap)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}

		candidates, err := s.SearchLexical(r.Context(), memory.Query{
			Text:   text,
			Scopes: nonEmpty(q["scope"]),
			Kinds:  nonEmpty(q["kind"]),
			Limit:  limit,
		})
		if err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("search: %w", err))
			return
		}

		items := make([]searchItemDTO, len(candidates))
		for i, c := range candidates {
			items[i] = searchItemDTO{
				memoryDTO: toDTO(c.Memory),
				Score:     c.Score,
				Source:    c.Source,
			}
		}

		writeJSON(w, http.StatusOK, searchResponse{Items: items})
	}
}

// --- query-параметры ---

// nonEmpty отбрасывает пустые строки из повторяемого query-параметра:
// present-but-empty (?scope=) не должен превращаться в фильтр scope IN (”),
// который обнулил бы выдачу — это режим «фильтр не задан». Возвращает nil,
// когда все значения пусты (или vals пуст), чтобы фильтр не применялся.
func nonEmpty(vals []string) []string {
	out := vals[:0:0]
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	return out
}

// listDefaultPageSize — дефолт limit для /api/memories и /api/search, когда
// параметр не задан.
const listDefaultPageSize = 50

// parseLimit разбирает query-параметр limit: пустая строка → def, иначе
// целое положительное число, зажатое сверху потолком maxLimit. Отрицательное
// или нечисловое значение — ошибка (400).
func parseLimit(raw string, def, maxLimit int) (int, error) {
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("параметр limit: не число %q", raw)
	}
	if n <= 0 {
		return 0, fmt.Errorf("параметр limit: должен быть положительным, получено %d", n)
	}
	if n > maxLimit {
		n = maxLimit
	}
	return n, nil
}

// parseNonNegativeInt разбирает query-параметр в неотрицательное целое.
// Пустая строка → def. Отрицательное значение зажимается в 0 (по спецификации
// задачи 6.2), нечисловое — ошибка (400).
func parseNonNegativeInt(raw string, def int) (int, error) {
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("не число %q", raw)
	}
	if n < 0 {
		n = 0
	}
	return n, nil
}

// --- JSON-хелперы ---

// errResponse — тело ошибки JSON API: {"error":"..."}
type errResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("memoryd web: кодирование JSON-ответа: %v", err)
	}
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, errResponse{Error: err.Error()})
}
