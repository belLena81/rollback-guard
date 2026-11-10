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

// EventDetail is the typed payload of an operation event. It is closed to this
// package: each detail type decides whether it is valid for an event kind and
// actor.
type EventDetail interface {
	validFor(kind EventKind, actor Actor) bool
}
type IncidentCreatedDetail struct{ Target DeploymentTarget }

func (value IncidentCreatedDetail) validFor(kind EventKind, _ Actor) bool {
	return kind == EventIncidentCreated && value.Target.Valid()
}

type StatusChangedDetail struct{ From, To Status }

func (value StatusChangedDetail) validFor(kind EventKind, _ Actor) bool {
	return kind == EventStatusChanged && value.From.Valid() && value.To.Valid() && value.From != value.To
}

type EvidenceCollectedDetail struct {
	EvidenceID EvidenceID
	Kind       EvidenceKind
}

func (value EvidenceCollectedDetail) validFor(kind EventKind, _ Actor) bool {
	return kind == EventEvidenceCollected && value.EvidenceID != "" && value.Kind >= EvidenceDeployment && value.Kind <= EvidenceKubernetesEvent
}

type DiagnosisCompletedDetail struct {
	ConclusionID   ConclusionID
	Recommendation Recommendation
}

func (value DiagnosisCompletedDetail) validFor(kind EventKind, _ Actor) bool {
	return kind == EventDiagnosisCompleted && value.ConclusionID != "" && (value.Recommendation == RecommendRollback || value.Recommendation == RecommendNoAction)
}

type PolicyEvaluatedDetail struct {
	DecisionID PolicyDecisionID
	Allowed    bool
}

func (value PolicyEvaluatedDetail) validFor(kind EventKind, _ Actor) bool {
	return kind == EventPolicyEvaluated && value.DecisionID != ""
}

type ProposalCreatedDetail struct {
	ActionID     ActionID
	ProposalHash ProposalHash
}

func (value ProposalCreatedDetail) validFor(kind EventKind, _ Actor) bool {
	return kind == EventProposalCreated && value.ActionID != "" && value.ProposalHash != ""
}

type ApprovalRecordedDetail struct {
	ApprovalID ApprovalID
	Actor      Actor
}

func (value ApprovalRecordedDetail) validFor(kind EventKind, actor Actor) bool {
	return kind == EventApprovalRecorded && value.ApprovalID != "" && value.Actor.IsHuman() && value.Actor == actor
}

type RejectionRecordedDetail struct {
	ApprovalID ApprovalID
	Reason     string
}

func (value RejectionRecordedDetail) validFor(kind EventKind, _ Actor) bool {
	return kind == EventRejectionRecorded && value.ApprovalID != "" && strings.TrimSpace(value.Reason) != ""
}

type ExecutionCompletedDetail struct {
	ExecutionID ExecutionID
	Outcome     ExecutionOutcome
}

func (value ExecutionCompletedDetail) validFor(kind EventKind, _ Actor) bool {
	return kind == EventExecutionCompleted && value.ExecutionID != "" && (value.Outcome == ExecutionSucceeded || value.Outcome == ExecutionFailed)
}

type VerificationCompletedDetail struct {
	Passed   bool
	Revision Revision
}

func (value VerificationCompletedDetail) validFor(kind EventKind, _ Actor) bool {
	return kind == EventVerificationCompleted && value.Revision != ""
}

type TerminalDetail struct{ Reason string }

func (value TerminalDetail) validFor(kind EventKind, _ Actor) bool {
	return (kind == EventIncidentFailed || kind == EventIncidentExpired || kind == EventIncidentCanceled) && strings.TrimSpace(value.Reason) != ""
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
	if !detail.validFor(kind, actor) {
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
