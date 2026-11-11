package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/belLena81/rollback-guard/internal/adapters/kubernetes"
	metricsprom "github.com/belLena81/rollback-guard/internal/adapters/prometheus"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/resilience"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// Handle adapts a typed tool body to an SDK handler. Every error the body
// returns is classified once by Failure into the tool's error envelope; the
// SDK-level error is always nil so callers see a structured tool result.
func Handle[In, Out any](body func(context.Context, In) (Out, error)) mcpsdk.ToolHandlerFor[In, Response[Out]] {
	return func(ctx context.Context, _ *mcpsdk.CallToolRequest, input In) (*mcpsdk.CallToolResult, Response[Out], error) {
		output, err := body(ctx, input)
		if err != nil {
			result, response := Failure[Out](err)
			return result, response, nil
		}
		return nil, Success(output), nil
	}
}

func Failure[T any](err error) (*mcpsdk.CallToolResult, Response[T]) {
	toolError := classifyError(err)
	result := &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: toolError.Message}}}
	result.SetError(fmt.Errorf("%s", toolError.Code))
	return result, Response[T]{OK: false, Error: &toolError}
}

func classifyError(err error) ToolError {
	switch {
	case errors.Is(err, kubernetes.ErrMutationNotRecorded):
		// The rollback is applied; only the ledger write failed. Never a
		// rejection: the client must reconcile.
		return ToolError{Code: CodeInternal, Message: "rollback applied but not recorded"}
	case errors.Is(err, context.Canceled):
		return ToolError{Code: CodeCanceled, Message: "operation canceled", Retryable: true}
	case errors.Is(err, context.DeadlineExceeded):
		return ToolError{Code: CodeTimeout, Message: "operation timed out", Retryable: true}
	case errors.Is(err, domain.ErrValidation), errors.Is(err, metricsprom.ErrUnsupportedKind):
		return ToolError{Code: CodeInvalidArgument, Message: "request validation failed"}
	case errors.Is(err, kubernetes.ErrTargetForbidden), errors.Is(err, metricsprom.ErrTargetForbidden):
		return ToolError{Code: CodeTargetForbidden, Message: "target is not allowed"}
	case errors.Is(err, domain.ErrNotFound), apierrors.IsNotFound(err):
		return ToolError{Code: CodeNotFound, Message: "resource not found"}
	case errors.Is(err, kubernetes.ErrRevisionDrift), apierrors.IsConflict(err):
		return ToolError{Code: CodeRevisionDrift, Message: "deployment revision changed"}
	case errors.Is(err, domain.ErrPolicyDenied):
		return ToolError{Code: CodePolicyDenied, Message: "rollback policy denied execution"}
	case errors.Is(err, domain.ErrApproverNotAuthorized):
		return ToolError{Code: CodeApproverNotAuthorized, Message: "approver is not currently authorized"}
	case errors.Is(err, domain.ErrApprovalMismatch), errors.Is(err, domain.ErrNonHumanApproval):
		return ToolError{Code: CodeApprovalMismatch, Message: "approval does not match proposal"}
	case errors.Is(err, domain.ErrExpired):
		return ToolError{Code: CodeExpired, Message: "proposal or approval expired"}
	case errors.Is(err, domain.ErrIdempotencyConflict):
		return ToolError{Code: CodeIdempotencyConflict, Message: "idempotency key conflicts with another request"}
	case apierrors.IsForbidden(err):
		return ToolError{Code: CodeTargetForbidden, Message: "Kubernetes identity is not authorized"}
	case errors.Is(err, metricsprom.ErrInvalidResult), errors.Is(err, resilience.ErrCircuitOpen),
		apierrors.IsServerTimeout(err), apierrors.IsTimeout(err), apierrors.IsTooManyRequests(err), apierrors.IsServiceUnavailable(err):
		return ToolError{Code: CodeDependencyUnavailable, Message: "infrastructure dependency is unavailable", Retryable: true}
	default:
		return ToolError{Code: CodeInternal, Message: "tool operation failed"}
	}
}
