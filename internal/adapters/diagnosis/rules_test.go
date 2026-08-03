package diagnosis_test

import (
	"context"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/diagnosis"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/belLena81/rollback-guard/internal/testkit"
)

func TestRuleDiagnoserScenarios(t *testing.T) {
	base := diagnosisInput(t)
	tests := []struct {
		name   string
		mutate func(*ports.InvestigationInput)
		want   domain.Recommendation
	}{
		{name: "correlated error storm", mutate: func(_ *ports.InvestigationInput) {}, want: domain.RecommendRollback},
		{name: "collector threshold is metadata", mutate: func(input *ports.InvestigationInput) { input.Metric.Threshold = .90 }, want: domain.RecommendRollback},
		{name: "crash loop", mutate: addCrashLoopEvidence(t), want: domain.RecommendRollback},
		{name: "missing target revision", mutate: func(input *ports.InvestigationInput) { input.Deployment.PreviousStableRevision = "" }, want: domain.RecommendNoAction},
		{name: "unrelated metric breach", mutate: func(input *ports.InvestigationInput) { input.Metric.Name = "cpu_usage" }, want: domain.RecommendNoAction},
		{name: "insufficient evidence", mutate: func(input *ports.InvestigationInput) { input.Evidence = nil }, want: domain.RecommendNoAction},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := base
			input.Evidence = append([]domain.Evidence(nil), base.Evidence...)
			test.mutate(&input)
			diagnoser, err := diagnosis.NewRuleDiagnoser(&testkit.IDGenerator{}, diagnosis.RuleDiagnoserConfig{CorrelationWindow: 10 * time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			conclusion, err := diagnoser.Diagnose(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			if conclusion.Recommendation() != test.want {
				t.Fatalf("recommendation = %s, want %s", conclusion.Recommendation(), test.want)
			}
		})
	}
}

func diagnosisInput(t *testing.T) ports.InvestigationInput {
	t.Helper()
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	target, err := domain.NewDeploymentTarget("cluster", "namespace", "api", "service")
	if err != nil {
		t.Fatal(err)
	}
	symptom, err := domain.NewSymptom("http_error_rate", 0.35, 0.10, "ratio", now)
	if err != nil {
		t.Fatal(err)
	}
	incident, err := domain.NewIncident("incident", target, symptom, now)
	if err != nil {
		t.Fatal(err)
	}
	deployment := domain.DeploymentEvidence{Target: target, CurrentRevision: "revision-2", PreviousStableRevision: "revision-1", DesiredReplicas: 3, ReadyReplicas: 1, AvailableReplicas: 1, Healthy: false, RolloutAt: now.Add(-time.Minute)}
	metric := domain.MetricEvidence{Name: "http_error_rate", Value: 0.35, Threshold: 0.10, Unit: "ratio", ObservedAt: now}
	deploymentEvidence, err := domain.NewDeploymentEvidence("deployment", incident.ID(), deployment, now)
	if err != nil {
		t.Fatal(err)
	}
	metricEvidence, err := domain.NewMetricEvidence("metric", incident.ID(), metric, now)
	if err != nil {
		t.Fatal(err)
	}
	return ports.InvestigationInput{Incident: incident, Deployment: deployment, Metric: metric, Evidence: []domain.Evidence{deploymentEvidence, metricEvidence}, Now: now}
}

func addCrashLoopEvidence(t *testing.T) func(*ports.InvestigationInput) {
	t.Helper()
	return func(input *ports.InvestigationInput) {
		evidence, err := domain.NewPodEvidence("pod", input.Incident.ID(), domain.PodEvidence{Pod: "api-123", CrashLoop: true, Restarts: 8, ObservedAt: input.Now}, input.Now)
		if err != nil {
			t.Fatal(err)
		}
		input.Evidence = append(input.Evidence, evidence)
	}
}
