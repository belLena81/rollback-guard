package config

import (
	"strings"
	"testing"
	"time"
)

func TestResilienceDefaults(t *testing.T) {
	clearConfigEnvironment(t)
	got, err := resilienceEnv()
	if err != nil {
		t.Fatal(err)
	}
	want := ResilienceConfig{
		RetryMaxAttempts:        3,
		RetryBaseDelay:          200 * time.Millisecond,
		RetryMaxDelay:           2 * time.Second,
		LLMRetryMaxAttempts:     2,
		BreakerFailureThreshold: 5,
		BreakerOpenTimeout:      30 * time.Second,
	}
	if got != want {
		t.Fatalf("defaults = %+v, want %+v", got, want)
	}
}

func TestResilienceOverridesAndValidation(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{name: "valid overrides", env: map[string]string{"RESILIENCE_RETRY_MAX_ATTEMPTS": "5", "RESILIENCE_RETRY_BASE_DELAY": "50ms", "RESILIENCE_RETRY_MAX_DELAY": "1s", "RESILIENCE_LLM_RETRY_MAX_ATTEMPTS": "1", "RESILIENCE_BREAKER_FAILURE_THRESHOLD": "2", "RESILIENCE_BREAKER_OPEN_TIMEOUT": "5s"}},
		{name: "lower bounds accepted", env: map[string]string{"RESILIENCE_RETRY_MAX_ATTEMPTS": "1", "RESILIENCE_LLM_RETRY_MAX_ATTEMPTS": "1", "RESILIENCE_RETRY_BASE_DELAY": "10ms", "RESILIENCE_RETRY_MAX_DELAY": "10ms", "RESILIENCE_BREAKER_FAILURE_THRESHOLD": "1", "RESILIENCE_BREAKER_OPEN_TIMEOUT": "1s"}},
		{name: "upper bounds accepted", env: map[string]string{"RESILIENCE_RETRY_MAX_ATTEMPTS": "10", "RESILIENCE_LLM_RETRY_MAX_ATTEMPTS": "10", "RESILIENCE_RETRY_BASE_DELAY": "1m", "RESILIENCE_RETRY_MAX_DELAY": "1m", "RESILIENCE_BREAKER_FAILURE_THRESHOLD": "100", "RESILIENCE_BREAKER_OPEN_TIMEOUT": "10m"}},
		{name: "threshold above max", env: map[string]string{"RESILIENCE_BREAKER_FAILURE_THRESHOLD": "101"}, wantErr: "RESILIENCE_BREAKER_FAILURE_THRESHOLD"},
		{name: "open timeout above max", env: map[string]string{"RESILIENCE_BREAKER_OPEN_TIMEOUT": "11m"}, wantErr: "RESILIENCE_BREAKER_OPEN_TIMEOUT"},
		{name: "attempts zero", env: map[string]string{"RESILIENCE_RETRY_MAX_ATTEMPTS": "0"}, wantErr: "RESILIENCE_RETRY_MAX_ATTEMPTS"},
		{name: "attempts too many", env: map[string]string{"RESILIENCE_RETRY_MAX_ATTEMPTS": "11"}, wantErr: "RESILIENCE_RETRY_MAX_ATTEMPTS"},
		{name: "attempts not a number", env: map[string]string{"RESILIENCE_RETRY_MAX_ATTEMPTS": "many"}, wantErr: "RESILIENCE_RETRY_MAX_ATTEMPTS"},
		{name: "llm attempts zero", env: map[string]string{"RESILIENCE_LLM_RETRY_MAX_ATTEMPTS": "0"}, wantErr: "RESILIENCE_LLM_RETRY_MAX_ATTEMPTS"},
		{name: "base delay too small", env: map[string]string{"RESILIENCE_RETRY_BASE_DELAY": "1ms"}, wantErr: "RESILIENCE_RETRY_BASE_DELAY"},
		{name: "base delay negative", env: map[string]string{"RESILIENCE_RETRY_BASE_DELAY": "-1s"}, wantErr: "RESILIENCE_RETRY_BASE_DELAY"},
		{name: "max delay below base", env: map[string]string{"RESILIENCE_RETRY_BASE_DELAY": "3s", "RESILIENCE_RETRY_MAX_DELAY": "1s"}, wantErr: "RESILIENCE_RETRY_MAX_DELAY"},
		{name: "max delay too large", env: map[string]string{"RESILIENCE_RETRY_MAX_DELAY": "2h"}, wantErr: "RESILIENCE_RETRY_MAX_DELAY"},
		{name: "threshold zero", env: map[string]string{"RESILIENCE_BREAKER_FAILURE_THRESHOLD": "0"}, wantErr: "RESILIENCE_BREAKER_FAILURE_THRESHOLD"},
		{name: "open timeout too small", env: map[string]string{"RESILIENCE_BREAKER_OPEN_TIMEOUT": "10ms"}, wantErr: "RESILIENCE_BREAKER_OPEN_TIMEOUT"},
		{name: "open timeout garbage", env: map[string]string{"RESILIENCE_BREAKER_OPEN_TIMEOUT": "soon"}, wantErr: "RESILIENCE_BREAKER_OPEN_TIMEOUT"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearConfigEnvironment(t)
			for key, value := range test.env {
				t.Setenv(key, value)
			}
			_, err := resilienceEnv()
			if test.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("err = %v, want it to name %s", err, test.wantErr)
			}
		})
	}
}

func TestResilienceMaxDelayDefaultFollowsALargerBaseDelay(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("RESILIENCE_RETRY_BASE_DELAY", "3s")
	got, err := resilienceEnv()
	if err != nil {
		t.Fatal(err)
	}
	if got.RetryMaxDelay != 3*time.Second {
		t.Fatalf("RetryMaxDelay = %s, want it to default to the 3s base delay", got.RetryMaxDelay)
	}
}

func TestEveryServiceLoadsResilienceSettings(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("DATABASE_URL", "postgres://rollback@database/rollback_guard")
	t.Setenv("RESILIENCE_BREAKER_FAILURE_THRESHOLD", "7")
	orchestrator, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	investigator, err := LoadInvestigator()
	if err != nil {
		t.Fatal(err)
	}
	kubernetesMCP, err := LoadKubernetesMCP()
	if err != nil {
		t.Fatal(err)
	}
	observability, err := LoadObservabilityMCP()
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]int{
		"orchestrator": orchestrator.Resilience.BreakerFailureThreshold, "investigator": investigator.Resilience.BreakerFailureThreshold,
		"kubernetes-mcp": kubernetesMCP.Resilience.BreakerFailureThreshold, "observability-mcp": observability.Resilience.BreakerFailureThreshold,
	} {
		if got != 7 {
			t.Errorf("%s threshold = %d, want 7", name, got)
		}
	}
	t.Setenv("RESILIENCE_RETRY_MAX_ATTEMPTS", "0")
	if _, err := Load(); err == nil {
		t.Error("orchestrator accepted invalid resilience settings")
	}
	if _, err := LoadInvestigator(); err == nil {
		t.Error("investigator accepted invalid resilience settings")
	}
	if _, err := LoadKubernetesMCP(); err == nil {
		t.Error("kubernetes MCP accepted invalid resilience settings")
	}
	if _, err := LoadObservabilityMCP(); err == nil {
		t.Error("observability MCP accepted invalid resilience settings")
	}
}
