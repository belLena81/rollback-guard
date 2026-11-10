// Package phase6_test proves the telemetry exit criteria in-process: one
// trace links a REST request, the asynchronous coordinator, the A2A hop, the
// Investigator, and its MCP hops; W3C trace context is on every A2A and MCP
// request; logs carry correlation fields and no forbidden content; and each
// process exposes its own /metrics.
package phase6_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	a2aadapter "github.com/belLena81/rollback-guard/internal/adapters/a2a"
	"github.com/belLena81/rollback-guard/internal/adapters/diagnosis"
	"github.com/belLena81/rollback-guard/internal/adapters/fake"
	"github.com/belLena81/rollback-guard/internal/adapters/httpapi"
	"github.com/belLena81/rollback-guard/internal/adapters/kubernetes"
	protocol "github.com/belLena81/rollback-guard/internal/adapters/mcp"
	"github.com/belLena81/rollback-guard/internal/adapters/mcp/kubernetesserver"
	"github.com/belLena81/rollback-guard/internal/adapters/mcp/observabilityserver"
	"github.com/belLena81/rollback-guard/internal/adapters/memory"
	metricsprom "github.com/belLena81/rollback-guard/internal/adapters/prometheus"
	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/httpserver"
	"github.com/belLena81/rollback-guard/internal/platform/runtime"
	"github.com/belLena81/rollback-guard/internal/platform/telemetry"
)

func TestTelemetryLinksRESTThroughA2AAndMCP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	spans := tracetest.NewInMemoryExporter()
	now := time.Now().UTC()
	target, err := domain.NewDeploymentTarget("local", "rollback-demo", "demo-api", "demo-api")
	if err != nil {
		t.Fatal(err)
	}

	// MCP servers.
	kubeProcess := newProcess(t, "kubernetes-mcp-readonly", spans)
	kubeHandler, err := kubernetesserver.NewHTTPHandler(&kubernetesBackend{now: now, target: target}, kubernetesserver.ModeReadonly, nil, time.Minute, kubeProcess.logger,
		protocol.WithMetricsHandler(kubeProcess.telemetry.MetricsHandler()), protocol.WithToolObserver(protocol.ServerKubernetesReadonly, kubeProcess.telemetry))
	if err != nil {
		t.Fatal(err)
	}
	kubeServer := kubeProcess.serve(kubeProcess.telemetry.InstrumentServer(kubeHandler, protocol.RouteOf))
	observabilityProcess := newProcess(t, "observability-mcp", spans)
	observabilityHandler, err := observabilityserver.NewHTTPHandler(metricsBackend{now: now}, nil, time.Minute, observabilityProcess.logger,
		protocol.WithMetricsHandler(observabilityProcess.telemetry.MetricsHandler()), protocol.WithToolObserver(protocol.ServerObservability, observabilityProcess.telemetry))
	if err != nil {
		t.Fatal(err)
	}
	observabilityServer := observabilityProcess.serve(observabilityProcess.telemetry.InstrumentServer(observabilityHandler, protocol.RouteOf))

	// Investigator.
	investigatorProcess := newProcess(t, "investigator", spans)
	ids := &runtime.IDGenerator{}
	readCaller := connectMCP(t, ctx, investigatorProcess.telemetry, kubeServer.URL+"/mcp", protocol.ServerKubernetesReadonly, []string{protocol.ToolGetDeployment, protocol.ToolGetReplicaSets, protocol.ToolListPods, protocol.ToolGetPodEvents, protocol.ToolGetRolloutStatus, protocol.ToolWatchRollout})
	metricsCaller := connectMCP(t, ctx, investigatorProcess.telemetry, observabilityServer.URL+"/mcp", protocol.ServerObservability, []string{protocol.ToolGetHTTPErrorRate, protocol.ToolCompareErrorRateWindows, protocol.ToolGetRequestLatency, protocol.ToolGetPodCPUUsage, protocol.ToolGetPodMemoryUsage, protocol.ToolEvaluateRecovery})
	inspector, err := protocol.NewDeploymentInspector(readCaller)
	if err != nil {
		t.Fatal(err)
	}
	metricsAdapter, err := protocol.NewMetricsAdapter(metricsCaller, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := diagnosis.NewRuleDiagnoser(ids, diagnosis.RuleDiagnoserConfig{CorrelationWindow: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	diagnoser, err := investigatorProcess.telemetry.ObserveDiagnoser(rules, domain.StrategyRules)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := application.NewInvestigationRunner(inspector, metricsAdapter, diagnoser, runtime.Clock{}, ids)
	if err != nil {
		t.Fatal(err)
	}
	var a2aHandler http.Handler
	investigatorServer := investigatorProcess.serve(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { a2aHandler.ServeHTTP(w, r) }))
	a2aServer, err := a2aadapter.NewServerHandler(runner, nil, investigatorProcess.logger, a2aadapter.ServerConfig{PublicURL: investigatorServer.URL, TaskTimeout: 20 * time.Second, Tracer: investigatorProcess.telemetry, Metrics: investigatorProcess.telemetry.MetricsHandler()})
	if err != nil {
		t.Fatal(err)
	}
	a2aHandler = investigatorProcess.telemetry.InstrumentServer(a2aServer, a2aadapter.RouteOf)

	// Orchestrator.
	orchestratorProcess := newProcess(t, "orchestrator", spans)
	orchestrator := orchestratorProcess.telemetry
	uow, err := orchestrator.ObserveUnitOfWork(memory.NewUnitOfWork())
	if err != nil {
		t.Fatal(err)
	}
	infra := &fake.Infrastructure{}
	policy := must(domain.NewEligibilityPolicy(domain.EligibilityPolicyConfig{MinimumConfidence: 0.8, CorrelationWindow: 10 * time.Minute, AllowedTargets: []domain.DeploymentTarget{target}}))
	system := must(domain.NewActor("rollback-guardian", domain.ActorSystem))
	human := must(domain.NewActor("local-admin", domain.ActorHuman))
	services, err := application.NewServices(application.Dependencies{UnitOfWork: uow, Inspector: infra, Metrics: infra, Diagnoser: rules, Executor: infra, RollbackPolicy: policy, ApprovalPolicy: domain.ApprovalAuthorizer{}, Clock: runtime.Clock{}, IDGenerator: ids, Tracer: orchestrator},
		application.WorkflowConfig{ProposalTTL: 15 * time.Minute, ApprovalTTL: 10 * time.Minute, CleanupTimeout: time.Second, SystemActor: system})
	if err != nil {
		t.Fatal(err)
	}
	a2aGateway, err := a2aadapter.NewGateway(ctx, investigatorServer.URL, orchestrator.HTTPClient(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeOrFail(t, a2aGateway) })
	gateway, err := orchestrator.ObserveGateway(a2aGateway)
	if err != nil {
		t.Fatal(err)
	}
	var reported []error
	coordinator, err := application.NewInvestigationCoordinator(uow, gateway, services.Investigations, runtime.Clock{}, application.InvestigationCoordinatorConfig{
		WorkerID: "worker-1", TaskTimeout: 20 * time.Second, CancelTimeout: time.Second, Tracer: orchestrator,
		ReportError: func(ctx context.Context, err error) {
			reported = append(reported, err)
			orchestratorProcess.logger.ErrorContext(ctx, "investigation coordinator iteration failed", "error", err)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	auth, err := httpserver.NewLocalAuthMiddleware(human)
	if err != nil {
		t.Fatal(err)
	}
	api, err := httpapi.NewServer(services, orchestratorProcess.logger, httpapi.WithRouteMiddleware(orchestrator.Route), httpapi.WithMetricsHandler(orchestrator.MetricsHandler()))
	if err != nil {
		t.Fatal(err)
	}
	restServer := orchestratorProcess.serve(api.Routes(nil, time.Second, auth))

	// A REST request starts the trace; the coordinator continues it later.
	body := `{"target":{"cluster":"local","namespace":"rollback-demo","deployment":"demo-api","service":"demo-api"},"symptom":{"kind":"http_error_rate","observed":0.35,"threshold":0.1,"unit":"ratio","startedAt":"` + now.Add(-30*time.Second).Format(time.RFC3339Nano) + `"}}`
	response, err := http.Post(restServer.URL+"/v1/incidents", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		Incident struct {
			ID string `json:"id"`
		} `json:"incident"`
	}
	decodeErr := json.NewDecoder(response.Body).Decode(&created)
	// Closed inline: the bodyclose linter cannot see through closeOrFail.
	if closeErr := response.Body.Close(); closeErr != nil {
		t.Errorf("close: %v", closeErr)
	}
	if response.StatusCode != http.StatusAccepted || decodeErr != nil {
		t.Fatalf("create incident = %d, %v", response.StatusCode, decodeErr)
	}
	if err := coordinator.Run(withDeadline(t, ctx, coordinatorDone(t, services, created.Incident.ID))); err != nil && !strings.Contains(err.Error(), "context canceled") {
		t.Fatal(err)
	}
	if len(reported) != 0 {
		t.Fatalf("coordinator reported %v", reported)
	}
	summary, err := services.Queries.GetIncidentSummary(ctx, domain.IncidentID(created.Incident.ID))
	if err != nil || summary.Incident.Status() != domain.StatusAwaitingApproval {
		t.Fatalf("incident after investigation = %#v, %v", summary.Incident.Status(), err)
	}

	// Spans: one trace from the REST request through every hop.
	restSpan := findSpan(t, spans, "orchestrator", "POST /v1/incidents")
	traceID := restSpan.SpanContext.TraceID()
	dispatch := findSpan(t, spans, "orchestrator", "investigation.dispatch")
	if dispatch.SpanContext.TraceID() != traceID || dispatch.Parent.SpanID() != restSpan.SpanContext.SpanID() {
		t.Fatalf("coordinator span is not a child of the REST span: parent %s, rest %s", dispatch.Parent.SpanID(), restSpan.SpanContext.SpanID())
	}
	for _, want := range []struct{ service, name string }{
		{"orchestrator", "a2a submit"}, {"orchestrator", "a2a watch"},
		{"investigator", "POST /a2a/*"}, {"investigator", "investigation.execute"}, {"investigator", "diagnosis"},
		{"investigator", "mcp call get_deployment"}, {"investigator", "mcp call get_http_error_rate"},
		{"kubernetes-mcp-readonly", "mcp tool get_deployment"}, {"observability-mcp", "mcp tool get_http_error_rate"},
	} {
		span := findSpan(t, spans, want.service, want.name)
		if span.SpanContext.TraceID() != traceID {
			t.Errorf("%s span %q is in trace %s, want %s", want.service, want.name, span.SpanContext.TraceID(), traceID)
		}
	}
	// Parent links, not just a shared trace ID, across both hops.
	for _, link := range []struct{ service, name, ancestorService, ancestorName string }{
		{"kubernetes-mcp-readonly", "mcp tool get_deployment", "investigator", "mcp call get_deployment"},
		{"observability-mcp", "mcp tool get_http_error_rate", "investigator", "mcp call get_http_error_rate"},
		{"investigator", "mcp call get_deployment", "investigator", "investigation.execute"},
		{"investigator", "investigation.execute", "orchestrator", "investigation.dispatch"},
		{"orchestrator", "a2a submit", "orchestrator", "investigation.dispatch"},
	} {
		span := findSpan(t, spans, link.service, link.name)
		if !hasAncestor(spans, span, link.ancestorService, link.ancestorName) {
			t.Errorf("%s:%q does not descend from %s:%q", link.service, link.name, link.ancestorService, link.ancestorName)
		}
	}

	// Propagation headers on the A2A and MCP hops.
	if !investigatorProcess.sawTraceparent(traceID.String(), "/a2a/") {
		t.Errorf("no A2A request carried traceparent for trace %s", traceID)
	}
	if !kubeProcess.sawTraceparent(traceID.String(), "/mcp") || !observabilityProcess.sawTraceparent(traceID.String(), "/mcp") {
		t.Errorf("MCP requests did not carry traceparent for trace %s", traceID)
	}

	// Logs: correlation fields on every path, no forbidden content anywhere.
	incidentID := created.Incident.ID
	requireLog(t, orchestratorProcess, "incident created", map[string]string{"incident_id": incidentID, "trace_id": traceID.String()}, "operation_id", "idempotency_key")
	requireLog(t, orchestratorProcess, "http request completed", map[string]string{"route": "/v1/incidents", "trace_id": traceID.String()})
	requireLog(t, orchestratorProcess, "a2a call completed", map[string]string{"incident_id": incidentID, "trace_id": traceID.String(), "operation": "submit"}, "operation_id", "idempotency_key", "task_id")
	requireLog(t, investigatorProcess, "investigation task started", map[string]string{"incident_id": incidentID, "trace_id": traceID.String()}, "operation_id", "idempotency_key", "task_id")
	requireLog(t, investigatorProcess, "mcp tool call", map[string]string{"incident_id": incidentID, "trace_id": traceID.String(), "tool": "get_deployment"}, "task_id")
	requireLog(t, kubeProcess, "mcp tool handled", map[string]string{"trace_id": traceID.String(), "tool": "get_deployment", "outcome": "ok"})
	for _, process := range []*process{orchestratorProcess, investigatorProcess, kubeProcess, observabilityProcess} {
		output := process.logs.String()
		for _, forbidden := range []string{"postgres://", "Authorization", "Bearer", "traceparent", "hypothesis", "prompt", "\"evidence\""} {
			if strings.Contains(output, forbidden) {
				t.Errorf("%s logs contain forbidden content %q", process.name, forbidden)
			}
		}
	}

	// Each process exposes its own metrics.
	requireMetric(t, restServer.URL, `rollback_guardian_incidents_total 1`)
	requireMetric(t, restServer.URL, `rollback_guardian_a2a_calls_total{operation="submit",outcome="ok"} 1`)
	requireMetric(t, restServer.URL, `rollback_guardian_http_server_requests_total{code="202",method="POST",route="/v1/incidents"} 1`)
	requireMetric(t, investigatorServer.URL, `rollback_guardian_mcp_calls_total{outcome="ok",server="kubernetes-readonly",tool="get_deployment"} 1`)
	requireMetric(t, investigatorServer.URL, `rollback_guardian_diagnoses_total{effective_strategy="rules",fallback_reason="none",requested_strategy="rules"} 1`)
	requireMetric(t, kubeServer.URL, `rollback_guardian_mcp_tool_requests_total{outcome="ok",server="kubernetes-readonly",tool="get_deployment"} 1`)
	requireMetric(t, observabilityServer.URL, `rollback_guardian_mcp_tool_requests_total{outcome="ok",server="observability",tool="get_http_error_rate"} 1`)
}

// process is one simulated service: its own telemetry provider, logs, and a
// record of incoming request headers.
type process struct {
	name      string
	telemetry *telemetry.Provider
	logger    *slog.Logger
	logs      *syncBuffer
	mu        sync.Mutex
	headers   []capturedRequest
}

type capturedRequest struct {
	path   string
	header http.Header
}

func newProcess(t *testing.T, name string, spans *tracetest.InMemoryExporter) *process {
	t.Helper()
	logs := &syncBuffer{}
	logger := telemetry.NewLogger(logs, slog.LevelInfo)
	provider, err := telemetry.New(telemetry.Settings{ServiceName: name, SampleRatio: 1}, telemetry.WithSyncExporter(spans), telemetry.WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown %s telemetry: %v", name, err)
		}
	})
	return &process{name: name, telemetry: provider, logger: logger, logs: logs}
}

func (p *process) serve(handler http.Handler) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.headers = append(p.headers, capturedRequest{path: r.URL.Path, header: r.Header.Clone()})
		p.mu.Unlock()
		handler.ServeHTTP(w, r)
	}))
	return server
}

func (p *process) sawTraceparent(traceID string, pathPrefix string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, request := range p.headers {
		if strings.HasPrefix(request.path, pathPrefix) && strings.Contains(request.header.Get("traceparent"), traceID) {
			return true
		}
	}
	return false
}

type syncBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *syncBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(data)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func connectMCP(t *testing.T, ctx context.Context, provider *telemetry.Provider, endpoint string, server string, tools []string) protocol.Caller {
	t.Helper()
	client, err := protocol.Connect(ctx, endpoint, provider.HTTPClient(10*time.Second), protocol.ClientOrchestrator)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeOrFail(t, client) })
	if err := client.RequireTools(ctx, tools); err != nil {
		t.Fatal(err)
	}
	bounded, err := protocol.BoundCalls(client, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := protocol.ObserveCalls(bounded, server, provider)
	if err != nil {
		t.Fatal(err)
	}
	return observed
}

// coordinatorDone returns a predicate that becomes true once the incident has
// left investigation.
func coordinatorDone(t *testing.T, services application.Services, incidentID string) func() bool {
	t.Helper()
	return func() bool {
		summary, err := services.Queries.GetIncidentSummary(context.Background(), domain.IncidentID(incidentID))
		return err == nil && summary.Incident.Status() != domain.StatusInvestigating
	}
}

// withDeadline returns a context canceled once done reports true.
func withDeadline(t *testing.T, parent context.Context, done func() bool) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	t.Cleanup(cancel)
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if done() {
					cancel()
					return
				}
			}
		}
	}()
	return ctx
}

// findSpan waits briefly for the span: server and executor spans end after
// the response that unblocked the test has been written.
func findSpan(t *testing.T, spans *tracetest.InMemoryExporter, service string, name string) tracetest.SpanStub {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, span := range spans.GetSpans() {
			if span.Name == name && serviceOf(span) == service {
				return span
			}
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	var names []string
	for _, span := range spans.GetSpans() {
		names = append(names, serviceOf(span)+":"+span.Name)
	}
	t.Fatalf("span %s:%q not found among %v", service, name, names)
	return tracetest.SpanStub{}
}

func serviceOf(span tracetest.SpanStub) string {
	for _, attribute := range span.Resource.Attributes() {
		if attribute.Key == "service.name" {
			return attribute.Value.AsString()
		}
	}
	return ""
}

func hasAncestor(spans *tracetest.InMemoryExporter, span tracetest.SpanStub, service string, name string) bool {
	byID := map[string]tracetest.SpanStub{}
	for _, candidate := range spans.GetSpans() {
		byID[candidate.SpanContext.SpanID().String()] = candidate
	}
	for current, ok := byID[span.Parent.SpanID().String()]; ok; current, ok = byID[current.Parent.SpanID().String()] {
		if serviceOf(current) == service && current.Name == name {
			return true
		}
	}
	return false
}

func requireLog(t *testing.T, p *process, message string, fields map[string]string, present ...string) {
	t.Helper()
	for _, line := range strings.Split(p.logs.String(), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("%s log line is not JSON: %q", p.name, line)
		}
		if record["msg"] != message {
			continue
		}
		matched := true
		for key, value := range fields {
			if record[key] != value {
				matched = false
			}
		}
		for _, key := range present {
			if value, ok := record[key].(string); !ok || value == "" {
				matched = false
			}
		}
		if matched {
			return
		}
	}
	t.Errorf("%s has no %q log with %v and %v:\n%s", p.name, message, fields, present, p.logs.String())
}

func requireMetric(t *testing.T, baseURL string, series string) {
	t.Helper()
	response, err := http.Get(baseURL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, response.Body)
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), series+"\n") {
		t.Errorf("%s/metrics lacks %s", baseURL, series)
	}
}

func closeOrFail(t *testing.T, closer io.Closer) {
	t.Helper()
	if err := closer.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
}

// must unwraps constructor results for constant test fixtures.
func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

type kubernetesBackend struct {
	now    time.Time
	target domain.DeploymentTarget
}

func (b *kubernetesBackend) InspectDeployment(context.Context, domain.DeploymentTarget) (domain.DeploymentEvidence, error) {
	return domain.DeploymentEvidence{Target: b.target, CurrentRevision: "revision-bad", PreviousStableRevision: "revision-stable", DesiredReplicas: 1, ReadyReplicas: 0, AvailableReplicas: 0, RolloutAt: b.now.Add(-time.Minute)}, nil
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
func (*kubernetesBackend) GetRolloutStatus(context.Context, domain.DeploymentTarget) (kubernetes.RolloutStatus, error) {
	return kubernetes.RolloutStatus{}, nil
}
func (*kubernetesBackend) WatchRollout(context.Context, domain.DeploymentTarget, domain.Revision) (kubernetes.RolloutStatus, error) {
	return kubernetes.RolloutStatus{}, nil
}
func (*kubernetesBackend) PrepareRollback(context.Context, domain.RollbackProposal) error { return nil }
func (*kubernetesBackend) ExecuteApprovedRollback(context.Context, domain.ProposalHash, domain.IdempotencyKey) (kubernetes.MutationResult, error) {
	return kubernetes.MutationResult{}, &domain.ApprovalMismatchError{Reason: "not used"}
}

type metricsBackend struct{ now time.Time }

func (b metricsBackend) QueryMetric(_ context.Context, _ domain.DeploymentTarget, request metricsprom.QueryRequest) (metricsprom.MetricSample, error) {
	return metricsprom.MetricSample{Name: request.Kind, Value: .35, Unit: "ratio", ObservedAt: b.now}, nil
}
