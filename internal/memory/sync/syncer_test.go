package sync

import (
	"context"
	"errors"
	"slices"
	"testing"

	"agentmemory/internal/memory"
)

const project = "X"

var both = Target{Project: project, Push: true, Pull: true}

// dev — разработчик: своя локальная база и свой Syncer к общей базе hub.
type dev struct {
	name  string
	local *fakeLocal
	s     *Syncer
}

func newDev(name string, hub *fakeHub, opts ...Option) *dev {
	l := newLocal(project)
	opts = append([]Option{WithNow(func() int64 { return 1000 })}, opts...)
	return &dev{name: name, local: l, s: New(l, hub, hubSchema, opts...)}
}

func (d *dev) run(t *testing.T, tg Target) Result {
	t.Helper()
	res, err := d.s.Run(context.Background(), tg, d.name)
	if err != nil {
		t.Fatalf("%s: Run: %v", d.name, err)
	}
	return res
}

// edit — правка записи разработчиком через write path: новый updated_at и updated_by.
func (d *dev) edit(id string, at int64, change func(*memory.Memory)) {
	m := clone(d.local.mem[id])
	change(&m)
	m.UpdatedAt, m.UpdatedBy = at, d.name
	d.local.save(m)
}

func rec(id string, kind memory.Kind, at int64, content string, links ...memory.Link) memory.Memory {
	return memory.Memory{
		Id: id, Kind: kind, Scope: Scope(project), Content: content, Importance: 3,
		CreatedAt: at, UpdatedAt: at, Links: links,
	}
}

func counts(r Result) [6]int {
	return [6]int{r.Pushed, r.Pulled, r.Conflicts, r.Stamped, r.Deferred, r.DroppedLinks}
}

func wantCounts(t *testing.T, who string, got Result, pushed, pulled, conflicts int) {
	t.Helper()
	if got.Pushed != pushed || got.Pulled != pulled || got.Conflicts != conflicts {
		t.Fatalf("%s: отправлено/получено/конфликтов = %d/%d/%d, want %d/%d/%d (%+v)",
			who, got.Pushed, got.Pulled, got.Conflicts, pushed, pulled, conflicts, got)
	}
}

func wantSame(t *testing.T, a, b memory.Memory) {
	t.Helper()
	if !same(a, b) {
		t.Fatalf("версии расходятся:\n%+v\n%+v", a, b)
	}
}

// synced — две машины с общим набором из одной записи a1 (автор Аня).
func synced(t *testing.T) (*fakeHub, *dev, *dev) {
	t.Helper()
	hub := newHub()
	a, b := newDev("Аня", hub), newDev("Борис", hub)
	a.local.save(rec("a1", memory.KindSemantic, 100, "факт"))
	a.run(t, both)
	b.run(t, both)
	return hub, a, b
}

func TestInsertBothWays(t *testing.T) {
	hub := newHub()
	a, b := newDev("Аня", hub), newDev("Борис", hub)
	a.local.save(rec("a1", memory.KindSemantic, 100, "факт"), rec("a2", memory.KindTask, 101, "задача"))

	res := a.run(t, both)
	wantCounts(t, "Аня", res, 2, 0, 0)
	if res.Stamped != 2 {
		t.Fatalf("Stamped = %d, want 2", res.Stamped)
	}
	wantCounts(t, "Борис", b.run(t, both), 0, 2, 0)
	for _, id := range []string{"a1", "a2"} {
		h, _ := hub.get(id)
		wantSame(t, a.local.mem[id], h)
		wantSame(t, b.local.mem[id], h)
		if h.Author != "Аня" {
			t.Fatalf("%s: автор %q, want Аня", id, h.Author)
		}
	}

	b.local.save(rec("b1", memory.KindProcedural, 200, "рецепт"))
	wantCounts(t, "Борис", b.run(t, both), 1, 0, 0)
	wantCounts(t, "Аня", a.run(t, both), 0, 1, 0)
	if a.local.mem["b1"].Author != "Борис" {
		t.Fatalf("b1 у Ани: автор %q, want Борис", a.local.mem["b1"].Author)
	}
}

func TestEditBothWays(t *testing.T) {
	_, a, b := synced(t)

	a.edit("a1", 200, func(m *memory.Memory) { m.Content = "правка Ани"; m.Tags = []string{"x"} })
	wantCounts(t, "Аня", a.run(t, both), 1, 0, 0)
	wantCounts(t, "Борис", b.run(t, both), 0, 1, 0)
	if m := b.local.mem["a1"]; m.Content != "правка Ани" || m.UpdatedBy != "Аня" || m.Author != "Аня" || !slices.Equal(m.Tags, []string{"x"}) {
		t.Fatalf("у Бориса: %+v", m)
	}

	b.edit("a1", 300, func(m *memory.Memory) { m.Content = "правка Бориса" })
	wantCounts(t, "Борис", b.run(t, both), 1, 0, 0)
	wantCounts(t, "Аня", a.run(t, both), 0, 1, 0)
	if m := a.local.mem["a1"]; m.Content != "правка Бориса" || m.UpdatedBy != "Борис" || m.Author != "Аня" {
		t.Fatalf("у Ани: %+v", m)
	}
}

func TestForgetPropagates(t *testing.T) {
	_, a, b := synced(t)
	fix := "fix1"
	a.edit("a1", 200, func(m *memory.Memory) { m.SupersededBy = &fix })
	a.run(t, both)
	wantCounts(t, "Борис", b.run(t, both), 0, 1, 0)
	if got := deref(b.local.mem["a1"].SupersededBy); got != fix {
		t.Fatalf("тумстоун не дошёл: superseded_by = %q", got)
	}
}

func TestIdempotent(t *testing.T) {
	_, a, b := synced(t)
	a.edit("a1", 200, func(m *memory.Memory) { m.Content = "правка" })
	b.local.save(rec("b1", memory.KindDebt, 150, "долг"))
	a.run(t, both)
	b.run(t, both)
	a.run(t, both)
	for _, d := range []*dev{a, b, a, b} {
		if got := counts(d.run(t, both)); got != [6]int{} {
			t.Fatalf("%s: повторный прогон без изменений = %v, want нули", d.name, got)
		}
	}
	if len(a.local.dirty)+len(b.local.dirty) != 0 {
		t.Fatalf("остались пометки: %v %v", a.local.dirty, b.local.dirty)
	}
}

func TestConflictNewerWins(t *testing.T) {
	hub, a, b := synced(t)

	// Обе правки сделаны без знания друг о друге; у Ани новее.
	a.edit("a1", 200, func(m *memory.Memory) { m.Content = "Аня" })
	b.edit("a1", 150, func(m *memory.Memory) { m.Content = "Борис" })
	a.run(t, both)
	wantCounts(t, "Борис", b.run(t, both), 0, 1, 1)
	if b.local.mem["a1"].Content != "Аня" {
		t.Fatalf("у Бориса %q, want Аня", b.local.mem["a1"].Content)
	}

	// Теперь новее у Бориса: его push перезаписывает версию Ани, которой он не видел.
	a.edit("a1", 250, func(m *memory.Memory) { m.Content = "Аня-2" })
	b.edit("a1", 300, func(m *memory.Memory) { m.Content = "Борис-2" })
	a.run(t, both)
	wantCounts(t, "Борис", b.run(t, both), 1, 0, 1)
	wantCounts(t, "Аня", a.run(t, both), 0, 1, 0)
	if h, _ := hub.get("a1"); h.Content != "Борис-2" || a.local.mem["a1"].Content != "Борис-2" {
		t.Fatalf("победила не новая версия: общая %q, у Ани %q", h.Content, a.local.mem["a1"].Content)
	}
	if st := b.local.states[project]; st.ConflictsTotal != 2 || st.LastConflicts != 1 {
		t.Fatalf("счётчики конфликтов Бориса: %+v", st)
	}
}

func TestTieSharedWins(t *testing.T) {
	_, a, b := synced(t)
	a.edit("a1", 200, func(m *memory.Memory) { m.Content = "Аня" })
	b.edit("a1", 200, func(m *memory.Memory) { m.Content = "Борис" })
	a.run(t, both)
	wantCounts(t, "Борис", b.run(t, both), 0, 1, 1)
	if b.local.mem["a1"].Content != "Аня" {
		t.Fatalf("ничья: у Бориса %q, want версия общей базы", b.local.mem["a1"].Content)
	}
}

func TestOnlySyncSetLeaves(t *testing.T) {
	hub := newHub()
	a := newDev("Аня", hub)
	ep := rec("e1", memory.KindEpisodic, 100, "эпизод")
	user := rec("u1", memory.KindSemantic, 100, "общий факт")
	user.Scope = "user:*"
	other := rec("y1", memory.KindSemantic, 100, "чужой проект")
	other.Scope = "project:Y"
	a.local.tracked["project:Y"] = true // цель Y есть, но прогон — по X
	a.local.save(ep, user, other, rec("f1", memory.KindSemantic, 100, "факт"))

	wantCounts(t, "Аня", a.run(t, both), 1, 0, 0)
	if len(hub.rows) != 1 {
		t.Fatalf("в общей базе %d записей, want только f1", len(hub.rows))
	}
	if _, ok := hub.get("f1"); !ok {
		t.Fatal("f1 не отправлена")
	}

	// В общей базе episodic проекта (прямой писатель) — pull его не приносит.
	hub.put(rec("e2", memory.KindEpisodic, 100, "чужой эпизод"))
	a.run(t, both)
	if _, ok := a.local.mem["e2"]; ok {
		t.Fatal("episodic пришёл из общей базы")
	}
}

func TestLinksOutsideSet(t *testing.T) {
	hub := newHub()
	a, b := newDev("Аня", hub), newDev("Борис", hub)
	toEp := memory.Link{Dst: "e1", Rel: memory.RelCauses}
	toFact := memory.Link{Dst: "f1"}
	a.local.save(
		rec("e1", memory.KindEpisodic, 100, "эпизод"),
		rec("f1", memory.KindSemantic, 100, "факт"),
		rec("t1", memory.KindTask, 101, "задача", toEp, toFact),
	)
	a.run(t, both)
	if h, _ := hub.get("t1"); !slices.Equal(h.Links, []memory.Link{toFact}) {
		t.Fatalf("в общую базу ушли связи %v, want только на f1", h.Links)
	}

	// Прямой писатель связал t1 с записью вне набора в общей базе.
	user := rec("u1", memory.KindSemantic, 100, "общее")
	user.Scope = "user:*"
	toUser := memory.Link{Dst: "u1"}
	h, _ := hub.get("t1")
	h.Links = append(h.Links, toUser)
	hub.put(user)
	hub.put(h)

	b.run(t, both)
	b.edit("t1", 200, func(m *memory.Memory) { m.Content = "правка Бориса" })
	b.run(t, both)
	if h, _ := hub.get("t1"); !sameSet(h.Links, []memory.Link{toFact, toUser}, linkKey) {
		t.Fatalf("push стёр связь прямого писателя: %v", h.Links)
	}

	wantCounts(t, "Аня", a.run(t, both), 0, 1, 0)
	m := a.local.mem["t1"]
	if m.Content != "правка Бориса" || !sameSet(m.Links, []memory.Link{toEp, toFact}, linkKey) {
		t.Fatalf("у Ани после pull: %q, связи %v — want связь на эпизод сохранена", m.Content, m.Links)
	}
	if _, ok := b.local.mem["u1"]; ok {
		t.Fatal("запись user:* пришла из общей базы")
	}
}

func TestAuthorStamping(t *testing.T) {
	hub := newHub()
	a := newDev("Аня", hub)
	old := rec("r0", memory.KindSemantic, 10, "старая общая без автора")
	hub.put(old)
	mine := old
	mine.UpdatedAt, mine.Content = 20, "моя правка, автора нет"
	a.local.save(mine, rec("r1", memory.KindSemantic, 10, "старая локальная"))

	res := a.run(t, Target{Project: project, Push: true})
	if res.Stamped != 1 || res.Pushed != 2 {
		t.Fatalf("Stamped/Pushed = %d/%d, want 1/2", res.Stamped, res.Pushed)
	}
	if h, _ := hub.get("r1"); h.Author != "Аня" || h.UpdatedBy != "Аня" || a.local.mem["r1"].Author != "Аня" {
		t.Fatalf("r1 без автора: общая %q/%q, локальная %q", h.Author, h.UpdatedBy, a.local.mem["r1"].Author)
	}
	if a.local.mem["r1"].UpdatedAt != 10 {
		t.Fatal("штамп автора сдвинул updated_at")
	}
	if h, _ := hub.get("r0"); h.Author != "" {
		t.Fatalf("уже лежащая в общей базе запись получила автора %q", h.Author)
	}
}

func TestPullPagesAndCursor(t *testing.T) {
	hub := newHub()
	for _, id := range []string{"p1", "p2", "p3", "p4", "p5"} {
		hub.put(rec(id, memory.KindSemantic, 100, id))
	}
	b := newDev("Борис", hub, WithLimits(2, 0))
	wantCounts(t, "Борис", b.run(t, both), 0, 5, 0)
	if hub.pages != 3 {
		t.Fatalf("страниц %d, want 3", hub.pages)
	}
	if got := b.local.states[project].Cursor; got != (Cursor{XID: hub.next}) {
		t.Fatalf("курсор %+v, want {%d ''}", got, hub.next)
	}

	hub.put(rec("p6", memory.KindSemantic, 100, "p6"))
	hub.delivered = 0
	wantCounts(t, "Борис", b.run(t, both), 0, 1, 0)
	if hub.delivered != 1 {
		t.Fatalf("pull перечитал %d записей, want только новую", hub.delivered)
	}
}

func TestPullFailureKeepsCursor(t *testing.T) {
	hub := newHub()
	for _, id := range []string{"p1", "p2", "p3", "p4", "p5"} {
		hub.put(rec(id, memory.KindSemantic, 100, id))
	}
	b := newDev("Борис", hub, WithLimits(2, 0))

	hub.failAfter = 1
	res, err := b.s.Run(context.Background(), both, b.name)
	if !errors.Is(err, errNetwork) {
		t.Fatalf("err = %v, want сбой сети", err)
	}
	st := b.local.states[project]
	if res.Pulled != 2 || st.Cursor != (Cursor{XID: 2, ID: "p2"}) || st.LastError == "" || st.LastOKAt != 0 {
		t.Fatalf("после сбоя: Pulled %d, состояние %+v", res.Pulled, st)
	}

	hub.failAfter = 0
	wantCounts(t, "Борис", b.run(t, both), 0, 3, 0)
	if st := b.local.states[project]; st.LastError != "" || st.LastOKAt != 1000 || len(b.local.mem) != 5 {
		t.Fatalf("повтор не довёл до конца: %+v, записей %d", st, len(b.local.mem))
	}
}

func TestApplyFailureKeepsCursor(t *testing.T) {
	hub := newHub()
	hub.put(rec("p1", memory.KindSemantic, 100, "p1"))
	b := newDev("Борис", hub)
	b.local.applyErr = errors.New("диск полон")
	if _, err := b.s.Run(context.Background(), both, b.name); err == nil {
		t.Fatal("сбой применения не вернул ошибку")
	}
	if st := b.local.states[project]; st.Cursor != (Cursor{}) {
		t.Fatalf("курсор сдвинут без применения: %+v", st.Cursor)
	}
}

func TestPushPullToggles(t *testing.T) {
	hub := newHub()
	hub.put(rec("h1", memory.KindSemantic, 100, "общая"))
	a, b := newDev("Аня", hub), newDev("Борис", hub)

	a.local.save(rec("a1", memory.KindSemantic, 100, "Аня"))
	wantCounts(t, "Аня", a.run(t, Target{Project: project, Push: true}), 1, 0, 0)
	if _, ok := a.local.mem["h1"]; ok {
		t.Fatal("при выключенном «Загружать» общая запись пришла")
	}

	b.local.save(rec("b1", memory.KindSemantic, 100, "Борис"))
	wantCounts(t, "Борис", b.run(t, Target{Project: project, Pull: true}), 0, 2, 0)
	if _, ok := hub.get("b1"); ok {
		t.Fatal("при выключенном «Отправлять» локальная запись ушла")
	}
	if b.local.states[project].PushMarked {
		t.Fatal("push_marked при выключенном «Отправлять»")
	}

	b.local.states[project] = State{}
	hub.dialErr = errors.New("не должно подключаться")
	if res := b.run(t, Target{Project: project}); counts(res) != [6]int{} {
		t.Fatalf("оба тумблера выключены: %+v", res)
	}
}

func TestPullOffLoserIsConflict(t *testing.T) {
	_, a, b := synced(t)
	b.edit("a1", 300, func(m *memory.Memory) { m.Content = "Борис" })
	b.run(t, both)
	a.edit("a1", 200, func(m *memory.Memory) { m.Content = "Аня" })

	wantCounts(t, "Аня", a.run(t, Target{Project: project, Push: true}), 0, 0, 1)
	if a.local.mem["a1"].Content != "Аня" || len(a.local.dirty) != 0 {
		t.Fatalf("локальная версия тронута или пометка не снята: %q %v", a.local.mem["a1"].Content, a.local.dirty)
	}
}

func TestPushLoserAppliedAtOnce(t *testing.T) {
	hub, a, _ := synced(t)
	a.edit("a1", 200, func(m *memory.Memory) { m.Content = "Аня" })
	// Между pull и push прямой писатель записал более новую версию.
	hub.beforeVers = func() {
		h, _ := hub.get("a1")
		h.Content, h.UpdatedAt, h.UpdatedBy = "Влад", 300, "Влад"
		hub.put(h)
	}
	wantCounts(t, "Аня", a.run(t, both), 0, 1, 1)
	if a.local.mem["a1"].Content != "Влад" || len(a.local.dirty) != 0 {
		t.Fatalf("проигравшая не применена сразу: %q %v", a.local.mem["a1"].Content, a.local.dirty)
	}
}

func TestPushStaleDeferred(t *testing.T) {
	hub, a, _ := synced(t)
	a.edit("a1", 300, func(m *memory.Memory) { m.Content = "Аня" })
	hub.beforePush = func() {
		h, _ := hub.get("a1")
		h.Content, h.UpdatedAt, h.UpdatedBy = "Влад", 200, "Влад"
		hub.put(h)
	}
	res := a.run(t, both)
	if res.Deferred != 1 || res.Pushed != 0 || a.local.dirty["a1"].seq == 0 {
		t.Fatalf("гонка CAS: %+v, пометка %v", res, a.local.dirty)
	}
	wantCounts(t, "Аня", a.run(t, both), 1, 0, 1)
	if h, _ := hub.get("a1"); h.Content != "Аня" {
		t.Fatalf("отложенная запись не дошла: %q", h.Content)
	}
}

func TestPushClosesFK(t *testing.T) {
	hub := newHub()
	a := newDev("Аня", hub, WithLimits(0, 1))
	// Задача помечена раньше эпика, на который ссылается: первая пачка — только она.
	a.local.save(
		rec("c1", memory.KindTask, 100, "задача", memory.Link{Dst: "p1", Rel: memory.RelPartOf}),
		rec("p1", memory.KindTask, 100, "эпик"),
	)
	wantCounts(t, "Аня", a.run(t, both), 2, 0, 0)
	if h, _ := hub.get("c1"); len(h.Links) != 1 {
		t.Fatalf("связь задачи не ушла: %v", h.Links)
	}
}

func TestPullRefsAsStubs(t *testing.T) {
	hub := newHub()
	q := rec("q1", memory.KindSemantic, 100, "q")
	p := rec("p1", memory.KindTask, 100, "эпик", memory.Link{Dst: "q1"})
	c := rec("c1", memory.KindTask, 100, "задача", memory.Link{Dst: "p1", Rel: memory.RelPartOf})
	hub.put(q)
	hub.put(p)
	hub.put(c)
	q.Content, p.Content = "q-2", "эпик-2" // правки двигают их за c1
	hub.put(q)
	hub.put(p)

	b := newDev("Борис", hub, WithLimits(1, 0))
	wantCounts(t, "Борис", b.run(t, both), 0, 3, 0)
	for _, id := range []string{"q1", "p1", "c1"} {
		h, _ := hub.get(id)
		wantSame(t, b.local.mem[id], h)
	}
}

func TestSchemaChecks(t *testing.T) {
	for _, tc := range []struct {
		version int
		want    error
	}{{0, ErrSchemaMissing}, {hubSchema - 1, ErrSchemaBehind}, {hubSchema + 1, ErrSchemaAhead}} {
		hub := newHub()
		hub.version = tc.version
		b := newDev("Борис", hub)
		if _, err := b.s.Run(context.Background(), both, b.name); !errors.Is(err, tc.want) {
			t.Fatalf("версия %d: err = %v, want %v", tc.version, err, tc.want)
		}
		if b.local.states[project].LastError != tc.want.Error() {
			t.Fatalf("версия %d: ошибка не в статусе: %+v", tc.version, b.local.states[project])
		}
	}

	b := newDev("Борис", newHub())
	b.local.ahead = true
	if _, err := b.s.Run(context.Background(), both, b.name); !errors.Is(err, ErrLocalAhead) {
		t.Fatalf("err = %v, want ErrLocalAhead", err)
	}
	if b.local.saves != 0 {
		t.Fatal("старый бинарь записал состояние в новую схему")
	}
}

func TestBaseChangeResyncs(t *testing.T) {
	hub, a, _ := synced(t)
	a.local.save(rec("a2", memory.KindSemantic, 100, "ещё"))
	a.run(t, both)

	// Цель переключили на новую пустую базу.
	fresh := newHub()
	fresh.id = "base-2"
	*hub = *fresh
	wantCounts(t, "Аня", a.run(t, both), 2, 0, 0)
	if st := a.local.states[project]; st.BaseID != "base-2" || st.Cursor != (Cursor{}) {
		t.Fatalf("состояние после смены базы: %+v", st)
	}
}

func TestRestoredBaseStops(t *testing.T) {
	hub, a, _ := synced(t)
	a.run(t, both) // курсор Ани — за последним изменением
	hub.next = 1   // восстановили из ранней копии: xid переиспользуются, base_id прежний
	if _, err := a.s.Run(context.Background(), both, a.name); !errors.Is(err, ErrRestored) {
		t.Fatalf("err = %v, want ErrRestored", err)
	}
}

func TestPushLoserKeepsLinkToBatchNeighbor(t *testing.T) {
	hub, a, _ := synced(t)
	a.local.save(rec("a2", memory.KindSemantic, 100, "сосед"))
	a.run(t, both)
	a.edit("a1", 200, func(m *memory.Memory) { m.Content = "Аня" })
	a.edit("a2", 200, func(m *memory.Memory) { m.Content = "Аня" })
	// Между pull и push прямой писатель связал a1 с a2 более новой версией.
	toA2 := memory.Link{Dst: "a2", Rel: memory.RelPartOf}
	hub.beforeVers = func() {
		h, _ := hub.get("a1")
		h.Content, h.UpdatedAt, h.UpdatedBy, h.Links = "Влад", 300, "Влад", []memory.Link{toA2}
		hub.put(h)
	}
	res := a.run(t, both)
	if res.Pushed != 1 || res.Pulled != 1 || res.Conflicts != 1 || res.DroppedLinks != 0 {
		t.Fatalf("итог: %+v", res)
	}
	if m := a.local.mem["a1"]; !slices.Equal(m.Links, []memory.Link{toA2}) {
		t.Fatalf("проигравшая потеряла связь на соседа по пачке: %v", m.Links)
	}
	if len(a.local.dirty) != 0 {
		t.Fatalf("остались пометки: %v", a.local.dirty)
	}
}

func TestInitialMarkAgainstFullBaseSendsNothing(t *testing.T) {
	hub, a, _ := synced(t)
	a.local.save(rec("a2", memory.KindTask, 100, "задача", memory.Link{Dst: "a1"}))
	a.run(t, both)
	b := newDev("Борис", hub)
	wantCounts(t, "Борис", b.run(t, Target{Project: project, Pull: true}), 0, 2, 0)

	next := hub.next
	wantCounts(t, "Борис", b.run(t, both), 0, 0, 0)
	if hub.next != next || len(b.local.dirty) != 0 {
		t.Fatalf("разметка против полной базы что-то записала (xid %d -> %d) или оставила пометки %v", next, hub.next, b.local.dirty)
	}
}

func TestLocalNewerUnmarkedIsPushed(t *testing.T) {
	hub := newHub()
	b := newDev("Борис", hub)
	b.local.states[project] = State{BaseID: hub.id, PushMarked: true}
	b.local.mem["r1"] = rec("r1", memory.KindSemantic, 500, "локальная новее") // без пометки
	old := rec("r1", memory.KindSemantic, 100, "прямой писатель с отстающими часами")
	old.UpdatedBy = "Влад"
	hub.put(old)

	res := b.run(t, both)
	if res.Pushed != 1 || res.Pulled != 0 {
		t.Fatalf("итог: %+v", res)
	}
	if h, _ := hub.get("r1"); h.Content != "локальная новее" {
		t.Fatalf("в общей базе %q", h.Content)
	}
}

func TestDroppedLinks(t *testing.T) {
	hub := newHub()
	hub.put(rec("r1", memory.KindSemantic, 100, "связь в никуда", memory.Link{Dst: "ghost"}))
	b := newDev("Борис", hub)
	res := b.run(t, both)
	if res.Pulled != 1 || res.DroppedLinks != 1 || len(b.local.mem["r1"].Links) != 0 {
		t.Fatalf("итог %+v, связи %v", res, b.local.mem["r1"].Links)
	}
}

func TestMarkKeepsEdit(t *testing.T) {
	_, a, b := synced(t)
	b.run(t, Target{Project: project, Pull: true}) // «Отправлять» выключили
	a.edit("a1", 300, func(m *memory.Memory) { m.Content = "Аня" })
	a.run(t, both)
	b.edit("a1", 200, func(m *memory.Memory) { m.Content = "Борис" })
	// Включили обратно: разметка не превращает правку Бориса в «просто разметку».
	wantCounts(t, "Борис", b.run(t, both), 0, 1, 1)
}

func TestNoNameNoStamp(t *testing.T) {
	hub := newHub()
	a := newDev("", hub)
	a.local.save(rec("r1", memory.KindSemantic, 100, "без имени"))
	if res := a.run(t, both); res.Stamped != 0 || res.Pushed != 1 {
		t.Fatalf("итог: %+v", res)
	}
	if h, _ := hub.get("r1"); h.Author != "" {
		t.Fatalf("автор %q при пустом имени", h.Author)
	}
}

func TestStateSavedAfterCancel(t *testing.T) {
	hub := newHub()
	hub.put(rec("r1", memory.KindSemantic, 100, "r1"))
	b := newDev("Борис", hub)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.s.Run(ctx, both, b.name); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if st := b.local.states[project]; st.LastError == "" || st.LastRunAt != 1000 {
		t.Fatalf("итог прогона под отменённым ctx не записан: %+v", st)
	}
}

func TestRestoredByMaxXID(t *testing.T) {
	hub, a, _ := synced(t)
	r := hub.rows["a1"]
	r.xid = hub.next + 5 // номер изменения «из будущего» при прежнем base_id
	hub.rows["a1"] = r
	if _, err := a.s.Run(context.Background(), both, a.name); !errors.Is(err, ErrRestored) {
		t.Fatalf("err = %v, want ErrRestored", err)
	}
}
