package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func mustActor(t *testing.T, id ActorID, kind ActorKind) Actor {
	t.Helper()
	actor, err := NewActor(id, kind)
	if err != nil {
		t.Fatal(err)
	}
	return actor
}

func TestAuthAuditEventConstruction(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.FixedZone("x", 3600))
	admin := mustActor(t, "github:3", ActorHuman)
	system := mustActor(t, "rollback-guardian", ActorSystem)
	tests := []struct {
		name  string
		build func() (AuthAuditEvent, error)
		ok    bool
	}{
		{"role changed", func() (AuthAuditEvent, error) {
			return NewRoleChangedAudit("a-1", admin, "user-2", RoleViewer, RoleOperator, now)
		}, true},
		{"role unchanged", func() (AuthAuditEvent, error) {
			return NewRoleChangedAudit("a-1", admin, "user-2", RoleViewer, RoleViewer, now)
		}, false},
		{"invalid role", func() (AuthAuditEvent, error) {
			return NewRoleChangedAudit("a-1", admin, "user-2", Role(0), RoleViewer, now)
		}, false},
		{"sessions revoked", func() (AuthAuditEvent, error) {
			return NewSessionsRevokedAudit("a-1", admin, "user-2", 0, now)
		}, true},
		{"negative count", func() (AuthAuditEvent, error) {
			return NewSessionsRevokedAudit("a-1", admin, "user-2", -1, now)
		}, false},
		{"logout", func() (AuthAuditEvent, error) { return NewLogoutAudit("a-1", admin, "user-3", now) }, true},
		{"user disabled", func() (AuthAuditEvent, error) { return NewUserDisabledAudit("a-1", admin, "user-2", 2, now) }, true},
		{"user disabled negative count", func() (AuthAuditEvent, error) {
			return NewUserDisabledAudit("a-1", admin, "user-2", -1, now)
		}, false},
		{"user enabled", func() (AuthAuditEvent, error) { return NewUserEnabledAudit("a-1", admin, "user-2", now) }, true},
		{"user enabled by system", func() (AuthAuditEvent, error) { return NewUserEnabledAudit("a-1", system, "user-2", now) }, false},
		{"rehydrated user_disabled without count", func() (AuthAuditEvent, error) {
			return RehydrateAuthAuditEvent(AuthAuditSnapshot{ID: "a-1", OccurredAt: now, ActorID: "github:3", Action: AuthAuditUserDisabled, TargetUserID: "user-3"})
		}, false},
		{"rehydrated user_enabled with roles", func() (AuthAuditEvent, error) {
			return RehydrateAuthAuditEvent(AuthAuditSnapshot{ID: "a-1", OccurredAt: now, ActorID: "github:3", Action: AuthAuditUserEnabled, TargetUserID: "user-3", FromRole: RoleViewer})
		}, false},
		{"rehydrated user_enabled with count", func() (AuthAuditEvent, error) {
			return RehydrateAuthAuditEvent(AuthAuditSnapshot{ID: "a-1", OccurredAt: now, ActorID: "github:3", Action: AuthAuditUserEnabled, TargetUserID: "user-3", HasSessionCount: true})
		}, false},
		{"system actor", func() (AuthAuditEvent, error) { return NewLogoutAudit("a-1", system, "user-3", now) }, false},
		{"blank id", func() (AuthAuditEvent, error) { return NewLogoutAudit(" ", admin, "user-3", now) }, false},
		{"blank target", func() (AuthAuditEvent, error) { return NewLogoutAudit("a-1", admin, "", now) }, false},
		{"zero time", func() (AuthAuditEvent, error) { return NewLogoutAudit("a-1", admin, "user-3", time.Time{}) }, false},
		{"rehydrated logout with roles", func() (AuthAuditEvent, error) {
			return RehydrateAuthAuditEvent(AuthAuditSnapshot{ID: "a-1", OccurredAt: now, ActorID: "github:3", Action: AuthAuditLogout, TargetUserID: "user-3", ToRole: RoleAdministrator})
		}, false},
		{"rehydrated unknown action", func() (AuthAuditEvent, error) {
			return RehydrateAuthAuditEvent(AuthAuditSnapshot{ID: "a-1", OccurredAt: now, ActorID: "github:3", Action: AuthAuditAction(9), TargetUserID: "user-3"})
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event, err := tt.build()
			if tt.ok {
				if err != nil {
					t.Fatal(err)
				}
				if !event.Valid() || event.OccurredAt().Location() != time.UTC {
					t.Fatalf("event = %+v", event)
				}
				back, err := RehydrateAuthAuditEvent(event.Snapshot())
				if err != nil || back != event {
					t.Fatalf("round trip = %+v, %v", back, err)
				}
				return
			}
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("err = %v, want validation", err)
			}
		})
	}
}

func TestAuthAuditActionRoundTrip(t *testing.T) {
	want := "role_changed,sessions_revoked,logout,user_disabled,user_enabled"
	var got []string
	for action := AuthAuditRoleChanged; action.Valid(); action++ {
		got = append(got, action.String())
		parsed, err := ParseAuthAuditAction(action.String())
		if err != nil || parsed != action {
			t.Fatalf("%s: %v %v", action, parsed, err)
		}
	}
	if joined := strings.Join(got, ","); joined != want {
		t.Fatalf("actions = %s, want %s", joined, want)
	}
	if _, err := ParseAuthAuditAction("login_denied"); err == nil {
		t.Fatal("parsed an out-of-scope action")
	}
}
