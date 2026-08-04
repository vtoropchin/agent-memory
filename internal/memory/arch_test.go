package memory_test

// Правило зависимостей (чистая архитектура): исходный код направлен внутрь —
// доменная модель и варианты использования не знают о деталях (драйверы БД,
// формат конфига, HTTP-клиенты). Компилятор Go уже запрещает циклы импортов
// (ADP), а internal/ ограничивает видимость наружу модуля; чего он не проверяет —
// направление зависимости между своими пакетами. Это проверяет тест.
//
// Считаются ПРЯМЫЕ внешние импорты первопартийных пакетов (agentmemory/...), до
// которых дотягивается проверяемый пакет. Транзитивные зависимости самих
// библиотек (x/text через pgx и т.п.) не считаются: они не про наш код, и от их
// обновления число нарушений скакать не должно.
//
// Известное нарушение зафиксировано базовой линией, а не ослаблением правила:
// порт store.Store объявлен в одном пакете с реализациями (sqlite.go,
// postgres.go), поэтому write/recall/maint компонуются с драйверами БД, хотя
// пользуются только интерфейсом. Лечится вынесением порта из пакета реализаций;
// до тех пор тест держит линию от роста.

import (
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// archTags — теги сборки, с которыми проект собирается и тестируется (Makefile).
// Без sqlite_fts5 граф зависимостей неполон: драйвер sqlite в него не попадёт.
const archTags = "sqlite_fts5"

// allowedExternal — белый список внешних модулей, разрешённых ядру. Именно белый:
// чёрный список устаревает молча при добавлении новой библиотеки. Корень модуля —
// первые три сегмента пути импорта.
var allowedExternal = map[string]bool{
	"github.com/oklog/ulid": true, // генерация идентификаторов, без ввода-вывода
}

// baseline — текущее число нарушений на пакет. Тест падает и при росте (появилась
// новая зависимость ядра от детали), и при снижении (нарушение убрано — линию
// нужно опустить, иначе она перестанет что-либо защищать).
var baseline = map[string]int{
	"agentmemory/internal/memory": 0, // сущности: только стандартная библиотека
	// sqlite-vec, pgx, snowball, go-sqlite3 — через store; toml — через config,
	// причём recall импортирует config напрямую (config.RecallConfig в подписи New).
	"agentmemory/internal/memory/write":  5,
	"agentmemory/internal/memory/recall": 5,
	"agentmemory/internal/memory/maint":  5,
}

func TestDependencyRule(t *testing.T) {
	for pkg, want := range baseline {
		t.Run(pkg, func(t *testing.T) {
			got := forbiddenDeps(t, pkg)
			switch {
			case len(got) > want:
				t.Errorf("ядро зависит от деталей: %d нарушений при базовой линии %d\n%s",
					len(got), want, strings.Join(got, "\n"))
			case len(got) < want:
				t.Errorf("нарушений стало меньше (%d при базовой линии %d) — опустите baseline[%q] до %d\n%s",
					len(got), want, pkg, len(got), strings.Join(got, "\n"))
			}
		})
	}
}

// forbiddenDeps возвращает описания нарушений вида "<свой пакет> импортирует
// <внешний модуль>" для всех первопартийных пакетов, достижимых из pkg.
func forbiddenDeps(t *testing.T, pkg string) []string {
	t.Helper()

	own := []string{}
	for _, dep := range goList(t, "-deps", pkg) {
		if strings.HasPrefix(dep, "agentmemory/") {
			own = append(own, dep)
		}
	}

	args := append([]string{"-f", "{{.ImportPath}}|{{join .Imports \" \"}}"}, own...)
	seen := map[string]bool{}
	var out []string
	for _, line := range goList(t, args...) {
		importer, imports, _ := strings.Cut(line, "|")
		for _, imp := range strings.Fields(imports) {
			root := moduleRoot(imp)
			if root == "" || allowedExternal[root] {
				continue
			}
			msg := importer + " импортирует " + root
			if !seen[msg] {
				seen[msg] = true
				out = append(out, msg)
			}
		}
	}
	sort.Strings(out)
	return out
}

// moduleRoot сводит путь импорта к корню внешнего модуля (первые три сегмента) и
// возвращает пустую строку для стандартной библиотеки и собственных пакетов.
// Признак стандартной библиотеки — точка в первом сегменте пути: у "os/exec" её
// нет, у "github.com/..." есть.
func moduleRoot(imp string) string {
	if strings.HasPrefix(imp, "agentmemory/") {
		return ""
	}
	parts := strings.Split(imp, "/")
	if !strings.Contains(parts[0], ".") {
		return ""
	}
	if len(parts) > 3 {
		parts = parts[:3]
	}
	return strings.Join(parts, "/")
}

func goList(t *testing.T, args ...string) []string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list", "-tags", archTags}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}
