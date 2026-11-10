package observabilityserver

import (
	"context"
	"log/slog"
	"net/http"
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
	annotations := &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: new(false), IdempotentHint: true, OpenWorldHint: new(false)}
	mcp.AddTool(
		server,
		&mcp.Tool{Name: protocol.ToolGetHTTPErrorRate, Description: "Read a fixed-window HTTP 5xx ratio", Annotations: annotations},
		metricHandler(backend, domain.SymptomHTTPErrorRate),
	)
	mcp.AddTool(
		server,
		&mcp.Tool{Name: protocol.ToolCompareErrorRateWindows, Description: "Compare current and baseline HTTP 5xx ratios", Annotations: annotations},
		compareWindows(backend),
	)
	mcp.AddTool(
		server,
		&mcp.Tool{Name: protocol.ToolGetRequestLatency, Description: "Read a fixed request latency percentile", Annotations: annotations},
		latency(backend),
	)
	mcp.AddTool(
		server,
		&mcp.Tool{Name: protocol.ToolGetPodCPUUsage, Description: "Read aggregate pod CPU usage for an allowed target", Annotations: annotations},
		metricHandler(backend, domain.SymptomPodCPUUsage),
	)
	mcp.AddTool(
		server,
		&mcp.Tool{Name: protocol.ToolGetPodMemoryUsage, Description: "Read aggregate pod memory usage for an allowed target", Annotations: annotations},
		metricHandler(backend, domain.SymptomPodMemoryUsage),
	)
	mcp.AddTool(
		server,
		&mcp.Tool{Name: protocol.ToolEvaluateRecovery, Description: "Evaluate typed rollout and metric recovery signals", Annotations: annotations},
		evaluateRecovery(backend),
	)
	return server, nil
}

func NewHTTPHandler(
	backend Backend,
	ready func(context.Context) error,
	sessionTimeout time.Duration,
	logger *slog.Logger,
	opts ...protocol.HandlerOption,
) (http.Handler, error) {
	server, err := New(backend)
	if err != nil {
		return nil, err
	}
	return protocol.HTTPHandler(server, ready, protocol.DefaultMaxRequestBytes, sessionTimeout, logger, opts...), nil
}
