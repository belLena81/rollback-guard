package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/config"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/belLena81/rollback-guard/internal/testkit"
)

// A failing provider opens the LLM breaker, and the rule diagnoser keeps
// answering throughout: an open breaker is just another unavailable provider.
func TestLLMBreakerLeavesRuleFallbackInCharge(t *testing.T) {
	var hits atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "overloaded", http.StatusServiceUnavailable)
	}))
	t.Cleanup(provider.Close)

	resilienceCfg := config.DefaultResilience()
	resilienceCfg.LLMRetryMaxAttempts = 2
	resilienceCfg.RetryBaseDelay = time.Millisecond
	resilienceCfg.RetryMaxDelay = 2 * time.Millisecond
	resilienceCfg.BreakerFailureThreshold = 2
	cfg := config.InvestigatorConfig{
		DiagnosisStrategy: domain.StrategyLLM, RuleCorrelationWindow: 10 * time.Minute, Resilience: resilienceCfg,
		LLMBaseURL: provider.URL + "/v1", LLMModel: "model", LLMAPIKey: "key", LLMTimeout: 5 * time.Second, LLMMaxOutputTokens: 512,
	}
	diagnoser, err := newDiagnoser(cfg, &testkit.IDGenerator{}, &http.Client{Timeout: 5 * time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	input := diagnosisInput(t)
	for call := 1; call <= 3; call++ {
		conclusion, err := diagnoser.Diagnose(t.Context(), input)
		if err != nil {
			t.Fatalf("call %d: %v", call, err)
		}
		provenance := conclusion.Provenance()
		if conclusion.Strategy() != domain.StrategyRules || provenance.FallbackReason != domain.FallbackProviderError {
			t.Fatalf("call %d: strategy=%v fallback=%v, want the rule diagnoser after a provider error", call, conclusion.Strategy(), provenance.FallbackReason)
		}
	}
	// Two attempts opened the breaker; the next two diagnoses never reached
	// the provider.
	if got := hits.Load(); got != 2 {
		t.Fatalf("provider hits = %d, want 2 (retry once, then the open breaker blocks calls)", got)
	}
}

func diagnosisInput(t *testing.T) ports.InvestigationInput {
	t.Helper()
	now := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	target, err := domain.NewDeploymentTarget("local", "rollback-demo", "demo-api", "demo-api")
	if err != nil {
		t.Fatal(err)
	}
	symptom, err := domain.NewSymptom("http_error_rate", .35, .10, "ratio", now)
	if err != nil {
		t.Fatal(err)
	}
	incident, err := domain.NewIncident("incident", target, symptom, now)
	if err != nil {
		t.Fatal(err)
	}
	deployment := domain.DeploymentEvidence{Target: target, CurrentRevision: "revision-2", PreviousStableRevision: "revision-1", DesiredReplicas: 3, ReadyReplicas: 1, AvailableReplicas: 1, RolloutAt: now.Add(-time.Minute)}
	metric := domain.MetricEvidence{Name: "http_error_rate", Value: .35, Threshold: .10, Unit: "ratio", ObservedAt: now}
	deploymentItem, err := domain.NewDeploymentEvidence("deployment", incident.ID(), deployment, now)
	if err != nil {
		t.Fatal(err)
	}
	metricItem, err := domain.NewMetricEvidence("metric", incident.ID(), metric, now)
	if err != nil {
		t.Fatal(err)
	}
	return ports.InvestigationInput{Incident: incident, Deployment: deployment, Metric: metric, Evidence: []domain.Evidence{deploymentItem, metricItem}, Now: now}
}
