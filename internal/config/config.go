// Package config загружает конфигурацию AgentMemory из TOML-файла с дефолтами,
// применяемыми при отсутствии файла или отдельных полей (DESIGN.md §13).
package config

import (
	"fmt"
	"os"
	"time"

	"github.com/BurntSushi/toml"
)

// StoreConfig — настройки хранилища.
type StoreConfig struct {
	Path string `toml:"path"`
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

// RecallConfig — настройки сборки recall-брифа.
type RecallConfig struct {
	DefaultTokenBudget int           `toml:"default_token_budget"`
	Weights            RecallWeights `toml:"weights"`
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
			Path: "~/.agentmemory/memory.db",
		},
		Embedder: EmbedderConfig{
			Provider: "noop", // Этап 1: без эмбеддингов
			BaseURL:  "",     // дефолт base_url провайдер-специфичен (ollama vs openai) — подставляется в cmd/memoryd.newEmbedder, не здесь
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

	return cfg, nil
}
