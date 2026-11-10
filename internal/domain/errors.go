package domain

import (
	"errors"
	"fmt"
)

var (
	ErrValidation               = errors.New("validation failed")
	ErrIllegalTransition        = errors.New("illegal transition")
	ErrNotFound                 = errors.New("not found")
	ErrAlreadyExists            = errors.New("already exists")
	ErrVersionConflict          = errors.New("version conflict")
	ErrInvalidEvidenceReference = errors.New("invalid evidence reference")
	ErrPolicyDenied             = errors.New("policy denied")
	ErrApprovalMismatch         = errors.New("approval mismatch")
	ErrNonHumanApproval         = errors.New("non-human approval")
	ErrExpired                  = errors.New("expired")
	ErrIdempotencyConflict      = errors.New("idempotency conflict")
	ErrForbidden                = errors.New("forbidden")
	ErrUserDisabled             = errors.New("user disabled")
	ErrNoActiveSession          = errors.New("no active session")
)

// NoActiveSessionError reports that a session token does not resolve to an
// active session of an enabled user. It deliberately does not say which
// condition failed (unknown, expired, revoked or disabled) and carries no
// token material; callers map it to 401.
type NoActiveSessionError struct{}

func (*NoActiveSessionError) Error() string {
	return "no active session"
}
func (*NoActiveSessionError) Unwrap() error {
	return ErrNoActiveSession
}

type ValidationError struct{ Field, Reason string }

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Reason)
}
func (e *ValidationError) Unwrap() error {
	return ErrValidation
}

func invalid(field, reason string) error {
	return &ValidationError{Field: field, Reason: reason}
}

type IllegalTransitionError struct{ From, To Status }

func (e *IllegalTransitionError) Error() string {
	return fmt.Sprintf("cannot transition incident from %s to %s", e.From, e.To)
}
func (e *IllegalTransitionError) Unwrap() error {
	return ErrIllegalTransition
}

type NotFoundError struct{ Resource, ID string }

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("%s %q not found", e.Resource, e.ID)
}
func (e *NotFoundError) Unwrap() error {
	return ErrNotFound
}

// AlreadyExistsError reports an insert of a record that is already stored
// under the same identity. It is a conflict with existing state, not a
// malformed request, so it is deliberately not a ValidationError.
type AlreadyExistsError struct{ Resource string }

func (e *AlreadyExistsError) Error() string {
	return e.Resource + " already exists"
}
func (e *AlreadyExistsError) Unwrap() error {
	return ErrAlreadyExists
}

type VersionConflictError struct{ Expected, Actual uint64 }

func (e *VersionConflictError) Error() string {
	return fmt.Sprintf("expected version %d, got %d", e.Expected, e.Actual)
}
func (e *VersionConflictError) Unwrap() error {
	return ErrVersionConflict
}

type EvidenceReferenceError struct{ EvidenceID EvidenceID }

func (e *EvidenceReferenceError) Error() string {
	return fmt.Sprintf("evidence %q does not belong to incident", e.EvidenceID)
}
func (e *EvidenceReferenceError) Unwrap() error {
	return ErrInvalidEvidenceReference
}

type PolicyDenialError struct{ Rule string }

func (e *PolicyDenialError) Error() string {
	return fmt.Sprintf("policy rule %q denied rollback", e.Rule)
}
func (e *PolicyDenialError) Unwrap() error {
	return ErrPolicyDenied
}

type ApprovalMismatchError struct{ Reason string }

func (e *ApprovalMismatchError) Error() string {
	return "approval mismatch: " + e.Reason
}
func (e *ApprovalMismatchError) Unwrap() error {
	return ErrApprovalMismatch
}

type NonHumanApprovalError struct{ Kind ActorKind }

func (e *NonHumanApprovalError) Error() string {
	return fmt.Sprintf("actor kind %q cannot decide approvals", e.Kind.String())
}
func (e *NonHumanApprovalError) Unwrap() error {
	return ErrNonHumanApproval
}

type ExpiryError struct{ Resource string }

func (e *ExpiryError) Error() string {
	return e.Resource + " expired"
}
func (e *ExpiryError) Unwrap() error {
	return ErrExpired
}

type IdempotencyConflictError struct{ Key IdempotencyKey }

func (e *IdempotencyConflictError) Error() string {
	return fmt.Sprintf("idempotency key %q was reused", e.Key)
}
func (e *IdempotencyConflictError) Unwrap() error {
	return ErrIdempotencyConflict
}

// ForbiddenError reports that a role may not perform an action under
// RolePolicy. It carries only the two enumeration values, never identity or
// credential material.
type ForbiddenError struct {
	Role   Role
	Action Action
}

func (e *ForbiddenError) Error() string {
	return fmt.Sprintf("role %q may not perform %q", e.Role.String(), e.Action.String())
}
func (e *ForbiddenError) Unwrap() error {
	return ErrForbidden
}

// UserDisabledError reports that a disabled user cannot act. It names only
// the internal user ID.
type UserDisabledError struct{ UserID UserID }

func (e *UserDisabledError) Error() string {
	return fmt.Sprintf("user %q is disabled", e.UserID)
}
func (e *UserDisabledError) Unwrap() error {
	return ErrUserDisabled
}
