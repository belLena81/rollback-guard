//go:build integration

package postgres

import (
	"errors"
	"testing"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// readOnlySQLState is PostgreSQL's read_only_sql_transaction.
const readOnlySQLState = "25006"

func requireReadOnlyViolation(t *testing.T, what string, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != readOnlySQLState {
		t.Fatalf("%s inside a read transaction = %v, want SQLSTATE %s", what, err, readOnlySQLState)
	}
}

// TestAuthReadTransactionReadsButCannotWrite proves the access mode, not only
// the narrower port type: the same repositories used for writes fail inside a
// read transaction.
func TestAuthReadTransactionReadsButCannotWrite(t *testing.T) {
	f := newAuthFixture(t)
	user := authUser(t, "user-1", 4242, "reader")
	f.mustTx(t, func(r ports.AuthRepositories) error { return r.Users().Create(user) })

	err := f.uow.WithinAuthReadTransaction(t.Context(), func(r ports.AuthReadRepositories) error {
		got, err := r.Users().GetByGitHubID(4242)
		if err != nil {
			return err
		}
		if got.ID() != user.ID() {
			t.Errorf("read user %q, want %q", got.ID(), user.ID())
		}
		if _, err := r.Audit().ListRecent(10); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Writes reached through the concrete write-side repositories of the same
	// adapter, inside a read-only transaction, are refused by PostgreSQL.
	// One transaction per attempt: a failed statement aborts its transaction.
	attempts := map[string]func(authRepositories) error{
		"user create": func(r authRepositories) error { return r.Users().Create(authUser(t, "user-2", 4343, "writer")) },
		"session revoke-all": func(r authRepositories) error {
			_, err := r.Sessions().RevokeAllForUser(user.ID(), authNow)
			return err
		},
		"lock for update": func(r authRepositories) error {
			_, err := r.Users().LockActiveAdministrators()
			return err
		},
	}
	for what, attempt := range attempts {
		err := withinReadTransaction(t.Context(), f.uow.pool, func(tx pgx.Tx) error {
			return attempt(authRepositories{&repositories{ctx: t.Context(), tx: tx}})
		})
		requireReadOnlyViolation(t, what, err)
	}
	// Nothing was written.
	f.mustTx(t, func(r ports.AuthRepositories) error {
		if _, err := r.Users().GetByID("user-2"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("user-2 lookup = %v, want not found", err)
		}
		return nil
	})
}
