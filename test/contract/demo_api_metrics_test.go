package contract_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metricsprom "github.com/belLena81/rollback-guard/internal/adapters/prometheus"
	"github.com/belLena81/rollback-guard/internal/demoapi"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/config"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

type capturingQueryAPI struct {
	now     time.Time
	queries []string
}

func (c *capturingQueryAPI) Query(_ context.Context, query string, _ time.Time, _ ...v1.Option) (model.Value, v1.Warnings, error) {
	c.queries = append(c.queries, query)
	return model.Vector{&model.Sample{Value: model.SampleValue(c.now.Unix()), Timestamp: model.TimeFromUnix(c.now.Unix())}}, nil, nil
}

// TestDemoAPIMetricsMatchObservabilityQueries binds the demo workload to the
// Prometheus adapter: with default configuration on both sides, every series
// selector the adapter emits must match a series the demo-api exposes.
func TestDemoAPIMetricsMatchObservabilityQueries(t *testing.T) {
	t.Setenv("ALLOWED_TARGETS", "")
	observability, err := config.LoadObservabilityMCP()
	if err != nil {
		t.Fatal(err)
	}
	demoConfig, err := demoapi.LoadConfig(func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if len(observability.AllowedTargets) != 1 {
		t.Fatalf("default allowed targets = %d, want 1", len(observability.AllowedTargets))
	}
	target := observability.AllowedTargets[0]
	if got := (demoapi.TargetLabels{Cluster: target.Cluster(), Namespace: target.Namespace(), Deployment: target.Deployment(), Service: target.Service()}); got != demoConfig.Target {
		t.Fatalf("demo-api default labels %+v do not match default ALLOWED_TARGETS %+v", demoConfig.Target, got)
	}

	exposed := scrapeDemoAPI(t, demoConfig)

	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		kind   string
		series string
	}{
		{kind: "http_error_rate", series: demoapi.RequestsTotalMetric},
		{kind: "request_latency", series: demoapi.RequestDurationMetric + "_bucket"},
	}
	for _, test := range tests {
		t.Run(test.kind, func(t *testing.T) {
			api := &capturingQueryAPI{now: now}
			client, err := metricsprom.NewClient(api, metricsprom.Config{AllowedTargets: observability.AllowedTargets, ObservationWindow: time.Minute, MaxSampleAge: time.Minute, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.QueryMetric(t.Context(), target, metricsprom.QueryRequest{Kind: domain.SymptomKind(test.kind), Window: time.Minute, Percentile: 0.95}); err != nil {
				t.Fatal(err)
			}
			selector := test.series + `{cluster="local",namespace="rollback-demo",deployment="demo-api",service="demo-api"`
			for _, query := range api.queries {
				if !strings.Contains(query, selector) {
					t.Fatalf("adapter query %q does not select %s", query, selector)
				}
			}
			if !exposed[selector] {
				t.Fatalf("demo-api does not expose a series matching %s", selector)
			}
		})
	}
}

// scrapeDemoAPI returns the set of "name{target labels" prefixes exposed by a
// demo-api that has served at least one /work request.
func scrapeDemoAPI(t *testing.T, cfg demoapi.Config) map[string]bool {
	t.Helper()
	handler, err := demoapi.NewHandler(cfg, demoapi.Deps{
		Sample: func() float64 { return 0 },
		Wait:   func(context.Context, time.Duration) error { return nil },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/work", "/metrics"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, http.NoBody))
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, recorder.Code)
		}
		if path != "/metrics" {
			continue
		}
		parser := expfmt.NewTextParser(model.UTF8Validation)
		families, err := parser.TextToMetricFamilies(strings.NewReader(recorder.Body.String()))
		if err != nil {
			t.Fatal(err)
		}
		exposed := map[string]bool{}
		for name, family := range families {
			series := name
			if family.GetType().String() == "HISTOGRAM" {
				series = name + "_bucket"
			}
			for _, metric := range family.GetMetric() {
				labels := map[string]string{}
				for _, pair := range metric.GetLabel() {
					labels[pair.GetName()] = pair.GetValue()
				}
				key := series + `{cluster="` + labels["cluster"] + `",namespace="` + labels["namespace"] + `",deployment="` + labels["deployment"] + `",service="` + labels["service"] + `"`
				exposed[key] = true
			}
		}
		return exposed
	}
	t.Fatal("metrics not scraped")
	return nil
}
