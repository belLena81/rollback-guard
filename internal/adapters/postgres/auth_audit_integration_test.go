//go:build integration

package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

func auditActor(t *testing.T, id string) domain.Actor {
	t.Helper()
	actor, err := domain.NewActor(domain.ActorID(id), domain.ActorHuman)
	if err != nil {
		t.Fatal(err)
	}
	return actor
}

// TestAuthAuditListPageWalksTiedTimestamps pages through events that share one
// timestamp: the (occurred_at, id) cursor must neither skip nor repeat them.
func TestAuthAuditListPageWalksTiedTimestamps(t *testing.T) {
	f := newAuthFixture(t)
	admin := auditActor(t, "github:3")
	var want []domain.AuthAuditEventID
	f.mustTx(t, func(repos ports.AuthRepositories) error {
		for i, id := range []domain.AuthAuditEventID{"audit-a", "audit-b", "audit-c", "audit-d", "audit-e"} {
			at := authNow
			if i >= 3 {
				at = authNow.Add(time.Second)
			}
			event, err := domain.NewLogoutAudit(id, admin, "user-1", at)
			if err != nil {
				return err
			}
			if err := repos.Audit().Append(event); err != nil {
				return err
			}
		}
		return nil
	})
	want = []domain.AuthAuditEventID{"audit-e", "audit-d", "audit-c", "audit-b", "audit-a"}

	var got []domain.AuthAuditEventID
	var cursor ports.AuthAuditCursor
	for range 5 {
		var page []domain.AuthAuditEvent
		err := f.uow.WithinAuthReadTransaction(t.Context(), func(r ports.AuthReadRepositories) error {
			var err error
			page, err = r.Audit().ListPage(cursor, 2)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, e := range page {
			got = append(got, e.ID())
		}
		last := page[len(page)-1]
		cursor = ports.AuthAuditCursor{OccurredAt: last.OccurredAt(), ID: last.ID()}
	}
	if len(got) != len(want) {
		t.Fatalf("paged ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("paged ids = %v, want %v", got, want)
		}
	}
}

func TestAuthAuditAppendListAndShape(t *testing.T) {
	f := newAuthFixture(t)
	admin := auditActor(t, "github:3")
	roleChanged, err := domain.NewRoleChangedAudit("audit-1", admin, "user-1", domain.RoleViewer, domain.RoleOperator, authNow)
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := domain.NewSessionsRevokedAudit("audit-2", admin, "user-1", 0, authNow.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	logout, err := domain.NewLogoutAudit("audit-3", admin, "user-3", authNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	f.mustTx(t, func(repos ports.AuthRepositories) error {
		for _, event := range []domain.AuthAuditEvent{roleChanged, revoked, logout} {
			if err := repos.Audit().Append(event); err != nil {
				return err
			}
		}
		return nil
	})
	var listed []domain.AuthAuditEvent
	f.mustTx(t, func(repos ports.AuthRepositories) error {
		var err error
		listed, err = repos.Audit().ListRecent(10)
		return err
	})
	if len(listed) != 3 || listed[0] != logout || listed[1] != revoked || listed[2] != roleChanged {
		t.Fatalf("listed = %v", listed)
	}

	err = f.tx(t, func(repos ports.AuthRepositories) error { return repos.Audit().Append(logout) })
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("duplicate append = %v, want already exists", err)
	}
	err = f.tx(t, func(repos ports.AuthRepositories) error { return repos.Audit().Append(domain.AuthAuditEvent{}) })
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("zero event append = %v, want validation", err)
	}
	err = f.tx(t, func(repos ports.AuthRepositories) error {
		_, err := repos.Audit().ListRecent(0)
		return err
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("ListRecent(0) = %v, want validation", err)
	}

	ctx := t.Context()
	for _, statement := range []string{
		`UPDATE auth_audit_events SET actor_id = 'github:9'`,
		`DELETE FROM auth_audit_events`,
	} {
		if _, err := f.shared.Exec(ctx, statement); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("%s error = %v, want append-only refusal", statement, err)
		}
	}
	// The shape constraint rejects rows no constructor builds.
	if _, err := f.shared.Exec(ctx, `
		INSERT INTO auth_audit_events (id, occurred_at, actor_id, action, target_user_id, to_role)
		VALUES ('audit-bad', now(), 'github:3', 'logout', 'user-3', 'administrator')`); err == nil {
		t.Fatal("logout row with a role was accepted")
	}
}

func TestAuthAuditMigrationHasNoSecretColumns(t *testing.T) {
	f := newAuthFixture(t)
	rows, err := f.shared.Query(t.Context(), `
		SELECT column_name FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'auth_audit_events' ORDER BY ordinal_position`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, name)
	}
	want := "id,occurred_at,actor_id,action,target_user_id,from_role,to_role,session_count"
	if got := strings.Join(columns, ","); got != want {
		t.Fatalf("columns = %s, want %s", got, want)
	}
}

func TestAuthAuditMigrationDown(t *testing.T) {
	f := newAuthFixture(t)
	ctx := t.Context()
	down, err := os.ReadFile("../../../migrations/000006_auth_audit_events.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, err := os.ReadFile("../../../migrations/000006_auth_audit_events.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	event, err := domain.NewLogoutAudit("audit-1", auditActor(t, "github:1"), "user-1", authNow)
	if err != nil {
		t.Fatal(err)
	}
	f.mustTx(t, func(repos ports.AuthRepositories) error { return repos.Audit().Append(event) })
	if _, err := f.shared.Exec(ctx, string(down)); err == nil || !strings.Contains(err.Error(), "cannot remove auth audit migration") {
		t.Fatalf("down with rows = %v, want refusal", err)
	}
	if _, err := f.shared.Exec(ctx, `TRUNCATE auth_audit_events`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.shared.Exec(ctx, string(down)); err != nil {
		t.Fatalf("down on empty table: %v", err)
	}
	if _, err := f.shared.Exec(ctx, `SELECT count(*) FROM users`); err != nil {
		t.Fatalf("down touched earlier migrations: %v", err)
	}
	if _, err := f.shared.Exec(ctx, string(up)); err != nil {
		t.Fatalf("up after down: %v", err)
	}
}

type pgAuditIDs struct {
	mu sync.Mutex
	n  int
}

func (g *pgAuditIDs) NewID(prefix string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return prefix + "-" + strings.Repeat("x", g.n)
}

type pgAuditClock struct{}

func (pgAuditClock) Now() time.Time { return authNow.Add(time.Minute) }

func seedAuthUser(t *testing.T, f authFixture, id domain.UserID, githubID int64, login string, role domain.Role) {
	t.Helper()
	user, err := domain.NewUser(id, githubID, login, "", nil, role, authNow)
	if err != nil {
		t.Fatal(err)
	}
	f.mustTx(t, func(repos ports.AuthRepositories) error { return repos.Users().Create(user) })
}

// TestConcurrentAdminDemotionsKeepOneAdministrator: two administrators
// demote each other at the same time. The row locks serialize them, so at
// most one succeeds and one administrator always remains.
func TestConcurrentAdminDemotionsKeepOneAdministrator(t *testing.T) {
	for round := range 5 {
		f := newAuthFixture(t)
		seedAuthUser(t, f, "admin-a", 501, "admin-a", domain.RoleAdministrator)
		seedAuthUser(t, f, "admin-b", 502, "admin-b", domain.RoleAdministrator)
		policy, err := application.NewRoleAccessPolicy(f.uow)
		if err != nil {
			t.Fatal(err)
		}
		service, err := application.NewAdminService(application.AdminDependencies{UnitOfWork: f.uow, Access: policy, Clock: pgAuditClock{}, IDGenerator: &pgAuditIDs{}})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, pair := range [][2]string{{"github:501", "github:502"}, {"github:502", "github:501"}} {
			wg.Go(func() {
				_, errs[i] = service.ChangeRole(ctx, auditActor(t, pair[0]), domain.ActorID(pair[1]), domain.RoleOperator)
			})
		}
		wg.Wait()
		cancel()
		var admins []domain.UserID
		f.mustTx(t, func(repos ports.AuthRepositories) error {
			var err error
			admins, err = repos.Users().LockActiveAdministrators()
			return err
		})
		if len(admins) != 1 {
			t.Fatalf("round %d: %d administrators remain (errors %v)", round, len(admins), errs)
		}
		for _, err := range errs {
			if err != nil && !errors.Is(err, domain.ErrForbidden) && !errors.Is(err, domain.ErrLastAdministrator) {
				t.Fatalf("round %d: unexpected error %v", round, err)
			}
		}
	}
}
