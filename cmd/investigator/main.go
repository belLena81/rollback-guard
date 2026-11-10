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
	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/bootstrap"
	"github.com/belLena81/rollback-guard/internal/platform/config"
	"github.com/belLena81/rollback-guard/internal/platform/httpserver"
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
			return err
		}
		return clients.ping(ctx)
	}
	handler, err := buildHTTP(cfg, investigator, pool, ready, tel, logger)
	if err != nil {
		return err
	}
	logger.Info("investigator agent card", "public_url", cfg.PublicURL)
	return httpserver.Serve(ctx, httpserver.Config{Name: serviceName, Addr: cfg.HTTPAddr}, handler, logger)
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
		return err
	}
	return c.observability.Ping(ctx)
}

// buildMCP connects the two MCP sessions the investigator collects evidence
// through. On failure it closes the session it already opened.
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
		Tools:    []string{mcpadapter.ToolGetDeployment},
	})
	if err != nil {
		return clients, err
	}
	clients.observability, err = bootstrap.ConnectMCP(connectCtx, mcpadapter.ClientInvestigator, httpClient, bootstrap.MCPServer{
		Name:     "observability",
		Endpoint: cfg.ObservabilityMCPURL,
		Tools:    []string{mcpadapter.ToolGetHTTPErrorRate},
	})
	return clients, err
}

// buildServices wires the investigation use case: evidence through MCP,
// diagnosis by the configured strategy, deduplicated by idempotency key.
func buildServices(cfg config.InvestigatorConfig, pool *pgxpool.Pool, clients mcpClients, tel *telemetry.Provider) (ports.Investigator, error) {
	read, err := bootstrap.ObservedCalls(clients.read, cfg.MCPCallTimeout, mcpadapter.ServerKubernetesReadonly, tel)
	if err != nil {
		return nil, err
	}
	observability, err := bootstrap.ObservedCalls(clients.observability, cfg.MCPCallTimeout, mcpadapter.ServerObservability, tel)
	if err != nil {
		return nil, err
	}
	inspector, err := mcpadapter.NewDeploymentInspector(read)
	if err != nil {
		return nil, err
	}
	metrics, err := mcpadapter.NewMetricsAdapter(observability, cfg.MetricsWindow)
	if err != nil {
		return nil, err
	}
	ids := &runtime.IDGenerator{}
	// The LLM provider client is deliberately not traced: its requests carry
	// the API key and prompt, and nothing about them is exported.
	strategyDiagnoser, err := newDiagnoser(cfg, ids, &http.Client{Timeout: cfg.LLMTimeout})
	if err != nil {
		return nil, err
	}
	diagnoser, err := tel.ObserveDiagnoser(strategyDiagnoser, cfg.DiagnosisStrategy)
	if err != nil {
		return nil, err
	}
	runner, err := application.NewInvestigationRunner(inspector, metrics, diagnoser, runtime.Clock{}, ids)
	if err != nil {
		return nil, err
	}
	return a2aadapter.NewIdempotentInvestigator(pool, runner, cfg.TaskTimeout)
}

// buildHTTP assembles the A2A server with durable task storage.
func buildHTTP(cfg config.InvestigatorConfig, investigator ports.Investigator, pool *pgxpool.Pool, ready httpserver.ReadyCheck, tel *telemetry.Provider, logger *slog.Logger) (http.Handler, error) {
	store, err := a2aadapter.NewPostgresTaskStore(pool)
	if err != nil {
		return nil, err
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
		return nil, err
	}
	return tel.InstrumentServer(handler, a2aadapter.RouteOf), nil
}

func newDiagnoser(cfg config.InvestigatorConfig, ids ports.IDGenerator, providerHTTPClient *http.Client) (ports.Diagnoser, error) {
	rules, err := diagnosis.NewRuleDiagnoser(ids, diagnosis.RuleDiagnoserConfig{CorrelationWindow: cfg.RuleCorrelationWindow})
	if err != nil {
		return nil, err
	}
	if cfg.DiagnosisStrategy == domain.StrategyRules {
		return rules, nil
	}
	reasoner, err := llmadapter.NewOpenAICompatibleReasoner(llmadapter.OpenAICompatibleConfig{
		BaseURL: cfg.LLMBaseURL, APIKey: cfg.LLMAPIKey, Model: cfg.LLMModel,
		MaxOutputTokens: cfg.LLMMaxOutputTokens, HTTPClient: providerHTTPClient,
	})
	if err != nil {
		return nil, err
	}
	modelConfig := application.ModelDiagnoserConfig{
		Model: cfg.LLMModel, PromptVersion: application.DiagnosisPromptVersion, Timeout: cfg.LLMTimeout,
	}
	switch cfg.DiagnosisStrategy {
	case domain.StrategyRules:
		return rules, nil
	case domain.StrategyLLM:
		return application.NewLLMDiagnoser(reasoner, rules, ids, modelConfig)
	case domain.StrategyHybrid:
		return application.NewHybridDiagnoser(reasoner, rules, ids, modelConfig)
	default:
		return nil, fmt.Errorf("unsupported diagnosis strategy %q", cfg.DiagnosisStrategy)
	}
}
