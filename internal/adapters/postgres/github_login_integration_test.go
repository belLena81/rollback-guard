//go:build integration

package postgres

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/githuboauth"
	"github.com/belLena81/rollback-guard/internal/adapters/githuboauth/githubfake"
	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/httpserver"
	"github.com/belLena81/rollback-guard/internal/platform/runtime"
	"github.com/belLena81/rollback-guard/internal/ports"
)

// The GitHub login on Postgres: the callback use case upserts the user and
// issues the session in one transaction, and the session then authenticates
// a request through SessionAuthMiddleware. A non-member leaves no rows.
func TestGitHubLoginOnPostgres(t *testing.T) {
	fixture := newAuthFixture(t)
	github := githubfake.New("pg-client", "pg-client-secret")
	t.Cleanup(github.Close)
	github.AddUser(githubfake.User{ID: 7001, Login: "pg-admin", Email: "admin@example.com", Organizations: []string{"Acme"}})
	github.AddUser(githubfake.User{ID: 7002, Login: "pg-outsider", Organizations: []string{"other"}})
	const redirectURL = "http://127.0.0.1:8080/v1/auth/github/callback"
	provider, err := githuboauth.New(githuboauth.Config{
		ClientID: "pg-client", ClientSecret: "pg-client-secret", RedirectURL: redirectURL,
		BaseURL: github.URL(), APIURL: github.URL(), Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	clock := &sessionTestClock{now: authNow}
	ids := &runtime.IDGenerator{}
	sessions, err := application.NewSessionService(application.SessionDependencies{
		UnitOfWork: fixture.uow, Clock: clock, IDGenerator: ids, Random: runtime.RandomSource{},
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	login, err := application.NewLoginService(application.LoginDependencies{
		Sessions: sessions, UnitOfWork: fixture.uow, Clock: clock, IDGenerator: ids, Random: runtime.RandomSource{}, Provider: provider,
	}, application.LoginPolicy{
		AllowedOrganization: "acme", BootstrapAdmins: []string{"PG-ADMIN"}, StateKey: []byte(strings.Repeat("k", application.MinLoginStateKeyBytes)),
	})
	if err != nil {
		t.Fatal(err)
	}
	callback := func(githubLogin string) (application.LoginResult, error) {
		t.Helper()
		start, err := login.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		authorize, err := url.Parse(start.AuthorizationURL)
		if err != nil {
			t.Fatal(err)
		}
		query := authorize.Query()
		code := github.IssueCode(githubLogin, query.Get("code_challenge"), redirectURL)
		return login.Complete(t.Context(), start.SealedState, query.Get("state"), code)
	}

	if _, err := callback("pg-outsider"); !errors.Is(err, application.ErrLoginDenied) {
		t.Fatalf("non-member login = %v, want ErrLoginDenied", err)
	}
	if err := fixture.tx(t, func(repos ports.AuthRepositories) error {
		_, err := repos.Users().GetByGitHubID(7002)
		return err
	}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("non-member user row lookup = %v, want not found", err)
	}

	result, err := callback("pg-admin")
	if err != nil {
		t.Fatal(err)
	}
	if result.User.Role() != domain.RoleAdministrator || result.Actor.ID() != "github:7001" {
		t.Fatalf("bootstrap admin = role %s actor %s", result.User.Role(), result.Actor.ID())
	}
	fixture.mustTx(t, func(repos ports.AuthRepositories) error {
		stored, err := repos.Users().GetByGitHubID(7001)
		if err != nil {
			return err
		}
		if stored.Login() != "pg-admin" || stored.Email() != "admin@example.com" || len(stored.Organizations()) != 1 {
			t.Errorf("stored user = %s %s %v", stored.Login(), stored.Email(), stored.Organizations())
		}
		return nil
	})

	middleware, err := httpserver.NewSessionAuthMiddleware(sessions, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	var actor domain.Actor
	handler := middleware.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, _ = httpserver.ActorFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://guard.example/v1/incidents/x", nil)
	request.AddCookie(httpserver.NewSessionCookie(result.Token, result.Session.ExpiresAt(), true))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent || actor.ID() != "github:7001" {
		t.Fatalf("authenticated request = %d actor %q", recorder.Code, actor.ID())
	}
}
