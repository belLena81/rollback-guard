package ports

import (
	"context"

	"github.com/belLena81/rollback-guard/internal/domain"
)

// Tracer is the application's view of distributed tracing. Carrier strings
// are opaque, persistable trace contexts (W3C traceparent values when tracing
// is wired); the application stores them and hands them back, never parses
// them.
type Tracer interface {
	// TraceCarrier returns the trace context active in ctx, or "" when ctx is
	// not traced.
	TraceCarrier(context.Context) string
	// StartSpan starts a span named name. Its parent is the span already in
	// ctx; when ctx has none, the parent is carrier, if it is a valid value
	// previously returned by TraceCarrier. end must be called exactly once.
	StartSpan(ctx context.Context, name string, carrier string) (traced context.Context, end func(error))
}

// NoopTracer is the default Tracer when tracing is not wired.
type NoopTracer struct{}

func (NoopTracer) TraceCarrier(context.Context) string { return "" }

func (NoopTracer) StartSpan(ctx context.Context, _ string, _ string) (context.Context, func(error)) {
	return ctx, func(error) {}
}

// Correlation holds the bounded identifiers that structured logs attach to
// every record written with a context. It never holds secrets or free text.
type Correlation struct {
	IncidentID     domain.IncidentID
	OperationID    string
	TaskID         string
	TraceID        string
	IdempotencyKey string
}

type correlationKey struct{}

// WithCorrelation returns ctx carrying the fields of update merged over any
// correlation already in ctx. Empty fields in update keep the existing value.
// Values that are not identifier-shaped (see CorrelationValue) are dropped,
// because several sources are remote callers.
func WithCorrelation(ctx context.Context, update Correlation) context.Context {
	current := CorrelationFromContext(ctx)
	if CorrelationValue(string(update.IncidentID)) {
		current.IncidentID = update.IncidentID
	}
	if CorrelationValue(update.OperationID) {
		current.OperationID = update.OperationID
	}
	if CorrelationValue(update.TaskID) {
		current.TaskID = update.TaskID
	}
	if CorrelationValue(update.TraceID) {
		current.TraceID = update.TraceID
	}
	if CorrelationValue(update.IdempotencyKey) {
		current.IdempotencyKey = update.IdempotencyKey
	}
	return context.WithValue(ctx, correlationKey{}, current)
}

// CorrelationValue reports whether value is a non-empty identifier of at
// most 128 characters drawn from [A-Za-z0-9._:-], so free text from remote
// callers cannot reach logs or exported spans through correlation.
func CorrelationValue(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.', r == ':':
		default:
			return false
		}
	}
	return true
}

// CorrelationFromContext returns the correlation carried by ctx, or the zero
// value.
func CorrelationFromContext(ctx context.Context) Correlation {
	if ctx == nil {
		return Correlation{}
	}
	correlation, ok := ctx.Value(correlationKey{}).(Correlation)
	if !ok {
		return Correlation{}
	}
	return correlation
}
