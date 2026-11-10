// Package bootstrap holds the process start-up steps the composition roots
// share: opening the PostgreSQL pool and connecting verified MCP sessions.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	mcpadapter "github.com/belLena81/rollback-guard/internal/adapters/mcp"
	"github.com/belLena81/rollback-guard/internal/platform/config"
	"github.com/belLena81/rollback-guard/internal/platform/telemetry"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool opens a PostgreSQL pool limited to maxConns connections. The pool
// connects lazily; readiness checks ping it. Errors never contain the
// database URL or any part of it.
func NewPool(ctx context.Context, databaseURL string, maxConns int32) (*pgxpool.Pool, error) {
	if strings.TrimSpace(databaseURL) == "" {
		return nil, errors.New("database URL is required")
	}
	if maxConns <= 0 {
		return nil, errors.New("database pool size must be positive")
	}
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		// The pgx parse error quotes the connection string, so it is
		// deliberately not wrapped.
		return nil, errors.New("parse database configuration: DATABASE_URL is not a valid PostgreSQL connection string")
	}
	poolConfig.MaxConns = maxConns
	// The pool outlives start-up: it keeps the context values but not its
	// cancellation, so requests in flight during graceful shutdown still
	// reach the database.
	pool, err := pgxpool.NewWithConfig(context.WithoutCancel(ctx), poolConfig)
	if err != nil {
		return nil, errors.New("create database pool: invalid pool configuration")
	}
	return pool, nil
}

// MCPServer describes one MCP server a process depends on.
type MCPServer struct {
	// Name labels errors and logs, for example "readonly kubernetes".
	Name string
	// Endpoint is the server's Streamable HTTP URL.
	Endpoint string
	// Tools is the exact tool catalog the server must serve.
	Tools []string
}

// ConnectMCP opens a session to server as clientName and fails closed unless
// the server's tool catalog is exactly server.Tools. On a catalog mismatch
// the session is closed before returning.
func ConnectMCP(ctx context.Context, clientName string, httpClient *http.Client, server MCPServer) (*mcpadapter.Client, error) {
	if strings.TrimSpace(server.Name) == "" || len(server.Tools) == 0 {
		return nil, errors.New("MCP server name and expected tool catalog are required")
	}
	client, err := mcpadapter.Connect(ctx, server.Endpoint, httpClient, clientName)
	if err != nil {
		return nil, fmt.Errorf("connect %s MCP: %w", server.Name, err)
	}
	if err := client.RequireTools(ctx, server.Tools); err != nil {
		return nil, fmt.Errorf("connect %s MCP: %w", server.Name, errors.Join(err, client.Close()))
	}
	return client, nil
}

// ObservedCalls bounds each call on client by timeout and reports it
// (including timeouts) to observer under the server telemetry label.
func ObservedCalls(client *mcpadapter.Client, timeout time.Duration, server string, observer mcpadapter.CallObserver) (mcpadapter.Caller, error) {
	if client == nil {
		return nil, errors.New("MCP client is required")
	}
	bounded, err := mcpadapter.BoundCalls(client, timeout)
	if err != nil {
		return nil, err
	}
	return mcpadapter.ObserveCalls(bounded, server, observer)
}

// CloseMCP closes client (a nil client is ignored) and logs a failure under
// the server's name.
func CloseMCP(client *mcpadapter.Client, name string, logger *slog.Logger) {
	if client == nil {
		return
	}
	if err := client.Close(); err != nil {
		logger.Warn("close MCP client", "server", name, "error", err)
	}
}

// StartTelemetry applies the configured log level to level and builds the
// process telemetry provider logging through logger. The returned stop
// function flushes and shuts the provider down; call it when the process
// exits.
func StartTelemetry(ctx context.Context, cfg config.TelemetryConfig, logger *slog.Logger, level *slog.LevelVar) (*telemetry.Provider, func(), error) {
	if logger == nil || level == nil {
		return nil, nil, errors.New("telemetry requires a logger and its level")
	}
	level.Set(cfg.LogLevel)
	provider, err := telemetry.New(ctx, telemetry.SettingsFrom(cfg), telemetry.WithLogger(logger))
	if err != nil {
		return nil, nil, err
	}
	stop := func() {
		// ctx is cancelled by the exit signal, which must not cut the flush short; the
		// provider applies its own configured shutdown timeout.
		if err := provider.Shutdown(context.WithoutCancel(ctx)); err != nil {
			logger.Warn("telemetry shutdown incomplete", "error", err)
		}
	}
	return provider, stop, nil
}
