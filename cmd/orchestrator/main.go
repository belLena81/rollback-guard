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

	a2aadapter "github.com/belLena81/rollback-guard/internal/adapters/a2a"
	"github.com/belLena81/rollback-guard/internal/adapters/diagnosis"
	"github.com/belLena81/rollback-guard/internal/adapters/httpapi"
	mcpadapter "github.com/belLena81/rollback-guard/internal/adapters/mcp"
	"github.com/belLena81/rollback-guard/internal/adapters/postgres"
	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/bootstrap"
	"github.com/belLena81/rollback-guard/internal/platform/config"
	"github.com/belLena81/rollback-guard/internal/platform/httpserver"
	"github.com/belLena81/rollback-guard/internal/platform/runtime"
	"github.com/belLena81/rollback-guard/internal/platform/telemetry"
	"github.com/belLena81/rollback-guard/internal/ports"
)

const serviceName = "orchestrator"

func main() {
	level := new(slog.LevelVar)
	logger := telemetry.NewLogger(os.Stdout, level)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	err := run(ctx, logger, level)
	stop()
	if err != nil {
		logger.Error("orchestrator exited", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger, level *slog.LevelVar) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	tel, stopTelemetry, err := bootstrap.StartTelemetry(cfg.Telemetry, logger, level)
	if err != nil {
		return err
	}
	defer stopTelemetry()
	pool, err := bootstrap.NewPool(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()
	store, err := postgres.NewUnitOfWork(pool)
	if err != nil {
		return err
	}
	uow, err := tel.ObserveUnitOfWork(store)
	if err != nil {
		return err
	}
	clients, err := buildMCP(ctx, cfg, tel, logger)
	if err != nil {
		return err
	}
	defer clients.close(logger)
	services, err := buildServices(cfg, uow, clients, tel)
	if err != nil {
		return err
	}
	coordinator, closeGateway, err := buildCoordinator(ctx, cfg, uow, services, tel, logger)
	if err != nil {
		return err
	}
	defer closeGateway()
	ready := func(ctx context.Context) error {
		if err := pool.Ping(ctx); err != nil {
			return err
		}
		return clients.ping(ctx)
	}
	handler, err := buildHTTP(cfg, services, ready, tel, logger)
	if err != nil {
		return err
	}
	return serve(ctx, cfg, handler, coordinator, logger)
}

// mcpClients are the orchestrator's three MCP sessions.
type mcpClients struct {
	read          *mcpadapter.Client
	write         *mcpadapter.Client
	observability *mcpadapter.Client
}

func (c mcpClients) close(logger *slog.Logger) {
	bootstrap.CloseMCP(c.observability, "observability", logger)
	bootstrap.CloseMCP(c.write, "rollback kubernetes", logger)
	bootstrap.CloseMCP(c.read, "readonly kubernetes", logger)
}

func (c mcpClients) ping(ctx context.Context) error {
	for _, client := range []*mcpadapter.Client{c.read, c.write, c.observability} {
		if err := client.Ping(ctx); err != nil {
			return err
		}
	}
	return nil
}

// buildMCP connects the three MCP sessions and fails closed unless each
// server serves exactly the expected tool catalog. On failure it closes the
// sessions it already opened.
func buildMCP(ctx context.Context, cfg config.Config, tel *telemetry.Provider, logger *slog.Logger) (clients mcpClients, err error) {
	connectCtx, cancel := context.WithTimeout(ctx, cfg.MCPConnectTimeout)
	defer cancel()
	defer func() {
		if err != nil {
			clients.close(logger)
			clients = mcpClients{}
		}
	}()
	// One HTTP request may carry a full rollout watch.
	httpClient := tel.HTTPClient(cfg.MCPWatchTimeout + cfg.MCPCallTimeout)
	clients.read, err = bootstrap.ConnectMCP(connectCtx, mcpadapter.ClientOrchestrator, httpClient, bootstrap.MCPServer{
		Name:     "readonly kubernetes",
		Endpoint: cfg.KubernetesReadMCPURL,
		Tools: []string{
			mcpadapter.ToolGetDeployment, mcpadapter.ToolGetReplicaSets, mcpadapter.ToolListPods,
			mcpadapter.ToolGetPodEvents, mcpadapter.ToolGetRolloutStatus, mcpadapter.ToolWatchRollout,
		},
	})
	if err != nil {
		return clients, err
	}
	clients.write, err = bootstrap.ConnectMCP(connectCtx, mcpadapter.ClientOrchestrator, httpClient, bootstrap.MCPServer{
		Name:     "rollback kubernetes",
		Endpoint: cfg.KubernetesWriteMCPURL,
		Tools:    []string{mcpadapter.ToolPrepareRollback, mcpadapter.ToolExecuteApprovedRollback},
	})
	if err != nil {
		return clients, err
	}
	clients.observability, err = bootstrap.ConnectMCP(connectCtx, mcpadapter.ClientOrchestrator, httpClient, bootstrap.MCPServer{
		Name:     "observability",
		Endpoint: cfg.ObservabilityMCPURL,
		Tools: []string{
			mcpadapter.ToolGetHTTPErrorRate, mcpadapter.ToolCompareErrorRateWindows, mcpadapter.ToolGetRequestLatency,
			mcpadapter.ToolGetPodCPUUsage, mcpadapter.ToolGetPodMemoryUsage, mcpadapter.ToolEvaluateRecovery,
		},
	})
	return clients, err
}

// buildServices wires the application use cases onto the MCP-backed ports.
func buildServices(cfg config.Config, uow ports.UnitOfWork, clients mcpClients, tel *telemetry.Provider) (application.Services, error) {
	read, err := bootstrap.ObservedCalls(clients.read, cfg.MCPCallTimeout, mcpadapter.ServerKubernetesReadonly, tel)
	if err != nil {
		return application.Services{}, err
	}
	write, err := bootstrap.ObservedCalls(clients.write, cfg.MCPCallTimeout, mcpadapter.ServerKubernetesRollback, tel)
	if err != nil {
		return application.Services{}, err
	}
	observability, err := bootstrap.ObservedCalls(clients.observability, cfg.MCPCallTimeout, mcpadapter.ServerObservability, tel)
	if err != nil {
		return application.Services{}, err
	}
	inspector, err := mcpadapter.NewDeploymentInspector(read)
	if err != nil {
		return application.Services{}, err
	}
	metrics, err := mcpadapter.NewMetricsAdapter(observability, cfg.MetricsWindow)
	if err != nil {
		return application.Services{}, err
	}
	executor, err := mcpadapter.NewRollbackExecutor(write, read, cfg.MCPWatchTimeout)
	if err != nil {
		return application.Services{}, err
	}
	ids := &runtime.IDGenerator{}
	diagnoser, err := diagnosis.NewRuleDiagnoser(ids, diagnosis.RuleDiagnoserConfig{CorrelationWindow: cfg.RuleCorrelationWindow})
	if err != nil {
		return application.Services{}, err
	}
	policy, err := domain.NewEligibilityPolicy(domain.EligibilityPolicyConfig{
		MinimumConfidence: cfg.RuleMinimumConfidence,
		CorrelationWindow: cfg.RuleCorrelationWindow,
		AllowedTargets:    cfg.AllowedTargets,
	})
	if err != nil {
		return application.Services{}, err
	}
	systemActor, err := domain.NewActor(cfg.SystemActorID, domain.ActorSystem)
	if err != nil {
		return application.Services{}, err
	}
	return application.NewServices(application.Dependencies{
		UnitOfWork:     uow,
		Inspector:      inspector,
		Metrics:        metrics,
		Diagnoser:      diagnoser,
		Executor:       executor,
		RollbackPolicy: policy,
		ApprovalPolicy: domain.ApprovalAuthorizer{},
		Clock:          runtime.Clock{},
		IDGenerator:    ids,
		Tracer:         tel,
	}, application.WorkflowConfig{
		ProposalTTL:    cfg.ProposalTTL,
		ApprovalTTL:    cfg.ApprovalTTL,
		CleanupTimeout: cfg.CleanupTimeout,
		SystemActor:    systemActor,
	})
}

// buildCoordinator connects the investigator over A2A and builds the
// background worker that delegates investigations to it. The returned
// function closes the A2A client.
func buildCoordinator(ctx context.Context, cfg config.Config, uow ports.UnitOfWork, services application.Services, tel *telemetry.Provider, logger *slog.Logger) (*application.InvestigationCoordinator, func(), error) {
	connectCtx, cancel := context.WithTimeout(ctx, cfg.A2AConnectTimeout)
	defer cancel()
	a2aGateway, err := a2aadapter.NewGateway(connectCtx, cfg.InvestigatorURL, tel.HTTPClient(cfg.A2ATaskTimeout+cfg.A2ACancelTimeout))
	if err != nil {
		return nil, nil, fmt.Errorf("connect investigator A2A agent: %w", err)
	}
	closeGateway := func() {
		if err := a2aGateway.Close(); err != nil {
			logger.Warn("close investigator A2A client", "error", err)
		}
	}
	gateway, err := tel.ObserveGateway(a2aGateway)
	if err != nil {
		closeGateway()
		return nil, nil, err
	}
	coordinator, err := application.NewInvestigationCoordinator(uow, gateway, services.Investigations, runtime.Clock{}, application.InvestigationCoordinatorConfig{
		WorkerID:      cfg.A2AWorkerID,
		TaskTimeout:   cfg.A2ATaskTimeout,
		CancelTimeout: cfg.A2ACancelTimeout,
		PollInterval:  cfg.A2APollInterval,
		LeaseTTL:      cfg.A2ALeaseTTL,
		MaxAttempts:   cfg.A2AMaxAttempts,
		Tracer:        tel,
		ReportError: func(ctx context.Context, err error) {
			logger.ErrorContext(ctx, "investigation coordinator iteration failed", "error", err)
		},
	})
	if err != nil {
		closeGateway()
		return nil, nil, err
	}
	return coordinator, closeGateway, nil
}

// buildHTTP assembles the REST API behind local authentication.
func buildHTTP(cfg config.Config, services application.Services, ready httpserver.ReadyCheck, tel *telemetry.Provider, logger *slog.Logger) (http.Handler, error) {
	localActor, err := domain.NewActor(cfg.LocalActorID, domain.ActorHuman)
	if err != nil {
		return nil, err
	}
	auth, err := httpserver.NewLocalAuthMiddleware(localActor)
	if err != nil {
		return nil, err
	}
	api, err := httpapi.NewServer(services, logger, httpapi.WithRouteMiddleware(tel.Route), httpapi.WithMetricsHandler(tel.MetricsHandler()))
	if err != nil {
		return nil, err
	}
	return api.Routes(ready, cfg.DBPingTimeout, auth), nil
}

// coordinatorRunner is the background worker served next to the API.
type coordinatorRunner interface {
	Run(context.Context) error
}

// serve runs the coordinator and the HTTP server until ctx is cancelled or
// either of them stops. A coordinator that stops while the process is
// running ends the process at once; during shutdown the HTTP server still
// drains first.
func serve(ctx context.Context, cfg config.Config, handler http.Handler, coordinator coordinatorRunner, logger *slog.Logger) error {
	coordinatorErr := make(chan error, 1)
	serverErr := make(chan error, 1)
	go func() {
		forwardCoordinatorError(ctx, coordinatorErr, coordinator.Run(ctx))
	}()
	go func() {
		serverErr <- httpserver.Serve(ctx, httpserver.Config{Name: serviceName, Addr: cfg.HTTPAddr}, handler, logger)
	}()
	select {
	case err := <-serverErr:
		return err
	case err := <-coordinatorErr:
		if ctx.Err() == nil {
			return err
		}
		return errors.Join(err, <-serverErr)
	}
}

func forwardCoordinatorError(ctx context.Context, errCh chan<- error, err error) {
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return
	}
	errCh <- err
}
