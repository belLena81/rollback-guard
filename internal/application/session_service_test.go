package application_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/fake"
	"github.com/belLena81/rollback-guard/internal/adapters/memory"
	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

var sessionStart = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

type sessionClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *sessionClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *sessionClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type sessionIDs struct {
	mu sync.Mutex
	n  int
}

func (g *sessionIDs) NewID(prefix string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return prefix + "-" + strings.Repeat("x", g.n)
}

type sessionFixture struct {
	store   *memory.AuthUnitOfWork
	clock   *sessionClock
	random  *fake.RandomSource
	service *application.SessionService
	user    domain.User
}

const sessionTTL = 8 * time.Hour

func newSessionFixture(t *testing.T) sessionFixture {
	t.Helper()
	f := sessionFixture{
		store:  memory.NewAuthUnitOfWork(),
		clock:  &sessionClock{now: sessionStart},
		random: &fake.RandomSource{},
	}
	service, err := application.NewSessionService(application.SessionDependencies{
		UnitOfWork:  f.store,
		Clock:       f.clock,
		IDGenerator: &sessionIDs{},
		Random:      f.random,
	}, sessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	f.service = service
	user, err := domain.NewUser("user-1", 4242, "octocat", "octo@example.com", []string{"acme"}, domain.RoleOperator, sessionStart)
	if err != nil {
		t.Fatal(err)
	}
	f.user = user
	f.mustTx(t, func(repos ports.AuthRepositories) error { return repos.Users().Create(user) })
	return f
}

func (f sessionFixture) mustTx(t *testing.T, fn func(ports.AuthRepositories) error) {
	t.Helper()
	if err := f.store.WithinAuthTransaction(t.Context(), fn); err != nil {
		t.Fatal(err)
	}
}

func (f sessionFixture) issue(t *testing.T) (string, domain.Session) {
	t.Helper()
	raw, session, err := f.service.Issue(t.Context(), f.user)
	if err != nil {
		t.Fatal(err)
	}
	return raw, session
}

func (f sessionFixture) updateUser(t *testing.T, change func(domain.User) (domain.User, error)) {
	t.Helper()
	f.mustTx(t, func(repos ports.AuthRepositories) error {
		current, err := repos.Users().GetByID(f.user.ID())
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

func TestNewSessionServiceValidatesDependencies(t *testing.T) {
	valid := application.SessionDependencies{
		UnitOfWork:  memory.NewAuthUnitOfWork(),
		Clock:       &sessionClock{now: sessionStart},
		IDGenerator: &sessionIDs{},
		Random:      &fake.RandomSource{},
	}
	tests := []struct {
		name   string
		mutate func(*application.SessionDependencies)
		ttl    time.Duration
	}{
		{name: "missing unit of work", mutate: func(d *application.SessionDependencies) { d.UnitOfWork = nil }, ttl: sessionTTL},
		{name: "missing clock", mutate: func(d *application.SessionDependencies) { d.Clock = nil }, ttl: sessionTTL},
		{name: "missing id generator", mutate: func(d *application.SessionDependencies) { d.IDGenerator = nil }, ttl: sessionTTL},
		{name: "missing random source", mutate: func(d *application.SessionDependencies) { d.Random = nil }, ttl: sessionTTL},
		{name: "zero ttl", mutate: func(*application.SessionDependencies) {}, ttl: 0},
		{name: "negative ttl", mutate: func(*application.SessionDependencies) {}, ttl: -time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := valid
			tt.mutate(&deps)
			if _, err := application.NewSessionService(deps, tt.ttl); err == nil {
				t.Fatal("expected constructor error")
			}
		})
	}
	if _, err := application.NewSessionService(valid, sessionTTL); err != nil {
		t.Fatalf("valid dependencies rejected: %v", err)
	}
}

func TestSessionIssueResolveRoundTrip(t *testing.T) {
	f := newSessionFixture(t)
	raw, session := f.issue(t)

	if strings.ContainsAny(raw, "=+/") || len(raw) != base64.RawURLEncoding.EncodedLen(application.SessionTokenBytes) {
		t.Fatalf("token is not unpadded base64url of %d bytes: len %d", application.SessionTokenBytes, len(raw))
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(decoded)
	want, err := domain.NewSessionTokenHash(digest[:])
	if err != nil {
		t.Fatal(err)
	}
	if !session.TokenHash().Equal(want) {
		t.Fatal("stored hash is not the SHA-256 of the token bytes")
	}
	if session.UserID() != f.user.ID() || !session.ExpiresAt().Equal(sessionStart.Add(sessionTTL)) {
		t.Fatalf("unexpected session %v expiring %s", session, session.ExpiresAt())
	}

	principal, err := f.service.Resolve(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if principal.SessionID() != session.ID() || principal.User().ID() != f.user.ID() || principal.User().Role() != domain.RoleOperator {
		t.Fatalf("unexpected principal %v", principal)
	}
	if principal.Actor().ID() != "github:4242" || !principal.Actor().IsHuman() {
		t.Fatalf("unexpected actor %s", principal.Actor().ID())
	}
}

func TestSessionIssueReturnsDistinctTokens(t *testing.T) {
	f := newSessionFixture(t)
	first, _ := f.issue(t)
	second, _ := f.issue(t)
	if first == second {
		t.Fatal("two sessions share a token")
	}
}

func TestSessionIssueRejectsDisabledAndInvalidUsers(t *testing.T) {
	f := newSessionFixture(t)
	disabled, err := f.user.Disable(sessionStart)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.service.Issue(t.Context(), disabled); !errors.Is(err, domain.ErrUserDisabled) {
		t.Fatalf("disabled user: got %v", err)
	}
	if _, _, err := f.service.Issue(t.Context(), domain.User{}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("zero user: got %v", err)
	}
	// The stored row decides: a caller holding a stale enabled copy of a
	// user disabled in the database gets no session either.
	f.updateUser(t, func(u domain.User) (domain.User, error) { return u.Disable(sessionStart) })
	if _, _, err := f.service.Issue(t.Context(), f.user); !errors.Is(err, domain.ErrUserDisabled) {
		t.Fatalf("stale enabled copy: got %v", err)
	}
}

func TestSessionIssueFailsWhenRandomSourceFails(t *testing.T) {
	f := newSessionFixture(t)
	f.random.Err = errors.New("entropy unavailable")
	raw, _, err := f.service.Issue(t.Context(), f.user)
	if err == nil || raw != "" {
		t.Fatalf("expected failure without a token, got %q, %v", raw, err)
	}
}

func TestSessionResolveFailuresAreIndistinguishable(t *testing.T) {
	tests := []struct {
		name  string
		token func(t *testing.T, f sessionFixture) string
	}{
		{name: "empty", token: func(*testing.T, sessionFixture) string { return "" }},
		{name: "malformed characters", token: func(*testing.T, sessionFixture) string { return "not a token!" }},
		{name: "padded", token: func(t *testing.T, f sessionFixture) string { raw, _ := f.issue(t); return raw + "=" }},
		{name: "standard base64 alphabet", token: func(*testing.T, sessionFixture) string {
			return base64.RawStdEncoding.EncodeToString([]byte(strings.Repeat("\xfb", application.SessionTokenBytes)))
		}},
		{name: "short", token: func(*testing.T, sessionFixture) string {
			return base64.RawURLEncoding.EncodeToString(make([]byte, 16))
		}},
		{name: "unknown", token: func(*testing.T, sessionFixture) string {
			return base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("k", application.SessionTokenBytes)))
		}},
		{name: "expired", token: func(t *testing.T, f sessionFixture) string {
			raw, _ := f.issue(t)
			f.clock.Advance(sessionTTL)
			return raw
		}},
		{name: "revoked", token: func(t *testing.T, f sessionFixture) string {
			raw, session := f.issue(t)
			if err := f.service.Revoke(t.Context(), session.ID()); err != nil {
				t.Fatal(err)
			}
			return raw
		}},
		{name: "disabled user", token: func(t *testing.T, f sessionFixture) string {
			raw, _ := f.issue(t)
			f.updateUser(t, func(u domain.User) (domain.User, error) { return u.Disable(sessionStart.Add(time.Minute)) })
			return raw
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newSessionFixture(t)
			token := tt.token(t, f)
			_, err := f.service.Resolve(t.Context(), token)
			if !errors.Is(err, domain.ErrUnauthenticated) {
				t.Fatalf("expected unauthenticated, got %v", err)
			}
			var typed *domain.UnauthenticatedError
			if !errors.As(err, &typed) || errors.Is(err, domain.ErrNoActiveSession) || err.Error() != "unauthenticated" {
				t.Fatalf("failure is distinguishable: %T %q", err, err.Error())
			}
		})
	}
}

func TestSessionResolveSeesLiveRole(t *testing.T) {
	f := newSessionFixture(t)
	raw, _ := f.issue(t)
	f.updateUser(t, func(u domain.User) (domain.User, error) {
		return u.WithRole(domain.RoleAdministrator, sessionStart.Add(time.Minute))
	})
	principal, err := f.service.Resolve(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if principal.User().Role() != domain.RoleAdministrator {
		t.Fatalf("role change not visible: %s", principal.User().Role())
	}
}

func TestSessionResolveJustBeforeExpiry(t *testing.T) {
	f := newSessionFixture(t)
	raw, _ := f.issue(t)
	f.clock.Advance(sessionTTL - time.Nanosecond)
	if _, err := f.service.Resolve(t.Context(), raw); err != nil {
		t.Fatalf("session should still be active: %v", err)
	}
}

func TestSessionRevokeAllForUser(t *testing.T) {
	f := newSessionFixture(t)
	first, _ := f.issue(t)
	second, _ := f.issue(t)
	count, err := f.service.RevokeAllForUser(t.Context(), f.user.ID())
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("revoked %d sessions, want 2", count)
	}
	for _, raw := range []string{first, second} {
		if _, err := f.service.Resolve(t.Context(), raw); !errors.Is(err, domain.ErrUnauthenticated) {
			t.Fatalf("revoked session still resolves: %v", err)
		}
	}
	if count, err := f.service.RevokeAllForUser(t.Context(), f.user.ID()); err != nil || count != 0 {
		t.Fatalf("second revoke-all: %d, %v", count, err)
	}
}

func TestSessionRevokeUnknownSessionIsNotFound(t *testing.T) {
	f := newSessionFixture(t)
	if err := f.service.Revoke(t.Context(), "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

type failingAuthUnitOfWork struct{ err error }

func (u failingAuthUnitOfWork) WithinAuthTransaction(context.Context, func(ports.AuthRepositories) error) error {
	return u.err
}

func (u failingAuthUnitOfWork) WithinAuthReadTransaction(context.Context, func(ports.AuthReadRepositories) error) error {
	return u.err
}

func TestSessionResolveInfrastructureErrorIsNotUnauthenticated(t *testing.T) {
	infra := errors.New("connection refused")
	service, err := application.NewSessionService(application.SessionDependencies{
		UnitOfWork:  failingAuthUnitOfWork{err: infra},
		Clock:       &sessionClock{now: sessionStart},
		IDGenerator: &sessionIDs{},
		Random:      &fake.RandomSource{},
	}, sessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	token := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("k", application.SessionTokenBytes)))
	_, err = service.Resolve(t.Context(), token)
	if err == nil || errors.Is(err, domain.ErrUnauthenticated) || !errors.Is(err, infra) {
		t.Fatalf("infrastructure error must stay distinguishable from 401: %v", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatal("error carries the token")
	}
}
