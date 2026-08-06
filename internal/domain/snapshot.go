package domain

import (
	"strings"
	"time"
)

type IncidentSnapshot struct {
	ID        IncidentID
	Target    DeploymentTarget
	Symptom   Symptom
	Status    Status
	Version   uint64
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (i Incident) Snapshot() IncidentSnapshot {
	return IncidentSnapshot{
		ID:        i.id,
		Target:    i.target,
		Symptom:   i.symptom,
		Status:    i.status,
		Version:   i.version,
		CreatedAt: i.createdAt,
		UpdatedAt: i.updatedAt,
	}
}

func RehydrateIncident(snapshot IncidentSnapshot) (Incident, error) {
	if err := requireText("incident.id", string(snapshot.ID)); err != nil {
		return Incident{}, err
	}
	if !snapshot.Target.Valid() {
		return Incident{}, invalid("incident.target", "must be valid")
	}
	if !snapshot.Symptom.Valid() {
		return Incident{}, invalid("incident.symptom", "must be valid")
	}
	if !snapshot.Status.Valid() {
		return Incident{}, invalid("incident.status", "must be valid")
	}
	if snapshot.Version == 0 {
		return Incident{}, invalid("incident.version", "must be positive")
	}
	if snapshot.CreatedAt.IsZero() || snapshot.UpdatedAt.IsZero() {
		return Incident{}, invalid("incident.timestamps", "must not be zero")
	}
	if snapshot.UpdatedAt.Before(snapshot.CreatedAt) {
		return Incident{}, invalid("incident.updated_at", "must not be before created_at")
	}
	return Incident{
		id:        snapshot.ID,
		target:    snapshot.Target,
		symptom:   snapshot.Symptom,
		status:    snapshot.Status,
		version:   snapshot.Version,
		createdAt: snapshot.CreatedAt.UTC(),
		updatedAt: snapshot.UpdatedAt.UTC(),
	}, nil
}

type EvidenceSnapshot struct {
	ID              EvidenceID
	IncidentID      IncidentID
	Kind            EvidenceKind
	CollectedAt     time.Time
	Deployment      *DeploymentEvidence
	Metric          *MetricEvidence
	Pod             *PodEvidence
	KubernetesEvent *KubernetesEventEvidence
}

func (e Evidence) Snapshot() EvidenceSnapshot {
	snapshot := EvidenceSnapshot{
		ID:          e.id,
		IncidentID:  e.incidentID,
		Kind:        e.kind,
		CollectedAt: e.collectedAt,
	}
	if e.deployment != nil {
		value := *e.deployment
		snapshot.Deployment = &value
	}
	if e.metric != nil {
		value := *e.metric
		snapshot.Metric = &value
	}
	if e.pod != nil {
		value := *e.pod
		snapshot.Pod = &value
	}
	if e.kubernetesEvent != nil {
		value := *e.kubernetesEvent
		snapshot.KubernetesEvent = &value
	}
	return snapshot
}

func RehydrateEvidence(snapshot EvidenceSnapshot) (Evidence, error) {
	switch snapshot.Kind {
	case EvidenceDeployment:
		if snapshot.Deployment == nil {
			return Evidence{}, invalid("evidence.deployment", "must be present")
		}
		return NewDeploymentEvidence(snapshot.ID, snapshot.IncidentID, *snapshot.Deployment, snapshot.CollectedAt)
	case EvidenceMetric:
		if snapshot.Metric == nil {
			return Evidence{}, invalid("evidence.metric", "must be present")
		}
		return NewMetricEvidence(snapshot.ID, snapshot.IncidentID, *snapshot.Metric, snapshot.CollectedAt)
	case EvidencePod:
		if snapshot.Pod == nil {
			return Evidence{}, invalid("evidence.pod", "must be present")
		}
		return NewPodEvidence(snapshot.ID, snapshot.IncidentID, *snapshot.Pod, snapshot.CollectedAt)
	case EvidenceKubernetesEvent:
		if snapshot.KubernetesEvent == nil {
			return Evidence{}, invalid("evidence.kubernetes_event", "must be present")
		}
		return NewKubernetesEventEvidence(snapshot.ID, snapshot.IncidentID, *snapshot.KubernetesEvent, snapshot.CollectedAt)
	default:
		return Evidence{}, invalid("evidence.kind", "unknown kind")
	}
}

type InvestigationConclusionSnapshot struct {
	ID             ConclusionID
	IncidentID     IncidentID
	Strategy       DiagnosisStrategy
	Hypothesis     string
	Confidence     float64
	EvidenceIDs    []EvidenceID
	Recommendation Recommendation
	Provenance     DiagnosisProvenance
	CreatedAt      time.Time
}

func (c InvestigationConclusion) Snapshot() InvestigationConclusionSnapshot {
	return InvestigationConclusionSnapshot{
		ID:             c.id,
		IncidentID:     c.incidentID,
		Strategy:       c.strategy,
		Hypothesis:     c.hypothesis,
		Confidence:     c.confidence,
		EvidenceIDs:    c.EvidenceIDs(),
		Recommendation: c.recommendation,
		Provenance:     c.provenance,
		CreatedAt:      c.createdAt,
	}
}

func RehydrateInvestigationConclusion(snapshot InvestigationConclusionSnapshot) (InvestigationConclusion, error) {
	return NewInvestigationConclusionWithProvenance(
		snapshot.ID,
		snapshot.IncidentID,
		snapshot.Strategy,
		snapshot.Hypothesis,
		snapshot.Confidence,
		snapshot.EvidenceIDs,
		snapshot.Recommendation,
		snapshot.Provenance,
		snapshot.CreatedAt,
	)
}

type PolicyDecisionSnapshot struct {
	ID         PolicyDecisionID
	IncidentID IncidentID
	Rules      []RuleResult
	DecidedAt  time.Time
}

func (d PolicyDecision) Snapshot() PolicyDecisionSnapshot {
	return PolicyDecisionSnapshot{
		ID:         d.id,
		IncidentID: d.incidentID,
		Rules:      d.Rules(),
		DecidedAt:  d.decidedAt,
	}
}

func RehydratePolicyDecision(snapshot PolicyDecisionSnapshot) (PolicyDecision, error) {
	return NewPolicyDecision(snapshot.ID, snapshot.IncidentID, snapshot.Rules, snapshot.DecidedAt)
}

type RollbackProposalSnapshot struct {
	ActionID         ActionID
	IncidentID       IncidentID
	PolicyDecisionID PolicyDecisionID
	Target           DeploymentTarget
	CurrentRevision  Revision
	TargetRevision   Revision
	ExpiresAt        time.Time
	Hash             ProposalHash
}

func (p RollbackProposal) Snapshot() RollbackProposalSnapshot {
	return RollbackProposalSnapshot{
		ActionID:         p.actionID,
		IncidentID:       p.incidentID,
		PolicyDecisionID: p.policyDecisionID,
		Target:           p.target,
		CurrentRevision:  p.currentRevision,
		TargetRevision:   p.targetRevision,
		ExpiresAt:        p.expiresAt,
		Hash:             p.hash,
	}
}

func RehydrateRollbackProposal(snapshot RollbackProposalSnapshot) (RollbackProposal, error) {
	proposal, err := NewRollbackProposal(
		snapshot.ActionID,
		snapshot.IncidentID,
		snapshot.PolicyDecisionID,
		snapshot.Target,
		snapshot.CurrentRevision,
		snapshot.TargetRevision,
		snapshot.ExpiresAt,
	)
	if err != nil {
		return RollbackProposal{}, err
	}
	if proposal.Hash() != snapshot.Hash {
		return RollbackProposal{}, &ApprovalMismatchError{Reason: "stored proposal hash does not match canonical proposal"}
	}
	return proposal, nil
}

type ApprovalSnapshot struct {
	ID              ApprovalID
	Actor           Actor
	Decision        ApprovalDecision
	ActionID        ActionID
	ProposalHash    ProposalHash
	DecidedAt       time.Time
	ExpiresAt       time.Time
	RejectionReason string
}

func (a Approval) Snapshot() ApprovalSnapshot {
	return ApprovalSnapshot{
		ID:              a.id,
		Actor:           a.actor,
		Decision:        a.decision,
		ActionID:        a.actionID,
		ProposalHash:    a.proposalHash,
		DecidedAt:       a.decidedAt,
		ExpiresAt:       a.expiresAt,
		RejectionReason: a.rejectionReason,
	}
}

func RehydrateApproval(snapshot ApprovalSnapshot) (Approval, error) {
	return NewApproval(
		snapshot.ID,
		snapshot.Actor,
		snapshot.Decision,
		snapshot.ActionID,
		snapshot.ProposalHash,
		snapshot.DecidedAt,
		snapshot.ExpiresAt,
		snapshot.RejectionReason,
	)
}

type RollbackExecutionSnapshot struct {
	ID             ExecutionID
	IncidentID     IncidentID
	IdempotencyKey IdempotencyKey
	FromRevision   Revision
	ToRevision     Revision
	Outcome        ExecutionOutcome
	StartedAt      time.Time
	CompletedAt    time.Time
	FailureReason  string
}

func (e RollbackExecution) Snapshot() RollbackExecutionSnapshot {
	return RollbackExecutionSnapshot{
		ID:             e.id,
		IncidentID:     e.incidentID,
		IdempotencyKey: e.key,
		FromRevision:   e.from,
		ToRevision:     e.to,
		Outcome:        e.outcome,
		StartedAt:      e.startedAt,
		CompletedAt:    e.completedAt,
		FailureReason:  e.failureReason,
	}
}

func RehydrateRollbackExecution(snapshot RollbackExecutionSnapshot) (RollbackExecution, error) {
	return NewRollbackExecution(
		snapshot.ID,
		snapshot.IncidentID,
		snapshot.IdempotencyKey,
		snapshot.FromRevision,
		snapshot.ToRevision,
		snapshot.Outcome,
		snapshot.StartedAt,
		snapshot.CompletedAt,
		snapshot.FailureReason,
	)
}

type RecoveryVerificationSnapshot struct {
	IncidentID        IncidentID
	ExecutionID       ExecutionID
	Passed            bool
	Revision          Revision
	DesiredReplicas   int
	ReadyReplicas     int
	AvailableReplicas int
	MetricValue       float64
	CheckedAt         time.Time
	FailureReason     string
}

func (v RecoveryVerification) Snapshot() RecoveryVerificationSnapshot {
	return RecoveryVerificationSnapshot{
		IncidentID:        v.incidentID,
		ExecutionID:       v.executionID,
		Passed:            v.passed,
		Revision:          v.revision,
		DesiredReplicas:   v.desired,
		ReadyReplicas:     v.ready,
		AvailableReplicas: v.available,
		MetricValue:       v.metricValue,
		CheckedAt:         v.checkedAt,
		FailureReason:     v.failureReason,
	}
}

func RehydrateRecoveryVerification(snapshot RecoveryVerificationSnapshot) (RecoveryVerification, error) {
	return NewRecoveryVerification(
		snapshot.IncidentID,
		snapshot.ExecutionID,
		snapshot.Passed,
		snapshot.Revision,
		snapshot.DesiredReplicas,
		snapshot.ReadyReplicas,
		snapshot.AvailableReplicas,
		snapshot.MetricValue,
		snapshot.CheckedAt,
		snapshot.FailureReason,
	)
}

type OperationEventSnapshot struct {
	ID         EventID
	IncidentID IncidentID
	Sequence   uint64
	Kind       EventKind
	OccurredAt time.Time
	Actor      Actor
	Detail     EventDetail
}

func (e OperationEvent) Snapshot() OperationEventSnapshot {
	return OperationEventSnapshot{
		ID:         e.id,
		IncidentID: e.incidentID,
		Sequence:   e.sequence,
		Kind:       e.kind,
		OccurredAt: e.occurredAt,
		Actor:      e.actor,
		Detail:     e.detail,
	}
}

func RehydrateOperationEvent(snapshot OperationEventSnapshot) (OperationEvent, error) {
	return NewOperationEvent(
		snapshot.ID,
		snapshot.IncidentID,
		snapshot.Sequence,
		snapshot.Kind,
		snapshot.OccurredAt,
		snapshot.Actor,
		snapshot.Detail,
	)
}

type IdempotencyRecordSnapshot struct {
	Key         IdempotencyKey
	Operation   IdempotencyOperation
	RequestHash string
	ResourceID  string
	State       IdempotencyState
	StartedAt   time.Time
	ResultID    ExecutionID
	CompletedAt time.Time
}

func (r IdempotencyRecord) Snapshot() IdempotencyRecordSnapshot {
	return IdempotencyRecordSnapshot{
		Key:         r.key,
		Operation:   r.operation,
		RequestHash: r.requestHash,
		ResourceID:  r.resourceID,
		State:       r.state,
		StartedAt:   r.startedAt,
		ResultID:    r.resultID,
		CompletedAt: r.completedAt,
	}
}

func RehydrateIdempotencyRecord(snapshot IdempotencyRecordSnapshot) (IdempotencyRecord, error) {
	if snapshot.Key == "" || !snapshot.Operation.Valid() || strings.TrimSpace(snapshot.RequestHash) == "" || strings.TrimSpace(snapshot.ResourceID) == "" || snapshot.StartedAt.IsZero() {
		return IdempotencyRecord{}, invalid("idempotency_record", "invalid record")
	}
	switch snapshot.State {
	case IdempotencyPending:
		if snapshot.ResultID != "" || !snapshot.CompletedAt.IsZero() {
			return IdempotencyRecord{}, invalid("idempotency_record.completion", "pending record cannot have completion data")
		}
	case IdempotencyCompleted:
		if snapshot.ResultID == "" || snapshot.CompletedAt.IsZero() || snapshot.CompletedAt.Before(snapshot.StartedAt) {
			return IdempotencyRecord{}, invalid("idempotency_record.completion", "invalid completion")
		}
	default:
		return IdempotencyRecord{}, invalid("idempotency_record.state", "unknown state")
	}
	return IdempotencyRecord{
		key:         snapshot.Key,
		operation:   snapshot.Operation,
		requestHash: strings.TrimSpace(snapshot.RequestHash),
		resourceID:  strings.TrimSpace(snapshot.ResourceID),
		state:       snapshot.State,
		startedAt:   snapshot.StartedAt.UTC(),
		resultID:    snapshot.ResultID,
		completedAt: snapshot.CompletedAt.UTC(),
	}, nil
}
