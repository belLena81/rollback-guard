package demoapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	metricsprom "github.com/belLena81/rollback-guard/internal/adapters/prometheus"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metric names the demo service emits; they are the series that
// internal/adapters/prometheus queries, so they are defined there. Renaming
// either one breaks error-rate and latency evidence collection.
const (
	RequestsTotalMetric   = metricsprom.SeriesRequestsTotal
	RequestDurationMetric = metricsprom.SeriesRequestDuration
)

// Deps are the injectable side effects of the handler.
type Deps struct {
	// Sample returns a uniform value in [0, 1) used to decide error-storm
	// failures.
	Sample func() float64
	// Wait blocks for the simulated work duration or until ctx is done.
	Wait func(context.Context, time.Duration) error
	// Logger records response write failures (usually client disconnects).
	Logger *slog.Logger
}

// NewHandler builds the demo HTTP API: GET /healthz, GET /work, GET /metrics.
// Only /work is instrumented, so probe and scrape traffic does not dilute the
// error rate the investigator measures.
func NewHandler(cfg Config, deps Deps) (http.Handler, error) {
	if deps.Sample == nil || deps.Wait == nil || deps.Logger == nil {
		return nil, errors.New("demo-api sample, wait and logger dependencies are required")
	}
	if err := cfg.Mode.validate(); err != nil {
		return nil, err
	}
	if err := cfg.Target.validate(); err != nil {
		return nil, err
	}

	registry := prometheus.NewRegistry()
	targetLabels := prometheus.Labels{
		"cluster":    cfg.Target.Cluster,
		"namespace":  cfg.Target.Namespace,
		"deployment": cfg.Target.Deployment,
		"service":    cfg.Target.Service,
	}
	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        RequestsTotalMetric,
		Help:        "Demo API /work requests by HTTP status code.",
		ConstLabels: targetLabels,
	}, []string{"code"})
	duration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:        RequestDurationMetric,
		Help:        "Demo API /work request latency in seconds.",
		ConstLabels: targetLabels,
		Buckets:     prometheus.DefBuckets,
	}, []string{"code"})
	for _, collector := range []prometheus.Collector{
		requests,
		duration,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	} {
		if err := registry.Register(collector); err != nil {
			return nil, fmt.Errorf("register demo-api metrics: %w", err)
		}
	}

	work := &workHandler{cfg: cfg, deps: deps}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", work.healthz)
	mux.Handle("GET /work", promhttp.InstrumentHandlerDuration(duration, promhttp.InstrumentHandlerCounter(requests, work)))
	mux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{Registry: registry}))
	return mux, nil
}

type workHandler struct {
	cfg  Config
	deps Deps
}

func (h *workHandler) healthz(w http.ResponseWriter, r *http.Request) {
	// A crashloop build never becomes ready, so a rollout to it stalls the
	// way a real crashing release does.
	if h.cfg.Mode == ModeCrashLoop {
		h.writeText(w, r, http.StatusServiceUnavailable, "starting\n")
		return
	}
	h.writeText(w, r, http.StatusOK, "ok\n")
}

func (h *workHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.Wait(r.Context(), h.cfg.WorkLatency); err != nil {
		h.writeText(w, r, http.StatusServiceUnavailable, "work interrupted\n")
		return
	}
	if h.cfg.Mode == ModeErrorStorm && h.deps.Sample() < h.cfg.ErrorRate {
		h.writeText(w, r, http.StatusInternalServerError, "simulated failure\n")
		return
	}
	h.writeText(w, r, http.StatusOK, fmt.Sprintf("work done version=%s mode=%s\n", h.cfg.Version, h.cfg.Mode))
}

func (h *workHandler) writeText(w http.ResponseWriter, r *http.Request, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if _, err := io.WriteString(w, body); err != nil {
		h.deps.Logger.DebugContext(r.Context(), "write demo-api response", "path", r.URL.Path, "error", err)
	}
}

// SleepContext waits for d or until ctx is done, whichever comes first.
func SleepContext(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
