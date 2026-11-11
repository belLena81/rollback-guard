package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type UnitOfWork struct {
	pool *pgxpool.Pool
}

func NewUnitOfWork(pool *pgxpool.Pool) (*UnitOfWork, error) {
	if pool == nil {
		return nil, errors.New("postgres pool is required")
	}
	return &UnitOfWork{pool: pool}, nil
}

func (u *UnitOfWork) WithinTransaction(ctx context.Context, fn func(ports.Repositories) error) error {
	return withinTransaction(ctx, u.pool, func(tx pgx.Tx) error {
		return fn(&repositories{ctx: ctx, tx: tx})
	})
}

// withinTransaction runs fn in one transaction on pool. fn's error is
// returned unwrapped so it keeps its domain identity.
func withinTransaction(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	return withinTransactionOptions(ctx, pool, pgx.TxOptions{}, fn)
}

// withinReadTransaction is withinTransaction with the access mode set to
// READ ONLY: PostgreSQL rejects every write, lock-for-update and DDL
// statement inside it.
func withinReadTransaction(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	return withinTransactionOptions(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, fn)
}

func withinTransactionOptions(ctx context.Context, pool *pgxpool.Pool, options pgx.TxOptions, fn func(pgx.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("context ended: %w", err)
	}
	// pgx.BeginFunc rolls back in a deferred call, so a panicking callback
	// still ends the transaction and returns the connection to the pool.
	var started, finished bool
	err := pgx.BeginTxFunc(ctx, pool, options, func(tx pgx.Tx) error {
		started = true
		if err := fn(tx); err != nil {
			return err
		}
		finished = true
		return nil
	})
	switch {
	case err == nil:
		return nil
	case !started:
		return fmt.Errorf("begin transaction: %w", err)
	case finished:
		return fmt.Errorf("commit transaction: %w", err)
	default:
		return err //nolint:wrapcheck // callback error keeps its domain identity; callers match it with errors.As
	}
}

type repositories struct {
	ctx context.Context
	tx  pgx.Tx
}

func (r *repositories) Incidents() ports.IncidentRepository {
	return incidentRepo{r}
}

func (r *repositories) Evidence() ports.EvidenceRepository {
	return evidenceRepo{r}
}

func (r *repositories) Conclusions() ports.ConclusionRepository {
	return conclusionRepo{r}
}

func (r *repositories) PolicyDecisions() ports.PolicyDecisionRepository {
	return decisionRepo{r}
}

func (r *repositories) Proposals() ports.ProposalRepository {
	return proposalRepo{r}
}

func (r *repositories) Approvals() ports.ApprovalRepository {
	return approvalRepo{r}
}

func (r *repositories) Executions() ports.ExecutionRepository {
	return executionRepo{r}
}

func (r *repositories) Verifications() ports.VerificationRepository {
	return verificationRepo{r}
}

func (r *repositories) Idempotency() ports.IdempotencyRepository {
	return idempotencyRepo{r}
}

func (r *repositories) InvestigationTasks() ports.InvestigationTaskRepository {
	return investigationTaskRepo{r}
}

func (r *repositories) Events() ports.EventRepository {
	return eventRepo{r}
}

// uniqueViolation is the PostgreSQL SQLSTATE for a unique constraint violation.
const uniqueViolation = "23505"

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation
}

func mapError(err error, resource, id string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return &domain.NotFoundError{Resource: resource, ID: id}
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case uniqueViolation:
			return &domain.AlreadyExistsError{Resource: resource}
		case "23503":
			return &domain.NotFoundError{Resource: resource, ID: id}
		case "23514":
			return &domain.ValidationError{Field: resource, Reason: "violates persistence constraint"}
		}
	}
	return err
}

var _ ports.UnitOfWork = (*UnitOfWork)(nil)
