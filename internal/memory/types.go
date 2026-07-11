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
	Tags         []string
	Links        []Link
}

// Query — параметры поиска для recall.
type Query struct {
	Text        string   // текст запроса
	Scope       string   // фильтр по scope
	Kinds       []string // фильтр по kind, пусто = все
	Limit       int      // максимум кандидатов от источника
	TokenBudget int      // бюджет токенов для сборки брифа
	SinceTs     int64    // фильтр свежести: только записи с created_at >= SinceTs (0 = без фильтра)
	Summarize   bool     // запросить у Reasoner LLM-сводку брифа (DESIGN.md §8)
}

// Candidate — кандидат на попадание в бриф: запись + оценка релевантности
// от конкретного источника поиска (для последующего RRF-слияния).
type Candidate struct {
	Memory Memory
	Score  float64
	Source string // "lexical" | "vector"
}

// Brief — результат сборки recall: готовый Markdown-текст и метаданные о том,
// сколько записей вошло/свёрнуто.
type Brief struct {
	Markdown     string
	Count        int // сколько записей вошло в бриф
	Collapsed    int // сколько релевантных записей свёрнуто (не поместились в бюджет)
	ApproxTokens int
}
