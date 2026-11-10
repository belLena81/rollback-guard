package kubernetesserver

import (
	"context"
	"errors"

	"github.com/belLena81/rollback-guard/internal/adapters/kubernetes"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/resilience"
)

type protectedBackend struct {
	next      Backend
	protector *resilience.Protector
}

// NewProtectedBackend retries transient Kubernetes API failures on the
// read methods and opens a breaker after consecutive ones. PrepareRollback,
// ExecuteApprovedRollback, and WatchRollout pass straight through: a mutation
// is never repeated here (the rollback ledger and the caller's idempotency
// key own that), and a rollout watch is a long call with its own deadline.
func NewProtectedBackend(next Backend, protector *resilience.Protector) (Backend, error) {
	if next == nil || protector == nil {
		return nil, errors.New("kubernetes backend and protector are required")
	}
	return protectedBackend{next: next, protector: protector}, nil
}

func read[T any](ctx context.Context, b protectedBackend, fn func(context.Context) (T, error)) (T, error) {
	return resilience.Call(ctx, b.protector, func(ctx context.Context) (T, error) { //nolint:wrapcheck // decorator: error passes through unchanged
		value, err := fn(ctx)
		return value, kubernetes.MarkTransientAPIError(err)
	})
}

func (b protectedBackend) InspectDeployment(ctx context.Context, target domain.DeploymentTarget) (domain.DeploymentEvidence, error) {
	return read(ctx, b, func(ctx context.Context) (domain.DeploymentEvidence, error) {
		return b.next.InspectDeployment(ctx, target)
	})
}

func (b protectedBackend) ReplicaSets(ctx context.Context, target domain.DeploymentTarget) ([]kubernetes.ReplicaSetSummary, error) {
	return read(ctx, b, func(ctx context.Context) ([]kubernetes.ReplicaSetSummary, error) {
		return b.next.ReplicaSets(ctx, target)
	})
}

type podList struct {
	pods      []kubernetes.PodSummary
	truncated bool
}

func (b protectedBackend) ListPods(ctx context.Context, target domain.DeploymentTarget, limit int) ([]kubernetes.PodSummary, bool, error) {
	list, err := read(ctx, b, func(ctx context.Context) (podList, error) {
		pods, truncated, err := b.next.ListPods(ctx, target, limit)
		return podList{pods: pods, truncated: truncated}, err //nolint:wrapcheck // decorator: original error passes through for classification
	})
	return list.pods, list.truncated, err
}

type eventList struct {
	events    []kubernetes.EventSummary
	truncated bool
}

func (b protectedBackend) PodEvents(ctx context.Context, target domain.DeploymentTarget, pod string, limit int) ([]kubernetes.EventSummary, bool, error) {
	list, err := read(ctx, b, func(ctx context.Context) (eventList, error) {
		events, truncated, err := b.next.PodEvents(ctx, target, pod, limit)
		return eventList{events: events, truncated: truncated}, err //nolint:wrapcheck // decorator: original error passes through for classification
	})
	return list.events, list.truncated, err
}

func (b protectedBackend) GetRolloutStatus(ctx context.Context, target domain.DeploymentTarget) (kubernetes.RolloutStatus, error) {
	return read(ctx, b, func(ctx context.Context) (kubernetes.RolloutStatus, error) {
		return b.next.GetRolloutStatus(ctx, target)
	})
}

func (b protectedBackend) WatchRollout(ctx context.Context, target domain.DeploymentTarget, revision domain.Revision) (kubernetes.RolloutStatus, error) {
	return b.next.WatchRollout(ctx, target, revision) //nolint:wrapcheck // decorator: original error passes through for classification
}

func (b protectedBackend) PrepareRollback(ctx context.Context, proposal domain.RollbackProposal) error {
	return b.next.PrepareRollback(ctx, proposal) //nolint:wrapcheck // decorator: original error passes through for classification
}

func (b protectedBackend) ExecuteApprovedRollback(ctx context.Context, hash domain.ProposalHash, key domain.IdempotencyKey) (kubernetes.MutationResult, error) {
	return b.next.ExecuteApprovedRollback(ctx, hash, key) //nolint:wrapcheck // decorator: original error passes through for classification
}
