package mcp

import (
	"context"
	"testing"

	"github.com/belLena81/rollback-guard/internal/adapters/kubernetes"
	metricsprom "github.com/belLena81/rollback-guard/internal/adapters/prometheus"
	"github.com/belLena81/rollback-guard/internal/domain"
)

func TestErrorMappingIsStableAndDoesNotLeakDetails(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		code      ErrorCode
		retryable bool
	}{
		{name: "validation", err: &domain.ValidationError{Field: "secret", Reason: "raw value"}, code: CodeInvalidArgument},
		{name: "forbidden", err: kubernetes.ErrTargetForbidden, code: CodeTargetForbidden},
		{name: "drift", err: kubernetes.ErrRevisionDrift, code: CodeRevisionDrift},
		{name: "expired", err: &domain.ExpiryError{Resource: "approval"}, code: CodeExpired},
		{name: "timeout", err: context.DeadlineExceeded, code: CodeTimeout, retryable: true},
		{name: "bad dependency result", err: metricsprom.ErrInvalidResult, code: CodeDependencyUnavailable, retryable: true},
		{name: "unknown", err: &privateError{}, code: CodeInternal},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := classifyError(test.err)
			if got.Code != test.code || got.Retryable != test.retryable {
				t.Fatalf("classifyError() = %#v", got)
			}
			if got.Message == test.err.Error() {
				t.Fatalf("error leaked internal detail %q", got.Message)
			}
		})
	}
}

type privateError struct{}

func (*privateError) Error() string { return "credential token: do-not-leak" }
