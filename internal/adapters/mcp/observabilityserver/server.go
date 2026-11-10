package observabilityserver

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	protocol "github.com/belLena81/rollback-guard/internal/adapters/mcp"
	metricsprom "github.com/belLena81/rollback-guard/internal/adapters/prometheus"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/buildinfo"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Backend interface {
	QueryMetric(context.Context, domain.DeploymentTarget, metricsprom.QueryRequest) (metricsprom.MetricSample, error)
}

func New(backend Backend) (*mcp.Server, error) {
	if backend == nil {
		return nil, &domain.ValidationError{Field: "observability_mcp", Reason: "backend is required"}
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "rollback-guardian-observability", Version: buildinfo.Version}, nil)
	annotations := &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: boolPointer(false), IdempotentHint: true, OpenWorldHint: boolPointer(false)}
	mcp.AddTool(server, &mcp.Tool{Name: protocol.ToolGetHTTPErrorRate, Description: "Read a fixed-window HTTP 5xx ratio", Annotations: annotations}, metricHandler(backend, "http_error_rate"))
	mcp.AddTool(server, &mcp.Tool{Name: protocol.ToolCompareErrorRateWindows, Description: "Compare current and baseline HTTP 5xx ratios", Annotations: annotations}, compareWindows(backend))
	mcp.AddTool(server, &mcp.Tool{Name: protocol.ToolGetRequestLatency, Description: "Read a fixed request latency percentile", Annotations: annotations}, latency(backend))
	mcp.AddTool(server, &mcp.Tool{Name: protocol.ToolGetPodCPUUsage, Description: "Read aggregate pod CPU usage for an allowed target", Annotations: annotations}, metricHandler(backend, "pod_cpu_usage"))
	mcp.AddTool(server, &mcp.Tool{Name: protocol.ToolGetPodMemoryUsage, Description: "Read aggregate pod memory usage for an allowed target", Annotations: annotations}, metricHandler(backend, "pod_memory_usage"))
	mcp.AddTool(server, &mcp.Tool{Name: protocol.ToolEvaluateRecovery, Description: "Evaluate typed rollout and metric recovery signals", Annotations: annotations}, evaluateRecovery(backend))
	return server, nil
}

func NewHTTPHandler(backend Backend, ready func(context.Context) error, sessionTimeout time.Duration, logger *slog.Logger, opts ...protocol.HandlerOption) (http.Handler, error) {
	server, err := New(backend)
	if err != nil {
		return nil, err
	}
	return protocol.HTTPHandler(server, ready, protocol.DefaultMaxRequestBytes, sessionTimeout, logger, opts...), nil
}

func metricHandler(backend Backend, kind string) mcp.ToolHandlerFor[protocol.MetricInput, protocol.Response[protocol.MetricOutput]] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, input protocol.MetricInput) (*mcp.CallToolResult, protocol.Response[protocol.MetricOutput], error) {
		if !validWindow(input.WindowSeconds) || input.Threshold <= 0 || math.IsNaN(input.Threshold) || math.IsInf(input.Threshold, 0) {
			result, response := protocol.Failure[protocol.MetricOutput](&domain.ValidationError{Field: "metric", Reason: "window or threshold is invalid"})
			return result, response, nil
		}
		target, err := input.Target.Domain()
		if err != nil {
			result, response := protocol.Failure[protocol.MetricOutput](err)
			return result, response, nil
		}
		sample, err := backend.QueryMetric(ctx, target, metricsprom.QueryRequest{Kind: kind, Window: time.Duration(input.WindowSeconds) * time.Second, Percentile: 0.95})
		if err != nil {
			result, response := protocol.Failure[protocol.MetricOutput](err)
			return result, response, nil
		}
		return nil, protocol.Success(metricOutput(sample)), nil
	}
}

func compareWindows(backend Backend) mcp.ToolHandlerFor[protocol.CompareWindowsInput, protocol.Response[protocol.CompareWindowsOutput]] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, input protocol.CompareWindowsInput) (*mcp.CallToolResult, protocol.Response[protocol.CompareWindowsOutput], error) {
		if !validWindow(input.CurrentWindowSeconds) || !validWindow(input.BaselineWindowSeconds) || input.BaselineOffsetSeconds < 0 || input.BaselineOffsetSeconds > 86400 {
			result, response := protocol.Failure[protocol.CompareWindowsOutput](&domain.ValidationError{Field: "metric_windows", Reason: "window or offset is invalid"})
			return result, response, nil
		}
		target, err := input.Target.Domain()
		if err != nil {
			result, response := protocol.Failure[protocol.CompareWindowsOutput](err)
			return result, response, nil
		}
		current, err := backend.QueryMetric(ctx, target, metricsprom.QueryRequest{Kind: "http_error_rate", Window: time.Duration(input.CurrentWindowSeconds) * time.Second})
		if err != nil {
			result, response := protocol.Failure[protocol.CompareWindowsOutput](err)
			return result, response, nil
		}
		baseline, err := backend.QueryMetric(ctx, target, metricsprom.QueryRequest{Kind: "http_error_rate", Window: time.Duration(input.BaselineWindowSeconds) * time.Second, Offset: time.Duration(input.BaselineOffsetSeconds) * time.Second})
		if err != nil {
			result, response := protocol.Failure[protocol.CompareWindowsOutput](err)
			return result, response, nil
		}
		return nil, protocol.Success(protocol.CompareWindowsOutput{Current: metricOutput(current), Baseline: metricOutput(baseline), Delta: current.Value - baseline.Value}), nil
	}
}

func latency(backend Backend) mcp.ToolHandlerFor[protocol.LatencyInput, protocol.Response[protocol.MetricOutput]] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, input protocol.LatencyInput) (*mcp.CallToolResult, protocol.Response[protocol.MetricOutput], error) {
		if !validWindow(input.WindowSeconds) || input.Threshold <= 0 || math.IsNaN(input.Threshold) || math.IsInf(input.Threshold, 0) {
			result, response := protocol.Failure[protocol.MetricOutput](&domain.ValidationError{Field: "metric", Reason: "window or threshold is invalid"})
			return result, response, nil
		}
		percentile, ok := map[string]float64{"p50": 0.50, "p95": 0.95, "p99": 0.99}[input.Percentile]
		if !ok {
			result, response := protocol.Failure[protocol.MetricOutput](&domain.ValidationError{Field: "percentile", Reason: "unsupported percentile"})
			return result, response, nil
		}
		target, err := input.Target.Domain()
		if err != nil {
			result, response := protocol.Failure[protocol.MetricOutput](err)
			return result, response, nil
		}
		sample, err := backend.QueryMetric(ctx, target, metricsprom.QueryRequest{Kind: "request_latency", Window: time.Duration(input.WindowSeconds) * time.Second, Percentile: percentile})
		if err != nil {
			result, response := protocol.Failure[protocol.MetricOutput](err)
			return result, response, nil
		}
		return nil, protocol.Success(metricOutput(sample)), nil
	}
}

func evaluateRecovery(backend Backend) mcp.ToolHandlerFor[protocol.RecoveryInput, protocol.Response[protocol.RecoveryOutput]] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, input protocol.RecoveryInput) (*mcp.CallToolResult, protocol.Response[protocol.RecoveryOutput], error) {
		if !validWindow(input.WindowSeconds) || input.Threshold <= 0 || math.IsNaN(input.Threshold) || math.IsInf(input.Threshold, 0) || input.DesiredReplicas < 0 || input.ReadyReplicas < 0 || input.AvailableReplicas < 0 {
			result, response := protocol.Failure[protocol.RecoveryOutput](&domain.ValidationError{Field: "recovery", Reason: "recovery bounds are invalid"})
			return result, response, nil
		}
		if strings.TrimSpace(input.ExpectedRevision) == "" || strings.TrimSpace(input.ObservedRevision) == "" {
			result, response := protocol.Failure[protocol.RecoveryOutput](&domain.ValidationError{Field: "recovery.revision", Reason: "expected and observed revisions are required"})
			return result, response, nil
		}
		target, err := input.Target.Domain()
		if err != nil {
			result, response := protocol.Failure[protocol.RecoveryOutput](err)
			return result, response, nil
		}
		sample, err := backend.QueryMetric(ctx, target, metricsprom.QueryRequest{Kind: input.MetricKind, Window: time.Duration(input.WindowSeconds) * time.Second, Percentile: 0.95})
		if err != nil {
			result, response := protocol.Failure[protocol.RecoveryOutput](err)
			return result, response, nil
		}
		if sample.Unit != input.Unit || input.Threshold <= 0 {
			result, response := protocol.Failure[protocol.RecoveryOutput](&domain.ValidationError{Field: "recovery.metric", Reason: "unit or threshold is invalid"})
			return result, response, nil
		}
		complete := input.Healthy && input.ExpectedRevision == input.ObservedRevision && input.DesiredReplicas == input.ReadyReplicas && input.DesiredReplicas == input.AvailableReplicas
		rollout := protocol.RolloutOutput{Revision: input.ObservedRevision, DesiredReplicas: input.DesiredReplicas, ReadyReplicas: input.ReadyReplicas, AvailableReplicas: input.AvailableReplicas, Complete: complete}
		return nil, protocol.Success(protocol.RecoveryOutput{Passed: complete && sample.Value < input.Threshold, Rollout: rollout, Metric: metricOutput(sample)}), nil
	}
}

func metricOutput(sample metricsprom.MetricSample) protocol.MetricOutput {
	return protocol.MetricOutput{Name: sample.Name, Value: sample.Value, Unit: sample.Unit, ObservedAt: sample.ObservedAt}
}

func boolPointer(value bool) *bool {
	return &value
}

func validWindow(seconds int) bool {
	return protocol.ValidWindowSeconds(seconds)
}
