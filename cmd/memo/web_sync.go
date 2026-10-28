// web_sync.go — JSON API раздела «Синхронизация» морды (Этап 19, задача 19.8,
// DESIGN.md §12, §13, §7.1). Тонкая доставка: профиль и цели пишутся в
// локальную БД (store.SyncLocal), пароль общей базы — в .env каталога локальной
// БД (saveSyncPassword), прогон — runSync (lease + memsync.Syncer), схема и
// ротация base_id — store.Hub. Правил синхронизации здесь нет; есть только
// валидация ввода на границе.
//
// Пароль write-only: ни один ответ, лог или текст ошибки его не содержит — в
// ответах только passwordSet. При driver = "postgres" хранилище уже общее:
// GET отвечает available:false, остальные методы — 409.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"agentmemory/internal/memory/store"
	memsync "agentmemory/internal/memory/sync"
)

// Границы ввода раздела «Синхронизация»: интервал фоновой синхронизации — от
// минуты (DESIGN.md §11) до суток; длины — защита от мусора, а не правило домена.
const (
	syncMinIntervalSec = 60
	syncMaxIntervalSec = 24 * 60 * 60
	syncMaxNameLen     = 200
	syncMaxFieldLen    = 255
	// syncHubOpTimeout ограничивает проверку подключения, применение схемы и
	// ротацию целиком (соединение — ещё и syncDialTimeout в hubDialer).
	syncHubOpTimeout = 30 * time.Second
)

// syncSSLModes — допустимые значения sslmode общей базы (libpq/pgx).
var syncSSLModes = []string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"}

// syncUnavailableReason — почему синхронизация недоступна при driver = "postgres".
const syncUnavailableReason = "хранилище уже общее (driver = postgres) — синхронизация не нужна"

// syncAPI — зависимости хендлеров раздела. local == nil — хранилище не
// локальное, синхронизация недоступна.
type syncAPI struct {
	s      store.Store
	local  *store.SyncLocal
	envDir string // каталог локальной БД: там .env с паролями общих баз
	// setAuthor — имя для правок этой морды (Writer.SetAuthor): новое имя профиля
	// действует на них сразу.
	setAuthor func(name string)
}

// registerSyncRoutes регистрирует маршруты /api/sync поверх хранилища s;
// envDir — каталог локальной БД (syncEnvDir), setAuthor — смена имени Writer'а морды.
func registerSyncRoutes(mux *http.ServeMux, s store.Store, envDir string, setAuthor func(name string)) {
	api := &syncAPI{s: s, envDir: envDir, setAuthor: setAuthor}
	api.local, _ = store.SyncLocalOf(s)

	mux.HandleFunc("GET /api/sync", api.handleGet)
	mux.HandleFunc("PUT /api/sync/profile", api.requireLocal(api.handlePutProfile))
	mux.HandleFunc("PUT /api/sync/targets/{project}", api.target(api.handlePutTarget))
	mux.HandleFunc("DELETE /api/sync/targets/{project}", api.target(api.handleDeleteTarget))
	mux.HandleFunc("POST /api/sync/targets/{project}/check", api.target(api.handleCheck))
	mux.HandleFunc("POST /api/sync/targets/{project}/schema", api.target(api.handleSchema))
	mux.HandleFunc("POST /api/sync/targets/{project}/rotate", api.target(api.handleRotate))
	mux.HandleFunc("POST /api/sync/targets/{project}/run", api.target(api.handleRun))
}

// requireLocal пропускает запрос, только если хранилище локальное; иначе 409.
func (a *syncAPI) requireLocal(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.local == nil {
			writeErr(w, http.StatusConflict, errors.New(syncUnavailableReason))
			return
		}
		next(w, r)
	}
}

// target — requireLocal плюс проверка имени проекта из пути.
func (a *syncAPI) target(next http.HandlerFunc) http.HandlerFunc {
	return a.requireLocal(func(w http.ResponseWriter, r *http.Request) {
		if err := validateSyncProject(r.PathValue("project")); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		next(w, r)
	})
}

// --- DTO ---

type syncResponse struct {
	Available bool            `json:"available"`
	Reason    string          `json:"reason,omitempty"`
	Profile   *syncProfileDTO `json:"profile,omitempty"`
	Projects  []string        `json:"projects"`
	Targets   []syncTargetDTO `json:"targets"`
}

type syncProfileDTO struct {
	Name        string `json:"name"`        // имя из профиля; пусто — берётся nameDefault
	NameDefault string `json:"nameDefault"` // git config --global user.name
	IntervalSec int    `json:"intervalSec"`
}

type syncTargetDTO struct {
	Project     string       `json:"project"`
	Host        string       `json:"host"`
	Port        int          `json:"port"`
	User        string       `json:"user"`
	DBName      string       `json:"dbname"`
	SSLMode     string       `json:"sslmode"`
	Push        bool         `json:"push"`
	Pull        bool         `json:"pull"`
	PasswordSet bool         `json:"passwordSet"` // сам пароль API не отдаёт никогда
	State       syncStateDTO `json:"state"`
	Warning     string       `json:"warning,omitempty"` // только в ответе PUT: пароль лёг в .env внутри git-репозитория
}

type syncStateDTO struct {
	LastRunAt      int64  `json:"lastRunAt"`
	LastOKAt       int64  `json:"lastOkAt"`
	LastError      string `json:"lastError"`
	Pushed         int    `json:"pushed"`
	Pulled         int    `json:"pulled"`
	Conflicts      int    `json:"conflicts"`
	ConflictsTotal int    `json:"conflictsTotal"`
	Pending        int    `json:"pending"` // помеченных неотправленными записей набора
}

type syncSchemaDTO struct {
	Have  int    `json:"have"`
	Want  int    `json:"want"`
	State string `json:"state"` // empty | outdated | ok | newer
}

type syncCheckResponse struct {
	Reachable bool           `json:"reachable"`
	Error     string         `json:"error,omitempty"`
	Schema    *syncSchemaDTO `json:"schema,omitempty"`
}

type syncRunResponse struct {
	Project      string `json:"project"`
	Pushed       int    `json:"pushed"`
	Pulled       int    `json:"pulled"`
	Conflicts    int    `json:"conflicts"`
	Stamped      int    `json:"stamped"`
	Deferred     int    `json:"deferred"`
	DroppedLinks int    `json:"droppedLinks"`
	Error        string `json:"error,omitempty"` // прогон упал; то же лежит в state.lastError
}

type syncProfileRequest struct {
	Name        string `json:"name"`
	IntervalSec int    `json:"intervalSec"` // 0 — по умолчанию (5 мин)
}

type syncTargetRequest struct {
	Host     string  `json:"host"`
	Port     int     `json:"port"` // 0 — по умолчанию (5432)
	User     string  `json:"user"`
	DBName   string  `json:"dbname"`
	SSLMode  string  `json:"sslmode"` // пусто — по умолчанию (prefer)
	Push     bool    `json:"push"`
	Pull     bool    `json:"pull"`
	Password *string `json:"password"` // нет поля — не трогать; строка — записать; "" — удалить
}

type syncConfirmRequest struct {
	Confirm bool `json:"confirm"`
}

// --- хендлеры ---

// handleGet — состояние раздела: профиль, проекты локальной БД для выбора и
// цели с итогом прогонов.
func (a *syncAPI) handleGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if a.local == nil {
		writeJSON(w, http.StatusOK, syncResponse{Reason: syncUnavailableReason, Projects: []string{}, Targets: []syncTargetDTO{}})
		return
	}
	profile, err := a.profileDTO(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	stats, err := a.s.Stats(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("список проектов: %w", err))
		return
	}
	projects := []string{}
	for scope := range stats.ByScope {
		// Имена, которые не пройдут validateSyncProject, в выбор не попадают:
		// цель для них всё равно не завести.
		if p, ok := strings.CutPrefix(scope, "project:"); ok && validateSyncProject(p) == nil {
			projects = append(projects, p)
		}
	}
	slices.Sort(projects)

	targets, err := a.local.Targets(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	resp := syncResponse{Available: true, Profile: &profile, Projects: projects, Targets: make([]syncTargetDTO, 0, len(targets))}
	for _, t := range targets {
		dto, err := a.targetDTO(ctx, t)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		resp.Targets = append(resp.Targets, dto)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handlePutProfile — имя разработчика и интервал фоновой синхронизации.
func (a *syncAPI) handlePutProfile(w http.ResponseWriter, r *http.Request) {
	var body syncProfileRequest
	if !decodeSyncBody(w, r, &body) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if err := validateSyncText("name", body.Name, syncMaxNameLen); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if body.IntervalSec != 0 && (body.IntervalSec < syncMinIntervalSec || body.IntervalSec > syncMaxIntervalSec) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("intervalSec: ожидается от %d до %d секунд (0 — по умолчанию)", syncMinIntervalSec, syncMaxIntervalSec))
		return
	}
	if err := a.local.SetProfile(r.Context(), store.SyncProfile{AuthorName: body.Name, IntervalSec: body.IntervalSec}); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// Свои правки морда подписывает новым именем без перезапуска; уже запущенные
	// MCP-серверы подхватят его после своего перезапуска (DESIGN.md §4).
	a.setAuthor(authorName(r.Context(), a.s))
	profile, err := a.profileDTO(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, profile)
}

// handlePutTarget создаёт или изменяет цель проекта; проект может ещё не иметь
// локальных записей (новичок подключает чужой проект). Разметку набора делает
// первый прогон Syncer, не морда (DESIGN.md §4.1).
func (a *syncAPI) handlePutTarget(w http.ResponseWriter, r *http.Request) {
	var body syncTargetRequest
	if !decodeSyncBody(w, r, &body) {
		return
	}
	body.Host, body.User, body.DBName = strings.TrimSpace(body.Host), strings.TrimSpace(body.User), strings.TrimSpace(body.DBName)
	if err := validateSyncTarget(body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	ctx := r.Context()
	t, err := a.local.PutTarget(ctx, store.SyncTarget{
		Project: r.PathValue("project"), Host: body.Host, Port: body.Port, User: body.User,
		DBName: body.DBName, SSLMode: body.SSLMode, Push: body.Push, Pull: body.Pull,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	var warning string
	if body.Password != nil {
		if *body.Password == "" {
			err = deleteSyncPassword(a.envDir, t.PasswordEnv)
		} else {
			err = saveSyncPassword(a.envDir, t.PasswordEnv, *body.Password)
			if err == nil && insideGitRepo(a.envDir) {
				warning = fmt.Sprintf("пароль сохранён в %s внутри git-репозитория — проверьте, что .env в .gitignore", filepath.Join(a.envDir, ".env"))
			}
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("цель сохранена, пароль — нет: %w", err))
			return
		}
	}
	dto, err := a.targetDTO(ctx, t)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	dto.Warning = warning
	writeJSON(w, http.StatusOK, dto)
}

// handleDeleteTarget удаляет цель и её пароль из .env; скачанные записи
// остаются (DESIGN.md §7.1). Удаление берёт lease цели: посреди прогона Syncer
// записал бы sync_state уже удалённой цели — тогда 409.
func (a *syncAPI) handleDeleteTarget(w http.ResponseWriter, r *http.Request) {
	ctx, project := r.Context(), r.PathValue("project")
	t, ok := a.loadTarget(w, r)
	if !ok {
		return
	}
	holder := syncRunHolder()
	leased, err := a.local.Lease(ctx, project, holder, time.Now().Add(syncLeaseTTL).Unix())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !leased {
		writeErr(w, http.StatusConflict, errors.New("синхронизация уже идёт — удалите цель после прогона"))
		return
	}
	defer func() {
		if err := a.local.Release(context.WithoutCancel(ctx), project, holder); err != nil {
			log.Printf("memo web: синхронизация %s: %v", project, err)
		}
	}()
	if err := a.local.DeleteTarget(ctx, project); err != nil {
		writeSyncTargetErr(w, project, err)
		return
	}
	if err := deleteSyncPassword(a.envDir, t.PasswordEnv); err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("цель удалена, пароль из .env — нет: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "project": project})
}

// handleCheck — «Проверить подключение»: только чтение, без DDL. Недоступная
// база — не ошибка запроса, а reachable:false с причиной.
func (a *syncAPI) handleCheck(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.loadTarget(w, r); !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), syncHubOpTimeout)
	defer cancel()
	hub, err := a.dialer().open(ctx, r.PathValue("project"))
	if err != nil {
		writeJSON(w, http.StatusOK, syncCheckResponse{Error: err.Error()})
		return
	}
	defer hub.Close()
	st, err := hub.Schema(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, syncCheckResponse{Reachable: true, Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, syncCheckResponse{Reachable: true, Schema: schemaDTO(st)})
}

// handleSchema — «Создать/обновить схему»: только с явным confirm; схема новее
// бинаря — 409, база не меняется.
func (a *syncAPI) handleSchema(w http.ResponseWriter, r *http.Request) {
	hub, ctx, done, ok := a.confirmedHub(w, r)
	if !ok {
		return
	}
	defer done()
	st, err := hub.Schema(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if st.Ahead() {
		writeErr(w, http.StatusConflict, errors.New(schemaStateText(st)))
		return
	}
	if err := hub.ApplySchema(ctx); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if st, err = hub.Schema(ctx); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, syncCheckResponse{Reachable: true, Schema: schemaDTO(st)})
}

// handleRotate — «База восстановлена из резервной копии»: новый base_id, у всех
// клиентов курсор сбросится (DESIGN.md §4.1). Только с явным confirm и на
// схеме версии бинаря.
func (a *syncAPI) handleRotate(w http.ResponseWriter, r *http.Request) {
	hub, ctx, done, ok := a.confirmedHub(w, r)
	if !ok {
		return
	}
	defer done()
	st, err := hub.Schema(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if !st.Current() {
		writeErr(w, http.StatusConflict, fmt.Errorf("ротация невозможна: %s", schemaStateText(st)))
		return
	}
	id, err := hub.RotateBase(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "baseId": id})
}

// handleRun — «Синхронизировать сейчас» под тем же lease, что фон. Прогон не
// обрывается закрытием вкладки (у него свой таймаут); упавший прогон — 200 с
// error: итог и ошибка уже лежат в sync_state, как после фонового.
func (a *syncAPI) handleRun(w http.ResponseWriter, r *http.Request) {
	ctx, project := context.WithoutCancel(r.Context()), r.PathValue("project")
	res, err := runSync(ctx, a.local, a.envDir, project, authorName(ctx, a.s))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, fmt.Errorf("цель синхронизации %q не найдена", project))
		return
	case errors.Is(err, errSyncBusy):
		writeErr(w, http.StatusConflict, errors.New("синхронизация уже идёт"))
		return
	}
	resp := syncRunResponse{
		Project: project, Pushed: res.Pushed, Pulled: res.Pulled, Conflicts: res.Conflicts,
		Stamped: res.Stamped, Deferred: res.Deferred, DroppedLinks: res.DroppedLinks,
	}
	if err != nil {
		resp.Error = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- помощники ---

func (a *syncAPI) dialer() hubDialer {
	return hubDialer{local: a.local, envDir: a.envDir}
}

// loadTarget читает цель проекта из пути; нет цели — 404.
func (a *syncAPI) loadTarget(w http.ResponseWriter, r *http.Request) (store.SyncTarget, bool) {
	project := r.PathValue("project")
	t, err := a.local.Target(r.Context(), project)
	if err != nil {
		writeSyncTargetErr(w, project, err)
		return store.SyncTarget{}, false
	}
	return t, true
}

// confirmedHub — общий вход операций, меняющих общую базу: тело {confirm:true}
// (без него — 400, база не трогается), существующая цель, подключение под
// syncHubOpTimeout (недоступна — 502). done закрывает соединение и таймаут.
func (a *syncAPI) confirmedHub(w http.ResponseWriter, r *http.Request) (*store.Hub, context.Context, func(), bool) {
	var body syncConfirmRequest
	if !decodeSyncBody(w, r, &body) {
		return nil, nil, nil, false
	}
	if !body.Confirm {
		writeErr(w, http.StatusBadRequest, errors.New("операция меняет общую базу — нужно подтверждение: {\"confirm\": true}"))
		return nil, nil, nil, false
	}
	if _, ok := a.loadTarget(w, r); !ok {
		return nil, nil, nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), syncHubOpTimeout)
	hub, err := a.dialer().open(ctx, r.PathValue("project"))
	if err != nil {
		cancel()
		writeErr(w, http.StatusBadGateway, err)
		return nil, nil, nil, false
	}
	return hub, ctx, func() { hub.Close(); cancel() }, true
}

func (a *syncAPI) profileDTO(ctx context.Context) (syncProfileDTO, error) {
	p, err := a.local.Profile(ctx)
	if err != nil {
		return syncProfileDTO{}, err
	}
	return syncProfileDTO{Name: p.AuthorName, NameDefault: gitUserName(ctx), IntervalSec: p.IntervalSec}, nil
}

// targetDTO — цель без пароля: только признак, задан ли он (в .env каталога БД
// или в окружении процесса — где его найдёт прогон), плюс итог прогонов.
func (a *syncAPI) targetDTO(ctx context.Context, t store.SyncTarget) (syncTargetDTO, error) {
	password, err := syncPassword(a.envDir, t.PasswordEnv)
	if err != nil {
		return syncTargetDTO{}, err
	}
	st, err := a.local.State(ctx, t.Project)
	if err != nil {
		return syncTargetDTO{}, err
	}
	pending, err := a.local.PendingCount(ctx, memsync.Scope(t.Project), memsync.Kinds())
	if err != nil {
		return syncTargetDTO{}, err
	}
	return syncTargetDTO{
		Project: t.Project, Host: t.Host, Port: t.Port, User: t.User, DBName: t.DBName, SSLMode: t.SSLMode,
		Push: t.Push, Pull: t.Pull, PasswordSet: password != "",
		State: syncStateDTO{
			LastRunAt: st.LastRunAt, LastOKAt: st.LastOKAt, LastError: st.LastError,
			Pushed: st.LastPushed, Pulled: st.LastPulled, Conflicts: st.LastConflicts,
			ConflictsTotal: st.ConflictsTotal, Pending: pending,
		},
	}, nil
}

func schemaDTO(st store.HubSchema) *syncSchemaDTO {
	dto := &syncSchemaDTO{Have: st.Version, Want: st.Supported}
	switch {
	case st.Empty:
		dto.State = "empty"
	case st.Ahead():
		dto.State = "newer"
	case st.Current():
		dto.State = "ok"
	default:
		dto.State = "outdated"
	}
	return dto
}

// schemaStateText — состояние схемы словами для текстов ошибок.
func schemaStateText(st store.HubSchema) string {
	switch {
	case st.Empty:
		return "в общей базе нет схемы — примените схему"
	case st.Ahead():
		return fmt.Sprintf("схема общей базы (версия %d) новее этого memo (%d) — обновите memo", st.Version, st.Supported)
	default:
		return fmt.Sprintf("схема общей базы (версия %d) старее этого memo (%d) — примените схему", st.Version, st.Supported)
	}
}

func writeSyncTargetErr(w http.ResponseWriter, project string, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, fmt.Errorf("цель синхронизации %q не найдена", project))
		return
	}
	writeErr(w, http.StatusInternalServerError, err)
}

// decodeSyncBody разбирает JSON-тело в out; лишнее поле или мусор — 400.
// Синтаксическая ошибка json цитирует символ тела (он может быть из пароля),
// поэтому в ответ уходит только позиция.
func decodeSyncBody(w http.ResponseWriter, r *http.Request, out any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) {
			err = fmt.Errorf("некорректный JSON (позиция %d)", syntax.Offset)
		}
		writeErr(w, http.StatusBadRequest, fmt.Errorf("разбор тела запроса: %w", err))
		return false
	}
	return true
}

// validateSyncProject — имя проекта цели (scope project:<имя>): буквы, цифры,
// '-', '_', '.'; без пробелов и разделителей пути.
func validateSyncProject(project string) error {
	if project == "" || utf8.RuneCountInString(project) > syncMaxNameLen {
		return fmt.Errorf("имя проекта: ожидается от 1 до %d символов", syncMaxNameLen)
	}
	for _, r := range project {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("-_.", r) {
			return fmt.Errorf("имя проекта %q: допустимы буквы, цифры и символы - _ .", project)
		}
	}
	return nil
}

// validateSyncText — строка без управляющих символов и не длиннее max.
func validateSyncText(field, v string, max int) error {
	if utf8.RuneCountInString(v) > max {
		return fmt.Errorf("%s: не длиннее %d символов", field, max)
	}
	if strings.ContainsFunc(v, unicode.IsControl) {
		return fmt.Errorf("%s: недопустимые управляющие символы", field)
	}
	return nil
}

// validateSyncTarget проверяет параметры подключения цели. Хост и имя базы
// попадают в URL DSN — символы его структуры в них запрещены.
func validateSyncTarget(b syncTargetRequest) error {
	for _, f := range []struct{ name, v string }{{"host", b.Host}, {"user", b.User}, {"dbname", b.DBName}} {
		if f.v == "" {
			return fmt.Errorf("%s обязателен", f.name)
		}
		if err := validateSyncText(f.name, f.v, syncMaxFieldLen); err != nil {
			return err
		}
	}
	if strings.ContainsFunc(b.Host, unicode.IsSpace) || strings.ContainsAny(b.Host, "/?#@\\") {
		return fmt.Errorf("host %q: ожидается имя хоста или IP-адрес", b.Host)
	}
	if strings.ContainsAny(b.DBName, "/?#") {
		return fmt.Errorf("dbname %q: недопустимые символы / ? #", b.DBName)
	}
	if b.Port < 0 || b.Port > 65535 {
		return fmt.Errorf("port: ожидается от 1 до 65535 (0 — по умолчанию 5432)")
	}
	if b.SSLMode != "" && !slices.Contains(syncSSLModes, b.SSLMode) {
		return fmt.Errorf("sslmode %q: ожидается одно из %s", b.SSLMode, strings.Join(syncSSLModes, ", "))
	}
	if b.Password != nil && strings.ContainsAny(*b.Password, "\r\n") {
		return errors.New("password: перевод строки недопустим")
	}
	return nil
}

// insideGitRepo — каталог dir лежит внутри git-репозитория (есть .git в нём или
// выше): тогда .env с паролем рискует попасть в коммит (DESIGN.md §13).
func insideGitRepo(dir string) bool {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}
