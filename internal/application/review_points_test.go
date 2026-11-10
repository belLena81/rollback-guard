package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/fake"
	"github.com/belLena81/rollback-guard/internal/adapters/memory"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/belLena81/rollback-guard/internal/testkit"
)

func TestRecoveredPendingExecutionRetryInvokesExecutorBeforeCompletion(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 3, 13, 0, 0, 0, time.UTC)
	uow := memory.NewUnitOfWork()
	incidentID, proposal := seedExecutingRollbackState(t, uow, now, now.Add(5*time.Minute))
	systemActor := mustActor(t, "guardian", domain.ActorSystem)
	clock := testkit.NewClock(now)
	policy := &countingApprovalPolicy{}
	inspector := &fake.Infrastructure{
		Deployment: domain.DeploymentEvidence{
			Target:                 proposal.Target(),
			CurrentRevision:        proposal.TargetRevision(),
			PreviousStableRevision: proposal.CurrentRevision(),
			DesiredReplicas:        1,
			ReadyReplicas:          1,
			AvailableReplicas:      1,
			Healthy:                true,
			RolloutAt:              now.Add(-time.Minute),
		},
	}
	executor := &successfulRollbackExecutor{
		executionID: "executor-execution",
		startedAt:   now.Add(-time.Minute),
		completedAt: now,
	}
	svc := &ExecutionService{
		uow:            uow,
		inspector:      inspector,
		executor:       executor,
		approvalPolicy: policy,
		clock:          clock,
		audit:          audit{ids: &testkit.IDGenerator{}, systemActor: systemActor},
		cleanupTimeout: time.Minute,
	}

	clock.Advance(20 * time.Minute)
	execution, err := svc.Execute(context.Background(), incidentID, "execute-1")
	if err != nil {
		t.Fatalf("resume execution error = %v, want nil", err)
	}
	if execution.Outcome() != domain.ExecutionSucceeded {
		t.Fatalf("resume execution outcome = %s, want succeeded", execution.Outcome())
	}
	if policy.calls != 1 {
		t.Fatalf("approval policy calls = %d, want 1", policy.calls)
	}
	if executor.calls != 1 {
		t.Fatalf("executor calls = %d, want 1", executor.calls)
	}
	if executor.key != "execute-1" {
		t.Fatalf("executor key = %q, want execute-1", executor.key)
	}
	if executor.approved.Proposal().Hash() != proposal.Hash() {
		t.Fatalf("executor proposal hash = %s, want %s", executor.approved.Proposal().Hash(), proposal.Hash())
	}

	incident, err := loadIncident(t, uow, incidentID)
	if err != nil {
		t.Fatal(err)
	}
	if incident.Status() != domain.StatusVerifying {
		t.Fatalf("incident status = %s, want verifying", incident.Status())
	}
	stored, err := loadExecution(t, uow, incidentID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Outcome() != domain.ExecutionSucceeded {
		t.Fatalf("stored execution outcome = %s, want succeeded", stored.Outcome())
	}
	if stored.ID() != executor.executionID {
		t.Fatalf("stored execution ID = %s, want executor result", stored.ID())
	}
}

func TestExecutorFailureReasonIsRedactedBeforePersistence(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 3, 14, 0, 0, 0, time.UTC)
	uow := memory.NewUnitOfWork()
	incidentID, proposal := seedApprovedRollbackState(t, uow, now)
	systemActor := mustActor(t, "guardian", domain.ActorSystem)
	inspector := &fake.Infrastructure{
		Deployment: domain.DeploymentEvidence{
			Target:                 proposal.Target(),
			CurrentRevision:        proposal.CurrentRevision(),
			PreviousStableRevision: proposal.TargetRevision(),
			RolloutAt:              now.Add(-time.Minute),
			DesiredReplicas:        3,
			ReadyReplicas:          3,
			AvailableReplicas:      3,
			Healthy:                true,
		},
	}
	executor := &failingRollbackExecutor{err: errors.New("executor leaked secret token=abc123")}
	svc := &ExecutionService{
		uow:            uow,
		inspector:      inspector,
		executor:       executor,
		approvalPolicy: domain.ApprovalAuthorizer{},
		clock:          testkit.NewClock(now),
		audit:          audit{ids: &testkit.IDGenerator{}, systemActor: systemActor},
		cleanupTimeout: time.Minute,
	}

	execution, err := svc.Execute(context.Background(), incidentID, "execute-2")
	if err == nil {
		t.Fatal("Execute() error = nil, want failure")
	}
	if execution.Outcome() != domain.ExecutionFailed {
		t.Fatalf("execution outcome = %s, want failed", execution.Outcome())
	}
	if execution.FailureReason() != executionFailureAuditReason {
		t.Fatalf("execution failure reason = %q, want %q", execution.FailureReason(), executionFailureAuditReason)
	}

	stored, event := loadExecutionAndFailureEvent(t, uow, incidentID)
	if stored.FailureReason() != executionFailureAuditReason {
		t.Fatalf("persisted failure reason = %q, want %q", stored.FailureReason(), executionFailureAuditReason)
	}
	detail, ok := event.Detail().(domain.TerminalDetail)
	if !ok {
		t.Fatalf("failure event detail type = %T, want domain.TerminalDetail", event.Detail())
	}
	if detail.Reason != executionFailureAuditReason {
		t.Fatalf("failure event reason = %q, want %q", detail.Reason, executionFailureAuditReason)
	}
}

func TestUnknownRollbackOutcomeLeavesDurableAttemptPending(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 4, 14, 0, 0, 0, time.UTC)
	uow := memory.NewUnitOfWork()
	incidentID, proposal := seedApprovedRollbackState(t, uow, now)
	systemActor := mustActor(t, "guardian", domain.ActorSystem)
	inspector := &fake.Infrastructure{Deployment: domain.DeploymentEvidence{Target: proposal.Target(), CurrentRevision: proposal.CurrentRevision(), PreviousStableRevision: proposal.TargetRevision(), RolloutAt: now.Add(-time.Minute)}}
	svc := &ExecutionService{audit: audit{ids: &testkit.IDGenerator{}, systemActor: systemActor}, uow: uow, inspector: inspector, executor: &failingRollbackExecutor{err: ports.ErrRollbackOutcomeUnknown}, approvalPolicy: domain.ApprovalAuthorizer{}, clock: testkit.NewClock(now), cleanupTimeout: time.Minute}

	_, err := svc.Execute(context.Background(), incidentID, "unknown-outcome")
	if !errors.Is(err, ports.ErrRollbackOutcomeUnknown) {
		t.Fatalf("Execute() error = %v, want unknown outcome", err)
	}
	incident, err := loadIncident(t, uow, incidentID)
	if err != nil {
		t.Fatal(err)
	}
	if incident.Status() != domain.StatusExecuting {
		t.Fatalf("incident status = %s, want executing", incident.Status())
	}
	if _, err := loadExecution(t, uow, incidentID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("execution lookup error = %v, want not found", err)
	}
	err = uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
		record, getErr := repositories.Idempotency().Get("unknown-outcome")
		if getErr != nil {
			return getErr
		}
		if record.State() != domain.IdempotencyPending {
			return errors.New("idempotency attempt is not pending")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestApprovalRetryAfterProposalExpiryReturnsExpiry(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	uow := memory.NewUnitOfWork()
	incidentID, proposal := seedAwaitingApprovalIncidentWithExpiredProposal(t, uow, now)
	systemActor := mustActor(t, "guardian", domain.ActorSystem)
	svc := &ApprovalService{
		uow:         uow,
		clock:       testkit.NewClock(now),
		audit:       audit{ids: &testkit.IDGenerator{}, systemActor: systemActor},
		approvalTTL: time.Minute,
		access:      allowAll{},
	}
	command := ApprovalCommand{
		IncidentID:   incidentID,
		Actor:        mustActor(t, "alice", domain.ActorHuman),
		ActionID:     proposal.ActionID(),
		ProposalHash: proposal.Hash(),
	}
	retryCommand := command

	_, err := svc.Approve(context.Background(), command)
	if !errors.Is(err, domain.ErrExpired) {
		t.Fatalf("first approval attempt error = %v, want expiry", err)
	}

	incident, err := loadIncident(t, uow, incidentID)
	if err != nil {
		t.Fatal(err)
	}
	if incident.Status() != domain.StatusExpired {
		t.Fatalf("incident status after expiry = %s, want expired", incident.Status())
	}

	_, err = svc.Approve(context.Background(), retryCommand)
	if !errors.Is(err, domain.ErrExpired) {
		t.Fatalf("retry approval error = %v, want expiry", err)
	}
}

func TestCancelRejectsBlankReasonBeforeTransaction(t *testing.T) {
	t.Parallel()

	clock := testkit.NewClock(time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC))
	uow := &transactionSpyUnitOfWork{}
	svc := &IncidentService{uow: uow, clock: clock, access: allowAll{}}

	for _, reason := range []string{"", "   ", "\n\t"} {
		t.Run("reason="+reason, func(t *testing.T) {
			uow.calls = 0
			_, err := svc.Cancel(context.Background(), CancelIncidentCommand{
				IncidentID: "incident-1",
				Actor:      mustActor(t, "alice", domain.ActorHuman),
				Reason:     reason,
			})
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("cancel error = %v, want validation", err)
			}
			var validation *domain.ValidationError
			if !errors.As(err, &validation) {
				t.Fatalf("cancel error = %v, want validation error", err)
			}
			if validation.Field != "cancel.reason" {
				t.Fatalf("validation field = %q, want cancel.reason", validation.Field)
			}
			if uow.calls != 0 {
				t.Fatalf("transaction count = %d, want 0", uow.calls)
			}
		})
	}
}

type transactionSpyUnitOfWork struct {
	calls int
}

func (u *transactionSpyUnitOfWork) WithinTransaction(context.Context, func(ports.Repositories) error) error {
	u.calls++
	return errors.New("transaction should not start for invalid cancel reason")
}

type countingApprovalPolicy struct {
	calls int
}

func (p *countingApprovalPolicy) Authorize(proposal domain.RollbackProposal, decision domain.PolicyDecision, approval domain.Approval, fresh domain.DeploymentEvidence, now time.Time) (domain.ApprovedRollback, error) {
	p.calls++
	return domain.AuthorizeRollback(proposal, decision, approval, fresh, now)
}

func seedAwaitingApprovalIncidentWithExpiredProposal(t *testing.T, uow *memory.UnitOfWork, now time.Time) (domain.IncidentID, domain.RollbackProposal) {
	t.Helper()

	target := mustTarget(t, "cluster", "namespace", "deployment", "service")
	symptom := mustSymptom(t, "http_error_rate", 0.25, 0.1, "ratio", now.Add(-10*time.Minute))
	incident, err := domain.NewIncident("incident-1", target, symptom, now.Add(-10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	var proposal domain.RollbackProposal
	err = uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
		if err := repositories.Incidents().Create(incident); err != nil {
			return err
		}
		if err := incident.StartInvestigation(now.Add(-9 * time.Minute)); err != nil {
			return err
		}
		if err := repositories.Incidents().Update(incident, 1); err != nil {
			return err
		}
		if err := incident.AwaitApproval(now.Add(-8 * time.Minute)); err != nil {
			return err
		}
		if err := repositories.Incidents().Update(incident, 2); err != nil {
			return err
		}
		decision, err := domain.NewPolicyDecision("policy-1", incident.ID(), []domain.RuleResult{{Name: "allowed", Passed: true}}, now.Add(-8*time.Minute))
		if err != nil {
			return err
		}
		if err := repositories.PolicyDecisions().Save(decision); err != nil {
			return err
		}
		proposal, err = domain.NewRollbackProposal("action-1", incident.ID(), decision.ID(), target, "revision-2", "revision-1", now.Add(-time.Minute))
		if err != nil {
			return err
		}
		if err := repositories.Proposals().Save(proposal); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return incident.ID(), proposal
}

func seedExecutingRollbackState(t *testing.T, uow *memory.UnitOfWork, now time.Time, approvalExpiresAt time.Time) (domain.IncidentID, domain.RollbackProposal) {
	t.Helper()

	target := mustTarget(t, "cluster", "namespace", "deployment", "service")
	symptom := mustSymptom(t, "http_error_rate", 0.25, 0.1, "ratio", now.Add(-10*time.Minute))
	incident, err := domain.NewIncident("incident-executing", target, symptom, now.Add(-10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	var proposal domain.RollbackProposal
	err = uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
		if err := repositories.Incidents().Create(incident); err != nil {
			return err
		}
		if err := incident.StartInvestigation(now.Add(-9 * time.Minute)); err != nil {
			return err
		}
		if err := repositories.Incidents().Update(incident, 1); err != nil {
			return err
		}
		if err := incident.AwaitApproval(now.Add(-8 * time.Minute)); err != nil {
			return err
		}
		if err := repositories.Incidents().Update(incident, 2); err != nil {
			return err
		}
		decision, err := domain.NewPolicyDecision("policy-executing", incident.ID(), []domain.RuleResult{{Name: "allowed", Passed: true}}, now.Add(-8*time.Minute))
		if err != nil {
			return err
		}
		if err := repositories.PolicyDecisions().Save(decision); err != nil {
			return err
		}
		proposal, err = domain.NewRollbackProposal("action-executing", incident.ID(), decision.ID(), target, "revision-1", "revision-2", now.Add(time.Minute))
		if err != nil {
			return err
		}
		if err := repositories.Proposals().Save(proposal); err != nil {
			return err
		}
		approval, err := domain.NewApproval("approval-executing", mustActor(t, "alice", domain.ActorHuman), domain.ApprovalApproved, proposal.ActionID(), proposal.Hash(), now.Add(-2*time.Minute), approvalExpiresAt, "")
		if err != nil {
			return err
		}
		if err := repositories.Approvals().Save(approval); err != nil {
			return err
		}
		if err := incident.Approve(now.Add(-2 * time.Minute)); err != nil {
			return err
		}
		if err := repositories.Incidents().Update(incident, 3); err != nil {
			return err
		}
		if err := incident.StartExecution(now.Add(-time.Minute)); err != nil {
			return err
		}
		if err := repositories.Incidents().Update(incident, 4); err != nil {
			return err
		}
		record, err := domain.NewPendingIdempotencyRecord("execute-1", domain.IdempotencyOperationExecuteRollback, string(proposal.Hash()), string(incident.ID()), now.Add(-time.Minute))
		if err != nil {
			return err
		}
		if err := repositories.Idempotency().Create(record); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return incident.ID(), proposal
}

func seedApprovedRollbackState(t *testing.T, uow *memory.UnitOfWork, now time.Time) (domain.IncidentID, domain.RollbackProposal) {
	t.Helper()

	target := mustTarget(t, "cluster", "namespace", "deployment", "service")
	symptom := mustSymptom(t, "http_error_rate", 0.25, 0.1, "ratio", now.Add(-10*time.Minute))
	incident, err := domain.NewIncident("incident-approved", target, symptom, now.Add(-10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	var proposal domain.RollbackProposal
	err = uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
		if err := repositories.Incidents().Create(incident); err != nil {
			return err
		}
		if err := incident.StartInvestigation(now.Add(-9 * time.Minute)); err != nil {
			return err
		}
		if err := repositories.Incidents().Update(incident, 1); err != nil {
			return err
		}
		if err := incident.AwaitApproval(now.Add(-8 * time.Minute)); err != nil {
			return err
		}
		if err := repositories.Incidents().Update(incident, 2); err != nil {
			return err
		}
		decision, err := domain.NewPolicyDecision("policy-approved", incident.ID(), []domain.RuleResult{{Name: "allowed", Passed: true}}, now.Add(-8*time.Minute))
		if err != nil {
			return err
		}
		if err := repositories.PolicyDecisions().Save(decision); err != nil {
			return err
		}
		proposal, err = domain.NewRollbackProposal("action-approved", incident.ID(), decision.ID(), target, "revision-1", "revision-2", now.Add(time.Minute))
		if err != nil {
			return err
		}
		if err := repositories.Proposals().Save(proposal); err != nil {
			return err
		}
		approval, err := domain.NewApproval("approval-approved", mustActor(t, "alice", domain.ActorHuman), domain.ApprovalApproved, proposal.ActionID(), proposal.Hash(), now.Add(-2*time.Minute), now.Add(time.Minute), "")
		if err != nil {
			return err
		}
		if err := repositories.Approvals().Save(approval); err != nil {
			return err
		}
		if err := incident.Approve(now.Add(-2 * time.Minute)); err != nil {
			return err
		}
		if err := repositories.Incidents().Update(incident, 3); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return incident.ID(), proposal
}

func loadIncident(t *testing.T, uow *memory.UnitOfWork, incidentID domain.IncidentID) (domain.Incident, error) {
	t.Helper()

	var incident domain.Incident
	err := uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
		var err error
		incident, err = repositories.Incidents().Get(incidentID)
		return err
	})
	return incident, err
}

func loadExecution(t *testing.T, uow *memory.UnitOfWork, incidentID domain.IncidentID) (domain.RollbackExecution, error) {
	t.Helper()

	var execution domain.RollbackExecution
	err := uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
		var err error
		execution, err = repositories.Executions().GetByIncident(incidentID)
		return err
	})
	return execution, err
}

func loadExecutionAndFailureEvent(t *testing.T, uow *memory.UnitOfWork, incidentID domain.IncidentID) (domain.RollbackExecution, domain.OperationEvent) {
	t.Helper()

	var execution domain.RollbackExecution
	var event domain.OperationEvent
	err := uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
		var err error
		execution, err = repositories.Executions().GetByIncident(incidentID)
		if err != nil {
			return err
		}
		events, err := repositories.Events().List(incidentID)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			return errors.New("no incident events recorded")
		}
		event = events[len(events)-1]
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return execution, event
}

type failingRollbackExecutor struct {
	err error
}

func (f *failingRollbackExecutor) ExecuteApprovedRollback(context.Context, domain.ApprovedRollback, domain.IdempotencyKey) (domain.RollbackExecution, error) {
	return domain.RollbackExecution{}, f.err
}

type successfulRollbackExecutor struct {
	executionID domain.ExecutionID
	startedAt   time.Time
	completedAt time.Time
	calls       int
	approved    domain.ApprovedRollback
	key         domain.IdempotencyKey
}

func (e *successfulRollbackExecutor) ExecuteApprovedRollback(_ context.Context, approved domain.ApprovedRollback, key domain.IdempotencyKey) (domain.RollbackExecution, error) {
	e.calls++
	e.approved = approved
	e.key = key
	proposal := approved.Proposal()
	return domain.NewRollbackExecution(
		e.executionID,
		proposal.IncidentID(),
		key,
		proposal.CurrentRevision(),
		proposal.TargetRevision(),
		domain.ExecutionSucceeded,
		e.startedAt,
		e.completedAt,
		"",
	)
}

func mustActor(t *testing.T, id string, kind domain.ActorKind) domain.Actor {
	t.Helper()

	actor, err := domain.NewActor(domain.ActorID(id), kind)
	if err != nil {
		t.Fatal(err)
	}
	return actor
}

func mustTarget(t *testing.T, cluster, namespace, deployment, service string) domain.DeploymentTarget {
	t.Helper()

	target, err := domain.NewDeploymentTarget(cluster, namespace, deployment, service)
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func mustSymptom(t *testing.T, kind domain.SymptomKind, observed, threshold float64, unit string, observedAt time.Time) domain.Symptom {
	t.Helper()

	symptom, err := domain.NewSymptom(kind, observed, threshold, unit, observedAt)
	if err != nil {
		t.Fatal(err)
	}
	return symptom
}
