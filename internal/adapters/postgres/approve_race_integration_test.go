//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/application/applicationtest"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Phase 7 gap 3: ApprovalService.Approve authorizes in one auth transaction
// and records the approval in a separate incident transaction. A demotion
// that commits between the two is not seen by the record step (the auth and
// incident stores are deliberately separate ports), so the approval is
// recorded. These tests pin that such an approval can never execute: the
// rollback MCP's guard re-checks the approver's current role in the same
// transaction that re-loads the approval (gap 2).

const raceApproverGitHubID = 7001

// demoteAfterAuthorize runs the real role policy and, right after an approve
// authorization succeeds, runs the hook once: a deterministic stand-in for a
// concurrent admin demotion landing between Authorize and the record step.
// Other actions (the system actor's incident creation, reads) are allowed, as
// the harness does by default.
type demoteAfterAuthorize struct {
	role  *application.RoleAccessPolicy
	hook  func()
	fired bool
}

func (p *demoteAfterAuthorize) Authorize(ctx context.Context, actor domain.Actor, action domain.Action) error {
	if action != domain.ActionApproveRollback {
		return applicationtest.AllowAll{}.Authorize(ctx, actor, action) //nolint:wrapcheck // test double
	}
	if err := p.role.Authorize(ctx, actor, action); err != nil {
		return err //nolint:wrapcheck // test double
	}
	if !p.fired {
		p.fired = true
		p.hook()
	}
	return nil
}

// guardExecutor stands in for the rollback MCP: it runs the real mutation
// guard (github mode) and, when the guard admits the attempt, completes it
// without touching Kubernetes.
type guardExecutor struct {
	guard *RollbackMutationGuard
	err   *error
}

func (e guardExecutor) ExecuteApprovedRollback(ctx context.Context, approved domain.ApprovedRollback, key domain.IdempotencyKey) (domain.RollbackExecution, error) {
	now := time.Now().UTC()
	attempt, err := e.guard.Begin(ctx, key, approved.Proposal().Hash(), now)
	*e.err = err
	if err != nil {
		return domain.RollbackExecution{}, err
	}
	if _, err := e.guard.Complete(ctx, attempt, false, now); err != nil {
		return domain.RollbackExecution{}, err
	}
	return domain.RollbackExecution{}, errors.New("guard admitted the attempt; this test expects a refusal")
}

func newApproveRaceHarness(t *testing.T, demote func(*pgxpool.Pool)) (*approvePostgresHarness, *pgxpool.Pool, *error) {
	t.Helper()
	var appPool *pgxpool.Pool
	var guardErr error
	h := newApprovePostgresHarnessWith(t, approveHarnessOptions{
		human: "github:7001",
		access: func(pool *pgxpool.Pool) application.AccessPolicy {
			appPool = pool
			seedApproverUser(t, pool, raceApproverGitHubID, "administrator", false)
			auth, err := NewAuthUnitOfWork(pool)
			if err != nil {
				t.Fatal(err)
			}
			role, err := application.NewRoleAccessPolicy(auth)
			if err != nil {
				t.Fatal(err)
			}
			return &demoteAfterAuthorize{role: role, hook: func() { demote(pool) }}
		},
		executor: func(pool *pgxpool.Pool) ports.RollbackExecutor {
			guard, err := NewRollbackMutationGuard(pool, NewGitHubApproverVerifier())
			if err != nil {
				t.Fatal(err)
			}
			return guardExecutor{guard: guard, err: &guardErr}
		},
	})
	return h, appPool, &guardErr
}

func TestApprovalRecordedAcrossADemotionCannotExecute(t *testing.T) {
	tests := []struct {
		name     string
		role     string
		disabled bool
	}{
		{name: "demoted to operator", role: "operator"},
		{name: "disabled", role: "administrator", disabled: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h, pool, guardErr := newApproveRaceHarness(t, func(pool *pgxpool.Pool) {
				setApproverUser(t, pool, raceApproverGitHubID, test.role, test.disabled)
			})

			// Approve is authorized against the then-current administrator;
			// the hook demotes before the record step, which does not re-read
			// users (documented gap 3 residue), so the approval is recorded.
			// Execution is then refused by the guard's re-check.
			_, err := h.services.ApproveAndExecute.Run(context.Background(), h.command)
			if err != nil && !errors.Is(err, domain.ErrApproverNotAuthorized) {
				t.Fatalf("Run() error = %v", err)
			}
			var refused *domain.ApproverNotAuthorizedError
			if !errors.As(*guardErr, &refused) {
				t.Fatalf("guard error = %v, want ApproverNotAuthorizedError", *guardErr)
			}
			var approvals int
			if err := pool.QueryRow(context.Background(),
				`SELECT count(*) FROM approvals WHERE actor_id = 'github:7001' AND decision = 'approved'`).Scan(&approvals); err != nil {
				t.Fatal(err)
			}
			if approvals != 1 {
				t.Fatalf("recorded approvals = %d, want 1 (the race records it)", approvals)
			}
			var rows int
			if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM rollback_mutations`).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if rows != 0 {
				t.Fatalf("rollback_mutations rows = %d, want 0", rows)
			}
			if got := h.infra.ExecutionCount(); got != 0 {
				t.Fatalf("mutations = %d, want 0", got)
			}
			after, err := h.queries.GetIncidentSummary(context.Background(), applicationtest.Reader(), h.incident)
			if err != nil {
				t.Fatal(err)
			}
			if after.Incident.Status() != domain.StatusFailed {
				t.Fatalf("incident status = %s, want failed (execution refused)", after.Incident.Status())
			}
		})
	}
}
