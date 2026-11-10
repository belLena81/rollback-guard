package application

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

type CreateIncidentCommand struct {
	Target  domain.DeploymentTarget
	Symptom domain.Symptom
	Actor   domain.Actor
}

func (s *IncidentService) CreateIncident(ctx context.Context, command CreateIncidentCommand) (domain.Incident, error) {
	c := s.workflow
	now := c.clock.Now()
	incident, err := domain.NewIncident(domain.IncidentID(c.ids.NewID("incident")), command.Target, command.Symptom, now)
	if err != nil {
		return domain.Incident{}, err
	}
	err = c.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		if err := repositories.Incidents().Create(incident); err != nil {
			return err
		}
		return c.appendEvent(ctx, repositories, incident.ID(), domain.EventIncidentCreated, command.Actor, domain.IncidentCreatedDetail{Target: command.Target}, now)
	})
	if err != nil {
		return domain.Incident{}, fmt.Errorf("create incident transaction: %w", err)
	}
	return incident, nil
}

func (s *IncidentService) CreateIncidentAsync(ctx context.Context, command CreateIncidentCommand) (domain.Incident, error) {
	c := s.workflow
	now := c.clock.Now()
	incident, err := domain.NewIncident(domain.IncidentID(c.ids.NewID("incident")), command.Target, command.Symptom, now)
	if err != nil {
		return domain.Incident{}, err
	}
	if err := incident.StartInvestigation(now); err != nil {
		return domain.Incident{}, err
	}
	operationID, messageID := c.ids.NewID("operation"), c.ids.NewID("message")
	// The dispatch persists the creating request's trace context so the
	// asynchronous coordinator continues the same trace.
	traceID := c.tracer.TraceCarrier(ctx)
	if traceID == "" {
		traceID = c.ids.NewID("trace")
	}
	dispatch := ports.InvestigationDispatch{
		IncidentID: incident.ID(), OperationID: operationID, MessageID: messageID, TraceID: traceID,
		IdempotencyKey: "investigation:" + string(incident.ID()), RequestHash: InvestigationRequestHash(incident),
		State: ports.InvestigationTaskQueued, Stage: ports.InvestigationStageQueued, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	err = c.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		if err := repositories.Incidents().Create(incident); err != nil {
			return err
		}
		if err := repositories.InvestigationTasks().Create(dispatch); err != nil {
			return err
		}
		if err := c.appendEvent(ctx, repositories, incident.ID(), domain.EventIncidentCreated, command.Actor, domain.IncidentCreatedDetail{Target: command.Target}, now); err != nil {
			return err
		}
		return c.appendEvent(ctx, repositories, incident.ID(), domain.EventStatusChanged, c.systemActor, domain.StatusChangedDetail{From: domain.StatusCreated, To: domain.StatusInvestigating}, now)
	})
	if err != nil {
		return domain.Incident{}, fmt.Errorf("create asynchronous incident transaction: %w", err)
	}
	return incident, nil
}

func InvestigationRequestHash(incident domain.Incident) string {
	s := incident.Snapshot()
	canonical := strings.Join([]string{
		string(s.ID),
		strconv.FormatUint(s.Version, 10),
		s.Status.String(),
		s.Target.Cluster(),
		s.Target.Namespace(),
		s.Target.Deployment(),
		s.Target.Service(),
		s.Symptom.Kind(),
		strconv.FormatFloat(s.Symptom.Observed(), 'g', -1, 64),
		strconv.FormatFloat(s.Symptom.Threshold(), 'g', -1, 64),
		s.Symptom.Unit(),
		canonicalInvestigationTime(s.Symptom.StartedAt()),
		canonicalInvestigationTime(s.CreatedAt),
		canonicalInvestigationTime(s.UpdatedAt),
	}, "\x00")
	return fmt.Sprintf("%x", sha256.Sum256([]byte(canonical)))
}

func canonicalInvestigationTime(value time.Time) string {
	return value.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
}

func (s *InvestigationService) Investigate(ctx context.Context, incidentID domain.IncidentID) (domain.Incident, error) {
	c := s.workflow
	incident, err := c.beginOrResumeInvestigation(ctx, incidentID)
	if err != nil {
		return domain.Incident{}, err
	}

	result, err := c.investigator.Investigate(ctx, ports.InvestigationRequest{Incident: incident})
	if err != nil {
		return domain.Incident{}, c.failAfterExternalError(ctx, incidentID, err)
	}
	return s.complete(ctx, incident, result)
}

func (s *InvestigationService) Complete(ctx context.Context, incidentID domain.IncidentID, result ports.InvestigationResult) (domain.Incident, error) {
	incident, err := s.workflow.loadInvestigatingIncident(ctx, incidentID)
	if err != nil {
		return domain.Incident{}, err
	}
	return s.complete(ctx, incident, result)
}

func (s *InvestigationService) complete(ctx context.Context, incident domain.Incident, result ports.InvestigationResult) (domain.Incident, error) {
	c := s.workflow
	incidentID := incident.ID()
	now := c.clock.Now()
	deployment := result.Deployment
	evidence := result.Evidence
	conclusion := result.Conclusion
	if !deployment.Target.Equal(incident.Target()) {
		return domain.Incident{}, c.failAfterExternalError(ctx, incidentID, &domain.ValidationError{Field: "investigation.deployment.target", Reason: "must match incident target"})
	}
	if err := validateEvidenceReferences(incidentID, conclusion, evidence); err != nil {
		return domain.Incident{}, c.failAfterExternalError(ctx, incidentID, err)
	}
	decision, err := c.rollbackPolicy.Evaluate(domain.EligibilityCandidate{DecisionID: domain.PolicyDecisionID(c.ids.NewID("policy")), Incident: incident, Conclusion: conclusion, Evidence: evidence, Now: now})
	if err != nil {
		return domain.Incident{}, c.failAfterExternalError(ctx, incidentID, fmt.Errorf("evaluate rollback policy: %w", err))
	}

	err = c.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		fresh, err := repositories.Incidents().Get(incidentID)
		if err != nil {
			return err
		}
		if fresh.Version() != incident.Version() {
			return &domain.VersionConflictError{Expected: incident.Version(), Actual: fresh.Version()}
		}
		for _, item := range evidence {
			if err := repositories.Evidence().Add(item); err != nil {
				return err
			}
			if err := c.appendEvent(ctx, repositories, incidentID, domain.EventEvidenceCollected, c.systemActor, domain.EvidenceCollectedDetail{EvidenceID: item.ID(), Kind: item.Kind()}, now); err != nil {
				return err
			}
		}
		if err := repositories.Conclusions().Save(conclusion); err != nil {
			return err
		}
		if err := c.appendEvent(ctx, repositories, incidentID, domain.EventDiagnosisCompleted, c.systemActor, domain.DiagnosisCompletedDetail{ConclusionID: conclusion.ID(), Recommendation: conclusion.Recommendation()}, now); err != nil {
			return err
		}
		if err := repositories.PolicyDecisions().Save(decision); err != nil {
			return err
		}
		if err := c.appendEvent(ctx, repositories, incidentID, domain.EventPolicyEvaluated, c.systemActor, domain.PolicyEvaluatedDetail{DecisionID: decision.ID(), Allowed: decision.Allowed()}, now); err != nil {
			return err
		}

		expectedVersion := fresh.Version()
		if !decision.Allowed() {
			if err := fresh.Decline(now); err != nil {
				return err
			}
			if err := repositories.Incidents().Update(fresh, expectedVersion); err != nil {
				return err
			}
			incident = fresh
			if err := c.completeInvestigationDispatch(repositories, incidentID, now); err != nil {
				return err
			}
			return c.appendEvent(ctx, repositories, incidentID, domain.EventStatusChanged, c.systemActor, domain.StatusChangedDetail{From: domain.StatusInvestigating, To: domain.StatusDeclined}, now)
		}

		proposal, err := domain.NewRollbackProposal(
			domain.ActionID(c.ids.NewID("action")),
			incidentID,
			decision.ID(),
			incident.Target(),
			deployment.CurrentRevision,
			deployment.PreviousStableRevision,
			now.Add(c.config.ProposalTTL),
		)
		if err != nil {
			return err
		}
		if err := repositories.Proposals().Save(proposal); err != nil {
			return err
		}
		if err := c.appendEvent(ctx, repositories, incidentID, domain.EventProposalCreated, c.systemActor, domain.ProposalCreatedDetail{ActionID: proposal.ActionID(), ProposalHash: proposal.Hash()}, now); err != nil {
			return err
		}
		if err := fresh.AwaitApproval(now); err != nil {
			return err
		}
		if err := repositories.Incidents().Update(fresh, expectedVersion); err != nil {
			return err
		}
		incident = fresh
		if err := c.completeInvestigationDispatch(repositories, incidentID, now); err != nil {
			return err
		}
		return c.appendEvent(ctx, repositories, incidentID, domain.EventStatusChanged, c.systemActor, domain.StatusChangedDetail{From: domain.StatusInvestigating, To: domain.StatusAwaitingApproval}, now)
	})
	if err != nil {
		return domain.Incident{}, fmt.Errorf("persist investigation: %w", err)
	}
	return incident, nil
}

func (c *workflow) completeInvestigationDispatch(repositories ports.Repositories, incidentID domain.IncidentID, now time.Time) error {
	dispatch, err := repositories.InvestigationTasks().GetByIncident(incidentID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	expected := dispatch.Version
	dispatch.State = ports.InvestigationTaskCompleted
	dispatch.Stage = ports.InvestigationStageFinalizing
	dispatch.CompletedAt = now
	dispatch.LeaseOwner = ""
	dispatch.LeaseExpiresAt = time.Time{}
	dispatch.Version++
	dispatch.UpdatedAt = now
	return repositories.InvestigationTasks().Update(dispatch, expected)
}

func (s *InvestigationService) Fail(ctx context.Context, incidentID domain.IncidentID, code string) error {
	c := s.workflow
	now := c.clock.Now()
	return c.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		incident, err := repositories.Incidents().Get(incidentID)
		if err != nil {
			return err
		}
		if incident.Status() == domain.StatusCanceled {
			return nil
		}
		alreadyFailed := incident.Status() == domain.StatusFailed
		if !alreadyFailed {
			expectedIncidentVersion := incident.Version()
			if err := incident.Fail(now); err != nil {
				return err
			}
			if err := repositories.Incidents().Update(incident, expectedIncidentVersion); err != nil {
				return err
			}
		}
		dispatch, err := repositories.InvestigationTasks().GetByIncident(incidentID)
		if err != nil {
			return err
		}
		expectedDispatchVersion := dispatch.Version
		dispatch.State = ports.InvestigationTaskFailed
		dispatch.FailureCode = code
		dispatch.CompletedAt = now
		dispatch.LeaseOwner = ""
		dispatch.LeaseExpiresAt = time.Time{}
		dispatch.Version++
		dispatch.UpdatedAt = now
		if err := repositories.InvestigationTasks().Update(dispatch, expectedDispatchVersion); err != nil {
			return err
		}
		if alreadyFailed {
			return nil
		}
		return c.appendEvent(ctx, repositories, incidentID, domain.EventIncidentFailed, c.systemActor, domain.TerminalDetail{Reason: code}, now)
	})
}

func (c *workflow) loadInvestigatingIncident(ctx context.Context, incidentID domain.IncidentID) (domain.Incident, error) {
	var incident domain.Incident
	err := c.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		var err error
		incident, err = repositories.Incidents().Get(incidentID)
		return err
	})
	if err != nil {
		return domain.Incident{}, fmt.Errorf("load investigating incident: %w", err)
	}
	if incident.Status() != domain.StatusInvestigating {
		return domain.Incident{}, &domain.IllegalTransitionError{From: incident.Status(), To: domain.StatusAwaitingApproval}
	}
	return incident, nil
}

func (c *workflow) beginOrResumeInvestigation(ctx context.Context, incidentID domain.IncidentID) (domain.Incident, error) {
	now := c.clock.Now()
	var result domain.Incident
	err := c.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		incident, err := repositories.Incidents().Get(incidentID)
		if err != nil {
			return err
		}
		if incident.Status() == domain.StatusInvestigating {
			result = incident
			return nil
		}

		expectedVersion := incident.Version()
		if err := incident.StartInvestigation(now); err != nil {
			return err
		}
		if err := repositories.Incidents().Update(incident, expectedVersion); err != nil {
			return err
		}
		result = incident
		return c.appendEvent(ctx, repositories, incidentID, domain.EventStatusChanged, c.systemActor, domain.StatusChangedDetail{From: domain.StatusCreated, To: domain.StatusInvestigating}, now)
	})
	if err != nil {
		return domain.Incident{}, fmt.Errorf("begin investigation: %w", err)
	}
	return result, nil
}

func validateEvidenceReferences(incidentID domain.IncidentID, conclusion domain.InvestigationConclusion, evidence []domain.Evidence) error {
	if conclusion.IncidentID() != incidentID {
		return &domain.ValidationError{Field: "conclusion.incident_id", Reason: "must match investigated incident"}
	}
	for _, reference := range conclusion.EvidenceIDs() {
		found := false
		for _, item := range evidence {
			if item.ID() == reference && item.IncidentID() == incidentID {
				found = true
				break
			}
		}
		if !found {
			return &domain.EvidenceReferenceError{EvidenceID: reference}
		}
	}
	for _, item := range evidence {
		if item.IncidentID() != incidentID {
			return &domain.ValidationError{Field: "evidence.incident_id", Reason: "must match investigated incident"}
		}
	}
	return nil
}

type ApprovalCommand struct {
	IncidentID   domain.IncidentID
	Actor        domain.Actor
	ActionID     domain.ActionID
	ProposalHash domain.ProposalHash
}

type RejectionCommand struct {
	IncidentID   domain.IncidentID
	Actor        domain.Actor
	ActionID     domain.ActionID
	ProposalHash domain.ProposalHash
	Reason       string
}

func (s *ApprovalService) Approve(ctx context.Context, command ApprovalCommand) (domain.Approval, error) {
	return s.recordDecision(ctx, command, domain.ApprovalApproved, "")
}

func (s *ApprovalService) Reject(ctx context.Context, command RejectionCommand) (domain.Approval, error) {
	approvalCommand := ApprovalCommand{IncidentID: command.IncidentID, Actor: command.Actor, ActionID: command.ActionID, ProposalHash: command.ProposalHash}
	return s.recordDecision(ctx, approvalCommand, domain.ApprovalRejected, command.Reason)
}

func (s *ApprovalService) recordDecision(ctx context.Context, command ApprovalCommand, decision domain.ApprovalDecision, reason string) (domain.Approval, error) {
	c := s.workflow
	now := c.clock.Now()
	var approval domain.Approval
	expired := false
	err := c.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		incident, err := repositories.Incidents().Get(command.IncidentID)
		if err != nil {
			return err
		}
		proposal, err := repositories.Proposals().GetByIncident(command.IncidentID)
		if err != nil {
			return err
		}
		existing, existingErr := repositories.Approvals().GetByIncident(command.IncidentID)
		if existingErr == nil {
			if existing.Actor() == command.Actor && existing.Decision() == decision && existing.ActionID() == command.ActionID && existing.ProposalHash() == command.ProposalHash && existing.RejectionReason() == reason {
				approval = existing
				return nil
			}
			return &domain.ApprovalMismatchError{Reason: "a different approval decision is already recorded"}
		}
		if !errors.Is(existingErr, domain.ErrNotFound) {
			return existingErr
		}
		if proposal.Expired(now) {
			if incident.Status() != domain.StatusExpired {
				expected := incident.Version()
				if transitionErr := incident.Expire(now); transitionErr != nil {
					return transitionErr
				}
				if updateErr := repositories.Incidents().Update(incident, expected); updateErr != nil {
					return updateErr
				}
				if eventErr := c.appendEvent(ctx, repositories, command.IncidentID, domain.EventIncidentExpired, c.systemActor, domain.TerminalDetail{Reason: "proposal expired"}, now); eventErr != nil {
					return eventErr
				}
			}
			expired = true
			return nil
		}
		if command.ActionID != proposal.ActionID() || command.ProposalHash != proposal.Hash() {
			return &domain.ApprovalMismatchError{Reason: "action or proposal hash does not match"}
		}
		expiresAt := now.Add(c.config.ApprovalTTL)
		if proposal.ExpiresAt().Before(expiresAt) {
			expiresAt = proposal.ExpiresAt()
		}
		approval, err = domain.NewApproval(domain.ApprovalID(c.ids.NewID("approval")), command.Actor, decision, command.ActionID, command.ProposalHash, now, expiresAt, reason)
		if err != nil {
			return err
		}
		expected := incident.Version()
		if decision == domain.ApprovalApproved {
			err = incident.Approve(now)
		} else {
			err = incident.Reject(now)
		}
		if err != nil {
			return err
		}
		if err := repositories.Approvals().Save(approval); err != nil {
			return err
		}
		if err := repositories.Incidents().Update(incident, expected); err != nil {
			return err
		}
		kind := domain.EventApprovalRecorded
		var detail domain.EventDetail = domain.ApprovalRecordedDetail{ApprovalID: approval.ID(), Actor: approval.Actor()}
		if decision == domain.ApprovalRejected {
			kind = domain.EventRejectionRecorded
			detail = domain.RejectionRecordedDetail{ApprovalID: approval.ID(), Reason: reason}
		}
		if err := c.appendEvent(ctx, repositories, command.IncidentID, kind, command.Actor, detail, now); err != nil {
			return err
		}
		return c.appendEvent(ctx, repositories, command.IncidentID, domain.EventStatusChanged, command.Actor, domain.StatusChangedDetail{From: domain.StatusAwaitingApproval, To: incident.Status()}, now)
	})
	if err != nil {
		return domain.Approval{}, fmt.Errorf("record approval decision: %w", err)
	}
	if expired {
		return domain.Approval{}, &domain.ExpiryError{Resource: "proposal"}
	}
	return approval, nil
}
