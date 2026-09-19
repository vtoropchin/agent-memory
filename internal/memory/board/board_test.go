package board

import (
	"context"
	"errors"
	"slices"
	"testing"

	"agentmemory/internal/memory"
)

// fakeSource — in-memory порт доски и карточки: все записи в одной карте,
// ListTasks/Neighbors/GetMany считают по ней, без БД.
type fakeSource struct {
	all        map[string]memory.Memory
	getManyErr error
	listCalls  int
	getCalls   int
}

func newFake(ms ...memory.Memory) *fakeSource {
	f := &fakeSource{all: map[string]memory.Memory{}}
	for _, m := range ms {
		f.all[m.Id] = m
	}
	return f
}

func (f *fakeSource) ListTasks(_ context.Context, scopes []string) ([]memory.Memory, error) {
	f.listCalls++
	var out []memory.Memory
	for _, m := range f.all {
		if m.Kind != memory.KindTask || m.SupersededBy != nil {
			continue
		}
		if len(scopes) > 0 && !slices.Contains(scopes, m.Scope) {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

func (f *fakeSource) GetMany(_ context.Context, ids []string) ([]memory.Memory, error) {
	f.getCalls++
	if f.getManyErr != nil {
		return nil, f.getManyErr
	}
	var out []memory.Memory
	for _, id := range ids {
		if m, ok := f.all[id]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeSource) Neighbors(_ context.Context, id string) ([]memory.Memory, error) {
	var out []memory.Memory
	self := f.all[id]
	for _, m := range f.all {
		if m.Id == id || m.SupersededBy != nil {
			continue
		}
		linked := false
		for _, l := range m.Links {
			if l.Dst == id {
				linked = true
			}
		}
		for _, l := range self.Links {
			if l.Dst == m.Id {
				linked = true
			}
		}
		if linked {
			out = append(out, m)
		}
	}
	return out, nil
}

func str(s string) *string { return &s }

// task — живая задача scope "p" с заголовком = id.
func task(id string, status memory.Status, opts ...func(*memory.Memory)) memory.Memory {
	m := memory.Memory{Id: id, Kind: memory.KindTask, Scope: "p", Status: status, Type: memory.TaskTypeTask, Summary: str("Задача " + id), Importance: 3, UpdatedAt: 100}
	for _, o := range opts {
		o(&m)
	}
	return m
}

func partOf(parent string) func(*memory.Memory) {
	return func(m *memory.Memory) { m.Links = append(m.Links, memory.Link{Dst: parent, Rel: memory.RelPartOf}) }
}

func blockedBy(ids ...string) func(*memory.Memory) {
	return func(m *memory.Memory) {
		for _, id := range ids {
			m.Links = append(m.Links, memory.Link{Dst: id, Rel: memory.RelBlockedBy})
		}
	}
}

func column(b Board, st memory.Status) Column {
	for _, c := range b.Columns {
		if c.Status == st {
			return c
		}
	}
	return Column{}
}

func cardIDs(c Column) []string {
	out := make([]string, len(c.Cards))
	for i, card := range c.Cards {
		out[i] = card.Memory.Id
	}
	return out
}

func mustBuild(t *testing.T, src Source, f Filter) Board {
	t.Helper()
	b, err := Build(context.Background(), src, f)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return b
}

// TestBuildColumnsOrderAndEmptyBoard: всегда семь колонок в порядке
// memory.Statuses, у пустой доски — пустые массивы карточек, не nil.
func TestBuildColumnsOrderAndEmptyBoard(t *testing.T) {
	b := mustBuild(t, newFake(), Filter{})
	if len(b.Columns) != len(memory.Statuses) {
		t.Fatalf("колонок %d, want %d", len(b.Columns), len(memory.Statuses))
	}
	for i, st := range memory.Statuses {
		if b.Columns[i].Status != st || b.Columns[i].Cards == nil || len(b.Columns[i].Cards) != 0 || b.Columns[i].Total != 0 {
			t.Errorf("Columns[%d] = %+v, want пустая колонка %s", i, b.Columns[i], st)
		}
	}
}

// TestBuildSortWithinColumn: importance ↓, затем updated_at ↓, затем id.
func TestBuildSortWithinColumn(t *testing.T) {
	src := newFake(
		task("c", memory.StatusTodo, func(m *memory.Memory) { m.Importance = 3; m.UpdatedAt = 10 }),
		task("b", memory.StatusTodo, func(m *memory.Memory) { m.Importance = 3; m.UpdatedAt = 10 }),
		task("a", memory.StatusTodo, func(m *memory.Memory) { m.Importance = 3; m.UpdatedAt = 20 }),
		task("z", memory.StatusTodo, func(m *memory.Memory) { m.Importance = 5; m.UpdatedAt = 1 }),
		task("d", memory.StatusDoing, func(m *memory.Memory) { m.Importance = 5 }),
	)
	b := mustBuild(t, src, Filter{})
	if got := cardIDs(column(b, memory.StatusTodo)); !slices.Equal(got, []string{"z", "a", "b", "c"}) {
		t.Errorf("todo = %v, want [z a b c]", got)
	}
	if got := cardIDs(column(b, memory.StatusDoing)); !slices.Equal(got, []string{"d"}) {
		t.Errorf("doing = %v, want [d]", got)
	}
}

// TestBuildDoneTruncation: колонка done усечена до DoneLimit самых свежих по
// updated_at, Total — до усечения; остальные колонки целиком; DoneLimit <= 0 —
// дефолт 30.
func TestBuildDoneTruncation(t *testing.T) {
	var ms []memory.Memory
	for i := 0; i < 40; i++ {
		id := string(rune('A'+i%26)) + string(rune('a'+i/26))
		ms = append(ms, task(id, memory.StatusDone, func(m *memory.Memory) { m.UpdatedAt = int64(i); m.Importance = 1 + i%5 }))
		ms = append(ms, task("todo-"+id, memory.StatusTodo))
	}
	src := newFake(ms...)

	b := mustBuild(t, src, Filter{DoneLimit: 3})
	done := column(b, memory.StatusDone)
	if done.Total != 40 || len(done.Cards) != 3 {
		t.Fatalf("done: Total=%d len=%d, want 40/3", done.Total, len(done.Cards))
	}
	// Три самых свежих (updated_at 39, 38, 37), внутри — общий порядок
	// (importance ↓, затем updated_at ↓).
	for _, c := range done.Cards {
		if c.Memory.UpdatedAt < 37 {
			t.Errorf("в done попала несвежая карточка updated_at=%d", c.Memory.UpdatedAt)
		}
	}
	for i := 1; i < len(done.Cards); i++ {
		a, b := done.Cards[i-1].Memory, done.Cards[i].Memory
		if a.Importance < b.Importance || (a.Importance == b.Importance && a.UpdatedAt < b.UpdatedAt) {
			t.Errorf("done не отсортирована общим правилом: %+v перед %+v", a, b)
		}
	}
	todo := column(b, memory.StatusTodo)
	if todo.Total != 40 || len(todo.Cards) != 40 {
		t.Errorf("todo не должна усекаться: Total=%d len=%d", todo.Total, len(todo.Cards))
	}

	b = mustBuild(t, src, Filter{})
	if done := column(b, memory.StatusDone); len(done.Cards) != defaultDoneLimit || done.Total != 40 {
		t.Errorf("дефолтное усечение: len=%d Total=%d, want %d/40", len(done.Cards), done.Total, defaultDoneLimit)
	}
}

// TestBuildBlocked: блокер в scope — из индекса; вне scope — через GetMany;
// done и тумстоун не блокируют; сам блокер не помечен.
func TestBuildBlocked(t *testing.T) {
	tomb := "tomb"
	src := newFake(
		task("in", memory.StatusTodo),
		task("out", memory.StatusDoing, func(m *memory.Memory) { m.Scope = "other"; m.Summary = str("Чужая") }),
		task("done", memory.StatusDone),
		task("tomb", memory.StatusTodo, func(m *memory.Memory) { m.SupersededBy = &tomb }),
		task("t1", memory.StatusTodo, blockedBy("in", "done")),
		task("t2", memory.StatusTodo, blockedBy("out", "tomb")),
		task("t3", memory.StatusTodo, blockedBy("done", "tomb")),
	)
	b := mustBuild(t, src, Filter{Scopes: []string{"p"}})
	if src.getCalls != 1 {
		t.Errorf("GetMany должен быть вызван ровно один раз для блокеров вне индекса, got %d", src.getCalls)
	}
	byID := map[string]Card{}
	for _, c := range column(b, memory.StatusTodo).Cards {
		byID[c.Memory.Id] = c
	}
	if c := byID["t1"]; !c.Blocked || len(c.BlockedBy) != 1 || c.BlockedBy[0].ID != "in" || c.BlockedBy[0].Summary != "Задача in" || c.BlockedBy[0].Status != memory.StatusTodo {
		t.Errorf("t1 = %+v, want Blocked by [in]", c)
	}
	if c := byID["t2"]; !c.Blocked || len(c.BlockedBy) != 1 || c.BlockedBy[0].ID != "out" || c.BlockedBy[0].Summary != "Чужая" {
		t.Errorf("t2 = %+v, want Blocked by [out] (кросс-scope через GetMany)", c)
	}
	if c := byID["t3"]; c.Blocked || len(c.BlockedBy) != 0 {
		t.Errorf("t3 = %+v, want не заблокирована (done и тумстоун не блокируют)", c)
	}
	if c := byID["in"]; c.Blocked {
		t.Errorf("блокер сам не должен быть заблокирован: %+v", c)
	}
	if _, ok := byID["out"]; ok {
		t.Errorf("задача чужого scope не должна попасть на доску")
	}
}

// TestBuildGetManyError: ошибка догрузки блокеров — ошибка Build (доска без
// блокировок вводила бы в заблуждение), а не тихая деградация.
func TestBuildGetManyError(t *testing.T) {
	src := newFake(task("t", memory.StatusTodo, blockedBy("ghost")))
	src.getManyErr = errors.New("сбой")
	if _, err := Build(context.Background(), src, Filter{}); err == nil || !errors.Is(err, src.getManyErr) {
		t.Errorf("Build при сбое GetMany = %v, want обёрнутую ошибку", err)
	}
}

// TestBuildParentAndProgress: родитель из индекса — полный Ref; вне scopes —
// только id; прогресс считается по всем живым детям до фильтров.
func TestBuildParentAndProgress(t *testing.T) {
	src := newFake(
		task("epic", memory.StatusDoing, func(m *memory.Memory) { m.Type = memory.TaskTypeEpic }),
		task("s1", memory.StatusDone, partOf("epic"), func(m *memory.Memory) { m.Type = memory.TaskTypeStory }),
		task("s2", memory.StatusTodo, partOf("epic"), func(m *memory.Memory) { m.Type = memory.TaskTypeStory }),
		task("s3", memory.StatusTodo, partOf("epic"), func(m *memory.Memory) { m.Type = memory.TaskTypeStory; m.Branch = "feat/x" }),
		task("orphan", memory.StatusTodo, partOf("far")),
		task("far", memory.StatusTodo, func(m *memory.Memory) { m.Scope = "other" }),
	)
	b := mustBuild(t, src, Filter{Scopes: []string{"p"}, Branch: "feat/x"})
	if got := cardIDs(column(b, memory.StatusTodo)); !slices.Equal(got, []string{"s3"}) {
		t.Fatalf("фильтр branch: todo = %v, want [s3]", got)
	}
	s3 := column(b, memory.StatusTodo).Cards[0]
	if s3.Parent == nil || s3.Parent.ID != "epic" || s3.Parent.Summary != "Задача epic" || s3.Parent.Type != memory.TaskTypeEpic || s3.Parent.Status != memory.StatusDoing {
		t.Errorf("Parent s3 = %+v, want полный Ref эпика", s3.Parent)
	}
	if s3.Progress != nil {
		t.Errorf("у задачи без детей Progress = %+v, want nil", s3.Progress)
	}

	b = mustBuild(t, src, Filter{Scopes: []string{"p"}, Types: []memory.TaskType{memory.TaskTypeEpic}})
	doing := column(b, memory.StatusDoing)
	if got := cardIDs(doing); !slices.Equal(got, []string{"epic"}) {
		t.Fatalf("фильтр types: doing = %v, want [epic]", got)
	}
	if p := doing.Cards[0].Progress; p == nil || p.Done != 1 || p.Total != 3 {
		t.Errorf("Progress эпика = %+v, want {1 3} (по всем детям, не по показанным)", p)
	}
	if doing.Cards[0].Parent != nil {
		t.Errorf("у корневого эпика Parent = %+v, want nil", doing.Cards[0].Parent)
	}

	b = mustBuild(t, src, Filter{Scopes: []string{"p"}})
	var orphan Card
	for _, c := range column(b, memory.StatusTodo).Cards {
		if c.Memory.Id == "orphan" {
			orphan = c
		}
	}
	if orphan.Parent == nil || orphan.Parent.ID != "far" || orphan.Parent.Summary != "" {
		t.Errorf("Parent вне scopes = %+v, want Ref только с id", orphan.Parent)
	}
}

// TestBuildFilters: каждый фильтр по отдельности.
func TestBuildFilters(t *testing.T) {
	src := newFake(
		task("epic", memory.StatusTodo, func(m *memory.Memory) { m.Type = memory.TaskTypeEpic; m.Tags = []string{"x"} }),
		task("a", memory.StatusTodo, partOf("epic"), func(m *memory.Memory) { m.Branch = "b1"; m.Workflow = "dev"; m.Tags = []string{"x", "y"} }),
		task("b", memory.StatusTodo, func(m *memory.Memory) { m.Branch = "b2"; m.Workflow = "dev" }),
		task("c", memory.StatusTodo, func(m *memory.Memory) { m.Type = memory.TaskTypeStory; m.Workflow = "review"; m.Tags = []string{"y"} }),
	)
	tests := []struct {
		name string
		f    Filter
		want []string
	}{
		{"без фильтра", Filter{}, []string{"a", "b", "c", "epic"}},
		{"types", Filter{Types: []memory.TaskType{memory.TaskTypeStory, memory.TaskTypeEpic}}, []string{"c", "epic"}},
		{"parent", Filter{ParentID: "epic"}, []string{"a"}},
		{"branch", Filter{Branch: "b2"}, []string{"b"}},
		{"workflow", Filter{Workflow: "dev"}, []string{"a", "b"}},
		{"tag", Filter{Tag: "y"}, []string{"a", "c"}},
		{"комбинация", Filter{Workflow: "dev", Tag: "x"}, []string{"a"}},
		{"ничего", Filter{Branch: "нет"}, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cardIDs(column(mustBuild(t, src, tt.f), memory.StatusTodo))
			if !slices.Equal(got, tt.want) {
				t.Errorf("todo = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestBuildUnknownStatusSkipped: задача с неизвестной стадией в колонки не
// попадает и не роняет сборку.
func TestBuildUnknownStatusSkipped(t *testing.T) {
	b := mustBuild(t, newFake(task("x", memory.Status("archived")), task("y", memory.StatusTodo)), Filter{})
	total := 0
	for _, c := range b.Columns {
		total += c.Total
	}
	if total != 1 {
		t.Errorf("на доске %d карточек, want 1 (неизвестная стадия пропущена)", total)
	}
}

// TestDescribe: карточка задачи — родитель (живой; тумстоун опускается), дети
// в порядке доски с прогрессом, живые блокеры, «что блокирует»; у не-задачи —
// пустой Detail без обращения к Source.
func TestDescribe(t *testing.T) {
	ctx := context.Background()
	tomb := "tomb"
	src := newFake(
		task("epic", memory.StatusDoing, func(m *memory.Memory) { m.Type = memory.TaskTypeEpic }),
		task("story", memory.StatusTodo, partOf("epic"), blockedBy("blk", "doneblk"), func(m *memory.Memory) { m.Type = memory.TaskTypeStory }),
		task("kidDone", memory.StatusDone, partOf("story")),
		task("kidOpen", memory.StatusTodo, partOf("story"), func(m *memory.Memory) { m.Importance = 5 }),
		task("kidTomb", memory.StatusTodo, partOf("story"), func(m *memory.Memory) { m.SupersededBy = &tomb }),
		task("blk", memory.StatusTodo, func(m *memory.Memory) { m.Scope = "other" }),
		task("doneblk", memory.StatusDone),
		task("waits", memory.StatusBacklog, blockedBy("story")),
		task("waitsDone", memory.StatusDone, blockedBy("story"), func(m *memory.Memory) { m.Importance = 5 }),
		memory.Memory{Id: "fact", Kind: memory.KindSemantic, Links: []memory.Link{{Dst: "story", Rel: memory.RelPartOf}, {Dst: "story", Rel: memory.RelBlockedBy}}},
	)

	d, err := Describe(ctx, src, src.all["story"])
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if d.Parent == nil || d.Parent.ID != "epic" || d.Parent.Type != memory.TaskTypeEpic || d.Parent.Summary != "Задача epic" {
		t.Errorf("Parent = %+v, want эпик", d.Parent)
	}
	if ids := refIDs(d.Children); !slices.Equal(ids, []string{"kidOpen", "kidDone"}) {
		t.Errorf("Children = %v, want [kidOpen kidDone] (порядок доски, тумстоун и факт исключены)", ids)
	}
	if d.Progress == nil || *d.Progress != (Progress{Done: 1, Total: 2}) {
		t.Errorf("Progress = %+v, want {1 2}", d.Progress)
	}
	if ids := refIDs(d.BlockedBy); !slices.Equal(ids, []string{"blk"}) {
		t.Errorf("BlockedBy = %v, want [blk] (done не блокирует, кросс-scope учтён)", ids)
	}
	if ids := refIDs(d.Blocks); !slices.Equal(ids, []string{"waits", "waitsDone"}) {
		t.Errorf("Blocks = %v, want [waits waitsDone] (порядок доски, факт исключён)", ids)
	}

	// Родитель-тумстоун — Parent nil (прежний Store.Get его показывал).
	src.all["epic"] = task("epic", memory.StatusDoing, func(m *memory.Memory) { m.SupersededBy = &tomb })
	d, err = Describe(ctx, src, src.all["story"])
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if d.Parent != nil {
		t.Errorf("родитель-тумстоун должен опускаться, got %+v", d.Parent)
	}

	// Не-задача — пустой Detail, Source не трогается.
	calls := src.getCalls
	d, err = Describe(ctx, src, src.all["fact"])
	if err != nil || d.Parent != nil || d.Progress != nil || len(d.Children) != 0 || len(d.BlockedBy) != 0 || len(d.Blocks) != 0 || src.getCalls != calls {
		t.Errorf("Describe(факт) = %+v, %v; want пустой Detail без обращений к Source", d, err)
	}
}

func refIDs(refs []Ref) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.ID
	}
	return out
}
