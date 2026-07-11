// Package store определяет интерфейс Store (DESIGN.md §6) и его реализацию поверх
// SQLite: метаданные + FTS5 (лексический индекс). Векторный индекс (sqlite-vec)
// подключается на Этапе 3 — до тех пор SearchVector/PutVector остаются заглушками.
package store

import (
	"context"

	"agentmemory/internal/memory"
)

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
}
