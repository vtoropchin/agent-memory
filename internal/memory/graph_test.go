package memory

import "testing"

func TestParentOf(t *testing.T) {
	tests := []struct {
		name   string
		links  []Link
		wantID string
		wantOK bool
	}{
		{name: "нет связей", links: nil, wantOK: false},
		{name: "только чужие rel", links: []Link{{Dst: "A", Rel: RelRelated}, {Dst: "B", Rel: RelCauses}}, wantOK: false},
		{name: "один part_of", links: []Link{{Dst: "A", Rel: RelRelated}, {Dst: "P", Rel: RelPartOf}}, wantID: "P", wantOK: true},
		{name: "несколько part_of — первый", links: []Link{{Dst: "P1", Rel: RelPartOf}, {Dst: "P2", Rel: RelPartOf}}, wantID: "P1", wantOK: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, ok := ParentOf(Memory{Links: tt.links})
			if ok != tt.wantOK || id != tt.wantID {
				t.Errorf("ParentOf = (%q, %v), want (%q, %v)", id, ok, tt.wantID, tt.wantOK)
			}
		})
	}
}

func TestChildrenOf(t *testing.T) {
	const epic = "E"
	neighbors := []Memory{
		{Id: "s1", Kind: KindTask, Links: []Link{{Dst: epic, Rel: RelPartOf}}},
		{Id: "s2", Kind: KindTask, Links: []Link{{Dst: "X", Rel: RelRelated}, {Dst: epic, Rel: RelPartOf}}},
		{Id: "rel", Kind: KindTask, Links: []Link{{Dst: epic, Rel: RelRelated}}},     // чужой rel — не ребёнок
		{Id: "other", Kind: KindTask, Links: []Link{{Dst: "Y", Rel: RelPartOf}}},     // part_of другой записи
		{Id: "fact", Kind: KindSemantic, Links: []Link{{Dst: epic, Rel: RelPartOf}}}, // не задача: part_of тематический, не иерархия
		{Id: "parent", Kind: KindTask, Links: nil},                                   // родитель эпика — без Links на эпик
	}
	tests := []struct {
		name string
		id   string
		want []string
	}{
		{name: "входящие part_of", id: epic, want: []string{"s1", "s2"}},
		{name: "нет детей", id: "Z", want: nil},
		{name: "пустые соседи", id: epic, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := neighbors
			if tt.name == "пустые соседи" {
				in = nil
			}
			got := ChildrenOf(tt.id, in)
			if len(got) != len(tt.want) {
				t.Fatalf("ChildrenOf = %+v, want ids %v", got, tt.want)
			}
			for i, w := range tt.want {
				if got[i].Id != w {
					t.Errorf("ChildrenOf[%d].Id = %q, want %q", i, got[i].Id, w)
				}
			}
		})
	}
}

func TestProgress(t *testing.T) {
	tomb := "tomb"
	tests := []struct {
		name                string
		children            []Memory
		wantDone, wantTotal int
	}{
		{name: "пусто", children: nil},
		{name: "все открыты", children: []Memory{{Status: StatusTodo}, {Status: StatusDoing}}, wantTotal: 2},
		{name: "часть готова", children: []Memory{{Status: StatusDone}, {Status: StatusTodo}, {Status: StatusDone}}, wantDone: 2, wantTotal: 3},
		{name: "тумстоуны не считаются", children: []Memory{{Status: StatusDone, SupersededBy: &tomb}, {Status: StatusTodo, SupersededBy: &tomb}, {Status: StatusDone}}, wantDone: 1, wantTotal: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			done, total := Progress(tt.children)
			if done != tt.wantDone || total != tt.wantTotal {
				t.Errorf("Progress = (%d, %d), want (%d, %d)", done, total, tt.wantDone, tt.wantTotal)
			}
		})
	}
}

func TestSortByStage(t *testing.T) {
	tasks := []Memory{
		{Id: "d", Status: StatusDone, Importance: 5},
		{Id: "t2", Status: StatusTodo, Importance: 3, UpdatedAt: 10},
		{Id: "t1", Status: StatusTodo, Importance: 3, UpdatedAt: 10},
		{Id: "t0", Status: StatusTodo, Importance: 3, UpdatedAt: 20}, // свежее — выше при равной важности
		{Id: "g", Status: StatusDoing, Importance: 1},
		{Id: "t5", Status: StatusTodo, Importance: 5},
		{Id: "b", Status: StatusBacklog, Importance: 1},
	}
	SortByStage(tasks)
	want := []string{"b", "t5", "t0", "t1", "t2", "g", "d"}
	for i, w := range want {
		if tasks[i].Id != w {
			t.Fatalf("SortByStage[%d] = %q, want %q (полный порядок: %+v)", i, tasks[i].Id, w, ids(tasks))
		}
	}
}

func ids(ms []Memory) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Id
	}
	return out
}

func TestBlockerIDs(t *testing.T) {
	m := Memory{Links: []Link{{Dst: "A", Rel: RelBlockedBy}, {Dst: "P", Rel: RelPartOf}, {Dst: "B", Rel: RelBlockedBy}, {Dst: "R", Rel: RelRelated}}}
	got := BlockerIDs(m)
	if len(got) != 2 || got[0] != "A" || got[1] != "B" {
		t.Errorf("BlockerIDs = %v, want [A B]", got)
	}
	if got := BlockerIDs(Memory{Links: []Link{{Dst: "P", Rel: RelPartOf}}}); got != nil {
		t.Errorf("BlockerIDs без blocked_by = %v, want nil", got)
	}
}

func TestLiveBlockers(t *testing.T) {
	tomb := "tomb"
	byID := map[string]Memory{
		"open":     {Id: "open", Kind: KindTask, Status: StatusTodo},
		"doing":    {Id: "doing", Kind: KindTask, Status: StatusDoing},
		"done":     {Id: "done", Kind: KindTask, Status: StatusDone},
		"tomb":     {Id: "tomb", Kind: KindTask, Status: StatusTodo, SupersededBy: &tomb},
		"tombdone": {Id: "tombdone", Kind: KindTask, Status: StatusDone, SupersededBy: &tomb},
	}
	links := func(ids ...string) []Link {
		out := make([]Link, len(ids))
		for i, id := range ids {
			out[i] = Link{Dst: id, Rel: RelBlockedBy}
		}
		return out
	}
	tests := []struct {
		name  string
		links []Link
		want  []string
	}{
		{name: "нет блокеров", links: []Link{{Dst: "open", Rel: RelRelated}}, want: nil},
		{name: "живой todo блокирует", links: links("open"), want: []string{"open"}},
		{name: "done не блокирует", links: links("done"), want: nil},
		{name: "тумстоун не блокирует", links: links("tomb", "tombdone"), want: nil},
		{name: "отсутствующий в карте пропускается", links: links("ghost", "doing"), want: []string{"doing"}},
		{name: "несколько блокеров, порядок Links", links: links("done", "doing", "tomb", "open"), want: []string{"doing", "open"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := LiveBlockers(Memory{Links: tt.links}, byID)
			if len(got) != len(tt.want) {
				t.Fatalf("LiveBlockers = %v, want %v", ids(got), tt.want)
			}
			for i, w := range tt.want {
				if got[i].Id != w {
					t.Errorf("LiveBlockers[%d] = %q, want %q", i, got[i].Id, w)
				}
			}
		})
	}
}

func TestBlockedBy(t *testing.T) {
	const blocker = "B"
	neighbors := []Memory{
		{Id: "t1", Kind: KindTask, Links: []Link{{Dst: blocker, Rel: RelBlockedBy}}},
		{Id: "child", Kind: KindTask, Links: []Link{{Dst: blocker, Rel: RelPartOf}}}, // иерархия, не блокировка
		{Id: "t2", Kind: KindTask, Links: []Link{{Dst: "X", Rel: RelRelated}, {Dst: blocker, Rel: RelBlockedBy}}},
		{Id: "other", Kind: KindTask, Links: []Link{{Dst: "Y", Rel: RelBlockedBy}}},        // блокируется другой записью
		{Id: "fact", Kind: KindSemantic, Links: []Link{{Dst: blocker, Rel: RelBlockedBy}}}, // не задача
	}
	got := BlockedBy(blocker, neighbors)
	if len(got) != 2 || got[0].Id != "t1" || got[1].Id != "t2" {
		t.Errorf("BlockedBy = %v, want [t1 t2]", ids(got))
	}
	if got := BlockedBy(blocker, nil); got != nil {
		t.Errorf("BlockedBy без соседей = %v, want nil", ids(got))
	}
}

func TestStatusAllowsBlocked(t *testing.T) {
	for _, st := range Statuses {
		want := st == StatusBacklog || st == StatusTodo
		if got := StatusAllowsBlocked(st); got != want {
			t.Errorf("StatusAllowsBlocked(%s) = %v, want %v", st, got, want)
		}
	}
}
