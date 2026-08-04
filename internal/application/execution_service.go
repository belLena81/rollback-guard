package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

const investigationFailureAuditReason = "investigation failed"
const executionFailureAuditReason = "rollback execution failed"

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
	c := s.workflow
	var loaded loadedExecution
	err := c.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		var err error
		loaded.incident, err = repositories.Incidents().Get(incidentID)
		if err != nil {
			return err
		}
		loaded.proposal, err = repositories.Proposals().GetByIncident(incidentID)
		if err != nil {
			return err
		}
		requestHash := string(loaded.proposal.Hash())
		var recordErr error
		loaded.record, recordErr = repositories.Idempotency().Get(key)
		if recordErr == nil {
			if loaded.record.Operation() != domain.IdempotencyOperationExecuteRollback || loaded.record.RequestHash() != requestHash || loaded.record.ResourceID() != string(incidentID) {
				return &domain.IdempotencyConflictError{Key: key}
			}
			switch loaded.record.State() {
			case domain.IdempotencyCompleted:
				loaded.existing, err = repositories.Executions().Get(loaded.record.ResultID())
				if err != nil {
					return err
				}
				if err := validateExecutionResult(loaded.existing, loaded.proposal, key); err != nil {
					return &domain.IdempotencyConflictError{Key: key}
				}
				loaded.completed = true
				return nil
			case domain.IdempotencyPending:
				if loaded.incident.Status() != domain.StatusExecuting {
					return &domain.IdempotencyConflictError{Key: key}
				}
				loaded.resume = true
			default:
				return &domain.IdempotencyConflictError{Key: key}
			}
		}
		if recordErr != nil && !errors.Is(recordErr, domain.ErrNotFound) {
			return recordErr
		}
		if errors.Is(recordErr, domain.ErrNotFound) && loaded.incident.Status() != domain.StatusApproved {
			return &domain.IllegalTransitionError{From: loaded.incident.Status(), To: domain.StatusExecuting}
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

func (s *ExecutionService) authorizeAndStart(ctx context.Context, loaded loadedExecution, key domain.IdempotencyKey) (executionPreparation, error) {
	c := s.workflow
	startedAt := c.clock.Now()
	if loaded.resume {
		return s.revalidatePendingExecution(ctx, loaded, key)
	}
	freshDeployment, err := c.inspector.InspectDeployment(ctx, loaded.proposal.Target())
	if err != nil {
		return executionPreparation{}, fmt.Errorf("reinspect deployment: %w", err)
	}
	startedAt = c.clock.Now()
	approvedRollback, err := c.approvalPolicy.Authorize(loaded.proposal, loaded.decision, loaded.approval, freshDeployment, startedAt)
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.config.CleanupTimeout)
		defer cancel()
		var cleanupErr error
		if errors.Is(err, domain.ErrExpired) {
			cleanupErr = c.expire(cleanupCtx, loaded.incident.ID(), err.Error())
		} else {
			cleanupErr = c.failIncident(cleanupCtx, loaded.incident.ID(), err.Error())
		}
		return executionPreparation{}, errors.Join(err, cleanupErr)
	}
	record, err := domain.NewPendingIdempotencyRecord(key, domain.IdempotencyOperationExecuteRollback, string(loaded.proposal.Hash()), string(loaded.incident.ID()), startedAt)
	if err != nil {
		return executionPreparation{}, fmt.Errorf("create idempotency attempt: %w", err)
	}
	err = c.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		fresh, err := repositories.Incidents().Get(loaded.incident.ID())
		if err != nil {
			return err
		}
		if fresh.Version() != loaded.incident.Version() {
			return &domain.VersionConflictError{Expected: loaded.incident.Version(), Actual: fresh.Version()}
		}
		expected, from := fresh.Version(), fresh.Status()
		if err := fresh.StartExecution(startedAt); err != nil {
			return err
		}
		if err := repositories.Incidents().Update(fresh, expected); err != nil {
			return err
		}
		if err := repositories.Idempotency().Create(record); err != nil {
			return err
		}
		return c.appendEvent(ctx, repositories, fresh.ID(), domain.EventStatusChanged, c.systemActor, domain.StatusChangedDetail{From: from, To: domain.StatusExecuting}, startedAt)
	})
	if err != nil {
		return executionPreparation{}, fmt.Errorf("start rollback execution: %w", err)
	}
	return executionPreparation{approved: approvedRollback, record: record, startedAt: startedAt}, nil
}

func (s *ExecutionService) revalidatePendingExecution(ctx context.Context, loaded loadedExecution, key domain.IdempotencyKey) (executionPreparation, error) {
	c := s.workflow
	startedAt := loaded.record.StartedAt()
	resumed, err := domain.ResumeApprovedRollback(loaded.proposal, loaded.decision, loaded.approval, loaded.record, key)
	if err != nil {
		return s.completePendingRevalidationFailure(ctx, loaded, startedAt, fmt.Errorf("resume approved rollback: %w", err)), nil
	}
	freshDeployment, err := c.inspector.InspectDeployment(ctx, loaded.proposal.Target())
	if err != nil {
		return executionPreparation{}, fmt.Errorf("reinspect pending deployment: %w", err)
	}
	authorizationTime := loaded.record.StartedAt()
	if freshDeployment.Target.Equal(loaded.proposal.Target()) && freshDeployment.CurrentRevision == loaded.proposal.TargetRevision() {
		if !deploymentRecovered(freshDeployment) {
			return executionPreparation{approved: resumed, record: loaded.record, startedAt: startedAt}, nil
		}
		if _, err := c.approvalPolicy.Authorize(
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
	approvedRollback, err := c.approvalPolicy.Authorize(loaded.proposal, loaded.decision, loaded.approval, freshDeployment, c.clock.Now())
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
	c := s.workflow
	execution, createErr := s.newFailedExecution(loaded.proposal, loaded.record, startedAt, cause)
	if createErr != nil {
		return executionPreparation{executionErr: errors.Join(cause, createErr), completed: true}
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.config.CleanupTimeout)
	defer cancel()
	completionErr := s.completeExecution(cleanupCtx, loaded.proposal, loaded.record, execution)
	return executionPreparation{
		execution:    execution,
		executionErr: errors.Join(cause, executionOutcomeError(execution), completionErr),
		completed:    true,
	}
}

func (s *ExecutionService) invokeExecutor(ctx context.Context, approved domain.ApprovedRollback, proposal domain.RollbackProposal, record domain.IdempotencyRecord, startedAt time.Time) (domain.RollbackExecution, error) {
	c := s.workflow
	execution, executeErr := c.executor.ExecuteApprovedRollback(ctx, approved, record.Key())
	if errors.Is(executeErr, ports.ErrRollbackOutcomeUnknown) {
		return domain.RollbackExecution{}, fmt.Errorf("execute approved rollback: %w", executeErr)
	}
	if executeErr == nil {
		executeErr = validateExecutionResult(execution, proposal, record.Key())
	}
	if executeErr != nil {
		var err error
		execution, err = s.newFailedExecution(proposal, record, startedAt, executeErr)
		if err != nil {
			return domain.RollbackExecution{}, errors.Join(executeErr, fmt.Errorf("create failed execution result: %w", err))
		}
	}
	return execution, nil
}

func (s *ExecutionService) newFailedExecution(proposal domain.RollbackProposal, record domain.IdempotencyRecord, startedAt time.Time, cause error) (domain.RollbackExecution, error) {
	return domain.NewRollbackExecution(
		domain.ExecutionID(s.workflow.ids.NewID("execution")),
		proposal.IncidentID(),
		record.Key(),
		proposal.CurrentRevision(),
		proposal.TargetRevision(),
		domain.ExecutionFailed,
		startedAt,
		s.workflow.clock.Now(),
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
	c := s.workflow
	completedAt := c.clock.Now()
	completedRecord, err := record.Complete(execution.ID(), completedAt)
	if err != nil {
		return fmt.Errorf("complete idempotency attempt: %w", err)
	}
	err = c.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		fresh, err := repositories.Incidents().Get(proposal.IncidentID())
		if err != nil {
			return err
		}
		if err := repositories.Executions().Save(execution); err != nil {
			return err
		}
		if err := repositories.Idempotency().Update(completedRecord, domain.IdempotencyPending); err != nil {
			return err
		}
		if err := c.appendEvent(ctx, repositories, proposal.IncidentID(), domain.EventExecutionCompleted, c.systemActor, domain.ExecutionCompletedDetail{ExecutionID: execution.ID(), Outcome: execution.Outcome()}, completedAt); err != nil {
			return err
		}
		expected := fresh.Version()
		from := fresh.Status()
		if execution.Outcome() == domain.ExecutionSucceeded {
			err = fresh.StartVerification(completedAt)
		} else {
			err = fresh.Fail(completedAt)
		}
		if err != nil {
			return err
		}
		if err := repositories.Incidents().Update(fresh, expected); err != nil {
			return err
		}
		kind := domain.EventStatusChanged
		var detail domain.EventDetail = domain.StatusChangedDetail{From: from, To: fresh.Status()}
		if fresh.Status() == domain.StatusFailed {
			kind = domain.EventIncidentFailed
			detail = domain.TerminalDetail{Reason: execution.FailureReason()}
		}
		return c.appendEvent(ctx, repositories, proposal.IncidentID(), kind, c.systemActor, detail, completedAt)
	})
	if err != nil {
		return fmt.Errorf("complete rollback execution: %w", err)
	}
	return nil
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

func (s *VerificationService) Verify(ctx context.Context, incidentID domain.IncidentID) (domain.RecoveryVerification, error) {
	c := s.workflow
	var incident domain.Incident
	var proposal domain.RollbackProposal
	var execution domain.RollbackExecution
	err := c.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		var err error
		incident, err = repositories.Incidents().Get(incidentID)
		if err != nil {
			return err
		}
		proposal, err = repositories.Proposals().GetByIncident(incidentID)
		if err != nil {
			return err
		}
		execution, err = repositories.Executions().GetByIncident(incidentID)
		return err
	})
	if err != nil {
		return domain.RecoveryVerification{}, fmt.Errorf("load recovery verification: %w", err)
	}
	if incident.Status() != domain.StatusVerifying {
		return domain.RecoveryVerification{}, &domain.IllegalTransitionError{From: incident.Status(), To: domain.StatusResolved}
	}

	deployment, err := c.inspector.InspectDeployment(ctx, incident.Target())
	if err != nil {
		return domain.RecoveryVerification{}, fmt.Errorf("inspect recovery deployment: %w", err)
	}
	metric, err := c.metrics.Observe(ctx, incident.Target(), incident.Symptom())
	if err != nil {
		return domain.RecoveryVerification{}, fmt.Errorf("observe recovery metric: %w", err)
	}
	now := c.clock.Now()
	verification, err := domain.EvaluateRecovery(incident.Target(), proposal.TargetRevision(), deployment, metric, incident.Symptom(), incidentID, execution.ID(), now)
	if err != nil {
		return domain.RecoveryVerification{}, err
	}
	err = c.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		fresh, err := repositories.Incidents().Get(incidentID)
		if err != nil {
			return err
		}
		if fresh.Version() != incident.Version() {
			return &domain.VersionConflictError{Expected: incident.Version(), Actual: fresh.Version()}
		}
		if err := repositories.Verifications().Save(verification); err != nil {
			return err
		}
		if err := c.appendEvent(ctx, repositories, incidentID, domain.EventVerificationCompleted, c.systemActor, domain.VerificationCompletedDetail{Passed: verification.Passed(), Revision: verification.Revision()}, now); err != nil {
			return err
		}
		expected := fresh.Version()
		if verification.Passed() {
			err = fresh.Resolve(now)
		} else {
			err = fresh.Fail(now)
		}
		if err != nil {
			return err
		}
		if err := repositories.Incidents().Update(fresh, expected); err != nil {
			return err
		}
		if verification.Passed() {
			return c.appendEvent(ctx, repositories, incidentID, domain.EventStatusChanged, c.systemActor, domain.StatusChangedDetail{From: domain.StatusVerifying, To: domain.StatusResolved}, now)
		}
		return c.appendEvent(ctx, repositories, incidentID, domain.EventIncidentFailed, c.systemActor, domain.TerminalDetail{Reason: verification.FailureReason()}, now)
	})
	if err != nil {
		return domain.RecoveryVerification{}, fmt.Errorf("persist recovery verification: %w", err)
	}
	if !verification.Passed() {
		return verification, fmt.Errorf("recovery verification failed: %s", verification.FailureReason())
	}
	return verification, nil
}

type CancelIncidentCommand struct {
	IncidentID domain.IncidentID
	Actor      domain.Actor
	Reason     string
}

func (s *IncidentService) Cancel(ctx context.Context, command CancelIncidentCommand) (domain.Incident, error) {
	if strings.TrimSpace(command.Reason) == "" {
		return domain.Incident{}, &domain.ValidationError{Field: "cancel.reason", Reason: "must not be empty"}
	}
	c := s.workflow
	now := c.clock.Now()
	var result domain.Incident
	err := c.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		incident, err := repositories.Incidents().Get(command.IncidentID)
		if err != nil {
			return err
		}
		expected := incident.Version()
		if err := incident.Cancel(now); err != nil {
			return err
		}
		if err := repositories.Incidents().Update(incident, expected); err != nil {
			return err
		}
		result = incident
		return c.appendEvent(ctx, repositories, command.IncidentID, domain.EventIncidentCanceled, command.Actor, domain.TerminalDetail{Reason: command.Reason}, now)
	})
	if err != nil {
		return domain.Incident{}, fmt.Errorf("cancel incident: %w", err)
	}
	return result, nil
}

func (c *workflow) expire(ctx context.Context, incidentID domain.IncidentID, reason string) error {
	now := c.clock.Now()
	return c.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		incident, err := repositories.Incidents().Get(incidentID)
		if err != nil {
			return err
		}
		expected := incident.Version()
		if err := incident.Expire(now); err != nil {
			return err
		}
		if err := repositories.Incidents().Update(incident, expected); err != nil {
			return err
		}
		return c.appendEvent(ctx, repositories, incidentID, domain.EventIncidentExpired, c.systemActor, domain.TerminalDetail{Reason: reason}, now)
	})
}

func (c *workflow) failAfterExternalError(ctx context.Context, incidentID domain.IncidentID, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.config.CleanupTimeout)
	defer cancel()
	if err := c.failIncident(cleanupCtx, incidentID, investigationFailureAuditReason); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func (c *workflow) failIncident(ctx context.Context, incidentID domain.IncidentID, reason string) error {
	now := c.clock.Now()
	return c.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		incident, err := repositories.Incidents().Get(incidentID)
		if err != nil {
			return err
		}
		expected := incident.Version()
		if err := incident.Fail(now); err != nil {
			return err
		}
		if err := repositories.Incidents().Update(incident, expected); err != nil {
			return err
		}
		return c.appendEvent(ctx, repositories, incidentID, domain.EventIncidentFailed, c.systemActor, domain.TerminalDetail{Reason: reason}, now)
	})
}
