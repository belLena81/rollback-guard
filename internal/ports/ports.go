package ports

import (
	"context"
	"errors"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
)

var ErrRollbackOutcomeUnknown = errors.New("rollback outcome is unknown")

var (
	ErrReasonerUnavailable     = errors.New("reasoner is unavailable")
	ErrReasonerRefused         = errors.New("reasoner refused the request")
	ErrReasonerInvalidResponse = errors.New("reasoner returned an invalid response")
)

// Identity provider failures (docs/phase7-auth.md, "GitHub login"). An
// IdentityProvider wraps exactly one of them and never the provider's
// response body, token or authorization code.
var (
	// ErrAuthorizationCodeRejected: the provider refused the authorization
	// code or PKCE verifier (wrong, expired or already used). The client
	// must start the login again.
	ErrAuthorizationCodeRejected = errors.New("authorization code rejected")
	// ErrIdentityProviderFailed: the provider was unreachable, timed out or
	// answered with an unusable response.
	ErrIdentityProviderFailed = errors.New("identity provider failed")
)

// GitHubIdentity is what an IdentityProvider learned about the signed-in
// GitHub user. It is untrusted input until the login service validates it.
type GitHubIdentity struct {
	GitHubID      int64
	Login         string
	Email         string
	Organizations []string
}

// IdentityProvider runs the provider side of the OAuth authorization-code
// flow with PKCE (S256). Exchange redeems the code, reads the user and their
// organizations, and discards the provider access token before returning.
type IdentityProvider interface {
	AuthorizationURL(state, codeChallenge string) string
	Exchange(ctx context.Context, code, codeVerifier string) (GitHubIdentity, error)
}

type DeploymentInspector interface {
	InspectDeployment(context.Context, domain.DeploymentTarget) (domain.DeploymentEvidence, error)
}
type MetricsPort interface {
	Observe(context.Context, domain.DeploymentTarget, domain.Symptom) (domain.MetricEvidence, error)
}
type InvestigationInput struct {
	Incident   domain.Incident
	Deployment domain.DeploymentEvidence
	Metric     domain.MetricEvidence
	Evidence   []domain.Evidence
	Now        time.Time
}
type Diagnoser interface {
	Diagnose(context.Context, InvestigationInput) (domain.InvestigationConclusion, error)
}

type ReasoningEvidence struct {
	ID              domain.EvidenceID
	Kind            domain.EvidenceKind
	CollectedAt     time.Time
	Deployment      *domain.DeploymentEvidence
	Metric          *domain.MetricEvidence
	Pod             *domain.PodEvidence
	KubernetesEvent *ReasoningKubernetesEvent
}

type ReasoningKubernetesEvent struct {
	Reason     string
	ObservedAt time.Time
}

type ReasoningCandidate struct {
	Hypothesis     string
	Confidence     float64
	EvidenceIDs    []domain.EvidenceID
	Recommendation string
}

type ReasoningRequest struct {
	PromptVersion string
	IncidentID    domain.IncidentID
	Target        domain.DeploymentTarget
	Symptom       domain.Symptom
	Evidence      []ReasoningEvidence
	RuleCandidate *ReasoningCandidate
}

type TokenUsage struct {
	InputTokens  int64
	OutputTokens int64
}

type ReasoningResult struct {
	Candidate ReasoningCandidate
	Model     string
	Usage     TokenUsage
}

type Reasoner interface {
	Reason(context.Context, ReasoningRequest) (ReasoningResult, error)
}
type InvestigationRequest struct {
	Incident       domain.Incident
	OperationID    string
	IdempotencyKey string
	RequestHash    string
	TaskID         string
	TraceID        string
	Progress       func(domain.InvestigationStage) error
}
type InvestigationResult struct {
	Deployment domain.DeploymentEvidence
	Evidence   []domain.Evidence
	Conclusion domain.InvestigationConclusion
}
type Investigator interface {
	Investigate(context.Context, InvestigationRequest) (InvestigationResult, error)
}

type InvestigationSubmission struct {
	Incident       domain.Incident
	OperationID    string
	IdempotencyKey string
	MessageID      string
	TraceID        string
	RequestHash    string
}

type InvestigationGateway interface {
	Submit(context.Context, InvestigationSubmission) (domain.InvestigationTaskRef, error)
	Watch(context.Context, domain.InvestigationTaskRef, func(domain.InvestigationStage) error) (InvestigationResult, error)
	Cancel(context.Context, domain.InvestigationTaskRef) error
}
type RollbackExecutor interface {
	// ExecuteApprovedRollback must atomically validate the target identity and
	// current revision immediately before applying the approved mutation.
	// ExecuteApprovedRollback must return the same terminal result without
	// repeating the mutation when called again with the same idempotency key.
	ExecuteApprovedRollback(context.Context, domain.ApprovedRollback, domain.IdempotencyKey) (domain.RollbackExecution, error)
}

// RollbackMutationLease is the ownership token of one acquisition of the
// per-key rollback mutation lock. Begin issues a fresh, unguessable lease for
// every pending attempt; only an attempt carrying that lease can complete or
// release the lock it acquired. A lease is opaque: callers compare and pass it,
// never build one.
type RollbackMutationLease string

// ErrRollbackMutationLeaseNotHeld is returned by Complete when the attempt's
// lease no longer holds the mutation lock. Nothing is written; a later Begin
// for the same key reconciles the pending ledger entry.
var ErrRollbackMutationLeaseNotHeld = errors.New("rollback mutation lease is not held")

type RollbackMutationAttempt struct {
	Mutation domain.RollbackMutation
	Approved domain.ApprovedRollback
	// Lease is set when Mutation is pending and this attempt holds the lock.
	// It is empty for an already completed mutation, which holds no lock.
	Lease RollbackMutationLease
	// ReconcileOnly is set when a pending mutation is resumed but its
	// approver is no longer authorized: an earlier attempt may already have
	// patched, so the holder may only record a deployment already at the
	// target revision (Complete with alreadyAtTarget) and must otherwise
	// Release and refuse with domain.ApproverNotAuthorizedError. It never
	// applies a mutation.
	ReconcileOnly bool
}
type RollbackMutationGuard interface {
	Begin(context.Context, domain.IdempotencyKey, domain.ProposalHash, time.Time) (RollbackMutationAttempt, error)
	// Complete records the attempt's pending mutation as completed and
	// releases the attempt's lock. It returns ErrRollbackMutationLeaseNotHeld,
	// without writing, when the attempt's lease does not hold the lock. It
	// returns either the completed mutation or an error, never both.
	Complete(context.Context, RollbackMutationAttempt, bool, time.Time) (domain.RollbackMutation, error)
	// Release gives up the lock held by a pending attempt returned by Begin
	// that will not be completed, so a later Begin for the same key can
	// proceed. It releases only the lock acquired by this attempt's lease: a
	// late Release from one attempt never unlocks another attempt for the
	// same key. Releasing an attempt that holds no lock is not an error.
	Release(context.Context, RollbackMutationAttempt) error
}
type RollbackPolicy interface {
	Evaluate(domain.EligibilityCandidate) (domain.PolicyDecision, error)
}
type ApprovalPolicy interface {
	Authorize(domain.RollbackProposal, domain.PolicyDecision, domain.Approval, domain.DeploymentEvidence, time.Time) (domain.ApprovedRollback, error)
}
type Clock interface{ Now() time.Time }
type IDGenerator interface{ NewID(prefix string) string }

// RandomSource fills b entirely with cryptographically secure random bytes,
// or returns an error and the caller must not use b.
type RandomSource interface{ Fill(b []byte) error }

type IncidentRepository interface {
	Create(domain.Incident) error
	Get(domain.IncidentID) (domain.Incident, error)
	Update(domain.Incident, uint64) error
}
type EvidenceRepository interface {
	Add(domain.Evidence) error
	List(domain.IncidentID) ([]domain.Evidence, error)
}
type ConclusionRepository interface {
	Save(domain.InvestigationConclusion) error
	Get(domain.IncidentID) (domain.InvestigationConclusion, error)
}
type PolicyDecisionRepository interface {
	Save(domain.PolicyDecision) error
	Get(domain.PolicyDecisionID) (domain.PolicyDecision, error)
}
type ProposalRepository interface {
	Save(domain.RollbackProposal) error
	GetByIncident(domain.IncidentID) (domain.RollbackProposal, error)
}
type ApprovalRepository interface {
	Save(domain.Approval) error
	GetByIncident(domain.IncidentID) (domain.Approval, error)
}
type ExecutionRepository interface {
	Save(domain.RollbackExecution) error
	Get(domain.ExecutionID) (domain.RollbackExecution, error)
	GetByIncident(domain.IncidentID) (domain.RollbackExecution, error)
}
type VerificationRepository interface {
	Save(domain.RecoveryVerification) error
	GetByIncident(domain.IncidentID) (domain.RecoveryVerification, error)
}
type IdempotencyRepository interface {
	Get(domain.IdempotencyKey) (domain.IdempotencyRecord, error)
	Create(domain.IdempotencyRecord) error
	Update(domain.IdempotencyRecord, domain.IdempotencyState) error
}
type InvestigationTaskRepository interface {
	Create(domain.InvestigationDispatch) error
	GetByIncident(domain.IncidentID) (domain.InvestigationDispatch, error)
	ClaimNext(time.Time, string, time.Time) (domain.InvestigationDispatch, error)
	Update(domain.InvestigationDispatch, uint64) error
}
type EventRepository interface {
	NextSequence(domain.IncidentID) (uint64, error)
	// Append returns a VersionConflictError when the sequence is already
	// taken. The caller must return that error from its unit of work so the
	// state change it was auditing is rolled back with it.
	Append(domain.OperationEvent) error
	List(domain.IncidentID) ([]domain.OperationEvent, error)
}

// Repositories is the set of repositories bound to one transaction. Methods
// take no context.Context by design: the context passed to
// UnitOfWork.WithinTransaction scopes the whole transaction, and adapters
// carry it, so cancelling it aborts every statement and prevents the commit.
// See docs/adr-repository-context.md.
type Repositories interface {
	Incidents() IncidentRepository
	Evidence() EvidenceRepository
	Conclusions() ConclusionRepository
	PolicyDecisions() PolicyDecisionRepository
	Proposals() ProposalRepository
	Approvals() ApprovalRepository
	Executions() ExecutionRepository
	Verifications() VerificationRepository
	Idempotency() IdempotencyRepository
	InvestigationTasks() InvestigationTaskRepository
	Events() EventRepository
}

// UnitOfWork runs fn in one atomic transaction. ctx governs the transaction's
// lifetime and every repository call made through the Repositories passed to
// fn; fn must not use those repositories after it returns.
type UnitOfWork interface {
	WithinTransaction(context.Context, func(Repositories) error) error
}

// UserRepository stores users. Create returns *domain.AlreadyExistsError when
// the ID, GitHub ID or login (case-insensitively) is taken; Update does too
// when the new login collides. Get* return *domain.NotFoundError for a missing
// user. Update writes only when the stored version equals expectedVersion and
// returns *domain.VersionConflictError otherwise; user must be one version
// past expectedVersion, and its GitHub ID and creation time must match the
// stored user, or it returns *domain.ValidationError.
type UserRepository interface {
	UserReader
	Create(domain.User) error
	Update(user domain.User, expectedVersion uint64) error
	// LockActiveAdministrators returns the IDs of enabled administrators and
	// locks them until the transaction ends, so two concurrent demotions
	// cannot both observe the other administrator and leave none.
	LockActiveAdministrators() ([]domain.UserID, error)
}

// UserReader is the read side of UserRepository.
type UserReader interface {
	GetByID(domain.UserID) (domain.User, error)
	GetByGitHubID(int64) (domain.User, error)
}

// AuthAuditReader is the read side of AuthAuditRepository.
type AuthAuditReader interface {
	// ListRecent returns at most limit events, newest first.
	ListRecent(limit int) ([]domain.AuthAuditEvent, error)
	// ListPage returns at most limit events, newest first, that are strictly
	// older than after in the order (occurred_at, id). The zero cursor starts
	// at the newest event.
	ListPage(after AuthAuditCursor, limit int) ([]domain.AuthAuditEvent, error)
}

// AuthAuditCursor is the position of an audit event in the newest-first
// order. The zero value means "before everything".
type AuthAuditCursor struct {
	OccurredAt time.Time
	ID         domain.AuthAuditEventID
}

// IsZero reports whether c starts a listing at the newest event.
func (c AuthAuditCursor) IsZero() bool { return c.ID == "" && c.OccurredAt.IsZero() }

// AuthAuditRepository is append-only: there is no update or delete.
type AuthAuditRepository interface {
	AuthAuditReader
	// Append returns *domain.AlreadyExistsError for a duplicate ID and
	// *domain.ValidationError for an event no constructor built.
	Append(domain.AuthAuditEvent) error
}

// SessionReader is the read side of SessionRepository.
type SessionReader interface {
	// ResolveActive returns the session holding tokenHash together with its
	// user, in one lookup, only when the session is unrevoked, now is before
	// its expiry, and the user is enabled. Every other case, including an
	// unknown hash, returns *domain.NoActiveSessionError without saying which.
	ResolveActive(tokenHash domain.SessionTokenHash, now time.Time) (domain.Session, domain.User, error)
}

// SessionRepository stores sessions by token hash; it never sees a raw token.
type SessionRepository interface {
	SessionReader
	// Create returns *domain.AlreadyExistsError when the ID or token hash is
	// taken, *domain.NotFoundError when the user does not exist, and
	// *domain.UserDisabledError when the user is disabled. It serializes
	// with a concurrent disable of the same user.
	Create(domain.Session) error
	// Revoke marks the session revoked at now (never before its creation).
	// Revoking a revoked session keeps the first time and is not an error.
	// A missing session returns *domain.NotFoundError.
	Revoke(id domain.SessionID, now time.Time) error
	// RevokeAllForUser revokes every unrevoked session of the user and
	// returns how many it revoked. An unknown user revokes nothing.
	RevokeAllForUser(userID domain.UserID, now time.Time) (int, error)
	// DeleteExpired deletes sessions whose expiry is at or before before and
	// returns how many it deleted.
	DeleteExpired(before time.Time) (int, error)
}

// AuthRepositories is the set of user and session repositories bound to one
// transaction. As with Repositories, methods take no context.Context: the
// context passed to AuthUnitOfWork.WithinAuthTransaction scopes them.
type AuthRepositories interface {
	Users() UserRepository
	Sessions() SessionRepository
	Audit() AuthAuditRepository
}

// AuthUnitOfWork runs fn in one atomic transaction over users and sessions,
// so a role change or disable commits together with session revocation.
// fn must not use the repositories after it returns.
type AuthUnitOfWork interface {
	WithinAuthTransaction(context.Context, func(AuthRepositories) error) error
	// WithinAuthReadTransaction runs fn in a read-only transaction over the
	// read side of the repositories (session resolution, live role lookups,
	// audit reads). The same context rules apply as for
	// WithinAuthTransaction. The Postgres adapter also sets the transaction
	// access mode to read only, so a write fails even if it is reached by
	// other means.
	WithinAuthReadTransaction(context.Context, func(AuthReadRepositories) error) error
}

// AuthReadRepositories is the read-only view of AuthRepositories.
type AuthReadRepositories interface {
	Users() UserReader
	Sessions() SessionReader
	Audit() AuthAuditReader
}
