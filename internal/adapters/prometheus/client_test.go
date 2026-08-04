package prometheus

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

type fakeAPI struct {
	value model.Value
	err   error
	query string
}

func (f *fakeAPI) Query(_ context.Context, query string, at time.Time, _ ...v1.Option) (model.Value, v1.Warnings, error) {
	f.query = query
	if strings.HasPrefix(query, "min(timestamp(") && f.err == nil {
		return model.Vector{&model.Sample{Value: model.SampleValue(at.Unix()), Timestamp: model.TimeFromUnix(at.Unix())}}, nil, nil
	}
	return f.value, nil, f.err
}

type queuedAPI struct {
	responses []queryResponse
	queries   []string
	times     []time.Time
}

type queryResponse struct {
	value    model.Value
	warnings v1.Warnings
	err      error
}

func (f *queuedAPI) Query(_ context.Context, query string, at time.Time, _ ...v1.Option) (model.Value, v1.Warnings, error) {
	f.queries = append(f.queries, query)
	f.times = append(f.times, at)
	if len(f.responses) == 0 {
		return nil, nil, errors.New("unexpected prometheus query")
	}
	response := f.responses[0]
	f.responses = f.responses[1:]
	return response.value, response.warnings, response.err
}

func TestQueryMetricRejectsStaleSourceBehindFreshAggregate(t *testing.T) {
	now := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	target := testTarget(t)
	api := &queuedAPI{responses: []queryResponse{
		{value: model.Vector{&model.Sample{Value: model.SampleValue(now.Add(-2 * time.Minute).Unix()), Timestamp: model.TimeFromUnix(now.Unix())}}},
	}}
	client, err := NewClient(api, Config{AllowedTargets: []domain.DeploymentTarget{target}, MaxSampleAge: time.Minute, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.QueryMetric(context.Background(), target, QueryRequest{Kind: "http_error_rate", Window: 5 * time.Minute})
	if !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("QueryMetric() error = %v", err)
	}
	if len(api.queries) != 1 || !strings.HasPrefix(api.queries[0], "min(timestamp(rollback_guardian_http_requests_total{") {
		t.Fatalf("queries = %#v", api.queries)
	}
}

func TestQueryMetricUsesSourceTimestampRelativeToRequestedWindow(t *testing.T) {
	now := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	target := testTarget(t)
	for _, test := range []struct {
		name   string
		offset time.Duration
	}{
		{name: "current"},
		{name: "historical", offset: time.Hour},
	} {
		t.Run(test.name, func(t *testing.T) {
			effectiveEnd := now.Add(-test.offset)
			sourceAt := effectiveEnd.Add(-30 * time.Second)
			api := &queuedAPI{responses: []queryResponse{
				{value: model.Vector{&model.Sample{Value: model.SampleValue(sourceAt.Unix()), Timestamp: model.TimeFromUnix(effectiveEnd.Unix())}}},
				{value: model.Vector{&model.Sample{Value: 0.25, Timestamp: model.TimeFromUnix(now.Unix())}}},
			}}
			client, err := NewClient(api, Config{AllowedTargets: []domain.DeploymentTarget{target}, MaxSampleAge: time.Minute, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}

			sample, err := client.QueryMetric(context.Background(), target, QueryRequest{Kind: "http_error_rate", Window: 5 * time.Minute, Offset: test.offset})
			if err != nil {
				t.Fatal(err)
			}
			if !sample.ObservedAt.Equal(sourceAt) {
				t.Fatalf("ObservedAt = %s, want %s", sample.ObservedAt, sourceAt)
			}
			if len(api.times) != 2 || !api.times[0].Equal(effectiveEnd) || !api.times[1].Equal(now) {
				t.Fatalf("query times = %v", api.times)
			}
			if strings.Contains(api.queries[0], "offset ") {
				t.Fatalf("freshness query contains offset: %q", api.queries[0])
			}
			if test.offset > 0 && !strings.Contains(api.queries[1], "offset 1h") {
				t.Fatalf("value query = %q", api.queries[1])
			}
		})
	}
}

func TestSourceFreshnessQueriesUseFixedRawMetrics(t *testing.T) {
	target := testTarget(t)
	client, err := NewClient(&fakeAPI{}, Config{AllowedTargets: []domain.DeploymentTarget{target}, MaxSampleAge: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	wantMatchers := `{cluster="local",namespace="demo",deployment="demo-api",service="demo-api"}`
	for _, test := range []struct {
		kind   string
		metric string
	}{
		{kind: "http_error_rate", metric: "rollback_guardian_http_requests_total"},
		{kind: "request_latency", metric: "rollback_guardian_http_request_duration_seconds_bucket"},
		{kind: "pod_cpu_usage", metric: "rollback_guardian_pod_cpu_usage_cores"},
		{kind: "pod_memory_usage", metric: "rollback_guardian_pod_memory_usage_bytes"},
	} {
		t.Run(test.kind, func(t *testing.T) {
			query, err := client.sourceFreshnessQuery(target, test.kind)
			if err != nil {
				t.Fatal(err)
			}
			want := "min(timestamp(" + test.metric + wantMatchers + "))"
			if query != want {
				t.Fatalf("sourceFreshnessQuery() = %q, want %q", query, want)
			}
		})
	}
}

func TestQueryMetricRejectsInvalidSourceFreshness(t *testing.T) {
	now := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	target := testTarget(t)
	for _, test := range []struct {
		name     string
		response queryResponse
	}{
		{name: "empty", response: queryResponse{value: model.Vector{}}},
		{name: "multiple", response: queryResponse{value: model.Vector{&model.Sample{Value: model.SampleValue(now.Unix()), Timestamp: model.TimeFromUnix(now.Unix())}, &model.Sample{Value: model.SampleValue(now.Unix()), Timestamp: model.TimeFromUnix(now.Unix())}}}},
		{name: "nan", response: queryResponse{value: model.Vector{&model.Sample{Value: model.SampleValue(math.NaN()), Timestamp: model.TimeFromUnix(now.Unix())}}}},
		{name: "non-positive", response: queryResponse{value: model.Vector{&model.Sample{Value: 0, Timestamp: model.TimeFromUnix(now.Unix())}}}},
		{name: "future", response: queryResponse{value: model.Vector{&model.Sample{Value: model.SampleValue(now.Add(2 * time.Second).Unix()), Timestamp: model.TimeFromUnix(now.Unix())}}}},
		{name: "warning", response: queryResponse{value: model.Vector{&model.Sample{Value: model.SampleValue(now.Unix()), Timestamp: model.TimeFromUnix(now.Unix())}}, warnings: v1.Warnings{"partial result"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := &queuedAPI{responses: []queryResponse{test.response}}
			client, err := NewClient(api, Config{AllowedTargets: []domain.DeploymentTarget{target}, MaxSampleAge: time.Minute, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.QueryMetric(context.Background(), target, QueryRequest{Kind: "http_error_rate", Window: 5 * time.Minute})
			if !errors.Is(err, ErrInvalidResult) {
				t.Fatalf("QueryMetric() error = %v", err)
			}
			if len(api.queries) != 1 || !strings.HasPrefix(api.queries[0], "min(timestamp(") {
				t.Fatalf("queries after invalid freshness = %#v", api.queries)
			}
		})
	}
}

func TestObserveBuildsFixedEscapedQuery(t *testing.T) {
	now := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	target := testTarget(t)
	symptom := testSymptom(t, now)
	api := &fakeAPI{value: model.Vector{&model.Sample{Value: 0.25, Timestamp: model.TimeFromUnixNano(now.UnixNano())}}}
	client, err := NewClient(api, Config{AllowedTargets: []domain.DeploymentTarget{target}, MaxSampleAge: time.Minute, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	evidence, err := client.Observe(context.Background(), target, symptom)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Value != 0.25 || evidence.Threshold != symptom.Threshold() || evidence.Unit != "ratio" {
		t.Fatalf("evidence = %#v", evidence)
	}
	if !strings.Contains(api.query, "rollback_guardian_http_requests_total") || strings.Contains(api.query, "query=") {
		t.Fatalf("query = %q", api.query)
	}
}

func TestHTTPErrorRateQueryPreservesLowTrafficAndMissingDataSemantics(t *testing.T) {
	now := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	target := testTarget(t)
	client, err := NewClient(&fakeAPI{}, Config{AllowedTargets: []domain.DeploymentTarget{target}, MaxSampleAge: time.Minute, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name   string
		offset time.Duration
	}{
		{name: "current window"},
		{name: "offset window", offset: time.Hour},
	} {
		t.Run(test.name, func(t *testing.T) {
			query, unit, err := client.query(target, QueryRequest{Kind: "http_error_rate", Window: 5 * time.Minute, Offset: test.offset})
			if err != nil {
				t.Fatal(err)
			}
			if unit != "ratio" {
				t.Fatalf("unit = %q", unit)
			}
			if strings.Contains(query, "clamp_min") || strings.Contains(query, "vector(0)") {
				t.Fatalf("unsafe error-rate query = %q", query)
			}
			if !strings.Contains(query, "> 0") || !strings.Contains(query, " or ") || !strings.Contains(query, " * 0") {
				t.Fatalf("query does not preserve zero/missing semantics: %q", query)
			}
			if test.offset > 0 && strings.Count(query, "offset ") != 3 {
				t.Fatalf("offset query = %q", query)
			}
		})
	}
}

func TestObserveRejectsInvalidPrometheusResults(t *testing.T) {
	now := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	target := testTarget(t)
	symptom := testSymptom(t, now)
	tests := []struct {
		name  string
		value model.Value
	}{
		{name: "empty", value: model.Vector{}},
		{name: "multiple", value: model.Vector{&model.Sample{Value: 1, Timestamp: model.TimeFromUnix(now.Unix())}, &model.Sample{Value: 2, Timestamp: model.TimeFromUnix(now.Unix())}}},
		{name: "stale", value: model.Vector{&model.Sample{Value: 1, Timestamp: model.TimeFromUnix(now.Add(-2 * time.Minute).Unix())}}},
		{name: "nan", value: model.Vector{&model.Sample{Value: model.SampleValue(math.NaN()), Timestamp: model.TimeFromUnix(now.Unix())}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := &fakeAPI{value: test.value}
			client, err := NewClient(api, Config{AllowedTargets: []domain.DeploymentTarget{target}, MaxSampleAge: time.Minute, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Observe(context.Background(), target, symptom); err == nil {
				t.Fatal("Observe() error = nil")
			}
		})
	}
}

func testTarget(t *testing.T) domain.DeploymentTarget {
	t.Helper()
	target, err := domain.NewDeploymentTarget("local", "demo", "demo-api", "demo-api")
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func testSymptom(t *testing.T, now time.Time) domain.Symptom {
	t.Helper()
	symptom, err := domain.NewSymptom("http_error_rate", 0.3, 0.1, "ratio", now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return symptom
}
