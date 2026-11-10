package postgres

import (
	"context"
	"crypto/rand"
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
	return ports.RollbackMutationAttempt{Mutation: existing, Approved: approved, Lease: lock.lease}, nil
}

// Complete records the attempt's mutation as completed while the attempt's
// lease still holds the per-key lock, then releases that lock. An attempt
// whose lease is not held writes nothing.
func (g *RollbackMutationGuard) Complete(ctx context.Context, attempt ports.RollbackMutationAttempt, alreadyApplied bool, completedAt time.Time) (_ domain.RollbackMutation, resultErr error) {
	pending := attempt.Mutation
	lock := g.takeRollbackMutationLock(pending.Key(), attempt.Lease)
	if lock == nil {
		return domain.RollbackMutation{}, fmt.Errorf("complete rollback mutation: %w", ports.ErrRollbackMutationLeaseNotHeld)
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

// Release unlocks the lock acquired by attempt's lease. A lock held by any
// other attempt for the same key, including a later replay, is left alone.
func (g *RollbackMutationGuard) Release(_ context.Context, attempt ports.RollbackMutationAttempt) error {
	lock := g.takeRollbackMutationLock(attempt.Mutation.Key(), attempt.Lease)
	if lock == nil {
		return nil
	}
	// Detached on purpose: the advisory lock must be dropped even when the
	// caller's request has ended.
	return lock.release(context.Background())
}

// rollbackMutationLockCloseTimeout bounds the advisory unlock and, when the
// unlock fails, closing its connection; the underlying network connection is
// closed even if it elapses.
const rollbackMutationLockCloseTimeout = 5 * time.Second

// rollbackMutationLeaseBytes is the entropy of a lease: 128 bits, so a lease
// can neither collide with nor be guessed from another attempt's.
const rollbackMutationLeaseBytes = 16

type rollbackMutationLock struct {
	key         domain.IdempotencyKey
	lease       ports.RollbackMutationLease
	conn        *pgxpool.Conn
	releaseHook func(context.Context) error
}

func newRollbackMutationLease() ports.RollbackMutationLease {
	var raw [rollbackMutationLeaseBytes]byte
	// crypto/rand.Read never returns an error (it crashes the program
	// instead), so the lease is always fully random.
	_, _ = rand.Read(raw[:]) //nolint:errcheck // documented never to fail
	return ports.RollbackMutationLease(hex.EncodeToString(raw[:]))
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
	return &rollbackMutationLock{key: key, lease: newRollbackMutationLease(), conn: conn}, nil
}

func (g *RollbackMutationGuard) retainRollbackMutationLock(key domain.IdempotencyKey, lock *rollbackMutationLock) {
	g.mu.Lock()
	previous := g.locks[key]
	g.locks[key] = lock
	g.mu.Unlock()
	if previous != nil {
		// Begin has already committed by the time a lock is retained, so a failure to
		// unlock the stale, replaced lock must not fail the new attempt. On unlock
		// failure release closes the session, which drops its advisory lock.
		_ = previous.release(context.Background()) //nolint:errcheck // intentional: see comment above; covered by TestRetainRollbackMutationLockIgnoresReplacedLockReleaseFailure
	}
}

// takeRollbackMutationLock removes and returns the retained lock for key only
// when it was acquired under lease. It returns nil, and leaves the map
// untouched, for an empty lease or a lock owned by another attempt.
func (g *RollbackMutationGuard) takeRollbackMutationLock(key domain.IdempotencyKey, lease ports.RollbackMutationLease) *rollbackMutationLock {
	if lease == "" {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	lock := g.locks[key]
	if lock == nil || lock.lease != lease {
		return nil
	}
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
	// Bounded so a blackholed session cannot hang the caller; on timeout the
	// session is closed below, which drops the lock server-side.
	unlockCtx, cancelUnlock := context.WithTimeout(ctx, rollbackMutationLockCloseTimeout)
	defer cancelUnlock()
	if _, err := l.conn.Exec(unlockCtx, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, l.key); err != nil {
		// The session may still hold the advisory lock. Close it so the pool destroys
		// the connection on Release instead of handing a lock-holding session to
		// another caller; ending the session releases the lock server-side.
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackMutationLockCloseTimeout)
		defer cancel()
		unlockErr := fmt.Errorf("unlock rollback mutation key: %w", err)
		if closeErr := l.conn.Conn().Close(closeCtx); closeErr != nil {
			return errors.Join(unlockErr, fmt.Errorf("close rollback mutation lock connection: %w", closeErr))
		}
		return unlockErr
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
