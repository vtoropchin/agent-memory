package store

import "testing"

// TestNormalizeForFTSInflection проверяет спайковую находку задачи 8.3 (см. раздел
// «Решение» в tasks/8.3-fts-russian-morphology.md): snowball снимает словоизменительные
// окончания (падеж/число) — разные словоформы ОДНОЙ лексемы должны стемиться в одно и
// то же.
func TestNormalizeForFTSInflection(t *testing.T) {
	cases := []struct{ a, b string }{
		{"проект", "проектом"},
		{"проект", "проектами"},
		{"тест", "тестами"},
		{"тесты", "тестом"},
		{"миграция", "миграцию"},
		{"хранилище", "хранилища"},
		{"флаг", "флагов"},
	}
	for _, c := range cases {
		sa, sb := normalizeForFTS(c.a), normalizeForFTS(c.b)
		if sa != sb {
			t.Errorf("normalizeForFTS(%q)=%q, normalizeForFTS(%q)=%q, want равны (словоизменение одной лексемы)", c.a, sa, c.b, sb)
		}
	}
}

// TestNormalizeForFTSDerivationBoundary документирует ГРАНИЦУ выбранного решения
// (раздел «Решение» задачи 8.3): лёгкий стеммер снимает только словоизменение, а не
// словообразование/синонимию — «собирать»/«сборка» и «компилировать»/«сборка» остаются
// разными основами. Это ожидаемое поведение (не баг), закрывается вектором (DESIGN §9).
func TestNormalizeForFTSDerivationBoundary(t *testing.T) {
	cases := []struct{ a, b string }{
		{"собирать", "сборка"},
		{"компилировать", "сборка"},
	}
	for _, c := range cases {
		sa, sb := normalizeForFTS(c.a), normalizeForFTS(c.b)
		if sa == sb {
			t.Errorf("normalizeForFTS(%q)=%q неожиданно совпал с normalizeForFTS(%q)=%q — граница задачи 8.3 предполагает, что эта пара НЕ мостится стеммингом", c.a, sa, c.b, sb)
		}
	}
}

// TestNormalizeForFTSLatinUnchanged проверяет, что латиница/идентификаторы/цифры не
// стеммятся (регрессия: `sqlite_fts5`, `make`, `SearchLexical` должны индексироваться
// как есть).
func TestNormalizeForFTSLatinUnchanged(t *testing.T) {
	for _, word := range []string{"SearchLexical", "make", "build", "sqlite", "fts5"} {
		if got := normalizeForFTS(word); got != word {
			t.Errorf("normalizeForFTS(%q) = %q, латиница не должна меняться", word, got)
		}
	}
}

// TestNormalizeForFTSMixedText проверяет разбиение смешанного (кириллица+латиница)
// текста: каждое слово нормализуется независимо, латиница остаётся собой.
func TestNormalizeForFTSMixedText(t *testing.T) {
	got := normalizeForFTS("Сборка через sqlite_fts5 и make build")
	want := "сборк через sqlite fts5 и make build"
	if got != want {
		t.Errorf("normalizeForFTS(...) = %q, want %q", got, want)
	}
}
