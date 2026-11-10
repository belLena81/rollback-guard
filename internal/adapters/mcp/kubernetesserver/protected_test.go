package kubernetesserver

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/kubernetes"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/resilience"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// countingBackend counts calls per method and fails reads with err.
type countingBackend struct {
	err   error
	calls map[string]int
}

func (b *countingBackend) hit(name string) error {
	if b.calls == nil {
		b.calls = map[string]int{}
	}
	b.calls[name]++
	return b.err
}

func (b *countingBackend) InspectDeployment(context.Context, domain.DeploymentTarget) (domain.DeploymentEvidence, error) {
	return domain.DeploymentEvidence{}, b.hit("InspectDeployment")
}

func (b *countingBackend) ReplicaSets(context.Context, domain.DeploymentTarget) ([]kubernetes.ReplicaSetSummary, error) {
	return nil, b.hit("ReplicaSets")
}

func (b *countingBackend) ListPods(context.Context, domain.DeploymentTarget, int) ([]kubernetes.PodSummary, bool, error) {
	return nil, false, b.hit("ListPods")
}

func (b *countingBackend) PodEvents(context.Context, domain.DeploymentTarget, string, int) ([]kubernetes.EventSummary, bool, error) {
	return nil, false, b.hit("PodEvents")
}

func (b *countingBackend) GetRolloutStatus(context.Context, domain.DeploymentTarget) (kubernetes.RolloutStatus, error) {
	return kubernetes.RolloutStatus{}, b.hit("GetRolloutStatus")
}

func (b *countingBackend) WatchRollout(context.Context, domain.DeploymentTarget, domain.Revision) (kubernetes.RolloutStatus, error) {
	return kubernetes.RolloutStatus{}, b.hit("WatchRollout")
}

func (b *countingBackend) PrepareRollback(context.Context, domain.RollbackProposal) error {
	return b.hit("PrepareRollback")
}

func (b *countingBackend) ExecuteApprovedRollback(context.Context, domain.ProposalHash, domain.IdempotencyKey) (kubernetes.MutationResult, error) {
	return kubernetes.MutationResult{}, b.hit("ExecuteApprovedRollback")
}

func testProtector(t *testing.T) *resilience.Protector {
	t.Helper()
	timing := resilience.Timing{Now: time.Now, Sleep: func(context.Context, time.Duration) error { return nil }, Jitter: func() float64 { return 0 }}
	retrier, err := resilience.NewRetrier(resilience.DependencyKubernetesAPI, resilience.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}, timing, nil)
	if err != nil {
		t.Fatal(err)
	}
	breaker, err := resilience.NewBreaker(resilience.DependencyKubernetesAPI, resilience.BreakerConfig{FailureThreshold: 100, OpenTimeout: time.Minute}, time.Now, nil)
	if err != nil {
		t.Fatal(err)
	}
	protector, err := resilience.NewProtector(retrier, breaker)
	if err != nil {
		t.Fatal(err)
	}
	return protector
}

var podsResource = schema.GroupResource{Resource: "pods"}

func TestProtectedBackendRetriesOnlyTransientKubernetesErrors(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantCalls int
	}{
		{"server timeout", apierrors.NewServerTimeout(podsResource, "list", 1), 3},
		{"timeout", apierrors.NewTimeoutError("slow", 1), 3},
		{"too many requests", apierrors.NewTooManyRequests("slow down", 1), 3},
		{"service unavailable", apierrors.NewServiceUnavailable("down"), 3},
		{"internal error", apierrors.NewInternalError(errors.New("etcd")), 3},
		{"deadline", context.DeadlineExceeded, 3},
		{"forbidden", apierrors.NewForbidden(podsResource, "x", errors.New("rbac")), 1},
		{"not found", apierrors.NewNotFound(podsResource, "x"), 1},
		{"conflict", apierrors.NewConflict(podsResource, "x", errors.New("c")), 1},
		{"bad request", apierrors.NewBadRequest("no"), 1},
		{"target not allowed", kubernetes.ErrTargetForbidden, 1},
		{"validation", &domain.ValidationError{Field: "f", Reason: "r"}, 1},
		{"unauthorized status", apierrors.NewGenericServerResponse(http.StatusUnauthorized, "get", podsResource, "x", "", 0, false), 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next := &countingBackend{err: tt.err}
			protected, err := NewProtectedBackend(next, testProtector(t))
			if err != nil {
				t.Fatal(err)
			}
			target := domain.DeploymentTarget{}
			_, gotErr := protected.InspectDeployment(context.Background(), target)
			if next.calls["InspectDeployment"] != tt.wantCalls || gotErr == nil {
				t.Fatalf("calls = %d err = %v, want %d calls", next.calls["InspectDeployment"], gotErr, tt.wantCalls)
			}
			for name, call := range map[string]func() error{
				"ReplicaSets":      func() error { _, err := protected.ReplicaSets(context.Background(), target); return err },
				"ListPods":         func() error { _, _, err := protected.ListPods(context.Background(), target, 1); return err },
				"PodEvents":        func() error { _, _, err := protected.PodEvents(context.Background(), target, "p", 1); return err },
				"GetRolloutStatus": func() error { _, err := protected.GetRolloutStatus(context.Background(), target); return err },
			} {
				if err := call(); err == nil {
					t.Errorf("%s: error was swallowed", name)
				}
				if next.calls[name] != tt.wantCalls {
					t.Errorf("%s calls = %d, want %d", name, next.calls[name], tt.wantCalls)
				}
			}
		})
	}
}

// The mutation, its preparation, and the long rollout watch are never
// retried or breaker-guarded by this read-side decorator.
func TestProtectedBackendNeverRetriesMutationsOrWatch(t *testing.T) {
	next := &countingBackend{err: apierrors.NewServiceUnavailable("down")}
	protected, err := NewProtectedBackend(next, testProtector(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := protected.PrepareRollback(context.Background(), domain.RollbackProposal{}); err == nil {
		t.Fatal("PrepareRollback error was swallowed")
	}
	if _, err := protected.ExecuteApprovedRollback(context.Background(), "hash", "key"); err == nil {
		t.Fatal("ExecuteApprovedRollback error was swallowed")
	}
	if _, err := protected.WatchRollout(context.Background(), domain.DeploymentTarget{}, "rev"); err == nil {
		t.Fatal("WatchRollout error was swallowed")
	}
	for _, name := range []string{"PrepareRollback", "ExecuteApprovedRollback", "WatchRollout"} {
		if next.calls[name] != 1 {
			t.Errorf("%s calls = %d, want exactly 1", name, next.calls[name])
		}
	}
}

func TestNewProtectedBackendRequiresDependencies(t *testing.T) {
	if _, err := NewProtectedBackend(nil, testProtector(t)); err == nil {
		t.Fatal("nil backend accepted")
	}
	if _, err := NewProtectedBackend(&countingBackend{}, nil); err == nil {
		t.Fatal("nil protector accepted")
	}
}
