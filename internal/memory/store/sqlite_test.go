// sqlite_test.go — тесты сборки DSN в OpenSQLite (задача 12.2). До неё
// OpenSQLite подставляла путь в "file:"-URI сырым, в отличие от
// SQLiteHasMemoriesContext (sqlite_readonly.go), которая уже прогоняла путь
// через escapeSQLiteURIPath — расхождение задевало рабочую БД, а не только
// read-only диагностику. Теперь оба места используют общий sqliteDSN
// (sqlite.go), поэтому здесь проверяются: (1) что для обычных путей (в т.ч.
// относительных) без "%"/"?"/"#" итоговый DSN побайтово не изменился и что
// его параметры реально доезжают до живого соединения (не только форма
// строки), (2) что OpenSQLite больше не путает файл по спецсимвольному пути с
// файлом, "обрезанным" по первому спецсимволу (тем же приёмом, что и
// TestSQLiteHasMemoriesContext_EscapesSpecialCharsInPath в
// sqlite_readonly_test.go), (3) что типичные вариации пути ("~", пробелы/
// кириллица, "#") по-прежнему открывают файл именно там, где ожидается.
package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestSqliteDSN_NoSpecialChars_Unchanged — regression: для пути без
// "%"/"?"/"#" escapeSQLiteURIPath — тождественная операция, поэтому DSN
// обязан совпасть байт в байт со старой сырой подстановкой
// (fmt.Sprintf("file:%s?...", expanded) до задачи 12.2). Проверяет только
// форму строки — сам факт, что параметры действительно доехали до живого
// соединения, проверяет TestOpenSQLite_ParamsReachConnection ниже (по итогам
// ревью 12.2: этот тест дублирует формат-строку sqliteDSN и остаётся зелёным
// даже при потере параметров внутри OpenSQLite).
func TestSqliteDSN_NoSpecialChars_Unchanged(t *testing.T) {
	got := sqliteDSN("/home/user/.agentmemory/memory.db", "_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000")
	want := "file:/home/user/.agentmemory/memory.db?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000"
	if got != want {
		t.Errorf("sqliteDSN = %q, want %q (DSN для пути без спецсимволов не должен меняться)", got, want)
	}
}

// TestOpenSQLite_ParamsReachConnection — регрессия по итогам ревью 12.2:
// TestSqliteDSN_NoSpecialChars_Unchanged проверяет только форму DSN-строки и
// остаётся зелёным, даже если параметры потерялись бы между sqliteDSN и
// реальным sql.Open внутри OpenSQLite. Здесь открывается настоящее соединение
// на обычном пути (без спецсимволов) и через PRAGMA проверяется, что
// "_foreign_keys=on"/"_journal_mode=WAL" реально применились к сессии — а не
// просто присутствуют в строке DSN.
func TestOpenSQLite_ParamsReachConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite(%q): %v", path, err)
	}
	defer s.Close()

	var journalMode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Errorf("journal_mode = %q, хочу wal (_journal_mode=WAL из DSN не применился)", journalMode)
	}

	var foreignKeys int
	if err := s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Errorf("foreign_keys = %d, хочу 1 (_foreign_keys=on из DSN не применился)", foreignKeys)
	}

	var busyTimeout int
	if err := s.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("PRAGMA busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Errorf("busy_timeout = %d, хочу 5000 (_busy_timeout=5000 из DSN не применился)", busyTimeout)
	}
}

// TestOpenSQLite_EscapesSpecialCharsInPath — тот же trap/real приём, что и
// TestSQLiteHasMemoriesContext_EscapesSpecialCharsInPath
// (sqlite_readonly_test.go): до задачи 12.2 OpenSQLite подставляла путь сырым,
// поэтому путь с "?"/"%" обрывался бы на файле-ловушке с "обрезанным" именем
// вместо настоящего файла.
func TestOpenSQLite_EscapesSpecialCharsInPath(t *testing.T) {
	dir := t.TempDir()

	// Файл-ловушка: имя совпадает с тем, во что "обрежется" настоящий путь
	// ниже при неэкранированной подстановке в file:-URI (всё после первого
	// "?" URI-парсер считает query-строкой).
	trap := filepath.Join(dir, "memory.db")
	trapDB, err := sql.Open("sqlite3", "file:"+escapeSQLiteURIPath(trap))
	if err != nil {
		t.Fatalf("sql.Open(trap): %v", err)
	}
	if _, err := trapDB.Exec(`CREATE TABLE trap_marker (id text)`); err != nil {
		trapDB.Close()
		t.Fatalf("CREATE TABLE (trap): %v", err)
	}
	if err := trapDB.Close(); err != nil {
		t.Fatalf("Close (trap): %v", err)
	}

	// Настоящий путь: "?" и "%" — оба символа ломают сырую подстановку.
	real := filepath.Join(dir, "memory.db?1%2f")

	s, err := OpenSQLite(real)
	if err != nil {
		t.Fatalf("OpenSQLite(%q): %v", real, err)
	}
	defer s.Close()

	id := putTestMemory(t, s, nil)
	got, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v (OpenSQLite открыла не тот файл)", err)
	}
	if got.Id != id {
		t.Fatalf("Get вернул Id=%q, хочу %q", got.Id, id)
	}

	if !fileExists(t, real) {
		t.Errorf("OpenSQLite(%q) не создала файл по настоящему пути", real)
	}

	// Файл-ловушка не должен получить схему memories — если бы escapeSQLiteURIPath
	// не сработала, baseSchema/Put легли бы именно в него (файл уже существовал,
	// поэтому в отличие от отсутствующего файла OpenSQLite молча открыла бы его
	// вместо ошибки).
	trapDB2, err := sql.Open("sqlite3", "file:"+escapeSQLiteURIPath(trap))
	if err != nil {
		t.Fatalf("sql.Open(trap re-check): %v", err)
	}
	defer trapDB2.Close()
	var hasMemoriesTable bool
	if err := trapDB2.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='memories')`,
	).Scan(&hasMemoriesTable); err != nil {
		t.Fatalf("проверка схемы файла-ловушки: %v", err)
	}
	if hasMemoriesTable {
		t.Errorf("OpenSQLite(%q) записала схему memories в файл-ловушку %q вместо настоящего пути", real, trap)
	}
}

// TestOpenSQLite_EdgeCasePaths — вариации пути, которые могла задеть правка
// (сведение к sqliteDSN/escapeSQLiteURIPath): путь с "~" (expandHome обязан
// отработать ДО экранирования — порядок вызовов в OpenSQLite не менялся, но
// именно эта комбинация могла разъехаться), путь с пробелами и кириллицей,
// путь с "#". Для каждого — Put/Get roundtrip плюс проверка, что файл
// физически создан по ожидаемому пути (а не "обрезан"). Относительный путь
// сюда намеренно не входит — покрывать его реальным OpenSQLite потребовало бы
// os.Chdir (меняет cwd всего процесса, мина под будущий t.Parallel(), см.
// ревью 12.2); отдельного теста сборки DSN для относительного пути не
// заведено — sqliteDSN не ветвится по относительности пути, так что
// TestSqliteDSN_NoSpecialChars_Unchanged уже покрывает эту логику.
func TestOpenSQLite_EdgeCasePaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := t.TempDir()

	cases := []struct {
		name string
		path string
	}{
		{"home_tilde", "~/memory.db"},
		{"spaces_cyrillic", filepath.Join(dir, "папка с пробелами", "память.db")},
		{"hash", filepath.Join(dir, "memory#1.db")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := OpenSQLite(tc.path)
			if err != nil {
				t.Fatalf("OpenSQLite(%q): %v", tc.path, err)
			}
			defer s.Close()

			id := putTestMemory(t, s, nil)
			got, err := s.Get(context.Background(), id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Id != id {
				t.Fatalf("Get вернул Id=%q, хочу %q", got.Id, id)
			}

			expanded, err := expandHome(tc.path)
			if err != nil {
				t.Fatalf("expandHome: %v", err)
			}
			if !fileExists(t, expanded) {
				t.Errorf("OpenSQLite(%q) не создала файл по ожидаемому пути %q", tc.path, expanded)
			}
		})
	}
}
