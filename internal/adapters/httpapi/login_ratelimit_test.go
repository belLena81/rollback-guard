package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/platform/httpserver"
)

func limitedHarness(t *testing.T, burst int) *loginHarness {
	t.Helper()
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	limiter, err := httpserver.NewRateLimiter(httpserver.RateLimitConfig{
		PerMinute: 6, Burst: burst, MaxClients: 10, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return newLoginHarness(t, false, time.Second, WithLoginRateLimit(limiter))
}

func fromAddr(method, path, remote string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = remote
	return r
}

func TestLoginRoutesAreRateLimitedWith429Shape(t *testing.T) {
	h := limitedHarness(t, 2)
	// The two routes share one bucket per client address.
	for i, path := range []string{httpserver.GitHubLoginPath, httpserver.GitHubCallbackPath} {
		if got := h.serve(fromAddr(http.MethodGet, path, "192.0.2.1:1000")); got.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d limited too early", i)
		}
	}
	causes := []*http.Request{
		fromAddr(http.MethodGet, httpserver.GitHubLoginPath, "192.0.2.1:1001"),
		fromAddr(http.MethodGet, httpserver.GitHubCallbackPath+"?code=x&state=y", "192.0.2.1:1002"),
	}
	causes[0].Header.Set("X-Forwarded-For", "203.0.113.9") // must not change the key
	var bodies []string
	for _, r := range causes {
		response := h.serve(r)
		if response.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429", response.Code)
		}
		if response.Header().Get("Retry-After") != "10" {
			t.Fatalf("Retry-After = %q, want 10", response.Header().Get("Retry-After"))
		}
		var body errorDTO
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Error != codeRateLimited || body.Message != messageRateLimited {
			t.Fatalf("body = %+v", body)
		}
		bodies = append(bodies, strings.TrimSpace(response.Body.String()))
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("429 bodies differ by cause: %q vs %q", bodies[0], bodies[1])
	}
	// Another client address is unaffected.
	if got := h.serve(fromAddr(http.MethodGet, httpserver.GitHubLoginPath, "192.0.2.2:1000")); got.Code != http.StatusFound {
		t.Fatalf("other client status = %d", got.Code)
	}
}

func TestRateLimitDoesNotTouchOtherPaths(t *testing.T) {
	h := limitedHarness(t, 1)
	h.serve(fromAddr(http.MethodGet, httpserver.GitHubLoginPath, "192.0.2.1:1"))
	if got := h.serve(fromAddr(http.MethodGet, httpserver.GitHubLoginPath, "192.0.2.1:1")); got.Code != http.StatusTooManyRequests {
		t.Fatalf("login not limited: %d", got.Code)
	}
	for _, path := range []string{"/health/live", "/metrics", "/v1/incidents/x"} {
		for range 5 {
			if got := h.serve(fromAddr(http.MethodGet, path, "192.0.2.1:1")); got.Code == http.StatusTooManyRequests {
				t.Fatalf("%s was rate limited", path)
			}
		}
	}
}

func TestRateLimitedResponseLogsNoSecretOrCredential(t *testing.T) {
	h := limitedHarness(t, 1)
	h.serve(fromAddr(http.MethodGet, httpserver.GitHubLoginPath, "192.0.2.1:1"))
	r := fromAddr(http.MethodGet, httpserver.GitHubCallbackPath+"?code=SECRETCODE&state=SECRETSTATE", "192.0.2.1:1")
	r.Header.Set("Authorization", "Bearer SECRETTOKEN")
	response := h.serve(r)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d", response.Code)
	}
	for _, secret := range append(h.secrets, "SECRETCODE", "SECRETSTATE", "SECRETTOKEN") {
		if strings.Contains(h.logs.String(), secret) || strings.Contains(response.Body.String(), secret) {
			t.Fatalf("secret %q leaked", secret)
		}
	}
}
