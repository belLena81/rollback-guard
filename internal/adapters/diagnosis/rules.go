package diagnosis

import (
	"context"
	"fmt"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

type RuleDiagnoser struct {
	ids               ports.IDGenerator
	correlationWindow time.Duration
}

type RuleDiagnoserConfig struct {
	CorrelationWindow time.Duration
}

func NewRuleDiagnoser(ids ports.IDGenerator, config RuleDiagnoserConfig) (*RuleDiagnoser, error) {
	if ids == nil {
		return nil, fmt.Errorf("id generator is required")
	}
	if config.CorrelationWindow <= 0 {
		return nil, fmt.Errorf("correlation window must be positive")
	}
	return &RuleDiagnoser{ids: ids, correlationWindow: config.CorrelationWindow}, nil
}

func (d *RuleDiagnoser) Diagnose(ctx context.Context, input ports.InvestigationInput) (domain.InvestigationConclusion, error) {
	if err := ctx.Err(); err != nil {
		return domain.InvestigationConclusion{}, err
	}

	delta := input.Incident.Symptom().StartedAt().Sub(input.Deployment.RolloutAt)
	deploymentEvidenceFound := false
	metricEvidenceFound := false
	for _, evidence := range input.Evidence {
		if evidence.IncidentID() != input.Incident.ID() {
			continue
		}
		if _, ok := evidence.Deployment(); ok {
			deploymentEvidenceFound = true
		}
		if _, ok := evidence.Metric(); ok {
			metricEvidenceFound = true
		}
	}
	// Rollout health is not required: a build that serves errors but passes
	// readiness completes its rollout, and the breached metric is the
	// regression signal (see domain.EligibilityPolicy).
	correlated := deploymentEvidenceFound &&
		metricEvidenceFound &&
		input.Metric.Name == input.Incident.Symptom().Kind() &&
		input.Metric.Value >= input.Incident.Symptom().Threshold() &&
		delta >= 0 &&
		delta <= d.correlationWindow &&
		domain.RolloutRecent(input.Deployment.RolloutAt, input.Now, d.correlationWindow) &&
		input.Deployment.PreviousStableRevision != "" &&
		input.Deployment.PreviousStableRevision != input.Deployment.CurrentRevision

	recommendation := domain.RecommendNoAction
	confidence := 0.40
	hypothesis := "available evidence does not establish a rollout-caused regression"
	if correlated {
		recommendation = domain.RecommendRollback
		confidence = 0.95
		hypothesis = "the current rollout is temporally correlated with a breached service metric and a stable rollback target exists"
	}

	evidenceIDs := make([]domain.EvidenceID, 0, len(input.Evidence))
	for _, evidence := range input.Evidence {
		evidenceIDs = append(evidenceIDs, evidence.ID())
	}
	return domain.NewInvestigationConclusion(
		domain.ConclusionID(d.ids.NewID("conclusion")),
		input.Incident.ID(),
		domain.StrategyRules,
		hypothesis,
		confidence,
		evidenceIDs,
		recommendation,
		input.Now,
	)
}

var _ ports.Diagnoser = (*RuleDiagnoser)(nil)
