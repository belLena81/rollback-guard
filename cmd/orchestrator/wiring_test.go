package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/mcp/mcptest"
	"github.com/belLena81/rollback-guard/internal/adapters/memory"
	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/config"
	"github.com/belLena81/rollback-guard/internal/platform/httpserver"
)

func wiringConfig(t *testing.T, read, write, observability *mcptest.Server) config.Config {
	t.Helper()
	target, err := domain.NewDeploymentTarget("local", "rollback-demo", "demo-api", "demo-api")
	if err != nil {
		t.Fatal(err)
	}
	cfg := mcpConfig(read, write, observability)
	cfg.DBPingTimeout = 750 * time.Millisecond
	cfg.ProposalTTL, cfg.ApprovalTTL, cfg.CleanupTimeout = time.Minute, time.Minute, time.Second
	cfg.SystemActorID, cfg.LocalActorID = "rollback-guardian", "local-admin"
	cfg.RuleMinimumConfidence, cfg.RuleCorrelationWindow = 0.8, 10*time.Minute
	cfg.AllowedTargets = []domain.DeploymentTarget{target}
	cfg.MetricsWindow = 5 * time.Minute
	cfg.Session = config.SessionConfig{TTL: time.Hour, CookieSecure: true}
	cfg.Auth = config.AuthConfig{Mode: config.AuthModeLocal}
	return cfg
}

// One HTTP request to the readonly server may carry a whole rollout watch,
// so the MCP HTTP client must allow MCP_WATCH_TIMEOUT on top of the per-call
// timeout instead of cutting the watch off on the client side.
func TestBuildMCPClientOutlivesARolloutWatch(t *testing.T) {
	const watch = 600 * time.Millisecond
	read := mcptest.NewSlowServer(t, watch, readonlyTools...)
	cfg := mcpConfig(read, mcptest.NewServer(t, rollbackTools...), mcptest.NewServer(t, observabilityTools...))
	cfg.MCPCallTimeout = 200 * time.Millisecond
	cfg.MCPWatchTimeout = 2 * time.Second
	logger := slog.New(slog.DiscardHandler)
	clients, err := buildMCP(context.Background(), cfg, testTelemetry(t), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer clients.close(logger)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := mcptest.Call(ctx, clients.read, "watch_rollout"); err != nil {
		t.Fatalf("a %s watch was cut off with MCP_CALL_TIMEOUT=%s, MCP_WATCH_TIMEOUT=%s: %v", watch, cfg.MCPCallTimeout, cfg.MCPWatchTimeout, err)
	}
}

func TestBuildServicesAndHTTPWireTheAPI(t *testing.T) {
	read := mcptest.NewServer(t, readonlyTools...)
	write := mcptest.NewServer(t, rollbackTools...)
	observability := mcptest.NewServer(t, observabilityTools...)
	cfg := wiringConfig(t, read, write, observability)
	logger := slog.New(slog.DiscardHandler)
	tel := testTelemetry(t)
	clients, err := buildMCP(context.Background(), cfg, tel, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer clients.close(logger)
	services, err := buildServices(cfg, memory.NewUnitOfWork(), clients, tel)
	if err != nil {
		t.Fatal(err)
	}
	var remaining time.Duration
	ready := func(ctx context.Context) error {
		if deadline, ok := ctx.Deadline(); ok {
			remaining = time.Until(deadline)
		}
		return clients.ping(ctx)
	}
	handler, err := buildHTTP(cfg, services, memory.NewAuthUnitOfWork(), ready, tel, logger)
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path, body string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(method, path, strings.NewReader(body)))
		return response
	}

	if live := do(http.MethodGet, httpserver.LivePath, ""); live.Code != http.StatusOK || live.Body.String() != httpserver.HealthOKBody {
		t.Fatalf("live = %d %q", live.Code, live.Body.String())
	}
	// Readiness pings the three real MCP sessions, bounded by DB_PING_TIMEOUT.
	if ready := do(http.MethodGet, httpserver.ReadyPath, ""); ready.Code != http.StatusOK || ready.Body.String() != httpserver.HealthOKBody {
		t.Fatalf("ready = %d %q", ready.Code, ready.Body.String())
	}
	if remaining <= cfg.DBPingTimeout-250*time.Millisecond || remaining > cfg.DBPingTimeout {
		t.Fatalf("readiness deadline in %s, want DB_PING_TIMEOUT (%s)", remaining, cfg.DBPingTimeout)
	}
	// The API is mounted behind the local actor: an unknown incident is a
	// 404 from the application, not a 401 or a mux 404 without a body.
	if missing := do(http.MethodGet, "/v1/incidents/does-not-exist", ""); missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), `"error"`) {
		t.Fatalf("GET unknown incident = %d %s", missing.Code, missing.Body.String())
	}
	if metrics := do(http.MethodGet, "/metrics", ""); metrics.Code != http.StatusOK || !strings.Contains(metrics.Body.String(), `route="/v1/incidents/{incidentId}"`) {
		t.Fatalf("metrics = %d, want the instrumented API route", metrics.Code)
	}
}

// The A2A connect has its own timeout; it must not wait for the (here much
// longer) MCP connect timeout or the task timeout of the A2A HTTP client.
func TestBuildCoordinatorBoundsTheA2AConnectByItsOwnTimeout(t *testing.T) {
	release := make(chan struct{})
	stalled := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(stalled.Close)
	t.Cleanup(func() { close(release) })
	cfg := config.Config{
		InvestigatorURL:   stalled.URL,
		Resilience:        config.DefaultResilience(),
		A2AConnectTimeout: 200 * time.Millisecond,
		MCPConnectTimeout: 30 * time.Second,
		A2ATaskTimeout:    30 * time.Second,
		A2ACancelTimeout:  30 * time.Second,
	}
	started := time.Now()

	coordinator, closeGateway, err := buildCoordinator(context.Background(), cfg, memory.NewUnitOfWork(), application.Services{}, testTelemetry(t), slog.New(slog.DiscardHandler))

	if err == nil {
		closeGateway()
		t.Fatalf("buildCoordinator() = %v, want a connect error", coordinator)
	}
	if !strings.Contains(err.Error(), "connect investigator A2A agent") {
		t.Fatalf("buildCoordinator() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("A2A connect took %s; it is not bounded by A2A_CONNECT_TIMEOUT (%s)", elapsed, cfg.A2AConnectTimeout)
	}
}
