package githuboauth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/githuboauth"
	"github.com/belLena81/rollback-guard/internal/adapters/githuboauth/githubfake"
	"github.com/belLena81/rollback-guard/internal/ports"
)

const (
	clientID     = "client-id-1"
	clientSecret = "client-secret-do-not-leak"
	redirectURL  = "http://127.0.0.1:8080/v1/auth/github/callback"
	verifier     = "verifier-0123456789-0123456789-0123456789-0123"
)

func challenge(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func newProvider(t *testing.T, fake *githubfake.Server, timeout time.Duration) *githuboauth.Provider {
	t.Helper()
	provider, err := githuboauth.New(githuboauth.Config{
		ClientID: clientID, ClientSecret: clientSecret, RedirectURL: redirectURL,
		BaseURL: fake.URL(), APIURL: fake.URL(), Timeout: timeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func newFake(t *testing.T) *githubfake.Server {
	t.Helper()
	fake := githubfake.New(clientID, clientSecret)
	t.Cleanup(fake.Close)
	fake.AddUser(githubfake.User{ID: 42, Login: "octocat", Email: "octo@example.com", Organizations: []string{"acme", "other"}})
	return fake
}

func TestAuthorizationURL(t *testing.T) {
	fake := newFake(t)
	raw := newProvider(t, fake, time.Second).AuthorizationURL("state-abc", challenge(verifier))
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Scheme + "://" + parsed.Host + parsed.Path; got != fake.URL()+githubfake.AuthorizePath {
		t.Fatalf("authorize endpoint = %s", got)
	}
	want := map[string]string{
		"client_id": clientID, "redirect_uri": redirectURL, "scope": "read:org user:email", "state": "state-abc",
		"code_challenge": challenge(verifier), "code_challenge_method": "S256", "response_type": "code",
	}
	for key, value := range want {
		if got := parsed.Query().Get(key); got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}
	if strings.Contains(raw, clientSecret) {
		t.Fatal("authorize URL carries the client secret")
	}
}

func TestExchangeReturnsIdentity(t *testing.T) {
	fake := newFake(t)
	code := fake.IssueCode("octocat", challenge(verifier), redirectURL)
	identity, err := newProvider(t, fake, time.Second).Exchange(context.Background(), code, verifier)
	if err != nil {
		t.Fatal(err)
	}
	want := ports.GitHubIdentity{GitHubID: 42, Login: "octocat", Email: "octo@example.com", Organizations: []string{"acme", "other"}}
	if identity.GitHubID != want.GitHubID || identity.Login != want.Login || identity.Email != want.Email || !slices.Equal(identity.Organizations, want.Organizations) {
		t.Fatalf("identity = %+v", identity)
	}
}

func TestExchangeWithoutPublicEmail(t *testing.T) {
	fake := newFake(t)
	fake.AddUser(githubfake.User{ID: 43, Login: "private", Organizations: []string{"acme"}})
	code := fake.IssueCode("private", challenge(verifier), redirectURL)
	identity, err := newProvider(t, fake, time.Second).Exchange(context.Background(), code, verifier)
	if err != nil || identity.Email != "" {
		t.Fatalf("identity = %+v, err %v", identity, err)
	}
}

func TestExchangeFollowsOrganizationPagesUpToBound(t *testing.T) {
	tests := []struct {
		orgs      int
		wantOrgs  int
		wantPages int
	}{
		{0, 0, 1},
		{99, 99, 1},
		{100, 100, 2},
		{250, 250, 3},
		{githuboauth.MaxOrganizationPages*100 + 200, githuboauth.MaxOrganizationPages * 100, githuboauth.MaxOrganizationPages},
	}
	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.orgs), func(t *testing.T) {
			fake := newFake(t)
			orgs := make([]string, tt.orgs)
			for i := range orgs {
				orgs[i] = fmt.Sprintf("org-%d", i)
			}
			fake.AddUser(githubfake.User{ID: 44, Login: "joiner", Organizations: orgs})
			code := fake.IssueCode("joiner", challenge(verifier), redirectURL)
			identity, err := newProvider(t, fake, time.Second).Exchange(context.Background(), code, verifier)
			if err != nil {
				t.Fatal(err)
			}
			if len(identity.Organizations) != tt.wantOrgs || !slices.Equal(identity.Organizations, orgs[:tt.wantOrgs]) {
				t.Fatalf("got %d organizations, want %d", len(identity.Organizations), tt.wantOrgs)
			}
			if got := fake.APIRequests(); got != 1+tt.wantPages {
				t.Fatalf("API requests = %d, want %d", got, 1+tt.wantPages)
			}
		})
	}
}

func TestExchangeRejectedCode(t *testing.T) {
	tests := map[string]func(*githubfake.Server) (string, string){
		"unknown code": func(*githubfake.Server) (string, string) { return "no-such-code", verifier },
		"wrong verifier": func(f *githubfake.Server) (string, string) {
			return f.IssueCode("octocat", challenge(verifier), redirectURL), verifier + "x"
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			fake := newFake(t)
			code, v := setup(fake)
			_, err := newProvider(t, fake, time.Second).Exchange(context.Background(), code, v)
			if !errors.Is(err, ports.ErrAuthorizationCodeRejected) {
				t.Fatalf("err = %v, want ErrAuthorizationCodeRejected", err)
			}
			assertClean(t, err, code)
		})
	}
	t.Run("code reuse", func(t *testing.T) {
		fake := newFake(t)
		provider := newProvider(t, fake, time.Second)
		code := fake.IssueCode("octocat", challenge(verifier), redirectURL)
		if _, err := provider.Exchange(context.Background(), code, verifier); err != nil {
			t.Fatal(err)
		}
		if _, err := provider.Exchange(context.Background(), code, verifier); !errors.Is(err, ports.ErrAuthorizationCodeRejected) {
			t.Fatalf("reused code err = %v", err)
		}
	})
}

func TestExchangeProviderFailures(t *testing.T) {
	tests := map[string]struct {
		setup   func(*githubfake.Server)
		timeout time.Duration
	}{
		"token 500":        {func(f *githubfake.Server) { f.FailToken(http.StatusInternalServerError) }, time.Second},
		"token 401":        {func(f *githubfake.Server) { f.FailToken(http.StatusUnauthorized) }, time.Second},
		"api 502":          {func(f *githubfake.Server) { f.FailAPI(http.StatusBadGateway) }, time.Second},
		"api 403":          {func(f *githubfake.Server) { f.FailAPI(http.StatusForbidden) }, time.Second},
		"timeout":          {func(f *githubfake.Server) { f.SetDelay(time.Second) }, 50 * time.Millisecond},
		"bad client creds": {nil, time.Second},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fake := newFake(t)
			if tt.setup != nil {
				tt.setup(fake)
			}
			provider := newProvider(t, fake, tt.timeout)
			if name == "bad client creds" {
				var err error
				provider, err = githuboauth.New(githuboauth.Config{
					ClientID: clientID, ClientSecret: "wrong", RedirectURL: redirectURL,
					BaseURL: fake.URL(), APIURL: fake.URL(), Timeout: tt.timeout,
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			code := fake.IssueCode("octocat", challenge(verifier), redirectURL)
			_, err := provider.Exchange(context.Background(), code, verifier)
			if !errors.Is(err, ports.ErrIdentityProviderFailed) {
				t.Fatalf("err = %v, want ErrIdentityProviderFailed", err)
			}
			assertClean(t, err, code)
			for _, token := range fake.IssuedTokens() {
				if strings.Contains(err.Error(), token) {
					t.Fatal("error carries the access token")
				}
			}
		})
	}
}

func TestNewValidates(t *testing.T) {
	valid := githuboauth.Config{
		ClientID: clientID, ClientSecret: clientSecret, RedirectURL: redirectURL,
		BaseURL: githuboauth.DefaultBaseURL, APIURL: githuboauth.DefaultAPIURL, Timeout: time.Second,
	}
	tests := map[string]func(*githuboauth.Config){
		"no client id":       func(c *githuboauth.Config) { c.ClientID = "" },
		"no secret":          func(c *githuboauth.Config) { c.ClientSecret = "" },
		"no timeout":         func(c *githuboauth.Config) { c.Timeout = 0 },
		"relative base":      func(c *githuboauth.Config) { c.BaseURL = "/github" },
		"ftp api":            func(c *githuboauth.Config) { c.APIURL = "ftp://api.github.com" },
		"api with creds":     func(c *githuboauth.Config) { c.APIURL = "https://u:p@api.github.com" },
		"redirect with frag": func(c *githuboauth.Config) { c.RedirectURL = redirectURL + "#x" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			config := valid
			mutate(&config)
			_, err := githuboauth.New(config)
			if err == nil {
				t.Fatal("New accepted invalid config")
			}
			if strings.Contains(err.Error(), clientSecret) {
				t.Fatal("error carries the client secret")
			}
		})
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		if out := fmt.Sprintf(verb, valid); strings.Contains(out, clientSecret) || !strings.Contains(out, clientID) {
			t.Fatalf("%s of Config = %q", verb, out)
		}
	}
	if _, err := githuboauth.New(valid); err != nil {
		t.Fatal(err)
	}
}

// assertClean fails when err carries the provider body, code or secret.
func assertClean(t *testing.T, err error, code string) {
	t.Helper()
	text := err.Error()
	for _, secret := range []string{githubfake.FailureBodyCanary, code, clientSecret, verifier} {
		if strings.Contains(text, secret) {
			t.Fatalf("error %q leaks %q", text, secret)
		}
	}
}
