//go:build integration

package postgres

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/httpserver"
	"github.com/belLena81/rollback-guard/internal/platform/runtime"
	"github.com/belLena81/rollback-guard/internal/ports"
)

type sessionTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *sessionTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *sessionTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// TestSessionAuthMiddlewareOnPostgres runs the session service and
// middleware against the real users/sessions schema: issue, authenticate,
// role change, immediate revocation, disable, and expiry.
func TestSessionAuthMiddlewareOnPostgres(t *testing.T) {
	fixture := newAuthFixture(t)
	clock := &sessionTestClock{now: authNow}
	const ttl = time.Hour
	service, err := application.NewSessionService(application.SessionDependencies{
		UnitOfWork:  fixture.uow,
		Clock:       clock,
		IDGenerator: &runtime.IDGenerator{},
		Random:      runtime.RandomSource{},
	}, ttl)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	middleware, err := httpserver.NewSessionAuthMiddleware(service, slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}
	var principal domain.Principal
	handler := middleware.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, ok := httpserver.ActorFromContext(r.Context())
		principal, _ = httpserver.PrincipalFromContext(r.Context())
		if !ok || actor != principal.Actor() {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	call := func(token string) (int, string) {
		request := httptest.NewRequest(http.MethodPost, "http://guard.example/v1/incidents", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		body, _ := io.ReadAll(recorder.Body) //nolint:errcheck // reading a recorder buffer cannot fail
		return recorder.Code, string(body)
	}
	user := authUser(t, "user-pg", 9001, "pg-octocat")
	fixture.mustTx(t, func(repos ports.AuthRepositories) error { return repos.Users().Create(user) })
	updateUser := func(change func(domain.User) (domain.User, error)) {
		t.Helper()
		fixture.mustTx(t, func(repos ports.AuthRepositories) error {
			current, err := repos.Users().GetByID(user.ID())
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

	raw, session, err := service.Issue(t.Context(), user)
	if err != nil {
		t.Fatal(err)
	}
	if status, body := call(raw); status != http.StatusNoContent {
		t.Fatalf("valid session: %d %s", status, body)
	}
	if principal.Actor().ID() != "github:9001" || principal.SessionID() != session.ID() || principal.User().Role() != domain.RoleOperator {
		t.Fatalf("unexpected principal %v role %s", principal, principal.User().Role())
	}

	updateUser(func(u domain.User) (domain.User, error) {
		return u.WithRole(domain.RoleViewer, authNow.Add(time.Minute))
	})
	if status, _ := call(raw); status != http.StatusNoContent || principal.User().Role() != domain.RoleViewer {
		t.Fatalf("role change not visible: %d %s", status, principal.User().Role())
	}

	if err := service.Revoke(t.Context(), session.ID()); err != nil {
		t.Fatal(err)
	}
	revokedStatus, revokedBody := call(raw)
	if revokedStatus != http.StatusUnauthorized {
		t.Fatalf("revoked session: %d", revokedStatus)
	}

	second, _, err := service.Issue(t.Context(), user)
	if err != nil {
		t.Fatal(err)
	}
	updateUser(func(u domain.User) (domain.User, error) { return u.Disable(authNow.Add(2 * time.Minute)) })
	if status, body := call(second); status != http.StatusUnauthorized || body != revokedBody {
		t.Fatalf("disabled user: %d %s", status, body)
	}
	updateUser(func(u domain.User) (domain.User, error) { return u.Enable(authNow.Add(3 * time.Minute)) })
	if status, _ := call(second); status != http.StatusNoContent {
		t.Fatalf("re-enabled user: %d", status)
	}
	clock.Advance(ttl)
	if status, body := call(second); status != http.StatusUnauthorized || body != revokedBody {
		t.Fatalf("expired session: %d %s", status, body)
	}
	if status, body := call("unknown-token"); status != http.StatusUnauthorized || body != revokedBody {
		t.Fatalf("unknown token: %d %s", status, body)
	}
	for _, secret := range []string{raw, second} {
		if bytes.Contains(logs.Bytes(), []byte(secret)) {
			t.Fatal("token logged")
		}
	}
}
