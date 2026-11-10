package a2aadapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

const (
	ExtensionURI       = "https://rollback-guardian.dev/a2a/extensions/investigation/v1"
	SchemaVersion      = "1"
	ResultArtifactName = "investigation-result"
	JSONMediaType      = "application/json"
	maxDataPartBytes   = 1 << 20
)

type targetDTO struct {
	Cluster    string `json:"cluster"`
	Namespace  string `json:"namespace"`
	Deployment string `json:"deployment"`
	Service    string `json:"service"`
}

type symptomDTO struct {
	Kind      string    `json:"kind"`
	Observed  float64   `json:"observed"`
	Threshold float64   `json:"threshold"`
	Unit      string    `json:"unit"`
	StartedAt time.Time `json:"startedAt"`
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

type requestDTO struct {
	SchemaVersion string      `json:"schemaVersion"`
	Incident      incidentDTO `json:"incident"`
}

type deploymentDTO struct {
	Target                 targetDTO `json:"target"`
	CurrentRevision        string    `json:"currentRevision"`
	PreviousStableRevision string    `json:"previousStableRevision"`
	DesiredReplicas        int       `json:"desiredReplicas"`
	ReadyReplicas          int       `json:"readyReplicas"`
	AvailableReplicas      int       `json:"availableReplicas"`
	Healthy                bool      `json:"healthy"`
	RolloutAt              time.Time `json:"rolloutAt"`
}

type metricDTO struct {
	Name       string    `json:"name"`
	Value      float64   `json:"value"`
	Threshold  float64   `json:"threshold"`
	Unit       string    `json:"unit"`
	ObservedAt time.Time `json:"observedAt"`
}

type podDTO struct {
	Pod        string    `json:"pod"`
	CrashLoop  bool      `json:"crashLoop"`
	Restarts   int       `json:"restarts"`
	ObservedAt time.Time `json:"observedAt"`
}

type kubernetesEventDTO struct {
	Reason     string    `json:"reason"`
	Message    string    `json:"message"`
	ObservedAt time.Time `json:"observedAt"`
}

type evidenceDTO struct {
	ID              string              `json:"id"`
	IncidentID      string              `json:"incidentId"`
	Kind            string              `json:"kind"`
	CollectedAt     time.Time           `json:"collectedAt"`
	Deployment      *deploymentDTO      `json:"deployment,omitempty"`
	Metric          *metricDTO          `json:"metric,omitempty"`
	Pod             *podDTO             `json:"pod,omitempty"`
	KubernetesEvent *kubernetesEventDTO `json:"kubernetesEvent,omitempty"`
}

type conclusionDTO struct {
	ID             string         `json:"id"`
	IncidentID     string         `json:"incidentId"`
	Strategy       string         `json:"strategy"`
	Hypothesis     string         `json:"hypothesis"`
	Confidence     float64        `json:"confidence"`
	EvidenceIDs    []string       `json:"evidenceIds"`
	Recommendation string         `json:"recommendation"`
	Provenance     *provenanceDTO `json:"provenance,omitempty"`
	CreatedAt      time.Time      `json:"createdAt"`
}

type provenanceDTO struct {
	RequestedStrategy string `json:"requestedStrategy"`
	Model             string `json:"model"`
	PromptVersion     string `json:"promptVersion"`
	InputTokens       int64  `json:"inputTokens"`
	OutputTokens      int64  `json:"outputTokens"`
	FallbackReason    string `json:"fallbackReason,omitempty"`
}

type resultDTO struct {
	SchemaVersion string        `json:"schemaVersion"`
	IncidentID    string        `json:"incidentId"`
	Deployment    deploymentDTO `json:"deployment"`
	Evidence      []evidenceDTO `json:"evidence"`
	Conclusion    conclusionDTO `json:"conclusion"`
}

type progressDTO struct {
	SchemaVersion string `json:"schemaVersion"`
	Stage         string `json:"stage"`
}

type correlationDTO struct {
	SchemaVersion  string `json:"schemaVersion"`
	OperationID    string `json:"operationId"`
	IdempotencyKey string `json:"idempotencyKey"`
	TraceID        string `json:"traceId"`
	RequestHash    string `json:"requestHash"`
}

func encodeRequest(incident domain.Incident) requestDTO {
	s := incident.Snapshot()
	return requestDTO{SchemaVersion: SchemaVersion, Incident: incidentDTO{
		ID: string(s.ID), Target: encodeTarget(s.Target), Symptom: encodeSymptom(s.Symptom),
		Status: s.Status.String(), Version: s.Version, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt,
	}}
}

func decodeRequest(value any) (ports.InvestigationRequest, error) {
	var wire requestDTO
	if err := strictDecode(value, &wire); err != nil {
		return ports.InvestigationRequest{}, err
	}
	if wire.SchemaVersion != SchemaVersion {
		return ports.InvestigationRequest{}, fmt.Errorf("unsupported investigation schema version %q", wire.SchemaVersion)
	}
	if wire.Incident.Status != domain.StatusInvestigating.String() {
		return ports.InvestigationRequest{}, fmt.Errorf("incident must be investigating")
	}
	target, err := domain.NewDeploymentTarget(wire.Incident.Target.Cluster, wire.Incident.Target.Namespace, wire.Incident.Target.Deployment, wire.Incident.Target.Service)
	if err != nil {
		return ports.InvestigationRequest{}, err
	}
	symptom, err := domain.NewSymptom(wire.Incident.Symptom.Kind, wire.Incident.Symptom.Observed, wire.Incident.Symptom.Threshold, wire.Incident.Symptom.Unit, wire.Incident.Symptom.StartedAt)
	if err != nil {
		return ports.InvestigationRequest{}, err
	}
	incident, err := domain.RehydrateIncident(domain.IncidentSnapshot{ID: domain.IncidentID(wire.Incident.ID), Target: target, Symptom: symptom, Status: domain.StatusInvestigating, Version: wire.Incident.Version, CreatedAt: wire.Incident.CreatedAt, UpdatedAt: wire.Incident.UpdatedAt})
	return ports.InvestigationRequest{Incident: incident}, err
}

func encodeResult(incidentID domain.IncidentID, result ports.InvestigationResult) resultDTO {
	evidence := make([]evidenceDTO, 0, len(result.Evidence))
	for _, item := range result.Evidence {
		evidence = append(evidence, encodeEvidence(item))
	}
	c := result.Conclusion.Snapshot()
	ids := make([]string, 0, len(c.EvidenceIDs))
	for _, id := range c.EvidenceIDs {
		ids = append(ids, string(id))
	}
	var provenance *provenanceDTO
	if c.Strategy != domain.StrategyRules || c.Provenance.RequestedStrategy != domain.StrategyRules {
		provenance = &provenanceDTO{
			RequestedStrategy: c.Provenance.RequestedStrategy.String(), Model: c.Provenance.Model,
			PromptVersion: c.Provenance.PromptVersion, InputTokens: c.Provenance.InputTokens,
			OutputTokens: c.Provenance.OutputTokens,
		}
		if c.Provenance.FallbackReason.Valid() {
			provenance.FallbackReason = c.Provenance.FallbackReason.String()
		}
	}
	return resultDTO{SchemaVersion: SchemaVersion, IncidentID: string(incidentID), Deployment: encodeDeployment(result.Deployment), Evidence: evidence,
		Conclusion: conclusionDTO{ID: string(c.ID), IncidentID: string(c.IncidentID), Strategy: c.Strategy.String(), Hypothesis: c.Hypothesis, Confidence: c.Confidence, EvidenceIDs: ids, Recommendation: c.Recommendation.String(), Provenance: provenance, CreatedAt: c.CreatedAt}}
}

func decodeResult(value any) (ports.InvestigationResult, error) {
	var wire resultDTO
	if err := strictDecode(value, &wire); err != nil {
		return ports.InvestigationResult{}, err
	}
	if wire.SchemaVersion != SchemaVersion {
		return ports.InvestigationResult{}, fmt.Errorf("unsupported investigation schema version %q", wire.SchemaVersion)
	}
	deployment, err := decodeDeployment(wire.Deployment)
	if err != nil {
		return ports.InvestigationResult{}, err
	}
	evidence := make([]domain.Evidence, 0, len(wire.Evidence))
	for _, item := range wire.Evidence {
		value, err := decodeEvidence(item)
		if err != nil {
			return ports.InvestigationResult{}, err
		}
		if value.IncidentID() != domain.IncidentID(wire.IncidentID) {
			return ports.InvestigationResult{}, fmt.Errorf("evidence incident id does not match result")
		}
		evidence = append(evidence, value)
	}
	strategy, err := domain.ParseDiagnosisStrategy(wire.Conclusion.Strategy)
	if err != nil {
		return ports.InvestigationResult{}, err
	}
	recommendation := domain.Recommendation(0)
	if wire.Conclusion.Recommendation == domain.RecommendRollback.String() {
		recommendation = domain.RecommendRollback
	} else if wire.Conclusion.Recommendation == domain.RecommendNoAction.String() {
		recommendation = domain.RecommendNoAction
	}
	ids := make([]domain.EvidenceID, 0, len(wire.Conclusion.EvidenceIDs))
	for _, id := range wire.Conclusion.EvidenceIDs {
		ids = append(ids, domain.EvidenceID(id))
	}
	var conclusion domain.InvestigationConclusion
	if wire.Conclusion.Provenance == nil {
		if strategy != domain.StrategyRules {
			return ports.InvestigationResult{}, fmt.Errorf("model diagnosis provenance is required")
		}
		conclusion, err = domain.NewInvestigationConclusion(domain.ConclusionID(wire.Conclusion.ID), domain.IncidentID(wire.Conclusion.IncidentID), strategy, wire.Conclusion.Hypothesis, wire.Conclusion.Confidence, ids, recommendation, wire.Conclusion.CreatedAt)
	} else {
		requestedStrategy, strategyErr := domain.ParseDiagnosisStrategy(wire.Conclusion.Provenance.RequestedStrategy)
		if strategyErr != nil {
			return ports.InvestigationResult{}, strategyErr
		}
		fallbackReason, fallbackErr := decodeFallbackReason(wire.Conclusion.Provenance.FallbackReason)
		if fallbackErr != nil {
			return ports.InvestigationResult{}, fallbackErr
		}
		conclusion, err = domain.NewInvestigationConclusionWithProvenance(
			domain.ConclusionID(wire.Conclusion.ID), domain.IncidentID(wire.Conclusion.IncidentID), strategy,
			wire.Conclusion.Hypothesis, wire.Conclusion.Confidence, ids, recommendation,
			domain.DiagnosisProvenance{
				RequestedStrategy: requestedStrategy, Model: wire.Conclusion.Provenance.Model,
				PromptVersion: wire.Conclusion.Provenance.PromptVersion, InputTokens: wire.Conclusion.Provenance.InputTokens,
				OutputTokens: wire.Conclusion.Provenance.OutputTokens, FallbackReason: fallbackReason,
			}, wire.Conclusion.CreatedAt,
		)
	}
	if err != nil {
		return ports.InvestigationResult{}, err
	}
	if wire.IncidentID != string(conclusion.IncidentID()) {
		return ports.InvestigationResult{}, fmt.Errorf("result incident id does not match conclusion")
	}
	return ports.InvestigationResult{Deployment: deployment, Evidence: evidence, Conclusion: conclusion}, nil
}

// decodeFallbackReason maps the absent wire value (no fallback happened) to the
// zero reason; every other value must be a canonical domain spelling.
func decodeFallbackReason(value string) (domain.DiagnosisFallbackReason, error) {
	if value == "" {
		return 0, nil
	}
	return domain.ParseDiagnosisFallbackReason(value)
}

func encodeTarget(v domain.DeploymentTarget) targetDTO {
	return targetDTO{v.Cluster(), v.Namespace(), v.Deployment(), v.Service()}
}
func encodeSymptom(v domain.Symptom) symptomDTO {
	return symptomDTO{v.Kind(), v.Observed(), v.Threshold(), v.Unit(), v.StartedAt()}
}
func encodeDeployment(v domain.DeploymentEvidence) deploymentDTO {
	return deploymentDTO{encodeTarget(v.Target), string(v.CurrentRevision), string(v.PreviousStableRevision), v.DesiredReplicas, v.ReadyReplicas, v.AvailableReplicas, v.Healthy, v.RolloutAt}
}
func decodeDeployment(v deploymentDTO) (domain.DeploymentEvidence, error) {
	target, err := domain.NewDeploymentTarget(v.Target.Cluster, v.Target.Namespace, v.Target.Deployment, v.Target.Service)
	if err != nil {
		return domain.DeploymentEvidence{}, err
	}
	return domain.DeploymentEvidence{Target: target, CurrentRevision: domain.Revision(v.CurrentRevision), PreviousStableRevision: domain.Revision(v.PreviousStableRevision), DesiredReplicas: v.DesiredReplicas, ReadyReplicas: v.ReadyReplicas, AvailableReplicas: v.AvailableReplicas, Healthy: v.Healthy, RolloutAt: v.RolloutAt}, nil
}

func encodeEvidence(v domain.Evidence) evidenceDTO {
	s := v.Snapshot()
	out := evidenceDTO{ID: string(s.ID), IncidentID: string(s.IncidentID), Kind: s.Kind.String(), CollectedAt: s.CollectedAt}
	if s.Deployment != nil {
		x := encodeDeployment(*s.Deployment)
		out.Deployment = &x
	}
	if s.Metric != nil {
		x := metricDTO{s.Metric.Name, s.Metric.Value, s.Metric.Threshold, s.Metric.Unit, s.Metric.ObservedAt}
		out.Metric = &x
	}
	if s.Pod != nil {
		x := podDTO{s.Pod.Pod, s.Pod.CrashLoop, s.Pod.Restarts, s.Pod.ObservedAt}
		out.Pod = &x
	}
	if s.KubernetesEvent != nil {
		x := kubernetesEventDTO{s.KubernetesEvent.Reason, s.KubernetesEvent.Message, s.KubernetesEvent.ObservedAt}
		out.KubernetesEvent = &x
	}
	return out
}

func decodeEvidence(v evidenceDTO) (domain.Evidence, error) {
	id, incidentID := domain.EvidenceID(v.ID), domain.IncidentID(v.IncidentID)
	switch v.Kind {
	case domain.EvidenceDeployment.String():
		if v.Deployment == nil {
			return domain.Evidence{}, fmt.Errorf("deployment evidence payload is required")
		}
		x, err := decodeDeployment(*v.Deployment)
		if err != nil {
			return domain.Evidence{}, err
		}
		return domain.NewDeploymentEvidence(id, incidentID, x, v.CollectedAt)
	case domain.EvidenceMetric.String():
		if v.Metric == nil {
			return domain.Evidence{}, fmt.Errorf("metric evidence payload is required")
		}
		return domain.NewMetricEvidence(id, incidentID, domain.MetricEvidence{Name: v.Metric.Name, Value: v.Metric.Value, Threshold: v.Metric.Threshold, Unit: v.Metric.Unit, ObservedAt: v.Metric.ObservedAt}, v.CollectedAt)
	case domain.EvidencePod.String():
		if v.Pod == nil {
			return domain.Evidence{}, fmt.Errorf("pod evidence payload is required")
		}
		return domain.NewPodEvidence(id, incidentID, domain.PodEvidence{Pod: v.Pod.Pod, CrashLoop: v.Pod.CrashLoop, Restarts: v.Pod.Restarts, ObservedAt: v.Pod.ObservedAt}, v.CollectedAt)
	case domain.EvidenceKubernetesEvent.String():
		if v.KubernetesEvent == nil {
			return domain.Evidence{}, fmt.Errorf("kubernetes event evidence payload is required")
		}
		return domain.NewKubernetesEventEvidence(id, incidentID, domain.KubernetesEventEvidence{Reason: v.KubernetesEvent.Reason, Message: v.KubernetesEvent.Message, ObservedAt: v.KubernetesEvent.ObservedAt}, v.CollectedAt)
	default:
		return domain.Evidence{}, fmt.Errorf("unknown evidence kind %q", v.Kind)
	}
}

func strictDecode(value any, target any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode DataPart: %w", err)
	}
	if len(data) > maxDataPartBytes {
		return fmt.Errorf("DataPart exceeds %d bytes", maxDataPartBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode DataPart: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("DataPart contains trailing data")
	}
	return nil
}

func dataMap(value any) (map[string]any, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result, nil
}
