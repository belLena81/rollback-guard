package observabilityserver

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	protocol "github.com/belLena81/rollback-guard/internal/adapters/mcp"
	metricsprom "github.com/belLena81/rollback-guard/internal/adapters/prometheus"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func metricHandler(backend Backend, kind domain.SymptomKind) mcp.ToolHandlerFor[protocol.MetricInput, protocol.Response[protocol.MetricOutput]] {
	return protocol.Handle(func(ctx context.Context, input protocol.MetricInput) (protocol.MetricOutput, error) {
		if !validWindow(input.WindowSeconds) || input.Threshold <= 0 || math.IsNaN(input.Threshold) || math.IsInf(input.Threshold, 0) {
			return protocol.MetricOutput{}, &domain.ValidationError{Field: "metric", Reason: "window or threshold is invalid"}
		}
		target, err := input.Target.Domain()
		if err != nil {
			return protocol.MetricOutput{}, fmt.Errorf("convert target: %w", err)
		}
		sample, err := backend.QueryMetric(
			ctx,
			target,
			metricsprom.QueryRequest{Kind: kind, Window: time.Duration(input.WindowSeconds) * time.Second, Percentile: 0.95},
		)
		if err != nil {
			return protocol.MetricOutput{}, fmt.Errorf("query metric backend: %w", err)
		}
		return metricOutput(sample), nil
	})
}

func compareWindows(backend Backend) mcp.ToolHandlerFor[protocol.CompareWindowsInput, protocol.Response[protocol.CompareWindowsOutput]] {
	return protocol.Handle(func(ctx context.Context, input protocol.CompareWindowsInput) (protocol.CompareWindowsOutput, error) {
		if !validWindow(input.CurrentWindowSeconds) || !validWindow(input.BaselineWindowSeconds) || input.BaselineOffsetSeconds < 0 ||
			input.BaselineOffsetSeconds > 86400 {
			return protocol.CompareWindowsOutput{}, &domain.ValidationError{Field: "metric_windows", Reason: "window or offset is invalid"}
		}
		target, err := input.Target.Domain()
		if err != nil {
			return protocol.CompareWindowsOutput{}, fmt.Errorf("convert target: %w", err)
		}
		current, err := backend.QueryMetric(
			ctx,
			target,
			metricsprom.QueryRequest{Kind: domain.SymptomHTTPErrorRate, Window: time.Duration(input.CurrentWindowSeconds) * time.Second},
		)
		if err != nil {
			return protocol.CompareWindowsOutput{}, fmt.Errorf("query metric backend: %w", err)
		}
		baseline, err := backend.QueryMetric(
			ctx,
			target,
			metricsprom.QueryRequest{
				Kind:   domain.SymptomHTTPErrorRate,
				Window: time.Duration(input.BaselineWindowSeconds) * time.Second,
				Offset: time.Duration(input.BaselineOffsetSeconds) * time.Second,
			},
		)
		if err != nil {
			return protocol.CompareWindowsOutput{}, fmt.Errorf("query metric backend: %w", err)
		}
		return protocol.CompareWindowsOutput{Current: metricOutput(current), Baseline: metricOutput(baseline), Delta: current.Value - baseline.Value}, nil
	})
}

func latency(backend Backend) mcp.ToolHandlerFor[protocol.LatencyInput, protocol.Response[protocol.MetricOutput]] {
	return protocol.Handle(func(ctx context.Context, input protocol.LatencyInput) (protocol.MetricOutput, error) {
		if !validWindow(input.WindowSeconds) || input.Threshold <= 0 || math.IsNaN(input.Threshold) || math.IsInf(input.Threshold, 0) {
			return protocol.MetricOutput{}, &domain.ValidationError{Field: "metric", Reason: "window or threshold is invalid"}
		}
		percentile, ok := map[string]float64{"p50": 0.50, "p95": 0.95, "p99": 0.99}[input.Percentile]
		if !ok {
			return protocol.MetricOutput{}, &domain.ValidationError{Field: "percentile", Reason: "unsupported percentile"}
		}
		target, err := input.Target.Domain()
		if err != nil {
			return protocol.MetricOutput{}, fmt.Errorf("convert target: %w", err)
		}
		sample, err := backend.QueryMetric(
			ctx,
			target,
			metricsprom.QueryRequest{Kind: domain.SymptomRequestLatency, Window: time.Duration(input.WindowSeconds) * time.Second, Percentile: percentile},
		)
		if err != nil {
			return protocol.MetricOutput{}, fmt.Errorf("query metric backend: %w", err)
		}
		return metricOutput(sample), nil
	})
}

func evaluateRecovery(backend Backend) mcp.ToolHandlerFor[protocol.RecoveryInput, protocol.Response[protocol.RecoveryOutput]] {
	return protocol.Handle(func(ctx context.Context, input protocol.RecoveryInput) (protocol.RecoveryOutput, error) {
		if !validWindow(input.WindowSeconds) || input.Threshold <= 0 || math.IsNaN(input.Threshold) || math.IsInf(input.Threshold, 0) ||
			input.DesiredReplicas < 0 ||
			input.ReadyReplicas < 0 ||
			input.AvailableReplicas < 0 {
			return protocol.RecoveryOutput{}, &domain.ValidationError{Field: "recovery", Reason: "recovery bounds are invalid"}
		}
		if strings.TrimSpace(input.ExpectedRevision) == "" || strings.TrimSpace(input.ObservedRevision) == "" {
			return protocol.RecoveryOutput{}, &domain.ValidationError{Field: "recovery.revision", Reason: "expected and observed revisions are required"}
		}
		target, err := input.Target.Domain()
		if err != nil {
			return protocol.RecoveryOutput{}, fmt.Errorf("convert target: %w", err)
		}
		sample, err := backend.QueryMetric(
			ctx,
			target,
			metricsprom.QueryRequest{Kind: domain.SymptomKind(input.MetricKind), Window: time.Duration(input.WindowSeconds) * time.Second, Percentile: 0.95},
		)
		if err != nil {
			return protocol.RecoveryOutput{}, fmt.Errorf("query metric backend: %w", err)
		}
		if sample.Unit != input.Unit || input.Threshold <= 0 {
			return protocol.RecoveryOutput{}, &domain.ValidationError{Field: "recovery.metric", Reason: "unit or threshold is invalid"}
		}
		complete := input.Healthy && input.ExpectedRevision == input.ObservedRevision && input.DesiredReplicas == input.ReadyReplicas &&
			input.DesiredReplicas == input.AvailableReplicas
		rollout := protocol.RolloutOutput{
			Revision:          input.ObservedRevision,
			DesiredReplicas:   input.DesiredReplicas,
			ReadyReplicas:     input.ReadyReplicas,
			AvailableReplicas: input.AvailableReplicas,
			Complete:          complete,
		}
		return protocol.RecoveryOutput{Passed: complete && sample.Value < input.Threshold, Rollout: rollout, Metric: metricOutput(sample)}, nil
	})
}

func metricOutput(sample metricsprom.MetricSample) protocol.MetricOutput {
	return protocol.MetricOutput{Name: sample.Name, Value: sample.Value, Unit: sample.Unit, ObservedAt: sample.ObservedAt}
}

func validWindow(seconds int) bool {
	return protocol.ValidWindowSeconds(seconds)
}
