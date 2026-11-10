//go:build integration

package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/diagnosis"
	"github.com/belLena81/rollback-guard/internal/adapters/fake"
	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/application/applicationtest"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/belLena81/rollback-guard/internal/testkit"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests run the approve -> execute -> verify use case on the real
// PostgreSQL unit of work, so its transactions, optimistic locks, and
// constraints are the ones under test.

type approvePostgresHarness struct {
	services application.Services
	infra    *fake.Infrastructure
	incident domain.IncidentID
	command  application.ApprovalCommand
	queries  *application.QueryService
}

func newApprovePostgresHarness(t *testing.T, unitOfWork func(*UnitOfWork) ports.UnitOfWork) *approvePostgresHarness {
	t.Helper()
	migrated := isolatedSchemaPool(t, "000001_initial_schema.up.sql", "000002_rollback_mutation_ledger.up.sql", "000003_a2a_investigation.up.sql", "000004_ai_diagnosis.up.sql")
	// The migration pool uses the simple protocol for multi-statement
	// files; the application runs on the default extended protocol, as in
	// production, against the same isolated schema.
	config := migrated.Config()
	config.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	store, err := NewUnitOfWork(pool)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
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
		Now:        clock.Now,
	}
	infra.AfterExecute = func() { infra.Metric.Value = 0.02 }
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
	human, err := domain.NewActor("alice", domain.ActorHuman)
	if err != nil {
		t.Fatal(err)
	}
	var uow ports.UnitOfWork = store
	if unitOfWork != nil {
		uow = unitOfWork(store)
	}
	services, err := application.NewServices(application.Dependencies{
		UnitOfWork: uow, Inspector: infra, Metrics: infra, Executor: infra,
		RollbackPolicy: policy, ApprovalPolicy: domain.ApprovalAuthorizer{}, Access: applicationtest.AllowAll{}, Clock: clock, IDGenerator: ids,
	}, application.WorkflowConfig{ProposalTTL: 15 * time.Minute, ApprovalTTL: 5 * time.Minute, CleanupTimeout: 5 * time.Second, SystemActor: system})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	runner, err := application.NewInvestigationRunner(infra, infra, diagnoser, clock, ids)
	if err != nil {
		t.Fatal(err)
	}
	flow := applicationtest.Flow{Services: services, UnitOfWork: uow, Clock: clock, IDs: ids, Runner: runner, SystemActor: system, CleanupTimeout: 5 * time.Second}
	incident, err := flow.CreateIncident(ctx, application.CreateIncidentCommand{Target: target, Symptom: symptom, Actor: system})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := flow.Investigate(ctx, incident.ID()); err != nil {
		t.Fatal(err)
	}
	proposal, err := flow.Proposal(ctx, incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	return &approvePostgresHarness{
		services: services, infra: infra, incident: incident.ID(), queries: services.Queries,
		command: application.ApprovalCommand{IncidentID: incident.ID(), Actor: human, ActionID: proposal.ActionID(), ProposalHash: proposal.Hash()},
	}
}

func TestApproveAndExecuteConcurrentSameApprovalOnPostgres(t *testing.T) {
	h := newApprovePostgresHarness(t, nil)
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Go(func() {
			_, errs[i] = h.services.ApproveAndExecute.Run(context.Background(), h.command)
		})
	}
	wg.Wait()

	succeeded := 0
	for _, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, domain.ErrVersionConflict), errors.Is(err, domain.ErrIdempotencyConflict), errors.Is(err, domain.ErrIllegalTransition),
			// A racer that loaded the approved incident before another
			// request applied the rollback sees the target revision on
			// re-inspection; it reports a mismatch and leaves the incident
			// alone (the assertions below prove it still resolves).
			errors.Is(err, domain.ErrApprovalMismatch):
		default:
			t.Fatalf("Run() error = %v, want success or a conflict", err)
		}
	}
	if succeeded == 0 {
		if _, err := h.services.ApproveAndExecute.Run(context.Background(), h.command); err != nil {
			t.Fatalf("retry after conflicts: %v", err)
		}
	}
	summary, err := h.queries.GetIncidentSummary(context.Background(), applicationtest.Reader(), h.incident)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Incident.Status() != domain.StatusResolved || summary.Execution == nil {
		t.Fatalf("summary = %+v, want resolved with one execution", summary)
	}
	if got := h.infra.ExecutionCount(); got != 1 {
		t.Fatalf("mutations = %d, want 1", got)
	}
	events, err := h.queries.ListEvents(context.Background(), applicationtest.Reader(), h.incident)
	if err != nil {
		t.Fatal(err)
	}
	completions := 0
	for _, event := range events {
		if event.Kind() == domain.EventExecutionCompleted {
			completions++
		}
	}
	if completions != 1 {
		t.Fatalf("execution_completed events = %d, want 1", completions)
	}
}

func TestApproveAndExecuteRollsBackStateWithFailedAuditEventOnPostgres(t *testing.T) {
	faults := &eventFaultUnitOfWork{kind: domain.EventExecutionCompleted}
	h := newApprovePostgresHarness(t, func(store *UnitOfWork) ports.UnitOfWork {
		faults.inner = store
		return faults
	})

	if _, err := h.services.ApproveAndExecute.Run(context.Background(), h.command); !errors.Is(err, errInjectedAppend) {
		t.Fatalf("Run() error = %v, want the injected append failure", err)
	}
	summary, err := h.queries.GetIncidentSummary(context.Background(), applicationtest.Reader(), h.incident)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Incident.Status() != domain.StatusExecuting || summary.Execution != nil {
		t.Fatalf("summary = %+v, want executing with no committed execution", summary)
	}

	faults.healed = true
	summary, err = h.services.ApproveAndExecute.Run(context.Background(), h.command)
	if err != nil {
		t.Fatalf("retried Run() error = %v", err)
	}
	if summary.Incident.Status() != domain.StatusResolved || h.infra.ExecutionCount() != 1 {
		t.Fatalf("status = %s, mutations = %d, want resolved/1", summary.Incident.Status(), h.infra.ExecutionCount())
	}
}

var errInjectedAppend = errors.New("injected event append failure")

type eventFaultUnitOfWork struct {
	inner  ports.UnitOfWork
	kind   domain.EventKind
	healed bool
}

func (u *eventFaultUnitOfWork) WithinTransaction(ctx context.Context, fn func(ports.Repositories) error) error {
	if u.healed {
		return u.inner.WithinTransaction(ctx, fn)
	}
	return u.inner.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		return fn(eventFaultRepositories{Repositories: repositories, kind: u.kind})
	})
}

type eventFaultRepositories struct {
	ports.Repositories
	kind domain.EventKind
}

func (r eventFaultRepositories) Events() ports.EventRepository {
	return eventFaultEvents{EventRepository: r.Repositories.Events(), kind: r.kind}
}

type eventFaultEvents struct {
	ports.EventRepository
	kind domain.EventKind
}

func (e eventFaultEvents) Append(event domain.OperationEvent) error {
	if event.Kind() == e.kind {
		return errInjectedAppend
	}
	return e.EventRepository.Append(event)
}
