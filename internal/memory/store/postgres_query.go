package store

import (
	"strconv"
	"strings"

	"agentmemory/internal/memory"
)

// argList накапливает позиционные аргументы запроса и раздаёт для них плейсхолдеры
// $1, $2, ... в порядке добавления — единая точка нумерации для запросов, чьи условия
// собираются по кускам (metaFiltersPG, scopePredicatePG и т.д., как в sqlite.go
// собираются conds/args в lockstep через `?`). add возвращает готовую подстроку
// плейсхолдера, которую вызывающий код вставляет прямо в текст запроса.
type argList struct {
	args []any
}

func (a *argList) add(v any) string {
	a.args = append(a.args, v)
	return "$" + strconv.Itoa(len(a.args))
}

// scopePredicatePG — аналог scopePredicate (sqlite.go) для $N-плейсхолдеров: условие
// фильтра по scope (DESIGN.md §2.1). Приоритет: набор scopes (m.scope IN (...)) >
// одиночный scope (m.scope = ...) > без фильтра (пустая строка), если оба параметра пусты.
func scopePredicatePG(a *argList, scopes []string, scope string) string {
	if len(scopes) > 0 {
		placeholders := make([]string, len(scopes))
		for i, sc := range scopes {
			placeholders[i] = a.add(sc)
		}
		return "m.scope IN (" + strings.Join(placeholders, ",") + ")"
	}
	if scope != "" {
		return "m.scope = " + a.add(scope)
	}
	return ""
}

// kindPredicatePG — аналог kindPredicate (sqlite.go) для $N-плейсхолдеров: условие
// фильтра по kind (m.kind IN (...)). Пустой набор kinds -> без фильтра.
func kindPredicatePG(a *argList, kinds []string) string {
	if len(kinds) == 0 {
		return ""
	}
	placeholders := make([]string, len(kinds))
	for i, k := range kinds {
		placeholders[i] = a.add(k)
	}
	return "m.kind IN (" + strings.Join(placeholders, ",") + ")"
}

// statusPredicatePG — аналог statusPredicate (sqlite.go) для $N-плейсхолдеров:
// фильтр по состоянию задачи (Query.Statuses, задача 14.1). Пустой набор — не
// «без фильтра», а исключение выполненных задач из выдачи; семантику и довод
// про NULL см. в годоке statusPredicate.
func statusPredicatePG(a *argList, statuses []string, includeDone bool) string {
	if len(statuses) == 0 {
		if includeDone {
			return ""
		}
		return "(m.status IS NULL OR m.kind <> " + a.add(string(memory.KindTask)) +
			" OR m.status <> " + a.add(string(memory.StatusDone)) + ")"
	}
	placeholders := make([]string, len(statuses))
	for i, s := range statuses {
		placeholders[i] = a.add(s)
	}
	return "m.status IN (" + strings.Join(placeholders, ",") + ")"
}

// typePredicatePG — аналог typePredicate (sqlite.go) для $N-плейсхолдеров:
// фильтр по типу задачи (Query.Types, Этап 15), m.task_type IN (...). Пустой
// набор -> без фильтра; непустой отбирает только задачи (у прочих kind
// task_type = NULL).
func typePredicatePG(a *argList, types []string) string {
	if len(types) == 0 {
		return ""
	}
	placeholders := make([]string, len(types))
	for i, t := range types {
		placeholders[i] = a.add(t)
	}
	return "m.task_type IN (" + strings.Join(placeholders, ",") + ")"
}

// tagsPredicatePG — аналог tagsPredicate (sqlite.go) для $N-плейсхолдеров: условие
// "хотя бы один из тегов" (OR-семантика, Query.Tags, задача 8.2) через EXISTS —
// запись не задваивается, если совпало несколько перечисленных тегов.
func tagsPredicatePG(a *argList, tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	placeholders := make([]string, len(tags))
	for i, t := range tags {
		placeholders[i] = a.add(t)
	}
	return "EXISTS (SELECT 1 FROM tags t WHERE t.memory_id = m.id AND t.tag IN (" + strings.Join(placeholders, ",") + "))"
}

// metaFiltersPG — аналог metaFilters (sqlite.go) для $N-плейсхолдеров: общие для
// SearchLexical и SearchVector предикаты по полям Query, кроме полнотекстового/
// векторного MATCH — единая точка, гарантирующая, что оба источника recall (DESIGN.md
// §8) отбирают кандидатов одинаково. now — текущий unix ts для фильтра TTL (задача
// 9.4, см. комментарий metaFilters в sqlite.go про двойную защиту от просроченных
// записей).
func metaFiltersPG(a *argList, q memory.Query, now int64) []string {
	conds := []string{
		"m.superseded_by IS NULL",
		"(m.expires_at IS NULL OR m.expires_at > " + a.add(now) + ")",
	}
	if clause := scopePredicatePG(a, q.Scopes, q.Scope); clause != "" {
		conds = append(conds, clause)
	}
	if clause := kindPredicatePG(a, q.Kinds); clause != "" {
		conds = append(conds, clause)
	}
	if clause := statusPredicatePG(a, q.Statuses, q.IncludeDone); clause != "" {
		conds = append(conds, clause)
	}
	if clause := typePredicatePG(a, q.Types); clause != "" {
		conds = append(conds, clause)
	}
	if q.SinceTs > 0 {
		conds = append(conds, "m.created_at >= "+a.add(q.SinceTs))
	}
	if clause := tagsPredicatePG(a, q.Tags); clause != "" {
		conds = append(conds, clause)
	}
	if q.CreatedAfter > 0 {
		conds = append(conds, "m.created_at >= "+a.add(q.CreatedAfter))
	}
	if q.CreatedBefore > 0 {
		conds = append(conds, "m.created_at <= "+a.add(q.CreatedBefore))
	}
	return conds
}

// sanitizeTSQuery строит SQL-выражение tsquery для безопасного полнотекстового поиска
// по text — аналог sanitizeFTSQuery (sqlite.go) для PostgreSQL. Разбивает text на слова
// той же токенизацией, что и SQLite-реализация (splitWords, normalize.go — общая
// граница токена для обоих бэкендов), для каждого уникального (без учёта регистра)
// слова добавляет в a параметр и оборачивает его в plainto_tsquery('russian', $N).
//
// Почему это безопасно (ключевое свойство sanitizeFTSQuery, которое нужно сохранить):
// plainto_tsquery трактует переданную строку как обычный текст, а НЕ как язык
// операторов tsquery (&, |, !, (), :*) — в отличие от to_tsquery/websearch_to_tsquery,
// она не даёт пользовательскому вводу внедрить оператор. Конфигурация 'russian'
// попутно даёт стемминг силами сервера (DESIGN.md §4) — отдельная стеммированная
// колонка (content_stemmed в SQLite, задача 8.3) здесь не нужна.
//
// Термины разных слов запроса соединяются через || (OR) — "запись, содержащая хотя бы
// одно слово запроса", та же семантика, что даёт OR в sanitizeFTSQuery. Пустой запрос
// (после токенизации не осталось слов) -> ("", false) — вызывающая сторона в этом
// случае не накладывает MATCH-условие вовсе (как useMatch=false в SearchLexical
// sqlite.go).
func sanitizeTSQuery(a *argList, text string) (expr string, ok bool) {
	words := splitWords(text)
	if len(words) == 0 {
		return "", false
	}

	seen := make(map[string]bool, len(words))
	var parts []string
	for _, w := range words {
		key := strings.ToLower(w)
		if seen[key] {
			continue
		}
		seen[key] = true
		parts = append(parts, "plainto_tsquery('russian', "+a.add(w)+")")
	}
	if len(parts) == 0 {
		return "", false
	}
	return "(" + strings.Join(parts, " || ") + ")", true
}

// vectorLiteral форматирует вектор в текстовое представление pgvector ("[v1,v2,...]"),
// пригодное для подстановки как строковый параметр с приведением типа ($N::vector) —
// pgvector принимает эту форму как вход для типа vector. FormatFloat с точностью -1
// (минимальное число знаков, воспроизводящее исходное float32 без потерь) — не более
// того, что нужно для косинусной дистанции, без искусственного обрезания точности.
func vectorLiteral(vec []float32) string {
	parts := make([]string, len(vec))
	for i, v := range vec {
		parts[i] = strconv.FormatFloat(float64(v), 'f', -1, 32)
	}
	return "[" + strings.Join(parts, ",") + "]"
}
