// Package config загружает конфигурацию AgentMemory из TOML-файла с дефолтами,
// применяемыми при отсутствии файла или отдельных полей (DESIGN.md §13).
package config

import (
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/BurntSushi/toml"
)

// StoreConfig — настройки хранилища. Driver выбирает бэкенд (DESIGN.md §13):
// "" и "sqlite" (дефолт) — файл SQLite по Path; "postgres" — PostgreSQL+pgvector
// по Host/Port/User/DBName/SSLMode. Password для postgres в TOML никогда не
// пишется (toml:"-") — подхватывается из env MEMO_POSTGRES_PASSWORD (или .env
// рядом с конфигом), тем же способом, что DEEPSEEK_API_KEY/OPENAI_API_KEY
// (см. cmd/memo).
type StoreConfig struct {
	Driver   string `toml:"driver"`  // "" | "sqlite" (дефолт) | "postgres"
	Path     string `toml:"path"`    // sqlite: путь к файлу БД (~ раскрывается)
	Host     string `toml:"host"`    // postgres: адрес сервера
	Port     int    `toml:"port"`    // postgres: порт, дефолт 5432
	User     string `toml:"user"`    // postgres: роль
	DBName   string `toml:"dbname"`  // postgres: имя базы
	SSLMode  string `toml:"sslmode"` // postgres: режим TLS ("disable", "require", ...), дефолт "disable"
	Password string `toml:"-"`       // НИКОГДА из TOML — только из env MEMO_POSTGRES_PASSWORD
}

// Validate проверяет StoreConfig на уровне конфигурации (не сети): Driver —
// одно из известных значений, а для postgres — что поля, необходимые для
// установления соединения (host/user/dbname), заданы. Password намеренно НЕ
// проверяется здесь: пустой пароль для postgres — не ошибка конфигурации
// (сервер может использовать trust-аутентификацию) — попытка подключения всё
// равно делается, а неудача — дело store.Open, а не Load (деградация, а не
// падение на этапе загрузки конфига, DESIGN.md §15).
func (c StoreConfig) Validate() error {
	switch c.Driver {
	case "", "sqlite":
		return nil
	case "postgres":
		if c.Host == "" || c.User == "" || c.DBName == "" {
			return fmt.Errorf("store.driver=postgres требует непустых store.host, store.user и store.dbname")
		}
		return nil
	default:
		return fmt.Errorf("store.driver %q неизвестен (допустимо: \"sqlite\", \"postgres\")", c.Driver)
	}
}

// DSN собирает connection string PostgreSQL в формате, который понимают
// pgx/lib-pq: postgres://user:pass@host:port/dbname?sslmode=... Пользователь и
// пароль URL-экранируются (net/url.Userinfo) — спецсимволы в пароле (@, :, /,
// пробелы и т.п.) не ломают разбор DSN драйвером. Пустой Password даёт DSN без
// пароля (userinfo без ":"), а не "user:" — это то, чего ожидают драйверы при
// trust-аутентификации. Имеет смысл только при Driver == "postgres".
func (c StoreConfig) DSN() string {
	userinfo := url.User(c.User)
	if c.Password != "" {
		userinfo = url.UserPassword(c.User, c.Password)
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     userinfo,
		Host:     fmt.Sprintf("%s:%d", c.Host, c.Port),
		Path:     "/" + c.DBName,
		RawQuery: url.Values{"sslmode": {c.SSLMode}}.Encode(),
	}
	return u.String()
}

// Target возвращает короткое человекочитаемое описание хранилища для
// логов/сообщений об ошибках (без пароля): путь к файлу для sqlite,
// host:port/dbname для postgres.
func (c StoreConfig) Target() string {
	switch c.Driver {
	case "postgres":
		return fmt.Sprintf("postgres://%s:%d/%s", c.Host, c.Port, c.DBName)
	default: // "", "sqlite"
		return c.Path
	}
}

// EmbedderConfig — настройки Embedder. provider="noop" отключает векторный слой (Этап 1).
type EmbedderConfig struct {
	Provider string `toml:"provider"` // noop | ollama | openai
	BaseURL  string `toml:"base_url"`
	Model    string `toml:"model"`
}

// ReasonerConfig — настройки Reasoner. Ключ провайдера читается из env, не отсюда.
type ReasonerConfig struct {
	Provider string `toml:"provider"` // noop | deepseek
	Model    string `toml:"model"`
}

// RecallWeights — веса бизнес-сигналов финального ранжирования (DESIGN.md §8).
type RecallWeights struct {
	Relevance  float64 `toml:"relevance"`
	Importance float64 `toml:"importance"`
	Recency    float64 `toml:"recency"`
	Access     float64 `toml:"access"`
}

// DefaultDebtWeight — дефолт понижающего множителя итогового скора записей
// kind=debt (техдолг, DESIGN.md §4/§8), используемый и Default() (когда TOML
// не переопределяет recall.debt_weight), и recall.New (когда
// config.RecallConfig.DebtWeight не задан или вне допустимого диапазона (0,
// 1] — см. RecallConfig.DebtWeight, RecallConfig.Validate) — одно число,
// общее для обоих мест, вместо двух независимо согласованных констант.
const DefaultDebtWeight = 0.6

// RecallConfig — настройки сборки recall-брифа.
type RecallConfig struct {
	DefaultTokenBudget int           `toml:"default_token_budget"`
	Weights            RecallWeights `toml:"weights"`
	// DebtWeight — понижающий множитель итогового скора записей kind=debt
	// (техдолг, DESIGN.md §4/§8): применяется в recall.Recaller.rank поверх
	// обычного final-score, чтобы накопленные долги не вытесняли факты из
	// бюджета токенов. 0 — единственное значение вне (0, 1], которое трактуется
	// как "не задано": отсутствующее в TOML поле и явное debt_weight = 0
	// неразличимы (BurntSushi/toml даёт для обоих нулевое float64) — обоим
	// откат на дефолт DefaultDebtWeight делает recall.New, а не ошибка. Любое
	// ДРУГОЕ значение вне (0, 1] (отрицательное, больше 1, NaN/±Inf — TOML 1.0
	// допускает такие литералы) — опечатка конфигурации, отвергается
	// Validate/Load (см. годок Validate).
	DebtWeight float64 `toml:"debt_weight"`
}

// Validate проверяет RecallConfig на уровне конфигурации. На сегодня
// покрывает только DebtWeight (имя метода не подразумевает, что остальные
// поля тоже провалидированы — Weights/DefaultTokenBudget валидации пока не
// требуют): значение — либо 0 ("не задано в TOML", откат на
// DefaultDebtWeight делает recall.New, а не Validate), либо строго в
// диапазоне (0, 1] — это множитель понижения скора, а не произвольное число.
// Форма условия "!(x > 0 && x <= 1)", а не "x <= 0 || x > 1", отвергает и
// NaN: обе стороны исходного условия ложны для NaN (любое сравнение с NaN —
// false), так что naive-форма пропускала бы NaN мимо обеих веток, а TOML 1.0
// (BurntSushi/toml) допускает литерал "nan". Значение вне диапазона — явная
// опечатка конфигурации (напр. "6" вместо "0.6"), fail-fast, а не молчаливая
// деградация (тот же принцип, что у MaintConfig.Durations).
func (r RecallConfig) Validate() error {
	if r.DebtWeight == 0 {
		return nil
	}
	if !(r.DebtWeight > 0 && r.DebtWeight <= 1) {
		return fmt.Errorf("recall.debt_weight %v должен быть в диапазоне (0, 1] (0 — использовать дефолт %v)", r.DebtWeight, DefaultDebtWeight)
	}
	return nil
}

// MaintConfig — настройки фоновых джобов обслуживания памяти (DESIGN.md §11).
type MaintConfig struct {
	DecayInterval    string `toml:"decay_interval"`    // напр. "24h"
	ConsolidateAfter string `toml:"consolidate_after"` // напр. "168h"
}

// Durations парсит DecayInterval/ConsolidateAfter в time.Duration
// (time.ParseDuration). Невалидная строка, а также непозитивная длительность
// (<= 0 — time.ParseDuration успешно разбирает и "0s", и отрицательные значения
// вроде "-1h") в любом из полей — ошибка с указанием, какое поле не годится;
// вызывающий код (Load, Этап 4.3) должен считать это ошибкой конфигурации, а не
// деградацией. Непозитивный decay_interval в частности недопустим за пределами
// этого метода: Scheduler.Run передаёт его в time.NewTicker, который паникует
// при d<=0.
func (m MaintConfig) Durations() (decayInterval, consolidateAfter time.Duration, err error) {
	decayInterval, err = time.ParseDuration(m.DecayInterval)
	if err != nil {
		return 0, 0, fmt.Errorf("maint.decay_interval %q: %w", m.DecayInterval, err)
	}
	if decayInterval <= 0 {
		return 0, 0, fmt.Errorf("maint.decay_interval %q должен быть положительным", m.DecayInterval)
	}
	consolidateAfter, err = time.ParseDuration(m.ConsolidateAfter)
	if err != nil {
		return 0, 0, fmt.Errorf("maint.consolidate_after %q: %w", m.ConsolidateAfter, err)
	}
	if consolidateAfter <= 0 {
		return 0, 0, fmt.Errorf("maint.consolidate_after %q должен быть положительным", m.ConsolidateAfter)
	}
	return decayInterval, consolidateAfter, nil
}

// ScopeConfig — идентичность инстанса и мультипроектность (DESIGN.md §2.1).
// Project пуст => мультипроектность выключена (одно-проектный/adhoc-режим): save
// требует явный scope, recall без scope не фильтрует.
type ScopeConfig struct {
	Project string   `toml:"project"` // имя проекта -> "домашнее" пространство "project:<name>"
	Shared  []string `toml:"shared"`  // пространства, всегда добавляемые к recall по умолчанию
}

// SaveScope возвращает "домашний" scope инстанса для save по умолчанию
// ("project:<name>"), либо "", если мультипроектность выключена.
func (c ScopeConfig) SaveScope() string {
	if c.Project == "" {
		return ""
	}
	return "project:" + c.Project
}

// RecallScopes возвращает набор scope для recall по умолчанию
// (["project:<name>"] + Shared), либо nil, если мультипроектность выключена.
func (c ScopeConfig) RecallScopes() []string {
	if c.Project == "" {
		return nil
	}
	return append([]string{"project:" + c.Project}, c.Shared...)
}

// Config — корневая структура конфигурации AgentMemory.
type Config struct {
	Store    StoreConfig    `toml:"store"`
	Embedder EmbedderConfig `toml:"embedder"`
	Reasoner ReasonerConfig `toml:"reasoner"`
	Recall   RecallConfig   `toml:"recall"`
	Maint    MaintConfig    `toml:"maint"`
	Scope    ScopeConfig    `toml:"scope"`
}

// Default возвращает конфигурацию по умолчанию (DESIGN.md §13), используемую когда
// файл конфига отсутствует или не переопределяет то или иное поле.
func Default() Config {
	return Config{
		Store: StoreConfig{
			Driver:  "sqlite",
			Path:    "~/.agentmemory/memory.db",
			Port:    5432,      // дефолт postgres-порта; неприменим при driver=sqlite
			SSLMode: "disable", // дефолт postgres-sslmode; неприменим при driver=sqlite
		},
		Embedder: EmbedderConfig{
			Provider: "noop", // Этап 1: без эмбеддингов
			BaseURL:  "",     // дефолт base_url провайдер-специфичен (ollama vs openai) — подставляется в cmd/memo.newEmbedder, не здесь
			Model:    "bge-m3",
		},
		Reasoner: ReasonerConfig{
			Provider: "noop", // Этап 1: без Reasoner
			Model:    "deepseek-chat",
		},
		Recall: RecallConfig{
			DefaultTokenBudget: 800,
			Weights: RecallWeights{
				Relevance:  1.0,
				Importance: 0.5,
				Recency:    0.4,
				Access:     0.2,
			},
			DebtWeight: DefaultDebtWeight,
		},
		Maint: MaintConfig{
			DecayInterval:    "24h",
			ConsolidateAfter: "168h",
		},
		Scope: ScopeConfig{
			Project: "", // мультипроектность выключена по умолчанию
			Shared:  []string{"user:*"},
		},
	}
}

// Load читает TOML-конфиг из path и накладывает его поверх дефолтов. Если файл не
// существует, возвращает чистые дефолты без ошибки — конфиг работает и без файла.
func Load(path string) (Config, error) {
	cfg := Default()

	if path == "" {
		return cfg, nil
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		return cfg, nil
	}

	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return Config{}, fmt.Errorf("разбор конфига %s: %w", path, err)
	}

	if _, _, err := cfg.Maint.Durations(); err != nil {
		return Config{}, fmt.Errorf("разбор конфига %s: %w", path, err)
	}

	if err := cfg.Store.Validate(); err != nil {
		return Config{}, fmt.Errorf("разбор конфига %s: %w", path, err)
	}

	if err := cfg.Recall.Validate(); err != nil {
		return Config{}, fmt.Errorf("разбор конфига %s: %w", path, err)
	}

	return cfg, nil
}
