package maint

import (
	"context"
	"fmt"
	"log"
	"sort"

	"agentmemory/internal/memory"
	"agentmemory/internal/memory/store"
	"agentmemory/internal/memory/write"
)

// DedupResult — статистика одного прогона Sweep.
type DedupResult struct {
	Scanned    int // сколько живых записей просмотрено (AllForReindex)
	Superseded int // сколько записей отправлено в тумстоун как near-duplicate
}

// DedupSweeper ретроактивно чистит накопившиеся почти-дубли (DESIGN.md §11, §7.2):
// write path (internal/memory/write) дедуплицирует только на вставке, но дубли
// всё равно накапливаются в обход него — импорт, консолидация (maint.Consolidate),
// правки content через memory_update. Sweep находит такие пары постфактум и
// сводит их к одной живой записи через Store.Supersede.
type DedupSweeper struct {
	store  store.Store
	writer *write.Writer // тумстоуны проигравших — через Forget: Writer несёт имя редактора (Этап 19)
}

// NewDedupSweeper создаёт DedupSweeper поверх Store и Writer.
func NewDedupSweeper(s store.Store, w *write.Writer) *DedupSweeper {
	return &DedupSweeper{store: s, writer: w}
}

// Sweep проходит по всем живым (superseded_by IS NULL) записям, группирует их по
// паре (Scope, Kind) и внутри каждой группы ищет near-duplicate пары по content —
// тем же правилом SimHash-сравнения, что и write path при вставке
// (write.IsNearDuplicate), чтобы sweep и write path не расходились в том, что
// считается дублем. Группировка полностью повторяет write path (см.
// internal/memory/write/write.go, dedup: SearchLexical фильтрует кандидатов по
// Scope И Kind): одинаковый текст в разных scope дублем не считается, и запись
// одного Kind никогда не supersede'ит запись другого Kind в том же scope — даже
// при идентичном content (episodic-наблюдение и semantic-факт легитимно разные
// сущности, пусть и сформулированные одинаково). Задачи (kind=task) в сравнение
// не попадают вовсе — write.SkipDedup, см. groupForDedup; они не входят и в
// DedupResult.Scanned (счётчик честно означает «просканировано на дубли»).
//
// Из каждой найденной пары выживает запись с большим Importance, при равенстве —
// более новая (CreatedAt); проигравшая помечается Writer.Forget(loserID,
// winnerID). Уже проигравшие в рамках этого же прогона повторно не сравниваются
// и не могут забрать чужую победу (см. sweepGroup).
//
// Деградация (DESIGN.md §15): ошибка Supersede на отдельной паре логируется в
// stderr и не прерывает обработку остальных пар этой и других групп — пара
// просто остаётся дублями до следующего прогона. Единственная ошибка, которую
// Sweep возвращает вызывающему коду, — ошибка самой выборки AllForReindex.
func (d *DedupSweeper) Sweep(ctx context.Context) (DedupResult, error) {
	all, err := d.store.AllForReindex(ctx)
	if err != nil {
		return DedupResult{}, fmt.Errorf("maint: выборка живых записей для dedup sweep: %w", err)
	}

	var result DedupResult
	for _, members := range groupForDedup(all) {
		result.Scanned += len(members)
		d.sweepGroup(ctx, members, &result)
	}
	return result, nil
}

// dedupGroupKey — ключ группировки живых записей по паре (Scope, Kind): dedup
// сравнивает записи только внутри одной такой группы (см. годок Sweep).
type dedupGroupKey struct {
	scope string
	kind  memory.Kind
}

// groupForDedup группирует all по паре (Scope, Kind) — той же гранулярности, что
// использует write path при поиске дублей на вставке (см. годок Sweep). Записи
// внутри каждой группы сортируются по CreatedAt (при равенстве — по Id, для
// полной детерминированности) — от этого порядка зависит, какая запись из цепочки
// похожих становится «чемпионом» первой в sweepGroup. Порядок обхода самих групп
// на результат не влияет: группы независимы (Supersede происходит только внутри
// одной группы), поэтому достаточно map, а не упорядоченного слайса.
// Kind, исключённый из дедупликации на write path (write.SkipDedup — задачи,
// kind=task), не попадает в группы и здесь: иначе ретроактивный sweep слил бы
// ровно те записи, которые Save сознательно не сливает, и правило зависело бы от
// того, каким путём запись попала в БД.
func groupForDedup(all []memory.Memory) map[dedupGroupKey][]memory.Memory {
	groups := make(map[dedupGroupKey][]memory.Memory)
	for _, m := range all {
		if write.SkipDedup(m.Kind) {
			continue
		}
		key := dedupGroupKey{scope: m.Scope, kind: m.Kind}
		groups[key] = append(groups[key], m)
	}
	for key := range groups {
		members := groups[key]
		sort.Slice(members, func(a, b int) bool {
			if members[a].CreatedAt != members[b].CreatedAt {
				return members[a].CreatedAt < members[b].CreatedAt
			}
			return members[a].Id < members[b].Id
		})
	}
	return groups
}

// sweepGroup сравнивает попарно все живые записи одной (scope, kind)-группы.
// O(n²) внутри группы допустимо: объём живых записей одного scope+kind на
// практике мал (десятки-сотни, не тысячи), а sweep — фоновый нечастый джоб, не
// часть write/recall горячего пути.
//
// members[i] — текущий «чемпион»: каждая ещё не проигравшая запись с индексом
// > i сравнивается с ним по очереди (write.IsNearDuplicate); при совпадении
// проигравшая помечается processed (и больше не участвует ни в одном сравнении —
// ни как чемпион, ни как претендент), а чемпион при необходимости переключается
// на победителя пары. Так цепочка из нескольких попарно похожих записей сходится
// к одной выжившей записи за один проход.
func (d *DedupSweeper) sweepGroup(ctx context.Context, members []memory.Memory, result *DedupResult) {
	processed := make(map[string]bool, len(members))
	for i := range members {
		if processed[members[i].Id] {
			continue
		}
		champion := members[i]
		for j := i + 1; j < len(members); j++ {
			if processed[members[j].Id] {
				continue
			}
			if !write.IsNearDuplicate(champion.Content, members[j].Content) {
				continue
			}
			winner, loser := pickDedupWinner(champion, members[j])
			if err := d.writer.Forget(ctx, loser.Id, winner.Id); err != nil {
				log.Printf("maint: dedup sweep supersede %q -> %q: %v", loser.Id, winner.Id, err)
				continue
			}
			processed[loser.Id] = true
			result.Superseded++
			champion = winner
		}
	}
}

// pickDedupWinner решает, какая из двух near-duplicate записей выживает
// (DESIGN.md §11): побеждает запись с большим Importance, при равенстве —
// более новая (большее CreatedAt). Если совпадают оба признака, выживает a —
// детерминированный, но в остальном произвольный тай-брейк: записи в этом случае
// действительно неотличимы по доступным сигналам.
func pickDedupWinner(a, b memory.Memory) (winner, loser memory.Memory) {
	if b.Importance > a.Importance {
		return b, a
	}
	if b.Importance == a.Importance && b.CreatedAt > a.CreatedAt {
		return b, a
	}
	return a, b
}
