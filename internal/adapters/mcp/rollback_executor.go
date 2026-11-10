package mcp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

type RollbackExecutor struct {
	rollback Caller
	readonly Caller
	timeout  time.Duration
}

func NewRollbackExecutor(rollback, readonly Caller, timeout time.Duration) (*RollbackExecutor, error) {
	if rollback == nil || readonly == nil || !domain.ValidRolloutWatch(timeout) {
		return nil, fmt.Errorf("invalid rollback MCP adapter configuration")
	}
	return &RollbackExecutor{rollback: rollback, readonly: readonly, timeout: timeout}, nil
}

func (a *RollbackExecutor) ExecuteApprovedRollback(ctx context.Context, approved domain.ApprovedRollback, key domain.IdempotencyKey) (domain.RollbackExecution, error) {
	proposal := approved.Proposal()
	current, inspectErr := a.inspect(ctx, proposal.Target())
	if inspectErr != nil && ambiguousToolError(inspectErr) {
		return domain.RollbackExecution{}, fmt.Errorf("inspect before rollback: %w", ports.ErrRollbackOutcomeUnknown)
	}
	alreadyApplied := inspectErr == nil && current.CurrentRevision == proposal.TargetRevision()
	if !alreadyApplied {
		prepare := PrepareRollbackInput{Proposal: ProposalFromDomain(proposal)}
		prepared, err := callTool[PrepareRollbackInput, PrepareRollbackOutput](ctx, a.rollback, ToolPrepareRollback, prepare)
		if err != nil {
			return domain.RollbackExecution{}, err
		}
		if prepared.ProposalHash != string(proposal.Hash()) || prepared.TargetRevision != string(proposal.TargetRevision()) {
			return domain.RollbackExecution{}, fmt.Errorf("rollback MCP preparation mismatch")
		}
	}
	input := ExecuteRollbackInput{ProposalHash: string(proposal.Hash()), IdempotencyKey: string(key)}
	mutation, err := a.executeMutation(ctx, input, proposal)
	waitCtx := ctx
	if err != nil {
		if !ambiguousToolError(err) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			return domain.RollbackExecution{}, err
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

func (a *RollbackExecutor) executeMutation(ctx context.Context, input ExecuteRollbackInput, proposal domain.RollbackProposal) (ExecuteRollbackOutput, error) {
	mutation, err := callTool[ExecuteRollbackInput, ExecuteRollbackOutput](ctx, a.rollback, ToolExecuteApprovedRollback, input)
	if err != nil {
		return ExecuteRollbackOutput{}, err
	}
	if mutation.FromRevision != string(proposal.CurrentRevision()) || mutation.ToRevision != string(proposal.TargetRevision()) || mutation.ExecutionID == "" {
		return ExecuteRollbackOutput{}, fmt.Errorf("rollback MCP returned inconsistent mutation result")
	}
	return mutation, nil
}

func (a *RollbackExecutor) waitAndBuild(ctx context.Context, approved domain.ApprovedRollback, key domain.IdempotencyKey, startedAt time.Time, mutationCompletedAt time.Time) (domain.RollbackExecution, error) {
	proposal := approved.Proposal()
	watchCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	output, err := callTool[WatchRolloutInput, RolloutOutput](watchCtx, a.readonly, ToolWatchRollout, WatchRolloutInput{Target: TargetFromDomain(proposal.Target()), ExpectedRevision: string(proposal.TargetRevision()), TimeoutSeconds: int(a.timeout / time.Second)})
	if err != nil || !output.Complete {
		return domain.RollbackExecution{}, fmt.Errorf("watch approved rollback: %w", ports.ErrRollbackOutcomeUnknown)
	}
	sum := deterministicExecutionID(key, proposal.Hash())
	completedAt := rollbackExecutionCompletedAt(startedAt, mutationCompletedAt, time.Now().UTC())
	return domain.NewRollbackExecution(sum, proposal.IncidentID(), key, proposal.CurrentRevision(), proposal.TargetRevision(), domain.ExecutionSucceeded, startedAt, completedAt, "")
}

func (a *RollbackExecutor) inspect(ctx context.Context, target domain.DeploymentTarget) (domain.DeploymentEvidence, error) {
	inspector, err := NewDeploymentInspector(a.readonly)
	if err != nil {
		return domain.DeploymentEvidence{}, err
	}
	return inspector.InspectDeployment(ctx, target)
}

func deterministicExecutionID(key domain.IdempotencyKey, hash domain.ProposalHash) domain.ExecutionID {
	return domain.ExecutionID("execution-" + sha256Hex(string(key) + "\n" + string(hash))[:32])
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
