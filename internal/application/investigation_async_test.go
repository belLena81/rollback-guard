package application

import (
	"context"
	"errors"
	"sync/atomic"
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

func TestCoordinatorCancelsInvestigationWhileWatchIsBlocked(t *testing.T) {
	services, coordinator, target, symptom, actor := asyncHarness(t)
	gateway := &blockedWatchGateway{started: make(chan struct{})}
	coordinator.gateway = gateway
	coordinator.config.PollInterval = 5 * time.Millisecond
	incident, err := services.Incidents.CreateIncidentAsync(context.Background(), CreateIncidentCommand{Target: target, Symptom: symptom, Actor: actor})
	if err != nil {
		t.Fatal(err)
	}
	processed := make(chan error, 1)
	go func() {
		processed <- coordinator.ProcessNext(context.Background())
	}()
	waitForSignal(t, gateway.started, "watch to start")
	if _, err := services.Incidents.Cancel(context.Background(), CancelIncidentCommand{IncidentID: incident.ID(), Actor: actor, Reason: "operator canceled"}); err != nil {
		t.Fatal(err)
	}
	if err := waitForResult(t, processed, "coordinator cancellation"); err != nil {
		t.Fatal(err)
	}
	if gateway.cancelCalls.Load() != 1 {
		t.Fatalf("remote cancellation calls = %d, want 1", gateway.cancelCalls.Load())
	}
	summary, err := services.Queries.GetIncidentSummary(context.Background(), incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Investigation == nil || summary.Investigation.State != ports.InvestigationTaskCanceled {
		t.Fatalf("investigation = %#v", summary.Investigation)
	}
	if summary.Conclusion != nil || summary.Proposal != nil {
		t.Fatalf("canceled investigation persisted conclusion/proposal = %#v/%#v", summary.Conclusion, summary.Proposal)
	}
}

func TestCoordinatorPreservesCancellationDuringProgress(t *testing.T) {
	services, coordinator, target, symptom, actor := asyncHarness(t)
	gateway := &progressAfterCancelGateway{started: make(chan struct{}), progress: make(chan struct{})}
	coordinator.gateway = gateway
	coordinator.config.PollInterval = time.Hour
	incident, err := services.Incidents.CreateIncidentAsync(context.Background(), CreateIncidentCommand{Target: target, Symptom: symptom, Actor: actor})
	if err != nil {
		t.Fatal(err)
	}
	processed := make(chan error, 1)
	go func() {
		processed <- coordinator.ProcessNext(context.Background())
	}()
	waitForSignal(t, gateway.started, "watch to start")
	if _, err := services.Incidents.Cancel(context.Background(), CancelIncidentCommand{IncidentID: incident.ID(), Actor: actor, Reason: "operator canceled"}); err != nil {
		t.Fatal(err)
	}
	close(gateway.progress)
	if err := waitForResult(t, processed, "progress cancellation"); err != nil {
		t.Fatal(err)
	}
	if !gateway.progressRejected.Load() {
		t.Fatal("progress callback accepted an update after cancellation")
	}
	summary, err := services.Queries.GetIncidentSummary(context.Background(), incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Investigation == nil || summary.Investigation.State != ports.InvestigationTaskCanceled {
		t.Fatalf("investigation = %#v", summary.Investigation)
	}
	if summary.Conclusion != nil || summary.Proposal != nil {
		t.Fatalf("canceled investigation persisted conclusion/proposal = %#v/%#v", summary.Conclusion, summary.Proposal)
	}
}

func TestCoordinatorCancelsSubmissionCompletedAfterRequest(t *testing.T) {
	services, coordinator, target, symptom, actor := asyncHarness(t)
	gateway := &blockedSubmitGateway{started: make(chan struct{}), release: make(chan struct{})}
	coordinator.gateway = gateway
	incident, err := services.Incidents.CreateIncidentAsync(context.Background(), CreateIncidentCommand{Target: target, Symptom: symptom, Actor: actor})
	if err != nil {
		t.Fatal(err)
	}
	processed := make(chan error, 1)
	go func() {
		processed <- coordinator.ProcessNext(context.Background())
	}()
	waitForSignal(t, gateway.started, "submission to start")
	if _, err := services.Incidents.Cancel(context.Background(), CancelIncidentCommand{IncidentID: incident.ID(), Actor: actor, Reason: "operator canceled"}); err != nil {
		t.Fatal(err)
	}
	close(gateway.release)
	if err := waitForResult(t, processed, "submission cancellation"); err != nil {
		t.Fatal(err)
	}
	if gateway.cancelCalls.Load() != 1 || gateway.watchCalls.Load() != 0 {
		t.Fatalf("remote cancel/watch calls = %d/%d, want 1/0", gateway.cancelCalls.Load(), gateway.watchCalls.Load())
	}
	summary, err := services.Queries.GetIncidentSummary(context.Background(), incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Investigation == nil || summary.Investigation.State != ports.InvestigationTaskCanceled || summary.Investigation.Ref.TaskID != "task-1" {
		t.Fatalf("investigation = %#v", summary.Investigation)
	}
}

func TestCoordinatorRejectsProgressAfterTimeoutFailure(t *testing.T) {
	services, coordinator, target, symptom, actor := asyncHarness(t)
	gateway := &lateProgressGateway{started: make(chan struct{}), release: make(chan struct{}), progressResult: make(chan error, 1)}
	coordinator.gateway = gateway
	coordinator.config.TaskTimeout = 10 * time.Millisecond
	coordinator.config.CancelTimeout = 50 * time.Millisecond
	incident, err := services.Incidents.CreateIncidentAsync(context.Background(), CreateIncidentCommand{Target: target, Symptom: symptom, Actor: actor})
	if err != nil {
		t.Fatal(err)
	}
	processed := make(chan error, 1)
	go func() {
		processed <- coordinator.ProcessNext(context.Background())
	}()
	waitForSignal(t, gateway.started, "watch to start")
	if err := waitForResult(t, processed, "investigation timeout"); err == nil {
		t.Fatal("expected investigation timeout")
	}
	close(gateway.release)
	if err := waitForResult(t, gateway.progressResult, "late progress callback"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("late progress error = %v, want deadline exceeded", err)
	}
	summary, err := services.Queries.GetIncidentSummary(context.Background(), incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Investigation == nil || summary.Investigation.State != ports.InvestigationTaskFailed {
		t.Fatalf("investigation = %#v", summary.Investigation)
	}
}

func TestInvestigationRequestHashUsesPostgresTimestampPrecision(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 123456789, time.UTC)
	target, err := domain.NewDeploymentTarget("local", "payments", "api", "api")
	if err != nil {
		t.Fatal(err)
	}
	newIncident := func(startedAt time.Time, observed float64) domain.Incident {
		symptom, symptomErr := domain.NewSymptom("error_rate", observed, 0.05, "ratio", startedAt)
		if symptomErr != nil {
			t.Fatal(symptomErr)
		}
		incident, incidentErr := domain.NewIncident("incident-1", target, symptom, now)
		if incidentErr != nil {
			t.Fatal(incidentErr)
		}
		if incidentErr = incident.StartInvestigation(now.Add(time.Second)); incidentErr != nil {
			t.Fatal(incidentErr)
		}
		return incident
	}
	original := newIncident(now, 0.2)
	postgresValue := newIncident(now.Truncate(time.Microsecond), 0.2)
	changed := newIncident(now, 0.3)
	if InvestigationRequestHash(original) != InvestigationRequestHash(postgresValue) {
		t.Fatal("equivalent PostgreSQL timestamps produced different hashes")
	}
	if InvestigationRequestHash(original) == InvestigationRequestHash(changed) {
		t.Fatal("meaningfully different incidents produced the same hash")
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

type blockedWatchGateway struct {
	started     chan struct{}
	cancelCalls atomic.Int32
}

func (g *blockedWatchGateway) Submit(context.Context, ports.InvestigationSubmission) (ports.InvestigationTaskRef, error) {
	return ports.InvestigationTaskRef{TaskID: "task-1", ContextID: "context-1"}, nil
}

func (g *blockedWatchGateway) Watch(ctx context.Context, _ ports.InvestigationTaskRef, _ func(ports.InvestigationStage) error) (ports.InvestigationResult, error) {
	close(g.started)
	<-ctx.Done()
	return ports.InvestigationResult{}, ctx.Err()
}

func (g *blockedWatchGateway) Get(context.Context, ports.InvestigationTaskRef) (ports.InvestigationTaskSnapshot, error) {
	return ports.InvestigationTaskSnapshot{}, nil
}

func (g *blockedWatchGateway) Cancel(context.Context, ports.InvestigationTaskRef) error {
	g.cancelCalls.Add(1)
	return nil
}

type progressAfterCancelGateway struct {
	started          chan struct{}
	progress         chan struct{}
	progressRejected atomic.Bool
	cancelCalls      atomic.Int32
}

func (g *progressAfterCancelGateway) Submit(context.Context, ports.InvestigationSubmission) (ports.InvestigationTaskRef, error) {
	return ports.InvestigationTaskRef{TaskID: "task-1", ContextID: "context-1"}, nil
}

func (g *progressAfterCancelGateway) Watch(_ context.Context, _ ports.InvestigationTaskRef, progress func(ports.InvestigationStage) error) (ports.InvestigationResult, error) {
	close(g.started)
	<-g.progress
	err := progress(ports.InvestigationStageMetrics)
	g.progressRejected.Store(err != nil)
	return ports.InvestigationResult{}, err
}

func (g *progressAfterCancelGateway) Get(context.Context, ports.InvestigationTaskRef) (ports.InvestigationTaskSnapshot, error) {
	return ports.InvestigationTaskSnapshot{}, nil
}

func (g *progressAfterCancelGateway) Cancel(context.Context, ports.InvestigationTaskRef) error {
	g.cancelCalls.Add(1)
	return nil
}

type blockedSubmitGateway struct {
	started     chan struct{}
	release     chan struct{}
	cancelCalls atomic.Int32
	watchCalls  atomic.Int32
}

type lateProgressGateway struct {
	started        chan struct{}
	release        chan struct{}
	progressResult chan error
}

func (g *lateProgressGateway) Submit(context.Context, ports.InvestigationSubmission) (ports.InvestigationTaskRef, error) {
	return ports.InvestigationTaskRef{TaskID: "task-1", ContextID: "context-1"}, nil
}

func (g *lateProgressGateway) Watch(ctx context.Context, _ ports.InvestigationTaskRef, progress func(ports.InvestigationStage) error) (ports.InvestigationResult, error) {
	close(g.started)
	<-g.release
	g.progressResult <- progress(ports.InvestigationStageMetrics)
	return ports.InvestigationResult{}, ctx.Err()
}

func (g *lateProgressGateway) Get(context.Context, ports.InvestigationTaskRef) (ports.InvestigationTaskSnapshot, error) {
	return ports.InvestigationTaskSnapshot{}, nil
}

func (g *lateProgressGateway) Cancel(context.Context, ports.InvestigationTaskRef) error {
	return nil
}

func (g *blockedSubmitGateway) Submit(context.Context, ports.InvestigationSubmission) (ports.InvestigationTaskRef, error) {
	close(g.started)
	<-g.release
	return ports.InvestigationTaskRef{TaskID: "task-1", ContextID: "context-1"}, nil
}

func (g *blockedSubmitGateway) Watch(context.Context, ports.InvestigationTaskRef, func(ports.InvestigationStage) error) (ports.InvestigationResult, error) {
	g.watchCalls.Add(1)
	return ports.InvestigationResult{}, errors.New("watch must not be called")
}

func (g *blockedSubmitGateway) Get(context.Context, ports.InvestigationTaskRef) (ports.InvestigationTaskSnapshot, error) {
	return ports.InvestigationTaskSnapshot{}, nil
}

func (g *blockedSubmitGateway) Cancel(context.Context, ports.InvestigationTaskRef) error {
	g.cancelCalls.Add(1)
	return nil
}

func waitForSignal(t *testing.T, signal <-chan struct{}, operation string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", operation)
	}
}

func waitForResult(t *testing.T, result <-chan error, operation string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", operation)
		return nil
	}
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
