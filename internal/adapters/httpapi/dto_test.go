package httpapi

import (
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
)

func TestConclusionResponseExposesOnlySanitizedProvenance(t *testing.T) {
	provenance := domain.DiagnosisProvenance{
		RequestedStrategy: domain.StrategyHybrid,
		Model:             "test-model",
		PromptVersion:     "rollback-diagnosis-v1",
		InputTokens:       40,
		OutputTokens:      12,
		FallbackReason:    domain.FallbackProviderRefusal,
	}
	conclusion, err := domain.NewInvestigationConclusionWithProvenance(
		"conclusion", "incident", domain.StrategyRules, "rules fallback", .9,
		[]domain.EvidenceID{"evidence"}, domain.RecommendRollback, provenance, time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	response := conclusionResponse(conclusion)
	if response.Strategy != "rules" || response.Provenance.RequestedStrategy != "hybrid" || response.Provenance.FallbackReason != "provider_refusal" {
		t.Fatalf("response = %#v", response)
	}
	if response.Provenance.Model != "test-model" || response.Provenance.InputTokens != 40 || response.Provenance.OutputTokens != 12 {
		t.Fatalf("provenance = %#v", response.Provenance)
	}
}
