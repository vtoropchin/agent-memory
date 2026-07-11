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
}
