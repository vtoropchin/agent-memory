package store

import (
	"strings"
	"unicode"

	"github.com/kljensen/snowball/russian"
)

// Задача 8.3 (DESIGN.md §8): чистый FTS5 unicode61 не знает русской морфологии —
// запрос «тестами»/«проекта» не матчит сохранённое «тесты»/«проект» (разные падежные/
// числовые формы одного слова). normalizeForFTS поднимает лексический слой до
// "поиска по основе слова" для кириллицы, не трогая латиницу/цифры/идентификаторы
// (`sqlite_fts5`, `make build`) — они остаются точными токенами, как раньше.
//
// Спайк (см. tasks/8.3-fts-russian-morphology.md, раздел «Решение») подтвердил: snowball
// хорошо снимает ИНФЛЕКЦИОННЫЕ окончания (падеж/число/время одной и той же лексемы:
// проект/проекта/проектом, тест/тесты/тестами), но НЕ мост между разными лексемами
// (собирать/сборка, компилировать/сборка — словообразовательная, а не словоизменительная
// разница; для неё нужен вектор, уже описанный в DESIGN §9 как отдельный слой).

// splitWords разбивает текст на "слова" — максимальные последовательности
// Unicode-букв/цифр. Единая граница токенизации для normalizeForFTS (индексация,
// Put/миграция) и sanitizeFTSQuery (запрос) — иначе стемминг индекса и запроса
// расходились бы на составных полях вида «собирать/тестировать».
func splitWords(text string) []string {
	var words []string
	var token []rune
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			token = append(token, r)
			continue
		}
		if len(token) > 0 {
			words = append(words, string(token))
			token = token[:0]
		}
	}
	if len(token) > 0 {
		words = append(words, string(token))
	}
	return words
}

// isCyrillicWord сообщает, состоит ли слово целиком из кириллических букв. Смешанные
// токены (транслит, идентификаторы, числа) стеммингу не подвергаются — безопаснее
// оставить их как есть, чем случайно сломать точный матч по имени/пути.
func isCyrillicWord(word string) bool {
	for _, r := range word {
		if !unicode.Is(unicode.Cyrillic, r) {
			return false
		}
	}
	return true
}

// stemWord приводит одно слово к основе: кириллица — через snowball-стеммер (после
// приведения к нижнему регистру, как это по умолчанию делает и сам unicode61 для
// колонки content), остальное — без изменений.
func stemWord(word string) string {
	if isCyrillicWord(word) {
		return russian.Stem(strings.ToLower(word), false)
	}
	return word
}

// normalizeForFTS готовит текст для индексации в колонку content_stemmed: разбивает
// на слова (splitWords) и стеммит каждое (stemWord), склеивая через пробел. Результат
// не предназначен для отображения — только для последующей токенизации FTS5. Один и
// тот же код вызывается и при индексации (Put, миграция схемы), и при построении
// MATCH-запроса (sanitizeFTSQuery) — иначе стеммер запроса и стеммер индекса могли бы
// разойтись.
func normalizeForFTS(text string) string {
	words := splitWords(text)
	if len(words) == 0 {
		return ""
	}
	stemmed := make([]string, len(words))
	for i, w := range words {
		stemmed[i] = stemWord(w)
	}
	return strings.Join(stemmed, " ")
}
