// Package write реализует write path AgentMemory (DESIGN.md §7): приём атомарного
// факта от агента, дедупликацию/усиление похожих записей, проверку связей и
// вставку в Store.
package write

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"agentmemory/internal/memory"
	"agentmemory/internal/memory/embed"
	"agentmemory/internal/memory/store"
)

// Ошибки валидации входа Save.
var (
	ErrEmptyContent = errors.New("write: content не может быть пустым")
	ErrEmptyScope   = errors.New("write: scope не может быть пустым")
	ErrUnknownKind  = errors.New("write: неизвестный kind")
)

// defaultDedupLimit — сколько кандидатов из того же scope/kind рассматриваем при
// поиске near-duplicate перед вставкой новой записи.
const defaultDedupLimit = 20

// simhashDistanceThreshold — максимальное расстояние Хэмминга между 64-битными
// SimHash-отпечатками content, при котором две записи считаются дублями (см. simhash.go).
// Подобрано эмпирически на коротких (5-10 слов) атомарных фактах: перефразировка того
// же факта с добавлением/заменой 1-2 слов даёт расстояние ~8 из 64; содержательно
// разные факты той же длины — от ~24 и выше. Порог 8 ловит перефразировки с запасом
// от ложных срабатываний на разных фактах.
const simhashDistanceThreshold = 8

// Writer — точка приёма новых фактов в память (DESIGN.md §3, §7).
type Writer struct {
	store    store.Store
	embedder embed.Embedder
	now      func() int64
	entropy  io.Reader
}

// Option настраивает Writer при создании (используется в тестах для детерминизма).
type Option func(*Writer)

// WithNow подменяет источник текущего unix-времени (по умолчанию — time.Now().Unix()).
func WithNow(now func() int64) Option {
	return func(w *Writer) { w.now = now }
}

// WithEntropy подменяет источник энтропии для генерации ULID (по умолчанию —
// потокобезопасный монотонный источник ulid.DefaultEntropy()).
func WithEntropy(entropy io.Reader) Option {
	return func(w *Writer) { w.entropy = entropy }
}

// New создаёт Writer поверх Store и Embedder. Embedder используется в Save/Update
// для синхронного расчёта вектора новой/изменённой записи (DESIGN.md §7.4, §9);
// пока Embedder=noop (Dim()==0), эмбеддинг не считается — vec-запись остаётся
// отложенной для фонового досчёта (см. Store.ListMissingVectors).
func New(s store.Store, e embed.Embedder, opts ...Option) *Writer {
	w := &Writer{
		store:    s,
		embedder: e,
		now:      func() int64 { return time.Now().Unix() },
		entropy:  ulid.DefaultEntropy(),
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// SaveInput — структурированный факт, который агент передаёт на вход Save.
// Классификацию (Kind) и разбиение на атомарные факты делает агент (DESIGN.md §7.1).
type SaveInput struct {
	Kind       memory.Kind
	Scope      string
	Content    string
	Summary    string // ультра-короткая форма для брифа (DESIGN.md §4); пусто = не указана.
	Importance int    // 1..5; 0 трактуется как «не указано» → дефолт 3
	Tags       []string
	Links      []string // id целевых записей; висячие ссылки пропускаются молча (см. Save)
	Source     string   // session id / откуда пришло; пусто = не указан
}

// UpdateInput — правка существующей записи для Writer.Update. Указатели различают
// «поле не передано» (nil, не трогаем) от «поле передано» — так правка меняет
// только то, что агент явно прислал; Tags — исключение, полностью заменяет старый
// набор (как и в mcp updateInput), поэтому различается по nil-срезу. Links —
// другое исключение: в отличие от Tags, ДОБАВЛЯЕТСЯ к существующим исходящим
// связям записи, а не заменяет их (см. Update). Это доменный инвариант графа
// памяти: правка не должна молча стирать уже существующие рёбра записи, просто
// потому что вызывающий добавляет новое, — связь удаляется только явным
// действием, а не как побочный эффект Update. (Ровно на это опирается второй
// проход импортёра, DESIGN.md §14 Этап 5: он резолвит [[wiki-ссылки]] уже после
// того, как запись сохранена без links.)
type UpdateInput struct {
	Id         string
	Content    *string
	Summary    *string
	Importance *int // нормализуется ClampImportance — тем же правилом, что и Save
	Tags       []string
	Links      []string // id целевых записей, добавляемых к текущим links (rel=related);
	// висячие ссылки пропускаются молча, как в Save (см. resolveLinks); nil — не менять.
}

// Save принимает атомарный факт и сохраняет его по write path DESIGN.md §7:
//  1. валидирует и нормализует вход;
//  2. ищет похожую запись в том же scope+kind (лексически + SimHash по content) —
//     если находит, усиливает её (объединяет теги, поднимает importance, Touch) и
//     возвращает её id вместо вставки новой записи (merged=true); Summary из in при
//     этом НЕ применяется (см. reinforce) — усиление не должно тихо переписывать
//     summary уже существующего факта чужим значением;
//  3. резолвит Links на существующие записи (см. ограничение ниже);
//  4. генерирует ULID и вставляет запись (вместе с Summary, если он передан) через
//     Store.Put одной операцией (FTS5-индекс обновляется триггерами внутри Put
//     автоматически); merged=false;
//  5. если Embedder включён (Dim() > 0), синхронно считает вектор content и
//     сохраняет его через Store.PutVector; ошибка эмбеддинга/PutVector не
//     проваливает Save — запись остаётся без вектора до фонового досчёта
//     (см. embedNow, Store.ListMissingVectors, DESIGN.md §7.4, §9).
//
// Возвращаемый merged честно сообщает вызывающему коду (mcp.handleSave), был ли
// факт слит с существующим дублем, вместо вставки новой записи, — без отдельного
// запроса к Store для проверки.
//
// Автоопределение противоречий (→ SupersededBy) в MVP не делается: по DESIGN.md §7.2
// это требует смыслового суждения (Reasoner, Этап 2). В Этапе 1 supersede — только
// явное действие через memory_update/memory_forget.
//
// Известный пробел MVP: схема §4 объявляет `links.dst REFERENCES memories(id)` (FK),
// поэтому сослаться на ещё не существующий id (истинно «висячая» ссылка, как
// допускает §7.3) невозможно без нарушения FK. Save трактует это консервативно:
// ссылки на существующие id устанавливаются с rel=related, ссылки на несуществующие
// id молча отбрасываются (Save не должен падать из-за одной плохой ссылки).
// Настоящий резолвер отложенных/висячих ссылок — предмет будущих этапов.
func (w *Writer) Save(ctx context.Context, in SaveInput) (id string, merged bool, err error) {
	content := strings.TrimSpace(in.Content)
	if content == "" {
		return "", false, ErrEmptyContent
	}
	scope := strings.TrimSpace(in.Scope)
	if scope == "" {
		return "", false, ErrEmptyScope
	}
	switch in.Kind {
	case memory.KindSemantic, memory.KindEpisodic, memory.KindProcedural:
	default:
		return "", false, fmt.Errorf("%w: %q", ErrUnknownKind, in.Kind)
	}
	importance := ClampImportance(in.Importance)
	tags := unionTags(nil, in.Tags)

	if id, merged, err := w.dedup(ctx, scope, in.Kind, content, importance, tags); err != nil {
		return "", false, err
	} else if merged {
		return id, true, nil
	}

	links, err := w.resolveLinks(ctx, in.Links)
	if err != nil {
		return "", false, err
	}

	id, err = w.newULID()
	if err != nil {
		return "", false, fmt.Errorf("write: генерация id: %w", err)
	}

	var sourcePtr *string
	if s := strings.TrimSpace(in.Source); s != "" {
		sourcePtr = &s
	}
	var summaryPtr *string
	if s := strings.TrimSpace(in.Summary); s != "" {
		summaryPtr = &s
	}

	now := w.now()
	m := memory.Memory{
		Id:         id,
		Kind:       in.Kind,
		Scope:      scope,
		Content:    content,
		Summary:    summaryPtr,
		Importance: importance,
		CreatedAt:  now,
		UpdatedAt:  now,
		Source:     sourcePtr,
		Tags:       tags,
		Links:      links,
	}
	if err := w.store.Put(ctx, m); err != nil {
		return "", false, fmt.Errorf("write: сохранение записи: %w", err)
	}
	w.embedNow(ctx, id, content)
	return id, false, nil
}

// embedNow считает и сохраняет вектор content для записи id, если Embedder включён
// (Dim() > 0). Noop (Dim()==0) — Embed вообще не вызывается, векторная индексация
// не требуется. Любая ошибка (Embed или PutVector) только логируется в stderr и не
// прерывает вызывающий Save/Update — запись уже сохранена, а вектор досчитает
// фоновый джоб по Store.ListMissingVectors (DESIGN.md §7.4, §9, §15: деградация,
// а не падение).
func (w *Writer) embedNow(ctx context.Context, id, content string) {
	if w.embedder.Dim() <= 0 {
		return
	}
	vecs, err := w.embedder.Embed(ctx, []string{content})
	if err != nil {
		log.Printf("write: эмбеддинг записи %q: %v (вектор будет досчитан позже)", id, err)
		return
	}
	if len(vecs) < 1 {
		log.Printf("write: эмбеддинг записи %q не вернул вектор (вектор будет досчитан позже)", id)
		return
	}
	if err := w.store.PutVector(ctx, id, vecs[0]); err != nil {
		log.Printf("write: сохранение вектора записи %q: %v (вектор будет досчитан позже)", id, err)
	}
}

// dedup ищет в scope+kind запись, похожую на content (точное совпадение после
// нормализации либо SimHash-расстояние ≤ simhashDistanceThreshold), и если находит —
// усиливает её и возвращает (id, true, nil). Если похожих нет, возвращает ("", false, nil).
//
// SearchLexical не заполняет Tags кандидатов (см. store.SearchLexical) — это не
// проблема: сравнение идёт только по Content, а при слиянии reinforce отдельно
// запрашивает полную запись через Store.Get.
func (w *Writer) dedup(ctx context.Context, scope string, kind memory.Kind, content string, importance int, tags []string) (string, bool, error) {
	candidates, err := w.store.SearchLexical(ctx, memory.Query{
		Text:  content,
		Scope: scope,
		Kinds: []string{string(kind)},
		Limit: defaultDedupLimit,
	})
	if err != nil {
		return "", false, fmt.Errorf("write: поиск дублей: %w", err)
	}

	normNew := normalizeContent(content)

	for _, cand := range candidates {
		normOld := normalizeContent(cand.Memory.Content)
		if !isNearDuplicate(normNew, normOld) {
			continue
		}
		id, err := w.reinforce(ctx, cand.Memory.Id, importance, tags)
		if err != nil {
			return "", false, err
		}
		return id, true, nil
	}
	return "", false, nil
}

// isNearDuplicate решает, считать ли normNew/normOld (уже нормализованный по
// normalizeContent content) одним и тем же фактом: точное совпадение всегда
// валидный признак дубля; SimHash-расстояние — тоже, но только когда у ОБЕИХ
// строк есть хотя бы один токен (см. tokenize). Контент из одной пунктуации
// токенизируется в пустой набор слов, а simhash64("") вырождается в 0 для любого
// такого текста — без этой защиты два содержательно разных, но одинаково
// «беззубых» факта дали бы Hamming-расстояние 0 и молча слились бы.
func isNearDuplicate(normNew, normOld string) bool {
	if normNew == normOld {
		return true
	}
	if len(tokenize(normNew)) == 0 || len(tokenize(normOld)) == 0 {
		return false
	}
	return hammingDistance(simhash64(normNew), simhash64(normOld)) <= simhashDistanceThreshold
}

// IsNearDuplicate экспортирует то же правило «дублирования» контента, которое
// write path использует при вставке (см. isNearDuplicate выше), для повторного
// использования вне пакета — в частности, dedup-sweep джобом (DESIGN.md §11,
// internal/memory/maint/dedup.go), который ретроактивно чистит почти-дубли,
// накопившиеся в обход write path (импорт, консолидация, правки content).
// Принимает СЫРОЙ (ненормализованный) content обеих записей: нормализация и расчёт
// SimHash делаются внутри той же логикой, что и при записи, — вызывающий код не
// дублирует ни normalizeContent, ни порог simhashDistanceThreshold.
func IsNearDuplicate(a, b string) bool {
	return isNearDuplicate(normalizeContent(a), normalizeContent(b))
}

// reinforce усиливает существующую запись вместо вставки дубля: объединяет теги
// (union), поднимает importance до максимума старого/нового, обновляет updated_at
// и вызывает Touch (accessed_at/access_count) — сигнал повторного использования факта.
// Summary новой записи (SaveInput.Summary) сюда сознательно не передаётся и не
// применяется — усиление дубля не должно тихо переписывать summary существующего
// факта значением из входа, который был признан просто повторением того же факта.
//
// Полную запись загружаем через Store.Get, а не переиспользуем кандидата из
// SearchLexical: у кандидата не заполнены ни Tags (см. store.SearchLexical), ни,
// главное, Links — merged, построенный поверх кандидата, затёр бы исходящие связи
// записи при Store.Put (Put безусловно перезаписывает links записи по её текущему
// состоянию в переданном Memory).
func (w *Writer) reinforce(ctx context.Context, existingID string, importance int, newTags []string) (string, error) {
	existing, err := w.store.Get(ctx, existingID)
	if err != nil {
		return "", fmt.Errorf("write: получение дубля %q: %w", existingID, err)
	}

	merged := existing
	merged.Tags = unionTags(existing.Tags, newTags)
	if importance > merged.Importance {
		merged.Importance = importance
	}
	merged.UpdatedAt = w.now()

	if err := w.store.Put(ctx, merged); err != nil {
		return "", fmt.Errorf("write: усиление дубля %q: %w", existingID, err)
	}
	if err := w.store.Touch(ctx, existingID); err != nil {
		return "", fmt.Errorf("write: touch дубля %q: %w", existingID, err)
	}
	return existingID, nil
}

// resolveLinks проверяет существование каждого id из ids и возвращает связи rel=related
// только для существующих записей; несуществующие id пропускаются молча (см. godoc Save).
func (w *Writer) resolveLinks(ctx context.Context, ids []string) ([]memory.Link, error) {
	var links []memory.Link
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, err := w.store.Get(ctx, id); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue // висячая ссылка — известный пробел MVP, см. godoc Save
			}
			return nil, fmt.Errorf("write: проверка link %q: %w", id, err)
		}
		links = append(links, memory.Link{Dst: id, Rel: memory.RelRelated})
	}
	return links, nil
}

// newULID генерирует новый ULID на основе w.now()/w.entropy.
func (w *Writer) newULID() (string, error) {
	id, err := ulid.New(ulid.Timestamp(time.Unix(w.now(), 0)), w.entropy)
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// Update правит существующую запись (DESIGN.md §12 memory_update): применяются
// только не-nil поля UpdateInput, importance нормализуется той же ClampImportance,
// что и Save, — единый источник нормализации, чтобы memory_save и memory_update
// вели себя одинаково на границах диапазона (0 → дефолт 3, вне 1..5 → clamp), а не
// расходились (одна падает ошибкой, другая клампит). Links, в отличие от Tags,
// не заменяет текущий набор связей, а добавляет к нему (см. UpdateInput.Links) —
// резолвится той же resolveLinks, что и Save (висячие ссылки пропускаются молча).
// Если записи с Id нет, возвращает ошибку, оборачивающую store.ErrNotFound
// (проверяется errors.Is).
func (w *Writer) Update(ctx context.Context, in UpdateInput) error {
	id := strings.TrimSpace(in.Id)
	if id == "" {
		return fmt.Errorf("write: id обязателен")
	}

	m, err := w.store.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("write: получение %q: %w", id, err)
	}

	changed := false
	if in.Content != nil {
		content := strings.TrimSpace(*in.Content)
		if content == "" {
			return ErrEmptyContent
		}
		m.Content = content
		changed = true
	}
	if in.Summary != nil {
		summary := strings.TrimSpace(*in.Summary)
		if summary == "" {
			m.Summary = nil
		} else {
			m.Summary = &summary
		}
		changed = true
	}
	if in.Importance != nil {
		m.Importance = ClampImportance(*in.Importance)
		changed = true
	}
	if in.Tags != nil {
		m.Tags = unionTags(nil, in.Tags)
		changed = true
	}
	if in.Links != nil {
		resolved, err := w.resolveLinks(ctx, in.Links)
		if err != nil {
			return fmt.Errorf("write: резолв links %q: %w", id, err)
		}
		// Инвариант графа: запись не ссылается сама на себя — ребро id->id
		// бессмысленно (self-сосед в ListNeighbors). Отсекаем self-петли здесь,
		// в доменном write-пути, чтобы правило держалось для всех вызывающих
		// (импортёр, будущий memory_update с Links), а не в каждом из них.
		filtered := resolved[:0]
		for _, l := range resolved {
			if l.Dst == id {
				continue
			}
			filtered = append(filtered, l)
		}
		m.Links = mergeLinks(m.Links, filtered)
		changed = true
	}

	if !changed {
		return nil
	}

	m.UpdatedAt = w.now()
	if err := w.store.Put(ctx, m); err != nil {
		return fmt.Errorf("write: сохранение %q: %w", id, err)
	}
	if in.Content != nil {
		w.embedNow(ctx, id, m.Content)
	}
	return nil
}

// Forget мягко удаляет запись id — тумстоун (DESIGN.md §4, §12), без DELETE:
// если replacedBy пусто, запись помечается заменённой самой собой
// (Store.Supersede(id, id)) — superseded_by становится непустым, запись выпадает
// из recall/SearchLexical (фильтр superseded_by IS NULL), но сама строка остаётся
// в БД — история и связи (links/tags) не теряются. Если replacedBy указан,
// проверяется его существование, и делается обычный supersede на конкретную
// заменяющую запись.
func (w *Writer) Forget(ctx context.Context, id string, replacedBy string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("write: id обязателен")
	}
	replacedBy = strings.TrimSpace(replacedBy)

	target := replacedBy
	if target == "" {
		target = id // тумстоун — см. godoc выше
	} else if _, err := w.store.Get(ctx, replacedBy); err != nil {
		return fmt.Errorf("write: проверка replaced_by %q: %w", replacedBy, err)
	}

	if err := w.store.Supersede(ctx, id, target); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// mergeLinks объединяет текущие и добавляемые связи без дублей по паре (Dst, Rel):
// используется Update при обработке UpdateInput.Links — в отличие от unionTags,
// применяемой к Tags при полной замене, здесь исходные связи ВСЕГДА сохраняются,
// а не переписываются входом (см. годок UpdateInput.Links).
func mergeLinks(existing, additional []memory.Link) []memory.Link {
	seen := make(map[memory.Link]bool, len(existing)+len(additional))
	var out []memory.Link
	for _, l := range existing {
		if seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	for _, l := range additional {
		if seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	return out
}

// ClampImportance приводит importance к диапазону [1,5]; 0 (не указано) → дефолт 3.
// Экспортирована, чтобы вызывающие вне пакета (напр. cmd/memimport при печати
// -dry-run) нормализовали importance тем же правилом, что и Save/Update, и не
// расходились с реально сохраняемым значением.
func ClampImportance(importance int) int {
	if importance == 0 {
		return 3
	}
	if importance < 1 {
		return 1
	}
	if importance > 5 {
		return 5
	}
	return importance
}

// unionTags объединяет два набора тегов без повторов, сохраняя порядок: сперва a, потом b.
// Также служит нормализацией одного набора тегов при a=nil (обрезка пробелов,
// отбрасывание пустых/повторяющихся).
func unionTags(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	var out []string
	for _, t := range a {
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	for _, t := range b {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}
