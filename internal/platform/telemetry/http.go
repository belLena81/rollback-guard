package telemetry

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// UnmatchedRoute labels requests that match no registered route, so scanners
// cannot create unbounded route label values.
const UnmatchedRoute = "unmatched"

// RouteFunc maps a request to its registered route pattern. It must return
// one of a fixed set of values (or UnmatchedRoute), never the raw path.
type RouteFunc func(*http.Request) string

// Route instruments one registered route. Use it when the route pattern is
// known at registration time (for example wrapping each ServeMux handler).
func (p *Provider) Route(route string, next http.Handler) http.Handler {
	return p.InstrumentServer(next, func(*http.Request) string { return route })
}

// InstrumentServer records request metrics, extracts the W3C trace context
// from incoming headers, starts a server span (except for health and metrics
// scrapes), and logs a completion record carrying correlation fields.
func (p *Provider) InstrumentServer(next http.Handler, routeOf RouteFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		route := routeOf(r)
		if route == "" {
			route = UnmatchedRoute
		}
		method := boundedMethod(r.Method)
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		ctx := r.Context()
		var span trace.Span
		traced := tracedRoute(route)
		if traced {
			ctx = p.propagator.Extract(ctx, propagation.HeaderCarrier(r.Header))
			ctx, span = p.startInternal(ctx, method+" "+route, trace.SpanKindServer,
				attribute.String("http.request.method", method), attribute.String("http.route", route))
		}
		// Deferred so a panicking handler still ends its span and is counted
		// (as a 500) before net/http's recovery sees the panic again.
		defer func() {
			recovered := recover()
			if recovered != nil {
				recorder.status = http.StatusInternalServerError
			}
			p.finishServer(ctx, span, method, route, recorder.status, time.Since(start))
			if recovered != nil {
				panic(recovered)
			}
		}()
		next.ServeHTTP(recorder, r.WithContext(ctx))
	})
}

func (p *Provider) finishServer(ctx context.Context, span trace.Span, method string, route string, status int, elapsed time.Duration) {
	code := strconv.Itoa(status)
	p.metrics.httpRequests.WithLabelValues(method, route, code).Inc()
	p.metrics.httpDuration.WithLabelValues(method, route, code).Observe(elapsed.Seconds())
	if span == nil {
		return
	}
	span.SetAttributes(attribute.Int("http.response.status_code", status))
	level := slog.LevelInfo
	if status >= http.StatusInternalServerError {
		span.SetStatus(codes.Error, code)
		level = slog.LevelWarn
	}
	p.logger.Log(ctx, level, "http request completed", "method", method, "route", route, "status", status, "duration_ms", elapsed.Milliseconds())
	span.End()
}

func tracedRoute(route string) bool {
	return route != "/metrics" && !strings.HasPrefix(route, "/health/") && route != UnmatchedRoute
}

func boundedMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return method
	default:
		return "OTHER"
	}
}

// Transport wraps base so every outgoing request carries the W3C trace
// context of its request context and gets a client span.
func (p *Provider) Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return otelhttp.NewTransport(base,
		otelhttp.WithTracerProvider(p.tracerProvider),
		otelhttp.WithPropagators(p.propagator),
		otelhttp.WithMeterProvider(noop.NewMeterProvider()),
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string { return "HTTP " + boundedMethod(r.Method) }),
	)
}

// HTTPClient returns a client whose transport propagates trace context.
func (p *Provider) HTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: p.Transport(nil)}
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if !r.wroteHeader {
		r.status = status
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(data []byte) (int, error) {
	r.wroteHeader = true
	return r.ResponseWriter.Write(data)
}

// Flush keeps streaming responses (A2A SSE, MCP) working through the wrapper.
func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hijacker, ok := r.ResponseWriter.(http.Hijacker); ok {
		return hijacker.Hijack()
	}
	return nil, nil, errors.New("response writer does not support hijacking")
}

func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
