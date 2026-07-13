// Package memory описывает доменную модель AgentMemory: атомарную единицу памяти,
// параметры запроса на её поиск и результат сборки recall-брифа.
package memory

// Kind — тип единицы памяти (см. DESIGN.md §2).
type Kind string

const (
	// KindSemantic — факты, знания, предпочтения. Живут долго.
	KindSemantic Kind = "semantic"
	// KindEpisodic — события сессий. Живут средне, устаревают.
	KindEpisodic Kind = "episodic"
	// KindProcedural — рецепты, воркфлоу. Живут долго.
	KindProcedural Kind = "procedural"
)

// Значения Link.Rel — тип связи между записями в графе links.
const (
	RelRelated    = "related"
	RelSupersedes = "supersedes"
	RelCauses     = "causes"
	RelPartOf     = "part_of"
)

// Link — направленное ребро графа памяти: текущая запись -> Dst с отношением Rel.
type Link struct {
	Dst string // id записи-цели
	Rel string // related | supersedes | causes | part_of (пусто трактуется как related)
}

// Memory — атомарная единица памяти. Соответствует строке таблицы `memories`
// (DESIGN.md §4) плюс связанные tags/links.
type Memory struct {
	Id           string // ULID
	Kind         Kind
	Scope        string // user:* | project:<name> | agent:self
	Content      string // сам факт, атомарно
	Summary      *string
	Importance   int   // 1..5
	CreatedAt    int64 // unix ts
	UpdatedAt    int64
	AccessedAt   *int64
	AccessCount  int
	SupersededBy *string // id заменившей записи (мягкое забывание)
	Source       *string // session id / откуда пришло
	// ExpiresAt — явный срок жизни записи (unix ts, задача 9.4): 0 (или NULL в
	// БД) означает вечную запись, без срока. В отличие от decay (эвристическое
	// затухание неиспользуемых episodic, DESIGN.md §11), TTL — точная, заранее
	// известная агенту дата истечения факта (см. write.SaveInput.TTL/
	// UpdateInput.TTL, maint.Expirer). recall скрывает просроченные записи
	// сразу (Store.SearchLexical/SearchVector), фоновый ExpireSweep превращает
	// их в тумстоун (Supersede) при следующем прогоне планировщика.
	ExpiresAt int64
	Tags      []string
	Links     []Link
}

// Query — параметры поиска для recall.
type Query struct {
	Text string // текст запроса
	// Scopes — набор пространств для фильтра (DESIGN.md §2.1), напр.
	// ["project:<name>", "user:*"]. Если пуст — откат на одиночный Scope; если
	// пусты оба поля — фильтра по scope нет (adhoc-режим, мультипроектность выключена).
	Scopes []string
	Scope  string   // фильтр по одиночному scope (используется write-путём/дедупом)
	Kinds  []string // фильтр по kind, пусто = все
	// Tags — фильтр по тегам: запись проходит, если имеет хотя бы один из
	// перечисленных тегов (OR-семантика, задача 8.2). Пусто = без фильтра.
	Tags        []string
	Limit       int   // максимум кандидатов от источника
	TokenBudget int   // бюджет токенов для сборки брифа
	SinceTs     int64 // фильтр свежести: только записи с created_at >= SinceTs (0 = без фильтра)
	// CreatedAfter/CreatedBefore — фильтр по дате создания (unix ts, 0 = фильтр
	// выключен), задача 8.2: created_at >= CreatedAfter и/или created_at <=
	// CreatedBefore. Отдельно от SinceTs — тот адресован свежести ранжирования, эти
	// поля пробрасываются явным пользовательским запросом (MCP memory_recall).
	CreatedAfter  int64
	CreatedBefore int64
	Summarize     bool // запросить у Reasoner LLM-сводку брифа (DESIGN.md §8)
	// Explain — добавить под каждой записью брифа трассу ранжирования: источники
	// кандидата (lexical/vector) и разложение итогового скора на слагаемые
	// (DESIGN.md §8, §12, задача 8.1). Отладка/тюнинг весов [recall].weights —
	// само ранжирование и отбор записей не меняются.
	Explain bool
}

// Candidate — кандидат на попадание в бриф: запись + оценка релевантности
// от конкретного источника поиска (для последующего RRF-слияния). Store.SearchLexical/
// SearchVector отдают Source ровно одного источника ("lexical" | "vector");
// recall.Recaller.Candidates (задача 10.1) отдаёт уже слитый после RRF результат, где
// Source может быть составным ("lexical+vector") — те же значения, что в explain
// (задача 8.1, DESIGN.md §8).
type Candidate struct {
	Memory Memory
	Score  float64
	Source string // "lexical" | "vector" | "lexical+vector"
}

// Brief — результат сборки recall: готовый Markdown-текст и метаданные о том,
// сколько записей вошло/свёрнуто.
type Brief struct {
	Markdown     string
	Count        int // сколько записей вошло в бриф
	Collapsed    int // сколько релевантных записей свёрнуто (не поместились в бюджет)
	ApproxTokens int
}
