package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	a2aadapter "github.com/belLena81/rollback-guard/internal/adapters/a2a"
	"github.com/belLena81/rollback-guard/internal/adapters/diagnosis"
	llmadapter "github.com/belLena81/rollback-guard/internal/adapters/llm"
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
	"github.com/jackc/pgx/v5/pgxpool"
)

const serviceName = "investigator"

func main() {
	level := new(slog.LevelVar)
	logger := telemetry.NewLogger(os.Stdout, level)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	err := run(ctx, logger, level)
	stop()
	if err != nil {
		logger.Error("investigator exited", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger, level *slog.LevelVar) error {
	cfg, err := config.LoadInvestigator()
	if err != nil {
		return fmt.Errorf("load investigator: %w", err)
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
	clients, err := buildMCP(ctx, cfg, tel, logger)
	if err != nil {
		return err
	}
	defer clients.close(logger)
	investigator, err := buildServices(cfg, pool, clients, tel)
	if err != nil {
		return err
	}
	ready := func(ctx context.Context) error {
		if err := pool.Ping(ctx); err != nil {
			return fmt.Errorf("ping database: %w", err)
		}
		return clients.ping(ctx)
	}
	handler, err := buildHTTP(cfg, investigator, pool, ready, tel, logger)
	if err != nil {
		return err
	}
	logger.Info("investigator agent card", "public_url", cfg.PublicURL)
	if err := httpserver.Serve(ctx, httpserver.Config{Name: serviceName, Addr: cfg.HTTPAddr}, handler, logger); err != nil {
		return fmt.Errorf("serve investigator: %w", err)
	}
	return nil
}

// mcpClients are the investigator's two read-only MCP sessions.
type mcpClients struct {
	read          *mcpadapter.Client
	observability *mcpadapter.Client
}

func (c mcpClients) close(logger *slog.Logger) {
	bootstrap.CloseMCP(c.observability, "observability", logger)
	bootstrap.CloseMCP(c.read, "readonly kubernetes", logger)
}

func (c mcpClients) ping(ctx context.Context) error {
	if err := c.read.Ping(ctx); err != nil {
		return fmt.Errorf("ping mcp server: %w", err)
	}
	if err := c.observability.Ping(ctx); err != nil {
		return fmt.Errorf("ping observability mcp: %w", err)
	}
	return nil
}

// buildMCP connects the two MCP sessions the investigator collects evidence
// through and fails closed unless each server serves exactly its full
// catalog. The investigator never connects to the rollback server. On
// failure it closes the session it already opened.
func buildMCP(ctx context.Context, cfg config.InvestigatorConfig, tel *telemetry.Provider, logger *slog.Logger) (clients mcpClients, err error) {
	connectCtx, cancel := context.WithTimeout(ctx, cfg.MCPConnectTimeout)
	defer cancel()
	defer func() {
		if err != nil {
			clients.close(logger)
			clients = mcpClients{}
		}
	}()
	httpClient := tel.HTTPClient(cfg.MCPCallTimeout)
	clients.read, err = bootstrap.ConnectMCP(connectCtx, mcpadapter.ClientInvestigator, httpClient, bootstrap.MCPServer{
		Name:     "readonly kubernetes",
		Endpoint: cfg.KubernetesReadMCPURL,
		Tools:    mcpadapter.KubernetesReadonlyCatalog(),
	})
	if err != nil {
		return clients, fmt.Errorf("connect mcp: %w", err)
	}
	clients.observability, err = bootstrap.ConnectMCP(connectCtx, mcpadapter.ClientInvestigator, httpClient, bootstrap.MCPServer{
		Name:     "observability",
		Endpoint: cfg.ObservabilityMCPURL,
		Tools:    mcpadapter.ObservabilityCatalog(),
	})
	if err != nil {
		return clients, fmt.Errorf("connect observability mcp: %w", err)
	}
	return clients, nil
}

// buildServices wires the investigation use case: evidence through MCP,
// diagnosis by the configured strategy, deduplicated by idempotency key.
func buildServices(cfg config.InvestigatorConfig, pool *pgxpool.Pool, clients mcpClients, tel *telemetry.Provider) (ports.Investigator, error) {
	read, err := bootstrap.ObservedCalls(clients.read, cfg.MCPCallTimeout, mcpadapter.ServerKubernetesReadonly, tel)
	if err != nil {
		return nil, fmt.Errorf("observe mcp calls: %w", err)
	}
	observability, err := bootstrap.ObservedCalls(clients.observability, cfg.MCPCallTimeout, mcpadapter.ServerObservability, tel)
	if err != nil {
		return nil, fmt.Errorf("observe mcp calls: %w", err)
	}
	rawInspector, err := mcpadapter.NewDeploymentInspector(read)
	if err != nil {
		return nil, fmt.Errorf("create deployment inspector: %w", err)
	}
	rawMetrics, err := mcpadapter.NewMetricsAdapter(observability, cfg.MetricsWindow)
	if err != nil {
		return nil, fmt.Errorf("create metrics adapter: %w", err)
	}
	readProtector, err := bootstrap.NewProtector(resilience.DependencyKubernetesReadonlyMCP, cfg.Resilience, tel)
	if err != nil {
		return nil, fmt.Errorf("create protector: %w", err)
	}
	observabilityProtector, err := bootstrap.NewProtector(resilience.DependencyObservabilityMCP, cfg.Resilience, tel)
	if err != nil {
		return nil, fmt.Errorf("create protector: %w", err)
	}
	inspector, err := resilience.NewInspector(rawInspector, readProtector)
	if err != nil {
		return nil, fmt.Errorf("protect deployment inspector: %w", err)
	}
	metrics, err := resilience.NewMetrics(rawMetrics, observabilityProtector)
	if err != nil {
		return nil, fmt.Errorf("protect metrics port: %w", err)
	}
	ids := &runtime.IDGenerator{}
	// The LLM provider client is deliberately not traced: its requests carry
	// the API key and prompt, and nothing about them is exported.
	strategyDiagnoser, err := newDiagnoser(cfg, ids, &http.Client{Timeout: cfg.LLMTimeout}, tel)
	if err != nil {
		return nil, err
	}
	diagnoser, err := tel.ObserveDiagnoser(strategyDiagnoser, cfg.DiagnosisStrategy)
	if err != nil {
		return nil, fmt.Errorf("observe diagnoser provider: %w", err)
	}
	runner, err := application.NewInvestigationRunner(inspector, metrics, diagnoser, runtime.Clock{}, ids)
	if err != nil {
		return nil, fmt.Errorf("create investigation runner: %w", err)
	}
	ledger, err := postgres.NewInvestigationRequestLedger(pool)
	if err != nil {
		return nil, fmt.Errorf("create investigation request ledger: %w", err)
	}
	investigator, err := a2aadapter.NewIdempotentInvestigator(ledger, runner, cfg.TaskTimeout, runtime.Clock{})
	if err != nil {
		return nil, fmt.Errorf("create idempotent investigator: %w", err)
	}
	return investigator, nil
}

// buildHTTP assembles the A2A server with durable task storage.
func buildHTTP(
	cfg config.InvestigatorConfig,
	investigator ports.Investigator,
	pool *pgxpool.Pool,
	ready httpserver.ReadyCheck,
	tel *telemetry.Provider,
	logger *slog.Logger,
) (http.Handler, error) {
	records, err := postgres.NewA2ATaskRecords(pool)
	if err != nil {
		return nil, fmt.Errorf("create a2a task records: %w", err)
	}
	store, err := a2aadapter.NewDurableTaskStore(records, runtime.Clock{})
	if err != nil {
		return nil, fmt.Errorf("create durable task store: %w", err)
	}
	handler, err := a2aadapter.NewServerHandler(investigator, store, logger, a2aadapter.ServerConfig{
		PublicURL:    cfg.PublicURL,
		TaskTimeout:  cfg.TaskTimeout,
		Ready:        ready,
		ReadyTimeout: cfg.DBPingTimeout,
		Tracer:       tel,
		Metrics:      tel.MetricsHandler(),
	})
	if err != nil {
		return nil, fmt.Errorf("create server handler: %w", err)
	}
	return tel.InstrumentServer(handler, a2aadapter.RouteOf), nil
}

// newDiagnoser builds the configured strategy. The LLM is wrapped in retry
// and a circuit breaker; an open breaker reads as an unavailable provider, so
// the rule diagnoser stays in charge. observer may be nil.
func newDiagnoser(
	cfg config.InvestigatorConfig,
	ids ports.IDGenerator,
	providerHTTPClient *http.Client,
	observer resilience.Observer,
) (ports.Diagnoser, error) {
	rules, err := diagnosis.NewRuleDiagnoser(ids, diagnosis.RuleDiagnoserConfig{CorrelationWindow: cfg.RuleCorrelationWindow})
	if err != nil {
		return nil, fmt.Errorf("create rule diagnoser: %w", err)
	}
	if cfg.DiagnosisStrategy == domain.StrategyRules {
		return rules, nil
	}
	rawReasoner, err := llmadapter.NewOpenAICompatibleReasoner(llmadapter.OpenAICompatibleConfig{
		BaseURL: cfg.LLMBaseURL, APIKey: cfg.LLMAPIKey, Model: cfg.LLMModel,
		MaxOutputTokens: cfg.LLMMaxOutputTokens, HTTPClient: providerHTTPClient,
	})
	if err != nil {
		return nil, fmt.Errorf("create open ai compatible reasoner: %w", err)
	}
	llmProtector, err := bootstrap.NewLLMProtector(cfg.Resilience, observer)
	if err != nil {
		return nil, fmt.Errorf("create llm protector: %w", err)
	}
	reasoner, err := resilience.NewReasoner(rawReasoner, llmProtector)
	if err != nil {
		return nil, fmt.Errorf("create reasoner: %w", err)
	}
	modelConfig := application.ModelDiagnoserConfig{
		Model: cfg.LLMModel, PromptVersion: application.DiagnosisPromptVersion, Timeout: cfg.LLMTimeout,
	}
	switch cfg.DiagnosisStrategy {
	case domain.StrategyRules:
		return rules, nil
	case domain.StrategyLLM:
		return wrapDiagnoser(application.NewLLMDiagnoser(reasoner, rules, ids, modelConfig))
	case domain.StrategyHybrid:
		return wrapDiagnoser(application.NewHybridDiagnoser(reasoner, rules, ids, modelConfig))
	default:
		return nil, fmt.Errorf("unsupported diagnosis strategy %q", cfg.DiagnosisStrategy)
	}
}

// wrapDiagnoser adds the operation name to a diagnoser constructor failure and
// keeps a nil concrete pointer from becoming a non-nil interface.
func wrapDiagnoser[T ports.Diagnoser](diagnoser T, err error) (ports.Diagnoser, error) {
	if err != nil {
		return nil, fmt.Errorf("create model diagnoser: %w", err)
	}
	return diagnoser, nil
}
