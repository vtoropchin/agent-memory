package config

import (
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
