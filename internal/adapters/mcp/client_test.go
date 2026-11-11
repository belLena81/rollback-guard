package mcp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/resilience"
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

func TestErrorFromToolKeepsUnknownOutcomesAmbiguous(t *testing.T) {
	tests := []struct {
		code          ErrorCode
		retryable     bool
		wantAmbiguous bool
	}{
		{code: CodeInternal, wantAmbiguous: true},
		{code: CodeDependencyUnavailable, wantAmbiguous: true},
		{code: ErrorCode("code_from_a_newer_server"), retryable: true, wantAmbiguous: true},
		{code: ErrorCode("code_from_a_newer_server"), wantAmbiguous: false},
		{code: CodeRevisionDrift, wantAmbiguous: false},
		{code: CodeIdempotencyConflict, wantAmbiguous: false},
	}
	for _, test := range tests {
		err := errorFromTool(ToolError{Code: test.code, Message: "tool failed", Retryable: test.retryable})
		if err == nil {
			t.Fatalf("errorFromTool(%s) = nil", test.code)
		}
		if got := ambiguousToolError(err); got != test.wantAmbiguous {
			t.Errorf("ambiguousToolError(errorFromTool(%s, retryable=%t)) = %t, want %t", test.code, test.retryable, got, test.wantAmbiguous)
		}
	}
}

// Only the server's dependency_unavailable code is worth retrying. Internal
// and unknown codes may already have had an effect, and the rest are answers.
func TestToolErrorsAreTransientOnlyForUnavailableDependencies(t *testing.T) {
	tests := []struct {
		code ErrorCode
		want bool
	}{
		{CodeDependencyUnavailable, true},
		{CodeTimeout, true},
		{CodeInternal, false},
		{ErrorCode("code_from_a_newer_server"), false},
		{CodeInvalidArgument, false},
		{CodeTargetForbidden, false},
		{CodeNotFound, false},
		{CodeRevisionDrift, false},
		{CodePolicyDenied, false},
		{CodeApprovalMismatch, false},
		{CodeApproverNotAuthorized, false},
		{CodeExpired, false},
		{CodeIdempotencyConflict, false},
		{CodeCanceled, false},
	}
	for _, test := range tests {
		err := fmt.Errorf("call: %w", errorFromTool(ToolError{Code: test.code, Message: "m", Retryable: true}))
		if got := resilience.IsTransient(err); got != test.want {
			t.Errorf("IsTransient(%s) = %t, want %t", test.code, got, test.want)
		}
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

func TestCatalogsAreDisjointAndReturnFreshSlices(t *testing.T) {
	t.Parallel()

	catalogs := map[string]func() []string{
		"kubernetes readonly": KubernetesReadonlyCatalog,
		"kubernetes rollback": KubernetesRollbackCatalog,
		"observability":       ObservabilityCatalog,
	}
	owner := map[string]string{}
	for name, catalog := range catalogs {
		first := catalog()
		if len(first) == 0 {
			t.Fatalf("%s catalog is empty", name)
		}
		for _, tool := range first {
			if previous, taken := owner[tool]; taken {
				t.Fatalf("tool %s is in both the %s and %s catalogs", tool, previous, name)
			}
			owner[tool] = name
		}
		want := slices.Clone(first)
		first[0] = "generic_shell"
		if again := catalog(); !slices.Equal(again, want) {
			t.Fatalf("%s catalog = %q after a caller modified an earlier result, want %q", name, again, want)
		}
	}
	// The mutating tool belongs to the rollback server only.
	if owner[ToolExecuteApprovedRollback] != "kubernetes rollback" {
		t.Fatalf("%s is in the %q catalog", ToolExecuteApprovedRollback, owner[ToolExecuteApprovedRollback])
	}
}

type blockingCaller struct{}

func (blockingCaller) CallTool(ctx context.Context, _ *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
