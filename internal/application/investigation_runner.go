package application

import (
	"context"
	"fmt"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

type InvestigationRunner struct {
	inspector ports.DeploymentInspector
	metrics   ports.MetricsPort
	diagnoser ports.Diagnoser
	clock     ports.Clock
	ids       ports.IDGenerator
}

func NewInvestigationRunner(inspector ports.DeploymentInspector, metrics ports.MetricsPort, diagnoser ports.Diagnoser, clock ports.Clock, ids ports.IDGenerator) (*InvestigationRunner, error) {
	if inspector == nil || metrics == nil || diagnoser == nil || clock == nil || ids == nil {
		return nil, fmt.Errorf("all investigation runner dependencies are required")
	}
	return &InvestigationRunner{inspector: inspector, metrics: metrics, diagnoser: diagnoser, clock: clock, ids: ids}, nil
}

func (r *InvestigationRunner) Investigate(ctx context.Context, request ports.InvestigationRequest) (ports.InvestigationResult, error) {
	incident := request.Incident
	if err := reportInvestigationProgress(request.Progress, ports.InvestigationStageDeployment); err != nil {
		return ports.InvestigationResult{}, err
	}
	deployment, err := r.inspector.InspectDeployment(ctx, incident.Target())
	if err != nil {
		return ports.InvestigationResult{}, fmt.Errorf("inspect deployment: %w", err)
	}
	if !deployment.Target.Equal(incident.Target()) {
		return ports.InvestigationResult{}, fmt.Errorf("deployment inspection returned a different target")
	}
	if err := reportInvestigationProgress(request.Progress, ports.InvestigationStageMetrics); err != nil {
		return ports.InvestigationResult{}, err
	}
	metric, err := r.metrics.Observe(ctx, incident.Target(), incident.Symptom())
	if err != nil {
		return ports.InvestigationResult{}, fmt.Errorf("observe metric: %w", err)
	}
	now := r.clock.Now()
	deploymentEvidence, err := domain.NewDeploymentEvidence(domain.EvidenceID(r.ids.NewID("evidence")), incident.ID(), deployment, now)
	if err != nil {
		return ports.InvestigationResult{}, err
	}
	metricEvidence, err := domain.NewMetricEvidence(domain.EvidenceID(r.ids.NewID("evidence")), incident.ID(), metric, now)
	if err != nil {
		return ports.InvestigationResult{}, err
	}
	evidence := []domain.Evidence{deploymentEvidence, metricEvidence}
	if err := reportInvestigationProgress(request.Progress, ports.InvestigationStageDiagnosis); err != nil {
		return ports.InvestigationResult{}, err
	}
	conclusion, err := r.diagnoser.Diagnose(ctx, ports.InvestigationInput{Incident: incident, Deployment: deployment, Metric: metric, Evidence: evidence, Now: now})
	if err != nil {
		return ports.InvestigationResult{}, fmt.Errorf("diagnose incident: %w", err)
	}
	if err := reportInvestigationProgress(request.Progress, ports.InvestigationStageFinalizing); err != nil {
		return ports.InvestigationResult{}, err
	}
	return ports.InvestigationResult{Deployment: deployment, Evidence: evidence, Conclusion: conclusion}, nil
}

func reportInvestigationProgress(report func(ports.InvestigationStage) error, stage ports.InvestigationStage) error {
	if report == nil {
		return nil
	}
	return report(stage)
}

var _ ports.Investigator = (*InvestigationRunner)(nil)
