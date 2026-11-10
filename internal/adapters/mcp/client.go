package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type Caller interface {
	CallTool(context.Context, *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error)
}

type Client struct {
	session *mcpsdk.ClientSession
}

type boundedCaller struct {
	next    Caller
	timeout time.Duration
}

func BoundCalls(caller Caller, timeout time.Duration) (Caller, error) {
	if caller == nil || timeout <= 0 {
		return nil, fmt.Errorf("invalid MCP call timeout")
	}
	return boundedCaller{next: caller, timeout: timeout}, nil
}

func (c boundedCaller) CallTool(ctx context.Context, params *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
	if params != nil && params.Name == ToolWatchRollout {
		return c.next.CallTool(ctx, params)
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	return c.next.CallTool(callCtx, params)
}

func Connect(ctx context.Context, endpoint string, httpClient *http.Client) (*Client, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("invalid MCP endpoint")
	}
	if httpClient == nil {
		return nil, fmt.Errorf("MCP HTTP client is required")
	}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "rollback-guardian-orchestrator", Version: "v0.3.0"}, nil)
	session, err := client.Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: endpoint, HTTPClient: httpClient, MaxRetries: -1, DisableStandaloneSSE: true}, nil)
	if err != nil {
		return nil, fmt.Errorf("connect MCP client: %w", err)
	}
	return &Client{session: session}, nil
}

func (c *Client) CallTool(ctx context.Context, params *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
	return c.session.CallTool(ctx, params)
}

func (c *Client) Ping(ctx context.Context) error {
	return c.session.Ping(ctx, nil)
}

func (c *Client) Close() error {
	return c.session.Close()
}

func (c *Client) RequireTools(ctx context.Context, expected []string) error {
	var got []string
	for tool, err := range c.session.Tools(ctx, nil) {
		if err != nil {
			return fmt.Errorf("list MCP tools: %w", err)
		}
		got = append(got, tool.Name)
	}
	return requireToolCatalog(got, expected)
}

// requireToolCatalog fails closed unless the served tool names are exactly
// the expected ones, in any order.
func requireToolCatalog(served, expected []string) error {
	want := slices.Sorted(slices.Values(expected))
	got := slices.Sorted(slices.Values(served))
	if !slices.Equal(got, want) {
		return fmt.Errorf("MCP tool catalog = %q, want %q", got, want)
	}
	return nil
}

func callTool[In, Out any](ctx context.Context, caller Caller, name string, input In) (Out, error) {
	var zero Out
	if caller == nil {
		return zero, fmt.Errorf("MCP caller is required")
	}
	result, err := caller.CallTool(ctx, &mcpsdk.CallToolParams{Name: name, Arguments: input})
	if err != nil {
		return zero, fmt.Errorf("call MCP tool %s: %w", name, err)
	}
	if result.StructuredContent == nil {
		if result.IsError {
			return zero, &domain.ValidationError{Field: "mcp_tool_input", Reason: "tool rejected request"}
		}
		return zero, fmt.Errorf("MCP tool %s returned no structured result", name)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return zero, fmt.Errorf("encode MCP tool %s result: %w", name, err)
	}
	var response Response[Out]
	if err := json.Unmarshal(encoded, &response); err != nil {
		return zero, fmt.Errorf("decode MCP tool %s result: %w", name, err)
	}
	if !response.OK || result.IsError {
		if response.Error == nil {
			return zero, fmt.Errorf("MCP tool %s returned an invalid error result", name)
		}
		return zero, errorFromTool(*response.Error)
	}
	if response.Data == nil || response.Error != nil {
		return zero, fmt.Errorf("MCP tool %s returned an invalid success result", name)
	}
	return *response.Data, nil
}

type toolCallError struct {
	code      ErrorCode
	message   string
	retryable bool
}

func (e *toolCallError) Error() string {
	return e.message
}

func errorFromTool(toolError ToolError) error {
	switch toolError.Code {
	case CodeInvalidArgument:
		return &domain.ValidationError{Field: "mcp_tool_input", Reason: toolError.Message}
	case CodeTargetForbidden:
		return &domain.ValidationError{Field: "mcp_target", Reason: toolError.Message}
	case CodeNotFound:
		return &domain.NotFoundError{Resource: "MCP resource", ID: "requested"}
	case CodeRevisionDrift, CodeApprovalMismatch:
		return &domain.ApprovalMismatchError{Reason: toolError.Message}
	case CodePolicyDenied:
		return &domain.PolicyDenialError{Rule: "mcp_policy_validation"}
	case CodeExpired:
		return &domain.ExpiryError{Resource: "MCP approval"}
	case CodeIdempotencyConflict:
		return &domain.IdempotencyConflictError{}
	case CodeCanceled:
		return context.Canceled
	case CodeTimeout:
		return context.DeadlineExceeded
	case CodeDependencyUnavailable, CodeInternal:
		// Outcome unknown: kept as a tool error so ambiguousToolError sees the code.
		return &toolCallError{code: toolError.Code, message: toolError.Message, retryable: toolError.Retryable}
	default:
		// Codes this client does not know stay tool errors and are never
		// mapped to a domain outcome.
		return &toolCallError{code: toolError.Code, message: toolError.Message, retryable: toolError.Retryable}
	}
}

func ambiguousToolError(err error) bool {
	var toolErr *toolCallError
	return errors.As(err, &toolErr) && (toolErr.retryable || toolErr.code == CodeInternal || toolErr.code == CodeDependencyUnavailable)
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
