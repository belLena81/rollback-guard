package domain_test

import (
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
)

func TestEligibilityPolicyReportsEveryRule(t *testing.T) {
	base := eligibilityFixture(t)
	differentCluster := mustTarget(t, "different", "namespace", "api", "service")
	differentNamespace := mustTarget(t, "cluster", "different", "api", "service")
	differentDeployment := mustTarget(t, "cluster", "namespace", "different", "service")
	differentService := mustTarget(t, "cluster", "namespace", "api", "different")
	tests := []struct {
		name   string
		rule   string
		mutate func(*eligibilityValues)
	}{
		{name: "recommendation", rule: "rollback_recommended", mutate: func(value *eligibilityValues) { value.recommendation = domain.RecommendNoAction }},
		{name: "confidence", rule: "minimum_confidence", mutate: func(value *eligibilityValues) { value.confidence = 0.79 }},
		{name: "foreign evidence", rule: "evidence_belongs_to_incident", mutate: func(value *eligibilityValues) { value.deploymentIncidentID = "another-incident" }},
		{name: "deployment reference", rule: "deployment_evidence_referenced", mutate: func(value *eligibilityValues) { value.referenceDeployment = false }},
		{name: "metric reference", rule: "metric_evidence_referenced", mutate: func(value *eligibilityValues) { value.referenceMetric = false }},
		{name: "cluster allow-list", rule: "deployment_target_allowed", mutate: func(value *eligibilityValues) { value.incidentTarget = differentCluster }},
		{name: "namespace allow-list", rule: "deployment_target_allowed", mutate: func(value *eligibilityValues) { value.incidentTarget = differentNamespace }},
		{name: "deployment allow-list", rule: "deployment_target_allowed", mutate: func(value *eligibilityValues) { value.incidentTarget = differentDeployment }},
		{name: "service allow-list", rule: "deployment_target_allowed", mutate: func(value *eligibilityValues) { value.incidentTarget = differentService }},
		{name: "deployment target", rule: "deployment_evidence_matches_incident_target", mutate: func(value *eligibilityValues) { value.deploymentTarget = differentDeployment }},
		{name: "metric name", rule: "metric_matches_symptom", mutate: func(value *eligibilityValues) { value.metricName = "latency" }},
		{name: "metric unit", rule: "metric_matches_symptom", mutate: func(value *eligibilityValues) { value.metricUnit = "seconds" }},
		{name: "current metric breach", rule: "current_metric_threshold_breached", mutate: func(value *eligibilityValues) { value.metricValue = 0.09 }},
		{name: "symptom breach", rule: "symptom_threshold_breached", mutate: func(value *eligibilityValues) { value.observed = 0.09 }},
		{name: "rollout health", rule: "current_rollout_unhealthy", mutate: func(value *eligibilityValues) { value.healthy = true }},
		{name: "temporal correlation", rule: "rollout_temporally_correlated", mutate: func(value *eligibilityValues) { value.rolloutAt = value.now.Add(time.Minute) }},
		{name: "stable revision", rule: "previous_stable_revision_distinct", mutate: func(value *eligibilityValues) { value.previousRevision = "revision-2" }},
	}
	policy := mustEligibilityPolicy(t, base.incidentTarget)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := base
			test.mutate(&values)
			decision, err := policy.Evaluate(values.candidate(t))
			if err != nil {
				t.Fatal(err)
			}
			if decision.Allowed() {
				t.Fatal("decision allowed, want denial")
			}
			assertRule(t, decision.Rules(), test.rule, false)
		})
	}
}

func TestEligibilityPolicyRejectsInvalidAllowedTargets(t *testing.T) {
	target := mustTarget(t, "cluster", "namespace", "api", "service")
	tests := []struct {
		name    string
		targets []domain.DeploymentTarget
	}{
		{name: "empty"},
		{name: "duplicate", targets: []domain.DeploymentTarget{target, target}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := domain.NewEligibilityPolicy(domain.EligibilityPolicyConfig{MinimumConfidence: .8, CorrelationWindow: 10 * time.Minute, AllowedTargets: test.targets}); err == nil {
				t.Fatal("NewEligibilityPolicy() error = nil, want invalid allow-list")
			}
		})
	}
}

func TestEligibilityPolicyUsesOnlyReferencedMetricEvidence(t *testing.T) {
	values := eligibilityFixture(t)
	values.metricValue = 0.09
	candidate := values.candidate(t)
	unreferenced, err := domain.NewMetricEvidence(
		"unreferenced-metric",
		candidate.Incident.ID(),
		domain.MetricEvidence{Name: "http_error_rate", Value: 0.35, Threshold: 0.10, Unit: "ratio", ObservedAt: values.now},
		values.now,
	)
	if err != nil {
		t.Fatal(err)
	}
	candidate.Evidence = append(candidate.Evidence, unreferenced)
	decision, err := mustEligibilityPolicy(t, candidate.Incident.Target()).Evaluate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Allowed() {
		t.Fatal("decision allowed using unreferenced breached metric")
	}
	assertRule(t, decision.Rules(), "current_metric_threshold_breached", false)
}

func mustEligibilityPolicy(t *testing.T, targets ...domain.DeploymentTarget) domain.EligibilityPolicy {
	t.Helper()
	policy, err := domain.NewEligibilityPolicy(domain.EligibilityPolicyConfig{MinimumConfidence: .8, CorrelationWindow: 10 * time.Minute, AllowedTargets: targets})
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func TestRecoveryPolicyRequiresEveryRecoveryCondition(t *testing.T) {
	values := eligibilityFixture(t)
	candidate := values.candidate(t)
	deployment, _ := candidate.Evidence[0].Deployment()
	deployment.CurrentRevision = "revision-1"
	deployment.Healthy = true
	deployment.ReadyReplicas = deployment.DesiredReplicas
	deployment.AvailableReplicas = deployment.DesiredReplicas
	metric := domain.MetricEvidence{Name: "http_error_rate", Value: 0.01, Threshold: 0.10, Unit: "ratio", ObservedAt: values.now}
	tests := []struct {
		name   string
		mutate func(*domain.DeploymentEvidence, *domain.MetricEvidence)
	}{
		{name: "revision", mutate: func(deployment *domain.DeploymentEvidence, _ *domain.MetricEvidence) {
			deployment.CurrentRevision = "revision-3"
		}},
		{name: "ready replicas", mutate: func(deployment *domain.DeploymentEvidence, _ *domain.MetricEvidence) { deployment.ReadyReplicas-- }},
		{name: "available replicas", mutate: func(deployment *domain.DeploymentEvidence, _ *domain.MetricEvidence) { deployment.AvailableReplicas-- }},
		{name: "health", mutate: func(deployment *domain.DeploymentEvidence, _ *domain.MetricEvidence) { deployment.Healthy = false }},
		{name: "strict metric boundary", mutate: func(_ *domain.DeploymentEvidence, metric *domain.MetricEvidence) { metric.Value = 0.10 }},
		{name: "metric unit", mutate: func(_ *domain.DeploymentEvidence, metric *domain.MetricEvidence) { metric.Unit = "percent" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			deploymentValue := deployment
			metricValue := metric
			test.mutate(&deploymentValue, &metricValue)
			verification, err := domain.EvaluateRecovery(candidate.Incident.Target(), "revision-1", deploymentValue, metricValue, candidate.Incident.Symptom(), candidate.Incident.ID(), "execution", values.now)
			if err != nil {
				t.Fatal(err)
			}
			if verification.Passed() {
				t.Fatal("verification passed, want failure")
			}
		})
	}
}

type eligibilityValues struct {
	now                  time.Time
	recommendation       domain.Recommendation
	confidence           float64
	deploymentIncidentID domain.IncidentID
	referenceDeployment  bool
	referenceMetric      bool
	observed             float64
	healthy              bool
	rolloutAt            time.Time
	previousRevision     domain.Revision
	incidentTarget       domain.DeploymentTarget
	deploymentTarget     domain.DeploymentTarget
	metricName           string
	metricValue          float64
	metricUnit           string
}

func eligibilityFixture(t *testing.T) eligibilityValues {
	t.Helper()
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	target := mustTarget(t, "cluster", "namespace", "api", "service")
	return eligibilityValues{
		now:                  now,
		recommendation:       domain.RecommendRollback,
		confidence:           0.95,
		deploymentIncidentID: "incident",
		referenceDeployment:  true,
		referenceMetric:      true,
		observed:             0.35,
		healthy:              false,
		rolloutAt:            now.Add(-time.Minute),
		previousRevision:     "revision-1",
		incidentTarget:       target,
		deploymentTarget:     target,
		metricName:           "http_error_rate",
		metricValue:          0.35,
		metricUnit:           "ratio",
	}
}

func (v eligibilityValues) candidate(t *testing.T) domain.EligibilityCandidate {
	t.Helper()
	symptom, err := domain.NewSymptom("http_error_rate", v.observed, 0.10, "ratio", v.now)
	if err != nil {
		t.Fatal(err)
	}
	incident, err := domain.NewIncident("incident", v.incidentTarget, symptom, v.now)
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := domain.NewDeploymentEvidence("deployment", v.deploymentIncidentID, domain.DeploymentEvidence{Target: v.deploymentTarget, CurrentRevision: "revision-2", PreviousStableRevision: v.previousRevision, DesiredReplicas: 3, ReadyReplicas: 1, AvailableReplicas: 1, Healthy: v.healthy, RolloutAt: v.rolloutAt}, v.now)
	if err != nil {
		t.Fatal(err)
	}
	metric, err := domain.NewMetricEvidence("metric", "incident", domain.MetricEvidence{Name: v.metricName, Value: v.metricValue, Threshold: 0.10, Unit: v.metricUnit, ObservedAt: v.now}, v.now)
	if err != nil {
		t.Fatal(err)
	}
	references := make([]domain.EvidenceID, 0, 2)
	if v.referenceDeployment {
		references = append(references, deployment.ID())
	}
	if v.referenceMetric {
		references = append(references, metric.ID())
	}
	if len(references) == 0 {
		references = append(references, "missing")
	}
	conclusion, err := domain.NewInvestigationConclusion("conclusion", "incident", domain.StrategyRules, "rollout regression", v.confidence, references, v.recommendation, v.now)
	if err != nil {
		t.Fatal(err)
	}
	return domain.EligibilityCandidate{DecisionID: "policy", Incident: incident, Conclusion: conclusion, Evidence: []domain.Evidence{deployment, metric}, Now: v.now}
}

func assertRule(t *testing.T, rules []domain.RuleResult, name string, passed bool) {
	t.Helper()
	for _, rule := range rules {
		if rule.Name == name {
			if rule.Passed != passed {
				t.Fatalf("rule %q passed = %v, want %v", name, rule.Passed, passed)
			}
			return
		}
	}
	t.Fatalf("rule %q not found", name)
}
