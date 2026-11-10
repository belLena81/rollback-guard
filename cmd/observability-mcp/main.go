package main

import (
	"context"
	"errors"
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
	"github.com/belLena81/rollback-guard/internal/platform/config"
	"github.com/belLena81/rollback-guard/internal/platform/telemetry"
	promapi "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

func main() {
	level := new(slog.LevelVar)
	logger := telemetry.NewLogger(os.Stdout, level)
	if err := run(logger, level); err != nil {
		logger.Error("observability MCP exited", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger, level *slog.LevelVar) error {
	cfg, err := config.LoadObservabilityMCP()
	if err != nil {
		return err
	}
	level.Set(cfg.Telemetry.LogLevel)
	tel, err := telemetry.New(telemetry.SettingsFrom(cfg.Telemetry), telemetry.WithLogger(logger))
	if err != nil {
		return err
	}
	defer func() {
		if err := tel.Shutdown(context.Background()); err != nil {
			logger.Warn("telemetry shutdown incomplete", "error", err)
		}
	}()
	// Prometheus queries join the tool call's trace.
	promClient, err := promapi.NewClient(promapi.Config{Address: cfg.PrometheusURL, RoundTripper: tel.Transport(promapi.DefaultRoundTripper)})
	if err != nil {
		return fmt.Errorf("create prometheus client: %w", err)
	}
	api := promv1.NewAPI(promClient)
	backend, err := metricsprom.NewClient(timeoutQueryAPI{api: api, timeout: cfg.RequestTimeout}, metricsprom.Config{AllowedTargets: cfg.AllowedTargets, ObservationWindow: cfg.MetricsWindow, MaxSampleAge: cfg.MaxSampleAge})
	if err != nil {
		return err
	}
	ready := func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
		defer cancel()
		_, err := api.Buildinfo(ctx)
		return err
	}
	handler, err := observabilityserver.NewHTTPHandler(backend, ready, cfg.SessionTimeout, logger, protocol.WithMetricsHandler(tel.MetricsHandler()), protocol.WithToolObserver(protocol.ServerObservability, tel))
	if err != nil {
		return err
	}
	return serve(cfg.HTTPAddr, tel.InstrumentServer(handler, protocol.RouteOf), logger)
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

func serve(addr string, handler http.Handler, logger *slog.Logger) error {
	server := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() {
		logger.Info("observability MCP listening", "addr", addr)
		errCh <- server.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
