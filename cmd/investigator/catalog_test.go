package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/kubernetes"
	"github.com/belLena81/rollback-guard/internal/adapters/mcp/kubernetesserver"
	"github.com/belLena81/rollback-guard/internal/adapters/mcp/observabilityserver"
	metricsprom "github.com/belLena81/rollback-guard/internal/adapters/prometheus"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/config"
)

// These tests run buildMCP against the real MCP servers rather than
// single-tool stand-ins. The investigator used to require a one-tool catalog
// from servers that serve six tools each, so it could never start against
// them, and the stand-in tests agreed with the wrong catalog.

// realServers holds the Streamable HTTP endpoints of in-process instances of
// the three production MCP servers over fake backends.
type realServers struct {
	readonly, rollback, observability string
}

func startRealServers(t *testing.T) realServers {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	serve := func(handler http.Handler, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)
		return server.URL + "/mcp"
	}
	return realServers{
		readonly:      serve(kubernetesserver.NewHTTPHandler(fakeKubernetes{}, kubernetesserver.ModeReadonly, nil, time.Minute, logger)),
		rollback:      serve(kubernetesserver.NewHTTPHandler(fakeKubernetes{}, kubernetesserver.ModeRollback, nil, time.Minute, logger)),
		observability: serve(observabilityserver.NewHTTPHandler(fakeMetrics{}, nil, time.Minute, logger)),
	}
}

func realServerConfig(read, observability string) config.InvestigatorConfig {
	return config.InvestigatorConfig{
		KubernetesReadMCPURL: read, ObservabilityMCPURL: observability,
		MCPConnectTimeout: 5 * time.Second, MCPCallTimeout: 5 * time.Second,
	}
}

func TestBuildMCPConnectsToTheRealReadonlyAndObservabilityServers(t *testing.T) {
	servers := startRealServers(t)
	logger := slog.New(slog.DiscardHandler)

	clients, err := buildMCP(context.Background(), realServerConfig(servers.readonly, servers.observability), testTelemetry(t), logger)
	if err != nil {
		t.Fatalf("buildMCP() against the real servers: %v", err)
	}
	defer clients.close(logger)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := clients.ping(ctx); err != nil {
		t.Fatalf("ping after buildMCP returned: %v", err)
	}
}

// The investigator has no rollback endpoint; pointed at the rollback server
// by misconfiguration, it must fail closed instead of opening a session.
func TestBuildMCPRefusesTheRollbackServer(t *testing.T) {
	servers := startRealServers(t)
	tests := []struct {
		name                string
		read, observability string
		want                string
	}{
		{name: "as the readonly server", read: servers.rollback, observability: servers.observability, want: "connect readonly kubernetes MCP"},
		{name: "as the observability server", read: servers.readonly, observability: servers.rollback, want: "connect observability MCP"},
		{name: "servers swapped", read: servers.observability, observability: servers.readonly, want: "connect readonly kubernetes MCP"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logger := slog.New(slog.DiscardHandler)
			clients, err := buildMCP(context.Background(), realServerConfig(test.read, test.observability), testTelemetry(t), logger)
			if err == nil {
				clients.close(logger)
				t.Fatal("buildMCP() succeeded, want a catalog error")
			}
			if !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "MCP tool catalog") {
				t.Fatalf("buildMCP() error = %v, want a %q catalog error", err, test.want)
			}
			if clients != (mcpClients{}) {
				t.Fatalf("buildMCP() returned open clients %+v with an error", clients)
			}
		})
	}
}

// fakeKubernetes backs the real Kubernetes MCP server; the catalog check
// only lists tools, so no method is called.
type fakeKubernetes struct{}

func (fakeKubernetes) InspectDeployment(context.Context, domain.DeploymentTarget) (domain.DeploymentEvidence, error) {
	return domain.DeploymentEvidence{}, nil
}
func (fakeKubernetes) ReplicaSets(context.Context, domain.DeploymentTarget) ([]kubernetes.ReplicaSetSummary, error) {
	return nil, nil
}
func (fakeKubernetes) ListPods(context.Context, domain.DeploymentTarget, int) ([]kubernetes.PodSummary, bool, error) {
	return nil, false, nil
}
func (fakeKubernetes) PodEvents(context.Context, domain.DeploymentTarget, string, int) ([]kubernetes.EventSummary, bool, error) {
	return nil, false, nil
}
func (fakeKubernetes) GetRolloutStatus(context.Context, domain.DeploymentTarget) (kubernetes.RolloutStatus, error) {
	return kubernetes.RolloutStatus{}, nil
}
func (fakeKubernetes) WatchRollout(context.Context, domain.DeploymentTarget, domain.Revision) (kubernetes.RolloutStatus, error) {
	return kubernetes.RolloutStatus{}, nil
}
func (fakeKubernetes) PrepareRollback(context.Context, domain.RollbackProposal) error {
	return nil
}
func (fakeKubernetes) ExecuteApprovedRollback(context.Context, domain.ProposalHash, domain.IdempotencyKey) (kubernetes.MutationResult, error) {
	return kubernetes.MutationResult{}, nil
}

// fakeMetrics backs the real observability MCP server.
type fakeMetrics struct{}

func (fakeMetrics) QueryMetric(context.Context, domain.DeploymentTarget, metricsprom.QueryRequest) (metricsprom.MetricSample, error) {
	return metricsprom.MetricSample{}, nil
}
