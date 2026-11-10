package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestUserWithProfile(t *testing.T) {
	admin, err := mustUser(t, nil).WithRole(RoleAdministrator, userNow)
	if err != nil {
		t.Fatal(err)
	}
	later := userNow.Add(time.Minute)
	tests := []struct {
		name  string
		login string
		email string
		orgs  []string
		now   time.Time
		field string // empty means valid
	}{
		{"refreshed", " octo-renamed ", "new@example.com", []string{"acme"}, later, ""},
		{"email removed", "octo-cat", "", nil, later, ""},
		{"bad login", "octo cat", "", nil, later, "user.login"},
		{"bad email", "octo-cat", "nope", nil, later, "user.email"},
		{"bad org", "octo-cat", "", []string{"a/b"}, later, "user.organizations"},
		{"clock went back", "octo-cat", "", nil, userNow.Add(-time.Second), "user.updated_at"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := admin.WithProfile(tt.login, tt.email, tt.orgs, tt.now)
			if tt.field != "" {
				var validation *ValidationError
				if !errors.As(err, &validation) || validation.Field != tt.field {
					t.Fatalf("WithProfile error = %v, want validation of %s", err, tt.field)
				}
				return
			}
			if err != nil {
				t.Fatalf("WithProfile: %v", err)
			}
			if got.Version() != admin.Version()+1 || got.Role() != RoleAdministrator || got.GitHubID() != admin.GitHubID() || got.Disabled() {
				t.Fatalf("WithProfile changed identity, role or version: %+v", got.Snapshot())
			}
			if !got.SameProfile(tt.login, tt.email, tt.orgs) {
				t.Fatalf("SameProfile after WithProfile = false: %+v", got.Snapshot())
			}
		})
	}
	if _, err := (User{}).WithProfile("octo-cat", "", nil, later); err == nil {
		t.Fatal("WithProfile on zero user succeeded")
	}
}

func TestUserSameProfile(t *testing.T) {
	user := mustUser(t, nil)
	tests := []struct {
		name  string
		login string
		email string
		orgs  []string
		want  bool
	}{
		{"identical", "octo-cat", "octo@example.com", []string{"acme", "acme-ops"}, true},
		{"trimmed", " octo-cat ", "octo@example.com ", []string{" acme", "acme-ops"}, true},
		{"login renamed", "octo-dog", "octo@example.com", []string{"acme", "acme-ops"}, false},
		{"login case changed", "Octo-Cat", "octo@example.com", []string{"acme", "acme-ops"}, false},
		{"email changed", "octo-cat", "", []string{"acme", "acme-ops"}, false},
		{"org removed", "octo-cat", "octo@example.com", []string{"acme"}, false},
		{"org order changed", "octo-cat", "octo@example.com", []string{"acme-ops", "acme"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := user.SameProfile(tt.login, tt.email, tt.orgs); got != tt.want {
				t.Fatalf("SameProfile = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsGitHubLogin(t *testing.T) {
	tests := map[string]bool{
		"octocat":    true,
		"Octo-Cat-1": true,
		strings.Repeat("a", MaxGitHubLoginLength): true,
		"":         false,
		"octo cat": false,
		"octo_cat": false,
		"a/b":      false,
		strings.Repeat("a", MaxGitHubLoginLength+1): false,
	}
	for value, want := range tests {
		if got := IsGitHubLogin(value); got != want {
			t.Errorf("IsGitHubLogin(%q) = %v, want %v", value, got, want)
		}
	}
}
