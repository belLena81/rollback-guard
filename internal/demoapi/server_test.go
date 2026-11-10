package demoapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

type recordingWait struct {
	calls []time.Duration
	err   error
}

func (w *recordingWait) wait(_ context.Context, d time.Duration) error {
	w.calls = append(w.calls, d)
	return w.err
}

func fixedSample(value float64) func() float64 {
	return func() float64 { return value }
}

func testConfig(mode Mode) Config {
	cfg := Config{
		HTTPAddr:    ":0",
		Mode:        mode,
		Version:     "test",
		Target:      TargetLabels{Cluster: "local", Namespace: "rollback-demo", Deployment: "demo-api", Service: "demo-api"},
		WorkLatency: 10 * time.Millisecond,
	}
	switch mode {
	case ModeErrorStorm:
		cfg.ErrorRate = 0.8
	case ModeHighLatency:
		cfg.WorkLatency = 1500 * time.Millisecond
	case ModeCrashLoop:
		cfg.CrashAfter = 2 * time.Second
	case ModeHealthy:
	}
	return cfg
}

func newTestHandler(t *testing.T, cfg Config, sample float64, wait *recordingWait) http.Handler {
	t.Helper()
	handler, err := NewHandler(cfg, Deps{Sample: fixedSample(sample), Wait: wait.wait, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func do(t *testing.T, handler http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), method, path, http.NoBody))
	return recorder
}

func TestHealthz(t *testing.T) {
	tests := []struct {
		mode Mode
		want int
	}{
		{mode: ModeHealthy, want: http.StatusOK},
		{mode: ModeErrorStorm, want: http.StatusOK},
		{mode: ModeHighLatency, want: http.StatusOK},
		{mode: ModeCrashLoop, want: http.StatusServiceUnavailable},
	}
	for _, test := range tests {
		t.Run(string(test.mode), func(t *testing.T) {
			handler := newTestHandler(t, testConfig(test.mode), 0, &recordingWait{})
			if got := do(t, handler, http.MethodGet, "/healthz").Code; got != test.want {
				t.Fatalf("status = %d, want %d", got, test.want)
			}
		})
	}
}

func TestWork(t *testing.T) {
	tests := []struct {
		name     string
		mode     Mode
		sample   float64
		waitErr  error
		want     int
		wantWait time.Duration
	}{
		{name: "healthy succeeds", mode: ModeHealthy, sample: 0, want: http.StatusOK, wantWait: 10 * time.Millisecond},
		{name: "error storm fails below rate", mode: ModeErrorStorm, sample: 0.79, want: http.StatusInternalServerError, wantWait: 10 * time.Millisecond},
		{name: "error storm succeeds at or above rate", mode: ModeErrorStorm, sample: 0.8, want: http.StatusOK, wantWait: 10 * time.Millisecond},
		{name: "high latency waits then succeeds", mode: ModeHighLatency, sample: 0, want: http.StatusOK, wantWait: 1500 * time.Millisecond},
		{name: "crashloop serves work until it exits", mode: ModeCrashLoop, sample: 0, want: http.StatusOK, wantWait: 10 * time.Millisecond},
		{name: "cancelled wait is unavailable", mode: ModeHighLatency, sample: 0, waitErr: context.Canceled, want: http.StatusServiceUnavailable, wantWait: 1500 * time.Millisecond},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			wait := &recordingWait{err: test.waitErr}
			handler := newTestHandler(t, testConfig(test.mode), test.sample, wait)
			if got := do(t, handler, http.MethodGet, "/work").Code; got != test.want {
				t.Fatalf("status = %d, want %d", got, test.want)
			}
			if len(wait.calls) != 1 || wait.calls[0] != test.wantWait {
				t.Fatalf("wait calls = %v, want [%s]", wait.calls, test.wantWait)
			}
		})
	}
}

func TestRejectsNonGetMethods(t *testing.T) {
	handler := newTestHandler(t, testConfig(ModeHealthy), 0, &recordingWait{})
	for _, path := range []string{"/work", "/healthz", "/metrics"} {
		if got := do(t, handler, http.MethodPost, path).Code; got != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s status = %d, want 405", path, got)
		}
	}
}

// The adapter in internal/adapters/prometheus queries these exact names and
// selects series by cluster, namespace, deployment and service, plus code
// (counter) and le (histogram buckets).
func TestMetricsExposeAdapterContract(t *testing.T) {
	handler := newTestHandler(t, testConfig(ModeErrorStorm), 0.1, &recordingWait{})
	for range 3 {
		do(t, handler, http.MethodGet, "/work")
	}
	do(t, handler, http.MethodGet, "/healthz")

	families := scrape(t, handler)
	targetLabels := map[string]string{"cluster": "local", "namespace": "rollback-demo", "deployment": "demo-api", "service": "demo-api"}

	counter := families["rollback_guardian_http_requests_total"]
	if counter == nil || counter.GetType() != dto.MetricType_COUNTER {
		t.Fatalf("missing counter rollback_guardian_http_requests_total, got %v", keys(families))
	}
	if len(counter.GetMetric()) != 1 {
		t.Fatalf("counter series = %d, want 1 (only /work is counted)", len(counter.GetMetric()))
	}
	series := counter.GetMetric()[0]
	assertLabels(t, series.GetLabel(), withLabel(targetLabels, "code", "500"))
	if got := series.GetCounter().GetValue(); got != 3 {
		t.Fatalf("counter = %v, want 3", got)
	}

	histogram := families["rollback_guardian_http_request_duration_seconds"]
	if histogram == nil || histogram.GetType() != dto.MetricType_HISTOGRAM {
		t.Fatalf("missing histogram rollback_guardian_http_request_duration_seconds, got %v", keys(families))
	}
	if len(histogram.GetMetric()) != 1 {
		t.Fatalf("histogram series = %d, want 1", len(histogram.GetMetric()))
	}
	assertLabels(t, histogram.GetMetric()[0].GetLabel(), withLabel(targetLabels, "code", "500"))
	if got := histogram.GetMetric()[0].GetHistogram().GetSampleCount(); got != 3 {
		t.Fatalf("histogram count = %d, want 3", got)
	}
	if len(histogram.GetMetric()[0].GetHistogram().GetBucket()) == 0 {
		t.Fatal("histogram has no le buckets")
	}
}

func TestNewHandlerValidatesDependencies(t *testing.T) {
	valid := Deps{Sample: fixedSample(0), Wait: (&recordingWait{}).wait, Logger: discardLogger()}
	tests := []struct {
		name string
		cfg  Config
		deps Deps
	}{
		{name: "nil sample", cfg: testConfig(ModeHealthy), deps: Deps{Wait: valid.Wait, Logger: valid.Logger}},
		{name: "nil wait", cfg: testConfig(ModeHealthy), deps: Deps{Sample: valid.Sample, Logger: valid.Logger}},
		{name: "nil logger", cfg: testConfig(ModeHealthy), deps: Deps{Sample: valid.Sample, Wait: valid.Wait}},
		{name: "invalid mode", cfg: Config{Mode: "flaky", Target: testConfig(ModeHealthy).Target}, deps: valid},
		{name: "empty target label", cfg: Config{Mode: ModeHealthy}, deps: valid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewHandler(test.cfg, test.deps); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestSleepContextHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := SleepContext(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if err := SleepContext(t.Context(), time.Millisecond); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func scrape(t *testing.T, handler http.Handler) map[string]*dto.MetricFamily {
	t.Helper()
	response := do(t, handler, http.MethodGet, "/metrics")
	if response.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", response.Code)
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(response.Body.String()))
	if err != nil {
		t.Fatal(err)
	}
	return families
}

func assertLabels(t *testing.T, got []*dto.LabelPair, want map[string]string) {
	t.Helper()
	actual := make(map[string]string, len(got))
	for _, pair := range got {
		actual[pair.GetName()] = pair.GetValue()
	}
	if len(actual) != len(want) {
		t.Fatalf("labels = %v, want %v", actual, want)
	}
	for name, value := range want {
		if actual[name] != value {
			t.Fatalf("labels = %v, want %v", actual, want)
		}
	}
}

func withLabel(base map[string]string, name, value string) map[string]string {
	out := make(map[string]string, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	out[name] = value
	return out
}

func keys(families map[string]*dto.MetricFamily) []string {
	out := make([]string, 0, len(families))
	for name := range families {
		out = append(out, name)
	}
	return out
}
