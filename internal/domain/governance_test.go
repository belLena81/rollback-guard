package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
)

func TestProposalHashBindsEveryCanonicalField(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 123, time.UTC)
	baseTarget := mustTarget(t, "cluster", "namespace", "api", "service")
	base := mustProposal(t, "action", "incident", "policy", baseTarget, "revision-2", "revision-1", now)
	tests := []struct {
		name     string
		proposal domain.RollbackProposal
	}{
		{name: "action", proposal: mustProposal(t, "different", "incident", "policy", baseTarget, "revision-2", "revision-1", now)},
		{name: "incident", proposal: mustProposal(t, "action", "different", "policy", baseTarget, "revision-2", "revision-1", now)},
		{name: "policy", proposal: mustProposal(t, "action", "incident", "different", baseTarget, "revision-2", "revision-1", now)},
		{name: "cluster", proposal: mustProposal(t, "action", "incident", "policy", mustTarget(t, "different", "namespace", "api", "service"), "revision-2", "revision-1", now)},
		{name: "namespace", proposal: mustProposal(t, "action", "incident", "policy", mustTarget(t, "cluster", "different", "api", "service"), "revision-2", "revision-1", now)},
		{name: "deployment", proposal: mustProposal(t, "action", "incident", "policy", mustTarget(t, "cluster", "namespace", "different", "service"), "revision-2", "revision-1", now)},
		{name: "service", proposal: mustProposal(t, "action", "incident", "policy", mustTarget(t, "cluster", "namespace", "api", "different"), "revision-2", "revision-1", now)},
		{name: "current revision", proposal: mustProposal(t, "action", "incident", "policy", baseTarget, "revision-3", "revision-1", now)},
		{name: "target revision", proposal: mustProposal(t, "action", "incident", "policy", baseTarget, "revision-2", "revision-0", now)},
		{name: "expiry", proposal: mustProposal(t, "action", "incident", "policy", baseTarget, "revision-2", "revision-1", now.Add(time.Microsecond))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.proposal.Hash() == base.Hash() {
				t.Fatalf("changed field did not change hash %q", base.Hash())
			}
		})
	}
}

func TestProposalHashSurvivesPostgresTimestampPrecision(t *testing.T) {
	expiresAt := time.Date(2026, 8, 3, 10, 0, 0, 123456789, time.UTC)
	proposal := mustProposal(t, "action", "incident", "policy", mustTarget(t, "cluster", "namespace", "api", "service"), "revision-2", "revision-1", expiresAt)
	snapshot := proposal.Snapshot()
	snapshot.ExpiresAt = snapshot.ExpiresAt.Truncate(time.Microsecond)
	rehydrated, err := domain.RehydrateRollbackProposal(snapshot)
	if err != nil {
		t.Fatalf("RehydrateRollbackProposal() error = %v", err)
	}
	if rehydrated.Hash() != proposal.Hash() {
		t.Fatalf("rehydrated hash = %s, want %s", rehydrated.Hash(), proposal.Hash())
	}
	if rehydrated.ExpiresAt() != snapshot.ExpiresAt {
		t.Fatalf("rehydrated expires_at = %s, want %s", rehydrated.ExpiresAt(), snapshot.ExpiresAt)
	}
}

func TestExpiryBoundaryIsExact(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	proposal := mustProposal(t, "action", "incident", "policy", mustTarget(t, "cluster", "namespace", "api", "service"), "revision-2", "revision-1", now.Add(time.Minute))
	if proposal.Expired(now.Add(time.Minute - time.Nanosecond)) {
		t.Fatal("proposal expired before boundary")
	}
	if !proposal.Expired(now.Add(time.Minute)) {
		t.Fatal("proposal did not expire at boundary")
	}
}

func TestAuthorizeRollbackRejectsRevisionDrift(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	target := mustTarget(t, "cluster", "namespace", "api", "service")
	proposal := mustProposal(t, "action", "incident", "policy", target, "revision-2", "revision-1", now.Add(time.Minute))
	decision, err := domain.NewPolicyDecision("policy", "incident", []domain.RuleResult{{Name: "allowed", Passed: true}}, now)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := domain.NewActor("alice", domain.ActorHuman)
	if err != nil {
		t.Fatal(err)
	}
	approval, err := domain.NewApproval("approval", actor, domain.ApprovalApproved, proposal.ActionID(), proposal.Hash(), now, now.Add(time.Minute), "")
	if err != nil {
		t.Fatal(err)
	}
	fresh := domain.DeploymentEvidence{Target: target, CurrentRevision: "revision-3"}
	_, err = domain.AuthorizeRollback(proposal, decision, approval, fresh, now)
	if !errors.Is(err, domain.ErrApprovalMismatch) {
		t.Fatalf("AuthorizeRollback() error = %v, want approval mismatch", err)
	}
}

func TestAuthorizeRollbackChecksEveryAuthorizationBoundary(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	target := mustTarget(t, "cluster", "namespace", "api", "service")
	otherTarget := mustTarget(t, "other", "namespace", "api", "service")
	actor, err := domain.NewActor("alice", domain.ActorHuman)
	if err != nil {
		t.Fatal(err)
	}
	proposal := mustProposal(t, "action", "incident", "policy", target, "revision-2", "revision-1", now.Add(2*time.Minute))
	allowed, err := domain.NewPolicyDecision("policy", "incident", []domain.RuleResult{{Name: "allowed", Passed: true}}, now)
	if err != nil {
		t.Fatal(err)
	}
	approved, err := domain.NewApproval("approval", actor, domain.ApprovalApproved, proposal.ActionID(), proposal.Hash(), now, now.Add(time.Minute), "")
	if err != nil {
		t.Fatal(err)
	}
	fresh := domain.DeploymentEvidence{Target: target, CurrentRevision: "revision-2"}

	tests := []struct {
		name     string
		proposal domain.RollbackProposal
		decision domain.PolicyDecision
		approval domain.Approval
		fresh    domain.DeploymentEvidence
		now      time.Time
		want     error
	}{
		{name: "valid", proposal: proposal, decision: allowed, approval: approved, fresh: fresh, now: now},
		{name: "proposal expired", proposal: proposal, decision: allowed, approval: approved, fresh: fresh, now: now.Add(2 * time.Minute), want: domain.ErrExpired},
		{name: "approval expired", proposal: proposal, decision: allowed, approval: approved, fresh: fresh, now: now.Add(time.Minute), want: domain.ErrExpired},
		{name: "different target", proposal: proposal, decision: allowed, approval: approved, fresh: domain.DeploymentEvidence{Target: otherTarget, CurrentRevision: "revision-2"}, now: now, want: domain.ErrApprovalMismatch},
		{name: "revision drift", proposal: proposal, decision: allowed, approval: approved, fresh: domain.DeploymentEvidence{Target: target, CurrentRevision: "revision-3"}, now: now, want: domain.ErrApprovalMismatch},
	}
	denied, err := domain.NewPolicyDecision("policy", "incident", []domain.RuleResult{{Name: "denied", Passed: false}}, now)
	if err != nil {
		t.Fatal(err)
	}
	tests = append(tests, struct {
		name     string
		proposal domain.RollbackProposal
		decision domain.PolicyDecision
		approval domain.Approval
		fresh    domain.DeploymentEvidence
		now      time.Time
		want     error
	}{name: "policy denied", proposal: proposal, decision: denied, approval: approved, fresh: fresh, now: now, want: domain.ErrPolicyDenied})
	rejected, err := domain.NewApproval("rejection", actor, domain.ApprovalRejected, proposal.ActionID(), proposal.Hash(), now, now.Add(time.Minute), "unsafe")
	if err != nil {
		t.Fatal(err)
	}
	tests = append(tests, struct {
		name     string
		proposal domain.RollbackProposal
		decision domain.PolicyDecision
		approval domain.Approval
		fresh    domain.DeploymentEvidence
		now      time.Time
		want     error
	}{name: "rejected", proposal: proposal, decision: allowed, approval: rejected, fresh: fresh, now: now, want: domain.ErrApprovalMismatch})

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := domain.AuthorizeRollback(test.proposal, test.decision, test.approval, test.fresh, test.now)
			if test.want == nil && err != nil {
				t.Fatalf("AuthorizeRollback() error = %v", err)
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("AuthorizeRollback() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestResumeApprovedRollbackRequiresMatchingPendingRecord(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	target := mustTarget(t, "cluster", "namespace", "api", "service")
	proposal := mustProposal(t, "action", "incident", "policy", target, "revision-2", "revision-1", now.Add(time.Minute))
	decision, err := domain.NewPolicyDecision("policy", "incident", []domain.RuleResult{{Name: "allowed", Passed: true}}, now)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := domain.NewActor("alice", domain.ActorHuman)
	if err != nil {
		t.Fatal(err)
	}
	approval, err := domain.NewApproval("approval", actor, domain.ApprovalApproved, proposal.ActionID(), proposal.Hash(), now, now.Add(time.Minute), "")
	if err != nil {
		t.Fatal(err)
	}
	record, err := domain.NewPendingIdempotencyRecord("execute", domain.IdempotencyOperationExecuteRollback, string(proposal.Hash()), string(proposal.IncidentID()), now)
	if err != nil {
		t.Fatal(err)
	}
	approved, err := domain.ResumeApprovedRollback(proposal, decision, approval, record, "execute")
	if err != nil {
		t.Fatal(err)
	}
	if approved.Proposal().Hash() != proposal.Hash() || approved.Approval().ID() != approval.ID() || approved.PolicyDecision().ID() != decision.ID() {
		t.Fatal("resumed rollback does not preserve proposal, policy decision, and approval")
	}

	tests := []struct {
		name, requestHash, resourceID string
		key                           domain.IdempotencyKey
	}{
		{name: "key", key: "different", requestHash: string(proposal.Hash()), resourceID: string(proposal.IncidentID())},
		{name: "request hash", key: "execute", requestHash: "different", resourceID: string(proposal.IncidentID())},
		{name: "resource", key: "execute", requestHash: string(proposal.Hash()), resourceID: "different"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed, createErr := domain.NewPendingIdempotencyRecord("execute", domain.IdempotencyOperationExecuteRollback, test.requestHash, test.resourceID, now)
			if createErr != nil {
				t.Fatal(createErr)
			}
			_, err := domain.ResumeApprovedRollback(proposal, decision, approval, changed, test.key)
			if !errors.Is(err, domain.ErrIdempotencyConflict) {
				t.Fatalf("ResumeApprovedRollback() error = %v, want idempotency conflict", err)
			}
		})
	}
}

func mustTarget(t *testing.T, cluster, namespace, deployment, service string) domain.DeploymentTarget {
	t.Helper()
	target, err := domain.NewDeploymentTarget(cluster, namespace, deployment, service)
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func mustProposal(t *testing.T, action domain.ActionID, incident domain.IncidentID, policy domain.PolicyDecisionID, target domain.DeploymentTarget, current, previous domain.Revision, expiresAt time.Time) domain.RollbackProposal {
	t.Helper()
	proposal, err := domain.NewRollbackProposal(action, incident, policy, target, current, previous, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	return proposal
}
