package application

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

// The authUsers fixture: user-1 viewer, user-2 operator, user-3 the only
// enabled administrator, user-4 a disabled administrator.

func TestDisableUser(t *testing.T) {
	tests := []struct {
		name         string
		actor        int64
		target       domain.ActorID
		wantErr      error
		wantDisabled bool
		audited      bool
	}{
		{name: "admin disables viewer", actor: 3, target: "github:1", wantDisabled: true, audited: true},
		{name: "admin disables operator", actor: 3, target: "github:2", wantDisabled: true, audited: true},
		{name: "already disabled is a no-op", actor: 3, target: "github:4", wantDisabled: true},
		{name: "operator forbidden", actor: 2, target: "github:1", wantErr: domain.ErrForbidden},
		{name: "viewer forbidden", actor: 1, target: "github:2", wantErr: domain.ErrForbidden},
		{name: "disabled admin unauthenticated", actor: 4, target: "github:1", wantErr: domain.ErrUnauthenticated},
		{name: "unknown target", actor: 3, target: "github:99", wantErr: domain.ErrNotFound},
		{name: "malformed target", actor: 3, target: "user-1", wantErr: domain.ErrValidation},
		{name: "last admin cannot disable self", actor: 3, target: "github:3", wantErr: domain.ErrLastAdministrator},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAdminFixture(t, nil, nil, nil)
			f.addSession(t, "session-a", "user-1", 1)
			f.addSession(t, "session-b", "user-2", 2)
			f.addSession(t, "session-c", "user-3", 3)
			before := map[domain.UserID]uint64{}
			for _, id := range []domain.UserID{"user-1", "user-2", "user-3", "user-4"} {
				before[id] = f.user(t, id).Version()
			}
			got, err := f.service.DisableUser(context.Background(), githubActor(t, tt.actor), tt.target)
			if tt.wantErr == nil && err != nil || tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("DisableUser = %v, want %v", err, tt.wantErr)
			}
			events := f.audit(t)
			if tt.wantErr != nil || !tt.audited {
				if len(events) != 0 {
					t.Fatalf("audit = %v, want none", events)
				}
				for id, version := range before {
					if f.user(t, id).Version() != version {
						t.Fatalf("%s was written by a refused or no-op disable", id)
					}
				}
				if tt.wantErr == nil && (!got.Disabled() || got.ID() != userOf(tt.target)) {
					t.Fatalf("no-op returned %+v", got.Snapshot())
				}
				return
			}
			stored := f.user(t, userOf(tt.target))
			if !stored.Disabled() || !got.Disabled() || stored.Version() != before[stored.ID()]+1 {
				t.Fatalf("stored = %+v, returned = %+v", stored.Snapshot(), got.Snapshot())
			}
			if _, err := domain.ActorFor(stored); !errors.Is(err, domain.ErrUserDisabled) {
				t.Fatalf("disabled user still yields an actor: %v", err)
			}
			if len(events) != 1 {
				t.Fatalf("audit has %d events, want 1", len(events))
			}
			e := events[0]
			if n, ok := e.SessionCount(); e.Action() != domain.AuthAuditUserDisabled || !ok || n != 1 || e.TargetUserID() != stored.ID() || e.Actor() != githubActor(t, tt.actor) {
				t.Fatalf("audit event = %+v", e.Snapshot())
			}
		})
	}
}

// TestDisableUserRevokesOnlyTheTargetsSessions: disabling revokes every open
// session of the target in the same transaction and no one else's.
func TestDisableUserRevokesOnlyTheTargetsSessions(t *testing.T) {
	f := newAdminFixture(t, nil, nil, nil)
	f.addSession(t, "session-1", "user-1", 1)
	f.addSession(t, "session-2", "user-1", 2)
	f.addSession(t, "session-3", "user-3", 3)
	ctx := context.Background()
	if _, err := f.service.DisableUser(ctx, githubActor(t, 3), "github:1"); err != nil {
		t.Fatal(err)
	}
	// Re-enable so the check below sees revocation, not just the disabled
	// flag: enabling must not resurrect the revoked sessions.
	if _, err := f.service.EnableUser(ctx, githubActor(t, 3), "github:1"); err != nil {
		t.Fatal(err)
	}
	if active := f.activeSession(t, 1); active {
		t.Fatal("target session 1 still active")
	}
	if active := f.activeSession(t, 2); active {
		t.Fatal("target session 2 still active")
	}
	if !f.activeSession(t, 3) {
		t.Fatal("the administrator's own session was revoked")
	}
	if n, _ := f.audit(t)[1].SessionCount(); n != 2 {
		t.Fatalf("audit session count = %d, want 2", n)
	}
}

func TestDisableUserLastAdministratorGuard(t *testing.T) {
	f := newAdminFixture(t, nil, nil, nil)
	ctx := context.Background()
	var last *domain.LastAdministratorError
	if _, err := f.service.DisableUser(ctx, githubActor(t, 3), "github:3"); !errors.As(err, &last) {
		t.Fatalf("disabling the only enabled admin = %v, want *LastAdministratorError", err)
	}
	if _, err := f.service.ChangeRole(ctx, githubActor(t, 3), "github:2", domain.RoleAdministrator); err != nil {
		t.Fatal(err)
	}
	// With a second admin, self-disable is allowed; then the remaining admin
	// is the last one and cannot disable themselves.
	if _, err := f.service.DisableUser(ctx, githubActor(t, 3), "github:3"); err != nil {
		t.Fatalf("self-disable with another admin = %v", err)
	}
	if _, err := f.service.DisableUser(ctx, githubActor(t, 2), "github:2"); !errors.Is(err, domain.ErrLastAdministrator) {
		t.Fatalf("disabling the new last admin = %v", err)
	}
	// The self-disabled admin can no longer act.
	if _, err := f.service.EnableUser(ctx, githubActor(t, 3), "github:3"); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("disabled admin enabling self = %v, want unauthenticated", err)
	}
}

func TestEnableUser(t *testing.T) {
	f := newAdminFixture(t, nil, nil, nil)
	ctx := context.Background()
	if _, err := f.service.EnableUser(ctx, githubActor(t, 2), "github:4"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("operator enable = %v, want forbidden", err)
	}
	if _, err := f.service.EnableUser(ctx, githubActor(t, 3), "github:99"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown target = %v, want not found", err)
	}
	same, err := f.service.EnableUser(ctx, githubActor(t, 3), "github:1")
	if err != nil || same.Disabled() || same.Version() != f.user(t, "user-1").Version() {
		t.Fatalf("enabling an enabled user = %+v, %v", same.Snapshot(), err)
	}
	if len(f.audit(t)) != 0 {
		t.Fatal("no-op enable wrote an audit event")
	}
	enabled, err := f.service.EnableUser(ctx, githubActor(t, 3), "github:4")
	if err != nil || enabled.Disabled() {
		t.Fatalf("enable = %+v, %v", enabled.Snapshot(), err)
	}
	if _, err := domain.ActorFor(f.user(t, "user-4")); err != nil {
		t.Fatalf("enabled user cannot act: %v", err)
	}
	events := f.audit(t)
	if len(events) != 1 {
		t.Fatalf("audit has %d events, want 1", len(events))
	}
	if e := events[0]; e.Action() != domain.AuthAuditUserEnabled || e.TargetUserID() != "user-4" || e.Actor() != githubActor(t, 3) {
		t.Fatalf("audit event = %+v", e.Snapshot())
	}
	// The re-enabled administrator counts again for the guard.
	if _, err := f.service.DisableUser(ctx, githubActor(t, 4), "github:3"); err != nil {
		t.Fatalf("re-enabled admin disabling the other admin = %v", err)
	}
}

func TestDisableEnableAuditFailureRollsBack(t *testing.T) {
	store := authUsers(t)
	f := newAdminFixture(t, nil, failingAudit{store}, store)
	f.addSession(t, "session-1", "user-1", 1)
	ctx := context.Background()
	if _, err := f.service.DisableUser(ctx, githubActor(t, 3), "github:1"); err == nil {
		t.Fatal("DisableUser succeeded without its audit event")
	}
	if f.user(t, "user-1").Disabled() || !f.activeSession(t, 1) {
		t.Fatal("disable or session revocation committed without its audit event")
	}
	if _, err := f.service.EnableUser(ctx, githubActor(t, 3), "github:4"); err == nil {
		t.Fatal("EnableUser succeeded without its audit event")
	}
	if !f.user(t, "user-4").Disabled() || len(f.audit(t)) != 0 {
		t.Fatal("enable committed without its audit event")
	}
}

// TestDisableUserRecheckInsideTransaction: a stale allow from the policy does
// not let a non-administrator disable or enable anyone.
func TestDisableUserRecheckInsideTransaction(t *testing.T) {
	f := newAdminFixture(t, allowAll{}, nil, nil)
	ctx := context.Background()
	if _, err := f.service.DisableUser(ctx, githubActor(t, 2), "github:1"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("operator with stale allow = %v, want forbidden", err)
	}
	if _, err := f.service.EnableUser(ctx, githubActor(t, 4), "github:4"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("disabled admin with stale allow = %v, want forbidden", err)
	}
	if f.user(t, "user-1").Disabled() || !f.user(t, "user-4").Disabled() || len(f.audit(t)) != 0 {
		t.Fatal("denied change was written")
	}
}

// activeSession reports whether the session created with addSession(fill)
// still resolves.
func (f adminFixture) activeSession(t *testing.T, fill byte) bool {
	t.Helper()
	hash, err := domain.NewSessionTokenHash(bytes.Repeat([]byte{fill}, domain.SessionTokenHashLength))
	if err != nil {
		t.Fatal(err)
	}
	err = f.store.WithinAuthTransaction(context.Background(), func(repos ports.AuthRepositories) error {
		_, _, err := repos.Sessions().ResolveActive(hash, userCreatedAt.Add(time.Minute))
		return err
	})
	if errors.Is(err, domain.ErrNoActiveSession) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}
