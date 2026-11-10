package domain

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestNewPrincipal(t *testing.T) {
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	user, err := NewUser("user-1", 77, "octocat", "", nil, RoleViewer, now)
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := user.Disable(now)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		user      User
		sessionID SessionID
		wantErr   error
	}{
		{name: "enabled user", user: user, sessionID: "session-1"},
		{name: "missing session", user: user, sessionID: " ", wantErr: ErrValidation},
		{name: "zero user", user: User{}, sessionID: "session-1", wantErr: ErrValidation},
		{name: "disabled user", user: disabled, sessionID: "session-1", wantErr: ErrUserDisabled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			principal, err := NewPrincipal(tt.user, tt.sessionID)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) || principal.Valid() {
					t.Fatalf("got %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil || !principal.Valid() {
				t.Fatalf("unexpected error %v", err)
			}
			if principal.Actor().ID() != "github:77" || !principal.Actor().IsHuman() || principal.SessionID() != tt.sessionID {
				t.Fatalf("unexpected principal %v", principal)
			}
			if got := fmt.Sprintf("%+v", principal); got != "Principal{actor:github:77 session:session-1}" {
				t.Fatalf("format: %s", got)
			}
		})
	}
}

func TestUnauthenticatedErrorIsOpaque(t *testing.T) {
	err := error(&UnauthenticatedError{})
	if !errors.Is(err, ErrUnauthenticated) || err.Error() != "unauthenticated" {
		t.Fatalf("got %v", err)
	}
	if errors.Is(err, ErrNoActiveSession) || errors.Is(err, ErrForbidden) {
		t.Fatal("unauthenticated must not match other auth errors")
	}
}
