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

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/config"
	"github.com/belLena81/rollback-guard/internal/platform/httpserver"
	"github.com/belLena81/rollback-guard/internal/platform/telemetry"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

// queryAPI is a promv1.API whose Query records the deadline it was given.
type queryAPI struct {
	promv1.API
	remaining time.Duration
}

func (a *queryAPI) Query(ctx context.Context, _ string, _ time.Time, _ ...promv1.Option) (model.Value, promv1.Warnings, error) {
	if deadline, ok := ctx.Deadline(); ok {
		a.remaining = time.Until(deadline)
	}
	return model.Vector{}, nil, nil
}

func testConfig(t *testing.T) config.ObservabilityMCPConfig {
	t.Helper()
	target, err := domain.NewDeploymentTarget("local", "rollback-demo", "demo-api", "demo-api")
	if err != nil {
		t.Fatal(err)
	}
	return config.ObservabilityMCPConfig{AllowedTargets: []domain.DeploymentTarget{target}, RequestTimeout: 750 * time.Millisecond, SessionTimeout: time.Minute, MetricsWindow: 5 * time.Minute, MaxSampleAge: time.Minute, Resilience: config.DefaultResilience()}
}

func TestTimeoutQueryAPIBoundsEachQuery(t *testing.T) {
	api := &queryAPI{}
	bounded := timeoutQueryAPI{api: api, timeout: 750 * time.Millisecond}

	if _, _, err := bounded.Query(context.Background(), "up", time.Now()); err != nil {
		t.Fatal(err)
	}

	if api.remaining <= 500*time.Millisecond || api.remaining > 750*time.Millisecond {
		t.Fatalf("query deadline in %s, want the configured 750ms", api.remaining)
	}
}

func TestBuildHTTPServesHealthAndMetrics(t *testing.T) {
	cfg := testConfig(t)
	backend, err := buildServices(cfg, &queryAPI{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		ready      httpserver.ReadyCheck
		wantStatus int
		wantBody   string
	}{
		{name: "prometheus reachable", ready: func(context.Context) error { return nil }, wantStatus: http.StatusOK, wantBody: httpserver.HealthOKBody},
		{name: "prometheus down", ready: func(context.Context) error { return errors.New("connection refused") }, wantStatus: http.StatusServiceUnavailable, wantBody: httpserver.HealthNotReadyBody},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tel, err := telemetry.New(t.Context(), telemetry.Settings{ServiceName: "observability-mcp", SampleRatio: 1})
			if err != nil {
				t.Fatal(err)
			}
			var remaining time.Duration
			ready := func(ctx context.Context) error {
				if deadline, ok := ctx.Deadline(); ok {
					remaining = time.Until(deadline)
				}
				return test.ready(ctx)
			}
			handler, err := buildHTTP(cfg, backend, ready, tel, slog.New(slog.DiscardHandler))
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
			// Readiness is bounded like any other Prometheus request.
			if remaining <= cfg.RequestTimeout-250*time.Millisecond || remaining > cfg.RequestTimeout {
				t.Fatalf("readiness deadline in %s, want PROMETHEUS_TIMEOUT (%s)", remaining, cfg.RequestTimeout)
			}
			if metrics := get("/metrics"); metrics.Code != http.StatusOK || !strings.Contains(metrics.Body.String(), `route="/health/ready"`) {
				t.Fatalf("metrics = %d, want the instrumented readiness request", metrics.Code)
			}
		})
	}
}
