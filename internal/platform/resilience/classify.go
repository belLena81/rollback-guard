// Package resilience provides retry and circuit-breaker decorators for the
// ports that call external systems. It classifies errors once, here, so every
// decorator agrees on what "transient" means.
package resilience

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

// Dependency names an external system. The values are a closed set used as
// metric labels; never build one from request data.
type Dependency string

// The external dependencies a process can guard.
const (
	DependencyKubernetesReadonlyMCP Dependency = "kubernetes_readonly_mcp"
	DependencyKubernetesRollbackMCP Dependency = "kubernetes_rollback_mcp"
	DependencyObservabilityMCP      Dependency = "observability_mcp"
	DependencyA2AInvestigator       Dependency = "a2a_investigator"
	DependencyLLM                   Dependency = "llm"
	DependencyPrometheus            Dependency = "prometheus"
	DependencyKubernetesAPI         Dependency = "kubernetes_api"
)

// ErrCircuitOpen is matched (errors.Is) by every CircuitOpenError.
var ErrCircuitOpen = errors.New("circuit breaker is open")

// CircuitOpenError is returned without calling the dependency while its
// breaker is open. The request was never sent.
type CircuitOpenError struct {
	Dependency Dependency
}

func (e *CircuitOpenError) Error() string {
	return fmt.Sprintf("circuit breaker for %s is open", e.Dependency)
}

func (e *CircuitOpenError) Is(target error) bool { return target == ErrCircuitOpen }

// transient is implemented by errors that know they are worth retrying. The
// adapters that own an SDK error type use it (or MarkTransient) so this
// package never imports an SDK.
type transient interface{ Transient() bool }

type transientError struct{ err error }

func (e transientError) Error() string   { return e.err.Error() }
func (e transientError) Unwrap() error   { return e.err }
func (e transientError) Transient() bool { return true }

// MarkTransient wraps err so IsTransient reports true for it. A nil error
// stays nil.
func MarkTransient(err error) error {
	if err == nil {
		return nil
	}
	return transientError{err: err}
}

// permanent lists the outcomes that retrying can never change: the dependency
// answered, or the outcome is deliberately ambiguous.
var permanent = []error{
	domain.ErrValidation, domain.ErrNotFound, domain.ErrVersionConflict, domain.ErrPolicyDenied,
	domain.ErrApprovalMismatch, domain.ErrNonHumanApproval, domain.ErrExpired, domain.ErrIdempotencyConflict,
	domain.ErrIllegalTransition, domain.ErrInvalidEvidenceReference, domain.ErrAlreadyExists,
	ports.ErrReasonerRefused, ports.ErrReasonerInvalidResponse, ports.ErrRollbackOutcomeUnknown,
	context.Canceled, ErrCircuitOpen,
}

// IsTransient reports whether err is a timeout, connection failure, or
// unavailable-dependency error that a later attempt could succeed on.
// Validation, policy, not-found, conflict, permission, and unclassified errors
// are not transient. The caller decides separately whether its own context
// has ended.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	for _, p := range permanent {
		if errors.Is(err, p) {
			return false
		}
	}
	var flagged transient
	if errors.As(err, &flagged) {
		return flagged.Transient()
	}
	var opErr *net.OpError
	switch {
	case errors.Is(err, ports.ErrReasonerUnavailable),
		errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, io.EOF),
		errors.Is(err, io.ErrUnexpectedEOF),
		errors.Is(err, net.ErrClosed),
		errors.As(err, &opErr):
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// RequestNotSent reports whether err proves the request never reached the
// dependency: the TCP connection could not be established, or a breaker
// rejected the call locally. It is false for a timeout or a failure after the
// connection was made, and always false for ErrRollbackOutcomeUnknown.
func RequestNotSent(err error) bool {
	if err == nil || errors.Is(err, ports.ErrRollbackOutcomeUnknown) {
		return false
	}
	if errors.Is(err, ErrCircuitOpen) {
		return true
	}
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}
