package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/mcp/mcptest"
	"github.com/belLena81/rollback-guard/internal/platform/buildinfo"
	"github.com/belLena81/rollback-guard/internal/platform/config"
	"github.com/belLena81/rollback-guard/internal/platform/telemetry"
)

// The catalogs are spelled out, not taken from production code: buildMCP
// must require exactly these tools and nothing else.
var (
	readonlyTools      = []string{"get_deployment", "get_replica_sets", "list_pods", "get_pod_events", "get_rollout_status", "watch_rollout"}
	rollbackTools      = []string{"prepare_rollback", "execute_approved_rollback"}
	observabilityTools = []string{"get_http_error_rate", "compare_error_rate_windows", "get_request_latency", "get_pod_cpu_usage", "get_pod_memory_usage", "evaluate_recovery"}
)

func testTelemetry(t *testing.T) *telemetry.Provider {
	t.Helper()
	tel, err := telemetry.New(t.Context(), telemetry.Settings{ServiceName: "orchestrator", SampleRatio: 1})
	if err != nil {
		t.Fatal(err)
	}
	return tel
}

func mcpConfig(read, write, observability *mcptest.Server) config.Config {
	return config.Config{
		KubernetesReadMCPURL: read.Endpoint, KubernetesWriteMCPURL: write.Endpoint, ObservabilityMCPURL: observability.Endpoint,
		MCPConnectTimeout: 5 * time.Second, MCPCallTimeout: 5 * time.Second, MCPWatchTimeout: 5 * time.Second, Resilience: config.DefaultResilience(),
	}
}

func TestBuildMCPConnectsAllThreeServersAsTheOrchestrator(t *testing.T) {
	read := mcptest.NewServer(t, readonlyTools...)
	write := mcptest.NewServer(t, rollbackTools...)
	observability := mcptest.NewServer(t, observabilityTools...)
	logger := slog.New(slog.DiscardHandler)

	clients, err := buildMCP(context.Background(), mcpConfig(read, write, observability), testTelemetry(t), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer clients.close(logger)

	// The sessions outlive the connect timeout context used to open them.
	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := clients.ping(pingCtx); err != nil {
		t.Fatalf("ping after buildMCP returned: %v", err)
	}
	for name, server := range map[string]*mcptest.Server{"readonly": read, "rollback": write, "observability": observability} {
		seen := server.Clients()
		if len(seen) == 0 {
			t.Fatalf("%s server saw no client", name)
		}
		for _, info := range seen {
			if info.Name != "rollback-guardian-orchestrator" || info.Version != buildinfo.Version {
				t.Fatalf("%s server saw client %+v, want the orchestrator at %s", name, info, buildinfo.Version)
			}
		}
	}
}

func TestBuildMCPFailsClosedOnAnyCatalogMismatch(t *testing.T) {
	tests := []struct {
		name                       string
		read, write, observability []string
		want                       string
	}{
		{name: "readonly server gained a tool", read: append([]string{"exec_in_pod"}, readonlyTools...), write: rollbackTools, observability: observabilityTools, want: "connect readonly kubernetes MCP"},
		{name: "readonly server lost a tool", read: readonlyTools[1:], write: rollbackTools, observability: observabilityTools, want: "connect readonly kubernetes MCP"},
		{name: "rollback server gained a tool", read: readonlyTools, write: append([]string{"apply_yaml"}, rollbackTools...), observability: observabilityTools, want: "connect rollback kubernetes MCP"},
		{name: "rollback server serves the readonly catalog", read: readonlyTools, write: readonlyTools, observability: observabilityTools, want: "connect rollback kubernetes MCP"},
		{name: "observability server lost a tool", read: readonlyTools, write: rollbackTools, observability: observabilityTools[:5], want: "connect observability MCP"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := mcpConfig(mcptest.NewServer(t, test.read...), mcptest.NewServer(t, test.write...), mcptest.NewServer(t, test.observability...))
			logger := slog.New(slog.DiscardHandler)

			clients, err := buildMCP(context.Background(), cfg, testTelemetry(t), logger)

			if err == nil {
				clients.close(logger)
				t.Fatal("buildMCP() succeeded, want a catalog error")
			}
			if !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "MCP tool catalog") {
				t.Fatalf("buildMCP() error = %v, want %q catalog error", err, test.want)
			}
			if clients != (mcpClients{}) {
				t.Fatalf("buildMCP() returned open clients %+v with an error", clients)
			}
		})
	}
}
