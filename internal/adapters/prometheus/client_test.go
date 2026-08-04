package prometheus

import (
	"context"
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

func (f *fakeAPI) Query(_ context.Context, query string, _ time.Time, _ ...v1.Option) (model.Value, v1.Warnings, error) {
	f.query = query
	return f.value, nil, f.err
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
