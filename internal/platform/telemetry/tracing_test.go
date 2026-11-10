package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/belLena81/rollback-guard/internal/platform/telemetry"
	"github.com/belLena81/rollback-guard/internal/ports"
)

func TestOutgoingRequestsCarryTraceContextIntoServerSpan(t *testing.T) {
	clientSpans, serverSpans := tracetest.NewInMemoryExporter(), tracetest.NewInMemoryExporter()
	client := newProvider(t, telemetry.WithSyncExporter(clientSpans))
	server := newProvider(t, telemetry.WithSyncExporter(serverSpans))
	var received http.Header
	upstream := httptest.NewServer(server.Route("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	})))
	defer upstream.Close()

	ctx, end := client.StartSpan(context.Background(), "investigation.dispatch", "")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL+"/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.HTTPClient(5 * time.Second).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	end(nil)

	parent := trace.SpanContextFromContext(ctx)
	traceparent := received.Get("traceparent")
	if !strings.Contains(traceparent, parent.TraceID().String()) {
		t.Fatalf("traceparent = %q, want trace %s", traceparent, parent.TraceID())
	}
	if received.Get("baggage") != "" {
		t.Fatal("baggage must not be propagated")
	}
	servers := serverSpans.GetSpans()
	if len(servers) != 1 || servers[0].Name != "POST /mcp" {
		t.Fatalf("server spans = %#v", servers)
	}
	if servers[0].SpanContext.TraceID() != parent.TraceID() || !servers[0].Parent.IsRemote() {
		t.Fatalf("server span is not a remote child of the client trace: %#v", servers[0].Parent)
	}
	var clientHTTP *tracetest.SpanStub
	for index, span := range clientSpans.GetSpans() {
		if span.Name == "HTTP POST" {
			clientHTTP = &clientSpans.GetSpans()[index]
		}
	}
	if clientHTTP == nil || servers[0].Parent.SpanID() != clientHTTP.SpanContext.SpanID() || clientHTTP.Parent.SpanID() != parent.SpanID() {
		t.Fatalf("span chain dispatch -> HTTP POST -> server is broken: client %#v", clientHTTP)
	}
}

func TestStartSpanResumesPersistedCarrier(t *testing.T) {
	spans := tracetest.NewInMemoryExporter()
	provider := newProvider(t, telemetry.WithSyncExporter(spans))
	requestCtx, endRequest := provider.StartSpan(context.Background(), "POST /v1/incidents", "")
	carrier := provider.TraceCarrier(requestCtx)
	endRequest(nil)
	request := trace.SpanContextFromContext(requestCtx)

	tests := []struct {
		name      string
		ctx       context.Context
		carrier   string
		sameTrace bool
	}{
		{name: "carrier from a finished request", ctx: context.Background(), carrier: carrier, sameTrace: true},
		{name: "legacy generated id starts a new trace", ctx: context.Background(), carrier: "trace-42", sameTrace: false},
		{name: "empty carrier starts a new trace", ctx: context.Background(), carrier: "", sameTrace: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, end := provider.StartSpan(test.ctx, "investigation.dispatch", test.carrier)
			end(nil)
			got := trace.SpanContextFromContext(ctx)
			if !got.IsValid() || (got.TraceID() == request.TraceID()) != test.sameTrace {
				t.Fatalf("trace = %s, request trace = %s, want same=%v", got.TraceID(), request.TraceID(), test.sameTrace)
			}
		})
	}
	if !strings.HasPrefix(carrier, "00-"+request.TraceID().String()+"-") {
		t.Fatalf("carrier = %q is not a W3C traceparent of the request", carrier)
	}
	if provider.TraceCarrier(context.Background()) != "" {
		t.Fatal("untraced context must have no carrier")
	}

	active, endActive := provider.StartSpan(context.Background(), "active", "")
	defer endActive(nil)
	child, endChild := provider.StartSpan(active, "child", carrier)
	endChild(nil)
	if trace.SpanContextFromContext(child).TraceID() != trace.SpanContextFromContext(active).TraceID() {
		t.Fatal("an active span must take precedence over a carrier")
	}
}

func TestLogHandlerAddsCorrelationFields(t *testing.T) {
	var buffer bytes.Buffer
	logger := telemetry.NewLogger(&buffer, slog.LevelInfo)
	provider := newProvider(t)
	ctx := ports.WithCorrelation(context.Background(), ports.Correlation{IncidentID: "incident-1", OperationID: "operation-1", TaskID: "task-1", IdempotencyKey: "investigation:incident-1"})
	traced, end := provider.StartSpan(ctx, "work", "")
	logger.InfoContext(traced, "traced")
	end(nil)
	carrier := provider.TraceCarrier(traced)
	logger.InfoContext(ports.WithCorrelation(context.Background(), ports.Correlation{TraceID: carrier}), "carrier only")
	logger.InfoContext(ports.WithCorrelation(context.Background(), ports.Correlation{IncidentID: "incident-1\nforged=1", TraceID: "free text with spaces"}), "hostile")
	logger.Info("no context")

	records := decodeRecords(t, &buffer)
	if len(records) != 4 {
		t.Fatalf("records = %d", len(records))
	}
	span := trace.SpanContextFromContext(traced)
	first := records[0]
	for field, want := range map[string]string{"incident_id": "incident-1", "operation_id": "operation-1", "task_id": "task-1", "idempotency_key": "investigation:incident-1", "trace_id": span.TraceID().String(), "span_id": span.SpanID().String()} {
		if first[field] != want {
			t.Errorf("traced record %s = %v, want %s", field, first[field], want)
		}
	}
	if records[1]["trace_id"] != span.TraceID().String() {
		t.Errorf("carrier-only record trace_id = %v, want trace id extracted from the carrier", records[1]["trace_id"])
	}
	if _, ok := records[2]["incident_id"]; ok {
		t.Error("non-identifier correlation values must be dropped")
	}
	if _, ok := records[2]["trace_id"]; ok {
		t.Error("free-text trace values must be dropped")
	}
	if _, ok := records[3]["trace_id"]; ok {
		t.Error("records without context must not invent correlation")
	}
}

func TestServerInstrumentationLogsRoutePatternNotPath(t *testing.T) {
	var buffer bytes.Buffer
	provider := newProvider(t, telemetry.WithLogger(telemetry.NewLogger(&buffer, slog.LevelInfo)))
	handler := provider.Route("/v1/incidents/{incidentId}", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	request := httptest.NewRequest(http.MethodGet, "/v1/incidents/incident-1?token=abc", nil)
	request.Header.Set("Authorization", "Bearer super-secret-token")
	handler.ServeHTTP(httptest.NewRecorder(), request)
	health := provider.Route("/health/live", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	health.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health/live", nil))

	output := buffer.String()
	for _, forbidden := range []string{"super-secret-token", "Authorization", "token=abc"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("request log leaks %q: %s", forbidden, output)
		}
	}
	records := decodeRecords(t, &buffer)
	if len(records) != 1 || records[0]["route"] != "/v1/incidents/{incidentId}" || records[0]["level"] != "WARN" || records[0]["trace_id"] == nil {
		t.Fatalf("records = %#v (health checks must not be logged)", records)
	}
}

func TestPanickingHandlerIsCountedAndSpanEnded(t *testing.T) {
	spans := tracetest.NewInMemoryExporter()
	provider := newProvider(t, telemetry.WithSyncExporter(spans))
	handler := provider.Route("/v1/incidents", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	func() {
		defer func() {
			recovered := recover()
			if err, ok := recovered.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
				t.Fatalf("panic = %v, want it re-raised unchanged", recovered)
			}
		}()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/incidents", nil))
	}()
	if got := metricValue(t, provider.Gatherer(), "rollback_guardian_http_server_requests_total", map[string]string{"route": "/v1/incidents", "code": "500"}); got != 1 {
		t.Fatalf("panicking request counted %v times as 500", got)
	}
	if ended := spans.GetSpans(); len(ended) != 1 || ended[0].Name != "POST /v1/incidents" {
		t.Fatalf("ended spans = %#v", ended)
	}
}

func TestShutdownIsBoundedWhenCollectorHangs(t *testing.T) {
	release := make(chan struct{})
	collector := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer collector.Close()
	defer close(release)
	provider, err := telemetry.New(telemetry.Settings{ServiceName: "test", OTLPEndpoint: collector.URL, SampleRatio: 1, ShutdownTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	_, end := provider.StartSpan(context.Background(), "pending", "")
	end(nil)
	started := time.Now()
	// The collector never answers, so the flush is expected to time out.
	if err := provider.Shutdown(context.Background()); err == nil {
		t.Log("shutdown completed before the collector timed out")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("Shutdown took %s with a 200ms bound", elapsed)
	}
}

func TestShutdownFlushesSpansToOTLPCollector(t *testing.T) {
	received := make(chan string, 4)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Method + " " + r.URL.Path + " " + r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()
	provider, err := telemetry.New(telemetry.Settings{ServiceName: "test", OTLPEndpoint: collector.URL + "/otlp", SampleRatio: 1, ShutdownTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, end := provider.StartSpan(context.Background(), "pending", "")
	end(nil)
	// A canceled caller context must not skip the bounded flush.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := provider.Shutdown(canceled); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-received:
		if request != "POST /otlp/v1/traces application/x-protobuf" {
			t.Fatalf("collector received %q", request)
		}
	default:
		t.Fatal("shutdown did not flush the pending span")
	}
}

func TestZeroSampleRatioRecordsNothing(t *testing.T) {
	spans := tracetest.NewInMemoryExporter()
	provider, err := telemetry.New(telemetry.Settings{ServiceName: "test", SampleRatio: 0}, telemetry.WithSyncExporter(spans))
	if err != nil {
		t.Fatal(err)
	}
	ctx, end := provider.StartSpan(context.Background(), "root", "")
	end(nil)
	if len(spans.GetSpans()) != 0 || trace.SpanContextFromContext(ctx).IsSampled() {
		t.Fatalf("spans exported with ratio 0: %d", len(spans.GetSpans()))
	}
	if !trace.SpanContextFromContext(ctx).IsValid() {
		t.Fatal("unsampled spans must still carry IDs for log correlation")
	}
}

func TestNewRejectsInvalidSettings(t *testing.T) {
	tests := []telemetry.Settings{
		{ServiceName: "", SampleRatio: 1},
		{ServiceName: "svc", SampleRatio: 2},
		{ServiceName: "svc", SampleRatio: 1, OTLPEndpoint: "collector:4318"},
	}
	for _, settings := range tests {
		if _, err := telemetry.New(settings); err == nil {
			t.Errorf("New(%+v) succeeded", settings)
		}
	}
}

func decodeRecords(t *testing.T, buffer *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buffer.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}
