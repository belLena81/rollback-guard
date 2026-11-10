package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

const investigationFailureAuditReason = "investigation failed"
const executionFailureAuditReason = "rollback execution failed"
const authorizationFailureAuditReason = "rollback authorization failed"
const authorizationExpiredAuditReason = "rollback authorization expired"

// ExecutionService runs an approved rollback exactly once per idempotency key,
// re-checking authorization immediately before calling the executor.
type ExecutionService struct {
	audit
	uow            ports.UnitOfWork
	clock          ports.Clock
	inspector      ports.DeploymentInspector
	executor       ports.RollbackExecutor
	approvalPolicy ports.ApprovalPolicy
	cleanupTimeout time.Duration
}

func (s *ExecutionService) Execute(ctx context.Context, incidentID domain.IncidentID, key domain.IdempotencyKey) (domain.RollbackExecution, error) {
	if key == "" {
		return domain.RollbackExecution{}, &domain.ValidationError{Field: "idempotency_key", Reason: "must not be empty"}
	}
	loaded, err := s.loadExecution(ctx, incidentID, key)
	if err != nil {
		return domain.RollbackExecution{}, fmt.Errorf("load rollback execution: %w", err)
	}
	if loaded.completed {
		return loaded.existing, executionOutcomeError(loaded.existing)
	}
	preparation, err := s.authorizeAndStart(ctx, loaded, key)
	if err != nil {
		return domain.RollbackExecution{}, err
	}
	if preparation.completed {
		return preparation.execution, preparation.executionErr
	}
	execution, err := s.invokeExecutor(ctx, preparation.approved, loaded.proposal, preparation.record, preparation.startedAt)
	if err != nil {
		return domain.RollbackExecution{}, err
	}
	if err := s.completeExecution(ctx, loaded.proposal, preparation.record, execution); err != nil {
		return domain.RollbackExecution{}, err
	}
	return execution, executionOutcomeError(execution)
}

type executionPreparation struct {
	approved     domain.ApprovedRollback
	record       domain.IdempotencyRecord
	startedAt    time.Time
	execution    domain.RollbackExecution
	executionErr error
	completed    bool
}

type loadedExecution struct {
	incident  domain.Incident
	proposal  domain.RollbackProposal
	approval  domain.Approval
	decision  domain.PolicyDecision
	record    domain.IdempotencyRecord
	existing  domain.RollbackExecution
	completed bool
	resume    bool
}

func (s *ExecutionService) loadExecution(ctx context.Context, incidentID domain.IncidentID, key domain.IdempotencyKey) (loadedExecution, error) {
	var loaded loadedExecution
	err := s.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		var err error
		loaded.incident, err = repositories.Incidents().Get(incidentID)
		if err != nil {
			return err
		}
		loaded.proposal, err = repositories.Proposals().GetByIncident(incidentID)
		if err != nil {
			return err
		}
		if done, err := s.readAttempt(repositories, &loaded, key); done || err != nil {
			return err
		}
		loaded.approval, err = repositories.Approvals().GetByIncident(incidentID)
		if err != nil {
			return err
		}
		loaded.decision, err = repositories.PolicyDecisions().Get(loaded.proposal.PolicyDecisionID())
		return err
	})
	return loaded, err
}

// readAttempt looks up the idempotency record for key and records in loaded
// what it means: a completed attempt (done, with its stored result), a pending
// one to resume, or no attempt yet, which needs an approved incident. A record
// that belongs to another operation, proposal, incident, or state is a
// conflict.
func (s *ExecutionService) readAttempt(repositories ports.Repositories, loaded *loadedExecution, key domain.IdempotencyKey) (done bool, err error) {
	record, err := repositories.Idempotency().Get(key)
	if errors.Is(err, domain.ErrNotFound) {
		if loaded.incident.Status() != domain.StatusApproved {
			return false, &domain.IllegalTransitionError{From: loaded.incident.Status(), To: domain.StatusExecuting}
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	loaded.record = record
	if record.Operation() != domain.IdempotencyOperationExecuteRollback || record.RequestHash() != string(loaded.proposal.Hash()) || record.ResourceID() != string(loaded.incident.ID()) {
		return false, &domain.IdempotencyConflictError{Key: key}
	}
	switch record.State() {
	case domain.IdempotencyCompleted:
		loaded.existing, err = repositories.Executions().Get(record.ResultID())
		if err != nil {
			return false, err
		}
		if validateExecutionResult(loaded.existing, loaded.proposal, key) != nil {
			return false, &domain.IdempotencyConflictError{Key: key}
		}
		loaded.completed = true
		return true, nil
	case domain.IdempotencyPending:
		if loaded.incident.Status() != domain.StatusExecuting {
			return false, &domain.IdempotencyConflictError{Key: key}
		}
		loaded.resume = true
		return false, nil
	default:
		return false, &domain.IdempotencyConflictError{Key: key}
	}
}

func (s *ExecutionService) authorizeAndStart(ctx context.Context, loaded loadedExecution, key domain.IdempotencyKey) (executionPreparation, error) {
	startedAt := s.clock.Now()
	if loaded.resume {
		return s.revalidatePendingExecution(ctx, loaded, key)
	}
	freshDeployment, err := s.inspector.InspectDeployment(ctx, loaded.proposal.Target())
	if err != nil {
		return executionPreparation{}, fmt.Errorf("reinspect deployment: %w", err)
	}
	startedAt = s.clock.Now()
	approvedRollback, err := s.approvalPolicy.Authorize(loaded.proposal, loaded.decision, loaded.approval, freshDeployment, startedAt)
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
		defer cancel()
		// Terminate only the incident this request authorized against. If a
		// concurrent request for the same approval has moved it on (started
		// or finished the rollback, which is also why the revision no longer
		// matches), this request's view is stale and it must not fail an
		// incident whose rollback is in flight or applied.
		var cleanupErr error
		if errors.Is(err, domain.ErrExpired) {
			cleanupErr = s.terminateIfUnchanged(cleanupCtx, loaded.incident.ID(), loaded.incident.Version(), (*domain.Incident).Expire, domain.EventIncidentExpired, authorizationExpiredAuditReason)
		} else {
			cleanupErr = s.terminateIfUnchanged(cleanupCtx, loaded.incident.ID(), loaded.incident.Version(), (*domain.Incident).Fail, domain.EventIncidentFailed, authorizationFailureAuditReason)
		}
		return executionPreparation{}, errors.Join(err, cleanupErr)
	}
	record, err := domain.NewPendingIdempotencyRecord(key, domain.IdempotencyOperationExecuteRollback, string(loaded.proposal.Hash()), string(loaded.incident.ID()), startedAt)
	if err != nil {
		return executionPreparation{}, fmt.Errorf("create idempotency attempt: %w", err)
	}
	err = s.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		fresh, err := repositories.Incidents().Get(loaded.incident.ID())
		if err != nil {
			return err
		}
		if fresh.Version() != loaded.incident.Version() {
			return &domain.VersionConflictError{Expected: loaded.incident.Version(), Actual: fresh.Version()}
		}
		if _, err := s.transition(ctx, repositories, fresh, startedAt, (*domain.Incident).StartExecution, statusChanged(s.systemActor)); err != nil {
			return err
		}
		return repositories.Idempotency().Create(record)
	})
	if err != nil {
		return executionPreparation{}, fmt.Errorf("start rollback execution: %w", err)
	}
	return executionPreparation{approved: approvedRollback, record: record, startedAt: startedAt}, nil
}

func (s *ExecutionService) revalidatePendingExecution(ctx context.Context, loaded loadedExecution, key domain.IdempotencyKey) (executionPreparation, error) {
	startedAt := loaded.record.StartedAt()
	resumed, err := domain.ResumeApprovedRollback(loaded.proposal, loaded.decision, loaded.approval, loaded.record, key)
	if err != nil {
		return s.completePendingRevalidationFailure(ctx, loaded, startedAt, fmt.Errorf("resume approved rollback: %w", err)), nil
	}
	freshDeployment, err := s.inspector.InspectDeployment(ctx, loaded.proposal.Target())
	if err != nil {
		return executionPreparation{}, fmt.Errorf("reinspect pending deployment: %w", err)
	}
	authorizationTime := loaded.record.StartedAt()
	if freshDeployment.Target.Equal(loaded.proposal.Target()) && freshDeployment.CurrentRevision == loaded.proposal.TargetRevision() {
		if !deploymentRecovered(freshDeployment) {
			return executionPreparation{approved: resumed, record: loaded.record, startedAt: startedAt}, nil
		}
		if _, err := s.approvalPolicy.Authorize(
			loaded.proposal,
			loaded.decision,
			loaded.approval,
			domain.DeploymentEvidence{Target: loaded.proposal.Target(), CurrentRevision: loaded.proposal.CurrentRevision()},
			authorizationTime,
		); err != nil {
			return s.completePendingRevalidationFailure(ctx, loaded, startedAt, err), nil
		}
		return executionPreparation{approved: resumed, record: loaded.record, startedAt: startedAt}, nil
	}
	approvedRollback, err := s.approvalPolicy.Authorize(loaded.proposal, loaded.decision, loaded.approval, freshDeployment, s.clock.Now())
	if err != nil {
		return s.completePendingRevalidationFailure(ctx, loaded, startedAt, err), nil
	}
	return executionPreparation{approved: approvedRollback, record: loaded.record, startedAt: startedAt}, nil
}

func deploymentRecovered(deployment domain.DeploymentEvidence) bool {
	return deployment.Healthy &&
		deployment.DesiredReplicas == deployment.ReadyReplicas &&
		deployment.DesiredReplicas == deployment.AvailableReplicas
}

func (s *ExecutionService) completePendingRevalidationFailure(ctx context.Context, loaded loadedExecution, startedAt time.Time, cause error) executionPreparation {
	execution, createErr := s.newFailedExecution(loaded.proposal, loaded.record, startedAt)
	if createErr != nil {
		return executionPreparation{executionErr: errors.Join(cause, createErr), completed: true}
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
	defer cancel()
	completionErr := s.completeExecution(cleanupCtx, loaded.proposal, loaded.record, execution)
	return executionPreparation{
		execution:    execution,
		executionErr: errors.Join(cause, executionOutcomeError(execution), completionErr),
		completed:    true,
	}
}

func (s *ExecutionService) invokeExecutor(ctx context.Context, approved domain.ApprovedRollback, proposal domain.RollbackProposal, record domain.IdempotencyRecord, startedAt time.Time) (domain.RollbackExecution, error) {
	execution, executeErr := s.executor.ExecuteApprovedRollback(ctx, approved, record.Key())
	if errors.Is(executeErr, ports.ErrRollbackOutcomeUnknown) {
		return domain.RollbackExecution{}, fmt.Errorf("execute approved rollback: %w", executeErr)
	}
	if executeErr == nil {
		executeErr = validateExecutionResult(execution, proposal, record.Key())
	}
	if executeErr != nil {
		var err error
		execution, err = s.newFailedExecution(proposal, record, startedAt)
		if err != nil {
			return domain.RollbackExecution{}, errors.Join(executeErr, fmt.Errorf("create failed execution result: %w", err))
		}
	}
	return execution, nil
}

func (s *ExecutionService) newFailedExecution(proposal domain.RollbackProposal, record domain.IdempotencyRecord, startedAt time.Time) (domain.RollbackExecution, error) {
	return domain.NewRollbackExecution(
		domain.ExecutionID(s.ids.NewID("execution")),
		proposal.IncidentID(),
		record.Key(),
		proposal.CurrentRevision(),
		proposal.TargetRevision(),
		domain.ExecutionFailed,
		startedAt,
		s.clock.Now(),
		executionFailureAuditReason,
	)
}

func executionOutcomeError(execution domain.RollbackExecution) error {
	if execution.Outcome() != domain.ExecutionFailed {
		return nil
	}
	return fmt.Errorf("rollback execution failed: %s", execution.FailureReason())
}

func (s *ExecutionService) completeExecution(ctx context.Context, proposal domain.RollbackProposal, record domain.IdempotencyRecord, execution domain.RollbackExecution) error {
	completedAt := s.clock.Now()
	completedRecord, err := record.Complete(execution.ID(), completedAt)
	if err != nil {
		return fmt.Errorf("complete idempotency attempt: %w", err)
	}
	err = s.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		fresh, err := repositories.Incidents().Get(proposal.IncidentID())
		if err != nil {
			return err
		}
		// The incident transition is written first: a concurrent request
		// that completed the same attempt has already moved the incident on,
		// so this one fails with a conflict here instead of a duplicate
		// execution insert. Everything below commits or rolls back together.
		apply := (*domain.Incident).StartVerification
		if execution.Outcome() != domain.ExecutionSucceeded {
			apply = (*domain.Incident).Fail
		}
		if _, err := s.transition(ctx, repositories, fresh, completedAt, apply, s.executionEvents(execution)); err != nil {
			return err
		}
		if err := repositories.Executions().Save(execution); err != nil {
			return err
		}
		return repositories.Idempotency().Update(completedRecord, domain.IdempotencyPending)
	})
	if err != nil {
		return fmt.Errorf("complete rollback execution: %w", err)
	}
	return nil
}

// executionEvents audits a completed execution, then the incident outcome it
// caused: verification starts, or the incident fails with the execution's
// failure reason.
func (s *ExecutionService) executionEvents(execution domain.RollbackExecution) eventsFor {
	return func(from, to domain.Status) []auditEvent {
		completed := s.systemEvent(domain.EventExecutionCompleted, domain.ExecutionCompletedDetail{ExecutionID: execution.ID(), Outcome: execution.Outcome()})
		if to == domain.StatusFailed {
			return []auditEvent{completed, s.systemEvent(domain.EventIncidentFailed, domain.TerminalDetail{Reason: execution.FailureReason()})}
		}
		return []auditEvent{completed, statusChangeEvent(s.systemActor, from, to)}
	}
}

func validateExecutionResult(execution domain.RollbackExecution, proposal domain.RollbackProposal, key domain.IdempotencyKey) error {
	if execution.ID() == "" ||
		execution.IncidentID() != proposal.IncidentID() ||
		execution.IdempotencyKey() != key ||
		execution.FromRevision() != proposal.CurrentRevision() ||
		execution.ToRevision() != proposal.TargetRevision() ||
		(execution.Outcome() != domain.ExecutionSucceeded && execution.Outcome() != domain.ExecutionFailed) {
		return fmt.Errorf("rollback executor returned a result that does not match the approved rollback")
	}
	return nil
}

// terminateIfUnchanged applies a terminal transition and its audit event in
// one transaction, but only while the incident is still at version. When the
// incident has changed since then it writes nothing and returns nil: the
// caller's failure belongs to a superseded view and the newer state wins.
func (s *ExecutionService) terminateIfUnchanged(ctx context.Context, incidentID domain.IncidentID, version uint64, apply func(*domain.Incident, time.Time) error, kind domain.EventKind, reason string) error {
	now := s.clock.Now()
	return s.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		incident, err := repositories.Incidents().Get(incidentID)
		if err != nil {
			return err
		}
		if incident.Version() != version {
			return nil
		}
		_, err = s.transition(ctx, repositories, incident, now, apply, terminated(s.systemActor, kind, reason))
		return err
	})
}
