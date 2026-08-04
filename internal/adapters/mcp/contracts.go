package mcp

import (
	"fmt"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
)

const (
	ToolGetDeployment           = "get_deployment"
	ToolGetReplicaSets          = "get_replica_sets"
	ToolListPods                = "list_pods"
	ToolGetPodEvents            = "get_pod_events"
	ToolGetRolloutStatus        = "get_rollout_status"
	ToolWatchRollout            = "watch_rollout"
	ToolPrepareRollback         = "prepare_rollback"
	ToolExecuteApprovedRollback = "execute_approved_rollback"
	ToolGetHTTPErrorRate        = "get_http_error_rate"
	ToolCompareErrorRateWindows = "compare_error_rate_windows"
	ToolGetRequestLatency       = "get_request_latency"
	ToolGetPodCPUUsage          = "get_pod_cpu_usage"
	ToolGetPodMemoryUsage       = "get_pod_memory_usage"
	ToolEvaluateRecovery        = "evaluate_recovery"
)

type ErrorCode string

const (
	CodeInvalidArgument       ErrorCode = "invalid_argument"
	CodeTargetForbidden       ErrorCode = "target_forbidden"
	CodeNotFound              ErrorCode = "not_found"
	CodeRevisionDrift         ErrorCode = "revision_drift"
	CodePolicyDenied          ErrorCode = "policy_denied"
	CodeApprovalMismatch      ErrorCode = "approval_mismatch"
	CodeExpired               ErrorCode = "expired"
	CodeIdempotencyConflict   ErrorCode = "idempotency_conflict"
	CodeDependencyUnavailable ErrorCode = "dependency_unavailable"
	CodeTimeout               ErrorCode = "timeout"
	CodeCanceled              ErrorCode = "canceled"
	CodeInternal              ErrorCode = "internal"
)

type ToolError struct {
	Code      ErrorCode `json:"code"`
	Message   string    `json:"message"`
	Retryable bool      `json:"retryable"`
}

type Response[T any] struct {
	OK    bool       `json:"ok"`
	Data  *T         `json:"data,omitempty"`
	Error *ToolError `json:"error,omitempty"`
}

func Success[T any](data T) Response[T] {
	return Response[T]{OK: true, Data: &data}
}

type Target struct {
	Cluster    string `json:"cluster" jsonschema:"cluster identifier"`
	Namespace  string `json:"namespace" jsonschema:"Kubernetes namespace"`
	Deployment string `json:"deployment" jsonschema:"Kubernetes Deployment name"`
	Service    string `json:"service" jsonschema:"service metric label"`
}

func TargetFromDomain(target domain.DeploymentTarget) Target {
	return Target{Cluster: target.Cluster(), Namespace: target.Namespace(), Deployment: target.Deployment(), Service: target.Service()}
}

func (t Target) Domain() (domain.DeploymentTarget, error) {
	target, err := domain.NewDeploymentTarget(t.Cluster, t.Namespace, t.Deployment, t.Service)
	if err != nil {
		return domain.DeploymentTarget{}, fmt.Errorf("target: %w", err)
	}
	return target, nil
}

type TargetInput struct {
	Target Target `json:"target"`
}

type DeploymentOutput struct {
	Target                 Target    `json:"target"`
	CurrentRevision        string    `json:"currentRevision"`
	PreviousStableRevision string    `json:"previousStableRevision,omitempty"`
	DesiredReplicas        int       `json:"desiredReplicas"`
	ReadyReplicas          int       `json:"readyReplicas"`
	AvailableReplicas      int       `json:"availableReplicas"`
	Healthy                bool      `json:"healthy"`
	RolloutAt              time.Time `json:"rolloutAt"`
}

type ReplicaSetOutput struct {
	Name              string    `json:"name"`
	UID               string    `json:"uid"`
	Revision          string    `json:"revision"`
	Stable            bool      `json:"stable"`
	DesiredReplicas   int       `json:"desiredReplicas"`
	ReadyReplicas     int       `json:"readyReplicas"`
	AvailableReplicas int       `json:"availableReplicas"`
	CreatedAt         time.Time `json:"createdAt"`
}

type ReplicaSetsOutput struct {
	Items []ReplicaSetOutput `json:"items"`
}

type ListInput struct {
	Target Target `json:"target"`
	Limit  int    `json:"limit"`
}

type PodOutput struct {
	Name      string    `json:"name"`
	UID       string    `json:"uid"`
	Phase     string    `json:"phase"`
	Ready     bool      `json:"ready"`
	Restarts  int32     `json:"restarts"`
	CreatedAt time.Time `json:"createdAt"`
}

type PodsOutput struct {
	Items     []PodOutput `json:"items"`
	Truncated bool        `json:"truncated"`
}

type PodEventsInput struct {
	Target  Target `json:"target"`
	PodName string `json:"podName"`
	Limit   int    `json:"limit"`
}

type EventOutput struct {
	Reason     string    `json:"reason"`
	Type       string    `json:"type"`
	Message    string    `json:"message"`
	Count      int32     `json:"count"`
	ObservedAt time.Time `json:"observedAt"`
}

type EventsOutput struct {
	Items     []EventOutput `json:"items"`
	Truncated bool          `json:"truncated"`
}

type WatchRolloutInput struct {
	Target           Target `json:"target"`
	ExpectedRevision string `json:"expectedRevision"`
	TimeoutSeconds   int    `json:"timeoutSeconds"`
}

type RolloutOutput struct {
	Revision           string `json:"revision"`
	DesiredReplicas    int    `json:"desiredReplicas"`
	UpdatedReplicas    int    `json:"updatedReplicas"`
	ReadyReplicas      int    `json:"readyReplicas"`
	AvailableReplicas  int    `json:"availableReplicas"`
	ObservedGeneration int64  `json:"observedGeneration"`
	Generation         int64  `json:"generation"`
	Complete           bool   `json:"complete"`
}

type Proposal struct {
	ActionID         string    `json:"actionId"`
	IncidentID       string    `json:"incidentId"`
	PolicyDecisionID string    `json:"policyDecisionId"`
	Target           Target    `json:"target"`
	CurrentRevision  string    `json:"currentRevision"`
	TargetRevision   string    `json:"targetRevision"`
	ExpiresAt        time.Time `json:"expiresAt"`
	Hash             string    `json:"hash"`
}

type PrepareRollbackInput struct {
	Proposal Proposal `json:"proposal"`
}

type PrepareRollbackOutput struct {
	ProposalHash   string `json:"proposalHash"`
	TargetRevision string `json:"targetRevision"`
}

type ExecuteRollbackInput struct {
	ProposalHash   string `json:"proposalHash"`
	IdempotencyKey string `json:"idempotencyKey"`
}

type ExecuteRollbackOutput struct {
	ExecutionID    string    `json:"executionId"`
	FromRevision   string    `json:"fromRevision"`
	ToRevision     string    `json:"toRevision"`
	StartedAt      time.Time `json:"startedAt"`
	CompletedAt    time.Time `json:"completedAt"`
	AlreadyApplied bool      `json:"alreadyApplied"`
}

type MetricInput struct {
	Target        Target  `json:"target"`
	WindowSeconds int     `json:"windowSeconds"`
	Threshold     float64 `json:"threshold"`
}

type LatencyInput struct {
	Target        Target  `json:"target"`
	WindowSeconds int     `json:"windowSeconds"`
	Threshold     float64 `json:"threshold"`
	Percentile    string  `json:"percentile"`
}

type CompareWindowsInput struct {
	Target                Target `json:"target"`
	CurrentWindowSeconds  int    `json:"currentWindowSeconds"`
	BaselineWindowSeconds int    `json:"baselineWindowSeconds"`
	BaselineOffsetSeconds int    `json:"baselineOffsetSeconds"`
}

type MetricOutput struct {
	Name       string    `json:"name"`
	Value      float64   `json:"value"`
	Unit       string    `json:"unit"`
	ObservedAt time.Time `json:"observedAt"`
}

type CompareWindowsOutput struct {
	Current  MetricOutput `json:"current"`
	Baseline MetricOutput `json:"baseline"`
	Delta    float64      `json:"delta"`
}

type RecoveryInput struct {
	Target            Target  `json:"target"`
	MetricKind        string  `json:"metricKind"`
	Unit              string  `json:"unit"`
	Threshold         float64 `json:"threshold"`
	ExpectedRevision  string  `json:"expectedRevision"`
	ObservedRevision  string  `json:"observedRevision"`
	DesiredReplicas   int     `json:"desiredReplicas"`
	ReadyReplicas     int     `json:"readyReplicas"`
	AvailableReplicas int     `json:"availableReplicas"`
	Healthy           bool    `json:"healthy"`
	WindowSeconds     int     `json:"windowSeconds"`
}

type RecoveryOutput struct {
	Passed  bool          `json:"passed"`
	Rollout RolloutOutput `json:"rollout"`
	Metric  MetricOutput  `json:"metric"`
}
