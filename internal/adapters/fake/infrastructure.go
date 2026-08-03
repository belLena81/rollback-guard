package fake

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

type Infrastructure struct {
	mu               sync.Mutex
	Deployment       domain.DeploymentEvidence
	Metric           domain.MetricEvidence
	InspectErr       error
	MetricErr        error
	ExecuteErr       error
	Executions       int
	Now              func() time.Time
	BeforeMutation   func(*domain.DeploymentEvidence)
	AfterExecute     func()
	executionResults map[domain.IdempotencyKey]domain.RollbackExecution
}

func (f *Infrastructure) InspectDeployment(ctx context.Context, target domain.DeploymentTarget) (domain.DeploymentEvidence, error) {
	if err := ctx.Err(); err != nil {
		return domain.DeploymentEvidence{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.InspectErr != nil {
		return domain.DeploymentEvidence{}, f.InspectErr
	}
	if !f.Deployment.Target.Equal(target) {
		return domain.DeploymentEvidence{}, fmt.Errorf("unexpected deployment target")
	}
	return f.Deployment, nil
}

func (f *Infrastructure) Observe(ctx context.Context, target domain.DeploymentTarget, _ domain.Symptom) (domain.MetricEvidence, error) {
	if err := ctx.Err(); err != nil {
		return domain.MetricEvidence{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.MetricErr != nil {
		return domain.MetricEvidence{}, f.MetricErr
	}
	if !f.Deployment.Target.Equal(target) {
		return domain.MetricEvidence{}, fmt.Errorf("unexpected metric target")
	}
	return f.Metric, nil
}

func (f *Infrastructure) ExecuteApprovedRollback(ctx context.Context, approved domain.ApprovedRollback, key domain.IdempotencyKey) (domain.RollbackExecution, error) {
	if err := ctx.Err(); err != nil {
		return domain.RollbackExecution{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if existing, ok := f.executionResults[key]; ok {
		return existing, nil
	}
	if f.BeforeMutation != nil {
		f.BeforeMutation(&f.Deployment)
	}
	proposal := approved.Proposal()
	if !f.Deployment.Target.Equal(proposal.Target()) || f.Deployment.CurrentRevision != proposal.CurrentRevision() {
		return domain.RollbackExecution{}, &domain.ApprovalMismatchError{Reason: "deployment identity or current revision changed at mutation boundary"}
	}
	defer func() {
		if f.AfterExecute != nil {
			f.AfterExecute()
		}
	}()
	f.Executions++
	now := time.Now().UTC()
	if f.Now != nil {
		now = f.Now()
	}
	if f.executionResults == nil {
		f.executionResults = make(map[domain.IdempotencyKey]domain.RollbackExecution)
	}
	if f.ExecuteErr != nil {
		execution, err := domain.NewRollbackExecution(
			domain.ExecutionID(fmt.Sprintf("execution-%d", f.Executions)),
			proposal.IncidentID(),
			key,
			proposal.CurrentRevision(),
			proposal.TargetRevision(),
			domain.ExecutionFailed,
			now,
			now,
			f.ExecuteErr.Error(),
		)
		if err == nil {
			f.executionResults[key] = execution
		}
		return execution, err
	}
	f.Deployment.CurrentRevision = proposal.TargetRevision()
	f.Deployment.Healthy = true
	f.Deployment.ReadyReplicas = f.Deployment.DesiredReplicas
	f.Deployment.AvailableReplicas = f.Deployment.DesiredReplicas
	execution, err := domain.NewRollbackExecution(
		domain.ExecutionID(fmt.Sprintf("execution-%d", f.Executions)),
		proposal.IncidentID(),
		key,
		proposal.CurrentRevision(),
		proposal.TargetRevision(),
		domain.ExecutionSucceeded,
		now,
		now,
		"",
	)
	if err == nil {
		f.executionResults[key] = execution
	}
	return execution, err
}

func (f *Infrastructure) SetRecoveryMetric(value float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Metric.Value = value
}

func (f *Infrastructure) SetCurrentRevision(revision domain.Revision) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Deployment.CurrentRevision = revision
}

func (f *Infrastructure) SetHealthy(healthy bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Deployment.Healthy = healthy
}

func (f *Infrastructure) ExecutionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Executions
}

var _ ports.DeploymentInspector = (*Infrastructure)(nil)
var _ ports.MetricsPort = (*Infrastructure)(nil)
var _ ports.RollbackExecutor = (*Infrastructure)(nil)
