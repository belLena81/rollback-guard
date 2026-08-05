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
		return nil, fmt.Errorf("postgres pool is required")
	}
	return &UnitOfWork{pool: pool}, nil
}

func (u *UnitOfWork) WithinTransaction(ctx context.Context, fn func(ports.Repositories) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := u.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	repositories := &repositories{ctx: ctx, tx: tx}
	if err := fn(repositories); err != nil {
		return errors.Join(err, tx.Rollback(ctx))
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
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
		case "23505":
			return &domain.ValidationError{Field: resource, Reason: "already exists"}
		case "23503":
			return &domain.NotFoundError{Resource: resource, ID: id}
		case "23514":
			return &domain.ValidationError{Field: resource, Reason: "violates persistence constraint"}
		}
	}
	return err
}

var _ ports.UnitOfWork = (*UnitOfWork)(nil)
