package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type RollbackMutationGuard struct {
	pool *pgxpool.Pool
}

func NewRollbackMutationGuard(pool *pgxpool.Pool) (*RollbackMutationGuard, error) {
	if pool == nil {
		return nil, fmt.Errorf("postgres pool is required")
	}
	return &RollbackMutationGuard{pool: pool}, nil
}

func (g *RollbackMutationGuard) Begin(ctx context.Context, key domain.IdempotencyKey, hash domain.ProposalHash, startedAt time.Time) (_ ports.RollbackMutationAttempt, resultErr error) {
	if key == "" || hash == "" || startedAt.IsZero() {
		return ports.RollbackMutationAttempt{}, &domain.ValidationError{Field: "rollback_mutation", Reason: "invalid key, hash, or start time"}
	}
	tx, err := g.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return ports.RollbackMutationAttempt{}, fmt.Errorf("begin rollback mutation: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			resultErr = errors.Join(resultErr, fmt.Errorf("rollback mutation transaction: %w", rollbackErr))
		}
	}()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
		return ports.RollbackMutationAttempt{}, fmt.Errorf("lock rollback mutation key: %w", err)
	}
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
			startedAt,
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
	return ports.RollbackMutationAttempt{Mutation: existing, Approved: approved}, nil
}

func (g *RollbackMutationGuard) Complete(ctx context.Context, pending domain.RollbackMutation, alreadyApplied bool, completedAt time.Time) (_ domain.RollbackMutation, resultErr error) {
	completed, err := pending.Complete(alreadyApplied, completedAt)
	if err != nil {
		return domain.RollbackMutation{}, err
	}
	tx, err := g.pool.BeginTx(ctx, pgx.TxOptions{})
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
