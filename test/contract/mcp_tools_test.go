package contract_test

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/kubernetes"
	protocol "github.com/belLena81/rollback-guard/internal/adapters/mcp"
	"github.com/belLena81/rollback-guard/internal/adapters/mcp/kubernetesserver"
	"github.com/belLena81/rollback-guard/internal/adapters/mcp/observabilityserver"
	metricsprom "github.com/belLena81/rollback-guard/internal/adapters/prometheus"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPToolCatalogsAreExactAndNarrow(t *testing.T) {
	kube := &fakeKubernetesBackend{}
	readonly, err := kubernetesserver.New(kube, kubernetesserver.ModeReadonly)
	if err != nil {
		t.Fatal(err)
	}
	rollback, err := kubernetesserver.New(kube, kubernetesserver.ModeRollback)
	if err != nil {
		t.Fatal(err)
	}
	observability, err := observabilityserver.New(fakeMetricsBackend{})
	if err != nil {
		t.Fatal(err)
	}

	// want is spelled out, not taken from production code, so widening a
	// server and its shared catalog together still fails here. The shared
	// catalog is what the orchestrator and investigator require at startup,
	// so it must equal both want and the served tools.
	tests := []struct {
		name    string
		server  *mcp.Server
		catalog []string
		want    []string
	}{
		{name: "kubernetes readonly", server: readonly, catalog: protocol.KubernetesReadonlyCatalog(), want: []string{"get_deployment", "get_replica_sets", "list_pods", "get_pod_events", "get_rollout_status", "watch_rollout"}},
		{name: "kubernetes rollback", server: rollback, catalog: protocol.KubernetesRollbackCatalog(), want: []string{"prepare_rollback", "execute_approved_rollback"}},
		{name: "observability", server: observability, catalog: protocol.ObservabilityCatalog(), want: []string{"get_http_error_rate", "compare_error_rate_windows", "get_request_latency", "get_pod_cpu_usage", "get_pod_memory_usage", "evaluate_recovery"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tools := listTools(t, test.server)
			got := make([]string, len(tools))
			for index, tool := range tools {
				got[index] = tool.Name
				if schema, ok := tool.InputSchema.(map[string]any); ok {
					assertClosedSchema(t, tool.Name, schema)
					if tool.Name == protocol.ToolExecuteApprovedRollback {
						assertExecuteRollbackSchema(t, schema)
					}
					encoded, err := json.Marshal(schema)
					if err != nil {
						t.Fatal(err)
					}
					if strings.Contains(string(encoded), `"query"`) || strings.Contains(string(encoded), `"command"`) || strings.Contains(string(encoded), `"patch"`) {
						t.Fatalf("tool %s exposes an arbitrary-operation input: %s", tool.Name, encoded)
					}
				}
				lower := strings.ToLower(tool.Name)
				for _, prohibited := range []string{"shell", "kubectl", "delete", "pod_exec", "yaml", "promql", "arbitrary_patch"} {
					if strings.Contains(lower, prohibited) {
						t.Fatalf("prohibited tool discovered: %s", tool.Name)
					}
				}
			}
			sort.Strings(got)
			sort.Strings(test.want)
			sort.Strings(test.catalog)
			if !slices.Equal(got, test.want) {
				t.Fatalf("served tools = %q, want %q", got, test.want)
			}
			if !slices.Equal(test.catalog, test.want) {
				t.Fatalf("shared client catalog = %q, want %q", test.catalog, test.want)
			}
		})
	}
}

func assertExecuteRollbackSchema(t *testing.T, schema map[string]any) {
	t.Helper()
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("execute rollback properties = %#v", schema["properties"])
	}
	if len(properties) != 2 || properties["proposalHash"] == nil || properties["idempotencyKey"] == nil {
		t.Fatalf("execute rollback properties = %#v", properties)
	}
	for _, forbidden := range []string{"proposal", "policyDecision", "approval"} {
		if _, exists := properties[forbidden]; exists {
			t.Fatalf("execute rollback exposes caller-asserted %s", forbidden)
		}
	}
}

func assertClosedSchema(t *testing.T, toolName string, value any) {
	t.Helper()
	switch typed := value.(type) {
	case map[string]any:
		if typed["type"] == "object" && typed["additionalProperties"] != false {
			t.Fatalf("tool %s has an open object schema: %#v", toolName, typed)
		}
		for _, child := range typed {
			assertClosedSchema(t, toolName, child)
		}
	case []any:
		for _, child := range typed {
			assertClosedSchema(t, toolName, child)
		}
	}
}

func listTools(t *testing.T, server *mcp.Server) []*mcp.Tool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "contract-test", Version: "v0.3.0"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := clientSession.Close(); err != nil {
			t.Errorf("close MCP client session: %v", err)
		}
		if err := serverSession.Close(); err != nil {
			t.Errorf("close MCP server session: %v", err)
		}
	})
	var result []*mcp.Tool
	for tool, err := range clientSession.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, tool)
	}
	return result
}

type fakeKubernetesBackend struct{}

func (*fakeKubernetesBackend) InspectDeployment(context.Context, domain.DeploymentTarget) (domain.DeploymentEvidence, error) {
	return domain.DeploymentEvidence{}, nil
}
func (*fakeKubernetesBackend) ReplicaSets(context.Context, domain.DeploymentTarget) ([]kubernetes.ReplicaSetSummary, error) {
	return nil, nil
}
func (*fakeKubernetesBackend) ListPods(context.Context, domain.DeploymentTarget, int) ([]kubernetes.PodSummary, bool, error) {
	return nil, false, nil
}
func (*fakeKubernetesBackend) PodEvents(context.Context, domain.DeploymentTarget, string, int) ([]kubernetes.EventSummary, bool, error) {
	return nil, false, nil
}
func (*fakeKubernetesBackend) GetRolloutStatus(context.Context, domain.DeploymentTarget) (kubernetes.RolloutStatus, error) {
	return kubernetes.RolloutStatus{}, nil
}
func (*fakeKubernetesBackend) WatchRollout(context.Context, domain.DeploymentTarget, domain.Revision) (kubernetes.RolloutStatus, error) {
	return kubernetes.RolloutStatus{}, nil
}
func (*fakeKubernetesBackend) PrepareRollback(context.Context, domain.RollbackProposal) error {
	return nil
}
func (*fakeKubernetesBackend) ExecuteApprovedRollback(context.Context, domain.ProposalHash, domain.IdempotencyKey) (kubernetes.MutationResult, error) {
	return kubernetes.MutationResult{}, nil
}

type fakeMetricsBackend struct{}

func (fakeMetricsBackend) QueryMetric(context.Context, domain.DeploymentTarget, metricsprom.QueryRequest) (metricsprom.MetricSample, error) {
	return metricsprom.MetricSample{}, nil
}
