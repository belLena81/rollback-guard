package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/fake"
	"github.com/belLena81/rollback-guard/internal/adapters/githuboauth"
	"github.com/belLena81/rollback-guard/internal/adapters/githuboauth/githubfake"
	"github.com/belLena81/rollback-guard/internal/adapters/memory"
	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/httpserver"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/belLena81/rollback-guard/internal/testkit"
)

const (
	loginClientID     = "test-client-id"
	loginClientSecret = "test-client-secret-never-logged"
	loginRedirectURL  = "http://127.0.0.1:8080/v1/auth/github/callback"
	loginOrg          = "acme"
)

type loginHarness struct {
	github   *githubfake.Server
	store    *memory.AuthUnitOfWork
	clock    *testkit.Clock
	handler  http.Handler
	logs     *bytes.Buffer
	sessions *application.SessionService
	// secrets collects every sensitive value seen, for log assertions.
	secrets []string
}

func newLoginHarness(t *testing.T, cookieSecure bool, timeout time.Duration, extra ...Option) *loginHarness {
	t.Helper()
	h := &loginHarness{
		github: githubfake.New(loginClientID, loginClientSecret),
		store:  memory.NewAuthUnitOfWork(),
		clock:  testkit.NewClock(time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)),
		logs:   &bytes.Buffer{},
	}
	t.Cleanup(h.github.Close)
	h.github.AddUser(githubfake.User{ID: 101, Login: "member", Email: "member@example.com", Organizations: []string{"ACME"}})
	h.github.AddUser(githubfake.User{ID: 102, Login: "outsider", Organizations: []string{"acme-evil"}})
	provider, err := githuboauth.New(githuboauth.Config{
		ClientID: loginClientID, ClientSecret: loginClientSecret, RedirectURL: loginRedirectURL,
		BaseURL: h.github.URL(), APIURL: h.github.URL(), Timeout: timeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := &testkit.IDGenerator{}
	random := &fake.RandomSource{}
	h.sessions, err = application.NewSessionService(application.SessionDependencies{UnitOfWork: h.store, Clock: h.clock, IDGenerator: ids, Random: random}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	stateKey := strings.Repeat("s", application.MinLoginStateKeyBytes)
	login, err := application.NewLoginService(application.LoginDependencies{
		Sessions: h.sessions, UnitOfWork: h.store, Clock: h.clock, IDGenerator: ids, Random: random, Provider: provider,
	}, application.LoginPolicy{AllowedOrganization: loginOrg, BootstrapAdmins: []domain.BootstrapAdmin{{Login: "root", GitHubID: 1}}, StateKey: []byte(stateKey)})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	server, err := NewServer(*newTestServer(t, nil).services, logger, append([]Option{WithGitHubLogin(login, cookieSecure)}, extra...)...)
	if err != nil {
		t.Fatal(err)
	}
	middleware, err := httpserver.NewSessionAuthMiddleware(h.sessions, logger)
	if err != nil {
		t.Fatal(err)
	}
	h.handler = server.Routes(nil, 0, middleware)
	h.secrets = []string{loginClientSecret, stateKey}
	return h
}

func (h *loginHarness) serve(r *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	h.handler.ServeHTTP(recorder, r)
	return recorder
}

func cookieNamed(response *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}

// begin calls the login endpoint and returns its response and state cookie.
func (h *loginHarness) begin(t *testing.T) (*httptest.ResponseRecorder, *http.Cookie) {
	t.Helper()
	response := h.serve(httptest.NewRequest(http.MethodGet, httpserver.GitHubLoginPath, nil))
	if response.Code != http.StatusFound {
		t.Fatalf("login status = %d body %s", response.Code, response.Body.String())
	}
	cookie := cookieNamed(response, LoginStateCookieName)
	if cookie == nil {
		t.Fatal("login set no state cookie")
	}
	h.secrets = append(h.secrets, cookie.Value)
	return response, cookie
}

// approve follows the Location to the fake GitHub as login and returns the
// callback query GitHub redirects back with.
func (h *loginHarness) approve(t *testing.T, location, login string) url.Values {
	t.Helper()
	h.github.SignInAs(login)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, location, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusFound {
		t.Fatalf("authorize status = %d", response.StatusCode)
	}
	back, err := url.Parse(response.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	h.secrets = append(h.secrets, back.Query().Get("code"), back.Query().Get("state"))
	return back.Query()
}

func (h *loginHarness) callback(query url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, httpserver.GitHubCallbackPath+"?"+query.Encode(), nil)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	return h.serve(request)
}

// signIn runs the whole flow for login and returns the callback response.
func (h *loginHarness) signIn(t *testing.T, login string) *httptest.ResponseRecorder {
	t.Helper()
	response, cookie := h.begin(t)
	query := h.approve(t, response.Header().Get("Location"), login)
	return h.callback(query, cookie)
}

func (h *loginHarness) assertCleanLogs(t *testing.T, extra ...string) {
	t.Helper()
	logs := h.logs.String()
	for _, secret := range append(append(append([]string{}, h.secrets...), extra...), h.github.IssuedTokens()...) {
		if secret != "" && strings.Contains(logs, secret) {
			t.Fatalf("logs contain a secret value %q:\n%s", secret, logs)
		}
	}
	if strings.Contains(logs, githubfake.FailureBodyCanary) {
		t.Fatalf("logs contain the provider body:\n%s", logs)
	}
}

func assertStateCookieCleared(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	cookie := cookieNamed(response, LoginStateCookieName)
	if cookie == nil || cookie.MaxAge >= 0 || cookie.Value != "" || cookie.Path != loginStateCookiePath {
		t.Fatalf("state cookie not cleared: %+v", cookie)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func assertErrorBody(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d; body %s", response.Code, status, response.Body.String())
	}
	var body errorDTO
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error != code {
		t.Fatalf("error body = %s", response.Body.String())
	}
	if cookieNamed(response, httpserver.SessionCookieName) != nil {
		t.Fatal("a session cookie was set on failure")
	}
}

func TestGitHubLoginRedirectsWithStateAndPKCE(t *testing.T) {
	for _, secure := range []bool{false, true} {
		h := newLoginHarness(t, secure, time.Second)
		response, cookie := h.begin(t)
		location, err := url.Parse(response.Header().Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		if location.Scheme+"://"+location.Host+location.Path != h.github.URL()+githubfake.AuthorizePath {
			t.Fatalf("Location = %s", location)
		}
		query := location.Query()
		for key, want := range map[string]string{
			"client_id": loginClientID, "redirect_uri": loginRedirectURL, "scope": "read:org user:email", "code_challenge_method": "S256",
		} {
			if got := query.Get(key); got != want {
				t.Errorf("%s = %q, want %q", key, got, want)
			}
		}
		if len(query.Get("state")) < 22 || len(query.Get("code_challenge")) != 43 {
			t.Fatalf("state %q / challenge %q too short", query.Get("state"), query.Get("code_challenge"))
		}
		if strings.Contains(response.Header().Get("Location"), loginClientSecret) || strings.Contains(cookie.Value, query.Get("state")) {
			t.Fatal("redirect leaks the secret or cookie carries the plain state")
		}
		if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/v1/auth/github" ||
			cookie.MaxAge != int(application.LoginStateTTL/time.Second) || cookie.Secure != secure {
			t.Fatalf("state cookie attributes = %+v", cookie)
		}
		if got := response.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q", got)
		}
	}
}

func TestGitHubCallbackRejectsBadStateWithoutExchange(t *testing.T) {
	tests := map[string]func(h *loginHarness, query url.Values, cookie *http.Cookie) (url.Values, []*http.Cookie){
		"missing cookie": func(_ *loginHarness, q url.Values, _ *http.Cookie) (url.Values, []*http.Cookie) { return q, nil },
		"tampered cookie": func(_ *loginHarness, q url.Values, c *http.Cookie) (url.Values, []*http.Cookie) {
			tampered := *c
			tampered.Value = "A" + c.Value[1:]
			if tampered.Value == c.Value {
				tampered.Value = "B" + c.Value[1:]
			}
			return q, []*http.Cookie{&tampered}
		},
		"duplicate cookie": func(_ *loginHarness, q url.Values, c *http.Cookie) (url.Values, []*http.Cookie) {
			return q, []*http.Cookie{c, c}
		},
		"expired": func(h *loginHarness, q url.Values, c *http.Cookie) (url.Values, []*http.Cookie) {
			h.clock.Advance(application.LoginStateTTL + time.Second)
			return q, []*http.Cookie{c}
		},
		"state mismatch": func(_ *loginHarness, q url.Values, c *http.Cookie) (url.Values, []*http.Cookie) {
			q.Set("state", "x"+q.Get("state"))
			return q, []*http.Cookie{c}
		},
		"repeated state": func(_ *loginHarness, q url.Values, c *http.Cookie) (url.Values, []*http.Cookie) {
			q.Add("state", q.Get("state"))
			return q, []*http.Cookie{c}
		},
		"missing code": func(_ *loginHarness, q url.Values, c *http.Cookie) (url.Values, []*http.Cookie) {
			q.Del("code")
			return q, []*http.Cookie{c}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			h := newLoginHarness(t, false, time.Second)
			response, cookie := h.begin(t)
			query, cookies := mutate(h, h.approve(t, response.Header().Get("Location"), "member"), cookie)
			result := h.callback(query, cookies...)
			assertErrorBody(t, result, http.StatusBadRequest, codeValidation)
			assertStateCookieCleared(t, result)
			if h.github.TokenRequests() != 0 {
				t.Fatal("the code was exchanged despite a bad state")
			}
			h.assertCleanLogs(t)
		})
	}
}

func TestGitHubCallbackRejectsReusedCode(t *testing.T) {
	h := newLoginHarness(t, false, time.Second)
	response, cookie := h.begin(t)
	query := h.approve(t, response.Header().Get("Location"), "member")
	if first := h.callback(query, cookie); first.Code != http.StatusOK {
		t.Fatalf("first callback = %d", first.Code)
	}
	second := h.callback(query, cookie)
	assertErrorBody(t, second, http.StatusBadRequest, codeValidation)
}

func TestGitHubCallbackDeniesNonMemberWithoutRows(t *testing.T) {
	h := newLoginHarness(t, false, time.Second)
	response := h.signIn(t, "outsider")
	assertErrorBody(t, response, http.StatusForbidden, codeForbidden)
	assertStateCookieCleared(t, response)
	err := h.store.WithinAuthTransaction(context.Background(), func(repos ports.AuthRepositories) error {
		_, err := repos.Users().GetByGitHubID(102)
		return err
	})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("non-member user row: %v", err)
	}
	h.assertCleanLogs(t)
}

func TestGitHubCallbackIssuesWorkingSession(t *testing.T) {
	h := newLoginHarness(t, false, time.Second)
	response := h.signIn(t, "member")
	if response.Code != http.StatusOK {
		t.Fatalf("callback = %d body %s", response.Code, response.Body.String())
	}
	assertStateCookieCleared(t, response)
	decoder := json.NewDecoder(bytes.NewReader(response.Body.Bytes()))
	decoder.DisallowUnknownFields()
	var body sessionResponseDTO
	if err := decoder.Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.ActorID != "github:101" || body.Login != "member" || body.Role != "viewer" || body.Token == "" ||
		!body.ExpiresAt.Equal(h.clock.Now().Add(time.Hour)) {
		t.Fatalf("session response = %+v", body)
	}
	sessionCookie := cookieNamed(response, httpserver.SessionCookieName)
	if sessionCookie == nil || sessionCookie.Value != body.Token || !sessionCookie.HttpOnly {
		t.Fatalf("session cookie = %+v", sessionCookie)
	}
	for name, authenticate := range map[string]func(*http.Request){
		"bearer": func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+body.Token) },
		"cookie": func(r *http.Request) { r.AddCookie(sessionCookie) },
	} {
		request := httptest.NewRequest(http.MethodGet, "/v1/incidents/no-such-incident", nil)
		authenticate(request)
		if got := h.serve(request).Code; got != http.StatusNotFound {
			t.Fatalf("%s-authenticated request = %d, want 404 past the middleware", name, got)
		}
	}
	unauthenticated := h.serve(httptest.NewRequest(http.MethodGet, "/v1/incidents/no-such-incident", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous request = %d", unauthenticated.Code)
	}
	h.assertCleanLogs(t, body.Token)
}

func TestGitHubCallbackDeniesDisabledUser(t *testing.T) {
	h := newLoginHarness(t, false, time.Second)
	if response := h.signIn(t, "member"); response.Code != http.StatusOK {
		t.Fatalf("first login = %d", response.Code)
	}
	if err := h.store.WithinAuthTransaction(context.Background(), func(repos ports.AuthRepositories) error {
		user, err := repos.Users().GetByGitHubID(101)
		if err != nil {
			return err
		}
		disabled, err := user.Disable(h.clock.Now())
		if err != nil {
			return err
		}
		return repos.Users().Update(disabled, user.Version())
	}); err != nil {
		t.Fatal(err)
	}
	response := h.signIn(t, "member")
	assertErrorBody(t, response, http.StatusForbidden, codeForbidden)
	assertStateCookieCleared(t, response)
}

func TestGitHubCallbackProviderFailureIsGeneric502(t *testing.T) {
	tests := map[string]struct {
		setup   func(*githubfake.Server)
		timeout time.Duration
	}{
		"token error": {func(f *githubfake.Server) { f.FailToken(http.StatusInternalServerError) }, time.Second},
		"api error":   {func(f *githubfake.Server) { f.FailAPI(http.StatusServiceUnavailable) }, time.Second},
		"timeout":     {nil, 50 * time.Millisecond},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			h := newLoginHarness(t, false, tt.timeout)
			response, cookie := h.begin(t)
			query := h.approve(t, response.Header().Get("Location"), "member")
			if tt.setup != nil {
				tt.setup(h.github)
			} else {
				h.github.SetDelay(time.Second)
			}
			result := h.callback(query, cookie)
			assertErrorBody(t, result, http.StatusBadGateway, codeBadGateway)
			assertStateCookieCleared(t, result)
			if strings.Contains(result.Body.String(), githubfake.FailureBodyCanary) {
				t.Fatal("provider body reached the client")
			}
			h.assertCleanLogs(t)
		})
	}
}

func TestGitHubLoginRoutesAbsentWithoutLogin(t *testing.T) {
	handler := newTestHandler(t)
	for _, path := range []string{httpserver.GitHubLoginPath, httpserver.GitHubCallbackPath} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s without github login = %d, want 404", path, recorder.Code)
		}
	}
}

// A login colliding with a stale login of another account (GitHub rename)
// is a fixed 409, not a 500.
func TestLoginErrorResponseMapsLoginCollision(t *testing.T) {
	status, code, message, cause := loginErrorResponse(fmt.Errorf("create user: %w", fmt.Errorf("user octocat: %w", domain.ErrAlreadyExists)))
	if status != http.StatusConflict || code != codeConflict || cause != "login_conflict" || strings.Contains(message, "octocat") {
		t.Fatalf("collision = %d %s %q %s", status, code, message, cause)
	}
}

func TestLoginErrorResponseIsFixed(t *testing.T) {
	leaky := errors.New("provider said: secret-body")
	for _, err := range []error{
		errors.Join(leaky, ports.ErrIdentityProviderFailed),
		errors.Join(leaky, application.ErrLoginDenied),
		errors.Join(leaky, application.ErrInvalidLoginRequest),
		leaky,
	} {
		_, _, message, _ := loginErrorResponse(err)
		if strings.Contains(message, "secret-body") {
			t.Fatalf("message %q echoes the error", message)
		}
	}
}
