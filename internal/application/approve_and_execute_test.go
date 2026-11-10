package application

import (
	"context"
	"errors"
	"slices"
	"sync"
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

// approveHarness drives ApproveAndExecuteService against in-process fakes.
// The incident is awaiting approval of a valid proposal when the harness is
// returned; inspector hooks and executor scripts are armed afterwards so they
// do not affect the investigation.
type approveHarness struct {
	services  Services
	uow       *memory.UnitOfWork
	clock     *testkit.Clock
	infra     *fake.Infrastructure
	inspector *hookInspector
	executor  *spyExecutor
	incident  domain.IncidentID
	proposal  domain.RollbackProposal
	human     domain.Actor
	faults    *failingEventUnitOfWork
	t         *testing.T
}

type harnessOptions struct {
	approvalPolicy ports.ApprovalPolicy
	failEventKind  domain.EventKind
	// access, when set, replaces the permissive policy once the harness has
	// set up an incident awaiting approval.
	access AccessPolicy
}

const testApprovalTTL = 5 * time.Minute

func newApproveHarness(t *testing.T, options harnessOptions) *approveHarness {
	t.Helper()
	now := time.Date(2026, 8, 6, 9, 0, 0, 0, time.UTC)
	clock := testkit.NewClock(now)
	ids := &testkit.IDGenerator{}
	target := mustTarget(t, "local", "demo", "demo-api", "demo-api")
	symptom := mustSymptom(t, "http_error_rate", 0.35, 0.10, "ratio", now)
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
	approvalPolicy := options.approvalPolicy
	if approvalPolicy == nil {
		approvalPolicy = domain.ApprovalAuthorizer{}
	}
	uow := memory.NewUnitOfWork()
	var unitOfWork ports.UnitOfWork = uow
	var faults *failingEventUnitOfWork
	if options.failEventKind != 0 {
		faults = &failingEventUnitOfWork{inner: uow, kind: options.failEventKind}
		unitOfWork = faults
	}
	inspector := &hookInspector{next: infra}
	executor := &spyExecutor{next: infra, clock: clock}
	system := mustActor(t, "rollback-guardian", domain.ActorSystem)
	access := &switchablePolicy{current: allowAll{}}
	services, err := NewServices(Dependencies{
		UnitOfWork: unitOfWork, Inspector: inspector, Metrics: infra, Executor: executor,
		RollbackPolicy: policy, ApprovalPolicy: approvalPolicy, Access: access, Clock: clock, IDGenerator: ids,
	}, WorkflowConfig{ProposalTTL: 15 * time.Minute, ApprovalTTL: testApprovalTTL, CleanupTimeout: time.Second, SystemActor: system})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	runner, err := NewInvestigationRunner(inspector, infra, diagnoser, clock, ids)
	if err != nil {
		t.Fatal(err)
	}
	incident, err := services.Incidents.CreateIncidentAsync(ctx, CreateIncidentCommand{Target: target, Symptom: symptom, Actor: system})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Investigate(ctx, ports.InvestigationRequest{Incident: incident})
	if err != nil {
		t.Fatal(err)
	}
	incident, err = services.Investigations.Complete(ctx, incident.ID(), result)
	if err != nil {
		t.Fatal(err)
	}
	if incident.Status() != domain.StatusAwaitingApproval {
		t.Fatalf("incident status = %s, want awaiting_approval", incident.Status())
	}
	summary, err := services.Queries.GetIncidentSummary(ctx, testReader(), incident.ID())
	if err != nil || summary.Proposal == nil {
		t.Fatalf("proposal after investigation = %v, %v", summary.Proposal, err)
	}
	proposal := *summary.Proposal
	if options.access != nil {
		access.current = options.access
	}
	return &approveHarness{
		services: services, uow: uow, clock: clock, infra: infra, inspector: inspector, executor: executor,
		incident: incident.ID(), proposal: proposal, human: mustActor(t, "alice", domain.ActorHuman), faults: faults, t: t,
	}
}

func (h *approveHarness) command() ApprovalCommand {
	return ApprovalCommand{IncidentID: h.incident, Actor: h.human, ActionID: h.proposal.ActionID(), ProposalHash: h.proposal.Hash()}
}

func (h *approveHarness) status(t *testing.T) domain.Status {
	t.Helper()
	incident, err := loadIncident(t, h.uow, h.incident)
	if err != nil {
		t.Fatal(err)
	}
	return incident.Status()
}

func (h *approveHarness) events(t *testing.T) []domain.OperationEvent {
	t.Helper()
	events, err := h.services.Queries.ListEvents(context.Background(), testReader(), h.incident)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func (h *approveHarness) approvalID(t *testing.T) domain.ApprovalID {
	t.Helper()
	summary, err := h.services.Queries.GetIncidentSummary(context.Background(), testReader(), h.incident)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Approval == nil {
		t.Fatal("no approval recorded")
	}
	return summary.Approval.ID()
}

func TestApproveAndExecuteResolvesAndReplaysWithoutSecondMutation(t *testing.T) {
	t.Parallel()
	h := newApproveHarness(t, harnessOptions{})

	summary, err := h.services.ApproveAndExecute.Run(context.Background(), h.command())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if summary.Incident.Status() != domain.StatusResolved || summary.Execution == nil || summary.Verification == nil || !summary.Verification.Passed() {
		t.Fatalf("summary = %+v, want resolved with execution and passed verification", summary)
	}
	wantKey := domain.IdempotencyKey("approval:" + string(h.approvalID(t)))
	calls := h.executor.snapshot()
	if len(calls) != 1 || calls[0].key != wantKey || calls[0].correlationKey != string(wantKey) {
		t.Fatalf("executor calls = %+v, want one call with key %q in the correlation", calls, wantKey)
	}
	assertAuthorizedCall(t, calls[0], h.proposal, h.human)

	replay, err := h.services.ApproveAndExecute.Run(context.Background(), h.command())
	if err != nil {
		t.Fatalf("replayed Run() error = %v", err)
	}
	if replay.Incident.Status() != domain.StatusResolved || replay.Execution == nil || replay.Execution.ID() != summary.Execution.ID() {
		t.Fatalf("replay summary = %+v, want the first resolved outcome", replay)
	}
	if got := h.infra.ExecutionCount(); got != 1 {
		t.Fatalf("mutations = %d, want exactly one for one approval", got)
	}
	if got := len(h.executor.snapshot()); got != 1 {
		t.Fatalf("executor calls after replay = %d, want 1: a completed key is answered from the record", got)
	}
	assertEventKinds(t, h.events(t), []domain.EventKind{
		domain.EventApprovalRecorded, domain.EventStatusChanged, // awaiting_approval -> approved
		domain.EventStatusChanged,         // approved -> executing
		domain.EventExecutionCompleted,    // execution recorded
		domain.EventStatusChanged,         // executing -> verifying
		domain.EventVerificationCompleted, // verification recorded
		domain.EventStatusChanged,         // verifying -> resolved
	})
}

func TestApproveAndExecuteFailureBranches(t *testing.T) {
	t.Parallel()
	agent, err := domain.NewActor("investigator", domain.ActorAgent)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		options harnessOptions
		arm     func(*approveHarness)
		command func(*approveHarness) ApprovalCommand
		// wantErr is nil when the failure is a committed outcome that Run
		// reports as a summary.
		wantErr       error
		wantStatus    domain.Status
		wantExecution *domain.ExecutionOutcome
		wantVerified  *bool
		wantMutations int
		// wantCalls is the number of executor calls; zero proves the rejection
		// happened before the executor was reached.
		wantCalls     int
		wantLastEvent domain.EventKind
	}{
		{
			name: "proposal hash mismatch",
			command: func(h *approveHarness) ApprovalCommand {
				command := h.command()
				command.ProposalHash = "sha256-other"
				return command
			},
			wantErr: domain.ErrApprovalMismatch, wantStatus: domain.StatusAwaitingApproval,
		},
		{
			name: "action mismatch",
			command: func(h *approveHarness) ApprovalCommand {
				command := h.command()
				command.ActionID = "action-other"
				return command
			},
			wantErr: domain.ErrApprovalMismatch, wantStatus: domain.StatusAwaitingApproval,
		},
		{
			name: "non-human approver",
			command: func(h *approveHarness) ApprovalCommand {
				command := h.command()
				command.Actor = agent
				return command
			},
			wantErr: domain.ErrNonHumanApproval, wantStatus: domain.StatusAwaitingApproval,
		},
		{
			name:    "proposal expired before approval",
			arm:     func(h *approveHarness) { h.clock.Advance(16 * time.Minute) },
			wantErr: domain.ErrExpired, wantStatus: domain.StatusExpired, wantLastEvent: domain.EventIncidentExpired,
		},
		{
			name: "approval expired before execution",
			arm: func(h *approveHarness) {
				h.inspector.set(func(*domain.DeploymentEvidence) { h.clock.Advance(testApprovalTTL + time.Second) })
			},
			wantErr: domain.ErrExpired, wantStatus: domain.StatusExpired, wantLastEvent: domain.EventIncidentExpired,
		},
		{
			name: "current revision drifted before execution",
			arm: func(h *approveHarness) {
				h.inspector.set(func(deployment *domain.DeploymentEvidence) { deployment.CurrentRevision = "revision-3" })
			},
			wantErr: domain.ErrApprovalMismatch, wantStatus: domain.StatusFailed, wantLastEvent: domain.EventIncidentFailed,
		},
		{
			name: "target identity changed before execution",
			arm: func(h *approveHarness) {
				other := h.proposal.Target()
				h.inspector.set(func(deployment *domain.DeploymentEvidence) {
					changed, err := domain.NewDeploymentTarget(other.Cluster(), other.Namespace(), "other-api", other.Service())
					if err == nil {
						deployment.Target = changed
					}
				})
			},
			wantErr: domain.ErrApprovalMismatch, wantStatus: domain.StatusFailed, wantLastEvent: domain.EventIncidentFailed,
		},
		{
			name:    "policy denies at execution",
			options: harnessOptions{approvalPolicy: rejectingApprovalPolicy{err: &domain.PolicyDenialError{Rule: "test"}}},
			wantErr: domain.ErrPolicyDenied, wantStatus: domain.StatusFailed, wantLastEvent: domain.EventIncidentFailed,
		},
		{
			name: "incident already rejected",
			arm: func(h *approveHarness) {
				if _, err := h.services.Approvals.Reject(context.Background(), RejectionCommand{
					IncidentID: h.incident, Actor: h.human, ActionID: h.proposal.ActionID(), ProposalHash: h.proposal.Hash(), Reason: "not now",
				}); err != nil {
					h.t.Fatal(err)
				}
			},
			wantErr: domain.ErrApprovalMismatch, wantStatus: domain.StatusRejected, wantLastEvent: domain.EventStatusChanged,
		},
		{
			name: "reinspection unavailable",
			arm: func(h *approveHarness) {
				h.inspector.setErr(errReadonlyUnavailable)
			},
			wantErr: errReadonlyUnavailable, wantStatus: domain.StatusApproved, wantLastEvent: domain.EventStatusChanged,
		},
		{
			name: "executor rejects at the mutation boundary",
			arm: func(h *approveHarness) {
				h.infra.BeforeMutation = func(deployment *domain.DeploymentEvidence) { deployment.CurrentRevision = "revision-3" }
			},
			wantCalls: 1, wantStatus: domain.StatusFailed, wantExecution: new(domain.ExecutionFailed), wantLastEvent: domain.EventIncidentFailed,
		},
		{
			name:          "executor reports a failed rollback",
			arm:           func(h *approveHarness) { h.infra.ExecuteErr = errors.New("rollout stalled") },
			wantStatus:    domain.StatusFailed,
			wantExecution: new(domain.ExecutionFailed), wantMutations: 1, wantCalls: 1, wantLastEvent: domain.EventIncidentFailed,
		},
		{
			name:      "rollback outcome unknown",
			arm:       func(h *approveHarness) { h.executor.script(ports.ErrRollbackOutcomeUnknown) },
			wantErr:   ports.ErrRollbackOutcomeUnknown,
			wantCalls: 1, wantStatus: domain.StatusExecuting, wantLastEvent: domain.EventStatusChanged,
		},
		{
			name:          "recovery verification fails",
			arm:           func(h *approveHarness) { h.infra.AfterExecute = nil },
			wantStatus:    domain.StatusFailed,
			wantExecution: new(domain.ExecutionSucceeded), wantVerified: new(false), wantMutations: 1, wantCalls: 1, wantLastEvent: domain.EventIncidentFailed,
		},
		{
			name: "recovery metrics unavailable",
			arm: func(h *approveHarness) {
				h.infra.AfterExecute = func() { h.infra.MetricErr = errMetricsUnavailable }
			},
			wantErr: errMetricsUnavailable, wantStatus: domain.StatusVerifying,
			wantExecution: new(domain.ExecutionSucceeded), wantMutations: 1, wantCalls: 1, wantLastEvent: domain.EventStatusChanged,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newApproveHarness(t, test.options)
			if test.arm != nil {
				test.arm(h)
			}
			command := h.command()
			if test.command != nil {
				command = test.command(h)
			}

			summary, err := h.services.ApproveAndExecute.Run(context.Background(), command)

			switch {
			case test.wantErr == nil && err != nil:
				t.Fatalf("Run() error = %v, want a committed outcome", err)
			case test.wantErr != nil && !errors.Is(err, test.wantErr):
				t.Fatalf("Run() error = %v, want %v", err, test.wantErr)
			}
			if err == nil && summary.Incident.Status() != test.wantStatus {
				t.Fatalf("summary status = %s, want %s", summary.Incident.Status(), test.wantStatus)
			}
			if got := h.status(t); got != test.wantStatus {
				t.Fatalf("stored status = %s, want %s", got, test.wantStatus)
			}
			stored, storedErr := h.services.Queries.GetIncidentSummary(context.Background(), testReader(), h.incident)
			if storedErr != nil {
				t.Fatal(storedErr)
			}
			switch {
			case test.wantExecution == nil && stored.Execution != nil:
				t.Fatalf("execution = %+v, want none", stored.Execution)
			case test.wantExecution != nil && (stored.Execution == nil || stored.Execution.Outcome() != *test.wantExecution):
				t.Fatalf("execution = %+v, want outcome %v", stored.Execution, *test.wantExecution)
			}
			switch {
			case test.wantVerified == nil && stored.Verification != nil && test.wantStatus != domain.StatusResolved:
				t.Fatalf("verification = %+v, want none", stored.Verification)
			case test.wantVerified != nil && (stored.Verification == nil || stored.Verification.Passed() != *test.wantVerified):
				t.Fatalf("verification = %+v, want passed=%v", stored.Verification, *test.wantVerified)
			}
			if got := h.infra.ExecutionCount(); got != test.wantMutations {
				t.Fatalf("mutations = %d, want %d", got, test.wantMutations)
			}
			calls := h.executor.snapshot()
			if len(calls) != test.wantCalls {
				t.Fatalf("executor calls = %d, want %d", len(calls), test.wantCalls)
			}
			for _, call := range calls {
				assertAuthorizedCall(t, call, h.proposal, h.human)
			}
			if test.wantLastEvent != 0 {
				events := h.events(t)
				if last := events[len(events)-1]; last.Kind() != test.wantLastEvent {
					t.Fatalf("last event = %s, want %s", last.Kind(), test.wantLastEvent)
				}
			}
		})
	}
}

func TestApproveAndExecuteRetryOfSameApprovalReconcilesUnknownOutcome(t *testing.T) {
	t.Parallel()
	h := newApproveHarness(t, harnessOptions{})
	h.executor.script(ports.ErrRollbackOutcomeUnknown)

	if _, err := h.services.ApproveAndExecute.Run(context.Background(), h.command()); !errors.Is(err, ports.ErrRollbackOutcomeUnknown) {
		t.Fatalf("first Run() error = %v, want unknown outcome", err)
	}
	summary, err := h.services.ApproveAndExecute.Run(context.Background(), h.command())
	if err != nil {
		t.Fatalf("retried Run() error = %v", err)
	}
	if summary.Incident.Status() != domain.StatusResolved {
		t.Fatalf("status after retry = %s, want resolved", summary.Incident.Status())
	}
	calls := h.executor.snapshot()
	if len(calls) != 2 || calls[0].key != calls[1].key {
		t.Fatalf("executor calls = %+v, want two calls with one idempotency key", calls)
	}
	if got := h.infra.ExecutionCount(); got != 1 {
		t.Fatalf("mutations = %d, want 1", got)
	}
}

func TestApproveAndExecuteDifferentApprovalCannotReuseCompletedExecution(t *testing.T) {
	t.Parallel()
	h := newApproveHarness(t, harnessOptions{})
	if _, err := h.services.ApproveAndExecute.Run(context.Background(), h.command()); err != nil {
		t.Fatal(err)
	}
	other := h.command()
	other.Actor = mustActor(t, "bob", domain.ActorHuman)

	_, err := h.services.ApproveAndExecute.Run(context.Background(), other)

	if !errors.Is(err, domain.ErrApprovalMismatch) {
		t.Fatalf("second approver error = %v, want approval mismatch", err)
	}
	if got := h.infra.ExecutionCount(); got != 1 {
		t.Fatalf("mutations = %d, want 1", got)
	}
}

// Each step's audit event is written in the same transaction as its state
// change: when the event cannot be appended, the state change is not
// committed either.
func TestApproveAndExecuteAuditEventsCommitWithTheirStateChange(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		failKind      domain.EventKind
		wantStatus    domain.Status
		wantExecution bool
	}{
		{name: "approval", failKind: domain.EventApprovalRecorded, wantStatus: domain.StatusAwaitingApproval},
		{name: "execution result", failKind: domain.EventExecutionCompleted, wantStatus: domain.StatusExecuting},
		{name: "verification result", failKind: domain.EventVerificationCompleted, wantStatus: domain.StatusVerifying, wantExecution: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newApproveHarness(t, harnessOptions{failEventKind: test.failKind})

			if _, err := h.services.ApproveAndExecute.Run(context.Background(), h.command()); !errors.Is(err, errEventAppendFailed) {
				t.Fatalf("Run() error = %v, want the append failure", err)
			}
			if got := h.status(t); got != test.wantStatus {
				t.Fatalf("status = %s, want %s: the state change must roll back with its event", got, test.wantStatus)
			}
			summary, err := h.services.Queries.GetIncidentSummary(context.Background(), testReader(), h.incident)
			if err != nil {
				t.Fatal(err)
			}
			if (summary.Execution != nil) != test.wantExecution {
				t.Fatalf("execution = %+v, want present=%v", summary.Execution, test.wantExecution)
			}
			if summary.Verification != nil {
				t.Fatalf("verification = %+v, want none", summary.Verification)
			}
			if test.failKind == domain.EventApprovalRecorded && summary.Approval != nil {
				t.Fatalf("approval = %+v, want none", summary.Approval)
			}
			for _, event := range h.events(t) {
				if event.Kind() == test.failKind {
					t.Fatalf("event %s committed although its append failed", test.failKind)
				}
			}
		})
	}
}

// A step whose result could not be committed is retried with the same
// approval: the retry finishes the incident and the executor's idempotency
// key keeps the mutation to one.
func TestApproveAndExecuteRetryAfterUncommittedStepMutatesOnce(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		failKind  domain.EventKind
		wantCalls int
	}{
		// The execution result was lost: the retry resumes the pending
		// attempt and calls the executor again with the same key.
		{name: "execution result", failKind: domain.EventExecutionCompleted, wantCalls: 2},
		// The execution is recorded: the retry replays it from the record
		// and only verifies.
		{name: "verification result", failKind: domain.EventVerificationCompleted, wantCalls: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newApproveHarness(t, harnessOptions{failEventKind: test.failKind})
			if _, err := h.services.ApproveAndExecute.Run(context.Background(), h.command()); !errors.Is(err, errEventAppendFailed) {
				t.Fatalf("first Run() error = %v, want the append failure", err)
			}
			h.faults.healed.Store(true)

			summary, err := h.services.ApproveAndExecute.Run(context.Background(), h.command())

			if err != nil {
				t.Fatalf("retried Run() error = %v", err)
			}
			if summary.Incident.Status() != domain.StatusResolved {
				t.Fatalf("status = %s, want resolved", summary.Incident.Status())
			}
			if got := h.infra.ExecutionCount(); got != 1 {
				t.Fatalf("mutations = %d, want 1", got)
			}
			calls := h.executor.snapshot()
			if len(calls) != test.wantCalls {
				t.Fatalf("executor calls = %d, want %d", len(calls), test.wantCalls)
			}
			for _, call := range calls {
				if call.key != calls[0].key {
					t.Fatalf("executor keys = %q and %q, want one key per approval", calls[0].key, call.key)
				}
			}
		})
	}
}

func TestApproveAndExecuteRetryAfterVerificationErrorVerifiesWithoutSecondMutation(t *testing.T) {
	t.Parallel()
	h := newApproveHarness(t, harnessOptions{})
	h.infra.AfterExecute = func() { h.infra.MetricErr = errMetricsUnavailable }
	if _, err := h.services.ApproveAndExecute.Run(context.Background(), h.command()); !errors.Is(err, errMetricsUnavailable) {
		t.Fatalf("first Run() error = %v, want metrics unavailable", err)
	}
	h.infra.MetricErr = nil
	h.infra.SetRecoveryMetric(0.02)

	summary, err := h.services.ApproveAndExecute.Run(context.Background(), h.command())

	if err != nil {
		t.Fatalf("retried Run() error = %v", err)
	}
	if summary.Incident.Status() != domain.StatusResolved || summary.Verification == nil || !summary.Verification.Passed() {
		t.Fatalf("summary = %+v, want resolved after verification", summary)
	}
	if got, calls := h.infra.ExecutionCount(), len(h.executor.snapshot()); got != 1 || calls != 1 {
		t.Fatalf("mutations = %d, executor calls = %d, want 1/1", got, calls)
	}
}

// Two requests carrying the same approval race. However they interleave,
// the deployment is mutated once, every executor call carries the one key,
// and the incident ends resolved.
func TestApproveAndExecuteConcurrentSameApprovalMutatesOnce(t *testing.T) {
	t.Parallel()
	for attempt := range 20 {
		h := newApproveHarness(t, harnessOptions{})
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i := range errs {
			wg.Go(func() {
				_, errs[i] = h.services.ApproveAndExecute.Run(context.Background(), h.command())
			})
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil && !errors.Is(err, domain.ErrVersionConflict) && !errors.Is(err, domain.ErrIdempotencyConflict) && !errors.Is(err, domain.ErrIllegalTransition) && !errors.Is(err, domain.ErrApprovalMismatch) {
				t.Fatalf("attempt %d: Run() error = %v, want success or a conflict", attempt, err)
			}
		}
		if errs[0] != nil && errs[1] != nil {
			// Both lost a race; the approval is recorded, so a retry finishes.
			if _, err := h.services.ApproveAndExecute.Run(context.Background(), h.command()); err != nil {
				t.Fatalf("attempt %d: retry after two conflicts: %v", attempt, err)
			}
		}
		if got := h.status(t); got != domain.StatusResolved {
			t.Fatalf("attempt %d: status = %s, want resolved", attempt, got)
		}
		if got := h.infra.ExecutionCount(); got != 1 {
			t.Fatalf("attempt %d: mutations = %d, want 1", attempt, got)
		}
		calls := h.executor.snapshot()
		for _, call := range calls {
			if call.key != calls[0].key {
				t.Fatalf("attempt %d: executor keys %q and %q", attempt, calls[0].key, call.key)
			}
		}
	}
}

// A double-submitted approval. Request B loads the approved incident; then
// request A starts the execution and the rollback is applied while A is still
// waiting on the executor; only then does B re-inspect. B sees the target
// revision and its authorization fails, but its view is stale: it must not
// fail the executing incident, so A completes and resolves it, with one
// mutation.
func TestApproveAndExecuteStaleConcurrentRequestDoesNotFailAppliedRollback(t *testing.T) {
	t.Parallel()
	h := newApproveHarness(t, harnessOptions{})
	applied, proceed, aDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var aErr error
	h.executor.afterFirstCall(func() {
		close(applied)
		<-proceed
	})
	h.inspector.set(func(deployment *domain.DeploymentEvidence) {
		h.inspector.set(nil) // B's re-inspection; A's runs unhooked
		go func() {
			defer close(aDone)
			_, aErr = h.services.ApproveAndExecute.Run(context.Background(), h.command())
		}()
		<-applied
		if fresh, err := h.infra.InspectDeployment(context.Background(), deployment.Target); err == nil {
			*deployment = fresh
		}
	})

	_, err := h.services.ApproveAndExecute.Run(context.Background(), h.command())
	close(proceed)
	<-aDone

	if aErr != nil {
		t.Fatalf("request A Run() error = %v, want it to complete the rollback", aErr)
	}
	if !errors.Is(err, domain.ErrApprovalMismatch) {
		t.Fatalf("stale Run() error = %v, want the authorization mismatch", err)
	}
	if got := h.status(t); got != domain.StatusResolved {
		t.Fatalf("status = %s, want resolved: a stale request must not fail it", got)
	}
	if got := h.infra.ExecutionCount(); got != 1 {
		t.Fatalf("mutations = %d, want 1", got)
	}
	for _, event := range h.events(t) {
		if event.Kind() == domain.EventIncidentFailed {
			t.Fatal("stale request appended incident_failed")
		}
	}
}

// When the terminal failure event cannot be appended, the failure is not
// committed, so Run must report the error rather than a committed outcome.
func TestApproveAndExecuteUncommittedFailureIsAnError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		failKind   domain.EventKind
		arm        func(*approveHarness)
		wantStatus domain.Status
	}{
		{name: "failed execution", failKind: domain.EventIncidentFailed, arm: func(h *approveHarness) { h.infra.ExecuteErr = errors.New("rollout stalled") }, wantStatus: domain.StatusExecuting},
		{name: "failed verification", failKind: domain.EventIncidentFailed, arm: func(h *approveHarness) { h.infra.AfterExecute = nil }, wantStatus: domain.StatusVerifying},
		{name: "expired approval", failKind: domain.EventIncidentExpired, arm: func(h *approveHarness) {
			h.inspector.set(func(*domain.DeploymentEvidence) { h.clock.Advance(testApprovalTTL + time.Second) })
		}, wantStatus: domain.StatusApproved},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newApproveHarness(t, harnessOptions{failEventKind: test.failKind})
			test.arm(h)

			if _, err := h.services.ApproveAndExecute.Run(context.Background(), h.command()); !errors.Is(err, errEventAppendFailed) {
				t.Fatalf("Run() error = %v, want the append failure", err)
			}
			if got := h.status(t); got != test.wantStatus {
				t.Fatalf("status = %s, want %s", got, test.wantStatus)
			}
		})
	}
}

func TestExecutionKeyForApprovalIsStableAndNamespaced(t *testing.T) {
	t.Parallel()
	if got := executionKeyForApproval("approval-7"); got != "approval:approval-7" {
		t.Fatalf("key = %q", got)
	}
	if executionKeyForApproval("a") == executionKeyForApproval("b") {
		t.Fatal("distinct approvals share an execution key")
	}
}

// assertAuthorizedCall checks that what reached the executor is the approved
// rollback of exactly this proposal, approved by a human, unexpired when the
// executor was called.
func assertAuthorizedCall(t *testing.T, call executorCall, proposal domain.RollbackProposal, approver domain.Actor) {
	t.Helper()
	got := call.approved
	if got.Proposal().Hash() != proposal.Hash() || got.Proposal().Target() != proposal.Target() || got.Proposal().CurrentRevision() != proposal.CurrentRevision() || got.Proposal().TargetRevision() != proposal.TargetRevision() {
		t.Fatalf("executor received proposal %+v, want %+v", got.Proposal(), proposal)
	}
	approval := got.Approval()
	if approval.ProposalHash() != proposal.Hash() || approval.ActionID() != proposal.ActionID() || approval.Decision() != domain.ApprovalApproved || approval.Actor() != approver {
		t.Fatalf("executor received approval %+v, want approval of %s by %v", approval, proposal.Hash(), approver)
	}
	if approval.Expired(call.at) || proposal.Expired(call.at) {
		t.Fatalf("executor called at %s with an expired approval or proposal", call.at)
	}
	if !got.PolicyDecision().Allowed() || got.PolicyDecision().ID() != proposal.PolicyDecisionID() {
		t.Fatalf("executor received policy decision %+v", got.PolicyDecision())
	}
	if call.key == "" {
		t.Fatal("executor called without an idempotency key")
	}
}

func assertEventKinds(t *testing.T, events []domain.OperationEvent, wantTail []domain.EventKind) {
	t.Helper()
	if len(events) < len(wantTail) {
		t.Fatalf("events = %d, want at least %d", len(events), len(wantTail))
	}
	got := make([]domain.EventKind, 0, len(wantTail))
	for _, event := range events[len(events)-len(wantTail):] {
		got = append(got, event.Kind())
	}
	if !slices.Equal(got, wantTail) {
		t.Fatalf("event kinds = %v, want %v", got, wantTail)
	}
}

var (
	errReadonlyUnavailable = errors.New("readonly MCP unavailable")
	errMetricsUnavailable  = errors.New("prometheus unavailable")
)

type hookInspector struct {
	mu   sync.Mutex
	next ports.DeploymentInspector
	hook func(*domain.DeploymentEvidence)
	err  error
}

func (i *hookInspector) set(hook func(*domain.DeploymentEvidence)) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.hook = hook
}

func (i *hookInspector) setErr(err error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.err = err
}

func (i *hookInspector) InspectDeployment(ctx context.Context, target domain.DeploymentTarget) (domain.DeploymentEvidence, error) {
	i.mu.Lock()
	hook, err := i.hook, i.err
	i.mu.Unlock()
	if err != nil {
		return domain.DeploymentEvidence{}, err
	}
	evidence, inspectErr := i.next.InspectDeployment(ctx, target)
	if inspectErr != nil || hook == nil {
		return evidence, inspectErr
	}
	hook(&evidence)
	return evidence, nil
}

type executorCall struct {
	approved       domain.ApprovedRollback
	key            domain.IdempotencyKey
	correlationKey string
	at             time.Time
}

// spyExecutor records every call and returns scripted errors (one per call,
// in order) before delegating to the fake infrastructure.
type spyExecutor struct {
	mu      sync.Mutex
	next    ports.RollbackExecutor
	clock   ports.Clock
	calls   []executorCall
	scripts []error
	after   func()
}

func (e *spyExecutor) script(errs ...error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.scripts = append(e.scripts, errs...)
}

func (e *spyExecutor) snapshot() []executorCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.calls)
}

func (e *spyExecutor) ExecuteApprovedRollback(ctx context.Context, approved domain.ApprovedRollback, key domain.IdempotencyKey) (domain.RollbackExecution, error) {
	e.mu.Lock()
	e.calls = append(e.calls, executorCall{approved: approved, key: key, correlationKey: ports.CorrelationFromContext(ctx).IdempotencyKey, at: e.clock.Now()})
	var scripted error
	if len(e.scripts) > 0 {
		scripted, e.scripts = e.scripts[0], e.scripts[1:]
	}
	e.mu.Unlock()
	if scripted != nil {
		return domain.RollbackExecution{}, scripted
	}
	execution, err := e.next.ExecuteApprovedRollback(ctx, approved, key)
	e.mu.Lock()
	after := e.after
	e.after = nil
	e.mu.Unlock()
	if after != nil {
		after()
	}
	return execution, err
}

// afterFirstCall runs hook once, after the first delegated call has applied
// the rollback and before the executor returns.
func (e *spyExecutor) afterFirstCall(hook func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.after = hook
}

var errEventAppendFailed = errors.New("event append failed")

// failingEventUnitOfWork fails every append of one event kind, so a test can
// prove the state change in the same transaction is rolled back.
type failingEventUnitOfWork struct {
	inner ports.UnitOfWork
	kind  domain.EventKind
	// healed turns the fault off, so a test can retry after it.
	healed atomic.Bool
}

func (u *failingEventUnitOfWork) WithinTransaction(ctx context.Context, fn func(ports.Repositories) error) error {
	if u.healed.Load() {
		return u.inner.WithinTransaction(ctx, fn)
	}
	return u.inner.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		return fn(failingEventRepositories{Repositories: repositories, kind: u.kind})
	})
}

type failingEventRepositories struct {
	ports.Repositories
	kind domain.EventKind
}

func (r failingEventRepositories) Events() ports.EventRepository {
	return failingEventRepository{EventRepository: r.Repositories.Events(), kind: r.kind}
}

type failingEventRepository struct {
	ports.EventRepository
	kind domain.EventKind
}

func (r failingEventRepository) Append(event domain.OperationEvent) error {
	if event.Kind() == r.kind {
		return errEventAppendFailed
	}
	return r.EventRepository.Append(event)
}
