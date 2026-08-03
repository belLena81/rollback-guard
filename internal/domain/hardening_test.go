package domain_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
)

func TestOperationEventRequiresMatchingValidatedDetail(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	target := mustTarget(t, "cluster", "namespace", "deployment", "service")
	system, err := domain.NewActor("guardian", domain.ActorSystem)
	if err != nil {
		t.Fatal(err)
	}
	human, err := domain.NewActor("alice", domain.ActorHuman)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		kind   domain.EventKind
		actor  domain.Actor
		detail domain.EventDetail
	}{
		{"incident created", domain.EventIncidentCreated, system, domain.IncidentCreatedDetail{Target: target}},
		{"status changed", domain.EventStatusChanged, system, domain.StatusChangedDetail{From: domain.StatusCreated, To: domain.StatusInvestigating}},
		{"evidence", domain.EventEvidenceCollected, system, domain.EvidenceCollectedDetail{EvidenceID: "evidence", Kind: domain.EvidenceMetric}},
		{"diagnosis", domain.EventDiagnosisCompleted, system, domain.DiagnosisCompletedDetail{ConclusionID: "conclusion", Recommendation: domain.RecommendRollback}},
		{"policy", domain.EventPolicyEvaluated, system, domain.PolicyEvaluatedDetail{DecisionID: "decision", Allowed: true}},
		{"proposal", domain.EventProposalCreated, system, domain.ProposalCreatedDetail{ActionID: "action", ProposalHash: "hash"}},
		{"approval", domain.EventApprovalRecorded, human, domain.ApprovalRecordedDetail{ApprovalID: "approval", Actor: human}},
		{"rejection", domain.EventRejectionRecorded, human, domain.RejectionRecordedDetail{ApprovalID: "approval", Reason: "unsafe"}},
		{"execution", domain.EventExecutionCompleted, system, domain.ExecutionCompletedDetail{ExecutionID: "execution", Outcome: domain.ExecutionSucceeded}},
		{"verification", domain.EventVerificationCompleted, system, domain.VerificationCompletedDetail{Passed: true, Revision: "revision"}},
		{"failed", domain.EventIncidentFailed, system, domain.TerminalDetail{Reason: "failed"}},
		{"expired", domain.EventIncidentExpired, system, domain.TerminalDetail{Reason: "expired"}},
		{"canceled", domain.EventIncidentCanceled, human, domain.TerminalDetail{Reason: "canceled"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := domain.NewOperationEvent("event", "incident", 1, test.kind, now, test.actor, test.detail); err != nil {
				t.Fatalf("valid event rejected: %v", err)
			}
			var wrong domain.EventDetail = domain.TerminalDetail{Reason: "wrong detail"}
			if test.kind == domain.EventIncidentFailed || test.kind == domain.EventIncidentExpired || test.kind == domain.EventIncidentCanceled {
				wrong = domain.PolicyEvaluatedDetail{DecisionID: "wrong"}
			}
			if _, err := domain.NewOperationEvent("event", "incident", 1, test.kind, now, test.actor, wrong); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("mismatched detail error = %v, want validation", err)
			}
		})
	}
}

func TestPolicyDecisionRejectsDuplicateRuleNames(t *testing.T) {
	_, err := domain.NewPolicyDecision("decision", "incident", []domain.RuleResult{{Name: "same", Passed: true}, {Name: "same", Passed: false}}, time.Now())
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want validation", err)
	}
}

func TestInvestigationConclusionRejectsNonFiniteConfidence(t *testing.T) {
	tests := []struct {
		name       string
		confidence float64
		wantError  bool
	}{
		{name: "zero", confidence: 0},
		{name: "one", confidence: 1},
		{name: "NaN", confidence: math.NaN(), wantError: true},
		{name: "positive infinity", confidence: math.Inf(1), wantError: true},
		{name: "negative infinity", confidence: math.Inf(-1), wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := domain.NewInvestigationConclusion("conclusion", "incident", domain.StrategyRules, "hypothesis", test.confidence, []domain.EvidenceID{"evidence"}, domain.RecommendRollback, time.Now())
			if test.wantError && !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("error = %v, want validation", err)
			}
			if !test.wantError && err != nil {
				t.Fatalf("error = %v, want nil", err)
			}
		})
	}
}

func TestSuccessfulResultsRejectFailureReasons(t *testing.T) {
	now := time.Now()
	if _, err := domain.NewRollbackExecution("execution", "incident", "key", "from", "to", domain.ExecutionSucceeded, now, now, "failure"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("execution error = %v, want validation", err)
	}
	if _, err := domain.NewRecoveryVerification("incident", "execution", true, "revision", 1, 1, 1, .01, now, "failure"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("verification error = %v, want validation", err)
	}
}

func TestIdempotencyRecordOnlyTransitionsPendingToCompleted(t *testing.T) {
	now := time.Now()
	record, err := domain.NewPendingIdempotencyRecord("key", domain.IdempotencyOperationExecuteRollback, "hash", "incident", now)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := record.Complete("execution", now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if completed.State() != domain.IdempotencyCompleted || completed.ResultID() != "execution" {
		t.Fatal("completion was not recorded")
	}
	if _, err := completed.Complete("other", now.Add(2*time.Second)); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("second completion error = %v, want validation", err)
	}
}

func TestEligibilityPolicyCopiesAllowedTargets(t *testing.T) {
	target := mustTarget(t, "cluster", "namespace", "deployment", "service")
	targets := []domain.DeploymentTarget{target}
	policy, err := domain.NewEligibilityPolicy(domain.EligibilityPolicyConfig{MinimumConfidence: .8, CorrelationWindow: time.Minute, AllowedTargets: targets})
	if err != nil {
		t.Fatal(err)
	}
	targets[0] = mustTarget(t, "other", "namespace", "deployment", "service")
	if err := policy.Validate(); err != nil {
		t.Fatalf("external mutation changed policy: %v", err)
	}
}

func TestDeploymentTargetValidationOrderIsDeterministic(t *testing.T) {
	_, err := domain.NewDeploymentTarget("", "", "", "")
	var validation *domain.ValidationError
	if !errors.As(err, &validation) || validation.Field != "cluster" {
		t.Fatalf("first validation field = %v, want cluster", err)
	}
}

func TestDomainValuesExposeCompleteImmutableSnapshots(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	target := mustTarget(t, "cluster", "namespace", "deployment", "service")
	symptom, err := domain.NewSymptom("errors", .2, .1, "ratio", now)
	if err != nil {
		t.Fatal(err)
	}
	incident, err := domain.NewIncident("incident", target, symptom, now)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := domain.NewActor("guardian", domain.ActorSystem)
	if err != nil {
		t.Fatal(err)
	}
	event, err := domain.NewOperationEvent("event", incident.ID(), 1, domain.EventIncidentCreated, now, actor, domain.IncidentCreatedDetail{Target: target})
	if err != nil {
		t.Fatal(err)
	}
	if event.ID() != "event" || event.IncidentID() != incident.ID() || event.Sequence() != 1 || event.Kind().String() != "incident.created" || event.OccurredAt() != now || event.Actor() != actor || event.Detail() == nil {
		t.Fatal("event accessors returned an incomplete snapshot")
	}
	if target.Cluster() != "cluster" || target.Namespace() != "namespace" || target.Deployment() != "deployment" || target.Service() != "service" || symptom.Observed() != .2 || incident.CreatedAt() != now || incident.UpdatedAt() != now {
		t.Fatal("incident accessors returned unexpected values")
	}
	pod, err := domain.NewPodEvidence("pod", incident.ID(), domain.PodEvidence{Pod: "pod-1", CrashLoop: true, Restarts: 2, ObservedAt: now}, now)
	if err != nil {
		t.Fatal(err)
	}
	kubeEvent, err := domain.NewKubernetesEventEvidence("kube-event", incident.ID(), domain.KubernetesEventEvidence{Reason: "BackOff", Message: "retrying", ObservedAt: now}, now)
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := pod.Pod(); !ok || value.Pod != "pod-1" || pod.CollectedAt() != now {
		t.Fatal("pod evidence snapshot mismatch")
	}
	if value, ok := kubeEvent.KubernetesEvent(); !ok || value.Reason != "BackOff" {
		t.Fatal("kubernetes event snapshot mismatch")
	}
	conclusion, err := domain.NewInvestigationConclusion("conclusion", incident.ID(), domain.StrategyRules, "rollout regression", .9, []domain.EvidenceID{pod.ID()}, domain.RecommendRollback, now)
	if err != nil {
		t.Fatal(err)
	}
	if conclusion.Strategy().String() != "rules" || conclusion.Hypothesis() == "" || conclusion.CreatedAt() != now || conclusion.Recommendation().String() != "rollback" || pod.Kind().String() != "pod" {
		t.Fatal("diagnosis accessors returned unexpected values")
	}
	execution, err := domain.NewRollbackExecution("execution", incident.ID(), "key", "revision-2", "revision-1", domain.ExecutionSucceeded, now, now.Add(time.Second), "")
	if err != nil {
		t.Fatal(err)
	}
	verification, err := domain.NewRecoveryVerification(incident.ID(), execution.ID(), true, "revision-1", 3, 3, 3, .01, now.Add(2*time.Second), "")
	if err != nil {
		t.Fatal(err)
	}
	if execution.Outcome().String() != "succeeded" || execution.StartedAt() != now || execution.CompletedAt() != now.Add(time.Second) || verification.DesiredReplicas() != 3 || verification.ReadyReplicas() != 3 || verification.AvailableReplicas() != 3 || verification.MetricValue() != .01 || verification.CheckedAt() != now.Add(2*time.Second) {
		t.Fatal("execution or verification snapshot mismatch")
	}
}
