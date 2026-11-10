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

type InvestigationTaskSnapshot struct {
	Ref         domain.InvestigationTaskRef
	State       domain.InvestigationTaskState
	Stage       domain.InvestigationStage
	Result      *InvestigationResult
	FailureCode string
}

type InvestigationGateway interface {
	Submit(context.Context, InvestigationSubmission) (domain.InvestigationTaskRef, error)
	Watch(context.Context, domain.InvestigationTaskRef, func(domain.InvestigationStage) error) (InvestigationResult, error)
	Get(context.Context, domain.InvestigationTaskRef) (InvestigationTaskSnapshot, error)
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
}
type RollbackMutationGuard interface {
	Begin(context.Context, domain.IdempotencyKey, domain.ProposalHash, time.Time) (RollbackMutationAttempt, error)
	// Complete records the attempt's pending mutation as completed and
	// releases the attempt's lock. It returns ErrRollbackMutationLeaseNotHeld,
	// without writing, when the attempt's lease does not hold the lock.
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
	NextSequence(context.Context, domain.IncidentID) (uint64, error)
	// Append returns a VersionConflictError when the sequence is already
	// taken. The caller must return that error from its unit of work so the
	// state change it was auditing is rolled back with it.
	Append(domain.OperationEvent) error
	List(domain.IncidentID) ([]domain.OperationEvent, error)
}
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
type UnitOfWork interface {
	WithinTransaction(context.Context, func(Repositories) error) error
}
