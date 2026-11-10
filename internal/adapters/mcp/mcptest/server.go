// Package mcptest provides an in-process MCP server for tests of code that
// connects MCP clients. It keeps MCP SDK types inside the MCP adapter tree.
package mcptest

import (
	"context"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	protocol "github.com/belLena81/rollback-guard/internal/adapters/mcp"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ClientInfo is the implementation info a client sent when initializing.
type ClientInfo struct {
	Name    string
	Version string
}

// Server is a running test MCP server.
type Server struct {
	// Endpoint is the Streamable HTTP URL to connect to.
	Endpoint string

	mu      sync.Mutex
	clients []ClientInfo
}

// Clients returns the info of every client that initialized a session.
func (s *Server) Clients() []ClientInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.clients)
}

type input struct{}

type output struct {
	OK bool `json:"ok"`
}

// NewServer serves trivial, immediate tools with the given names. See
// NewSlowServer.
func NewServer(t *testing.T, tools ...string) *Server {
	t.Helper()
	return NewSlowServer(t, 0, tools...)
}

// NewSlowServer serves tools that each take delay to answer, over the same HTTP
// handler the real servers use, until the test ends.
func NewSlowServer(t *testing.T, delay time.Duration, tools ...string) *Server {
	t.Helper()
	recorded := &Server{}
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "mcptest", Version: "v0"}, nil)
	for _, name := range tools {
		mcpsdk.AddTool(
			server,
			&mcpsdk.Tool{Name: name, Description: "test tool"},
			func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ input) (*mcpsdk.CallToolResult, output, error) {
				select {
				case <-ctx.Done():
					return nil, output{}, ctx.Err()
				case <-time.After(delay):
					return nil, output{OK: true}, nil
				}
			},
		)
	}
	server.AddReceivingMiddleware(func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, request mcpsdk.Request) (mcpsdk.Result, error) {
			if params, ok := request.GetParams().(*mcpsdk.InitializeParams); ok && params.ClientInfo != nil {
				recorded.mu.Lock()
				recorded.clients = append(recorded.clients, ClientInfo{Name: params.ClientInfo.Name, Version: params.ClientInfo.Version})
				recorded.mu.Unlock()
			}
			return next(ctx, method, request)
		}
	})
	httpServer := httptest.NewServer(protocol.HTTPHandler(server, nil, 0, time.Minute, nil))
	t.Cleanup(httpServer.Close)
	recorded.Endpoint = httpServer.URL + "/mcp"
	return recorded
}

// Call invokes tool with empty arguments on client and reports only whether
// the call completed, so callers outside the adapter need no SDK types.
func Call(ctx context.Context, client *protocol.Client, tool string) error {
	_, err := client.CallTool(ctx, &mcpsdk.CallToolParams{Name: tool, Arguments: input{}})
	return err
}
