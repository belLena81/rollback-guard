package bootstrap

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	mcpadapter "github.com/belLena81/rollback-guard/internal/adapters/mcp"
	"github.com/belLena81/rollback-guard/internal/adapters/mcp/mcptest"
	"github.com/belLena81/rollback-guard/internal/platform/buildinfo"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestNewPoolAppliesTheConnectionLimit(t *testing.T) {
	// The pool connects lazily, so no database is needed.
	pool, err := NewPool(context.Background(), "postgres://rollback@127.0.0.1:1/rollback_guard", 7)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if got := pool.Config().MaxConns; got != 7 {
		t.Fatalf("MaxConns = %d, want 7", got)
	}
}

func TestNewPoolRejectsInvalidInputWithoutLeakingTheURL(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		maxConns int32
		want     string
	}{
		{name: "empty URL", url: " ", maxConns: 4, want: "database URL is required"},
		{name: "zero pool size", url: "postgres://rollback@db/rollback_guard", maxConns: 0, want: "pool size must be positive"},
		{name: "negative pool size", url: "postgres://rollback@db/rollback_guard", maxConns: -1, want: "pool size must be positive"},
		{name: "unparseable URL", url: "postgres://admin-user:s3cr3t-pass@db-host.internal:notaport/rollback_guard", maxConns: 4, want: "parse database configuration"},
		{name: "unknown option", url: "postgres://admin-user:s3cr3t-pass@db-host.internal/rollback_guard?pool_max_conns=many", maxConns: 4, want: "parse database configuration"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pool, err := NewPool(context.Background(), test.url, test.maxConns)
			if err == nil {
				pool.Close()
				t.Fatal("NewPool() succeeded, want an error")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewPool() error = %q, want %q", err, test.want)
			}
			for _, secret := range []string{"admin-user", "s3cr3t-pass", "db-host.internal", "rollback_guard"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("NewPool() error %q leaks %q from the database URL", err, secret)
				}
			}
		})
	}
}

type pingInput struct{}

// newMCPServer serves the named tools and reports the client info of every
// initialize request.
func newMCPServer(t *testing.T, tools ...string) (endpoint string, clients func() []mcptest.ClientInfo) {
	t.Helper()
	server := mcptest.NewServer(t, tools...)
	return server.Endpoint, server.Clients
}

func TestConnectMCPIdentifiesTheCallingProcess(t *testing.T) {
	for _, clientName := range []string{mcpadapter.ClientOrchestrator, mcpadapter.ClientInvestigator} {
		t.Run(clientName, func(t *testing.T) {
			endpoint, clients := newMCPServer(t, "alpha", "beta")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			client, err := ConnectMCP(ctx, clientName, &http.Client{Timeout: 5 * time.Second}, MCPServer{Name: "test", Endpoint: endpoint, Tools: []string{"beta", "alpha"}})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := client.Close(); err != nil {
					t.Errorf("close MCP client: %v", err)
				}
			}()

			if err := client.Ping(ctx); err != nil {
				t.Fatalf("ping over the connected session: %v", err)
			}
			seen := clients()
			if len(seen) == 0 {
				t.Fatal("server saw no initialize request")
			}
			for _, info := range seen {
				if info.Name != clientName || info.Version != buildinfo.Version {
					t.Fatalf("client info = %+v, want name %q version %q", info, clientName, buildinfo.Version)
				}
			}
		})
	}
}

func TestConnectMCPFailsClosed(t *testing.T) {
	endpoint, _ := newMCPServer(t, "alpha", "beta")
	httpClient := &http.Client{Timeout: 5 * time.Second}
	tests := []struct {
		name       string
		clientName string
		httpClient *http.Client
		server     MCPServer
		want       string
	}{
		{name: "server serves an unexpected tool", clientName: mcpadapter.ClientOrchestrator, httpClient: httpClient, server: MCPServer{Name: "readonly kubernetes", Endpoint: endpoint, Tools: []string{"alpha"}}, want: "connect readonly kubernetes MCP: MCP tool catalog"},
		{name: "server lacks an expected tool", clientName: mcpadapter.ClientOrchestrator, httpClient: httpClient, server: MCPServer{Name: "readonly kubernetes", Endpoint: endpoint, Tools: []string{"alpha", "beta", "generic_shell"}}, want: "connect readonly kubernetes MCP: MCP tool catalog"},
		{name: "empty expected catalog", clientName: mcpadapter.ClientOrchestrator, httpClient: httpClient, server: MCPServer{Name: "observability", Endpoint: endpoint}, want: "expected tool catalog are required"},
		{name: "missing server name", clientName: mcpadapter.ClientOrchestrator, httpClient: httpClient, server: MCPServer{Endpoint: endpoint, Tools: []string{"alpha", "beta"}}, want: "MCP server name"},
		{name: "missing client name", httpClient: httpClient, server: MCPServer{Name: "observability", Endpoint: endpoint, Tools: []string{"alpha", "beta"}}, want: "connect observability MCP: MCP client name is required"},
		{name: "missing HTTP client", clientName: mcpadapter.ClientInvestigator, server: MCPServer{Name: "observability", Endpoint: endpoint, Tools: []string{"alpha", "beta"}}, want: "connect observability MCP: MCP HTTP client is required"},
		{name: "invalid endpoint", clientName: mcpadapter.ClientInvestigator, httpClient: httpClient, server: MCPServer{Name: "observability", Endpoint: "ftp://mcp", Tools: []string{"alpha"}}, want: "connect observability MCP: invalid MCP endpoint"},
		{name: "unreachable server", clientName: mcpadapter.ClientInvestigator, httpClient: httpClient, server: MCPServer{Name: "rollback kubernetes", Endpoint: "http://127.0.0.1:1/mcp", Tools: []string{"alpha"}}, want: "connect rollback kubernetes MCP: connect MCP client"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, err := ConnectMCP(ctx, test.clientName, test.httpClient, test.server)
			if err == nil {
				if closeErr := client.Close(); closeErr != nil {
					t.Errorf("close MCP client: %v", closeErr)
				}
				t.Fatal("ConnectMCP() succeeded, want an error")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ConnectMCP() error = %q, want %q", err, test.want)
			}
		})
	}
}

type recordingObserver struct {
	mu    sync.Mutex
	calls []string
}

func (o *recordingObserver) ObserveToolCall(ctx context.Context, server, tool string) (context.Context, func(string)) {
	return ctx, func(outcome string) {
		o.mu.Lock()
		defer o.mu.Unlock()
		o.calls = append(o.calls, server+"/"+tool+"/"+outcome)
	}
}

func TestObservedCallsBoundsAndReportsEachCall(t *testing.T) {
	endpoint, _ := newMCPServer(t, mcpadapter.ToolGetDeployment)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := ConnectMCP(ctx, mcpadapter.ClientInvestigator, &http.Client{Timeout: 5 * time.Second}, MCPServer{Name: "test", Endpoint: endpoint, Tools: []string{mcpadapter.ToolGetDeployment}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Errorf("close MCP client: %v", err)
		}
	}()
	observer := &recordingObserver{}

	caller, err := ObservedCalls(client, time.Second, mcpadapter.ServerKubernetesReadonly, observer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := caller.CallTool(ctx, &mcpsdk.CallToolParams{Name: mcpadapter.ToolGetDeployment, Arguments: pingInput{}}); err != nil {
		t.Fatal(err)
	}

	observer.mu.Lock()
	defer observer.mu.Unlock()
	if len(observer.calls) != 1 || !strings.HasPrefix(observer.calls[0], mcpadapter.ServerKubernetesReadonly+"/"+mcpadapter.ToolGetDeployment+"/") {
		t.Fatalf("observed calls = %v, want one %s call", observer.calls, mcpadapter.ToolGetDeployment)
	}
}

func TestObservedCallsRejectsInvalidInput(t *testing.T) {
	endpoint, _ := newMCPServer(t, "alpha")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := ConnectMCP(ctx, mcpadapter.ClientOrchestrator, &http.Client{Timeout: 5 * time.Second}, MCPServer{Name: "test", Endpoint: endpoint, Tools: []string{"alpha"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Errorf("close MCP client: %v", err)
		}
	}()
	observer := &recordingObserver{}
	if _, err := ObservedCalls(nil, time.Second, mcpadapter.ServerObservability, observer); err == nil {
		t.Error("nil client accepted")
	}
	if _, err := ObservedCalls(client, 0, mcpadapter.ServerObservability, observer); err == nil {
		t.Error("zero timeout accepted")
	}
	if _, err := ObservedCalls(client, time.Second, "", observer); err == nil {
		t.Error("empty server label accepted")
	}
	if _, err := ObservedCalls(client, time.Second, mcpadapter.ServerObservability, nil); err == nil {
		t.Error("nil observer accepted")
	}
}
