//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRollbackMutationGuardBlocksReplayUntilCompletion(t *testing.T) {
	pool := isolatedRollbackGuardPool(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	proposal := seedTrustedRollback(t, pool, "incident-1", "key-1", now)
	guards := make([]*RollbackMutationGuard, 2)
	for index := range guards {
		var err error
		guards[index], err = NewRollbackMutationGuard(pool)
		if err != nil {
			t.Fatal(err)
		}
	}

	first, err := guards[0].Begin(context.Background(), "key-1", proposal.Hash(), now)
	if err != nil {
		t.Fatal(err)
	}

	secondStarted := make(chan struct{})
	secondResult := make(chan ports.RollbackMutationAttempt, 1)
	secondErr := make(chan error, 1)
	go func() {
		close(secondStarted)
		attempt, beginErr := guards[1].Begin(context.Background(), "key-1", proposal.Hash(), now)
		secondResult <- attempt
		secondErr <- beginErr
	}()
	<-secondStarted
	select {
	case attempt := <-secondResult:
		t.Fatalf("second Begin returned before completion with %#v", attempt.Mutation.Snapshot())
	case err := <-secondErr:
		t.Fatalf("second Begin errored before completion: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM rollback_mutations WHERE idempotency_key = 'key-1'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("rollback mutation rows = %d, want 1", count)
	}

	completed, err := guards[0].Complete(context.Background(), first, false, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := <-secondErr; err != nil {
		t.Fatal(err)
	}
	replayed := <-secondResult
	if replayed.Mutation.Snapshot() != completed.Snapshot() {
		t.Fatalf("replayed mutation = %#v, want %#v", replayed.Mutation.Snapshot(), completed.Snapshot())
	}
	if _, err := pool.Exec(context.Background(), `
		UPDATE idempotency_keys SET state = 'completed', result_id = 'application-execution', completed_at = $1 WHERE key = 'key-1';
		UPDATE incidents SET status = 'verifying', version = version + 1, updated_at = $1 WHERE id = 'incident-1'
	`, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	replayed, err = guards[0].Begin(context.Background(), "key-1", proposal.Hash(), now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Mutation.Snapshot() != completed.Snapshot() {
		t.Fatalf("completed replay = %#v, want %#v", replayed.Mutation.Snapshot(), completed.Snapshot())
	}
	if _, err := guards[1].Begin(context.Background(), "key-1", "different-hash", now.Add(4*time.Second)); !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("completed conflicting replay error = %v", err)
	}
}

// Two attempts on one key in one process: a late, duplicate release (or a
// completion) from the first attempt must not unlock the replay that now
// holds the lock. Before leases, Release was keyed by idempotency key only and
// would have dropped the replay's advisory lock here.
func TestRollbackMutationGuardLateReleaseDoesNotUnlockConcurrentReplay(t *testing.T) {
	pool := isolatedRollbackGuardPool(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	key := uniqueRollbackKey("key-late-release")
	proposal := seedTrustedRollback(t, pool, "incident-late-release", key, now)
	guard, err := NewRollbackMutationGuard(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	first, err := guard.Begin(ctx, key, proposal.Hash(), now)
	if err != nil {
		t.Fatal(err)
	}
	releaseOnCleanup(t, guard, first)
	if first.Lease == "" || first.Mutation.State() != domain.RollbackMutationPending {
		t.Fatalf("first attempt = lease %q state %s, want a leased pending attempt", first.Lease, first.Mutation.State())
	}
	replay := beginAsync(t, guard, key, proposal.Hash(), now.Add(time.Second))
	replay.requireWaitingOnLock(t, pool, key, "replay while the first attempt holds the lock")

	if err := guard.Release(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := replay.wait(t)
	releaseOnCleanup(t, guard, second)
	if second.Lease == "" || second.Lease == first.Lease || second.Mutation.State() != domain.RollbackMutationPending {
		t.Fatalf("replay = lease %q (first %q) state %s, want a new lease on the pending mutation", second.Lease, first.Lease, second.Mutation.State())
	}

	// The first request's deferred release and a stray completion arrive
	// late, while the replay is mid-mutation.
	if err := guard.Release(ctx, first); err != nil {
		t.Fatalf("late release error = %v, want nil for a lease that holds nothing", err)
	}
	if _, err := guard.Complete(ctx, first, false, now.Add(2*time.Second)); !errors.Is(err, ports.ErrRollbackMutationLeaseNotHeld) {
		t.Fatalf("late complete error = %v, want lease not held", err)
	}
	requireAdvisoryLockHeld(t, pool, key)
	requireMutationState(t, pool, key, "pending")
	third := beginAsync(t, guard, key, proposal.Hash(), now.Add(3*time.Second))
	third.requireWaitingOnLock(t, pool, key, "a third attempt after the late release")
	requireAdvisoryLockHeld(t, pool, key)
	third.requireNotReturned(t, "a third attempt after the late release")

	completed, err := guard.Complete(ctx, second, false, now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	thirdAttempt := third.wait(t)
	if thirdAttempt.Mutation.Snapshot() != completed.Snapshot() || thirdAttempt.Lease != "" {
		t.Fatalf("third attempt = %#v lease %q, want the completed mutation and no lease", thirdAttempt.Mutation.Snapshot(), thirdAttempt.Lease)
	}
	if err := guard.Release(ctx, second); err != nil {
		t.Fatalf("release after completion = %v, want nil", err)
	}
	requireAdvisoryLockFree(t, pool, key)
}

// Two guards model two rollback MCP replicas. Each lease is only meaningful
// to the guard that issued it.
func TestRollbackMutationGuardIgnoresLeaseFromAnotherGuard(t *testing.T) {
	pool := isolatedRollbackGuardPool(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	key := uniqueRollbackKey("key-foreign-lease")
	proposal := seedTrustedRollback(t, pool, "incident-foreign-lease", key, now)
	owner, err := NewRollbackMutationGuard(pool)
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewRollbackMutationGuard(pool)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := owner.Begin(context.Background(), key, proposal.Hash(), now)
	if err != nil {
		t.Fatal(err)
	}
	releaseOnCleanup(t, owner, attempt)

	if err := other.Release(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Complete(context.Background(), attempt, false, now.Add(time.Second)); !errors.Is(err, ports.ErrRollbackMutationLeaseNotHeld) {
		t.Fatalf("foreign complete error = %v, want lease not held", err)
	}
	requireAdvisoryLockHeld(t, pool, key)
	requireMutationState(t, pool, key, "pending")
	if err := owner.Release(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	requireAdvisoryLockFree(t, pool, key)
}

// uniqueRollbackKey keeps keys distinct across concurrent runs against one
// test database: advisory locks are database-wide, not per schema.
func uniqueRollbackKey(prefix string) domain.IdempotencyKey {
	return domain.IdempotencyKey(fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()))
}

// releaseOnCleanup releases attempt when the test ends, so a failing test
// cannot leave a lock-holding connection checked out and hang pool.Close.
// Releasing an attempt that holds nothing is a no-op.
func releaseOnCleanup(t *testing.T, guard *RollbackMutationGuard, attempt ports.RollbackMutationAttempt) {
	t.Helper()
	t.Cleanup(func() {
		if err := guard.Release(context.Background(), attempt); err != nil {
			t.Logf("cleanup release: %v", err)
		}
	})
}

type asyncBegin struct {
	attempt chan ports.RollbackMutationAttempt
	err     chan error
}

// beginAsync runs Begin in a goroutine. Its context is cancelled when the
// test ends, so a Begin still waiting on the lock gives its connection back,
// and a lease it acquired is released.
func beginAsync(t *testing.T, guard *RollbackMutationGuard, key domain.IdempotencyKey, hash domain.ProposalHash, at time.Time) asyncBegin {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	result := asyncBegin{attempt: make(chan ports.RollbackMutationAttempt, 1), err: make(chan error, 1)}
	go func() {
		defer close(done)
		attempt, err := guard.Begin(ctx, key, hash, at)
		result.err <- err
		result.attempt <- attempt
		if err == nil {
			releaseOnCleanup(t, guard, attempt)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return result
}

// requireWaitingOnLock waits until PostgreSQL shows an ungranted advisory
// lock request for key, which proves the goroutine is blocked on this key's
// lock rather than merely slow, and that Begin has not returned.
func (b asyncBegin) requireWaitingOnLock(t *testing.T, pool *pgxpool.Pool, key domain.IdempotencyKey, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(context.Background(), `
			SELECT count(*) FROM pg_locks
			WHERE locktype = 'advisory' AND NOT granted AND objsubid = 1
				AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
				AND classid::text::bigint = ((hashtextextended($1, 0) >> 32) & 4294967295)
				AND objid::text::bigint = (hashtextextended($1, 0) & 4294967295)
		`, key).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == 1 {
			break
		}
		b.requireNotReturned(t, what)
		if time.Now().After(deadline) {
			t.Fatalf("%s: no waiting advisory lock request for %q", what, key)
		}
		time.Sleep(10 * time.Millisecond)
	}
	b.requireNotReturned(t, what)
}

func (b asyncBegin) requireNotReturned(t *testing.T, what string) {
	t.Helper()
	select {
	case err := <-b.err:
		t.Fatalf("%s: Begin returned (err %v) instead of waiting for the lock", what, err)
	default:
	}
}

func (b asyncBegin) wait(t *testing.T) ports.RollbackMutationAttempt {
	t.Helper()
	select {
	case err := <-b.err:
		if err != nil {
			t.Fatal(err)
		}
		return <-b.attempt
	case <-time.After(10 * time.Second):
		t.Fatal("Begin did not acquire the released lock")
		return ports.RollbackMutationAttempt{}
	}
}

// tryAdvisoryLock probes the per-key lock from a separate session and
// immediately gives it back when it was free.
func tryAdvisoryLock(t *testing.T, pool *pgxpool.Pool, key domain.IdempotencyKey) bool {
	t.Helper()
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	var acquired bool
	if err := conn.QueryRow(context.Background(), `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, key).Scan(&acquired); err != nil {
		t.Fatal(err)
	}
	if acquired {
		if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, key); err != nil {
			t.Fatal(err)
		}
	}
	return acquired
}

func requireAdvisoryLockHeld(t *testing.T, pool *pgxpool.Pool, key domain.IdempotencyKey) {
	t.Helper()
	if tryAdvisoryLock(t, pool, key) {
		t.Fatalf("advisory lock for %q is free, want it held by the live attempt", key)
	}
}

func requireAdvisoryLockFree(t *testing.T, pool *pgxpool.Pool, key domain.IdempotencyKey) {
	t.Helper()
	if !tryAdvisoryLock(t, pool, key) {
		t.Fatalf("advisory lock for %q is still held", key)
	}
}

func requireMutationState(t *testing.T, pool *pgxpool.Pool, key domain.IdempotencyKey, want string) {
	t.Helper()
	var state string
	if err := pool.QueryRow(context.Background(), `SELECT state::text FROM rollback_mutations WHERE idempotency_key = $1`, key).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != want {
		t.Fatalf("rollback mutation state = %s, want %s", state, want)
	}
}

func TestRollbackMutationGuardAllowsStalePendingReplay(t *testing.T) {
	pool := isolatedRollbackGuardPool(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	proposal := seedTrustedRollback(t, pool, "incident-2", "key-2", now)
	first, err := NewRollbackMutationGuard(pool)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewRollbackMutationGuard(pool)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := first.Begin(context.Background(), "key-2", proposal.Hash(), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Release(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}

	replayed, err := second.Begin(context.Background(), "key-2", proposal.Hash(), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Mutation.Snapshot() != attempt.Mutation.Snapshot() {
		t.Fatalf("stale pending replay = %#v, want %#v", replayed.Mutation.Snapshot(), attempt.Mutation.Snapshot())
	}
	if err := second.Release(context.Background(), replayed); err != nil {
		t.Fatal(err)
	}
}

func TestRollbackMutationGuardAuthorizesFirstUseAtRecordStart(t *testing.T) {
	pool := isolatedRollbackGuardPool(t)
	recordStartedAt := time.Now().UTC().Truncate(time.Microsecond)
	proposal := seedTrustedRollbackWithExpiry(t, pool, "incident-3", "key-3", recordStartedAt, recordStartedAt.Add(time.Second))
	guard, err := NewRollbackMutationGuard(pool)
	if err != nil {
		t.Fatal(err)
	}

	attempt, err := guard.Begin(context.Background(), "key-3", proposal.Hash(), recordStartedAt.Add(2*time.Second))
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	if !attempt.Mutation.StartedAt().Equal(recordStartedAt) {
		t.Fatalf("mutation started at = %s, want %s", attempt.Mutation.StartedAt(), recordStartedAt)
	}
	if err := guard.Release(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
}

func TestRollbackMutationLockReleaseClosesConnectionWhenUnlockFails(t *testing.T) {
	pool := isolatedRollbackGuardPool(t)
	guard, err := NewRollbackMutationGuard(pool)
	if err != nil {
		t.Fatal(err)
	}
	key := domain.IdempotencyKey("key-unlock-failure")
	lock, err := guard.acquireRollbackMutationLock(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	session := lock.conn.Conn()
	failingCtx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := lock.release(failingCtx); err == nil {
		t.Fatal("release() error = nil, want unlock failure")
	}

	if !session.IsClosed() {
		t.Fatal("connection still holding the advisory lock was returned to the pool open")
	}
	var acquired bool
	if err := pool.QueryRow(context.Background(), `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, key).Scan(&acquired); err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("advisory lock is still held after a failed unlock")
	}
	if _, err := pool.Exec(context.Background(), `SELECT pg_advisory_unlock_all()`); err != nil {
		t.Fatal(err)
	}
}

func isolatedRollbackGuardPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return isolatedSchemaPool(t, "000001_initial_schema.up.sql", "000002_rollback_mutation_ledger.up.sql")
}

// isolatedSchemaPool returns a pool bound to a fresh schema with the named
// migrations applied in order. The schema is dropped when the test ends.
func isolatedSchemaPool(t *testing.T, migrations ...string) *pgxpool.Pool {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("ROLLBACK_GUARD_POSTGRES_TEST_DSN"))
	if dsn == "" {
		t.Skip("ROLLBACK_GUARD_POSTGRES_TEST_DSN is not configured")
	}
	adminConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(adminConfig.ConnConfig.Database), "test") {
		t.Fatalf("integration database %q must contain test", adminConfig.ConnConfig.Database)
	}
	admin, err := pgxpool.NewWithConfig(context.Background(), adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("rollback_guard_test_%d", time.Now().UnixNano())
	if !strings.HasPrefix(schema, "rollback_guard_test_") {
		t.Fatalf("unsafe test schema %q", schema)
	}
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(context.Background(), "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Errorf("drop isolated test schema: %v", err)
		}
	})
	testConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	testConfig.ConnConfig.RuntimeParams["search_path"] = schema
	testConfig.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	pool, err := pgxpool.NewWithConfig(context.Background(), testConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, name := range migrations {
		path := "../../../migrations/" + name
		migration, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if _, execErr := pool.Exec(context.Background(), string(migration)); execErr != nil {
			t.Fatalf("apply %s: %v", path, execErr)
		}
	}
	return pool
}

func seedTrustedRollback(t *testing.T, pool *pgxpool.Pool, incidentID string, key domain.IdempotencyKey, now time.Time) domain.RollbackProposal {
	t.Helper()
	return seedTrustedRollbackWithExpiry(t, pool, incidentID, key, now, now.Add(5*time.Minute))
}

func seedTrustedRollbackWithExpiry(t *testing.T, pool *pgxpool.Pool, incidentID string, key domain.IdempotencyKey, now time.Time, expiresAt time.Time) domain.RollbackProposal {
	t.Helper()
	target, err := domain.NewDeploymentTarget("local", "demo", "demo-api", "demo-api")
	if err != nil {
		t.Fatal(err)
	}
	decision, err := domain.NewPolicyDecision(domain.PolicyDecisionID("policy-"+incidentID), domain.IncidentID(incidentID), []domain.RuleResult{{Name: "eligible", Passed: true}}, now)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := domain.NewRollbackProposal(domain.ActionID("action-"+incidentID), domain.IncidentID(incidentID), decision.ID(), target, "revision-bad", "revision-stable", expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := domain.NewActor("operator", domain.ActorHuman)
	if err != nil {
		t.Fatal(err)
	}
	approval, err := domain.NewApproval(domain.ApprovalID("approval-"+incidentID), actor, domain.ApprovalApproved, proposal.ActionID(), proposal.Hash(), now, expiresAt, "")
	if err != nil {
		t.Fatal(err)
	}
	rules, err := json.Marshal(decision.Rules())
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// A no-op after the commit below; otherwise it undoes the fixture.
		if err := tx.Rollback(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("roll back fixture transaction: %v", err)
		}
	}()
	if _, err := tx.Exec(context.Background(), `
		INSERT INTO incidents (
			id, cluster, namespace, deployment, service, symptom_kind, symptom_observed,
			symptom_threshold, symptom_unit, symptom_started_at, status, version, created_at, updated_at
		) VALUES ($1,'local','demo','demo-api','demo-api','http_error_rate',0.5,0.1,'ratio',$2,'executing',4,$2,$2)
	`, incidentID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `INSERT INTO policy_decisions (id, incident_id, allowed, rules, decided_at) VALUES ($1,$2,true,$3,$4)`, decision.ID(), incidentID, string(rules), now); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `
		INSERT INTO action_proposals (
			action_id, incident_id, policy_decision_id, cluster, namespace, deployment, service,
			current_revision, target_revision, expires_at, proposal_hash
		) VALUES ($1,$2,$3,'local','demo','demo-api','demo-api',$4,$5,$6,$7)
	`, proposal.ActionID(), incidentID, proposal.PolicyDecisionID(), proposal.CurrentRevision(), proposal.TargetRevision(), proposal.ExpiresAt(), proposal.Hash()); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `
		INSERT INTO approvals (
			id, incident_id, actor_id, actor_kind, decision, action_id, proposal_hash, decided_at, expires_at, rejection_reason
		) VALUES ($1,$2,$3,'human','approved',$4,$5,$6,$7,'')
	`, approval.ID(), incidentID, approval.Actor().ID(), approval.ActionID(), approval.ProposalHash(), approval.DecidedAt(), approval.ExpiresAt()); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `
		INSERT INTO idempotency_keys (key, operation, request_hash, resource_id, state, started_at)
		VALUES ($1,'execute_rollback',$2,$3,'pending',$4)
	`, key, proposal.Hash(), incidentID, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	return proposal
}
