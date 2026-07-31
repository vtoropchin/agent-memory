package store

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/oklog/ulid/v2"
)

// TestConcurrentInit_SameSchema — регрессия на гонку, устранённую advisory-локом
// в pgStore.init (schemaInitLockKey, postgres.go): несколько НЕЗАВИСИМЫХ соединений
// (каждое — свой *sql.DB, т.е. свой пул, как у разных процессов memo, а не разные
// коннекшны одного пула) одновременно вызывают init на ОДНОЙ И ТОЙ ЖЕ, ещё не
// существующей схеме. До фикса это стабильно падало (ошибки вида "повторяющееся
// значение ключа нарушает ограничение уникальности pg_type_typname_nsp_index" при
// первом создании таблиц и "тriggers... уже существует"/"tuple concurrently updated"
// при переприменении DROP+CREATE TRIGGER, см. комментарий у schemaInitLockKey) —
// сценарий из прода: MCP-сервер уже держит Store открытым, параллельно кто-то ещё
// запускает `memo save`/`doctor`/`migrate` на той же БД.
//
// Не использует newTestPgStoreSchema/openPostgres напрямую: cfg.DSN() (config.
// StoreConfig) не умеет задавать search_path, а тест намеренно создаёт СВОЮ схему —
// поэтому DSN собирается вручную поверх MEMO_TEST_POSTGRES_DSN, тем же приёмом, что и
// newTestPgStoreSchema (см. её комментарий про изоляцию).
func TestConcurrentInit_SameSchema(t *testing.T) {
	baseDSN := skipIfNoPgTestDSN(t)
	ctx := context.Background()

	schema := "memo_test_concurrent_" + strings.ToLower(ulid.Make().String())
	bootstrap, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatalf("bootstrap-соединение: %v", err)
	}
	// Регистрируется ДО cleanup'а с DROP SCHEMA (LIFO: последний зарегистрированный
	// cleanup выполняется первым) — иначе bootstrap.Close() отработал бы раньше, и
	// DROP SCHEMA ниже упал бы на "database is closed".
	t.Cleanup(func() { bootstrap.Close() })
	if _, err := bootstrap.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("создание тестовой схемы %s: %v", schema, err)
	}
	t.Cleanup(func() {
		if _, err := bootstrap.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Errorf("удаление тестовой схемы %s (общая база memo не должна копить мусор): %v", schema, err)
		}
	})

	u, err := url.Parse(baseDSN)
	if err != nil {
		t.Fatalf("разбор %s: %v", pgTestDSNEnv, err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	dsn := u.String()

	const concurrency = 8
	var wg sync.WaitGroup
	errs := make([]error, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Своё *sql.DB (не общий пул) на каждую горутину — намеренно
			// имитирует конкурентные Open из РАЗНЫХ процессов, а не просто разные
			// коннекшны одного *sql.DB.
			db, err := sql.Open("pgx", dsn)
			if err != nil {
				errs[i] = err
				return
			}
			defer db.Close()
			s := &pgStore{db: db, now: defaultNow}
			errs[i] = s.init(ctx)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("горутина %d: init: %v", i, err)
		}
	}

	// Повторный init на уже инициализированной схеме (второй раз, теперь без
	// конкуренции) — идемпотентность не должна была пострадать от advisory-лока.
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("открытие соединения для повторного init: %v", err)
	}
	defer db.Close()
	s := &pgStore{db: db, now: defaultNow}
	if err := s.init(ctx); err != nil {
		t.Fatalf("повторный init на уже инициализированной схеме: %v", err)
	}
}

// skipIfNoPgTestDSN возвращает MEMO_TEST_POSTGRES_DSN или пропускает тест, если она
// не задана — тот же гейт, что у newTestPgStoreSchema, вынесенный отдельно для
// тестов, которым (в отличие от неё) не нужен готовый *pgStore на новой схеме, а
// нужен сам DSN (тест конкурентности сам управляет несколькими соединениями).
func skipIfNoPgTestDSN(t *testing.T) string {
	t.Helper()
	baseDSN := os.Getenv(pgTestDSNEnv)
	if baseDSN == "" {
		t.Skipf("%s не задан — пропуск postgres-тестов store. Пример:\n  %s=postgres://memo:memo@postgres.lan:5432/memo?sslmode=disable make test", pgTestDSNEnv, pgTestDSNEnv)
	}
	return baseDSN
}
