//go:build integration

package postgres

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

func newPostgresAdminService(t *testing.T, f authFixture) *application.AdminService {
	t.Helper()
	policy, err := application.NewRoleAccessPolicy(f.uow)
	if err != nil {
		t.Fatal(err)
	}
	service, err := application.NewAdminService(application.AdminDependencies{UnitOfWork: f.uow, Access: policy, Clock: pgAuditClock{}, IDGenerator: &pgAuditIDs{}})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// activeOnPostgres reports whether the session created with authHash(fill)
// still resolves.
func activeOnPostgres(t *testing.T, f authFixture, fill byte) bool {
	t.Helper()
	err := f.tx(t, func(repos ports.AuthRepositories) error {
		_, _, err := repos.Sessions().ResolveActive(authHash(t, fill), authNow.Add(2*time.Minute))
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

// TestAdminDisableEnableUserOnPostgres is the Postgres parity of the memory
// AdminService tests: disable revokes the target's sessions and writes one
// user_disabled row atomically, the last-administrator guard holds, no-ops
// write nothing, and enable writes one user_enabled row.
func TestAdminDisableEnableUserOnPostgres(t *testing.T) {
	f := newAuthFixture(t)
	ctx := t.Context()
	seedAuthUser(t, f, "user-viewer", 601, "viewer", domain.RoleViewer)
	seedAuthUser(t, f, "user-operator", 602, "operator", domain.RoleOperator)
	seedAuthUser(t, f, "user-admin", 603, "admin", domain.RoleAdministrator)
	f.mustTx(t, func(repos ports.AuthRepositories) error {
		for i, s := range []domain.Session{
			authSession(t, "s-1", "user-viewer", 1, time.Hour),
			authSession(t, "s-2", "user-viewer", 2, time.Hour),
			authSession(t, "s-3", "user-admin", 3, time.Hour),
		} {
			if err := repos.Sessions().Create(s); err != nil {
				return fmt.Errorf("session %d: %w", i, err)
			}
		}
		return nil
	})
	service := newPostgresAdminService(t, f)
	admin := auditActor(t, "github:603")
	if _, err := service.DisableUser(ctx, auditActor(t, "github:602"), "github:601"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("operator disable = %v, want forbidden", err)
	}
	if _, err := service.DisableUser(ctx, admin, "github:999"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown target = %v, want not found", err)
	}
	if _, err := service.DisableUser(ctx, admin, "github:603"); !errors.Is(err, domain.ErrLastAdministrator) {
		t.Fatalf("last admin self-disable = %v, want last administrator", err)
	}
	disabled, err := service.DisableUser(ctx, admin, "github:601")
	if err != nil || !disabled.Disabled() {
		t.Fatalf("disable = %+v, %v", disabled.Snapshot(), err)
	}
	if _, err := service.DisableUser(ctx, admin, "github:601"); err != nil {
		t.Fatalf("second disable = %v", err)
	}
	if _, err := service.EnableUser(ctx, admin, "github:601"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.EnableUser(ctx, admin, "github:601"); err != nil {
		t.Fatalf("second enable = %v", err)
	}
	// Re-enabled, so a still-active session would resolve again.
	if activeOnPostgres(t, f, 1) || activeOnPostgres(t, f, 2) || !activeOnPostgres(t, f, 3) {
		t.Fatal("disable must revoke exactly the target's sessions")
	}
	var listed []domain.AuthAuditEvent
	f.mustTx(t, func(repos ports.AuthRepositories) error {
		var err error
		listed, err = repos.Audit().ListRecent(10)
		return err
	})
	if len(listed) != 2 {
		t.Fatalf("audit rows = %v, want one user_disabled and one user_enabled", auditActions(listed))
	}
	byAction := map[domain.AuthAuditAction]domain.AuthAuditEvent{}
	for _, e := range listed {
		byAction[e.Action()] = e
	}
	if e := byAction[domain.AuthAuditUserDisabled]; e.TargetUserID() != "user-viewer" || e.Actor() != admin {
		t.Fatalf("user_disabled row = %+v", e.Snapshot())
	}
	if n, ok := byAction[domain.AuthAuditUserDisabled].SessionCount(); !ok || n != 2 {
		t.Fatalf("user_disabled session count = %d %v, want 2", n, ok)
	}
	if e := byAction[domain.AuthAuditUserEnabled]; e.TargetUserID() != "user-viewer" || e.Actor() != admin {
		t.Fatalf("user_enabled row = %+v", e.Snapshot())
	}
	// The shape constraint rejects a user_enabled row with a session count.
	if _, err := f.shared.Exec(ctx, `
		INSERT INTO auth_audit_events (id, occurred_at, actor_id, action, target_user_id, session_count)
		VALUES ('audit-bad', now(), 'github:603', 'user_enabled', 'user-viewer', 1)`); err == nil {
		t.Fatal("user_enabled row with a session count was accepted")
	}
}

// TestDisableAuditFailureRollsBackOnPostgres: when the audit insert fails,
// the disable and the session revocation roll back with it.
func TestDisableAuditFailureRollsBackOnPostgres(t *testing.T) {
	f := newAuthFixture(t)
	seedAuthUser(t, f, "user-viewer", 611, "viewer", domain.RoleViewer)
	seedAuthUser(t, f, "user-admin", 613, "admin", domain.RoleAdministrator)
	f.mustTx(t, func(repos ports.AuthRepositories) error {
		return repos.Sessions().Create(authSession(t, "s-1", "user-viewer", 1, time.Hour))
	})
	// pgAuditIDs hands out "auth-audit-x" first; occupy it so the insert
	// fails on the primary key after the user and session changes.
	seed, err := domain.NewLogoutAudit("auth-audit-x", auditActor(t, "github:613"), "user-admin", authNow)
	if err != nil {
		t.Fatal(err)
	}
	f.mustTx(t, func(repos ports.AuthRepositories) error { return repos.Audit().Append(seed) })
	service := newPostgresAdminService(t, f)
	if _, err := service.DisableUser(t.Context(), auditActor(t, "github:613"), "github:611"); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("disable = %v, want the audit insert's already-exists failure", err)
	}
	f.mustTx(t, func(repos ports.AuthRepositories) error {
		user, err := repos.Users().GetByID("user-viewer")
		if err != nil {
			return err
		}
		if user.Disabled() {
			return errors.New("user disabled without an audit row")
		}
		return nil
	})
	if !activeOnPostgres(t, f, 1) {
		t.Fatal("session revoked without an audit row")
	}
}

func TestAuthAuditUserStatusMigrationDown(t *testing.T) {
	f := newAuthFixture(t)
	ctx := t.Context()
	down, err := os.ReadFile("../../../migrations/000007_auth_audit_user_status.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, err := os.ReadFile("../../../migrations/000007_auth_audit_user_status.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	logout, err := domain.NewLogoutAudit("audit-1", auditActor(t, "github:1"), "user-1", authNow)
	if err != nil {
		t.Fatal(err)
	}
	enabled, err := domain.NewUserEnabledAudit("audit-2", auditActor(t, "github:1"), "user-2", authNow)
	if err != nil {
		t.Fatal(err)
	}
	f.mustTx(t, func(repos ports.AuthRepositories) error {
		if err := repos.Audit().Append(logout); err != nil {
			return err
		}
		return repos.Audit().Append(enabled)
	})
	if _, err := f.shared.Exec(ctx, string(down)); err == nil || !strings.Contains(err.Error(), "cannot remove user status audit actions") {
		t.Fatalf("down with user_enabled rows = %v, want refusal", err)
	}
	// Only rows of the new actions block the down migration.
	for _, statement := range []string{
		`ALTER TABLE auth_audit_events DISABLE TRIGGER auth_audit_events_append_only`,
		`DELETE FROM auth_audit_events WHERE action = 'user_enabled'`,
		`ALTER TABLE auth_audit_events ENABLE TRIGGER auth_audit_events_append_only`,
	} {
		if _, err := f.shared.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if _, err := f.shared.Exec(ctx, string(down)); err != nil {
		t.Fatalf("down with only earlier actions: %v", err)
	}
	if _, err := f.shared.Exec(ctx, `
		INSERT INTO auth_audit_events (id, occurred_at, actor_id, action, target_user_id)
		VALUES ('audit-3', now(), 'github:1', 'user_enabled', 'user-2')`); err == nil {
		t.Fatal("user_enabled accepted after down")
	}
	if _, err := f.shared.Exec(ctx, string(up)); err != nil {
		t.Fatalf("up after down: %v", err)
	}
}

func auditActions(events []domain.AuthAuditEvent) []string {
	actions := make([]string, 0, len(events))
	for _, e := range events {
		actions = append(actions, e.Action().String())
	}
	return actions
}
