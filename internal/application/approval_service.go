package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

// ApprovalService records the human decision on an exact proposal.
type ApprovalService struct {
	audit
	uow         ports.UnitOfWork
	clock       ports.Clock
	approvalTTL time.Duration
	access      AccessPolicy
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

// Approve authorizes command.Actor for ActionApproveRollback before the
// approval transaction; that same actor is the one recorded.
// Approve authorizes and then records in two transactions on two separate
// ports (ports.AuthUnitOfWork, ports.UnitOfWork). A demotion or disable that
// commits between them is not re-checked here: re-reading users inside the
// incident transaction would merge the auth and incident stores, and holding
// the auth transaction open around the record step would take two pool
// connections per approve. The approval such a race records cannot execute:
// the rollback MCP re-checks the approver's current role in the transaction
// that re-loads the approval (docs/phase7-auth.md, Known gaps).
func (s *ApprovalService) Approve(ctx context.Context, command ApprovalCommand) (domain.Approval, error) {
	if err := s.access.Authorize(ctx, command.Actor, domain.ActionApproveRollback); err != nil {
		return domain.Approval{}, fmt.Errorf("authorize approve rollback: %w", err)
	}
	return s.recordDecision(ctx, command, domain.ApprovalApproved, "")
}

func (s *ApprovalService) Reject(ctx context.Context, command RejectionCommand) (domain.Approval, error) {
	if err := s.access.Authorize(ctx, command.Actor, domain.ActionRejectIncident); err != nil {
		return domain.Approval{}, fmt.Errorf("authorize reject incident: %w", err)
	}
	approvalCommand := ApprovalCommand{IncidentID: command.IncidentID, Actor: command.Actor, ActionID: command.ActionID, ProposalHash: command.ProposalHash}
	return s.recordDecision(ctx, approvalCommand, domain.ApprovalRejected, command.Reason)
}

// decisionOutcome is the result of recording a decision: the approval, or
// that the proposal had expired (in which case the incident is expired).
type decisionOutcome struct {
	approval domain.Approval
	expired  bool
}

func (s *ApprovalService) recordDecision(
	ctx context.Context,
	command ApprovalCommand,
	decision domain.ApprovalDecision,
	reason string,
) (domain.Approval, error) {
	now := s.clock.Now()
	var outcome decisionOutcome
	err := s.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		var err error
		outcome, err = s.decide(repositories, command, decision, reason, now)
		return err
	})
	if err != nil {
		return domain.Approval{}, fmt.Errorf("record approval decision: %w", err)
	}
	if outcome.expired {
		return domain.Approval{}, &domain.ExpiryError{Resource: "proposal"}
	}
	return outcome.approval, nil
}

func (s *ApprovalService) decide(
	repositories ports.Repositories,
	command ApprovalCommand,
	decision domain.ApprovalDecision,
	reason string,
	now time.Time,
) (decisionOutcome, error) {
	incident, err := repositories.Incidents().Get(command.IncidentID)
	if err != nil {
		return decisionOutcome{}, err
	}
	proposal, err := repositories.Proposals().GetByIncident(command.IncidentID)
	if err != nil {
		return decisionOutcome{}, err
	}
	if existing, found, err := s.existingDecision(repositories, command, decision, reason); found || err != nil {
		return decisionOutcome{approval: existing}, err
	}
	if proposal.Expired(now) {
		return decisionOutcome{expired: true}, s.expire(repositories, incident, now)
	}
	if command.ActionID != proposal.ActionID() || command.ProposalHash != proposal.Hash() {
		return decisionOutcome{}, &domain.ApprovalMismatchError{Reason: "action or proposal hash does not match"}
	}
	expiresAt := now.Add(s.approvalTTL)
	if proposal.ExpiresAt().Before(expiresAt) {
		expiresAt = proposal.ExpiresAt()
	}
	approval, err := domain.NewApproval(
		domain.ApprovalID(s.ids.NewID("approval")),
		command.Actor,
		decision,
		command.ActionID,
		command.ProposalHash,
		now,
		expiresAt,
		reason,
	)
	if err != nil {
		return decisionOutcome{}, err
	}
	apply := (*domain.Incident).Approve
	if decision != domain.ApprovalApproved {
		apply = (*domain.Incident).Reject
	}
	// Transition first: a concurrent request that recorded the same
	// decision has already moved the incident on, so this one fails on
	// the version guard (a conflict) instead of a duplicate approval
	// insert. Both writes commit or roll back together.
	if _, err := s.transition(repositories, incident, now, apply, approvalEvents(command.Actor, approval, reason)); err != nil {
		return decisionOutcome{}, err
	}
	return decisionOutcome{approval: approval}, repositories.Approvals().Save(approval)
}

// existingDecision reports a decision already recorded for the incident.
// Replaying the same decision returns it; any other is a mismatch.
func (s *ApprovalService) existingDecision(
	repositories ports.Repositories,
	command ApprovalCommand,
	decision domain.ApprovalDecision,
	reason string,
) (domain.Approval, bool, error) {
	existing, err := repositories.Approvals().GetByIncident(command.IncidentID)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.Approval{}, false, nil
	}
	if err != nil {
		return domain.Approval{}, true, err
	}
	if existing.Actor() == command.Actor && existing.Decision() == decision && existing.ActionID() == command.ActionID &&
		existing.ProposalHash() == command.ProposalHash &&
		existing.RejectionReason() == reason {
		return existing, true, nil
	}
	return domain.Approval{}, true, &domain.ApprovalMismatchError{Reason: "a different approval decision is already recorded"}
}

// expire marks the incident expired because its proposal ran out before a
// decision was recorded; an incident already expired is left as it is.
func (s *ApprovalService) expire(repositories ports.Repositories, incident domain.Incident, now time.Time) error {
	if incident.Status() == domain.StatusExpired {
		return nil
	}
	_, err := s.transition(repositories, incident, now, (*domain.Incident).Expire, terminated(s.systemActor, domain.EventIncidentExpired, "proposal expired"))
	return err
}
