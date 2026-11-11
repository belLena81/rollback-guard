package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/belLena81/rollback-guard/internal/domain"
)

const (
	testClientSecret = "client-secret-value-never-printed"
	testStateSecret  = "state-secret-value-0123456789abcdef-never-printed"
)

func githubEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL":              testDatabaseURL,
		"AUTH_MODE":                 "github",
		"AUTH_GITHUB_CLIENT_ID":     "Iv1.0123456789abcdef",
		"AUTH_GITHUB_CLIENT_SECRET": testClientSecret,
		"AUTH_GITHUB_REDIRECT_URL":  "https://guardian.example.com/v1/auth/github/callback",
		"AUTH_GITHUB_ALLOWED_ORG":   "acme",
		"AUTH_BOOTSTRAP_ADMINS":     "alice:101, Bob-1:202",
		"AUTH_STATE_SECRET":         testStateSecret,
	}
}

func validGitHubConfig() GitHubAuthConfig {
	return GitHubAuthConfig{
		ClientID:            "Iv1.0123456789abcdef",
		ClientSecret:        NewSecret(testClientSecret),
		RedirectURL:         "https://guardian.example.com/v1/auth/github/callback",
		AllowedOrganization: "acme",
		BootstrapAdmins:     []domain.BootstrapAdmin{{Login: "alice", GitHubID: 101}, {Login: "Bob-1", GitHubID: 202}},
		StateSecret:         NewSecret(testStateSecret),
		BaseURL:             DefaultGitHubBaseURL,
		APIURL:              DefaultGitHubAPIURL,
		HTTPTimeout:         GitHubHTTPTimeout,
		RateLimit:           LoginRateLimit{PerMinute: DefaultLoginRateLimitPerMinute, Burst: DefaultLoginRateLimitBurst},
	}
}

func rateLimited(perMinute, burst int) GitHubAuthConfig {
	g := validGitHubConfig()
	g.RateLimit = LoginRateLimit{PerMinute: perMinute, Burst: burst}
	return g
}

func TestLoadAuthConfig(t *testing.T) {
	loopback := validGitHubConfig()
	loopback.RedirectURL = "http://127.0.0.1:8080/v1/auth/github/callback"
	loopback.BaseURL = "http://localhost:9999"
	loopback.APIURL = "http://[::1]:9999/api"

	tests := []struct {
		name    string
		local   bool
		env     map[string]string
		want    AuthConfig
		wantErr string
	}{
		// The default fails closed: no AUTH_MODE means github, which needs
		// the GitHub settings.
		{name: "default is github and fails closed without settings", local: true, env: map[string]string{"AUTH_MODE": ""}, wantErr: "AUTH_GITHUB_CLIENT_ID is required"},
		{name: "default is github", env: map[string]string{"AUTH_MODE": ""}, want: AuthConfig{Mode: AuthModeGitHub, GitHub: validGitHubConfig()}},
		{name: "blank mode is the default", env: map[string]string{"AUTH_MODE": "  "}, want: AuthConfig{Mode: AuthModeGitHub, GitHub: validGitHubConfig()}},
		{name: "explicit local", local: true, env: map[string]string{"AUTH_MODE": "local"}, want: AuthConfig{Mode: AuthModeLocal}},
		{
			name: "local ignores invalid github settings", local: true,
			env:  map[string]string{"AUTH_MODE": "local", "AUTH_STATE_SECRET": "short", "AUTH_GITHUB_REDIRECT_URL": "http://evil.example"},
			want: AuthConfig{Mode: AuthModeLocal},
		},
		{name: "unknown mode", local: true, env: map[string]string{"AUTH_MODE": "GitHub"}, wantErr: "AUTH_MODE must be local or github"},
		{name: "github", want: AuthConfig{Mode: AuthModeGitHub, GitHub: validGitHubConfig()}},
		{
			name: "github loopback over http",
			env: map[string]string{
				"AUTH_GITHUB_REDIRECT_URL": loopback.RedirectURL, "AUTH_GITHUB_BASE_URL": "http://localhost:9999/",
				"AUTH_GITHUB_API_URL": "http://[::1]:9999/api",
			},
			want: AuthConfig{Mode: AuthModeGitHub, GitHub: loopback},
		},
		{name: "missing client id", env: map[string]string{"AUTH_GITHUB_CLIENT_ID": ""}, wantErr: "AUTH_GITHUB_CLIENT_ID is required"},
		{name: "client id with space", env: map[string]string{"AUTH_GITHUB_CLIENT_ID": "a b"}, wantErr: "AUTH_GITHUB_CLIENT_ID"},
		{name: "missing client secret", env: map[string]string{"AUTH_GITHUB_CLIENT_SECRET": ""}, wantErr: "AUTH_GITHUB_CLIENT_SECRET is required"},
		{name: "missing state secret", env: map[string]string{"AUTH_STATE_SECRET": ""}, wantErr: "AUTH_STATE_SECRET is required"},
		{name: "short state secret", env: map[string]string{"AUTH_STATE_SECRET": strings.Repeat("x", 31)}, wantErr: "at least 32 bytes"},
		{name: "missing redirect", env: map[string]string{"AUTH_GITHUB_REDIRECT_URL": ""}, wantErr: "AUTH_GITHUB_REDIRECT_URL is required"},
		{name: "http redirect off loopback", env: map[string]string{"AUTH_GITHUB_REDIRECT_URL": "http://guardian.example.com/cb"}, wantErr: "must use HTTPS"},
		{name: "redirect without path", env: map[string]string{"AUTH_GITHUB_REDIRECT_URL": "https://guardian.example.com"}, wantErr: "absolute URL"},
		{name: "redirect with query", env: map[string]string{"AUTH_GITHUB_REDIRECT_URL": "https://guardian.example.com/cb?x=1"}, wantErr: "absolute URL"},
		{name: "relative redirect", env: map[string]string{"AUTH_GITHUB_REDIRECT_URL": "/v1/auth/github/callback"}, wantErr: "absolute URL"},
		{name: "base url with credentials", env: map[string]string{"AUTH_GITHUB_BASE_URL": "https://user:pw@github.example"}, wantErr: "AUTH_GITHUB_BASE_URL must be"},
		{name: "http base url off loopback", env: map[string]string{"AUTH_GITHUB_BASE_URL": "http://github.com"}, wantErr: "AUTH_GITHUB_BASE_URL must use HTTPS"},
		{name: "http api url off loopback", env: map[string]string{"AUTH_GITHUB_API_URL": "http://10.0.0.1"}, wantErr: "AUTH_GITHUB_API_URL must use HTTPS"},
		{name: "missing org", env: map[string]string{"AUTH_GITHUB_ALLOWED_ORG": ""}, wantErr: "AUTH_GITHUB_ALLOWED_ORG is required"},
		{name: "bad org", env: map[string]string{"AUTH_GITHUB_ALLOWED_ORG": "acme/evil"}, wantErr: "AUTH_GITHUB_ALLOWED_ORG"},
		{name: "missing admins", env: map[string]string{"AUTH_BOOTSTRAP_ADMINS": ""}, wantErr: "AUTH_BOOTSTRAP_ADMINS is required"},
		{name: "bad admin login", env: map[string]string{"AUTH_BOOTSTRAP_ADMINS": "alice:1,bob smith:2"}, wantErr: "AUTH_BOOTSTRAP_ADMINS must be"},
		{name: "admin login without id", env: map[string]string{"AUTH_BOOTSTRAP_ADMINS": "alice:1,bob"}, wantErr: "AUTH_BOOTSTRAP_ADMINS must be"},
		{name: "admin non-numeric id", env: map[string]string{"AUTH_BOOTSTRAP_ADMINS": "alice:x1"}, wantErr: "AUTH_BOOTSTRAP_ADMINS must be"},
		{name: "admin zero id", env: map[string]string{"AUTH_BOOTSTRAP_ADMINS": "alice:0"}, wantErr: "AUTH_BOOTSTRAP_ADMINS must be"},
		{name: "admin id without login", env: map[string]string{"AUTH_BOOTSTRAP_ADMINS": ":5"}, wantErr: "AUTH_BOOTSTRAP_ADMINS must be"},
		{name: "empty admin entry", env: map[string]string{"AUTH_BOOTSTRAP_ADMINS": "alice:1,,bob:2"}, wantErr: "AUTH_BOOTSTRAP_ADMINS must be"},
		{
			name: "custom rate limit", env: map[string]string{"AUTH_RATE_LIMIT_PER_MINUTE": "30", "AUTH_RATE_LIMIT_BURST": "5"},
			want: AuthConfig{Mode: AuthModeGitHub, GitHub: rateLimited(30, 5)},
		},
		{name: "rate limit zero", env: map[string]string{"AUTH_RATE_LIMIT_PER_MINUTE": "0"}, wantErr: "AUTH_RATE_LIMIT_PER_MINUTE must be between 1 and 600"},
		{name: "rate limit too high", env: map[string]string{"AUTH_RATE_LIMIT_PER_MINUTE": "601"}, wantErr: "AUTH_RATE_LIMIT_PER_MINUTE must be between"},
		{name: "burst zero", env: map[string]string{"AUTH_RATE_LIMIT_BURST": "0"}, wantErr: "AUTH_RATE_LIMIT_BURST must be between 1 and 100"},
		{name: "burst not a number", env: map[string]string{"AUTH_RATE_LIMIT_BURST": "x"}, wantErr: "AUTH_RATE_LIMIT_BURST must be between"},
		{name: "local ignores bad rate limit", local: true, env: map[string]string{"AUTH_MODE": "local", "AUTH_RATE_LIMIT_BURST": "x"}, want: AuthConfig{Mode: AuthModeLocal}},
		{name: "too many admins", env: map[string]string{"AUTH_BOOTSTRAP_ADMINS": strings.Repeat("a:1,", 32) + "a:1"}, wantErr: "at most 32"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{"DATABASE_URL": testDatabaseURL}
			if !tt.local {
				env = githubEnv()
			}
			maps.Copy(env, tt.env)
			isolateEnvironment(t, env)
			got, err := Load()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("got %v, want error containing %q", err, tt.wantErr)
				}
				for _, secret := range []string{testClientSecret, testStateSecret, "pw@"} {
					if strings.Contains(err.Error(), secret) {
						t.Fatalf("error %q leaks a secret", err)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Auth, tt.want) {
				t.Fatalf("got %+v, want %+v", got.Auth, tt.want)
			}
			if got.Auth.GitHub.ClientSecret.Reveal() != tt.want.GitHub.ClientSecret.Reveal() {
				t.Fatal("client secret not preserved")
			}
		})
	}
}

func TestSecretsNeverRender(t *testing.T) {
	isolateEnvironment(t, githubEnv())
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("config", "auth", cfg.Auth, "secret", cfg.Auth.GitHub.StateSecret)
	slog.New(slog.NewTextHandler(&logs, nil)).Info("config", "secret", cfg.Auth.GitHub.ClientSecret)
	encoded, err := json.Marshal(cfg.Auth)
	if err != nil {
		t.Fatal(err)
	}
	renderings := []string{
		fmt.Sprint(cfg), fmt.Sprintf("%v %+v %#v %s %q %x", cfg, cfg, cfg, cfg.Auth.GitHub.ClientSecret, cfg.Auth.GitHub.StateSecret, cfg.Auth.GitHub.ClientSecret),
		fmt.Sprintf("%+v %#v", cfg.Auth, cfg.Auth.GitHub), cfg.Auth.GitHub.ClientSecret.String(), string(encoded), logs.String(),
	}
	for _, rendering := range renderings {
		for _, secret := range []string{testClientSecret, testStateSecret} {
			if strings.Contains(rendering, secret) {
				t.Fatalf("rendering leaks a secret: %s", rendering)
			}
		}
	}
	if !strings.Contains(string(encoded), redactedSecret) {
		t.Fatalf("JSON rendering = %s", encoded)
	}
}
