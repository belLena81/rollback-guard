package domain

import "fmt"

var (
	ErrValidation               = fmt.Errorf("validation failed")
	ErrIllegalTransition        = fmt.Errorf("illegal transition")
	ErrNotFound                 = fmt.Errorf("not found")
	ErrAlreadyExists            = fmt.Errorf("already exists")
	ErrVersionConflict          = fmt.Errorf("version conflict")
	ErrInvalidEvidenceReference = fmt.Errorf("invalid evidence reference")
	ErrPolicyDenied             = fmt.Errorf("policy denied")
	ErrApprovalMismatch         = fmt.Errorf("approval mismatch")
	ErrNonHumanApproval         = fmt.Errorf("non-human approval")
	ErrExpired                  = fmt.Errorf("expired")
	ErrIdempotencyConflict      = fmt.Errorf("idempotency conflict")
)

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
