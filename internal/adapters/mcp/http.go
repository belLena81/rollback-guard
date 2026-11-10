package mcp

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/belLena81/rollback-guard/internal/platform/httpserver"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const DefaultMaxRequestBytes int64 = 1 << 20

// HandlerOption adds optional telemetry to HTTPHandler.
type HandlerOption func(*handlerOptions)

type handlerOptions struct {
	metrics      http.Handler
	serverName   string
	observer     ToolObserver
	readyTimeout time.Duration
}

// WithReadyTimeout bounds each readiness check. Without it the shared
// default (httpserver.DefaultReadyTimeout) applies.
func WithReadyTimeout(timeout time.Duration) HandlerOption {
	return func(o *handlerOptions) { o.readyTimeout = timeout }
}

// WithMetricsHandler serves handler at GET /metrics.
func WithMetricsHandler(handler http.Handler) HandlerOption {
	return func(o *handlerOptions) { o.metrics = handler }
}

// WithToolObserver reports every tools/call handled by the server to
// observer under serverName.
func WithToolObserver(serverName string, observer ToolObserver) HandlerOption {
	return func(o *handlerOptions) {
		o.serverName = serverName
		o.observer = observer
	}
}

func HTTPHandler(server *mcpsdk.Server, ready func(context.Context) error, maxBytes int64, sessionTimeout time.Duration, logger *slog.Logger, opts ...HandlerOption) http.Handler {
	var o handlerOptions
	for _, opt := range opts {
		opt(&o)
	}
	if o.observer != nil && o.serverName != "" {
		server.AddReceivingMiddleware(observeToolsMiddleware(o.serverName, o.observer))
	}
	if logger == nil {
		logger = slog.Default()
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxRequestBytes
	}
	mcpHandler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, &mcpsdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true, Logger: logger, SessionTimeout: sessionTimeout})
	mux := http.NewServeMux()
	mux.Handle("/mcp", http.NewCrossOriginProtection().Handler(limitBody(mcpHandler, maxBytes)))
	httpserver.NewHealth(ready, o.readyTimeout, logger).Register(mux)
	if o.metrics != nil {
		mux.Handle("GET /metrics", o.metrics)
	}
	return mux
}

func limitBody(next http.Handler, maxBytes int64) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.ContentLength > maxBytes {
			http.Error(response, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		request.Body = http.MaxBytesReader(response, request.Body, maxBytes)
		next.ServeHTTP(response, request)
	})
}
