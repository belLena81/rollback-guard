package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/diagnosis"
	"github.com/belLena81/rollback-guard/internal/adapters/fake"
	"github.com/belLena81/rollback-guard/internal/adapters/memory"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/belLena81/rollback-guard/internal/testkit"
)

func TestAsyncIncidentCreationAndCoordinatorCompletion(t *testing.T) {
	services, coordinator, target, symptom, actor := asyncHarness(t)
	incident, err := services.Incidents.CreateIncidentAsync(context.Background(), CreateIncidentCommand{Target: target, Symptom: symptom, Actor: actor})
	if err != nil {
		t.Fatal(err)
	}
	if incident.Status() != domain.StatusInvestigating {
		t.Fatalf("status = %s, want investigating", incident.Status())
	}
	if err := coordinator.ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	summary, err := services.Queries.GetIncidentSummary(context.Background(), incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Incident.Status() != domain.StatusAwaitingApproval || summary.Investigation == nil || summary.Investigation.State != ports.InvestigationTaskCompleted {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestCancelQueuedInvestigationCommitsIncidentAndTaskTogether(t *testing.T) {
	services, _, target, symptom, actor := asyncHarness(t)
	incident, err := services.Incidents.CreateIncidentAsync(context.Background(), CreateIncidentCommand{Target: target, Symptom: symptom, Actor: actor})
	if err != nil {
		t.Fatal(err)
	}
	canceled, err := services.Incidents.Cancel(context.Background(), CancelIncidentCommand{IncidentID: incident.ID(), Actor: actor, Reason: "operator canceled"})
	if err != nil {
		t.Fatal(err)
	}
	if canceled.Status() != domain.StatusCanceled {
		t.Fatalf("incident status = %s", canceled.Status())
	}
	summary, err := services.Queries.GetIncidentSummary(context.Background(), incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Investigation == nil || summary.Investigation.State != ports.InvestigationTaskCanceled || summary.Investigation.CompletedAt.IsZero() {
		t.Fatalf("investigation = %#v", summary.Investigation)
	}
}

func TestCoordinatorRetriesAmbiguousSubmissionWithSameCorrelation(t *testing.T) {
	services, coordinator, target, symptom, actor := asyncHarness(t)
	base, ok := coordinator.gateway.(*localInvestigationGateway)
	if !ok {
		t.Fatal("unexpected gateway type")
	}
	flaky := &flakySubmissionGateway{localInvestigationGateway: base}
	coordinator.gateway = flaky
	incident, err := services.Incidents.CreateIncidentAsync(context.Background(), CreateIncidentCommand{Target: target, Symptom: symptom, Actor: actor})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.ProcessNext(context.Background()); err == nil {
		t.Fatal("expected first submission error")
	}
	summary, err := services.Queries.GetIncidentSummary(context.Background(), incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Investigation == nil || summary.Investigation.State != ports.InvestigationTaskQueued {
		t.Fatalf("first attempt = %#v", summary.Investigation)
	}
	if err := coordinator.ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	summary, err = services.Queries.GetIncidentSummary(context.Background(), incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Investigation.State != ports.InvestigationTaskCompleted || flaky.first.IdempotencyKey != flaky.second.IdempotencyKey || flaky.first.RequestHash != flaky.second.RequestHash {
		t.Fatalf("retry result = %#v correlation = %#v/%#v", summary.Investigation, flaky.first, flaky.second)
	}
}

func asyncHarness(t *testing.T) (Services, *InvestigationCoordinator, domain.DeploymentTarget, domain.Symptom, domain.Actor) {
	t.Helper()
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	clock := testkit.NewClock(now)
	ids := &testkit.IDGenerator{}
	target, err := domain.NewDeploymentTarget("local", "rollback-demo", "demo-api", "demo-api")
	if err != nil {
		t.Fatal(err)
	}
	symptom, err := domain.NewSymptom("http_error_rate", 0.35, 0.10, "ratio", now)
	if err != nil {
		t.Fatal(err)
	}
	infra := &fake.Infrastructure{
		Deployment: domain.DeploymentEvidence{Target: target, CurrentRevision: "revision-2", PreviousStableRevision: "revision-1", DesiredReplicas: 3, ReadyReplicas: 1, AvailableReplicas: 1, RolloutAt: now.Add(-time.Minute)},
		Metric:     domain.MetricEvidence{Name: "http_error_rate", Value: 0.35, Threshold: 0.10, Unit: "ratio", ObservedAt: now},
	}
	diagnoser, err := diagnosis.NewRuleDiagnoser(ids, diagnosis.RuleDiagnoserConfig{CorrelationWindow: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := domain.NewEligibilityPolicy(domain.EligibilityPolicyConfig{MinimumConfidence: 0.8, CorrelationWindow: 10 * time.Minute, AllowedTargets: []domain.DeploymentTarget{target}})
	if err != nil {
		t.Fatal(err)
	}
	system, err := domain.NewActor("rollback-guardian", domain.ActorSystem)
	if err != nil {
		t.Fatal(err)
	}
	human, err := domain.NewActor("local-admin", domain.ActorHuman)
	if err != nil {
		t.Fatal(err)
	}
	uow := memory.NewUnitOfWork()
	services, err := NewServices(Dependencies{UnitOfWork: uow, Inspector: infra, Metrics: infra, Diagnoser: diagnoser, Executor: infra, RollbackPolicy: policy, ApprovalPolicy: domain.ApprovalAuthorizer{}, Clock: clock, IDGenerator: ids}, WorkflowConfig{ProposalTTL: time.Minute, ApprovalTTL: time.Minute, CleanupTimeout: time.Second, SystemActor: system})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewInvestigationRunner(infra, infra, diagnoser, clock, ids)
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewInvestigationCoordinator(uow, &localInvestigationGateway{investigator: runner}, services.Investigations, clock, InvestigationCoordinatorConfig{WorkerID: "test-worker", TaskTimeout: time.Second, CancelTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return services, coordinator, target, symptom, human
}

type localInvestigationGateway struct {
	investigator ports.Investigator
	request      ports.InvestigationSubmission
	result       ports.InvestigationResult
}

type flakySubmissionGateway struct {
	*localInvestigationGateway
	calls         int
	first, second ports.InvestigationSubmission
}

func (g *flakySubmissionGateway) Submit(ctx context.Context, request ports.InvestigationSubmission) (ports.InvestigationTaskRef, error) {
	g.calls++
	if g.calls == 1 {
		g.first = request
		return ports.InvestigationTaskRef{}, errors.New("response lost")
	}
	g.second = request
	return g.localInvestigationGateway.Submit(ctx, request)
}

func (g *localInvestigationGateway) Submit(_ context.Context, request ports.InvestigationSubmission) (ports.InvestigationTaskRef, error) {
	g.request = request
	return ports.InvestigationTaskRef{TaskID: "task-1", ContextID: "context-1"}, nil
}

func (g *localInvestigationGateway) Watch(ctx context.Context, ref ports.InvestigationTaskRef, progress func(ports.InvestigationStage) error) (ports.InvestigationResult, error) {
	for _, stage := range []ports.InvestigationStage{ports.InvestigationStageDeployment, ports.InvestigationStageMetrics, ports.InvestigationStageDiagnosis, ports.InvestigationStageFinalizing} {
		if err := progress(stage); err != nil {
			return ports.InvestigationResult{}, err
		}
	}
	result, err := g.investigator.Investigate(ctx, ports.InvestigationRequest{Incident: g.request.Incident})
	g.result = result
	return result, err
}

func (g *localInvestigationGateway) Get(context.Context, ports.InvestigationTaskRef) (ports.InvestigationTaskSnapshot, error) {
	return ports.InvestigationTaskSnapshot{Ref: ports.InvestigationTaskRef{TaskID: "task-1", ContextID: "context-1"}, State: ports.InvestigationTaskCompleted, Stage: ports.InvestigationStageFinalizing, Result: &g.result}, nil
}

func (g *localInvestigationGateway) Cancel(context.Context, ports.InvestigationTaskRef) error {
	return nil
}
