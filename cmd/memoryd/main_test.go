package main

import (
	"fmt"
	"testing"

	"agentmemory/internal/config"
	"agentmemory/internal/memory/reason"
)

func TestNewReasoner(t *testing.T) {
	tests := []struct {
		name    string
		cfg     config.ReasonerConfig
		apiKey  string
		wantT   string // "%T" ожидаемого типа Reasoner
		wantErr bool
	}{
		{
			name:  "noop",
			cfg:   config.ReasonerConfig{Provider: "noop", Model: "deepseek-chat"},
			wantT: "*reason.Noop",
		},
		{
			name:   "deepseek без ключа деградирует до noop",
			cfg:    config.ReasonerConfig{Provider: "deepseek", Model: "deepseek-chat"},
			apiKey: "",
			wantT:  "*reason.Noop",
		},
		{
			name:   "deepseek с ключом",
			cfg:    config.ReasonerConfig{Provider: "deepseek", Model: "deepseek-chat"},
			apiKey: "sk-test",
			wantT:  "*reason.DeepSeek",
		},
		{
			name:   "deepseek с ключом, но пустой model деградирует до noop",
			cfg:    config.ReasonerConfig{Provider: "deepseek", Model: ""},
			apiKey: "sk-test",
			wantT:  "*reason.Noop",
		},
		{
			name:    "неизвестный provider — ошибка",
			cfg:     config.ReasonerConfig{Provider: "openai", Model: "gpt-4"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(deepseekAPIKeyEnv, tt.apiKey)

			got, err := newReasoner(tt.cfg)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("newReasoner(%+v) = %v, nil; want error", tt.cfg, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("newReasoner(%+v) вернул ошибку: %v", tt.cfg, err)
			}
			if gotT := fmt.Sprintf("%T", got); gotT != tt.wantT {
				t.Fatalf("newReasoner(%+v) вернул тип %s, хотим %s", tt.cfg, gotT, tt.wantT)
			}
			var _ reason.Reasoner = got
		})
	}
}
