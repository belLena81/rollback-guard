package config

import (
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
)

// everyVariable lists every environment variable any loader reads.
var everyVariable = []string{
	"HTTP_ADDR", "DATABASE_URL", "DB_MAX_CONNS", "DB_PING_TIMEOUT",
	"PROPOSAL_TTL", "APPROVAL_TTL", "CLEANUP_TIMEOUT", "SYSTEM_ACTOR_ID", "LOCAL_ACTOR_ID",
	"RULE_MIN_CONFIDENCE", "RULE_CORRELATION_WINDOW", "ALLOWED_TARGETS", "CLUSTER_ID", "KUBECONFIG", "KUBERNETES_MCP_MODE",
	"KUBERNETES_READ_MCP_URL", "KUBERNETES_ROLLBACK_MCP_URL", "OBSERVABILITY_MCP_URL",
	"MCP_CONNECT_TIMEOUT", "MCP_CALL_TIMEOUT", "MCP_WATCH_TIMEOUT", "MCP_REQUEST_TIMEOUT", "MCP_SESSION_TIMEOUT",
	"METRICS_WINDOW", "METRICS_MAX_SAMPLE_AGE", "PROMETHEUS_URL", "PROMETHEUS_TIMEOUT",
	"INVESTIGATOR_URL", "INVESTIGATOR_PUBLIC_URL", "A2A_CONNECT_TIMEOUT", "A2A_TASK_TIMEOUT", "A2A_CANCEL_TIMEOUT",
	"A2A_POLL_INTERVAL", "A2A_LEASE_TTL", "A2A_WORKER_ID", "A2A_MAX_ATTEMPTS",
	"DIAGNOSIS_STRATEGY", "LLM_PROVIDER", "LLM_BASE_URL", "LLM_MODEL", "LLM_API_KEY", "LLM_TIMEOUT", "LLM_MAX_OUTPUT_TOKENS",
	"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_SERVICE_NAME", "OTEL_TRACES_SAMPLE_RATIO", "TELEMETRY_SHUTDOWN_TIMEOUT", "LOG_LEVEL",
	"SESSION_TTL", "SESSION_COOKIE_SECURE",
}

const testDatabaseURL = "postgres://rollback@database/rollback_guard"

// isolateEnvironment clears every variable and then applies env.
func isolateEnvironment(t *testing.T, env map[string]string) {
	t.Helper()
	for _, key := range everyVariable {
		t.Setenv(key, "")
	}
	for key, value := range env {
		t.Setenv(key, value)
	}
}

func defaultTargets(t *testing.T) []domain.DeploymentTarget {
	t.Helper()
	target, err := domain.NewDeploymentTarget("local", "rollback-demo", "demo-api", "demo-api")
	if err != nil {
		t.Fatal(err)
	}
	return []domain.DeploymentTarget{target}
}

func defaultResilience() ResilienceConfig {
	return ResilienceConfig{
		RetryMaxAttempts: 3, RetryBaseDelay: 200 * time.Millisecond, RetryMaxDelay: 2 * time.Second,
		LLMRetryMaxAttempts: 2, BreakerFailureThreshold: 5, BreakerOpenTimeout: 30 * time.Second,
	}
}

func defaultTelemetry(service string) TelemetryConfig {
	return TelemetryConfig{ServiceName: service, SampleRatio: 1, ShutdownTimeout: 5 * time.Second, LogLevel: slog.LevelInfo}
}

// The tests below spell every default out as a literal on purpose: a changed
// default has to be changed here too, and listed in the phase docs.

func TestOrchestratorDefaults(t *testing.T) {
	isolateEnvironment(t, map[string]string{"DATABASE_URL": testDatabaseURL})
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		HTTPAddr:              "127.0.0.1:8080",
		DatabaseURL:           testDatabaseURL,
		DBMaxConns:            4,
		DBPingTimeout:         2 * time.Second,
		ProposalTTL:           15 * time.Minute,
		ApprovalTTL:           10 * time.Minute,
		CleanupTimeout:        5 * time.Second,
		SystemActorID:         "rollback-guardian",
		LocalActorID:          "local-admin",
		RuleMinimumConfidence: 0.8,
		RuleCorrelationWindow: 10 * time.Minute,
		AllowedTargets:        defaultTargets(t),
		KubernetesReadMCPURL:  "http://127.0.0.1:8091/mcp",
		KubernetesWriteMCPURL: "http://127.0.0.1:8092/mcp",
		ObservabilityMCPURL:   "http://127.0.0.1:8093/mcp",
		MCPConnectTimeout:     5 * time.Second,
		MCPCallTimeout:        10 * time.Second,
		MCPWatchTimeout:       90 * time.Second,
		MetricsWindow:         5 * time.Minute,
		RecoveryTimeout:       5*time.Minute + 30*time.Second,
		RecoveryPollInterval:  5 * time.Second,
		InvestigatorURL:       "http://127.0.0.1:8094",
		A2AConnectTimeout:     5 * time.Second,
		A2ATaskTimeout:        45 * time.Second,
		A2ACancelTimeout:      5 * time.Second,
		A2APollInterval:       250 * time.Millisecond,
		A2ALeaseTTL:           50 * time.Second,
		A2AWorkerID:           "orchestrator-1",
		A2AMaxAttempts:        3,
		Telemetry:             defaultTelemetry("orchestrator"),
		Resilience:            defaultResilience(),
		Session:               SessionConfig{TTL: 8 * time.Hour, CookieSecure: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load() defaults:\n got %+v\nwant %+v", got, want)
	}
}

func TestInvestigatorDefaults(t *testing.T) {
	isolateEnvironment(t, map[string]string{"DATABASE_URL": testDatabaseURL})
	got, err := LoadInvestigator()
	if err != nil {
		t.Fatal(err)
	}
	want := InvestigatorConfig{
		HTTPAddr:              "127.0.0.1:8094",
		PublicURL:             "http://127.0.0.1:8094",
		DatabaseURL:           testDatabaseURL,
		DBMaxConns:            4,
		DBPingTimeout:         2 * time.Second,
		KubernetesReadMCPURL:  "http://127.0.0.1:8091/mcp",
		ObservabilityMCPURL:   "http://127.0.0.1:8093/mcp",
		MCPConnectTimeout:     5 * time.Second,
		MCPCallTimeout:        10 * time.Second,
		TaskTimeout:           45 * time.Second,
		MetricsWindow:         5 * time.Minute,
		RuleCorrelationWindow: 10 * time.Minute,
		DiagnosisStrategy:     domain.StrategyRules,
		LLMTimeout:            15 * time.Second,
		LLMMaxOutputTokens:    512,
		Telemetry:             defaultTelemetry("investigator"),
		Resilience:            defaultResilience(),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadInvestigator() defaults:\n got %+v\nwant %+v", got, want)
	}
}

func TestKubernetesMCPDefaults(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want KubernetesMCPConfig
	}{
		{
			name: "readonly reads no database settings",
			env:  map[string]string{"DATABASE_URL": testDatabaseURL, "DB_MAX_CONNS": "not-a-number", "DB_PING_TIMEOUT": "not-a-duration"},
			want: KubernetesMCPConfig{HTTPAddr: "127.0.0.1:8091", Mode: "readonly", ClusterID: "local", RequestTimeout: 2 * time.Minute, SessionTimeout: 5 * time.Minute, Telemetry: defaultTelemetry("kubernetes-mcp-readonly"), Resilience: defaultResilience()},
		},
		{
			name: "rollback",
			env:  map[string]string{"KUBERNETES_MCP_MODE": "rollback", "DATABASE_URL": testDatabaseURL},
			want: KubernetesMCPConfig{HTTPAddr: "127.0.0.1:8092", Mode: "rollback", ClusterID: "local", DatabaseURL: testDatabaseURL, DBMaxConns: 4, DBPingTimeout: 2 * time.Second, RequestTimeout: 2 * time.Minute, SessionTimeout: 5 * time.Minute, Telemetry: defaultTelemetry("kubernetes-mcp-rollback"), Resilience: defaultResilience()},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			isolateEnvironment(t, test.env)
			got, err := LoadKubernetesMCP()
			if err != nil {
				t.Fatal(err)
			}
			test.want.AllowedTargets = defaultTargets(t)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("LoadKubernetesMCP() defaults:\n got %+v\nwant %+v", got, test.want)
			}
		})
	}
}

func TestObservabilityMCPDefaults(t *testing.T) {
	isolateEnvironment(t, nil)
	got, err := LoadObservabilityMCP()
	if err != nil {
		t.Fatal(err)
	}
	want := ObservabilityMCPConfig{
		HTTPAddr:       "127.0.0.1:8093",
		PrometheusURL:  "http://127.0.0.1:9090",
		AllowedTargets: defaultTargets(t),
		RequestTimeout: 5 * time.Second,
		SessionTimeout: 5 * time.Minute,
		MetricsWindow:  5 * time.Minute,
		MaxSampleAge:   time.Minute,
		Telemetry:      defaultTelemetry("observability-mcp"),
		Resilience:     defaultResilience(),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadObservabilityMCP() defaults:\n got %+v\nwant %+v", got, want)
	}
}

// Until Phase 7 adds authentication no service may listen beyond loopback
// unless the deployment asks for it.
func TestEveryServiceBindsLoopbackByDefault(t *testing.T) {
	loaders := map[string]func(*testing.T) (string, error){
		"orchestrator": func(t *testing.T) (string, error) { cfg, err := Load(); return cfg.HTTPAddr, err },
		"investigator": func(t *testing.T) (string, error) { cfg, err := LoadInvestigator(); return cfg.HTTPAddr, err },
		"kubernetes MCP readonly": func(t *testing.T) (string, error) {
			cfg, err := LoadKubernetesMCP()
			return cfg.HTTPAddr, err
		},
		"kubernetes MCP rollback": func(t *testing.T) (string, error) {
			t.Setenv("KUBERNETES_MCP_MODE", "rollback")
			cfg, err := LoadKubernetesMCP()
			return cfg.HTTPAddr, err
		},
		"observability MCP": func(t *testing.T) (string, error) { cfg, err := LoadObservabilityMCP(); return cfg.HTTPAddr, err },
	}
	seen := map[string]string{}
	for name, load := range loaders {
		t.Run(name, func(t *testing.T) {
			isolateEnvironment(t, map[string]string{"DATABASE_URL": testDatabaseURL})
			addr, err := load(t)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(addr, "127.0.0.1:") {
				t.Fatalf("default HTTP_ADDR = %q, want a 127.0.0.1 address", addr)
			}
			if other, duplicate := seen[addr]; duplicate {
				t.Fatalf("default HTTP_ADDR %q is shared with %s", addr, other)
			}
			seen[addr] = name

			t.Setenv("HTTP_ADDR", "0.0.0.0:9999")
			if addr, err = load(t); err != nil || addr != "0.0.0.0:9999" {
				t.Fatalf("explicit HTTP_ADDR = %q, %v; want it honoured", addr, err)
			}
		})
	}
}

func TestA2AConnectTimeout(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    time.Duration
		wantErr string
	}{
		{name: "defaults to the MCP connect default", want: 5 * time.Second},
		{name: "follows MCP_CONNECT_TIMEOUT as before the split", env: map[string]string{"MCP_CONNECT_TIMEOUT": "12s"}, want: 12 * time.Second},
		{name: "own setting", env: map[string]string{"A2A_CONNECT_TIMEOUT": "3s"}, want: 3 * time.Second},
		{name: "own setting wins over MCP_CONNECT_TIMEOUT", env: map[string]string{"MCP_CONNECT_TIMEOUT": "12s", "A2A_CONNECT_TIMEOUT": "3s"}, want: 3 * time.Second},
		{name: "upper bound", env: map[string]string{"A2A_CONNECT_TIMEOUT": "30s"}, want: 30 * time.Second},
		{name: "above the bound", env: map[string]string{"A2A_CONNECT_TIMEOUT": "31s"}, wantErr: "A2A_CONNECT_TIMEOUT must be positive and no greater than 30s, got 31s"},
		{name: "not positive", env: map[string]string{"A2A_CONNECT_TIMEOUT": "0s"}, wantErr: "A2A_CONNECT_TIMEOUT must be positive and no greater than 30s"},
		{name: "not a duration", env: map[string]string{"A2A_CONNECT_TIMEOUT": "soon"}, wantErr: `A2A_CONNECT_TIMEOUT must be positive and no greater than 30s: time: invalid duration "soon"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			isolateEnvironment(t, map[string]string{"DATABASE_URL": testDatabaseURL})
			for key, value := range test.env {
				t.Setenv(key, value)
			}
			cfg, err := Load()
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("Load() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.A2AConnectTimeout != test.want {
				t.Fatalf("A2AConnectTimeout = %s, want %s", cfg.A2AConnectTimeout, test.want)
			}
		})
	}
}

func TestDatabaseURLIsRequiredWhereTheServiceUsesPostgres(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		load func() error
		want string
	}{
		{name: "orchestrator", load: func() error { _, err := Load(); return err }, want: "DATABASE_URL is required"},
		{name: "orchestrator blank", env: map[string]string{"DATABASE_URL": "   "}, load: func() error { _, err := Load(); return err }, want: "DATABASE_URL is required"},
		{name: "investigator", load: func() error { _, err := LoadInvestigator(); return err }, want: "DATABASE_URL is required"},
		{name: "kubernetes MCP rollback", env: map[string]string{"KUBERNETES_MCP_MODE": "rollback"}, load: func() error { _, err := LoadKubernetesMCP(); return err }, want: "DATABASE_URL is required in rollback mode"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			isolateEnvironment(t, test.env)
			if err := test.load(); err == nil || err.Error() != test.want {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

// The watch and window bounds are the domain's; configuration must reject
// exactly what the adapters reject.
func TestLoadersUseTheDomainBounds(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		load    func() error
		wantErr string
	}{
		{name: "watch at the lower bound", key: "MCP_WATCH_TIMEOUT", value: domain.MinRolloutWatch.String(), load: func() error { _, err := Load(); return err }},
		{name: "watch at the upper bound", key: "MCP_WATCH_TIMEOUT", value: domain.MaxRolloutWatch.String(), load: func() error { _, err := Load(); return err }},
		{name: "watch above the upper bound", key: "MCP_WATCH_TIMEOUT", value: "121s", load: func() error { _, err := Load(); return err }, wantErr: "MCP_WATCH_TIMEOUT must be between 1s and 2m, got 2m1s"},
		{name: "watch below the lower bound", key: "MCP_WATCH_TIMEOUT", value: "999ms", load: func() error { _, err := Load(); return err }, wantErr: "MCP_WATCH_TIMEOUT must be between 1s and 2m, got 999ms"},
		{name: "orchestrator window at the bounds", key: "METRICS_WINDOW", value: domain.MaxMetricsWindow.String(), load: func() error { _, err := Load(); return err }},
		{name: "orchestrator window too long", key: "METRICS_WINDOW", value: "61m", load: func() error { _, err := Load(); return err }, wantErr: "METRICS_WINDOW must be between 30s and 1h, got 1h1m"},
		{name: "investigator window too short", key: "METRICS_WINDOW", value: "29s", load: func() error { _, err := LoadInvestigator(); return err }, wantErr: "METRICS_WINDOW must be between 30s and 1h, got 29s"},
		{name: "observability window lower bound", key: "METRICS_WINDOW", value: domain.MinMetricsWindow.String(), load: func() error { _, err := LoadObservabilityMCP(); return err }},
		{name: "observability window not a duration", key: "METRICS_WINDOW", value: "five", load: func() error { _, err := LoadObservabilityMCP(); return err }, wantErr: `METRICS_WINDOW must be between 30s and 1h: time: invalid duration "five"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			isolateEnvironment(t, map[string]string{"DATABASE_URL": testDatabaseURL, test.key: test.value})
			err := test.load()
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("%s=%s rejected: %v", test.key, test.value, err)
				}
				return
			}
			if err == nil || err.Error() != test.wantErr {
				t.Fatalf("error = %v, want %q", err, test.wantErr)
			}
		})
	}
}
