// Package config загружает конфигурацию AgentMemory из TOML-файла с дефолтами,
// применяемыми при отсутствии файла или отдельных полей (DESIGN.md §13).
package config

import (
	"fmt"
	"os"

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

// Config — корневая структура конфигурации AgentMemory.
type Config struct {
	Store    StoreConfig    `toml:"store"`
	Embedder EmbedderConfig `toml:"embedder"`
	Reasoner ReasonerConfig `toml:"reasoner"`
	Recall   RecallConfig   `toml:"recall"`
	Maint    MaintConfig    `toml:"maint"`
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
			BaseURL:  "http://localhost:11434",
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

	return cfg, nil
}
