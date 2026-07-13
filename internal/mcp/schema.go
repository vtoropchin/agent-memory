package mcp

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"agentmemory/internal/memory"
	"agentmemory/internal/memory/write"
)

// kindEnum — допустимые значения поля kind/kinds во входных схемах инструментов
// (DESIGN.md §2: semantic | episodic | procedural).
var kindEnum = []any{"semantic", "episodic", "procedural"}

// linkRelValues — допустимые значения rel во входном links (DESIGN.md §4: типы
// рёбер графа связей). Используются и для валидации (normalizeLinks), и для
// текста ошибки/описания схемы — одно перечисление, не расходится.
var linkRelValues = []string{memory.RelRelated, memory.RelSupersedes, memory.RelCauses, memory.RelPartOf}

// ttlDescription — общее описание поля ttl для memory_save/memory_update
// (DESIGN.md §4, §11, §12, задача 9.4): Go-длительность от текущего момента, а
// не абсолютная дата — тот же принцип, что у recall created_after/created_before
// (агенту проще сказать "актуально ещё сутки", чем посчитать unix ts).
const ttlDescription = "явный срок жизни факта: Go-длительность от текущего момента (\"24h\" — сутки, \"72h\" — трое суток, \"168h\" — неделя); пусто = вечно, без срока. Задавай, когда у факта есть заранее известная дата истечения (токен деплоя действует до даты X, фичефлаг до релиза, встреча в конкретный день) — по истечении запись перестаёт участвовать в recall и уходит в архив."

// linksDescription — общее описание поля links для memory_save/memory_update
// (задача 9.3, закрывает тех-долг задачи 5.1: типизированные связи доступны из
// обоих инструментов). Обратная совместимость обязательна — элемент массива либо
// ULID-строка (id существующей записи, тип связи будет related), либо объект
// {"id":"...", "rel":"..."} с явным типом связи.
const linksDescription = "связи с уже существующими записями графа памяти. Элемент — либо строка-id (ULID), либо объект {\"id\":\"01J...\",\"rel\":\"causes\"}. Типы связи (rel): related — просто связано (по умолчанию, в т.ч. для строковой формы); supersedes — эта запись новая версия целевой (ТОЛЬКО ребро графа, не заменяет фактическое устаревание — для него используй memory_update+memory_forget); causes — эта запись следствие целевой; part_of — эта запись часть процедуры/темы целевой. Пример: [\"01J8Z...\", {\"id\":\"01J8Z...\",\"rel\":\"causes\"}]."

// linkEntry — один элемент входного links (DESIGN.md §4, §7, §12; задача 9.3):
// либо ULID-строка цели (back-compat, Rel остаётся пустым — дефолт в related
// применяет write.resolveLinks, единственное место), либо объект
// {"id":"...","rel":"..."}. UnmarshalJSON нормализует обе формы в единый
// внутренний вид (Dst, Rel) до передачи в write; сама валидация значения Rel —
// в normalizeLinks, не здесь (разбор формата и валидация значения — разные заботы).
type linkEntry struct {
	Dst string
	Rel string
}

// UnmarshalJSON принимает элемент links либо как JSON-строку (id, back-compat),
// либо как объект {"id":"...", "rel":"..."}.
func (l *linkEntry) UnmarshalJSON(data []byte) error {
	var id string
	if err := json.Unmarshal(data, &id); err == nil {
		l.Dst = id
		l.Rel = ""
		return nil
	}
	var obj struct {
		ID  string `json:"id"`
		Rel string `json:"rel"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return fmt.Errorf("links: элемент должен быть строкой id либо объектом {\"id\":.., \"rel\":..}: %w", err)
	}
	l.Dst = obj.ID
	l.Rel = obj.Rel
	return nil
}

// normalizeLinks валидирует и переводит уже разобранные элементы links (обе формы
// unifицированы linkEntry.UnmarshalJSON) в write.LinkInput — общая для
// memory_save/memory_update точка валидации (DESIGN.md §12, задача 9.3). Непустой
// Rel обязан входить в linkRelValues: неизвестное значение (опечатка) сразу
// возвращает ошибку инструмента с перечнем допустимых, БЕЗ молчаливого фолбэка в
// related (fail-fast). Пустой Rel (back-compat строковая форма или объект без
// rel) пропускается как есть — дефолт в related делает write.resolveLinks.
func normalizeLinks(entries []linkEntry) ([]write.LinkInput, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	out := make([]write.LinkInput, 0, len(entries))
	for _, e := range entries {
		if e.Rel != "" && !slices.Contains(linkRelValues, e.Rel) {
			return nil, fmt.Errorf("links: неизвестный rel %q, допустимые значения: %s", e.Rel, strings.Join(linkRelValues, ", "))
		}
		out = append(out, write.LinkInput{Dst: e.Dst, Rel: e.Rel})
	}
	return out, nil
}

// linkItemSchema строит JSON-схему одного элемента links: oneOf строки
// (back-compat) и объекта {"id":.., "rel":..} — типизированная форма (DESIGN.md
// §4, §7, §12; задача 9.3). Rel сознательно без Enum в схеме — валидация значения
// (с содержательной ошибкой, перечисляющей допустимые варианты) делается кодом в
// normalizeLinks, а не JSON-схемой: там текст ошибки полностью под нашим
// контролем и покрыт тестами, а не зависит от формата сообщений валидатора схемы.
func linkItemSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		OneOf: []*jsonschema.Schema{
			{Type: "string", Description: "id (ULID) существующей записи — back-compat форма, тип связи будет related"},
			{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"id":  {Type: "string", Description: "id (ULID) целевой записи"},
					"rel": {Type: "string", Description: "тип связи: related | supersedes | causes | part_of (см. описание поля links)"},
				},
				Required: []string{"id"},
			},
		},
	}
}

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
	Kind       string      `json:"kind" jsonschema:"тип памяти: semantic (факт/знание/предпочтение) | episodic (событие сессии) | procedural (рецепт/воркфлоу)"`
	Content    string      `json:"content" jsonschema:"сам факт, атомарно — 1-3 предложения"`
	Scope      string      `json:"scope" jsonschema:"пространство имён записи: user:*, project:<name> или agent:self"`
	Importance int         `json:"importance,omitempty" jsonschema:"важность 1..5 (по умолчанию 3, если не указано)"`
	Tags       []string    `json:"tags,omitempty" jsonschema:"теги для группировки и поиска"`
	Links      []linkEntry `json:"links,omitempty"`
	Source     string      `json:"source,omitempty" jsonschema:"откуда пришёл факт: id сессии или иной источник"`
	TTL        string      `json:"ttl,omitempty" jsonschema:"явный срок жизни факта (см. описание поля)"`
}

func saveInputSchema() *jsonschema.Schema {
	s := mustSchema[saveInput]()
	s.Properties["kind"].Enum = kindEnum
	s.Properties["links"] = &jsonschema.Schema{Type: "array", Items: linkItemSchema(), Description: linksDescription}
	s.Properties["ttl"].Description = ttlDescription
	return s
}

// recallInput — вход инструмента memory_recall (DESIGN.md §12).
type recallInput struct {
	Query  string   `json:"query" jsonschema:"текст запроса — что нужно вспомнить, в свободной форме"`
	Scope  string   `json:"scope,omitempty" jsonschema:"фильтр по одиночному пространству имён: user:*, project:<name> или agent:self (back-compat, см. scopes)"`
	Scopes []string `json:"scopes,omitempty" jsonschema:"фильтр по набору пространств имён, перекрывает scope, напр. [\"project:<name>\", \"user:*\"]; пусто — дефолт инстанса: свой проект (project:<name>) плюс общий user:*; если у инстанса не настроен проект — без фильтра"`
	Kinds  []string `json:"kinds,omitempty" jsonschema:"фильтр по типу памяти; пусто — искать по всем типам"`
	Tags   []string `json:"tags,omitempty" jsonschema:"вернуть только записи, у которых есть хотя бы один из перечисленных тегов (OR); пусто — без фильтра по тегам"`
	// CreatedAfter/CreatedBefore — задача 8.2. Формат выбран длительностью назад от
	// текущего момента (не абсолютной датой) — типичный запрос агента формулируется
	// относительно "сейчас" ("за последнюю неделю"), а не конкретным календарным
	// днём; парсится через time.ParseDuration (см. parseRecallDuration в recall.go).
	CreatedAfter  string `json:"created_after,omitempty" jsonschema:"нижняя граница свежести: вернуть только записи не старше указанной длительности назад от текущего момента, формат time.ParseDuration (\"168h\" = за последнюю неделю, \"24h\" = за сутки); пусто — без нижней границы"`
	CreatedBefore string `json:"created_before,omitempty" jsonschema:"верхняя граница свежести: вернуть только записи старше указанной длительности назад от текущего момента, тот же формат, что created_after; пусто — без верхней границы"`
	TokenBudget   int    `json:"token_budget,omitempty" jsonschema:"бюджет токенов на бриф (по умолчанию берётся из конфига)"`
	Summarize     bool   `json:"summarize,omitempty" jsonschema:"дополнительно сжать бриф в короткую LLM-сводку (### Сводка) поверх списка записей"`
	Explain       bool   `json:"explain,omitempty" jsonschema:"добавить к каждой записи трассу ранжирования: источники (lexical/vector), RRF и разложение итогового скора — для отладки и тюнинга весов"`
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
	ID         string      `json:"id" jsonschema:"id (ULID) обновляемой записи"`
	Content    *string     `json:"content,omitempty" jsonschema:"новый текст факта, если его нужно изменить"`
	Summary    *string     `json:"summary,omitempty" jsonschema:"новая краткая форма для брифа; пустая строка очищает summary"`
	Importance *int        `json:"importance,omitempty" jsonschema:"новая важность 1..5"`
	Tags       []string    `json:"tags,omitempty" jsonschema:"новый полный набор тегов — полностью заменяет старый"`
	Links      []linkEntry `json:"links,omitempty"` // ДОБАВЛЯЮТСЯ к текущим links записи (не заменяют, в отличие от Tags — см. write.UpdateInput.Links)
	// TTL — указатель, а не string: отличает "не менять срок" (nil, поле не
	// передано) от "снять срок" (пустая строка или "0", write.UpdateInput.TTL).
	TTL *string `json:"ttl,omitempty"`
}

func updateInputSchema() *jsonschema.Schema {
	s := mustSchema[updateInput]()
	linksSchema := &jsonschema.Schema{Type: "array", Items: linkItemSchema(), Description: linksDescription + " В отличие от tags, links ДОБАВЛЯЕТСЯ к уже существующим связям записи, а не заменяет их."}
	s.Properties["links"] = linksSchema
	s.Properties["ttl"].Description = ttlDescription + " Чтобы продлить или установить срок, передай длительность; чтобы снять срок (сделать запись вечной), передай \"0\"; не указано — текущий срок не меняется."
	return s
}

// forgetInput — вход инструмента memory_forget (DESIGN.md §12).
type forgetInput struct {
	ID         string `json:"id" jsonschema:"id (ULID) записи, которую нужно забыть"`
	ReplacedBy string `json:"replaced_by,omitempty" jsonschema:"id (ULID) записи-замены, если факт устарел из-за более новой; если не указано — запись становится тумстоуном (архивируется без замены)"`
}

func forgetInputSchema() *jsonschema.Schema {
	return mustSchema[forgetInput]()
}
