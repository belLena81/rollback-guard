package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/mcp/kubernetesserver"
	"github.com/belLena81/rollback-guard/internal/platform/config"
	"github.com/belLena81/rollback-guard/internal/platform/httpserver"
	"github.com/belLena81/rollback-guard/internal/platform/telemetry"
	"k8s.io/client-go/kubernetes/fake"
)

// Readiness is bounded by DB_PING_TIMEOUT in rollback mode; readonly mode
// loads no database settings and falls back to the shared default.
func TestBuildHTTPBoundsReadinessPerMode(t *testing.T) {
	tests := []struct {
		name          string
		mode          string
		dbPingTimeout time.Duration
		want          time.Duration
	}{
		{name: "rollback uses DB_PING_TIMEOUT", mode: config.KubernetesMCPModeRollback, dbPingTimeout: 750 * time.Millisecond, want: 750 * time.Millisecond},
		{name: "readonly uses the shared default", mode: config.KubernetesMCPModeReadonly, want: httpserver.DefaultReadyTimeout},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := testConfig(t, test.mode)
			cfg.DBPingTimeout = test.dbPingTimeout
			tel, err := telemetry.New(t.Context(), telemetry.Settings{ServiceName: "kubernetes-mcp", SampleRatio: 1})
			if err != nil {
				t.Fatal(err)
			}
			backend, err := buildServices(cfg, fake.NewClientset(), nil)
			if err != nil {
				t.Fatal(err)
			}
			var remaining time.Duration
			ready := func(ctx context.Context) error {
				if deadline, ok := ctx.Deadline(); ok {
					remaining = time.Until(deadline)
				}
				return nil
			}
			handler, err := buildHTTP(cfg, backend, ready, tel, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, httpserver.ReadyPath, nil))

			if response.Code != http.StatusOK {
				t.Fatalf("ready = %d %q", response.Code, response.Body.String())
			}
			if remaining <= test.want-250*time.Millisecond || remaining > test.want {
				t.Fatalf("readiness deadline in %s, want %s", remaining, test.want)
			}
		})
	}
}

// Configuration validates the mode as a string; the server package owns the
// type. The two spellings must not drift apart.
func TestConfigModesMatchTheServerModes(t *testing.T) {
	if config.KubernetesMCPModeReadonly != string(kubernetesserver.ModeReadonly) || config.KubernetesMCPModeRollback != string(kubernetesserver.ModeRollback) {
		t.Fatalf("config modes %q/%q differ from server modes %q/%q", config.KubernetesMCPModeReadonly, config.KubernetesMCPModeRollback, kubernetesserver.ModeReadonly, kubernetesserver.ModeRollback)
	}
}
