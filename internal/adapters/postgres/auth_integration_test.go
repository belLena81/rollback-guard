//go:build integration

package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/jackc/pgx/v5/pgxpool"
)

var allUpMigrations = []string{
	"000001_initial_schema.up.sql",
	"000002_rollback_mutation_ledger.up.sql",
	"000003_a2a_investigation.up.sql",
	"000004_ai_diagnosis.up.sql",
	"000005_auth_users_sessions.up.sql",
}

var authNow = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

type authFixture struct {
	shared *pgxpool.Pool
	uow    *AuthUnitOfWork
}

func newAuthFixture(t *testing.T) authFixture {
	t.Helper()
	shared := isolatedSchemaPool(t, allUpMigrations...)
	uow, err := NewAuthUnitOfWork(repositoryPool(t, shared, 4))
	if err != nil {
		t.Fatal(err)
	}
	return authFixture{shared: shared, uow: uow}
}

func (f authFixture) tx(t *testing.T, fn func(ports.AuthRepositories) error) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	return f.uow.WithinAuthTransaction(ctx, fn)
}

func (f authFixture) mustTx(t *testing.T, fn func(ports.AuthRepositories) error) {
	t.Helper()
	if err := f.tx(t, fn); err != nil {
		t.Fatal(err)
	}
}

func authUser(t *testing.T, id domain.UserID, githubID int64, login string) domain.User {
	t.Helper()
	user, err := domain.NewUser(id, githubID, login, login+"@example.com", []string{"acme"}, domain.RoleOperator, authNow)
	if err != nil {
		t.Fatal(err)
	}
	return user
}

func authHash(t *testing.T, fill byte) domain.SessionTokenHash {
	t.Helper()
	hash, err := domain.NewSessionTokenHash(bytes.Repeat([]byte{fill}, domain.SessionTokenHashLength))
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func authSession(t *testing.T, id domain.SessionID, userID domain.UserID, fill byte, ttl time.Duration) domain.Session {
	t.Helper()
	session, err := domain.NewSession(id, userID, authHash(t, fill), authNow, authNow.Add(ttl))
	if err != nil {
		t.Fatal(err)
	}
	return session
}

// assertNoHashLeak fails when err's message carries the digest in any of
// the encodings PostgreSQL or Go would use.
func assertNoHashLeak(t *testing.T, err error, fill byte) {
	t.Helper()
	if err == nil {
		return
	}
	message := strings.ToLower(err.Error())
	hexed := strings.Repeat(fmt.Sprintf("%02x", fill), 4)
	if strings.Contains(message, hexed) || strings.Contains(message, "token_hash") || strings.Contains(err.Error(), string([]byte{fill, fill})) {
		t.Fatalf("error leaks the token hash: %q", err.Error())
	}
}

func TestAuthUserRepositoryRoundTripAndLookups(t *testing.T) {
	f := newAuthFixture(t)
	user := authUser(t, "user-1", 101, "Octo-Cat")
	f.mustTx(t, func(r ports.AuthRepositories) error { return r.Users().Create(user) })

	f.mustTx(t, func(r ports.AuthRepositories) error {
		byID, err := r.Users().GetByID("user-1")
		if err != nil {
			return err
		}
		byGitHub, err := r.Users().GetByGitHubID(101)
		if err != nil {
			return err
		}
		for _, got := range []domain.User{byID, byGitHub} {
			s, want := got.Snapshot(), user.Snapshot()
			if s.ID != want.ID || s.GitHubID != want.GitHubID || s.Login != want.Login || s.Email != want.Email ||
				strings.Join(s.Organizations, ",") != "acme" || s.Role != want.Role || s.Disabled ||
				s.Version != 1 || !s.CreatedAt.Equal(want.CreatedAt) || !s.UpdatedAt.Equal(want.UpdatedAt) {
				t.Fatalf("stored user = %+v, want %+v", s, want)
			}
		}
		return nil
	})

	f.mustTx(t, func(r ports.AuthRepositories) error {
		var notFound *domain.NotFoundError
		if _, err := r.Users().GetByID("missing"); !errors.As(err, &notFound) {
			t.Fatalf("GetByID(missing) error = %v, want NotFoundError", err)
		}
		if _, err := r.Users().GetByGitHubID(999); !errors.As(err, &notFound) {
			t.Fatalf("GetByGitHubID(missing) error = %v, want NotFoundError", err)
		}
		return nil
	})
}

func TestAuthUserRepositoryEmptyOrganizationsAndEmail(t *testing.T) {
	f := newAuthFixture(t)
	user, err := domain.NewUser("user-1", 7, "solo", "", nil, domain.RoleViewer, authNow)
	if err != nil {
		t.Fatal(err)
	}
	f.mustTx(t, func(r ports.AuthRepositories) error { return r.Users().Create(user) })
	f.mustTx(t, func(r ports.AuthRepositories) error {
		got, err := r.Users().GetByID("user-1")
		if err != nil {
			return err
		}
		if got.Email() != "" || len(got.Organizations()) != 0 {
			t.Fatalf("stored user = %+v, want empty email and organizations", got.Snapshot())
		}
		return nil
	})
}

func TestAuthUserRepositoryRejectsDuplicates(t *testing.T) {
	tests := []struct {
		name string
		user func(*testing.T) domain.User
	}{
		{name: "same id", user: func(t *testing.T) domain.User { return authUser(t, "user-1", 202, "other") }},
		{name: "same github id", user: func(t *testing.T) domain.User { return authUser(t, "user-2", 101, "other") }},
		{name: "same login", user: func(t *testing.T) domain.User { return authUser(t, "user-2", 202, "octo") }},
		{name: "login differing in case", user: func(t *testing.T) domain.User { return authUser(t, "user-2", 202, "OCTO") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAuthFixture(t)
			f.mustTx(t, func(r ports.AuthRepositories) error { return r.Users().Create(authUser(t, "user-1", 101, "octo")) })
			err := f.tx(t, func(r ports.AuthRepositories) error { return r.Users().Create(tt.user(t)) })
			var exists *domain.AlreadyExistsError
			if !errors.As(err, &exists) {
				t.Fatalf("Create() error = %v, want AlreadyExistsError", err)
			}
		})
	}
}

func TestAuthUserRepositoryUpdate(t *testing.T) {
	f := newAuthFixture(t)
	user := authUser(t, "user-1", 101, "octo")
	f.mustTx(t, func(r ports.AuthRepositories) error {
		if err := r.Users().Create(user); err != nil {
			return err
		}
		return r.Users().Create(authUser(t, "user-2", 202, "taken"))
	})

	promoted, err := user.WithRole(domain.RoleAdministrator, authNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := promoted.Disable(authNow.Add(2 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	f.mustTx(t, func(r ports.AuthRepositories) error {
		if err := r.Users().Update(promoted, 1); err != nil {
			return err
		}
		return r.Users().Update(disabled, 2)
	})
	f.mustTx(t, func(r ports.AuthRepositories) error {
		got, err := r.Users().GetByID("user-1")
		if err != nil {
			return err
		}
		if got.Role() != domain.RoleAdministrator || !got.Disabled() || got.Version() != 3 || !got.UpdatedAt().Equal(authNow.Add(2*time.Minute)) {
			t.Fatalf("updated user = %+v", got.Snapshot())
		}
		return nil
	})

	t.Run("stale version", func(t *testing.T) {
		stale, err := user.WithRole(domain.RoleViewer, authNow.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		err = f.tx(t, func(r ports.AuthRepositories) error { return r.Users().Update(stale, 1) })
		var conflict *domain.VersionConflictError
		if !errors.As(err, &conflict) || conflict.Expected != 1 || conflict.Actual != 3 {
			t.Fatalf("Update(stale) error = %v, want VersionConflictError expected 1 actual 3", err)
		}
	})

	t.Run("missing user", func(t *testing.T) {
		ghost := authUser(t, "ghost", 303, "ghost")
		err := f.tx(t, func(r ports.AuthRepositories) error { return r.Users().Update(ghost, 1) })
		var notFound *domain.NotFoundError
		if !errors.As(err, &notFound) {
			t.Fatalf("Update(missing) error = %v, want NotFoundError", err)
		}
	})

	t.Run("login collision", func(t *testing.T) {
		snapshot := disabled.Snapshot()
		snapshot.Login = "Taken"
		snapshot.Version = 4
		renamed, err := domain.RehydrateUser(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		err = f.tx(t, func(r ports.AuthRepositories) error { return r.Users().Update(renamed, 3) })
		var exists *domain.AlreadyExistsError
		if !errors.As(err, &exists) {
			t.Fatalf("Update(login collision) error = %v, want AlreadyExistsError", err)
		}
	})
}

func TestAuthSessionCreateRejectsDuplicatesAndUnknownUser(t *testing.T) {
	f := newAuthFixture(t)
	f.mustTx(t, func(r ports.AuthRepositories) error {
		if err := r.Users().Create(authUser(t, "user-1", 101, "octo")); err != nil {
			return err
		}
		return r.Sessions().Create(authSession(t, "session-1", "user-1", 0xab, time.Hour))
	})

	duplicateHash := f.tx(t, func(r ports.AuthRepositories) error {
		return r.Sessions().Create(authSession(t, "session-2", "user-1", 0xab, time.Hour))
	})
	var exists *domain.AlreadyExistsError
	if !errors.As(duplicateHash, &exists) {
		t.Fatalf("Create(duplicate hash) error = %v, want AlreadyExistsError", duplicateHash)
	}
	assertNoHashLeak(t, duplicateHash, 0xab)

	duplicateID := f.tx(t, func(r ports.AuthRepositories) error {
		return r.Sessions().Create(authSession(t, "session-1", "user-1", 0xcd, time.Hour))
	})
	if !errors.As(duplicateID, &exists) {
		t.Fatalf("Create(duplicate id) error = %v, want AlreadyExistsError", duplicateID)
	}

	unknownUser := f.tx(t, func(r ports.AuthRepositories) error {
		return r.Sessions().Create(authSession(t, "session-3", "ghost", 0xef, time.Hour))
	})
	var notFound *domain.NotFoundError
	if !errors.As(unknownUser, &notFound) || notFound.Resource != "user" {
		t.Fatalf("Create(unknown user) error = %v, want user NotFoundError", unknownUser)
	}
	assertNoHashLeak(t, unknownUser, 0xef)
}

func TestAuthResolveActiveSession(t *testing.T) {
	f := newAuthFixture(t)
	revokedSession := authSession(t, "session-revoked", "user-1", 0x02, time.Hour)
	f.mustTx(t, func(r ports.AuthRepositories) error {
		if err := r.Users().Create(authUser(t, "user-1", 101, "octo")); err != nil {
			return err
		}
		disabled, err := authUser(t, "user-2", 202, "gone").Disable(authNow)
		if err != nil {
			return err
		}
		if err := r.Users().Create(disabled); err != nil {
			return err
		}
		for _, session := range []domain.Session{
			authSession(t, "session-active", "user-1", 0x01, time.Hour),
			revokedSession,
			authSession(t, "session-disabled", "user-2", 0x03, time.Hour),
		} {
			if err := r.Sessions().Create(session); err != nil {
				return err
			}
		}
		return r.Sessions().Revoke(revokedSession.ID(), authNow.Add(time.Minute))
	})

	tests := []struct {
		name   string
		fill   byte
		now    time.Time
		active bool
	}{
		{name: "active", fill: 0x01, now: authNow.Add(30 * time.Minute), active: true},
		{name: "just before expiry", fill: 0x01, now: authNow.Add(time.Hour - time.Microsecond), active: true},
		{name: "at expiry", fill: 0x01, now: authNow.Add(time.Hour)},
		{name: "expired", fill: 0x01, now: authNow.Add(2 * time.Hour)},
		{name: "revoked", fill: 0x02, now: authNow.Add(2 * time.Minute)},
		{name: "disabled user", fill: 0x03, now: authNow.Add(time.Minute)},
		{name: "unknown token", fill: 0x04, now: authNow.Add(time.Minute)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f.mustTx(t, func(r ports.AuthRepositories) error {
				session, user, err := r.Sessions().ResolveActive(authHash(t, tt.fill), tt.now)
				if tt.active {
					if err != nil {
						t.Fatalf("ResolveActive() error = %v", err)
					}
					if session.ID() != "session-active" || user.ID() != "user-1" || user.Role() != domain.RoleOperator ||
						!session.TokenHash().Equal(authHash(t, tt.fill)) || !session.IsActive(tt.now) {
						t.Fatalf("ResolveActive() = %v %v", session, user.Snapshot())
					}
					return nil
				}
				var inactive *domain.NoActiveSessionError
				if !errors.As(err, &inactive) || err.Error() != "no active session" {
					t.Fatalf("ResolveActive() error = %v, want the uniform NoActiveSessionError", err)
				}
				assertNoHashLeak(t, err, tt.fill)
				return nil
			})
		})
	}
}

func TestAuthResolveActiveUsesTokenHashIndex(t *testing.T) {
	f := newAuthFixture(t)
	ctx := t.Context()
	conn, err := f.shared.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	// The table is tiny, so forbid sequential scans to prove the index serves
	// the lookup rather than relying on planner costs.
	if _, err := conn.Exec(ctx, "SET enable_seqscan = off"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := conn.Exec(context.Background(), "RESET enable_seqscan"); err != nil {
			t.Error(err)
		}
	}()
	query := strings.ReplaceAll(strings.ReplaceAll(resolveActiveSessionSQL, "$1", `'\x01'::bytea`), "$2", `'2026-11-10T09:00:00Z'::timestamptz`)
	rows, err := conn.Query(ctx, "EXPLAIN "+query)
	if err != nil {
		t.Fatal(err)
	}
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "sessions_token_hash_key") {
		t.Fatalf("ResolveActive plan does not use sessions_token_hash_key:\n%s", plan.String())
	}
}

func TestAuthSessionRevoke(t *testing.T) {
	f := newAuthFixture(t)
	f.mustTx(t, func(r ports.AuthRepositories) error {
		if err := r.Users().Create(authUser(t, "user-1", 101, "octo")); err != nil {
			return err
		}
		return r.Sessions().Create(authSession(t, "session-1", "user-1", 0x01, time.Hour))
	})
	first := authNow.Add(time.Minute)
	f.mustTx(t, func(r ports.AuthRepositories) error {
		if err := r.Sessions().Revoke("session-1", first); err != nil {
			return err
		}
		return r.Sessions().Revoke("session-1", first.Add(time.Minute))
	})
	var revokedAt time.Time
	if err := f.shared.QueryRow(t.Context(), `SELECT revoked_at FROM sessions WHERE id = 'session-1'`).Scan(&revokedAt); err != nil {
		t.Fatal(err)
	}
	if !revokedAt.Equal(first) {
		t.Fatalf("revoked_at = %v, want the first revocation %v", revokedAt, first)
	}

	err := f.tx(t, func(r ports.AuthRepositories) error { return r.Sessions().Revoke("missing", first) })
	var notFound *domain.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("Revoke(missing) error = %v, want NotFoundError", err)
	}
}

func TestAuthRevokeAllForUser(t *testing.T) {
	f := newAuthFixture(t)
	f.mustTx(t, func(r ports.AuthRepositories) error {
		for _, user := range []domain.User{authUser(t, "user-1", 101, "octo"), authUser(t, "user-2", 202, "other")} {
			if err := r.Users().Create(user); err != nil {
				return err
			}
		}
		for _, session := range []domain.Session{
			authSession(t, "a", "user-1", 0x01, time.Hour),
			authSession(t, "b", "user-1", 0x02, time.Hour),
			authSession(t, "c", "user-1", 0x03, time.Hour),
			authSession(t, "d", "user-2", 0x04, time.Hour),
		} {
			if err := r.Sessions().Create(session); err != nil {
				return err
			}
		}
		return r.Sessions().Revoke("c", authNow.Add(time.Minute))
	})

	var count int
	f.mustTx(t, func(r ports.AuthRepositories) error {
		var err error
		count, err = r.Sessions().RevokeAllForUser("user-1", authNow.Add(2*time.Minute))
		return err
	})
	if count != 2 {
		t.Fatalf("RevokeAllForUser() = %d, want 2", count)
	}
	f.mustTx(t, func(r ports.AuthRepositories) error {
		later := authNow.Add(3 * time.Minute)
		for _, fill := range []byte{0x01, 0x02, 0x03} {
			if _, _, err := r.Sessions().ResolveActive(authHash(t, fill), later); !errors.Is(err, domain.ErrNoActiveSession) {
				t.Fatalf("session %x still resolves: %v", fill, err)
			}
		}
		if _, _, err := r.Sessions().ResolveActive(authHash(t, 0x04), later); err != nil {
			t.Fatalf("another user's session was revoked: %v", err)
		}
		unknown, err := r.Sessions().RevokeAllForUser("ghost", later)
		if err != nil || unknown != 0 {
			t.Fatalf("RevokeAllForUser(ghost) = %d, %v; want 0, nil", unknown, err)
		}
		return nil
	})
}

func TestAuthDeleteExpired(t *testing.T) {
	f := newAuthFixture(t)
	f.mustTx(t, func(r ports.AuthRepositories) error {
		if err := r.Users().Create(authUser(t, "user-1", 101, "octo")); err != nil {
			return err
		}
		for _, session := range []domain.Session{
			authSession(t, "short", "user-1", 0x01, time.Minute),
			authSession(t, "exact", "user-1", 0x02, time.Hour),
			authSession(t, "long", "user-1", 0x03, 2*time.Hour),
		} {
			if err := r.Sessions().Create(session); err != nil {
				return err
			}
		}
		return nil
	})
	var deleted int
	f.mustTx(t, func(r ports.AuthRepositories) error {
		var err error
		deleted, err = r.Sessions().DeleteExpired(authNow.Add(time.Hour))
		return err
	})
	if deleted != 2 {
		t.Fatalf("DeleteExpired() = %d, want 2", deleted)
	}
	var remaining []string
	rows, err := f.shared.Query(t.Context(), `SELECT id FROM sessions ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		remaining = append(remaining, id)
	}
	if strings.Join(remaining, ",") != "long" {
		t.Fatalf("remaining sessions = %v, want [long]", remaining)
	}
}

func TestAuthTransactionRollsBackRoleChangeAndRevocation(t *testing.T) {
	f := newAuthFixture(t)
	user := authUser(t, "user-1", 101, "octo")
	f.mustTx(t, func(r ports.AuthRepositories) error {
		if err := r.Users().Create(user); err != nil {
			return err
		}
		return r.Sessions().Create(authSession(t, "session-1", "user-1", 0x01, time.Hour))
	})

	failure := errors.New("audit write failed")
	err := f.tx(t, func(r ports.AuthRepositories) error {
		disabled, err := user.Disable(authNow.Add(time.Minute))
		if err != nil {
			return err
		}
		if err := r.Users().Update(disabled, user.Version()); err != nil {
			return err
		}
		if count, err := r.Sessions().RevokeAllForUser(user.ID(), authNow.Add(time.Minute)); err != nil || count != 1 {
			t.Fatalf("RevokeAllForUser() = %d, %v", count, err)
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatalf("WithinAuthTransaction() error = %v, want the callback error", err)
	}
	f.mustTx(t, func(r ports.AuthRepositories) error {
		stored, err := r.Users().GetByID(user.ID())
		if err != nil {
			return err
		}
		if stored.Disabled() || stored.Version() != 1 {
			t.Fatalf("user change survived the rollback: %+v", stored.Snapshot())
		}
		if _, _, err := r.Sessions().ResolveActive(authHash(t, 0x01), authNow.Add(2*time.Minute)); err != nil {
			t.Fatalf("session revocation survived the rollback: %v", err)
		}
		return nil
	})
}

func TestAuthRepositoriesRejectZeroTime(t *testing.T) {
	f := newAuthFixture(t)
	f.mustTx(t, func(r ports.AuthRepositories) error {
		if _, _, err := r.Sessions().ResolveActive(authHash(t, 1), time.Time{}); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("ResolveActive(zero now) error = %v", err)
		}
		if err := r.Sessions().Revoke("s", time.Time{}); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("Revoke(zero now) error = %v", err)
		}
		if _, err := r.Sessions().RevokeAllForUser("u", time.Time{}); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("RevokeAllForUser(zero now) error = %v", err)
		}
		if _, err := r.Sessions().DeleteExpired(time.Time{}); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("DeleteExpired(zero) error = %v", err)
		}
		return nil
	})
}

func TestAuthSchemaConstraintsMirrorDomain(t *testing.T) {
	f := newAuthFixture(t)
	ctx := t.Context()
	if _, err := f.shared.Exec(ctx, `
		INSERT INTO users (id, github_id, login, role, version, created_at, updated_at)
		VALUES ('user-1', 1, 'octo', 'viewer', 1, '2026-11-10T09:00:00Z', '2026-11-10T09:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		sql  string
	}{
		{name: "unknown role", sql: `INSERT INTO users (id, github_id, login, role, version, created_at, updated_at)
			VALUES ('u2', 2, 'x', 'root', 1, '2026-11-10T09:00:00Z', '2026-11-10T09:00:00Z')`},
		{name: "non-positive github id", sql: `INSERT INTO users (id, github_id, login, role, version, created_at, updated_at)
			VALUES ('u2', 0, 'x', 'viewer', 1, '2026-11-10T09:00:00Z', '2026-11-10T09:00:00Z')`},
		{name: "invalid login", sql: `INSERT INTO users (id, github_id, login, role, version, created_at, updated_at)
			VALUES ('u2', 2, 'bad login', 'viewer', 1, '2026-11-10T09:00:00Z', '2026-11-10T09:00:00Z')`},
		{name: "invalid email", sql: `INSERT INTO users (id, github_id, login, email, role, version, created_at, updated_at)
			VALUES ('u2', 2, 'x', 'no-at-sign', 'viewer', 1, '2026-11-10T09:00:00Z', '2026-11-10T09:00:00Z')`},
		{name: "null organizations", sql: `INSERT INTO users (id, github_id, login, organizations, role, version, created_at, updated_at)
			VALUES ('u2', 2, 'x', NULL, 'viewer', 1, '2026-11-10T09:00:00Z', '2026-11-10T09:00:00Z')`},
		{name: "zero version", sql: `INSERT INTO users (id, github_id, login, role, version, created_at, updated_at)
			VALUES ('u2', 2, 'x', 'viewer', 0, '2026-11-10T09:00:00Z', '2026-11-10T09:00:00Z')`},
		{name: "short token hash", sql: `INSERT INTO sessions (id, user_id, token_hash, created_at, expires_at, last_seen_at)
			VALUES ('s', 'user-1', '\x0102', '2026-11-10T09:00:00Z', '2026-11-10T10:00:00Z', '2026-11-10T09:00:00Z')`},
		{name: "expiry not after creation", sql: `INSERT INTO sessions (id, user_id, token_hash, created_at, expires_at, last_seen_at)
			VALUES ('s', 'user-1', decode(repeat('01', 32), 'hex'), '2026-11-10T09:00:00Z', '2026-11-10T09:00:00Z', '2026-11-10T09:00:00Z')`},
		{name: "revoked before creation", sql: `INSERT INTO sessions (id, user_id, token_hash, created_at, expires_at, revoked_at, last_seen_at)
			VALUES ('s', 'user-1', decode(repeat('01', 32), 'hex'), '2026-11-10T09:00:00Z', '2026-11-10T10:00:00Z',
				'2026-11-10T08:00:00Z', '2026-11-10T09:00:00Z')`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := f.shared.Exec(ctx, tt.sql); err == nil {
				t.Fatal("insert succeeded, want a constraint violation")
			}
		})
	}
	var rawTokenColumns int
	if err := f.shared.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name IN ('users', 'sessions')
			AND column_name LIKE '%token%' AND column_name <> 'token_hash'`).Scan(&rawTokenColumns); err != nil {
		t.Fatal(err)
	}
	if rawTokenColumns != 0 {
		t.Fatalf("found %d token columns besides token_hash", rawTokenColumns)
	}
}

func TestAuthMigrationDown(t *testing.T) {
	shared := isolatedSchemaPool(t, allUpMigrations...)
	ctx := t.Context()
	down, err := os.ReadFile("../../../migrations/000005_auth_users_sessions.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, err := os.ReadFile("../../../migrations/000005_auth_users_sessions.up.sql")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := shared.Exec(ctx, `
		INSERT INTO users (id, github_id, login, role, version, created_at, updated_at)
		VALUES ('user-1', 1, 'octo', 'viewer', 1, '2026-11-10T09:00:00Z', '2026-11-10T09:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := shared.Exec(ctx, string(down)); err == nil || !strings.Contains(err.Error(), "cannot remove auth migration") {
		t.Fatalf("down with users error = %v, want a refusal", err)
	}
	if _, err := shared.Exec(ctx, `DELETE FROM users`); err != nil {
		t.Fatal(err)
	}

	if _, err := shared.Exec(ctx, string(down)); err != nil {
		t.Fatalf("down on empty tables: %v", err)
	}
	var remaining int
	if err := shared.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name IN ('users', 'sessions')`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("%d auth tables remain after down", remaining)
	}
	if _, err := shared.Exec(ctx, `SELECT count(*) FROM incidents`); err != nil {
		t.Fatalf("down touched earlier migrations: %v", err)
	}
	if _, err := shared.Exec(ctx, string(up)); err != nil {
		t.Fatalf("up after down: %v", err)
	}
}
