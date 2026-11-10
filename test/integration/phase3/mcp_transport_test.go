//go:build integration

package phase3_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/kubernetes"
	protocol "github.com/belLena81/rollback-guard/internal/adapters/mcp"
	"github.com/belLena81/rollback-guard/internal/adapters/mcp/kubernetesserver"
	"github.com/belLena81/rollback-guard/internal/adapters/mcp/observabilityserver"
	metricsprom "github.com/belLena81/rollback-guard/internal/adapters/prometheus"
	"github.com/belLena81/rollback-guard/internal/domain"
	rgruntime "github.com/belLena81/rollback-guard/internal/platform/runtime"
)

func TestPortsOperateAcrossStreamableHTTPMCP(t *testing.T) {
	now := time.Now().UTC().Add(-time.Second)
	target, err := domain.NewDeploymentTarget("local", "rollback-demo", "demo-api", "demo-api")
	if err != nil {
		t.Fatal(err)
	}
	kube := &kubernetesBackend{now: now, target: target, current: "revision-bad", stable: "revision-stable"}
	readonly := serveKubernetes(t, kube, kubernetesserver.ModeReadonly)
	rollback := serveKubernetes(t, kube, kubernetesserver.ModeRollback)
	metricsServer := serveMetrics(t, metricsBackend{now: now})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpClient := &http.Client{Timeout: 2 * time.Second}
	readClient := connect(t, ctx, readonly.URL+"/mcp", httpClient, []string{protocol.ToolGetDeployment, protocol.ToolGetReplicaSets, protocol.ToolListPods, protocol.ToolGetPodEvents, protocol.ToolGetRolloutStatus, protocol.ToolWatchRollout})
	writeClient := connect(t, ctx, rollback.URL+"/mcp", httpClient, []string{protocol.ToolPrepareRollback, protocol.ToolExecuteApprovedRollback})
	metricsClient := connect(t, ctx, metricsServer.URL+"/mcp", httpClient, []string{protocol.ToolGetHTTPErrorRate, protocol.ToolCompareErrorRateWindows, protocol.ToolGetRequestLatency, protocol.ToolGetPodCPUUsage, protocol.ToolGetPodMemoryUsage, protocol.ToolEvaluateRecovery})

	inspector, err := protocol.NewDeploymentInspector(readClient)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := inspector.InspectDeployment(ctx, target)
	if err != nil || evidence.CurrentRevision != "revision-bad" {
		t.Fatalf("InspectDeployment() = %#v, %v", evidence, err)
	}
	metrics, err := protocol.NewMetricsAdapter(metricsClient, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	symptom, err := domain.NewSymptom("http_error_rate", .35, .10, "ratio", now)
	if err != nil {
		t.Fatal(err)
	}
	metric, err := metrics.Observe(ctx, target, symptom)
	if err != nil || metric.Value != .35 {
		t.Fatalf("Observe() = %#v, %v", metric, err)
	}

	approved := approvedRollback(t, now, target)
	kube.approvedHash = approved.Proposal().Hash()
	executor, err := protocol.NewRollbackExecutor(writeClient, readClient, 5*time.Second, rgruntime.Clock{})
	if err != nil {
		t.Fatal(err)
	}
	execution, err := executor.ExecuteApprovedRollback(ctx, approved, "phase3-integration")
	if err != nil || execution.Outcome() != domain.ExecutionSucceeded || kube.current != kube.stable {
		t.Fatalf("ExecuteApprovedRollback() = %#v, %v; current = %s", execution, err, kube.current)
	}
}

func connect(t *testing.T, ctx context.Context, endpoint string, client *http.Client, tools []string) *protocol.Client {
	t.Helper()
	connected, err := protocol.Connect(ctx, endpoint, client, protocol.ClientOrchestrator)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := connected.Close(); err != nil {
			t.Errorf("close MCP client: %v", err)
		}
	})
	if err := connected.RequireTools(ctx, tools); err != nil {
		t.Fatal(err)
	}
	return connected
}

func serveKubernetes(t *testing.T, backend *kubernetesBackend, mode kubernetesserver.Mode) *httptest.Server {
	t.Helper()
	handler, err := kubernetesserver.NewHTTPHandler(backend, mode, nil, time.Minute, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func serveMetrics(t *testing.T, backend metricsBackend) *httptest.Server {
	t.Helper()
	handler, err := observabilityserver.NewHTTPHandler(backend, nil, time.Minute, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func approvedRollback(t *testing.T, now time.Time, target domain.DeploymentTarget) domain.ApprovedRollback {
	t.Helper()
	decision, err := domain.NewPolicyDecision("policy", "incident", []domain.RuleResult{{Name: "eligible", Passed: true}}, now)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := domain.NewRollbackProposal("action", "incident", decision.ID(), target, "revision-bad", "revision-stable", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	actor, err := domain.NewActor("operator", domain.ActorHuman)
	if err != nil {
		t.Fatal(err)
	}
	approval, err := domain.NewApproval("approval", actor, domain.ApprovalApproved, proposal.ActionID(), proposal.Hash(), now, now.Add(time.Minute), "")
	if err != nil {
		t.Fatal(err)
	}
	approved, err := domain.AuthorizeRollback(proposal, decision, approval, domain.DeploymentEvidence{Target: target, CurrentRevision: "revision-bad"}, now)
	if err != nil {
		t.Fatal(err)
	}
	return approved
}

type kubernetesBackend struct {
	now             time.Time
	target          domain.DeploymentTarget
	current, stable domain.Revision
	approvedHash    domain.ProposalHash
}

func (b *kubernetesBackend) InspectDeployment(context.Context, domain.DeploymentTarget) (domain.DeploymentEvidence, error) {
	healthy := b.current == b.stable
	ready := 1
	if !healthy {
		ready = 0
	}
	return domain.DeploymentEvidence{Target: b.target, CurrentRevision: b.current, PreviousStableRevision: b.stable, DesiredReplicas: 1, ReadyReplicas: ready, AvailableReplicas: ready, Healthy: healthy, RolloutAt: b.now.Add(-time.Minute)}, nil
}
func (*kubernetesBackend) ReplicaSets(context.Context, domain.DeploymentTarget) ([]kubernetes.ReplicaSetSummary, error) {
	return nil, nil
}
func (*kubernetesBackend) ListPods(context.Context, domain.DeploymentTarget, int) ([]kubernetes.PodSummary, bool, error) {
	return nil, false, nil
}
func (*kubernetesBackend) PodEvents(context.Context, domain.DeploymentTarget, string, int) ([]kubernetes.EventSummary, bool, error) {
	return nil, false, nil
}
func (b *kubernetesBackend) GetRolloutStatus(context.Context, domain.DeploymentTarget) (kubernetes.RolloutStatus, error) {
	return b.status(), nil
}
func (b *kubernetesBackend) WatchRollout(context.Context, domain.DeploymentTarget, domain.Revision) (kubernetes.RolloutStatus, error) {
	return b.status(), nil
}
func (*kubernetesBackend) PrepareRollback(context.Context, domain.RollbackProposal) error { return nil }

func (b *kubernetesBackend) ExecuteApprovedRollback(_ context.Context, hash domain.ProposalHash, _ domain.IdempotencyKey) (kubernetes.MutationResult, error) {
	if hash != b.approvedHash {
		return kubernetes.MutationResult{}, &domain.ApprovalMismatchError{Reason: "trusted execution record is missing"}
	}
	from := b.current
	b.current = b.stable
	return kubernetes.MutationResult{ExecutionID: "mcp-execution", FromRevision: from, ToRevision: b.stable, StartedAt: b.now, CompletedAt: b.now}, nil
}
func (b *kubernetesBackend) status() kubernetes.RolloutStatus {
	return kubernetes.RolloutStatus{Revision: b.current, DesiredReplicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1, ObservedGeneration: 2, Generation: 2, Complete: b.current == b.stable}
}

type metricsBackend struct{ now time.Time }

func (b metricsBackend) QueryMetric(_ context.Context, _ domain.DeploymentTarget, request metricsprom.QueryRequest) (metricsprom.MetricSample, error) {
	return metricsprom.MetricSample{Name: request.Kind.String(), Value: .35, Unit: "ratio", ObservedAt: b.now}, nil
}
