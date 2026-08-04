package mcp

import "github.com/belLena81/rollback-guard/internal/domain"

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
