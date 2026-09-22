// Package store определяет интерфейс Store (DESIGN.md §6) и его реализации:
// SQLite (метаданные + FTS5 лексический индекс + sqlite-vec векторный индекс,
// sqlite.go) и PostgreSQL+pgvector (postgres.go, DESIGN.md §13). Open — фабрика,
// выбирающая реализацию по config.StoreConfig.Driver (factory.go); для прямого
// открытия SQLite в обход конфига (напр. memimport, работающий с произвольным
// файлом) есть OpenSQLite.
package store

import (
	"context"

	"agentmemory/internal/memory"
)

// BrowseFilter — параметры пагинируемого листинга памяти для read-only web-морды
// (DESIGN.md §12). Живёт в пакете store (концерн read-пути store), а не в доменном
// memory: memory.Query имеет другую семантику (кандидаты поиска, TokenBudget,
// Summarize) и для browse-листинга не подходит.
type BrowseFilter struct {
	Scopes            []string // фильтр scope (m.scope IN (...)); пусто — все scope
	Kinds             []string // фильтр kind; пусто — все типы
	IncludeSuperseded bool     // false (дефолт) — только живые (superseded_by IS NULL)
	Limit             int      // размер страницы; <=0 → дефолт (listDefaultLimit)
	Offset            int      // смещение для пагинации; <0 трактовать как 0
	// ExpiresBefore — только записи с явным TTL, истекающим до этого момента
	// (0 < expires_at < ExpiresBefore); 0 — без фильтра. При заданном фильтре
	// порядок выдачи — expires_at ASC (ближайшие первыми), а не created_at DESC:
	// секция «Истекает скоро» новой web-морды (DESIGN.md §14 Этап 16) иначе
	// усекалась бы по давности создания, а не по сроку.
	ExpiresBefore int64
}

// Stats — сводные счётчики памяти для дашборда web-морды (DESIGN.md §12).
type Stats struct {
	Total      int            // живых записей (superseded_by IS NULL)
	Superseded int            // архивных (superseded_by IS NOT NULL)
	ByScope    map[string]int // живые записи по scope
	ByKind     map[string]int // живые записи по kind
	// PendingVectors — живые записи без строки в векторном индексе: очередь на
	// досчёт вектора (тот же предикат, что у ListMissingVectors). Нет индекса
	// (Embedder всегда был noop) — 0 без ошибки. Панель «Сервисы» новой
	// web-морды (DESIGN.md §14 Этап 16).
	PendingVectors int
}

// VectorStatus — состояние векторного слоя для одной записи (для карточки
// web-морды, DESIGN.md §12/задача 6.4). Нулевое значение означает «векторный
// индекс ещё не создавался».
type VectorStatus struct {
	Embedded bool   // есть строка в memories_vec
	Model    string // из embed_meta (пусто, если векторный индекс не создавался)
	Dim      int    // из embed_meta (0, если не создавался)
}

// HealthInfo — быстрый read-only снимок здоровья файла БД для `memoryd doctor`
// (DESIGN.md §15, задача 9.1). QuickCheck — результат `PRAGMA quick_check`
// (быстрая проверка целостности; "ok" означает отсутствие проблем, любая другая
// строка — первая найденная проблема). JournalMode — текущий режим журнала
// (`PRAGMA journal_mode`, ожидается "wal" — Open всегда запрашивает WAL через DSN).
type HealthInfo struct {
	QuickCheck  string
	JournalMode string
}

// Store — единая точка доступа к хранилищу памяти: метаданные, лексический и
// (в будущем) векторный индексы, граф связей.
type Store interface {
	// Close закрывает соединение с хранилищем. Вызывающая сторона (server-режим,
	// CLI-подкоманды, doctor) держит Store под defer Close() на весь свой прогон.
	Close() error
	Put(ctx context.Context, m memory.Memory) error
	Get(ctx context.Context, id string) (memory.Memory, error)
	// GetMany возвращает записи по списку id — живые и тумстоуны (вызывающий
	// сам фильтрует по SupersededBy: правило «блокирует = жив и не done» —
	// memory.LiveBlockers), с Tags и Links (батчами, не N+1). Порядок не
	// гарантируется, отсутствующие id молча пропускаются (без ErrNotFound: для
	// потребителя это «нет такого блокера», не сбой), пустой ids — пустой
	// результат без запроса. Введён для блокировки задач (Этап 15, задача 15.4):
	// write грузит блокеров одной задачи, recall — блокеров всех задач брифа,
	// доска (board) — кросс-scope блокеров, не попавших в ListTasks.
	GetMany(ctx context.Context, ids []string) ([]memory.Memory, error)
	// ListTasks возвращает все живые задачи (kind = task, superseded_by IS
	// NULL) в scopes (пусто — во всех scope), с Tags и Links (батчами), без
	// пагинации, отсортированные по updated_at DESC — источник kanban-доски
	// (internal/memory/board, Этап 15, задача 15.5). Специализированный read по
	// образцу ListEpisodicForDecay: List с его страницами и без Links под доску
	// не растягивается флагами. Стадия не фильтруется (done возвращается) —
	// усечение колонки done и правило блокировки — забота board.
	ListTasks(ctx context.Context, scopes []string) ([]memory.Memory, error)
	SearchLexical(ctx context.Context, q memory.Query) ([]memory.Candidate, error)
	SearchVector(ctx context.Context, vec []float32, q memory.Query) ([]memory.Candidate, error) // no-op пока нет Embedder
	// Neighbors возвращает живые записи, связанные с id рёбрами links в любом
	// направлении (граф-обход). Tags и Links соседей загружены: по Links соседа
	// потребитель восстанавливает направление и rel ребра к id — так строится
	// обратный обход (дети эпика по part_of — memory.ChildrenOf, задача 15.3;
	// блокеры по blocked_by — 15.4) без отдельного «входящего» запроса, пока
	// множества соседей малы (признак пересмотра — Neighbors стал дорогим).
	Neighbors(ctx context.Context, id string) ([]memory.Memory, error)
	Supersede(ctx context.Context, oldID, newID string) error
	Touch(ctx context.Context, id string) error // accessed_at++, access_count++
	// List возвращает страницу записей под фильтр (для browse-морды), отсортированную
	// по created_at DESC (новые первыми), и total — общее число записей под тот же
	// фильтр БЕЗ учёта Limit/Offset (для пагинации). Заполняет Tags (для чипов в
	// списке); Links НЕ грузит (тяжело для листинга — детальная карточка берёт их
	// через Get). limit<=0 → дефолт listDefaultLimit (50); offset<0 → 0.
	List(ctx context.Context, f BrowseFilter) (items []memory.Memory, total int, err error)
	// Stats возвращает сводные счётчики (DESIGN.md §12, дашборд морды): всего живых,
	// архивных, разбивки живых по scope и kind. Дёшево (агрегатные COUNT/GROUP BY).
	Stats(ctx context.Context) (Stats, error)
	// AllForExport возвращает ВСЕ записи под фильтром scopes — включая тумстоуны
	// (superseded_by IS NOT NULL) — вместе с Tags и Links, отсортированные по id
	// ASC (детерминированный, диффабельный порядок бэкапа). В отличие от
	// AllForReindex (только живые записи, для досчёта вектора) и List
	// (постранично, без Links) — это единственный метод, дающий полный снимок
	// инстанса для `memoryd export` (DESIGN.md §12, задача 9.2). scopes пуст —
	// вся БД; непусто — частичный экспорт (m.scope IN (...)). Векторы не
	// возвращаются — они производные и не входят в формат экспорта.
	AllForExport(ctx context.Context, scopes []string) ([]memory.Memory, error)
	// reindex-поддержка
	EmbedMeta(ctx context.Context) (model string, dim int, err error)
	ResetVectorIndex(ctx context.Context, model string, dim int) error
	AllForReindex(ctx context.Context) ([]memory.Memory, error)
	PutVector(ctx context.Context, id string, vec []float32) error
	// ListMissingVectors возвращает не устаревшие (superseded_by IS NULL) записи, для
	// которых ещё нет строки в векторном индексе — очередь на досчёт вектора фоновым
	// джобом, когда Save/Update отложили эмбеддинг из-за временной недоступности
	// Embedder (DESIGN.md §7.4, §9). limit <= 0 означает без ограничения. Если
	// векторный индекс ещё не создавался (Embedder всегда был noop), возвращает
	// пустой список без ошибки — деградация, а не падение (DESIGN.md §15).
	ListMissingVectors(ctx context.Context, limit int) ([]memory.Memory, error)
	// ListEpisodicForConsolidation возвращает episodic-записи старше olderThan
	// (created_at < olderThan) без superseded_by, отсортированные по scope, затем
	// created_at — для группировки consolidation-джобом (DESIGN.md §11). limit <= 0
	// означает без ограничения.
	ListEpisodicForConsolidation(ctx context.Context, olderThan int64, limit int) ([]memory.Memory, error)
	// ListEpisodicForDecay возвращает episodic-записи без superseded_by, к которым не
	// обращались долго: COALESCE(accessed_at, created_at) < notAccessedSince —
	// то есть время последнего обращения, а при его отсутствии время создания
	// (DESIGN.md §11, §4: accessed_at/access_count — сигналы decay). Отсортированы по
	// этой же дате по возрастанию (самые старые первыми) — decay-джоб обрабатывает их
	// в этом порядке. limit <= 0 означает без ограничения.
	ListEpisodicForDecay(ctx context.Context, notAccessedSince int64, limit int) ([]memory.Memory, error)
	// VectorStatus возвращает состояние вектора записи id (DESIGN.md §12, задача
	// 6.4: бейдж статуса вектора в карточке морды). Если векторный индекс ещё не
	// создавался (Embedder всегда был noop) — Embedded=false, Model="", Dim=0 без
	// ошибки (деградация, DESIGN.md §15). Дёшево: проверка наличия строки +
	// чтение embed_meta.
	VectorStatus(ctx context.Context, id string) (VectorStatus, error)
	// SemanticNeighbors возвращает до k ближайших по cosine-дистанции живых
	// записей к записи id (исключая её саму и superseded), отсортированных по
	// возрастанию дистанции (DESIGN.md §12, задача 6.4: «семантически близкие» в
	// карточке морды). Candidate.Score = distance (честная дистанция, не
	// 1/(1+d) — морде нужно показать её как есть), Candidate.Source = "vector".
	// Tags/Links не заполняются (лёгкий список, как в SearchVector). Если k<=0,
	// у записи нет вектора, либо векторный индекс ещё не создавался — пустой
	// список без ошибки (деградация, DESIGN.md §15).
	SemanticNeighbors(ctx context.Context, id string, k int) ([]memory.Candidate, error)
	// ListExpired возвращает живые записи (superseded_by IS NULL) с истёкшим явным
	// сроком жизни: 0 < expires_at < now (DESIGN.md §4, §11, задача 9.4) —
	// очередь на архивирование фоновым джобом maint.Expirer. Отсортированы по
	// expires_at по возрастанию (самые просроченные первыми). limit <= 0
	// означает без ограничения. Как и ListEpisodicForDecay/Consolidation,
	// Tags/Links не подгружаются — потребителю нужен только Id.
	ListExpired(ctx context.Context, now int64, limit int) ([]memory.Memory, error)
	// Health возвращает быстрый read-only снимок целостности файла БД
	// (DESIGN.md §15, задача 9.1 `memoryd doctor`): quick_check и текущий
	// journal_mode. Не изменяет данные, не выполняет полный integrity_check
	// (дорогой на больших БД) — только быструю проверку.
	Health(ctx context.Context) (HealthInfo, error)
	// SchemaVersion возвращает текущую версию схемы хранилища: SQLite — PRAGMA
	// user_version, PostgreSQL — таблица schema_version (0, если строки нет).
	// Нумерация у драйверов разная (см. sqlite.go schemaVersion и postgres.go
	// pgSchemaVersion) — это подпись для панели «Сервисы» web-морды (DESIGN.md
	// §14 Этап 16), а не признак совместимости между бэкендами.
	SchemaVersion(ctx context.Context) (int, error)
	// LastMaintRun возвращает unix-время последнего завершённого прогона
	// планировщика maintenance-джобов (maint.Scheduler.RunOnce); 0 — прогонов
	// ещё не было. Хранится в однострочной таблице maint_meta (по образцу
	// embed_meta): планировщик живёт в процессе MCP-сервера, а читает отметку
	// `memo web` — без персистентности «последний прогон» не узнать (DESIGN.md
	// §14 Этап 16).
	LastMaintRun(ctx context.Context) (int64, error)
	// SetLastMaintRun записывает время завершения прогона планировщика,
	// перезаписывая предыдущее (одна отметка на весь прогон, не по джобам).
	SetLastMaintRun(ctx context.Context, at int64) error
}
