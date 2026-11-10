package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	protocol "github.com/belLena81/rollback-guard/internal/adapters/mcp"
	"github.com/belLena81/rollback-guard/internal/adapters/mcp/observabilityserver"
	metricsprom "github.com/belLena81/rollback-guard/internal/adapters/prometheus"
	"github.com/belLena81/rollback-guard/internal/platform/bootstrap"
	"github.com/belLena81/rollback-guard/internal/platform/config"
	"github.com/belLena81/rollback-guard/internal/platform/httpserver"
	"github.com/belLena81/rollback-guard/internal/platform/resilience"
	"github.com/belLena81/rollback-guard/internal/platform/telemetry"
	promapi "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

const serviceName = "observability MCP"

func main() {
	level := new(slog.LevelVar)
	logger := telemetry.NewLogger(os.Stdout, level)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	err := run(ctx, logger, level)
	stop()
	if err != nil {
		logger.Error("observability MCP exited", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger, level *slog.LevelVar) error {
	cfg, err := config.LoadObservabilityMCP()
	if err != nil {
		return err
	}
	tel, stopTelemetry, err := bootstrap.StartTelemetry(ctx, cfg.Telemetry, logger, level)
	if err != nil {
		return err
	}
	defer stopTelemetry()
	// Prometheus queries join the tool call's trace.
	promClient, err := promapi.NewClient(promapi.Config{Address: cfg.PrometheusURL, RoundTripper: tel.Transport(promapi.DefaultRoundTripper)})
	if err != nil {
		return fmt.Errorf("create prometheus client: %w", err)
	}
	api := promv1.NewAPI(promClient)
	backend, err := buildServices(cfg, api, tel)
	if err != nil {
		return err
	}
	ready := func(ctx context.Context) error {
		_, err := api.Buildinfo(ctx)
		return err
	}
	handler, err := buildHTTP(cfg, backend, ready, tel, logger)
	if err != nil {
		return err
	}
	return httpserver.Serve(ctx, httpserver.Config{Name: serviceName, Addr: cfg.HTTPAddr}, handler, logger)
}

// buildServices creates the Prometheus-backed metrics backend.
//
// Each Prometheus attempt gets its own RequestTimeout; transient failures
// are retried behind one breaker for Prometheus. observer may be nil.
func buildServices(cfg config.ObservabilityMCPConfig, api promv1.API, observer resilience.Observer) (*metricsprom.Client, error) {
	protector, err := bootstrap.NewProtector(resilience.DependencyPrometheus, cfg.Resilience, observer)
	if err != nil {
		return nil, err
	}
	protected, err := metricsprom.NewProtectedQueryAPI(timeoutQueryAPI{api: api, timeout: cfg.RequestTimeout}, protector)
	if err != nil {
		return nil, err
	}
	return metricsprom.NewClient(protected, metricsprom.Config{
		AllowedTargets:    cfg.AllowedTargets,
		ObservationWindow: cfg.MetricsWindow,
		MaxSampleAge:      cfg.MaxSampleAge,
	})
}

// buildHTTP assembles the MCP server; readiness is bounded like any other
// Prometheus request.
func buildHTTP(cfg config.ObservabilityMCPConfig, backend observabilityserver.Backend, ready httpserver.ReadyCheck, tel *telemetry.Provider, logger *slog.Logger) (http.Handler, error) {
	handler, err := observabilityserver.NewHTTPHandler(backend, ready, cfg.SessionTimeout, logger,
		protocol.WithMetricsHandler(tel.MetricsHandler()),
		protocol.WithToolObserver(protocol.ServerObservability, tel),
		protocol.WithReadyTimeout(cfg.RequestTimeout),
	)
	if err != nil {
		return nil, err
	}
	return tel.InstrumentServer(handler, protocol.RouteOf), nil
}

type timeoutQueryAPI struct {
	api     promv1.API
	timeout time.Duration
}

func (a timeoutQueryAPI) Query(ctx context.Context, query string, at time.Time, options ...promv1.Option) (model.Value, promv1.Warnings, error) {
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	return a.api.Query(ctx, query, at, options...)
}
