package ports

import (
	"context"
	"errors"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
)

var ErrRollbackOutcomeUnknown = errors.New("rollback outcome is unknown")

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
type RollbackExecutor interface {
	// ExecuteApprovedRollback must atomically validate the target identity and
	// current revision immediately before applying the approved mutation.
	// ExecuteApprovedRollback must return the same terminal result without
	// repeating the mutation when called again with the same idempotency key.
	ExecuteApprovedRollback(context.Context, domain.ApprovedRollback, domain.IdempotencyKey) (domain.RollbackExecution, error)
}
type RollbackMutationAttempt struct {
	Mutation domain.RollbackMutation
	Approved domain.ApprovedRollback
}
type RollbackMutationGuard interface {
	Begin(context.Context, domain.IdempotencyKey, domain.ProposalHash, time.Time) (RollbackMutationAttempt, error)
	Complete(context.Context, domain.RollbackMutation, bool, time.Time) (domain.RollbackMutation, error)
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
type EventRepository interface {
	NextSequence(context.Context, domain.IncidentID) (uint64, error)
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
	Events() EventRepository
}
type UnitOfWork interface {
	WithinTransaction(context.Context, func(Repositories) error) error
}
