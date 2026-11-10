package application

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/belLena81/rollback-guard/internal/ports"
)

type spanRecord struct {
	name        string
	carrier     string
	correlation ports.Correlation
	err         error
	ended       bool
}

type tracedKey struct{}

// recordingTracer is a ports.Tracer fake: TraceCarrier reports the name of the
// innermost span in ctx, or the request carrier set by the test.
type recordingTracer struct {
	mu           sync.Mutex
	requestTrace string
	spans        []*spanRecord
}

func (r *recordingTracer) TraceCarrier(ctx context.Context) string {
	if span, ok := ctx.Value(tracedKey{}).(*spanRecord); ok {
		return "carrier-of-" + span.name
	}
	return r.requestTrace
}

func (r *recordingTracer) StartSpan(ctx context.Context, name string, carrier string) (context.Context, func(error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record := &spanRecord{name: name, carrier: carrier, correlation: ports.CorrelationFromContext(ctx)}
	r.spans = append(r.spans, record)
	return context.WithValue(ctx, tracedKey{}, record), func(err error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		record.err = err
		record.ended = true
	}
}

func TestCreateIncidentAsyncPersistsRequestTraceCarrier(t *testing.T) {
	tests := []struct {
		name         string
		requestTrace string
		wantPrefix   string
	}{
		{name: "traced request", requestTrace: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01", wantPrefix: "00-0af7651916cd43dd8448eb211c80319c"},
		{name: "untraced request falls back to generated id", requestTrace: "", wantPrefix: "trace-"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			services, _, target, symptom, actor := asyncHarness(t)
			services.Incidents.tracer = &recordingTracer{requestTrace: test.requestTrace}
			incident, err := services.Incidents.CreateIncidentAsync(context.Background(), CreateIncidentCommand{Target: target, Symptom: symptom, Actor: actor})
			if err != nil {
				t.Fatal(err)
			}
			summary, err := services.Queries.GetIncidentSummary(context.Background(), incident.ID())
			if err != nil {
				t.Fatal(err)
			}
			if summary.Investigation == nil || len(summary.Investigation.TraceID) < len(test.wantPrefix) || summary.Investigation.TraceID[:len(test.wantPrefix)] != test.wantPrefix {
				t.Fatalf("dispatch trace = %#v, want prefix %q", summary.Investigation, test.wantPrefix)
			}
		})
	}
}

func TestCoordinatorResumesPersistedTraceWithCorrelation(t *testing.T) {
	services, coordinator, target, symptom, actor := asyncHarness(t)
	const requestTrace = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	tracer := &recordingTracer{requestTrace: requestTrace}
	services.Incidents.tracer = tracer
	coordinator.config.Tracer = tracer
	gateway, ok := coordinator.gateway.(*localInvestigationGateway)
	if !ok {
		t.Fatal("unexpected gateway type")
	}
	incident, err := services.Incidents.CreateIncidentAsync(context.Background(), CreateIncidentCommand{Target: target, Symptom: symptom, Actor: actor})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(tracer.spans) != 1 {
		t.Fatalf("spans = %#v", tracer.spans)
	}
	span := tracer.spans[0]
	if span.name != "investigation.dispatch" || span.carrier != requestTrace || !span.ended || span.err != nil {
		t.Fatalf("dispatch span = %#v", span)
	}
	if span.correlation.IncidentID != incident.ID() || span.correlation.OperationID == "" || span.correlation.IdempotencyKey != "investigation:"+string(incident.ID()) || span.correlation.TraceID != requestTrace {
		t.Fatalf("dispatch correlation = %#v", span.correlation)
	}
	if gateway.request.TraceID != "carrier-of-investigation.dispatch" {
		t.Fatalf("submission trace = %q, want the dispatch span carrier", gateway.request.TraceID)
	}
}

func TestCoordinatorReportsErrorsWithDispatchCorrelation(t *testing.T) {
	services, coordinator, target, symptom, actor := asyncHarness(t)
	base, ok := coordinator.gateway.(*localInvestigationGateway)
	if !ok {
		t.Fatal("unexpected gateway type")
	}
	coordinator.gateway = &flakySubmissionGateway{localInvestigationGateway: base}
	tracer := &recordingTracer{}
	coordinator.config.Tracer = tracer
	incident, err := services.Incidents.CreateIncidentAsync(context.Background(), CreateIncidentCommand{Target: target, Symptom: symptom, Actor: actor})
	if err != nil {
		t.Fatal(err)
	}
	reportCtx, err := coordinator.processNext(context.Background())
	if err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("processNext() error = %v, want submission failure", err)
	}
	correlation := ports.CorrelationFromContext(reportCtx)
	if correlation.IncidentID != incident.ID() || correlation.OperationID == "" || correlation.IdempotencyKey == "" || correlation.TraceID == "" {
		t.Fatalf("report correlation = %#v", correlation)
	}
	if len(tracer.spans) != 1 || !tracer.spans[0].ended || tracer.spans[0].err == nil {
		t.Fatalf("dispatch span must end with the iteration error: %#v", tracer.spans)
	}
}
