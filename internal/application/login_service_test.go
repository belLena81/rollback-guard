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

// scriptedProvider is a ports.IdentityProvider that records what it was
// given and answers with a scripted identity or error.
type scriptedProvider struct {
	mu         sync.Mutex
	identity   ports.GitHubIdentity
	err        error
	state      string
	challenge  string
	exchanges  int
	code       string
	verifier   string
	authorizeN int
}

func (p *scriptedProvider) AuthorizationURL(state, codeChallenge string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.authorizeN++
	p.state, p.challenge = state, codeChallenge
	return "https://github.example/login/oauth/authorize?state=" + state
}

func (p *scriptedProvider) Exchange(_ context.Context, code, codeVerifier string) (ports.GitHubIdentity, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.exchanges++
	p.code, p.verifier = code, codeVerifier
	if p.err != nil {
		return ports.GitHubIdentity{}, p.err
	}
	return p.identity, nil
}

func (p *scriptedProvider) setIdentity(identity ports.GitHubIdentity) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.identity = identity
}

func (p *scriptedProvider) exchangeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exchanges
}

var loginStateKey = []byte(strings.Repeat("k", application.MinLoginStateKeyBytes))

type loginFixture struct {
	store    *memory.AuthUnitOfWork
	clock    *sessionClock
	provider *scriptedProvider
	sessions *application.SessionService
	login    *application.LoginService
}

func member(id int64, login string) ports.GitHubIdentity {
	return ports.GitHubIdentity{GitHubID: id, Login: login, Email: login + "@example.com", Organizations: []string{"other", "ACME"}}
}

func newLoginFixture(t *testing.T) *loginFixture {
	t.Helper()
	f := &loginFixture{
		store:    memory.NewAuthUnitOfWork(),
		clock:    &sessionClock{now: sessionStart},
		provider: &scriptedProvider{identity: member(7, "octocat")},
	}
	ids := &sessionIDs{}
	random := &fake.RandomSource{}
	var err error
	f.sessions, err = application.NewSessionService(application.SessionDependencies{
		UnitOfWork: f.store, Clock: f.clock, IDGenerator: ids, Random: random,
	}, sessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	f.login, err = application.NewLoginService(application.LoginDependencies{
		Sessions: f.sessions, UnitOfWork: f.store, Clock: f.clock, IDGenerator: ids, Random: random, Provider: f.provider,
	}, application.LoginPolicy{AllowedOrganization: "acme", BootstrapAdmins: []string{"Root-Admin"}, StateKey: loginStateKey})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// signIn runs a full Begin/Complete round trip.
func (f *loginFixture) signIn(t *testing.T) (application.LoginResult, error) {
	t.Helper()
	start, err := f.login.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return f.login.Complete(context.Background(), start.SealedState, f.provider.state, "code-1")
}

func (f *loginFixture) userByGitHubID(t *testing.T, id int64) (domain.User, bool) {
	t.Helper()
	var user domain.User
	err := f.store.WithinAuthTransaction(context.Background(), func(repos ports.AuthRepositories) error {
		var err error
		user, err = repos.Users().GetByGitHubID(id)
		return err
	})
	if errors.Is(err, domain.ErrNotFound) {
		return domain.User{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return user, true
}

func TestLoginBeginUsesFreshStateAndS256Challenge(t *testing.T) {
	f := newLoginFixture(t)
	start, err := f.login.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	firstState := f.provider.state
	if len(firstState) != 43 || len(f.provider.challenge) != 43 {
		t.Fatalf("state %q / challenge %q must be 256-bit base64url", firstState, f.provider.challenge)
	}
	if !start.ExpiresAt.Equal(sessionStart.Add(application.LoginStateTTL)) {
		t.Fatalf("ExpiresAt = %v", start.ExpiresAt)
	}
	if strings.Contains(start.SealedState, firstState) {
		t.Fatal("sealed state must not be the plain state")
	}
	if _, err := f.login.Complete(context.Background(), start.SealedState, firstState, "code-1"); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(f.provider.verifier))
	if got := base64.RawURLEncoding.EncodeToString(sum[:]); got != f.provider.challenge {
		t.Fatalf("challenge %q is not S256(verifier) %q", f.provider.challenge, got)
	}
	if len(f.provider.verifier) < 43 || f.provider.code != "code-1" {
		t.Fatalf("exchange got verifier %q code %q", f.provider.verifier, f.provider.code)
	}
	if _, err := f.login.Begin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.provider.state == firstState {
		t.Fatal("state reused across logins")
	}
}

func TestLoginBeginFailsWhenRandomFails(t *testing.T) {
	f := newLoginFixture(t)
	login, err := application.NewLoginService(application.LoginDependencies{
		Sessions: f.sessions, UnitOfWork: f.store, Clock: f.clock, IDGenerator: &sessionIDs{},
		Random: &fake.RandomSource{Err: errors.New("entropy exhausted")}, Provider: f.provider,
	}, application.LoginPolicy{AllowedOrganization: "acme", StateKey: loginStateKey})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := login.Begin(context.Background()); err == nil {
		t.Fatal("Begin succeeded without randomness")
	}
}

func TestLoginCompleteRejectsBadStateWithoutExchange(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(f *loginFixture, sealed, state, code *string)
	}{
		{"missing cookie", func(_ *loginFixture, sealed, _, _ *string) { *sealed = "" }},
		{"tampered payload", func(_ *loginFixture, sealed, _, _ *string) { *sealed = flipFirst(*sealed) }},
		{"tampered mac", func(_ *loginFixture, sealed, _, _ *string) {
			payload, mac, _ := strings.Cut(*sealed, ".")
			*sealed = payload + "." + flipFirst(mac)
		}},
		{"no separator", func(_ *loginFixture, sealed, _, _ *string) { *sealed = strings.Replace(*sealed, ".", "", 1) }},
		{"state mismatch", func(_ *loginFixture, _, state, _ *string) { *state = flipFirst(*state) }},
		{"state missing", func(_ *loginFixture, _, state, _ *string) { *state = "" }},
		{"expired", func(f *loginFixture, _, _, _ *string) { f.clock.Advance(application.LoginStateTTL) }},
		{"code missing", func(_ *loginFixture, _, _, code *string) { *code = "" }},
		{"code too long", func(_ *loginFixture, _, _, code *string) {
			*code = strings.Repeat("c", application.MaxAuthorizationCodeLength+1)
		}},
		{"code with control character", func(_ *loginFixture, _, _, code *string) { *code = "abc\n" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newLoginFixture(t)
			start, err := f.login.Begin(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			sealed, state, code := start.SealedState, f.provider.state, "code-1"
			tt.mutate(f, &sealed, &state, &code)
			_, err = f.login.Complete(context.Background(), sealed, state, code)
			if !errors.Is(err, application.ErrInvalidLoginRequest) {
				t.Fatalf("Complete error = %v, want ErrInvalidLoginRequest", err)
			}
			if f.provider.exchangeCount() != 0 {
				t.Fatal("provider exchange ran for an invalid login request")
			}
		})
	}
}

func TestLoginCompleteRejectsStateSealedUnderAnotherKey(t *testing.T) {
	f := newLoginFixture(t)
	other, err := application.NewLoginService(application.LoginDependencies{
		Sessions: f.sessions, UnitOfWork: f.store, Clock: f.clock, IDGenerator: &sessionIDs{}, Random: &fake.RandomSource{}, Provider: f.provider,
	}, application.LoginPolicy{AllowedOrganization: "acme", StateKey: []byte(strings.Repeat("x", 40))})
	if err != nil {
		t.Fatal(err)
	}
	start, err := other.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.login.Complete(context.Background(), start.SealedState, f.provider.state, "code-1"); !errors.Is(err, application.ErrInvalidLoginRequest) {
		t.Fatalf("Complete error = %v, want ErrInvalidLoginRequest", err)
	}
}

func TestLoginNonMemberIsDeniedWithoutRows(t *testing.T) {
	f := newLoginFixture(t)
	f.provider.setIdentity(ports.GitHubIdentity{GitHubID: 8, Login: "outsider", Organizations: []string{"acme-evil", "other"}})
	_, err := f.signIn(t)
	if !errors.Is(err, application.ErrLoginDenied) {
		t.Fatalf("Complete error = %v, want ErrLoginDenied", err)
	}
	if _, ok := f.userByGitHubID(t, 8); ok {
		t.Fatal("a user row was written for a non-member")
	}
}

func TestLoginMemberIsCreatedAsViewerWithWorkingSession(t *testing.T) {
	f := newLoginFixture(t)
	result, err := f.signIn(t)
	if err != nil {
		t.Fatal(err)
	}
	if result.User.Role() != domain.RoleViewer || result.User.Login() != "octocat" || result.Actor.ID() != "github:7" {
		t.Fatalf("result user %+v actor %v", result.User.Snapshot(), result.Actor)
	}
	if !result.Session.ExpiresAt().Equal(sessionStart.Add(sessionTTL)) {
		t.Fatalf("session expires %v", result.Session.ExpiresAt())
	}
	principal, err := f.sessions.Resolve(context.Background(), result.Token)
	if err != nil {
		t.Fatalf("Resolve issued token: %v", err)
	}
	if principal.Actor() != result.Actor || principal.User().Role() != domain.RoleViewer {
		t.Fatalf("principal %v role %v", principal.Actor(), principal.User().Role())
	}
}

func TestLoginBootstrapAdminOnlyAtCreation(t *testing.T) {
	f := newLoginFixture(t)
	f.provider.setIdentity(member(99, "root-admin"))
	result, err := f.signIn(t)
	if err != nil {
		t.Fatal(err)
	}
	if result.User.Role() != domain.RoleAdministrator {
		t.Fatalf("bootstrap admin created as %v", result.User.Role())
	}
	f.clock.Advance(time.Minute)
	if err := f.store.WithinAuthTransaction(context.Background(), func(repos ports.AuthRepositories) error {
		demoted, err := result.User.WithRole(domain.RoleViewer, f.clock.Now())
		if err != nil {
			return err
		}
		return repos.Users().Update(demoted, result.User.Version())
	}); err != nil {
		t.Fatal(err)
	}
	again, err := f.signIn(t)
	if err != nil {
		t.Fatal(err)
	}
	if again.User.Role() != domain.RoleViewer {
		t.Fatalf("existing bootstrap admin re-elevated to %v", again.User.Role())
	}
}

func TestLoginRefreshesProfileAndKeepsRole(t *testing.T) {
	f := newLoginFixture(t)
	first, err := f.signIn(t)
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Minute)
	f.provider.setIdentity(ports.GitHubIdentity{GitHubID: 7, Login: "octo-renamed", Organizations: []string{"acme"}})
	second, err := f.signIn(t)
	if err != nil {
		t.Fatal(err)
	}
	if second.User.ID() != first.User.ID() || second.User.Login() != "octo-renamed" || second.User.Email() != "" ||
		second.User.Version() != first.User.Version()+1 || second.User.Role() != domain.RoleViewer {
		t.Fatalf("refreshed user %+v", second.User.Snapshot())
	}
	third, err := f.signIn(t)
	if err != nil {
		t.Fatal(err)
	}
	if third.User.Version() != second.User.Version() {
		t.Fatal("an unchanged profile was rewritten")
	}
}

func TestLoginDisabledUserGetsNoSession(t *testing.T) {
	f := newLoginFixture(t)
	first, err := f.signIn(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.WithinAuthTransaction(context.Background(), func(repos ports.AuthRepositories) error {
		disabled, err := first.User.Disable(f.clock.Now())
		if err != nil {
			return err
		}
		return repos.Users().Update(disabled, first.User.Version())
	}); err != nil {
		t.Fatal(err)
	}
	result, err := f.signIn(t)
	if !errors.Is(err, domain.ErrUserDisabled) || result.Token != "" {
		t.Fatalf("Complete for disabled user = %v, token set %v", err, result.Token != "")
	}
}

func TestLoginPassesProviderFailuresThrough(t *testing.T) {
	for _, want := range []error{ports.ErrIdentityProviderFailed, ports.ErrAuthorizationCodeRejected} {
		f := newLoginFixture(t)
		f.provider.err = want
		if _, err := f.signIn(t); !errors.Is(err, want) {
			t.Fatalf("Complete error = %v, want %v", err, want)
		}
	}
}

func TestLoginRejectsUnusableIdentity(t *testing.T) {
	for name, identity := range map[string]ports.GitHubIdentity{
		"bad login":  {GitHubID: 7, Login: "octo cat", Organizations: []string{"acme"}},
		"bad id":     {GitHubID: 0, Login: "octocat", Organizations: []string{"acme"}},
		"bad email":  {GitHubID: 7, Login: "octocat", Email: "no-at-sign", Organizations: []string{"acme"}},
		"bad org":    {GitHubID: 7, Login: "octocat", Organizations: []string{"acme", "a b"}},
		"empty orgs": {GitHubID: 7, Login: "octocat"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newLoginFixture(t)
			f.provider.setIdentity(identity)
			_, err := f.signIn(t)
			if !errors.Is(err, ports.ErrIdentityProviderFailed) && !errors.Is(err, application.ErrLoginDenied) {
				t.Fatalf("Complete error = %v", err)
			}
			if _, ok := f.userByGitHubID(t, 7); ok {
				t.Fatal("user row written for unusable identity")
			}
		})
	}
}

func TestNewLoginServiceValidates(t *testing.T) {
	f := newLoginFixture(t)
	deps := application.LoginDependencies{
		Sessions: f.sessions, UnitOfWork: f.store, Clock: f.clock, IDGenerator: &sessionIDs{}, Random: &fake.RandomSource{}, Provider: f.provider,
	}
	valid := application.LoginPolicy{AllowedOrganization: "acme", BootstrapAdmins: []string{"root"}, StateKey: loginStateKey}
	tests := map[string]func(*application.LoginDependencies, *application.LoginPolicy){
		"no provider": func(d *application.LoginDependencies, _ *application.LoginPolicy) { d.Provider = nil },
		"no sessions": func(d *application.LoginDependencies, _ *application.LoginPolicy) { d.Sessions = nil },
		"no random":   func(d *application.LoginDependencies, _ *application.LoginPolicy) { d.Random = nil },
		"no org":      func(_ *application.LoginDependencies, p *application.LoginPolicy) { p.AllowedOrganization = "" },
		"bad org":     func(_ *application.LoginDependencies, p *application.LoginPolicy) { p.AllowedOrganization = "a/b" },
		"bad bootstrap admin": func(_ *application.LoginDependencies, p *application.LoginPolicy) {
			p.BootstrapAdmins = []string{"a b"}
		},
		"short key": func(_ *application.LoginDependencies, p *application.LoginPolicy) {
			p.StateKey = loginStateKey[:application.MinLoginStateKeyBytes-1]
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			d, p := deps, valid
			mutate(&d, &p)
			if _, err := application.NewLoginService(d, p); err == nil {
				t.Fatal("NewLoginService accepted invalid input")
			}
		})
	}
	if _, err := application.NewLoginService(deps, valid); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
}

func flipFirst(value string) string {
	if value == "" {
		return "x"
	}
	if value[0] == 'A' {
		return "B" + value[1:]
	}
	return "A" + value[1:]
}
