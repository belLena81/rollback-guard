package application

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/belLena81/rollback-guard/internal/testkit"
)

func TestLLMDiagnoserReturnsValidatedModelConclusion(t *testing.T) {
	input := aiDiagnosisInput(t)
	reasoner := &scriptedReasoner{result: validReasoningResult(input)}
	fallback := &scriptedDiagnoser{conclusion: ruleConclusion(t, input)}
	diagnoser := newTestLLMDiagnoser(t, reasoner, fallback)

	conclusion, err := diagnoser.Diagnose(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if conclusion.Strategy() != domain.StrategyLLM || conclusion.Provenance().RequestedStrategy != domain.StrategyLLM {
		t.Fatalf("strategy/provenance = %s/%#v", conclusion.Strategy(), conclusion.Provenance())
	}
	if conclusion.Provenance().Model != "test-model" || conclusion.Provenance().InputTokens != 40 || fallback.calls != 0 || reasoner.calls != 1 {
		t.Fatalf("conclusion/fallback/reasoner = %#v/%d/%d", conclusion.Snapshot(), fallback.calls, reasoner.calls)
	}
	if reasoner.request.RuleCandidate != nil || len(reasoner.request.Evidence) != len(input.Evidence) {
		t.Fatalf("reasoning request = %#v", reasoner.request)
	}
}

func TestLLMDiagnoserFallsBackForInvalidOutput(t *testing.T) {
	input := aiDiagnosisInput(t)
	tests := []struct {
		name   string
		mutate func(*ports.ReasoningResult)
	}{
		{name: "blank hypothesis", mutate: func(result *ports.ReasoningResult) { result.Candidate.Hypothesis = " " }},
		{name: "non finite confidence", mutate: func(result *ports.ReasoningResult) { result.Candidate.Confidence = math.NaN() }},
		{name: "unknown recommendation", mutate: func(result *ports.ReasoningResult) { result.Candidate.Recommendation = "delete" }},
		{name: "duplicate evidence", mutate: func(result *ports.ReasoningResult) {
			result.Candidate.EvidenceIDs = []domain.EvidenceID{input.Evidence[0].ID(), input.Evidence[0].ID()}
		}},
		{name: "unknown evidence", mutate: func(result *ports.ReasoningResult) { result.Candidate.EvidenceIDs = []domain.EvidenceID{"missing"} }},
		{name: "negative usage", mutate: func(result *ports.ReasoningResult) { result.Usage.InputTokens = -1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := validReasoningResult(input)
			test.mutate(&result)
			reasoner := &scriptedReasoner{result: result}
			rules := ruleConclusion(t, input)
			fallback := &scriptedDiagnoser{conclusion: rules}
			diagnoser := newTestLLMDiagnoser(t, reasoner, fallback)

			conclusion, err := diagnoser.Diagnose(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			if conclusion.Strategy() != domain.StrategyRules || conclusion.ID() != rules.ID() || fallback.calls != 1 {
				t.Fatalf("fallback = %#v, calls = %d", conclusion.Snapshot(), fallback.calls)
			}
			if conclusion.Provenance().RequestedStrategy != domain.StrategyLLM || conclusion.Provenance().FallbackReason != domain.FallbackInvalidOutput {
				t.Fatalf("provenance = %#v", conclusion.Provenance())
			}
		})
	}
}

func TestLLMDiagnoserClassifiesProviderFallback(t *testing.T) {
	input := aiDiagnosisInput(t)
	tests := []struct {
		name   string
		err    error
		reason domain.DiagnosisFallbackReason
	}{
		{name: "unavailable", err: ports.ErrReasonerUnavailable, reason: domain.FallbackProviderError},
		{name: "refused", err: ports.ErrReasonerRefused, reason: domain.FallbackProviderRefusal},
		{name: "invalid response", err: ports.ErrReasonerInvalidResponse, reason: domain.FallbackInvalidOutput},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fallback := &scriptedDiagnoser{conclusion: ruleConclusion(t, input)}
			diagnoser := newTestLLMDiagnoser(t, &scriptedReasoner{err: test.err}, fallback)
			conclusion, err := diagnoser.Diagnose(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			if conclusion.Provenance().FallbackReason != test.reason || fallback.calls != 1 {
				t.Fatalf("provenance/calls = %#v/%d", conclusion.Provenance(), fallback.calls)
			}
		})
	}
}

func TestLLMDiagnoserPropagatesParentCancellation(t *testing.T) {
	input := aiDiagnosisInput(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fallback := &scriptedDiagnoser{conclusion: ruleConclusion(t, input)}
	diagnoser := newTestLLMDiagnoser(t, &scriptedReasoner{err: context.Canceled}, fallback)
	_, err := diagnoser.Diagnose(ctx, input)
	if !errors.Is(err, context.Canceled) || fallback.calls != 0 {
		t.Fatalf("error/fallback calls = %v/%d", err, fallback.calls)
	}
}

func TestLLMDiagnoserPropagatesCancellationDuringReasoning(t *testing.T) {
	input := aiDiagnosisInput(t)
	entered := make(chan struct{})
	reasoner := reasonerFunc(func(ctx context.Context, _ ports.ReasoningRequest) (ports.ReasoningResult, error) {
		close(entered)
		<-ctx.Done()
		return ports.ReasoningResult{}, ctx.Err()
	})
	fallback := &scriptedDiagnoser{conclusion: ruleConclusion(t, input)}
	diagnoser := newTestLLMDiagnoser(t, reasoner, fallback)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := diagnoser.Diagnose(ctx, input)
		done <- err
	}()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || fallback.calls != 0 {
		t.Fatalf("error/fallback calls = %v/%d", err, fallback.calls)
	}
}

func TestLLMDiagnoserReportsModelAndFallbackFailures(t *testing.T) {
	input := aiDiagnosisInput(t)
	ruleErr := errors.New("rules unavailable")
	fallback := &scriptedDiagnoser{err: ruleErr}
	diagnoser := newTestLLMDiagnoser(t, &scriptedReasoner{err: ports.ErrReasonerUnavailable}, fallback)
	_, err := diagnoser.Diagnose(context.Background(), input)
	if !errors.Is(err, ports.ErrReasonerUnavailable) || !errors.Is(err, ruleErr) {
		t.Fatalf("error = %v, want both model and fallback failures", err)
	}
}

func TestLLMDiagnoserFallsBackAfterLocalReasonerTimeout(t *testing.T) {
	input := aiDiagnosisInput(t)
	fallback := &scriptedDiagnoser{conclusion: ruleConclusion(t, input)}
	reasoner := reasonerFunc(func(ctx context.Context, _ ports.ReasoningRequest) (ports.ReasoningResult, error) {
		<-ctx.Done()
		return ports.ReasoningResult{}, ctx.Err()
	})
	diagnoser, err := NewLLMDiagnoser(reasoner, fallback, &testkit.IDGenerator{}, ModelDiagnoserConfig{
		Model: "configured-model", PromptVersion: DiagnosisPromptVersion, Timeout: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	conclusion, err := diagnoser.Diagnose(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if conclusion.Provenance().FallbackReason != domain.FallbackProviderTimeout || fallback.calls != 1 {
		t.Fatalf("provenance/calls = %#v/%d", conclusion.Provenance(), fallback.calls)
	}
}

func TestHybridDiagnoserReviewsRulesAndReusesThemOnFailure(t *testing.T) {
	input := aiDiagnosisInput(t)
	rules := ruleConclusion(t, input)
	fallback := &scriptedDiagnoser{conclusion: rules}
	reasoner := &scriptedReasoner{err: ports.ErrReasonerUnavailable}
	diagnoser, err := NewHybridDiagnoser(reasoner, fallback, &testkit.IDGenerator{}, ModelDiagnoserConfig{
		Model: "configured-model", PromptVersion: "rollback-diagnosis-v1", Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	conclusion, err := diagnoser.Diagnose(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if fallback.calls != 1 || reasoner.calls != 1 || reasoner.request.RuleCandidate == nil {
		t.Fatalf("rule/reasoner calls = %d/%d, request = %#v", fallback.calls, reasoner.calls, reasoner.request)
	}
	if conclusion.ID() != rules.ID() || conclusion.Provenance().RequestedStrategy != domain.StrategyHybrid || conclusion.Provenance().FallbackReason != domain.FallbackProviderError {
		t.Fatalf("conclusion = %#v", conclusion.Snapshot())
	}
}

func TestHybridDiagnoserReturnsValidatedReview(t *testing.T) {
	input := aiDiagnosisInput(t)
	rules := &scriptedDiagnoser{conclusion: ruleConclusion(t, input)}
	reasoner := &scriptedReasoner{result: validReasoningResult(input)}
	diagnoser, err := NewHybridDiagnoser(reasoner, rules, &testkit.IDGenerator{}, ModelDiagnoserConfig{
		Model: "configured-model", PromptVersion: DiagnosisPromptVersion, Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	conclusion, err := diagnoser.Diagnose(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if conclusion.Strategy() != domain.StrategyHybrid || rules.calls != 1 || reasoner.calls != 1 || reasoner.request.RuleCandidate == nil {
		t.Fatalf("conclusion/calls/request = %#v/%d/%d/%#v", conclusion.Snapshot(), rules.calls, reasoner.calls, reasoner.request)
	}
}

func newTestLLMDiagnoser(t *testing.T, reasoner ports.Reasoner, fallback ports.Diagnoser) *LLMDiagnoser {
	t.Helper()
	diagnoser, err := NewLLMDiagnoser(reasoner, fallback, &testkit.IDGenerator{}, ModelDiagnoserConfig{
		Model: "configured-model", PromptVersion: "rollback-diagnosis-v1", Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return diagnoser
}

func aiDiagnosisInput(t *testing.T) ports.InvestigationInput {
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

func validReasoningResult(input ports.InvestigationInput) ports.ReasoningResult {
	return ports.ReasoningResult{
		Candidate: ports.ReasoningCandidate{
			Hypothesis: "the rollout is correlated with the error-rate regression", Confidence: .93,
			EvidenceIDs: []domain.EvidenceID{input.Evidence[0].ID(), input.Evidence[1].ID()}, Recommendation: "rollback",
		},
		Model: "test-model", Usage: ports.TokenUsage{InputTokens: 40, OutputTokens: 12},
	}
}

func ruleConclusion(t *testing.T, input ports.InvestigationInput) domain.InvestigationConclusion {
	t.Helper()
	conclusion, err := domain.NewInvestigationConclusion("rule-conclusion", input.Incident.ID(), domain.StrategyRules, "rule hypothesis", .95, []domain.EvidenceID{input.Evidence[0].ID(), input.Evidence[1].ID()}, domain.RecommendRollback, input.Now)
	if err != nil {
		t.Fatal(err)
	}
	return conclusion
}

type scriptedReasoner struct {
	result  ports.ReasoningResult
	err     error
	request ports.ReasoningRequest
	calls   int
}

func (r *scriptedReasoner) Reason(_ context.Context, request ports.ReasoningRequest) (ports.ReasoningResult, error) {
	r.calls++
	r.request = request
	return r.result, r.err
}

type scriptedDiagnoser struct {
	conclusion domain.InvestigationConclusion
	err        error
	calls      int
}

func (d *scriptedDiagnoser) Diagnose(context.Context, ports.InvestigationInput) (domain.InvestigationConclusion, error) {
	d.calls++
	return d.conclusion, d.err
}

type reasonerFunc func(context.Context, ports.ReasoningRequest) (ports.ReasoningResult, error)

func (f reasonerFunc) Reason(ctx context.Context, request ports.ReasoningRequest) (ports.ReasoningResult, error) {
	return f(ctx, request)
}
