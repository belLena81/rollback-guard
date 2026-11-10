package memory

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

var authNow = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

func authUser(t *testing.T, id domain.UserID, githubID int64, login string) domain.User {
	t.Helper()
	user, err := domain.NewUser(id, githubID, login, "", []string{"acme"}, domain.RoleOperator, authNow)
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

func authTx(t *testing.T, uow *AuthUnitOfWork, fn func(ports.AuthRepositories) error) error {
	t.Helper()
	return uow.WithinAuthTransaction(context.Background(), fn)
}

func mustAuthTx(t *testing.T, uow *AuthUnitOfWork, fn func(ports.AuthRepositories) error) {
	t.Helper()
	if err := authTx(t, uow, fn); err != nil {
		t.Fatal(err)
	}
}

func TestAuthUserRepository(t *testing.T) {
	uow := NewAuthUnitOfWork()
	user := authUser(t, "user-1", 101, "octo")
	mustAuthTx(t, uow, func(r ports.AuthRepositories) error { return r.Users().Create(user) })

	duplicates := map[string]domain.User{
		"same id":        authUser(t, "user-1", 202, "other"),
		"same github id": authUser(t, "user-2", 101, "other"),
		"same login":     authUser(t, "user-2", 202, "OCTO"),
	}
	for name, duplicate := range duplicates {
		err := authTx(t, uow, func(r ports.AuthRepositories) error { return r.Users().Create(duplicate) })
		if !errors.Is(err, domain.ErrAlreadyExists) {
			t.Fatalf("%s: Create() error = %v, want ErrAlreadyExists", name, err)
		}
	}

	mustAuthTx(t, uow, func(r ports.AuthRepositories) error {
		if got, err := r.Users().GetByID("user-1"); err != nil || got.GitHubID() != 101 {
			t.Fatalf("GetByID() = %v, %v", got.Snapshot(), err)
		}
		if got, err := r.Users().GetByGitHubID(101); err != nil || got.ID() != "user-1" {
			t.Fatalf("GetByGitHubID() = %v, %v", got.Snapshot(), err)
		}
		if _, err := r.Users().GetByID("missing"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("GetByID(missing) error = %v", err)
		}
		if _, err := r.Users().GetByGitHubID(9); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("GetByGitHubID(missing) error = %v", err)
		}
		return r.Users().Create(authUser(t, "user-2", 202, "taken"))
	})

	promoted, err := user.WithRole(domain.RoleAdministrator, authNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	mustAuthTx(t, uow, func(r ports.AuthRepositories) error { return r.Users().Update(promoted, 1) })

	err = authTx(t, uow, func(r ports.AuthRepositories) error { return r.Users().Update(promoted, 1) })
	var conflict *domain.VersionConflictError
	if !errors.As(err, &conflict) || conflict.Expected != 1 || conflict.Actual != 2 {
		t.Fatalf("Update(stale) error = %v, want VersionConflictError expected 1 actual 2", err)
	}
	err = authTx(t, uow, func(r ports.AuthRepositories) error { return r.Users().Update(authUser(t, "ghost", 9, "ghost"), 0) })
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Update(missing) error = %v", err)
	}
	snapshot := promoted.Snapshot()
	snapshot.Login, snapshot.Version = "Taken", 3
	renamed, err := domain.RehydrateUser(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	err = authTx(t, uow, func(r ports.AuthRepositories) error { return r.Users().Update(renamed, 2) })
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("Update(login collision) error = %v", err)
	}

	err = authTx(t, uow, func(r ports.AuthRepositories) error { return r.Users().Update(promoted, 2) })
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Update(unbumped version) error = %v, want ErrValidation", err)
	}
	moved := promoted.Snapshot()
	moved.GitHubID, moved.Version = 999, 3
	movedUser, err := domain.RehydrateUser(moved)
	if err != nil {
		t.Fatal(err)
	}
	err = authTx(t, uow, func(r ports.AuthRepositories) error { return r.Users().Update(movedUser, 2) })
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Update(changed github id) error = %v, want ErrValidation", err)
	}
}

func TestAuthSessionCreateRejectsDisabledUser(t *testing.T) {
	uow := NewAuthUnitOfWork()
	disabled, err := authUser(t, "user-1", 101, "octo").Disable(authNow)
	if err != nil {
		t.Fatal(err)
	}
	mustAuthTx(t, uow, func(r ports.AuthRepositories) error { return r.Users().Create(disabled) })
	err = authTx(t, uow, func(r ports.AuthRepositories) error {
		return r.Sessions().Create(authSession(t, "s1", "user-1", 1, time.Hour))
	})
	if !errors.Is(err, domain.ErrUserDisabled) {
		t.Fatalf("Create(disabled user) error = %v, want ErrUserDisabled", err)
	}
}

func TestAuthSessionCreateRejectsDuplicatesAndUnknownUser(t *testing.T) {
	uow := NewAuthUnitOfWork()
	mustAuthTx(t, uow, func(r ports.AuthRepositories) error {
		if err := r.Users().Create(authUser(t, "user-1", 101, "octo")); err != nil {
			return err
		}
		return r.Sessions().Create(authSession(t, "s1", "user-1", 1, time.Hour))
	})
	tests := []struct {
		name    string
		session domain.Session
		want    error
	}{
		{name: "duplicate hash", session: authSession(t, "s2", "user-1", 1, time.Hour), want: domain.ErrAlreadyExists},
		{name: "duplicate id", session: authSession(t, "s1", "user-1", 2, time.Hour), want: domain.ErrAlreadyExists},
		{name: "unknown user", session: authSession(t, "s3", "ghost", 3, time.Hour), want: domain.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := authTx(t, uow, func(r ports.AuthRepositories) error { return r.Sessions().Create(tt.session) })
			if !errors.Is(err, tt.want) {
				t.Fatalf("Create() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestAuthResolveActiveSession(t *testing.T) {
	uow := NewAuthUnitOfWork()
	mustAuthTx(t, uow, func(r ports.AuthRepositories) error {
		if err := r.Users().Create(authUser(t, "user-1", 101, "octo")); err != nil {
			return err
		}
		gone := authUser(t, "user-2", 202, "gone")
		if err := r.Users().Create(gone); err != nil {
			return err
		}
		for _, session := range []domain.Session{
			authSession(t, "active", "user-1", 1, time.Hour),
			authSession(t, "revoked", "user-1", 2, time.Hour),
			authSession(t, "disabled", "user-2", 3, time.Hour),
		} {
			if err := r.Sessions().Create(session); err != nil {
				return err
			}
		}
		disabled, err := gone.Disable(authNow)
		if err != nil {
			return err
		}
		if err := r.Users().Update(disabled, gone.Version()); err != nil {
			return err
		}
		return r.Sessions().Revoke("revoked", authNow.Add(time.Minute))
	})
	tests := []struct {
		name   string
		fill   byte
		now    time.Time
		active bool
	}{
		{name: "active", fill: 1, now: authNow.Add(time.Minute), active: true},
		{name: "at expiry", fill: 1, now: authNow.Add(time.Hour)},
		{name: "expired", fill: 1, now: authNow.Add(2 * time.Hour)},
		{name: "revoked", fill: 2, now: authNow.Add(2 * time.Minute)},
		{name: "disabled user", fill: 3, now: authNow.Add(time.Minute)},
		{name: "unknown token", fill: 4, now: authNow.Add(time.Minute)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mustAuthTx(t, uow, func(r ports.AuthRepositories) error {
				session, user, err := r.Sessions().ResolveActive(authHash(t, tt.fill), tt.now)
				if tt.active {
					if err != nil || session.ID() != "active" || user.ID() != "user-1" {
						t.Fatalf("ResolveActive() = %v, %v, %v", session, user.Snapshot(), err)
					}
					return nil
				}
				var inactive *domain.NoActiveSessionError
				if !errors.As(err, &inactive) {
					t.Fatalf("ResolveActive() error = %v, want NoActiveSessionError", err)
				}
				return nil
			})
		})
	}
	mustAuthTx(t, uow, func(r ports.AuthRepositories) error {
		if _, _, err := r.Sessions().ResolveActive(authHash(t, 1), time.Time{}); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("ResolveActive(zero now) error = %v", err)
		}
		return nil
	})
}

func TestAuthRevocationAndExpiry(t *testing.T) {
	uow := NewAuthUnitOfWork()
	mustAuthTx(t, uow, func(r ports.AuthRepositories) error {
		for _, user := range []domain.User{authUser(t, "user-1", 101, "octo"), authUser(t, "user-2", 202, "other")} {
			if err := r.Users().Create(user); err != nil {
				return err
			}
		}
		for _, session := range []domain.Session{
			authSession(t, "a", "user-1", 1, time.Hour),
			authSession(t, "b", "user-1", 2, time.Minute),
			authSession(t, "c", "user-1", 3, 2*time.Hour),
			authSession(t, "d", "user-2", 4, 2*time.Hour),
		} {
			if err := r.Sessions().Create(session); err != nil {
				return err
			}
		}
		return nil
	})

	first := authNow.Add(time.Minute)
	mustAuthTx(t, uow, func(r ports.AuthRepositories) error {
		if err := r.Sessions().Revoke("c", first); err != nil {
			return err
		}
		if err := r.Sessions().Revoke("c", first.Add(time.Hour)); err != nil {
			return err
		}
		if err := r.Sessions().Revoke("missing", first); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("Revoke(missing) error = %v", err)
		}
		count, err := r.Sessions().RevokeAllForUser("user-1", authNow.Add(-time.Hour))
		if err != nil || count != 2 {
			t.Fatalf("RevokeAllForUser() = %d, %v; want 2", count, err)
		}
		if count, err := r.Sessions().RevokeAllForUser("ghost", first); err != nil || count != 0 {
			t.Fatalf("RevokeAllForUser(ghost) = %d, %v", count, err)
		}
		return nil
	})
	if got := uow.data.sessions["c"].RevokedAt(); !got.Equal(first) {
		t.Fatalf("revoked_at = %v, want the first revocation %v", got, first)
	}
	if got := uow.data.sessions["a"].RevokedAt(); !got.Equal(authNow) {
		t.Fatalf("revoked_at = %v, want clamped to creation %v", got, authNow)
	}
	if uow.data.sessions["d"].Revoked() {
		t.Fatal("another user's session was revoked")
	}

	mustAuthTx(t, uow, func(r ports.AuthRepositories) error {
		deleted, err := r.Sessions().DeleteExpired(authNow.Add(time.Hour))
		if err != nil || deleted != 2 {
			t.Fatalf("DeleteExpired() = %d, %v; want 2", deleted, err)
		}
		return nil
	})
	if _, ok := uow.data.sessions["c"]; !ok || len(uow.data.sessions) != 2 {
		t.Fatalf("remaining sessions = %d", len(uow.data.sessions))
	}
}

func TestAuthTransactionIsAtomic(t *testing.T) {
	uow := NewAuthUnitOfWork()
	user := authUser(t, "user-1", 101, "octo")
	mustAuthTx(t, uow, func(r ports.AuthRepositories) error {
		if err := r.Users().Create(user); err != nil {
			return err
		}
		return r.Sessions().Create(authSession(t, "s1", "user-1", 1, time.Hour))
	})
	failure := errors.New("audit write failed")
	err := authTx(t, uow, func(r ports.AuthRepositories) error {
		disabled, err := user.Disable(authNow.Add(time.Minute))
		if err != nil {
			return err
		}
		if err := r.Users().Update(disabled, 1); err != nil {
			return err
		}
		if _, err := r.Sessions().RevokeAllForUser("user-1", authNow.Add(time.Minute)); err != nil {
			return err
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatalf("WithinAuthTransaction() error = %v", err)
	}
	mustAuthTx(t, uow, func(r ports.AuthRepositories) error {
		if _, _, err := r.Sessions().ResolveActive(authHash(t, 1), authNow.Add(2*time.Minute)); err != nil {
			t.Fatalf("rolled-back change is visible: %v", err)
		}
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	if err := uow.WithinAuthTransaction(ctx, func(ports.AuthRepositories) error { called = true; return nil }); !errors.Is(err, context.Canceled) || called {
		t.Fatalf("cancelled WithinAuthTransaction() = %v, called %v", err, called)
	}
}
