package httpserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHealthResponses(t *testing.T) {
	tests := []struct {
		name       string
		check      ReadyCheck
		path       string
		wantStatus int
		wantBody   string
	}{
		{name: "live ignores a failing readiness check", check: func(context.Context) error { return errors.New("down") }, path: LivePath, wantStatus: http.StatusOK, wantBody: `{"status":"ok"}`},
		{name: "ready without a check", path: ReadyPath, wantStatus: http.StatusOK, wantBody: `{"status":"ok"}`},
		{name: "ready with a passing check", check: func(context.Context) error { return nil }, path: ReadyPath, wantStatus: http.StatusOK, wantBody: `{"status":"ok"}`},
		{name: "not ready hides the cause", check: func(context.Context) error { return errors.New("dial postgres://user:secret@db failed") }, path: ReadyPath, wantStatus: http.StatusServiceUnavailable, wantBody: `{"status":"not_ready"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mux := http.NewServeMux()
			NewHealth(test.check, time.Second, nil).Register(mux)
			response := httptest.NewRecorder()

			mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if got := response.Body.String(); got != test.wantBody {
				t.Fatalf("body = %q, want %q", got, test.wantBody)
			}
			if got := response.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q", got)
			}
		})
	}
}

func TestHealthRoutesAcceptOnlyGET(t *testing.T) {
	mux := http.NewServeMux()
	NewHealth(nil, time.Second, nil).Register(mux)
	for _, path := range []string{LivePath, ReadyPath} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))
		if response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s status = %d, want 405", path, response.Code)
		}
	}
}

func TestReadyBoundsTheCheck(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
		want    time.Duration
	}{
		{name: "configured timeout", timeout: 750 * time.Millisecond, want: 750 * time.Millisecond},
		{name: "zero falls back to the default", timeout: 0, want: DefaultReadyTimeout},
		{name: "negative falls back to the default", timeout: -time.Second, want: DefaultReadyTimeout},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var remaining time.Duration
			health := NewHealth(func(ctx context.Context) error {
				deadline, ok := ctx.Deadline()
				if !ok {
					t.Error("readiness check context has no deadline")
				}
				remaining = time.Until(deadline)
				return nil
			}, test.timeout, nil)
			response := httptest.NewRecorder()

			health.Ready(response, httptest.NewRequest(http.MethodGet, ReadyPath, nil))

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d", response.Code)
			}
			if remaining <= test.want-200*time.Millisecond || remaining > test.want {
				t.Fatalf("check deadline in %s, want about %s", remaining, test.want)
			}
		})
	}
}

func TestReadyReportsNotReadyWhenTheCheckOutlivesItsTimeout(t *testing.T) {
	health := NewHealth(func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}, 20*time.Millisecond, nil)
	response := httptest.NewRecorder()
	started := time.Now()

	health.Ready(response, httptest.NewRequest(http.MethodGet, ReadyPath, nil))

	if response.Code != http.StatusServiceUnavailable || response.Body.String() != HealthNotReadyBody {
		t.Fatalf("status = %d body = %q", response.Code, response.Body.String())
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("readiness took %s, want it bounded by the timeout", elapsed)
	}
}
