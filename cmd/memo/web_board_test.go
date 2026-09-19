package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"agentmemory/internal/memory"
	"agentmemory/internal/memory/store"
)

// putTask — живая задача для тестов доски: заголовок = summary, стадия и
// опциональные правки.
func putTask(t *testing.T, s store.Store, summary string, status memory.Status, mutate func(*memory.Memory)) memory.Memory {
	t.Helper()
	return putTestMemory(t, s, func(m *memory.Memory) {
		title := summary
		m.Kind, m.Status, m.Type, m.Summary = memory.KindTask, status, memory.TaskTypeTask, &title
		if mutate != nil {
			mutate(m)
		}
	})
}

// TestHandleBoard — форма ответа /api/board: семь колонок в порядке стадий,
// карточка без content, blocked/blockedBy, parent/progress, фильтры
// scope/type/parent/branch/workflow/tag, doneLimit/total, 400 на неизвестный
// type.
func TestHandleBoard(t *testing.T) {
	s := newTestWebStore(t)
	epic := putTask(t, s, "Эпик доски", memory.StatusDoing, func(m *memory.Memory) { m.Type = memory.TaskTypeEpic; m.Importance = 5 })
	blocker := putTask(t, s, "Блокер", memory.StatusTodo, func(m *memory.Memory) { m.Scope = "project:other" })
	story := putTask(t, s, "История", memory.StatusTodo, func(m *memory.Memory) {
		m.Type, m.Branch, m.Workflow, m.Tags = memory.TaskTypeStory, "feat/x", "dev", []string{"kanban"}
		m.Links = []memory.Link{{Dst: epic.Id, Rel: memory.RelPartOf}, {Dst: blocker.Id, Rel: memory.RelBlockedBy}}
	})
	for i := 0; i < 3; i++ {
		putTask(t, s, "Готовая", memory.StatusDone, func(m *memory.Memory) {
			m.UpdatedAt = int64(2000 + i)
			m.Links = []memory.Link{{Dst: epic.Id, Rel: memory.RelPartOf}}
		})
	}
	putTestMemory(t, s, nil) // факт — на доску не попадает

	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	var b boardResponse
	if resp := getJSON(t, srv.URL+"/api/board?scope=project:agentmemory&doneLimit=2", &b); resp.StatusCode != http.StatusOK {
		t.Fatalf("статус = %d, хочу 200", resp.StatusCode)
	}
	if len(b.Columns) != len(memory.Statuses) {
		t.Fatalf("колонок %d, хочу %d", len(b.Columns), len(memory.Statuses))
	}
	cols := map[string]boardColumnDTO{}
	for i, c := range b.Columns {
		if c.Status != string(memory.Statuses[i]) {
			t.Errorf("Columns[%d].status = %q, хочу %q", i, c.Status, memory.Statuses[i])
		}
		cols[c.Status] = c
	}
	if c := cols["backlog"]; c.Cards == nil || len(c.Cards) != 0 || c.Total != 0 {
		t.Errorf("пустая колонка backlog = %+v, хочу cards:[] total:0", c)
	}
	if c := cols["done"]; c.Total != 3 || len(c.Cards) != 2 {
		t.Errorf("done: total=%d cards=%d, хочу 3/2 (doneLimit)", c.Total, len(c.Cards))
	}
	if c := cols["todo"]; len(c.Cards) != 1 {
		t.Fatalf("todo: %+v, хочу одну карточку (блокер чужого scope не в выдаче)", c.Cards)
	}
	card := cols["todo"].Cards[0]
	if card.Id != story.Id || card.Summary == nil || *card.Summary != "История" || card.Type != "story" || card.Branch != "feat/x" || card.Workflow != "dev" || len(card.Tags) != 1 {
		t.Errorf("карточка истории = %+v", card)
	}
	if !card.Blocked || len(card.BlockedBy) != 1 || card.BlockedBy[0].Id != blocker.Id || card.BlockedBy[0].Summary == nil || *card.BlockedBy[0].Summary != "Блокер" {
		t.Errorf("blocked/blockedBy = %v/%+v, хочу блокер из другого scope", card.Blocked, card.BlockedBy)
	}
	if card.Parent == nil || card.Parent.Id != epic.Id || card.Parent.Type != "epic" || card.Progress != nil {
		t.Errorf("parent/progress истории = %+v/%+v", card.Parent, card.Progress)
	}
	epicCard := cols["doing"].Cards[0]
	if epicCard.Id != epic.Id || epicCard.Progress == nil || epicCard.Progress.Done != 3 || epicCard.Progress.Total != 4 || epicCard.Parent != nil || epicCard.Blocked {
		t.Errorf("карточка эпика = %+v, хочу progress {3 4}", epicCard)
	}

	// Фильтры.
	for _, tc := range []struct {
		query string
		want  int // всего карточек на доске
	}{
		{"type=epic", 1},
		{"type=story&type=epic", 2},
		{"parent=" + epic.Id, 4},
		{"branch=feat/x", 1},
		{"workflow=dev", 1},
		{"tag=kanban", 1},
		{"branch=none", 0},
		{"scope=project:other", 1},
	} {
		var fb boardResponse
		getJSON(t, srv.URL+"/api/board?"+tc.query, &fb)
		total := 0
		for _, c := range fb.Columns {
			total += len(c.Cards)
		}
		if total != tc.want {
			t.Errorf("/api/board?%s: карточек %d, хочу %d", tc.query, total, tc.want)
		}
	}

	var e errResponse
	if resp := getJSON(t, srv.URL+"/api/board?type=bug", &e); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("type=bug: статус = %d, хочу 400 (%s)", resp.StatusCode, e.Error)
	}
	if resp := getJSON(t, srv.URL+"/api/board?doneLimit=x", &e); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("doneLimit=x: статус = %d, хочу 400", resp.StatusCode)
	}
}

// TestHandlePatchMemory_TaskFields — PATCH переводит карточку по стадиям и
// правит type/branch/workflow; 409 с блокерами у заблокированной; 400 на
// status "blocked", неизвестный rel и content; unlinks снимает блокировку;
// links добавляет родителя.
func TestHandlePatchMemory_TaskFields(t *testing.T) {
	s := newTestWebStore(t)
	blocker := putTask(t, s, "Блокер", memory.StatusTodo, nil)
	epic := putTask(t, s, "Эпик", memory.StatusDoing, func(m *memory.Memory) { m.Type = memory.TaskTypeEpic })
	task := putTask(t, s, "Задача", memory.StatusTodo, func(m *memory.Memory) {
		m.Links = []memory.Link{{Dst: blocker.Id, Rel: memory.RelBlockedBy}}
	})
	fact := putTestMemory(t, s, nil)

	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()
	url := srv.URL + "/api/memories/" + task.Id

	var blocked blockedResponse
	if resp := patchJSON(t, url, `{"status":"doing"}`, &blocked); resp.StatusCode != http.StatusConflict {
		t.Fatalf("PATCH doing у заблокированной: статус = %d, хочу 409", resp.StatusCode)
	}
	if len(blocked.Blockers) != 1 || blocked.Blockers[0].Id != blocker.Id || blocked.Blockers[0].Summary == nil || *blocked.Blockers[0].Summary != "Блокер" || blocked.Error == "" {
		t.Errorf("тело 409 = %+v, хочу error + blockers [Блокер]", blocked)
	}

	var e errResponse
	if resp := patchJSON(t, url, `{"status":"blocked"}`, &e); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status blocked: статус = %d, хочу 400 (%s)", resp.StatusCode, e.Error)
	}
	if resp := patchJSON(t, url, `{"type":"bug"}`, &e); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("type bug: статус = %d, хочу 400", resp.StatusCode)
	}
	if resp := patchJSON(t, url, `{"links":[{"id":"`+epic.Id+`","rel":"blocks"}]}`, &e); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("rel blocks: статус = %d, хочу 400", resp.StatusCode)
	}
	if resp := patchJSON(t, url, `{"content":"новый"}`, &e); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("content: статус = %d, хочу 400 (содержание из морды не правится)", resp.StatusCode)
	}
	if resp := patchJSON(t, url, `{"links":[{"id":"`+fact.Id+`","rel":"blocked_by"}]}`, &e); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("blocked_by на факт: статус = %d, хочу 400", resp.StatusCode)
	}
	if resp := patchJSON(t, srv.URL+"/api/memories/"+fact.Id, `{"status":"doing"}`, &e); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status у факта: статус = %d, хочу 400", resp.StatusCode)
	}

	var dto memoryDTO
	if resp := patchJSON(t, url, `{"unlinks":[{"id":"`+blocker.Id+`","rel":"blocked_by"}],"links":[{"id":"`+epic.Id+`","rel":"part_of"}]}`, &dto); resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH unlinks+links: статус = %d, хочу 200", resp.StatusCode)
	}
	if len(dto.Links) != 1 || dto.Links[0].Dst != epic.Id || dto.Links[0].Rel != "part_of" {
		t.Errorf("links после PATCH = %+v, хочу только part_of эпика", dto.Links)
	}
	if resp := patchJSON(t, url, `{"status":"doing","type":"story","branch":"feat/y","workflow":"dev"}`, &dto); resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH doing после unlinks: статус = %d, хочу 200", resp.StatusCode)
	}
	if dto.Status != "doing" || dto.Type != "story" || dto.Branch != "feat/y" || dto.Workflow != "dev" {
		t.Errorf("DTO после PATCH = %+v", dto)
	}

	// Карточка теперь отдаёт родителя, а у эпика — детей и progress; у блокера
	// — «blocks» пуст (связь снята).
	var card memoryCardResponse
	getJSON(t, srv.URL+"/api/memories/"+task.Id, &card)
	if card.Parent == nil || card.Parent.Id != epic.Id || len(card.BlockedBy) != 0 || card.Blocks == nil {
		t.Errorf("карточка задачи = parent %+v, blockedBy %+v, blocks %+v", card.Parent, card.BlockedBy, card.Blocks)
	}
	var blockerCard memoryCardResponse
	getJSON(t, srv.URL+"/api/memories/"+blocker.Id, &blockerCard)
	if len(blockerCard.Blocks) != 0 {
		t.Errorf("после unlinks блокер не должен никого блокировать: %+v", blockerCard.Blocks)
	}
}

// TestHandleGetMemory_BlockingAndForgottenParent — карточка: blockedBy/blocks
// заполняются, забытый родитель не показывается (techdebt 15.3 закрыт
// board.Describe).
func TestHandleGetMemory_BlockingAndForgottenParent(t *testing.T) {
	s := newTestWebStore(t)
	epic := putTask(t, s, "Эпик", memory.StatusTodo, nil)
	blocker := putTask(t, s, "Блокер", memory.StatusTodo, nil)
	task := putTask(t, s, "Задача", memory.StatusTodo, func(m *memory.Memory) {
		m.Links = []memory.Link{{Dst: epic.Id, Rel: memory.RelPartOf}, {Dst: blocker.Id, Rel: memory.RelBlockedBy}}
	})

	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	var card memoryCardResponse
	getJSON(t, srv.URL+"/api/memories/"+task.Id, &card)
	if card.Parent == nil || len(card.BlockedBy) != 1 || card.BlockedBy[0].Id != blocker.Id {
		t.Fatalf("карточка до forget: parent %+v, blockedBy %+v", card.Parent, card.BlockedBy)
	}
	var blockerCard memoryCardResponse
	getJSON(t, srv.URL+"/api/memories/"+blocker.Id, &blockerCard)
	if len(blockerCard.Blocks) != 1 || blockerCard.Blocks[0].Id != task.Id {
		t.Errorf("blocks блокера = %+v, хочу [задача]", blockerCard.Blocks)
	}

	if resp := deleteJSON(t, srv.URL+"/api/memories/"+epic.Id, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE эпика: статус = %d", resp.StatusCode)
	}
	getJSON(t, srv.URL+"/api/memories/"+task.Id, &card)
	if card.Parent != nil {
		t.Errorf("забытый родитель не должен показываться: %+v", card.Parent)
	}
}
