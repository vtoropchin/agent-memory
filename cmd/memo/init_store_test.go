// Тесты выбора бэкенда хранилища в `memo init` (DESIGN.md §13, §14 Этап 12):
// рендер секции [store], попадание пароля в .env (и НЕ в TOML), неинтерактивный
// прогон с -store-driver=postgres и обратная совместимость дефолтного вывода.
// Сетевая проба подменяется стабом (stubStoreProbe) — единственный тест,
// ходящий в живой PostgreSQL, отделён переменной окружения и скипается без неё
// (TestProbeStoreConnectionLive_RealServer).
package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"agentmemory/internal/config"
)

// isolateHomeForTest перенаправляет $HOME в свежий временный каталог теста —
// обязательная изоляция для любого теста, зовущего doInit со
// StoreDriver=postgres (доработка задачи 12.1 по итогам ревью): при
// отсутствующем <dir>/agentmemory.toml findExistingSqliteMemory проверяет
// дефолтный путь ~/.agentmemory/memory.db (defaultStorePath) — без этой
// изоляции тест читал бы РЕАЛЬНУЮ память разработчика/CI-раннера из его
// домашнего каталога (найден живой файл на несколько МиБ при ревью). Общий
// хелпер вместо дублирования t.Setenv("HOME", t.TempDir()) в каждом тесте;
// возвращает изолированный каталог — нужен тестам, которым помимо самой
// изоляции требуется сам путь (например, чтобы положить туда дефолтный
// ~/.agentmemory/memory.db).
func isolateHomeForTest(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

// stubStoreProbe подменяет пакетную пробу подключения (probeStoreConnection,
// init.go) на стаб, возвращающий (nil, probeErr) — без предупреждений о
// готовности БД, см. stubStoreProbeWithWarnings для тестов этой части задачи
// 12.1 — и восстанавливает оригинал через t.Cleanup. Возвращает указатель на
// config.StoreConfig, с которым пробу позвали (нулевой, если не звали вовсе)
// — так тест проверяет и то, что init пробует именно введённые параметры.
func stubStoreProbe(t *testing.T, probeErr error) *config.StoreConfig {
	t.Helper()
	return stubStoreProbeWithWarnings(t, nil, probeErr)
}

// stubStoreProbeWithWarnings — как stubStoreProbe, но стаб возвращает ещё и
// warnings (диагностика готовности БД: pgvector/право CREATE, задача 12.1).
func stubStoreProbeWithWarnings(t *testing.T, warnings []string, probeErr error) *config.StoreConfig {
	t.Helper()
	var got config.StoreConfig
	orig := probeStoreConnection
	probeStoreConnection = func(cfg config.StoreConfig) ([]string, error) {
		got = cfg
		if probeErr != nil {
			return nil, probeErr
		}
		return warnings, nil
	}
	t.Cleanup(func() { probeStoreConnection = orig })
	return &got
}

// TestRenderStoreTomlBlock_SQLiteUnchanged — регрессия обратной совместимости:
// секция [store] для дефолтного (sqlite) прогона обязана остаться побайтово
// той же, что до появления второго бэкенда, в том числе БЕЗ строки driver.
// Ожидаемый текст выписан литералом намеренно (а не собран из тех же констант,
// что рендерер) — иначе тест «поедет» вместе с любой правкой рендера.
func TestRenderStoreTomlBlock_SQLiteUnchanged(t *testing.T) {
	want := `# Общий файл БД на все проекты (DESIGN.md §2.1: один файл, изоляция через scope).
# Каталог создаётся автоматически при первом старте (store.Open).
path = "~/.agentmemory/memory.db"
`
	got := renderStoreTomlBlock(applyInitDefaults(initConfig{}))
	if got != want {
		t.Errorf("секция [store] для sqlite изменилась:\nполучено:\n%s\nхочу:\n%s", got, want)
	}
}

// TestRenderStoreTomlBlock_PostgresFieldsWithoutPassword проверяет, что для
// postgres в секцию попадают все параметры подключения — и что пароля там нет
// даже когда он задан (DESIGN.md §13: секреты только в env/.env).
func TestRenderStoreTomlBlock_PostgresFieldsWithoutPassword(t *testing.T) {
	cfg := applyInitDefaults(initConfig{
		StoreDriver:      storeDriverPostgres,
		PostgresHost:     "postgres.lan",
		PostgresPassword: "s3cret",
	})

	got := renderStoreTomlBlock(cfg)

	for _, want := range []string{
		`driver  = "postgres"`,
		`host    = "postgres.lan"`,
		fmt.Sprintf("port    = %d", defaultPostgresPort),
		fmt.Sprintf(`user    = %q`, defaultPostgresUser),
		fmt.Sprintf(`dbname  = %q`, defaultPostgresDBName),
		fmt.Sprintf(`sslmode = %q`, defaultPostgresSSLMode),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("секция [store] не содержит %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "s3cret") {
		t.Errorf("пароль просочился в секцию [store]:\n%s", got)
	}
}

// TestDoInit_PostgresWritesPasswordToEnvNotToml — сквозная проверка секрета:
// пароль уходит в .env под MEMO_POSTGRES_PASSWORD (мерж с уже существующим
// файлом: чужой ключ сохраняется), в agentmemory.toml не попадает, а сам
// конфиг читается config.Load обратно как валидный postgres-store.
func TestDoInit_PostgresWritesPasswordToEnvNotToml(t *testing.T) {
	isolateHomeForTest(t)
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("# чужой файл\nFOREIGN_KEY=keep-me\nMEMO_POSTGRES_PASSWORD=old\n"), 0o600); err != nil {
		t.Fatalf("подготовка .env: %v", err)
	}
	probed := stubStoreProbe(t, nil)

	var out strings.Builder
	err := doInit(initConfig{
		Project:          "myproj",
		Dir:              dir,
		StoreDriver:      storeDriverPostgres,
		PostgresHost:     "postgres.lan",
		PostgresDBName:   "team",
		PostgresPassword: "s3cret",
	}, &out)
	if err != nil {
		t.Fatalf("doInit: %v", err)
	}

	if probed.Driver != storeDriverPostgres || probed.Host != "postgres.lan" || probed.DBName != "team" || probed.Password != "s3cret" {
		t.Errorf("проба вызвана с %+v, хочу параметры введённого подключения", *probed)
	}

	tomlPath := filepath.Join(dir, "agentmemory.toml")
	tomlRaw, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatalf("чтение %s: %v", tomlPath, err)
	}
	if strings.Contains(string(tomlRaw), "s3cret") {
		t.Errorf("пароль попал в agentmemory.toml:\n%s", tomlRaw)
	}

	cfg, err := config.Load(tomlPath)
	if err != nil {
		t.Fatalf("config.Load(%s): %v", tomlPath, err)
	}
	if cfg.Store.Driver != storeDriverPostgres || cfg.Store.Host != "postgres.lan" || cfg.Store.DBName != "team" {
		t.Errorf("Store = %+v, хочу postgres postgres.lan/team", cfg.Store)
	}
	if cfg.Store.Port != defaultPostgresPort || cfg.Store.SSLMode != defaultPostgresSSLMode {
		t.Errorf("Store порт/sslmode = %d/%q, хочу дефолты %d/%q", cfg.Store.Port, cfg.Store.SSLMode, defaultPostgresPort, defaultPostgresSSLMode)
	}
	if cfg.Store.Password != "" {
		t.Errorf("config.Load вернул непустой Store.Password %q — пароль не должен читаться из TOML", cfg.Store.Password)
	}

	envRaw, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("чтение %s: %v", envPath, err)
	}
	env := string(envRaw)
	if !strings.Contains(env, "MEMO_POSTGRES_PASSWORD=s3cret") {
		t.Errorf(".env не содержит обновлённый пароль:\n%s", env)
	}
	if strings.Contains(env, "MEMO_POSTGRES_PASSWORD=old") {
		t.Errorf(".env сохранил старое значение пароля (не upsert):\n%s", env)
	}
	if !strings.Contains(env, "FOREIGN_KEY=keep-me") || !strings.Contains(env, "# чужой файл") {
		t.Errorf(".env потерял чужое содержимое:\n%s", env)
	}
}

// TestDoInit_PostgresEmptyPasswordSkipsEnv проверяет, что пустой пароль — не
// ошибка (trust-аутентификация) и .env из-за него не создаётся зря, но строка
// .env в .gitignore появляется всё равно: при postgres пользователю штатно
// предлагают создать этот файл позже руками.
func TestDoInit_PostgresEmptyPasswordSkipsEnv(t *testing.T) {
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

	if _, err := os.Stat(filepath.Join(dir, ".env")); !os.IsNotExist(err) {
		t.Errorf(".env создан при пустом пароле: err=%v", err)
	}

	gitignore, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatalf("чтение .gitignore: %v", err)
	}
	if !strings.Contains(string(gitignore), ".env") {
		t.Errorf(".gitignore не игнорирует .env при driver=postgres:\n%s", gitignore)
	}
}

// TestDoInit_SQLiteIgnoresPostgresFlags — инвариант «поля чужого бэкенда не
// текут»: при sqlite заданные -postgres-* не попадают ни в TOML, ни в .env
// (пароль!), и сетевая проба не выполняется вовсе.
func TestDoInit_SQLiteIgnoresPostgresFlags(t *testing.T) {
	dir := t.TempDir()
	probeCalled := false
	orig := probeStoreConnection
	probeStoreConnection = func(config.StoreConfig) ([]string, error) {
		probeCalled = true
		return nil, nil
	}
	t.Cleanup(func() { probeStoreConnection = orig })

	var out strings.Builder
	err := doInit(initConfig{
		Project:          "myproj",
		Dir:              dir,
		ReasonerProvider: "noop",
		StoreDriver:      storeDriverSQLite,
		PostgresHost:     "postgres.lan",
		PostgresPassword: "s3cret",
	}, &out)
	if err != nil {
		t.Fatalf("doInit: %v", err)
	}

	if probeCalled {
		t.Errorf("проба подключения вызвана при driver=sqlite")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "agentmemory.toml"))
	if err != nil {
		t.Fatalf("чтение agentmemory.toml: %v", err)
	}
	if strings.Contains(string(raw), "postgres") || strings.Contains(string(raw), "s3cret") {
		t.Errorf("postgres-поля просочились в sqlite-конфиг:\n%s", raw)
	}
	if _, err := os.Stat(filepath.Join(dir, ".env")); !os.IsNotExist(err) {
		t.Errorf(".env создан при driver=sqlite (пароль postgres не должен записываться): err=%v", err)
	}
}

// TestDoInit_PostgresPasswordFromExistingDotEnv — повторный `memo init` в уже
// настроенном проекте: пароль не передан флагом, но лежит в <dir>/.env с
// прошлого прогона — он должен попасть в пробу (иначе она падает на
// аутентификации) и при этом .env не должен быть перезаписан.
func TestDoInit_PostgresPasswordFromExistingDotEnv(t *testing.T) {
	isolateHomeForTest(t)
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	envContent := "FOREIGN_KEY=keep-me\nMEMO_POSTGRES_PASSWORD=from-dotenv\n"
	if err := os.WriteFile(envPath, []byte(envContent), 0o600); err != nil {
		t.Fatalf("подготовка .env: %v", err)
	}
	probed := stubStoreProbe(t, nil)

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

	if probed.Password != "from-dotenv" {
		t.Errorf("проба получила Password %q, хочу пароль из <dir>/.env", probed.Password)
	}

	got, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("чтение .env: %v", err)
	}
	if string(got) != envContent {
		t.Errorf(".env изменён повторным init:\n%s\nхочу:\n%s", got, envContent)
	}
}

// TestParsePostgresPortInput — единственное место с правилом допустимого порта
// (через него идут и промпт, и флаг -postgres-port).
func TestParsePostgresPortInput(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{in: "", want: defaultPostgresPort},
		{in: " 6432 ", want: 6432},
		{in: "65535", want: 65535},
		{in: "0", wantErr: true},
		{in: "-5", wantErr: true},
		{in: "65536", wantErr: true},
		{in: "5432x", wantErr: true},
	}

	for _, tc := range cases {
		got, err := parsePostgresPortInput(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parsePostgresPortInput(%q) = %d, хочу ошибку", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parsePostgresPortInput(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parsePostgresPortInput(%q) = %d, хочу %d", tc.in, got, tc.want)
		}
	}
}

// TestDoInit_StoreValidationAndProbeErrors проверяет три способа не получить
// заведомо нерабочий конфиг в неинтерактивном прогоне: неизвестный драйвер,
// отсутствие обязательных полей postgres и неудачная проба подключения. Во всех
// случаях agentmemory.toml не должен появиться вовсе.
func TestDoInit_StoreValidationAndProbeErrors(t *testing.T) {
	cases := []struct {
		name     string
		cfg      initConfig
		probeErr error
		wantIn   string
	}{
		{
			name:   "неизвестный драйвер",
			cfg:    initConfig{StoreDriver: "mysql"},
			wantIn: "mysql",
		},
		{
			name:   "postgres без host",
			cfg:    initConfig{StoreDriver: storeDriverPostgres},
			wantIn: "store.host",
		},
		{
			name:   "порт вне диапазона",
			cfg:    initConfig{StoreDriver: storeDriverPostgres, PostgresHost: "postgres.lan", PostgresPort: -5},
			wantIn: "1..65535",
		},
		{
			name:     "неудачная проба",
			cfg:      initConfig{StoreDriver: storeDriverPostgres, PostgresHost: "postgres.lan"},
			probeErr: errors.New("connection refused"),
			wantIn:   "connection refused",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			stubStoreProbe(t, tc.probeErr)

			cfg := tc.cfg
			cfg.Project = "myproj"
			cfg.Dir = dir

			var out strings.Builder
			err := doInit(cfg, &out)
			if err == nil {
				t.Fatalf("doInit прошёл без ошибки, хочу ошибку про %q", tc.wantIn)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("ошибка %q не упоминает %q", err, tc.wantIn)
			}
			if _, statErr := os.Stat(filepath.Join(dir, "agentmemory.toml")); !os.IsNotExist(statErr) {
				t.Errorf("agentmemory.toml создан несмотря на ошибку: err=%v", statErr)
			}
		})
	}
}

// TestRunInit_NoInputFlag_PostgresFlags проверяет неинтерактивный путь целиком
// (разбор новых флагов -store-driver/-postgres-*), включая то, что -postgres-password
// уходит в .env, а не в TOML.
func TestRunInit_NoInputFlag_PostgresFlags(t *testing.T) {
	isolateHomeForTest(t)
	dir := t.TempDir()
	probed := stubStoreProbe(t, nil)

	var out strings.Builder
	err := runInit([]string{
		"-dir", dir,
		"-project", "myproj",
		"-no-input",
		"-store-driver", "postgres",
		"-postgres-host", "db.example.lan",
		"-postgres-port", "6432",
		"-postgres-user", "alice",
		"-postgres-dbname", "team",
		"-postgres-sslmode", "require",
		"-postgres-password", "s3cret",
	}, &out)
	if err != nil {
		t.Fatalf("runInit: %v", err)
	}

	want := config.StoreConfig{
		Driver: storeDriverPostgres, Host: "db.example.lan", Port: 6432,
		User: "alice", DBName: "team", SSLMode: "require", Password: "s3cret",
	}
	if *probed != want {
		t.Errorf("проба вызвана с %+v, хочу %+v", *probed, want)
	}

	tomlPath := filepath.Join(dir, "agentmemory.toml")
	tomlRaw, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatalf("чтение %s: %v", tomlPath, err)
	}
	if strings.Contains(string(tomlRaw), "path ") {
		t.Errorf("секция [store] для postgres содержит sqlite-путь:\n%s", tomlRaw)
	}

	cfg, err := config.Load(tomlPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	got := cfg.Store
	got.Password = "s3cret" // в TOML пароля нет по определению — сверяем остальное одним сравнением
	got.Path = ""           // path в postgres-конфиг не пишется, Load оставляет дефолт config.Default()
	if got != want {
		t.Errorf("Store из сгенерированного конфига = %+v, хочу %+v", got, want)
	}

	envRaw, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatalf("чтение .env: %v", err)
	}
	if !strings.Contains(string(envRaw), "MEMO_POSTGRES_PASSWORD=s3cret") {
		t.Errorf(".env не содержит пароль:\n%s", envRaw)
	}
}

// TestRunInit_UnknownStoreDriverRejectedEarly проверяет, что опечатка в
// -store-driver отклоняется в runInit (до мастера и до любых файлов), а не
// уводит молча в sqlite-ветку.
func TestRunInit_UnknownStoreDriverRejectedEarly(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder

	err := runInit([]string{"-dir", dir, "-project", "myproj", "-no-input", "-store-driver", "postgre"}, &out)
	if err == nil {
		t.Fatalf("runInit прошёл с неизвестным драйвером")
	}
	if !strings.Contains(err.Error(), "postgre") {
		t.Errorf("ошибка %q не называет неизвестный драйвер", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "agentmemory.toml")); !os.IsNotExist(statErr) {
		t.Errorf("agentmemory.toml создан несмотря на ошибку: err=%v", statErr)
	}
}

// TestRunInit_DefaultStoreSectionUnchanged — регрессия обратной совместимости
// на уровне файла: прогон без новых флагов даёт ровно ту же секцию [store]
// (sqlite, без driver), что и до появления второго бэкенда, и не трогает пробу.
func TestRunInit_DefaultStoreSectionUnchanged(t *testing.T) {
	dir := t.TempDir()
	probeCalled := false
	orig := probeStoreConnection
	probeStoreConnection = func(config.StoreConfig) ([]string, error) {
		probeCalled = true
		return nil, nil
	}
	t.Cleanup(func() { probeStoreConnection = orig })

	var out strings.Builder
	if err := runInit([]string{"-dir", dir, "-project", "myproj", "-no-input"}, &out); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	if probeCalled {
		t.Errorf("проба подключения вызвана при driver=sqlite")
	}

	raw, err := os.ReadFile(filepath.Join(dir, "agentmemory.toml"))
	if err != nil {
		t.Fatalf("чтение agentmemory.toml: %v", err)
	}
	want := `[store]
# Общий файл БД на все проекты (DESIGN.md §2.1: один файл, изоляция через scope).
# Каталог создаётся автоматически при первом старте (store.Open).
path = "~/.agentmemory/memory.db"

[embedder]
`
	if !strings.Contains(string(raw), want) {
		t.Errorf("секция [store] дефолтного конфига изменилась:\n%s", raw)
	}
}

// pgProbeTestDSNEnv — DSN живого тестового сервера PostgreSQL для интеграционной
// проверки реальной пробы. Конвенция та же, что у postgres-тестов store
// (MEMO_TEST_POSTGRES_DSN, internal/memory/store/postgres_test.go): переменная не
// задана — тест скипается, поэтому `make test` без сервера остаётся зелёным.
const pgProbeTestDSNEnv = "MEMO_TEST_POSTGRES_DSN"

// TestProbeStoreConnectionLive_RealServer прогоняет НЕподменённую пробу против
// живого сервера. Проверяется именно инвариант read-only-пробы: она проходит на
// базе, где схемы памяти может не быть вовсе (Health не читает таблицы memo), и
// сама ничего не создаёт — поэтому тест безопасен для общей командной базы и не
// требует уборки за собой (ни TRUNCATE, ни DROP тут делать нечего).
func TestProbeStoreConnectionLive_RealServer(t *testing.T) {
	dsn := os.Getenv(pgProbeTestDSNEnv)
	if dsn == "" {
		t.Skipf("%s не задан — интеграционная проба пропущена", pgProbeTestDSNEnv)
	}
	isolateHomeForTest(t)

	cfg, err := storeConfigFromDSN(dsn)
	if err != nil {
		t.Fatalf("разбор %s: %v", pgProbeTestDSNEnv, err)
	}

	t.Run("готовая база — без предупреждений", func(t *testing.T) {
		warnings, err := probeStoreConnectionLive(cfg)
		if err != nil {
			t.Fatalf("проба %s: %v", cfg.Target(), err)
		}
		// Сервер задачи 12.1 (см. её тестовый DSN в README/tasks) уже настроен
		// прод-использованием memo: pgvector установлен, роль владеет своей базой
		// — готовая база не должна давать предупреждений о готовности.
		if len(warnings) != 0 {
			t.Errorf("проба готовой БД %s вернула предупреждения: %v", cfg.Target(), warnings)
		}
	})

	// Доработка по итогам ревью: "warnings пуст" выше — ложнозелёная проверка
	// сама по себе. Она пройдёт одинаково и если диагностика готовности
	// реально посчитала "всё ок", и если вызов CheckPostgresReadiness вырезать
	// из probeStoreConnectionLive целиком (warnings тогда тоже nil) — на
	// готовом сервере эти два случая неотличимы по одному только len==0. Этот
	// подтест создаёт РЕАЛЬНОЕ негативное состояние на живом сервере (схема,
	// в search_path которой нет public — тип vector физически не резолвится)
	// тем же приёмом PGOPTIONS, что doctor_test.go/postgres_test.go, и требует,
	// чтобы проба его нашла — только так можно доказать, что диагностика
	// реально выполняется, а не просто ничего не возвращает.
	t.Run("схема без public в search_path — диагностика реально находит нерезолвящийся vector", func(t *testing.T) {
		// newTestPgSchema (migrate_test.go) — общий bootstrap изолированной
		// схемы для интеграционных postgres-тестов cmd/memo (создание,
		// t.Cleanup с DROP SCHEMA, PGOPTIONS): withPublic=false — та же
		// изоляция, что у store.newTestPgStore (internal/memory/store/
		// postgres_test.go), направляет ЛЮБОЕ новое соединение pgx
		// (dialPostgres -> sql.Open("pgx", cfg.DSN()), который подхватывает
		// PGOPTIONS так же, как явный DSN-параметр) в схему, где pgvector
		// (установлен глобально в public) не резолвится.
		newTestPgSchema(t, dsn, "memo_test_probe_", false)

		warnings, err := probeStoreConnectionLive(cfg)
		if err != nil {
			t.Fatalf("проба %s: %v", cfg.Target(), err)
		}
		if len(warnings) != 1 {
			t.Fatalf("проба изолированной схемы без public вернула %d предупреждений, хочу 1 (про vector): %v", len(warnings), warnings)
		}
		if !strings.Contains(warnings[0], "vector") {
			t.Errorf("предупреждение = %q, хочу упоминание vector/pgvector", warnings[0])
		}
		// "GRANT", а не "CREATE": текст самого vector-предупреждения legitimно
		// упоминает "CREATE EXTENSION" (см. storeReadinessWarnings) — "CREATE"
		// не отличил бы "нет права CREATE" от этого; "GRANT" встречается
		// только в CanCreate-предупреждении (см. init_readiness_test.go).
		if strings.Contains(warnings[0], "GRANT") {
			t.Errorf("предупреждение = %q не должно упоминать право CREATE (GRANT) — роль владеет только что созданной ею схемой", warnings[0])
		}
	})

	// Тот же путь целиком: init с этими параметрами доходит до конца и
	// генерирует конфиг, который config.Load принимает как валидный.
	t.Run("doInit доходит до конца и генерирует валидный конфиг", func(t *testing.T) {
		dir := t.TempDir()
		var out strings.Builder
		err := doInit(initConfig{
			Project:          "probe-integration",
			Dir:              dir,
			ReasonerProvider: "noop",
			EmbedderProvider: "noop",
			StoreDriver:      storeDriverPostgres,
			PostgresHost:     cfg.Host,
			PostgresPort:     cfg.Port,
			PostgresUser:     cfg.User,
			PostgresDBName:   cfg.DBName,
			PostgresSSLMode:  cfg.SSLMode,
			PostgresPassword: cfg.Password,
		}, &out)
		if err != nil {
			t.Fatalf("doInit с живой пробой: %v", err)
		}
		loaded, err := config.Load(filepath.Join(dir, "agentmemory.toml"))
		if err != nil {
			t.Fatalf("config.Load: %v", err)
		}
		if loaded.Store.Target() != cfg.Target() {
			t.Errorf("Target сгенерированного конфига = %q, хочу %q", loaded.Store.Target(), cfg.Target())
		}
	})
}

// storeConfigFromDSN разбирает postgres:// DSN тестового сервера в
// config.StoreConfig — только для интеграционного теста выше (прод-код ходит в
// обратную сторону, StoreConfig.DSN).
func storeConfigFromDSN(dsn string) (config.StoreConfig, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return config.StoreConfig{}, err
	}
	port := defaultPostgresPort
	if p := u.Port(); p != "" {
		port, err = strconv.Atoi(p)
		if err != nil {
			return config.StoreConfig{}, fmt.Errorf("порт %q: %w", p, err)
		}
	}
	password, _ := u.User.Password()
	sslmode := u.Query().Get("sslmode")
	if sslmode == "" {
		sslmode = defaultPostgresSSLMode
	}
	return config.StoreConfig{
		Driver:   storeDriverPostgres,
		Host:     u.Hostname(),
		Port:     port,
		User:     u.User.Username(),
		DBName:   strings.TrimPrefix(u.Path, "/"),
		SSLMode:  sslmode,
		Password: password,
	}, nil
}
