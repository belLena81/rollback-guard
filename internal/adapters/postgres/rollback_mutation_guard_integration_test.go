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

	completed, err := guards[0].Complete(context.Background(), first.Mutation, false, now.Add(time.Second))
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
	if err := first.Release(context.Background(), attempt.Mutation); err != nil {
		t.Fatal(err)
	}

	replayed, err := second.Begin(context.Background(), "key-2", proposal.Hash(), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Mutation.Snapshot() != attempt.Mutation.Snapshot() {
		t.Fatalf("stale pending replay = %#v, want %#v", replayed.Mutation.Snapshot(), attempt.Mutation.Snapshot())
	}
	if err := second.Release(context.Background(), replayed.Mutation); err != nil {
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
	if err := guard.Release(context.Background(), attempt.Mutation); err != nil {
		t.Fatal(err)
	}
}

func isolatedRollbackGuardPool(t *testing.T) *pgxpool.Pool {
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
	for _, path := range []string{"../../../migrations/000001_initial_schema.up.sql", "../../../migrations/000002_rollback_mutation_ledger.up.sql"} {
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
	defer func() { _ = tx.Rollback(context.Background()) }()
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
