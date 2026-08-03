package httpapi

import (
	"time"

	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
)

type targetDTO struct {
	Cluster    string `json:"cluster"`
	Namespace  string `json:"namespace"`
	Deployment string `json:"deployment"`
	Service    string `json:"service"`
}

func (d targetDTO) domain() (domain.DeploymentTarget, error) {
	return domain.NewDeploymentTarget(d.Cluster, d.Namespace, d.Deployment, d.Service)
}

func targetResponse(target domain.DeploymentTarget) targetDTO {
	return targetDTO{Cluster: target.Cluster(), Namespace: target.Namespace(), Deployment: target.Deployment(), Service: target.Service()}
}

type symptomDTO struct {
	Kind      string    `json:"kind"`
	Observed  float64   `json:"observed"`
	Threshold float64   `json:"threshold"`
	Unit      string    `json:"unit"`
	StartedAt time.Time `json:"startedAt"`
}

func (d symptomDTO) domain() (domain.Symptom, error) {
	return domain.NewSymptom(d.Kind, d.Observed, d.Threshold, d.Unit, d.StartedAt)
}

func symptomResponse(symptom domain.Symptom) symptomDTO {
	return symptomDTO{Kind: symptom.Kind(), Observed: symptom.Observed(), Threshold: symptom.Threshold(), Unit: symptom.Unit(), StartedAt: symptom.StartedAt()}
}

type actorDTO struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

func actorResponse(actor domain.Actor) actorDTO {
	return actorDTO{ID: string(actor.ID()), Kind: actor.Kind().String()}
}

type incidentDTO struct {
	ID        string     `json:"id"`
	Target    targetDTO  `json:"target"`
	Symptom   symptomDTO `json:"symptom"`
	Status    string     `json:"status"`
	Version   uint64     `json:"version"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
}

type conclusionDTO struct {
	ID             string    `json:"id"`
	Strategy       string    `json:"strategy"`
	Hypothesis     string    `json:"hypothesis"`
	Confidence     float64   `json:"confidence"`
	EvidenceIDs    []string  `json:"evidenceIds"`
	Recommendation string    `json:"recommendation"`
	CreatedAt      time.Time `json:"createdAt"`
}

type proposalDTO struct {
	ActionID        string    `json:"actionId"`
	ProposalHash    string    `json:"proposalHash"`
	CurrentRevision string    `json:"currentRevision"`
	TargetRevision  string    `json:"targetRevision"`
	ExpiresAt       time.Time `json:"expiresAt"`
}

type approvalDTO struct {
	ID              string    `json:"id"`
	Actor           actorDTO  `json:"actor"`
	Decision        string    `json:"decision"`
	ActionID        string    `json:"actionId"`
	ProposalHash    string    `json:"proposalHash"`
	DecidedAt       time.Time `json:"decidedAt"`
	ExpiresAt       time.Time `json:"expiresAt"`
	RejectionReason string    `json:"rejectionReason,omitempty"`
}

type executionDTO struct {
	ID             string    `json:"id"`
	IdempotencyKey string    `json:"idempotencyKey"`
	FromRevision   string    `json:"fromRevision"`
	ToRevision     string    `json:"toRevision"`
	Outcome        string    `json:"outcome"`
	StartedAt      time.Time `json:"startedAt"`
	CompletedAt    time.Time `json:"completedAt"`
	FailureReason  string    `json:"failureReason,omitempty"`
}

type verificationDTO struct {
	Passed            bool      `json:"passed"`
	Revision          string    `json:"revision"`
	DesiredReplicas   int       `json:"desiredReplicas"`
	ReadyReplicas     int       `json:"readyReplicas"`
	AvailableReplicas int       `json:"availableReplicas"`
	MetricValue       float64   `json:"metricValue"`
	CheckedAt         time.Time `json:"checkedAt"`
	FailureReason     string    `json:"failureReason,omitempty"`
}

type incidentSummaryDTO struct {
	Incident     incidentDTO      `json:"incident"`
	Conclusion   *conclusionDTO   `json:"conclusion,omitempty"`
	Proposal     *proposalDTO     `json:"proposal,omitempty"`
	Approval     *approvalDTO     `json:"approval,omitempty"`
	Execution    *executionDTO    `json:"execution,omitempty"`
	Verification *verificationDTO `json:"verification,omitempty"`
}

func summaryResponse(summary application.IncidentSummary) incidentSummaryDTO {
	response := incidentSummaryDTO{Incident: incidentResponse(summary.Incident)}
	if summary.Conclusion != nil {
		value := conclusionResponse(*summary.Conclusion)
		response.Conclusion = &value
	}
	if summary.Proposal != nil {
		value := proposalResponse(*summary.Proposal)
		response.Proposal = &value
	}
	if summary.Approval != nil {
		value := approvalResponse(*summary.Approval)
		response.Approval = &value
	}
	if summary.Execution != nil {
		value := executionResponse(*summary.Execution)
		response.Execution = &value
	}
	if summary.Verification != nil {
		value := verificationResponse(*summary.Verification)
		response.Verification = &value
	}
	return response
}

func incidentResponse(incident domain.Incident) incidentDTO {
	return incidentDTO{
		ID:        string(incident.ID()),
		Target:    targetResponse(incident.Target()),
		Symptom:   symptomResponse(incident.Symptom()),
		Status:    incident.Status().String(),
		Version:   incident.Version(),
		CreatedAt: incident.CreatedAt(),
		UpdatedAt: incident.UpdatedAt(),
	}
}

func conclusionResponse(conclusion domain.InvestigationConclusion) conclusionDTO {
	evidenceIDs := conclusion.EvidenceIDs()
	ids := make([]string, 0, len(evidenceIDs))
	for _, id := range evidenceIDs {
		ids = append(ids, string(id))
	}
	return conclusionDTO{
		ID:             string(conclusion.ID()),
		Strategy:       conclusion.Strategy().String(),
		Hypothesis:     conclusion.Hypothesis(),
		Confidence:     conclusion.Confidence(),
		EvidenceIDs:    ids,
		Recommendation: conclusion.Recommendation().String(),
		CreatedAt:      conclusion.CreatedAt(),
	}
}

func proposalResponse(proposal domain.RollbackProposal) proposalDTO {
	return proposalDTO{
		ActionID:        string(proposal.ActionID()),
		ProposalHash:    string(proposal.Hash()),
		CurrentRevision: string(proposal.CurrentRevision()),
		TargetRevision:  string(proposal.TargetRevision()),
		ExpiresAt:       proposal.ExpiresAt(),
	}
}

func approvalResponse(approval domain.Approval) approvalDTO {
	return approvalDTO{
		ID:              string(approval.ID()),
		Actor:           actorResponse(approval.Actor()),
		Decision:        approval.Decision().String(),
		ActionID:        string(approval.ActionID()),
		ProposalHash:    string(approval.ProposalHash()),
		DecidedAt:       approval.DecidedAt(),
		ExpiresAt:       approval.ExpiresAt(),
		RejectionReason: approval.RejectionReason(),
	}
}

func executionResponse(execution domain.RollbackExecution) executionDTO {
	return executionDTO{
		ID:             string(execution.ID()),
		IdempotencyKey: string(execution.IdempotencyKey()),
		FromRevision:   string(execution.FromRevision()),
		ToRevision:     string(execution.ToRevision()),
		Outcome:        execution.Outcome().String(),
		StartedAt:      execution.StartedAt(),
		CompletedAt:    execution.CompletedAt(),
		FailureReason:  execution.FailureReason(),
	}
}

func verificationResponse(verification domain.RecoveryVerification) verificationDTO {
	return verificationDTO{
		Passed:            verification.Passed(),
		Revision:          string(verification.Revision()),
		DesiredReplicas:   verification.DesiredReplicas(),
		ReadyReplicas:     verification.ReadyReplicas(),
		AvailableReplicas: verification.AvailableReplicas(),
		MetricValue:       verification.MetricValue(),
		CheckedAt:         verification.CheckedAt(),
		FailureReason:     verification.FailureReason(),
	}
}

type eventDTO struct {
	ID         string         `json:"id"`
	IncidentID string         `json:"incidentId"`
	Sequence   uint64         `json:"sequence"`
	Kind       string         `json:"kind"`
	OccurredAt time.Time      `json:"occurredAt"`
	Actor      actorDTO       `json:"actor"`
	Detail     map[string]any `json:"detail"`
}

func eventResponse(event domain.OperationEvent) eventDTO {
	return eventDTO{
		ID:         string(event.ID()),
		IncidentID: string(event.IncidentID()),
		Sequence:   event.Sequence(),
		Kind:       event.Kind().String(),
		OccurredAt: event.OccurredAt(),
		Actor:      actorResponse(event.Actor()),
		Detail:     detailResponse(event.Detail()),
	}
}

func detailResponse(detail domain.EventDetail) map[string]any {
	switch value := detail.(type) {
	case domain.IncidentCreatedDetail:
		return map[string]any{"target": targetResponse(value.Target)}
	case domain.StatusChangedDetail:
		return map[string]any{"from": value.From.String(), "to": value.To.String()}
	case domain.EvidenceCollectedDetail:
		return map[string]any{"evidenceId": string(value.EvidenceID), "kind": value.Kind.String()}
	case domain.DiagnosisCompletedDetail:
		return map[string]any{"conclusionId": string(value.ConclusionID), "recommendation": value.Recommendation.String()}
	case domain.PolicyEvaluatedDetail:
		return map[string]any{"decisionId": string(value.DecisionID), "allowed": value.Allowed}
	case domain.ProposalCreatedDetail:
		return map[string]any{"actionId": string(value.ActionID), "proposalHash": string(value.ProposalHash)}
	case domain.ApprovalRecordedDetail:
		return map[string]any{"approvalId": string(value.ApprovalID), "actor": actorResponse(value.Actor)}
	case domain.RejectionRecordedDetail:
		return map[string]any{"approvalId": string(value.ApprovalID), "reason": value.Reason}
	case domain.ExecutionCompletedDetail:
		return map[string]any{"executionId": string(value.ExecutionID), "outcome": value.Outcome.String()}
	case domain.VerificationCompletedDetail:
		return map[string]any{"passed": value.Passed, "revision": string(value.Revision)}
	case domain.TerminalDetail:
		return map[string]any{"reason": value.Reason}
	default:
		return map[string]any{}
	}
}
