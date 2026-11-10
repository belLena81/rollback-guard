package telemetry_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/diagnosis"
	"github.com/belLena81/rollback-guard/internal/adapters/fake"
	"github.com/belLena81/rollback-guard/internal/adapters/memory"
	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/application/applicationtest"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/resilience"
	"github.com/belLena81/rollback-guard/internal/platform/telemetry"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/belLena81/rollback-guard/internal/testkit"
)

func newProvider(t *testing.T, opts ...telemetry.Option) *telemetry.Provider {
	t.Helper()
	provider, err := telemetry.New(t.Context(), telemetry.Settings{ServiceName: "test", SampleRatio: 1}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { shutdown(t, provider) })
	return provider
}

// TestMetricCatalogue pins every metric name and its label set, and proves
// no identifier leaks into a label value.
func TestMetricCatalogue(t *testing.T) {
	provider := newProvider(t)
	ctx := ports.WithCorrelation(context.Background(), ports.Correlation{IncidentID: "incident-secret-id", IdempotencyKey: "approval:approval-7", OperationID: "operation-9"})

	route := provider.Route("/v1/incidents/{incidentId}", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) }))
	route.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/incidents/incident-secret-id", nil))
	weird := provider.Route("/v1/incidents", http.NotFoundHandler())
	weird.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("PROPFIND", "/v1/incidents", nil))

	_, finish := provider.ObserveToolCall(ctx, "kubernetes-readonly", "get_deployment")
	finish("ok")
	_, finish = provider.ObserveToolRequest(ctx, http.Header{}, "kubernetes-rollback", "execute_approved_rollback")
	finish("tool_error")

	gateway, err := provider.ObserveGateway(stubGateway{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.Submit(ctx, ports.InvestigationSubmission{}); err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.Watch(ctx, domain.InvestigationTaskRef{TaskID: "task-secret"}, nil); err == nil {
		t.Fatal("expected watch failure")
	}

	diagnoser, err := provider.ObserveDiagnoser(stubDiagnoser{conclusion: fallbackConclusion(t)}, domain.StrategyHybrid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := diagnoser.Diagnose(ctx, ports.InvestigationInput{}); err != nil {
		t.Fatal(err)
	}

	provider.RetryAttempted(resilience.DependencyLLM, resilience.RetryRetried)
	provider.RetryAttempted(resilience.DependencyLLM, resilience.RetryRetried)
	provider.RetryAttempted(resilience.DependencyLLM, resilience.RetryExhausted)
	provider.BreakerStateChanged(resilience.DependencyPrometheus, resilience.StateOpen)
	provider.BreakerStateChanged(resilience.DependencyPrometheus, resilience.StateHalfOpen)
	provider.BreakerStateChanged(resilience.DependencyPrometheus, resilience.StateClosed)

	runLifecycle(t, provider)

	labels, values := rollbackGuardianFamilies(t, provider.Gatherer())
	want := map[string]string{
		"rollback_guardian_http_server_requests_total":           "code,method,route",
		"rollback_guardian_http_server_request_duration_seconds": "code,method,route",
		"rollback_guardian_incidents_total":                      "",
		"rollback_guardian_incidents_terminal_total":             "status",
		"rollback_guardian_approval_decisions_total":             "decision",
		"rollback_guardian_investigation_dispatches_total":       "outcome,reason",
		"rollback_guardian_investigation_duration_seconds":       "outcome",
		"rollback_guardian_a2a_calls_total":                      "operation,outcome",
		"rollback_guardian_a2a_call_duration_seconds":            "operation,outcome",
		"rollback_guardian_mcp_calls_total":                      "outcome,server,tool",
		"rollback_guardian_mcp_call_duration_seconds":            "outcome,server,tool",
		"rollback_guardian_mcp_tool_requests_total":              "outcome,server,tool",
		"rollback_guardian_mcp_tool_request_duration_seconds":    "outcome,server,tool",
		"rollback_guardian_diagnoses_total":                      "effective_strategy,fallback_reason,requested_strategy",
		"rollback_guardian_rollback_executions_total":            "outcome",
		"rollback_guardian_recovery_verifications_total":         "result",
		"rollback_guardian_retries_total":                        "dependency,outcome",
		"rollback_guardian_circuit_breaker_transitions_total":    "dependency,state",
	}
	for name, labelSet := range want {
		got, ok := labels[name]
		if !ok {
			t.Errorf("metric %s was not exposed", name)
			continue
		}
		if got != labelSet {
			t.Errorf("metric %s labels = %q, want %q", name, got, labelSet)
		}
	}
	for name := range labels {
		if _, ok := want[name]; !ok {
			t.Errorf("unexpected metric %s: add it to the catalogue and docs", name)
		}
	}
	for _, value := range values {
		for _, forbidden := range []string{"incident-", "approval:", "operation-", "task-", "PROPFIND"} {
			if strings.Contains(value, forbidden) {
				t.Errorf("label value %q carries an unbounded identifier", value)
			}
		}
	}
	if got := metricValue(t, provider.Gatherer(), "rollback_guardian_http_server_requests_total", map[string]string{"method": "OTHER", "route": "/v1/incidents", "code": "404"}); got != 1 {
		t.Errorf("unknown method series = %v, want bounded OTHER", got)
	}
	if got := metricValue(t, provider.Gatherer(), "rollback_guardian_diagnoses_total", map[string]string{"requested_strategy": "hybrid", "effective_strategy": "rules", "fallback_reason": "provider_timeout"}); got != 1 {
		t.Errorf("diagnosis fallback series = %v", got)
	}
	if got := metricValue(t, provider.Gatherer(), "rollback_guardian_retries_total", map[string]string{"dependency": "llm", "outcome": "retried"}); got != 2 {
		t.Errorf("retried series = %v, want 2", got)
	}
	if got := metricValue(t, provider.Gatherer(), "rollback_guardian_retries_total", map[string]string{"dependency": "llm", "outcome": "exhausted"}); got != 1 {
		t.Errorf("exhausted series = %v, want 1", got)
	}
	for _, state := range []string{"open", "half_open", "closed"} {
		if got := metricValue(t, provider.Gatherer(), "rollback_guardian_circuit_breaker_transitions_total", map[string]string{"dependency": "prometheus", "state": state}); got != 1 {
			t.Errorf("breaker transition to %s = %v, want 1", state, got)
		}
	}
	if got := metricValue(t, provider.Gatherer(), "rollback_guardian_a2a_calls_total", map[string]string{"operation": "watch", "outcome": "timeout"}); got != 1 {
		t.Errorf("a2a watch timeout series = %v", got)
	}
}

func TestMetricsHandlerServesPrometheusText(t *testing.T) {
	provider := newProvider(t)
	recorder := httptest.NewRecorder()
	provider.MetricsHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("metrics response = %d %q", recorder.Code, recorder.Header().Get("Content-Type"))
	}
	for _, want := range []string{"go_goroutines", "process_"} {
		if !strings.Contains(recorder.Body.String(), want) {
			t.Fatalf("metrics body lacks %s", want)
		}
	}
}

func TestProvidersHaveIndependentRegistries(t *testing.T) {
	first, second := newProvider(t), newProvider(t)
	_, finish := first.ObserveToolCall(context.Background(), "observability", "get_http_error_rate")
	finish("ok")
	if got := metricValue(t, second.Gatherer(), "rollback_guardian_mcp_calls_total", nil); got != 0 {
		t.Fatalf("second registry saw %v calls from the first", got)
	}
}

func TestUnitOfWorkMetricsFollowCommittedEvents(t *testing.T) {
	provider := newProvider(t)
	gatherer := provider.Gatherer()
	runLifecycle(t, provider)

	checks := []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{name: "rollback_guardian_incidents_total", want: 3},
		{name: "rollback_guardian_incidents_terminal_total", labels: map[string]string{"status": "resolved"}, want: 1},
		{name: "rollback_guardian_incidents_terminal_total", labels: map[string]string{"status": "failed"}, want: 1},
		{name: "rollback_guardian_incidents_terminal_total", labels: map[string]string{"status": "canceled"}, want: 1},
		{name: "rollback_guardian_approval_decisions_total", labels: map[string]string{"decision": "approved"}, want: 1},
		{name: "rollback_guardian_rollback_executions_total", labels: map[string]string{"outcome": "succeeded"}, want: 1},
		{name: "rollback_guardian_recovery_verifications_total", labels: map[string]string{"result": "passed"}, want: 1},
		{name: "rollback_guardian_investigation_dispatches_total", labels: map[string]string{"outcome": "failed", "reason": "investigation_timeout"}, want: 1},
		{name: "rollback_guardian_investigation_dispatches_total", labels: map[string]string{"outcome": "canceled", "reason": "none"}, want: 1},
		{name: "rollback_guardian_investigation_duration_seconds", labels: map[string]string{"outcome": "failed"}, want: 1},
	}
	for _, check := range checks {
		if got := metricValue(t, gatherer, check.name, check.labels); got != check.want {
			t.Errorf("%s%v = %v, want %v", check.name, check.labels, got, check.want)
		}
	}
}

func TestUnitOfWorkDiscardsObservationsFromRolledBackTransactions(t *testing.T) {
	provider := newProvider(t)
	uow, err := provider.ObserveUnitOfWork(memory.NewUnitOfWork())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	target := must(domain.NewDeploymentTarget("local", "rollback-demo", "demo-api", "demo-api"))
	symptom := must(domain.NewSymptom("http_error_rate", 0.35, 0.10, "ratio", now))
	incident := must(domain.NewIncident("incident-1", target, symptom, now))
	actor := must(domain.NewActor("operator", domain.ActorHuman))
	event, err := domain.NewOperationEvent("event-1", incident.ID(), 1, domain.EventIncidentCreated, now, actor, domain.IncidentCreatedDetail{Target: target})
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("abort")
	err = uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
		if err := repositories.Incidents().Create(incident); err != nil {
			return err
		}
		if err := repositories.Events().Append(event); err != nil {
			return err
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatalf("WithinTransaction() = %v", err)
	}
	if got := metricValue(t, provider.Gatherer(), "rollback_guardian_incidents_total", nil); got != 0 {
		t.Fatalf("rolled back incident counted: %v", got)
	}
}

// runLifecycle drives three incidents through the application with the
// observed unit of work: one resolved by rollback, one whose investigation
// times out (failed twice, counted once), and one canceled while queued.
func runLifecycle(t *testing.T, provider *telemetry.Provider) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	clock := testkit.NewClock(now)
	ids := &testkit.IDGenerator{}
	target := must(domain.NewDeploymentTarget("local", "rollback-demo", "demo-api", "demo-api"))
	symptom := must(domain.NewSymptom("http_error_rate", 0.35, 0.10, "ratio", now))
	infra := &fake.Infrastructure{
		Deployment: domain.DeploymentEvidence{Target: target, CurrentRevision: "revision-2", PreviousStableRevision: "revision-1", DesiredReplicas: 3, ReadyReplicas: 1, AvailableReplicas: 1, RolloutAt: now.Add(-time.Minute)},
		Metric:     domain.MetricEvidence{Name: "http_error_rate", Value: 0.35, Threshold: 0.10, Unit: "ratio", ObservedAt: now},
		Now:        clock.Now,
	}
	diagnoser, err := diagnosis.NewRuleDiagnoser(ids, diagnosis.RuleDiagnoserConfig{CorrelationWindow: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	policy := must(domain.NewEligibilityPolicy(domain.EligibilityPolicyConfig{MinimumConfidence: 0.8, CorrelationWindow: 10 * time.Minute, AllowedTargets: []domain.DeploymentTarget{target}}))
	system := must(domain.NewActor("rollback-guardian", domain.ActorSystem))
	human := must(domain.NewActor("local-admin", domain.ActorHuman))
	uow, err := provider.ObserveUnitOfWork(memory.NewUnitOfWork())
	if err != nil {
		t.Fatal(err)
	}
	services, err := application.NewServices(application.Dependencies{UnitOfWork: uow, Inspector: infra, Metrics: infra, Executor: infra, RollbackPolicy: policy, ApprovalPolicy: domain.ApprovalAuthorizer{}, Access: applicationtest.AllowAll{}, Clock: clock, IDGenerator: ids, Tracer: provider}, application.WorkflowConfig{ProposalTTL: 15 * time.Minute, ApprovalTTL: 10 * time.Minute, CleanupTimeout: time.Second, SystemActor: system})
	if err != nil {
		t.Fatal(err)
	}

	runner, err := application.NewInvestigationRunner(infra, infra, diagnoser, clock, ids)
	if err != nil {
		t.Fatal(err)
	}
	flow := applicationtest.Flow{Services: services, UnitOfWork: uow, Clock: clock, IDs: ids, Runner: runner, SystemActor: system, CleanupTimeout: time.Second}
	resolved, err := flow.CreateIncident(ctx, application.CreateIncidentCommand{Target: target, Symptom: symptom, Actor: human})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := flow.Investigate(ctx, resolved.ID()); err != nil {
		t.Fatal(err)
	}
	proposal, err := flow.Proposal(ctx, resolved.ID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.Approvals.Approve(ctx, application.ApprovalCommand{IncidentID: resolved.ID(), Actor: human, ActionID: proposal.ActionID(), ProposalHash: proposal.Hash()}); err != nil {
		t.Fatal(err)
	}
	if _, err := services.Executions.Execute(ctx, resolved.ID(), "execute-1"); err != nil {
		t.Fatal(err)
	}
	infra.SetRecoveryMetric(0.02)
	if _, err := services.Verifications.Verify(ctx, resolved.ID()); err != nil {
		t.Fatal(err)
	}

	timedOut, err := services.Incidents.CreateIncidentAsync(ctx, application.CreateIncidentCommand{Target: target, Symptom: symptom, Actor: human})
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(3 * time.Second)
	for range 2 {
		if err := services.Investigations.Fail(ctx, timedOut.ID(), "investigation_timeout"); err != nil {
			t.Fatal(err)
		}
	}

	canceled, err := services.Incidents.CreateIncidentAsync(ctx, application.CreateIncidentCommand{Target: target, Symptom: symptom, Actor: human})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.Incidents.Cancel(ctx, application.CancelIncidentCommand{IncidentID: canceled.ID(), Actor: human, Reason: "operator canceled"}); err != nil {
		t.Fatal(err)
	}
}

type stubGateway struct{}

func (stubGateway) Submit(context.Context, ports.InvestigationSubmission) (domain.InvestigationTaskRef, error) {
	return domain.InvestigationTaskRef{TaskID: "task-1", ContextID: "context-1"}, nil
}

func (stubGateway) Watch(context.Context, domain.InvestigationTaskRef, func(domain.InvestigationStage) error) (ports.InvestigationResult, error) {
	return ports.InvestigationResult{}, context.DeadlineExceeded
}

func (stubGateway) Cancel(context.Context, domain.InvestigationTaskRef) error { return nil }

type stubDiagnoser struct {
	conclusion domain.InvestigationConclusion
}

func (d stubDiagnoser) Diagnose(context.Context, ports.InvestigationInput) (domain.InvestigationConclusion, error) {
	return d.conclusion, nil
}

func fallbackConclusion(t *testing.T) domain.InvestigationConclusion {
	t.Helper()
	conclusion, err := domain.NewInvestigationConclusionWithProvenance("conclusion-1", "incident-1", domain.StrategyRules, "new revision unhealthy", 0.95, []domain.EvidenceID{"evidence-1"}, domain.RecommendRollback,
		domain.DiagnosisProvenance{RequestedStrategy: domain.StrategyHybrid, Model: "model", PromptVersion: application.DiagnosisPromptVersion, FallbackReason: domain.FallbackProviderTimeout}, time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return conclusion
}
