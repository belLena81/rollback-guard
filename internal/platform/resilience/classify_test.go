package resilience_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/resilience"
	"github.com/belLena81/rollback-guard/internal/ports"
)

type transientFlag bool

func (t transientFlag) Error() string   { return "flagged" }
func (t transientFlag) Transient() bool { return bool(t) }

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestIsTransient(t *testing.T) {
	dialErr := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"deadline exceeded", context.DeadlineExceeded, true},
		{"wrapped deadline", fmt.Errorf("call: %w", context.DeadlineExceeded), true},
		{"canceled", context.Canceled, false},
		{"connection refused", dialErr, true},
		{"connection reset", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"EOF", io.EOF, true},
		{"flagged transient", fmt.Errorf("x: %w", transientFlag(true)), true},
		{"flagged permanent", transientFlag(false), false},
		{"reasoner unavailable", ports.ErrReasonerUnavailable, true},
		{"reasoner refused", ports.ErrReasonerRefused, false},
		{"reasoner invalid", ports.ErrReasonerInvalidResponse, false},
		{"validation", &domain.ValidationError{Field: "f", Reason: "r"}, false},
		{"not found", &domain.NotFoundError{Resource: "r", ID: "i"}, false},
		{"policy denied", &domain.PolicyDenialError{Rule: "r"}, false},
		{"approval mismatch", &domain.ApprovalMismatchError{Reason: "r"}, false},
		{"expired", &domain.ExpiryError{Resource: "r"}, false},
		{"idempotency conflict", &domain.IdempotencyConflictError{}, false},
		{"version conflict", &domain.VersionConflictError{}, false},
		{"outcome unknown beats transport error", fmt.Errorf("%w: %w", ports.ErrRollbackOutcomeUnknown, dialErr), false},
		{"validation beats timeout", errors.Join(&domain.ValidationError{Field: "f", Reason: "r"}, context.DeadlineExceeded), false},
		{"open breaker is not retried", &resilience.CircuitOpenError{Dependency: resilience.DependencyPrometheus}, false},
		{"unknown error", errors.New("boom"), false},
		{"closed connection", net.ErrClosed, true},
		{"dns failure", &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", Name: "x"}}, true},
		{"non-op net timeout", timeoutErr{}, true},
		{"canceled joined with transient cause", errors.Join(context.Canceled, context.DeadlineExceeded), false},
		{"non-human approval", domain.ErrNonHumanApproval, false},
		{"illegal transition", domain.ErrIllegalTransition, false},
		{"already exists", domain.ErrAlreadyExists, false},
		{"invalid evidence reference", domain.ErrInvalidEvidenceReference, false},
		{"permanent flag beats wrapped deadline", fmt.Errorf("x: %w", errors.Join(transientFlag(false), context.DeadlineExceeded)), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resilience.IsTransient(tt.err); got != tt.want {
				t.Fatalf("IsTransient(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestRequestNotSent(t *testing.T) {
	dial := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"dial failure", fmt.Errorf("call tool: %w", dial), true},
		{"open breaker", &resilience.CircuitOpenError{Dependency: resilience.DependencyKubernetesRollbackMCP}, true},
		{"read failure after connect", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, false},
		{"timeout", context.DeadlineExceeded, false},
		{"unavailable tool error", transientFlag(true), false},
		{"dial failure marked outcome unknown", fmt.Errorf("%w: %w", ports.ErrRollbackOutcomeUnknown, dial), false},
		{"nil", nil, false},
		{"dns failure while dialing", &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", Name: "x"}}, true},
		{"dial joined with deadline", errors.Join(dial, context.DeadlineExceeded), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resilience.RequestNotSent(tt.err); got != tt.want {
				t.Fatalf("RequestNotSent(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestMarkTransientSurvivesWrapping(t *testing.T) {
	err := fmt.Errorf("outer: %w", resilience.MarkTransient(errors.New("upstream 503")))
	if !resilience.IsTransient(err) {
		t.Fatal("marked error must be transient")
	}
	if resilience.MarkTransient(nil) != nil {
		t.Fatal("MarkTransient(nil) must be nil")
	}
}
