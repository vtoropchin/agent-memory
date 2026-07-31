package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"agentmemory/internal/config"
	"agentmemory/internal/memory"
	"agentmemory/internal/memory/store"
)

// --- parseMigrateEndpoint -----------------------------------------------

func TestParseMigrateEndpoint_SQLite(t *testing.T) {
	cfg, err := parseMigrateEndpoint("sqlite:/tmp/foo/memory.db")
	if err != nil {
		t.Fatalf("parseMigrateEndpoint: %v", err)
	}
	if cfg.Driver != "sqlite" || cfg.Path != "/tmp/foo/memory.db" {
		t.Errorf("cfg = %+v, want driver=sqlite path=/tmp/foo/memory.db", cfg)
	}
}

func TestParseMigrateEndpoint_SQLiteEmptyPath(t *testing.T) {
	if _, err := parseMigrateEndpoint("sqlite:"); err == nil {
		t.Fatal("ожидалась ошибка на пустом пути sqlite:")
	}
}

func TestParseMigrateEndpoint_Postgres(t *testing.T) {
	cfg, err := parseMigrateEndpoint("postgres://memo@postgres.lan:5432/memo?sslmode=disable")
	if err != nil {
		t.Fatalf("parseMigrateEndpoint: %v", err)
	}
	want := config.StoreConfig{Driver: "postgres", Host: "postgres.lan", Port: 5432, User: "memo", DBName: "memo", SSLMode: "disable"}
	if cfg != want {
		t.Errorf("cfg = %+v, want %+v", cfg, want)
	}
}

// TestParseMigrateEndpoint_PostgresDefaults проверяет дефолты порта (5432) и
// sslmode ("disable"), когда URL их не задаёт — те же дефолты, что у
// config.Default().Store.
func TestParseMigrateEndpoint_PostgresDefaults(t *testing.T) {
	cfg, err := parseMigrateEndpoint("postgres://memo@postgres.lan/memo")
	if err != nil {
		t.Fatalf("parseMigrateEndpoint: %v", err)
	}
	if cfg.Port != 5432 {
		t.Errorf("Port = %d, want 5432", cfg.Port)
	}
	if cfg.SSLMode != "disable" {
		t.Errorf("SSLMode = %q, want disable", cfg.SSLMode)
	}
}

// TestParseMigrateEndpoint_PostgresPasswordRejected проверяет, что пароль в
// самом URL — ошибка (секреты только из env MEMO_POSTGRES_PASSWORD, DESIGN.md §13).
func TestParseMigrateEndpoint_PostgresPasswordRejected(t *testing.T) {
	_, err := parseMigrateEndpoint("postgres://memo:secret@postgres.lan:5432/memo")
	if err == nil {
		t.Fatal("ожидалась ошибка: пароль в URL не должен приниматься")
	}
	if !strings.Contains(err.Error(), memoPostgresPasswordEnv) {
		t.Errorf("ошибка %q не упоминает %s", err, memoPostgresPasswordEnv)
	}
}

func TestParseMigrateEndpoint_PostgresMissingFields(t *testing.T) {
	if _, err := parseMigrateEndpoint("postgres:///memo"); err == nil {
		t.Fatal("ожидалась ошибка: пустой host")
	}
}

func TestParseMigrateEndpoint_UnknownFormat(t *testing.T) {
	if _, err := parseMigrateEndpoint("mysql://foo/bar"); err == nil {
		t.Fatal("ожидалась ошибка на неизвестном формате")
	}
}

// TestFormatMigrateEndpoint_RoundTripsThroughParse — formatMigrateEndpoint
// (обратная к parseMigrateEndpoint, используется подсказкой `memo init` про
// `memo migrate`, см. doInit в init.go) обязана отдавать строку, которую
// parseMigrateEndpoint разбирает обратно в эквивалентный config.StoreConfig —
// это и есть страховка от расхождения между парсером и его обратной функцией.
func TestFormatMigrateEndpoint_RoundTripsThroughParse(t *testing.T) {
	cases := []config.StoreConfig{
		{Driver: "sqlite", Path: "/home/u/memory.db"},
		{Driver: "postgres", Host: "postgres.lan", Port: 5432, User: "memo", DBName: "memo", SSLMode: "disable"},
		// Пароль в cfg — formatMigrateEndpoint обязана его отбросить: строка
		// не должна содержать секрет, а parseMigrateEndpoint и вовсе отвергает
		// пароль в URL как ошибку (см. TestParseMigrateEndpoint_PostgresPasswordRejected).
		{Driver: "postgres", Host: "postgres.lan", Port: 6432, User: "alice", DBName: "team", SSLMode: "require", Password: "s3cret"},
	}
	for _, cfg := range cases {
		endpoint := formatMigrateEndpoint(cfg)
		if strings.Contains(endpoint, "s3cret") {
			t.Errorf("formatMigrateEndpoint(%+v) = %q содержит пароль", cfg, endpoint)
		}
		got, err := parseMigrateEndpoint(endpoint)
		if err != nil {
			t.Fatalf("parseMigrateEndpoint(%q): %v", endpoint, err)
		}
		want := cfg
		want.Password = ""
		if got != want {
			t.Errorf("round-trip formatMigrateEndpoint -> parseMigrateEndpoint = %+v, хочу %+v", got, want)
		}
	}
}

// --- runMigrate: разбор флагов/аргументов -------------------------------

func TestRunMigrate_WrongArgCount(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"sqlite:/tmp/a.db"},
		{"sqlite:/tmp/a.db", "sqlite:/tmp/b.db", "extra"},
	} {
		var out bytes.Buffer
		if err := runMigrate(args, &out); err == nil {
			t.Errorf("runMigrate(%v) без ошибки, ожидалась ошибка про число аргументов", args)
		}
	}
}

func TestRunMigrate_SameSourceAndDest(t *testing.T) {
	dbPath, st := newExportTestStore(t)
	st.Close()

	var out bytes.Buffer
	err := runMigrate([]string{"sqlite:" + dbPath, "sqlite:" + dbPath}, &out)
	if err == nil {
		t.Fatal("ожидалась ошибка: источник и приёмник совпадают")
	}
}

// --- runMigrate: dry-run --------------------------------------------------

// TestRunMigrate_DryRun_DoesNotTouchDestination проверяет, что dry-run не
// открывает приёмник вовсе: несуществующий sqlite-файл приёмника не должен
// быть создан.
func TestRunMigrate_DryRun_DoesNotTouchDestination(t *testing.T) {
	srcPath, src := newExportTestStore(t)
	ctx := context.Background()
	if err := src.Put(ctx, memory.Memory{Id: ulid.Make().String(), Kind: memory.KindSemantic, Scope: "project:x", Content: "c", Importance: 3, CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	src.Close()

	dstPath := filepath.Join(t.TempDir(), "dest.db")

	var out bytes.Buffer
	if err := runMigrate([]string{"-dry-run", "sqlite:" + srcPath, "sqlite:" + dstPath}, &out); err != nil {
		t.Fatalf("runMigrate: %v", err)
	}
	if !strings.Contains(out.String(), "1 записей") {
		t.Errorf("вывод dry-run не содержит сводку по источнику: %s", out.String())
	}
	if _, err := os.Stat(dstPath); err == nil {
		t.Error("dry-run не должен создавать файл приёмника")
	}
}

// --- runMigrate: полный перенос sqlite -> sqlite --------------------------

// seedMigrateFixture наполняет store живой записью, её тумстоуном (с
// цепочкой supersedes), тегами и связью — минимальный набор, покрывающий все
// поля, которые должен пронести перенос (DESIGN.md §12: AllForExport).
func seedMigrateFixture(t *testing.T, s store.Store, scope string) (oldID, newID string) {
	t.Helper()
	ctx := context.Background()
	oldID = ulid.Make().String()
	newID = ulid.Make().String()

	if err := s.Put(ctx, memory.Memory{
		Id: newID, Kind: memory.KindSemantic, Scope: scope,
		Content: "новая версия факта", Importance: 4, CreatedAt: 2000, UpdatedAt: 2000,
		Tags: []string{"a", "b"},
	}); err != nil {
		t.Fatalf("Put newID: %v", err)
	}
	if err := s.Put(ctx, memory.Memory{
		Id: oldID, Kind: memory.KindSemantic, Scope: scope,
		Content: "старая версия факта", Importance: 3, CreatedAt: 1000, UpdatedAt: 1500,
		SupersededBy: strPtr(newID),
		Links:        []memory.Link{{Dst: newID, Rel: memory.RelSupersedes}},
	}); err != nil {
		t.Fatalf("Put oldID: %v", err)
	}
	return oldID, newID
}

// TestRunMigrate_SQLiteToSQLite прогоняет живую запись + тумстоун + теги +
// связь через реальный перенос sqlite->sqlite (без внешнего сервера) и
// проверяет содержимое приёмника напрямую через Store.Get.
func TestRunMigrate_SQLiteToSQLite(t *testing.T) {
	srcPath, src := newExportTestStore(t)
	oldID, newID := seedMigrateFixture(t, src, "project:agentmemory")
	src.Close()

	dstPath := filepath.Join(t.TempDir(), "dest.db")

	var out bytes.Buffer
	if err := runMigrate([]string{"sqlite:" + srcPath, "sqlite:" + dstPath}, &out); err != nil {
		t.Fatalf("runMigrate: %v\nвывод:\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "перенесено 2, пропущено (уже есть в приёмнике) 0") {
		t.Errorf("вывод не содержит ожидаемую сводку переноса: %s", out.String())
	}
	if !strings.Contains(out.String(), "все записи источника присутствуют в приёмнике") {
		t.Errorf("вывод не содержит успешную сверку: %s", out.String())
	}
	if !strings.Contains(out.String(), "векторы не переносились") {
		t.Errorf("вывод не предупреждает про векторы: %s", out.String())
	}

	dst, err := store.OpenSQLite(dstPath)
	if err != nil {
		t.Fatalf("open dest: %v", err)
	}
	defer dst.Close()
	ctx := context.Background()

	newM, err := dst.Get(ctx, newID)
	if err != nil {
		t.Fatalf("Get newID в приёмнике: %v", err)
	}
	if len(newM.Tags) != 2 || newM.Tags[0] != "a" || newM.Tags[1] != "b" {
		t.Errorf("newM.Tags = %v, want [a b]", newM.Tags)
	}

	oldM, err := dst.Get(ctx, oldID)
	if err != nil {
		t.Fatalf("Get oldID в приёмнике: %v", err)
	}
	if oldM.SupersededBy == nil || *oldM.SupersededBy != newID {
		t.Errorf("oldM.SupersededBy = %v, want %s", oldM.SupersededBy, newID)
	}
	if len(oldM.Links) != 1 || oldM.Links[0].Dst != newID || oldM.Links[0].Rel != memory.RelSupersedes {
		t.Errorf("oldM.Links = %+v, want одно ребро -> %s (rel=%s)", oldM.Links, newID, memory.RelSupersedes)
	}
}

// TestRunMigrate_Idempotent проверяет повторный запуск: все id уже есть в
// приёмнике -> Skipped == общее число записей, конфликтов/потерь нет (Put —
// upsert, но migrate по умолчанию не перезаписывает существующее).
func TestRunMigrate_Idempotent(t *testing.T) {
	srcPath, src := newExportTestStore(t)
	seedMigrateFixture(t, src, "project:agentmemory")
	src.Close()

	dstPath := filepath.Join(t.TempDir(), "dest.db")

	var out1 bytes.Buffer
	if err := runMigrate([]string{"sqlite:" + srcPath, "sqlite:" + dstPath}, &out1); err != nil {
		t.Fatalf("первый runMigrate: %v", err)
	}

	var out2 bytes.Buffer
	if err := runMigrate([]string{"sqlite:" + srcPath, "sqlite:" + dstPath}, &out2); err != nil {
		t.Fatalf("второй runMigrate: %v\nвывод:\n%s", err, out2.String())
	}
	if !strings.Contains(out2.String(), "перенесено 0, пропущено (уже есть в приёмнике) 2") {
		t.Errorf("повторный запуск должен пропустить все 2 записи: %s", out2.String())
	}
}

// TestRunMigrate_Overwrite проверяет -overwrite: изменённое содержимое
// источника перезаписывает существующую запись приёмника.
func TestRunMigrate_Overwrite(t *testing.T) {
	srcPath, src := newExportTestStore(t)
	ctx := context.Background()
	id := ulid.Make().String()
	if err := src.Put(ctx, memory.Memory{Id: id, Kind: memory.KindSemantic, Scope: "project:x", Content: "v1", Importance: 3, CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatalf("Put v1: %v", err)
	}
	src.Close()

	dstPath := filepath.Join(t.TempDir(), "dest.db")
	var out1 bytes.Buffer
	if err := runMigrate([]string{"sqlite:" + srcPath, "sqlite:" + dstPath}, &out1); err != nil {
		t.Fatalf("первый runMigrate: %v", err)
	}

	// Меняем содержимое записи в источнике (тот же id, Put — upsert).
	src2, err := store.OpenSQLite(srcPath)
	if err != nil {
		t.Fatalf("reopen src: %v", err)
	}
	if err := src2.Put(ctx, memory.Memory{Id: id, Kind: memory.KindSemantic, Scope: "project:x", Content: "v2", Importance: 3, CreatedAt: 1, UpdatedAt: 2}); err != nil {
		t.Fatalf("Put v2: %v", err)
	}
	src2.Close()

	var out2 bytes.Buffer
	if err := runMigrate([]string{"-overwrite", "sqlite:" + srcPath, "sqlite:" + dstPath}, &out2); err != nil {
		t.Fatalf("второй runMigrate -overwrite: %v", err)
	}
	if !strings.Contains(out2.String(), "перенесено 1, пропущено (уже есть в приёмнике) 0") {
		t.Errorf("-overwrite должен перезаписать конфликтующую запись: %s", out2.String())
	}

	dst, err := store.OpenSQLite(dstPath)
	if err != nil {
		t.Fatalf("open dest: %v", err)
	}
	defer dst.Close()
	m, err := dst.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if m.Content != "v2" {
		t.Errorf("Content = %q, want v2 (после -overwrite)", m.Content)
	}
}

// TestRunMigrate_ScopesFilter проверяет частичный перенос: -scopes
// ограничивает перенос указанным пространством.
func TestRunMigrate_ScopesFilter(t *testing.T) {
	srcPath, src := newExportTestStore(t)
	ctx := context.Background()
	idAlpha := ulid.Make().String()
	idBeta := ulid.Make().String()
	if err := src.Put(ctx, memory.Memory{Id: idAlpha, Kind: memory.KindSemantic, Scope: "project:alpha", Content: "alpha", Importance: 3, CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatalf("Put alpha: %v", err)
	}
	if err := src.Put(ctx, memory.Memory{Id: idBeta, Kind: memory.KindSemantic, Scope: "project:beta", Content: "beta", Importance: 3, CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatalf("Put beta: %v", err)
	}
	src.Close()

	dstPath := filepath.Join(t.TempDir(), "dest.db")
	var out bytes.Buffer
	if err := runMigrate([]string{"-scopes", "project:alpha", "sqlite:" + srcPath, "sqlite:" + dstPath}, &out); err != nil {
		t.Fatalf("runMigrate: %v", err)
	}

	dst, err := store.OpenSQLite(dstPath)
	if err != nil {
		t.Fatalf("open dest: %v", err)
	}
	defer dst.Close()
	if _, err := dst.Get(ctx, idAlpha); err != nil {
		t.Errorf("idAlpha должен быть перенесён: %v", err)
	}
	if _, err := dst.Get(ctx, idBeta); err == nil {
		t.Error("idBeta не должен быть перенесён (scope отфильтрован)")
	}
}

// --- интеграционный тест с живым PostgreSQL --------------------------------
//
// Гейтится переменной окружения MEMO_TEST_POSTGRES_DSN (полный DSN вида
// postgres://user:pass@host:port/dbname?sslmode=disable — пароль тут допустим,
// это конфиг самого теста, а не аргумент CLI, который parseMigrateEndpoint
// намеренно отвергает). Не задана — пропуск (t.Skip): интеграция с реальным
// сервером не должна ломать `make test` в окружениях без доступа к
// postgres.lan. Тестовые записи используют уникальный scope на каждый прогон
// (ulid в имени) и удаляются точечно по id в t.Cleanup — база общая
// (postgres.lan/memo), TRUNCATE/DROP по её таблицам не выполняются нигде в
// этом тесте.
func TestRunMigrate_SQLiteToPostgresLive(t *testing.T) {
	dsn := os.Getenv("MEMO_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("MEMO_TEST_POSTGRES_DSN не задана — пропуск интеграционного теста с живым PostgreSQL")
	}
	pgCfg, password, err := parseTestPostgresDSN(dsn)
	if err != nil {
		t.Fatalf("разбор MEMO_TEST_POSTGRES_DSN: %v", err)
	}

	srcPath, src := newExportTestStore(t)
	scope := "project:migrate-live-test-" + ulid.Make().String()
	oldID, newID := seedMigrateFixture(t, src, scope)
	src.Close()

	t.Cleanup(func() { cleanupPostgresTestRows(t, pgCfg.DSN(), oldID, newID) })

	if password != "" {
		t.Setenv(memoPostgresPasswordEnv, password)
	}
	destArg := fmt.Sprintf("postgres://%s@%s:%d/%s?sslmode=%s", pgCfg.User, pgCfg.Host, pgCfg.Port, pgCfg.DBName, pgCfg.SSLMode)

	var out bytes.Buffer
	if err := runMigrate([]string{"-scopes", scope, "sqlite:" + srcPath, destArg}, &out); err != nil {
		t.Fatalf("runMigrate sqlite->postgres: %v\nвывод:\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "все записи источника присутствуют в приёмнике") {
		t.Errorf("сверка не прошла: %s", out.String())
	}

	pgCfg.Password = password
	pg, err := store.Open(pgCfg)
	if err != nil {
		t.Fatalf("store.Open(postgres) для проверки: %v", err)
	}
	defer pg.Close()

	ctx := context.Background()
	newM, err := pg.Get(ctx, newID)
	if err != nil {
		t.Fatalf("Get newID в postgres: %v", err)
	}
	if len(newM.Tags) != 2 {
		t.Errorf("newM.Tags = %v, want 2 тега", newM.Tags)
	}
	oldM, err := pg.Get(ctx, oldID)
	if err != nil {
		t.Fatalf("Get oldID в postgres: %v", err)
	}
	if oldM.SupersededBy == nil || *oldM.SupersededBy != newID {
		t.Errorf("oldM.SupersededBy = %v, want %s", oldM.SupersededBy, newID)
	}
	if len(oldM.Links) != 1 || oldM.Links[0].Dst != newID {
		t.Errorf("oldM.Links = %+v, want ребро -> %s", oldM.Links, newID)
	}

	// Перенос обратно в свежий sqlite — второе направление того же пути
	// (DESIGN.md §14 Этап 12: перенос работает в обе стороны).
	backPath := filepath.Join(t.TempDir(), "roundtrip.db")
	var backOut bytes.Buffer
	if err := runMigrate([]string{"-scopes", scope, destArg, "sqlite:" + backPath}, &backOut); err != nil {
		t.Fatalf("runMigrate postgres->sqlite: %v\nвывод:\n%s", err, backOut.String())
	}
	back, err := store.OpenSQLite(backPath)
	if err != nil {
		t.Fatalf("open roundtrip sqlite: %v", err)
	}
	defer back.Close()
	if _, err := back.Get(ctx, newID); err != nil {
		t.Errorf("newID отсутствует после обратного переноса: %v", err)
	}
	if _, err := back.Get(ctx, oldID); err != nil {
		t.Errorf("oldID отсутствует после обратного переноса: %v", err)
	}
}

// parseTestPostgresDSN разбирает полный DSN теста (с паролем в userinfo) в
// config.StoreConfig — в отличие от parseMigrateEndpoint, которая пароль в
// URL намеренно отвергает (это конфиг CLI-аргумента, не тестового окружения).
func parseTestPostgresDSN(dsn string) (cfg config.StoreConfig, password string, err error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return config.StoreConfig{}, "", fmt.Errorf("разбор DSN: %w", err)
	}
	if u.User == nil || u.User.Username() == "" {
		return config.StoreConfig{}, "", fmt.Errorf("DSN без пользователя (postgres://user[:pass]@host:port/dbname)")
	}
	password, _ = u.User.Password()
	port := 5432
	if p := u.Port(); p != "" {
		port, err = strconv.Atoi(p)
		if err != nil {
			return config.StoreConfig{}, "", fmt.Errorf("порт %q: %w", p, err)
		}
	}
	sslmode := u.Query().Get("sslmode")
	if sslmode == "" {
		sslmode = "disable"
	}
	cfg = config.StoreConfig{
		Driver:  "postgres",
		Host:    u.Hostname(),
		Port:    port,
		User:    u.User.Username(),
		DBName:  strings.TrimPrefix(u.Path, "/"),
		SSLMode: sslmode,
	}
	if err := cfg.Validate(); err != nil {
		return config.StoreConfig{}, "", err
	}
	return cfg, password, nil
}

// newTestPgSchema создаёт на живом тестовом сервере (dsn) изолированную
// временную схему PostgreSQL и направляет туда ЛЮБОЕ новое pgx-соединение
// процесса через PGOPTIONS (-c search_path=...) — общий bootstrap для
// интеграционных тестов cmd/memo (doctor_test.go, init_store_test.go),
// которым нужна пустая/предсказуемая схема, а не общая командная база
// (postgres.lan/memo, которую нельзя TRUNCATE/DROP — только read-only
// запросы, см. cleanupPostgresTestRows выше для сравнения с точечной
// уборкой). Уборка регистрируется в порядке: закрытие bootstrap-соединения
// первым cleanup'ом, DROP SCHEMA вторым — t.Cleanup вызывает их в порядке
// LIFO, поэтому DROP SCHEMA реально отрабатывает раньше bootstrap.Close, а
// не наоборот (иначе DROP упал бы на уже закрытом соединении) — общая база
// memo не должна копить мусор от тестовых прогонов.
//
// withPublic добавляет ",public" в search_path — нужен тестам, которым важна
// видимость типа vector (pgvector установлен глобально в public на тестовом
// сервере, см. newTestPgStoreWithVector в internal/memory/store/postgres_test.go);
// тестам отрицательного случая (vector НЕ должен резолвиться) он не нужен.
// prefix — префикс имени схемы, для различимости по имени вызывающего теста
// при отладке по логам сервера. Возвращает имя схемы и открытое
// bootstrap-соединение (для вызывающих, которым после диагностики нужно
// что-то в схеме проверить напрямую — например, что doctor не создал в ней
// таблиц) — оно закрывается тем же t.Cleanup, что и DROP SCHEMA, не раньше.
func newTestPgSchema(t *testing.T, dsn, prefix string, withPublic bool) (schema string, bootstrap *sql.DB) {
	t.Helper()
	bootstrap, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("bootstrap-соединение: %v", err)
	}
	t.Cleanup(func() { bootstrap.Close() })

	schema = prefix + strings.ToLower(ulid.Make().String())
	if _, err := bootstrap.ExecContext(context.Background(), `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("создание тестовой схемы %s: %v", schema, err)
	}
	t.Cleanup(func() {
		if _, err := bootstrap.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Errorf("удаление тестовой схемы %s (общая база memo не должна копить мусор): %v", schema, err)
		}
	})

	searchPath := schema
	if withPublic {
		searchPath += ",public"
	}
	t.Setenv("PGOPTIONS", "-c search_path="+searchPath)
	return schema, bootstrap
}

// cleanupPostgresTestRows удаляет ТОЛЬКО перечисленные id (точечно, не
// TRUNCATE/DROP) из общей postgres.lan/memo — напрямую через database/sql
// (драйвер "pgx" уже зарегистрирован транзитивным импортом internal/memory/store),
// в обход интерфейса Store: Store намеренно не предоставляет hard-delete
// (только мягкое забыание через Supersede, DESIGN.md §7), а тестовым данным
// не место в общей базе даже как тумстоуны. Дочерние таблицы (links, tags,
// memories_vec) чистятся раньше memories из-за FK; ошибки по memories_vec
// игнорируются — таблица создаётся лениво (ResetVectorIndex) и в этом тесте
// (embedder=noop нигде не запускался) обычно не существует вовсе.
func cleanupPostgresTestRows(t *testing.T, dsn string, ids ...string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Logf("cleanupPostgresTestRows: sql.Open: %v", err)
		return
	}
	defer db.Close()
	for _, id := range ids {
		if _, err := db.Exec(`DELETE FROM links WHERE src = $1 OR dst = $1`, id); err != nil {
			t.Logf("cleanupPostgresTestRows: DELETE links %s: %v", id, err)
		}
		if _, err := db.Exec(`DELETE FROM tags WHERE memory_id = $1`, id); err != nil {
			t.Logf("cleanupPostgresTestRows: DELETE tags %s: %v", id, err)
		}
		_, _ = db.Exec(`DELETE FROM memories_vec WHERE memory_id = $1`, id) // таблица может не существовать — игнорируем
		if _, err := db.Exec(`DELETE FROM memories WHERE id = $1`, id); err != nil {
			t.Logf("cleanupPostgresTestRows: DELETE memories %s: %v", id, err)
		}
	}
}
