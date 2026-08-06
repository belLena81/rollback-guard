package postgres

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
)

func statusText(status domain.Status) string {
	return status.String()
}

func parseStatus(value string) (domain.Status, error) {
	for status := domain.StatusCreated; status <= domain.StatusFailed; status++ {
		if status.String() == value {
			return status, nil
		}
	}
	return 0, fmt.Errorf("unknown status %q", value)
}

func actorKindText(kind domain.ActorKind) string {
	return kind.String()
}

func parseActorKind(value string) (domain.ActorKind, error) {
	switch value {
	case "human":
		return domain.ActorHuman, nil
	case "agent":
		return domain.ActorAgent, nil
	case "system":
		return domain.ActorSystem, nil
	case "service":
		return domain.ActorService, nil
	default:
		return 0, fmt.Errorf("unknown actor kind %q", value)
	}
}

func evidenceKindText(kind domain.EvidenceKind) string {
	return kind.String()
}

func parseEvidenceKind(value string) (domain.EvidenceKind, error) {
	switch value {
	case "deployment":
		return domain.EvidenceDeployment, nil
	case "metric":
		return domain.EvidenceMetric, nil
	case "pod":
		return domain.EvidencePod, nil
	case "kubernetes_event":
		return domain.EvidenceKubernetesEvent, nil
	default:
		return 0, fmt.Errorf("unknown evidence kind %q", value)
	}
}

func recommendationText(value domain.Recommendation) string {
	return value.String()
}

func parseRecommendation(value string) (domain.Recommendation, error) {
	switch value {
	case "rollback":
		return domain.RecommendRollback, nil
	case "no_action":
		return domain.RecommendNoAction, nil
	default:
		return 0, fmt.Errorf("unknown recommendation %q", value)
	}
}

func strategyText(value domain.DiagnosisStrategy) string {
	return value.String()
}

func parseStrategy(value string) (domain.DiagnosisStrategy, error) {
	switch value {
	case "rules":
		return domain.StrategyRules, nil
	case "llm":
		return domain.StrategyLLM, nil
	case "hybrid":
		return domain.StrategyHybrid, nil
	default:
		return 0, fmt.Errorf("unknown diagnosis strategy %q", value)
	}
}

func parseDiagnosisFallbackReason(value string) (domain.DiagnosisFallbackReason, error) {
	switch value {
	case "provider_error":
		return domain.FallbackProviderError, nil
	case "provider_timeout":
		return domain.FallbackProviderTimeout, nil
	case "provider_refusal":
		return domain.FallbackProviderRefusal, nil
	case "invalid_output":
		return domain.FallbackInvalidOutput, nil
	default:
		return 0, fmt.Errorf("unknown diagnosis fallback reason %q", value)
	}
}

func approvalDecisionText(value domain.ApprovalDecision) string {
	return value.String()
}

func parseApprovalDecision(value string) (domain.ApprovalDecision, error) {
	switch value {
	case "approved":
		return domain.ApprovalApproved, nil
	case "rejected":
		return domain.ApprovalRejected, nil
	default:
		return 0, fmt.Errorf("unknown approval decision %q", value)
	}
}

func executionOutcomeText(value domain.ExecutionOutcome) string {
	return value.String()
}

func parseExecutionOutcome(value string) (domain.ExecutionOutcome, error) {
	switch value {
	case "succeeded":
		return domain.ExecutionSucceeded, nil
	case "failed":
		return domain.ExecutionFailed, nil
	default:
		return 0, fmt.Errorf("unknown execution outcome %q", value)
	}
}

func idempotencyOperationText(value domain.IdempotencyOperation) string {
	if value == domain.IdempotencyOperationExecuteRollback {
		return "execute_rollback"
	}
	return "unknown"
}

func parseIdempotencyOperation(value string) (domain.IdempotencyOperation, error) {
	if value == "execute_rollback" {
		return domain.IdempotencyOperationExecuteRollback, nil
	}
	return 0, fmt.Errorf("unknown idempotency operation %q", value)
}

func idempotencyStateText(value domain.IdempotencyState) string {
	switch value {
	case domain.IdempotencyPending:
		return "pending"
	case domain.IdempotencyCompleted:
		return "completed"
	default:
		return "unknown"
	}
}

func parseIdempotencyState(value string) (domain.IdempotencyState, error) {
	switch value {
	case "pending":
		return domain.IdempotencyPending, nil
	case "completed":
		return domain.IdempotencyCompleted, nil
	default:
		return 0, fmt.Errorf("unknown idempotency state %q", value)
	}
}

type deploymentTargetJSON struct {
	Cluster    string `json:"cluster"`
	Namespace  string `json:"namespace"`
	Deployment string `json:"deployment"`
	Service    string `json:"service"`
}

func targetJSON(target domain.DeploymentTarget) deploymentTargetJSON {
	return deploymentTargetJSON{
		Cluster:    target.Cluster(),
		Namespace:  target.Namespace(),
		Deployment: target.Deployment(),
		Service:    target.Service(),
	}
}

func parseTarget(value deploymentTargetJSON) (domain.DeploymentTarget, error) {
	return domain.NewDeploymentTarget(value.Cluster, value.Namespace, value.Deployment, value.Service)
}

type evidencePayload struct {
	Target                 deploymentTargetJSON `json:"target,omitempty"`
	CurrentRevision        string               `json:"current_revision,omitempty"`
	PreviousStableRevision string               `json:"previous_stable_revision,omitempty"`
	DesiredReplicas        int                  `json:"desired_replicas,omitempty"`
	ReadyReplicas          int                  `json:"ready_replicas,omitempty"`
	AvailableReplicas      int                  `json:"available_replicas,omitempty"`
	Healthy                bool                 `json:"healthy,omitempty"`
	RolloutAt              time.Time            `json:"rollout_at,omitempty"`
	Name                   string               `json:"name,omitempty"`
	Value                  float64              `json:"value,omitempty"`
	Threshold              float64              `json:"threshold,omitempty"`
	Unit                   string               `json:"unit,omitempty"`
	ObservedAt             time.Time            `json:"observed_at,omitempty"`
	Pod                    string               `json:"pod,omitempty"`
	CrashLoop              bool                 `json:"crash_loop,omitempty"`
	Restarts               int                  `json:"restarts,omitempty"`
	Reason                 string               `json:"reason,omitempty"`
	Message                string               `json:"message,omitempty"`
}

func marshalEvidencePayload(evidence domain.Evidence) ([]byte, error) {
	if value, ok := evidence.Deployment(); ok {
		return json.Marshal(evidencePayload{
			Target:                 targetJSON(value.Target),
			CurrentRevision:        string(value.CurrentRevision),
			PreviousStableRevision: string(value.PreviousStableRevision),
			DesiredReplicas:        value.DesiredReplicas,
			ReadyReplicas:          value.ReadyReplicas,
			AvailableReplicas:      value.AvailableReplicas,
			Healthy:                value.Healthy,
			RolloutAt:              value.RolloutAt,
		})
	}
	if value, ok := evidence.Metric(); ok {
		return json.Marshal(evidencePayload{
			Name:       value.Name,
			Value:      value.Value,
			Threshold:  value.Threshold,
			Unit:       value.Unit,
			ObservedAt: value.ObservedAt,
		})
	}
	if value, ok := evidence.Pod(); ok {
		return json.Marshal(evidencePayload{
			Pod:        value.Pod,
			CrashLoop:  value.CrashLoop,
			Restarts:   value.Restarts,
			ObservedAt: value.ObservedAt,
		})
	}
	if value, ok := evidence.KubernetesEvent(); ok {
		return json.Marshal(evidencePayload{
			Reason:     value.Reason,
			Message:    value.Message,
			ObservedAt: value.ObservedAt,
		})
	}
	return nil, fmt.Errorf("evidence has no payload")
}

func unmarshalEvidencePayload(id domain.EvidenceID, incidentID domain.IncidentID, kind domain.EvidenceKind, collectedAt time.Time, data []byte) (domain.Evidence, error) {
	var payload evidencePayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return domain.Evidence{}, err
	}
	switch kind {
	case domain.EvidenceDeployment:
		target, err := parseTarget(payload.Target)
		if err != nil {
			return domain.Evidence{}, err
		}
		value := domain.DeploymentEvidence{
			Target:                 target,
			CurrentRevision:        domain.Revision(payload.CurrentRevision),
			PreviousStableRevision: domain.Revision(payload.PreviousStableRevision),
			DesiredReplicas:        payload.DesiredReplicas,
			ReadyReplicas:          payload.ReadyReplicas,
			AvailableReplicas:      payload.AvailableReplicas,
			Healthy:                payload.Healthy,
			RolloutAt:              payload.RolloutAt,
		}
		return domain.RehydrateEvidence(domain.EvidenceSnapshot{ID: id, IncidentID: incidentID, Kind: kind, CollectedAt: collectedAt, Deployment: &value})
	case domain.EvidenceMetric:
		value := domain.MetricEvidence{Name: payload.Name, Value: payload.Value, Threshold: payload.Threshold, Unit: payload.Unit, ObservedAt: payload.ObservedAt}
		return domain.RehydrateEvidence(domain.EvidenceSnapshot{ID: id, IncidentID: incidentID, Kind: kind, CollectedAt: collectedAt, Metric: &value})
	case domain.EvidencePod:
		value := domain.PodEvidence{Pod: payload.Pod, CrashLoop: payload.CrashLoop, Restarts: payload.Restarts, ObservedAt: payload.ObservedAt}
		return domain.RehydrateEvidence(domain.EvidenceSnapshot{ID: id, IncidentID: incidentID, Kind: kind, CollectedAt: collectedAt, Pod: &value})
	case domain.EvidenceKubernetesEvent:
		value := domain.KubernetesEventEvidence{Reason: payload.Reason, Message: payload.Message, ObservedAt: payload.ObservedAt}
		return domain.RehydrateEvidence(domain.EvidenceSnapshot{ID: id, IncidentID: incidentID, Kind: kind, CollectedAt: collectedAt, KubernetesEvent: &value})
	default:
		return domain.Evidence{}, fmt.Errorf("unknown evidence kind")
	}
}

type eventDetailPayload struct {
	Target         deploymentTargetJSON `json:"target,omitempty"`
	From           string               `json:"from,omitempty"`
	To             string               `json:"to,omitempty"`
	EvidenceID     string               `json:"evidence_id,omitempty"`
	Kind           string               `json:"kind,omitempty"`
	ConclusionID   string               `json:"conclusion_id,omitempty"`
	Recommendation string               `json:"recommendation,omitempty"`
	DecisionID     string               `json:"decision_id,omitempty"`
	Allowed        bool                 `json:"allowed,omitempty"`
	ActionID       string               `json:"action_id,omitempty"`
	ProposalHash   string               `json:"proposal_hash,omitempty"`
	ApprovalID     string               `json:"approval_id,omitempty"`
	ActorID        string               `json:"actor_id,omitempty"`
	ActorKind      string               `json:"actor_kind,omitempty"`
	Reason         string               `json:"reason,omitempty"`
	ExecutionID    string               `json:"execution_id,omitempty"`
	Outcome        string               `json:"outcome,omitempty"`
	Passed         bool                 `json:"passed,omitempty"`
	Revision       string               `json:"revision,omitempty"`
}

func marshalEventDetail(detail domain.EventDetail) ([]byte, error) {
	switch value := detail.(type) {
	case domain.IncidentCreatedDetail:
		return json.Marshal(eventDetailPayload{Target: targetJSON(value.Target)})
	case domain.StatusChangedDetail:
		return json.Marshal(eventDetailPayload{From: value.From.String(), To: value.To.String()})
	case domain.EvidenceCollectedDetail:
		return json.Marshal(eventDetailPayload{EvidenceID: string(value.EvidenceID), Kind: value.Kind.String()})
	case domain.DiagnosisCompletedDetail:
		return json.Marshal(eventDetailPayload{ConclusionID: string(value.ConclusionID), Recommendation: value.Recommendation.String()})
	case domain.PolicyEvaluatedDetail:
		return json.Marshal(eventDetailPayload{DecisionID: string(value.DecisionID), Allowed: value.Allowed})
	case domain.ProposalCreatedDetail:
		return json.Marshal(eventDetailPayload{ActionID: string(value.ActionID), ProposalHash: string(value.ProposalHash)})
	case domain.ApprovalRecordedDetail:
		return json.Marshal(eventDetailPayload{ApprovalID: string(value.ApprovalID), ActorID: string(value.Actor.ID()), ActorKind: value.Actor.Kind().String()})
	case domain.RejectionRecordedDetail:
		return json.Marshal(eventDetailPayload{ApprovalID: string(value.ApprovalID), Reason: value.Reason})
	case domain.ExecutionCompletedDetail:
		return json.Marshal(eventDetailPayload{ExecutionID: string(value.ExecutionID), Outcome: value.Outcome.String()})
	case domain.VerificationCompletedDetail:
		return json.Marshal(eventDetailPayload{Passed: value.Passed, Revision: string(value.Revision)})
	case domain.TerminalDetail:
		return json.Marshal(eventDetailPayload{Reason: value.Reason})
	default:
		return nil, fmt.Errorf("unsupported event detail %T", detail)
	}
}

func unmarshalEventDetail(kind domain.EventKind, data []byte) (domain.EventDetail, error) {
	var payload eventDetailPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	switch kind {
	case domain.EventIncidentCreated:
		target, err := parseTarget(payload.Target)
		if err != nil {
			return nil, err
		}
		return domain.IncidentCreatedDetail{Target: target}, nil
	case domain.EventStatusChanged:
		from, err := parseStatus(payload.From)
		if err != nil {
			return nil, err
		}
		to, err := parseStatus(payload.To)
		if err != nil {
			return nil, err
		}
		return domain.StatusChangedDetail{From: from, To: to}, nil
	case domain.EventEvidenceCollected:
		evidenceKind, err := parseEvidenceKind(payload.Kind)
		if err != nil {
			return nil, err
		}
		return domain.EvidenceCollectedDetail{EvidenceID: domain.EvidenceID(payload.EvidenceID), Kind: evidenceKind}, nil
	case domain.EventDiagnosisCompleted:
		recommendation, err := parseRecommendation(payload.Recommendation)
		if err != nil {
			return nil, err
		}
		return domain.DiagnosisCompletedDetail{ConclusionID: domain.ConclusionID(payload.ConclusionID), Recommendation: recommendation}, nil
	case domain.EventPolicyEvaluated:
		return domain.PolicyEvaluatedDetail{DecisionID: domain.PolicyDecisionID(payload.DecisionID), Allowed: payload.Allowed}, nil
	case domain.EventProposalCreated:
		return domain.ProposalCreatedDetail{ActionID: domain.ActionID(payload.ActionID), ProposalHash: domain.ProposalHash(payload.ProposalHash)}, nil
	case domain.EventApprovalRecorded:
		actorKind, err := parseActorKind(payload.ActorKind)
		if err != nil {
			return nil, err
		}
		actor, err := domain.NewActor(domain.ActorID(payload.ActorID), actorKind)
		if err != nil {
			return nil, err
		}
		return domain.ApprovalRecordedDetail{ApprovalID: domain.ApprovalID(payload.ApprovalID), Actor: actor}, nil
	case domain.EventRejectionRecorded:
		return domain.RejectionRecordedDetail{ApprovalID: domain.ApprovalID(payload.ApprovalID), Reason: payload.Reason}, nil
	case domain.EventExecutionCompleted:
		outcome, err := parseExecutionOutcome(payload.Outcome)
		if err != nil {
			return nil, err
		}
		return domain.ExecutionCompletedDetail{ExecutionID: domain.ExecutionID(payload.ExecutionID), Outcome: outcome}, nil
	case domain.EventVerificationCompleted:
		return domain.VerificationCompletedDetail{Passed: payload.Passed, Revision: domain.Revision(payload.Revision)}, nil
	case domain.EventIncidentFailed, domain.EventIncidentExpired, domain.EventIncidentCanceled:
		return domain.TerminalDetail{Reason: payload.Reason}, nil
	default:
		return nil, fmt.Errorf("unknown event kind")
	}
}

func eventKindText(kind domain.EventKind) string {
	return kind.String()
}

func parseEventKind(value string) (domain.EventKind, error) {
	for kind := domain.EventIncidentCreated; kind <= domain.EventIncidentCanceled; kind++ {
		if kind.String() == value {
			return kind, nil
		}
	}
	return 0, fmt.Errorf("unknown event kind %q", value)
}
