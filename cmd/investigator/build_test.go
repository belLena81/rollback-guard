package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mcpadapter "github.com/belLena81/rollback-guard/internal/adapters/mcp"
	"github.com/belLena81/rollback-guard/internal/adapters/mcp/mcptest"
	"github.com/belLena81/rollback-guard/internal/platform/buildinfo"
	"github.com/belLena81/rollback-guard/internal/platform/config"
	"github.com/belLena81/rollback-guard/internal/platform/httpserver"
	"github.com/belLena81/rollback-guard/internal/platform/telemetry"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testTelemetry(t *testing.T) *telemetry.Provider {
	t.Helper()
	tel, err := telemetry.New(telemetry.Settings{ServiceName: "investigator", SampleRatio: 1})
	if err != nil {
		t.Fatal(err)
	}
	return tel
}

// The investigator used to introduce itself to the MCP servers as the
// orchestrator.
func TestBuildMCPConnectsAsTheInvestigator(t *testing.T) {
	read := mcptest.NewServer(t, mcpadapter.KubernetesReadonlyCatalog()...)
	observability := mcptest.NewServer(t, mcpadapter.ObservabilityCatalog()...)
	cfg := config.InvestigatorConfig{KubernetesReadMCPURL: read.Endpoint, ObservabilityMCPURL: observability.Endpoint, MCPConnectTimeout: 5 * time.Second, MCPCallTimeout: 5 * time.Second}
	logger := slog.New(slog.DiscardHandler)

	clients, err := buildMCP(context.Background(), cfg, testTelemetry(t), logger)
	if err != nil {
		t.Fatal(err)
	}
	defer clients.close(logger)

	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := clients.ping(pingCtx); err != nil {
		t.Fatalf("ping after buildMCP returned: %v", err)
	}
	for name, server := range map[string]*mcptest.Server{"readonly": read, "observability": observability} {
		seen := server.Clients()
		if len(seen) == 0 {
			t.Fatalf("%s server saw no client", name)
		}
		for _, info := range seen {
			if info.Name != "rollback-guardian-investigator" || info.Version != buildinfo.Version {
				t.Fatalf("%s server saw client %+v, want the investigator at %s", name, info, buildinfo.Version)
			}
		}
	}
}

func TestBuildMCPClosesTheFirstSessionWhenTheSecondFails(t *testing.T) {
	read := mcptest.NewServer(t, mcpadapter.KubernetesReadonlyCatalog()...)
	observability := mcptest.NewServer(t, append(mcpadapter.ObservabilityCatalog(), "generic_shell")...)
	cfg := config.InvestigatorConfig{KubernetesReadMCPURL: read.Endpoint, ObservabilityMCPURL: observability.Endpoint, MCPConnectTimeout: 5 * time.Second, MCPCallTimeout: 5 * time.Second}
	logger := slog.New(slog.DiscardHandler)

	clients, err := buildMCP(context.Background(), cfg, testTelemetry(t), logger)

	if err == nil {
		clients.close(logger)
		t.Fatal("buildMCP() succeeded, want a catalog error")
	}
	if !strings.Contains(err.Error(), "connect observability MCP") {
		t.Fatalf("buildMCP() error = %v", err)
	}
	if clients != (mcpClients{}) {
		t.Fatalf("buildMCP() returned open clients %+v with an error", clients)
	}
}

type noInvestigator struct{}

func (noInvestigator) Investigate(context.Context, ports.InvestigationRequest) (ports.InvestigationResult, error) {
	return ports.InvestigationResult{}, errors.New("not used")
}

func TestBuildHTTPServesHealthBoundedByDBPingTimeout(t *testing.T) {
	// The pool connects lazily, so no database is needed.
	pool, err := pgxpool.New(context.Background(), "postgres://rollback@127.0.0.1:1/rollback_guard")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	cfg := config.InvestigatorConfig{PublicURL: "http://127.0.0.1:8094", TaskTimeout: 45 * time.Second, DBPingTimeout: 750 * time.Millisecond}
	tests := []struct {
		name       string
		readyErr   error
		wantStatus int
		wantBody   string
	}{
		{name: "dependencies respond", wantStatus: http.StatusOK, wantBody: httpserver.HealthOKBody},
		{name: "a dependency is down", readyErr: errors.New("ping failed"), wantStatus: http.StatusServiceUnavailable, wantBody: httpserver.HealthNotReadyBody},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var remaining time.Duration
			ready := func(ctx context.Context) error {
				if deadline, ok := ctx.Deadline(); ok {
					remaining = time.Until(deadline)
				}
				return test.readyErr
			}
			handler, err := buildHTTP(cfg, noInvestigator{}, pool, ready, testTelemetry(t), slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			get := func(path string) *httptest.ResponseRecorder {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
				return response
			}

			if live := get(httpserver.LivePath); live.Code != http.StatusOK || live.Body.String() != httpserver.HealthOKBody {
				t.Fatalf("live = %d %q", live.Code, live.Body.String())
			}
			if response := get(httpserver.ReadyPath); response.Code != test.wantStatus || response.Body.String() != test.wantBody {
				t.Fatalf("ready = %d %q, want %d %q", response.Code, response.Body.String(), test.wantStatus, test.wantBody)
			}
			// DB_PING_TIMEOUT used to be parsed and ignored here.
			if remaining <= cfg.DBPingTimeout-250*time.Millisecond || remaining > cfg.DBPingTimeout {
				t.Fatalf("readiness deadline in %s, want DB_PING_TIMEOUT (%s)", remaining, cfg.DBPingTimeout)
			}
			if card := get("/.well-known/agent-card.json"); card.Code != http.StatusOK || !strings.Contains(card.Body.String(), "http://127.0.0.1:8094/a2a") {
				t.Fatalf("agent card = %d %s", card.Code, card.Body.String())
			}
			if metrics := get("/metrics"); metrics.Code != http.StatusOK || !strings.Contains(metrics.Body.String(), `route="/health/ready"`) {
				t.Fatalf("metrics = %d, want the instrumented readiness request", metrics.Code)
			}
		})
	}
}
