package main

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/belLena81/rollback-guard/internal/platform/resilience"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

type retryRecorder struct {
	mu      sync.Mutex
	retries []string
}

func (r *retryRecorder) RetryAttempted(dep resilience.Dependency, outcome resilience.RetryOutcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.retries = append(r.retries, string(dep)+"/"+string(outcome))
}

func (*retryRecorder) BreakerStateChanged(resilience.Dependency, resilience.BreakerState) {}

// protectBackend must retry a real service's read when the API server is
// unavailable, and report it to the process observer.
func TestProtectBackendRetriesUnavailableAPIServerReads(t *testing.T) {
	cfg := testConfig(t, "readonly")
	clientset := fake.NewSimpleClientset()
	gets := 0
	clientset.PrependReactor("get", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
		gets++
		return true, nil, apierrors.NewServiceUnavailable("etcd leader change")
	})
	service, err := buildServices(cfg, clientset, nil)
	if err != nil {
		t.Fatal(err)
	}
	observer := &retryRecorder{}
	backend, err := protectBackend(cfg, service, observer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.InspectDeployment(context.Background(), cfg.AllowedTargets[0]); err == nil {
		t.Fatal("expected the unavailable error after the retries")
	}
	if want := cfg.Resilience.RetryMaxAttempts; gets != want {
		t.Fatalf("API server reads = %d, want %d bounded attempts", gets, want)
	}
	if len(observer.retries) == 0 || observer.retries[0] != "kubernetes_api/retried" {
		t.Fatalf("observer saw %v, want kubernetes_api retries", observer.retries)
	}
}

func TestProtectBackendDoesNotRetryForbiddenReads(t *testing.T) {
	cfg := testConfig(t, "readonly")
	clientset := fake.NewSimpleClientset()
	gets := 0
	clientset.PrependReactor("get", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
		gets++
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "demo-api", errors.New("rbac"))
	})
	service, err := buildServices(cfg, clientset, nil)
	if err != nil {
		t.Fatal(err)
	}
	backend, err := protectBackend(cfg, service, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.InspectDeployment(context.Background(), cfg.AllowedTargets[0]); err == nil {
		t.Fatal("expected the forbidden error")
	}
	if gets != 1 {
		t.Fatalf("API server reads = %d, want no retry of a permission error", gets)
	}
}
