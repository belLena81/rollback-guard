package domain

import "slices"

import "time"

type EligibilityCandidate struct {
	DecisionID PolicyDecisionID
	Incident   Incident
	Conclusion InvestigationConclusion
	Evidence   []Evidence
	Now        time.Time
}

type EligibilityPolicyConfig struct {
	MinimumConfidence float64
	CorrelationWindow time.Duration
	AllowedTargets    []DeploymentTarget
}

type EligibilityPolicy struct {
	minimumConfidence float64
	correlationWindow time.Duration
	allowedTargets    []DeploymentTarget
}

func NewEligibilityPolicy(config EligibilityPolicyConfig) (EligibilityPolicy, error) {
	policy := EligibilityPolicy{
		minimumConfidence: config.MinimumConfidence,
		correlationWindow: config.CorrelationWindow,
		allowedTargets:    slices.Clone(config.AllowedTargets),
	}
	if err := policy.Validate(); err != nil {
		return EligibilityPolicy{}, err
	}
	return policy, nil
}

func (p EligibilityPolicy) Validate() error {
	if p.minimumConfidence < 0 || p.minimumConfidence > 1 || p.correlationWindow <= 0 {
		return invalid("eligibility_policy", "invalid configuration")
	}
	if len(p.allowedTargets) == 0 {
		return invalid("eligibility_policy.allowed_targets", "must not be empty")
	}
	seen := make(map[DeploymentTarget]struct{}, len(p.allowedTargets))
	for _, target := range p.allowedTargets {
		if !target.Valid() {
			return invalid("eligibility_policy.allowed_targets", "contains invalid target")
		}
		if _, duplicate := seen[target]; duplicate {
			return invalid("eligibility_policy.allowed_targets", "contains duplicate target")
		}
		seen[target] = struct{}{}
	}
	return nil
}

// referencedEvidence is what a conclusion's evidence references resolve to:
// the last referenced deployment and metric evidence, and whether every
// reference points at evidence that exists and belongs to the incident.
type referencedEvidence struct {
	deployment *DeploymentEvidence
	metric     *MetricEvidence
	belongs    bool
}

func resolveReferencedEvidence(candidate EligibilityCandidate) referencedEvidence {
	result := referencedEvidence{belongs: true}
	referenced := map[EvidenceID]bool{}
	for _, id := range candidate.Conclusion.EvidenceIDs() {
		referenced[id] = true
	}
	for _, evidence := range candidate.Evidence {
		isReferenced := referenced[evidence.ID()]
		if d, ok := evidence.Deployment(); ok && isReferenced {
			copyValue := d
			result.deployment = &copyValue
		}
		if m, ok := evidence.Metric(); ok && isReferenced {
			copyValue := m
			result.metric = &copyValue
		}
		if isReferenced && evidence.IncidentID() != candidate.Incident.ID() {
			result.belongs = false
		}
	}
	for id := range referenced {
		result.belongs = result.belongs && evidenceBelongs(candidate, id)
	}
	return result
}

func evidenceBelongs(candidate EligibilityCandidate, id EvidenceID) bool {
	return slices.ContainsFunc(candidate.Evidence, func(e Evidence) bool {
		return e.ID() == id && e.IncidentID() == candidate.Incident.ID()
	})
}

func (p EligibilityPolicy) allowsTarget(target DeploymentTarget) bool {
	return slices.ContainsFunc(p.allowedTargets, target.Equal)
}

func (p EligibilityPolicy) Evaluate(candidate EligibilityCandidate) (PolicyDecision, error) {
	if err := p.Validate(); err != nil {
		return PolicyDecision{}, err
	}
	resolved := resolveReferencedEvidence(candidate)
	belongs, deployment, metric := resolved.belongs, resolved.deployment, resolved.metric
	deploymentReferenced, metricReferenced := deployment != nil, metric != nil
	targetAllowed := p.allowsTarget(candidate.Incident.Target())
	// Rollout health is deliberately not a rule. A build that serves errors
	// but passes readiness completes its rollout, so requiring an unhealthy
	// rollout made exactly that regression ineligible. Eligibility rests on
	// the breached metric evidence, the rollout correlation, and a distinct
	// stable target; a stalled rollout without a breached metric is declined.
	//
	// The correlation is checked twice: against the reported symptom start
	// (client-supplied) and against the policy's own clock (rollout_recent),
	// so an old rollout cannot be made to look correlated by reporting a
	// symptom start right after it.
	deploymentMatches, correlated, recent, stable := false, false, false, false
	if deployment != nil {
		deploymentMatches = deployment.Target.Equal(candidate.Incident.Target())
		stable = deployment.PreviousStableRevision != "" && deployment.PreviousStableRevision != deployment.CurrentRevision
		delta := candidate.Incident.Symptom().StartedAt().Sub(deployment.RolloutAt)
		correlated = delta >= 0 && delta <= p.correlationWindow
		recent = RolloutRecent(deployment.RolloutAt, candidate.Now, p.correlationWindow)
	}
	metricMatches, metricBreached := false, false
	if metric != nil {
		metricMatches = metric.Name == candidate.Incident.Symptom().Kind().String() && metric.Unit == candidate.Incident.Symptom().Unit()
		metricBreached = metric.Value >= candidate.Incident.Symptom().Threshold()
	}
	rules := []RuleResult{
		{Name: "rollback_recommended", Passed: candidate.Conclusion.Recommendation() == RecommendRollback},
		{Name: "minimum_confidence", Passed: candidate.Conclusion.Confidence() >= p.minimumConfidence},
		{Name: "conclusion_belongs_to_incident", Passed: candidate.Conclusion.IncidentID() == candidate.Incident.ID()},
		{Name: "evidence_belongs_to_incident", Passed: belongs},
		{Name: "deployment_evidence_referenced", Passed: deploymentReferenced},
		{Name: "metric_evidence_referenced", Passed: metricReferenced},
		{Name: "deployment_target_allowed", Passed: targetAllowed},
		{Name: "deployment_evidence_matches_incident_target", Passed: deploymentMatches},
		{Name: "metric_matches_symptom", Passed: metricMatches},
		{Name: "current_metric_threshold_breached", Passed: metricBreached},
		{Name: "symptom_threshold_breached", Passed: candidate.Incident.Symptom().Breached()},
		{Name: "rollout_temporally_correlated", Passed: correlated},
		{Name: "rollout_recent", Passed: recent},
		{Name: "previous_stable_revision_distinct", Passed: stable},
	}
	return NewPolicyDecision(candidate.DecisionID, candidate.Incident.ID(), rules, candidate.Now)
}

// RolloutClockSkew is how far a rollout time (from the Kubernetes API
// server's clock) may lie ahead of the evaluator's clock and still count as
// "now" rather than as a rollout in the future.
const RolloutClockSkew = 30 * time.Second

// RolloutRecent reports whether a rollout at rolloutAt happened at most window
// before now and not after it (up to RolloutClockSkew). now is the
// evaluator's own clock, never a client-reported time.
func RolloutRecent(rolloutAt, now time.Time, window time.Duration) bool {
	age := now.Sub(rolloutAt)
	return !rolloutAt.IsZero() && age >= -RolloutClockSkew && age <= window
}

func EvaluateRecovery(target DeploymentTarget, targetRevision Revision, deployment DeploymentEvidence, metric MetricEvidence, symptom Symptom, incidentID IncidentID, executionID ExecutionID, now time.Time) (RecoveryVerification, error) {
	passed := deployment.Target.Equal(target) && deployment.CurrentRevision == targetRevision && deployment.Healthy && deployment.DesiredReplicas == deployment.ReadyReplicas && deployment.DesiredReplicas == deployment.AvailableReplicas && metric.Name == symptom.Kind().String() && metric.Unit == symptom.Unit() && metric.Value < symptom.Threshold()
	reason := ""
	if !passed {
		reason = "revision, replicas, rollout health, or recovery metric did not recover"
	}
	return NewRecoveryVerification(incidentID, executionID, passed, deployment.CurrentRevision, deployment.DesiredReplicas, deployment.ReadyReplicas, deployment.AvailableReplicas, metric.Value, now, reason)
}
