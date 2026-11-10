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

	a2aadapter "github.com/belLena81/rollback-guard/internal/adapters/a2a"
	"github.com/belLena81/rollback-guard/internal/adapters/githuboauth"
	"github.com/belLena81/rollback-guard/internal/adapters/httpapi"
	mcpadapter "github.com/belLena81/rollback-guard/internal/adapters/mcp"
	"github.com/belLena81/rollback-guard/internal/adapters/postgres"
	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/bootstrap"
	"github.com/belLena81/rollback-guard/internal/platform/config"
	"github.com/belLena81/rollback-guard/internal/platform/httpserver"
	"github.com/belLena81/rollback-guard/internal/platform/resilience"
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
		return fmt.Errorf("load: %w", err)
	}
	tel, stopTelemetry, err := bootstrap.StartTelemetry(ctx, cfg.Telemetry, logger, level)
	if err != nil {
		return fmt.Errorf("start telemetry: %w", err)
	}
	defer stopTelemetry()
	pool, err := bootstrap.NewPool(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()
	store, err := postgres.NewUnitOfWork(pool)
	if err != nil {
		return fmt.Errorf("create unit of work: %w", err)
	}
	uow, err := tel.ObserveUnitOfWork(store)
	if err != nil {
		return fmt.Errorf("observe unit of work provider: %w", err)
	}
	clients, err := buildMCP(ctx, cfg, tel, logger)
	if err != nil {
		return err
	}
	defer clients.close(logger)
	authStore, err := postgres.NewAuthUnitOfWork(pool)
	if err != nil {
		return fmt.Errorf("create auth unit of work: %w", err)
	}
	services, err := buildServices(cfg, uow, authStore, clients, tel)
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
			return fmt.Errorf("ping database: %w", err)
		}
		return clients.ping(ctx)
	}
	handler, err := buildHTTP(cfg, services, authStore, ready, tel, logger)
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
			return fmt.Errorf("ping mcp server: %w", err)
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
		Tools:    mcpadapter.KubernetesReadonlyCatalog(),
	})
	if err != nil {
		return clients, fmt.Errorf("connect mcp: %w", err)
	}
	clients.write, err = bootstrap.ConnectMCP(connectCtx, mcpadapter.ClientOrchestrator, httpClient, bootstrap.MCPServer{
		Name:     "rollback kubernetes",
		Endpoint: cfg.KubernetesWriteMCPURL,
		Tools:    mcpadapter.KubernetesRollbackCatalog(),
	})
	if err != nil {
		return clients, fmt.Errorf("connect mcp: %w", err)
	}
	clients.observability, err = bootstrap.ConnectMCP(connectCtx, mcpadapter.ClientOrchestrator, httpClient, bootstrap.MCPServer{
		Name:     "observability",
		Endpoint: cfg.ObservabilityMCPURL,
		Tools:    mcpadapter.ObservabilityCatalog(),
	})
	if err != nil {
		return clients, fmt.Errorf("connect observability mcp: %w", err)
	}
	return clients, nil
}

// buildServices wires the application use cases onto the MCP-backed ports.
// buildAccessPolicy selects the authorization of human use cases by
// AUTH_MODE: local allows only the configured local actor; github resolves
// the actor's live role from the users table on every call.
func buildAccessPolicy(cfg config.Config, authStore ports.AuthUnitOfWork) (application.AccessPolicy, error) {
	switch cfg.Auth.Mode {
	case config.AuthModeLocal:
		localActor, err := domain.NewActor(cfg.LocalActorID, domain.ActorHuman)
		if err != nil {
			return nil, err
		}
		access, err := application.NewLocalAccessPolicy(localActor)
		if err != nil {
			return nil, fmt.Errorf("create local access policy: %w", err)
		}
		return access, nil
	case config.AuthModeGitHub:
		access, err := application.NewRoleAccessPolicy(authStore)
		if err != nil {
			return nil, fmt.Errorf("create role access policy: %w", err)
		}
		return access, nil
	default:
		return nil, fmt.Errorf("unsupported auth mode %q", cfg.Auth.Mode)
	}
}

func buildServices(
	cfg config.Config,
	uow ports.UnitOfWork,
	authStore ports.AuthUnitOfWork,
	clients mcpClients,
	tel *telemetry.Provider,
) (application.Services, error) {
	access, err := buildAccessPolicy(cfg, authStore)
	if err != nil {
		return application.Services{}, err
	}
	read, err := bootstrap.ObservedCalls(clients.read, cfg.MCPCallTimeout, mcpadapter.ServerKubernetesReadonly, tel)
	if err != nil {
		return application.Services{}, fmt.Errorf("observe mcp calls: %w", err)
	}
	write, err := bootstrap.ObservedCalls(clients.write, cfg.MCPCallTimeout, mcpadapter.ServerKubernetesRollback, tel)
	if err != nil {
		return application.Services{}, fmt.Errorf("observe mcp calls: %w", err)
	}
	observability, err := bootstrap.ObservedCalls(clients.observability, cfg.MCPCallTimeout, mcpadapter.ServerObservability, tel)
	if err != nil {
		return application.Services{}, fmt.Errorf("observe mcp calls: %w", err)
	}
	inspector, err := mcpadapter.NewDeploymentInspector(read)
	if err != nil {
		return application.Services{}, fmt.Errorf("create deployment inspector: %w", err)
	}
	metrics, err := mcpadapter.NewMetricsAdapter(observability, cfg.MetricsWindow)
	if err != nil {
		return application.Services{}, fmt.Errorf("create metrics adapter: %w", err)
	}
	executor, err := mcpadapter.NewRollbackExecutor(write, read, cfg.MCPWatchTimeout, runtime.Clock{})
	if err != nil {
		return application.Services{}, fmt.Errorf("create rollback executor: %w", err)
	}
	// Reads retry transient failures behind one breaker per MCP server; the
	// rollback executor gets only the narrow not-sent retry (see
	// resilience.NewExecutor).
	readProtector, err := bootstrap.NewProtector(resilience.DependencyKubernetesReadonlyMCP, cfg.Resilience, tel)
	if err != nil {
		return application.Services{}, fmt.Errorf("create protector: %w", err)
	}
	observabilityProtector, err := bootstrap.NewProtector(resilience.DependencyObservabilityMCP, cfg.Resilience, tel)
	if err != nil {
		return application.Services{}, fmt.Errorf("create protector: %w", err)
	}
	mutationRetrier, err := bootstrap.NewMutationRetrier(resilience.DependencyKubernetesRollbackMCP, cfg.Resilience, tel)
	if err != nil {
		return application.Services{}, fmt.Errorf("create mutation retrier: %w", err)
	}
	protectedInspector, err := resilience.NewInspector(inspector, readProtector)
	if err != nil {
		return application.Services{}, fmt.Errorf("protect deployment inspector: %w", err)
	}
	protectedMetrics, err := resilience.NewMetrics(metrics, observabilityProtector)
	if err != nil {
		return application.Services{}, fmt.Errorf("protect metrics port: %w", err)
	}
	protectedExecutor, err := resilience.NewExecutor(executor, mutationRetrier)
	if err != nil {
		return application.Services{}, fmt.Errorf("create executor: %w", err)
	}
	ids := &runtime.IDGenerator{}
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
	services, err := application.NewServices(application.Dependencies{
		UnitOfWork:     uow,
		Inspector:      protectedInspector,
		Metrics:        protectedMetrics,
		Executor:       protectedExecutor,
		RollbackPolicy: policy,
		ApprovalPolicy: domain.ApprovalAuthorizer{},
		Clock:          runtime.Clock{},
		IDGenerator:    ids,
		Access:         access,
		Tracer:         tel,
	}, application.WorkflowConfig{
		ProposalTTL:          cfg.ProposalTTL,
		ApprovalTTL:          cfg.ApprovalTTL,
		CleanupTimeout:       cfg.CleanupTimeout,
		SystemActor:          systemActor,
		RecoveryTimeout:      cfg.RecoveryTimeout,
		RecoveryPollInterval: cfg.RecoveryPollInterval,
	})
	if err != nil {
		return application.Services{}, fmt.Errorf("create application services: %w", err)
	}
	return services, nil
}

// buildCoordinator connects the investigator over A2A and builds the
// background worker that delegates investigations to it. The returned
// function closes the A2A client.
func buildCoordinator(
	ctx context.Context,
	cfg config.Config,
	uow ports.UnitOfWork,
	services application.Services,
	tel *telemetry.Provider,
	logger *slog.Logger,
) (*application.InvestigationCoordinator, func(), error) {
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
	observedGateway, err := tel.ObserveGateway(a2aGateway)
	if err != nil {
		closeGateway()
		return nil, nil, fmt.Errorf("observe gateway provider: %w", err)
	}
	a2aProtector, err := bootstrap.NewProtector(resilience.DependencyA2AInvestigator, cfg.Resilience, tel)
	if err != nil {
		closeGateway()
		return nil, nil, fmt.Errorf("create protector: %w", err)
	}
	// Outside the observed gateway, so every attempt is counted as a call.
	gateway, err := resilience.NewGateway(observedGateway, a2aProtector)
	if err != nil {
		closeGateway()
		return nil, nil, fmt.Errorf("create gateway: %w", err)
	}
	coordinator, err := application.NewInvestigationCoordinator(
		uow,
		gateway,
		services.Investigations,
		runtime.Clock{},
		application.InvestigationCoordinatorConfig{
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
		},
	)
	if err != nil {
		closeGateway()
		return nil, nil, fmt.Errorf("create investigation coordinator: %w", err)
	}
	return coordinator, closeGateway, nil
}

// buildHTTP assembles the REST API behind local authentication.
func buildHTTP(
	cfg config.Config,
	services application.Services,
	authStore ports.AuthUnitOfWork,
	ready httpserver.ReadyCheck,
	tel *telemetry.Provider,
	logger *slog.Logger,
) (http.Handler, error) {
	auth, loginOptions, err := buildAuth(cfg, authStore, logger)
	if err != nil {
		return nil, err
	}
	options := append([]httpapi.Option{httpapi.WithRouteMiddleware(tel.Route), httpapi.WithMetricsHandler(tel.MetricsHandler())}, loginOptions...)
	api, err := httpapi.NewServer(services, logger, options...)
	if err != nil {
		return nil, fmt.Errorf("create api server: %w", err)
	}
	return api.Routes(ready, cfg.DBPingTimeout, auth), nil
}

// buildAuth selects request authentication by AUTH_MODE. Local mode injects
// LOCAL_ACTOR_ID and registers no login routes (they answer 404); github mode
// requires a server-side session issued by the GitHub OAuth login.
func buildAuth(cfg config.Config, authStore ports.AuthUnitOfWork, logger *slog.Logger) (httpserver.AuthMiddleware, []httpapi.Option, error) {
	if cfg.Session.CookieSecure {
		logger.Info("authentication configured", "auth_mode", string(cfg.Auth.Mode))
	} else {
		logger.Warn("authentication configured with SESSION_COOKIE_SECURE=false; cookies are sent over plain HTTP", "auth_mode", string(cfg.Auth.Mode))
	}
	switch cfg.Auth.Mode {
	case config.AuthModeLocal:
		localActor, err := domain.NewActor(cfg.LocalActorID, domain.ActorHuman)
		if err != nil {
			return nil, nil, err
		}
		auth, err := httpserver.NewLocalAuthMiddleware(localActor)
		if err != nil {
			return nil, nil, fmt.Errorf("create local auth middleware: %w", err)
		}
		return auth, nil, nil
	case config.AuthModeGitHub:
		return buildGitHubAuth(cfg, authStore, logger)
	default:
		return nil, nil, fmt.Errorf("unsupported auth mode %q", cfg.Auth.Mode)
	}
}

func buildGitHubAuth(cfg config.Config, authStore ports.AuthUnitOfWork, logger *slog.Logger) (httpserver.AuthMiddleware, []httpapi.Option, error) {
	github := cfg.Auth.GitHub
	ids := &runtime.IDGenerator{}
	// Session tokens, login state and PKCE verifiers come from crypto/rand.
	random := runtime.RandomSource{}
	sessions, err := application.NewSessionService(
		application.SessionDependencies{UnitOfWork: authStore, Clock: runtime.Clock{}, IDGenerator: ids, Random: random},
		cfg.Session.TTL,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("create session service: %w", err)
	}
	// The token exchange is not retried: an authorization code is single-use.
	provider, err := githuboauth.New(githuboauth.Config{
		ClientID:     github.ClientID,
		ClientSecret: github.ClientSecret.Reveal(),
		RedirectURL:  github.RedirectURL,
		BaseURL:      github.BaseURL,
		APIURL:       github.APIURL,
		Timeout:      github.HTTPTimeout,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("create github identity provider: %w", err)
	}
	login, err := application.NewLoginService(
		application.LoginDependencies{Sessions: sessions, UnitOfWork: authStore, Clock: runtime.Clock{}, IDGenerator: ids, Random: random, Provider: provider},
		application.LoginPolicy{
			AllowedOrganization: github.AllowedOrganization,
			BootstrapAdmins:     github.BootstrapAdmins,
			StateKey:            []byte(github.StateSecret.Reveal()),
		},
	)
	if err != nil {
		return nil, nil, fmt.Errorf("create login service: %w", err)
	}
	auth, err := httpserver.NewSessionAuthMiddleware(sessions, logger)
	if err != nil {
		return nil, nil, fmt.Errorf("create session auth middleware: %w", err)
	}
	access, err := application.NewRoleAccessPolicy(authStore)
	if err != nil {
		return nil, nil, fmt.Errorf("create access policy: %w", err)
	}
	admin, err := application.NewAdminService(application.AdminDependencies{UnitOfWork: authStore, Access: access, Clock: runtime.Clock{}, IDGenerator: ids})
	if err != nil {
		return nil, nil, fmt.Errorf("create admin service: %w", err)
	}
	return auth, []httpapi.Option{httpapi.WithGitHubLogin(login, cfg.Session.CookieSecure), httpapi.WithUserAdmin(admin)}, nil
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
	coordinatorDone := make(chan struct{})
	serverErr := make(chan error, 1)
	go func() {
		defer close(coordinatorDone)
		forwardCoordinatorError(ctx, coordinatorErr, coordinator.Run(ctx))
	}()
	go func() {
		serverErr <- httpserver.Serve(ctx, httpserver.Config{Name: serviceName, Addr: cfg.HTTPAddr}, handler, logger)
	}()
	select {
	case err := <-serverErr:
		if ctx.Err() == nil {
			// The server failed while running (for example it could not
			// listen); the process ends with that error.
			return err
		}
		return errors.Join(awaitCoordinator(coordinatorDone, coordinatorErr), err)
	case err := <-coordinatorErr:
		if ctx.Err() == nil {
			return err
		}
		return errors.Join(err, <-serverErr)
	}
}

// awaitCoordinator waits, after shutdown began and the HTTP server drained,
// for the coordinator to stop, so its failure is reported and it no longer
// uses the dependencies the caller is about to close. The wait is bounded
// like the server's own shutdown.
func awaitCoordinator(done <-chan struct{}, coordinatorErr <-chan error) error {
	select {
	case <-done:
	case <-time.After(httpserver.ShutdownTimeout):
		return errors.New("investigation coordinator did not stop in time")
	}
	select {
	case err := <-coordinatorErr:
		return err
	default:
		return nil
	}
}

func forwardCoordinatorError(ctx context.Context, errCh chan<- error, err error) {
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return
	}
	errCh <- err
}
