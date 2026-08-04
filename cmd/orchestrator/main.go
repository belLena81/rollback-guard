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

	"github.com/belLena81/rollback-guard/internal/adapters/diagnosis"
	"github.com/belLena81/rollback-guard/internal/adapters/httpapi"
	mcpadapter "github.com/belLena81/rollback-guard/internal/adapters/mcp"
	"github.com/belLena81/rollback-guard/internal/adapters/postgres"
	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/config"
	"github.com/belLena81/rollback-guard/internal/platform/httpserver"
	"github.com/belLena81/rollback-guard/internal/platform/runtime"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("orchestrator exited", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	poolConfig, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	poolConfig.MaxConns = cfg.DBMaxConns
	pool, err := pgxpool.NewWithConfig(context.Background(), poolConfig)
	if err != nil {
		return err
	}
	defer pool.Close()
	uow, err := postgres.NewUnitOfWork(pool)
	if err != nil {
		return err
	}
	clock := runtime.Clock{}
	ids := &runtime.IDGenerator{}
	diagnoser, err := diagnosis.NewRuleDiagnoser(ids, diagnosis.RuleDiagnoserConfig{CorrelationWindow: cfg.RuleCorrelationWindow})
	if err != nil {
		return err
	}
	policy, err := domain.NewEligibilityPolicy(domain.EligibilityPolicyConfig{
		MinimumConfidence: cfg.RuleMinimumConfidence,
		CorrelationWindow: cfg.RuleCorrelationWindow,
		AllowedTargets:    cfg.AllowedTargets,
	})
	if err != nil {
		return err
	}
	systemActor, err := domain.NewActor(cfg.SystemActorID, domain.ActorSystem)
	if err != nil {
		return err
	}
	localActor, err := domain.NewActor(cfg.LocalActorID, domain.ActorHuman)
	if err != nil {
		return err
	}
	connectCtx, cancelConnect := context.WithTimeout(context.Background(), cfg.MCPConnectTimeout)
	defer cancelConnect()
	mcpHTTPClient := &http.Client{Timeout: cfg.MCPWatchTimeout + cfg.MCPCallTimeout}
	readClient, err := connectMCP(connectCtx, cfg.KubernetesReadMCPURL, mcpHTTPClient, []string{mcpadapter.ToolGetDeployment, mcpadapter.ToolGetReplicaSets, mcpadapter.ToolListPods, mcpadapter.ToolGetPodEvents, mcpadapter.ToolGetRolloutStatus, mcpadapter.ToolWatchRollout})
	if err != nil {
		return fmt.Errorf("connect readonly kubernetes MCP: %w", err)
	}
	defer closeMCP(readClient, "readonly kubernetes", logger)
	writeClient, err := connectMCP(connectCtx, cfg.KubernetesWriteMCPURL, mcpHTTPClient, []string{mcpadapter.ToolPrepareRollback, mcpadapter.ToolExecuteApprovedRollback})
	if err != nil {
		return fmt.Errorf("connect rollback kubernetes MCP: %w", err)
	}
	defer closeMCP(writeClient, "rollback kubernetes", logger)
	observabilityClient, err := connectMCP(connectCtx, cfg.ObservabilityMCPURL, mcpHTTPClient, []string{mcpadapter.ToolGetHTTPErrorRate, mcpadapter.ToolCompareErrorRateWindows, mcpadapter.ToolGetRequestLatency, mcpadapter.ToolGetPodCPUUsage, mcpadapter.ToolGetPodMemoryUsage, mcpadapter.ToolEvaluateRecovery})
	if err != nil {
		return fmt.Errorf("connect observability MCP: %w", err)
	}
	defer closeMCP(observabilityClient, "observability", logger)
	boundedRead, err := mcpadapter.BoundCalls(readClient, cfg.MCPCallTimeout)
	if err != nil {
		return err
	}
	boundedWrite, err := mcpadapter.BoundCalls(writeClient, cfg.MCPCallTimeout)
	if err != nil {
		return err
	}
	boundedObservability, err := mcpadapter.BoundCalls(observabilityClient, cfg.MCPCallTimeout)
	if err != nil {
		return err
	}
	inspector, err := mcpadapter.NewDeploymentInspector(boundedRead)
	if err != nil {
		return err
	}
	metrics, err := mcpadapter.NewMetricsAdapter(boundedObservability, cfg.MetricsWindow)
	if err != nil {
		return err
	}
	executor, err := mcpadapter.NewRollbackExecutor(boundedWrite, boundedRead, cfg.MCPWatchTimeout)
	if err != nil {
		return err
	}
	services, err := application.NewServices(application.Dependencies{
		UnitOfWork:     uow,
		Inspector:      inspector,
		Metrics:        metrics,
		Diagnoser:      diagnoser,
		Executor:       executor,
		RollbackPolicy: policy,
		ApprovalPolicy: domain.ApprovalAuthorizer{},
		Clock:          clock,
		IDGenerator:    ids,
	}, application.WorkflowConfig{
		ProposalTTL:    cfg.ProposalTTL,
		ApprovalTTL:    cfg.ApprovalTTL,
		CleanupTimeout: cfg.CleanupTimeout,
		SystemActor:    systemActor,
	})
	if err != nil {
		return err
	}
	auth, err := httpserver.NewLocalAuthMiddleware(localActor)
	if err != nil {
		return err
	}
	api, err := httpapi.NewServer(services, logger)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: api.Routes(func(ctx context.Context) error {
			if err := pool.Ping(ctx); err != nil {
				return err
			}
			for _, client := range []*mcpadapter.Client{readClient, writeClient, observabilityClient} {
				if err := client.Ping(ctx); err != nil {
					return err
				}
			}
			return nil
		}, cfg.DBPingTimeout, auth),
		ReadHeaderTimeout: 5 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() {
		logger.Info("orchestrator listening", "addr", cfg.HTTPAddr)
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

func connectMCP(ctx context.Context, endpoint string, httpClient *http.Client, tools []string) (*mcpadapter.Client, error) {
	client, err := mcpadapter.Connect(ctx, endpoint, httpClient)
	if err != nil {
		return nil, err
	}
	if err := client.RequireTools(ctx, tools); err != nil {
		return nil, errors.Join(err, client.Close())
	}
	return client, nil
}

func closeMCP(client *mcpadapter.Client, name string, logger *slog.Logger) {
	if err := client.Close(); err != nil {
		logger.Warn("close MCP client", "server", name, "error", err)
	}
}
