package telemetry

import (
	"context"
	"io"
	"log/slog"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/belLena81/rollback-guard/internal/ports"
)

// Log field names shared by every service.
const (
	FieldIncidentID     = "incident_id"
	FieldOperationID    = "operation_id"
	FieldTaskID         = "task_id"
	FieldTraceID        = "trace_id"
	FieldSpanID         = "span_id"
	FieldIdempotencyKey = "idempotency_key"
)

// NewLogger returns a JSON logger whose records carry the correlation fields
// found in the logging context.
func NewLogger(w io.Writer, level slog.Leveler) *slog.Logger {
	return slog.New(NewLogHandler(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})))
}

// NewLogHandler wraps next so every record logged with a context gets
// incident_id, operation_id, task_id, idempotency_key (from
// ports.Correlation), and trace_id/span_id (from the active span, or from a
// persisted carrier when no span is active). Call sites never add them by
// hand.
func NewLogHandler(next slog.Handler) slog.Handler {
	return correlationHandler{next: next}
}

type correlationHandler struct {
	next slog.Handler
}

func (h correlationHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h correlationHandler) Handle(ctx context.Context, record slog.Record) error {
	if ctx != nil {
		record.AddAttrs(correlationLogAttrs(ctx)...)
	}
	return h.next.Handle(ctx, record)
}

func (h correlationHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return correlationHandler{next: h.next.WithAttrs(attrs)}
}

func (h correlationHandler) WithGroup(name string) slog.Handler {
	return correlationHandler{next: h.next.WithGroup(name)}
}

func correlationLogAttrs(ctx context.Context) []slog.Attr {
	c := ports.CorrelationFromContext(ctx)
	var attrs []slog.Attr
	add := func(name string, value string) {
		if value != "" && identifierLike(value) {
			attrs = append(attrs, slog.String(name, value))
		}
	}
	add(FieldIncidentID, string(c.IncidentID))
	add(FieldOperationID, c.OperationID)
	add(FieldTaskID, c.TaskID)
	add(FieldIdempotencyKey, c.IdempotencyKey)
	if span := trace.SpanContextFromContext(ctx); span.IsValid() {
		attrs = append(attrs, slog.String(FieldTraceID, span.TraceID().String()), slog.String(FieldSpanID, span.SpanID().String()))
	} else if traceID := carrierTraceID(c.TraceID); traceID != "" {
		attrs = append(attrs, slog.String(FieldTraceID, traceID))
	}
	return attrs
}

// carrierTraceID returns the trace ID inside a persisted W3C carrier, or the
// value itself for legacy non-W3C identifiers.
func carrierTraceID(carrier string) string {
	if carrier == "" {
		return ""
	}
	ctx := propagation.TraceContext{}.Extract(context.Background(), propagation.MapCarrier{traceparentHeader: carrier})
	if span := trace.SpanContextFromContext(ctx); span.IsValid() {
		return span.TraceID().String()
	}
	if !ports.CorrelationValue(carrier) {
		return ""
	}
	return carrier
}

// identifierLike re-checks values at the log boundary in case a context was
// built without ports.WithCorrelation.
func identifierLike(value string) bool {
	return ports.CorrelationValue(value)
}
