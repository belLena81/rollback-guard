//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRollbackMutationGuardSerializesFirstUseAcrossInstances(t *testing.T) {
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

	start := make(chan struct{})
	results := make(chan ports.RollbackMutationAttempt, len(guards))
	errCh := make(chan error, len(guards))
	var wait sync.WaitGroup
	for _, guard := range guards {
		wait.Add(1)
		go func(guard *RollbackMutationGuard) {
			defer wait.Done()
			<-start
			attempt, err := guard.Begin(context.Background(), "key-1", proposal.Hash(), now)
			results <- attempt
			errCh <- err
		}(guard)
	}
	close(start)
	wait.Wait()
	close(results)
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	var want domain.RollbackMutationSnapshot
	var attempts []ports.RollbackMutationAttempt
	for attempt := range results {
		attempts = append(attempts, attempt)
		got := attempt.Mutation.Snapshot()
		if want.Key == "" {
			want = got
			continue
		}
		if got != want {
			t.Fatalf("concurrent mutation = %#v, want %#v", got, want)
		}
	}
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM rollback_mutations WHERE idempotency_key = 'key-1'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("rollback mutation rows = %d, want 1", count)
	}

	completionResults := make(chan domain.RollbackMutation, len(guards))
	errCh = make(chan error, len(guards))
	for index, guard := range guards {
		wait.Add(1)
		go func(index int, guard *RollbackMutationGuard) {
			defer wait.Done()
			completed, err := guard.Complete(context.Background(), attempts[index].Mutation, index == 1, now.Add(time.Duration(index+1)*time.Second))
			completionResults <- completed
			errCh <- err
		}(index, guard)
	}
	wait.Wait()
	close(completionResults)
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	var completed domain.RollbackMutationSnapshot
	for result := range completionResults {
		got := result.Snapshot()
		if completed.Key == "" {
			completed = got
			continue
		}
		if got != completed {
			t.Fatalf("concurrent completion = %#v, want %#v", got, completed)
		}
	}
	if _, err := pool.Exec(context.Background(), `
		UPDATE idempotency_keys SET state = 'completed', result_id = 'application-execution', completed_at = $1 WHERE key = 'key-1';
		UPDATE incidents SET status = 'verifying', version = version + 1, updated_at = $1 WHERE id = 'incident-1'
	`, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	replayed, err := guards[0].Begin(context.Background(), "key-1", proposal.Hash(), now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Mutation.Snapshot() != completed {
		t.Fatalf("completed replay = %#v, want %#v", replayed.Mutation.Snapshot(), completed)
	}
	if _, err := guards[1].Begin(context.Background(), "key-1", "different-hash", now.Add(4*time.Second)); !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("completed conflicting replay error = %v", err)
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
	target, err := domain.NewDeploymentTarget("local", "demo", "demo-api", "demo-api")
	if err != nil {
		t.Fatal(err)
	}
	decision, err := domain.NewPolicyDecision(domain.PolicyDecisionID("policy-"+incidentID), domain.IncidentID(incidentID), []domain.RuleResult{{Name: "eligible", Passed: true}}, now)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := domain.NewRollbackProposal(domain.ActionID("action-"+incidentID), domain.IncidentID(incidentID), decision.ID(), target, "revision-bad", "revision-stable", now.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	actor, err := domain.NewActor("operator", domain.ActorHuman)
	if err != nil {
		t.Fatal(err)
	}
	approval, err := domain.NewApproval(domain.ApprovalID("approval-"+incidentID), actor, domain.ApprovalApproved, proposal.ActionID(), proposal.Hash(), now, now.Add(5*time.Minute), "")
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
	if _, err := tx.Exec(context.Background(), `INSERT INTO policy_decisions (id, incident_id, allowed, rules, decided_at) VALUES ($1,$2,true,$3,$4)`, decision.ID(), incidentID, rules, now); err != nil {
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
