package contract_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	a2aadapter "github.com/belLena81/rollback-guard/internal/adapters/a2a"
	"github.com/belLena81/rollback-guard/internal/adapters/httpapi"
	protocol "github.com/belLena81/rollback-guard/internal/adapters/mcp"
	"github.com/belLena81/rollback-guard/internal/adapters/mcp/kubernetesserver"
	"github.com/belLena81/rollback-guard/internal/adapters/mcp/observabilityserver"
	"github.com/belLena81/rollback-guard/internal/ports"
)

type noInvestigator struct{}

func (noInvestigator) Investigate(context.Context, ports.InvestigationRequest) (ports.InvestigationResult, error) {
	return ports.InvestigationResult{}, errors.New("not used")
}

// healthHandlers builds the HTTP handler of every service with the given
// readiness check, bounded by readyTimeout.
func healthHandlers(t *testing.T, ready func(context.Context) error, readyTimeout time.Duration) map[string]http.Handler {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	api, err := httpapi.NewServer(contractServices(t), logger)
	if err != nil {
		t.Fatal(err)
	}
	readonly, err := kubernetesserver.NewHTTPHandler(&fakeKubernetesBackend{}, kubernetesserver.ModeReadonly, ready, time.Minute, logger, protocol.WithReadyTimeout(readyTimeout))
	if err != nil {
		t.Fatal(err)
	}
	rollback, err := kubernetesserver.NewHTTPHandler(&fakeKubernetesBackend{}, kubernetesserver.ModeRollback, ready, time.Minute, logger, protocol.WithReadyTimeout(readyTimeout))
	if err != nil {
		t.Fatal(err)
	}
	observability, err := observabilityserver.NewHTTPHandler(fakeMetricsBackend{}, ready, time.Minute, logger, protocol.WithReadyTimeout(readyTimeout))
	if err != nil {
		t.Fatal(err)
	}
	investigator, err := a2aadapter.NewServerHandler(noInvestigator{}, nil, logger, a2aadapter.ServerConfig{PublicURL: "http://127.0.0.1:8094", TaskTimeout: time.Second, Ready: ready, ReadyTimeout: readyTimeout})
	if err != nil {
		t.Fatal(err)
	}
	return map[string]http.Handler{
		"orchestrator":            api.Routes(ready, readyTimeout, nil),
		"kubernetes MCP readonly": readonly,
		"kubernetes MCP rollback": rollback,
		"observability MCP":       observability,
		"investigator":            investigator,
	}
}

// TestEveryServiceSharesOneHealthContract pins the health responses of all
// five processes. The orchestrator's are part of the REST contract
// (api/openapi.yaml); the others must not drift from them.
func TestEveryServiceSharesOneHealthContract(t *testing.T) {
	const (
		okBody       = `{"status":"ok"}`
		notReadyBody = `{"status":"not_ready"}`
	)
	tests := []struct {
		name       string
		ready      func(context.Context) error
		path       string
		wantStatus int
		wantBody   string
	}{
		{name: "live", ready: func(context.Context) error { return errors.New("dependency down") }, path: "/health/live", wantStatus: http.StatusOK, wantBody: okBody},
		{name: "ready without dependencies", path: "/health/ready", wantStatus: http.StatusOK, wantBody: okBody},
		{name: "ready", ready: func(context.Context) error { return nil }, path: "/health/ready", wantStatus: http.StatusOK, wantBody: okBody},
		{name: "not ready", ready: func(context.Context) error { return errors.New("dial postgres://user:secret@db: refused") }, path: "/health/ready", wantStatus: http.StatusServiceUnavailable, wantBody: notReadyBody},
	}
	for _, test := range tests {
		for service, handler := range healthHandlers(t, test.ready, time.Second) {
			t.Run(test.name+"/"+service, func(t *testing.T) {
				response := httptest.NewRecorder()

				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))

				if response.Code != test.wantStatus {
					t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
				}
				if got := response.Body.String(); got != test.wantBody {
					t.Fatalf("body = %q, want %q", got, test.wantBody)
				}
				if got := response.Header().Get("Content-Type"); got != "application/json" {
					t.Fatalf("Content-Type = %q, want application/json", got)
				}
			})
		}
	}
}

// TestEveryServiceBoundsReadinessByItsConfiguredTimeout proves each handler
// passes its configured timeout to the shared check instead of a fixed one.
func TestEveryServiceBoundsReadinessByItsConfiguredTimeout(t *testing.T) {
	const configured = 750 * time.Millisecond
	var remaining time.Duration
	ready := func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			return errors.New("readiness check has no deadline")
		}
		remaining = time.Until(deadline)
		return nil
	}
	for service, handler := range healthHandlers(t, ready, configured) {
		t.Run(service, func(t *testing.T) {
			remaining = 0
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health/ready", nil))

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
			}
			if remaining <= configured-250*time.Millisecond || remaining > configured {
				t.Fatalf("readiness deadline in %s, want the configured %s", remaining, configured)
			}
		})
	}
}
