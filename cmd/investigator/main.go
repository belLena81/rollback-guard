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
	"github.com/belLena81/rollback-guard/internal/adapters/diagnosis"
	llmadapter "github.com/belLena81/rollback-guard/internal/adapters/llm"
	mcpadapter "github.com/belLena81/rollback-guard/internal/adapters/mcp"
	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/config"
	"github.com/belLena81/rollback-guard/internal/platform/runtime"
	"github.com/belLena81/rollback-guard/internal/platform/telemetry"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	level := new(slog.LevelVar)
	logger := telemetry.NewLogger(os.Stdout, level)
	if err := run(logger, level); err != nil {
		logger.Error("investigator exited", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger, level *slog.LevelVar) error {
	cfg, err := config.LoadInvestigator()
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
	connectCtx, cancelConnect := context.WithTimeout(context.Background(), cfg.MCPConnectTimeout)
	defer cancelConnect()
	mcpHTTPClient := tel.HTTPClient(cfg.MCPCallTimeout)
	readClient, err := connectMCP(connectCtx, cfg.KubernetesReadMCPURL, mcpHTTPClient, []string{mcpadapter.ToolGetDeployment})
	if err != nil {
		return fmt.Errorf("connect readonly kubernetes MCP: %w", err)
	}
	defer func() {
		if err := readClient.Close(); err != nil {
			logger.Warn("close readonly Kubernetes MCP client", "error", err)
		}
	}()
	observabilityClient, err := connectMCP(connectCtx, cfg.ObservabilityMCPURL, mcpHTTPClient, []string{mcpadapter.ToolGetHTTPErrorRate})
	if err != nil {
		return fmt.Errorf("connect observability MCP: %w", err)
	}
	defer func() {
		if err := observabilityClient.Close(); err != nil {
			logger.Warn("close observability MCP client", "error", err)
		}
	}()
	boundedRead, err := observedCalls(readClient, cfg.MCPCallTimeout, mcpadapter.ServerKubernetesReadonly, tel)
	if err != nil {
		return err
	}
	boundedObservability, err := observedCalls(observabilityClient, cfg.MCPCallTimeout, mcpadapter.ServerObservability, tel)
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
	ids := &runtime.IDGenerator{}
	// The LLM provider client is deliberately not traced: its requests carry
	// the API key and prompt, and nothing about them is exported.
	strategyDiagnoser, err := newDiagnoser(cfg, ids, &http.Client{Timeout: cfg.LLMTimeout})
	if err != nil {
		return err
	}
	diagnoser, err := tel.ObserveDiagnoser(strategyDiagnoser, cfg.DiagnosisStrategy)
	if err != nil {
		return err
	}
	runner, err := application.NewInvestigationRunner(inspector, metrics, diagnoser, runtime.Clock{}, ids)
	if err != nil {
		return err
	}
	idempotentRunner, err := a2aadapter.NewIdempotentInvestigator(pool, runner)
	if err != nil {
		return err
	}
	store, err := a2aadapter.NewPostgresTaskStore(pool)
	if err != nil {
		return err
	}
	handler, err := a2aadapter.NewServerHandler(idempotentRunner, store, logger, a2aadapter.ServerConfig{PublicURL: cfg.PublicURL, TaskTimeout: cfg.TaskTimeout, Tracer: tel, Metrics: tel.MetricsHandler(), Ready: func(ctx context.Context) error {
		if err := pool.Ping(ctx); err != nil {
			return err
		}
		if err := readClient.Ping(ctx); err != nil {
			return err
		}
		return observabilityClient.Ping(ctx)
	}})
	if err != nil {
		return err
	}
	server := &http.Server{Addr: cfg.HTTPAddr, Handler: tel.InstrumentServer(handler, a2aadapter.RouteOf), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() {
		logger.Info("investigator listening", "addr", cfg.HTTPAddr, "public_url", cfg.PublicURL)
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
	case domain.StrategyLLM:
		return application.NewLLMDiagnoser(reasoner, rules, ids, modelConfig)
	case domain.StrategyHybrid:
		return application.NewHybridDiagnoser(reasoner, rules, ids, modelConfig)
	default:
		return nil, fmt.Errorf("unsupported diagnosis strategy %q", cfg.DiagnosisStrategy)
	}
}

// observedCalls bounds each call and reports it (including timeouts) to
// telemetry under the server's label.
func observedCalls(client *mcpadapter.Client, timeout time.Duration, server string, observer mcpadapter.CallObserver) (mcpadapter.Caller, error) {
	bounded, err := mcpadapter.BoundCalls(client, timeout)
	if err != nil {
		return nil, err
	}
	return mcpadapter.ObserveCalls(bounded, server, observer)
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
