package mcp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRollbackExecutorReconcilesCanceledMutationWithDetachedBoundedContext(t *testing.T) {
	now := time.Now().UTC()
	approved := testApprovedRollback(t, now)
	proposal := approved.Proposal()
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), reconciliationContextKey{}, "trace"))
	defer cancel()

	readCalls := 0
	readonly := callerFunc(func(callCtx context.Context, params *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
		readCalls++
		switch readCalls {
		case 1:
			return successfulToolResult(deploymentOutput(proposal, proposal.CurrentRevision(), false, now)), nil
		case 2:
			if callCtx.Err() != nil {
				return nil, callCtx.Err()
			}
			if callCtx.Value(reconciliationContextKey{}) != "trace" {
				t.Error("reconciliation context lost request values")
			}
			if _, ok := callCtx.Deadline(); !ok {
				t.Error("reconciliation context has no deadline")
			}
			return successfulToolResult(deploymentOutput(proposal, proposal.TargetRevision(), true, now)), nil
		case 3:
			if params.Name != ToolWatchRollout {
				t.Fatalf("third readonly call = %q, want %q", params.Name, ToolWatchRollout)
			}
			if callCtx.Err() != nil {
				return nil, callCtx.Err()
			}
			return successfulToolResult(RolloutOutput{Revision: string(proposal.TargetRevision()), DesiredReplicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1, ObservedGeneration: 2, Generation: 2, Complete: true}), nil
		default:
			t.Fatalf("unexpected readonly call %d to %q", readCalls, params.Name)
			return nil, nil
		}
	})
	rollback := callerFunc(func(_ context.Context, params *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
		switch params.Name {
		case ToolPrepareRollback:
			return successfulToolResult(PrepareRollbackOutput{ProposalHash: string(proposal.Hash()), TargetRevision: string(proposal.TargetRevision())}), nil
		case ToolExecuteApprovedRollback:
			cancel()
			return nil, context.Canceled
		default:
			t.Fatalf("unexpected rollback call to %q", params.Name)
			return nil, nil
		}
	})
	executor, err := NewRollbackExecutor(rollback, readonly, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	execution, err := executor.ExecuteApprovedRollback(ctx, approved, "cancel-reconcile")
	if err != nil {
		t.Fatal(err)
	}
	if execution.Outcome() != domain.ExecutionSucceeded || execution.ToRevision() != proposal.TargetRevision() {
		t.Fatalf("execution = %#v", execution)
	}
}

func TestRollbackExecutorReturnsUnknownWhenCanceledMutationCannotBeReconciled(t *testing.T) {
	now := time.Now().UTC()
	approved := testApprovedRollback(t, now)
	proposal := approved.Proposal()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	readCalls := 0
	readonly := callerFunc(func(callCtx context.Context, _ *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
		readCalls++
		if callCtx.Err() != nil {
			return nil, callCtx.Err()
		}
		return successfulToolResult(deploymentOutput(proposal, proposal.CurrentRevision(), false, now)), nil
	})
	rollback := callerFunc(func(_ context.Context, params *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
		if params.Name == ToolPrepareRollback {
			return successfulToolResult(PrepareRollbackOutput{ProposalHash: string(proposal.Hash()), TargetRevision: string(proposal.TargetRevision())}), nil
		}
		cancel()
		return nil, context.Canceled
	})
	executor, err := NewRollbackExecutor(rollback, readonly, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	_, err = executor.ExecuteApprovedRollback(ctx, approved, "cancel-unknown")
	if !errors.Is(err, ports.ErrRollbackOutcomeUnknown) {
		t.Fatalf("ExecuteApprovedRollback() error = %v", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("ambiguous mutation returned definitive cancellation: %v", err)
	}
	if readCalls != 2 {
		t.Fatalf("readonly calls = %d, want 2", readCalls)
	}
}

func TestRollbackExecutorDoesNotReconcileCancellationBeforeMutation(t *testing.T) {
	now := time.Now().UTC()
	approved := testApprovedRollback(t, now)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	readonly := callerFunc(func(callCtx context.Context, _ *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
		return nil, callCtx.Err()
	})
	executeCalls := 0
	rollback := callerFunc(func(callCtx context.Context, params *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
		if params.Name == ToolExecuteApprovedRollback {
			executeCalls++
		}
		return nil, callCtx.Err()
	})
	executor, err := NewRollbackExecutor(rollback, readonly, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	_, err = executor.ExecuteApprovedRollback(ctx, approved, "cancel-before-mutation")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ExecuteApprovedRollback() error = %v", err)
	}
	if errors.Is(err, ports.ErrRollbackOutcomeUnknown) {
		t.Fatalf("pre-mutation cancellation became ambiguous: %v", err)
	}
	if executeCalls != 0 {
		t.Fatalf("execute calls = %d, want 0", executeCalls)
	}
}

type reconciliationContextKey struct{}

type callerFunc func(context.Context, *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error)

func (f callerFunc) CallTool(ctx context.Context, params *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
	return f(ctx, params)
}

func successfulToolResult[T any](output T) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{StructuredContent: Success(output)}
}

func deploymentOutput(proposal domain.RollbackProposal, revision domain.Revision, healthy bool, now time.Time) DeploymentOutput {
	ready := 0
	if healthy {
		ready = 1
	}
	return DeploymentOutput{Target: TargetFromDomain(proposal.Target()), CurrentRevision: string(revision), PreviousStableRevision: string(proposal.TargetRevision()), DesiredReplicas: 1, ReadyReplicas: ready, AvailableReplicas: ready, Healthy: healthy, RolloutAt: now.Add(-time.Minute)}
}

func testApprovedRollback(t *testing.T, now time.Time) domain.ApprovedRollback {
	t.Helper()
	target, err := domain.NewDeploymentTarget("local", "demo", "demo-api", "demo-api")
	if err != nil {
		t.Fatal(err)
	}
	decision, err := domain.NewPolicyDecision("policy", "incident", []domain.RuleResult{{Name: "eligible", Passed: true}}, now)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := domain.NewRollbackProposal("action", "incident", decision.ID(), target, "revision-bad", "revision-stable", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	actor, err := domain.NewActor("operator", domain.ActorHuman)
	if err != nil {
		t.Fatal(err)
	}
	approval, err := domain.NewApproval("approval", actor, domain.ApprovalApproved, proposal.ActionID(), proposal.Hash(), now, now.Add(time.Minute), "")
	if err != nil {
		t.Fatal(err)
	}
	approved, err := domain.AuthorizeRollback(proposal, decision, approval, domain.DeploymentEvidence{Target: target, CurrentRevision: proposal.CurrentRevision()}, now)
	if err != nil {
		t.Fatal(err)
	}
	return approved
}
