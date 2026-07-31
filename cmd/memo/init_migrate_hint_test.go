// Тесты подсказки `memo migrate` при переезде на postgres (задача 12.1,
// DESIGN.md §14 Этап 12): findExistingSqliteMemory/nonEmptySQLiteFile (точная
// read-only проверка через store.SQLiteHasMemoriesContext, без открытия
// "боевым" store.OpenSQLite) и печать готовой команды в doInit.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"agentmemory/internal/memory"
	"agentmemory/internal/memory/store"
)

// writeEmptySQLiteFile создаёт по пути path настоящий файл SQLite со схемой
// AgentMemory (store.OpenSQLite), но без единой записи памяти — регрессия
// прежней калибровки по размеру файла: свежая БД физически занимает
// заметное место (десятки КиБ на схему/индексы FTS5), но nonEmptySQLiteFile
// обязана считать её пустой независимо от размера файла на диске.
func writeEmptySQLiteFile(t *testing.T, path string) {
	t.Helper()
	s, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite(%q): %v", path, err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// writeNonEmptySQLiteFile — как writeEmptySQLiteFile, но с одной записью
// памяти (включая тумстоун, если tombstone=true — nonEmptySQLiteFile обязана
// считать и такую БД непустой: перенос `memo migrate` переносит тумстоуны
// тоже, см. комментарий у store.SQLiteHasMemoriesContext). Кладёт запись через
// putDoctorMemory (doctor_test.go) — общий хелпер пакета для "открыть
// временный store и положить одну запись с разумными дефолтами", вместо
// собственной копии литерала memory.Memory.
func writeNonEmptySQLiteFile(t *testing.T, path string, tombstone bool) {
	t.Helper()
	s, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite(%q): %v", path, err)
	}
	defer s.Close()

	putDoctorMemory(t, s, func(m *memory.Memory) {
		m.Content = "запись для теста подсказки migrate"
		if tombstone {
			supersededBy := ulid.Make().String()
			m.SupersededBy = &supersededBy
		}
	})
}

// defaultHomeSQLitePath возвращает штатный дефолтный путь
// ~/.agentmemory/memory.db под уже изолированным home (isolateHomeForTest) и
// создаёт родительский каталог — общая подготовка для тестов, которым нужен
// файл именно по этому пути (defaultStorePath), а не по кастомному из
// agentmemory.toml.
func defaultHomeSQLitePath(t *testing.T, home string) string {
	t.Helper()
	path := filepath.Join(home, ".agentmemory", "memory.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return path
}

func TestNonEmptySQLiteFile(t *testing.T) {
	dir := t.TempDir()

	t.Run("отсутствующий файл", func(t *testing.T) {
		found, warn := nonEmptySQLiteFile(filepath.Join(dir, "nope.db"))
		if found {
			t.Error("отсутствующий файл не должен считаться непустой БД")
		}
		if warn != "" {
			t.Errorf("отсутствующий файл — не сбой проверки, warn = %q, хочу \"\"", warn)
		}
	})

	t.Run("свежесозданная пустая БД (со схемой, без записей)", func(t *testing.T) {
		path := filepath.Join(dir, "empty.db")
		writeEmptySQLiteFile(t, path)
		found, warn := nonEmptySQLiteFile(path)
		if found {
			t.Error("БД без единой записи памяти не должна считаться непустой, независимо от размера файла со схемой")
		}
		if warn != "" {
			t.Errorf("пустая, но валидная БД — не сбой проверки, warn = %q, хочу \"\"", warn)
		}
	})

	t.Run("БД с живой записью", func(t *testing.T) {
		path := filepath.Join(dir, "live.db")
		writeNonEmptySQLiteFile(t, path, false)
		if found, _ := nonEmptySQLiteFile(path); !found {
			t.Error("БД с записью должна считаться непустой")
		}
	})

	t.Run("БД только с тумстоуном тоже непуста — migrate переносит и его", func(t *testing.T) {
		path := filepath.Join(dir, "tombstone.db")
		writeNonEmptySQLiteFile(t, path, true)
		if found, _ := nonEmptySQLiteFile(path); !found {
			t.Error("БД с тумстоуном должна считаться непустой (memo migrate переносит тумстоуны через AllForExport)")
		}
	})

	t.Run("каталог вместо файла — не паникует, репортит сбой проверки, а не молчит", func(t *testing.T) {
		path := filepath.Join(dir, "adir")
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		found, warn := nonEmptySQLiteFile(path)
		if found {
			t.Error("каталог не должен считаться файлом БД")
		}
		// В отличие от "файла нет"/"таблицы memories нет" (легитимное "данных
		// нет"), это настоящий сбой проверки (path — не файл SQLite вовсе) —
		// доработка задачи 12.1 (симметрия с диагностикой готовности postgres):
		// такой сбой должен быть виден вызывающей стороне, а не молча
		// сведён к тому же false, что и обычное отсутствие данных.
		if warn == "" {
			t.Error("проверка каталога как файла БД должна сообщить о сбое (warn пуст)")
		}
	})

	t.Run("не sqlite файл — не паникует, репортит сбой проверки, а не молчит", func(t *testing.T) {
		path := filepath.Join(dir, "garbage.db")
		if err := os.WriteFile(path, []byte("не sqlite, просто мусорные байты"), 0o644); err != nil {
			t.Fatalf("запись %s: %v", path, err)
		}
		found, warn := nonEmptySQLiteFile(path)
		if found {
			t.Error("битый/чужой файл не должен считаться непустой БД памяти")
		}
		if warn == "" {
			t.Error("проверка битого файла должна сообщить о сбое (warn пуст)")
		}
	})
}

// TestFindExistingSqliteMemory_DefaultPath — без записанного agentmemory.toml
// findExistingSqliteMemory проверяет только дефолтный путь defaultStorePath.
func TestFindExistingSqliteMemory_DefaultPath(t *testing.T) {
	home := isolateHomeForTest(t)
	dir := t.TempDir() // <dir> прогона init — отдельный, без agentmemory.toml

	defaultPath := defaultHomeSQLitePath(t, home)
	writeNonEmptySQLiteFile(t, defaultPath, false)

	got, warn := findExistingSqliteMemory(dir)
	if got != defaultPath {
		t.Errorf("findExistingSqliteMemory = %q, хочу дефолтный путь %q", got, defaultPath)
	}
	if warn != "" {
		t.Errorf("найденный кандидат — успех, warn = %q, хочу \"\"", warn)
	}
}

// TestFindExistingSqliteMemory_FromExistingConfig — <dir>/agentmemory.toml от
// предыдущего sqlite-прогона init указывает на кастомный путь: он и должен
// быть найден, а не (или раньше) дефолтный ~/.agentmemory/memory.db.
func TestFindExistingSqliteMemory_FromExistingConfig(t *testing.T) {
	isolateHomeForTest(t)
	dir := t.TempDir()

	customPath := filepath.Join(dir, "custom-memory.db")
	writeNonEmptySQLiteFile(t, customPath, false)
	writeMinimalConfig(t, dir, customPath)

	got, _ := findExistingSqliteMemory(dir)
	if got != customPath {
		t.Errorf("findExistingSqliteMemory = %q, хочу путь из существующего agentmemory.toml %q", got, customPath)
	}
}

// TestFindExistingSqliteMemory_RelativePathResolvedAgainstConfigDir —
// доработка задачи 12.1 по итогам ревью: config.Load не абсолютизирует
// store.path относительно каталога конфига (её контракт этой задачей не
// трогается), поэтому findExistingSqliteMemory обязана резолвить
// относительный путь из <dir>/agentmemory.toml сама, ОТНОСИТЕЛЬНО dir
// (resolveConfigRelativePath), а не относительно cwd процесса теста —
// иначе "./memory.db" из чужого TOML либо не находился бы вовсе, либо (что
// хуже) указывал на случайный посторонний файл с тем же относительным
// именем в текущем каталоге запуска.
func TestFindExistingSqliteMemory_RelativePathResolvedAgainstConfigDir(t *testing.T) {
	isolateHomeForTest(t)
	dir := t.TempDir()

	// Файл лежит в dir (относительный путь резолвится ОТ dir), а не от cwd
	// процесса теста — cwd в момент вызова findExistingSqliteMemory ниже
	// заведомо не совпадает с dir.
	relPath := "./nested/relative-memory.db"
	absPath := filepath.Join(dir, "nested", "relative-memory.db")
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeNonEmptySQLiteFile(t, absPath, false)
	writeMinimalConfig(t, dir, relPath)

	got, _ := findExistingSqliteMemory(dir)
	if got != absPath {
		t.Errorf("findExistingSqliteMemory = %q, хочу абсолютизированный (относительно %q) путь %q", got, dir, absPath)
	}
}

// TestFindExistingSqliteMemory_EmptyEverywhere — ни кастомного, ни дефолтного
// непустого файла нет: подсказки быть не должно.
func TestFindExistingSqliteMemory_EmptyEverywhere(t *testing.T) {
	isolateHomeForTest(t)
	dir := t.TempDir()

	if got, warn := findExistingSqliteMemory(dir); got != "" || warn != "" {
		t.Errorf("findExistingSqliteMemory = (%q, %q), хочу (\"\", \"\") (файлов с данными нет, обе проверки — легитимный \"нет\", не сбой)", got, warn)
	}
}

// TestFindExistingSqliteMemory_ReportsCheckFailure — доработка задачи 12.1
// (симметрия с диагностикой готовности postgres, см. её комментарий): если ни
// один кандидат не найден, но проверка хотя бы одного из них не завершилась
// (файл битый/недоступен, а не просто отсутствует), findExistingSqliteMemory
// обязана вернуть это как предупреждение, а не молча "" — иначе накопленная
// память могла бы остаться незамеченной без единого слова в выводе `memo init`.
func TestFindExistingSqliteMemory_ReportsCheckFailure(t *testing.T) {
	home := isolateHomeForTest(t)
	dir := t.TempDir()

	// Дефолтный кандидат — не файл SQLite вовсе: проверка реально ломается,
	// а не просто "файла нет".
	garbagePath := defaultHomeSQLitePath(t, home)
	if err := os.WriteFile(garbagePath, []byte("не sqlite"), 0o644); err != nil {
		t.Fatalf("запись %s: %v", garbagePath, err)
	}

	got, warn := findExistingSqliteMemory(dir)
	if got != "" {
		t.Errorf("findExistingSqliteMemory нашла %q на битом файле, хочу \"\"", got)
	}
	if warn == "" {
		t.Error("findExistingSqliteMemory не сообщила о сбое проверки битого кандидата")
	}
}

// TestDoInit_PostgresPrintsMigrateHintForNonEmptySqlite — сквозная проверка:
// доступная postgres-проба (стаб) + непустой дефолтный sqlite-файл дают в
// выводе doInit готовую команду `memo migrate`, парсящуюся обратно
// parseMigrateEndpoint (cmd/memo/migrate.go) без пароля в postgres-URL.
func TestDoInit_PostgresPrintsMigrateHintForNonEmptySqlite(t *testing.T) {
	home := isolateHomeForTest(t)
	dir := t.TempDir()

	sqlitePath := defaultHomeSQLitePath(t, home)
	writeNonEmptySQLiteFile(t, sqlitePath, false)

	stubStoreProbe(t, nil)

	var out strings.Builder
	err := doInit(initConfig{
		Project:          "myproj",
		Dir:              dir,
		ReasonerProvider: "noop",
		StoreDriver:      storeDriverPostgres,
		PostgresHost:     "postgres.lan",
		PostgresDBName:   "team",
		PostgresPassword: "s3cret",
	}, &out)
	if err != nil {
		t.Fatalf("doInit: %v", err)
	}

	text := out.String()
	if !strings.Contains(text, "memo migrate") {
		t.Fatalf("вывод не содержит подсказку migrate:\n%s", text)
	}
	if strings.Contains(text, "s3cret") {
		t.Errorf("пароль просочился в подсказку migrate:\n%s", text)
	}

	src, dst := extractMigrateArgs(t, text)
	if src != "sqlite:"+sqlitePath {
		t.Errorf("источник подсказки = %q, хочу %q", src, "sqlite:"+sqlitePath)
	}

	srcCfg, err := parseMigrateEndpoint(src)
	if err != nil {
		t.Fatalf("parseMigrateEndpoint(%q): %v", src, err)
	}
	if srcCfg.Driver != "sqlite" || srcCfg.Path != sqlitePath {
		t.Errorf("разобранный источник = %+v, хочу sqlite %q", srcCfg, sqlitePath)
	}

	dstCfg, err := parseMigrateEndpoint(dst)
	if err != nil {
		t.Fatalf("parseMigrateEndpoint(%q): %v", dst, err)
	}
	if dstCfg.Driver != "postgres" || dstCfg.Host != "postgres.lan" || dstCfg.DBName != "team" {
		t.Errorf("разобранный приёмник = %+v, хочу postgres postgres.lan/team", dstCfg)
	}
}

// TestDoInit_PostgresMigrateHintShellSafeForSpecialPath — доработка задачи
// 12.1 по итогам ревью: путь к найденному sqlite-файлу нарочно содержит $,
// пробел и одинарную кавычку — все три символа, которые Go-кавычки (%q)
// печатают буквально, но шелл трактует по-разному ($ — интерполяция
// переменной, пробел — разделитель аргументов, ' — требует экранирования
// внутри '...'-строки). Печатаемая doInit команда обязана оставаться одним
// корректным shell-аргументом независимо от этого — проверяется тем же
// настоящим `sh` через extractMigrateArgs (никаких других допущений об
// экранировании).
func TestDoInit_PostgresMigrateHintShellSafeForSpecialPath(t *testing.T) {
	isolateHomeForTest(t)
	dir := t.TempDir()

	special := filepath.Join(dir, "it's $HOME memory.db")
	writeNonEmptySQLiteFile(t, special, false)
	writeMinimalConfig(t, dir, special)

	stubStoreProbe(t, nil)

	var out strings.Builder
	err := doInit(initConfig{
		Project:          "myproj",
		Dir:              dir,
		ReasonerProvider: "noop",
		StoreDriver:      storeDriverPostgres,
		PostgresHost:     "postgres.lan",
	}, &out)
	if err != nil {
		t.Fatalf("doInit: %v", err)
	}

	src, dst := extractMigrateArgs(t, out.String())
	if src != "sqlite:"+special {
		t.Errorf("источник подсказки после разбора sh = %q, хочу %q (шелл, вероятно, интерполировал $HOME или разбил на несколько аргументов по пробелу)", src, "sqlite:"+special)
	}

	srcCfg, err := parseMigrateEndpoint(src)
	if err != nil {
		t.Fatalf("parseMigrateEndpoint(%q): %v", src, err)
	}
	if srcCfg.Path != special {
		t.Errorf("разобранный источник Path = %q, хочу %q", srcCfg.Path, special)
	}

	if _, err := parseMigrateEndpoint(dst); err != nil {
		t.Fatalf("parseMigrateEndpoint(%q): %v", dst, err)
	}
}

// TestDoInit_PostgresNoMigrateHintWithoutSqliteFile — без накопленной
// локальной памяти подсказки быть не должно.
func TestDoInit_PostgresNoMigrateHintWithoutSqliteFile(t *testing.T) {
	isolateHomeForTest(t)
	dir := t.TempDir()

	stubStoreProbe(t, nil)

	var out strings.Builder
	err := doInit(initConfig{
		Project:          "myproj",
		Dir:              dir,
		ReasonerProvider: "noop",
		StoreDriver:      storeDriverPostgres,
		PostgresHost:     "postgres.lan",
	}, &out)
	if err != nil {
		t.Fatalf("doInit: %v", err)
	}
	if strings.Contains(out.String(), "memo migrate") {
		t.Errorf("вывод содержит подсказку migrate без накопленной sqlite-памяти:\n%s", out.String())
	}
}

// TestDoInit_PostgresNoMigrateHintForEmptySqliteFile — свежий, только что
// созданный (со схемой, но без записей) sqlite-файл не должен провоцировать
// подсказку: регрессия прежней калибровки по размеру файла, которая на любой
// будущей миграции схемы рисковала ложно посчитать пустую БД "непустой".
func TestDoInit_PostgresNoMigrateHintForEmptySqliteFile(t *testing.T) {
	home := isolateHomeForTest(t)
	dir := t.TempDir()

	sqlitePath := defaultHomeSQLitePath(t, home)
	writeEmptySQLiteFile(t, sqlitePath)

	stubStoreProbe(t, nil)

	var out strings.Builder
	err := doInit(initConfig{
		Project:          "myproj",
		Dir:              dir,
		ReasonerProvider: "noop",
		StoreDriver:      storeDriverPostgres,
		PostgresHost:     "postgres.lan",
	}, &out)
	if err != nil {
		t.Fatalf("doInit: %v", err)
	}
	if strings.Contains(out.String(), "memo migrate") {
		t.Errorf("вывод содержит подсказку migrate для пустой (без записей) sqlite БД:\n%s", out.String())
	}
}

// TestDoInit_SQLiteNoMigrateHint — при driver=sqlite подсказка про migrate не
// имеет смысла (переезда не происходит) и не должна печататься, даже если
// дефолтный sqlite-файл существует и непуст.
func TestDoInit_SQLiteNoMigrateHint(t *testing.T) {
	home := isolateHomeForTest(t)
	dir := t.TempDir()

	sqlitePath := defaultHomeSQLitePath(t, home)
	writeNonEmptySQLiteFile(t, sqlitePath, false)

	var out strings.Builder
	err := doInit(initConfig{
		Project:          "myproj",
		Dir:              dir,
		ReasonerProvider: "noop",
		StoreDriver:      storeDriverSQLite,
	}, &out)
	if err != nil {
		t.Fatalf("doInit: %v", err)
	}
	if strings.Contains(out.String(), "memo migrate") {
		t.Errorf("вывод содержит подсказку migrate при driver=sqlite:\n%s", out.String())
	}
}

// extractMigrateArgs достаёт два аргумента напечатанной doInit строки
// "  memo migrate <src> <dst>\n" (init.go: shellQuoteSingle) — не
// самостоятельным парсером shell-кавычек (легко ошибиться и проверять не то,
// что реально исполнил бы шелл), а настоящим sh: `set -- <src> <dst>; printf
// '%s\n' "$@"` разбирает ровно ту строку, которую пользователь скопировал бы
// в терминал, и печатает оба позиционных параметра по одному на строку — этим
// же приёмом проверяется TestShellQuoteSingle_RoundTripsThroughSh.
func extractMigrateArgs(t *testing.T, out string) (src, dst string) {
	t.Helper()
	const prefix = "memo migrate "
	idx := strings.Index(out, prefix)
	if idx < 0 {
		t.Fatalf("строка %q не найдена в выводе:\n%s", prefix, out)
	}
	line := out[idx+len(prefix):]
	if nl := strings.IndexByte(line, '\n'); nl >= 0 {
		line = line[:nl]
	}

	script := "set -- " + line + `; printf '%s\n' "$@"`
	cmdOut, err := exec.Command("sh", "-c", script).Output()
	if err != nil {
		t.Fatalf("sh не смог разобрать напечатанную команду %q: %v", line, err)
	}
	parts := strings.Split(strings.TrimRight(string(cmdOut), "\n"), "\n")
	if len(parts) != 2 {
		t.Fatalf("напечатанная команда %q дала %d аргумент(ов) вместо 2: %v", line, len(parts), parts)
	}
	return parts[0], parts[1]
}

// TestShellQuoteSingle_RoundTripsThroughSh — доработка задачи 12.1 по итогам
// ревью: подсказка `memo migrate` печатается через shellQuoteSingle (init.go),
// а не через Go-кавычки (%q), которые не экранируют $, обратные кавычки и ! —
// путь вроде "/home/u/$PROJ/memory.db", скопированный из вывода в sh/bash/zsh,
// шелл подставил бы как переменную. Проверяется не вручную выписанным
// ожидаемым текстом (легко ошибиться в самой проверке так же, как в
// экранировании), а настоящим `sh`: `printf '%s' <quoted>` обязан вернуть
// байт-в-байт исходную строку.
func TestShellQuoteSingle_RoundTripsThroughSh(t *testing.T) {
	cases := []string{
		"",
		"plain",
		"/home/u/$PROJ/memory.db",
		"/path with spaces/memory.db",
		"it's a test",
		`'''`,
		"back`tick`and!bang",
		"mix: it's $HOME 'quoted' with spaces",
	}
	for _, want := range cases {
		t.Run(want, func(t *testing.T) {
			quoted := shellQuoteSingle(want)
			out, err := exec.Command("sh", "-c", "printf '%s' "+quoted).Output()
			if err != nil {
				t.Fatalf("sh -c с %q: %v", quoted, err)
			}
			if string(out) != want {
				t.Errorf("shellQuoteSingle(%q) = %q, после разбора sh вернул %q, хочу исходную строку", want, quoted, string(out))
			}
		})
	}
}
