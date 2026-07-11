package write

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"agentmemory/internal/memory"
	"agentmemory/internal/memory/embed"
	"agentmemory/internal/memory/store"
)

// fakeEmbedder — управляемая тестами реализация embed.Embedder: возвращает
// фиксированный вектор Vec (или ошибку Err) и считает число вызовов Embed.
type fakeEmbedder struct {
	Vec       []float32
	Err       error
	CallCount int
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.CallCount++
	if f.Err != nil {
		return nil, f.Err
	}
	vecs := make([][]float32, len(texts))
	for i := range texts {
		vecs[i] = f.Vec
	}
	return vecs, nil
}

func (f *fakeEmbedder) Model() string { return "fake" }
func (f *fakeEmbedder) Dim() int      { return len(f.Vec) }

var _ embed.Embedder = (*fakeEmbedder)(nil)

func newTestWriter(t *testing.T) (*Writer, store.Store) {
	t.Helper()
	return newTestWriterWithEmbedder(t, embed.NewNoop())
}

func newTestWriterWithEmbedder(t *testing.T, e embed.Embedder) (*Writer, store.Store) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	var st store.Store = s
	now := int64(1000)
	w := New(st, e, WithNow(func() int64 { return now }))
	return w, st
}

func TestSaveDistinctFactsSameScope(t *testing.T) {
	ctx := context.Background()
	w, st := newTestWriter(t)

	id1, merged1, err := w.Save(ctx, SaveInput{
		Kind: memory.KindSemantic, Scope: "project:agentmemory",
		Content: "Проект использует SQLite и FTS5 для поиска", Importance: 4,
	})
	if err != nil {
		t.Fatalf("Save 1: %v", err)
	}
	if merged1 {
		t.Errorf("Save 1: merged = true, want false (новая запись)")
	}
	id2, merged2, err := w.Save(ctx, SaveInput{
		Kind: memory.KindSemantic, Scope: "project:agentmemory",
		Content: "Деплой через make build и make test перед релизом", Importance: 3,
	})
	if err != nil {
		t.Fatalf("Save 2: %v", err)
	}
	if merged2 {
		t.Errorf("Save 2: merged = true, want false (новая запись)")
	}
	if id1 == id2 {
		t.Fatalf("distinct facts got the same id %q", id1)
	}
	if _, err := st.Get(ctx, id1); err != nil {
		t.Errorf("Get id1: %v", err)
	}
	if _, err := st.Get(ctx, id2); err != nil {
		t.Errorf("Get id2: %v", err)
	}
}

func TestSaveNearDuplicateMergesIntoExisting(t *testing.T) {
	ctx := context.Background()
	w, st := newTestWriter(t)

	id1, merged1, err := w.Save(ctx, SaveInput{
		Kind: memory.KindSemantic, Scope: "project:agentmemory",
		Content:    "Проект использует Go 1.23 и SQLite через mattn/go-sqlite3",
		Importance: 3, Tags: []string{"go", "stack"},
	})
	if err != nil {
		t.Fatalf("Save 1: %v", err)
	}
	if merged1 {
		t.Errorf("Save 1: merged = true, want false (первая вставка)")
	}

	id2, merged2, err := w.Save(ctx, SaveInput{
		Kind: memory.KindSemantic, Scope: "project:agentmemory",
		Content:    "Проект использует Go 1.23 и SQLite (пакет mattn/go-sqlite3)",
		Importance: 5, Tags: []string{"stack", "db"},
	})
	if err != nil {
		t.Fatalf("Save 2: %v", err)
	}
	if !merged2 {
		t.Errorf("Save 2: merged = false, want true (near-duplicate)")
	}
	if id1 != id2 {
		t.Fatalf("near-duplicate should merge into existing: id1=%q id2=%q", id1, id2)
	}

	got, err := st.Get(ctx, id1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Importance != 5 {
		t.Errorf("Importance = %d, want 5 (max усиление)", got.Importance)
	}
	wantTags := map[string]bool{"go": true, "stack": true, "db": true}
	if len(got.Tags) != len(wantTags) {
		t.Errorf("Tags = %v, want объединение %v", got.Tags, wantTags)
	}
	for _, tag := range got.Tags {
		if !wantTags[tag] {
			t.Errorf("unexpected tag %q", tag)
		}
	}
	if got.AccessCount != 1 {
		t.Errorf("AccessCount = %d, want 1 (Touch при усилении)", got.AccessCount)
	}

	// Убеждаемся, что новая запись не создана.
	all, err := st.SearchLexical(ctx, memory.Query{Scope: "project:agentmemory", Text: "Проект", Limit: 10})
	if err != nil {
		t.Fatalf("SearchLexical: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("candidates = %+v, want ровно 1 запись после мерджа", all)
	}
}

// TestSaveWritesSummaryInOneOperation — SaveInput.Summary попадает в запись при
// вставке одной операцией Put (без отдельного Update).
func TestSaveWritesSummaryInOneOperation(t *testing.T) {
	ctx := context.Background()
	w, st := newTestWriter(t)

	id, merged, err := w.Save(ctx, SaveInput{
		Kind: memory.KindSemantic, Scope: "s-summary", Content: "факт с summary",
		Summary: "краткая форма",
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if merged {
		t.Fatalf("Save: merged = true, want false (новая запись)")
	}
	got, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Summary == nil || *got.Summary != "краткая форма" {
		t.Errorf("Summary = %v, want 'краткая форма'", got.Summary)
	}
}

// TestSaveMergeDoesNotOverwriteExistingSummary — усиление дубля (reinforce) не
// должно переписывать summary уже существующей записи значением из нового входа:
// новый вход — лишь подтверждение того же факта, а не более авторитетный источник.
func TestSaveMergeDoesNotOverwriteExistingSummary(t *testing.T) {
	ctx := context.Background()
	w, st := newTestWriter(t)

	id1, _, err := w.Save(ctx, SaveInput{
		Kind: memory.KindSemantic, Scope: "s-summary-merge",
		Content: "Проект использует Go 1.23 и SQLite через mattn/go-sqlite3",
		Summary: "старое summary",
	})
	if err != nil {
		t.Fatalf("Save 1: %v", err)
	}

	id2, merged, err := w.Save(ctx, SaveInput{
		Kind: memory.KindSemantic, Scope: "s-summary-merge",
		Content: "Проект использует Go 1.23 и SQLite (пакет mattn/go-sqlite3)",
		Summary: "новое summary, которое не должно применяться",
	})
	if err != nil {
		t.Fatalf("Save 2: %v", err)
	}
	if !merged || id2 != id1 {
		t.Fatalf("Save 2: merged=%v id=%q, want merged=true id=%q", merged, id2, id1)
	}

	got, err := st.Get(ctx, id1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Summary == nil || *got.Summary != "старое summary" {
		t.Errorf("Summary после мерджа = %v, want сохранённое 'старое summary'", got.Summary)
	}
}

func TestSaveSameContentDifferentScopeIsolated(t *testing.T) {
	ctx := context.Background()
	w, _ := newTestWriter(t)

	content := "Проект использует SQLite и FTS5 для поиска"
	id1, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "project:a", Content: content})
	if err != nil {
		t.Fatalf("Save 1: %v", err)
	}
	id2, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "project:b", Content: content})
	if err != nil {
		t.Fatalf("Save 2: %v", err)
	}
	if id1 == id2 {
		t.Fatalf("одинаковый content в разных scope должен дать разные записи, got id1=id2=%q", id1)
	}
}

func TestSaveValidation(t *testing.T) {
	ctx := context.Background()
	w, _ := newTestWriter(t)

	t.Run("пустой content", func(t *testing.T) {
		_, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "s", Content: "   "})
		if !errors.Is(err, ErrEmptyContent) {
			t.Errorf("err = %v, want ErrEmptyContent", err)
		}
	})

	t.Run("пустой scope", func(t *testing.T) {
		_, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "  ", Content: "факт"})
		if !errors.Is(err, ErrEmptyScope) {
			t.Errorf("err = %v, want ErrEmptyScope", err)
		}
	})

	t.Run("неизвестный kind", func(t *testing.T) {
		_, _, err := w.Save(ctx, SaveInput{Kind: memory.Kind("unknown"), Scope: "s", Content: "факт"})
		if !errors.Is(err, ErrUnknownKind) {
			t.Errorf("err = %v, want ErrUnknownKind", err)
		}
	})

	t.Run("importance=0 -> дефолт 3", func(t *testing.T) {
		id, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "s-imp0", Content: "факт про импортанс ноль"})
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		st := w.store
		got, err := st.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Importance != 3 {
			t.Errorf("Importance = %d, want 3", got.Importance)
		}
	})

	t.Run("importance=9 -> clamp 5", func(t *testing.T) {
		id, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "s-imp9", Content: "факт про импортанс девять", Importance: 9})
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		got, err := w.store.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Importance != 5 {
			t.Errorf("Importance = %d, want 5 (clamp)", got.Importance)
		}
	})
}

func TestSaveLinks(t *testing.T) {
	ctx := context.Background()
	w, st := newTestWriter(t)

	targetID, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "s-links", Content: "запись-цель для линка"})
	if err != nil {
		t.Fatalf("Save target: %v", err)
	}

	srcID, _, err := w.Save(ctx, SaveInput{
		Kind: memory.KindSemantic, Scope: "s-links", Content: "запись-источник ссылается на цель",
		Links: []string{targetID, "01ARZ3NDEKTSV4RRFFQ69G5FAV" /* несуществующий, но валидный ULID */},
	})
	if err != nil {
		t.Fatalf("Save src: %v", err)
	}

	neighbors, err := st.Neighbors(ctx, targetID)
	if err != nil {
		t.Fatalf("Neighbors: %v", err)
	}
	if len(neighbors) != 1 || neighbors[0].Id != srcID {
		t.Fatalf("neighbors = %+v, want содержать только %q", neighbors, srcID)
	}

	got, err := st.Get(ctx, srcID)
	if err != nil {
		t.Fatalf("Get src: %v", err)
	}
	if len(got.Links) != 1 || got.Links[0].Dst != targetID || got.Links[0].Rel != memory.RelRelated {
		t.Errorf("Links = %+v, want ровно 1 ссылка на %q (висячая должна быть пропущена)", got.Links, targetID)
	}
}

// TestReinforceKeepsExistingLinks — регресс на потерю исходящих links при усилении
// дубля (см. reinforce): SearchLexical не заполняет Tags/Links кандидата, поэтому
// merged, построенный поверх кандидата напрямую, затирал бы links записи при
// Store.Put. reinforce должен сначала запросить полную запись через Store.Get.
func TestReinforceKeepsExistingLinks(t *testing.T) {
	ctx := context.Background()
	w, st := newTestWriter(t)

	targetID, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "s-reinforce", Content: "запись-цель для линка"})
	if err != nil {
		t.Fatalf("Save target: %v", err)
	}

	srcID, _, err := w.Save(ctx, SaveInput{
		Kind: memory.KindSemantic, Scope: "s-reinforce",
		Content: "Проект использует Go 1.23 и SQLite через mattn/go-sqlite3",
		Links:   []string{targetID},
	})
	if err != nil {
		t.Fatalf("Save src: %v", err)
	}

	// near-duplicate того же факта должен смёржиться в srcID (усиление), а не
	// затереть его links.
	mergedID, merged, err := w.Save(ctx, SaveInput{
		Kind: memory.KindSemantic, Scope: "s-reinforce",
		Content: "Проект использует Go 1.23 и SQLite (пакет mattn/go-sqlite3)",
	})
	if err != nil {
		t.Fatalf("Save near-duplicate: %v", err)
	}
	if !merged || mergedID != srcID {
		t.Fatalf("Save near-duplicate: merged=%v id=%q, want merged=true id=%q", merged, mergedID, srcID)
	}

	got, err := st.Get(ctx, srcID)
	if err != nil {
		t.Fatalf("Get src после усиления: %v", err)
	}
	if len(got.Links) != 1 || got.Links[0].Dst != targetID {
		t.Errorf("Links после усиления = %+v, want сохранённую ссылку на %q", got.Links, targetID)
	}
}

// TestSavePunctuationOnlyFactsNotMerged — регресс на ложное SimHash-слияние: контент
// из одной пунктуации токенизируется в пустой набор слов, simhash64 для него
// вырождается в 0, и два содержательно разных таких факта раньше молча сливались
// бы (Hamming-расстояние 0). Save должен вставить их как разные записи.
func TestSavePunctuationOnlyFactsNotMerged(t *testing.T) {
	ctx := context.Background()
	w, _ := newTestWriter(t)

	id1, merged1, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "s-punct", Content: "!!!"})
	if err != nil {
		t.Fatalf("Save 1: %v", err)
	}
	if merged1 {
		t.Fatalf("Save 1: merged = true, want false")
	}

	id2, merged2, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "s-punct", Content: "???"})
	if err != nil {
		t.Fatalf("Save 2: %v", err)
	}
	if merged2 {
		t.Fatalf("Save 2: merged = true, want false (два разных пунктуационных факта не должны слиться)")
	}
	if id1 == id2 {
		t.Fatalf("два разных факта получили один id %q", id1)
	}
}

func TestWriterUpdate(t *testing.T) {
	ctx := context.Background()
	w, st := newTestWriter(t)

	id, _, err := w.Save(ctx, SaveInput{
		Kind: memory.KindSemantic, Scope: "s-update", Content: "исходный факт",
		Importance: 2, Tags: []string{"orig"},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	t.Run("меняет только переданные поля", func(t *testing.T) {
		newContent := "обновлённый факт"
		if err := w.Update(ctx, UpdateInput{Id: id, Content: &newContent}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		got, err := st.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Content != newContent {
			t.Errorf("Content = %q, want %q", got.Content, newContent)
		}
		if got.Importance != 2 {
			t.Errorf("Importance = %d, want непосредственно 2 (не передавалось)", got.Importance)
		}
		if len(got.Tags) != 1 || got.Tags[0] != "orig" {
			t.Errorf("Tags = %v, want [orig] (не передавались)", got.Tags)
		}
	})

	t.Run("importance нормализуется как в Save (clamp вне 1..5)", func(t *testing.T) {
		imp := 9
		if err := w.Update(ctx, UpdateInput{Id: id, Importance: &imp}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		got, err := st.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Importance != 5 {
			t.Errorf("Importance = %d, want 5 (clamp, как в Save)", got.Importance)
		}
	})

	t.Run("неизвестный id -> ошибка", func(t *testing.T) {
		err := w.Update(ctx, UpdateInput{Id: "01ARZ3NDEKTSV4RRFFQ69G5FAV"})
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("err = %v, want обёрнутый store.ErrNotFound", err)
		}
	})
}

// TestWriterUpdateLinks — Links в UpdateInput ДОБАВЛЯЕТ связи к уже существующим,
// а не заменяет их (в отличие от Tags), и молча пропускает висячие ссылки —
// нужно двухпроходным сценариям (импорт файловой памяти, DESIGN.md §14 Этап 5),
// где запись сохраняется до того, как известны id всех её [[wiki-ссылок]].
func TestWriterUpdateLinks(t *testing.T) {
	ctx := context.Background()
	w, st := newTestWriter(t)

	targetA, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "s-update-links", Content: "первая целевая запись про базы данных"})
	if err != nil {
		t.Fatalf("Save targetA: %v", err)
	}
	targetB, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "s-update-links", Content: "вторая целевая запись про сетевые протоколы"})
	if err != nil {
		t.Fatalf("Save targetB: %v", err)
	}
	srcID, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "s-update-links", Content: "источник без links при сохранении"})
	if err != nil {
		t.Fatalf("Save src: %v", err)
	}

	if err := w.Update(ctx, UpdateInput{Id: srcID, Links: []string{targetA}}); err != nil {
		t.Fatalf("Update (первая ссылка): %v", err)
	}
	got, err := st.Get(ctx, srcID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Links) != 1 || got.Links[0].Dst != targetA || got.Links[0].Rel != memory.RelRelated {
		t.Fatalf("Links после первого Update = %+v, want ровно 1 ссылку на %q", got.Links, targetA)
	}

	// Второй Update с новой ссылкой + висячей должен ДОБАВИТЬ targetB, сохранив
	// targetA, и молча пропустить несуществующий id.
	if err := w.Update(ctx, UpdateInput{Id: srcID, Links: []string{targetB, "01ARZ3NDEKTSV4RRFFQ69G5FAV"}}); err != nil {
		t.Fatalf("Update (вторая ссылка): %v", err)
	}
	got, err = st.Get(ctx, srcID)
	if err != nil {
		t.Fatalf("Get после второго Update: %v", err)
	}
	if len(got.Links) != 2 {
		t.Fatalf("Links после второго Update = %+v, want 2 ссылки (targetA сохранена + targetB добавлена)", got.Links)
	}
	dsts := map[string]bool{got.Links[0].Dst: true, got.Links[1].Dst: true}
	if !dsts[targetA] || !dsts[targetB] {
		t.Errorf("Links = %+v, want содержать %q и %q", got.Links, targetA, targetB)
	}
}

// TestWriterUpdateSkipsSelfLink — Update отсекает self-петлю (Dst == id записи):
// ребро id->id бессмысленно, инвариант держится в доменном write-пути для всех
// вызывающих. Ссылка на себя, поданная вперемешку с валидной, не мешает валидной.
func TestWriterUpdateSkipsSelfLink(t *testing.T) {
	ctx := context.Background()
	w, st := newTestWriter(t)

	other, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "s-self-link", Content: "валидная цель про базы данных"})
	if err != nil {
		t.Fatalf("Save other: %v", err)
	}
	srcID, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "s-self-link", Content: "источник ссылается сам на себя"})
	if err != nil {
		t.Fatalf("Save src: %v", err)
	}

	if err := w.Update(ctx, UpdateInput{Id: srcID, Links: []string{srcID, other}}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := st.Get(ctx, srcID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Links) != 1 || got.Links[0].Dst != other {
		t.Errorf("Links = %+v, want ровно 1 ссылку на %q (self-петля на %q отсечена)", got.Links, other, srcID)
	}
}

func TestWriterForget(t *testing.T) {
	ctx := context.Background()
	w, st := newTestWriter(t)

	id, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "s-forget", Content: "факт про забывание"})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	t.Run("без replacedBy - тумстоун, выпадает из SearchLexical", func(t *testing.T) {
		if err := w.Forget(ctx, id, ""); err != nil {
			t.Fatalf("Forget: %v", err)
		}
		cands, err := st.SearchLexical(ctx, memory.Query{Text: "забывание", Scope: "s-forget", Limit: 10})
		if err != nil {
			t.Fatalf("SearchLexical: %v", err)
		}
		if len(cands) != 0 {
			t.Errorf("candidates = %+v, want пусто после Forget", cands)
		}
		got, err := st.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.SupersededBy == nil || *got.SupersededBy != id {
			t.Errorf("SupersededBy = %v, want указатель на себя (тумстоун)", got.SupersededBy)
		}
	})

	t.Run("с replacedBy - проставляет ссылку", func(t *testing.T) {
		other, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "s-forget", Content: "другой факт"})
		if err != nil {
			t.Fatalf("Save other: %v", err)
		}
		replacement, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "s-forget", Content: "факт-замена"})
		if err != nil {
			t.Fatalf("Save replacement: %v", err)
		}

		if err := w.Forget(ctx, other, replacement); err != nil {
			t.Fatalf("Forget: %v", err)
		}
		got, err := st.Get(ctx, other)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.SupersededBy == nil || *got.SupersededBy != replacement {
			t.Errorf("SupersededBy = %v, want %q", got.SupersededBy, replacement)
		}
	})

	t.Run("неизвестный replacedBy -> ошибка", func(t *testing.T) {
		another, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "s-forget", Content: "ещё один факт"})
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		if err := w.Forget(ctx, another, "01ARZ3NDEKTSV4RRFFQ69G5FAV"); err == nil {
			t.Error("Forget с несуществующим replacedBy должен вернуть ошибку")
		}
	})
}

// TestSaveEmbedsAndStoresVector — Save с включённым Embedder (Dim() > 0) считает
// вектор content и сохраняет его через Store.PutVector (DESIGN.md §7.4, §9).
// Точность сохранённого вектора проверяется через SearchVector: KNN по тому же
// вектору должен найти запись с нулевой дистанцией (score 1.0).
func TestSaveEmbedsAndStoresVector(t *testing.T) {
	ctx := context.Background()
	fe := &fakeEmbedder{Vec: []float32{1, 0, 0}}
	w, st := newTestWriterWithEmbedder(t, fe)
	if err := st.ResetVectorIndex(ctx, fe.Model(), fe.Dim()); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}

	id, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "s-embed", Content: "факт для эмбеддинга"})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if fe.CallCount != 1 {
		t.Errorf("CallCount = %d, want 1", fe.CallCount)
	}

	cands, err := st.SearchVector(ctx, fe.Vec, memory.Query{Limit: 5})
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	if len(cands) != 1 || cands[0].Memory.Id != id {
		t.Fatalf("SearchVector = %+v, want ровно запись %q", cands, id)
	}
	if cands[0].Score != 1.0 {
		t.Errorf("Score = %v, want 1.0 (нулевая дистанция — вектор сохранён без искажений)", cands[0].Score)
	}
}

// TestSaveEmbedErrorDegradesGracefully — ошибка Embed не должна проваливать Save:
// запись сохраняется без вектора и остаётся видна через ListMissingVectors —
// очередь на досчёт фоновым джобом (DESIGN.md §7.4, §9, §15: деградация, а не падение).
func TestSaveEmbedErrorDegradesGracefully(t *testing.T) {
	ctx := context.Background()
	fe := &fakeEmbedder{Vec: []float32{1, 0, 0}, Err: errors.New("embedder недоступен")}
	w, st := newTestWriterWithEmbedder(t, fe)
	if err := st.ResetVectorIndex(ctx, fe.Model(), fe.Dim()); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}

	id, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "s-embed-err", Content: "факт без вектора"})
	if err != nil {
		t.Fatalf("Save должен вернуть успех даже при ошибке Embed: %v", err)
	}
	if fe.CallCount != 1 {
		t.Errorf("CallCount = %d, want 1", fe.CallCount)
	}

	missing, err := st.ListMissingVectors(ctx, 0)
	if err != nil {
		t.Fatalf("ListMissingVectors: %v", err)
	}
	found := false
	for _, m := range missing {
		if m.Id == id {
			found = true
		}
	}
	if !found {
		t.Errorf("ListMissingVectors = %+v, want содержать %q (вектор не сохранён)", missing, id)
	}
}

// TestSaveNoopEmbedderSkipsEmbed — Dim() == 0 означает, что Embed вообще не должен
// вызываться (не просто «результат игнорируется»): дешёвая деградация, без лишней
// работы, пока Embedder отключён (DESIGN.md §7.4, §9).
func TestSaveNoopEmbedderSkipsEmbed(t *testing.T) {
	ctx := context.Background()
	fe := &fakeEmbedder{} // Vec == nil => Dim() == 0, как у embed.Noop
	w, _ := newTestWriterWithEmbedder(t, fe)

	if _, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "s-noop", Content: "факт без эмбеддера"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if fe.CallCount != 0 {
		t.Errorf("CallCount = %d, want 0 (Dim()==0 => Embed не вызывается)", fe.CallCount)
	}
}

// TestUpdateContentReembeds — Update, меняющий Content, должен пересчитать вектор от
// НОВОГО content и перезаписать его в Store (DESIGN.md §7.4, §9).
func TestUpdateContentReembeds(t *testing.T) {
	ctx := context.Background()
	fe := &fakeEmbedder{Vec: []float32{1, 0, 0}}
	w, st := newTestWriterWithEmbedder(t, fe)
	if err := st.ResetVectorIndex(ctx, fe.Model(), fe.Dim()); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}

	id, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "s-reembed", Content: "исходный факт"})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if fe.CallCount != 1 {
		t.Fatalf("CallCount после Save = %d, want 1", fe.CallCount)
	}

	fe.Vec = []float32{0, 1, 0}
	newContent := "обновлённый факт"
	if err := w.Update(ctx, UpdateInput{Id: id, Content: &newContent}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if fe.CallCount != 2 {
		t.Errorf("CallCount после Update = %d, want 2 (переэмбеддинг нового content)", fe.CallCount)
	}

	cands, err := st.SearchVector(ctx, []float32{0, 1, 0}, memory.Query{Limit: 5})
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	if len(cands) != 1 || cands[0].Memory.Id != id || cands[0].Score != 1.0 {
		t.Fatalf("SearchVector = %+v, want ровно запись %q с новым вектором", cands, id)
	}
}
