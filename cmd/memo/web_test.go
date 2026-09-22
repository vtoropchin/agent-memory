package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"agentmemory/internal/config"
	"agentmemory/internal/memory"
	"agentmemory/internal/memory/embed"
	"agentmemory/internal/memory/reason"
	"agentmemory/internal/memory/recall"
	"agentmemory/internal/memory/store"
	"agentmemory/internal/memory/write"
)

func newTestWebStore(t *testing.T) store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "memory.db")
	s, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newTestID(t *testing.T) string {
	t.Helper()
	return ulid.Make().String()
}

// testRecallWeights — веса ранжирования для тестового Recaller (произвольные,
// но ненулевые по каждому слагаемому — как в internal/memory/recall/recall_test.go).
func testRecallWeights() config.RecallWeights {
	return config.RecallWeights{Relevance: 1.0, Importance: 0.5, Recency: 0.4, Access: 0.2}
}

// newTestWebHandler собирает handler web-морды для теста поверх s с
// embed.NewNoop() (write.Writer.Forget не требует эмбеддера; Recaller с Noop
// ведёт себя как чистый FTS5 — векторный слой выключен, Dim()==0).
func newTestWebHandler(s store.Store) http.Handler {
	return newTestWebHandlerWithEmbedder(s, embed.NewNoop())
}

// newTestWebHandlerWithEmbedder — то же самое, но с явно заданным Embedder
// (задача 10.1): используется тестами семантического поиска с fakeEmbedder.
func newTestWebHandlerWithEmbedder(s store.Store, embedder embed.Embedder) http.Handler {
	w := write.New(s, embed.NewNoop())
	rc := recall.New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: testRecallWeights()}, reason.NewNoop(), embedder)
	return newWebHandler(s, w, rc, embedder, webInstance{})
}

// fakeEmbedder — тестовая реализация embed.Embedder (тот же паттерн, что в
// internal/memory/recall/recall_test.go): фиксированный вектор для любого
// текста запроса, заданная размерность.
type fakeEmbedder struct {
	dim int
	vec []float32
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = f.vec
	}
	return out, nil
}

func (f *fakeEmbedder) Model() string { return "fake" }
func (f *fakeEmbedder) Dim() int      { return f.dim }

var _ embed.Embedder = (*fakeEmbedder)(nil)

// putTestMemory кладёт в s запись памяти с разумными дефолтами, переопределяя
// только поля, важные для конкретного теста (через mutate).
// TestHandleStats_HomeScope — /api/stats отдаёт домашний scope инстанса, на котором
// морда открывает доску по умолчанию (дефект D1 ручной проверки Этапа 15).
func TestHandleStats_HomeScope(t *testing.T) {
	s := newTestWebStore(t)
	w := write.New(s, embed.NewNoop())
	rc := recall.New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: testRecallWeights()}, reason.NewNoop(), embed.NewNoop())
	srv := httptest.NewServer(newWebHandler(s, w, rc, embed.NewNoop(), webInstance{HomeScope: "project:home"}))
	defer srv.Close()
	var got statsResponse
	getJSON(t, srv.URL+"/api/stats", &got)
	if got.HomeScope != "project:home" {
		t.Fatalf("homeScope = %q, want project:home", got.HomeScope)
	}
}

func putTestMemory(t *testing.T, s store.Store, mutate func(*memory.Memory)) memory.Memory {
	t.Helper()
	m := memory.Memory{
		Id:         newTestID(t),
		Kind:       memory.KindSemantic,
		Scope:      "project:agentmemory",
		Content:    "содержимое по умолчанию",
		Importance: 3,
		CreatedAt:  1000,
		UpdatedAt:  1000,
	}
	if mutate != nil {
		mutate(&m)
	}
	if err := s.Put(context.Background(), m); err != nil {
		t.Fatalf("Put: %v", err)
	}
	return m
}

// doJSON — общий хелпер для getJSON/deleteJSON: делает запрос method к url и,
// если out != nil, декодирует JSON-тело ответа в out.
func doJSON(t *testing.T, method, url string, out any) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("NewRequest %s %s: %v", method, url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	if out != nil {
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode JSON от %s: %v", url, err)
		}
	}
	return resp
}

func getJSON(t *testing.T, url string, out any) *http.Response {
	t.Helper()
	return doJSON(t, http.MethodGet, url, out)
}

func TestHandleStats(t *testing.T) {
	s := newTestWebStore(t)
	putTestMemory(t, s, func(m *memory.Memory) { m.Kind = memory.KindSemantic; m.Scope = "project:agentmemory" })
	putTestMemory(t, s, func(m *memory.Memory) { m.Kind = memory.KindEpisodic; m.Scope = "user:*" })
	superseder := putTestMemory(t, s, func(m *memory.Memory) { m.Kind = memory.KindSemantic; m.Scope = "project:agentmemory" })
	old := putTestMemory(t, s, func(m *memory.Memory) { m.Kind = memory.KindSemantic; m.Scope = "project:agentmemory" })
	if err := s.Supersede(context.Background(), old.Id, superseder.Id); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	var got statsResponse
	resp := getJSON(t, srv.URL+"/api/stats", &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус = %d, хочу 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if got.Total != 3 {
		t.Errorf("Total = %d, хочу 3", got.Total)
	}
	if got.Superseded != 1 {
		t.Errorf("Superseded = %d, хочу 1", got.Superseded)
	}
	if got.ByScope["project:agentmemory"] != 2 {
		t.Errorf("ByScope[project:agentmemory] = %d, хочу 2", got.ByScope["project:agentmemory"])
	}
	if got.ByKind["episodic"] != 1 {
		t.Errorf("ByKind[episodic] = %d, хочу 1", got.ByKind["episodic"])
	}
}

func TestHandleListMemories_Pagination(t *testing.T) {
	s := newTestWebStore(t)
	for i := 0; i < 5; i++ {
		putTestMemory(t, s, func(m *memory.Memory) {})
	}

	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	var page1 memoriesResponse
	getJSON(t, srv.URL+"/api/memories?limit=2&offset=0", &page1)
	if len(page1.Items) != 2 || page1.Total != 5 || page1.Limit != 2 || page1.Offset != 0 {
		t.Fatalf("page1 = %+v", page1)
	}

	var page2 memoriesResponse
	getJSON(t, srv.URL+"/api/memories?limit=2&offset=2", &page2)
	if len(page2.Items) != 2 || page2.Total != 5 || page2.Offset != 2 {
		t.Fatalf("page2 = %+v", page2)
	}

	var defaultPage memoriesResponse
	getJSON(t, srv.URL+"/api/memories", &defaultPage)
	if defaultPage.Limit != listDefaultPageSize || len(defaultPage.Items) != 5 {
		t.Fatalf("defaultPage = %+v", defaultPage)
	}

	// limit сверх потолка зажимается.
	var capped memoriesResponse
	getJSON(t, srv.URL+"/api/memories?limit=99999", &capped)
	if capped.Limit != browseLimitCap {
		t.Errorf("Limit = %d, хочу потолок %d", capped.Limit, browseLimitCap)
	}
}

func TestHandleListMemories_Filters(t *testing.T) {
	s := newTestWebStore(t)
	putTestMemory(t, s, func(m *memory.Memory) { m.Scope = "project:a"; m.Kind = memory.KindSemantic })
	putTestMemory(t, s, func(m *memory.Memory) { m.Scope = "project:b"; m.Kind = memory.KindEpisodic })
	superseder := putTestMemory(t, s, func(m *memory.Memory) { m.Scope = "project:a"; m.Kind = memory.KindSemantic })
	old := putTestMemory(t, s, func(m *memory.Memory) { m.Scope = "project:a"; m.Kind = memory.KindSemantic })
	if err := s.Supersede(context.Background(), old.Id, superseder.Id); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	var byScope memoriesResponse
	getJSON(t, srv.URL+"/api/memories?scope=project:b", &byScope)
	if byScope.Total != 1 || len(byScope.Items) != 1 || byScope.Items[0].Scope != "project:b" {
		t.Fatalf("byScope = %+v", byScope)
	}

	var byKind memoriesResponse
	getJSON(t, srv.URL+"/api/memories?kind=episodic", &byKind)
	if byKind.Total != 1 || byKind.Items[0].Kind != "episodic" {
		t.Fatalf("byKind = %+v", byKind)
	}

	// По умолчанию superseded не включаются: живых project:a — 2 (первая запись
	// и превзошедшая), старая (old) — под superseded_by, исключена.
	var withoutSuperseded memoriesResponse
	getJSON(t, srv.URL+"/api/memories?scope=project:a", &withoutSuperseded)
	if withoutSuperseded.Total != 2 {
		t.Fatalf("withoutSuperseded.Total = %d, хочу 2", withoutSuperseded.Total)
	}

	var withSuperseded memoriesResponse
	getJSON(t, srv.URL+"/api/memories?scope=project:a&superseded=1", &withSuperseded)
	if withSuperseded.Total != 3 {
		t.Fatalf("withSuperseded.Total = %d, хочу 3", withSuperseded.Total)
	}
}

// TestHandleListMemories_EmptyFilterParamsIgnored — регресс на баг, когда
// present-but-empty query-параметр (?scope=&kind=) превращался в
// r.URL.Query()["scope"] == [""] (слайс длины 1, не nil) и уходил в
// BrowseFilter.Scopes как фильтр scope IN (”), обнуляя выдачу. Пустой
// параметр должен означать «фильтр не задан».
func TestHandleListMemories_EmptyFilterParamsIgnored(t *testing.T) {
	s := newTestWebStore(t)
	for i := 0; i < 3; i++ {
		putTestMemory(t, s, func(m *memory.Memory) {})
	}

	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	var got memoriesResponse
	resp := getJSON(t, srv.URL+"/api/memories?scope=&kind=", &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус = %d, хочу 200", resp.StatusCode)
	}
	if got.Total != 3 || len(got.Items) != 3 {
		t.Fatalf("got = %+v, хочу все 3 записи (пустой scope/kind не должен фильтровать)", got)
	}
}

// TestHandleSearch_EmptyScopeParamIgnored — тот же баг, но для /api/search:
// пустой scope не должен резать выдачу лексического поиска.
func TestHandleSearch_EmptyScopeParamIgnored(t *testing.T) {
	s := newTestWebStore(t)
	putTestMemory(t, s, func(m *memory.Memory) { m.Content = "уникальная фраза про жирафов" })

	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	var found searchResponse
	resp := getJSON(t, srv.URL+"/api/search?q=жирафов&scope=", &found)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус = %d, хочу 200", resp.StatusCode)
	}
	if len(found.Items) != 1 {
		t.Fatalf("items = %+v, хочу 1 совпадение (пустой scope не должен резать выдачу)", found.Items)
	}
}

func TestHandleGetMemory(t *testing.T) {
	s := newTestWebStore(t)
	summary := "сводка"
	m := putTestMemory(t, s, func(m *memory.Memory) {
		m.Summary = &summary
		m.Tags = []string{"go", "stack"}
	})
	other := putTestMemory(t, s, func(m *memory.Memory) {})
	if err := s.Put(context.Background(), memory.Memory{
		Id: m.Id, Kind: m.Kind, Scope: m.Scope, Content: m.Content, Summary: m.Summary,
		Importance: m.Importance, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt, Tags: m.Tags,
		Links: []memory.Link{{Dst: other.Id, Rel: memory.RelRelated}},
	}); err != nil {
		t.Fatalf("Put (со связью): %v", err)
	}

	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	var card memoryCardResponse
	resp := getJSON(t, srv.URL+"/api/memories/"+m.Id, &card)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус = %d, хочу 200", resp.StatusCode)
	}
	if card.Memory.Id != m.Id {
		t.Errorf("Memory.Id = %q, хочу %q", card.Memory.Id, m.Id)
	}
	if len(card.Memory.Tags) != 2 {
		t.Errorf("Tags = %v, хочу 2 элемента", card.Memory.Tags)
	}
	if len(card.Memory.Links) != 1 || card.Memory.Links[0].Dst != other.Id || card.Memory.Links[0].Rel != memory.RelRelated {
		t.Errorf("Links = %+v", card.Memory.Links)
	}
	if len(card.Neighbors) != 1 || card.Neighbors[0].Id != other.Id {
		t.Errorf("Neighbors = %+v, хочу [%s]", card.Neighbors, other.Id)
	}

	// Неизвестный id -> 404.
	resp2 := getJSON(t, srv.URL+"/api/memories/01ARZ3NDEKTSV4RRFFQ69G5FAV", nil)
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("статус для неизвестного id = %d, хочу 404", resp2.StatusCode)
	}
}

// TestHandleGetMemory_TaskHierarchy: карточка эпика несёт детей (задачи с
// part_of на эпик, в порядке доски) и прогресс; карточка истории — родителя;
// карточка не-задачи — parent/progress null и пустой children (задача 15.3).
func TestHandleGetMemory_TaskHierarchy(t *testing.T) {
	s := newTestWebStore(t)
	title := func(v string) *string { return &v }
	epic := putTestMemory(t, s, func(m *memory.Memory) {
		m.Kind, m.Status, m.Type, m.Summary = memory.KindTask, memory.StatusDoing, memory.TaskTypeEpic, title("Kanban-доска")
	})
	storyDone := putTestMemory(t, s, func(m *memory.Memory) {
		m.Kind, m.Status, m.Type, m.Summary = memory.KindTask, memory.StatusDone, memory.TaskTypeStory, title("Модель v2")
		m.Importance = 5
		m.Links = []memory.Link{{Dst: epic.Id, Rel: memory.RelPartOf}}
	})
	storyOpen := putTestMemory(t, s, func(m *memory.Memory) {
		m.Kind, m.Status, m.Type, m.Summary = memory.KindTask, memory.StatusTodo, memory.TaskTypeStory, title("Иерархия")
		m.Links = []memory.Link{{Dst: epic.Id, Rel: memory.RelPartOf}}
	})
	// related-сосед и не-задача с part_of — не дети.
	putTestMemory(t, s, func(m *memory.Memory) {
		m.Links = []memory.Link{{Dst: epic.Id, Rel: memory.RelRelated}}
	})
	fact := putTestMemory(t, s, func(m *memory.Memory) {
		m.Links = []memory.Link{{Dst: epic.Id, Rel: memory.RelPartOf}}
	})

	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	var card memoryCardResponse
	if resp := getJSON(t, srv.URL+"/api/memories/"+epic.Id, &card); resp.StatusCode != http.StatusOK {
		t.Fatalf("статус = %d, хочу 200", resp.StatusCode)
	}
	if card.Parent != nil {
		t.Errorf("Parent эпика = %+v, хочу nil", card.Parent)
	}
	if len(card.Children) != 2 || card.Children[0].Id != storyOpen.Id || card.Children[1].Id != storyDone.Id {
		t.Errorf("Children = %+v, хочу [%s (todo), %s (done)] в порядке стадий", card.Children, storyOpen.Id, storyDone.Id)
	}
	if len(card.Children) == 2 && (card.Children[0].Status != "todo" || card.Children[0].Type != "story" || card.Children[0].Summary == nil || *card.Children[0].Summary != "Иерархия") {
		t.Errorf("Children[0] = %+v, хочу {todo story Иерархия}", card.Children[0])
	}
	if card.Progress == nil || card.Progress.Done != 1 || card.Progress.Total != 2 {
		t.Errorf("Progress = %+v, хочу {1 2}", card.Progress)
	}

	var storyCard memoryCardResponse
	getJSON(t, srv.URL+"/api/memories/"+storyOpen.Id, &storyCard)
	if storyCard.Parent == nil || storyCard.Parent.Id != epic.Id || storyCard.Parent.Type != "epic" || storyCard.Parent.Status != "doing" {
		t.Errorf("Parent истории = %+v, хочу эпик %s (epic, doing)", storyCard.Parent, epic.Id)
	}
	if len(storyCard.Children) != 0 || storyCard.Progress == nil || storyCard.Progress.Total != 0 {
		t.Errorf("у истории без детей Children = %+v, Progress = %+v; хочу [] и {0 0}", storyCard.Children, storyCard.Progress)
	}

	var factCard memoryCardResponse
	getJSON(t, srv.URL+"/api/memories/"+fact.Id, &factCard)
	if factCard.Parent != nil || factCard.Progress != nil || factCard.Children == nil || len(factCard.Children) != 0 {
		t.Errorf("у не-задачи Parent/Progress = %+v/%+v, Children = %+v; хочу null/null/[]", factCard.Parent, factCard.Progress, factCard.Children)
	}
}

// deleteJSON шлёт DELETE и декодирует JSON-тело ответа в out (если не nil).
func deleteJSON(t *testing.T, url string, out any) *http.Response {
	t.Helper()
	return doJSON(t, http.MethodDelete, url, out)
}

// patchJSON шлёт PATCH с телом bodyJSON (сырая строка — так тест может
// послать как валидный, так и намеренно битый JSON) и декодирует ответ в out
// (если не nil).
func patchJSON(t *testing.T, url, bodyJSON string, out any) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPatch, url, strings.NewReader(bodyJSON))
	if err != nil {
		t.Fatalf("NewRequest PATCH %s: %v", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH %s: %v", url, err)
	}
	if out != nil {
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode JSON от %s: %v", url, err)
		}
	}
	return resp
}

// TestHandlePatchMemory_Importance — PATCH importance меняет значение в БД,
// сдвигает updated_at, но НЕ трогает accessed_at (decay-часы не сбрасываются
// правкой метаданных — тот же инвариант, что и в DESIGN.md §4.1: accessed_at
// сигнализирует ОБРАЩЕНИЕ к факту при recall, а не редактирование).
func TestHandlePatchMemory_Importance(t *testing.T) {
	s := newTestWebStore(t)
	accessedAt := int64(500)
	m := putTestMemory(t, s, func(m *memory.Memory) {
		m.Importance = 2
		m.AccessedAt = &accessedAt
		m.UpdatedAt = 1000
	})

	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	var got memoryDTO
	resp := patchJSON(t, srv.URL+"/api/memories/"+m.Id, `{"importance":5}`, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус = %d, хочу 200", resp.StatusCode)
	}
	if got.Importance != 5 {
		t.Errorf("Importance в ответе = %d, хочу 5", got.Importance)
	}

	after, err := s.Get(context.Background(), m.Id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.Importance != 5 {
		t.Errorf("Importance в БД = %d, хочу 5", after.Importance)
	}
	if after.UpdatedAt <= 1000 {
		t.Errorf("UpdatedAt = %d, хочу сдвиг вперёд от 1000", after.UpdatedAt)
	}
	if after.AccessedAt == nil || *after.AccessedAt != accessedAt {
		t.Errorf("AccessedAt = %v, хочу неизменный %d (PATCH не должен сбрасывать decay-часы)", after.AccessedAt, accessedAt)
	}
}

// TestHandlePatchMemory_TagsReplaceAndFTS — PATCH tags заменяет набор целиком
// (не аддитивно), новый тег находится через FTS5 (тумблер триггеров
// tags_ai/tags_ad, см. sqlite.go), а tags:[] снимает все теги.
func TestHandlePatchMemory_TagsReplaceAndFTS(t *testing.T) {
	s := newTestWebStore(t)
	m := putTestMemory(t, s, func(m *memory.Memory) { m.Tags = []string{"old1", "old2"} })

	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	var got memoryDTO
	resp := patchJSON(t, srv.URL+"/api/memories/"+m.Id, `{"tags":["new1","new2"]}`, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус = %d, хочу 200", resp.StatusCode)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "new1" || got.Tags[1] != "new2" {
		t.Errorf("Tags = %v, хочу [new1 new2] (полная замена, старые теги сняты)", got.Tags)
	}

	var found searchResponse
	respSearch := getJSON(t, srv.URL+"/api/search?mode=lexical&q=new1", &found)
	if respSearch.StatusCode != http.StatusOK {
		t.Fatalf("статус поиска = %d, хочу 200", respSearch.StatusCode)
	}
	if len(found.Items) != 1 || found.Items[0].Id != m.Id {
		t.Fatalf("поиск по новому тегу = %+v, хочу найти %q (FTS синхронизирован)", found.Items, m.Id)
	}

	var got2 memoryDTO
	patchJSON(t, srv.URL+"/api/memories/"+m.Id, `{"tags":[]}`, &got2)
	if len(got2.Tags) != 0 {
		t.Errorf("Tags после tags:[] = %v, хочу пусто (снять все теги)", got2.Tags)
	}
}

// TestHandlePatchMemory_PartialDoesNotTouchOtherField — частичный PATCH
// (только importance или только tags) не трогает второе поле.
func TestHandlePatchMemory_PartialDoesNotTouchOtherField(t *testing.T) {
	s := newTestWebStore(t)
	m := putTestMemory(t, s, func(m *memory.Memory) { m.Importance = 4; m.Tags = []string{"keep"} })

	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	var got memoryDTO
	patchJSON(t, srv.URL+"/api/memories/"+m.Id, `{"importance":1}`, &got)
	if len(got.Tags) != 1 || got.Tags[0] != "keep" {
		t.Errorf("Tags после PATCH только importance = %v, хочу [keep] нетронутым", got.Tags)
	}

	var got2 memoryDTO
	patchJSON(t, srv.URL+"/api/memories/"+m.Id, `{"tags":["only"]}`, &got2)
	if got2.Importance != 1 {
		t.Errorf("Importance после PATCH только tags = %d, хочу 1 нетронутым", got2.Importance)
	}
}

// TestHandlePatchMemory_ImportanceClamped — importance вне 1..5 не отклоняется
// 400, а зажимается (симметрично MCP memory_update, см. годок ClampImportance).
func TestHandlePatchMemory_ImportanceClamped(t *testing.T) {
	s := newTestWebStore(t)
	m := putTestMemory(t, s, func(m *memory.Memory) { m.Importance = 3 })

	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	var got memoryDTO
	resp := patchJSON(t, srv.URL+"/api/memories/"+m.Id, `{"importance":99}`, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус = %d, хочу 200 (кламп, не 400)", resp.StatusCode)
	}
	if got.Importance != 5 {
		t.Errorf("Importance = %d, хочу 5 (зажато сверху)", got.Importance)
	}
}

// TestHandlePatchMemory_UnknownFieldBadRequest — присутствие поля сверх
// importance/tags (content и т.п.) -> 400: защита контракта от тихого
// расширения (content/kind/scope/links морда не редактирует никогда).
func TestHandlePatchMemory_UnknownFieldBadRequest(t *testing.T) {
	s := newTestWebStore(t)
	m := putTestMemory(t, s, func(m *memory.Memory) {})

	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	// kind (как и scope) из браузера не меняется — жёсткая граница §12; content
	// с Этапа 16 допустим, поэтому лишним полем здесь служит kind.
	resp := patchJSON(t, srv.URL+"/api/memories/"+m.Id, `{"kind":"episodic"}`, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("статус = %d, хочу 400 (kind не допускается в PATCH)", resp.StatusCode)
	}
}

// TestHandlePatchMemory_NotFound — PATCH несуществующего id -> 404.
func TestHandlePatchMemory_NotFound(t *testing.T) {
	s := newTestWebStore(t)
	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	resp := patchJSON(t, srv.URL+"/api/memories/01ARZ3NDEKTSV4RRFFQ69G5FAV", `{"importance":4}`, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("статус = %d, хочу 404", resp.StatusCode)
	}
}

// TestHandlePatchMemory_SupersededConflict — PATCH архивной (superseded)
// записи -> 409: правка метаданных тумстоуна недоступна.
func TestHandlePatchMemory_SupersededConflict(t *testing.T) {
	s := newTestWebStore(t)
	x := putTestMemory(t, s, func(m *memory.Memory) {})
	y := putTestMemory(t, s, func(m *memory.Memory) {})
	if err := s.Supersede(context.Background(), x.Id, y.Id); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	resp := patchJSON(t, srv.URL+"/api/memories/"+x.Id, `{"importance":4}`, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("статус = %d, хочу 409 (архивная запись)", resp.StatusCode)
	}
}

// TestHandlePatchMemory_MalformedJSONBadRequest — битый JSON в теле -> 400.
func TestHandlePatchMemory_MalformedJSONBadRequest(t *testing.T) {
	s := newTestWebStore(t)
	m := putTestMemory(t, s, func(m *memory.Memory) {})

	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	resp := patchJSON(t, srv.URL+"/api/memories/"+m.Id, `{"importance":`, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("статус = %d, хочу 400 (битый JSON)", resp.StatusCode)
	}
}

// TestHandleForgetMemory — DELETE /api/memories/{id} мягко удаляет живую
// запись (тумстоун через write.Writer.Forget): 200 {ok:true,id:...}, запись
// выпадает из живого списка, а Store напрямую показывает superseded_by == id.
func TestHandleForgetMemory(t *testing.T) {
	s := newTestWebStore(t)
	m := putTestMemory(t, s, func(m *memory.Memory) {})

	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	var got forgetResponse
	resp := deleteJSON(t, srv.URL+"/api/memories/"+m.Id, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус = %d, хочу 200", resp.StatusCode)
	}
	if !got.Ok || got.Id != m.Id {
		t.Errorf("тело = %+v, хочу {ok:true id:%s}", got, m.Id)
	}

	// Выпала из живого списка.
	var list memoriesResponse
	getJSON(t, srv.URL+"/api/memories", &list)
	for _, item := range list.Items {
		if item.Id == m.Id {
			t.Errorf("запись %q всё ещё в живом списке после forget", m.Id)
		}
	}

	// Store напрямую подтверждает тумстоун.
	after, err := s.Get(context.Background(), m.Id)
	if err != nil {
		t.Fatalf("Get после forget: %v", err)
	}
	if after.SupersededBy == nil || *after.SupersededBy != m.Id {
		t.Errorf("SupersededBy = %v, хочу указатель на саму запись %q (тумстоун)", after.SupersededBy, m.Id)
	}
}

// TestHandleForgetMemory_NotFound — DELETE несуществующего id -> 404 с
// JSON-телом ошибки.
func TestHandleForgetMemory_NotFound(t *testing.T) {
	s := newTestWebStore(t)

	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	var got errResponse
	resp := deleteJSON(t, srv.URL+"/api/memories/01ARZ3NDEKTSV4RRFFQ69G5FAV", &got)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("статус = %d, хочу 404", resp.StatusCode)
	}
	if got.Error == "" {
		t.Error("error пуст, хочу непустое сообщение")
	}
}

// TestHandleForgetMemory_AlreadyArchivedIdempotent — DELETE уже архивной
// записи (superseded_by указывает на настоящую заменяющую запись, а не на
// себя) должен быть идемпотентным: 200 {ok:true}, но указатель supersededBy
// не должен переписываться в self-тумстоун — иначе provenance (кем реально
// заменена запись) теряется.
func TestHandleForgetMemory_AlreadyArchivedIdempotent(t *testing.T) {
	s := newTestWebStore(t)
	x := putTestMemory(t, s, func(m *memory.Memory) {})
	y := putTestMemory(t, s, func(m *memory.Memory) {})
	if err := s.Supersede(context.Background(), x.Id, y.Id); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	var got forgetResponse
	resp := deleteJSON(t, srv.URL+"/api/memories/"+x.Id, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус = %d, хочу 200", resp.StatusCode)
	}
	if !got.Ok || got.Id != x.Id {
		t.Errorf("тело = %+v, хочу {ok:true id:%s}", got, x.Id)
	}

	after, err := s.Get(context.Background(), x.Id)
	if err != nil {
		t.Fatalf("Get после forget: %v", err)
	}
	if after.SupersededBy == nil || *after.SupersededBy != y.Id {
		t.Errorf("SupersededBy = %v, хочу указатель на %q (provenance сохранён, не self-тумстоун)", after.SupersededBy, y.Id)
	}
}

// TestHandleGetMemory_EmbeddingDegradesWithoutVectorLayer — карточка записи
// без векторного слоя (нет вектора, ResetVectorIndex не вызывался) деградирует:
// embedding.embedded=false, semanticNeighbors — пустой массив (не null),
// статус 200 (не 500).
func TestHandleGetMemory_EmbeddingDegradesWithoutVectorLayer(t *testing.T) {
	s := newTestWebStore(t)
	m := putTestMemory(t, s, func(m *memory.Memory) {})

	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/memories/" + m.Id)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус = %d, хочу 200 (деградация без падения)", resp.StatusCode)
	}

	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}

	embedding, ok := raw["embedding"].(map[string]any)
	if !ok {
		t.Fatalf("нет ключа embedding в ответе: %+v", raw)
	}
	if embedded, ok := embedding["embedded"].(bool); !ok || embedded {
		t.Errorf("embedding.embedded = %v, хочу false", embedding["embedded"])
	}

	semanticNeighbors, ok := raw["semanticNeighbors"].([]any)
	if !ok {
		t.Fatalf("semanticNeighbors не массив: %+v", raw["semanticNeighbors"])
	}
	if len(semanticNeighbors) != 0 {
		t.Errorf("semanticNeighbors = %v, хочу пустой массив", semanticNeighbors)
	}
}

// TestHandleGetMemory_SemanticNeighborsWithVectors — позитивный кейс: после
// ResetVectorIndex+PutVector карточка отдаёт embedding.embedded=true и
// semanticNeighbors с полем distance, посчитанным из Store.SemanticNeighbors.
func TestHandleGetMemory_SemanticNeighborsWithVectors(t *testing.T) {
	s := newTestWebStore(t)
	anchor := putTestMemory(t, s, func(m *memory.Memory) {})
	near := putTestMemory(t, s, func(m *memory.Memory) {})

	ctx := context.Background()
	if err := s.ResetVectorIndex(ctx, "bge-m3", 3); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}
	if err := s.PutVector(ctx, anchor.Id, []float32{1, 0, 0}); err != nil {
		t.Fatalf("PutVector anchor: %v", err)
	}
	if err := s.PutVector(ctx, near.Id, []float32{0.9, 0.1, 0}); err != nil {
		t.Fatalf("PutVector near: %v", err)
	}

	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	var card memoryCardResponse
	resp := getJSON(t, srv.URL+"/api/memories/"+anchor.Id, &card)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус = %d, хочу 200", resp.StatusCode)
	}

	if !card.Embedding.Embedded || card.Embedding.Model != "bge-m3" || card.Embedding.Dim != 3 {
		t.Errorf("Embedding = %+v, хочу {true bge-m3 3}", card.Embedding)
	}

	if len(card.SemanticNeighbors) != 1 || card.SemanticNeighbors[0].Id != near.Id {
		t.Fatalf("SemanticNeighbors = %+v, хочу [%s]", card.SemanticNeighbors, near.Id)
	}
	if card.SemanticNeighbors[0].Distance <= 0 {
		t.Errorf("Distance = %v, хочу > 0", card.SemanticNeighbors[0].Distance)
	}
}

func TestHandleGetMemory_NullableFieldsAndCamelCase(t *testing.T) {
	s := newTestWebStore(t)
	m := putTestMemory(t, s, func(m *memory.Memory) {})

	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/memories/" + m.Id)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	memRaw, ok := raw["memory"].(map[string]any)
	if !ok {
		t.Fatalf("нет ключа memory в ответе: %+v", raw)
	}

	for _, key := range []string{"id", "kind", "scope", "content", "summary", "importance",
		"createdAt", "updatedAt", "accessedAt", "accessCount", "supersededBy", "source", "tags", "links"} {
		if _, ok := memRaw[key]; !ok {
			t.Errorf("отсутствует ключ %q в DTO: %+v", key, memRaw)
		}
	}

	for _, key := range []string{"summary", "accessedAt", "supersededBy", "source"} {
		if memRaw[key] != nil {
			t.Errorf("%s = %v, хочу null (nullable-поле без значения)", key, memRaw[key])
		}
	}

	if tags, ok := memRaw["tags"].([]any); !ok || len(tags) != 0 {
		t.Errorf("tags = %v, хочу пустой массив", memRaw["tags"])
	}
	if links, ok := memRaw["links"].([]any); !ok || len(links) != 0 {
		t.Errorf("links = %v, хочу пустой массив", memRaw["links"])
	}
}

func TestHandleSearch(t *testing.T) {
	s := newTestWebStore(t)
	putTestMemory(t, s, func(m *memory.Memory) { m.Content = "уникальная фраза про жирафов" })
	putTestMemory(t, s, func(m *memory.Memory) { m.Content = "нечто совсем другое" })

	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	var found searchResponse
	resp := getJSON(t, srv.URL+"/api/search?q=жирафов", &found)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус = %d, хочу 200", resp.StatusCode)
	}
	if len(found.Items) != 1 {
		t.Fatalf("items = %+v, хочу 1 совпадение", found.Items)
	}
	if found.Items[0].Source != "lexical" {
		t.Errorf("Source = %q, хочу lexical", found.Items[0].Source)
	}

	// Пустой q -> 400.
	resp2, err := http.Get(srv.URL + "/api/search?q=%20%20")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("статус для пустого q = %d, хочу 400", resp2.StatusCode)
	}

	resp3, err := http.Get(srv.URL + "/api/search")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp3.StatusCode != http.StatusBadRequest {
		t.Errorf("статус без q = %d, хочу 400", resp3.StatusCode)
	}
}

// TestHandleSearch_InvalidModeBadRequest — неизвестное значение mode -> 400.
func TestHandleSearch_InvalidModeBadRequest(t *testing.T) {
	s := newTestWebStore(t)
	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/search?q=x&mode=bogus")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("статус для mode=bogus = %d, хочу 400", resp.StatusCode)
	}
}

// TestHandleSearch_NoopEmbedderDegradesToLexicalWithVectorFalse — задача 10.1:
// дефолт mode=semantic с Embedder-Noop (векторный слой выключен) деградирует
// молча до чистого FTS5 — 200, не 500, и searchResponse.Vector=false, чтобы
// фронт показал плашку «поиск по смыслу недоступен».
func TestHandleSearch_NoopEmbedderDegradesToLexicalWithVectorFalse(t *testing.T) {
	s := newTestWebStore(t)
	putTestMemory(t, s, func(m *memory.Memory) { m.Content = "уникальная фраза про жирафов" })

	srv := httptest.NewServer(newTestWebHandler(s)) // embed.NewNoop() по умолчанию
	defer srv.Close()

	var got searchResponse
	resp := getJSON(t, srv.URL+"/api/search?q=жирафов", &got) // mode не задан -> semantic
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус = %d, хочу 200 (деградация, не 500)", resp.StatusCode)
	}
	if got.Vector {
		t.Errorf("Vector = true, хочу false (Embedder-Noop, векторный слой выключен)")
	}
	if len(got.Items) != 1 || got.Items[0].Source != "lexical" {
		t.Fatalf("Items = %+v, хочу 1 запись через lexical (фолбэк FTS5)", got.Items)
	}
}

// TestHandleSearch_ModeDefaultMatchesExplicitSemantic — дефолт /api/search без
// mode должен вести себя так же, как явный mode=semantic (задача 10.1).
func TestHandleSearch_ModeDefaultMatchesExplicitSemantic(t *testing.T) {
	s := newTestWebStore(t)
	putTestMemory(t, s, func(m *memory.Memory) { m.Content = "уникальная фраза про жирафов" })

	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	var withoutMode, explicitSemantic searchResponse
	getJSON(t, srv.URL+"/api/search?q=жирафов", &withoutMode)
	getJSON(t, srv.URL+"/api/search?q=жирафов&mode=semantic", &explicitSemantic)

	if len(withoutMode.Items) != len(explicitSemantic.Items) || withoutMode.Vector != explicitSemantic.Vector {
		t.Errorf("mode по умолчанию должен совпадать с явным mode=semantic: %+v vs %+v", withoutMode, explicitSemantic)
	}
}

// TestHandleSearch_ModeLexicalUnaffectedByVectorLayer — mode=lexical сохраняет
// прежний контракт задачи 6.2: всегда чистый FTS5 через Store.SearchLexical,
// даже когда у морды есть рабочий Embedder (fakeEmbedder) — векторный источник
// в lexical-режиме не подключается вовсе.
func TestHandleSearch_ModeLexicalUnaffectedByVectorLayer(t *testing.T) {
	s := newTestWebStore(t)
	putTestMemory(t, s, func(m *memory.Memory) { m.Content = "уникальная фраза про жирафов" })

	fe := &fakeEmbedder{dim: 3, vec: []float32{1, 0, 0}}
	srv := httptest.NewServer(newTestWebHandlerWithEmbedder(s, fe))
	defer srv.Close()

	var got searchResponse
	resp := getJSON(t, srv.URL+"/api/search?q=жирафов&mode=lexical", &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("статус = %d, хочу 200", resp.StatusCode)
	}
	if len(got.Items) != 1 || got.Items[0].Source != "lexical" {
		t.Fatalf("Items = %+v, хочу 1 запись через lexical", got.Items)
	}
	if fe.dim <= 0 {
		t.Fatalf("тест невалиден: fakeEmbedder должен быть включён (dim>0)")
	}
}

// TestHandleSearch_SemanticFindsParaphraseLexicalDoesNot — центральный сценарий
// задачи 10.1 (паттерн перенесён из recall.TestRecall_FoundOnlyByVector): запись
// без единого общего лексического токена с запросом находится в mode=semantic
// через векторный источник Candidates, но не находится в mode=lexical.
func TestHandleSearch_SemanticFindsParaphraseLexicalDoesNot(t *testing.T) {
	ctx := context.Background()
	s := newTestWebStore(t)

	if err := s.ResetVectorIndex(ctx, "fake-model", 3); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}

	m := putTestMemory(t, s, func(m *memory.Memory) {
		m.Content = "Сборка тесты проекта запускаются через Makefile"
	})
	if err := s.PutVector(ctx, m.Id, []float32{1, 0, 0}); err != nil {
		t.Fatalf("PutVector: %v", err)
	}

	fe := &fakeEmbedder{dim: 3, vec: []float32{1, 0, 0}}
	srv := httptest.NewServer(newTestWebHandlerWithEmbedder(s, fe))
	defer srv.Close()

	// Перефразировка без единого общего токена с сохранённым Content.
	q := url.QueryEscape("собирать тестировать репозиторий")

	var semantic searchResponse
	respSem := getJSON(t, srv.URL+"/api/search?mode=semantic&q="+q, &semantic)
	if respSem.StatusCode != http.StatusOK {
		t.Fatalf("статус (semantic) = %d, хочу 200", respSem.StatusCode)
	}
	if !semantic.Vector {
		t.Errorf("Vector = false, хочу true (fakeEmbedder включён)")
	}
	if len(semantic.Items) != 1 || semantic.Items[0].Id != m.Id {
		t.Fatalf("semantic.Items = %+v, хочу найти %q через вектор", semantic.Items, m.Id)
	}
	if semantic.Items[0].Source != "vector" {
		t.Errorf("Source = %q, хочу vector", semantic.Items[0].Source)
	}

	var lexical searchResponse
	respLex := getJSON(t, srv.URL+"/api/search?mode=lexical&q="+q, &lexical)
	if respLex.StatusCode != http.StatusOK {
		t.Fatalf("статус (lexical) = %d, хочу 200", respLex.StatusCode)
	}
	if len(lexical.Items) != 0 {
		t.Errorf("lexical.Items = %+v, хочу пусто (перефразировка не должна матчиться лексически)", lexical.Items)
	}
}

func TestUnknownAPIPathNotFound(t *testing.T) {
	s := newTestWebStore(t)
	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/does-not-exist")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("статус = %d, хочу 404 (не должно перехватываться статикой)", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, хочу JSON (а не plain-text 404 от файлового хендлера)", ct)
	}
	var body errResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	if body.Error == "" {
		t.Error("error пуст, хочу непустое сообщение")
	}
}

func TestStaticIndexServed(t *testing.T) {
	s := newTestWebStore(t)
	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("статус = %d, хочу 200 (index.html из embed.FS)", resp.StatusCode)
	}
}

// TestRunWeb_LoadsDotEnvBeforeOpeningStore закрепляет регрессию: `memo web`
// была единственной подкомандой, которая не вызывала loadDotEnvForConfig.
// Пока .env хранил только ключи LLM-провайдеров, морде не нужные, это ничего
// не ломало — но с driver="postgres" там лежит MEMO_POSTGRES_PASSWORD, секрет
// самого хранилища, и `memo web -config …` падал на аутентификации там, где
// recall/save/doctor/migrate работали.
//
// Проверяем именно порядок: .env должен быть прочитан ДО открытия store.
// Конфиг указывает на заведомо недоступный адрес, поэтому runWeb обязан
// вернуть ошибку подключения — но к этому моменту переменная из .env уже
// должна стоять в окружении.
func TestRunWeb_LoadsDotEnvBeforeOpeningStore(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agentmemory.toml")
	cfg := "" +
		"[store]\n" +
		"driver  = \"postgres\"\n" +
		"host    = \"127.0.0.1\"\n" +
		"port    = 1\n" + // никто не слушает — connection refused сразу, без сети
		"user    = \"memo\"\n" +
		"dbname  = \"memo\"\n" +
		"sslmode = \"disable\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("запись конфига: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("MEMO_POSTGRES_PASSWORD=from-dotenv\n"), 0o600); err != nil {
		t.Fatalf("запись .env: %v", err)
	}

	// Пустое значение в shell-env перекрывается значением из .env (см.
	// TestLoadDotEnv_EmptyEnvOverriddenByDotEnv); t.Setenv вернёт прежнее
	// окружение после теста.
	t.Setenv(memoPostgresPasswordEnv, "")

	err := runWeb([]string{"-config", cfgPath}, io.Discard)
	if err == nil {
		t.Fatal("runWeb вернула nil, хочу ошибку подключения к недоступному хосту")
	}

	if got := os.Getenv(memoPostgresPasswordEnv); got != "from-dotenv" {
		t.Errorf("%s = %q, хочу \"from-dotenv\" — .env не загружен до открытия хранилища", memoPostgresPasswordEnv, got)
	}
}

// postJSON шлёт POST с телом bodyJSON (сырая строка — тест может послать и
// намеренно битый JSON) и декодирует ответ в out (если не nil).
func postJSON(t *testing.T, url, bodyJSON string, out any) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(bodyJSON))
	if err != nil {
		t.Fatalf("NewRequest POST %s: %v", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	if out != nil {
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode JSON от %s: %v", url, err)
		}
	}
	return resp
}

// newTestWebHandlerWithInstance — handler с заданным webInstance: нужен
// тестам, которым важен домашний scope инстанса (дефолт scope у POST).
func newTestWebHandlerWithInstance(s store.Store, inst webInstance) http.Handler {
	w := write.New(s, embed.NewNoop())
	rc := recall.New(s, config.RecallConfig{DefaultTokenBudget: 800, Weights: testRecallWeights()}, reason.NewNoop(), embed.NewNoop())
	return newWebHandler(s, w, rc, embed.NewNoop(), inst)
}

// TestHandleCreateMemory_TaskAppearsOnBoard — успешное создание задачи
// (Этап 17, задача 17.1): 201 + memoryDTO, поля задачи сохранены, карточка
// видна в /api/board в колонке своей стадии.
func TestHandleCreateMemory_TaskAppearsOnBoard(t *testing.T) {
	s := newTestWebStore(t)
	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	var created memoryDTO
	resp := postJSON(t, srv.URL+"/api/memories", `{
		"kind":"task","scope":"project:agentmemory","type":"story",
		"summary":"Завести доску","content":"полный текст задачи",
		"status":"doing","importance":5,"tags":["web","доска"],
		"branch":"feat/board","workflow":"dev"
	}`, &created)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("статус = %d, ожидался 201", resp.StatusCode)
	}
	if created.Id == "" {
		t.Fatalf("id пустой: %+v", created)
	}
	if created.Kind != string(memory.KindTask) || created.Status != "doing" {
		t.Fatalf("kind/status не сохранены: %+v", created)
	}
	if created.Type != "story" {
		t.Fatalf("type не сохранён: %+v", created)
	}
	if created.Branch != "feat/board" || created.Workflow != "dev" {
		t.Fatalf("branch/workflow не сохранены: %+v", created)
	}
	if created.Importance != 5 || len(created.Tags) != 2 {
		t.Fatalf("importance/tags не сохранены: %+v", created)
	}

	var board boardResponse
	getJSON(t, srv.URL+"/api/board?scope=project:agentmemory", &board)
	found := false
	for _, col := range board.Columns {
		for _, c := range col.Cards {
			if c.Id == created.Id {
				if col.Status != "doing" {
					t.Fatalf("карточка в колонке %q, ожидалась doing", col.Status)
				}
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("созданная задача не видна на доске")
	}
}

// TestHandleCreateMemory_NonTaskBadRequest — из браузера создаётся только
// kind=task: любой другой вид (и пустой kind) — 400, запись не появляется.
func TestHandleCreateMemory_NonTaskBadRequest(t *testing.T) {
	s := newTestWebStore(t)
	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	for _, kind := range []string{"semantic", "", "debt"} {
		body := `{"kind":"` + kind + `","scope":"project:agentmemory","summary":"x","content":"y"}`
		resp := postJSON(t, srv.URL+"/api/memories", body, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("kind=%q: статус = %d, ожидался 400", kind, resp.StatusCode)
		}
	}
	var list memoriesResponse
	getJSON(t, srv.URL+"/api/memories", &list)
	if len(list.Items) != 0 {
		t.Fatalf("ничего не должно было создаться, а в памяти %d записей", len(list.Items))
	}
}

// TestHandleCreateMemory_EmptySummaryBadRequest — заголовок задачи
// обязателен (write.ErrTaskSummaryRequired) → 400 от домена, не 500.
func TestHandleCreateMemory_EmptySummaryBadRequest(t *testing.T) {
	s := newTestWebStore(t)
	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/api/memories", `{"kind":"task","scope":"project:agentmemory","summary":"  ","content":"текст"}`, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("статус = %d, ожидался 400", resp.StatusCode)
	}
}

// TestHandleCreateMemory_PartOfLinksParent — links с rel=part_of создаёт
// ребро к родителю (задача внутрь истории); ребро видно в карточке родителя
// как дочерняя запись.
func TestHandleCreateMemory_PartOfLinksParent(t *testing.T) {
	s := newTestWebStore(t)
	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	var parent memoryDTO
	postJSON(t, srv.URL+"/api/memories", `{"kind":"task","scope":"project:agentmemory","type":"story","summary":"История","content":"текст"}`, &parent)

	var child memoryDTO
	resp := postJSON(t, srv.URL+"/api/memories", `{"kind":"task","scope":"project:agentmemory","type":"task","summary":"Задача","content":"текст",
		"links":[{"id":"`+parent.Id+`","rel":"part_of"}]}`, &child)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("статус = %d, ожидался 201", resp.StatusCode)
	}

	var card memoryCardResponse
	getJSON(t, srv.URL+"/api/memories/"+parent.Id, &card)
	if len(card.Children) != 1 || card.Children[0].Id != child.Id {
		t.Fatalf("дочерняя задача не привязана к родителю: %+v", card.Children)
	}
}

// TestHandleCreateMemory_RejectsNonPartOfLink — при создании допустима
// единственная связь «входит в»: blocked_by и прочие рёбра ставятся уже на
// существующей карточке через PATCH.
func TestHandleCreateMemory_RejectsNonPartOfLink(t *testing.T) {
	s := newTestWebStore(t)
	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	blocker := putTestMemory(t, s, func(m *memory.Memory) { m.Kind = memory.KindTask })
	resp := postJSON(t, srv.URL+"/api/memories", `{"kind":"task","scope":"project:agentmemory","summary":"x","content":"y",
		"links":[{"id":"`+blocker.Id+`","rel":"blocked_by"}]}`, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("статус = %d, ожидался 400", resp.StatusCode)
	}
}

// TestHandleCreateMemory_EmptyScopeFallsBackToHomeScope — пустой scope берёт
// домашний scope инстанса (как MCP memory_save), а не падает ErrEmptyScope.
func TestHandleCreateMemory_EmptyScopeFallsBackToHomeScope(t *testing.T) {
	s := newTestWebStore(t)
	srv := httptest.NewServer(newTestWebHandlerWithInstance(s, webInstance{HomeScope: "project:home"}))
	defer srv.Close()

	var created memoryDTO
	resp := postJSON(t, srv.URL+"/api/memories", `{"kind":"task","summary":"Без scope","content":"текст"}`, &created)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("статус = %d, ожидался 201", resp.StatusCode)
	}
	if created.Scope != "project:home" {
		t.Fatalf("scope = %q, ожидался домашний project:home", created.Scope)
	}
}

// TestHandleCreateMemory_EmptyContentUsesSummary — пустое описание не ошибка:
// карточка заводится заголовком (§2.6 макета), иначе ErrEmptyContent не дал
// бы создать быструю задачу с доски.
func TestHandleCreateMemory_EmptyContentUsesSummary(t *testing.T) {
	s := newTestWebStore(t)
	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	var created memoryDTO
	resp := postJSON(t, srv.URL+"/api/memories", `{"kind":"task","scope":"project:agentmemory","summary":"Только заголовок"}`, &created)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("статус = %d, ожидался 201", resp.StatusCode)
	}
	if created.Content != "Только заголовок" {
		t.Fatalf("content = %q, ожидался заголовок", created.Content)
	}
}

// TestHandleCreateMemory_UnknownFieldBadRequest — тело POST закрыто
// whitelist-ом: лишнее поле → 400 (та же защита контракта, что у PATCH).
func TestHandleCreateMemory_UnknownFieldBadRequest(t *testing.T) {
	s := newTestWebStore(t)
	srv := httptest.NewServer(newTestWebHandler(s))
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/api/memories", `{"kind":"task","summary":"x","content":"y","ttl":"72h"}`, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("статус = %d, ожидался 400", resp.StatusCode)
	}
}
