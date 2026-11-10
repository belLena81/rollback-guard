package mcp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/resilience"
	"github.com/belLena81/rollback-guard/internal/ports"
)

type RollbackExecutor struct {
	rollback Caller
	readonly Caller
	timeout  time.Duration
	clock    ports.Clock
}

func NewRollbackExecutor(rollback, readonly Caller, timeout time.Duration, clock ports.Clock) (*RollbackExecutor, error) {
	if rollback == nil || readonly == nil || clock == nil || !domain.ValidRolloutWatch(timeout) {
		return nil, errors.New("invalid rollback MCP adapter configuration")
	}
	return &RollbackExecutor{rollback: rollback, readonly: readonly, timeout: timeout, clock: clock}, nil
}

func (a *RollbackExecutor) ExecuteApprovedRollback(
	ctx context.Context,
	approved domain.ApprovedRollback,
	key domain.IdempotencyKey,
) (domain.RollbackExecution, error) {
	proposal := approved.Proposal()
	current, inspectErr := a.inspect(ctx, proposal.Target())
	if inspectErr != nil && ambiguousToolError(inspectErr) {
		return domain.RollbackExecution{}, fmt.Errorf("inspect before rollback: %w", ports.ErrRollbackOutcomeUnknown)
	}
	alreadyApplied := inspectErr == nil && current.CurrentRevision == proposal.TargetRevision()
	if !alreadyApplied {
		if err := a.prepare(ctx, proposal, inspectErr); err != nil {
			return domain.RollbackExecution{}, err
		}
	}
	input := ExecuteRollbackInput{ProposalHash: string(proposal.Hash()), IdempotencyKey: string(key)}
	mutation, err := a.executeMutation(ctx, input, proposal)
	waitCtx := ctx
	if err != nil {
		switch mutationFailure(err) {
		case mutationNotSent, mutationRejected:
			return domain.RollbackExecution{}, err
		case mutationAmbiguous:
		}
		reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.timeout)
		defer cancel()
		reconciled, reconcileErr := a.inspect(reconcileCtx, proposal.Target())
		if reconcileErr != nil || reconciled.CurrentRevision != proposal.TargetRevision() {
			return domain.RollbackExecution{}, fmt.Errorf("execute rollback outcome: %w", ports.ErrRollbackOutcomeUnknown)
		}
		mutation, err = a.executeMutation(reconcileCtx, input, proposal)
		if err != nil {
			return domain.RollbackExecution{}, fmt.Errorf("replay rollback mutation: %w", ports.ErrRollbackOutcomeUnknown)
		}
		waitCtx = reconcileCtx
	}
	return a.waitAndBuild(waitCtx, approved, key, mutation.StartedAt, mutation.CompletedAt)
}

// prepare asks the rollback server to validate the proposal before the
// mutation. inspectErr is the failure of the inspection that preceded it, nil
// when the deployment was read.
func (a *RollbackExecutor) prepare(ctx context.Context, proposal domain.RollbackProposal, inspectErr error) error {
	if err := ctx.Err(); err != nil {
		// The caller gave up before anything that can mutate was sent.
		return fmt.Errorf("before prepare rollback: %w", err)
	}
	prepare := PrepareRollbackInput{Proposal: ProposalFromDomain(proposal)}
	prepared, err := callTool[PrepareRollbackInput, PrepareRollbackOutput](ctx, a.rollback, ToolPrepareRollback, prepare)
	if err != nil {
		// With a failed inspection, "not applied" was never
		// established: a revision-drift answer may mean an earlier
		// attempt already applied this rollback. Only a request that
		// never left stays a plain (retryable) failure.
		if inspectErr != nil && !resilience.RequestNotSent(err) {
			return fmt.Errorf("prepare rollback after failed inspection: %w", ports.ErrRollbackOutcomeUnknown)
		}
		return err
	}
	if prepared.ProposalHash != string(proposal.Hash()) || prepared.TargetRevision != string(proposal.TargetRevision()) {
		return errors.New("rollback MCP preparation mismatch")
	}
	return nil
}

// mutationFailureKind classifies a failed execute_approved_rollback call by
// what it proves about the server.
type mutationFailureKind uint8

const (
	// mutationAmbiguous: the request may have reached the server and the
	// server may have applied the rollback. Only reconciliation can tell.
	mutationAmbiguous mutationFailureKind = iota + 1
	// mutationNotSent: the TCP dial failed, so the request never left this
	// process. Returned unchanged so the resilience decorator may repeat it.
	mutationNotSent
	// mutationRejected: the server answered with a definitive rejection
	// (validation, approval, policy, expiry, idempotency, not found) and did
	// not mutate.
	mutationRejected
)

// mutationRejections are the outcomes the server reports only after deciding
// not to mutate. errorFromTool produces them from the server's structured
// tool error; no transport failure wraps them.
var mutationRejections = []error{
	domain.ErrValidation, domain.ErrNotFound, domain.ErrApprovalMismatch, domain.ErrNonHumanApproval,
	domain.ErrPolicyDenied, domain.ErrExpired, domain.ErrIdempotencyConflict,
}

// mutationFailure is fail-safe: anything that is neither a provable pre-send
// dial failure nor a definitive server rejection is ambiguous. That covers
// EOF, connection resets, timeouts and cancellation, HTTP 5xx, a lost or
// expired session, internal and unknown tool error codes, and responses that
// cannot be decoded or do not match the request.
func mutationFailure(err error) mutationFailureKind {
	if resilience.RequestNotSent(err) {
		return mutationNotSent
	}
	for _, rejection := range mutationRejections {
		if errors.Is(err, rejection) {
			return mutationRejected
		}
	}
	return mutationAmbiguous
}

func (a *RollbackExecutor) executeMutation(ctx context.Context, input ExecuteRollbackInput, proposal domain.RollbackProposal) (ExecuteRollbackOutput, error) {
	mutation, err := callTool[ExecuteRollbackInput, ExecuteRollbackOutput](ctx, a.rollback, ToolExecuteApprovedRollback, input)
	if err != nil {
		return ExecuteRollbackOutput{}, err
	}
	if mutation.FromRevision != string(proposal.CurrentRevision()) || mutation.ToRevision != string(proposal.TargetRevision()) || mutation.ExecutionID == "" {
		return ExecuteRollbackOutput{}, errors.New("rollback MCP returned inconsistent mutation result")
	}
	return mutation, nil
}

func (a *RollbackExecutor) waitAndBuild(
	ctx context.Context,
	approved domain.ApprovedRollback,
	key domain.IdempotencyKey,
	startedAt time.Time,
	mutationCompletedAt time.Time,
) (domain.RollbackExecution, error) {
	proposal := approved.Proposal()
	watchCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	output, err := callTool[WatchRolloutInput, RolloutOutput](
		watchCtx,
		a.readonly,
		ToolWatchRollout,
		WatchRolloutInput{
			Target:           TargetFromDomain(proposal.Target()),
			ExpectedRevision: string(proposal.TargetRevision()),
			TimeoutSeconds:   int(a.timeout / time.Second),
		},
	)
	if err != nil || !output.Complete {
		return domain.RollbackExecution{}, fmt.Errorf("watch approved rollback: %w", ports.ErrRollbackOutcomeUnknown)
	}
	executionID := domain.DeterministicExecutionID(key, proposal.Hash())
	completedAt := rollbackExecutionCompletedAt(startedAt, mutationCompletedAt, a.clock.Now())
	return domain.NewRollbackExecution(
		executionID,
		proposal.IncidentID(),
		key,
		proposal.CurrentRevision(),
		proposal.TargetRevision(),
		domain.ExecutionSucceeded,
		startedAt,
		completedAt,
		"",
	)
}

func (a *RollbackExecutor) inspect(ctx context.Context, target domain.DeploymentTarget) (domain.DeploymentEvidence, error) {
	inspector, err := NewDeploymentInspector(a.readonly)
	if err != nil {
		return domain.DeploymentEvidence{}, err
	}
	return inspector.InspectDeployment(ctx, target)
}

func rollbackExecutionCompletedAt(startedAt, mutationCompletedAt, observedAt time.Time) time.Time {
	completedAt := observedAt.UTC()
	if completedAt.Before(startedAt) {
		completedAt = startedAt
	}
	if !mutationCompletedAt.IsZero() && completedAt.Before(mutationCompletedAt) {
		completedAt = mutationCompletedAt
	}
	return completedAt
}
