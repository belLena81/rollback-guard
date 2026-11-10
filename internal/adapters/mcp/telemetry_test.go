package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/belLena81/rollback-guard/internal/ports"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCallOutcomeIsBounded(t *testing.T) {
	tests := []struct {
		name   string
		result *mcpsdk.CallToolResult
		err    error
		want   string
	}{
		{name: "ok", result: &mcpsdk.CallToolResult{}, want: OutcomeOK},
		{name: "tool error", result: &mcpsdk.CallToolResult{IsError: true}, want: OutcomeToolError},
		{name: "timeout", err: fmt.Errorf("call: %w", context.DeadlineExceeded), want: OutcomeTimeout},
		{name: "canceled", err: context.Canceled, want: OutcomeCanceled},
		{name: "transport error", err: errors.New("connection refused to http://secret-host"), want: OutcomeError},
		{name: "missing result", want: OutcomeError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := callOutcome(test.result, test.err); got != test.want {
				t.Fatalf("callOutcome() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestBoundedToolRejectsUnknownNames(t *testing.T) {
	for _, name := range []string{ToolGetDeployment, ToolExecuteApprovedRollback, ToolEvaluateRecovery} {
		if boundedTool(name) != name {
			t.Fatalf("boundedTool(%q) changed a catalog name", name)
		}
	}
	for _, name := range []string{"", "kubectl", "get_deployment; rm -rf", "incident-123"} {
		if got := boundedTool(name); got != unknownTool {
			t.Fatalf("boundedTool(%q) = %q, want %q", name, got, unknownTool)
		}
	}
}

type recordingObserver struct {
	server, tool, outcome string
	correlation           ports.Correlation
	header                http.Header
}

func (r *recordingObserver) ObserveToolCall(ctx context.Context, server string, tool string) (context.Context, func(string)) {
	r.server, r.tool = server, tool
	return ctx, func(outcome string) { r.outcome = outcome }
}

func (r *recordingObserver) ObserveToolRequest(ctx context.Context, header http.Header, server string, tool string) (context.Context, func(string)) {
	r.server, r.tool, r.header = server, tool, header
	r.correlation = ports.CorrelationFromContext(ctx)
	return ctx, func(outcome string) { r.outcome = outcome }
}

type stubCaller struct{ result *mcpsdk.CallToolResult }

func (s stubCaller) CallTool(context.Context, *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
	return s.result, nil
}

func TestObserveCallsReportsServerToolAndOutcome(t *testing.T) {
	observer := &recordingObserver{}
	caller, err := ObserveCalls(stubCaller{result: &mcpsdk.CallToolResult{IsError: true}}, ServerKubernetesRollback, observer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := caller.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: ToolExecuteApprovedRollback}); err != nil {
		t.Fatal(err)
	}
	if observer.server != ServerKubernetesRollback || observer.tool != ToolExecuteApprovedRollback || observer.outcome != OutcomeToolError {
		t.Fatalf("observed = %+v", observer)
	}
	if _, err := ObserveCalls(nil, ServerObservability, observer); err == nil {
		t.Fatal("expected nil caller to be rejected")
	}
}

func TestServerMiddlewareCorrelatesIdempotencyKeyOnly(t *testing.T) {
	observer := &recordingObserver{}
	middleware := observeToolsMiddleware(ServerKubernetesRollback, observer)
	handler := middleware(func(context.Context, string, mcpsdk.Request) (mcpsdk.Result, error) {
		return &mcpsdk.CallToolResult{}, nil
	})
	header := http.Header{"Traceparent": []string{"00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"}}
	request := &mcpsdk.ServerRequest[*mcpsdk.CallToolParamsRaw]{
		Params: &mcpsdk.CallToolParamsRaw{Name: ToolExecuteApprovedRollback, Arguments: []byte(`{"proposalHash":"sha256-abc","idempotencyKey":"approval:approval-1"}`)},
		Extra:  &mcpsdk.RequestExtra{Header: header},
	}
	if _, err := handler(context.Background(), "tools/call", request); err != nil {
		t.Fatal(err)
	}
	if observer.tool != ToolExecuteApprovedRollback || observer.outcome != OutcomeOK || observer.header.Get("traceparent") == "" {
		t.Fatalf("observed = %+v", observer)
	}
	if observer.correlation != (ports.Correlation{IdempotencyKey: "approval:approval-1"}) {
		t.Fatalf("correlation = %+v, want only the idempotency key", observer.correlation)
	}
	unknown := &mcpsdk.ServerRequest[*mcpsdk.CallToolParamsRaw]{Params: &mcpsdk.CallToolParamsRaw{Name: "kubectl_exec"}}
	if _, err := handler(context.Background(), "tools/call", unknown); err != nil {
		t.Fatal(err)
	}
	if observer.tool != unknownTool {
		t.Fatalf("unknown tool label = %q", observer.tool)
	}
}
