package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/diagnosis"
	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/config"
	"github.com/belLena81/rollback-guard/internal/testkit"
)

func TestNewDiagnoserSelectsConfiguredStrategy(t *testing.T) {
	tests := []struct {
		name     string
		strategy domain.DiagnosisStrategy
		assert   func(t *testing.T, value any)
	}{
		{name: "rules", strategy: domain.StrategyRules, assert: func(t *testing.T, value any) {
			if _, ok := value.(*diagnosis.RuleDiagnoser); !ok {
				t.Fatalf("diagnoser = %T", value)
			}
		}},
		{name: "llm", strategy: domain.StrategyLLM, assert: func(t *testing.T, value any) {
			if _, ok := value.(*application.LLMDiagnoser); !ok {
				t.Fatalf("diagnoser = %T", value)
			}
		}},
		{name: "hybrid", strategy: domain.StrategyHybrid, assert: func(t *testing.T, value any) {
			if _, ok := value.(*application.HybridDiagnoser); !ok {
				t.Fatalf("diagnoser = %T", value)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.InvestigatorConfig{
				DiagnosisStrategy: test.strategy, RuleCorrelationWindow: 10 * time.Minute,
			}
			var client *http.Client
			if test.strategy != domain.StrategyRules {
				cfg.LLMBaseURL = "https://provider.example/v1"
				cfg.LLMModel = "model"
				cfg.LLMAPIKey = "key"
				cfg.LLMTimeout = time.Second
				cfg.LLMMaxOutputTokens = 512
				client = &http.Client{}
			}
			diagnoser, err := newDiagnoser(cfg, &testkit.IDGenerator{}, client)
			if err != nil {
				t.Fatal(err)
			}
			test.assert(t, diagnoser)
		})
	}
}
