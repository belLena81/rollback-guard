package application

import (
	"context"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/diagnosis"
	"github.com/belLena81/rollback-guard/internal/adapters/fake"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/belLena81/rollback-guard/internal/testkit"
)

func TestInvestigationRunnerCollectsEvidenceAndDiagnoses(t *testing.T) {
	now := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	target, err := domain.NewDeploymentTarget("local", "rollback-demo", "demo-api", "demo-api")
	if err != nil {
		t.Fatal(err)
	}
	symptom, err := domain.NewSymptom("http_error_rate", 0.35, 0.10, "ratio", now)
	if err != nil {
		t.Fatal(err)
	}
	incident, err := domain.NewIncident("incident-1", target, symptom, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := incident.StartInvestigation(now); err != nil {
		t.Fatal(err)
	}
	infrastructure := &fake.Infrastructure{
		Deployment: domain.DeploymentEvidence{
			Target: target, CurrentRevision: "revision-2", PreviousStableRevision: "revision-1",
			DesiredReplicas: 3, ReadyReplicas: 1, AvailableReplicas: 1, RolloutAt: now.Add(-time.Minute),
		},
		Metric: domain.MetricEvidence{Name: "http_error_rate", Value: 0.35, Threshold: 0.10, Unit: "ratio", ObservedAt: now},
	}
	ids := &testkit.IDGenerator{}
	diagnoser, err := diagnosis.NewRuleDiagnoser(ids, diagnosis.RuleDiagnoserConfig{CorrelationWindow: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewInvestigationRunner(infrastructure, infrastructure, diagnoser, testkit.NewClock(now), ids)
	if err != nil {
		t.Fatal(err)
	}

	result, err := runner.Investigate(context.Background(), ports.InvestigationRequest{Incident: incident})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Deployment.Target.Equal(target) || len(result.Evidence) != 2 {
		t.Fatalf("result = %#v", result)
	}
	if result.Conclusion.IncidentID() != incident.ID() || result.Conclusion.Recommendation() != domain.RecommendRollback {
		t.Fatalf("conclusion = %#v", result.Conclusion.Snapshot())
	}
}
