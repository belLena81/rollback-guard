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
	summary, err := services.Queries.GetIncidentSummary(context.Background(), testReader(), incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Incident.Status() != domain.StatusAwaitingApproval || summary.Investigation == nil || summary.Investigation.State != domain.InvestigationTaskCompleted {
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
	summary, err := services.Queries.GetIncidentSummary(context.Background(), testReader(), incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Investigation == nil || summary.Investigation.State != domain.InvestigationTaskCanceled || summary.Investigation.CompletedAt.IsZero() {
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
	summary, err := services.Queries.GetIncidentSummary(context.Background(), testReader(), incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Investigation == nil || summary.Investigation.State != domain.InvestigationTaskQueued {
		t.Fatalf("first attempt = %#v", summary.Investigation)
	}
	if err := coordinator.ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	summary, err = services.Queries.GetIncidentSummary(context.Background(), testReader(), incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Investigation.State != domain.InvestigationTaskCompleted || flaky.first.IdempotencyKey != flaky.second.IdempotencyKey || flaky.first.RequestHash != flaky.second.RequestHash {
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
	summary, err := services.Queries.GetIncidentSummary(context.Background(), testReader(), incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Investigation == nil || summary.Investigation.State != domain.InvestigationTaskCanceled {
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
	summary, err := services.Queries.GetIncidentSummary(context.Background(), testReader(), incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Investigation == nil || summary.Investigation.State != domain.InvestigationTaskCanceled {
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
	summary, err := services.Queries.GetIncidentSummary(context.Background(), testReader(), incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Investigation == nil || summary.Investigation.State != domain.InvestigationTaskCanceled || summary.Investigation.Ref.TaskID != "task-1" {
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
	summary, err := services.Queries.GetIncidentSummary(context.Background(), testReader(), incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Investigation == nil || summary.Investigation.State != domain.InvestigationTaskFailed {
		t.Fatalf("investigation = %#v", summary.Investigation)
	}
}

func TestCoordinatorShutdownPreservesLeasedInvestigationOnFinalAttempt(t *testing.T) {
	services, coordinator, target, symptom, actor := asyncHarness(t)
	gateway := &blockedWatchGateway{started: make(chan struct{})}
	coordinator.gateway = gateway
	coordinator.config.MaxAttempts = 1
	coordinator.config.TaskTimeout = time.Minute
	incident, err := services.Incidents.CreateIncidentAsync(context.Background(), CreateIncidentCommand{Target: target, Symptom: symptom, Actor: actor})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	processed := make(chan error, 1)
	go func() {
		processed <- coordinator.ProcessNext(ctx)
	}()
	waitForSignal(t, gateway.started, "watch to start")
	cancel()
	if err := waitForResult(t, processed, "coordinator shutdown"); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown error = %v, want context canceled", err)
	}
	if gateway.cancelCalls.Load() != 0 {
		t.Fatalf("remote cancellation calls = %d, want 0", gateway.cancelCalls.Load())
	}
	summary, err := services.Queries.GetIncidentSummary(context.Background(), testReader(), incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Incident.Status() != domain.StatusInvestigating {
		t.Fatalf("incident status = %s, want investigating", summary.Incident.Status())
	}
	if summary.Investigation == nil {
		t.Fatal("missing investigation dispatch")
	}
	if summary.Investigation.State.Terminal() || summary.Investigation.State != domain.InvestigationTaskSubmitted {
		t.Fatalf("investigation state = %s, want submitted non-terminal", summary.Investigation.State)
	}
	if summary.Investigation.Ref.TaskID != "task-1" || summary.Investigation.LeaseExpiresAt.IsZero() || !summary.Investigation.CompletedAt.IsZero() || summary.Investigation.FailureCode != "" {
		t.Fatalf("investigation after shutdown = %#v", summary.Investigation)
	}
}

func TestInvestigationRequestHashUsesPostgresTimestampPrecision(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 123456789, time.UTC)
	target, err := domain.NewDeploymentTarget("local", "payments", "api", "api")
	if err != nil {
		t.Fatal(err)
	}
	newIncident := func(startedAt time.Time, observed float64) domain.Incident {
		symptom, symptomErr := domain.NewSymptom("http_error_rate", observed, 0.05, "ratio", startedAt)
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
	services, err := NewServices(Dependencies{UnitOfWork: uow, Inspector: infra, Metrics: infra, Executor: infra, RollbackPolicy: policy, ApprovalPolicy: domain.ApprovalAuthorizer{}, Access: allowAll{}, Clock: clock, IDGenerator: ids}, WorkflowConfig{ProposalTTL: time.Minute, ApprovalTTL: time.Minute, CleanupTimeout: time.Second, SystemActor: system})
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

func (g *blockedWatchGateway) Submit(context.Context, ports.InvestigationSubmission) (domain.InvestigationTaskRef, error) {
	return domain.InvestigationTaskRef{TaskID: "task-1", ContextID: "context-1"}, nil
}

func (g *blockedWatchGateway) Watch(ctx context.Context, _ domain.InvestigationTaskRef, _ func(domain.InvestigationStage) error) (ports.InvestigationResult, error) {
	close(g.started)
	<-ctx.Done()
	return ports.InvestigationResult{}, ctx.Err()
}

func (g *blockedWatchGateway) Cancel(context.Context, domain.InvestigationTaskRef) error {
	g.cancelCalls.Add(1)
	return nil
}

type progressAfterCancelGateway struct {
	started          chan struct{}
	progress         chan struct{}
	progressRejected atomic.Bool
	cancelCalls      atomic.Int32
}

func (g *progressAfterCancelGateway) Submit(context.Context, ports.InvestigationSubmission) (domain.InvestigationTaskRef, error) {
	return domain.InvestigationTaskRef{TaskID: "task-1", ContextID: "context-1"}, nil
}

func (g *progressAfterCancelGateway) Watch(_ context.Context, _ domain.InvestigationTaskRef, progress func(domain.InvestigationStage) error) (ports.InvestigationResult, error) {
	close(g.started)
	<-g.progress
	err := progress(domain.InvestigationStageMetrics)
	g.progressRejected.Store(err != nil)
	return ports.InvestigationResult{}, err
}

func (g *progressAfterCancelGateway) Cancel(context.Context, domain.InvestigationTaskRef) error {
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

func (g *lateProgressGateway) Submit(context.Context, ports.InvestigationSubmission) (domain.InvestigationTaskRef, error) {
	return domain.InvestigationTaskRef{TaskID: "task-1", ContextID: "context-1"}, nil
}

func (g *lateProgressGateway) Watch(ctx context.Context, _ domain.InvestigationTaskRef, progress func(domain.InvestigationStage) error) (ports.InvestigationResult, error) {
	close(g.started)
	<-g.release
	g.progressResult <- progress(domain.InvestigationStageMetrics)
	return ports.InvestigationResult{}, ctx.Err()
}

func (g *lateProgressGateway) Cancel(context.Context, domain.InvestigationTaskRef) error {
	return nil
}

func (g *blockedSubmitGateway) Submit(context.Context, ports.InvestigationSubmission) (domain.InvestigationTaskRef, error) {
	close(g.started)
	<-g.release
	return domain.InvestigationTaskRef{TaskID: "task-1", ContextID: "context-1"}, nil
}

func (g *blockedSubmitGateway) Watch(context.Context, domain.InvestigationTaskRef, func(domain.InvestigationStage) error) (ports.InvestigationResult, error) {
	g.watchCalls.Add(1)
	return ports.InvestigationResult{}, errors.New("watch must not be called")
}

func (g *blockedSubmitGateway) Cancel(context.Context, domain.InvestigationTaskRef) error {
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

func (g *flakySubmissionGateway) Submit(ctx context.Context, request ports.InvestigationSubmission) (domain.InvestigationTaskRef, error) {
	g.calls++
	if g.calls == 1 {
		g.first = request
		return domain.InvestigationTaskRef{}, errors.New("response lost")
	}
	g.second = request
	return g.localInvestigationGateway.Submit(ctx, request)
}

func (g *localInvestigationGateway) Submit(_ context.Context, request ports.InvestigationSubmission) (domain.InvestigationTaskRef, error) {
	g.request = request
	return domain.InvestigationTaskRef{TaskID: "task-1", ContextID: "context-1"}, nil
}

func (g *localInvestigationGateway) Watch(ctx context.Context, ref domain.InvestigationTaskRef, progress func(domain.InvestigationStage) error) (ports.InvestigationResult, error) {
	for _, stage := range []domain.InvestigationStage{domain.InvestigationStageDeployment, domain.InvestigationStageMetrics, domain.InvestigationStageDiagnosis, domain.InvestigationStageFinalizing} {
		if err := progress(stage); err != nil {
			return ports.InvestigationResult{}, err
		}
	}
	result, err := g.investigator.Investigate(ctx, ports.InvestigationRequest{Incident: g.request.Incident})
	g.result = result
	return result, err
}

func (g *localInvestigationGateway) Cancel(context.Context, domain.InvestigationTaskRef) error {
	return nil
}
