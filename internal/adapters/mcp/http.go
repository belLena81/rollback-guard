package mcp

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const DefaultMaxRequestBytes int64 = 1 << 20

func HTTPHandler(server *mcpsdk.Server, ready func(context.Context) error, maxBytes int64, sessionTimeout time.Duration, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxRequestBytes
	}
	mcpHandler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, &mcpsdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true, Logger: logger, SessionTimeout: sessionTimeout})
	mux := http.NewServeMux()
	mux.Handle("/mcp", http.NewCrossOriginProtection().Handler(limitBody(mcpHandler, maxBytes)))
	mux.HandleFunc("GET /health/live", func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusOK)
		writeHealth(response, `{"status":"ok"}`, logger)
	})
	mux.HandleFunc("GET /health/ready", func(response http.ResponseWriter, request *http.Request) {
		if ready != nil && ready(request.Context()) != nil {
			response.Header().Set("Content-Type", "application/json")
			response.WriteHeader(http.StatusServiceUnavailable)
			writeHealth(response, `{"status":"unavailable"}`, logger)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusOK)
		writeHealth(response, `{"status":"ok"}`, logger)
	})
	return mux
}

func writeHealth(response http.ResponseWriter, body string, logger *slog.Logger) {
	if _, err := response.Write([]byte(body)); err != nil {
		logger.Debug("write health response", "error", err)
	}
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
