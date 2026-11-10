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

// InvestigationService persists the outcome of an investigation: evidence,
// conclusion, policy decision, and the proposal or the decline.
type InvestigationService struct {
	audit
	uow            ports.UnitOfWork
	clock          ports.Clock
	rollbackPolicy ports.RollbackPolicy
	proposalTTL    time.Duration
	cleanupTimeout time.Duration
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
		s.Symptom.Kind().String(),
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

func (s *InvestigationService) Complete(ctx context.Context, incidentID domain.IncidentID, result ports.InvestigationResult) (domain.Incident, error) {
	incident, err := s.loadInvestigatingIncident(ctx, incidentID)
	if err != nil {
		return domain.Incident{}, err
	}
	return s.complete(ctx, incident, result)
}

func (s *InvestigationService) complete(ctx context.Context, incident domain.Incident, result ports.InvestigationResult) (domain.Incident, error) {
	now := s.clock.Now()
	decision, err := s.evaluate(ctx, incident, result, now)
	if err != nil {
		return domain.Incident{}, err
	}
	var concluded domain.Incident
	err = s.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		fresh, err := repositories.Incidents().Get(incident.ID())
		if err != nil {
			return err
		}
		if fresh.Version() != incident.Version() {
			return &domain.VersionConflictError{Expected: incident.Version(), Actual: fresh.Version()}
		}
		if err := s.recordFindings(repositories, incident.ID(), result, decision, now); err != nil {
			return err
		}
		concluded, err = s.conclude(repositories, fresh, result.Deployment, decision, now)
		return err
	})
	if err != nil {
		return domain.Incident{}, fmt.Errorf("persist investigation: %w", err)
	}
	return concluded, nil
}

// evaluate validates the untrusted investigation result against the incident
// and runs the rollback policy on it. Any failure fails the incident.
func (s *InvestigationService) evaluate(
	ctx context.Context,
	incident domain.Incident,
	result ports.InvestigationResult,
	now time.Time,
) (domain.PolicyDecision, error) {
	if !result.Deployment.Target.Equal(incident.Target()) {
		return domain.PolicyDecision{}, s.failAfterExternalError(
			ctx,
			incident.ID(),
			&domain.ValidationError{Field: "investigation.deployment.target", Reason: "must match incident target"},
		)
	}
	if err := validateEvidenceReferences(incident.ID(), result.Conclusion, result.Evidence); err != nil {
		return domain.PolicyDecision{}, s.failAfterExternalError(ctx, incident.ID(), err)
	}
	decision, err := s.rollbackPolicy.Evaluate(
		domain.EligibilityCandidate{
			DecisionID: domain.PolicyDecisionID(s.ids.NewID("policy")),
			Incident:   incident,
			Conclusion: result.Conclusion,
			Evidence:   result.Evidence,
			Now:        now,
		},
	)
	if err != nil {
		return domain.PolicyDecision{}, s.failAfterExternalError(ctx, incident.ID(), fmt.Errorf("evaluate rollback policy: %w", err))
	}
	return decision, nil
}

// recordFindings stores the evidence, the conclusion, and the policy decision
// with an audit event for each.
func (s *InvestigationService) recordFindings(
	repositories ports.Repositories,
	incidentID domain.IncidentID,
	result ports.InvestigationResult,
	decision domain.PolicyDecision,
	now time.Time,
) error {
	for _, item := range result.Evidence {
		if err := repositories.Evidence().Add(item); err != nil {
			return err
		}
		if err := s.appendEvents(
			repositories,
			incidentID,
			now,
			s.systemEvent(domain.EventEvidenceCollected, domain.EvidenceCollectedDetail{EvidenceID: item.ID(), Kind: item.Kind()}),
		); err != nil {
			return err
		}
	}
	conclusion := result.Conclusion
	if err := repositories.Conclusions().Save(conclusion); err != nil {
		return err
	}
	if err := s.appendEvents(
		repositories,
		incidentID,
		now,
		s.systemEvent(
			domain.EventDiagnosisCompleted,
			domain.DiagnosisCompletedDetail{ConclusionID: conclusion.ID(), Recommendation: conclusion.Recommendation()},
		),
	); err != nil {
		return err
	}
	if err := repositories.PolicyDecisions().Save(decision); err != nil {
		return err
	}
	return s.appendEvents(
		repositories,
		incidentID,
		now,
		s.systemEvent(domain.EventPolicyEvaluated, domain.PolicyEvaluatedDetail{DecisionID: decision.ID(), Allowed: decision.Allowed()}),
	)
}

// conclude moves the investigating incident on: declined when policy denied
// the rollback, otherwise awaiting approval of a new proposal. Either way the
// investigation dispatch completes in the same transaction.
func (s *InvestigationService) conclude(
	repositories ports.Repositories,
	incident domain.Incident,
	deployment domain.DeploymentEvidence,
	decision domain.PolicyDecision,
	now time.Time,
) (domain.Incident, error) {
	if !decision.Allowed() {
		return s.moveAndCompleteDispatch(repositories, incident, (*domain.Incident).Decline, now)
	}
	proposal, err := domain.NewRollbackProposal(
		domain.ActionID(s.ids.NewID("action")),
		incident.ID(),
		decision.ID(),
		incident.Target(),
		deployment.CurrentRevision,
		deployment.PreviousStableRevision,
		now.Add(s.proposalTTL),
	)
	if err != nil {
		return domain.Incident{}, err
	}
	if err := repositories.Proposals().Save(proposal); err != nil {
		return domain.Incident{}, err
	}
	if err := s.appendEvents(
		repositories,
		incident.ID(),
		now,
		s.systemEvent(domain.EventProposalCreated, domain.ProposalCreatedDetail{ActionID: proposal.ActionID(), ProposalHash: proposal.Hash()}),
	); err != nil {
		return domain.Incident{}, err
	}
	return s.moveAndCompleteDispatch(repositories, incident, (*domain.Incident).AwaitApproval, now)
}

func (s *InvestigationService) moveAndCompleteDispatch(
	repositories ports.Repositories,
	incident domain.Incident,
	apply func(*domain.Incident, time.Time) error,
	now time.Time,
) (domain.Incident, error) {
	moved, err := s.transition(repositories, incident, now, apply, statusChanged(s.systemActor))
	if err != nil {
		return domain.Incident{}, err
	}
	return moved, s.completeInvestigationDispatch(repositories, incident.ID(), now)
}

func (s *InvestigationService) completeInvestigationDispatch(repositories ports.Repositories, incidentID domain.IncidentID, now time.Time) error {
	dispatch, err := repositories.InvestigationTasks().GetByIncident(incidentID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	expected := dispatch.Version
	dispatch.Complete(now)
	return repositories.InvestigationTasks().Update(dispatch, expected)
}

func (s *InvestigationService) Fail(ctx context.Context, incidentID domain.IncidentID, code string) error {
	now := s.clock.Now()
	return s.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		incident, err := repositories.Incidents().Get(incidentID)
		if err != nil {
			return err
		}
		if incident.Status() == domain.StatusCanceled {
			return nil
		}
		if incident.Status() != domain.StatusFailed {
			if _, err := s.transition(
				repositories,
				incident,
				now,
				(*domain.Incident).Fail,
				terminated(s.systemActor, domain.EventIncidentFailed, code),
			); err != nil {
				return err
			}
		}
		dispatch, err := repositories.InvestigationTasks().GetByIncident(incidentID)
		if err != nil {
			return err
		}
		expectedDispatchVersion := dispatch.Version
		dispatch.Fail(code, now)
		return repositories.InvestigationTasks().Update(dispatch, expectedDispatchVersion)
	})
}

func (s *InvestigationService) loadInvestigatingIncident(ctx context.Context, incidentID domain.IncidentID) (domain.Incident, error) {
	var incident domain.Incident
	err := s.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
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

func (s *InvestigationService) failAfterExternalError(ctx context.Context, incidentID domain.IncidentID, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
	defer cancel()
	if err := s.failIncident(cleanupCtx, incidentID, investigationFailureAuditReason); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func (s *InvestigationService) failIncident(ctx context.Context, incidentID domain.IncidentID, reason string) error {
	now := s.clock.Now()
	return s.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		incident, err := repositories.Incidents().Get(incidentID)
		if err != nil {
			return err
		}
		_, err = s.transition(repositories, incident, now, (*domain.Incident).Fail, terminated(s.systemActor, domain.EventIncidentFailed, reason))
		return err
	})
}
