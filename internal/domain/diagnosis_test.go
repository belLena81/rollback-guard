package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
)

func TestDiagnosisStrategyValues(t *testing.T) {
	tests := []struct {
		strategy domain.DiagnosisStrategy
		name     string
	}{
		{strategy: domain.StrategyRules, name: "rules"},
		{strategy: domain.StrategyLLM, name: "llm"},
		{strategy: domain.StrategyHybrid, name: "hybrid"},
	}
	for _, test := range tests {
		if !test.strategy.Valid() || test.strategy.String() != test.name {
			t.Fatalf("strategy %d = %q, valid %t", test.strategy, test.strategy.String(), test.strategy.Valid())
		}
	}
	if domain.DiagnosisStrategy(99).Valid() || domain.DiagnosisStrategy(99).String() != "unknown" {
		t.Fatal("unknown strategy accepted")
	}
}

func TestInvestigationConclusionPreservesModelProvenance(t *testing.T) {
	now := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	provenance := domain.DiagnosisProvenance{
		RequestedStrategy: domain.StrategyHybrid,
		Model:             "test-model",
		PromptVersion:     "rollback-diagnosis-v1",
		InputTokens:       120,
		OutputTokens:      24,
	}
	conclusion, err := domain.NewInvestigationConclusionWithProvenance(
		"conclusion", "incident", domain.StrategyHybrid, "rollout regression", .91,
		[]domain.EvidenceID{"deployment", "metric"}, domain.RecommendRollback, provenance, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if conclusion.Provenance() != provenance {
		t.Fatalf("provenance = %#v, want %#v", conclusion.Provenance(), provenance)
	}
	rehydrated, err := domain.RehydrateInvestigationConclusion(conclusion.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if rehydrated.Provenance() != provenance {
		t.Fatalf("rehydrated provenance = %#v, want %#v", rehydrated.Provenance(), provenance)
	}
}

func TestInvestigationConclusionValidatesProvenance(t *testing.T) {
	now := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	model := domain.DiagnosisProvenance{
		RequestedStrategy: domain.StrategyLLM,
		Model:             "test-model",
		PromptVersion:     "rollback-diagnosis-v1",
	}
	tests := []struct {
		name       string
		effective  domain.DiagnosisStrategy
		provenance domain.DiagnosisProvenance
	}{
		{name: "model strategy requires metadata", effective: domain.StrategyLLM, provenance: domain.DiagnosisProvenance{RequestedStrategy: domain.StrategyLLM}},
		{name: "successful model cannot have fallback", effective: domain.StrategyLLM, provenance: domain.DiagnosisProvenance{RequestedStrategy: domain.StrategyLLM, Model: "test-model", PromptVersion: "rollback-diagnosis-v1", FallbackReason: domain.FallbackProviderError}},
		{name: "fallback must be effectively rules", effective: domain.StrategyHybrid, provenance: domain.DiagnosisProvenance{RequestedStrategy: domain.StrategyHybrid, Model: "test-model", PromptVersion: "rollback-diagnosis-v1", FallbackReason: domain.FallbackInvalidOutput}},
		{name: "fallback requires requested model strategy", effective: domain.StrategyRules, provenance: domain.DiagnosisProvenance{RequestedStrategy: domain.StrategyRules, FallbackReason: domain.FallbackProviderError}},
		{name: "negative usage", effective: domain.StrategyLLM, provenance: domain.DiagnosisProvenance{RequestedStrategy: domain.StrategyLLM, Model: "test-model", PromptVersion: "rollback-diagnosis-v1", InputTokens: -1}},
		{name: "blank model", effective: domain.StrategyLLM, provenance: domain.DiagnosisProvenance{RequestedStrategy: domain.StrategyLLM, Model: " ", PromptVersion: "rollback-diagnosis-v1"}},
		{name: "blank prompt version", effective: domain.StrategyRules, provenance: domain.DiagnosisProvenance{RequestedStrategy: domain.StrategyLLM, Model: "test-model", PromptVersion: "\t", FallbackReason: domain.FallbackProviderError}},
		{name: "mismatched successful strategy", effective: domain.StrategyHybrid, provenance: model},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := domain.NewInvestigationConclusionWithProvenance(
				"conclusion", "incident", test.effective, "hypothesis", .9,
				[]domain.EvidenceID{"evidence"}, domain.RecommendRollback, test.provenance, now,
			)
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("error = %v, want validation", err)
			}
		})
	}
}

func TestLegacyRuleConclusionGetsRuleProvenance(t *testing.T) {
	conclusion, err := domain.NewInvestigationConclusion(
		"conclusion", "incident", domain.StrategyRules, "hypothesis", .9,
		[]domain.EvidenceID{"evidence"}, domain.RecommendRollback, time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if conclusion.Provenance() != (domain.DiagnosisProvenance{RequestedStrategy: domain.StrategyRules}) {
		t.Fatalf("provenance = %#v", conclusion.Provenance())
	}
}
