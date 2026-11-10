package ports_test

import (
	"context"
	"strings"
	"testing"

	"github.com/belLena81/rollback-guard/internal/ports"
)

func TestWithCorrelationMergesNonEmptyFields(t *testing.T) {
	tests := []struct {
		name    string
		initial ports.Correlation
		update  ports.Correlation
		want    ports.Correlation
	}{
		{name: "empty context", update: ports.Correlation{IncidentID: "incident-1"}, want: ports.Correlation{IncidentID: "incident-1"}},
		{name: "keeps existing", initial: ports.Correlation{IncidentID: "incident-1", OperationID: "operation-1"}, update: ports.Correlation{TaskID: "task-1"}, want: ports.Correlation{IncidentID: "incident-1", OperationID: "operation-1", TaskID: "task-1"}},
		{name: "overrides set fields", initial: ports.Correlation{TraceID: "old", IdempotencyKey: "key-1"}, update: ports.Correlation{TraceID: "new"}, want: ports.Correlation{TraceID: "new", IdempotencyKey: "key-1"}},
		{name: "w3c carrier is an identifier", update: ports.Correlation{TraceID: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"}, want: ports.Correlation{TraceID: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"}},
		{name: "drops free text from remote callers", initial: ports.Correlation{IncidentID: "incident-1"}, update: ports.Correlation{IncidentID: "incident-1\nforged=1", OperationID: "drop table incidents", TaskID: strings.Repeat("a", 129), IdempotencyKey: "approval:approval-1"}, want: ports.Correlation{IncidentID: "incident-1", IdempotencyKey: "approval:approval-1"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			if test.initial != (ports.Correlation{}) {
				ctx = ports.WithCorrelation(ctx, test.initial)
			}
			got := ports.CorrelationFromContext(ports.WithCorrelation(ctx, test.update))
			if got != test.want {
				t.Fatalf("correlation = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestNoopTracerIsInert(t *testing.T) {
	ctx := context.Background()
	var tracer ports.Tracer = ports.NoopTracer{}
	if carrier := tracer.TraceCarrier(ctx); carrier != "" {
		t.Fatalf("carrier = %q", carrier)
	}
	traced, end := tracer.StartSpan(ctx, "span", "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01")
	end(nil)
	if traced != ctx {
		t.Fatal("noop tracer changed the context")
	}
}
