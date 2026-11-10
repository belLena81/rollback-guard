package application

import (
	"context"
	"fmt"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

// ApproveAndExecuteService runs one human approval through the whole rollback
// path: record the approval, execute the approved rollback, and verify
// recovery. It owns the order of those steps, the idempotency key that binds
// the execution to the approval, and the decision whether a failure has
// already been recorded as the incident's outcome.
//
// The safety checks themselves stay where they were: ApprovalService binds
// the approval to the exact proposal hash, ExecutionService re-inspects the
// deployment and re-authorizes (policy, expiry, hash, target and current
// revision) before calling the executor, and the executor's idempotency key
// makes a replay of the same approval return the first result without a
// second mutation.
type ApproveAndExecuteService struct {
	approvals     *ApprovalService
	executions    *ExecutionService
	verifications *VerificationService
	queries       *QueryService
}

// executionKeyPrefix namespaces execution idempotency keys derived from an
// approval, so a key can never collide with one minted for another purpose.
const executionKeyPrefix = "approval:"

// executionKeyForApproval derives the execution idempotency key from the
// approval. Every retry of the same approval therefore reuses one key, and
// the executor's ledger applies the mutation at most once per approval.
func executionKeyForApproval(id domain.ApprovalID) domain.IdempotencyKey {
	return domain.IdempotencyKey(executionKeyPrefix + string(id))
}

// Run approves the proposal named in command, executes it, and verifies
// recovery. It returns the incident summary after the last step that ran.
//
// A failed execution or a failed verification that has been committed as the
// incident's terminal outcome is a result, not an error: Run returns the
// summary describing it and a nil error. Every other failure, including an
// unknown rollback outcome (ports.ErrRollbackOutcomeUnknown, which leaves the
// attempt pending so that a retry of the same approval reconciles it), is
// returned as an error.
func (s *ApproveAndExecuteService) Run(ctx context.Context, command ApprovalCommand) (IncidentSummary, error) {
	approval, err := s.approvals.Approve(ctx, command)
	if err != nil {
		return IncidentSummary{}, err
	}
	key := executionKeyForApproval(approval.ID())
	executionCtx := ports.WithCorrelation(ctx, ports.Correlation{IdempotencyKey: string(key)})
	if _, err := s.executions.Execute(executionCtx, command.IncidentID, key); err != nil {
		return s.committedOrError(ctx, command.IncidentID, err, executionFailureCommitted)
	}
	summary, err := s.queries.GetIncidentSummary(ctx, command.IncidentID)
	if err != nil {
		return IncidentSummary{}, fmt.Errorf("load summary after execution: %w", err)
	}
	if summary.Incident.Status() != domain.StatusVerifying {
		// A replay of an approval whose incident has already moved on
		// (resolved, or failed during an earlier verification).
		return summary, nil
	}
	if _, err := s.verifications.Verify(ctx, command.IncidentID); err != nil {
		return s.committedOrError(ctx, command.IncidentID, err, verificationFailureCommitted)
	}
	summary, err = s.queries.GetIncidentSummary(ctx, command.IncidentID)
	if err != nil {
		return IncidentSummary{}, fmt.Errorf("load summary after verification: %w", err)
	}
	return summary, nil
}

// committedOrError returns the incident summary with a nil error when cause
// has already been committed as the incident's terminal outcome, and cause
// otherwise. A summary that cannot be read leaves cause as the answer.
func (s *ApproveAndExecuteService) committedOrError(ctx context.Context, incidentID domain.IncidentID, cause error, committed func(IncidentSummary) bool) (IncidentSummary, error) {
	summary, err := s.queries.GetIncidentSummary(ctx, incidentID)
	if err != nil || !committed(summary) {
		return IncidentSummary{}, cause
	}
	return summary, nil
}

func executionFailureCommitted(summary IncidentSummary) bool {
	return summary.Execution != nil &&
		summary.Incident.Status() == domain.StatusFailed &&
		summary.Execution.Outcome() == domain.ExecutionFailed
}

func verificationFailureCommitted(summary IncidentSummary) bool {
	return summary.Verification != nil &&
		summary.Incident.Status() == domain.StatusFailed &&
		!summary.Verification.Passed()
}
