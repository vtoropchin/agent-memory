// Package reason определяет интерфейс Reasoner — LLM-слой для суммаризации и
// консолидации памяти (DESIGN.md §6, §10) и заглушку Noop для Этапа 1, до подключения
// DeepSeek на Этапе 2.
package reason

import "context"

// Reasoner — текстовый процессор памяти поверх chat-модели (DeepSeek).
type Reasoner interface {
	Complete(ctx context.Context, prompt string) (string, error)
}

// Noop — заглушка Reasoner для Этапа 1: суммаризация/консолидация отключены.
type Noop struct{}

// NewNoop создаёт заглушку Reasoner, используемую пока Reasoner не подключён в конфиге.
func NewNoop() *Noop {
	return &Noop{}
}

// Complete всегда возвращает пустую строку без ошибки — вызывающий код должен
// рассчитывать на деградацию (например, показывать бриф без LLM-суммаризации).
func (n *Noop) Complete(_ context.Context, _ string) (string, error) {
	return "", nil
}

var _ Reasoner = (*Noop)(nil)
