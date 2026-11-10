package httpserver_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/fake"
	"github.com/belLena81/rollback-guard/internal/adapters/memory"
	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/httpserver"
	"github.com/belLena81/rollback-guard/internal/ports"
)

var authStart = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

const authTTL = 8 * time.Hour

type authClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *authClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *authClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type authIDs struct {
	mu sync.Mutex
	n  int
}

func (g *authIDs) NewID(prefix string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return prefix + "-" + strings.Repeat("i", g.n)
}

// seen records what the protected handler observed.
type seen struct {
	called        bool
	actor         domain.Actor
	actorOK       bool
	principal     domain.Principal
	principalOK   bool
	authorization string
	cookie        string
}

type authHarness struct {
	store   *memory.AuthUnitOfWork
	clock   *authClock
	service *application.SessionService
	user    domain.User
	logs    *bytes.Buffer
	handler http.Handler
	last    *seen
}

func newAuthHarness(t *testing.T) *authHarness {
	t.Helper()
	h := &authHarness{store: memory.NewAuthUnitOfWork(), clock: &authClock{now: authStart}, logs: &bytes.Buffer{}, last: &seen{}}
	service, err := application.NewSessionService(application.SessionDependencies{
		UnitOfWork: h.store, Clock: h.clock, IDGenerator: &authIDs{}, Random: &fake.RandomSource{},
	}, authTTL)
	if err != nil {
		t.Fatal(err)
	}
	h.service = service
	user, err := domain.NewUser("user-1", 4242, "octocat", "octo@example.com", []string{"acme"}, domain.RoleOperator, authStart)
	if err != nil {
		t.Fatal(err)
	}
	h.user = user
	h.mustTx(t, func(repos ports.AuthRepositories) error { return repos.Users().Create(user) })
	h.handler = h.wrap(t, service)
	return h
}

func (h *authHarness) wrap(t *testing.T, resolver httpserver.SessionResolver) http.Handler {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	middleware, err := httpserver.NewSessionAuthMiddleware(resolver, logger)
	if err != nil {
		t.Fatal(err)
	}
	var auth httpserver.AuthMiddleware = middleware
	return auth.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.last.called = true
		h.last.actor, h.last.actorOK = httpserver.ActorFromContext(r.Context())
		h.last.principal, h.last.principalOK = httpserver.PrincipalFromContext(r.Context())
		h.last.authorization = r.Header.Get("Authorization")
		h.last.cookie = r.Header.Get("Cookie")
		w.WriteHeader(http.StatusOK)
	}))
}

func (h *authHarness) mustTx(t *testing.T, fn func(ports.AuthRepositories) error) {
	t.Helper()
	if err := h.store.WithinAuthTransaction(t.Context(), fn); err != nil {
		t.Fatal(err)
	}
}

func (h *authHarness) issue(t *testing.T) (string, domain.Session) {
	t.Helper()
	raw, session, err := h.service.Issue(t.Context(), h.user)
	if err != nil {
		t.Fatal(err)
	}
	return raw, session
}

func (h *authHarness) updateUser(t *testing.T, change func(domain.User) (domain.User, error)) {
	t.Helper()
	h.mustTx(t, func(repos ports.AuthRepositories) error {
		current, err := repos.Users().GetByID(h.user.ID())
		if err != nil {
			return err
		}
		next, err := change(current)
		if err != nil {
			return err
		}
		return repos.Users().Update(next, current.Version())
	})
}

type requestOption func(*http.Request)

func bearer(token string) requestOption {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

func cookie(token string) requestOption {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: httpserver.SessionCookieName, Value: token}) }
}

func header(name, value string) requestOption {
	return func(r *http.Request) { r.Header.Add(name, value) }
}

func (h *authHarness) do(method, path string, options ...requestOption) *httptest.ResponseRecorder {
	*h.last = seen{}
	request := httptest.NewRequest(method, "http://guard.example"+path, nil)
	for _, option := range options {
		option(request)
	}
	recorder := httptest.NewRecorder()
	h.handler.ServeHTTP(recorder, request)
	return recorder
}

func assertUnauthenticated(t *testing.T, h *authHarness, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401: %s", response.Code, response.Body)
	}
	if got := response.Header().Get("WWW-Authenticate"); got != "Bearer" {
		t.Fatalf("WWW-Authenticate %q", got)
	}
	if h.last.called {
		t.Fatal("protected handler ran for an unauthenticated request")
	}
}

func TestSessionAuthMiddlewareFlow(t *testing.T) {
	h := newAuthHarness(t)
	raw, session := h.issue(t)

	response := h.do(http.MethodGet, "/v1/incidents/inc-1", bearer(raw))
	if response.Code != http.StatusOK || !h.last.actorOK || !h.last.principalOK {
		t.Fatalf("valid session rejected: %d %s", response.Code, response.Body)
	}
	if h.last.actor.ID() != "github:4242" || !h.last.actor.IsHuman() || h.last.principal.SessionID() != session.ID() {
		t.Fatalf("unexpected identity %s %v", h.last.actor.ID(), h.last.principal)
	}
	if h.last.principal.User().Role() != domain.RoleOperator {
		t.Fatalf("role %s", h.last.principal.User().Role())
	}

	t.Run("role change is visible on the next request", func(t *testing.T) {
		h.updateUser(t, func(u domain.User) (domain.User, error) {
			return u.WithRole(domain.RoleAdministrator, authStart.Add(time.Minute))
		})
		if response := h.do(http.MethodGet, "/v1/incidents/inc-1", bearer(raw)); response.Code != http.StatusOK {
			t.Fatalf("status %d", response.Code)
		}
		if h.last.principal.User().Role() != domain.RoleAdministrator {
			t.Fatalf("stale role %s", h.last.principal.User().Role())
		}
	})

	t.Run("revocation applies to the next request", func(t *testing.T) {
		if err := h.service.Revoke(t.Context(), session.ID()); err != nil {
			t.Fatal(err)
		}
		assertUnauthenticated(t, h, h.do(http.MethodGet, "/v1/incidents/inc-1", bearer(raw)))
	})
}

func TestSessionAuthMiddlewareRejectsDisabledUserImmediately(t *testing.T) {
	h := newAuthHarness(t)
	raw, _ := h.issue(t)
	if response := h.do(http.MethodGet, "/v1/incidents/inc-1", cookie(raw)); response.Code != http.StatusOK {
		t.Fatalf("status %d", response.Code)
	}
	h.updateUser(t, func(u domain.User) (domain.User, error) { return u.Disable(authStart.Add(time.Minute)) })
	assertUnauthenticated(t, h, h.do(http.MethodGet, "/v1/incidents/inc-1", cookie(raw)))
}

func TestSessionAuthMiddlewareRejectsExpiredSession(t *testing.T) {
	h := newAuthHarness(t)
	raw, _ := h.issue(t)
	h.clock.Advance(authTTL)
	assertUnauthenticated(t, h, h.do(http.MethodGet, "/v1/incidents/inc-1", bearer(raw)))
}

func TestSessionAuthMiddlewareFailuresAreIdentical(t *testing.T) {
	h := newAuthHarness(t)
	expired, _ := h.issue(t)
	revoked, revokedSession := h.issue(t)
	if err := h.service.Revoke(t.Context(), revokedSession.ID()); err != nil {
		t.Fatal(err)
	}
	unknown := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte("u"), application.SessionTokenBytes))
	h.clock.Advance(authTTL)

	tests := []struct {
		name    string
		token   string
		options []requestOption
	}{
		{name: "no credentials"},
		{name: "empty bearer", options: []requestOption{header("Authorization", "Bearer ")}},
		{name: "empty authorization", options: []requestOption{header("Authorization", "")}},
		{name: "basic scheme", token: unknown, options: []requestOption{header("Authorization", "Basic "+unknown)}},
		{name: "two authorization headers", token: unknown, options: []requestOption{bearer(unknown), header("Authorization", "Bearer "+unknown)}},
		{name: "malformed bearer", token: "not-a-token", options: []requestOption{bearer("not-a-token")}},
		{name: "unknown bearer", token: unknown, options: []requestOption{bearer(unknown)}},
		{name: "unknown cookie", token: unknown, options: []requestOption{cookie(unknown)}},
		{name: "empty cookie", options: []requestOption{cookie("")}},
		{name: "duplicate cookies", token: unknown, options: []requestOption{cookie(unknown), cookie(unknown)}},
		{name: "expired", token: expired, options: []requestOption{bearer(expired)}},
		{name: "revoked", token: revoked, options: []requestOption{cookie(revoked)}},
	}
	var reference []byte
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := h.do(http.MethodGet, "/v1/incidents/inc-1", tt.options...)
			assertUnauthenticated(t, h, response)
			body := response.Body.Bytes()
			if tt.token != "" && bytes.Contains(body, []byte(tt.token)) {
				t.Fatal("response echoes the token")
			}
			if reference == nil {
				reference = body
				if string(body) != `{"error":"unauthenticated","message":"authentication required"}`+"\n" {
					t.Fatalf("unexpected body %s", body)
				}
				return
			}
			if !bytes.Equal(body, reference) {
				t.Fatalf("body differs:\n%s\n%s", body, reference)
			}
		})
	}
}

func TestSessionAuthMiddlewareCarriesRequestID(t *testing.T) {
	h := newAuthHarness(t)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "req-7")
		h.handler.ServeHTTP(w, r)
	})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/incidents/inc-1", nil))
	if want := `{"error":"unauthenticated","message":"authentication required","requestId":"req-7"}` + "\n"; recorder.Body.String() != want {
		t.Fatalf("body %s", recorder.Body)
	}
}

func TestSessionAuthMiddlewareBearerTakesPrecedenceOverCookie(t *testing.T) {
	h := newAuthHarness(t)
	valid, session := h.issue(t)
	other, _ := h.issue(t)
	unknown := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte("u"), application.SessionTokenBytes))

	if response := h.do(http.MethodGet, "/v1/x", bearer(valid), cookie(unknown)); response.Code != http.StatusOK {
		t.Fatalf("valid bearer with bad cookie: %d", response.Code)
	}
	if h.last.principal.SessionID() != session.ID() {
		t.Fatal("cookie session used although a bearer token was present")
	}
	if response := h.do(http.MethodGet, "/v1/x", bearer(valid), cookie(other)); response.Code != http.StatusOK || h.last.principal.SessionID() != session.ID() {
		t.Fatalf("bearer did not win over a valid cookie: %d", response.Code)
	}
	assertUnauthenticated(t, h, h.do(http.MethodGet, "/v1/x", bearer(unknown), cookie(valid)))
	assertUnauthenticated(t, h, h.do(http.MethodGet, "/v1/x", header("Authorization", "Basic abc"), cookie(valid)))
	if response := h.do(http.MethodGet, "/v1/x", cookie(valid)); response.Code != http.StatusOK {
		t.Fatalf("cookie alone: %d", response.Code)
	}
}

func TestSessionAuthMiddlewareHidesCredentialsFromHandlers(t *testing.T) {
	h := newAuthHarness(t)
	raw, _ := h.issue(t)
	if response := h.do(http.MethodGet, "/v1/x", bearer(raw), cookie(raw), header("Cookie", "theme=dark")); response.Code != http.StatusOK {
		t.Fatalf("status %d", response.Code)
	}
	if h.last.authorization != "" || strings.Contains(h.last.cookie, raw) || strings.Contains(h.last.cookie, httpserver.SessionCookieName) {
		t.Fatalf("handler saw credentials: %q %q", h.last.authorization, h.last.cookie)
	}
	if !strings.Contains(h.last.cookie, "theme=dark") {
		t.Fatalf("unrelated cookie dropped: %q", h.last.cookie)
	}
}

type countingResolver struct {
	calls int
	err   error
}

func (r *countingResolver) Resolve(context.Context, string) (domain.Principal, error) {
	r.calls++
	if r.err != nil {
		return domain.Principal{}, r.err
	}
	return domain.Principal{}, &domain.UnauthenticatedError{}
}

func TestSessionAuthMiddlewarePublicPathsAreExactMatch(t *testing.T) {
	tests := []struct {
		path   string
		public bool
	}{
		{path: "/health/live", public: true},
		{path: "/health/ready", public: true},
		{path: "/metrics", public: true},
		{path: "/v1/auth/github/login", public: true},
		{path: "/v1/auth/github/callback", public: true},
		{path: "/health/live/"},
		{path: "/health/ready/x"},
		{path: "/health"},
		{path: "/metrics/x"},
		{path: "/metrics/"},
		{path: "/METRICS"},
		{path: "/v1/auth/github/login/"},
		{path: "/v1/auth/github/callbackx"},
		{path: "/v1/auth/github"},
		{path: "/v1/auth/logout"},
		{path: "/v1/me"},
		{path: "/v1/incidents"},
		{path: "/"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			h := newAuthHarness(t)
			resolver := &countingResolver{}
			h.handler = h.wrap(t, resolver)
			response := h.do(http.MethodGet, tt.path)
			if httpserver.IsPublicPath(tt.path) != tt.public {
				t.Fatalf("IsPublicPath(%q) != %v", tt.path, tt.public)
			}
			if tt.public {
				if response.Code != http.StatusOK || !h.last.called || h.last.actorOK || resolver.calls != 0 {
					t.Fatalf("public path: status %d called %v actor %v resolver %d", response.Code, h.last.called, h.last.actorOK, resolver.calls)
				}
				return
			}
			assertUnauthenticated(t, h, response)
		})
	}
}

func TestSessionAuthMiddlewareCSRF(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		carrier string
		headers map[string]string
		want    int
	}{
		{name: "cookie post without origin or referer", method: http.MethodPost, carrier: "cookie", want: http.StatusOK},
		{name: "cookie post same origin", method: http.MethodPost, carrier: "cookie", headers: map[string]string{"Origin": "https://guard.example"}, want: http.StatusOK},
		{name: "cookie post same origin other case", method: http.MethodPost, carrier: "cookie", headers: map[string]string{"Origin": "https://GUARD.example"}, want: http.StatusOK},
		{name: "cookie post cross origin", method: http.MethodPost, carrier: "cookie", headers: map[string]string{"Origin": "https://evil.example"}, want: http.StatusForbidden},
		{name: "cookie post other port", method: http.MethodPost, carrier: "cookie", headers: map[string]string{"Origin": "https://guard.example:8443"}, want: http.StatusForbidden},
		{name: "cookie post null origin", method: http.MethodPost, carrier: "cookie", headers: map[string]string{"Origin": "null"}, want: http.StatusForbidden},
		{name: "cookie post cross origin with same-host referer", method: http.MethodPost, carrier: "cookie", headers: map[string]string{"Origin": "https://evil.example", "Referer": "https://guard.example/ui"}, want: http.StatusForbidden},
		{name: "cookie post same-host referer", method: http.MethodPost, carrier: "cookie", headers: map[string]string{"Referer": "https://guard.example/ui"}, want: http.StatusOK},
		{name: "cookie post cross-site referer", method: http.MethodPost, carrier: "cookie", headers: map[string]string{"Referer": "https://evil.example/page"}, want: http.StatusForbidden},
		{name: "cookie post suffix host referer", method: http.MethodPost, carrier: "cookie", headers: map[string]string{"Referer": "https://guard.example.evil.example/"}, want: http.StatusForbidden},
		{name: "cookie put cross origin", method: http.MethodPut, carrier: "cookie", headers: map[string]string{"Origin": "https://evil.example"}, want: http.StatusForbidden},
		{name: "cookie patch cross origin", method: http.MethodPatch, carrier: "cookie", headers: map[string]string{"Origin": "https://evil.example"}, want: http.StatusForbidden},
		{name: "cookie delete cross origin", method: http.MethodDelete, carrier: "cookie", headers: map[string]string{"Origin": "https://evil.example"}, want: http.StatusForbidden},
		{name: "cookie get cross origin", method: http.MethodGet, carrier: "cookie", headers: map[string]string{"Origin": "https://evil.example"}, want: http.StatusOK},
		{name: "bearer post cross origin", method: http.MethodPost, carrier: "bearer", headers: map[string]string{"Origin": "https://evil.example"}, want: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newAuthHarness(t)
			raw, _ := h.issue(t)
			options := []requestOption{cookie(raw)}
			if tt.carrier == "bearer" {
				options = []requestOption{bearer(raw)}
			}
			for name, value := range tt.headers {
				options = append(options, header(name, value))
			}
			response := h.do(tt.method, "/v1/incidents", options...)
			if response.Code != tt.want {
				t.Fatalf("status %d, want %d: %s", response.Code, tt.want, response.Body)
			}
			if tt.want == http.StatusForbidden {
				if h.last.called || response.Body.String() != `{"error":"forbidden","message":"cross-site request rejected"}`+"\n" {
					t.Fatalf("forbidden response: called %v body %s", h.last.called, response.Body)
				}
			}
		})
	}
}

func TestSessionAuthMiddlewareInfrastructureErrorIsNotUnauthenticated(t *testing.T) {
	h := newAuthHarness(t)
	resolver := &countingResolver{err: errors.New("dial tcp 10.0.0.5:5432: connection refused password=hunter2")}
	h.handler = h.wrap(t, resolver)
	unknown := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte("u"), application.SessionTokenBytes))
	response := h.do(http.MethodGet, "/v1/incidents", bearer(unknown))
	if response.Code != http.StatusInternalServerError || h.last.called {
		t.Fatalf("status %d called %v", response.Code, h.last.called)
	}
	if response.Header().Get("WWW-Authenticate") != "" {
		t.Fatal("infrastructure failure must not ask the client to authenticate")
	}
	if want := `{"error":"internal_error","message":"internal server error"}` + "\n"; response.Body.String() != want {
		t.Fatalf("body %s", response.Body)
	}
	if strings.Contains(h.logs.String(), "hunter2") || strings.Contains(h.logs.String(), "10.0.0.5") {
		t.Fatalf("infrastructure detail logged: %s", h.logs)
	}
}

func TestSessionAuthMiddlewareNeverLogsCredentials(t *testing.T) {
	h := newAuthHarness(t)
	raw, session := h.issue(t)
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(decoded)
	unknown := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte("u"), application.SessionTokenBytes))

	h.do(http.MethodGet, "/v1/incidents", bearer(raw))
	h.do(http.MethodPost, "/v1/incidents", cookie(raw), header("Origin", "https://evil.example"))
	h.do(http.MethodGet, "/v1/incidents", bearer(unknown))
	h.do(http.MethodGet, "/v1/incidents", cookie(unknown))
	h.do(http.MethodGet, "/v1/incidents", header("Authorization", "Basic "+unknown))
	if err := h.service.Revoke(t.Context(), session.ID()); err != nil {
		t.Fatal(err)
	}
	h.do(http.MethodGet, "/v1/incidents", bearer(raw))

	logs := h.logs.String()
	if !strings.Contains(logs, "request unauthenticated") || !strings.Contains(logs, "cross-site request rejected") {
		t.Fatalf("expected the failures to be logged: %s", logs)
	}
	for _, secret := range []string{
		raw, unknown,
		hex.EncodeToString(digest[:]),
		base64.StdEncoding.EncodeToString(digest[:]),
		base64.RawURLEncoding.EncodeToString(digest[:]),
		"Authorization", "authorization", "Bearer", "Basic", httpserver.SessionCookieName,
	} {
		if strings.Contains(logs, secret) {
			t.Fatalf("log output contains %q:\n%s", secret, logs)
		}
	}
}

func TestNewSessionAuthMiddlewareValidatesDependencies(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := httpserver.NewSessionAuthMiddleware(nil, logger); err == nil {
		t.Fatal("nil resolver accepted")
	}
	if _, err := httpserver.NewSessionAuthMiddleware(&countingResolver{}, nil); err == nil {
		t.Fatal("nil logger accepted")
	}
}

func TestSessionCookies(t *testing.T) {
	expires := time.Date(2026, 11, 10, 17, 0, 0, 0, time.UTC)
	for _, secure := range []bool{true, false} {
		c := httpserver.NewSessionCookie("tok", expires, secure)
		if c.Name != httpserver.SessionCookieName || c.Value != "tok" || c.Path != "/" || !c.HttpOnly ||
			c.SameSite != http.SameSiteStrictMode || c.Secure != secure || !c.Expires.Equal(expires) {
			t.Fatalf("session cookie %+v", c)
		}
		if err := c.Valid(); err != nil {
			t.Fatal(err)
		}
		cleared := httpserver.ClearSessionCookie(secure)
		if cleared.Name != httpserver.SessionCookieName || cleared.Value != "" || cleared.Path != "/" || cleared.MaxAge >= 0 ||
			!cleared.HttpOnly || cleared.SameSite != http.SameSiteStrictMode || cleared.Secure != secure {
			t.Fatalf("clear cookie %+v", cleared)
		}
		header := cleared.String()
		if !strings.Contains(header, "Max-Age=0") {
			t.Fatalf("clear cookie header %q", header)
		}
	}
}
