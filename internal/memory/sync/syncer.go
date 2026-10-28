package sync

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"agentmemory/internal/memory"
)

// Размеры по умолчанию: страница pull и пачка push — порядка сотен записей,
// чтобы большой первичный обмен под таймаутом прогона продвигался, а не
// откатывался целиком (DESIGN.md §7.1).
const (
	defaultPageSize  = 200
	defaultBatchSize = 200
)

// Причины, по которым прогон не идёт (DESIGN.md §4.1, §7.1). Пишутся в
// State.LastError и видны в морде и doctor.
var (
	ErrLocalAhead    = errors.New("локальная база новее этой версии memo — обновите memo")
	ErrSchemaMissing = errors.New("в общей базе нет схемы — примените схему")
	ErrSchemaBehind  = errors.New("схема общей базы старее этой версии memo — примените схему")
	ErrSchemaAhead   = errors.New("схема общей базы новее этой версии memo — обновите memo")
	ErrRestored      = errors.New("общая база восстановлена из копии — выполните ротацию base_id")
)

// Syncer синхронизирует набор проекта с его общей базой: pull, затем push
// (DESIGN.md §7.1). Сам lease не берёт — это забота вызывающего (планировщик,
// CLI), Syncer только прогоняет одну цель.
type Syncer struct {
	local     Local
	dialer    Dialer
	schema    int // версия схемы общей базы, которую понимает бинарь (pgSchemaVersion)
	now       func() int64
	pageSize  int
	batchSize int
}

// Option настраивает Syncer при создании.
type Option func(*Syncer)

// WithNow подменяет источник времени (unix ts) — для детерминированных тестов.
func WithNow(now func() int64) Option {
	return func(s *Syncer) { s.now = now }
}

// WithLimits задаёт размер страницы pull и пачки push; значения <= 0 не меняют дефолт.
func WithLimits(page, batch int) Option {
	return func(s *Syncer) {
		if page > 0 {
			s.pageSize = page
		}
		if batch > 0 {
			s.batchSize = batch
		}
	}
}

// New собирает Syncer. schema — версия схемы общей базы, с которой бинарь
// умеет работать: синхронизация идёт только при точном совпадении.
func New(local Local, dialer Dialer, schema int, opts ...Option) *Syncer {
	s := &Syncer{
		local:     local,
		dialer:    dialer,
		schema:    schema,
		now:       func() int64 { return time.Now().Unix() },
		pageSize:  defaultPageSize,
		batchSize: defaultBatchSize,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Run — один прогон цели t от имени name (автор отправляемых записей; пусто —
// авторы не проставляются). Итог и ошибка пишутся в State цели, кроме случая
// ErrLocalAhead: старый бинарь не пишет в схему, которую обновил более новый.
// Result отражает сделанное и при ошибке — прогресс прошедших страниц и пачек
// сохранён.
func (s *Syncer) Run(ctx context.Context, t Target, name string) (Result, error) {
	res := Result{Project: t.Project}
	ahead, err := s.local.SchemaAhead(ctx)
	if err != nil {
		return res, fmt.Errorf("версия локальной базы: %w", err)
	}
	if ahead {
		return res, ErrLocalAhead
	}
	st, err := s.local.State(ctx, t.Project)
	if err != nil {
		return res, fmt.Errorf("состояние синхронизации: %w", err)
	}
	st.LastRunAt = s.now()

	p := &pass{s: s, t: t, name: name, scope: Scope(t.Project), st: &st, res: &res}
	runErr := p.run(ctx)

	st.LastPushed, st.LastPulled, st.LastConflicts = res.Pushed, res.Pulled, res.Conflicts
	st.ConflictsTotal += res.Conflicts
	if runErr != nil {
		st.LastError = runErr.Error()
	} else {
		st.LastError, st.LastOKAt = "", st.LastRunAt
	}
	// Итог пишется и после отмены ctx (таймаут прогона): иначе ошибка
	// таймаута не дошла бы до статуса.
	if err := s.local.SaveState(context.WithoutCancel(ctx), t.Project, st); err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("сохранение состояния синхронизации: %w", err))
	}
	return res, runErr
}

// pass — состояние одного прогона.
type pass struct {
	s     *Syncer
	sh    Shared
	t     Target
	name  string
	scope string
	st    *State
	start Cursor // курсор pull на начало прогона: версии не меньше него клиент ещё не видел
	res   *Result
}

func (p *pass) run(ctx context.Context) error {
	if !p.t.Push {
		p.st.PushMarked = false
	}
	if !p.t.Push && !p.t.Pull {
		return nil
	}
	sh, err := p.s.dialer.Dial(ctx, p.t.Project)
	if err != nil {
		return fmt.Errorf("подключение к общей базе: %w", err)
	}
	defer sh.Close()
	p.sh = sh

	base, err := sh.Base(ctx)
	if err != nil {
		return fmt.Errorf("состояние общей базы: %w", err)
	}
	switch {
	case base.Version == 0:
		return ErrSchemaMissing
	case base.Version < p.s.schema:
		return ErrSchemaBehind
	case base.Version > p.s.schema:
		return ErrSchemaAhead
	}
	if base.ID != p.st.BaseID {
		// Другая база (сменили цель, схему применили заново, ротация после
		// восстановления): курсор с начала, набор сверяется целиком.
		p.st.BaseID, p.st.Cursor, p.st.PushMarked = base.ID, Cursor{}, false
	} else if p.st.Cursor.XID > base.XMax || base.MaxXID > base.XMax {
		// Та же база, а номера изменений «из будущего»: её восстановили из
		// копии, xid переиспользуются, и курсор пропустил бы строки. Молча
		// сбрасывать курсор нельзя — нужна явная ротация (§4.1).
		return ErrRestored
	}
	p.start = p.st.Cursor

	if p.t.Push && !p.st.PushMarked {
		if err := p.s.local.Mark(ctx, p.scope, syncKinds, nil); err != nil {
			return fmt.Errorf("разметка набора: %w", err)
		}
		p.st.PushMarked = true
	}
	if p.t.Pull {
		if err := p.pull(ctx); err != nil {
			return fmt.Errorf("pull: %w", err)
		}
	}
	if p.t.Push {
		if err := p.push(ctx); err != nil {
			return fmt.Errorf("push: %w", err)
		}
	}
	return nil
}

// pull применяет страницы изменений общей базы; курсор сохраняется после
// каждой применённой страницы и только после неё.
func (p *pass) pull(ctx context.Context) error {
	return p.sh.Pull(ctx, p.scope, syncKinds, p.st.Cursor, p.s.pageSize, func(pg Page) error {
		if err := p.receive(ctx, pg.Records, pg.Refs); err != nil {
			return err
		}
		if p.st.Cursor.less(pg.Next) { // курсор не убывает (§4.1)
			p.st.Cursor = pg.Next
		}
		if err := p.s.local.SaveState(ctx, p.t.Project, *p.st); err != nil {
			return fmt.Errorf("сохранение курсора: %w", err)
		}
		return nil
	})
}

// receive применяет локально версии общей базы records (страница pull или
// проигравшие LWW при push). refs — записи общей базы, на которые ссылаются
// связи records: из набора они применяются заглушкой версии — без связей
// общей базы, со своими локальными (адресаты их связей могут лежать на
// дальних страницах, а полные связи придут с собственной страницей записи);
// вне набора — служат только классификации рёбер.
func (p *pass) receive(ctx context.Context, records, refs []Remote) error {
	idx := map[string]memory.Memory{} // всё известное о записях: набор или нет
	isRec := map[string]bool{}
	var recs, stubs []Remote
	for _, r := range records {
		idx[r.Memory.Id] = r.Memory
		if inSet(r.Memory, p.scope) && !isRec[r.Memory.Id] {
			isRec[r.Memory.Id] = true
			recs = append(recs, r)
		}
	}
	isStub := map[string]bool{}
	for _, r := range refs {
		id := r.Memory.Id
		if _, ok := idx[id]; !ok {
			idx[id] = r.Memory
		}
		if inSet(r.Memory, p.scope) && !isRec[id] && !isStub[id] {
			isStub[id] = true
			stubs = append(stubs, r)
		}
	}
	if len(recs)+len(stubs) == 0 {
		return nil
	}
	ids := make([]string, 0, len(recs)+len(stubs))
	for _, r := range slices.Concat(recs, stubs) {
		ids = append(ids, r.Memory.Id)
	}
	loc, err := p.localVersions(ctx, ids, recs, idx)
	if err != nil {
		return err
	}

	in := func(dst string) bool { m, ok := idx[dst]; return ok && inSet(m, p.scope) }
	out := func(dst string) bool { return !in(dst) }
	var batch []Guarded
	var toMark []string
	conflict := map[string]bool{}
	dropped := map[string]int{}
	decide := func(r Remote, stub bool) {
		id := r.Memory.Id
		l, have := loc[id]
		m := r.Memory
		if stub {
			m.Links = l.Memory.Links
		} else {
			// Связи = связи общей базы внутри набора ∪ локальные вне набора:
			// pull не стирает локальные рёбра на эпизоды и user:*.
			m.Links = mergeLinks(r.Memory.Links, in, l.Memory.Links, out)
			for _, ln := range r.Memory.Links {
				if _, ok := idx[ln.Dst]; !ok {
					dropped[id]++ // адресат не нашёлся ни в снимке, ни локально — ребро не применить (FK)
				}
			}
		}
		if have && same(m, l.Memory) {
			return
		}
		if have && l.Memory.UpdatedAt > m.UpdatedAt {
			// Локальная новее — её отправит push. Обычно она уже помечена;
			// если нет (общую версию записал прямой писатель с отстающими
			// часами) — пометить.
			if l.Seq == 0 {
				toMark = append(toMark, id)
			}
			return
		}
		// Общая новее или ничья (побеждает общая — правило сходится на всех машинах).
		g := Guarded{Memory: m}
		if have {
			g.ExpectAt, g.ExpectSeq = l.Memory.UpdatedAt, l.Seq
			conflict[id] = l.Edited && l.Seq != 0 // отброшена локальная правка; разметка конфликтом не считается
		}
		batch = append(batch, g)
	}
	for _, r := range recs {
		decide(r, false)
	}
	for _, r := range stubs {
		decide(r, true)
	}

	if len(toMark) > 0 {
		if err := p.s.local.Mark(ctx, p.scope, syncKinds, toMark); err != nil {
			return fmt.Errorf("пометка локальных версий: %w", err)
		}
	}
	if len(batch) == 0 {
		return nil
	}
	applied, stale, err := p.s.local.Apply(ctx, batch)
	if err != nil {
		return fmt.Errorf("применение локально: %w", err)
	}
	p.res.Deferred += len(stale)
	for _, id := range applied {
		if isRec[id] {
			p.res.Pulled++
			p.res.DroppedLinks += dropped[id]
		}
		if conflict[id] {
			p.res.Conflicts++
		}
	}
	return nil
}

// localVersions читает локальные версии ids и дополняет idx ими и адресатами
// связей — локальных и связей recs, которых нет в снимке общей базы (сосед по
// пачке push в Refs не попадает): иначе ребро на запись, которая есть с обеих
// сторон, считалось бы ребром без адресата.
func (p *pass) localVersions(ctx context.Context, ids []string, recs []Remote, idx map[string]memory.Memory) (map[string]Pending, error) {
	loc, err := p.s.local.Versions(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("локальные версии: %w", err)
	}
	var dsts []string
	need := func(links []memory.Link) {
		for _, ln := range links {
			if _, ok := idx[ln.Dst]; !ok && !slices.Contains(dsts, ln.Dst) {
				dsts = append(dsts, ln.Dst)
			}
		}
	}
	for _, l := range loc {
		if _, ok := idx[l.Memory.Id]; !ok {
			idx[l.Memory.Id] = l.Memory
		}
		need(l.Memory.Links)
	}
	for _, r := range recs {
		need(r.Memory.Links)
	}
	if len(dsts) == 0 {
		return loc, nil
	}
	more, err := p.s.local.Versions(ctx, dsts)
	if err != nil {
		return nil, fmt.Errorf("локальные версии адресатов связей: %w", err)
	}
	for id, l := range more {
		idx[id] = l.Memory
	}
	return loc, nil
}

// push отправляет помеченное пачками; каждая пачка — своя транзакция общей
// стороны, поэтому большой первичный push продвигается под таймаутом.
func (p *pass) push(ctx context.Context) error {
	var after int64
	for {
		pend, err := p.s.local.Dirty(ctx, p.scope, syncKinds, after, p.s.batchSize)
		if err != nil {
			return fmt.Errorf("неотправленное: %w", err)
		}
		if len(pend) == 0 {
			return nil
		}
		for _, x := range pend {
			after = max(after, x.Seq)
		}
		if err := p.pushBatch(ctx, pend); err != nil {
			return err
		}
		if err := p.s.local.SaveState(ctx, p.t.Project, *p.st); err != nil {
			return fmt.Errorf("сохранение состояния: %w", err)
		}
	}
}

func (p *pass) pushBatch(ctx context.Context, pend []Pending) error {
	loc := map[string]Pending{} // локальные версии: пачка и адресаты связей
	idx := map[string]memory.Memory{}
	inBatch := map[string]bool{}
	var order []string
	for _, x := range pend {
		if id := x.Memory.Id; inSet(x.Memory, p.scope) && !inBatch[id] {
			inBatch[id] = true
			order = append(order, id)
			loc[id], idx[id] = x, x.Memory
		}
	}

	// Версии общей базы и замыкание пачки по FK общей базы: адресат локальной
	// связи из набора, которого в общей базе нет, добавляется в пачку из
	// локальных данных (пачка может немного превысить лимит).
	remote := map[string]Remote{}
	var refs []Remote
	queried, looked := map[string]bool{}, map[string]bool{}
	queue, grown := slices.Clone(order), slices.Clone(order)
	for len(queue) > 0 {
		snap, err := p.sh.Versions(ctx, queue)
		if err != nil {
			return fmt.Errorf("версии общей базы: %w", err)
		}
		for _, id := range queue {
			queried[id] = true
		}
		for _, r := range snap.Records {
			remote[r.Memory.Id] = r
			if _, ok := idx[r.Memory.Id]; !ok {
				idx[r.Memory.Id] = r.Memory
			}
		}
		for _, r := range snap.Refs {
			refs = append(refs, r)
			if _, ok := idx[r.Memory.Id]; !ok {
				idx[r.Memory.Id] = r.Memory
			}
		}
		for _, id := range queue {
			if _, ok := remote[id]; !ok && !inBatch[id] {
				inBatch[id] = true
				order = append(order, id)
				grown = append(grown, id)
			}
		}

		var dsts []string
		for _, id := range grown {
			for _, ln := range loc[id].Memory.Links {
				if _, ok := loc[ln.Dst]; !ok && !looked[ln.Dst] {
					looked[ln.Dst] = true
					dsts = append(dsts, ln.Dst)
				}
			}
		}
		if len(dsts) > 0 {
			more, err := p.s.local.Versions(ctx, dsts)
			if err != nil {
				return fmt.Errorf("локальные версии адресатов связей: %w", err)
			}
			for id, x := range more {
				loc[id], idx[id] = x, x.Memory
			}
		}
		queue = nil
		for _, id := range grown {
			for _, ln := range loc[id].Memory.Links {
				d := ln.Dst
				if x, ok := loc[d]; ok && inSet(x.Memory, p.scope) && !inBatch[d] && !queried[d] && !slices.Contains(queue, d) {
					queue = append(queue, d)
				}
			}
		}
		grown = nil
	}

	in := func(dst string) bool { m, ok := idx[dst]; return ok && inSet(m, p.scope) }
	out := func(dst string) bool { return !in(dst) }
	var recs []Guarded
	var done []Pending
	var losers []Remote
	overwrite := map[string]bool{}
	for _, id := range order {
		l := loc[id]
		r, have := remote[id]
		// Связи в общую базу = локальные внутри набора ∪ общие вне набора:
		// рёбра на эпизоды не уходят, рёбра прямых писателей не стираются.
		o := l.Memory
		o.Links = mergeLinks(l.Memory.Links, in, r.Memory.Links, out)
		switch {
		case have && same(o, r.Memory):
			done = append(done, l)
		case have && r.Memory.UpdatedAt >= l.Memory.UpdatedAt:
			// Общая новее или ничья. При включённом «Загружать» она
			// применяется сразу: строку, уже прочитанную pull, он больше не
			// принесёт. При выключенном локальная не трогается, а её правка
			// отброшена — конфликт.
			if p.t.Pull {
				losers = append(losers, r)
			} else {
				done = append(done, l)
				if l.Edited && l.Seq != 0 {
					p.res.Conflicts++
				}
			}
		default:
			g := Guarded{Memory: o}
			if have {
				g.ExpectXID = r.XID
				// Перезаписываем чужую версию, которой клиент не видел к началу
				// прогона: изменение другой стороны отброшено.
				overwrite[id] = r.Memory.UpdatedBy != p.name && r.XID >= p.start.XID
			}
			recs = append(recs, g)
		}
	}

	if err := p.stamp(ctx, recs); err != nil {
		return err
	}
	if len(recs) > 0 {
		// Единый порядок блокировок у параллельных писателей — без взаимоблокировок.
		slices.SortFunc(recs, func(a, b Guarded) int { return strings.Compare(a.Memory.Id, b.Memory.Id) })
		written, stale, err := p.sh.Push(ctx, recs)
		if err != nil {
			return fmt.Errorf("отправка: %w", err)
		}
		p.res.Pushed += len(written)
		p.res.Deferred += len(stale)
		for _, id := range written {
			done = append(done, loc[id])
			if overwrite[id] {
				p.res.Conflicts++
			}
		}
	}
	if len(done) > 0 {
		if err := p.s.local.MarkPushed(ctx, done); err != nil {
			return fmt.Errorf("снятие пометок: %w", err)
		}
	}
	if len(losers) == 0 {
		return nil
	}
	// Проигравшим нужны адресаты их связей из общей базы (FK локально).
	need := map[string]bool{}
	for _, r := range losers {
		for _, ln := range r.Memory.Links {
			need[ln.Dst] = true
		}
	}
	var lref []Remote
	for _, r := range refs {
		if need[r.Memory.Id] {
			lref = append(lref, r)
		}
	}
	return p.receive(ctx, losers, lref)
}

// stamp проставляет автора записям пачки, которых в общей базе ещё нет: в
// исходящей копии и локально. Записи, уже лежащие в общей базе, не
// штампуются — иначе чужая запись без автора получила бы чужое авторство.
func (p *pass) stamp(ctx context.Context, recs []Guarded) error {
	if p.name == "" {
		return nil
	}
	var ids []string
	for i := range recs {
		m := &recs[i].Memory
		if recs[i].ExpectXID != 0 || (m.Author != "" && m.UpdatedBy != "") {
			continue
		}
		if m.Author == "" {
			m.Author = p.name
		}
		if m.UpdatedBy == "" {
			m.UpdatedBy = p.name
		}
		ids = append(ids, m.Id)
	}
	if len(ids) == 0 {
		return nil
	}
	if err := p.s.local.StampAuthor(ctx, ids, p.name); err != nil {
		return fmt.Errorf("автор записей: %w", err)
	}
	p.res.Stamped += len(ids)
	return nil
}
