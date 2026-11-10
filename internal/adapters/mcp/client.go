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
	"strings"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/buildinfo"
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

// MCP client implementation names, one per process that connects to the MCP
// servers.
const (
	ClientOrchestrator = "rollback-guardian-orchestrator"
	ClientInvestigator = "rollback-guardian-investigator"
)

// Connect opens a session identifying itself as clientName, which the
// composition root chooses (ClientOrchestrator or ClientInvestigator).
func Connect(ctx context.Context, endpoint string, httpClient *http.Client, clientName string) (*Client, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("invalid MCP endpoint")
	}
	if httpClient == nil {
		return nil, fmt.Errorf("MCP HTTP client is required")
	}
	if strings.TrimSpace(clientName) == "" {
		return nil, fmt.Errorf("MCP client name is required")
	}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: clientName, Version: buildinfo.Version}, nil)
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
			// Every tool here answers with a structured result, including its
			// rejections. An unstructured error comes from the SDK, possibly
			// after the handler ran, so it is not a definitive rejection.
			return zero, &toolCallError{code: CodeInternal, message: "MCP tool " + name + " returned an unstructured error"}
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

// Transient lets the resilience decorators retry reads when the server says
// a dependency was unavailable. Internal and unrecognized codes are not
// transient: the server may already have acted.
func (e *toolCallError) Transient() bool {
	return e.code == CodeDependencyUnavailable
}

func errorFromTool(toolError ToolError) error {
	switch toolError.Code {
	// Reasons are fixed here, never copied from the server's message: a
	// validation error's text can reach an API response body.
	case CodeInvalidArgument:
		return &domain.ValidationError{Field: "mcp_tool_input", Reason: "tool rejected the request"}
	case CodeTargetForbidden:
		return &domain.ValidationError{Field: "mcp_target", Reason: "target is not allowed"}
	case CodeNotFound:
		return &domain.NotFoundError{Resource: "MCP resource", ID: "requested"}
	case CodeRevisionDrift, CodeApprovalMismatch:
		return &domain.ApprovalMismatchError{Reason: "MCP server reported revision drift or an approval mismatch"}
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
