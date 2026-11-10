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

func actorKindText(kind domain.ActorKind) string {
	return kind.String()
}

func evidenceKindText(kind domain.EvidenceKind) string {
	return kind.String()
}

func recommendationText(value domain.Recommendation) string {
	return value.String()
}

func strategyText(value domain.DiagnosisStrategy) string {
	return value.String()
}

func approvalDecisionText(value domain.ApprovalDecision) string {
	return value.String()
}

func executionOutcomeText(value domain.ExecutionOutcome) string {
	return value.String()
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
		return statusChangedDetail(payload)
	case domain.EventEvidenceCollected:
		evidenceKind, err := domain.ParseEvidenceKind(payload.Kind)
		if err != nil {
			return nil, err
		}
		return domain.EvidenceCollectedDetail{EvidenceID: domain.EvidenceID(payload.EvidenceID), Kind: evidenceKind}, nil
	case domain.EventDiagnosisCompleted:
		recommendation, err := domain.ParseRecommendation(payload.Recommendation)
		if err != nil {
			return nil, err
		}
		return domain.DiagnosisCompletedDetail{ConclusionID: domain.ConclusionID(payload.ConclusionID), Recommendation: recommendation}, nil
	case domain.EventPolicyEvaluated:
		return domain.PolicyEvaluatedDetail{DecisionID: domain.PolicyDecisionID(payload.DecisionID), Allowed: payload.Allowed}, nil
	case domain.EventProposalCreated:
		return domain.ProposalCreatedDetail{ActionID: domain.ActionID(payload.ActionID), ProposalHash: domain.ProposalHash(payload.ProposalHash)}, nil
	case domain.EventApprovalRecorded:
		return approvalRecordedDetail(payload)
	case domain.EventRejectionRecorded:
		return domain.RejectionRecordedDetail{ApprovalID: domain.ApprovalID(payload.ApprovalID), Reason: payload.Reason}, nil
	case domain.EventExecutionCompleted:
		outcome, err := domain.ParseExecutionOutcome(payload.Outcome)
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

func statusChangedDetail(payload eventDetailPayload) (domain.EventDetail, error) {
	from, err := domain.ParseStatus(payload.From)
	if err != nil {
		return nil, err
	}
	to, err := domain.ParseStatus(payload.To)
	if err != nil {
		return nil, err
	}
	return domain.StatusChangedDetail{From: from, To: to}, nil
}

func approvalRecordedDetail(payload eventDetailPayload) (domain.EventDetail, error) {
	actorKind, err := domain.ParseActorKind(payload.ActorKind)
	if err != nil {
		return nil, err
	}
	actor, err := domain.NewActor(domain.ActorID(payload.ActorID), actorKind)
	if err != nil {
		return nil, err
	}
	return domain.ApprovalRecordedDetail{ApprovalID: domain.ApprovalID(payload.ApprovalID), Actor: actor}, nil
}

func eventKindText(kind domain.EventKind) string {
	return kind.String()
}
