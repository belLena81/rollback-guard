package mcp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestBoundCallsAppliesShortDeadlineExceptToRolloutWatch(t *testing.T) {
	caller, err := BoundCalls(blockingCaller{}, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	_, err = caller.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: ToolGetDeployment})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ordinary call error = %v", err)
	}

	watchCtx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = caller.CallTool(watchCtx, &mcpsdk.CallToolParams{Name: ToolWatchRollout})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("watch call error = %v", err)
	}
}

func TestCallToolMapsTargetForbiddenToDomainClientError(t *testing.T) {
	caller := callerFunc(func(context.Context, *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{
			StructuredContent: Response[DeploymentOutput]{
				OK: false,
				Error: &ToolError{
					Code:    CodeTargetForbidden,
					Message: "target is not allowed",
				},
			},
			IsError: true,
		}, nil
	})

	_, err := callTool[TargetInput, DeploymentOutput](context.Background(), caller, ToolGetDeployment, TargetInput{})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("callTool() error = %v, want validation", err)
	}
	if ambiguousToolError(err) {
		t.Fatalf("target forbidden error treated as ambiguous: %v", err)
	}
}

type blockingCaller struct{}

func (blockingCaller) CallTool(ctx context.Context, _ *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
