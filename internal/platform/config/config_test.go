package config

import (
	"strings"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
)

func TestDefaultAllowedTargetsMatchDemoRBACNamespace(t *testing.T) {
	tests := []struct {
		name string
		load func(t *testing.T) []string
	}{
		{
			name: "orchestrator",
			load: func(t *testing.T) []string {
				t.Helper()
				t.Setenv("DATABASE_URL", "postgres://rollback@database/rollback_guard")
				cfg, err := Load()
				if err != nil {
					t.Fatal(err)
				}
				return targetStrings(cfg.AllowedTargets)
			},
		},
		{
			name: "kubernetes MCP",
			load: func(t *testing.T) []string {
				t.Helper()
				cfg, err := LoadKubernetesMCP()
				if err != nil {
					t.Fatal(err)
				}
				return targetStrings(cfg.AllowedTargets)
			},
		},
		{
			name: "observability MCP",
			load: func(t *testing.T) []string {
				t.Helper()
				cfg, err := LoadObservabilityMCP()
				if err != nil {
					t.Fatal(err)
				}
				return targetStrings(cfg.AllowedTargets)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearConfigEnvironment(t)
			got := test.load(t)
			if len(got) != 1 || got[0] != "local/rollback-demo/demo-api/demo-api" {
				t.Fatalf("default targets = %v", got)
			}
		})
	}
}

func TestLoadRejectsSubSecondMCPWatchTimeout(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("DATABASE_URL", "postgres://rollback@database/rollback_guard")
	t.Setenv("MCP_WATCH_TIMEOUT", "500ms")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "between 1s and 2m") {
		t.Fatalf("Load() error = %v, want watch timeout range error", err)
	}
}

func TestLoadRequiresA2ALeaseToCoverCancellationCleanup(t *testing.T) {
	tests := []struct {
		name     string
		leaseTTL string
		wantErr  bool
	}{
		{name: "task timeout only", leaseTTL: "10s", wantErr: true},
		{name: "task plus cancellation timeout", leaseTTL: "15s"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearConfigEnvironment(t)
			t.Setenv("DATABASE_URL", "postgres://rollback@database/rollback_guard")
			t.Setenv("A2A_TASK_TIMEOUT", "10s")
			t.Setenv("A2A_CANCEL_TIMEOUT", "5s")
			t.Setenv("A2A_LEASE_TTL", test.leaseTTL)

			cfg, err := Load()
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), "A2A_TASK_TIMEOUT plus A2A_CANCEL_TIMEOUT") {
					t.Fatalf("Load() error = %v, want lease coverage error", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.A2ALeaseTTL != 15*time.Second {
				t.Fatalf("A2ALeaseTTL = %v, want 15s", cfg.A2ALeaseTTL)
			}
		})
	}
}

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

func TestLoadKubernetesMCPRequiresDatabaseOnlyForRollbackMode(t *testing.T) {
	clearConfigEnvironment(t)
	if _, err := LoadKubernetesMCP(); err != nil {
		t.Fatalf("readonly configuration error = %v", err)
	}

	clearConfigEnvironment(t)
	t.Setenv("KUBERNETES_MCP_MODE", "rollback")
	if _, err := LoadKubernetesMCP(); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("rollback configuration error = %v", err)
	}

	t.Setenv("DATABASE_URL", "postgres://rollback@database/rollback_guard")
	cfg, err := LoadKubernetesMCP()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseURL == "" || cfg.DBMaxConns != 4 || cfg.DBPingTimeout <= 0 {
		t.Fatalf("rollback database configuration = %#v", cfg)
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

func TestLoadInvestigatorDefaultsToRuleDiagnosisWithoutLLMConfiguration(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("DATABASE_URL", "postgres://rollback@database/rollback_guard")
	t.Setenv("LLM_PROVIDER", "dormant-invalid-provider")
	t.Setenv("LLM_BASE_URL", "://invalid")
	t.Setenv("LLM_TIMEOUT", "not-a-duration")
	t.Setenv("LLM_MAX_OUTPUT_TOKENS", "invalid")
	cfg, err := LoadInvestigator()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DiagnosisStrategy != domain.StrategyRules {
		t.Fatalf("DiagnosisStrategy = %s, want rules", cfg.DiagnosisStrategy)
	}
}

func TestLoadInvestigatorValidatesModelDiagnosisConfiguration(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    domain.DiagnosisStrategy
		wantErr string
	}{
		{name: "llm loopback", env: map[string]string{"DIAGNOSIS_STRATEGY": "llm_based", "LLM_BASE_URL": "http://127.0.0.1:11434/v1", "LLM_MODEL": "local-model"}, want: domain.StrategyLLM},
		{name: "hybrid hosted", env: map[string]string{"DIAGNOSIS_STRATEGY": "hybrid", "LLM_BASE_URL": "https://provider.example/v1", "LLM_MODEL": "hosted-model", "LLM_API_KEY": "test-key"}, want: domain.StrategyHybrid},
		{name: "unknown strategy", env: map[string]string{"DIAGNOSIS_STRATEGY": "automatic"}, wantErr: "DIAGNOSIS_STRATEGY"},
		{name: "unknown provider", env: map[string]string{"DIAGNOSIS_STRATEGY": "llm_based", "LLM_PROVIDER": "native", "LLM_BASE_URL": "http://localhost:11434/v1", "LLM_MODEL": "model"}, wantErr: "LLM_PROVIDER"},
		{name: "missing base URL", env: map[string]string{"DIAGNOSIS_STRATEGY": "llm_based", "LLM_MODEL": "model"}, wantErr: "LLM_BASE_URL"},
		{name: "missing model", env: map[string]string{"DIAGNOSIS_STRATEGY": "llm_based", "LLM_BASE_URL": "http://localhost:11434/v1"}, wantErr: "LLM_MODEL"},
		{name: "hosted key required", env: map[string]string{"DIAGNOSIS_STRATEGY": "llm_based", "LLM_BASE_URL": "https://provider.example/v1", "LLM_MODEL": "model"}, wantErr: "LLM_API_KEY"},
		{name: "insecure remote URL", env: map[string]string{"DIAGNOSIS_STRATEGY": "llm_based", "LLM_BASE_URL": "http://provider.example/v1", "LLM_MODEL": "model", "LLM_API_KEY": "key"}, wantErr: "HTTPS"},
		{name: "timeout exceeds limit", env: map[string]string{"DIAGNOSIS_STRATEGY": "llm_based", "LLM_BASE_URL": "http://localhost:11434/v1", "LLM_MODEL": "model", "LLM_TIMEOUT": "31s"}, wantErr: "LLM_TIMEOUT"},
		{name: "reported timeout budget exhaustion", env: map[string]string{"DIAGNOSIS_STRATEGY": "llm_based", "LLM_BASE_URL": "http://localhost:11434/v1", "LLM_MODEL": "model", "MCP_CALL_TIMEOUT": "10s", "LLM_TIMEOUT": "15s", "A2A_TASK_TIMEOUT": "20s"}, wantErr: "rule fallback"},
		{name: "exact timeout budget", env: map[string]string{"DIAGNOSIS_STRATEGY": "llm_based", "LLM_BASE_URL": "http://localhost:11434/v1", "LLM_MODEL": "model", "MCP_CALL_TIMEOUT": "10s", "LLM_TIMEOUT": "15s", "A2A_TASK_TIMEOUT": "36s"}, want: domain.StrategyLLM},
		{name: "below timeout budget by one nanosecond", env: map[string]string{"DIAGNOSIS_STRATEGY": "llm_based", "LLM_BASE_URL": "http://localhost:11434/v1", "LLM_MODEL": "model", "MCP_CALL_TIMEOUT": "10s", "LLM_TIMEOUT": "15s", "A2A_TASK_TIMEOUT": "35999999999ns"}, wantErr: "rule fallback"},
		{name: "hybrid timeout budget exhaustion", env: map[string]string{"DIAGNOSIS_STRATEGY": "hybrid", "LLM_BASE_URL": "http://localhost:11434/v1", "LLM_MODEL": "model", "MCP_CALL_TIMEOUT": "10s", "LLM_TIMEOUT": "15s", "A2A_TASK_TIMEOUT": "35s"}, wantErr: "rule fallback"},
		{name: "output tokens too small", env: map[string]string{"DIAGNOSIS_STRATEGY": "llm_based", "LLM_BASE_URL": "http://localhost:11434/v1", "LLM_MODEL": "model", "LLM_MAX_OUTPUT_TOKENS": "63"}, wantErr: "LLM_MAX_OUTPUT_TOKENS"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearConfigEnvironment(t)
			t.Setenv("DATABASE_URL", "postgres://rollback@database/rollback_guard")
			for key, value := range test.env {
				t.Setenv(key, value)
			}
			cfg, err := LoadInvestigator()
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("LoadInvestigator() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.DiagnosisStrategy != test.want {
				t.Fatalf("DiagnosisStrategy = %s, want %s", cfg.DiagnosisStrategy, test.want)
			}
		})
	}
}

func clearConfigEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range []string{"ALLOWED_TARGETS", "CLUSTER_ID", "KUBERNETES_MCP_MODE", "MCP_REQUEST_TIMEOUT", "MCP_SESSION_TIMEOUT", "METRICS_WINDOW", "METRICS_MAX_SAMPLE_AGE", "PROMETHEUS_TIMEOUT", "DATABASE_URL", "DB_MAX_CONNS", "DB_PING_TIMEOUT", "MCP_CONNECT_TIMEOUT", "MCP_CALL_TIMEOUT", "MCP_WATCH_TIMEOUT", "A2A_TASK_TIMEOUT", "A2A_CANCEL_TIMEOUT", "A2A_POLL_INTERVAL", "A2A_LEASE_TTL", "A2A_MAX_ATTEMPTS", "INVESTIGATOR_URL", "A2A_WORKER_ID", "DIAGNOSIS_STRATEGY", "LLM_PROVIDER", "LLM_BASE_URL", "LLM_MODEL", "LLM_API_KEY", "LLM_TIMEOUT", "LLM_MAX_OUTPUT_TOKENS"} {
		t.Setenv(key, "")
	}
}

func targetStrings(targets []domain.DeploymentTarget) []string {
	out := make([]string, len(targets))
	for index, target := range targets {
		out[index] = target.Cluster() + "/" + target.Namespace() + "/" + target.Deployment() + "/" + target.Service()
	}
	return out
}
