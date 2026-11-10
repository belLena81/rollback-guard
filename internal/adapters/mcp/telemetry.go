package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/belLena81/rollback-guard/internal/ports"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Server names used as the bounded "server" telemetry label.
const (
	ServerKubernetesReadonly = "kubernetes-readonly"
	ServerKubernetesRollback = "kubernetes-rollback"
	ServerObservability      = "observability"
)

// Telemetry outcome values for MCP calls.
const (
	OutcomeOK        = "ok"
	OutcomeToolError = "tool_error"
	OutcomeTimeout   = "timeout"
	OutcomeCanceled  = "canceled"
	OutcomeError     = "error"
	unknownTool      = "unknown"
)

var knownTools = map[string]bool{
	ToolGetDeployment: true, ToolGetReplicaSets: true, ToolListPods: true, ToolGetPodEvents: true,
	ToolGetRolloutStatus: true, ToolWatchRollout: true, ToolPrepareRollback: true, ToolExecuteApprovedRollback: true,
	ToolGetHTTPErrorRate: true, ToolCompareErrorRateWindows: true, ToolGetRequestLatency: true,
	ToolGetPodCPUUsage: true, ToolGetPodMemoryUsage: true, ToolEvaluateRecovery: true,
}

// boundedTool keeps tool names from untrusted callers out of label values.
func boundedTool(name string) string {
	if knownTools[name] {
		return name
	}
	return unknownTool
}

// CallObserver instruments outgoing tool calls. It is implemented outside
// this package and sees only strings, never SDK types.
type CallObserver interface {
	ObserveToolCall(ctx context.Context, server string, tool string) (context.Context, func(outcome string))
}

// ToolObserver instruments tool calls handled by an MCP server. header is
// the HTTP header of the request that carried the call (for W3C trace
// context extraction); it may be nil.
type ToolObserver interface {
	ObserveToolRequest(ctx context.Context, header http.Header, server string, tool string) (context.Context, func(outcome string))
}

type observedCaller struct {
	next     Caller
	server   string
	observer CallObserver
}

// ObserveCalls decorates caller so each tool call is reported to observer
// under server. Wrap it outside BoundCalls so timeouts are observed.
func ObserveCalls(caller Caller, server string, observer CallObserver) (Caller, error) {
	if caller == nil || observer == nil || server == "" {
		return nil, fmt.Errorf("caller, server name, and observer are required")
	}
	return observedCaller{next: caller, server: server, observer: observer}, nil
}

func (c observedCaller) CallTool(ctx context.Context, params *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
	tool := unknownTool
	if params != nil {
		tool = boundedTool(params.Name)
	}
	ctx, finish := c.observer.ObserveToolCall(ctx, c.server, tool)
	result, err := c.next.CallTool(ctx, params)
	finish(callOutcome(result, err))
	return result, err
}

func callOutcome(result *mcpsdk.CallToolResult, err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return OutcomeTimeout
	case errors.Is(err, context.Canceled):
		return OutcomeCanceled
	case err != nil:
		return OutcomeError
	case result == nil:
		return OutcomeError
	case result.IsError:
		return OutcomeToolError
	default:
		return OutcomeOK
	}
}

// correlationArguments are the tool arguments that are log-correlation
// identifiers. Everything else in a tool call stays out of logs.
type correlationArguments struct {
	IdempotencyKey string `json:"idempotencyKey"`
}

func observeToolsMiddleware(server string, observer ToolObserver) mcpsdk.Middleware {
	return func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, request mcpsdk.Request) (mcpsdk.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, request)
			}
			tool := unknownTool
			if params, ok := request.GetParams().(*mcpsdk.CallToolParamsRaw); ok && params != nil {
				tool = boundedTool(params.Name)
				var arguments correlationArguments
				if json.Unmarshal(params.Arguments, &arguments) == nil && arguments.IdempotencyKey != "" {
					ctx = ports.WithCorrelation(ctx, ports.Correlation{IdempotencyKey: arguments.IdempotencyKey})
				}
			}
			var header http.Header
			if extra := request.GetExtra(); extra != nil {
				header = extra.Header
			}
			ctx, finish := observer.ObserveToolRequest(ctx, header, server, tool)
			result, err := next(ctx, method, request)
			callResult, isCallResult := result.(*mcpsdk.CallToolResult)
			if err == nil && (!isCallResult || callResult == nil) {
				finish(OutcomeError)
			} else {
				finish(callOutcome(callResult, err))
			}
			return result, err
		}
	}
}

// RouteOf maps requests to the fixed routes served by HTTPHandler, for HTTP
// telemetry labels.
func RouteOf(request *http.Request) string {
	switch request.URL.Path {
	case "/mcp", "/health/live", "/health/ready", "/metrics":
		return request.URL.Path
	default:
		return "unmatched"
	}
}
