package maint

import (
	"context"
	"fmt"
	"log"

	"agentmemory/internal/memory"
	"agentmemory/internal/memory/embed"
	"agentmemory/internal/memory/store"
)

// reindexBatchSize — сколько записей эмбеддим за один вызов Embedder.Embed при
// reindex/досчёте очереди.
const reindexBatchSize = 64

// missingVectorsPageSize — сколько записей запрашиваем за один ListMissingVectors
// при досчёте очереди (DESIGN.md §9).
const missingVectorsPageSize = 256

// ReindexResult — статистика одного прогона CheckAndReindex.
type ReindexResult struct {
	Reindexed bool // true, если модель/dim Embedder разошлись с embed_meta и векторный индекс пересоздавался
	Embedded  int  // сколько векторов успешно посчитано и сохранено (PutVector) за этот прогон — и при полном reindex, и при досчёте очереди
	Failed    int  // сколько записей не удалось заэмбеддить (ошибка Embed батча, нехватка векторов в ответе или ошибка PutVector) — останутся в очереди до следующего прогона. Это счётчик попыток, а не уникальных записей: одна и та же запись может быть учтена в Failed несколько раз за один прогон (полный reindex и сразу следующий за ним досчёт очереди на шаге d обе попытки суммируются) и на разных страницах досчёта, если Embed стабильно падает, — поэтому Failed может превышать число уникальных недоэмбеженных записей
}

// Reindexer сверяет embed_meta Store с текущим Embedder и, если модель/размерность
// разошлись (первое включение Embedder или смена модели в конфиге), пересоздаёт
// векторный индекс и перегоняет через него весь контент заново (DESIGN.md §9).
// Независимо от того, случился ли reindex, каждый прогон также досчитывает очередь
// записей без вектора (Store.ListMissingVectors) — тех, что write path оставил без
// эмбеддинга из-за временной ошибки (DESIGN.md §7.4). Пока reindex не завершился,
// recall естественным образом деградирует до FTS5: vec-таблица наполняется
// постепенно, недостающие записи просто не участвуют в векторном поиске.
type Reindexer struct {
	store    store.Store
	embedder embed.Embedder
}

// NewReindexer создаёт Reindexer поверх Store и Embedder.
func NewReindexer(s store.Store, e embed.Embedder) *Reindexer {
	return &Reindexer{store: s, embedder: e}
}

// CheckAndReindex — точка входа джоба (DESIGN.md §11). Сам планировщик (когда и как
// часто вызывать) — предмет задачи 4.3, здесь только логика одного прогона.
//
// Если Embedder=noop (Dim()<=0), ничего не делает — векторный слой выключен,
// Embed вообще не вызывается. Иначе сверяет embed_meta с текущими Model()/Dim():
// расхождение (в т.ч. пустая meta — индекс ещё не создавался ни разу) означает
// полный reindex — ResetVectorIndex под новую модель/dim и перегон всего контента
// через AllForReindex. Затем, вне зависимости от того, случился ли reindex,
// досчитывается очередь ListMissingVectors — это покрывает как записи, отложенные
// write path, так и хвост, недоэмбеженный самим reindex из-за временных ошибок Embed.
//
// Наружу возвращаются только ошибки EmbedMeta/ResetVectorIndex/AllForReindex (без
// них продолжать reindex бессмысленно) и отмена ctx между батчами. Ошибки самого
// эмбеддинга (Embed батча, PutVector отдельной записи) — деградация (DESIGN.md §15):
// логируются в stderr, запись остаётся в очереди на следующий прогон.
func (r *Reindexer) CheckAndReindex(ctx context.Context) (ReindexResult, error) {
	if r.embedder.Dim() <= 0 {
		return ReindexResult{}, nil
	}

	var result ReindexResult
	model, dim, err := r.store.EmbedMeta(ctx)
	if err != nil {
		return ReindexResult{}, fmt.Errorf("maint: чтение embed_meta: %w", err)
	}

	if model != r.embedder.Model() || dim != r.embedder.Dim() {
		if err := r.store.ResetVectorIndex(ctx, r.embedder.Model(), r.embedder.Dim()); err != nil {
			return result, fmt.Errorf("maint: пересоздание векторного индекса: %w", err)
		}
		all, err := r.store.AllForReindex(ctx)
		if err != nil {
			return result, fmt.Errorf("maint: выборка записей для reindex: %w", err)
		}
		if _, err := r.embedItems(ctx, all, &result); err != nil {
			return result, err
		}
		result.Reindexed = true
	}

	if err := r.drainMissing(ctx, &result); err != nil {
		return result, err
	}
	return result, nil
}

// drainMissing досчитывает очередь записей без вектора (DESIGN.md §9, §11):
// повторяет ListMissingVectors, пока список не опустеет. Ошибка ListMissingVectors
// логируется, и досчёт прекращается без ошибки наружу (DESIGN.md §15) — очередь
// доберёт следующий прогон джоба.
//
// Если за целую страницу не удалось сохранить ни одного вектора (Embed/PutVector
// стабильно проваливаются — например, внешний Embedder-сервис недоступен), досчёт
// тоже прекращается: иначе повторный ListMissingVectors вернул бы ту же страницу
// снова и цикл не завершился бы никогда.
func (r *Reindexer) drainMissing(ctx context.Context, result *ReindexResult) error {
	for {
		missing, err := r.store.ListMissingVectors(ctx, missingVectorsPageSize)
		if err != nil {
			log.Printf("maint: reindex: выборка очереди недостающих векторов: %v", err)
			return nil
		}
		if len(missing) == 0 {
			return nil
		}

		progressed, err := r.embedItems(ctx, missing, result)
		if err != nil {
			return err
		}
		if progressed == 0 {
			log.Printf("maint: reindex: досчёт очереди остановлен — ни одна из %d записей не получила вектор", len(missing))
			return nil
		}
	}
}

// embedItems эмбеддит items батчами по reindexBatchSize, сохраняя векторы через
// Store.PutVector и обновляя result.Embedded/result.Failed. Возвращает число
// записей, для которых вектор успешно сохранён за этот вызов (используется
// drainMissing для детекции отсутствия прогресса), и ctx.Err(), если контекст
// отменили между батчами — тогда оставшиеся items не обрабатываются, а текущий
// result возвращается вызывающему коду как есть.
func (r *Reindexer) embedItems(ctx context.Context, items []memory.Memory, result *ReindexResult) (int, error) {
	progressed := 0
	for start := 0; start < len(items); start += reindexBatchSize {
		if err := ctx.Err(); err != nil {
			return progressed, err
		}
		end := start + reindexBatchSize
		if end > len(items) {
			end = len(items)
		}
		progressed += r.embedBatch(ctx, items[start:end], result)
	}
	return progressed, nil
}

// embedBatch эмбеддит один батч записей и сохраняет полученные векторы через
// PutVector, обновляя result.Embedded/result.Failed. Возвращает число записей
// батча, для которых вектор успешно сохранён.
//
// Гварды повторяют write.Writer.embedNow (см. internal/memory/write/write.go):
// ошибка самого Embed валит батч целиком (result.Failed += len(batch), лог в
// stderr, недостающее доберёт следующий прогон джоба через ListMissingVectors);
// если Embed вернул меньше векторов, чем было текстов в батче, — недостающий хвост
// тоже считается Failed; ошибка PutVector отдельной записи — Failed++ только на
// неё, остальные записи батча это не останавливает.
func (r *Reindexer) embedBatch(ctx context.Context, batch []memory.Memory, result *ReindexResult) int {
	contents := make([]string, len(batch))
	for i, m := range batch {
		contents[i] = m.Content
	}
	vecs, err := r.embedder.Embed(ctx, contents)
	if err != nil {
		log.Printf("maint: reindex: Embed батча из %d записей: %v (доберёт следующий прогон)", len(batch), err)
		result.Failed += len(batch)
		return 0
	}

	n := len(vecs)
	if n > len(batch) {
		n = len(batch)
	}
	if n < len(batch) {
		log.Printf("maint: reindex: Embed вернул %d векторов на %d записей батча (доберёт следующий прогон)", len(vecs), len(batch))
		result.Failed += len(batch) - n
	}

	succeeded := 0
	for i := 0; i < n; i++ {
		if len(vecs[i]) == 0 {
			log.Printf("maint: reindex: Embed не вернул вектор для записи %q (доберёт следующий прогон)", batch[i].Id)
			result.Failed++
			continue
		}
		if err := r.store.PutVector(ctx, batch[i].Id, vecs[i]); err != nil {
			log.Printf("maint: reindex: сохранение вектора %q: %v (доберёт следующий прогон)", batch[i].Id, err)
			result.Failed++
			continue
		}
		result.Embedded++
		succeeded++
	}
	return succeeded
}
