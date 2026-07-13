package maint

import (
	"context"
	"fmt"
	"log"
	"time"

	"agentmemory/internal/memory/store"
)

// ExpireResult — статистика одного прогона ExpireSweep.
type ExpireResult struct {
	Scanned  int // сколько просроченных кандидатов вернул Store.ListExpired
	Archived int // сколько записей превращено в тумстоун
}

// Expirer архивирует записи с истёкшим явным TTL (Memory.ExpiresAt, DESIGN.md §4,
// §11, §12, задача 9.4). В отличие от Decayer (эвристическое затухание
// неиспользуемых episodic — вес падает постепенно, полное архивирование только
// после долгого простоя), Expirer работает по детерминированному, заранее
// известному агенту сроку годности факта — записи с любым Kind, если у них истёк
// TTL, сразу превращаются в тумстоун, без промежуточного понижения importance.
//
// recall уже скрывает такие записи немедленно (Store.SearchLexical/SearchVector,
// см. metaFilters) — ExpireSweep дополняет это гигиеной БД между прогонами
// планировщика: превращает уже скрытое из recall в тумстоун (Store.Supersede),
// та же семантика мягкого удаления, что использует Decay/memory_forget.
type Expirer struct {
	store store.Store
}

// NewExpirer создаёт Expirer поверх Store.
func NewExpirer(s store.Store) *Expirer {
	return &Expirer{store: s}
}

// ExpireSweep архивирует (self-supersede-тумстоун Store.Supersede(id, id)) все
// живые записи с истёкшим TTL (Store.ListExpired: 0 < expires_at < now, без
// superseded_by). limit <= 0 означает без ограничения на число кандидатов за один
// прогон.
//
// Деградация (DESIGN.md §15): ошибка Store на отдельной записи логируется в
// stderr и не прерывает обработку остальных кандидатов — эта запись просто не
// учитывается в Archived за текущий прогон и будет снова предложена следующим
// вызовом. Единственная ошибка, возвращаемая вызывающему коду, — ошибка самой
// выборки ListExpired. Идемпотентно: уже заархивированная запись (superseded_by
// проставлен) больше не возвращается ListExpired, повторный прогон её не трогает.
func (e *Expirer) ExpireSweep(ctx context.Context, limit int) (ExpireResult, error) {
	now := time.Now().Unix()

	candidates, err := e.store.ListExpired(ctx, now, limit)
	if err != nil {
		return ExpireResult{}, fmt.Errorf("maint: выборка просроченных записей: %w", err)
	}

	var result ExpireResult
	for _, m := range candidates {
		result.Scanned++
		if err := e.store.Supersede(ctx, m.Id, m.Id); err != nil {
			log.Printf("maint: expire sweep архивирование %q: %v", m.Id, err)
			continue
		}
		result.Archived++
	}
	return result, nil
}
