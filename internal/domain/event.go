package domain

import (
	"strings"
	"time"
)

type EventKind uint8

const (
	EventIncidentCreated EventKind = iota + 1
	EventStatusChanged
	EventEvidenceCollected
	EventDiagnosisCompleted
	EventPolicyEvaluated
	EventProposalCreated
	EventApprovalRecorded
	EventRejectionRecorded
	EventExecutionCompleted
	EventVerificationCompleted
	EventIncidentFailed
	EventIncidentExpired
	EventIncidentCanceled
)

func (k EventKind) String() string {
	names := [...]string{"unknown", "incident.created", "incident.status_changed", "investigation.evidence_collected", "investigation.diagnosis_completed", "policy.evaluated", "rollback.proposal_created", "rollback.approved", "rollback.rejected", "rollback.execution_completed", "rollback.verification_completed", "incident.failed", "incident.expired", "incident.canceled"}
	if int(k) >= len(names) {
		return names[0]
	}
	return names[k]
}

type EventDetail interface{ eventDetail() }
type IncidentCreatedDetail struct{ Target DeploymentTarget }

func (IncidentCreatedDetail) eventDetail() {
}

type StatusChangedDetail struct{ From, To Status }

func (StatusChangedDetail) eventDetail() {
}

type EvidenceCollectedDetail struct {
	EvidenceID EvidenceID
	Kind       EvidenceKind
}

func (EvidenceCollectedDetail) eventDetail() {
}

type DiagnosisCompletedDetail struct {
	ConclusionID   ConclusionID
	Recommendation Recommendation
}

func (DiagnosisCompletedDetail) eventDetail() {
}

type PolicyEvaluatedDetail struct {
	DecisionID PolicyDecisionID
	Allowed    bool
}

func (PolicyEvaluatedDetail) eventDetail() {
}

type ProposalCreatedDetail struct {
	ActionID     ActionID
	ProposalHash ProposalHash
}

func (ProposalCreatedDetail) eventDetail() {
}

type ApprovalRecordedDetail struct {
	ApprovalID ApprovalID
	Actor      Actor
}

func (ApprovalRecordedDetail) eventDetail() {
}

type RejectionRecordedDetail struct {
	ApprovalID ApprovalID
	Reason     string
}

func (RejectionRecordedDetail) eventDetail() {
}

type ExecutionCompletedDetail struct {
	ExecutionID ExecutionID
	Outcome     ExecutionOutcome
}

func (ExecutionCompletedDetail) eventDetail() {
}

type VerificationCompletedDetail struct {
	Passed   bool
	Revision Revision
}

func (VerificationCompletedDetail) eventDetail() {
}

type TerminalDetail struct{ Reason string }

func (TerminalDetail) eventDetail() {
}

type OperationEvent struct {
	id         EventID
	incidentID IncidentID
	sequence   uint64
	kind       EventKind
	occurredAt time.Time
	actor      Actor
	detail     EventDetail
}

func NewOperationEvent(id EventID, incidentID IncidentID, sequence uint64, kind EventKind, occurredAt time.Time, actor Actor, detail EventDetail) (OperationEvent, error) {
	if id == "" || incidentID == "" || sequence == 0 || kind < EventIncidentCreated || kind > EventIncidentCanceled || occurredAt.IsZero() || !actor.Valid() || detail == nil {
		return OperationEvent{}, invalid("event", "invalid event")
	}
	if err := validateEventDetail(kind, actor, detail); err != nil {
		return OperationEvent{}, err
	}
	return OperationEvent{id: id, incidentID: incidentID, sequence: sequence, kind: kind, occurredAt: occurredAt.UTC(), actor: actor, detail: detail}, nil
}

func validateEventDetail(kind EventKind, actor Actor, detail EventDetail) error {
	valid := false
	switch value := detail.(type) {
	case IncidentCreatedDetail:
		valid = kind == EventIncidentCreated && value.Target.Valid()
	case StatusChangedDetail:
		valid = kind == EventStatusChanged && value.From.Valid() && value.To.Valid() && value.From != value.To
	case EvidenceCollectedDetail:
		valid = kind == EventEvidenceCollected && value.EvidenceID != "" && value.Kind >= EvidenceDeployment && value.Kind <= EvidenceKubernetesEvent
	case DiagnosisCompletedDetail:
		valid = kind == EventDiagnosisCompleted && value.ConclusionID != "" && (value.Recommendation == RecommendRollback || value.Recommendation == RecommendNoAction)
	case PolicyEvaluatedDetail:
		valid = kind == EventPolicyEvaluated && value.DecisionID != ""
	case ProposalCreatedDetail:
		valid = kind == EventProposalCreated && value.ActionID != "" && value.ProposalHash != ""
	case ApprovalRecordedDetail:
		valid = kind == EventApprovalRecorded && value.ApprovalID != "" && value.Actor.IsHuman() && value.Actor == actor
	case RejectionRecordedDetail:
		valid = kind == EventRejectionRecorded && value.ApprovalID != "" && strings.TrimSpace(value.Reason) != ""
	case ExecutionCompletedDetail:
		valid = kind == EventExecutionCompleted && value.ExecutionID != "" && (value.Outcome == ExecutionSucceeded || value.Outcome == ExecutionFailed)
	case VerificationCompletedDetail:
		valid = kind == EventVerificationCompleted && value.Revision != ""
	case TerminalDetail:
		valid = (kind == EventIncidentFailed || kind == EventIncidentExpired || kind == EventIncidentCanceled) && strings.TrimSpace(value.Reason) != ""
	}
	if !valid {
		return invalid("event.detail", "does not match event kind or contains invalid values")
	}
	return nil
}
func (e OperationEvent) ID() EventID {
	return e.id
}
func (e OperationEvent) IncidentID() IncidentID {
	return e.incidentID
}
func (e OperationEvent) Sequence() uint64 {
	return e.sequence
}
func (e OperationEvent) Kind() EventKind {
	return e.kind
}
func (e OperationEvent) OccurredAt() time.Time {
	return e.occurredAt
}
func (e OperationEvent) Actor() Actor {
	return e.actor
}
func (e OperationEvent) Detail() EventDetail {
	return e.detail
}
