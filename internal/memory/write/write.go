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
	"slices"
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
	// ErrInvalidTTL — TTL не разобрался как Go-длительность (time.ParseDuration)
	// либо разобрался в неположительную длительность (задача 9.4). Fail-fast:
	// Save/Update возвращают ошибку, а не молча трактуют невалидный ttl как
	// "вечно" — агент должен узнать об опечатке в формате сразу.
	ErrInvalidTTL = errors.New("write: невалидный ttl")
	// ErrUnknownStatus — status не входит в memory.Statuses (задача 14.1).
	ErrUnknownStatus = errors.New("write: неизвестный status")
	// ErrStatusNotApplicable — status передан для записи, у которой его не
	// бывает: состояние есть только у задач (memory.KindTask). Fail-fast, а не
	// молчаливое отбрасывание, — иначе агент, перепутавший kind, считал бы, что
	// завёл задачу, и не нашёл бы её ни по одному фильтру статуса.
	ErrStatusNotApplicable = errors.New("write: status допустим только для kind=task")
	// ErrTaskSummaryRequired — у задачи (kind=task) не задан summary. Content
	// задачи — документ целиком, в бриф recall выводится Summary (см.
	// recall.formatLine), поэтому заголовок задачи обязателен: без него бриф
	// распух бы полным текстом одной задачи.
	ErrTaskSummaryRequired = errors.New("write: для kind=task обязателен summary — заголовок задачи")
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

// LinkInput — один элемент входного Links для Save/Update: Dst — id целевой записи,
// Rel — тип связи графа (DESIGN.md §4: related | supersedes | causes | part_of).
// Пустой Rel трактуется как related — единственное место этого дефолта, см.
// resolveLinks (задача 9.3, закрывает тех-долг задачи 5.1: типизированные связи
// доступны и из memory_save, и из memory_update). Валидация Rel против набора
// допустимых значений — забота вызывающего слоя (mcp), на границе входа; write
// доверяет уже провалидированному значению и лишь применяет дефолт для пустого.
//
// rel=supersedes здесь — ПРОСТО ребро графа связей, наравне с остальными; оно НЕ
// подменяет поле memory.Memory.SupersededBy и механику Writer.Forget/Store.Supersede
// (мягкое удаление, исключение записи из recall) — это две независимые вещи: ребро
// graph — знание агента о связи между двумя живыми записями, superseded_by —
// экспирация конкретной записи.
type LinkInput struct {
	Dst string
	Rel string
}

// SaveInput — структурированный факт, который агент передаёт на вход Save.
// Классификацию (Kind) и разбиение на атомарные факты делает агент (DESIGN.md §7.1).
type SaveInput struct {
	Kind    memory.Kind
	Scope   string
	Content string
	Summary string // ультра-короткая форма для брифа (DESIGN.md §4); пусто = не указана.
	// Status — состояние задачи (memory.Status), только для Kind == KindTask:
	// пусто → memory.StatusTodo (задача заводится не начатой). Для остальных
	// kind непустое значение — ErrStatusNotApplicable (см. normalizeStatus).
	Status     string
	Importance int // 1..5; 0 трактуется как «не указано» → дефолт 3
	Tags       []string
	Links      []LinkInput // связи с целевыми записями; висячие ссылки пропускаются молча (см. Save)
	Source     string      // session id / откуда пришло; пусто = не указан
	// TTL — явный срок жизни факта: Go-длительность от текущего момента
	// (time.ParseDuration, напр. "72h"), а не абсолютная дата — агенту почти
	// всегда известно "сколько актуально", а не "до какого unix ts" (тот же
	// принцип, что у recall CreatedAfter/CreatedBefore, DESIGN.md §12). Пусто —
	// вечная запись (ExpiresAt=0). Невалидная строка или неположительная
	// длительность — ErrInvalidTTL (fail-fast, см. parseTTL).
	TTL string
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
	// Status — новое состояние задачи (memory.Status), задача 14.1: nil — не
	// менять. Применим только к записи с Kind == KindTask (иначе
	// ErrStatusNotApplicable), значение обязано входить в memory.Statuses.
	// Пустая строка здесь НЕ сентинел «снять» (в отличие от Summary/TTL):
	// задача без состояния бессмысленна, поэтому пустой status — та же ошибка,
	// что и неизвестный.
	Status *string
	Links  []LinkInput // связи, добавляемые к текущим links записи (пустой Rel -> related);
	// висячие ссылки пропускаются молча, как в Save (см. resolveLinks); nil — не менять.
	// TTL продлевает или снимает срок жизни записи (задача 9.4): nil — не менять
	// текущий ExpiresAt; "" или "0" — снять срок (запись становится вечной,
	// ЯВНО задокументированный сентинел, тот же паттерн, что у Summary выше:
	// пустая строка снимает поле); любая другая Go-длительность (напр. "72h") —
	// новый срок, считается от ТЕКУЩЕГО момента (не от исходного created_at) —
	// это и есть "продлить". Невалидная строка (кроме "0"/"") — ErrInvalidTTL.
	TTL *string
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
// SaveInput.TTL (задача 9.4) разбирается ДО дедупликации (fail-fast: невалидная
// строка возвращает ErrInvalidTTL, даже если факт в итоге слился бы с
// существующей записью, а не вставился новой) — см. parseTTL.
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
// ссылки на существующие id устанавливаются с типом Rel, который передал вызывающий
// (пустой Rel -> related, см. LinkInput/resolveLinks), ссылки на несуществующие
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
	case memory.KindSemantic, memory.KindEpisodic, memory.KindProcedural, memory.KindDebt, memory.KindTask:
	default:
		return "", false, fmt.Errorf("%w: %q", ErrUnknownKind, in.Kind)
	}
	summary := strings.TrimSpace(in.Summary)
	if in.Kind == memory.KindTask && summary == "" {
		return "", false, ErrTaskSummaryRequired
	}
	status, err := normalizeStatus(in.Kind, in.Status)
	if err != nil {
		return "", false, err
	}
	now := w.now()
	expiresAt, err := parseTTL(in.TTL, now)
	if err != nil {
		return "", false, err
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
	if summary != "" {
		summaryPtr = &summary
	}

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
		ExpiresAt:  expiresAt,
		Status:     status,
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
//
// Задачи (memory.KindTask) дедупом не проходят вовсе — см. SkipDedup.
func (w *Writer) dedup(ctx context.Context, scope string, kind memory.Kind, content string, importance int, tags []string) (string, bool, error) {
	if SkipDedup(kind) {
		return "", false, nil
	}
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

// SkipDedup сообщает, исключён ли этот kind из дедупликации похожего контента —
// и на write path (Writer.dedup), и в ретроактивном dedup-sweep (DESIGN.md §11,
// maint.DedupSweeper), чтобы правило существовало в коде ровно один раз.
//
// Исключены задачи (memory.KindTask): у них content — документ (описание,
// границы, критерий приёмки), и две РАЗНЫЕ задачи по одной подсистеме
// текстуально похожи настолько, что SimHash-порог, откалиброванный на коротких
// атомарных фактах (simhashDistanceThreshold), счёл бы их дублем. Цена ошибки
// здесь несимметрична: лишний факт в памяти — шум, а слитая задача — потерянная
// работа, о которой агент уже не вспомнит. Дубли задач разводит сам агент,
// заводящий их осознанно, а не эвристика.
func SkipDedup(kind memory.Kind) bool { return kind == memory.KindTask }

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

// resolveLinks проверяет существование каждого Dst из ins и возвращает связи только
// для существующих записей; несуществующие id пропускаются молча (см. godoc Save).
// Единственное место дефолта Rel: пустой Rel (back-compat строковая форма входа
// MCP, см. mcp.linkEntry) трактуется как memory.RelRelated — Save и Update оба
// проходят через resolveLinks, поэтому дефолт не расходится между инструментами.
func (w *Writer) resolveLinks(ctx context.Context, ins []LinkInput) ([]memory.Link, error) {
	var links []memory.Link
	for _, in := range ins {
		id := strings.TrimSpace(in.Dst)
		if id == "" {
			continue
		}
		if _, err := w.store.Get(ctx, id); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue // висячая ссылка — известный пробел MVP, см. godoc Save
			}
			return nil, fmt.Errorf("write: проверка link %q: %w", id, err)
		}
		rel := strings.TrimSpace(in.Rel)
		if rel == "" {
			rel = memory.RelRelated
		}
		links = append(links, memory.Link{Dst: id, Rel: rel})
	}
	return links, nil
}

// parseTTL разбирает строку TTL (SaveInput.TTL/непустой *UpdateInput.TTL,
// задача 9.4) — Go-длительность (time.ParseDuration, напр. "72h") — в abs unix ts
// истечения (now + длительность). Пустая строка означает "без срока" и
// возвращает (0, nil) — вечная запись/поле не тронуто вызывающим кодом (сентинел
// "0" для Update разбирается отдельно, ДО вызова этой функции, см.
// Writer.Update). Невалидная строка (не парсится time.ParseDuration) или
// неположительная длительность (<= 0) — ErrInvalidTTL: fail-fast, а не
// молчаливый откат к вечной записи.
func parseTTL(ttl string, now int64) (int64, error) {
	ttl = strings.TrimSpace(ttl)
	if ttl == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(ttl)
	if err != nil {
		return 0, fmt.Errorf("%w %q: %w", ErrInvalidTTL, ttl, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%w %q: длительность должна быть положительной", ErrInvalidTTL, ttl)
	}
	return now + int64(d.Seconds()), nil
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
// TTL (задача 9.4) продлевает срок жизни (новый ExpiresAt = сейчас + длительность)
// или снимает его ("0"/"" → вечная, см. годок UpdateInput.TTL) — см. parseTTL.
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
	if in.TTL != nil {
		ttl := strings.TrimSpace(*in.TTL)
		if ttl == "" || ttl == "0" {
			// Сентинел "снять срок" (задача 9.4, см. годок UpdateInput.TTL) —
			// та же трактовка пустой строки, что и у Summary выше.
			m.ExpiresAt = 0
		} else {
			expiresAt, err := parseTTL(ttl, w.now())
			if err != nil {
				return err
			}
			m.ExpiresAt = expiresAt
		}
		changed = true
	}
	if in.Status != nil {
		status, err := normalizeStatus(m.Kind, *in.Status)
		if err != nil {
			return err
		}
		// Пустой вход для задачи normalizeStatus трактует как «не указан» и даёт
		// дефолт todo — при СОЗДАНИИ это верно, при правке означало бы молчаливый
		// откат состояния к начальному вместо того, о чём просил вызывающий.
		if strings.TrimSpace(*in.Status) == "" {
			return fmt.Errorf("%w: пустое значение", ErrUnknownStatus)
		}
		m.Status = status
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

// normalizeStatus приводит переданный status к memory.Status с учётом kind
// записи (задача 14.1) — единственное место этого правила для Save и Update:
//   - kind != task: непустой status — ErrStatusNotApplicable (состояние есть
//     только у задач), пустой — "" (у записи нет статуса);
//   - kind == task: пустой status — дефолт memory.StatusTodo (задача заводится
//     не начатой), значение вне memory.Statuses — ErrUnknownStatus.
func normalizeStatus(kind memory.Kind, status string) (memory.Status, error) {
	status = strings.TrimSpace(status)
	if kind != memory.KindTask {
		if status != "" {
			return "", fmt.Errorf("%w (получен %q для kind=%q)", ErrStatusNotApplicable, status, kind)
		}
		return "", nil
	}
	if status == "" {
		return memory.StatusTodo, nil
	}
	if !slices.Contains(memory.Statuses, memory.Status(status)) {
		return "", fmt.Errorf("%w: %q, допустимые значения: %s", ErrUnknownStatus, status, statusList())
	}
	return memory.Status(status), nil
}

// statusList перечисляет допустимые статусы через запятую — для текста ошибки
// (тот же перечень memory.Statuses, что и в валидации; не расходится).
func statusList() string {
	out := make([]string, len(memory.Statuses))
	for i, s := range memory.Statuses {
		out[i] = string(s)
	}
	return strings.Join(out, ", ")
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
