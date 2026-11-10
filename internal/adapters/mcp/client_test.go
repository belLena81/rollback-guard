package mcp

import (
	"context"
	"errors"
	"slices"
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

func TestRequireToolCatalogComparesExactNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		served   []string
		expected []string
		wantErr  bool
	}{
		{name: "same names in the same order", served: []string{"a", "b"}, expected: []string{"a", "b"}},
		{name: "same names in another order", served: []string{"b", "a"}, expected: []string{"a", "b"}},
		{name: "both empty", served: nil, expected: []string{}},
		{name: "missing tool", served: []string{"a"}, expected: []string{"a", "b"}, wantErr: true},
		{name: "extra tool", served: []string{"a", "b", "c"}, expected: []string{"a", "b"}, wantErr: true},
		{name: "different tool", served: []string{"a", "c"}, expected: []string{"a", "b"}, wantErr: true},
		{name: "duplicate tool", served: []string{"a", "a"}, expected: []string{"a"}, wantErr: true},
		{name: "one served name with a space is not two expected names", served: []string{"a b"}, expected: []string{"a", "b"}, wantErr: true},
		{name: "two served names are not one expected name with a space", served: []string{"a", "b"}, expected: []string{"a b"}, wantErr: true},
		{name: "space moves between names", served: []string{"a b", "c"}, expected: []string{"a", "b c"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			served := slices.Clone(test.served)
			expected := slices.Clone(test.expected)
			err := requireToolCatalog(served, expected)
			if (err != nil) != test.wantErr {
				t.Fatalf("requireToolCatalog(%q, %q) error = %v, wantErr %t", test.served, test.expected, err, test.wantErr)
			}
			if !slices.Equal(served, test.served) || !slices.Equal(expected, test.expected) {
				t.Fatalf("inputs were reordered: served %q, expected %q", served, expected)
			}
		})
	}
}

type blockingCaller struct{}

func (blockingCaller) CallTool(ctx context.Context, _ *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
