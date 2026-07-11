// Package store определяет интерфейс Store (DESIGN.md §6) и его реализацию поверх
// SQLite: метаданные + FTS5 (лексический индекс). Векторный индекс (sqlite-vec)
// подключается на Этапе 3 — до тех пор SearchVector/PutVector остаются заглушками.
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
}

// Stats — сводные счётчики памяти для дашборда web-морды (DESIGN.md §12).
type Stats struct {
	Total      int            // живых записей (superseded_by IS NULL)
	Superseded int            // архивных (superseded_by IS NOT NULL)
	ByScope    map[string]int // живые записи по scope
	ByKind     map[string]int // живые записи по kind
}

// Store — единая точка доступа к хранилищу памяти: метаданные, лексический и
// (в будущем) векторный индексы, граф связей.
type Store interface {
	Put(ctx context.Context, m memory.Memory) error
	Get(ctx context.Context, id string) (memory.Memory, error)
	SearchLexical(ctx context.Context, q memory.Query) ([]memory.Candidate, error)
	SearchVector(ctx context.Context, vec []float32, q memory.Query) ([]memory.Candidate, error) // no-op пока нет Embedder
	Neighbors(ctx context.Context, id string) ([]memory.Memory, error)                           // граф-обход
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
}
