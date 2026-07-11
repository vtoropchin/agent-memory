package write

import (
	"hash/fnv"
	"math/bits"
	"strings"
	"unicode"
)

// simhash64 считает 64-битный SimHash по словам text (см. DESIGN.md §7.2): каждое
// слово хешируется независимо (FNV-64a), затем по каждому из 64 битов накапливается
// голос "за"/"против" по всем словам, а итоговый бит фингерпринта — знак накопленной
// суммы. Похожие по составу слов тексты получают близкие по Хэммингу отпечатки, что
// позволяет ловить near-duplicate записи без внешних библиотек.
func simhash64(text string) uint64 {
	words := tokenize(text)
	var weights [64]int
	for _, word := range words {
		h := fnv.New64a()
		_, _ = h.Write([]byte(word))
		hv := h.Sum64()
		for i := 0; i < 64; i++ {
			if hv&(1<<uint(i)) != 0 {
				weights[i]++
			} else {
				weights[i]--
			}
		}
	}
	var out uint64
	for i := 0; i < 64; i++ {
		if weights[i] > 0 {
			out |= 1 << uint(i)
		}
	}
	return out
}

// hammingDistance возвращает число различающихся битов между двумя отпечатками.
func hammingDistance(a, b uint64) int {
	return bits.OnesCount64(a ^ b)
}

// tokenize разбивает text на слова в нижнем регистре, обрезая пунктуацию по краям
// каждого слова (точки, скобки, тире и т.п.), чтобы «поиска» и «поиска.» давали один
// и тот же токен — иначе SimHash считал бы пунктуацию значимым отличием.
func tokenize(text string) []string {
	fields := strings.Fields(strings.ToLower(text))
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimFunc(f, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsNumber(r)
		})
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// normalizeContent приводит content к нижнему регистру и схлопывает пробелы —
// быстрая проверка точного совпадения (без учёта регистра/пробелов) до вычисления
// SimHash. Не обрезает пунктуацию внутри слов: это лишь оптимизация для явных
// дублей, а не единственная линия защиты — пунктуационные различия всё равно
// ловятся SimHash-расстоянием через tokenize.
func normalizeContent(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}
