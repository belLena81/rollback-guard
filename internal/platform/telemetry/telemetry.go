// Package telemetry owns the per-process Prometheus registry, the
// OpenTelemetry tracer provider, W3C trace-context propagation, HTTP
// instrumentation, correlated structured logging, and the decorators that
// record workflow metrics around application ports. Prometheus and OTel SDK
// types do not leave this package.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/belLena81/rollback-guard/internal/platform/config"
	"github.com/belLena81/rollback-guard/internal/ports"
)

const (
	instrumentationName    = "github.com/belLena81/rollback-guard"
	traceparentHeader      = "traceparent"
	defaultShutdownTimeout = 5 * time.Second
)

// Settings selects the trace exporter and identifies the process.
type Settings struct {
	ServiceName string
	// OTLPEndpoint is the OTLP/HTTP base URL (for example
	// http://127.0.0.1:4318). Empty disables span export; spans are still
	// created so logs and persisted carriers keep real trace IDs.
	OTLPEndpoint    string
	SampleRatio     float64
	ShutdownTimeout time.Duration
}

// SettingsFrom converts validated service configuration.
func SettingsFrom(c config.TelemetryConfig) Settings {
	return Settings{ServiceName: c.ServiceName, OTLPEndpoint: c.OTLPEndpoint, SampleRatio: c.SampleRatio, ShutdownTimeout: c.ShutdownTimeout}
}

// Option customises a Provider; tests use it to capture spans in memory.
type Option func(*options)

type options struct {
	exporter sdktrace.SpanExporter
	logger   *slog.Logger
}

// WithSyncExporter exports every ended span synchronously to exporter. It
// replaces the configured OTLP exporter.
func WithSyncExporter(exporter sdktrace.SpanExporter) Option {
	return func(o *options) { o.exporter = exporter }
}

// WithLogger sets the logger used by instrumentation (request and call logs).
func WithLogger(logger *slog.Logger) Option {
	return func(o *options) { o.logger = logger }
}

// Provider is one process's telemetry. Construct it once in the composition
// root and inject it; there is no global registry or tracer provider.
type Provider struct {
	service         string
	registry        *prometheus.Registry
	metrics         *metrics
	tracerProvider  *sdktrace.TracerProvider
	tracer          trace.Tracer
	propagator      propagation.TextMapPropagator
	logger          *slog.Logger
	shutdownTimeout time.Duration
}

var _ ports.Tracer = (*Provider)(nil)

func New(ctx context.Context, settings Settings, opts ...Option) (*Provider, error) {
	if strings.TrimSpace(settings.ServiceName) == "" {
		return nil, errors.New("telemetry service name is required")
	}
	if settings.SampleRatio < 0 || settings.SampleRatio > 1 {
		return nil, errors.New("telemetry sample ratio must be between 0 and 1")
	}
	if settings.ShutdownTimeout <= 0 {
		settings.ShutdownTimeout = defaultShutdownTimeout
	}
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	if o.logger == nil {
		o.logger = slog.New(slog.DiscardHandler)
	}
	registry := prometheus.NewRegistry()
	if err := registry.Register(collectors.NewGoCollector()); err != nil {
		return nil, fmt.Errorf("register telemetry metrics: %w", err)
	}
	if err := registry.Register(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{})); err != nil {
		return nil, fmt.Errorf("register telemetry metrics: %w", err)
	}
	m, err := newMetrics(registry)
	if err != nil {
		return nil, err
	}
	res, err := resource.Merge(resource.Empty(), resource.NewSchemaless(attribute.String("service.name", settings.ServiceName)))
	if err != nil {
		return nil, fmt.Errorf("merge telemetry resource: %w", err)
	}
	providerOptions := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(settings.SampleRatio))),
	}
	switch {
	case o.exporter != nil:
		providerOptions = append(providerOptions, sdktrace.WithSyncer(o.exporter))
	case settings.OTLPEndpoint != "":
		exporter, err := newOTLPExporter(ctx, settings.OTLPEndpoint)
		if err != nil {
			return nil, err
		}
		providerOptions = append(providerOptions, sdktrace.WithBatcher(exporter))
	}
	tracerProvider := sdktrace.NewTracerProvider(providerOptions...)
	return &Provider{
		service:         settings.ServiceName,
		registry:        registry,
		metrics:         m,
		tracerProvider:  tracerProvider,
		tracer:          tracerProvider.Tracer(instrumentationName),
		propagator:      propagation.TraceContext{},
		logger:          o.logger,
		shutdownTimeout: settings.ShutdownTimeout,
	}, nil
}

func newOTLPExporter(ctx context.Context, endpoint string) (sdktrace.SpanExporter, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("invalid OTLP endpoint")
	}
	path := strings.TrimRight(parsed.Path, "/") + "/v1/traces"
	exporterOptions := []otlptracehttp.Option{otlptracehttp.WithEndpoint(parsed.Host), otlptracehttp.WithURLPath(path)}
	if parsed.Scheme == "http" {
		exporterOptions = append(exporterOptions, otlptracehttp.WithInsecure())
	}
	// The exporter connects lazily; creation never blocks on the collector.
	exporter, err := otlptracehttp.New(ctx, exporterOptions...)
	if err != nil {
		return nil, fmt.Errorf("create OTLP trace exporter: %w", err)
	}
	return exporter, nil
}

// Shutdown flushes pending spans, bounded by the configured shutdown timeout
// even if ctx has no deadline.
func (p *Provider) Shutdown(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.shutdownTimeout)
	defer cancel()
	if err := p.tracerProvider.Shutdown(ctx); err != nil {
		return fmt.Errorf("shut down tracer provider: %w", err)
	}
	return nil
}

// MetricsHandler serves this process's registry in the Prometheus text
// exposition format.
func (p *Provider) MetricsHandler() http.Handler {
	return promhttp.HandlerFor(p.registry, promhttp.HandlerOpts{Registry: p.registry})
}

// Gatherer exposes the registry for tests and in-process inspection.
func (p *Provider) Gatherer() prometheus.Gatherer {
	return p.registry
}

func (p *Provider) TraceCarrier(ctx context.Context) string {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ""
	}
	carrier := propagation.MapCarrier{}
	p.propagator.Inject(ctx, carrier)
	return carrier.Get(traceparentHeader)
}

func (p *Provider) StartSpan(ctx context.Context, name string, carrier string) (context.Context, func(error)) {
	ctx = p.resume(ctx, carrier)
	ctx, span := p.tracer.Start(ctx, name, trace.WithAttributes(correlationAttributes(ctx)...))
	return ctx, func(err error) { endSpan(span, err) }
}

// resume makes carrier the remote parent of ctx when ctx is not already
// traced. Invalid or legacy carriers leave ctx untouched, which starts a new
// trace.
func (p *Provider) resume(ctx context.Context, carrier string) context.Context {
	if carrier == "" || trace.SpanContextFromContext(ctx).IsValid() {
		return ctx
	}
	return p.propagator.Extract(ctx, propagation.MapCarrier{traceparentHeader: carrier})
}

func (p *Provider) startInternal(ctx context.Context, name string, kind trace.SpanKind, attributes ...attribute.KeyValue) (context.Context, trace.Span) {
	attributes = append(attributes, correlationAttributes(ctx)...)
	return p.tracer.Start(ctx, name, trace.WithSpanKind(kind), trace.WithAttributes(attributes...))
}

// endSpan records failure without the error text: errors can wrap provider or
// remote messages that must not be exported.
func endSpan(span trace.Span, err error) {
	if err != nil {
		span.SetStatus(codes.Error, outcomeOf(err))
	}
	span.End()
}

func correlationAttributes(ctx context.Context) []attribute.KeyValue {
	c := ports.CorrelationFromContext(ctx)
	var attributes []attribute.KeyValue
	if c.IncidentID != "" {
		attributes = append(attributes, attribute.String("rollback_guardian.incident_id", string(c.IncidentID)))
	}
	if c.OperationID != "" {
		attributes = append(attributes, attribute.String("rollback_guardian.operation_id", c.OperationID))
	}
	if c.TaskID != "" {
		attributes = append(attributes, attribute.String("rollback_guardian.task_id", c.TaskID))
	}
	return attributes
}

// outcomeOf maps an error to a bounded label value.
func outcomeOf(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "error"
	}
}
