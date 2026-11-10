package postgres

import (
	"context"
	"crypto/rand"
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
		return nil, errors.New("postgres pool is required")
	}
	return &RollbackMutationGuard{pool: pool, locks: make(map[domain.IdempotencyKey]*rollbackMutationLock)}, nil
}

func (g *RollbackMutationGuard) Begin(
	ctx context.Context,
	key domain.IdempotencyKey,
	hash domain.ProposalHash,
	startedAt time.Time,
) (result ports.RollbackMutationAttempt, resultErr error) {
	// Like Complete, Begin returns an attempt or an error, never both: a
	// failed deferred rollback or unlock clears the attempt.
	defer func() {
		if resultErr != nil {
			result = ports.RollbackMutationAttempt{}
		}
	}()
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
			// Detached on purpose: the lock must be dropped even if ctx is
			// cancelled; release applies its own timeout.
			resultErr = errors.Join(resultErr, lock.release(context.WithoutCancel(ctx)))
		}
	}()
	tx, err := lock.conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return ports.RollbackMutationAttempt{}, fmt.Errorf("begin rollback mutation: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, rollbackTransaction(ctx, tx, "rollback mutation transaction"))
	}()

	existing, found, err := loadRollbackMutation(ctx, tx, key)
	if err != nil {
		return ports.RollbackMutationAttempt{}, err
	}
	if found && existing.ProposalHash() != hash {
		return ports.RollbackMutationAttempt{}, &domain.IdempotencyConflictError{Key: key}
	}
	if found && existing.State() == domain.RollbackMutationCompleted {
		if err := tx.Commit(ctx); err != nil {
			return ports.RollbackMutationAttempt{}, fmt.Errorf("commit rollback mutation replay: %w", err)
		}
		return ports.RollbackMutationAttempt{Mutation: existing}, nil
	}
	trusted, err := authorizeTrustedExecution(ctx, tx, key, hash, found)
	if err != nil {
		return ports.RollbackMutationAttempt{}, err
	}
	if !found {
		existing, err = insertPendingRollbackMutation(ctx, tx, key, hash, trusted)
		if err != nil {
			return ports.RollbackMutationAttempt{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return ports.RollbackMutationAttempt{}, fmt.Errorf("commit rollback mutation start: %w", err)
	}
	releaseOnReturn = false
	g.retainRollbackMutationLock(ctx, key, lock)
	return ports.RollbackMutationAttempt{Mutation: existing, Approved: trusted.approved, Lease: lock.lease}, nil
}

// trustedExecution is the rollback the guard re-derived from the durable
// records, never from the caller.
type trustedExecution struct {
	approved domain.ApprovedRollback
	proposal domain.RollbackProposal
	record   domain.IdempotencyRecord
}

// authorizeTrustedExecution re-checks the pending execution, its incident,
// proposal, policy decision, and approval against the database inside tx, and
// authorizes the rollback from them. resume is true when a pending mutation
// already exists for the key, which is then resumed rather than authorized
// afresh.
func authorizeTrustedExecution(ctx context.Context, tx pgx.Tx, key domain.IdempotencyKey, hash domain.ProposalHash, resume bool) (trustedExecution, error) {
	repositories := &repositories{ctx: ctx, tx: tx}
	record, err := repositories.Idempotency().Get(key)
	if errors.Is(err, domain.ErrNotFound) {
		return trustedExecution{}, &domain.ApprovalMismatchError{Reason: "trusted execution record is missing"}
	}
	if err != nil {
		return trustedExecution{}, err
	}
	if record.RequestHash() != string(hash) {
		return trustedExecution{}, &domain.IdempotencyConflictError{Key: key}
	}
	if record.Operation() != domain.IdempotencyOperationExecuteRollback || record.State() != domain.IdempotencyPending {
		return trustedExecution{}, &domain.ApprovalMismatchError{Reason: "trusted execution is not pending"}
	}
	incidentID := domain.IncidentID(record.ResourceID())
	incident, err := repositories.Incidents().Get(incidentID)
	if err != nil {
		return trustedExecution{}, err
	}
	if incident.Status() != domain.StatusExecuting {
		return trustedExecution{}, &domain.ApprovalMismatchError{Reason: "incident is not executing"}
	}
	proposal, err := repositories.Proposals().GetByIncident(incidentID)
	if err != nil {
		return trustedExecution{}, err
	}
	if proposal.Hash() != hash {
		return trustedExecution{}, &domain.IdempotencyConflictError{Key: key}
	}
	decision, err := repositories.PolicyDecisions().Get(proposal.PolicyDecisionID())
	if err != nil {
		return trustedExecution{}, err
	}
	approval, err := repositories.Approvals().GetByIncident(incidentID)
	if err != nil {
		return trustedExecution{}, err
	}
	approved, err := authorizeOrResume(proposal, decision, approval, record, key, resume)
	if err != nil {
		return trustedExecution{}, err
	}
	return trustedExecution{approved: approved, proposal: proposal, record: record}, nil
}

func authorizeOrResume(
	proposal domain.RollbackProposal,
	decision domain.PolicyDecision,
	approval domain.Approval,
	record domain.IdempotencyRecord,
	key domain.IdempotencyKey,
	resume bool,
) (domain.ApprovedRollback, error) {
	if resume {
		return domain.ResumeApprovedRollback(proposal, decision, approval, record, key)
	}
	return domain.AuthorizeRollback(
		proposal,
		decision,
		approval,
		domain.DeploymentEvidence{Target: proposal.Target(), CurrentRevision: proposal.CurrentRevision()},
		record.StartedAt(),
	)
}

func insertPendingRollbackMutation(
	ctx context.Context,
	tx pgx.Tx,
	key domain.IdempotencyKey,
	hash domain.ProposalHash,
	trusted trustedExecution,
) (domain.RollbackMutation, error) {
	pending, err := domain.NewPendingRollbackMutation(
		key,
		hash,
		domain.DeterministicExecutionID(key, hash),
		trusted.proposal.CurrentRevision(),
		trusted.proposal.TargetRevision(),
		trusted.record.StartedAt(),
	)
	if err != nil {
		return domain.RollbackMutation{}, err
	}
	snapshot := pending.Snapshot()
	if _, err := tx.Exec(ctx, `
		INSERT INTO rollback_mutations (
			idempotency_key, proposal_hash, execution_id, from_revision, to_revision,
			state, started_at, completed_at, already_applied
		) VALUES ($1,$2,$3,$4,$5,$6,$7,NULL,NULL)
	`, snapshot.Key, snapshot.ProposalHash, snapshot.ExecutionID, snapshot.FromRevision, snapshot.ToRevision,
		rollbackMutationStateText(snapshot.State), snapshot.StartedAt); err != nil {
		return domain.RollbackMutation{}, fmt.Errorf("insert rollback mutation: %w", err)
	}
	return pending, nil
}

// Complete records the attempt's mutation as completed while the attempt's
// lease still holds the per-key lock, then releases that lock. An attempt
// whose lease is not held writes nothing.
//
// It returns either the completed mutation or an error, never both. When the
// completion commits but the lock release then fails, only the error is
// returned; the completion is durable, so a replay through Begin returns it.
func (g *RollbackMutationGuard) Complete(
	ctx context.Context,
	attempt ports.RollbackMutationAttempt,
	alreadyApplied bool,
	completedAt time.Time,
) (result domain.RollbackMutation, resultErr error) {
	pending := attempt.Mutation
	lock := g.takeRollbackMutationLock(pending.Key(), attempt.Lease)
	if lock == nil {
		return domain.RollbackMutation{}, fmt.Errorf("complete rollback mutation: %w", ports.ErrRollbackMutationLeaseNotHeld)
	}
	defer func() {
		// Detached on purpose: the lock must be dropped even if ctx is
		// cancelled; release applies its own timeout.
		resultErr = errors.Join(resultErr, lock.release(context.WithoutCancel(ctx)))
		if resultErr != nil {
			result = domain.RollbackMutation{}
		}
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
		resultErr = errors.Join(resultErr, rollbackTransaction(ctx, tx, "rollback mutation completion transaction"))
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
func (g *RollbackMutationGuard) Release(ctx context.Context, attempt ports.RollbackMutationAttempt) error {
	lock := g.takeRollbackMutationLock(attempt.Mutation.Key(), attempt.Lease)
	if lock == nil {
		return nil
	}
	// Detached on purpose (WithoutCancel keeps the values, drops the
	// cancellation; release bounds the unlock with its own timeout): the
	// advisory lock must be dropped even when the
	// caller's request has ended.
	return lock.release(context.WithoutCancel(ctx))
}

// rollbackMutationTxRollbackTimeout bounds the deferred transaction rollback,
// which runs detached from the caller's context.
const rollbackMutationTxRollbackTimeout = 5 * time.Second

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

func (g *RollbackMutationGuard) retainRollbackMutationLock(ctx context.Context, key domain.IdempotencyKey, lock *rollbackMutationLock) {
	g.mu.Lock()
	previous := g.locks[key]
	g.locks[key] = lock
	g.mu.Unlock()
	if previous != nil {
		// Detached on purpose, as in Release. Begin has already committed by the
		// time a lock is retained, so a failure to unlock the stale, replaced lock
		// must not fail the new attempt. On unlock failure release closes the
		// session, which drops its advisory lock. Covered by
		// TestRetainRollbackMutationLockIgnoresReplacedLockReleaseFailure.
		detached := context.WithoutCancel(ctx)
		_ = previous.release(detached) //nolint:errcheck // intentional: see comment above
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
	var unlocked bool
	err := l.conn.QueryRow(unlockCtx, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, l.key).Scan(&unlocked)
	switch {
	case err != nil:
		// The session may still hold the advisory lock.
		return l.closeSession(ctx, fmt.Errorf("unlock rollback mutation key: %w", err))
	case !unlocked:
		// PostgreSQL returns false when this session does not hold the lock:
		// the per-key mutual exclusion was already lost, so this is not a
		// successful release. The session's lock state is unexpected, so it is
		// closed rather than reused.
		return l.closeSession(ctx, fmt.Errorf("unlock rollback mutation key: %w", errRollbackMutationLockNotHeld))
	}
	return nil
}

// closeSession closes the lock's connection so the pool destroys it on
// Release instead of handing a possibly lock-holding session to another
// caller; ending the session releases its advisory locks server-side. It
// returns cause, joined with any close failure.
func (l *rollbackMutationLock) closeSession(ctx context.Context, cause error) error {
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackMutationLockCloseTimeout)
	defer cancel()
	if closeErr := l.conn.Conn().Close(closeCtx); closeErr != nil {
		return errors.Join(cause, fmt.Errorf("close rollback mutation lock connection: %w", closeErr))
	}
	return cause
}

// errRollbackMutationLockNotHeld reports that pg_advisory_unlock found no lock
// held by the attempt's session.
var errRollbackMutationLockNotHeld = errors.New("rollback mutation advisory lock was not held")

// rollbackTransaction rolls tx back unless it has already been committed.
//
// Detached on purpose: it runs from deferred cleanup, often after ctx was
// cancelled. On a cancelled ctx the rollback would fail at once and pgx would
// drop the connection, and with it the session's advisory lock, before the
// guard's own unlock runs. WithoutCancel keeps ctx's values (trace
// correlation); the timeout bounds a blackholed session.
func rollbackTransaction(ctx context.Context, tx pgx.Tx, operation string) error {
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackMutationTxRollbackTimeout)
	defer cancel()
	if err := tx.Rollback(rollbackCtx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return fmt.Errorf("%s: %w", operation, err)
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
	`, key).Scan(&snapshot.Key, &snapshot.ProposalHash, &snapshot.ExecutionID, &snapshot.FromRevision, &snapshot.ToRevision,
		&state, &snapshot.StartedAt, &completedAt, &alreadyApplied)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.RollbackMutation{}, false, nil
	}
	if err != nil {
		return domain.RollbackMutation{}, false, fmt.Errorf("load rollback mutation: %w", err)
	}
	snapshot.State, err = domain.ParseRollbackMutationState(state)
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

func rollbackMutationStateText(state domain.RollbackMutationState) string {
	return state.String()
}

var _ ports.RollbackMutationGuard = (*RollbackMutationGuard)(nil)
