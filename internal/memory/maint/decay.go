package maint

import (
	"context"
	"fmt"
	"log"
	"time"

	"agentmemory/internal/memory/store"
	"agentmemory/internal/memory/write"
)

// DecayResult — статистика одного прогона Decay.
type DecayResult struct {
	Scanned  int // сколько episodic-кандидатов вернул ListEpisodicForDecay
	Demoted  int // сколько записей понижено по importance
	Archived int // сколько записей отправлено в архив (self-supersede-тумстоун)
}

// Decayer понижает вес неиспользуемых episodic-записей и архивирует совсем старые
// (DESIGN.md §11). Semantic/procedural decay не подлежат (§2: живут долго) — Decayer
// работает только с episodic, что обеспечивает Store.ListEpisodicForDecay.
type Decayer struct {
	store  store.Store
	writer *write.Writer
}

// NewDecayer создаёт Decayer поверх Store и Writer. Writer нужен, чтобы понижать
// importance через обычный write path (Writer.Update), переиспользуя единую
// нормализацию importance (clampImportance) вместо прямой записи в Store.
func NewDecayer(s store.Store, w *write.Writer) *Decayer {
	return &Decayer{store: s, writer: w}
}

// Decay проходит по episodic-записям, к которым не обращались дольше demoteAfter
// (сигналы — accessed_at/created_at, access_count, см. DESIGN.md §4, §11), и для
// каждой:
//   - если с последнего обращения прошло дольше archiveAfter И importance уже
//     минимален (<= 1) — запись отправляется в архив: Writer.Forget(id, ""),
//     тот же self-supersede-тумстоун, что использует memory_forget (см.
//     .claude/memory/mvp-notes.md). После этого запись выпадает из
//     recall/SearchLexical, но строка и история остаются в БД;
//   - иначе — importance понижается на 1 (не ниже 1) через Writer.Update.
//
// limit <= 0 означает без ограничения на число кандидатов за один прогон. Дефолты
// порогов (demoteAfter/archiveAfter) не хардкодятся в этом пакете — их задаёт
// вызывающий (планировщик, Этап 4.3).
//
// Деградация (DESIGN.md §15): ошибка Store/Writer на отдельной записи логируется в
// stderr и не прерывает обработку остальных кандидатов — эта запись просто не
// учитывается в Demoted/Archived за текущий прогон и будет снова предложена
// следующим вызовом Decay. Единственная ошибка, возвращаемая вызывающему коду, —
// ошибка самой выборки ListEpisodicForDecay.
func (d *Decayer) Decay(ctx context.Context, demoteAfter, archiveAfter time.Duration, limit int) (DecayResult, error) {
	now := time.Now().Unix()
	notAccessedSince := now - int64(demoteAfter.Seconds())

	candidates, err := d.store.ListEpisodicForDecay(ctx, notAccessedSince, limit)
	if err != nil {
		return DecayResult{}, fmt.Errorf("maint: выборка episodic для decay: %w", err)
	}

	var result DecayResult
	archiveThreshold := int64(archiveAfter.Seconds())
	for _, m := range candidates {
		result.Scanned++

		lastActivity := m.CreatedAt
		if m.AccessedAt != nil {
			lastActivity = *m.AccessedAt
		}
		idle := now - lastActivity

		if idle >= archiveThreshold && m.Importance <= 1 {
			if err := d.writer.Forget(ctx, m.Id, ""); err != nil {
				log.Printf("maint: decay архивирование %q: %v", m.Id, err)
				continue
			}
			result.Archived++
			continue
		}

		newImportance := m.Importance - 1
		if newImportance < 1 {
			newImportance = 1
		}
		if err := d.writer.Update(ctx, write.UpdateInput{Id: m.Id, Importance: &newImportance}); err != nil {
			log.Printf("maint: decay понижение importance %q: %v", m.Id, err)
			continue
		}
		result.Demoted++
	}
	return result, nil
}
