package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/platform/resilience"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The opposite case: the server received the call and then dropped the
// connection without answering. That must never look like "not sent", or the
// retry rule could repeat a request the server already acted on.
func TestDroppedConnectionAfterSendIsNotRequestNotSent(t *testing.T) {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "drop-test", Version: "v0"}, nil)
	mcpsdk.AddTool(server, &mcpsdk.Tool{Name: ToolGetDeployment, Description: "test"}, func(context.Context, *mcpsdk.CallToolRequest, struct{}) (*mcpsdk.CallToolResult, struct{}, error) {
		return nil, struct{}{}, nil
	})
	var drop atomic.Bool
	inner := HTTPHandler(server, nil, 0, time.Minute, nil)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if drop.Load() && r.Method == http.MethodPost {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("test server cannot hijack")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Error("hijack:", err)
				return
			}
			// The request was read; no response is ever written.
			if err := conn.Close(); err != nil {
				t.Log("close hijacked connection:", err)
			}
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)
	client, err := Connect(context.Background(), httpServer.URL+"/mcp", &http.Client{Timeout: 5 * time.Second}, ClientOrchestrator)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Log("close client:", err)
		}
	})
	drop.Store(true)

	_, err = callTool[struct{}, struct{}](context.Background(), client, ToolGetDeployment, struct{}{})
	if err == nil {
		t.Fatal("dropped call succeeded")
	}
	t.Logf("error chain: %v", err)
	if resilience.RequestNotSent(err) {
		t.Fatalf("RequestNotSent(%v) = true for a request the server already received", err)
	}
}

// The rollback retry rule relies on a refused connection still being
// recognizable after it travels through the MCP SDK and callTool.
func TestRefusedConnectionIsRecognizedAsRequestNotSent(t *testing.T) {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "dial-test", Version: "v0"}, nil)
	mcpsdk.AddTool(server, &mcpsdk.Tool{Name: ToolGetDeployment, Description: "test"}, func(context.Context, *mcpsdk.CallToolRequest, struct{}) (*mcpsdk.CallToolResult, struct{}, error) {
		return nil, struct{}{}, nil
	})
	httpServer := httptest.NewServer(HTTPHandler(server, nil, 0, time.Minute, nil))
	client, err := Connect(context.Background(), httpServer.URL+"/mcp", &http.Client{Timeout: 5 * time.Second}, ClientOrchestrator)
	if err != nil {
		httpServer.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Log("close client:", err)
		}
	})
	httpServer.Close() // nothing listens on the endpoint any more

	_, err = callTool[struct{}, struct{}](context.Background(), client, ToolGetDeployment, struct{}{})
	if err == nil {
		t.Fatal("call to a closed server succeeded")
	}
	t.Logf("error chain: %T %v", err, err)
	if !resilience.RequestNotSent(err) {
		t.Fatalf("RequestNotSent(%v) = false; the retry rule would never fire against a real client", err)
	}
}
