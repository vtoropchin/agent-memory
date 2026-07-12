package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/oklog/ulid/v2"

	"agentmemory/internal/memory"
	"agentmemory/internal/memory/embed"
	"agentmemory/internal/memory/store"
	"agentmemory/internal/memory/write"
)

func newTestWebStore(t *testing.T) store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "memory.db")
	s, err := store.Open(path)
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

// newTestWebHandler собирает handler web-морды для теста поверх s, с
// write.Writer на embed.NewNoop() (Forget не требует эмбеддера).
func newTestWebHandler(s store.Store) http.Handler {
	return newWebHandler(s, write.New(s, embed.NewNoop()))
}

// putTestMemory кладёт в s запись памяти с разумными дефолтами, переопределяя
// только поля, важные для конкретного теста (через mutate).
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

// deleteJSON шлёт DELETE и декодирует JSON-тело ответа в out (если не nil).
func deleteJSON(t *testing.T, url string, out any) *http.Response {
	t.Helper()
	return doJSON(t, http.MethodDelete, url, out)
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
