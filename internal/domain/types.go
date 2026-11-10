package domain

import (
	"math"
	"strings"
	"time"
)

type IncidentID string
type EvidenceID string
type ConclusionID string
type PolicyDecisionID string
type ActionID string
type ApprovalID string
type ExecutionID string
type EventID string
type ActorID string
type IdempotencyKey string
type Revision string
type ProposalHash string

func requireText(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return invalid(field, "must not be empty")
	}
	return nil
}

type ActorKind uint8

const (
	ActorHuman ActorKind = iota + 1
	ActorAgent
	ActorSystem
	ActorService
)

func (k ActorKind) String() string {
	switch k {
	case ActorHuman:
		return "human"
	case ActorAgent:
		return "agent"
	case ActorSystem:
		return "system"
	case ActorService:
		return "service"
	default:
		return unknownName
	}
}

func ParseActorKind(value string) (ActorKind, error) {
	return parseEnum("actor kind", value, ActorHuman, ActorService)
}

type Actor struct {
	id   ActorID
	kind ActorKind
}

func NewActor(id ActorID, kind ActorKind) (Actor, error) {
	if err := requireText("actor.id", string(id)); err != nil {
		return Actor{}, err
	}
	switch kind {
	case ActorHuman, ActorAgent, ActorSystem, ActorService:
	default:
		return Actor{}, invalid("actor.kind", "unknown kind")
	}
	return Actor{id: id, kind: kind}, nil
}
func (a Actor) ID() ActorID {
	return a.id
}
func (a Actor) Kind() ActorKind {
	return a.kind
}
func (a Actor) IsHuman() bool {
	return a.kind == ActorHuman
}
func (a Actor) Valid() bool {
	return a.id != "" && a.kind >= ActorHuman && a.kind <= ActorService
}

type DeploymentTarget struct{ cluster, namespace, deployment, service string }

func NewDeploymentTarget(cluster, namespace, deployment, service string) (DeploymentTarget, error) {
	for _, item := range []struct{ field, value string }{
		{"cluster", cluster},
		{"namespace", namespace},
		{"deployment", deployment},
		{"service", service},
	} {
		if err := requireText(item.field, item.value); err != nil {
			return DeploymentTarget{}, err
		}
	}
	return DeploymentTarget{cluster: cluster, namespace: namespace, deployment: deployment, service: service}, nil
}
func (t DeploymentTarget) Cluster() string {
	return t.cluster
}
func (t DeploymentTarget) Namespace() string {
	return t.namespace
}
func (t DeploymentTarget) Deployment() string {
	return t.deployment
}
func (t DeploymentTarget) Service() string {
	return t.service
}
func (t DeploymentTarget) Equal(other DeploymentTarget) bool {
	return t == other
}
func (t DeploymentTarget) Valid() bool {
	return t.cluster != "" && t.namespace != "" && t.deployment != "" && t.service != ""
}

// SymptomKind names the metric a symptom is measured by. It is a closed set:
// each member is a series the observability backend can query, so an
// unsupported kind is rejected when the symptom is built, not when the
// investigation later asks for evidence.
type SymptomKind string

const (
	SymptomHTTPErrorRate  SymptomKind = "http_error_rate"
	SymptomRequestLatency SymptomKind = "request_latency"
	SymptomPodCPUUsage    SymptomKind = "pod_cpu_usage"
	SymptomPodMemoryUsage SymptomKind = "pod_memory_usage"
)

func (k SymptomKind) String() string { return string(k) }

// Valid reports whether k is one of the supported kinds.
func (k SymptomKind) Valid() bool {
	switch k {
	case SymptomHTTPErrorRate, SymptomRequestLatency, SymptomPodCPUUsage, SymptomPodMemoryUsage:
		return true
	default:
		return false
	}
}

// ParseSymptomKind returns the supported kind spelled value.
func ParseSymptomKind(value string) (SymptomKind, error) {
	if kind := SymptomKind(value); kind.Valid() {
		return kind, nil
	}
	return "", invalid("symptom.kind", "unsupported symptom kind")
}

type Symptom struct {
	kind                SymptomKind
	observed, threshold float64
	unit                string
	startedAt           time.Time
}

func NewSymptom(kind SymptomKind, observed, threshold float64, unit string, startedAt time.Time) (Symptom, error) {
	if !kind.Valid() {
		return Symptom{}, invalid("symptom.kind", "unsupported symptom kind")
	}
	if math.IsNaN(observed) || math.IsInf(observed, 0) {
		return Symptom{}, invalid("symptom.observed", "must be finite")
	}
	if math.IsNaN(threshold) || math.IsInf(threshold, 0) || threshold <= 0 {
		return Symptom{}, invalid("symptom.threshold", "must be finite and positive")
	}
	if err := requireText("symptom.unit", unit); err != nil {
		return Symptom{}, err
	}
	if startedAt.IsZero() {
		return Symptom{}, invalid("symptom.started_at", "must not be zero")
	}
	return Symptom{kind: kind, observed: observed, threshold: threshold, unit: unit, startedAt: startedAt.UTC()}, nil
}
func (s Symptom) Kind() SymptomKind {
	return s.kind
}
func (s Symptom) Observed() float64 {
	return s.observed
}
func (s Symptom) Threshold() float64 {
	return s.threshold
}
func (s Symptom) Unit() string {
	return s.unit
}
func (s Symptom) StartedAt() time.Time {
	return s.startedAt
}
func (s Symptom) Breached() bool {
	return s.observed >= s.threshold
}
func (s Symptom) Valid() bool {
	return s.kind != "" &&
		!math.IsNaN(s.observed) &&
		!math.IsInf(s.observed, 0) &&
		!math.IsNaN(s.threshold) &&
		!math.IsInf(s.threshold, 0) &&
		s.threshold > 0 &&
		s.unit != "" &&
		!s.startedAt.IsZero()
}
