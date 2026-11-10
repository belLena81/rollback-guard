package application

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/memory"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

// userOf maps the fixture's public actor IDs to internal IDs (github:N is
// user-N in authUsers).
func userOf(target domain.ActorID) domain.UserID {
	return domain.UserID("user-" + strings.TrimPrefix(string(target), "github:"))
}

type counterIDs struct{ n int }

func (g *counterIDs) NewID(prefix string) string {
	g.n++
	return fmt.Sprintf("%s-%d", prefix, g.n)
}

type adminFixture struct {
	store   *memory.AuthUnitOfWork
	service *AdminService
}

func newAdminFixture(t *testing.T, access AccessPolicy, uow ports.AuthUnitOfWork, store *memory.AuthUnitOfWork) adminFixture {
	t.Helper()
	if store == nil {
		store = authUsers(t)
	}
	if uow == nil {
		uow = store
	}
	if access == nil {
		access = newRolePolicy(t, store)
	}
	service, err := NewAdminService(AdminDependencies{UnitOfWork: uow, Access: access, Clock: nilClock{}, IDGenerator: &counterIDs{}})
	if err != nil {
		t.Fatal(err)
	}
	return adminFixture{store: store, service: service}
}

func (f adminFixture) user(t *testing.T, id domain.UserID) domain.User {
	t.Helper()
	var user domain.User
	err := f.store.WithinAuthTransaction(context.Background(), func(repos ports.AuthRepositories) error {
		var err error
		user, err = repos.Users().GetByID(id)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return user
}

func (f adminFixture) audit(t *testing.T) []domain.AuthAuditEvent {
	t.Helper()
	var events []domain.AuthAuditEvent
	err := f.store.WithinAuthTransaction(context.Background(), func(repos ports.AuthRepositories) error {
		var err error
		events, err = repos.Audit().ListRecent(100)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func (f adminFixture) addSession(t *testing.T, id domain.SessionID, user domain.UserID, fill byte) {
	t.Helper()
	hash, err := domain.NewSessionTokenHash(bytes.Repeat([]byte{fill}, domain.SessionTokenHashLength))
	if err != nil {
		t.Fatal(err)
	}
	session, err := domain.NewSession(id, user, hash, userCreatedAt, userCreatedAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	err = f.store.WithinAuthTransaction(context.Background(), func(repos ports.AuthRepositories) error {
		return repos.Sessions().Create(session)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNewAdminServiceRequiresDependencies(t *testing.T) {
	full := AdminDependencies{UnitOfWork: memory.NewAuthUnitOfWork(), Access: allowAll{}, Clock: nilClock{}, IDGenerator: &counterIDs{}}
	for name, mutate := range map[string]func(*AdminDependencies){
		"uow":    func(d *AdminDependencies) { d.UnitOfWork = nil },
		"access": func(d *AdminDependencies) { d.Access = nil },
		"clock":  func(d *AdminDependencies) { d.Clock = nil },
		"ids":    func(d *AdminDependencies) { d.IDGenerator = nil },
	} {
		deps := full
		mutate(&deps)
		if _, err := NewAdminService(deps); err == nil {
			t.Errorf("nil %s accepted", name)
		}
	}
}

func TestChangeRole(t *testing.T) {
	tests := []struct {
		name     string
		actor    int64
		target   domain.ActorID
		role     domain.Role
		wantErr  error
		wantRole domain.Role
		audited  bool
	}{
		{name: "admin promotes viewer", actor: 3, target: "github:1", role: domain.RoleOperator, wantRole: domain.RoleOperator, audited: true},
		{name: "admin promotes to administrator", actor: 3, target: "github:2", role: domain.RoleAdministrator, wantRole: domain.RoleAdministrator, audited: true},
		{name: "same role is a no-op", actor: 3, target: "github:1", role: domain.RoleViewer, wantRole: domain.RoleViewer},
		{name: "operator forbidden", actor: 2, target: "github:1", role: domain.RoleOperator, wantErr: domain.ErrForbidden, wantRole: domain.RoleViewer},
		{name: "viewer forbidden", actor: 1, target: "github:1", role: domain.RoleAdministrator, wantErr: domain.ErrForbidden, wantRole: domain.RoleViewer},
		{name: "disabled admin unauthenticated", actor: 4, target: "github:1", role: domain.RoleOperator, wantErr: domain.ErrUnauthenticated, wantRole: domain.RoleViewer},
		{name: "unknown target", actor: 3, target: "github:99", role: domain.RoleOperator, wantErr: domain.ErrNotFound},
		{name: "invalid role", actor: 3, target: "github:1", role: domain.Role(9), wantErr: domain.ErrValidation, wantRole: domain.RoleViewer},
		{name: "last admin cannot demote self", actor: 3, target: "github:3", role: domain.RoleOperator, wantErr: domain.ErrLastAdministrator, wantRole: domain.RoleAdministrator},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAdminFixture(t, nil, nil, nil)
			_, err := f.service.ChangeRole(context.Background(), githubActor(t, tt.actor), tt.target, tt.role)
			if tt.wantErr == nil && err != nil || tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("ChangeRole = %v, want %v", err, tt.wantErr)
			}
			if tt.wantRole != 0 {
				if got := f.user(t, userOf(tt.target)).Role(); got != tt.wantRole {
					t.Fatalf("role = %s, want %s", got, tt.wantRole)
				}
			}
			events := f.audit(t)
			if !tt.audited {
				if len(events) != 0 {
					t.Fatalf("audit = %v, want none", events)
				}
				return
			}
			if len(events) != 1 {
				t.Fatalf("audit has %d events, want 1", len(events))
			}
			e := events[0]
			if e.Action() != domain.AuthAuditRoleChanged || e.Actor() != githubActor(t, tt.actor) || e.TargetUserID() != userOf(tt.target) || e.ToRole() != tt.role || e.FromRole() == tt.role {
				t.Fatalf("audit event = %+v", e.Snapshot())
			}
		})
	}
}

func TestChangeRoleLastAdministratorGuard(t *testing.T) {
	f := newAdminFixture(t, nil, nil, nil)
	ctx := context.Background()
	admin := githubActor(t, 3)
	// The disabled administrator (user-4) does not count.
	if _, err := f.service.ChangeRole(ctx, admin, "github:3", domain.RoleViewer); !errors.Is(err, domain.ErrLastAdministrator) {
		t.Fatalf("demoting the only enabled admin = %v, want last-administrator", err)
	}
	var last *domain.LastAdministratorError
	if _, err := f.service.ChangeRole(ctx, admin, "github:3", domain.RoleViewer); !errors.As(err, &last) {
		t.Fatalf("typed error lost through wrapping: %v", err)
	}
	if _, err := f.service.ChangeRole(ctx, admin, "github:2", domain.RoleAdministrator); err != nil {
		t.Fatal(err)
	}
	// With a second admin, self-demotion is allowed; then the new admin is
	// the last one and cannot be demoted by anyone.
	if _, err := f.service.ChangeRole(ctx, admin, "github:3", domain.RoleOperator); err != nil {
		t.Fatalf("self-demotion with another admin = %v", err)
	}
	if _, err := f.service.ChangeRole(ctx, githubActor(t, 2), "github:2", domain.RoleOperator); !errors.Is(err, domain.ErrLastAdministrator) {
		t.Fatalf("demoting the new last admin = %v", err)
	}
	if got := len(f.audit(t)); got != 2 {
		t.Fatalf("audit has %d events, want 2", got)
	}
}

// TestAdminRecheckInsideTransaction: the policy said yes (stale), but the
// actor is not an administrator when the transaction runs.
func TestAdminRecheckInsideTransaction(t *testing.T) {
	f := newAdminFixture(t, allowAll{}, nil, nil)
	ctx := context.Background()
	if _, err := f.service.ChangeRole(ctx, githubActor(t, 2), "github:1", domain.RoleAdministrator); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("operator with stale allow = %v, want forbidden", err)
	}
	if _, err := f.service.RevokeUserSessions(ctx, githubActor(t, 4), "github:1"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("disabled admin with stale allow = %v, want forbidden", err)
	}
	if _, err := f.service.ChangeRole(ctx, mustActor(t, "local-admin", domain.ActorHuman), "github:1", domain.RoleOperator); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("non-github actor = %v, want forbidden", err)
	}
	if f.user(t, "user-1").Role() != domain.RoleViewer || len(f.audit(t)) != 0 {
		t.Fatal("denied change was written")
	}
}

// failingAudit wraps a memory store so every audit append fails after the
// user and session changes in the same transaction were made.
type failingAudit struct{ inner *memory.AuthUnitOfWork }

func (u failingAudit) WithinAuthTransaction(ctx context.Context, fn func(ports.AuthRepositories) error) error {
	return u.inner.WithinAuthTransaction(ctx, func(repos ports.AuthRepositories) error {
		return fn(failingAuditRepos{repos})
	})
}

type failingAuditRepos struct{ ports.AuthRepositories }

func (r failingAuditRepos) Audit() ports.AuthAuditRepository { return failingAuditRepo{} }

type failingAuditRepo struct{}

func (failingAuditRepo) Append(domain.AuthAuditEvent) error { return errors.New("audit write failed") }
func (failingAuditRepo) ListRecent(int) ([]domain.AuthAuditEvent, error) {
	return nil, errors.New("unused")
}

func TestAuditFailureRollsBackTheChange(t *testing.T) {
	store := authUsers(t)
	f := newAdminFixture(t, nil, failingAudit{store}, store)
	f.addSession(t, "session-1", "user-1", 1)
	ctx := context.Background()
	if _, err := f.service.ChangeRole(ctx, githubActor(t, 3), "github:1", domain.RoleOperator); err == nil {
		t.Fatal("ChangeRole succeeded without its audit event")
	}
	if f.user(t, "user-1").Role() != domain.RoleViewer {
		t.Fatal("role change committed without its audit event")
	}
	if _, err := f.service.RevokeUserSessions(ctx, githubActor(t, 3), "github:1"); err == nil {
		t.Fatal("RevokeUserSessions succeeded without its audit event")
	}
	principal := principalFor(t, f.user(t, "user-1"), "session-1")
	if err := f.service.Logout(ctx, principal); err == nil {
		t.Fatal("Logout succeeded without its audit event")
	}
	err := store.WithinAuthTransaction(ctx, func(repos ports.AuthRepositories) error {
		hash, _ := domain.NewSessionTokenHash(bytes.Repeat([]byte{1}, domain.SessionTokenHashLength))
		_, _, err := repos.Sessions().ResolveActive(hash, userCreatedAt.Add(time.Minute))
		return err
	})
	if err != nil {
		t.Fatalf("session revoked without its audit event: %v", err)
	}
	if len(f.audit(t)) != 0 {
		t.Fatal("audit rows written by rolled-back transactions")
	}
}

func principalFor(t *testing.T, user domain.User, session domain.SessionID) domain.Principal {
	t.Helper()
	principal, err := domain.NewPrincipal(user, session)
	if err != nil {
		t.Fatal(err)
	}
	return principal
}

func TestRevokeUserSessions(t *testing.T) {
	f := newAdminFixture(t, nil, nil, nil)
	f.addSession(t, "session-1", "user-1", 1)
	f.addSession(t, "session-2", "user-1", 2)
	f.addSession(t, "session-3", "user-2", 3)
	ctx := context.Background()
	if _, err := f.service.RevokeUserSessions(ctx, githubActor(t, 2), "github:1"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("operator revoke = %v, want forbidden", err)
	}
	if _, err := f.service.RevokeUserSessions(ctx, githubActor(t, 3), "github:99"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown target = %v, want not found", err)
	}
	count, err := f.service.RevokeUserSessions(ctx, githubActor(t, 3), "github:1")
	if err != nil || count != 2 {
		t.Fatalf("revoke = %d, %v; want 2", count, err)
	}
	again, err := f.service.RevokeUserSessions(ctx, githubActor(t, 3), "github:1")
	if err != nil || again != 0 {
		t.Fatalf("second revoke = %d, %v; want 0", again, err)
	}
	events := f.audit(t)
	if len(events) != 2 {
		t.Fatalf("audit has %d events, want 2", len(events))
	}
	for _, e := range events {
		if n, ok := e.SessionCount(); e.Action() != domain.AuthAuditSessionsRevoked || !ok || (n != 2 && n != 0) || e.TargetUserID() != "user-1" || e.Actor() != githubActor(t, 3) {
			t.Fatalf("audit event = %+v", e.Snapshot())
		}
	}
}

func TestLogout(t *testing.T) {
	f := newAdminFixture(t, nil, nil, nil)
	f.addSession(t, "session-1", "user-1", 1)
	ctx := context.Background()
	if err := f.service.Logout(ctx, domain.Principal{}); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("zero principal = %v, want unauthenticated", err)
	}
	if err := f.service.Logout(ctx, principalFor(t, f.user(t, "user-1"), "session-1")); err != nil {
		t.Fatal(err)
	}
	err := f.store.WithinAuthTransaction(ctx, func(repos ports.AuthRepositories) error {
		hash, _ := domain.NewSessionTokenHash(bytes.Repeat([]byte{1}, domain.SessionTokenHashLength))
		_, _, err := repos.Sessions().ResolveActive(hash, userCreatedAt.Add(time.Minute))
		return err
	})
	if !errors.Is(err, domain.ErrNoActiveSession) {
		t.Fatalf("session still active after logout: %v", err)
	}
	events := f.audit(t)
	if len(events) != 1 || events[0].Action() != domain.AuthAuditLogout || events[0].Actor() != githubActor(t, 1) || events[0].TargetUserID() != "user-1" {
		t.Fatalf("audit = %v", events)
	}
}
