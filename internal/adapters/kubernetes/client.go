package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

var (
	ErrTargetForbidden = errors.New("deployment target is not allowed")
	ErrStableRevision  = errors.New("stable rollback revision is invalid")
	ErrRevisionDrift   = errors.New("deployment revision drifted")
)

type Config struct {
	ClusterID      string
	AllowedTargets []domain.DeploymentTarget
	Now            func() time.Time
}

type Service struct {
	client  kubernetes.Interface
	cluster string
	allowed map[domain.DeploymentTarget]struct{}
	now     func() time.Time
	mu      sync.Mutex
	results map[domain.IdempotencyKey]mutationRecord
}

type mutationRecord struct {
	hash   domain.ProposalHash
	result MutationResult
}

type ReplicaSetSummary struct {
	Name              string
	UID               string
	Revision          domain.Revision
	Stable            bool
	DesiredReplicas   int
	ReadyReplicas     int
	AvailableReplicas int
	CreatedAt         time.Time
}

type MutationResult struct {
	ExecutionID    domain.ExecutionID
	FromRevision   domain.Revision
	ToRevision     domain.Revision
	StartedAt      time.Time
	CompletedAt    time.Time
	AlreadyApplied bool
}

type PodSummary struct {
	Name      string
	UID       string
	Phase     string
	Ready     bool
	Restarts  int32
	CreatedAt time.Time
}

type EventSummary struct {
	Reason     string
	Type       string
	Message    string
	Count      int32
	ObservedAt time.Time
}

type RolloutStatus struct {
	Revision           domain.Revision
	DesiredReplicas    int
	UpdatedReplicas    int
	ReadyReplicas      int
	AvailableReplicas  int
	ObservedGeneration int64
	Generation         int64
	Complete           bool
}

func NewService(client kubernetes.Interface, config Config) (*Service, error) {
	if client == nil || strings.TrimSpace(config.ClusterID) == "" || len(config.AllowedTargets) == 0 {
		return nil, fmt.Errorf("invalid kubernetes service configuration")
	}
	allowed := make(map[domain.DeploymentTarget]struct{}, len(config.AllowedTargets))
	for _, target := range config.AllowedTargets {
		if !target.Valid() || target.Cluster() != config.ClusterID {
			return nil, fmt.Errorf("invalid allowed deployment target")
		}
		allowed[target] = struct{}{}
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &Service{client: client, cluster: config.ClusterID, allowed: allowed, now: now, results: map[domain.IdempotencyKey]mutationRecord{}}, nil
}

func (s *Service) InspectDeployment(ctx context.Context, target domain.DeploymentTarget) (domain.DeploymentEvidence, error) {
	deployment, replicaSets, err := s.loadDeployment(ctx, target)
	if err != nil {
		return domain.DeploymentEvidence{}, err
	}
	currentRevision, err := TemplateRevision(deployment.Spec.Template)
	if err != nil {
		return domain.DeploymentEvidence{}, fmt.Errorf("digest current deployment template: %w", err)
	}
	var current *appsv1.ReplicaSet
	var stableRevision domain.Revision
	stableCount := 0
	for index := range replicaSets {
		revision, digestErr := TemplateRevision(replicaSets[index].Spec.Template)
		if digestErr != nil {
			return domain.DeploymentEvidence{}, fmt.Errorf("digest replica set template: %w", digestErr)
		}
		if revision == currentRevision {
			copyValue := replicaSets[index]
			current = &copyValue
		}
		if replicaSets[index].Annotations[StableRevisionAnnotation] == "true" {
			stableCount++
			stableRevision = revision
		}
	}
	if current == nil {
		return domain.DeploymentEvidence{}, fmt.Errorf("current replica set: %w", domain.ErrNotFound)
	}
	if stableCount > 1 {
		return domain.DeploymentEvidence{}, fmt.Errorf("multiple stable replica sets: %w", ErrStableRevision)
	}
	desired := desiredReplicas(deployment)
	return domain.DeploymentEvidence{
		Target:                 target,
		CurrentRevision:        currentRevision,
		PreviousStableRevision: stableRevision,
		DesiredReplicas:        desired,
		ReadyReplicas:          int(deployment.Status.ReadyReplicas),
		AvailableReplicas:      int(deployment.Status.AvailableReplicas),
		Healthy:                rolloutComplete(deployment),
		RolloutAt:              current.CreationTimestamp.UTC(),
	}, nil
}

func (s *Service) ReplicaSets(ctx context.Context, target domain.DeploymentTarget) ([]ReplicaSetSummary, error) {
	_, replicaSets, err := s.loadDeployment(ctx, target)
	if err != nil {
		return nil, err
	}
	result := make([]ReplicaSetSummary, 0, len(replicaSets))
	for _, replicaSet := range replicaSets {
		revision, digestErr := TemplateRevision(replicaSet.Spec.Template)
		if digestErr != nil {
			return nil, digestErr
		}
		result = append(result, ReplicaSetSummary{Name: replicaSet.Name, UID: string(replicaSet.UID), Revision: revision, Stable: replicaSet.Annotations[StableRevisionAnnotation] == "true", DesiredReplicas: int(replicaSet.Status.Replicas), ReadyReplicas: int(replicaSet.Status.ReadyReplicas), AvailableReplicas: int(replicaSet.Status.AvailableReplicas), CreatedAt: replicaSet.CreationTimestamp.UTC()})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].Name < result[j].Name
		}
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return result, nil
}

func (s *Service) ListPods(ctx context.Context, target domain.DeploymentTarget, limit int) ([]PodSummary, bool, error) {
	deployment, replicaSets, err := s.loadDeployment(ctx, target)
	if err != nil {
		return nil, false, err
	}
	if limit <= 0 || limit > 100 {
		return nil, false, &domain.ValidationError{Field: "limit", Reason: "must be between 1 and 100"}
	}
	owned := make(map[types.UID]struct{}, len(replicaSets))
	for _, replicaSet := range replicaSets {
		owned[replicaSet.UID] = struct{}{}
	}
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil {
		return nil, false, err
	}
	pods, err := s.client.CoreV1().Pods(target.Namespace()).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return nil, false, fmt.Errorf("list deployment pods: %w", err)
	}
	result := make([]PodSummary, 0, len(pods.Items))
	for _, pod := range pods.Items {
		owner := metav1.GetControllerOf(&pod)
		if owner == nil {
			continue
		}
		if _, ok := owned[owner.UID]; !ok || owner.Kind != "ReplicaSet" {
			continue
		}
		ready := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		var restarts int32
		for _, status := range pod.Status.ContainerStatuses {
			restarts += status.RestartCount
		}
		result = append(result, PodSummary{Name: pod.Name, UID: string(pod.UID), Phase: string(pod.Status.Phase), Ready: ready, Restarts: restarts, CreatedAt: pod.CreationTimestamp.UTC()})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	truncated := len(result) > limit
	if truncated {
		result = result[:limit]
	}
	return result, truncated, nil
}

func (s *Service) PodEvents(ctx context.Context, target domain.DeploymentTarget, podName string, limit int) ([]EventSummary, bool, error) {
	if strings.TrimSpace(podName) == "" || limit <= 0 || limit > 100 {
		return nil, false, &domain.ValidationError{Field: "pod_events", Reason: "invalid pod name or limit"}
	}
	pods, _, err := s.ListPods(ctx, target, 100)
	if err != nil {
		return nil, false, err
	}
	var podUID string
	for _, pod := range pods {
		if pod.Name == podName {
			podUID = pod.UID
			break
		}
	}
	if podUID == "" {
		return nil, false, &domain.NotFoundError{Resource: "deployment pod", ID: podName}
	}
	events, err := s.client.CoreV1().Events(target.Namespace()).List(ctx, metav1.ListOptions{FieldSelector: fields.OneTermEqualSelector("involvedObject.uid", podUID).String()})
	if err != nil {
		return nil, false, fmt.Errorf("list pod events: %w", err)
	}
	result := make([]EventSummary, 0, len(events.Items))
	for _, event := range events.Items {
		observedAt := event.EventTime.Time
		if observedAt.IsZero() {
			observedAt = event.LastTimestamp.Time
		}
		result = append(result, EventSummary{Reason: event.Reason, Type: event.Type, Message: event.Message, Count: event.Count, ObservedAt: observedAt.UTC()})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ObservedAt.Before(result[j].ObservedAt) })
	truncated := len(result) > limit
	if truncated {
		result = result[len(result)-limit:]
	}
	return result, truncated, nil
}

func (s *Service) GetRolloutStatus(ctx context.Context, target domain.DeploymentTarget) (RolloutStatus, error) {
	deployment, _, err := s.loadDeployment(ctx, target)
	if err != nil {
		return RolloutStatus{}, err
	}
	revision, err := TemplateRevision(deployment.Spec.Template)
	if err != nil {
		return RolloutStatus{}, err
	}
	return RolloutStatus{Revision: revision, DesiredReplicas: desiredReplicas(deployment), UpdatedReplicas: int(deployment.Status.UpdatedReplicas), ReadyReplicas: int(deployment.Status.ReadyReplicas), AvailableReplicas: int(deployment.Status.AvailableReplicas), ObservedGeneration: deployment.Status.ObservedGeneration, Generation: deployment.Generation, Complete: rolloutComplete(deployment)}, nil
}

func (s *Service) WatchRollout(ctx context.Context, target domain.DeploymentTarget, expected domain.Revision) (RolloutStatus, error) {
	if expected == "" {
		return RolloutStatus{}, &domain.ValidationError{Field: "expected_revision", Reason: "must not be empty"}
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := s.GetRolloutStatus(ctx, target)
		if err != nil {
			return RolloutStatus{}, err
		}
		if status.Revision != expected {
			return RolloutStatus{}, ErrRevisionDrift
		}
		if status.Complete {
			return status, nil
		}
		select {
		case <-ctx.Done():
			return RolloutStatus{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Service) PrepareRollback(ctx context.Context, proposal domain.RollbackProposal) error {
	evidence, err := s.InspectDeployment(ctx, proposal.Target())
	if err != nil {
		return err
	}
	if evidence.CurrentRevision != proposal.CurrentRevision() || evidence.PreviousStableRevision != proposal.TargetRevision() || evidence.CurrentRevision == evidence.PreviousStableRevision {
		return ErrRevisionDrift
	}
	if proposal.Expired(s.now()) {
		return &domain.ExpiryError{Resource: "proposal"}
	}
	return nil
}

func (s *Service) ExecuteApprovedRollback(ctx context.Context, approved domain.ApprovedRollback, key domain.IdempotencyKey) (MutationResult, error) {
	if key == "" {
		return MutationResult{}, &domain.ValidationError{Field: "idempotency_key", Reason: "must not be empty"}
	}
	proposal := approved.Proposal()
	s.mu.Lock()
	defer s.mu.Unlock()
	if record, exists := s.results[key]; exists {
		if record.hash != proposal.Hash() {
			return MutationResult{}, &domain.IdempotencyConflictError{Key: key}
		}
		return record.result, nil
	}

	deployment, replicaSets, err := s.loadDeployment(ctx, proposal.Target())
	if err != nil {
		return MutationResult{}, err
	}
	currentRevision, err := TemplateRevision(deployment.Spec.Template)
	if err != nil {
		return MutationResult{}, err
	}
	if currentRevision == proposal.TargetRevision() {
		result := newMutationResult(key, proposal, s.now(), true)
		s.results[key] = mutationRecord{hash: proposal.Hash(), result: result}
		return result, nil
	}
	if currentRevision != proposal.CurrentRevision() {
		return MutationResult{}, ErrRevisionDrift
	}
	stable, err := stableReplicaSet(replicaSets, proposal.TargetRevision())
	if err != nil {
		return MutationResult{}, err
	}
	fresh := domain.DeploymentEvidence{Target: proposal.Target(), CurrentRevision: currentRevision}
	if _, err := domain.AuthorizeRollback(proposal, approved.PolicyDecision(), approved.Approval(), fresh, s.now()); err != nil {
		return MutationResult{}, err
	}
	patch, err := rollbackPatch(deployment.ResourceVersion, stable.Spec.Template)
	if err != nil {
		return MutationResult{}, err
	}
	startedAt := s.now().UTC()
	if _, err := s.client.AppsV1().Deployments(proposal.Target().Namespace()).Patch(ctx, proposal.Target().Deployment(), types.StrategicMergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return MutationResult{}, fmt.Errorf("patch approved deployment template: %w", err)
	}
	result := newMutationResult(key, proposal, startedAt, false)
	s.results[key] = mutationRecord{hash: proposal.Hash(), result: result}
	return result, nil
}

func (s *Service) loadDeployment(ctx context.Context, target domain.DeploymentTarget) (*appsv1.Deployment, []appsv1.ReplicaSet, error) {
	if _, ok := s.allowed[target]; !ok || target.Cluster() != s.cluster {
		return nil, nil, ErrTargetForbidden
	}
	deployment, err := s.client.AppsV1().Deployments(target.Namespace()).Get(ctx, target.Deployment(), metav1.GetOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("get deployment: %w", err)
	}
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil {
		return nil, nil, fmt.Errorf("deployment selector: %w", err)
	}
	list, err := s.client.AppsV1().ReplicaSets(target.Namespace()).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return nil, nil, fmt.Errorf("list replica sets: %w", err)
	}
	owned := make([]appsv1.ReplicaSet, 0, len(list.Items))
	for _, replicaSet := range list.Items {
		owner := metav1.GetControllerOf(&replicaSet)
		if owner != nil && owner.APIVersion == "apps/v1" && owner.Kind == "Deployment" && owner.Name == deployment.Name && owner.UID == deployment.UID {
			owned = append(owned, replicaSet)
		}
	}
	return deployment, owned, nil
}

func stableReplicaSet(replicaSets []appsv1.ReplicaSet, wanted domain.Revision) (*appsv1.ReplicaSet, error) {
	var result *appsv1.ReplicaSet
	for index := range replicaSets {
		if replicaSets[index].Annotations[StableRevisionAnnotation] != "true" {
			continue
		}
		revision, err := TemplateRevision(replicaSets[index].Spec.Template)
		if err != nil {
			return nil, err
		}
		if revision != wanted {
			continue
		}
		if result != nil {
			return nil, ErrStableRevision
		}
		result = &replicaSets[index]
	}
	if result == nil {
		return nil, ErrStableRevision
	}
	return result, nil
}

func rollbackPatch(resourceVersion string, template corev1.PodTemplateSpec) ([]byte, error) {
	normalized := normalizeTemplate(template)
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return nil, err
	}
	var templatePatch map[string]any
	if err := json.Unmarshal(encoded, &templatePatch); err != nil {
		return nil, err
	}
	templatePatch["$patch"] = "replace"
	return json.Marshal(map[string]any{
		"metadata": map[string]any{"resourceVersion": resourceVersion},
		"spec":     map[string]any{"template": templatePatch},
	})
}

func containsReplaceDirective(patch []byte) bool {
	return strings.Contains(string(patch), `"$patch":"replace"`)
}

func desiredReplicas(deployment *appsv1.Deployment) int {
	if deployment.Spec.Replicas == nil {
		return 1
	}
	return int(*deployment.Spec.Replicas)
}

func rolloutComplete(deployment *appsv1.Deployment) bool {
	desired := int32(desiredReplicas(deployment))
	if deployment.Spec.Paused || deployment.Status.ObservedGeneration < deployment.Generation || deployment.Status.UpdatedReplicas != desired || deployment.Status.ReadyReplicas != desired || deployment.Status.AvailableReplicas != desired || deployment.Status.UnavailableReplicas != 0 {
		return false
	}
	for _, condition := range deployment.Status.Conditions {
		if condition.Type == appsv1.DeploymentProgressing && condition.Status == "False" && condition.Reason == "ProgressDeadlineExceeded" {
			return false
		}
	}
	return true
}

func newMutationResult(key domain.IdempotencyKey, proposal domain.RollbackProposal, startedAt time.Time, alreadyApplied bool) MutationResult {
	sum := sha256String(string(key) + "\n" + string(proposal.Hash()))
	return MutationResult{ExecutionID: domain.ExecutionID("execution-" + sum[:32]), FromRevision: proposal.CurrentRevision(), ToRevision: proposal.TargetRevision(), StartedAt: startedAt.UTC(), CompletedAt: startedAt.UTC(), AlreadyApplied: alreadyApplied}
}

func sha256String(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
