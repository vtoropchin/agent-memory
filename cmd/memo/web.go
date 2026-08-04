// Подкоманда `memo web` — HTTP-морда просмотра памяти (DESIGN.md §12,
// Этап 6, задача 6.2; семантический поиск — задача 10.1; правка метаданных —
// задача 10.2). Открывает тот же общий файл БД, что и MCP-сервер (WAL +
// busy_timeout безопасны для параллельных читателей), и отдаёт JSON API
// поверх read-методов Store (List/Stats/Get/Neighbors/SearchLexical) плюс
// статический фронтенд (сам фронтенд — задача 6.3/10.1/10.2). Чтение
// по-прежнему доминирует; write-операций две, обе тонкие обёртки над
// доменным `write.Writer` (та же семантика, что у одноимённых MCP-
// инструментов): мягкое удаление (`DELETE /api/memories/{id}` → `Writer.Forget`,
// тумстоун) и правка сигналов ранжирования/группировки (`PATCH
// /api/memories/{id}` → `Writer.Update` с `UpdateInput{Importance,Tags}`,
// задача 10.2). Content/kind/scope/links из браузера НЕ редактируются —
// содержание фактов меняет только агент через MCP (`memory_update`/
// `memory_save` с supersede); морда правит исключительно importance и tags.
// Создания новых записей из браузера нет, hard DELETE не делается.
//
// `/api/search` (задача 10.1) поднимает поверх Store ещё и `recall.Recaller` с
// реальным Embedder (та же фабрика `newEmbedder`, что у MCP-сервера/CLI) — это
// единственное место, где морда вызывает внешний сервис (ollama/OpenAI), и
// только для эмбеддинга ПОИСКОВОГО ЗАПРОСА, не записей: суженный инвариант
// оффлайновости (DESIGN.md §12) — морда не пишет в БД и не эмбеддит записи,
// эмбеддинг запроса допустим с полной деградацией до FTS5 при недоступности
// Embedder. Reasoner мордe не нужен (нет суммаризации в JSON API) — Recaller
// строится с reason.NewNoop().
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
// runInit). Reasoner/Embedder не создаются — морда только читает store
// лексически (без ollama/DeepSeek).
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
		fmt.Fprintln(stdout, "поднимает HTTP-морду для просмотра и поиска памяти в браузере (read-only).")
		fmt.Fprintln(stdout, "Использование: memo web [флаги]")
		flagSet.PrintDefaults()
	}
	addr := flagSet.String("addr", defaultWebAddr, "адрес прослушивания HTTP")
	configPath := flagSet.String("config", "", "путь к TOML-конфигу (опционально)")
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

	w := write.New(s, memembed.NewNoop())
	rc := recall.New(s, cfg.Recall, reason.NewNoop(), embedder)

	srv := &http.Server{
		Addr:    *addr,
		Handler: newWebHandler(s, w, rc, embedder),
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

// newWebHandler собирает мультиплексор web-морды: JSON API под /api/ (см.
// каждый handler) + статика (embed.FS) на /. API-роуты регистрируются до
// файлового хендлера, чтобы /api/* не перехватывался статикой. w используется
// для обеих write-операций (DELETE — мягкое удаление, PATCH — правка
// importance/tags, задача 10.2); rc/embedder — только для /api/search в
// режиме semantic (задача 10.1) — остальные роуты целиком read-only поверх s.
func newWebHandler(s store.Store, w *write.Writer, rc *recall.Recaller, embedder memembed.Embedder) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/stats", handleStats(s))
	mux.HandleFunc("GET /api/memories", handleListMemories(s))
	mux.HandleFunc("GET /api/memories/{id}", handleGetMemory(s))
	mux.HandleFunc("PATCH /api/memories/{id}", handlePatchMemory(s, w))
	mux.HandleFunc("DELETE /api/memories/{id}", handleForgetMemory(s, w))
	mux.HandleFunc("GET /api/search", handleSearch(s, rc, embedder))
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
	// Status — состояние задачи (kind=task, задача 14.1): пусто у записей
	// прочих типов. Read-only, как и ExpiresAt: смена статуса — работа агента
	// через MCP, web-морда остаётся чтением + курированием метаданных
	// (importance/tags, DESIGN.md §12).
	Status string    `json:"status"`
	Tags   []string  `json:"tags"`
	Links  []linkDTO `json:"links"`
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

// memoryCardResponse — тело ответа GET /api/memories/{id}.
type memoryCardResponse struct {
	Memory            memoryDTO             `json:"memory"`
	Neighbors         []memoryDTO           `json:"neighbors"`
	Embedding         embeddingStatusDTO    `json:"embedding"`
	SemanticNeighbors []semanticNeighborDTO `json:"semanticNeighbors"`
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

		writeJSON(w, http.StatusOK, memoryCardResponse{
			Memory:    toDTO(m),
			Neighbors: toDTOs(neighbors),
			Embedding: embeddingStatusDTO{
				Embedded: vecStatus.Embedded,
				Model:    vecStatus.Model,
				Dim:      vecStatus.Dim,
			},
			SemanticNeighbors: semanticNeighbors,
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
// Content/kind/scope/links сюда не входят: DisallowUnknownFields (см.
// handlePatchMemory) превращает присутствие лишнего поля в 400 — защита
// контракта от тихого расширения (правку контента морда не делает никогда).
type patchMemoryRequest struct {
	Importance *int      `json:"importance"`
	Tags       *[]string `json:"tags"`
}

// patchMemoryAllowedFields — перечень допустимых полей тела PATCH
// /api/memories/{id} для текста ошибки 400 на лишнее поле.
const patchMemoryAllowedFields = "importance, tags"

// handlePatchMemory — правка importance/tags живой записи (задача 10.2) через
// доменный write.Writer.Update (write.UpdateInput{Importance, Tags}): кламп
// importance в 1..5, updated_at, синхронизация FTS5 (триггеры tags_ai/tags_ad
// на таблице tags, см. sqlite.go) — вся эта логика уже в домене, здесь только
// разбор входа и HTTP-ответ, ничего не дублируется. accessed_at/access_count
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

		in := write.UpdateInput{Id: id, Importance: body.Importance}
		if body.Tags != nil {
			in.Tags = *body.Tags
		}
		if err := w.Update(r.Context(), in); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeErr(rw, http.StatusNotFound, fmt.Errorf("запись %q не найдена", id))
				return
			}
			writeErr(rw, http.StatusInternalServerError, fmt.Errorf("update %q: %w", id, err))
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
