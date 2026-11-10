package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type RollbackMutationGuard struct {
	pool  *pgxpool.Pool
	mu    sync.Mutex
	locks map[domain.IdempotencyKey]*rollbackMutationLock
}

func NewRollbackMutationGuard(pool *pgxpool.Pool) (*RollbackMutationGuard, error) {
	if pool == nil {
		return nil, fmt.Errorf("postgres pool is required")
	}
	return &RollbackMutationGuard{pool: pool, locks: make(map[domain.IdempotencyKey]*rollbackMutationLock)}, nil
}

func (g *RollbackMutationGuard) Begin(ctx context.Context, key domain.IdempotencyKey, hash domain.ProposalHash, startedAt time.Time) (_ ports.RollbackMutationAttempt, resultErr error) {
	if key == "" || hash == "" || startedAt.IsZero() {
		return ports.RollbackMutationAttempt{}, &domain.ValidationError{Field: "rollback_mutation", Reason: "invalid key, hash, or start time"}
	}
	lock, err := g.acquireRollbackMutationLock(ctx, key)
	if err != nil {
		return ports.RollbackMutationAttempt{}, err
	}
	releaseOnReturn := true
	defer func() {
		if releaseOnReturn {
			resultErr = errors.Join(resultErr, lock.release(context.Background()))
		}
	}()
	tx, err := lock.conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return ports.RollbackMutationAttempt{}, fmt.Errorf("begin rollback mutation: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			resultErr = errors.Join(resultErr, fmt.Errorf("rollback mutation transaction: %w", rollbackErr))
		}
	}()

	existing, found, err := loadRollbackMutation(ctx, tx, key)
	if err != nil {
		return ports.RollbackMutationAttempt{}, err
	}
	if found {
		if existing.ProposalHash() != hash {
			return ports.RollbackMutationAttempt{}, &domain.IdempotencyConflictError{Key: key}
		}
		if existing.State() == domain.RollbackMutationCompleted {
			if err := tx.Commit(ctx); err != nil {
				return ports.RollbackMutationAttempt{}, fmt.Errorf("commit rollback mutation replay: %w", err)
			}
			return ports.RollbackMutationAttempt{Mutation: existing}, nil
		}
	}
	repositories := &repositories{ctx: ctx, tx: tx}
	record, err := repositories.Idempotency().Get(key)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return ports.RollbackMutationAttempt{}, &domain.ApprovalMismatchError{Reason: "trusted execution record is missing"}
		}
		return ports.RollbackMutationAttempt{}, err
	}
	if record.Operation() != domain.IdempotencyOperationExecuteRollback || record.State() != domain.IdempotencyPending || record.RequestHash() != string(hash) {
		if record.RequestHash() != string(hash) {
			return ports.RollbackMutationAttempt{}, &domain.IdempotencyConflictError{Key: key}
		}
		return ports.RollbackMutationAttempt{}, &domain.ApprovalMismatchError{Reason: "trusted execution is not pending"}
	}
	incidentID := domain.IncidentID(record.ResourceID())
	incident, err := repositories.Incidents().Get(incidentID)
	if err != nil {
		return ports.RollbackMutationAttempt{}, err
	}
	if incident.Status() != domain.StatusExecuting {
		return ports.RollbackMutationAttempt{}, &domain.ApprovalMismatchError{Reason: "incident is not executing"}
	}
	proposal, err := repositories.Proposals().GetByIncident(incidentID)
	if err != nil {
		return ports.RollbackMutationAttempt{}, err
	}
	if proposal.Hash() != hash {
		return ports.RollbackMutationAttempt{}, &domain.IdempotencyConflictError{Key: key}
	}
	decision, err := repositories.PolicyDecisions().Get(proposal.PolicyDecisionID())
	if err != nil {
		return ports.RollbackMutationAttempt{}, err
	}
	approval, err := repositories.Approvals().GetByIncident(incidentID)
	if err != nil {
		return ports.RollbackMutationAttempt{}, err
	}
	var approved domain.ApprovedRollback
	if found {
		approved, err = domain.ResumeApprovedRollback(proposal, decision, approval, record, key)
	} else {
		approved, err = domain.AuthorizeRollback(
			proposal,
			decision,
			approval,
			domain.DeploymentEvidence{Target: proposal.Target(), CurrentRevision: proposal.CurrentRevision()},
			record.StartedAt(),
		)
	}
	if err != nil {
		return ports.RollbackMutationAttempt{}, err
	}
	if !found {
		existing, err = domain.NewPendingRollbackMutation(key, hash, rollbackMutationExecutionID(key, hash), proposal.CurrentRevision(), proposal.TargetRevision(), record.StartedAt())
		if err != nil {
			return ports.RollbackMutationAttempt{}, err
		}
		snapshot := existing.Snapshot()
		if _, err := tx.Exec(ctx, `
			INSERT INTO rollback_mutations (
				idempotency_key, proposal_hash, execution_id, from_revision, to_revision,
				state, started_at, completed_at, already_applied
			) VALUES ($1,$2,$3,$4,$5,$6,$7,NULL,NULL)
		`, snapshot.Key, snapshot.ProposalHash, snapshot.ExecutionID, snapshot.FromRevision, snapshot.ToRevision, rollbackMutationStateText(snapshot.State), snapshot.StartedAt); err != nil {
			return ports.RollbackMutationAttempt{}, fmt.Errorf("insert rollback mutation: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return ports.RollbackMutationAttempt{}, fmt.Errorf("commit rollback mutation start: %w", err)
	}
	releaseOnReturn = false
	g.retainRollbackMutationLock(key, lock)
	return ports.RollbackMutationAttempt{Mutation: existing, Approved: approved}, nil
}

func (g *RollbackMutationGuard) Complete(ctx context.Context, pending domain.RollbackMutation, alreadyApplied bool, completedAt time.Time) (_ domain.RollbackMutation, resultErr error) {
	lock, err := g.takeRollbackMutationLock(ctx, pending.Key())
	if err != nil {
		return domain.RollbackMutation{}, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, lock.release(context.Background()))
	}()
	completed, err := pending.Complete(alreadyApplied, completedAt)
	if err != nil {
		return domain.RollbackMutation{}, err
	}
	tx, err := lock.conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.RollbackMutation{}, fmt.Errorf("begin rollback mutation completion: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			resultErr = errors.Join(resultErr, fmt.Errorf("rollback mutation completion transaction: %w", rollbackErr))
		}
	}()
	snapshot := completed.Snapshot()
	command, err := tx.Exec(ctx, `
		UPDATE rollback_mutations
		SET state = $1, completed_at = $2, already_applied = $3
		WHERE idempotency_key = $4 AND proposal_hash = $5 AND execution_id = $6
			AND from_revision = $7 AND to_revision = $8 AND started_at = $9 AND state = $10
	`, rollbackMutationStateText(snapshot.State), snapshot.CompletedAt, snapshot.AlreadyApplied,
		snapshot.Key, snapshot.ProposalHash, snapshot.ExecutionID, snapshot.FromRevision,
		snapshot.ToRevision, snapshot.StartedAt, rollbackMutationStateText(domain.RollbackMutationPending))
	if err != nil {
		return domain.RollbackMutation{}, fmt.Errorf("complete rollback mutation: %w", err)
	}
	if command.RowsAffected() == 0 {
		winner, found, loadErr := loadRollbackMutation(ctx, tx, pending.Key())
		if loadErr != nil {
			return domain.RollbackMutation{}, loadErr
		}
		if !found || winner.ProposalHash() != pending.ProposalHash() || winner.State() != domain.RollbackMutationCompleted {
			return domain.RollbackMutation{}, &domain.IdempotencyConflictError{Key: pending.Key()}
		}
		completed = winner
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.RollbackMutation{}, fmt.Errorf("commit rollback mutation completion: %w", err)
	}
	return completed, nil
}

func (g *RollbackMutationGuard) Release(_ context.Context, pending domain.RollbackMutation) error {
	lock := g.removeRollbackMutationLock(pending.Key())
	if lock == nil {
		return nil
	}
	return lock.release(context.Background())
}

type rollbackMutationLock struct {
	key         domain.IdempotencyKey
	conn        *pgxpool.Conn
	releaseHook func(context.Context) error
}

func (g *RollbackMutationGuard) acquireRollbackMutationLock(ctx context.Context, key domain.IdempotencyKey) (*rollbackMutationLock, error) {
	conn, err := g.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire rollback mutation lock connection: %w", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, key); err != nil {
		conn.Release()
		return nil, fmt.Errorf("lock rollback mutation key: %w", err)
	}
	return &rollbackMutationLock{key: key, conn: conn}, nil
}

func (g *RollbackMutationGuard) retainRollbackMutationLock(key domain.IdempotencyKey, lock *rollbackMutationLock) {
	g.mu.Lock()
	previous := g.locks[key]
	g.locks[key] = lock
	g.mu.Unlock()
	if previous != nil {
		// Begin has already committed by the time a lock is retained, so a failure to
		// unlock the stale, replaced lock must not fail the new attempt. release still
		// returns the connection to the pool even when the unlock fails.
		_ = previous.release(context.Background()) //nolint:errcheck // intentional: see comment above; covered by TestRetainRollbackMutationLockIgnoresReplacedLockReleaseFailure
	}
}

func (g *RollbackMutationGuard) takeRollbackMutationLock(ctx context.Context, key domain.IdempotencyKey) (*rollbackMutationLock, error) {
	lock := g.removeRollbackMutationLock(key)
	if lock != nil {
		return lock, nil
	}
	return g.acquireRollbackMutationLock(ctx, key)
}

func (g *RollbackMutationGuard) removeRollbackMutationLock(key domain.IdempotencyKey) *rollbackMutationLock {
	g.mu.Lock()
	defer g.mu.Unlock()
	lock := g.locks[key]
	delete(g.locks, key)
	return lock
}

func (l *rollbackMutationLock) release(ctx context.Context) error {
	if l != nil && l.releaseHook != nil {
		return l.releaseHook(ctx)
	}
	if l == nil || l.conn == nil {
		return nil
	}
	defer l.conn.Release()
	if _, err := l.conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, l.key); err != nil {
		return fmt.Errorf("unlock rollback mutation key: %w", err)
	}
	return nil
}

func loadRollbackMutation(ctx context.Context, tx pgx.Tx, key domain.IdempotencyKey) (domain.RollbackMutation, bool, error) {
	var snapshot domain.RollbackMutationSnapshot
	var state string
	var completedAt *time.Time
	var alreadyApplied *bool
	err := tx.QueryRow(ctx, `
		SELECT idempotency_key, proposal_hash, execution_id, from_revision, to_revision,
			state, started_at, completed_at, already_applied
		FROM rollback_mutations WHERE idempotency_key = $1
	`, key).Scan(&snapshot.Key, &snapshot.ProposalHash, &snapshot.ExecutionID, &snapshot.FromRevision, &snapshot.ToRevision, &state, &snapshot.StartedAt, &completedAt, &alreadyApplied)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.RollbackMutation{}, false, nil
	}
	if err != nil {
		return domain.RollbackMutation{}, false, fmt.Errorf("load rollback mutation: %w", err)
	}
	snapshot.State, err = parseRollbackMutationState(state)
	if err != nil {
		return domain.RollbackMutation{}, false, err
	}
	if completedAt != nil {
		snapshot.CompletedAt = *completedAt
	}
	if alreadyApplied != nil {
		snapshot.AlreadyApplied = *alreadyApplied
	}
	record, err := domain.RehydrateRollbackMutation(snapshot)
	return record, true, err
}

func rollbackMutationExecutionID(key domain.IdempotencyKey, hash domain.ProposalHash) domain.ExecutionID {
	sum := sha256.Sum256([]byte(string(key) + "\n" + string(hash)))
	return domain.ExecutionID("mcp-mutation-" + hex.EncodeToString(sum[:])[:32])
}

func rollbackMutationStateText(state domain.RollbackMutationState) string {
	return state.String()
}

func parseRollbackMutationState(value string) (domain.RollbackMutationState, error) {
	switch value {
	case "pending":
		return domain.RollbackMutationPending, nil
	case "completed":
		return domain.RollbackMutationCompleted, nil
	default:
		return 0, fmt.Errorf("unknown rollback mutation state %q", value)
	}
}

var _ ports.RollbackMutationGuard = (*RollbackMutationGuard)(nil)
