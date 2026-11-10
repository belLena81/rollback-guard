package domain

import (
	"errors"
	"testing"
	"time"
)

func TestSymptomKind(t *testing.T) {
	now := time.Unix(1700000000, 0)
	tests := []struct {
		name  string
		value string
		valid bool
	}{
		{"http error rate", "http_error_rate", true},
		{"request latency", "request_latency", true},
		{"pod cpu usage", "pod_cpu_usage", true},
		{"pod memory usage", "pod_memory_usage", true},
		{"empty", "", false},
		{"unsupported", "error_rate", false},
		{"wrong case", "HTTP_ERROR_RATE", false},
		{"padded", " http_error_rate", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			kind, err := ParseSymptomKind(test.value)
			if (err == nil) != test.valid {
				t.Fatalf("ParseSymptomKind(%q) error = %v, want valid=%v", test.value, err, test.valid)
			}
			if !test.valid && !errors.Is(err, ErrValidation) {
				t.Errorf("error %v is not a validation error", err)
			}
			_, symptomErr := NewSymptom(SymptomKind(test.value), 0.2, 0.1, "ratio", now)
			if (symptomErr == nil) != test.valid {
				t.Errorf("NewSymptom(%q) error = %v, want valid=%v", test.value, symptomErr, test.valid)
			}
			if test.valid && kind.String() != test.value {
				t.Errorf("kind = %q, want %q", kind, test.value)
			}
		})
	}
}

// enumCase describes one enumeration: its members and its Parse function.
type enumCase[T enumValue] struct {
	name    string
	members []T
	parse   func(string) (T, error)
}

func runEnumCase[T enumValue](t *testing.T, c enumCase[T]) {
	t.Helper()
	t.Run(c.name, func(t *testing.T) {
		for _, member := range c.members {
			got, err := c.parse(member.String())
			if err != nil || got != member {
				t.Errorf("parse(%q) = %v, %v; want %v", member.String(), got, err, member)
			}
		}
		for _, bad := range []string{"", "unknown", "UNKNOWN ", " ", "no-such-member"} {
			if got, err := c.parse(bad); err == nil {
				t.Errorf("parse(%q) = %v, nil; want error", bad, got)
			}
		}
	})
}

func TestParseEnumsRoundTripAndRejectInvalid(t *testing.T) {
	runEnumCase(t, enumCase[Status]{"status", []Status{
		StatusCreated, StatusInvestigating, StatusAwaitingApproval, StatusApproved, StatusExecuting,
		StatusVerifying, StatusResolved, StatusCanceled, StatusDeclined, StatusRejected, StatusExpired, StatusFailed,
	}, ParseStatus})
	runEnumCase(t, enumCase[ActorKind]{"actor kind", []ActorKind{ActorHuman, ActorAgent, ActorSystem, ActorService}, ParseActorKind})
	runEnumCase(t, enumCase[EvidenceKind]{"evidence kind", []EvidenceKind{
		EvidenceDeployment, EvidenceMetric, EvidencePod, EvidenceKubernetesEvent,
	}, ParseEvidenceKind})
	runEnumCase(t, enumCase[Recommendation]{"recommendation", []Recommendation{RecommendRollback, RecommendNoAction}, ParseRecommendation})
	runEnumCase(t, enumCase[DiagnosisStrategy]{"strategy", []DiagnosisStrategy{StrategyRules, StrategyLLM, StrategyHybrid}, ParseDiagnosisStrategy})
	runEnumCase(t, enumCase[DiagnosisFallbackReason]{"fallback reason", []DiagnosisFallbackReason{
		FallbackProviderError, FallbackProviderTimeout, FallbackProviderRefusal, FallbackInvalidOutput,
	}, ParseDiagnosisFallbackReason})
	runEnumCase(t, enumCase[ApprovalDecision]{"approval decision", []ApprovalDecision{ApprovalApproved, ApprovalRejected}, ParseApprovalDecision})
	runEnumCase(t, enumCase[ExecutionOutcome]{"execution outcome", []ExecutionOutcome{ExecutionSucceeded, ExecutionFailed}, ParseExecutionOutcome})
	runEnumCase(t, enumCase[IdempotencyOperation]{"idempotency operation", []IdempotencyOperation{
		IdempotencyOperationExecuteRollback,
	}, ParseIdempotencyOperation})
	runEnumCase(t, enumCase[IdempotencyState]{"idempotency state", []IdempotencyState{IdempotencyPending, IdempotencyCompleted}, ParseIdempotencyState})
	runEnumCase(t, enumCase[InvestigationTaskState]{"task state", []InvestigationTaskState{
		InvestigationTaskQueued, InvestigationTaskSubmitting, InvestigationTaskSubmitted, InvestigationTaskWorking,
		InvestigationTaskCancelRequested, InvestigationTaskCompleted, InvestigationTaskFailed, InvestigationTaskCanceled,
	}, ParseInvestigationTaskState})
	runEnumCase(t, enumCase[InvestigationStage]{"stage", []InvestigationStage{
		InvestigationStageQueued, InvestigationStageDeployment, InvestigationStageMetrics,
		InvestigationStageDiagnosis, InvestigationStageFinalizing,
	}, ParseInvestigationStage})
	runEnumCase(t, enumCase[RollbackMutationState]{"mutation state", []RollbackMutationState{
		RollbackMutationPending, RollbackMutationCompleted,
	}, ParseRollbackMutationState})
	runEnumCase(t, enumCase[EventKind]{"event kind", []EventKind{
		EventIncidentCreated, EventStatusChanged, EventEvidenceCollected, EventDiagnosisCompleted, EventPolicyEvaluated,
		EventProposalCreated, EventApprovalRecorded, EventRejectionRecorded, EventExecutionCompleted,
		EventVerificationCompleted, EventIncidentFailed, EventIncidentExpired, EventIncidentCanceled,
	}, ParseEventKind})
}
