// Package mcptest provides an in-process MCP server for tests of code that
// connects MCP clients. It keeps MCP SDK types inside the MCP adapter tree.
package mcptest

import (
	"context"
	"net/http/httptest"
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
	return append([]ClientInfo(nil), s.clients...)
}

type input struct{}

type output struct {
	OK bool `json:"ok"`
}

// NewServer serves trivial tools with the given names over the same HTTP
// handler the real servers use, until the test ends.
func NewServer(t *testing.T, tools ...string) *Server {
	t.Helper()
	recorded := &Server{}
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "mcptest", Version: "v0"}, nil)
	for _, name := range tools {
		mcpsdk.AddTool(server, &mcpsdk.Tool{Name: name, Description: "test tool"}, func(context.Context, *mcpsdk.CallToolRequest, input) (*mcpsdk.CallToolResult, output, error) {
			return nil, output{OK: true}, nil
		})
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
