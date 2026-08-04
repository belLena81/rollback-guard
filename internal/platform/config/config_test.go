package config

import (
	"strings"
	"testing"
)

func TestLoadKubernetesMCPRejectsUnsafeConfiguration(t *testing.T) {
	tests := []struct {
		name  string
		env   map[string]string
		match string
	}{
		{name: "unknown mode", env: map[string]string{"KUBERNETES_MCP_MODE": "admin"}, match: "readonly or rollback"},
		{name: "unbounded request", env: map[string]string{"MCP_REQUEST_TIMEOUT": "3m"}, match: "no greater than 2m"},
		{name: "cluster mismatch", env: map[string]string{"CLUSTER_ID": "prod", "ALLOWED_TARGETS": "local/demo/api/api"}, match: "must use CLUSTER_ID"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearConfigEnvironment(t)
			for key, value := range test.env {
				t.Setenv(key, value)
			}
			_, err := LoadKubernetesMCP()
			if err == nil || !strings.Contains(err.Error(), test.match) {
				t.Fatalf("LoadKubernetesMCP() error = %v, want %q", err, test.match)
			}
		})
	}
}

func TestLoadObservabilityMCPRejectsUnsafeWindow(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("METRICS_WINDOW", "10s")
	_, err := LoadObservabilityMCP()
	if err == nil || !strings.Contains(err.Error(), "between 30s and 1h") {
		t.Fatalf("LoadObservabilityMCP() error = %v", err)
	}
}

func TestLoadObservabilityMCPRejectsUnboundedDependencyTimeout(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("PROMETHEUS_TIMEOUT", "31s")
	_, err := LoadObservabilityMCP()
	if err == nil || !strings.Contains(err.Error(), "no greater than 30s") {
		t.Fatalf("LoadObservabilityMCP() error = %v", err)
	}
}

func clearConfigEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range []string{"ALLOWED_TARGETS", "CLUSTER_ID", "KUBERNETES_MCP_MODE", "MCP_REQUEST_TIMEOUT", "MCP_SESSION_TIMEOUT", "METRICS_WINDOW", "METRICS_MAX_SAMPLE_AGE", "PROMETHEUS_TIMEOUT"} {
		t.Setenv(key, "")
	}
}
