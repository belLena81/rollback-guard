package prometheus

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

// Series the adapter queries. internal/demoapi exports the first two, so
// renaming one here renames what the demo service emits; the contract test in
// test/contract ties both sides to the generated queries.
const (
	SeriesRequestsTotal   = "rollback_guardian_http_requests_total"
	SeriesRequestDuration = "rollback_guardian_http_request_duration_seconds"
	SeriesPodCPUUsage     = "rollback_guardian_pod_cpu_usage_cores"
	SeriesPodMemoryUsage  = "rollback_guardian_pod_memory_usage_bytes"
)

var (
	ErrTargetForbidden = errors.New("metrics target is not allowed")
	ErrInvalidResult   = errors.New("invalid prometheus result")
	ErrUnsupportedKind = errors.New("unsupported metric kind")
)

type QueryAPI interface {
	Query(context.Context, string, time.Time, ...v1.Option) (model.Value, v1.Warnings, error)
}

type Config struct {
	AllowedTargets    []domain.DeploymentTarget
	ObservationWindow time.Duration
	MaxSampleAge      time.Duration
	Now               func() time.Time
}

type Client struct {
	api     QueryAPI
	window  time.Duration
	maxAge  time.Duration
	now     func() time.Time
	allowed map[domain.DeploymentTarget]struct{}
}

type QueryRequest struct {
	Kind       domain.SymptomKind
	Window     time.Duration
	Offset     time.Duration
	Percentile float64
}

type MetricSample struct {
	Name       string
	Value      float64
	Unit       string
	ObservedAt time.Time
}

func NewClient(api QueryAPI, config Config) (*Client, error) {
	if api == nil || len(config.AllowedTargets) == 0 || config.MaxSampleAge <= 0 {
		return nil, fmt.Errorf("invalid prometheus client configuration")
	}
	window := config.ObservationWindow
	if window <= 0 {
		window = 5 * time.Minute
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	allowed := make(map[domain.DeploymentTarget]struct{}, len(config.AllowedTargets))
	for _, target := range config.AllowedTargets {
		if !target.Valid() {
			return nil, fmt.Errorf("invalid allowed metrics target")
		}
		allowed[target] = struct{}{}
	}
	return &Client{api: api, window: window, maxAge: config.MaxSampleAge, now: now, allowed: allowed}, nil
}

func (c *Client) Observe(ctx context.Context, target domain.DeploymentTarget, symptom domain.Symptom) (domain.MetricEvidence, error) {
	sample, err := c.QueryMetric(ctx, target, QueryRequest{Kind: symptom.Kind(), Window: c.window, Percentile: 0.95})
	if err != nil {
		return domain.MetricEvidence{}, err
	}
	if symptom.Unit() != sample.Unit {
		return domain.MetricEvidence{}, &domain.ValidationError{Field: "symptom.unit", Reason: "does not match metric kind"}
	}
	return domain.MetricEvidence{Name: symptom.Kind().String(), Value: sample.Value, Threshold: symptom.Threshold(), Unit: sample.Unit, ObservedAt: sample.ObservedAt}, nil
}

func (c *Client) QueryMetric(ctx context.Context, target domain.DeploymentTarget, request QueryRequest) (MetricSample, error) {
	if _, ok := c.allowed[target]; !ok {
		return MetricSample{}, ErrTargetForbidden
	}
	if request.Window <= 0 || request.Window > domain.MaxMetricsWindow || request.Offset < 0 || request.Offset > 24*time.Hour {
		return MetricSample{}, &domain.ValidationError{Field: "metric_window", Reason: "out of range"}
	}
	query, unit, err := c.query(target, request)
	if err != nil {
		return MetricSample{}, err
	}
	observedAt := c.now().UTC()
	sourceQuery, err := c.sourceFreshnessQuery(target, request.Kind)
	if err != nil {
		return MetricSample{}, err
	}
	effectiveEnd := observedAt.Add(-request.Offset)
	sourceValue, sourceWarnings, err := c.api.Query(ctx, sourceQuery, effectiveEnd)
	if err != nil {
		return MetricSample{}, fmt.Errorf("query prometheus source freshness: %w", err)
	}
	if len(sourceWarnings) > 0 {
		return MetricSample{}, fmt.Errorf("prometheus returned source freshness warnings: %w", ErrInvalidResult)
	}
	sourceSample, err := c.singleSample(sourceValue, effectiveEnd)
	if err != nil {
		return MetricSample{}, err
	}
	sourceTimestamp := float64(sourceSample.Value)
	seconds, fraction := math.Modf(sourceTimestamp)
	sourceObservedAt := time.Unix(int64(seconds), int64(fraction*float64(time.Second))).UTC()
	if sourceTimestamp <= 0 || sourceObservedAt.After(effectiveEnd.Add(time.Second)) || effectiveEnd.Sub(sourceObservedAt) > c.maxAge {
		return MetricSample{}, ErrInvalidResult
	}
	value, warnings, err := c.api.Query(ctx, query, observedAt)
	if err != nil {
		return MetricSample{}, fmt.Errorf("query prometheus metric: %w", err)
	}
	if len(warnings) > 0 {
		return MetricSample{}, fmt.Errorf("prometheus returned warnings: %w", ErrInvalidResult)
	}
	sample, err := c.singleSample(value, observedAt)
	if err != nil {
		return MetricSample{}, err
	}
	return MetricSample{Name: request.Kind.String(), Value: float64(sample.Value), Unit: unit, ObservedAt: sourceObservedAt}, nil
}

func (c *Client) query(target domain.DeploymentTarget, request QueryRequest) (string, string, error) {
	matchers := targetMatchers(target)
	window := durationLiteral(request.Window)
	offset := ""
	if request.Offset > 0 {
		offset = " offset " + durationLiteral(request.Offset)
	}
	switch request.Kind {
	case domain.SymptomHTTPErrorRate:
		all := SeriesRequestsTotal + "{" + matchers + "}"
		errorsOnly := SeriesRequestsTotal + "{" + matchers + ",code=~\"5..\"}"
		errorRate := fmt.Sprintf("sum(rate(%s[%s]%s))", errorsOnly, window, offset)
		totalRate := fmt.Sprintf("sum(rate(%s[%s]%s))", all, window, offset)
		// A healthy pod has no 5xx series, so the numerator falls back to
		// 0 x total. The denominator drops windows without traffic, so zero
		// traffic yields no sample (an error), never a 0 error rate that
		// would pass recovery on a service that serves nothing.
		return fmt.Sprintf("(%s or (%s * 0)) / (%s > 0)", errorRate, totalRate, totalRate), "ratio", nil
	case domain.SymptomRequestLatency:
		if request.Percentile != 0.50 && request.Percentile != 0.95 && request.Percentile != 0.99 {
			return "", "", &domain.ValidationError{Field: "percentile", Reason: "must be p50, p95, or p99"}
		}
		return fmt.Sprintf("histogram_quantile(%g, sum by (le) (rate(%s_bucket{%s}[%s]%s)))", request.Percentile, SeriesRequestDuration, matchers, window, offset), "seconds", nil
	case domain.SymptomPodCPUUsage:
		return "sum(" + SeriesPodCPUUsage + "{" + matchers + "}" + offset + ")", "cores", nil
	case domain.SymptomPodMemoryUsage:
		return "sum(" + SeriesPodMemoryUsage + "{" + matchers + "}" + offset + ")", "bytes", nil
	default:
		return "", "", fmt.Errorf("metric %q: %w", request.Kind, ErrUnsupportedKind)
	}
}

func (c *Client) sourceFreshnessQuery(target domain.DeploymentTarget, kind domain.SymptomKind) (string, error) {
	var metric string
	switch kind {
	case domain.SymptomHTTPErrorRate:
		metric = SeriesRequestsTotal
	case domain.SymptomRequestLatency:
		metric = SeriesRequestDuration + "_bucket"
	case domain.SymptomPodCPUUsage:
		metric = SeriesPodCPUUsage
	case domain.SymptomPodMemoryUsage:
		metric = SeriesPodMemoryUsage
	default:
		return "", fmt.Errorf("metric %q: %w", kind, ErrUnsupportedKind)
	}
	return "min(timestamp(" + metric + "{" + targetMatchers(target) + "}))", nil
}

func targetMatchers(target domain.DeploymentTarget) string {
	return fmt.Sprintf("cluster=%s,namespace=%s,deployment=%s,service=%s", quote(target.Cluster()), quote(target.Namespace()), quote(target.Deployment()), quote(target.Service()))
}

func (c *Client) singleSample(value model.Value, now time.Time) (*model.Sample, error) {
	vector, ok := value.(model.Vector)
	if !ok || len(vector) != 1 || vector[0] == nil {
		return nil, ErrInvalidResult
	}
	sample := vector[0]
	numeric := float64(sample.Value)
	if math.IsNaN(numeric) || math.IsInf(numeric, 0) {
		return nil, ErrInvalidResult
	}
	timestamp := sample.Timestamp.Time()
	if timestamp.After(now.Add(time.Second)) || now.Sub(timestamp) > c.maxAge {
		return nil, ErrInvalidResult
	}
	return sample, nil
}

func quote(value string) string {
	return strconv.Quote(value)
}

func durationLiteral(value time.Duration) string {
	return strings.ReplaceAll(value.String(), "µ", "u")
}
