package config

import (
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestScopeConfigSaveScope(t *testing.T) {
	t.Run("project задан", func(t *testing.T) {
		c := ScopeConfig{Project: "myproject"}
		if got := c.SaveScope(); got != "project:myproject" {
			t.Errorf("SaveScope() = %q, want %q", got, "project:myproject")
		}
	})

	t.Run("project пуст", func(t *testing.T) {
		c := ScopeConfig{Project: ""}
		if got := c.SaveScope(); got != "" {
			t.Errorf("SaveScope() = %q, want пусто", got)
		}
	})
}

func TestScopeConfigRecallScopes(t *testing.T) {
	t.Run("project задан", func(t *testing.T) {
		c := ScopeConfig{Project: "myproject", Shared: []string{"user:*"}}
		want := []string{"project:myproject", "user:*"}
		if got := c.RecallScopes(); !reflect.DeepEqual(got, want) {
			t.Errorf("RecallScopes() = %v, want %v", got, want)
		}
	})

	t.Run("project пуст", func(t *testing.T) {
		c := ScopeConfig{Project: "", Shared: []string{"user:*"}}
		if got := c.RecallScopes(); got != nil {
			t.Errorf("RecallScopes() = %v, want nil", got)
		}
	})
}

func TestDefaultScope(t *testing.T) {
	cfg := Default()
	if cfg.Scope.Project != "" {
		t.Errorf("Default().Scope.Project = %q, want пусто", cfg.Scope.Project)
	}
	want := []string{"user:*"}
	if !reflect.DeepEqual(cfg.Scope.Shared, want) {
		t.Errorf("Default().Scope.Shared = %v, want %v", cfg.Scope.Shared, want)
	}
}

// TestDefaultEmbedderBaseURLEmpty фиксирует регрессию: BaseURL по умолчанию должен
// быть пуст, а не хардкодить ollama-специфичный адрес — иначе при provider="openai"
// без явного base_url в TOML запрос ушёл бы на ollama-эндпоинт вместо
// провайдер-специфичного дефолта (тот выбирается в cmd/memo.newEmbedder, не здесь).
func TestDefaultEmbedderBaseURLEmpty(t *testing.T) {
	cfg := Default()
	if cfg.Embedder.BaseURL != "" {
		t.Errorf("Default().Embedder.BaseURL = %q, want пусто (дефолт провайдер-специфичен, живёт в cmd/memo)", cfg.Embedder.BaseURL)
	}
}

func TestLoadScopeFromTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	toml := `
[scope]
project = "agentmemory"
shared = ["user:*", "team:core"]
`
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Scope.Project != "agentmemory" {
		t.Errorf("Scope.Project = %q, want %q", cfg.Scope.Project, "agentmemory")
	}
	want := []string{"user:*", "team:core"}
	if !reflect.DeepEqual(cfg.Scope.Shared, want) {
		t.Errorf("Scope.Shared = %v, want %v (должен полностью перезаписать дефолт)", cfg.Scope.Shared, want)
	}
	// Остальные секции остаются дефолтными — [scope] не влияет на них.
	if cfg.Store.Path != Default().Store.Path {
		t.Errorf("Store.Path изменился при загрузке только [scope]: %q", cfg.Store.Path)
	}
}

func TestMaintConfigDurationsValid(t *testing.T) {
	m := MaintConfig{DecayInterval: "24h", ConsolidateAfter: "168h"}
	decay, consolidate, err := m.Durations()
	if err != nil {
		t.Fatalf("Durations: %v", err)
	}
	if decay != 24*time.Hour {
		t.Errorf("decayInterval = %v, want 24h", decay)
	}
	if consolidate != 168*time.Hour {
		t.Errorf("consolidateAfter = %v, want 168h", consolidate)
	}
}

func TestMaintConfigDurationsInvalidDecayInterval(t *testing.T) {
	m := MaintConfig{DecayInterval: "не-длительность", ConsolidateAfter: "168h"}
	if _, _, err := m.Durations(); err == nil {
		t.Errorf("Durations() err = nil, want ошибку для невалидного decay_interval")
	}
}

func TestMaintConfigDurationsInvalidConsolidateAfter(t *testing.T) {
	m := MaintConfig{DecayInterval: "24h", ConsolidateAfter: "не-длительность"}
	if _, _, err := m.Durations(); err == nil {
		t.Errorf("Durations() err = nil, want ошибку для невалидного consolidate_after")
	}
}

// TestMaintConfigDurationsNonPositiveRejected — time.ParseDuration успешно
// разбирает и "0s", и отрицательные значения вроде "-1h" без ошибки; Durations()
// обязан отвергать такие значения отдельно, иначе непозитивный decay_interval
// доходит до time.NewTicker в Scheduler.Run и роняет процесс паникой.
func TestMaintConfigDurationsNonPositiveRejected(t *testing.T) {
	cases := []struct {
		name string
		m    MaintConfig
	}{
		{"decay_interval = 0s", MaintConfig{DecayInterval: "0s", ConsolidateAfter: "168h"}},
		{"decay_interval = -1h", MaintConfig{DecayInterval: "-1h", ConsolidateAfter: "168h"}},
		{"consolidate_after = 0s", MaintConfig{DecayInterval: "24h", ConsolidateAfter: "0s"}},
		{"consolidate_after = -1h", MaintConfig{DecayInterval: "24h", ConsolidateAfter: "-1h"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := tc.m.Durations(); err == nil {
				t.Errorf("Durations() err = nil, want ошибку для непозитивной длительности")
			}
		})
	}
}

// TestLoadRejectsNonPositiveMaintDuration — тот же случай на уровне Load: TOML с
// decay_interval = "0s" должен фейлить загрузку конфига, а не проходить как
// валидный (что уронило бы memo на старте через time.NewTicker).
func TestLoadRejectsNonPositiveMaintDuration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	toml := `
[maint]
decay_interval = "0s"
`
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Errorf("Load() err = nil, want ошибку из-за непозитивного maint.decay_interval")
	}
}

func TestDefaultMaintDurationsAreValid(t *testing.T) {
	if _, _, err := Default().Maint.Durations(); err != nil {
		t.Errorf("Default().Maint.Durations(): %v, дефолты должны быть валидны", err)
	}
}

func TestLoadRejectsInvalidMaintDuration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	toml := `
[maint]
decay_interval = "не-длительность"
`
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Errorf("Load() err = nil, want ошибку из-за невалидного maint.decay_interval")
	}
}

func TestLoadAcceptsValidMaintDuration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	toml := `
[maint]
decay_interval = "1h"
consolidate_after = "72h"
`
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	decay, consolidate, err := cfg.Maint.Durations()
	if err != nil {
		t.Fatalf("Durations: %v", err)
	}
	if decay != time.Hour {
		t.Errorf("decayInterval = %v, want 1h", decay)
	}
	if consolidate != 72*time.Hour {
		t.Errorf("consolidateAfter = %v, want 72h", consolidate)
	}
}

// TestDefaultStoreDriverIsSQLite фиксирует обратную совместимость (DESIGN.md
// §13): дефолт driver — "sqlite", Path — прежний, существующие конфиги без
// [store].driver продолжают работать без единой правки.
func TestDefaultStoreDriverIsSQLite(t *testing.T) {
	cfg := Default()
	if cfg.Store.Driver != "sqlite" {
		t.Errorf("Default().Store.Driver = %q, want %q", cfg.Store.Driver, "sqlite")
	}
	if cfg.Store.Path != "~/.agentmemory/memory.db" {
		t.Errorf("Default().Store.Path = %q, дефолтный путь не должен меняться", cfg.Store.Path)
	}
}

// TestLoadEmptyDriverTreatedAsSQLite — конфиг без [store].driver (старые
// конфиги, написанные до появления этого поля) продолжает работать как
// sqlite: значение остаётся дефолтным ("sqlite" из Default()), явно пустой
// driver ("" в TOML) тоже валиден и трактуется как sqlite (StoreConfig.Validate).
func TestLoadEmptyDriverTreatedAsSQLite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	toml := `
[store]
path = "/tmp/custom.db"
`
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Store.Driver != "sqlite" {
		t.Errorf("Store.Driver = %q, want %q (старый конфиг без driver остаётся дефолтом)", cfg.Store.Driver, "sqlite")
	}
	if cfg.Store.Path != "/tmp/custom.db" {
		t.Errorf("Store.Path = %q, want %q", cfg.Store.Path, "/tmp/custom.db")
	}
	if err := (StoreConfig{Driver: ""}).Validate(); err != nil {
		t.Errorf("Validate() явно пустого driver = %v, want nil", err)
	}
}

// TestLoadParsesPostgresSection проверяет разбор секции [store] с
// driver="postgres": все поля попадают в StoreConfig, Password остаётся
// пустым (никогда не читается из TOML — только из env, см. cmd/memo).
func TestLoadParsesPostgresSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	toml := `
[store]
driver  = "postgres"
host    = "postgres.lan"
port    = 5432
user    = "memo"
dbname  = "memo"
sslmode = "disable"
`
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Path здесь не проверяем: TOML не задаёт store.path, поэтому в нём остаётся
	// sqlite-дефолт из Default() — неиспользуемый и безобидный при driver=postgres,
	// но сравнивать его с чем-либо конкретным нет смысла для этого теста.
	got := cfg.Store
	got.Path = ""
	want := StoreConfig{
		Driver:  "postgres",
		Host:    "postgres.lan",
		Port:    5432,
		User:    "memo",
		DBName:  "memo",
		SSLMode: "disable",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Store (без Path) = %+v, want %+v", got, want)
	}
	if cfg.Store.Password != "" {
		t.Errorf("Store.Password = %q, want пусто (пароль в TOML не читается)", cfg.Store.Password)
	}
}

// TestStoreConfigValidate покрывает StoreConfig.Validate: дефолт/sqlite всегда
// валидны, postgres требует host/user/dbname, неизвестный driver — ошибка.
func TestStoreConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     StoreConfig
		wantErr bool
	}{
		{"пустой driver — валиден", StoreConfig{}, false},
		{"driver sqlite — валиден", StoreConfig{Driver: "sqlite", Path: "x.db"}, false},
		{"driver postgres с полными полями — валиден", StoreConfig{Driver: "postgres", Host: "h", User: "u", DBName: "d"}, false},
		{"driver postgres без host — ошибка", StoreConfig{Driver: "postgres", User: "u", DBName: "d"}, true},
		{"driver postgres без user — ошибка", StoreConfig{Driver: "postgres", Host: "h", DBName: "d"}, true},
		{"driver postgres без dbname — ошибка", StoreConfig{Driver: "postgres", Host: "h", User: "u"}, true},
		{"неизвестный driver — ошибка", StoreConfig{Driver: "mysql"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestLoadRejectsInvalidStoreDriver — на уровне Load: неизвестный
// store.driver должен фейлить загрузку конфига (опечатка, а не деградация).
func TestLoadRejectsInvalidStoreDriver(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	toml := `
[store]
driver = "mysql"
`
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Errorf("Load() err = nil, want ошибку из-за неизвестного store.driver")
	}
}

// TestStoreConfigDSN проверяет сборку DSN и URL-экранирование спецсимволов в
// пароле/логине (@, :, /, пробел) — они не должны ломать разбор DSN
// драйвером pgx/lib-pq.
func TestStoreConfigDSN(t *testing.T) {
	c := StoreConfig{
		Driver:   "postgres",
		Host:     "postgres.lan",
		Port:     5432,
		User:     "memo",
		DBName:   "memo",
		SSLMode:  "disable",
		Password: "p@ss:w/ord ро",
	}
	dsn := c.DSN()

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("DSN() = %q не парсится как URL: %v", dsn, err)
	}
	if u.Scheme != "postgres" {
		t.Errorf("Scheme = %q, want postgres", u.Scheme)
	}
	if u.Hostname() != "postgres.lan" || u.Port() != "5432" {
		t.Errorf("Host = %q, want postgres.lan:5432", u.Host)
	}
	if u.User.Username() != "memo" {
		t.Errorf("User = %q, want memo", u.User.Username())
	}
	pw, ok := u.User.Password()
	if !ok || pw != "p@ss:w/ord ро" {
		t.Errorf("Password = %q (ok=%v), want %q (экранирование должно давать корректный round-trip)", pw, ok, "p@ss:w/ord ро")
	}
	if strings.TrimPrefix(u.Path, "/") != "memo" {
		t.Errorf("Path = %q, want /memo", u.Path)
	}
	if got := u.Query().Get("sslmode"); got != "disable" {
		t.Errorf("sslmode = %q, want disable", got)
	}
}

// TestStoreConfigDSNEmptyPassword — пустой пароль не должен превращаться в
// "user:" (двоеточие без значения) — драйверы при trust-аутентификации
// ожидают userinfo вовсе без ":".
func TestStoreConfigDSNEmptyPassword(t *testing.T) {
	c := StoreConfig{Host: "h", Port: 5432, User: "memo", DBName: "d", SSLMode: "disable"}
	dsn := c.DSN()
	if strings.Contains(dsn, "memo:@") {
		t.Errorf("DSN() = %q содержит пустой пароль как \"user:@\", want userinfo без двоеточия", dsn)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("DSN() = %q не парсится как URL: %v", dsn, err)
	}
	if _, ok := u.User.Password(); ok {
		t.Errorf("Password() ok = true при пустом Password, want false")
	}
}

// TestDefaultRecallDebtWeight фиксирует дефолт понижающего множителя для kind=debt
// (задача с техдолгом): 0.6, тот же, что recall.New подставляет при отсутствии
// поля в TOML. Сравнение намеренно с литералом, а не с константой DefaultDebtWeight
// — иначе тест стал бы тавтологией (значение сравнивалось бы само с собой) и не
// заметил бы случайную правку самой константы.
func TestDefaultRecallDebtWeight(t *testing.T) {
	cfg := Default()
	if cfg.Recall.DebtWeight != 0.6 {
		t.Errorf("Default().Recall.DebtWeight = %v, want 0.6", cfg.Recall.DebtWeight)
	}
	if err := cfg.Recall.Validate(); err != nil {
		t.Errorf("Default().Recall.Validate() = %v, дефолт должен быть валиден", err)
	}
}

// TestRecallConfigValidate покрывает RecallConfig.Validate: 0 (поле не задано в
// TOML) валиден и трактуется отдельно от Load/recall.New как "используй дефолт",
// значения внутри (0, 1] валидны, вне диапазона — ошибка.
func TestRecallConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		weight  float64
		wantErr bool
	}{
		{"0 — не задано, валидно", 0, false},
		{"0.6 — в диапазоне", 0.6, false},
		{"1 — верхняя граница включена", 1, false},
		{"1.5 — вне диапазона", 1.5, true},
		{"-0.1 — отрицательное", -0.1, true},
		{"NaN — отвергается", math.NaN(), true},
		{"+Inf — отвергается", math.Inf(1), true},
		{"-Inf — отвергается", math.Inf(-1), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := (RecallConfig{DebtWeight: tc.weight}).Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestLoadRecallDebtWeight покрывает recall.debt_weight на уровне Load (TOML ->
// Load -> assert): явное значение читается как есть, поле, отсутствующее в TOML,
// оставляет дефолт DefaultDebtWeight нетронутым, а значение вне (0, 1] (включая
// NaN — баг из code review: TOML 1.0/BurntSushi/toml v1.6.0 допускает литерал
// "nan" для float-полей, а обычные сравнения ==, <, > всегда дают false для NaN,
// так что старая форма проверки RecallConfig.Validate пропускала бы его мимо
// обеих веток) фейлит загрузку конфига целиком — явная опечатка, а не
// молчаливая деградация.
func TestLoadRecallDebtWeight(t *testing.T) {
	tests := []struct {
		name     string
		tomlBody string
		wantErr  bool
		want     float64
	}{
		{"явное значение читается", "[recall]\ndebt_weight = 0.3\n", false, 0.3},
		// Литерал 0.6, а не константа DefaultDebtWeight — тот же довод, что и в
		// TestDefaultRecallDebtWeight: иначе кейс сравнивал бы значение само с собой.
		{"поле не задано — остаётся дефолт", "[recall]\ndefault_token_budget = 500\n", false, 0.6},
		{"NaN — ошибка", "[recall]\ndebt_weight = nan\n", true, 0},
		{"вне (0, 1] — ошибка", "[recall]\ndebt_weight = 6\n", true, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")
			if err := os.WriteFile(path, []byte(tc.tomlBody), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			cfg, err := Load(path)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Load() err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if cfg.Recall.DebtWeight != tc.want {
				t.Errorf("Recall.DebtWeight = %v, want %v", cfg.Recall.DebtWeight, tc.want)
			}
		})
	}
}

func TestLoadScopeProjectOnlyKeepsDefaultShared(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	toml := `
[scope]
project = "agentmemory"
`
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Scope.Project != "agentmemory" {
		t.Errorf("Scope.Project = %q, want %q", cfg.Scope.Project, "agentmemory")
	}
	want := []string{"user:*"}
	if !reflect.DeepEqual(cfg.Scope.Shared, want) {
		t.Errorf("Scope.Shared = %v, want дефолт %v (shared не указан в TOML)", cfg.Scope.Shared, want)
	}
}

// -config может быть каталогом проекта: тогда читается <dir>/agentmemory.toml.
func TestLoadFromDirectoryReadsDefaultFileName(t *testing.T) {
	dir := t.TempDir()
	toml := `
[scope]
project = "fromdir"
`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(toml), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load(dir): %v", err)
	}
	if cfg.Scope.Project != "fromdir" {
		t.Errorf("Scope.Project = %q, want %q", cfg.Scope.Project, "fromdir")
	}
}

// Явно названный -config обязан существовать: каталог без agentmemory.toml,
// несуществующий путь и отсутствующий файл — ошибка с абсолютным путём, а не
// тихие дефолты (иначе опечатка в пути незаметно переключает сессию на
// ~/.agentmemory/memory.db). Дефолты — только при пустом path.
func TestLoadExplicitPathMustExist(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct{ name, path, wantInErr string }{
		{"dir without config", dir, "в каталоге " + dir + " нет " + FileName},
		{"nonexistent path", filepath.Join(dir, "nope"), filepath.Join(dir, "nope")},
		{"missing file", filepath.Join(dir, "x.toml"), filepath.Join(dir, "x.toml")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(tc.path)
			if err == nil {
				t.Fatalf("Load(%q) = nil, want ошибка", tc.path)
			}
			if !strings.Contains(err.Error(), tc.wantInErr) {
				t.Errorf("ошибка %q не содержит %q", err, tc.wantInErr)
			}
		})
	}
	if _, err := Load(""); err != nil {
		t.Errorf("Load(\"\") = %v, want дефолты без ошибки", err)
	}
}

// Относительный store.path из конфига, загруженного по каталогу, разрешается
// от этого каталога, а не от cwd процесса; абсолютный и ~-путь не трогаются.
func TestLoadFromDirectoryResolvesRelativeStorePath(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		wantJoined bool
	}{
		{"relative", "./memory.db", true},
		{"bare", "memory.db", true},
		{"absolute", "/var/lib/memo/memory.db", false},
		{"home", "~/.agentmemory/memory.db", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			toml := "[store]\npath = \"" + tc.path + "\"\n"
			if err := os.WriteFile(filepath.Join(dir, FileName), []byte(toml), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			cfg, err := Load(dir)
			if err != nil {
				t.Fatalf("Load(dir): %v", err)
			}
			want := tc.path
			if tc.wantJoined {
				want = filepath.Join(dir, tc.path)
			}
			if cfg.Store.Path != want {
				t.Errorf("Store.Path = %q, want %q", cfg.Store.Path, want)
			}
		})
	}
}

// Путь к ФАЙЛУ работает как раньше: относительный store.path остаётся как есть
// (его разрешает потребитель от cwd — контракт не менялся).
func TestLoadFromFileKeepsRelativeStorePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[store]\npath = \"./memory.db\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Store.Path != "./memory.db" {
		t.Errorf("Store.Path = %q, want %q", cfg.Store.Path, "./memory.db")
	}
}

// FilePath — единая точка ответа «какой файл конфига стоит за -config»:
// для каталога — <dir>/agentmemory.toml, для файла и пустой строки — как есть.
func TestFilePath(t *testing.T) {
	dir := t.TempDir()
	if got, want := FilePath(dir), filepath.Join(dir, FileName); got != want {
		t.Errorf("FilePath(dir) = %q, want %q", got, want)
	}
	file := filepath.Join(dir, "x.toml")
	if got := FilePath(file); got != file {
		t.Errorf("FilePath(file) = %q, want %q", got, file)
	}
	if got := FilePath(""); got != "" {
		t.Errorf("FilePath(\"\") = %q, want пусто", got)
	}
}
