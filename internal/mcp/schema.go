package mcp

import (
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
)

// kindEnum — допустимые значения поля kind/kinds во входных схемах инструментов
// (DESIGN.md §2: semantic | episodic | procedural).
var kindEnum = []any{"semantic", "episodic", "procedural"}

// mustSchema строит JSON-схему из Go-типа T через инференс jsonschema-go (описания
// берутся из тегов `jsonschema`, обязательность — из отсутствия `omitempty`/`omitzero`,
// см. godoc jsonschema.For). Ошибка инференса возможна только при программистской
// ошибке в определении типа T, поэтому — как и mcp.AddTool в SDK — паникуем при
// регистрации инструментов, а не пробрасываем ошибку в рантайм-путь запроса.
func mustSchema[T any]() *jsonschema.Schema {
	s, err := jsonschema.For[T](nil)
	if err != nil {
		panic(fmt.Sprintf("mcp: построение схемы %T: %v", *new(T), err))
	}
	return s
}

// saveInput — вход инструмента memory_save (DESIGN.md §12).
type saveInput struct {
	Kind       string   `json:"kind" jsonschema:"тип памяти: semantic (факт/знание/предпочтение) | episodic (событие сессии) | procedural (рецепт/воркфлоу)"`
	Content    string   `json:"content" jsonschema:"сам факт, атомарно — 1-3 предложения"`
	Scope      string   `json:"scope" jsonschema:"пространство имён записи: user:*, project:<name> или agent:self"`
	Importance int      `json:"importance,omitempty" jsonschema:"важность 1..5 (по умолчанию 3, если не указано)"`
	Tags       []string `json:"tags,omitempty" jsonschema:"теги для группировки и поиска"`
	Links      []string `json:"links,omitempty" jsonschema:"id (ULID) уже существующих записей, с которыми связан этот факт"`
	Source     string   `json:"source,omitempty" jsonschema:"откуда пришёл факт: id сессии или иной источник"`
}

func saveInputSchema() *jsonschema.Schema {
	s := mustSchema[saveInput]()
	s.Properties["kind"].Enum = kindEnum
	return s
}

// recallInput — вход инструмента memory_recall (DESIGN.md §12).
type recallInput struct {
	Query       string   `json:"query" jsonschema:"текст запроса — что нужно вспомнить, в свободной форме"`
	Scope       string   `json:"scope,omitempty" jsonschema:"фильтр по одиночному пространству имён: user:*, project:<name> или agent:self (back-compat, см. scopes)"`
	Scopes      []string `json:"scopes,omitempty" jsonschema:"фильтр по набору пространств имён, перекрывает scope, напр. [\"project:<name>\", \"user:*\"]; пусто — дефолт инстанса: свой проект (project:<name>) плюс общий user:*; если у инстанса не настроен проект — без фильтра"`
	Kinds       []string `json:"kinds,omitempty" jsonschema:"фильтр по типу памяти; пусто — искать по всем типам"`
	TokenBudget int      `json:"token_budget,omitempty" jsonschema:"бюджет токенов на бриф (по умолчанию берётся из конфига)"`
	Summarize   bool     `json:"summarize,omitempty" jsonschema:"дополнительно сжать бриф в короткую LLM-сводку (### Сводка) поверх списка записей"`
	Explain     bool     `json:"explain,omitempty" jsonschema:"добавить к каждой записи трассу ранжирования: источники (lexical/vector), RRF и разложение итогового скора — для отладки и тюнинга весов"`
}

func recallInputSchema() *jsonschema.Schema {
	s := mustSchema[recallInput]()
	s.Properties["kinds"].Items.Enum = kindEnum
	return s
}

// updateInput — вход инструмента memory_update (DESIGN.md §12). Указатели различают
// «поле не передано» (nil, не трогаем) от «поле передано» — так правка меняет только
// то, что агент явно прислал.
type updateInput struct {
	ID         string   `json:"id" jsonschema:"id (ULID) обновляемой записи"`
	Content    *string  `json:"content,omitempty" jsonschema:"новый текст факта, если его нужно изменить"`
	Summary    *string  `json:"summary,omitempty" jsonschema:"новая краткая форма для брифа; пустая строка очищает summary"`
	Importance *int     `json:"importance,omitempty" jsonschema:"новая важность 1..5"`
	Tags       []string `json:"tags,omitempty" jsonschema:"новый полный набор тегов — полностью заменяет старый"`
}

func updateInputSchema() *jsonschema.Schema {
	return mustSchema[updateInput]()
}

// forgetInput — вход инструмента memory_forget (DESIGN.md §12).
type forgetInput struct {
	ID         string `json:"id" jsonschema:"id (ULID) записи, которую нужно забыть"`
	ReplacedBy string `json:"replaced_by,omitempty" jsonschema:"id (ULID) записи-замены, если факт устарел из-за более новой; если не указано — запись становится тумстоуном (архивируется без замены)"`
}

func forgetInputSchema() *jsonschema.Schema {
	return mustSchema[forgetInput]()
}
