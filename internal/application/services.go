package application

import (
	"fmt"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

type Dependencies struct {
	UnitOfWork     ports.UnitOfWork
	Inspector      ports.DeploymentInspector
	Metrics        ports.MetricsPort
	Diagnoser      ports.Diagnoser
	Executor       ports.RollbackExecutor
	RollbackPolicy ports.RollbackPolicy
	ApprovalPolicy ports.ApprovalPolicy
	Clock          ports.Clock
	IDGenerator    ports.IDGenerator
	// Tracer is optional; nil disables tracing.
	Tracer ports.Tracer
}

type WorkflowConfig struct {
	ProposalTTL    time.Duration
	ApprovalTTL    time.Duration
	CleanupTimeout time.Duration
	SystemActor    domain.Actor
}

type workflow struct {
	uow            ports.UnitOfWork
	inspector      ports.DeploymentInspector
	metrics        ports.MetricsPort
	investigator   ports.Investigator
	executor       ports.RollbackExecutor
	rollbackPolicy ports.RollbackPolicy
	approvalPolicy ports.ApprovalPolicy
	clock          ports.Clock
	ids            ports.IDGenerator
	tracer         ports.Tracer
	config         WorkflowConfig
	systemActor    domain.Actor
}

func (c WorkflowConfig) validate() error {
	if c.ProposalTTL <= 0 || c.ApprovalTTL <= 0 || c.CleanupTimeout <= 0 {
		return &domain.ValidationError{Field: "workflow_config", Reason: "durations must be positive"}
	}
	if !c.SystemActor.Valid() || c.SystemActor.Kind() != domain.ActorSystem {
		return &domain.ValidationError{Field: "workflow_config.system_actor", Reason: "must be a system actor"}
	}
	return nil
}

type Services struct {
	Incidents      *IncidentService
	Investigations *InvestigationService
	Approvals      *ApprovalService
	Executions     *ExecutionService
	Verifications  *VerificationService
	Queries        *QueryService
}

type IncidentService struct{ workflow *workflow }
type InvestigationService struct{ workflow *workflow }
type ApprovalService struct{ workflow *workflow }
type ExecutionService struct{ workflow *workflow }
type VerificationService struct{ workflow *workflow }
type QueryService struct{ workflow *workflow }

func NewServices(dependencies Dependencies, config WorkflowConfig) (Services, error) {
	if dependencies.UnitOfWork == nil || dependencies.Inspector == nil || dependencies.Metrics == nil || dependencies.Diagnoser == nil || dependencies.Executor == nil || dependencies.RollbackPolicy == nil || dependencies.ApprovalPolicy == nil || dependencies.Clock == nil || dependencies.IDGenerator == nil {
		return Services{}, fmt.Errorf("all service dependencies are required")
	}
	if err := config.validate(); err != nil {
		return Services{}, fmt.Errorf("validate workflow config: %w", err)
	}
	investigator, err := NewInvestigationRunner(dependencies.Inspector, dependencies.Metrics, dependencies.Diagnoser, dependencies.Clock, dependencies.IDGenerator)
	if err != nil {
		return Services{}, err
	}
	tracer := dependencies.Tracer
	if tracer == nil {
		tracer = ports.NoopTracer{}
	}
	w := &workflow{
		tracer: tracer,
		uow:    dependencies.UnitOfWork, inspector: dependencies.Inspector, metrics: dependencies.Metrics,
		investigator: investigator, executor: dependencies.Executor, rollbackPolicy: dependencies.RollbackPolicy,
		approvalPolicy: dependencies.ApprovalPolicy, clock: dependencies.Clock, ids: dependencies.IDGenerator,
		config: config, systemActor: config.SystemActor,
	}
	return Services{
		Incidents: &IncidentService{workflow: w}, Investigations: &InvestigationService{workflow: w},
		Approvals: &ApprovalService{workflow: w}, Executions: &ExecutionService{workflow: w},
		Verifications: &VerificationService{workflow: w}, Queries: &QueryService{workflow: w},
	}, nil
}
