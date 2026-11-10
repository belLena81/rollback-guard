package postgres

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/jackc/pgx/v5"
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
	Target                 deploymentTargetJSON `json:"target,omitempty"` //nolint:modernize // R7: stored JSON keeps omitempty bytes (out of scope)
	CurrentRevision        string               `json:"current_revision,omitempty"`
	PreviousStableRevision string               `json:"previous_stable_revision,omitempty"`
	DesiredReplicas        int                  `json:"desired_replicas,omitempty"`
	ReadyReplicas          int                  `json:"ready_replicas,omitempty"`
	AvailableReplicas      int                  `json:"available_replicas,omitempty"`
	Healthy                bool                 `json:"healthy,omitempty"`
	RolloutAt              time.Time            `json:"rollout_at,omitempty"` //nolint:modernize // R7: stored JSON keeps omitempty bytes (out of scope)
	Name                   string               `json:"name,omitempty"`
	Value                  float64              `json:"value,omitempty"`
	Threshold              float64              `json:"threshold,omitempty"`
	Unit                   string               `json:"unit,omitempty"`
	ObservedAt             time.Time            `json:"observed_at,omitempty"` //nolint:modernize // R7: stored JSON keeps omitempty bytes (out of scope)
	Pod                    string               `json:"pod,omitempty"`
	CrashLoop              bool                 `json:"crash_loop,omitempty"`
	Restarts               int                  `json:"restarts,omitempty"`
	Reason                 string               `json:"reason,omitempty"`
	Message                string               `json:"message,omitempty"`
}

func marshalEvidencePayload(evidence domain.Evidence) ([]byte, error) {
	if value, ok := evidence.Deployment(); ok {
		return marshalPayload(evidencePayload{
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
		return marshalPayload(evidencePayload{
			Name:       value.Name,
			Value:      value.Value,
			Threshold:  value.Threshold,
			Unit:       value.Unit,
			ObservedAt: value.ObservedAt,
		})
	}
	if value, ok := evidence.Pod(); ok {
		return marshalPayload(evidencePayload{
			Pod:        value.Pod,
			CrashLoop:  value.CrashLoop,
			Restarts:   value.Restarts,
			ObservedAt: value.ObservedAt,
		})
	}
	if value, ok := evidence.KubernetesEvent(); ok {
		return marshalPayload(evidencePayload{
			Reason:     value.Reason,
			Message:    value.Message,
			ObservedAt: value.ObservedAt,
		})
	}
	return nil, errors.New("evidence has no payload")
}

func unmarshalEvidencePayload(
	id domain.EvidenceID,
	incidentID domain.IncidentID,
	kind domain.EvidenceKind,
	collectedAt time.Time,
	data []byte,
) (domain.Evidence, error) {
	var payload evidencePayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return domain.Evidence{}, fmt.Errorf("decode stored payload: %w", err)
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
		value := domain.MetricEvidence{
			Name:       payload.Name,
			Value:      payload.Value,
			Threshold:  payload.Threshold,
			Unit:       payload.Unit,
			ObservedAt: payload.ObservedAt,
		}
		return domain.RehydrateEvidence(domain.EvidenceSnapshot{ID: id, IncidentID: incidentID, Kind: kind, CollectedAt: collectedAt, Metric: &value})
	case domain.EvidencePod:
		value := domain.PodEvidence{Pod: payload.Pod, CrashLoop: payload.CrashLoop, Restarts: payload.Restarts, ObservedAt: payload.ObservedAt}
		return domain.RehydrateEvidence(domain.EvidenceSnapshot{ID: id, IncidentID: incidentID, Kind: kind, CollectedAt: collectedAt, Pod: &value})
	case domain.EvidenceKubernetesEvent:
		value := domain.KubernetesEventEvidence{Reason: payload.Reason, Message: payload.Message, ObservedAt: payload.ObservedAt}
		return domain.RehydrateEvidence(domain.EvidenceSnapshot{ID: id, IncidentID: incidentID, Kind: kind, CollectedAt: collectedAt, KubernetesEvent: &value})
	default:
		return domain.Evidence{}, errors.New("unknown evidence kind")
	}
}

type eventDetailPayload struct {
	Target         deploymentTargetJSON `json:"target,omitempty"` //nolint:modernize // R7: stored JSON keeps omitempty bytes (out of scope)
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
		return marshalPayload(eventDetailPayload{Target: targetJSON(value.Target)})
	case domain.StatusChangedDetail:
		return marshalPayload(eventDetailPayload{From: value.From.String(), To: value.To.String()})
	case domain.EvidenceCollectedDetail:
		return marshalPayload(eventDetailPayload{EvidenceID: string(value.EvidenceID), Kind: value.Kind.String()})
	case domain.DiagnosisCompletedDetail:
		return marshalPayload(eventDetailPayload{ConclusionID: string(value.ConclusionID), Recommendation: value.Recommendation.String()})
	case domain.PolicyEvaluatedDetail:
		return marshalPayload(eventDetailPayload{DecisionID: string(value.DecisionID), Allowed: value.Allowed})
	case domain.ProposalCreatedDetail:
		return marshalPayload(eventDetailPayload{ActionID: string(value.ActionID), ProposalHash: string(value.ProposalHash)})
	case domain.ApprovalRecordedDetail:
		return marshalPayload(eventDetailPayload{ApprovalID: string(value.ApprovalID), ActorID: string(value.Actor.ID()), ActorKind: value.Actor.Kind().String()})
	case domain.RejectionRecordedDetail:
		return marshalPayload(eventDetailPayload{ApprovalID: string(value.ApprovalID), Reason: value.Reason})
	case domain.ExecutionCompletedDetail:
		return marshalPayload(eventDetailPayload{ExecutionID: string(value.ExecutionID), Outcome: value.Outcome.String()})
	case domain.VerificationCompletedDetail:
		return marshalPayload(eventDetailPayload{Passed: value.Passed, Revision: string(value.Revision)})
	case domain.TerminalDetail:
		return marshalPayload(eventDetailPayload{Reason: value.Reason})
	default:
		return nil, fmt.Errorf("unsupported event detail %T", detail)
	}
}

func unmarshalEventDetail(kind domain.EventKind, data []byte) (domain.EventDetail, error) {
	var payload eventDetailPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("decode stored payload: %w", err)
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
		return nil, errors.New("unknown event kind")
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

// marshalPayload encodes a stored JSON payload, naming the operation on failure.
func marshalPayload(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal stored payload: %w", err)
	}
	return encoded, nil
}

// collectRows reads every row with fn, naming the collection on failure. Errors
// returned by fn (including domain errors) stay reachable through errors.Is/As.
func collectRows[T any](rows pgx.Rows, what string, fn pgx.RowToFunc[T]) ([]T, error) {
	items, err := pgx.CollectRows(rows, fn)
	if err != nil {
		return nil, fmt.Errorf("collect %s: %w", what, err)
	}
	return items, nil
}
