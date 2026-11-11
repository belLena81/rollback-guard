//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The e2e stack runs the orchestrator with AUTH_MODE=github against a fake
// GitHub (make services-up DEMO_AUTH=github, docs/phase7-auth.md). Every API
// call below carries a real session token read from the 0600 token file that
// scripts/demo/auth-login.sh wrote. Tokens are never logged or put in
// failure messages.

const (
	sessionViewer        = "viewer"
	sessionOperator      = "operator"
	sessionAdministrator = "administrator"
	anonymous            = ""

	actorOperator      = "github:1002"
	actorAdministrator = "github:1001"
)

var tokenKey = regexp.MustCompile(`^[a-z-]+$`)

// loadTokens reads KEY=TOKEN lines from AUTH_TOKENS_FILE (default
// $DEMO_STATE_DIR/auth-tokens under the repository root).
func loadTokens(t *testing.T, root string) map[string]string {
	t.Helper()
	path := strings.TrimSpace(os.Getenv("AUTH_TOKENS_FILE"))
	if path == "" {
		state := strings.TrimSpace(os.Getenv("DEMO_STATE_DIR"))
		if state == "" {
			state = ".demo"
		}
		if !filepath.IsAbs(state) {
			state = filepath.Join(root, state)
		}
		path = filepath.Join(state, "auth-tokens")
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("no session tokens (%v); start the stack with make services-up DEMO_AUTH=github", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Errorf("close token file: %v", err)
		}
	}()
	tokens := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if key, token, ok := strings.Cut(scanner.Text(), "="); ok && tokenKey.MatchString(key) && token != "" {
			tokens[key] = token
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read token file: %v", err)
	}
	for _, name := range []string{sessionViewer, sessionOperator, sessionAdministrator} {
		if tokens[name] == "" {
			t.Fatalf("token file has no %s session; run make auth-login", name)
		}
	}
	return tokens
}

// sessionCookieName is httpserver.SessionCookieName; the e2e test talks to
// the stack only over HTTP.
const sessionCookieName = "rollback_guard_session"

// loginResult is the callback's outcome. Only the token is a secret.
type loginResult struct {
	status int
	token  string
	// sessionCookie reports a Set-Cookie that sets a non-empty session.
	sessionCookie bool
	// errorCode is the JSON error code of a refused callback.
	errorCode string
}

// login performs the real OAuth login for the fake user demo-NAME. A token it
// obtains is recorded for the output scan (recordMinted).
func (e env) login(ctx context.Context, t *testing.T, name string) loginResult {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Jar:           jar,
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	redirect := func(target string) string {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("login of demo-%s: request failed", name)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatalf("login of demo-%s: closing the response failed", name)
		}
		next := response.Header.Get("Location")
		if response.StatusCode != http.StatusFound || next == "" {
			t.Fatalf("login of demo-%s: expected a redirect, got HTTP %d", name, response.StatusCode)
		}
		return next
	}
	authorize := redirect(e.orchestrator + "/v1/auth/github/login")
	callback := redirect(authorize + "&login=" + url.QueryEscape("demo-"+name))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, callback, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("login of demo-%s: callback failed", name)
	}
	defer closeBody(t, response.Body)
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	result := loginResult{status: response.StatusCode}
	for _, cookie := range response.Cookies() {
		if cookie.Name == sessionCookieName && cookie.Value != "" && cookie.MaxAge >= 0 {
			result.sessionCookie = true
		}
	}
	if response.StatusCode != http.StatusOK {
		var refused struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(data, &refused); err != nil {
			t.Fatalf("login of demo-%s: HTTP %d without a JSON error", name, response.StatusCode)
		}
		result.errorCode = refused.Error
		return result
	}
	var session struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &session); err != nil || session.Token == "" {
		t.Fatalf("login of demo-%s: no session in the response", name)
	}
	result.token = session.Token
	recordMinted(t, "login-"+name, session.Token)
	return result
}

// recordMinted appends a token this test obtained to E2E_MINTED_TOKENS_FILE
// (0600, KEY=TOKEN), so `make e2e-test` can scan the test output for it as
// well as for the tokens in the auth-tokens file.
func recordMinted(t *testing.T, key, token string) {
	t.Helper()
	path := strings.TrimSpace(os.Getenv("E2E_MINTED_TOKENS_FILE"))
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open the minted token file: %v", err)
	}
	if _, err := file.WriteString(key + "=" + token + "\n"); err != nil {
		t.Fatal("write the minted token file failed")
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close the minted token file: %v", err)
	}
}

// expectRefused sends one request as `as` and requires status want with the
// JSON error code.
func (e env) expectRefused(ctx context.Context, t *testing.T, description string, want int, code, as, method, path string, body any) {
	t.Helper()
	if got := str(t, decode(t, e.expect(ctx, t, description, want, as, method, path, body)), "error"); got != code {
		t.Fatalf("%s: error code %q, want %q", description, got, code)
	}
}

// expect sends one request as the session `as` and requires status want.
func (e env) expect(ctx context.Context, t *testing.T, description string, want int, as, method, path string, body any) []byte {
	t.Helper()
	status, data := e.callAs(ctx, t, as, method, path, body, "", 30*time.Second)
	if status != want {
		t.Fatalf("%s: HTTP %d, want %d (%s)", description, status, want, data)
	}
	return data
}

// requireIdentities checks the three sessions resolve to the expected role and
// GitHub actor, that anonymous access, a non-member login and a viewer's
// create are refused, and that logout ends exactly one session.
func (e env) requireIdentities(ctx context.Context, t *testing.T) {
	t.Helper()
	e.expectRefused(ctx, t, "no credentials", http.StatusUnauthorized, "unauthenticated", anonymous, http.MethodGet, "/v1/me", nil)
	for _, want := range []struct{ session, role, actor string }{
		{sessionAdministrator, "administrator", actorAdministrator},
		{sessionOperator, "operator", actorOperator},
		{sessionViewer, "viewer", "github:1003"},
	} {
		me := decode(t, e.expect(ctx, t, want.session+" session", http.StatusOK, want.session, http.MethodGet, "/v1/me", nil))
		if str(t, me, "role") != want.role || str(t, me, "actorId") != want.actor {
			t.Fatalf("%s session is %v %v, want %s %s", want.session, me["role"], me["actorId"], want.role, want.actor)
		}
	}
	outsider := e.login(ctx, t, "outsider")
	if outsider.status != http.StatusForbidden || outsider.token != "" || outsider.sessionCookie || outsider.errorCode != "forbidden" {
		t.Fatalf("non-member login: HTTP %d, error %q, session in body %v, session cookie %v; want 403 forbidden without any session",
			outsider.status, outsider.errorCode, outsider.token != "", outsider.sessionCookie)
	}
	e.expectRefused(ctx, t, "a viewer creates an incident", http.StatusForbidden, "forbidden", sessionViewer, http.MethodPost, "/v1/incidents", e.incidentRequest(0.5))

	second := e.login(ctx, t, "viewer")
	if second.status != http.StatusOK || !second.sessionCookie {
		t.Fatalf("second viewer login: HTTP %d, session cookie %v", second.status, second.sessionCookie)
	}
	e.tokens["viewer-logout"] = second.token
	e.expect(ctx, t, "second viewer session", http.StatusOK, "viewer-logout", http.MethodGet, "/v1/me", nil)
	e.expect(ctx, t, "logout", http.StatusNoContent, "viewer-logout", http.MethodPost, "/v1/auth/logout", nil)
	e.expect(ctx, t, "the logged-out session", http.StatusUnauthorized, "viewer-logout", http.MethodGet, "/v1/me", nil)
	e.expect(ctx, t, "the other viewer session", http.StatusOK, sessionViewer, http.MethodGet, "/v1/me", nil)
}

// requireApprovalRefused: an operator cannot approve and a viewer cannot reject
// an incident awaiting approval, which stays awaiting_approval.
func (e env) requireApprovalRefused(ctx context.Context, t *testing.T, id, actionID, proposalHash string) {
	t.Helper()
	e.expectRefused(ctx, t, "an operator approves", http.StatusForbidden, "forbidden", sessionOperator, http.MethodPost, "/v1/incidents/"+id+"/approve",
		map[string]string{"actionId": actionID, "proposalHash": proposalHash})
	e.expectRefused(ctx, t, "a viewer rejects", http.StatusForbidden, "forbidden", sessionViewer, http.MethodPost, "/v1/incidents/"+id+"/reject",
		map[string]string{"actionId": actionID, "proposalHash": proposalHash, "reason": "e2e: viewer must not reject"})
	incident := object(t, decode(t, e.expect(ctx, t, "read the incident", http.StatusOK, sessionViewer, http.MethodGet, "/v1/incidents/"+id, nil)), "incident")
	if got := str(t, incident, "status"); got != "awaiting_approval" {
		t.Fatalf("incident %s is %s after refused requests, want awaiting_approval", id, got)
	}
}

// requireActors checks the audit timeline attributes incident.created to
// creator and, when approver is set, rollback.approved to that human.
func requireActors(t *testing.T, events timeline, creator, approver string) {
	t.Helper()
	created := events.ofKind("incident.created")
	if len(created) != 1 || created[0].Actor.ID != creator {
		t.Fatalf("incident.created events %v, want one by %s", created, creator)
	}
	approved := events.ofKind("rollback.approved")
	switch {
	case approver != "" && (len(approved) != 1 || approved[0].Actor.Kind != "human" || approved[0].Actor.ID != approver):
		t.Fatalf("rollback.approved events %v, want one by the human %s", approved, approver)
	case approver == "" && len(approved) != 0:
		t.Fatalf("unexpected rollback.approved events %v", approved)
	}
}

func (e env) incidentRequest(observed float64) map[string]any {
	return map[string]any{
		"target":  map[string]string{"cluster": e.cluster, "namespace": e.namespace, "deployment": e.deployment, "service": e.deployment},
		"symptom": map[string]any{"kind": "http_error_rate", "observed": observed, "threshold": threshold, "unit": "ratio", "startedAt": time.Now().UTC().Format(time.RFC3339)},
	}
}
