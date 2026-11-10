package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/belLena81/rollback-guard/internal/domain"
)

type DeploymentInspector struct {
	caller Caller
}

func NewDeploymentInspector(caller Caller) (*DeploymentInspector, error) {
	if caller == nil {
		return nil, errors.New("deployment MCP caller is required")
	}
	return &DeploymentInspector{caller: caller}, nil
}

func (a *DeploymentInspector) InspectDeployment(ctx context.Context, target domain.DeploymentTarget) (domain.DeploymentEvidence, error) {
	output, err := callTool[TargetInput, DeploymentOutput](ctx, a.caller, ToolGetDeployment, TargetInput{Target: TargetFromDomain(target)})
	if err != nil {
		return domain.DeploymentEvidence{}, fmt.Errorf("inspect deployment through MCP: %w", err)
	}
	returnedTarget, err := output.Target.Domain()
	if err != nil || !returnedTarget.Equal(target) || output.CurrentRevision == "" || output.RolloutAt.IsZero() {
		return domain.DeploymentEvidence{}, errors.New("deployment MCP returned inconsistent evidence")
	}
	return domain.DeploymentEvidence{Target: returnedTarget, CurrentRevision: domain.Revision(output.CurrentRevision), PreviousStableRevision: domain.Revision(output.PreviousStableRevision), DesiredReplicas: output.DesiredReplicas, ReadyReplicas: output.ReadyReplicas, AvailableReplicas: output.AvailableReplicas, Healthy: output.Healthy, RolloutAt: output.RolloutAt.UTC()}, nil
}
