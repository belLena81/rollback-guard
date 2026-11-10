package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	kubernetesadapter "github.com/belLena81/rollback-guard/internal/adapters/kubernetes"
	protocol "github.com/belLena81/rollback-guard/internal/adapters/mcp"
	"github.com/belLena81/rollback-guard/internal/adapters/mcp/kubernetesserver"
	postgresadapter "github.com/belLena81/rollback-guard/internal/adapters/postgres"
	"github.com/belLena81/rollback-guard/internal/platform/bootstrap"
	"github.com/belLena81/rollback-guard/internal/platform/config"
	"github.com/belLena81/rollback-guard/internal/platform/httpserver"
	"github.com/belLena81/rollback-guard/internal/platform/telemetry"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/jackc/pgx/v5/pgxpool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const serviceName = "kubernetes MCP"

func main() {
	level := new(slog.LevelVar)
	logger := telemetry.NewLogger(os.Stdout, level)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	err := run(ctx, logger, level)
	stop()
	if err != nil {
		logger.Error("kubernetes MCP exited", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger, level *slog.LevelVar) error {
	cfg, err := config.LoadKubernetesMCP()
	if err != nil {
		return err
	}
	tel, stopTelemetry, err := bootstrap.StartTelemetry(cfg.Telemetry, logger, level)
	if err != nil {
		return err
	}
	defer stopTelemetry()
	clientset, err := buildKubernetes(cfg, tel)
	if err != nil {
		return err
	}
	// Only rollback mode connects to PostgreSQL, for the mutation guard.
	var pool *pgxpool.Pool
	if cfg.Mode == config.KubernetesMCPModeRollback {
		pool, err = bootstrap.NewPool(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
		if err != nil {
			return fmt.Errorf("rollback database: %w", err)
		}
		defer pool.Close()
	}
	backend, err := buildServices(cfg, clientset, pool)
	if err != nil {
		return err
	}
	handler, err := buildHTTP(cfg, backend, readiness(cfg, clientset, pool), tel, logger)
	if err != nil {
		return err
	}
	return httpserver.Serve(ctx, httpserver.Config{Name: serviceName, Addr: cfg.HTTPAddr}, handler, logger)
}

// buildKubernetes creates the API client for the process's own service
// account; its calls join the tool call's trace.
func buildKubernetes(cfg config.KubernetesMCPConfig, tel *telemetry.Provider) (kubernetes.Interface, error) {
	restConfig, err := kubernetesConfig(cfg.Kubeconfig)
	if err != nil {
		return nil, err
	}
	restConfig.Timeout = cfg.RequestTimeout
	restConfig.Wrap(tel.Transport)
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create kubernetes client: %w", err)
	}
	return clientset, nil
}

// buildServices creates the Kubernetes backend. A nil pool (readonly mode)
// leaves it without a mutation guard.
func buildServices(cfg config.KubernetesMCPConfig, clientset kubernetes.Interface, pool *pgxpool.Pool) (*kubernetesadapter.Service, error) {
	var mutationGuard ports.RollbackMutationGuard
	if pool != nil {
		guard, err := postgresadapter.NewRollbackMutationGuard(pool)
		if err != nil {
			return nil, err
		}
		mutationGuard = guard
	}
	return kubernetesadapter.NewService(clientset, kubernetesadapter.Config{ClusterID: cfg.ClusterID, AllowedTargets: cfg.AllowedTargets, MutationGuard: mutationGuard})
}

// readiness checks PostgreSQL (rollback mode only) and that the first
// allowed Deployment is readable with the process's credentials.
func readiness(cfg config.KubernetesMCPConfig, clientset kubernetes.Interface, pool *pgxpool.Pool) httpserver.ReadyCheck {
	return func(ctx context.Context) error {
		if pool != nil {
			if err := pool.Ping(ctx); err != nil {
				return err
			}
		}
		target := cfg.AllowedTargets[0]
		_, err := clientset.AppsV1().Deployments(target.Namespace()).Get(ctx, target.Deployment(), metav1.GetOptions{})
		return err
	}
}

// buildHTTP assembles the MCP server for the configured mode.
func buildHTTP(cfg config.KubernetesMCPConfig, backend kubernetesserver.Backend, ready httpserver.ReadyCheck, tel *telemetry.Provider, logger *slog.Logger) (http.Handler, error) {
	mode := kubernetesserver.Mode(cfg.Mode)
	serverName := protocol.ServerKubernetesReadonly
	if mode == kubernetesserver.ModeRollback {
		serverName = protocol.ServerKubernetesRollback
	}
	handler, err := kubernetesserver.NewHTTPHandler(backend, mode, ready, cfg.SessionTimeout, logger,
		protocol.WithMetricsHandler(tel.MetricsHandler()),
		protocol.WithToolObserver(serverName, tel),
		// Zero in readonly mode, which selects the shared default.
		protocol.WithReadyTimeout(cfg.DBPingTimeout),
	)
	if err != nil {
		return nil, err
	}
	return tel.InstrumentServer(handler, protocol.RouteOf), nil
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
