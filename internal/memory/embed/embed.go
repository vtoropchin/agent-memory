// Package embed определяет интерфейс Embedder — опциональный семантический вектор
// (DESIGN.md §6, §9) и заглушку Noop для этапа MVP, когда векторный слой отключён.
package embed

import "context"

// Embedder превращает тексты в векторы фиксированной размерности Dim() для модели Model().
// Реализации: noop (MVP) | ollama | openai (см. DESIGN.md §9).
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Model() string
	Dim() int
}

// Noop — заглушка Embedder для Этапа 1: не считает векторы, recall деградирует до FTS5.
type Noop struct{}

// NewNoop создаёт заглушку Embedder, используемую пока Embedder отключён в конфиге.
func NewNoop() *Noop {
	return &Noop{}
}

// Embed возвращает по одному nil-вектору на каждый текст — вычислений не производится.
func (n *Noop) Embed(_ context.Context, texts []string) ([][]float32, error) {
	return make([][]float32, len(texts)), nil
}

// Model возвращает имя модели-заглушки.
func (n *Noop) Model() string {
	return "noop"
}

// Dim возвращает размерность вектора — 0, т.к. векторы не считаются.
func (n *Noop) Dim() int {
	return 0
}

var _ Embedder = (*Noop)(nil)
