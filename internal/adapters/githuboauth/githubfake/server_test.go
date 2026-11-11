package githubfake_test

import (
	"net"
	"net/http"
	"net/url"
	"testing"

	"github.com/belLena81/rollback-guard/internal/adapters/githuboauth/githubfake"
)

func noRedirect() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func authorize(t *testing.T, base string, extra url.Values) *http.Response {
	t.Helper()
	query := url.Values{
		"client_id": {"cid"}, "code_challenge_method": {"S256"}, "code_challenge": {"c"},
		"state": {"s"}, "redirect_uri": {"http://127.0.0.1:1/cb"},
	}
	for k, v := range extra {
		query[k] = v
	}
	resp, err := noRedirect().Get(base + githubfake.AuthorizePath + "?" + query.Encode())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestListenServesOnTheGivenAddress(t *testing.T) {
	fake, err := githubfake.Listen("127.0.0.1:0", "cid", "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer fake.Close()
	parsed, err := url.Parse(fake.URL())
	if err != nil {
		t.Fatal(err)
	}
	if ip := net.ParseIP(parsed.Hostname()); ip == nil || !ip.IsLoopback() {
		t.Fatalf("URL %q is not loopback", fake.URL())
	}
}

func TestListenRefusesANonLoopbackAddress(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", ":0", "192.0.2.1:80", "example.com:80"} {
		if fake, err := githubfake.Listen(addr, "cid", "secret"); err == nil {
			fake.Close()
			t.Errorf("Listen(%q) succeeded, want an error", addr)
		}
	}
}

func TestAuthorizeLoginHintSelectsTheAccount(t *testing.T) {
	fake := githubfake.New("cid", "secret")
	defer fake.Close()
	fake.AddUser(githubfake.User{ID: 1, Login: "alice"})
	fake.AddUser(githubfake.User{ID: 2, Login: "bob"})
	fake.SignInAs("alice")

	tests := []struct {
		name  string
		extra url.Values
		want  int
	}{
		{"hint wins over the signed-in account", url.Values{"login": {"Bob"}}, http.StatusFound},
		{"no hint uses the signed-in account", nil, http.StatusFound},
		{"unknown hinted account is rejected", url.Values{"login": {"nobody"}}, http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := authorize(t, fake.URL(), test.extra).StatusCode; got != test.want {
				t.Fatalf("status %d, want %d", got, test.want)
			}
		})
	}
}
