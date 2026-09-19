// Package memory описывает доменную модель AgentMemory: атомарную единицу памяти,
// параметры запроса на её поиск и результат сборки recall-брифа.
package memory

// Kind — тип единицы памяти (см. DESIGN.md §2).
type Kind string

const (
	// KindSemantic — факты, знания, предпочтения. Живут долго.
	KindSemantic Kind = "semantic"
	// KindEpisodic — события сессий. Живут средне, устаревают.
	KindEpisodic Kind = "episodic"
	// KindProcedural — рецепты, воркфлоу. Живут долго.
	KindProcedural Kind = "procedural"
	// KindDebt — техдолг: известная проблема кода/архитектуры, отложенная
	// осознанно (не путать с episodic — это не событие сессии, а факт о
	// состоянии проекта, и живёт по тем же правилам, что semantic/procedural:
	// decay/consolidate его не трогают, см. maint.Decayer/Consolidator, которые
	// работают только с episodic). Открытый долг — обычная живая запись; закрытие
	// долга (фикс применён) — не отдельное поле статуса, а тот же тумстоун-
	// механизм, что и у любой другой памяти: Writer.Forget/Store.Supersede с
	// replaced_by, указывающим на запись о фиксе. В recall долг попадает в
	// дефолтную выдачу (без явного фильтра kinds), но выводится отдельной
	// секцией брифа и получает понижающий множитель итогового скора
	// (config.RecallConfig.DebtWeight, recall.Recaller) — накопленные долги не
	// должны вытеснять факты из бюджета токенов.
	KindDebt Kind = "debt"
	// KindTask — единица работы агента: то, что раньше жило отдельным файлом
	// задачи в репозитории (tasks/*.md). В отличие от остальных kind, у задачи
	// есть явное состояние (Memory.Status) — открытая задача не описывается
	// одним лишь фактом существования записи, потому что «взята в работу» и «ещё
	// не начата» — разные вещи для агента, восстанавливающего контекст. Content
	// задачи — полноценный документ (что сделать, границы, критерий приёмки), а
	// Summary — её заголовок: именно он попадает в recall-бриф, полный текст
	// агент забирает по id (mcp memory_get). Живёт до явного закрытия: decay/
	// consolidate работают только с episodic, дедуп задачи не сливает (разные
	// задачи легитимно похожи текстуально, см. write.Writer.dedup и
	// maint.DedupSweeper). С Этапа 15 у задачи, помимо стадии доски (Status),
	// есть тип (Type: эпик/история/задача), git-ветка (Branch) и имя процесса,
	// которым она выполняется (Workflow) — все четыре поля только у этого kind.
	KindTask Kind = "task"
)

// Status — стадия kanban-доски для единицы работы (Kind == KindTask, DESIGN.md
// §14 Этап 15). Для остальных kind пусто: статус не универсальное свойство
// памяти, а часть модели задачи. Стадии идут в порядке жизненного цикла
// «бэклог → к выполнению → в работе → ревью → тестирование → к релизу →
// готово», но переходы порядком не ограничены — карточку можно вернуть назад.
// Блокировка задачи — НЕ стадия, а вычисляемое свойство из связей
// rel=blocked_by (задача 15.4, см. LiveBlockers): прежний статус blocked удалён
// (миграция схемы 3 -> 4 переводит его в todo). backlog/todo — единственные
// стадии, допустимые у заблокированной задачи (StatusAllowsBlocked): дальше
// её не пустит write path, пока жив хотя бы один блокер. Завершённая задача (StatusDone) остаётся
// живой записью — историю работы не стирают, — но по умолчанию не попадает в
// recall (см. Query.Statuses); отмена задачи, наоборот, — обычный тумстоун
// через write.Writer.Forget.
type Status string

const (
	// StatusBacklog — сырая идея: агент завёл, человек ещё не отобрал в работу.
	StatusBacklog Status = "backlog"
	// StatusTodo — задача отобрана к выполнению, работа не начата. Дефолт при
	// сохранении task.
	StatusTodo Status = "todo"
	// StatusDoing — задача взята в работу.
	StatusDoing Status = "doing"
	// StatusReview — результат на ревью.
	StatusReview Status = "review"
	// StatusTesting — результат тестируется (очередь и активная стадия
	// сознательно склеены: WIP-лимитов нет).
	StatusTesting Status = "testing"
	// StatusRelease — готово к релизу, ждёт выкатки.
	StatusRelease Status = "release"
	// StatusDone — задача выполнена (в релизе). Запись живёт дальше (история),
	// но из дефолтной выдачи recall исключается.
	StatusDone Status = "done"
)

// Statuses — все допустимые значения Status в порядке жизненного цикла; это же
// порядок колонок kanban-доски. Единственный перечень: валидация write path,
// enum MCP-схем и фильтры store ссылаются на него, а не заводят свои копии.
var Statuses = []Status{StatusBacklog, StatusTodo, StatusDoing, StatusReview, StatusTesting, StatusRelease, StatusDone}

// TaskType — тип единицы работы (Kind == KindTask, DESIGN.md §14 Этап 15):
// эпик, история или задача. Поле, а не тег — доска и фильтры опираются на
// него, а теги агенты ставили вразнобой (epic/эпик/История). Иерархия между
// ними — существующие связи rel=part_of строго от дочерней к родительской
// (задача part_of истории, история part_of эпика).
type TaskType string

const (
	// TaskTypeEpic — крупная цель, объединяющая истории/задачи. Обычная
	// карточка со своим хранимым статусом, ходит по колонкам как все.
	TaskTypeEpic TaskType = "epic"
	// TaskTypeStory — пользовательская история внутри эпика.
	TaskTypeStory TaskType = "story"
	// TaskTypeTask — обычная задача. Дефолт при сохранении task.
	TaskTypeTask TaskType = "task"
)

// TaskTypes — все допустимые значения TaskType. Единственный перечень для
// валидации write path и enum MCP-схем, как Statuses.
var TaskTypes = []TaskType{TaskTypeEpic, TaskTypeStory, TaskTypeTask}

// Значения Link.Rel — тип связи между записями в графе links.
const (
	RelRelated    = "related"
	RelSupersedes = "supersedes"
	RelCauses     = "causes"
	RelPartOf     = "part_of"
	// RelBlockedBy — блокировка задачи задачей (DESIGN.md §14 Этап 15, задача
	// 15.4): ребро идёт ОТ заблокированной задачи К блокеру («на этой карточке —
	// кем заблокирована»). Единственное каноническое значение: зеркального
	// blocks нет, «что блокирует эта задача» — обратный обход (BlockedBy).
	// Блокирует только живой и не done блокер (LiveBlockers); цель ребра обязана
	// быть задачей (write.ErrBlockerNotTask).
	RelBlockedBy = "blocked_by"
)

// Link — направленное ребро графа памяти: текущая запись -> Dst с отношением Rel.
type Link struct {
	Dst string // id записи-цели
	Rel string // related | supersedes | causes | part_of | blocked_by (пусто трактуется как related)
}

// Memory — атомарная единица памяти. Соответствует строке таблицы `memories`
// (DESIGN.md §4) плюс связанные tags/links.
type Memory struct {
	Id           string // ULID
	Kind         Kind
	Scope        string // user:* | project:<name> | agent:self
	Content      string // сам факт, атомарно
	Summary      *string
	Importance   int   // 1..5
	CreatedAt    int64 // unix ts
	UpdatedAt    int64
	AccessedAt   *int64
	AccessCount  int
	SupersededBy *string // id заменившей записи (мягкое забывание)
	Source       *string // session id / откуда пришло
	// ExpiresAt — явный срок жизни записи (unix ts, задача 9.4): 0 (или NULL в
	// БД) означает вечную запись, без срока. В отличие от decay (эвристическое
	// затухание неиспользуемых episodic, DESIGN.md §11), TTL — точная, заранее
	// известная агенту дата истечения факта (см. write.SaveInput.TTL/
	// UpdateInput.TTL, maint.Expirer). recall скрывает просроченные записи
	// сразу (Store.SearchLexical/SearchVector), фоновый ExpireSweep превращает
	// их в тумстоун (Supersede) при следующем прогоне планировщика.
	ExpiresAt int64
	// Status — стадия задачи (см. Status): заполняется только у Kind ==
	// KindTask, у остальных типов памяти пусто. В БД — nullable-колонка status
	// (пусто хранится как NULL).
	Status Status
	// Type — тип задачи (см. TaskType): только у Kind == KindTask (дефолт
	// TaskTypeTask), у остальных пусто. В БД — nullable-колонка task_type.
	Type TaskType
	// Branch — имя git-ветки, в которой ведётся задача (свободная строка без
	// валидации, напр. "feat/kanban-board"): только у Kind == KindTask, у
	// остальных пусто. В БД — nullable-колонка branch.
	Branch string
	// Workflow — имя процесса, которым задача выполняется ("dev", "ultracode",
	// "code-review", …), не id запуска: только у Kind == KindTask, у остальных
	// пусто. В БД — nullable-колонка workflow.
	Workflow string
	Tags     []string
	Links    []Link
}

// Query — параметры поиска для recall.
type Query struct {
	Text string // текст запроса
	// Scopes — набор пространств для фильтра (DESIGN.md §2.1), напр.
	// ["project:<name>", "user:*"]. Если пуст — откат на одиночный Scope; если
	// пусты оба поля — фильтра по scope нет (adhoc-режим, мультипроектность выключена).
	Scopes []string
	Scope  string   // фильтр по одиночному scope (используется write-путём/дедупом)
	Kinds  []string // фильтр по kind, пусто = все
	// Statuses — фильтр по состоянию задачи (Memory.Status): вернуть только
	// записи с одним из перечисленных статусов (OR). Пусто НЕ означает «без
	// фильтра»: по умолчанию из выдачи исключаются выполненные задачи
	// (Kind == KindTask и Status == StatusDone) — сделанное не должно
	// вытеснять актуальное, а увидеть его можно, спросив явно
	// (Statuses = ["done"]). Записи прочих kind фильтр не затрагивает.
	Statuses []string
	// IncludeDone отключает это умолчание: при пустом Statuses выдача не
	// фильтруется по состоянию вообще (выполненные задачи наравне со всем
	// остальным). Нужно потребителям, которым память — предмет обзора, а не
	// рабочий контекст: поиск web-морды, где скрытая от поиска задача всё равно
	// видна в листинге. При непустом Statuses флаг не действует — явный набор
	// состояний сам по себе точен.
	IncludeDone bool
	// Types — фильтр по типу задачи (Memory.Type, Этап 15): вернуть только
	// задачи с одним из перечисленных типов (OR). Пусто = без фильтра. Записи
	// прочих kind (у них тип NULL) фильтр не пропускает — как Statuses с
	// явным набором: непустой Types сам по себе означает «только задачи».
	Types []string
	// Tags — фильтр по тегам: запись проходит, если имеет хотя бы один из
	// перечисленных тегов (OR-семантика, задача 8.2). Пусто = без фильтра.
	Tags        []string
	Limit       int   // максимум кандидатов от источника
	TokenBudget int   // бюджет токенов для сборки брифа
	SinceTs     int64 // фильтр свежести: только записи с created_at >= SinceTs (0 = без фильтра)
	// CreatedAfter/CreatedBefore — фильтр по дате создания (unix ts, 0 = фильтр
	// выключен), задача 8.2: created_at >= CreatedAfter и/или created_at <=
	// CreatedBefore. Отдельно от SinceTs — тот адресован свежести ранжирования, эти
	// поля пробрасываются явным пользовательским запросом (MCP memory_recall).
	CreatedAfter  int64
	CreatedBefore int64
	Summarize     bool // запросить у Reasoner LLM-сводку брифа (DESIGN.md §8)
	// Explain — добавить под каждой записью брифа трассу ранжирования: источники
	// кандидата (lexical/vector) и разложение итогового скора на слагаемые
	// (DESIGN.md §8, §12, задача 8.1). Отладка/тюнинг весов [recall].weights —
	// само ранжирование и отбор записей не меняются.
	Explain bool
}

// Candidate — кандидат на попадание в бриф: запись + оценка релевантности
// от конкретного источника поиска (для последующего RRF-слияния). Store.SearchLexical/
// SearchVector отдают Source ровно одного источника ("lexical" | "vector");
// recall.Recaller.Candidates (задача 10.1) отдаёт уже слитый после RRF результат, где
// Source может быть составным ("lexical+vector") — те же значения, что в explain
// (задача 8.1, DESIGN.md §8).
type Candidate struct {
	Memory Memory
	Score  float64
	Source string // "lexical" | "vector" | "lexical+vector"
}

// Brief — результат сборки recall: готовый Markdown-текст и метаданные о том,
// сколько записей вошло/свёрнуто.
type Brief struct {
	Markdown     string
	Count        int // сколько записей вошло в бриф
	Collapsed    int // сколько релевантных записей свёрнуто (не поместились в бюджет)
	ApproxTokens int
}
