// Подкоманда `memo web` — HTTP-морда просмотра памяти (DESIGN.md §12,
// Этап 6, задача 6.2; семантический поиск — задача 10.1; правка метаданных —
// задача 10.2). Открывает тот же общий файл БД, что и MCP-сервер (WAL +
// busy_timeout безопасны для параллельных читателей), и отдаёт JSON API
// поверх read-методов Store (List/Stats/Get/Neighbors/SearchLexical) плюс
// статический фронтенд (Этап 16 — новая морда по принятому дизайну, задачи
// 16.2+). Чтение по-прежнему доминирует; write-операций две, обе тонкие обёртки
// над доменным `write.Writer` (та же семантика, что у одноимённых MCP-
// инструментов): мягкое удаление (`DELETE /api/memories/{id}` → `Writer.Forget`,
// тумстоун) и правка записи (`PATCH /api/memories/{id}` → `Writer.Update`,
// whitelist-DTO 1:1 в `write.UpdateInput`): importance/tags (задача 10.2),
// рабочие метаданные задачи (Этап 15) и — с Этапа 16 — content/summary.
// Инвариант «содержание из браузера не редактируется» снят (DESIGN.md §14
// Этап 16); остаются: kind/scope не меняются, записи не создаются, hard DELETE
// не делается.
//
// Embedder (та же фабрика `newEmbedder`, что у MCP-сервера/CLI) морда вызывает
// в двух местах: `/api/search?mode=semantic` эмбеддит ПОИСКОВЫЙ ЗАПРОС (задача
// 10.1), а `Writer.Update` при правке content — саму запись (иначе в БД остался
// бы устаревший вектор, не видимый очереди досчёта). Инвариант оффлайновости
// (DESIGN.md §12): морда пишет в БД только через доменный `write.Writer` и
// эмбеддит ровно то же, что MCP `memory_update`; недоступный Embedder — полная
// деградация до FTS5, а не ошибка. Reasoner мордe не нужен (нет суммаризации в
// JSON API) — Recaller строится с reason.NewNoop(); фабрика `newReasoner`
// зовётся только чтобы сообщить его доступность в `/api/stats`.
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
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"agentmemory/internal/config"
	"agentmemory/internal/memory"
	"agentmemory/internal/memory/board"
	memembed "agentmemory/internal/memory/embed"
	"agentmemory/internal/memory/reason"
	"agentmemory/internal/memory/recall"
	"agentmemory/internal/memory/store"
	"agentmemory/internal/memory/write"
)

// defaultWebAddr — адрес прослушивания `memo web` по умолчанию: только
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
// runInit). Embedder создаётся той же фабрикой, что у MCP-сервера (поиск по
// смыслу и переэмбеддинг при правке текста); Reasoner — только чтобы узнать
// его доступность для панели «Сервисы» (см. комментарий пакета).
//
// .env при этом грузится наравне с остальными подкомандами (задача 12: до
// появления PostgreSQL-бэкенда морде секреты были не нужны вовсе — .env
// хранил только ключи LLM-провайдеров, которыми она не пользуется, поэтому
// загрузку сознательно пропускали. С driver="postgres" в .env лежит
// MEMO_POSTGRES_PASSWORD — секрет САМОГО хранилища, без которого не открыть
// store, а значит пропуск загрузки ронял `memo web` на аутентификации там,
// где все прочие команды работали).
func runWeb(args []string, stdout io.Writer) error {
	flagSet := flag.NewFlagSet("web", flag.ExitOnError)
	flagSet.SetOutput(stdout)
	flagSet.Usage = func() {
		fmt.Fprintln(stdout, "поднимает HTTP-морду памяти в браузере: обзор, поиск, правка записей, доска задач.")
		fmt.Fprintln(stdout, "Использование: memo web [флаги]")
		flagSet.PrintDefaults()
	}
	addr := flagSet.String("addr", defaultWebAddr, "адрес прослушивания HTTP")
	configPath := flagSet.String("config", "", "путь к TOML-конфигу или каталогу проекта с agentmemory.toml (опционально; если задан — обязан существовать)")
	dbPath := flagSet.String("db", "", "путь к файлу SQLite, переопределяет store.path из конфига (опционально)")
	_ = flagSet.Parse(args) // flag.ExitOnError сам завершает процесс при ошибке разбора

	loadDotEnvForConfig(*configPath, "memo web")

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
			log.Printf("memo web: закрытие хранилища: %v", err)
		}
	}()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// embedder — та же фабрика newEmbedder, что у MCP-сервера/CLI (main.go):
	// "" / "noop" в конфиге, недоступная ollama/OpenAI или опечатка в модели — везде
	// откат на Noop (Dim()==0), только неизвестный provider — ошибка старта. Recaller
	// с Noop-эмбеддером ведёт себя как чистый FTS5 (searchVector рано выходит при
	// Dim()<=0) — деградация семантического поиска до лексического бесшовна.
	embedder, err := newEmbedder(ctx, cfg.Embedder)
	if err != nil {
		return fmt.Errorf("создание embedder: %w", err)
	}
	// Reasoner морда не использует — фабрика нужна ради ответа «доступен ли»
	// в /api/stats (нет ключа/модели → Noop → недоступен; опечатка в provider —
	// ошибка старта, как у MCP-сервера).
	reasoner, err := newReasoner(cfg.Reasoner)
	if err != nil {
		return fmt.Errorf("создание reasoner: %w", err)
	}

	// Writer с реальным Embedder: правка content из панели записи (Этап 16)
	// переэмбеддит запись, как memory_update; при Noop — вектор досчитает reindex.
	w := write.New(s, embedder, write.WithAuthor(authorName(ctx, s)))
	rc := recall.New(s, cfg.Recall, reason.NewNoop(), embedder)

	srv := &http.Server{
		Addr:    *addr,
		Handler: newWebHandler(s, w, rc, embedder, newWebInstance(cfg, reasonerAvailable(reasoner))),
	}

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

// webInstance — то, что о запущенном инстансе знает composition root, а не
// Store: домашний scope и провайдеры для панели «Сервисы» обзора (DESIGN.md
// §14 Этап 16). Имена провайдеров и драйвера — из конфига; доступность
// Reasoner — решение фабрики newReasoner (не Noop); доступность Embedder
// здесь не хранится — её отдаёт сам экземпляр (Dim() > 0) в handleStats.
type webInstance struct {
	HomeScope         string // "project:<name>" из [scope]; пусто при выключенной мультипроектности
	StoreDriver       string // sqlite | postgres
	StoreTarget       string // путь к файлу или postgres://host:port/db — без пароля (config.StoreConfig.Target)
	EmbedderProvider  string // noop | ollama | openai
	EmbedderModel     string
	ReasonerProvider  string // noop | deepseek | cliproxy
	ReasonerModel     string
	ReasonerAvailable bool
}

// newWebInstance собирает webInstance из конфига; пустые provider/driver
// показываются своими дефолтами (noop/sqlite), как их трактуют фабрики.
func newWebInstance(cfg config.Config, reasonerAvailable bool) webInstance {
	driver := cfg.Store.Driver
	if driver == "" {
		driver = "sqlite"
	}
	embedderProvider, embedderModel := providerAndModel(cfg.Embedder.Provider, cfg.Embedder.Model)
	reasonerProvider, reasonerModel := providerAndModel(cfg.Reasoner.Provider, cfg.Reasoner.Model)
	return webInstance{
		HomeScope:         cfg.Scope.SaveScope(),
		StoreDriver:       driver,
		StoreTarget:       cfg.Store.Target(),
		EmbedderProvider:  embedderProvider,
		EmbedderModel:     embedderModel,
		ReasonerProvider:  reasonerProvider,
		ReasonerModel:     reasonerModel,
		ReasonerAvailable: reasonerAvailable,
	}
}

// newWebHandler собирает мультиплексор web-морды: JSON API под /api/ (см.
// каждый handler) + статика (embed.FS) на /. API-роуты регистрируются до
// файлового хендлера, чтобы /api/* не перехватывался статикой. w используется
// для всех трёх write-операций (POST — создание задачи, DELETE — мягкое
// удаление, PATCH — правка записи);
// rc — только для /api/search в режиме semantic (задача 10.1); embedder — там
// же и в /api/stats (доступность/dim для панели «Сервисы») — остальные роуты
// целиком read-only поверх s. inst — домашний scope и провайдеры инстанса
// (домашний scope — ещё и дефолт scope у POST /api/memories).
func newWebHandler(s store.Store, w *write.Writer, rc *recall.Recaller, embedder memembed.Embedder, inst webInstance) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/stats", handleStats(s, embedder, inst))
	mux.HandleFunc("GET /api/memories", handleListMemories(s))
	mux.HandleFunc("POST /api/memories", handleCreateMemory(s, w, inst))
	mux.HandleFunc("GET /api/memories/{id}", handleGetMemory(s))
	mux.HandleFunc("PATCH /api/memories/{id}", handlePatchMemory(s, w))
	mux.HandleFunc("DELETE /api/memories/{id}", handleForgetMemory(s, w))
	mux.HandleFunc("GET /api/search", handleSearch(s, rc, embedder))
	mux.HandleFunc("GET /api/board", handleBoard(s))
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
		panic(fmt.Sprintf("memo web: подкаталог web не найден в embed.FS: %v", err))
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
	Id           string  `json:"id"`
	Kind         string  `json:"kind"`
	Scope        string  `json:"scope"`
	Content      string  `json:"content"`
	Summary      *string `json:"summary"`
	Importance   int     `json:"importance"`
	CreatedAt    int64   `json:"createdAt"`
	UpdatedAt    int64   `json:"updatedAt"`
	AccessedAt   *int64  `json:"accessedAt"`
	AccessCount  int     `json:"accessCount"`
	SupersededBy *string `json:"supersededBy"`
	Source       *string `json:"source"`
	// ExpiresAt — явный срок жизни записи (unix ts, задача 9.4): 0 = вечная,
	// без срока (Memory.ExpiresAt). Read-only показ в карточке — правка TTL из
	// web-морды вне этой версии (см. DESIGN.md §12: read + курирование метаданных).
	ExpiresAt int64 `json:"expiresAt"`
	// Status — стадия задачи (kind=task): пусто у записей прочих типов. С
	// Этапа 15 правится PATCH (доска) — рабочие метаданные задачи, не
	// содержание (DESIGN.md §12).
	Status string `json:"status"`
	// Type/Branch/Workflow — тип задачи (epic|story|task), git-ветка и процесс
	// (kind=task, Этап 15): пусты у прочих типов; правятся PATCH.
	Type     string    `json:"type"`
	Branch   string    `json:"branch"`
	Workflow string    `json:"workflow"`
	Tags     []string  `json:"tags"`
	Links    []linkDTO `json:"links"`
}

// linkDTO — представление memory.Link ({dst, rel}) для JSON API.
type linkDTO struct {
	Dst string `json:"dst"`
	Rel string `json:"rel"`
}

// searchItemDTO — элемент выдачи /api/search: memoryDTO + оценка релевантности
// и источник кандидата ("lexical" в mode=lexical; "lexical"/"vector"/
// "lexical+vector" в mode=semantic — задача 10.1, см. handleSearch).
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
		ExpiresAt:    m.ExpiresAt,
		Status:       string(m.Status),
		Type:         string(m.Type),
		Branch:       m.Branch,
		Workflow:     m.Workflow,
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
	HomeScope  string         `json:"homeScope,omitempty"` // домашний scope инстанса ([scope] project)
	Providers  providersDTO   `json:"providers"`           // панель «Сервисы» обзора (Этап 16)
	Jobs       jobsDTO        `json:"jobs"`
}

// providersDTO — состояние сервисов инстанса для панели «Сервисы» и плашки
// деградации до первого поиска (DESIGN.md §14 Этап 16). Доступность
// эмбеддера статическая — на момент старта `memo web` (как флаг vector в
// /api/search); падение ollama посреди работы обнаружится при следующем запуске.
type providersDTO struct {
	Store    storeProviderDTO    `json:"store"`
	Embedder embedderProviderDTO `json:"embedder"`
	Reasoner reasonerProviderDTO `json:"reasoner"`
}

type storeProviderDTO struct {
	Driver string `json:"driver"`
	Target string `json:"target"` // без пароля
	Schema int    `json:"schema"` // версия схемы драйвера (у sqlite и postgres нумерация своя)
}

type embedderProviderDTO struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Dim       int    `json:"dim"`       // 0, когда недоступен (Noop)
	Available bool   `json:"available"` // Dim() > 0 у собранного экземпляра
	Pending   int    `json:"pending"`   // живые записи без вектора — очередь на эмбеддинг
}

type reasonerProviderDTO struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Available bool   `json:"available"`
}

// jobsDTO — фоновые джобы: время последнего завершённого прогона планировщика
// MCP-сервера (Store.LastMaintRun); null — прогонов ещё не было.
type jobsDTO struct {
	LastRunAt *int64 `json:"lastRunAt"`
}

func handleStats(s store.Store, embedder memembed.Embedder, inst webInstance) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stats, err := s.Stats(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("stats: %w", err))
			return
		}
		schema, err := s.SchemaVersion(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("schema version: %w", err))
			return
		}
		lastRun, err := s.LastMaintRun(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("last maint run: %w", err))
			return
		}
		var jobs jobsDTO
		if lastRun > 0 {
			jobs.LastRunAt = &lastRun
		}
		writeJSON(w, http.StatusOK, statsResponse{
			Total:      stats.Total,
			Superseded: stats.Superseded,
			ByScope:    stats.ByScope,
			ByKind:     stats.ByKind,
			HomeScope:  inst.HomeScope,
			Providers: providersDTO{
				Store: storeProviderDTO{Driver: inst.StoreDriver, Target: inst.StoreTarget, Schema: schema},
				Embedder: embedderProviderDTO{
					Provider:  inst.EmbedderProvider,
					Model:     inst.EmbedderModel,
					Dim:       embedder.Dim(),
					Available: embedder.Dim() > 0,
					Pending:   stats.PendingVectors,
				},
				Reasoner: reasonerProviderDTO{Provider: inst.ReasonerProvider, Model: inst.ReasonerModel, Available: inst.ReasonerAvailable},
			},
			Jobs: jobs,
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
		// expiresBefore — unix-время: «Истекает скоро» на обзоре (Этап 16);
		// 0/пусто — без фильтра, порядок и сортировку задаёт BrowseFilter.
		expiresBefore, err := parseNonNegativeInt(q.Get("expiresBefore"), 0)
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("параметр expiresBefore: %w", err))
			return
		}

		f := store.BrowseFilter{
			Scopes:            nonEmpty(q["scope"]),
			Kinds:             nonEmpty(q["kind"]),
			IncludeSuperseded: q.Get("superseded") == "1",
			Limit:             limit,
			Offset:            offset,
			ExpiresBefore:     int64(expiresBefore),
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

// semanticNeighborsCount — сколько семантических соседей запрашивать для
// карточки записи (задача 6.4, N по умолчанию).
const semanticNeighborsCount = 5

// embeddingStatusDTO — состояние векторного слоя записи для карточки
// (задача 6.4): смэпено ли, какой моделью и с какой размерностью.
type embeddingStatusDTO struct {
	Embedded bool   `json:"embedded"`
	Model    string `json:"model"`
	Dim      int    `json:"dim"`
}

// semanticNeighborDTO — элемент semanticNeighbors в ответе карточки: memoryDTO
// + честная cosine-дистанция (не similarity) до записи-якоря.
type semanticNeighborDTO struct {
	memoryDTO
	Distance float64 `json:"distance"`
}

// taskRefDTO — краткая ссылка на задачу в иерархии карточки (родитель/дети,
// задача 15.3): ровно то, что нужно строке списка и переходу на карточку.
type taskRefDTO struct {
	Id      string  `json:"id"`
	Summary *string `json:"summary"`
	Status  string  `json:"status"`
	Type    string  `json:"type"`
}

// progressDTO — прогресс родителя по живым детям (memory.Progress).
type progressDTO struct {
	Done  int `json:"done"`
	Total int `json:"total"`
}

// memoryCardResponse — тело ответа GET /api/memories/{id}. Parent/Children/
// Progress — иерархия задачи (kind=task, DESIGN.md §14 Этап 15, задача 15.3),
// BlockedBy/Blocks — блокировка (задача 15.4): живые блокеры и задачи, которые
// эта запись блокирует. Всё — board.Describe (задача 15.5: одна политика на
// mcp и web), здесь только DTO. У записей прочих kind parent/progress — null,
// массивы — пустые.
type memoryCardResponse struct {
	Memory            memoryDTO             `json:"memory"`
	Neighbors         []memoryDTO           `json:"neighbors"`
	Embedding         embeddingStatusDTO    `json:"embedding"`
	SemanticNeighbors []semanticNeighborDTO `json:"semanticNeighbors"`
	Parent            *taskRefDTO           `json:"parent"`
	Children          []taskRefDTO          `json:"children"`
	Progress          *progressDTO          `json:"progress"`
	BlockedBy         []taskRefDTO          `json:"blockedBy"`
	Blocks            []taskRefDTO          `json:"blocks"`
}

// toTaskRef конвертирует board.Ref в DTO; пустой Summary — null (у задачи
// заголовок обязателен, пусто означает «запись недоступна, известен id»).
func toTaskRef(r board.Ref) taskRefDTO {
	dto := taskRefDTO{Id: r.ID, Status: string(r.Status), Type: string(r.Type)}
	if r.Summary != "" {
		summary := r.Summary
		dto.Summary = &summary
	}
	return dto
}

// toTaskRefs — массив DTO; nil → пустой массив (фронт ждёт [], не null).
func toTaskRefs(refs []board.Ref) []taskRefDTO {
	out := make([]taskRefDTO, len(refs))
	for i, r := range refs {
		out[i] = toTaskRef(r)
	}
	return out
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

		vecStatus, err := s.VectorStatus(r.Context(), id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("vector status %q: %w", id, err))
			return
		}

		semanticCandidates, err := s.SemanticNeighbors(r.Context(), id, semanticNeighborsCount)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("semantic neighbors %q: %w", id, err))
			return
		}

		semanticNeighbors := []semanticNeighborDTO{}
		for _, c := range semanticCandidates {
			semanticNeighbors = append(semanticNeighbors, semanticNeighborDTO{
				memoryDTO: toDTO(c.Memory),
				Distance:  c.Score,
			})
		}

		detail, err := board.Describe(r.Context(), s, m)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("detail %q: %w", id, err))
			return
		}
		var parent *taskRefDTO
		if detail.Parent != nil {
			ref := toTaskRef(*detail.Parent)
			parent = &ref
		}
		var progress *progressDTO
		if detail.Progress != nil {
			progress = &progressDTO{Done: detail.Progress.Done, Total: detail.Progress.Total}
		}

		writeJSON(w, http.StatusOK, memoryCardResponse{
			Memory:    toDTO(m),
			Neighbors: toDTOs(neighbors),
			Embedding: embeddingStatusDTO{
				Embedded: vecStatus.Embedded,
				Model:    vecStatus.Model,
				Dim:      vecStatus.Dim,
			},
			SemanticNeighbors: semanticNeighbors,
			Parent:            parent,
			Children:          toTaskRefs(detail.Children),
			Progress:          progress,
			BlockedBy:         toTaskRefs(detail.BlockedBy),
			Blocks:            toTaskRefs(detail.Blocks),
		})
	}
}

// patchMemoryRequest — тело PATCH /api/memories/{id} (задача 10.2). Указатели
// различают «поле не передано» (nil, не трогаем) от «поле передано» — та же
// техника, что у write.UpdateInput, которое это тело и заполняет один в один.
// Tags — указатель НА СЛАЙС: nil ⇒ теги не трогать, не-nil (в т.ч. пустой []) ⇒
// заменить набор целиком (tags:[] снимает все — заменяющая, не аддитивная
// семантика, как и у write.UpdateInput.Tags). JSON-литерал null для любого
// поля стандартным поведением encoding/json обнуляет указатель — "importance:
// null"/"tags: null" неотличимы от отсутствия поля, что здесь и требуется
// (частичный PATCH не должен трогать поле, которое явно обнулили до null).
// Content/kind/scope/summary сюда не входят: DisallowUnknownFields (см.
// handlePatchMemory) превращает присутствие лишнего поля в 400 — защита
// контракта от тихого расширения (содержание фактов морда не правит никогда,
// DESIGN.md §12). С Этапа 15 (задача 15.5) добавлены рабочие метаданные
// задачи — Status/Type/Branch/Workflow и связи Links/Unlinks — whitelist-DTO
// 1:1 в write.UpdateInput, без собственной логики: применимость к kind,
// перечни значений и правило блокировки проверяет домен, морда только маппит
// его ошибки в HTTP-статусы (см. patchErrorStatus).
type patchMemoryRequest struct {
	Content    *string        `json:"content"` // Этап 16: правка текста из панели записи; пустой → 400 (ErrEmptyContent)
	Summary    *string        `json:"summary"` // Этап 16: пустая строка — снять заголовок (как в memory_update)
	Importance *int           `json:"importance"`
	Tags       *[]string      `json:"tags"`
	Status     *string        `json:"status"`
	Type       *string        `json:"type"`
	Branch     *string        `json:"branch"`
	Workflow   *string        `json:"workflow"`
	Links      []patchLinkDTO `json:"links"`   // добавляются (аддитивно, как в MCP)
	Unlinks    []patchLinkDTO `json:"unlinks"` // снимаются; единственный способ убрать ребро
}

// patchLinkDTO — элемент links/unlinks PATCH: {id, rel}. Rel обязателен и
// проверяется по memory.LinkRels на границе входа (как rel в MCP-схеме);
// дефолта related здесь нет — фронт всегда знает, какое ребро правит.
type patchLinkDTO struct {
	ID  string `json:"id"`
	Rel string `json:"rel"`
}

// patchMemoryAllowedFields — перечень допустимых полей тела PATCH
// /api/memories/{id} для текста ошибки 400 на лишнее поле.
const patchMemoryAllowedFields = "content, summary, importance, tags, status, type, branch, workflow, links, unlinks"

// patchLinks валидирует и переводит links/unlinks PATCH в write.LinkInput:
// пустой id или rel вне memory.LinkRels — ошибка (400 у вызывающего).
func patchLinks(field string, in []patchLinkDTO) ([]write.LinkInput, error) {
	if in == nil {
		return nil, nil
	}
	out := make([]write.LinkInput, 0, len(in))
	for _, l := range in {
		if strings.TrimSpace(l.ID) == "" {
			return nil, fmt.Errorf("%s: id обязателен", field)
		}
		if !slices.Contains(memory.LinkRels, l.Rel) {
			return nil, fmt.Errorf("%s: неизвестный rel %q, допустимые значения: %s", field, l.Rel, strings.Join(memory.LinkRels, ", "))
		}
		out = append(out, write.LinkInput{Dst: l.ID, Rel: l.Rel})
	}
	return out, nil
}

// blockedResponse — тело 409 на write.ErrTaskBlocked: ошибка плюс перечень
// живых блокеров, чтобы фронт показал, кого ждёт карточка.
type blockedResponse struct {
	Error    string       `json:"error"`
	Blockers []taskRefDTO `json:"blockers"`
}

// writePatchError маппит ошибку write.Writer.Update в HTTP (errors.Is, не
// текст): ErrTaskBlocked → 409 с блокерами; ошибки валидации домена (стадия,
// тип, применимость к kind, блокер-не-задача) → 400; ErrNotFound → 404;
// прочее → 500. Никаких доменных проверок здесь нет — только маппинг.
func writePatchError(rw http.ResponseWriter, id string, err error) {
	var blocked *write.TaskBlockedError
	switch {
	case errors.As(err, &blocked):
		refs := make([]taskRefDTO, len(blocked.Blockers))
		for i, b := range blocked.Blockers {
			refs[i] = taskRefDTO{Id: b.Id, Summary: b.Summary, Status: string(b.Status), Type: string(b.Type)}
		}
		writeJSON(rw, http.StatusConflict, blockedResponse{Error: err.Error(), Blockers: refs})
	case errors.Is(err, write.ErrUnknownStatus), errors.Is(err, write.ErrUnknownTaskType),
		errors.Is(err, write.ErrStatusNotApplicable), errors.Is(err, write.ErrTaskFieldNotApplicable),
		errors.Is(err, write.ErrBlockerNotTask), errors.Is(err, write.ErrEmptyContent):
		writeErr(rw, http.StatusBadRequest, err)
	case errors.Is(err, store.ErrNotFound):
		writeErr(rw, http.StatusNotFound, fmt.Errorf("запись %q не найдена", id))
	default:
		writeErr(rw, http.StatusInternalServerError, fmt.Errorf("update %q: %w", id, err))
	}
}

// handlePatchMemory — правка importance/tags живой записи (задача 10.2), с
// Этапа 15 (задача 15.5) — рабочих метаданных задачи (status/type/branch/
// workflow, links/unlinks — переносы карточек доски, блокировка, родитель), а
// с Этапа 16 — content/summary (панель записи, «Изменить»; инвариант §12
// снят) через доменный write.Writer.Update (write.UpdateInput 1:1): кламп
// importance в 1..5, updated_at, синхронизация FTS5 (триггеры tags_ai/tags_ad
// на таблице tags, см. sqlite.go), переэмбеддинг при смене content — вся эта
// логика уже в домене, здесь только разбор входа и HTTP-ответ, ничего не
// дублируется. accessed_at/access_count
// НЕ меняются (Update не вызывает Store.Touch) — правка метаданных не
// сбрасывает decay-часы записи (DESIGN.md §4: accessed_at — сигнал именно
// ОБРАЩЕНИЯ к факту при recall, а не редактирования).
//
// Importance вне 1..5 не отклоняется отдельной проверкой на границе: то же
// ClampImportance внутри Writer.Update зажимает его в диапазон молча —
// решение (кламп, а не 400) выбрано специально СИММЕТРИЧНО поведению
// MCP-инструмента memory_update, который через тот же Writer.Update не падает
// на out-of-range importance, а нормализует его; два входа в один и тот же
// домен не должны расходиться в трактовке границы.
//
// Тумстоун (m.SupersededBy != nil) — 409: архивную запись курировать нечего,
// правка метаданных недоступна. Writer.Update сам такую проверку не делает
// (это решение уровня web-морды, как и в handleForgetMemory) — граница нужна
// явно здесь, а не в домене, т.к. MCP memory_update умышленно НЕ блокируется
// на superseded-записи (агент может, например, поправить теги архивной
// записи без явного анти-паттерна) — 409 специфичен для UX морды.
func handlePatchMemory(s store.Store, w *write.Writer) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if strings.TrimSpace(id) == "" {
			writeErr(rw, http.StatusBadRequest, fmt.Errorf("параметр id обязателен"))
			return
		}

		var body patchMemoryRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			if strings.Contains(err.Error(), "unknown field") {
				writeErr(rw, http.StatusBadRequest, fmt.Errorf("недопустимое поле в теле запроса: %w (допустимые поля: %s)", err, patchMemoryAllowedFields))
				return
			}
			writeErr(rw, http.StatusBadRequest, fmt.Errorf("разбор тела запроса: %w", err))
			return
		}

		m, err := s.Get(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeErr(rw, http.StatusNotFound, fmt.Errorf("запись %q не найдена", id))
				return
			}
			writeErr(rw, http.StatusInternalServerError, fmt.Errorf("get %q: %w", id, err))
			return
		}
		if m.SupersededBy != nil {
			writeErr(rw, http.StatusConflict, fmt.Errorf("запись %q архивирована, правка недоступна", id))
			return
		}

		links, err := patchLinks("links", body.Links)
		if err != nil {
			writeErr(rw, http.StatusBadRequest, err)
			return
		}
		unlinks, err := patchLinks("unlinks", body.Unlinks)
		if err != nil {
			writeErr(rw, http.StatusBadRequest, err)
			return
		}
		in := write.UpdateInput{
			Id:         id,
			Content:    body.Content,
			Summary:    body.Summary,
			Importance: body.Importance,
			Status:     body.Status,
			Type:       body.Type,
			Branch:     body.Branch,
			Workflow:   body.Workflow,
			Links:      links,
			Unlinks:    unlinks,
		}
		if body.Tags != nil {
			in.Tags = *body.Tags
		}
		if err := w.Update(r.Context(), in); err != nil {
			writePatchError(rw, id, err)
			return
		}

		updated, err := s.Get(r.Context(), id)
		if err != nil {
			writeErr(rw, http.StatusInternalServerError, fmt.Errorf("get %q после update: %w", id, err))
			return
		}
		writeJSON(rw, http.StatusOK, toDTO(updated))
	}
}

// createMemoryRequest — тело POST /api/memories: whitelist-DTO 1:1 в
// write.SaveInput, без прохода «как есть» в домен. Kind присутствует явно и
// обязан быть "task": из браузера заводится только задача (DESIGN.md §12 —
// инвариант «морда не создаёт записи» снят ровно для kind=task, остальные
// виды памяти по-прежнему пишет только агент через memory_save). Указателей
// здесь нет — в отличие от PATCH это не частичная правка, а полный набор
// полей новой записи: пустое поле означает «дефолт домена», а не «не трогать».
type createMemoryRequest struct {
	Kind       string         `json:"kind"`
	Scope      string         `json:"scope"`
	Type       string         `json:"type"`
	Summary    string         `json:"summary"`
	Content    string         `json:"content"`
	Status     string         `json:"status"`
	Importance int            `json:"importance"`
	Tags       []string       `json:"tags"`
	Branch     string         `json:"branch"`
	Workflow   string         `json:"workflow"`
	Links      []patchLinkDTO `json:"links"`
}

// createMemoryAllowedFields — перечень допустимых полей тела POST
// /api/memories для текста ошибки 400 на лишнее поле (как у PATCH).
const createMemoryAllowedFields = "kind, scope, type, summary, content, status, importance, tags, branch, workflow, links"

// handleCreateMemory — создание задачи из браузера (Этап 17). Единственная
// write-операция морды, которая заводит новую запись; всё остальное —
// правка и мягкое удаление существующих.
//
// Границы, заданные здесь, а не в домене (домен обязан уметь писать любой
// kind — его зовёт MCP memory_save):
//   - kind != "task" (в т.ч. пустой) → 400. Факты/эпизоды/процедуры/долг
//     создаёт только агент: у морды нет ни контекста для атомарного факта,
//     ни дедупа в UI.
//   - links допускают только rel=part_of: при создании из браузера
//     осмысленна ровно одна связь — «входит в» (задача → история/эпик).
//     blocked_by и прочие рёбра ставятся уже на существующей карточке через
//     PATCH, где виден весь граф.
//
// Пустой scope → домашний scope инстанса (inst.HomeScope), как в MCP
// memory_save: морда открыта в конкретном проекте, и заставлять пользователя
// печатать scope руками незачем. Ошибки домена маппятся тем же
// writePatchError — перечень ошибок Save и Update совпадает (валидация полей
// задачи общая), дублировать маппинг не нужно; store.ErrNotFound от Save
// недостижим, ветка 404 просто не сработает.
//
// Дедуп: Writer.Save для kind=task его не делает (DESIGN.md §7 — задачи не
// сливаются), поэтому merged из Save здесь не интерпретируется.
func handleCreateMemory(s store.Store, w *write.Writer, inst webInstance) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		var body createMemoryRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			if strings.Contains(err.Error(), "unknown field") {
				writeErr(rw, http.StatusBadRequest, fmt.Errorf("недопустимое поле в теле запроса: %w (допустимые поля: %s)", err, createMemoryAllowedFields))
				return
			}
			writeErr(rw, http.StatusBadRequest, fmt.Errorf("разбор тела запроса: %w", err))
			return
		}

		if strings.TrimSpace(body.Kind) != string(memory.KindTask) {
			writeErr(rw, http.StatusBadRequest, fmt.Errorf("из браузера создаётся только kind=%s (получено %q): остальные виды памяти пишет агент через memory_save", memory.KindTask, body.Kind))
			return
		}

		links, err := createLinks(body.Links)
		if err != nil {
			writeErr(rw, http.StatusBadRequest, err)
			return
		}

		scope := strings.TrimSpace(body.Scope)
		if scope == "" {
			scope = inst.HomeScope
		}

		// Пустое описание — не ошибка входа: карточка заводится заголовком
		// (§2.6 макета), иначе ErrEmptyContent не дал бы создать быструю
		// задачу. Подстановка именно здесь, а не в домене: для memory_save
		// пустой content остаётся ошибкой.
		content := strings.TrimSpace(body.Content)
		if content == "" {
			content = strings.TrimSpace(body.Summary)
		}

		id, _, err := w.Save(r.Context(), write.SaveInput{
			Kind:       memory.KindTask,
			Scope:      scope,
			Content:    content,
			Summary:    body.Summary,
			Status:     body.Status,
			Type:       body.Type,
			Branch:     body.Branch,
			Workflow:   body.Workflow,
			Importance: body.Importance,
			Tags:       body.Tags,
			Links:      links,
		})
		if err != nil {
			writeCreateError(rw, err)
			return
		}

		created, err := s.Get(r.Context(), id)
		if err != nil {
			writeErr(rw, http.StatusInternalServerError, fmt.Errorf("get %q после create: %w", id, err))
			return
		}
		writeJSON(rw, http.StatusCreated, toDTO(created))
	}
}

// createLinks переводит links тела POST в write.LinkInput, разрешая
// единственный rel — part_of (см. handleCreateMemory).
func createLinks(in []patchLinkDTO) ([]write.LinkInput, error) {
	if in == nil {
		return nil, nil
	}
	out := make([]write.LinkInput, 0, len(in))
	for _, l := range in {
		if strings.TrimSpace(l.ID) == "" {
			return nil, fmt.Errorf("links: id обязателен")
		}
		if l.Rel != memory.RelPartOf {
			return nil, fmt.Errorf("links: при создании допустим только rel=%s (получено %q)", memory.RelPartOf, l.Rel)
		}
		out = append(out, write.LinkInput{Dst: l.ID, Rel: l.Rel})
	}
	return out, nil
}

// writeCreateError маппит ошибку write.Writer.Save в HTTP: ошибки валидации
// входа и полей задачи → 400, прочее (store) → 500. ErrTaskBlocked здесь
// возможен (Save проверяет пост-состояние), но при создании с rel=part_of
// недостижим — отдельной ветки 409 нет, такая ошибка пойдёт как 400 вместе с
// остальной валидацией домена.
func writeCreateError(rw http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, write.ErrEmptyContent), errors.Is(err, write.ErrEmptyScope),
		errors.Is(err, write.ErrUnknownKind), errors.Is(err, write.ErrInvalidTTL),
		errors.Is(err, write.ErrTaskSummaryRequired), errors.Is(err, write.ErrUnknownStatus),
		errors.Is(err, write.ErrUnknownTaskType), errors.Is(err, write.ErrStatusNotApplicable),
		errors.Is(err, write.ErrTaskFieldNotApplicable), errors.Is(err, write.ErrBlockerNotTask),
		errors.Is(err, write.ErrTaskBlocked):
		writeErr(rw, http.StatusBadRequest, err)
	default:
		writeErr(rw, http.StatusInternalServerError, fmt.Errorf("create: %w", err))
	}
}

// forgetResponse — тело ответа DELETE /api/memories/{id}.
type forgetResponse struct {
	Ok bool   `json:"ok"`
	Id string `json:"id"`
}

// handleForgetMemory — мягкое удаление записи (тумстоун) через доменный
// write.Writer.Forget: та же семантика, что у MCP-инструмента memory_forget
// (Store.Supersede(id, id)), никакого hard DELETE. Пустой replacedBy — запись
// помечена заменённой самой собой и выпадает из recall/списка живых, но
// строка/связи/история остаются в БД. Идемпотентна для уже архивной записи
// (SupersededBy != nil): Forget не вызывается повторно, иначе Supersede(id, id)
// затёр бы указатель на настоящую заменяющую запись self-тумстоуном и стёр бы
// provenance. Между Get и Forget есть окно TOCTOU под конкурентной записью —
// на локальном single-user инструменте худший случай не хуже прежнего
// поведения, усложнять транзакцией ради этого не нужно.
func handleForgetMemory(s store.Store, w *write.Writer) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if strings.TrimSpace(id) == "" {
			writeErr(rw, http.StatusBadRequest, fmt.Errorf("параметр id обязателен"))
			return
		}

		m, err := s.Get(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeErr(rw, http.StatusNotFound, fmt.Errorf("запись %q не найдена", id))
				return
			}
			writeErr(rw, http.StatusInternalServerError, fmt.Errorf("get %q: %w", id, err))
			return
		}

		if m.SupersededBy != nil {
			// Уже архивна — повторное удаление успешно и ничего не разрушает.
			writeJSON(rw, http.StatusOK, forgetResponse{Ok: true, Id: id})
			return
		}

		if err := w.Forget(r.Context(), id, ""); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeErr(rw, http.StatusNotFound, fmt.Errorf("запись %q не найдена", id))
				return
			}
			writeErr(rw, http.StatusInternalServerError, fmt.Errorf("forget %q: %w", id, err))
			return
		}

		writeJSON(rw, http.StatusOK, forgetResponse{Ok: true, Id: id})
	}
}

// searchModeLexical/searchModeSemantic — допустимые значения query-параметра
// mode у /api/search (задача 10.1). semantic — дефолт (гибрид: FTS5 + вектор
// при доступном Embedder, RRF, бизнес-ранжирование, см. recall.Recaller.Candidates);
// lexical — прежнее поведение (Store.SearchLexical напрямую), оставлено для
// сравнения "глазами" и как явный обход векторного слоя.
const (
	searchModeLexical  = "lexical"
	searchModeSemantic = "semantic"
)

// searchResponse — тело ответа GET /api/search. Vector — доступен ли векторный
// слой у этого инстанса морды (Embedder.Dim() > 0): false в mode=semantic
// означает, что запрос молча деградировал до чистого FTS5 (недоступна
// ollama/OpenAI либо embedder.provider=noop в конфиге) — фронт показывает
// плашку деградации. В mode=lexical поле тоже присутствует (честный статус
// слоя), но не влияет на сам запрос — векторный источник в lexical не
// используется независимо от него.
type searchResponse struct {
	Items  []searchItemDTO `json:"items"`
	Vector bool            `json:"vector"`
}

// handleSearch — поиск по памяти в двух режимах (задача 10.1): mode=lexical —
// прежний чистый FTS5 (Store.SearchLexical); mode=semantic (дефолт) — гибрид
// через recall.Recaller.Candidates (тот же путь, что и MCP memory_recall, без
// сборки Markdown-брифа). Деградация встроена в сам Candidates (DESIGN.md §15):
// недоступный/noop Embedder — гибрид молча превращается в чистый FTS5, ошибка
// наружу не идёт; searchResponse.Vector сообщает фронту, произошла ли эта
// деградация, отдельно от самой выдачи.
func handleSearch(s store.Store, rc *recall.Recaller, embedder memembed.Embedder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		text := strings.TrimSpace(q.Get("q"))
		if text == "" {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("параметр q обязателен"))
			return
		}

		mode := q.Get("mode")
		if mode == "" {
			mode = searchModeSemantic
		}
		if mode != searchModeLexical && mode != searchModeSemantic {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("параметр mode: неизвестное значение %q (ожидается %q или %q)", mode, searchModeLexical, searchModeSemantic))
			return
		}

		limit, err := parseLimit(q.Get("limit"), listDefaultPageSize, browseLimitCap)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}

		query := memory.Query{
			Text:   text,
			Scopes: nonEmpty(q["scope"]),
			Kinds:  nonEmpty(q["kind"]),
			// IncludeDone (задача 14.1): в read path пустой фильтр состояний
			// означает «всё, кроме выполненных задач» — правило recall-брифа
			// агента, где сделанное не должно вытеснять актуальное. Морда —
			// инструмент обзора: поиск в ней обязан находить и выполненные
			// задачи, иначе они видны в листинге (Store.List статус не
			// фильтрует), но пропадают в поиске по тем же данным.
			IncludeDone: true,
			Limit:       limit,
		}

		var candidates []memory.Candidate
		if mode == searchModeLexical {
			candidates, err = s.SearchLexical(r.Context(), query)
		} else {
			candidates, err = rc.Candidates(r.Context(), query)
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("search: %w", err))
			return
		}
		// Candidates сливает выдачу двух источников (лексика+вектор) до limit каждый
		// — итог может превысить запрошенный limit; режем до него, как и в lexical
		// mode (Store.SearchLexical уже соблюдает limit сам).
		if len(candidates) > limit {
			candidates = candidates[:limit]
		}

		items := make([]searchItemDTO, len(candidates))
		for i, c := range candidates {
			items[i] = searchItemDTO{
				memoryDTO: toDTO(c.Memory),
				Score:     c.Score,
				Source:    c.Source,
			}
		}

		writeJSON(w, http.StatusOK, searchResponse{Items: items, Vector: embedder.Dim() > 0})
	}
}

// --- доска (задача 15.5) ---

// boardCardDTO — карточка доски GET /api/board: поля задачи без content
// (карточка записи целиком — GET /api/memories/{id}) плюс вычисленные
// board.Card свойства. Массивы всегда массивы (не null); parent/progress —
// null, когда их нет.
type boardCardDTO struct {
	Id         string       `json:"id"`
	Summary    *string      `json:"summary"`
	Type       string       `json:"type"`
	Status     string       `json:"status"`
	Importance int          `json:"importance"`
	Tags       []string     `json:"tags"`
	Branch     string       `json:"branch"`
	Workflow   string       `json:"workflow"`
	UpdatedAt  int64        `json:"updatedAt"`
	Blocked    bool         `json:"blocked"`
	BlockedBy  []taskRefDTO `json:"blockedBy"`
	Parent     *taskRefDTO  `json:"parent"`
	Progress   *progressDTO `json:"progress"`
}

// boardColumnDTO — колонка доски: стадия, общее число карточек под фильтр
// (у done — до усечения doneLimit) и сами карточки.
type boardColumnDTO struct {
	Status string         `json:"status"`
	Total  int            `json:"total"`
	Cards  []boardCardDTO `json:"cards"`
}

// boardResponse — тело GET /api/board: колонки в порядке memory.Statuses.
type boardResponse struct {
	Columns []boardColumnDTO `json:"columns"`
}

func toBoardCard(c board.Card) boardCardDTO {
	tags := c.Memory.Tags
	if tags == nil {
		tags = []string{}
	}
	dto := boardCardDTO{
		Id:         c.Memory.Id,
		Summary:    c.Memory.Summary,
		Type:       string(c.Memory.Type),
		Status:     string(c.Memory.Status),
		Importance: c.Memory.Importance,
		Tags:       tags,
		Branch:     c.Memory.Branch,
		Workflow:   c.Memory.Workflow,
		UpdatedAt:  c.Memory.UpdatedAt,
		Blocked:    c.Blocked,
		BlockedBy:  toTaskRefs(c.BlockedBy),
	}
	if c.Parent != nil {
		ref := toTaskRef(*c.Parent)
		dto.Parent = &ref
	}
	if c.Progress != nil {
		dto.Progress = &progressDTO{Done: c.Progress.Done, Total: c.Progress.Total}
	}
	return dto
}

// handleBoard — GET /api/board?scope=…(повторяемый)&type=…(повторяемый)&
// parent=…&branch=…&workflow=…&tag=…&doneLimit=N: тонкая доставка
// board.Build (store удовлетворяет board.Source структурно). Неизвестный type
// или невалидный doneLimit → 400; ошибка сборки → 500. Логики доски здесь нет.
func handleBoard(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var types []memory.TaskType
		for _, raw := range nonEmpty(q["type"]) {
			tt := memory.TaskType(raw)
			if !slices.Contains(memory.TaskTypes, tt) {
				writeErr(w, http.StatusBadRequest, fmt.Errorf("параметр type: неизвестное значение %q (ожидается %s)", raw, joinTaskTypes()))
				return
			}
			types = append(types, tt)
		}
		doneLimit, err := parseNonNegativeInt(q.Get("doneLimit"), 0)
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("параметр doneLimit: %w", err))
			return
		}

		b, err := board.Build(r.Context(), s, board.Filter{
			Scopes:    nonEmpty(q["scope"]),
			Types:     types,
			ParentID:  strings.TrimSpace(q.Get("parent")),
			Branch:    strings.TrimSpace(q.Get("branch")),
			Workflow:  strings.TrimSpace(q.Get("workflow")),
			Tag:       strings.TrimSpace(q.Get("tag")),
			DoneLimit: doneLimit,
		})
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}

		columns := make([]boardColumnDTO, len(b.Columns))
		for i, col := range b.Columns {
			cards := make([]boardCardDTO, len(col.Cards))
			for j, c := range col.Cards {
				cards[j] = toBoardCard(c)
			}
			columns[i] = boardColumnDTO{Status: string(col.Status), Total: col.Total, Cards: cards}
		}
		writeJSON(w, http.StatusOK, boardResponse{Columns: columns})
	}
}

// joinTaskTypes — перечень memory.TaskTypes для текста ошибки.
func joinTaskTypes() string {
	out := make([]string, len(memory.TaskTypes))
	for i, tt := range memory.TaskTypes {
		out[i] = string(tt)
	}
	return strings.Join(out, ", ")
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
		log.Printf("memo web: кодирование JSON-ответа: %v", err)
	}
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, errResponse{Error: err.Error()})
}
