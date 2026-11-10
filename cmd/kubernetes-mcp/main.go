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

	kubernetesadapter "github.com/belLena81/rollback-guard/internal/adapters/kubernetes"
	protocol "github.com/belLena81/rollback-guard/internal/adapters/mcp"
	"github.com/belLena81/rollback-guard/internal/adapters/mcp/kubernetesserver"
	postgresadapter "github.com/belLena81/rollback-guard/internal/adapters/postgres"
	"github.com/belLena81/rollback-guard/internal/platform/config"
	"github.com/belLena81/rollback-guard/internal/platform/telemetry"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/jackc/pgx/v5/pgxpool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	level := new(slog.LevelVar)
	logger := telemetry.NewLogger(os.Stdout, level)
	if err := run(logger, level); err != nil {
		logger.Error("kubernetes MCP exited", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger, level *slog.LevelVar) error {
	cfg, err := config.LoadKubernetesMCP()
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
	restConfig, err := kubernetesConfig(cfg.Kubeconfig)
	if err != nil {
		return err
	}
	restConfig.Timeout = cfg.RequestTimeout
	// Kubernetes API calls join the tool call's trace.
	restConfig.Wrap(tel.Transport)
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create kubernetes client: %w", err)
	}
	var pool *pgxpool.Pool
	var mutationGuard ports.RollbackMutationGuard
	if cfg.Mode == string(kubernetesserver.ModeRollback) {
		poolConfig, parseErr := pgxpool.ParseConfig(cfg.DatabaseURL)
		if parseErr != nil {
			return fmt.Errorf("parse rollback database configuration: %w", parseErr)
		}
		poolConfig.MaxConns = cfg.DBMaxConns
		pool, err = pgxpool.NewWithConfig(context.Background(), poolConfig)
		if err != nil {
			return fmt.Errorf("create rollback database pool: %w", err)
		}
		defer pool.Close()
		mutationGuard, err = postgresadapter.NewRollbackMutationGuard(pool)
		if err != nil {
			return err
		}
	}
	backend, err := kubernetesadapter.NewService(clientset, kubernetesadapter.Config{ClusterID: cfg.ClusterID, AllowedTargets: cfg.AllowedTargets, MutationGuard: mutationGuard})
	if err != nil {
		return err
	}
	mode := kubernetesserver.Mode(cfg.Mode)
	ready := func(ctx context.Context) error {
		readinessTimeout := 2 * time.Second
		if cfg.DBPingTimeout > 0 {
			readinessTimeout = cfg.DBPingTimeout
		}
		ctx, cancel := context.WithTimeout(ctx, readinessTimeout)
		defer cancel()
		if pool != nil {
			if err := pool.Ping(ctx); err != nil {
				return err
			}
		}
		target := cfg.AllowedTargets[0]
		_, err := clientset.AppsV1().Deployments(target.Namespace()).Get(ctx, target.Deployment(), metav1.GetOptions{})
		return err
	}
	serverName := protocol.ServerKubernetesReadonly
	if mode == kubernetesserver.ModeRollback {
		serverName = protocol.ServerKubernetesRollback
	}
	handler, err := kubernetesserver.NewHTTPHandler(backend, mode, ready, cfg.SessionTimeout, logger, protocol.WithMetricsHandler(tel.MetricsHandler()), protocol.WithToolObserver(serverName, tel))
	if err != nil {
		return err
	}
	return serve(cfg.HTTPAddr, tel.InstrumentServer(handler, protocol.RouteOf), logger)
}

func kubernetesConfig(path string) (*rest.Config, error) {
	if path != "" {
		cfg, err := clientcmd.BuildConfigFromFlags("", path)
		if err != nil {
			return nil, fmt.Errorf("load kubeconfig: %w", err)
		}
		return cfg, nil
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("load in-cluster kubernetes config: %w", err)
	}
	return cfg, nil
}

func serve(addr string, handler http.Handler, logger *slog.Logger) error {
	server := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() {
		logger.Info("kubernetes MCP listening", "addr", addr)
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
