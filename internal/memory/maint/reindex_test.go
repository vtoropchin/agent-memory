package maint

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"agentmemory/internal/memory"
	"agentmemory/internal/memory/embed"
	"agentmemory/internal/memory/store"
)

// reindexFakeEmbedder — управляемая тестами реализация embed.Embedder: по
// умолчанию возвращает детерминированный вектор (зависящий только от dim, чтобы у
// всех текстов батча были валидные ненулевые векторы) либо ошибку err на каждый
// вызов Embed, и считает число вызовов Embed. Отдельный от write.fakeEmbedder
// тип — тестовый хелпер write-пакета неэкспортирован, а тянуть на него
// зависимость ради этого не стоит (см. задание).
//
// embedFn, если задан, полностью подменяет поведение по умолчанию (err/dim
// игнорируются) — нужен тестам, которым требуется выборочный или частичный сбой
// (конкретный по счёту вызов, либо часть записей внутри батча: усечённый ответ
// или пустой вектор для некоторых текстов), недостижимый через err — оно валит
// каждый вызов целиком. Получает номер вызова (считая с 1) и тексты батча.
type reindexFakeEmbedder struct {
	model string
	dim   int
	err   error
	calls int

	embedFn func(call int, texts []string) ([][]float32, error)
}

func (f *reindexFakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls++
	if f.embedFn != nil {
		return f.embedFn(f.calls, texts)
	}
	if f.err != nil {
		return nil, f.err
	}
	vecs := make([][]float32, len(texts))
	for i := range texts {
		vec := make([]float32, f.dim)
		for j := range vec {
			vec[j] = 1 // содержимое не важно для теста — важно, что вектор непустой и нужной размерности
		}
		vecs[i] = vec
	}
	return vecs, nil
}

func (f *reindexFakeEmbedder) Model() string { return f.model }
func (f *reindexFakeEmbedder) Dim() int      { return f.dim }

var _ embed.Embedder = (*reindexFakeEmbedder)(nil)

// putMemory вставляет запись напрямую в Store (в обход write path — векторный слой
// не нужен для этих тестов, а Store.Put его и не считает).
func putMemory(t *testing.T, s store.Store, scope, content string) memory.Memory {
	t.Helper()
	now := time.Now().Unix()
	m := memory.Memory{
		Id:        ulid.Make().String(),
		Kind:      memory.KindSemantic,
		Scope:     scope,
		Content:   content,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.Put(context.Background(), m); err != nil {
		t.Fatalf("putMemory: %v", err)
	}
	return m
}

// putMemoryAt — как putMemory, но с явным created_at. Нужен тестам, которым важен
// детерминированный порядок записей в ListMissingVectors (ORDER BY created_at) —
// putMemory использует time.Now().Unix(), и несколько записей, вставленных в одну
// секунду, получили бы одинаковый created_at и непредсказуемый взаимный порядок.
func putMemoryAt(t *testing.T, s store.Store, scope, content string, createdAt int64) memory.Memory {
	t.Helper()
	m := memory.Memory{
		Id:        ulid.Make().String(),
		Kind:      memory.KindSemantic,
		Scope:     scope,
		Content:   content,
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
	if err := s.Put(context.Background(), m); err != nil {
		t.Fatalf("putMemoryAt: %v", err)
	}
	return m
}

func TestCheckAndReindexNoopEmbedderIsNoop(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	putMemory(t, s, "project:x", "запись без вектора")

	r := NewReindexer(s, embed.NewNoop())
	result, err := r.CheckAndReindex(ctx)
	if err != nil {
		t.Fatalf("CheckAndReindex: %v", err)
	}
	if result != (ReindexResult{}) {
		t.Errorf("result = %+v, want пустой", result)
	}

	model, dim, err := s.EmbedMeta(ctx)
	if err != nil {
		t.Fatalf("EmbedMeta: %v", err)
	}
	if model != "" || dim != 0 {
		t.Errorf("EmbedMeta = (%q, %d), want (\"\", 0) — ResetVectorIndex не должен вызываться", model, dim)
	}
}

func TestCheckAndReindexEmptyMetaFullReindex(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	a := putMemory(t, s, "project:x", "первая запись")
	b := putMemory(t, s, "project:x", "вторая запись")

	fe := &reindexFakeEmbedder{model: "fake-model", dim: 3}
	r := NewReindexer(s, fe)

	result, err := r.CheckAndReindex(ctx)
	if err != nil {
		t.Fatalf("CheckAndReindex: %v", err)
	}
	if !result.Reindexed {
		t.Errorf("Reindexed = false, want true")
	}
	if result.Embedded != 2 {
		t.Errorf("Embedded = %d, want 2", result.Embedded)
	}
	if result.Failed != 0 {
		t.Errorf("Failed = %d, want 0", result.Failed)
	}

	model, dim, err := s.EmbedMeta(ctx)
	if err != nil {
		t.Fatalf("EmbedMeta: %v", err)
	}
	if model != fe.model || dim != fe.dim {
		t.Errorf("EmbedMeta = (%q, %d), want (%q, %d)", model, dim, fe.model, fe.dim)
	}

	missing, err := s.ListMissingVectors(ctx, 0)
	if err != nil {
		t.Fatalf("ListMissingVectors: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("ListMissingVectors = %+v, want пусто", missing)
	}

	// вектора обеих записей реально найдены в векторном поиске.
	cands, err := s.SearchVector(ctx, []float32{1, 1, 1}, memory.Query{Limit: 10})
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("SearchVector = %+v, want 2 кандидата", cands)
	}
	gotIDs := map[string]bool{cands[0].Memory.Id: true, cands[1].Memory.Id: true}
	if !gotIDs[a.Id] || !gotIDs[b.Id] {
		t.Errorf("SearchVector ids = %v, want %q и %q", gotIDs, a.Id, b.Id)
	}
}

func TestCheckAndReindexMatchingMetaDrainsQueueOnly(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	putMemory(t, s, "project:x", "первая запись")

	fe := &reindexFakeEmbedder{model: "fake-model", dim: 3}
	r := NewReindexer(s, fe)

	// первый прогон создаёт индекс и эмбеддит существующую запись.
	if _, err := r.CheckAndReindex(ctx); err != nil {
		t.Fatalf("CheckAndReindex #1: %v", err)
	}
	fe.calls = 0

	// запись, вставленная напрямую через Store.Put, минует write path и остаётся
	// без вектора — должна попасть в очередь досчёта.
	late := putMemory(t, s, "project:x", "запись без вектора добавлена позже")

	result, err := r.CheckAndReindex(ctx)
	if err != nil {
		t.Fatalf("CheckAndReindex #2: %v", err)
	}
	if result.Reindexed {
		t.Errorf("Reindexed = true, want false (модель/dim совпали)")
	}
	if result.Embedded != 1 {
		t.Errorf("Embedded = %d, want 1 (досчитана только late)", result.Embedded)
	}
	if fe.calls != 1 {
		t.Errorf("Embed calls = %d, want 1 (одна запись в очереди — один батч)", fe.calls)
	}

	missing, err := s.ListMissingVectors(ctx, 0)
	if err != nil {
		t.Fatalf("ListMissingVectors: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("ListMissingVectors = %+v, want пусто после досчёта", missing)
	}

	got, err := s.Get(ctx, late.Id)
	if err != nil {
		t.Fatalf("Get late: %v", err)
	}
	if got.Id != late.Id {
		t.Errorf("Get late вернул другую запись: %+v", got)
	}
}

func TestCheckAndReindexModelChangeResetsAndReembeds(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	putMemory(t, s, "project:x", "запись под старую модель")

	old := &reindexFakeEmbedder{model: "model-a", dim: 3}
	r := NewReindexer(s, old)
	if _, err := r.CheckAndReindex(ctx); err != nil {
		t.Fatalf("CheckAndReindex (old model): %v", err)
	}

	newer := &reindexFakeEmbedder{model: "model-b", dim: 5}
	r2 := NewReindexer(s, newer)
	result, err := r2.CheckAndReindex(ctx)
	if err != nil {
		t.Fatalf("CheckAndReindex (new model): %v", err)
	}
	if !result.Reindexed {
		t.Errorf("Reindexed = false, want true (модель сменилась)")
	}
	if result.Embedded != 1 {
		t.Errorf("Embedded = %d, want 1", result.Embedded)
	}

	model, dim, err := s.EmbedMeta(ctx)
	if err != nil {
		t.Fatalf("EmbedMeta: %v", err)
	}
	if model != "model-b" || dim != 5 {
		t.Errorf("EmbedMeta = (%q, %d), want (model-b, 5)", model, dim)
	}
}

func TestCheckAndReindexEmbedErrorDoesNotHangAndReportsFailed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := newTestStore(t)
	for i := 0; i < 5; i++ {
		putMemory(t, s, "project:x", "запись")
	}

	fe := &reindexFakeEmbedder{model: "fake-model", dim: 3, err: errors.New("boom: embedder недоступен")}
	r := NewReindexer(s, fe)

	result, err := r.CheckAndReindex(ctx)
	if err != nil {
		t.Fatalf("CheckAndReindex: %v (не должен возвращать ошибку — деградация)", err)
	}
	if !result.Reindexed {
		t.Errorf("Reindexed = false, want true (индекс пересоздан, даже если эмбеддинг не удался)")
	}
	if result.Embedded != 0 {
		t.Errorf("Embedded = %d, want 0", result.Embedded)
	}
	// Failed = 10, не 5: в рамках одного CheckAndReindex записи проходят через
	// embedItems дважды — сперва полный reindex (AllForReindex), затем сразу же
	// drainMissing видит те же 5 записей всё ещё без вектора (ResetVectorIndex
	// уже создал таблицу, но ни одна запись в неё не попала) и делает ещё одну
	// попытку; обе попытки проваливаются и обе учитываются в Failed.
	if result.Failed != 10 {
		t.Errorf("Failed = %d, want 10 (5 записей x 2 попытки: reindex + немедленный досчёт очереди)", result.Failed)
	}

	// EmbedMeta уже записана под новую модель — второй прогон не должен повторно
	// звать ResetVectorIndex, но досчёт очереди снова упрётся в ту же ошибку и
	// обязан завершиться (не зависнуть), не добавив ни одного вектора.
	fe.calls = 0
	result2, err := r.CheckAndReindex(ctx)
	if err != nil {
		t.Fatalf("CheckAndReindex #2: %v", err)
	}
	if result2.Reindexed {
		t.Errorf("Reindexed = true, want false (meta уже совпадает)")
	}
	if result2.Embedded != 0 {
		t.Errorf("Embedded = %d, want 0", result2.Embedded)
	}
	if result2.Failed != 5 {
		t.Errorf("Failed = %d, want 5 (та же очередь, всё ещё не эмбеддится)", result2.Failed)
	}
	if fe.calls == 0 {
		t.Errorf("Embed calls = 0, want > 0 (досчёт должен был попытаться)")
	}
}

func TestCheckAndReindexMultiBatchAllSucceed(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	// больше reindexBatchSize (64) записей — два батча; проверяем, что успешный
	// прогон Embed эмбеддит все записи независимо от числа батчей.
	total := reindexBatchSize + 10
	for i := 0; i < total; i++ {
		putMemory(t, s, "project:x", "запись")
	}

	fe := &reindexFakeEmbedder{model: "fake-model", dim: 3}
	r := NewReindexer(s, fe)

	result, err := r.CheckAndReindex(ctx)
	if err != nil {
		t.Fatalf("CheckAndReindex: %v", err)
	}
	if result.Embedded != total {
		t.Errorf("Embedded = %d, want %d", result.Embedded, total)
	}
	if fe.calls != 2 {
		t.Errorf("Embed calls = %d, want 2 (два батча по 64/10)", fe.calls)
	}
}

// TestCheckAndReindexFailedBatchStaysQueuedOtherBatchEmbeds — реальный частичный
// сбой между двумя батчами: Embed первого батча (64 записи) стабильно
// (в т.ч. на повторных попытках досчёта) возвращает ошибку, второго (10 записей) —
// всегда успешен. Проверяет, что провал одного батча не блокирует эмбеддинг
// остальных, что записи провалившегося батча учтены в Failed и что они остаются в
// очереди ListMissingVectors после завершения прогона.
func TestCheckAndReindexFailedBatchStaysQueuedOtherBatchEmbeds(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	fe := &reindexFakeEmbedder{model: "fake-model", dim: 3}
	// Meta сразу под текущую модель — прогон сразу идёт в drainMissing (очередь
	// недостающих векторов), без отдельной фазы полного reindex: так батчи и их
	// порядок предсказуемы, а точка приложения теста — сама логика частичного
	// сбоя, не reindex vs draining.
	if err := s.ResetVectorIndex(ctx, fe.model, fe.dim); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}

	const failCount = reindexBatchSize // первый батч целиком
	const okCount = 10                 // второй батч целиком
	base := time.Now().Unix()
	for i := 0; i < failCount; i++ {
		putMemoryAt(t, s, "project:x", "плохая запись", base+int64(i))
	}
	for i := 0; i < okCount; i++ {
		putMemoryAt(t, s, "project:x", "хорошая запись", base+int64(failCount+i))
	}

	// Батч с "плохая запись" валится на каждой попытке (в т.ч. на повторном
	// досчёте drainMissing) — иначе к концу прогона он бы всё-таки
	// доэмбеддился и не остался бы в очереди, как того требует тест.
	fe.embedFn = func(_ int, texts []string) ([][]float32, error) {
		if texts[0] == "плохая запись" {
			return nil, errors.New("boom: батч недоступен")
		}
		vecs := make([][]float32, len(texts))
		for i := range vecs {
			vecs[i] = []float32{1, 1, 1}
		}
		return vecs, nil
	}

	r := NewReindexer(s, fe)
	result, err := r.CheckAndReindex(ctx)
	if err != nil {
		t.Fatalf("CheckAndReindex: %v (не должен возвращать ошибку — деградация)", err)
	}
	if result.Embedded != okCount {
		t.Errorf("Embedded = %d, want %d (только хорошая запись)", result.Embedded, okCount)
	}
	// Failed = 2*failCount, не failCount: плохой батч проваливается дважды в
	// рамках одного прогона — на первом проходе drainMissing и на немедленном
	// повторе, пока список недостающих не опустеет (см. Failed doc-comment).
	if result.Failed != 2*failCount {
		t.Errorf("Failed = %d, want %d (плохой батч x 2 попытки)", result.Failed, 2*failCount)
	}

	missing, err := s.ListMissingVectors(ctx, 0)
	if err != nil {
		t.Fatalf("ListMissingVectors: %v", err)
	}
	if len(missing) != failCount {
		t.Fatalf("ListMissingVectors = %d записей, want %d (плохой батч остался в очереди)", len(missing), failCount)
	}
	for _, m := range missing {
		if m.Content != "плохая запись" {
			t.Errorf("ListMissingVectors содержит неожиданную запись: %+v", m)
		}
	}
}

// TestCheckAndReindexEmbedFewerVectorsThanBatchFailsTail проверяет ветку
// reindex.go, где Embed вернул меньше векторов, чем было текстов в батче
// (усечённый ответ): недостающий хвост батча считается Failed, а не паникует и не
// теряет остальные записи.
func TestCheckAndReindexEmbedFewerVectorsThanBatchFailsTail(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	fe := &reindexFakeEmbedder{model: "fake-model", dim: 3}
	if err := s.ResetVectorIndex(ctx, fe.model, fe.dim); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}

	const n = 5
	base := time.Now().Unix()
	var records []memory.Memory
	for i := 0; i < n; i++ {
		records = append(records, putMemoryAt(t, s, "project:x", fmt.Sprintf("запись %d", i), base+int64(i)))
	}

	// На каждый вызов Embed возвращает вектор только для первых len(texts)-2
	// текстов батча (не меньше 0) — детерминированно и стабильно на повторных
	// попытках (иначе досчёт хвоста меньшим батчем мог бы случайно совпасть по
	// размеру и уйти как полностью успешный, скрыв проверяемую ветку).
	fe.embedFn = func(_ int, texts []string) ([][]float32, error) {
		want := len(texts) - 2
		if want < 0 {
			want = 0
		}
		vecs := make([][]float32, want)
		for i := range vecs {
			vecs[i] = []float32{1, 1, 1}
		}
		return vecs, nil
	}

	r := NewReindexer(s, fe)
	result, err := r.CheckAndReindex(ctx)
	if err != nil {
		t.Fatalf("CheckAndReindex: %v", err)
	}
	if result.Embedded != n-2 {
		t.Errorf("Embedded = %d, want %d", result.Embedded, n-2)
	}
	// Первая попытка (5 текстов, 3 вектора) даёт Failed=2 за хвост батча; вторая
	// попытка досчёта — на оставшихся 2 записях (2-2=0 векторов) — добавляет
	// ещё 2 (см. Failed doc-comment: счётчик попыток, не уникальных записей).
	if result.Failed != 4 {
		t.Errorf("Failed = %d, want 4 (хвост батча x 2 попытки)", result.Failed)
	}

	missing, err := s.ListMissingVectors(ctx, 0)
	if err != nil {
		t.Fatalf("ListMissingVectors: %v", err)
	}
	if len(missing) != 2 {
		t.Fatalf("ListMissingVectors = %d записей, want 2", len(missing))
	}
	wantMissing := map[string]bool{records[3].Id: true, records[4].Id: true}
	for _, m := range missing {
		if !wantMissing[m.Id] {
			t.Errorf("ListMissingVectors содержит неожиданную запись %q", m.Id)
		}
	}
}

// TestCheckAndReindexEmptyVectorForSomeRecordsCountedFailed проверяет ветку
// reindex.go, где Embed вернул вектор нужной длины ответа, но пустой (len==0) для
// части записей батча: такая запись считается Failed, остальные записи батча —
// нет.
func TestCheckAndReindexEmptyVectorForSomeRecordsCountedFailed(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	fe := &reindexFakeEmbedder{model: "fake-model", dim: 3}
	if err := s.ResetVectorIndex(ctx, fe.model, fe.dim); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}

	base := time.Now().Unix()
	putMemoryAt(t, s, "project:x", "хорошая запись 1", base)
	bad := putMemoryAt(t, s, "project:x", "проблемная запись", base+1)
	putMemoryAt(t, s, "project:x", "хорошая запись 2", base+2)

	// По содержимому (а не по позиции в батче) — стабильно на повторных
	// попытках досчёта, независимо от того, как batch перегруппируется.
	fe.embedFn = func(_ int, texts []string) ([][]float32, error) {
		vecs := make([][]float32, len(texts))
		for i, txt := range texts {
			if txt == "проблемная запись" {
				vecs[i] = []float32{} // Embedder вернул пустой эмбеддинг для этой записи
				continue
			}
			vecs[i] = []float32{1, 1, 1}
		}
		return vecs, nil
	}

	r := NewReindexer(s, fe)
	result, err := r.CheckAndReindex(ctx)
	if err != nil {
		t.Fatalf("CheckAndReindex: %v", err)
	}
	if result.Embedded != 2 {
		t.Errorf("Embedded = %d, want 2 (обе хорошие записи)", result.Embedded)
	}
	// Первая попытка (3 текста) даёт Failed=1 за проблемную запись; повторный
	// досчёт (уже только эта запись) добавляет ещё 1.
	if result.Failed != 2 {
		t.Errorf("Failed = %d, want 2 (проблемная запись x 2 попытки)", result.Failed)
	}

	missing, err := s.ListMissingVectors(ctx, 0)
	if err != nil {
		t.Fatalf("ListMissingVectors: %v", err)
	}
	if len(missing) != 1 || missing[0].Id != bad.Id {
		t.Errorf("ListMissingVectors = %+v, want только %q", missing, bad.Id)
	}
}

// cancelAfterNPutVector оборачивает store.Store и вызывает cancel сразу после
// n-го успешного PutVector — используется, чтобы детерминированно (без гонки)
// приземлить отмену ctx строго между двумя батчами reindex: после того, как
// последняя запись первого батча реально сохранена, но до проверки ctx.Err() перед
// вторым батчем (reindex.go:132-134). Отмена ctx ДО вызова PutVector (например,
// изнутри Embed первого батча) была бы гонкой с драйвером mattn/go-sqlite3:
// экспериментально она детерминированно проваливает сам PutVector (interrupted),
// что не даёт проверить требуемое «первый батч заэмбежен, второй — нет».
type cancelAfterNPutVector struct {
	store.Store
	n      int
	cancel context.CancelFunc
	count  int
}

func (c *cancelAfterNPutVector) PutVector(ctx context.Context, id string, vec []float32) error {
	err := c.Store.PutVector(ctx, id, vec)
	c.count++
	if c.count == c.n {
		c.cancel()
	}
	return err
}

func TestCheckAndReindexCtxCancelBetweenBatchesStopsReindex(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newTestStore(t)

	total := reindexBatchSize + 6
	for i := 0; i < total; i++ {
		putMemory(t, s, "project:x", "запись")
	}

	wrapped := &cancelAfterNPutVector{Store: s, n: reindexBatchSize, cancel: cancel}
	fe := &reindexFakeEmbedder{model: "fake-model", dim: 3}
	r := NewReindexer(wrapped, fe)

	result, err := r.CheckAndReindex(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CheckAndReindex err = %v, want context.Canceled", err)
	}
	if result.Embedded != reindexBatchSize {
		t.Errorf("Embedded = %d, want %d (только первый батч)", result.Embedded, reindexBatchSize)
	}
	if result.Failed != 0 {
		t.Errorf("Failed = %d, want 0", result.Failed)
	}
	// Reindexed остаётся false: CheckAndReindex возвращает ошибку из embedItems
	// раньше строки, которая выставляет result.Reindexed = true (reindex.go) —
	// даже несмотря на то, что ResetVectorIndex и часть эмбеддинга уже случились.
	if result.Reindexed {
		t.Errorf("Reindexed = true, want false (ранний return по ctx.Err() до result.Reindexed = true)")
	}
	if fe.calls != 1 {
		t.Errorf("Embed calls = %d, want 1 (второй батч не должен был начаться)", fe.calls)
	}

	missing, err := s.ListMissingVectors(context.Background(), 0)
	if err != nil {
		t.Fatalf("ListMissingVectors: %v", err)
	}
	if len(missing) != total-reindexBatchSize {
		t.Errorf("ListMissingVectors = %d, want %d (второй батч не обработан)", len(missing), total-reindexBatchSize)
	}
}
