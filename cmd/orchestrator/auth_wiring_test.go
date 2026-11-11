package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/githuboauth/githubfake"
	"github.com/belLena81/rollback-guard/internal/adapters/httpapi"
	"github.com/belLena81/rollback-guard/internal/adapters/mcp/mcptest"
	"github.com/belLena81/rollback-guard/internal/adapters/memory"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/config"
	"github.com/belLena81/rollback-guard/internal/platform/httpserver"
)

const (
	wiringClientSecret = "wiring-client-secret-never-logged"
	wiringStateSecret  = "wiring-state-secret-0123456789abcdef-never-logged"
)

func authHandler(t *testing.T, mutate func(*config.Config)) (http.Handler, *bytes.Buffer) {
	t.Helper()
	read := mcptest.NewServer(t, readonlyTools...)
	write := mcptest.NewServer(t, rollbackTools...)
	observability := mcptest.NewServer(t, observabilityTools...)
	cfg := wiringConfig(t, read, write, observability)
	cfg.Session = config.SessionConfig{TTL: time.Hour, CookieSecure: true}
	cfg.Auth = config.AuthConfig{Mode: config.AuthModeLocal}
	mutate(&cfg)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	tel := testTelemetry(t)
	clients, err := buildMCP(context.Background(), cfg, tel, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clients.close(logger) })
	services, err := buildServices(cfg, memory.NewUnitOfWork(), memory.NewAuthUnitOfWork(), clients, tel)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := buildHTTP(cfg, services, memory.NewAuthUnitOfWork(), clients.ping, tel, logger)
	if err != nil {
		t.Fatal(err)
	}
	return handler, &logs
}

func serveAuth(handler http.Handler, path string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder
}

func TestBuildHTTPLocalModeHasNoLoginRoutes(t *testing.T) {
	handler, logs := authHandler(t, func(*config.Config) {})
	for _, path := range []string{httpserver.GitHubLoginPath, httpserver.GitHubCallbackPath} {
		if got := serveAuth(handler, path).Code; got != http.StatusNotFound {
			t.Fatalf("local mode %s = %d, want 404", path, got)
		}
	}
	if got := serveAuth(handler, "/v1/incidents/x").Code; got != http.StatusNotFound {
		t.Fatalf("local mode incident lookup = %d, want 404 from the application", got)
	}
	if !strings.Contains(logs.String(), `"auth_mode":"local"`) || strings.Contains(logs.String(), "WARN") {
		t.Fatalf("logs = %s", logs.String())
	}
}

func TestBuildHTTPGitHubModeRequiresSessions(t *testing.T) {
	github := githubfake.New("wiring-client", wiringClientSecret)
	t.Cleanup(github.Close)
	handler, logs := authHandler(t, func(cfg *config.Config) {
		cfg.Session.CookieSecure = false
		cfg.Auth = config.AuthConfig{Mode: config.AuthModeGitHub, GitHub: config.GitHubAuthConfig{
			ClientID:            "wiring-client",
			ClientSecret:        config.NewSecret(wiringClientSecret),
			RedirectURL:         "http://127.0.0.1:8080/v1/auth/github/callback",
			AllowedOrganization: "acme",
			BootstrapAdmins:     []domain.BootstrapAdmin{{Login: "root", GitHubID: 1}},
			StateSecret:         config.NewSecret(wiringStateSecret),
			BaseURL:             github.URL(),
			APIURL:              github.URL(),
			HTTPTimeout:         time.Second,
			RateLimit:           config.LoginRateLimit{PerMinute: config.DefaultLoginRateLimitPerMinute, Burst: config.DefaultLoginRateLimitBurst},
		}}
	})

	login := serveAuth(handler, httpserver.GitHubLoginPath)
	if login.Code != http.StatusFound || !strings.HasPrefix(login.Header().Get("Location"), github.URL()+githubfake.AuthorizePath) {
		t.Fatalf("login = %d Location %q", login.Code, login.Header().Get("Location"))
	}
	var stateCookie *http.Cookie
	for _, cookie := range login.Result().Cookies() {
		if cookie.Name == httpapi.LoginStateCookieName {
			stateCookie = cookie
		}
	}
	if stateCookie == nil || stateCookie.Secure {
		t.Fatalf("state cookie = %+v, want one without Secure (SESSION_COOKIE_SECURE=false)", stateCookie)
	}
	if got := serveAuth(handler, "/v1/incidents/x").Code; got != http.StatusUnauthorized {
		t.Fatalf("github mode without a session = %d, want 401", got)
	}
	if got := serveAuth(handler, httpserver.GitHubCallbackPath).Code; got != http.StatusBadRequest {
		t.Fatalf("callback without state = %d, want 400", got)
	}
	if github.TokenRequests() != 0 {
		t.Fatal("a callback without state reached GitHub")
	}
	output := logs.String()
	if !strings.Contains(output, `"auth_mode":"github"`) || !strings.Contains(output, "SESSION_COOKIE_SECURE=false") {
		t.Fatalf("logs = %s", output)
	}
	for _, secret := range []string{wiringClientSecret, wiringStateSecret, stateCookie.Value} {
		if strings.Contains(output, secret) {
			t.Fatalf("logs leak a secret: %s", output)
		}
	}
}
