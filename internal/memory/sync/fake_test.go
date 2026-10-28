package sync

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"

	"agentmemory/internal/memory"
)

// Фейки портов исполняют ту же механику, что адаптеры (DESIGN.md §7.1):
// CAS на обеих сторонах, FK на links.dst, пометки неотправленного с seq,
// номер изменения общей базы на транзакцию. Решений LWW они не принимают.

func clone(m memory.Memory) memory.Memory {
	m.Tags = slices.Clone(m.Tags)
	m.Links = slices.Clone(m.Links)
	return m
}

type mark struct {
	seq    int64
	edited bool
}

// fakeLocal — локальная база инстанса.
type fakeLocal struct {
	mem      map[string]memory.Memory
	dirty    map[string]mark
	seq      int64
	tracked  map[string]bool // scope целей: их правки помечают «триггеры»
	states   map[string]State
	saves    int
	ahead    bool
	applyErr error
}

func newLocal(projects ...string) *fakeLocal {
	l := &fakeLocal{
		mem:     map[string]memory.Memory{},
		dirty:   map[string]mark{},
		tracked: map[string]bool{},
		states:  map[string]State{},
	}
	for _, p := range projects {
		l.tracked[Scope(p)] = true
	}
	return l
}

// save — запись через write path: триггер помечает правку записи scope цели.
func (l *fakeLocal) save(ms ...memory.Memory) {
	for _, m := range ms {
		l.mem[m.Id] = clone(m)
		if l.tracked[m.Scope] {
			l.seq++
			l.dirty[m.Id] = mark{seq: l.seq, edited: true}
		}
	}
}

func (l *fakeLocal) pending(id string) Pending {
	d := l.dirty[id]
	return Pending{Memory: clone(l.mem[id]), Seq: d.seq, Edited: d.edited}
}

func (l *fakeLocal) State(_ context.Context, project string) (State, error) {
	return l.states[project], nil
}

func (l *fakeLocal) SaveState(ctx context.Context, project string, st State) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.saves++
	l.states[project] = st
	return nil
}

func (l *fakeLocal) SchemaAhead(context.Context) (bool, error) { return l.ahead, nil }

func (l *fakeLocal) Mark(_ context.Context, scope string, kinds []memory.Kind, ids []string) error {
	if ids == nil {
		for id := range l.mem {
			ids = append(ids, id)
		}
		sort.Strings(ids)
	}
	for _, id := range ids {
		if m, ok := l.mem[id]; ok && m.Scope == scope && slices.Contains(kinds, m.Kind) {
			l.seq++
			l.dirty[id] = mark{seq: l.seq, edited: l.dirty[id].edited}
		}
	}
	return nil
}

func (l *fakeLocal) Dirty(_ context.Context, scope string, kinds []memory.Kind, after int64, limit int) ([]Pending, error) {
	var out []Pending
	for id, d := range l.dirty {
		m := l.mem[id]
		if m.Scope != scope {
			continue
		}
		if !slices.Contains(kinds, m.Kind) {
			delete(l.dirty, id)
			continue
		}
		if d.seq > after {
			out = append(out, l.pending(id))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (l *fakeLocal) Versions(_ context.Context, ids []string) (map[string]Pending, error) {
	out := map[string]Pending{}
	for _, id := range ids {
		if _, ok := l.mem[id]; ok {
			out[id] = l.pending(id)
		}
	}
	return out, nil
}

func (l *fakeLocal) StampAuthor(_ context.Context, ids []string, name string) error {
	for _, id := range ids {
		m := l.mem[id]
		if m.Author == "" {
			m.Author = name
		}
		if m.UpdatedBy == "" {
			m.UpdatedBy = name
		}
		l.mem[id] = m
	}
	return nil
}

func (l *fakeLocal) Apply(_ context.Context, recs []Guarded) (applied, stale []string, err error) {
	if l.applyErr != nil {
		return nil, nil, l.applyErr
	}
	ok := map[string]memory.Memory{}
	for _, g := range recs {
		cur, have := l.mem[g.Memory.Id]
		if g.ExpectAt == 0 && have || g.ExpectAt != 0 && (!have || cur.UpdatedAt != g.ExpectAt || l.dirty[cur.Id].seq != g.ExpectSeq) {
			stale = append(stale, g.Memory.Id)
			continue
		}
		m := clone(g.Memory)
		m.AccessedAt, m.AccessCount = cur.AccessedAt, cur.AccessCount
		ok[m.Id] = m
	}
	for _, m := range ok { // FK на links.dst: транзакция целиком откатывается
		for _, ln := range m.Links {
			if _, have := l.mem[ln.Dst]; !have {
				if _, inTx := ok[ln.Dst]; !inTx {
					return nil, nil, fmt.Errorf("FK: %s -> %s", m.Id, ln.Dst)
				}
			}
		}
	}
	for id, m := range ok {
		l.mem[id] = m
		delete(l.dirty, id)
		applied = append(applied, id)
	}
	return applied, stale, nil
}

func (l *fakeLocal) MarkPushed(_ context.Context, done []Pending) error {
	for _, p := range done {
		if d, ok := l.dirty[p.Memory.Id]; ok && d.seq == p.Seq {
			delete(l.dirty, p.Memory.Id)
		}
	}
	return nil
}

type hubRow struct {
	m   memory.Memory
	xid uint64
}

// fakeHub — общая база: каждая транзакция получает следующий номер изменения;
// параллельных транзакций нет, поэтому горизонт снимка равен xmax.
type fakeHub struct {
	rows       map[string]hubRow
	next       uint64 // xmax: номер следующей транзакции
	id         string
	version    int
	dialErr    error
	failAfter  int // >0 — Pull падает после стольких отданных страниц
	pages      int // страниц отдано всего
	delivered  int // записей отдано pull всего
	beforeVers func()
	beforePush func()
}

const hubSchema = 3

func newHub() *fakeHub {
	return &fakeHub{rows: map[string]hubRow{}, next: 1, id: "base-1", version: hubSchema}
}

// put — прямой писатель (driver=postgres): одна транзакция.
func (h *fakeHub) put(ms ...memory.Memory) {
	for _, m := range ms {
		h.rows[m.Id] = hubRow{m: clone(m), xid: h.next}
	}
	h.next++
}

func (h *fakeHub) get(id string) (memory.Memory, bool) {
	r, ok := h.rows[id]
	return r.m, ok
}

func (h *fakeHub) Dial(context.Context, string) (Shared, error) {
	if h.dialErr != nil {
		return nil, h.dialErr
	}
	return h, nil
}

func (h *fakeHub) Close() error { return nil }

func (h *fakeHub) Base(context.Context) (Base, error) {
	var maxXID uint64
	for _, r := range h.rows {
		maxXID = max(maxXID, r.xid)
	}
	return Base{ID: h.id, Version: h.version, XMax: h.next, MaxXID: maxXID}, nil
}

func (h *fakeHub) remote(r hubRow) Remote { return Remote{Memory: clone(r.m), XID: r.xid} }

// refs — записи базы, на которые ссылаются связи rows, кроме самих rows.
func (h *fakeHub) refs(rows []hubRow) []Remote {
	own := map[string]bool{}
	for _, r := range rows {
		own[r.m.Id] = true
	}
	var out []Remote
	for _, r := range rows {
		for _, ln := range r.m.Links {
			if d, ok := h.rows[ln.Dst]; ok && !own[ln.Dst] {
				own[ln.Dst] = true
				out = append(out, h.remote(d))
			}
		}
	}
	return out
}

var errNetwork = errors.New("соединение с общей базой разорвано")

func (h *fakeHub) Pull(ctx context.Context, scope string, kinds []memory.Kind, from Cursor, limit int, page func(Page) error) error {
	var rows []hubRow
	for _, r := range h.rows {
		if r.m.Scope == scope && slices.Contains(kinds, r.m.Kind) && from.less(Cursor{XID: r.xid, ID: r.m.Id}) {
			rows = append(rows, r)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		return Cursor{rows[i].xid, rows[i].m.Id}.less(Cursor{rows[j].xid, rows[j].m.Id})
	})
	sent := 0
	for i := 0; i < len(rows); i += limit {
		if h.failAfter > 0 && sent >= h.failAfter {
			return errNetwork
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk := rows[i:min(i+limit, len(rows))]
		pg := Page{Refs: h.refs(chunk), Next: Cursor{XID: h.next}}
		for _, r := range chunk {
			pg.Records = append(pg.Records, h.remote(r))
		}
		if last := chunk[len(chunk)-1]; i+limit < len(rows) {
			pg.Next = Cursor{XID: last.xid, ID: last.m.Id}
		}
		if err := page(pg); err != nil {
			return err
		}
		sent++
		h.pages++
		h.delivered += len(chunk)
	}
	return nil
}

func (h *fakeHub) Versions(_ context.Context, ids []string) (Snapshot, error) {
	if h.beforeVers != nil {
		h.beforeVers()
		h.beforeVers = nil
	}
	var rows []hubRow
	for _, id := range ids {
		if r, ok := h.rows[id]; ok {
			rows = append(rows, r)
		}
	}
	var snap Snapshot
	for _, r := range rows {
		snap.Records = append(snap.Records, h.remote(r))
	}
	asked := map[string]bool{}
	for _, id := range ids {
		asked[id] = true
	}
	for _, r := range h.refs(rows) {
		if !asked[r.Memory.Id] {
			snap.Refs = append(snap.Refs, r)
		}
	}
	return snap, nil
}

func (h *fakeHub) Push(_ context.Context, recs []Guarded) (written, stale []string, err error) {
	if h.beforePush != nil {
		h.beforePush()
		h.beforePush = nil
	}
	ok := map[string]memory.Memory{}
	for _, g := range recs {
		cur, have := h.rows[g.Memory.Id]
		if g.ExpectXID == 0 && have || g.ExpectXID != 0 && (!have || cur.xid != g.ExpectXID) {
			stale = append(stale, g.Memory.Id)
			continue
		}
		ok[g.Memory.Id] = clone(g.Memory)
	}
	for _, m := range ok {
		for _, ln := range m.Links {
			if _, have := h.rows[ln.Dst]; !have {
				if _, inTx := ok[ln.Dst]; !inTx {
					return nil, nil, fmt.Errorf("FK общей базы: %s -> %s", m.Id, ln.Dst)
				}
			}
		}
	}
	for _, g := range recs {
		if m, w := ok[g.Memory.Id]; w {
			h.rows[m.Id] = hubRow{m: m, xid: h.next}
			written = append(written, m.Id)
		}
	}
	if len(written) > 0 {
		h.next++
	}
	return written, stale, nil
}
