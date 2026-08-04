package mcp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestHTTPHandlerBoundsRequestsAndSeparatesHealth(t *testing.T) {
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test", Version: "v1"}, nil)
	handler := HTTPHandler(server, func(context.Context) error { return errors.New("down") }, 8, time.Minute, slog.Default())

	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("0123456789"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status = %d", response.Code)
	}

	for _, test := range []struct {
		path string
		want int
	}{{path: "/health/live", want: http.StatusOK}, {path: "/health/ready", want: http.StatusServiceUnavailable}} {
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
		if response.Code != test.want {
			t.Fatalf("GET %s status = %d, want %d", test.path, response.Code, test.want)
		}
	}
}
