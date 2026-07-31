// sqlite_readonly_test.go — тесты SQLiteHasMemoriesContext (задача 12.1,
// доработки по итогам ревью/симплификации): регрессия на главное обещание
// функции — read-only диагностика не должна ничего создавать рядом с
// проверяемым файлом — плюс корректность URI-экранирования пути и эвристики
// про несброшенный WAL.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"agentmemory/internal/memory"
)

func fileExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	t.Fatalf("os.Stat(%q): %v", path, err)
	return false
}

// TestSQLiteHasMemoriesContext_DoesNotLeaveWALFiles — регрессия по итогам
// ревью задачи 12.1 (пункт A): при простом mode=ro (без immutable=1) SQLite
// при открытии WAL-режимной базы заводит рядом "<path>-wal"/"<path>-shm" и
// оставляет их после Close, что противоречит и доккомментарию
// SQLiteHasMemoriesContext, и обещанию "memo init ничего не создаёт"
// (DESIGN.md §15). immutable=1 обязан это предотвратить.
func TestSQLiteHasMemoriesContext_DoesNotLeaveWALFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	m := memory.Memory{
		Id: newID(t), Kind: memory.KindSemantic, Scope: "project:x",
		Content: "запись", Importance: 3, CreatedAt: 1, UpdatedAt: 1,
	}
	if err := s.Put(context.Background(), m); err != nil {
		s.Close()
		t.Fatalf("Put: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// После штатного Close чисто закрытая WAL-БД не должна иметь рядом
	// -wal/-shm файлов (checkpoint при закрытии их убирает) — иначе
	// последующая проверка ниже не отличила бы файлы, оставленные ЭТИМ
	// закрытием, от файлов, которые могла бы оставить сама
	// SQLiteHasMemoriesContext.
	if fileExists(t, path+"-wal") || fileExists(t, path+"-shm") {
		t.Fatalf("после штатного Close(%q) не ожидались -wal/-shm файлы — тест сам по себе не даёт чистого стенда", path)
	}

	has, err := SQLiteHasMemoriesContext(context.Background(), path)
	if err != nil {
		t.Fatalf("SQLiteHasMemoriesContext: %v", err)
	}
	if !has {
		t.Error("has = false, хочу true (одна запись положена выше)")
	}

	if fileExists(t, path+"-wal") {
		t.Error("SQLiteHasMemoriesContext оставила рядом с файлом -wal — read-only диагностика не должна ничего создавать (см. immutable=1 в доккомментарии)")
	}
	if fileExists(t, path+"-shm") {
		t.Error("SQLiteHasMemoriesContext оставила рядом с файлом -shm — read-only диагностика не должна ничего создавать (см. immutable=1 в доккомментарии)")
	}
}

// TestSQLiteHasMemoriesContext_FileNotExist — отсутствующий файл не ошибка,
// просто false (и SQLiteHasMemoriesContext не создаёт его — иначе следующий
// os.Stat в этом тесте нашёл бы файл).
func TestSQLiteHasMemoriesContext_FileNotExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.db")
	has, err := SQLiteHasMemoriesContext(context.Background(), path)
	if err != nil {
		t.Fatalf("SQLiteHasMemoriesContext: %v", err)
	}
	if has {
		t.Error("has = true для отсутствующего файла, хочу false")
	}
	if fileExists(t, path) {
		t.Error("SQLiteHasMemoriesContext создала отсутствующий файл")
	}
}

// TestSQLiteHasMemoriesContext_NoMemoriesTable — файл существует и валиден
// как SQLite, но таблицы memories в нём нет (например, совсем свежий файл
// без применённой схемы) — тоже false, не ошибка.
func TestSQLiteHasMemoriesContext_NoMemoriesTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.db")
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE other (id text)`); err != nil {
		db.Close()
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	has, err := SQLiteHasMemoriesContext(context.Background(), path)
	if err != nil {
		t.Fatalf("SQLiteHasMemoriesContext: %v", err)
	}
	if has {
		t.Error("has = true без таблицы memories, хочу false")
	}
}

// TestSQLiteHasMemoriesContext_EscapesSpecialCharsInPath — регрессия по
// итогам ревью задачи 12.1 (пункт B): путь, подставленный в "file:"-URI
// сырым, обрывается на первом "?"/"#", а "%HH" декодируется — файл
// "/x/memory.db?1" открыл бы "/x/memory.db" и нашёл бы вовсе не тот файл.
// Файл-ловушка нарочно с ПУСТОЙ таблицей memories (а не с несколькими
// записями, как раньше, когда функция считала точное число) — так поломка
// экранирования однозначно видна как false вместо ожидаемого true, а не как
// "число не совпало".
func TestSQLiteHasMemoriesContext_EscapesSpecialCharsInPath(t *testing.T) {
	dir := t.TempDir()

	// Файл-ловушка: если экранирование сломано и путь обрывается на "?",
	// SQLiteHasMemoriesContext по ошибке откроет ЭТОТ файл вместо настоящего.
	trap := filepath.Join(dir, "memory.db")
	trapDB, err := sql.Open("sqlite3", "file:"+trap)
	if err != nil {
		t.Fatalf("sql.Open(trap): %v", err)
	}
	if _, err := trapDB.Exec(`CREATE TABLE memories (id text)`); err != nil {
		trapDB.Close()
		t.Fatalf("CREATE TABLE (trap): %v", err)
	}
	if err := trapDB.Close(); err != nil {
		t.Fatalf("Close (trap): %v", err)
	}

	// Настоящий файл: имя содержит "?" и "%" — оба символа, ломающие
	// неэкранированную подстановку в file:-URI. Создаётся напрямую через
	// database/sql с тем же escapeSQLiteURIPath, который проверяем (а не
	// через OpenSQLite — та же сырая подстановка пути есть и в OpenSQLite,
	// см. отчёт по задаче: воспроизводится этим же trap-приёмом, но чинить
	// её в рамках этой задачи не входит в объём работы).
	real := filepath.Join(dir, "memory.db?1%2f")
	realDB, err := sql.Open("sqlite3", "file:"+escapeSQLiteURIPath(real))
	if err != nil {
		t.Fatalf("sql.Open(real): %v", err)
	}
	if _, err := realDB.Exec(`CREATE TABLE memories (id text)`); err != nil {
		realDB.Close()
		t.Fatalf("CREATE TABLE (real): %v", err)
	}
	if _, err := realDB.Exec(`INSERT INTO memories (id) VALUES (?)`, fmt.Sprintf("real-%d", 0)); err != nil {
		realDB.Close()
		t.Fatalf("INSERT (real): %v", err)
	}
	if err := realDB.Close(); err != nil {
		t.Fatalf("Close (real): %v", err)
	}

	has, err := SQLiteHasMemoriesContext(context.Background(), real)
	if err != nil {
		t.Fatalf("SQLiteHasMemoriesContext(%q): %v", real, err)
	}
	if !has {
		t.Errorf("has = false для %q, хочу true (запись настоящего файла, а не пустая ловушка %q)", real, trap)
	}
}

// TestWalLikelyHasUncommittedData_Heuristic — узкая эвристика (пункт A):
// has=false, но рядом лежит непустой WAL — воспроизводится держащим открытую
// читающую транзакцию соединением, которая блокирует checkpoint при закрытии
// писателя (тем самым WAL не сбрасывается в главный файл).
func TestWalLikelyHasUncommittedData_Heuristic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")

	writer, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?_journal_mode=WAL", path))
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	if _, err := writer.Exec(`CREATE TABLE memories (id text)`); err != nil {
		writer.Close()
		t.Fatalf("CREATE TABLE: %v", err)
	}
	writer.Close()

	writer, err = sql.Open("sqlite3", fmt.Sprintf("file:%s?_journal_mode=WAL", path))
	if err != nil {
		t.Fatalf("reopen writer: %v", err)
	}
	defer writer.Close()

	reader, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?_journal_mode=WAL", path))
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	defer reader.Close()

	readerTx, err := reader.Begin()
	if err != nil {
		t.Fatalf("reader.Begin: %v", err)
	}
	defer readerTx.Rollback()
	var dummy int
	if err := readerTx.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&dummy); err != nil {
		t.Fatalf("readerTx snapshot query: %v", err)
	}

	// Пока readerTx держит старый снимок открытым, писатель не может
	// checkpoint'нуть WAL при закрытии — вставленные строки остаются только
	// в "<path>-wal".
	if _, err := writer.Exec(`INSERT INTO memories (id) VALUES ('a'), ('b')`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("writer.Close: %v", err)
	}

	if !fileExists(t, path+"-wal") {
		t.Fatalf("тест не воспроизвёл несброшенный WAL — %q не существует, дальнейшая проверка бессмысленна", path+"-wal")
	}

	has, err := SQLiteHasMemoriesContext(context.Background(), path)
	if err != nil {
		t.Fatalf("SQLiteHasMemoriesContext: %v", err)
	}
	if !has {
		t.Error("has = false при непустом несброшенном WAL — эвристика walLikelyHasUncommittedData не сработала, подсказка про memo migrate потерялась бы")
	}
}
