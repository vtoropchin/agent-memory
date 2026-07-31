// sqlite_readonly.go — read-only доступ к файлу SQLite ДЛЯ ДИАГНОСТИКИ (задача
// 12.1, доработки по итогам ревью), а не для обычной работы с памятью.
// Отличие от OpenSQLite: та открывает (или СОЗДАЁТ) файл и накатывает
// baseSchema/migrate как часть открытия — законный побочный эффект обычного
// старта memo, но недопустимый для простой проверки "есть ли тут что-нибудь".
// Симметрично тому, зачем у postgres есть отдельная OpenPostgresReadOnly (см.
// её комментарий в postgres.go) — только для sqlite нет постоянного
// соединения/пула: диагностике достаточно открыть, прочитать и закрыть.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
)

// SQLiteHasMemoriesContext сообщает, есть ли в файле SQLite path хотя бы одна
// запись памяти — ВКЛЮЧАЯ тумстоуны (superseded_by IS NOT NULL), а не только
// живые записи. Это осознанный выбор понятия "накопленная память" именно для
// цели, ради которой функция существует (подсказка `memo init` про `memo
// migrate`, см. nonEmptySQLiteFile/findExistingSqliteMemory в
// cmd/memo/init.go): `memo migrate` переносит через Store.AllForExport ВСЕ
// записи целиком, включая тумстоуны (см. её комментарий и cmd/memo/migrate.go)
// — и именно этот факт, а не «сколько живых записей», честно отвечает на
// вопрос «есть ли данные, которые migrate перенесёт». Запрос —
// SELECT EXISTS(SELECT 1 FROM memories) — останавливается на первой найденной
// строке, а не сканирует таблицу целиком (в отличие от прежнего
// SELECT COUNT(*)): важно для дефолтного кандидата ~/.agentmemory/memory.db —
// общего файла на все проекты (DESIGN.md §2.1), который растёт быстрее
// остальных и путь к которому может вести на сетевой маунт.
//
// Открывает файл строго read-only и immutable (DSN-параметры mode=ro и
// immutable=1 — оба стандартные для SQLite, не выдумка драйвера, см.
// TestReadonly в mattn/go-sqlite3 и раздел "The immutable Query Parameter" в
// https://www.sqlite.org/uri.html): в отличие от OpenSQLite НЕ создаёт
// отсутствующий файл и не применяет baseSchema/migrate. Заменяет прежнюю
// эвристику "непустой файл по суммарному размеру + WAL"
// (sqliteMemoryHeaderThreshold) — та была калибровкой на текущий размер
// пустой схемы и рисковала ложно сработать на пустой БД после любой будущей
// миграции схемы (новая таблица/индекс поднимают базовый размер пустой БД).
//
// Почему immutable=1, а не голый mode=ro (регрессия по итогам ревью задачи
// 12.1, подтверждена эмпирически — см. TestSQLiteHasMemoriesContext_DoesNotLeaveWALFiles):
// mode=ro сам по себе НЕ создаёт отсутствующий главный файл (это по-прежнему
// так), но при открытии WAL-режимной базы SQLite заводит рядом файлы
// "<path>-wal"/"<path>-shm" (нужны ему для чтения консистентного снимка через
// wal-index) и оставляет их лежать после Close — вопреки и доккомментарию
// этой функции, и обещанию "memo init ничего не создаёт" (DESIGN.md §15). В
// сценарии, где `memo init`/`memo migrate` запускается от другого
// пользователя (sudo, CI-раннер, другой uid контейнера), эти осиротевшие
// файлы с чужим владельцем ломают штатный `memo`, которому они больше не
// принадлежат. immutable=1 говорит SQLite, что файл на носителе,
// предполагаемом неизменяемым: драйверу запрещено трогать wal-index и
// блокировки, поэтому файлы рядом не появляются вовсе.
//
// Плата за immutable=1: содержимое ещё не сброшенного в главный файл WAL
// (несброшенные данные лежат физически в "<path>-wal", пока их не
// checkpoint'нул кто-то из обычных, не-immutable соединений) для immutable-
// соединения невидимо — свежая БД, у которой все записи ещё лежат в WAL, даст
// false вместо реального true. Это компенсирует walLikelyHasUncommittedData
// ниже: узкая, явная эвристика "false, но рядом непустой WAL — считаем, что
// память есть". Асимметрия оправдана природой вызывающей стороны
// (nonEmptySQLiteFile/findExistingSqliteMemory, подсказка про `memo
// migrate`): ложноположительный результат здесь безвреден (просто лишняя
// строка подсказки), а ложноотрицательный убивает саму цель функции — не
// заметить накопленную память и не предложить перенос. В отличие от прежней
// версии (SQLiteMemoryCount), которая в этом случае возвращала числовой
// сентинел "count=1" (частную политику вызывающей стороны, протащенную через
// общий API), этот случай возвращает честный true — у функции булев контракт,
// и "рядом непустой WAL" укладывается в него ровно так же, как и обычный
// подсчёт.
//
// Два случая НЕ считаются ошибкой (возвращают (false, nil)), потому что оба
// они означают одно и то же для вызывающей стороны — "переносить нечего":
//   - файла нет (os.IsNotExist) — типичный случай для проекта, который никогда
//     не пользовался sqlite;
//   - файл есть, но в нём нет таблицы memories — это либо не файл памяти memo
//     вовсе, либо совсем свежий файл, чья схема ещё не накатана (OpenSQLite
//     применяет baseSchema внутри Open, здесь его не было).
//
// Прочие ошибки (файл повреждён, не является файлом SQLite, нет прав на
// чтение, отмена/таймаут ctx) возвращаются как есть — это диагностика, не
// отказ: вызывающая сторона (nonEmptySQLiteFile в cmd/memo/init.go) обязана
// трактовать такую ошибку иначе, чем "файла с данными нет" — см. её
// комментарий про симметрию с диагностикой готовности PostgreSQL.
//
// ctx ограничивает и открытие read-only соединения, и оба запроса ниже — тот
// же инвариант, что уже есть у постгрес-пробы того же прогона
// (probeStoreTimeout/OpenPostgresReadOnlyContext, cmd/memo/init.go):
// установка не должна висеть бесконечно, даже если файл лежит на подвисшем
// сетевом маунте (NFS/SMB) — локальный диск обычно быстр, но не
// гарантированно. Форму без ctx не заводим: единственный вызывающий,
// nonEmptySQLiteFile, всегда передаёт дедлайн, тесты передают
// context.Background().
func SQLiteHasMemoriesContext(ctx context.Context, path string) (bool, error) {
	expanded, err := expandHome(path)
	if err != nil {
		return false, fmt.Errorf("раскрытие пути %q: %w", path, err)
	}

	// Явная проверка существования ДО открытия: mode=ro сама по себе не
	// создаёт отсутствующий файл (в отличие от дефолтного rwc в OpenSQLite),
	// но sql.Open в database/sql ленив (соединение к БД не устанавливается,
	// пока не выполнен первый запрос) — без этой проверки отсутствие файла
	// проявилось бы только на QueryRow ниже как менее внятная ошибка ввода-
	// вывода вместо чистого "файла нет — считаем false".
	if _, err := os.Stat(expanded); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("проверка файла %q: %w", expanded, err)
	}

	dsn := fmt.Sprintf("file:%s?mode=ro&immutable=1&_busy_timeout=5000", escapeSQLiteURIPath(expanded))
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return false, fmt.Errorf("открытие sqlite %q (read-only): %w", expanded, err)
	}
	defer db.Close()

	// Таблицы memories может не быть — не через try/catch запроса ниже
	// (ошибка "no such table" отличалась бы от прочих поломок только
	// сравнением текста), а явной проверкой sqlite_master (sqliteTableExists,
	// тот же приём, что ftsTableExists/hasVecTable в sqlite.go).
	exists, err := sqliteTableExists(ctx, db, "memories")
	if err != nil {
		return false, fmt.Errorf("проверка таблицы memories в %q: %w", expanded, err)
	}
	if !exists {
		return false, nil
	}

	var hasRow bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM memories)`).Scan(&hasRow); err != nil {
		return false, fmt.Errorf("проверка записей в %q: %w", expanded, err)
	}

	// hasRow==false не обязательно значит "переносить нечего" — см.
	// доккомментарий функции про immutable=1: если рядом лежит непустой WAL,
	// там могут быть несброшенные записи, невидимые этому immutable-
	// соединению.
	if !hasRow && walLikelyHasUncommittedData(expanded) {
		return true, nil
	}

	return hasRow, nil
}

// sqliteWALHeaderSize — размер фиксированного заголовка WAL-файла SQLite в
// байтах (см. формат WAL в https://www.sqlite.org/fileformat2.html, раздел
// "Write-Ahead Log Format"). Используется как порог в
// walLikelyHasUncommittedData, чтобы не принять WAL-файл, в котором нет ни
// одного фрейма (только заголовок — такое бывает, если checkpoint усечённо
// оставил файл вместо удаления), за признак несброшенных данных.
const sqliteWALHeaderSize = 32

// walLikelyHasUncommittedData сообщает, лежит ли рядом с dbPath WAL-файл
// ("<dbPath>-wal"), в котором может быть хотя бы один ещё не сброшенный в
// главный файл фрейм. Точно посчитать фреймы значило бы распарсить формат
// WAL — избыточно для эвристики "точно есть/наверняка нет"; размер файла
// больше одного заголовка уже достаточно узкий сигнал (обычный checkpoint
// либо удаляет WAL-файл целиком, либо усекает его до нуля/заголовка — см.
// доккомментарий SQLiteHasMemoriesContext и
// TestSQLiteHasMemoriesContext_DoesNotLeaveWALFiles).
// Отсутствие файла (обычный случай — либо WAL-режим не использовался, либо
// был полностью checkpoint'нут при штатном закрытии) — не ошибка, просто
// "нет несброшенных данных".
func walLikelyHasUncommittedData(dbPath string) bool {
	fi, err := os.Stat(dbPath + "-wal")
	if err != nil {
		return false
	}
	return fi.Size() > sqliteWALHeaderSize
}

// escapeSQLiteURIPath экранирует path по правилам SQLite для превращения
// файлового пути в "file:"-URI (https://www.sqlite.org/uri.html, раздел 3.1
// "Converting A Filename Into A URI"): официальный минимальный рецепт SQLite
// — заменить "?" на "%3f" и "#" на "%23" (иначе эти символы обрываются как
// начало query-строки/фрагмента URI, а path утекает не туда, куда ожидалось,
// без единой ошибки от драйвера). Экранирование "%" добавлено сверх рецепта
// sqlite.org намеренно: без него буквальный "%" в имени файла (например,
// "%2f") SQLite прочитал бы как уже готовую percent-encoded последовательность
// и подставил бы декодированный байт в путь, снова открыв не тот файл.
// Порядок важен: "%" экранируется ПЕРВЫМ, до "?" и "#" — иначе "%", только
// что вставленный на месте "?"/"#", был бы экранирован повторно.
func escapeSQLiteURIPath(path string) string {
	path = strings.ReplaceAll(path, "%", "%25")
	path = strings.ReplaceAll(path, "?", "%3f")
	path = strings.ReplaceAll(path, "#", "%23")
	return path
}
