package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

const (
	// LivePath and ReadyPath are the health routes every service exposes.
	LivePath  = "/health/live"
	ReadyPath = "/health/ready"

	// DefaultReadyTimeout bounds one readiness check when the service
	// configures no timeout of its own.
	DefaultReadyTimeout = 2 * time.Second

	// HealthOKBody is the body of every successful health response and
	// HealthNotReadyBody the body of every 503 readiness response. The
	// orchestrator's copies are part of the REST contract.
	HealthOKBody       = `{"status":"ok"}`
	HealthNotReadyBody = `{"status":"not_ready"}`
)

// ReadyCheck reports whether the service's dependencies respond. A nil check
// is always ready. The error is never written to the response.
type ReadyCheck func(context.Context) error

// Health serves the liveness and readiness endpoints shared by every service.
type Health struct {
	check   ReadyCheck
	timeout time.Duration
	logger  *slog.Logger
}

// NewHealth bounds every readiness check by timeout, or by
// DefaultReadyTimeout when timeout is not positive.
func NewHealth(check ReadyCheck, timeout time.Duration, logger *slog.Logger) *Health {
	if timeout <= 0 {
		timeout = DefaultReadyTimeout
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Health{check: check, timeout: timeout, logger: logger}
}

// Live answers 200 whenever the process can serve HTTP.
func (h *Health) Live(w http.ResponseWriter, r *http.Request) {
	h.write(w, r, http.StatusOK, HealthOKBody)
}

// Ready answers 200 when the readiness check passes within the timeout and
// 503 otherwise.
func (h *Health) Ready(w http.ResponseWriter, r *http.Request) {
	if h.check != nil {
		ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
		defer cancel()
		if err := h.check(ctx); err != nil {
			h.write(w, r, http.StatusServiceUnavailable, HealthNotReadyBody)
			return
		}
	}
	h.write(w, r, http.StatusOK, HealthOKBody)
}

// Register adds both endpoints to mux.
func (h *Health) Register(mux *http.ServeMux) {
	mux.HandleFunc(http.MethodGet+" "+LivePath, h.Live)
	mux.HandleFunc(http.MethodGet+" "+ReadyPath, h.Ready)
}

func (h *Health) write(w http.ResponseWriter, r *http.Request, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write([]byte(body)); err != nil {
		h.logger.DebugContext(r.Context(), "write health response", "path", r.URL.Path, "error", err)
	}
}
