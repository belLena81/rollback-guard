package mcp

import (
	"fmt"

	"github.com/belLena81/rollback-guard/internal/domain"
)

func ProposalFromDomain(proposal domain.RollbackProposal) Proposal {
	return Proposal{ActionID: string(proposal.ActionID()), IncidentID: string(proposal.IncidentID()), PolicyDecisionID: string(proposal.PolicyDecisionID()), Target: TargetFromDomain(proposal.Target()), CurrentRevision: string(proposal.CurrentRevision()), TargetRevision: string(proposal.TargetRevision()), ExpiresAt: proposal.ExpiresAt(), Hash: string(proposal.Hash())}
}

func (p Proposal) Domain() (domain.RollbackProposal, error) {
	target, err := p.Target.Domain()
	if err != nil {
		return domain.RollbackProposal{}, err
	}
	return domain.RehydrateRollbackProposal(domain.RollbackProposalSnapshot{ActionID: domain.ActionID(p.ActionID), IncidentID: domain.IncidentID(p.IncidentID), PolicyDecisionID: domain.PolicyDecisionID(p.PolicyDecisionID), Target: target, CurrentRevision: domain.Revision(p.CurrentRevision), TargetRevision: domain.Revision(p.TargetRevision), ExpiresAt: p.ExpiresAt, Hash: domain.ProposalHash(p.Hash)})
}

func PolicyDecisionFromDomain(decision domain.PolicyDecision) PolicyDecision {
	rules := decision.Rules()
	result := make([]Rule, len(rules))
	for index, rule := range rules {
		result[index] = Rule{Name: rule.Name, Passed: rule.Passed}
	}
	return PolicyDecision{ID: string(decision.ID()), IncidentID: string(decision.IncidentID()), Rules: result, DecidedAt: decision.DecidedAt()}
}

func (d PolicyDecision) Domain() (domain.PolicyDecision, error) {
	rules := make([]domain.RuleResult, len(d.Rules))
	for index, rule := range d.Rules {
		rules[index] = domain.RuleResult{Name: rule.Name, Passed: rule.Passed}
	}
	return domain.NewPolicyDecision(domain.PolicyDecisionID(d.ID), domain.IncidentID(d.IncidentID), rules, d.DecidedAt)
}

func ApprovalFromDomain(approval domain.Approval) Approval {
	return Approval{ID: string(approval.ID()), ActorID: string(approval.Actor().ID()), ActorKind: approval.Actor().Kind().String(), Decision: approval.Decision().String(), ActionID: string(approval.ActionID()), ProposalHash: string(approval.ProposalHash()), DecidedAt: approval.DecidedAt(), ExpiresAt: approval.ExpiresAt()}
}

func (a Approval) Domain() (domain.Approval, error) {
	var kind domain.ActorKind
	switch a.ActorKind {
	case "human":
		kind = domain.ActorHuman
	case "agent":
		kind = domain.ActorAgent
	case "system":
		kind = domain.ActorSystem
	case "service":
		kind = domain.ActorService
	default:
		return domain.Approval{}, &domain.ValidationError{Field: "approval.actor_kind", Reason: "unknown kind"}
	}
	actor, err := domain.NewActor(domain.ActorID(a.ActorID), kind)
	if err != nil {
		return domain.Approval{}, err
	}
	var decision domain.ApprovalDecision
	switch a.Decision {
	case "approved":
		decision = domain.ApprovalApproved
	case "rejected":
		decision = domain.ApprovalRejected
	default:
		return domain.Approval{}, &domain.ValidationError{Field: "approval.decision", Reason: "unknown decision"}
	}
	approval, err := domain.NewApproval(domain.ApprovalID(a.ID), actor, decision, domain.ActionID(a.ActionID), domain.ProposalHash(a.ProposalHash), a.DecidedAt, a.ExpiresAt, "")
	if err != nil {
		return domain.Approval{}, fmt.Errorf("approval: %w", err)
	}
	return approval, nil
}
