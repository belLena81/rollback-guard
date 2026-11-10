package bootstrap

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/config"
	"github.com/belLena81/rollback-guard/internal/platform/resilience"
)

func testResilience() config.ResilienceConfig {
	return config.ResilienceConfig{
		RetryMaxAttempts: 4, RetryBaseDelay: time.Millisecond, RetryMaxDelay: 2 * time.Millisecond,
		LLMRetryMaxAttempts: 2, BreakerFailureThreshold: 100, BreakerOpenTimeout: time.Minute,
	}
}

type countingInspector struct{ calls int }

func (c *countingInspector) InspectDeployment(context.Context, domain.DeploymentTarget) (domain.DeploymentEvidence, error) {
	c.calls++
	return domain.DeploymentEvidence{}, context.DeadlineExceeded
}

func TestNewProtectorUsesTheConfiguredAttempts(t *testing.T) {
	protector, err := NewProtector(resilience.DependencyObservabilityMCP, testResilience(), nil)
	if err != nil {
		t.Fatal(err)
	}
	next := &countingInspector{}
	decorated, err := resilience.NewInspector(next, protector)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decorated.InspectDeployment(context.Background(), domain.DeploymentTarget{}); err == nil {
		t.Fatal("expected the last transient error")
	}
	if next.calls != 4 {
		t.Fatalf("calls = %d, want the configured 4 attempts", next.calls)
	}
}

func TestNewLLMProtectorUsesTheLLMAttempts(t *testing.T) {
	protector, err := NewLLMProtector(testResilience(), nil)
	if err != nil {
		t.Fatal(err)
	}
	next := &countingInspector{}
	decorated, err := resilience.NewInspector(next, protector)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decorated.InspectDeployment(context.Background(), domain.DeploymentTarget{}); err == nil {
		t.Fatal("expected the last transient error")
	}
	if next.calls != 2 {
		t.Fatalf("calls = %d, want the LLM limit of 2 attempts", next.calls)
	}
}

func TestNewMutationRetrierAllowsAtMostTwoAttempts(t *testing.T) {
	cfg := testResilience()
	cfg.RetryMaxAttempts = 9
	retrier, err := NewMutationRetrier(resilience.DependencyKubernetesRollbackMCP, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	_, err = resilience.Do(context.Background(), retrier, func(error) bool { return true }, func(context.Context) (int, error) {
		calls++
		return 0, errors.New("fail")
	})
	if err == nil || calls != 2 {
		t.Fatalf("calls = %d err = %v, want a mutation retrier capped at 2 attempts", calls, err)
	}
}

func TestResilienceBuildersRejectInvalidSettings(t *testing.T) {
	bad := testResilience()
	bad.BreakerFailureThreshold = 0
	if _, err := NewProtector(resilience.DependencyPrometheus, bad, nil); err == nil {
		t.Fatal("invalid breaker settings accepted")
	}
	bad = testResilience()
	bad.RetryMaxDelay = 0
	if _, err := NewMutationRetrier(resilience.DependencyKubernetesRollbackMCP, bad, nil); err == nil {
		t.Fatal("invalid retry settings accepted")
	}
}
