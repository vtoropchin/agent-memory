package maint

import (
	"context"
	"log"
	"time"
)

// decaySweepLimit — мягкая граница на число episodic-кандидатов, рассматриваемых
// Decay за один прогон Scheduler. Decay/consolidate — фоновые джобы, не часть
// горячего пути; лимит нужен только чтобы один прогон не растянулся на
// неограниченное время при большой базе — следующий тик доберёт остаток.
const decaySweepLimit = 500

// consolidateBatchLimit — лимит batchLimit, передаваемый в Consolidate из
// Scheduler.RunOnce (см. Consolidator.Consolidate: мягкая граница на суммарное
// число обрабатываемых за вызов episodic-записей, задаётся вызывающим кодом).
const consolidateBatchLimit = 50

// Scheduler запускает все maintenance-джобы (DESIGN.md §11) по расписанию:
// decay → dedup sweep → consolidation → досчёт векторов (reindex). Порядок
// внутри одного прогона фиксирован и продуман: decay/sweep сначала сокращают
// объём и шум эпизодов, которые иначе попали бы в дорогую консолидацию через
// Reasoner; reindex — последним, чтобы подхватить и то, что успели создать/
// изменить предыдущие джобы этого же прогона.
type Scheduler struct {
	decayer      *Decayer
	dedup        *DedupSweeper
	consolidator *Consolidator
	reindexer    *Reindexer

	interval         time.Duration // как часто запускать RunOnce (decay_interval)
	consolidateAfter time.Duration // возраст episodic, после которого он консолидируется (consolidate_after)
}

// NewScheduler создаёт Scheduler поверх уже сконструированных джобов. Порядок
// параметров d, ds, c, r соответствует порядку их выполнения в RunOnce (decay →
// sweep → consolidate → reindex). interval берётся из конфига
// maint.decay_interval, consolidateAfter — из maint.consolidate_after
// (DESIGN.md §13). consolidateAfter используется не только для Consolidate, но и
// как база порогов Decay — см. годок RunOnce.
func NewScheduler(d *Decayer, ds *DedupSweeper, c *Consolidator, r *Reindexer, interval, consolidateAfter time.Duration) *Scheduler {
	return &Scheduler{
		decayer:          d,
		dedup:            ds,
		consolidator:     c,
		reindexer:        r,
		interval:         interval,
		consolidateAfter: consolidateAfter,
	}
}

// RunOnce выполняет один прогон всех джобов последовательно:
//  1. Decay — demoteAfter = consolidateAfter (эпизод, ещё не доросший до
//     консолидации, начинает терять importance ровно к тому же моменту, когда
//     стал бы её кандидатом — единая точка отсчёта «эпизод устарел»);
//     archiveAfter = 4×consolidateAfter (архивируем только то, что не
//     консолидировалось и не обращалось к себе ещё вчетверо дольше — запас,
//     чтобы медленные/редкие джобы консолидации успели забрать запись в
//     semantic до архивации). limit — decaySweepLimit.
//  2. Sweep — чистка накопившихся почти-дублей (в т.ч. тех, что мог породить
//     предыдущий прогон Consolidate).
//  3. Consolidate(consolidateAfter, consolidateBatchLimit).
//  4. CheckAndReindex — досчёт векторов, в т.ч. для фактов, только что
//     созданных консолидацией.
//
// Ошибка отдельного джоба (DESIGN.md §15: деградация, а не падение) логируется
// в stderr, и выполнение продолжается со следующего джоба — один сбойный джоб
// не должен блокировать остальные. Результаты (или отметка об ошибке) всех
// четырёх джобов сводятся в одну итоговую строку лога.
func (s *Scheduler) RunOnce(ctx context.Context) {
	decayResult, decayErr := s.decayer.Decay(ctx, s.consolidateAfter, 4*s.consolidateAfter, decaySweepLimit)
	if decayErr != nil {
		log.Printf("maint: scheduler: decay: %v", decayErr)
	}

	dedupResult, dedupErr := s.dedup.Sweep(ctx)
	if dedupErr != nil {
		log.Printf("maint: scheduler: dedup sweep: %v", dedupErr)
	}

	consolidateResult, consolidateErr := s.consolidator.Consolidate(ctx, s.consolidateAfter, consolidateBatchLimit)
	if consolidateErr != nil {
		log.Printf("maint: scheduler: consolidate: %v", consolidateErr)
	}

	reindexResult, reindexErr := s.reindexer.CheckAndReindex(ctx)
	if reindexErr != nil {
		log.Printf("maint: scheduler: reindex: %v", reindexErr)
	}

	log.Printf(
		"maint: scheduler: прогон завершён: decay(scanned=%d demoted=%d archived=%d err=%v) "+
			"dedup(scanned=%d superseded=%d err=%v) "+
			"consolidate(scanned=%d created=%d merged=%d superseded=%d skipped=%d err=%v) "+
			"reindex(reindexed=%v embedded=%d failed=%d err=%v)",
		decayResult.Scanned, decayResult.Demoted, decayResult.Archived, decayErr,
		dedupResult.Scanned, dedupResult.Superseded, dedupErr,
		consolidateResult.Scanned, consolidateResult.Created, consolidateResult.Merged, consolidateResult.Superseded, consolidateResult.Skipped, consolidateErr,
		reindexResult.Reindexed, reindexResult.Embedded, reindexResult.Failed, reindexErr,
	)
}

// Run запускает RunOnce немедленно, затем повторяет его по тикеру interval, пока
// ctx не отменён. Паника внутри одного прогона (в т.ч. внутри отдельного джоба)
// перехватывается и логируется — она не должна ронять процесс memoryd, ради
// которого Scheduler работает в фоне (DESIGN.md §15): следующий тик просто
// попробует снова.
func (s *Scheduler) Run(ctx context.Context) {
	s.runOnceRecovered(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runOnceRecovered(ctx)
		}
	}
}

// runOnceRecovered вызывает RunOnce под defer/recover — см. годок Run.
func (s *Scheduler) runOnceRecovered(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("maint: scheduler: паника в прогоне джобов: %v", r)
		}
	}()
	s.RunOnce(ctx)
}
