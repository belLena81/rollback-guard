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
	now := s.clock.Now()
	var approval domain.Approval
	expired := false
	err := s.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
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
				if _, err := s.transition(ctx, repositories, incident, now, (*domain.Incident).Expire, terminated(s.systemActor, domain.EventIncidentExpired, "proposal expired")); err != nil {
					return err
				}
			}
			expired = true
			return nil
		}
		if command.ActionID != proposal.ActionID() || command.ProposalHash != proposal.Hash() {
			return &domain.ApprovalMismatchError{Reason: "action or proposal hash does not match"}
		}
		expiresAt := now.Add(s.approvalTTL)
		if proposal.ExpiresAt().Before(expiresAt) {
			expiresAt = proposal.ExpiresAt()
		}
		approval, err = domain.NewApproval(domain.ApprovalID(s.ids.NewID("approval")), command.Actor, decision, command.ActionID, command.ProposalHash, now, expiresAt, reason)
		if err != nil {
			return err
		}
		apply := (*domain.Incident).Approve
		if decision != domain.ApprovalApproved {
			apply = (*domain.Incident).Reject
		}
		// Transition first: a concurrent request that recorded the same
		// decision has already moved the incident on, so this one fails on
		// the version guard (a conflict) instead of a duplicate approval
		// insert. Both writes commit or roll back together.
		if _, err := s.transition(ctx, repositories, incident, now, apply, approvalEvents(command.Actor, approval, reason)); err != nil {
			return err
		}
		return repositories.Approvals().Save(approval)
	})
	if err != nil {
		return domain.Approval{}, fmt.Errorf("record approval decision: %w", err)
	}
	if expired {
		return domain.Approval{}, &domain.ExpiryError{Resource: "proposal"}
	}
	return approval, nil
}
