package mcp

import (
	"context"
	"errors"
	"testing"
	"time"

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

type blockingCaller struct{}

func (blockingCaller) CallTool(ctx context.Context, _ *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
