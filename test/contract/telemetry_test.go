package contract_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/belLena81/rollback-guard/internal/adapters/fake"
	"github.com/belLena81/rollback-guard/internal/adapters/httpapi"
	protocol "github.com/belLena81/rollback-guard/internal/adapters/mcp"
	"github.com/belLena81/rollback-guard/internal/adapters/mcp/kubernetesserver"
	"github.com/belLena81/rollback-guard/internal/adapters/mcp/observabilityserver"
	"github.com/belLena81/rollback-guard/internal/adapters/memory"
	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/telemetry"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/belLena81/rollback-guard/internal/testkit"
)

func TestOpenAPIDescribesPrometheusMetricsEndpoint(t *testing.T) {
	data, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.UnmarshalStrict(data, &document); err != nil {
		t.Fatalf("parse OpenAPI: %v", err)
	}
	paths := requireObject(t, document, "paths")
	metrics := requireObject(t, paths, "/metrics")
	get := requireObject(t, metrics, "get")
	responses := requireObject(t, get, "responses")
	ok := requireObject(t, responses, "200")
	content := requireObject(t, ok, "content")
	if _, exists := content["text/plain"]; !exists || len(content) != 1 {
		t.Fatalf("/metrics content = %v, want only text/plain", content)
	}
	if description, isString := ok["description"].(string); !isString || strings.Contains(strings.ToLower(description), "placeholder") {
		t.Fatalf("/metrics is still documented as a placeholder")
	}
}

func TestOrchestratorMetricsEndpointMatchesContract(t *testing.T) {
	provider, err := telemetry.New(telemetry.Settings{ServiceName: "orchestrator", SampleRatio: 1})
	if err != nil {
		t.Fatal(err)
	}
	server, err := httpapi.NewServer(contractServices(t), slog.New(slog.DiscardHandler), httpapi.WithRouteMiddleware(provider.Route), httpapi.WithMetricsHandler(provider.MetricsHandler()))
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Routes(nil, time.Second, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health/live", nil))
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("GET /metrics = %d %q", recorder.Code, recorder.Header().Get("Content-Type"))
	}
	for _, want := range []string{"rollback_guardian_incidents_total 0", `rollback_guardian_http_server_requests_total{code="200",method="GET",route="/health/live"} 1`} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics body lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Placeholder") {
		t.Fatal("placeholder metrics are still served")
	}
	// Instrumentation must not change the mux's method handling.
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/v1/incidents", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT /v1/incidents = %d, want 405", recorder.Code)
	}
}

// TestMCPTelemetryOptionsKeepToolCatalogs proves instrumentation adds a
// /metrics HTTP route but never a tool.
func TestMCPTelemetryOptionsKeepToolCatalogs(t *testing.T) {
	provider, err := telemetry.New(telemetry.Settings{ServiceName: "mcp", SampleRatio: 1})
	if err != nil {
		t.Fatal(err)
	}
	options := func(server string) []protocol.HandlerOption {
		return []protocol.HandlerOption{protocol.WithMetricsHandler(provider.MetricsHandler()), protocol.WithToolObserver(server, provider)}
	}
	readonly, err := kubernetesserver.NewHTTPHandler(&fakeKubernetesBackend{}, kubernetesserver.ModeReadonly, nil, time.Minute, nil, options(protocol.ServerKubernetesReadonly)...)
	if err != nil {
		t.Fatal(err)
	}
	rollback, err := kubernetesserver.NewHTTPHandler(&fakeKubernetesBackend{}, kubernetesserver.ModeRollback, nil, time.Minute, nil, options(protocol.ServerKubernetesRollback)...)
	if err != nil {
		t.Fatal(err)
	}
	observability, err := observabilityserver.NewHTTPHandler(fakeMetricsBackend{}, nil, time.Minute, nil, options(protocol.ServerObservability)...)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		handler http.Handler
		want    []string
	}{
		{name: "kubernetes readonly", handler: readonly, want: []string{protocol.ToolGetDeployment, protocol.ToolGetReplicaSets, protocol.ToolListPods, protocol.ToolGetPodEvents, protocol.ToolGetRolloutStatus, protocol.ToolWatchRollout}},
		{name: "kubernetes rollback", handler: rollback, want: []string{protocol.ToolPrepareRollback, protocol.ToolExecuteApprovedRollback}},
		{name: "observability", handler: observability, want: []string{protocol.ToolGetHTTPErrorRate, protocol.ToolCompareErrorRateWindows, protocol.ToolGetRequestLatency, protocol.ToolGetPodCPUUsage, protocol.ToolGetPodMemoryUsage, protocol.ToolEvaluateRecovery}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(provider.InstrumentServer(test.handler, protocol.RouteOf))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, err := protocol.Connect(ctx, server.URL+"/mcp", provider.HTTPClient(5*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			defer closeOrFail(t, client)
			if err := client.RequireTools(ctx, test.want); err != nil {
				t.Fatal(err)
			}
			// The catalog check fails closed on any difference from the served tools.
			for name, catalog := range map[string][]string{
				"server serves a tool the client does not expect": test.want[1:],
				"server lacks a tool the client expects":          append(slices.Clone(test.want), "generic_shell"),
				"expected names joined by a space":                {strings.Join(test.want, " ")},
			} {
				if err := client.RequireTools(ctx, catalog); err == nil {
					t.Fatalf("RequireTools() accepted a mismatching catalog: %s", name)
				}
			}
			response, err := http.Get(server.URL + "/metrics")
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(response.Body)
			// Closed inline: the bodyclose linter cannot see through closeOrFail.
			if closeErr := response.Body.Close(); closeErr != nil {
				t.Errorf("close: %v", closeErr)
			}
			if err != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(data), "rollback_guardian_http_server_requests_total") {
				t.Fatalf("GET /metrics = %d, %v", response.StatusCode, err)
			}
			post, err := http.Post(server.URL+"/metrics", "text/plain", nil)
			if err != nil {
				t.Fatal(err)
			}
			if closeErr := post.Body.Close(); closeErr != nil {
				t.Errorf("close: %v", closeErr)
			}
			if post.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("POST /metrics = %d, want 405", post.StatusCode)
			}
		})
	}
}

func closeOrFail(t *testing.T, closer io.Closer) {
	t.Helper()
	if err := closer.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
}

type noDiagnoser struct{}

func (noDiagnoser) Diagnose(context.Context, ports.InvestigationInput) (domain.InvestigationConclusion, error) {
	return domain.InvestigationConclusion{}, context.Canceled
}

func contractServices(t *testing.T) application.Services {
	t.Helper()
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	target, err := domain.NewDeploymentTarget("local", "rollback-demo", "demo-api", "demo-api")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := domain.NewEligibilityPolicy(domain.EligibilityPolicyConfig{MinimumConfidence: 0.8, CorrelationWindow: 10 * time.Minute, AllowedTargets: []domain.DeploymentTarget{target}})
	if err != nil {
		t.Fatal(err)
	}
	system, err := domain.NewActor("rollback-guardian", domain.ActorSystem)
	if err != nil {
		t.Fatal(err)
	}
	infra := &fake.Infrastructure{}
	services, err := application.NewServices(application.Dependencies{UnitOfWork: memory.NewUnitOfWork(), Inspector: infra, Metrics: infra, Diagnoser: noDiagnoser{}, Executor: infra, RollbackPolicy: policy, ApprovalPolicy: domain.ApprovalAuthorizer{}, Clock: testkit.NewClock(now), IDGenerator: &testkit.IDGenerator{}},
		application.WorkflowConfig{ProposalTTL: time.Minute, ApprovalTTL: time.Minute, CleanupTimeout: time.Second, SystemActor: system})
	if err != nil {
		t.Fatal(err)
	}
	return services
}
