package application

import (
	"errors"
	"fmt"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

type Dependencies struct {
	UnitOfWork     ports.UnitOfWork
	Inspector      ports.DeploymentInspector
	Metrics        ports.MetricsPort
	Executor       ports.RollbackExecutor
	RollbackPolicy ports.RollbackPolicy
	ApprovalPolicy ports.ApprovalPolicy
	Clock          ports.Clock
	IDGenerator    ports.IDGenerator
	// Access authorizes every human-initiated use case. Required: there is
	// no permissive default.
	Access AccessPolicy
	// Tracer is optional; nil disables tracing.
	Tracer ports.Tracer
}

type WorkflowConfig struct {
	ProposalTTL    time.Duration
	ApprovalTTL    time.Duration
	CleanupTimeout time.Duration
	SystemActor    domain.Actor
	// RecoveryTimeout bounds how long recovery verification keeps checking
	// after a rollback: right after the rollout completes, the metrics window
	// still holds the errors from before it. Verify checks once, then every
	// RecoveryPollInterval while the check fails, at most
	// 1 + RecoveryTimeout/RecoveryPollInterval times, and records the first
	// passing result or the last failing one. Zero checks exactly once.
	RecoveryTimeout      time.Duration
	RecoveryPollInterval time.Duration
}

func (c WorkflowConfig) validate() error {
	if c.ProposalTTL <= 0 || c.ApprovalTTL <= 0 || c.CleanupTimeout <= 0 {
		return &domain.ValidationError{Field: "workflow_config", Reason: "durations must be positive"}
	}
	if c.RecoveryTimeout < 0 || c.RecoveryPollInterval < 0 || (c.RecoveryTimeout > 0 && c.RecoveryPollInterval == 0) {
		return &domain.ValidationError{Field: "workflow_config.recovery", Reason: "recovery timeout must not be negative and needs a positive poll interval"}
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
	// ApproveAndExecute composes Approvals, Executions, Verifications, and
	// Queries into the approve -> execute -> verify use case.
	ApproveAndExecute *ApproveAndExecuteService
}

func NewServices(dependencies Dependencies, config WorkflowConfig) (Services, error) {
	if dependencies.UnitOfWork == nil || dependencies.Inspector == nil || dependencies.Metrics == nil || dependencies.Executor == nil ||
		dependencies.RollbackPolicy == nil ||
		dependencies.ApprovalPolicy == nil ||
		dependencies.Clock == nil ||
		dependencies.IDGenerator == nil ||
		dependencies.Access == nil {
		return Services{}, errors.New("all service dependencies are required")
	}
	if err := config.validate(); err != nil {
		return Services{}, fmt.Errorf("validate workflow config: %w", err)
	}
	tracer := dependencies.Tracer
	if tracer == nil {
		tracer = ports.NoopTracer{}
	}
	trail := audit{ids: dependencies.IDGenerator, systemActor: config.SystemActor}
	uow, clock, access := dependencies.UnitOfWork, dependencies.Clock, dependencies.Access
	services := Services{
		Incidents: &IncidentService{audit: trail, uow: uow, clock: clock, tracer: tracer, access: access},
		Investigations: &InvestigationService{
			audit: trail, uow: uow, clock: clock,
			rollbackPolicy: dependencies.RollbackPolicy, proposalTTL: config.ProposalTTL, cleanupTimeout: config.CleanupTimeout,
		},
		Approvals: &ApprovalService{audit: trail, uow: uow, clock: clock, approvalTTL: config.ApprovalTTL, access: access},
		Executions: &ExecutionService{
			audit: trail, uow: uow, clock: clock, inspector: dependencies.Inspector, executor: dependencies.Executor,
			approvalPolicy: dependencies.ApprovalPolicy, cleanupTimeout: config.CleanupTimeout,
		},
		Verifications: &VerificationService{
			audit: trail, uow: uow, clock: clock, inspector: dependencies.Inspector, metrics: dependencies.Metrics,
			recoveryTimeout: config.RecoveryTimeout, recoveryPollInterval: config.RecoveryPollInterval,
		},
		Queries: &QueryService{uow: uow, access: access},
	}
	services.ApproveAndExecute = &ApproveAndExecuteService{
		approvals: services.Approvals, executions: services.Executions,
		verifications: services.Verifications, queries: services.Queries,
	}
	return services, nil
}
