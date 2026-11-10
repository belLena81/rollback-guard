package mcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/platform/resilience"
	"github.com/belLena81/rollback-guard/internal/ports"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// These tests run the real RollbackExecutor adapter under the resilience
// decorator to prove which failures reach the retry rule unchanged.

func fastRetrier(t *testing.T) *resilience.Retrier {
	t.Helper()
	timing := resilience.Timing{Now: time.Now, Sleep: func(context.Context, time.Duration) error { return nil }, Jitter: func() float64 { return 0 }}
	retrier, err := resilience.NewRetrier(resilience.DependencyKubernetesRollbackMCP, resilience.RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}, timing, nil)
	if err != nil {
		t.Fatal(err)
	}
	return retrier
}

func dialFailure() error {
	// The shape the MCP SDK produces: the dial error wrapped in transport text.
	return fmt.Errorf("sending tools/call: Post: %w", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED})
}

func TestRollbackExecutorRetriesPrepareOnlyWhenTheDialFailed(t *testing.T) {
	approved := testApprovedRollback(t, time.Now().UTC())
	prepares, executes := 0, 0
	readonly := callerFunc(func(context.Context, *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
		return nil, dialFailure()
	})
	rollback := callerFunc(func(_ context.Context, params *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
		switch params.Name {
		case ToolPrepareRollback:
			prepares++
		case ToolExecuteApprovedRollback:
			executes++
		}
		return nil, dialFailure()
	})
	adapter, err := NewRollbackExecutor(rollback, readonly, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	decorated, err := resilience.NewExecutor(adapter, fastRetrier(t))
	if err != nil {
		t.Fatal(err)
	}
	_, err = decorated.ExecuteApprovedRollback(context.Background(), approved, "key")
	if !resilience.RequestNotSent(err) || prepares != 2 || executes != 0 {
		t.Fatalf("err=%v prepares=%d executes=%d, want two bounded prepare attempts and no execute", err, prepares, executes)
	}
}

func TestRollbackExecutorExecuteFailuresAfterTheRequestWasSentAreNotRetried(t *testing.T) {
	for name, executeErr := range map[string]error{
		"reset after send": &net.OpError{Op: "read", Err: syscall.ECONNRESET},
		"timeout":          context.DeadlineExceeded,
	} {
		t.Run(name, func(t *testing.T) {
			approved := testApprovedRollback(t, time.Now().UTC())
			proposal := approved.Proposal()
			executes := 0
			readonly := callerFunc(func(context.Context, *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
				return nil, errors.New("readonly unreachable")
			})
			rollback := callerFunc(func(_ context.Context, params *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
				if params.Name == ToolPrepareRollback {
					return successfulToolResult(PrepareRollbackOutput{ProposalHash: string(proposal.Hash()), TargetRevision: string(proposal.TargetRevision())}), nil
				}
				executes++
				return nil, executeErr
			})
			adapter, err := NewRollbackExecutor(rollback, readonly, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			decorated, err := resilience.NewExecutor(adapter, fastRetrier(t))
			if err != nil {
				t.Fatal(err)
			}
			_, err = decorated.ExecuteApprovedRollback(context.Background(), approved, "key")
			if err == nil || resilience.RequestNotSent(err) {
				t.Fatalf("err = %v, want a failure that is not classified as not-sent", err)
			}
			if executes != 1 {
				t.Fatalf("executes = %d, want exactly one: the decorator must not repeat a sent request", executes)
			}
			if !errors.Is(err, ports.ErrRollbackOutcomeUnknown) {
				t.Fatalf("err = %v, want the adapter's unknown-outcome error preserved", err)
			}
		})
	}
}
