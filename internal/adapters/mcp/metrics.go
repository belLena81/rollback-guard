package mcp

import (
	"context"
	"fmt"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
)

type MetricsAdapter struct {
	caller Caller
	window time.Duration
}

func NewMetricsAdapter(caller Caller, window time.Duration) (*MetricsAdapter, error) {
	if caller == nil || window < 30*time.Second || window > time.Hour {
		return nil, fmt.Errorf("invalid metrics MCP adapter configuration")
	}
	return &MetricsAdapter{caller: caller, window: window}, nil
}

func (a *MetricsAdapter) Observe(ctx context.Context, target domain.DeploymentTarget, symptom domain.Symptom) (domain.MetricEvidence, error) {
	input := MetricInput{Target: TargetFromDomain(target), WindowSeconds: int(a.window / time.Second), Threshold: symptom.Threshold()}
	var output MetricOutput
	var err error
	switch symptom.Kind() {
	case "http_error_rate":
		output, err = callTool[MetricInput, MetricOutput](ctx, a.caller, ToolGetHTTPErrorRate, input)
	case "request_latency":
		output, err = callTool[LatencyInput, MetricOutput](ctx, a.caller, ToolGetRequestLatency, LatencyInput{Target: input.Target, WindowSeconds: input.WindowSeconds, Threshold: input.Threshold, Percentile: "p95"})
	case "pod_cpu_usage":
		output, err = callTool[MetricInput, MetricOutput](ctx, a.caller, ToolGetPodCPUUsage, input)
	case "pod_memory_usage":
		output, err = callTool[MetricInput, MetricOutput](ctx, a.caller, ToolGetPodMemoryUsage, input)
	default:
		return domain.MetricEvidence{}, &domain.ValidationError{Field: "symptom.kind", Reason: "unsupported metrics MCP tool"}
	}
	if err != nil {
		return domain.MetricEvidence{}, fmt.Errorf("observe metric through MCP: %w", err)
	}
	if output.Name != symptom.Kind() || output.Unit != symptom.Unit() || output.ObservedAt.IsZero() {
		return domain.MetricEvidence{}, fmt.Errorf("metrics MCP returned inconsistent evidence")
	}
	return domain.MetricEvidence{Name: output.Name, Value: output.Value, Threshold: symptom.Threshold(), Unit: output.Unit, ObservedAt: output.ObservedAt.UTC()}, nil
}
