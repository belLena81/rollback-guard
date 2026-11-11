//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testLocalApprover is the actor the pre-existing guard fixtures approve as;
// testApproverVerifier is the local-mode verifier that accepts exactly it.
const testLocalApprover domain.ActorID = "operator"

func testApproverVerifier(t *testing.T) ApproverVerifier {
	t.Helper()
	actor, err := domain.NewActor(testLocalApprover, domain.ActorHuman)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewLocalApproverVerifier(actor)
	if err != nil {
		t.Fatal(err)
	}
	return verifier
}

func seedApproverUser(t *testing.T, pool *pgxpool.Pool, githubID int64, role string, disabled bool) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO users (id, github_id, login, email, organizations, role, disabled, version, created_at, updated_at)
		VALUES ($1, $2, $3, '', '{}', $4, $5, 1, $6, $6)
	`, fmt.Sprintf("user-%d", githubID), githubID, fmt.Sprintf("user-%d", githubID), role, disabled, now); err != nil {
		t.Fatal(err)
	}
}

func setApproverUser(t *testing.T, pool *pgxpool.Pool, githubID int64, role string, disabled bool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE users SET role = $1, disabled = $2, version = version + 1 WHERE github_id = $3`, role, disabled, githubID); err != nil {
		t.Fatal(err)
	}
}

// assertRefusedWithoutState checks the fixed typed refusal and that nothing
// changed: no ledger row, the execution record still pending, the incident
// still executing.
func assertRefusedWithoutState(t *testing.T, pool *pgxpool.Pool, err error, incidentID string, key domain.IdempotencyKey) {
	t.Helper()
	var refused *domain.ApproverNotAuthorizedError
	if !errors.As(err, &refused) {
		t.Fatalf("Begin error = %v, want ApproverNotAuthorizedError", err)
	}
	var rows int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM rollback_mutations WHERE idempotency_key = $1`, key).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	var keyState, status string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM idempotency_keys WHERE key = $1`, key).Scan(&keyState); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT status FROM incidents WHERE id = $1`, incidentID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || keyState != "pending" || status != "executing" {
		t.Fatalf("after refusal: ledger rows %d, key state %q, incident %q; want 0, pending, executing", rows, keyState, status)
	}
}

func TestRollbackMutationGuardRechecksGitHubApproverAtExecution(t *testing.T) {
	// Each case gets its own schema: one active incident per deployment.
	var pool *pgxpool.Pool
	now := time.Now().UTC().Truncate(time.Microsecond)
	tests := []struct {
		name     string
		approver domain.ActorID
		seed     func(t *testing.T)
		allowed  bool
	}{
		{name: "enabled administrator", approver: "github:2001", seed: func(t *testing.T) { seedApproverUser(t, pool, 2001, "administrator", false) }, allowed: true},
		{name: "demoted to operator", approver: "github:2002", seed: func(t *testing.T) {
			seedApproverUser(t, pool, 2002, "administrator", false)
			setApproverUser(t, pool, 2002, "operator", false)
		}},
		{name: "demoted to viewer", approver: "github:2003", seed: func(t *testing.T) { seedApproverUser(t, pool, 2003, "viewer", false) }},
		{name: "disabled administrator", approver: "github:2004", seed: func(t *testing.T) {
			seedApproverUser(t, pool, 2004, "administrator", false)
			setApproverUser(t, pool, 2004, "administrator", true)
		}},
		{name: "unknown user", approver: "github:2005", seed: func(*testing.T) {}},
		{name: "non-github actor", approver: "local-admin", seed: func(*testing.T) {}},
		{name: "non-canonical github id", approver: "github:02006", seed: func(t *testing.T) { seedApproverUser(t, pool, 2006, "administrator", false) }},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			incidentID := fmt.Sprintf("incident-approver-%d", index)
			key := domain.IdempotencyKey(fmt.Sprintf("key-approver-%d", index))
			pool = isolatedRollbackGuardPool(t)
			guard, err := NewRollbackMutationGuard(pool, NewGitHubApproverVerifier())
			if err != nil {
				t.Fatal(err)
			}
			test.seed(t)
			proposal := seedTrustedRollbackApprovedBy(t, pool, incidentID, key, now, now.Add(5*time.Minute), test.approver)
			attempt, err := guard.Begin(context.Background(), key, proposal.Hash(), now)
			if !test.allowed {
				assertRefusedWithoutState(t, pool, err, incidentID, key)
				return
			}
			if err != nil {
				t.Fatalf("Begin error = %v", err)
			}
			if attempt.Mutation.State() != domain.RollbackMutationPending || attempt.Approved.Approval().Actor().ID() != test.approver {
				t.Fatalf("attempt = %#v", attempt.Mutation.Snapshot())
			}
			if err := guard.Release(context.Background(), attempt); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRollbackMutationGuardApproverRoleRestoredIsExecutableAgain(t *testing.T) {
	pool := isolatedRollbackGuardPool(t)
	guard, err := NewRollbackMutationGuard(pool, NewGitHubApproverVerifier())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	seedApproverUser(t, pool, 3001, "administrator", false)
	proposal := seedTrustedRollbackApprovedBy(t, pool, "incident-restore", "key-restore", now, now.Add(5*time.Minute), "github:3001")

	setApproverUser(t, pool, 3001, "operator", false)
	_, err = guard.Begin(context.Background(), "key-restore", proposal.Hash(), now)
	assertRefusedWithoutState(t, pool, err, "incident-restore", "key-restore")

	setApproverUser(t, pool, 3001, "administrator", true)
	_, err = guard.Begin(context.Background(), "key-restore", proposal.Hash(), now)
	assertRefusedWithoutState(t, pool, err, "incident-restore", "key-restore")

	setApproverUser(t, pool, 3001, "administrator", false)
	attempt, err := guard.Begin(context.Background(), "key-restore", proposal.Hash(), now)
	if err != nil {
		t.Fatalf("Begin after restore error = %v", err)
	}
	completed, err := guard.Complete(context.Background(), attempt, false, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}

	// The ledger is unaffected: once completed, a replay returns the recorded
	// mutation without a new authorization, even after a later demotion, and
	// writes no second row.
	setApproverUser(t, pool, 3001, "viewer", false)
	replay, err := guard.Begin(context.Background(), "key-restore", proposal.Hash(), now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("completed replay error = %v", err)
	}
	if replay.Mutation.Snapshot() != completed.Snapshot() {
		t.Fatalf("replay = %#v, want %#v", replay.Mutation.Snapshot(), completed.Snapshot())
	}
	var rows int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM rollback_mutations WHERE idempotency_key = 'key-restore'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("ledger rows = %d, want 1", rows)
	}
}

func TestRollbackMutationGuardResumeRechecksApprover(t *testing.T) {
	pool := isolatedRollbackGuardPool(t)
	guard, err := NewRollbackMutationGuard(pool, NewGitHubApproverVerifier())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	seedApproverUser(t, pool, 4001, "administrator", false)
	proposal := seedTrustedRollbackApprovedBy(t, pool, "incident-resume", "key-resume", now, now.Add(5*time.Minute), "github:4001")
	attempt, err := guard.Begin(context.Background(), "key-resume", proposal.Hash(), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := guard.Release(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	setApproverUser(t, pool, 4001, "operator", false)
	var refused *domain.ApproverNotAuthorizedError
	if _, err := guard.Begin(context.Background(), "key-resume", proposal.Hash(), now.Add(time.Second)); !errors.As(err, &refused) {
		t.Fatalf("resume error = %v, want ApproverNotAuthorizedError", err)
	}
	var state string
	if err := pool.QueryRow(context.Background(), `SELECT state::text FROM rollback_mutations WHERE idempotency_key = 'key-resume'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "pending" {
		t.Fatalf("ledger state after refused resume = %q, want pending (unchanged)", state)
	}
}

func TestRollbackMutationGuardLocalModeAcceptsOnlyTheLocalActor(t *testing.T) {
	pool := isolatedRollbackGuardPool(t)
	local, err := domain.NewActor("local-admin", domain.ActorHuman)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewLocalApproverVerifier(local)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := NewRollbackMutationGuard(pool, verifier)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)

	// No users row exists in local mode.
	proposal := seedTrustedRollbackApprovedBy(t, pool, "incident-local", "key-local", now, now.Add(5*time.Minute), "local-admin")
	attempt, err := guard.Begin(context.Background(), "key-local", proposal.Hash(), now)
	if err != nil {
		t.Fatalf("local actor Begin error = %v", err)
	}
	if _, err := guard.Complete(context.Background(), attempt, false, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	pool = isolatedRollbackGuardPool(t)
	guard, err = NewRollbackMutationGuard(pool, verifier)
	if err != nil {
		t.Fatal(err)
	}
	seedApproverUser(t, pool, 5001, "administrator", false)
	other := seedTrustedRollbackApprovedBy(t, pool, "incident-other", "key-other", now, now.Add(5*time.Minute), "github:5001")
	_, err = guard.Begin(context.Background(), "key-other", other.Hash(), now)
	assertRefusedWithoutState(t, pool, err, "incident-other", "key-other")
}
