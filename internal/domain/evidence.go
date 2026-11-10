package domain

import (
	"math"
	"strings"
	"time"
)

type EvidenceKind uint8

const (
	EvidenceDeployment EvidenceKind = iota + 1
	EvidenceMetric
	EvidencePod
	EvidenceKubernetesEvent
)

func (k EvidenceKind) String() string {
	names := [...]string{unknownName, "deployment", "metric", "pod", "kubernetes_event"}
	if int(k) >= len(names) {
		return names[0]
	}
	return names[k]
}

func ParseEvidenceKind(value string) (EvidenceKind, error) {
	return parseEnum("evidence kind", value, EvidenceDeployment, EvidenceKubernetesEvent)
}

type DeploymentEvidence struct {
	Target                                            DeploymentTarget
	CurrentRevision                                   Revision
	PreviousStableRevision                            Revision
	DesiredReplicas, ReadyReplicas, AvailableReplicas int
	Healthy                                           bool
	RolloutAt                                         time.Time
}

type MetricEvidence struct {
	Name             string
	Value, Threshold float64
	Unit             string
	ObservedAt       time.Time
}
type PodEvidence struct {
	Pod        string
	CrashLoop  bool
	Restarts   int
	ObservedAt time.Time
}
type KubernetesEventEvidence struct {
	Reason, Message string
	ObservedAt      time.Time
}

type Evidence struct {
	id              EvidenceID
	incidentID      IncidentID
	kind            EvidenceKind
	collectedAt     time.Time
	deployment      *DeploymentEvidence
	metric          *MetricEvidence
	pod             *PodEvidence
	kubernetesEvent *KubernetesEventEvidence
}

func NewDeploymentEvidence(id EvidenceID, incidentID IncidentID, value DeploymentEvidence, collectedAt time.Time) (Evidence, error) {
	if err := validateEvidenceBase(id, incidentID, collectedAt); err != nil {
		return Evidence{}, err
	}
	if value.CurrentRevision == "" {
		return Evidence{}, invalid("deployment.current_revision", "must not be empty")
	}
	if !value.Target.Valid() {
		return Evidence{}, invalid("deployment.target", "must be valid")
	}
	if value.RolloutAt.IsZero() {
		return Evidence{}, invalid("deployment.rollout_at", "must not be zero")
	}
	if value.DesiredReplicas < 0 || value.ReadyReplicas < 0 || value.AvailableReplicas < 0 {
		return Evidence{}, invalid("deployment.replicas", "must not be negative")
	}
	copyValue := value
	return Evidence{id: id, incidentID: incidentID, kind: EvidenceDeployment, collectedAt: collectedAt.UTC(), deployment: &copyValue}, nil
}
func NewMetricEvidence(id EvidenceID, incidentID IncidentID, value MetricEvidence, collectedAt time.Time) (Evidence, error) {
	if err := validateEvidenceBase(id, incidentID, collectedAt); err != nil {
		return Evidence{}, err
	}
	if value.Name == "" || value.Unit == "" || value.ObservedAt.IsZero() {
		return Evidence{}, invalid("metric", "name, unit, and observed time are required")
	}
	if math.IsNaN(value.Value) || math.IsInf(value.Value, 0) || math.IsNaN(value.Threshold) || math.IsInf(value.Threshold, 0) || value.Threshold <= 0 {
		return Evidence{}, invalid("metric.value", "value must be finite and threshold must be finite and positive")
	}
	copyValue := value
	return Evidence{id: id, incidentID: incidentID, kind: EvidenceMetric, collectedAt: collectedAt.UTC(), metric: &copyValue}, nil
}
func NewPodEvidence(id EvidenceID, incidentID IncidentID, value PodEvidence, collectedAt time.Time) (Evidence, error) {
	if err := validateEvidenceBase(id, incidentID, collectedAt); err != nil {
		return Evidence{}, err
	}
	if value.Pod == "" || value.ObservedAt.IsZero() || value.Restarts < 0 {
		return Evidence{}, invalid("pod", "invalid pod evidence")
	}
	copyValue := value
	return Evidence{id: id, incidentID: incidentID, kind: EvidencePod, collectedAt: collectedAt.UTC(), pod: &copyValue}, nil
}
func NewKubernetesEventEvidence(id EvidenceID, incidentID IncidentID, value KubernetesEventEvidence, collectedAt time.Time) (Evidence, error) {
	if err := validateEvidenceBase(id, incidentID, collectedAt); err != nil {
		return Evidence{}, err
	}
	if value.Reason == "" || value.ObservedAt.IsZero() {
		return Evidence{}, invalid("kubernetes_event", "reason and observed time are required")
	}
	copyValue := value
	return Evidence{id: id, incidentID: incidentID, kind: EvidenceKubernetesEvent, collectedAt: collectedAt.UTC(), kubernetesEvent: &copyValue}, nil
}
func validateEvidenceBase(id EvidenceID, incidentID IncidentID, collectedAt time.Time) error {
	if err := requireText("evidence.id", string(id)); err != nil {
		return err
	}
	if err := requireText("evidence.incident_id", string(incidentID)); err != nil {
		return err
	}
	if collectedAt.IsZero() {
		return invalid("evidence.collected_at", "must not be zero")
	}
	return nil
}
func (e Evidence) ID() EvidenceID {
	return e.id
}
func (e Evidence) IncidentID() IncidentID {
	return e.incidentID
}
func (e Evidence) Kind() EvidenceKind {
	return e.kind
}
func (e Evidence) CollectedAt() time.Time {
	return e.collectedAt
}
func (e Evidence) Deployment() (DeploymentEvidence, bool) {
	if e.deployment == nil {
		return DeploymentEvidence{}, false
	}
	return *e.deployment, true
}
func (e Evidence) Metric() (MetricEvidence, bool) {
	if e.metric == nil {
		return MetricEvidence{}, false
	}
	return *e.metric, true
}
func (e Evidence) Pod() (PodEvidence, bool) {
	if e.pod == nil {
		return PodEvidence{}, false
	}
	return *e.pod, true
}
func (e Evidence) KubernetesEvent() (KubernetesEventEvidence, bool) {
	if e.kubernetesEvent == nil {
		return KubernetesEventEvidence{}, false
	}
	return *e.kubernetesEvent, true
}

type Recommendation uint8

const (
	RecommendRollback Recommendation = iota + 1
	RecommendNoAction
)

func (r Recommendation) String() string {
	if r == RecommendRollback {
		return "rollback"
	}
	if r == RecommendNoAction {
		return "no_action"
	}
	return unknownName
}

func ParseRecommendation(value string) (Recommendation, error) {
	return parseEnum("recommendation", value, RecommendRollback, RecommendNoAction)
}

type DiagnosisStrategy uint8

const (
	StrategyRules DiagnosisStrategy = iota + 1
	StrategyLLM
	StrategyHybrid
)

func (s DiagnosisStrategy) String() string {
	names := [...]string{unknownName, "rules", "llm", "hybrid"}
	if int(s) >= len(names) {
		return names[0]
	}
	return names[s]
}

// ParseDiagnosisStrategy accepts the canonical spelling ("rules", "llm",
// "hybrid"), which is also what is stored and sent on the wire.
func ParseDiagnosisStrategy(value string) (DiagnosisStrategy, error) {
	return parseEnum("diagnosis strategy", value, StrategyRules, StrategyHybrid)
}

func (s DiagnosisStrategy) Valid() bool {
	return s >= StrategyRules && s <= StrategyHybrid
}

type DiagnosisFallbackReason uint8

const (
	FallbackProviderError DiagnosisFallbackReason = iota + 1
	FallbackProviderTimeout
	FallbackProviderRefusal
	FallbackInvalidOutput
)

func (r DiagnosisFallbackReason) String() string {
	names := [...]string{"", "provider_error", "provider_timeout", "provider_refusal", "invalid_output"}
	if int(r) >= len(names) {
		return unknownName
	}
	return names[r]
}

func ParseDiagnosisFallbackReason(value string) (DiagnosisFallbackReason, error) {
	return parseEnum("diagnosis fallback reason", value, FallbackProviderError, FallbackInvalidOutput)
}

func (r DiagnosisFallbackReason) Valid() bool {
	return r >= FallbackProviderError && r <= FallbackInvalidOutput
}

type DiagnosisProvenance struct {
	RequestedStrategy DiagnosisStrategy
	Model             string
	PromptVersion     string
	InputTokens       int64
	OutputTokens      int64
	FallbackReason    DiagnosisFallbackReason
}

type InvestigationConclusion struct {
	id             ConclusionID
	incidentID     IncidentID
	strategy       DiagnosisStrategy
	hypothesis     string
	confidence     float64
	evidenceIDs    []EvidenceID
	recommendation Recommendation
	provenance     DiagnosisProvenance
	createdAt      time.Time
}

func NewInvestigationConclusion(id ConclusionID, incidentID IncidentID, strategy DiagnosisStrategy, hypothesis string, confidence float64, evidenceIDs []EvidenceID, recommendation Recommendation, now time.Time) (InvestigationConclusion, error) {
	return NewInvestigationConclusionWithProvenance(
		id,
		incidentID,
		strategy,
		hypothesis,
		confidence,
		evidenceIDs,
		recommendation,
		DiagnosisProvenance{RequestedStrategy: strategy},
		now,
	)
}

func NewInvestigationConclusionWithProvenance(id ConclusionID, incidentID IncidentID, strategy DiagnosisStrategy, hypothesis string, confidence float64, evidenceIDs []EvidenceID, recommendation Recommendation, provenance DiagnosisProvenance, now time.Time) (InvestigationConclusion, error) {
	if id == "" || incidentID == "" || !strategy.Valid() || hypothesis == "" || now.IsZero() {
		return InvestigationConclusion{}, invalid("conclusion", "required value is empty")
	}
	if math.IsNaN(confidence) || math.IsInf(confidence, 0) || confidence < 0 || confidence > 1 {
		return InvestigationConclusion{}, invalid("conclusion.confidence", "must be between zero and one")
	}
	if len(evidenceIDs) == 0 && recommendation == RecommendRollback {
		return InvestigationConclusion{}, invalid("conclusion.evidence_ids", "must not be empty")
	}
	seen := map[EvidenceID]bool{}
	ids := append([]EvidenceID(nil), evidenceIDs...)
	for _, evidenceID := range ids {
		if evidenceID == "" || seen[evidenceID] {
			return InvestigationConclusion{}, invalid("conclusion.evidence_ids", "must be non-empty and unique")
		}
		seen[evidenceID] = true
	}
	if recommendation != RecommendRollback && recommendation != RecommendNoAction {
		return InvestigationConclusion{}, invalid("conclusion.recommendation", "unknown recommendation")
	}
	if err := validateDiagnosisProvenance(strategy, provenance); err != nil {
		return InvestigationConclusion{}, err
	}
	return InvestigationConclusion{id: id, incidentID: incidentID, strategy: strategy, hypothesis: hypothesis, confidence: confidence, evidenceIDs: ids, recommendation: recommendation, provenance: provenance, createdAt: now.UTC()}, nil
}

func validateDiagnosisProvenance(effective DiagnosisStrategy, provenance DiagnosisProvenance) error {
	if !provenance.RequestedStrategy.Valid() {
		return invalid("conclusion.provenance.requested_strategy", "must be valid")
	}
	if provenance.InputTokens < 0 || provenance.OutputTokens < 0 {
		return invalid("conclusion.provenance.tokens", "must not be negative")
	}
	model := strings.TrimSpace(provenance.Model)
	promptVersion := strings.TrimSpace(provenance.PromptVersion)
	if effective == StrategyRules && provenance.RequestedStrategy == StrategyRules {
		if provenance.Model != "" || provenance.PromptVersion != "" || provenance.InputTokens != 0 || provenance.OutputTokens != 0 || provenance.FallbackReason != 0 {
			return invalid("conclusion.provenance", "rule diagnosis must not contain model metadata")
		}
		return nil
	}
	if effective == StrategyRules {
		if provenance.RequestedStrategy == StrategyRules || model == "" || promptVersion == "" || !provenance.FallbackReason.Valid() {
			return invalid("conclusion.provenance", "model fallback metadata is incomplete")
		}
		return nil
	}
	if provenance.RequestedStrategy != effective || model == "" || promptVersion == "" || provenance.FallbackReason != 0 {
		return invalid("conclusion.provenance", "successful model metadata is inconsistent")
	}
	return nil
}
func (c InvestigationConclusion) ID() ConclusionID {
	return c.id
}
func (c InvestigationConclusion) IncidentID() IncidentID {
	return c.incidentID
}
func (c InvestigationConclusion) Strategy() DiagnosisStrategy {
	return c.strategy
}
func (c InvestigationConclusion) Hypothesis() string {
	return c.hypothesis
}
func (c InvestigationConclusion) Confidence() float64 {
	return c.confidence
}
func (c InvestigationConclusion) EvidenceIDs() []EvidenceID {
	return append([]EvidenceID(nil), c.evidenceIDs...)
}
func (c InvestigationConclusion) Recommendation() Recommendation {
	return c.recommendation
}
func (c InvestigationConclusion) Provenance() DiagnosisProvenance {
	return c.provenance
}
func (c InvestigationConclusion) CreatedAt() time.Time {
	return c.createdAt
}
