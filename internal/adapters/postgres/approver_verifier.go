package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/jackc/pgx/v5"
)

// ApproverVerifier decides, inside the rollback mutation guard's transaction
// and in the same read that re-checks the approval record, whether the actor
// who approved may still have the rollback executed. It returns nil to allow,
// *domain.ApproverNotAuthorizedError to refuse, and any other error for an
// infrastructure failure. The interface is sealed: only this package's
// constructors build one.
type ApproverVerifier interface {
	verifyApprover(ctx context.Context, tx pgx.Tx, approver domain.Actor) error
}

// approverRoleSQL reads only the three columns the decision needs, so the
// rollback MCP's database role can be limited to a column-level
// SELECT (github_id, role, disabled) ON users.
const approverRoleSQL = `SELECT role, disabled FROM users WHERE github_id = $1`

type githubApproverVerifier struct{}

// NewGitHubApproverVerifier is the AUTH_MODE=github verifier: the approver
// must be a github:<id> human actor whose users row is currently enabled and
// whose current role may approve a rollback under domain.RolePolicy.
func NewGitHubApproverVerifier() ApproverVerifier {
	return githubApproverVerifier{}
}

func (githubApproverVerifier) verifyApprover(ctx context.Context, tx pgx.Tx, approver domain.Actor) error {
	if !approver.Valid() || approver.Kind() != domain.ActorHuman {
		return &domain.ApproverNotAuthorizedError{}
	}
	githubID, ok := domain.GitHubIDOf(approver.ID())
	if !ok {
		return &domain.ApproverNotAuthorizedError{}
	}
	var (
		roleText string
		disabled bool
	)
	err := tx.QueryRow(ctx, approverRoleSQL, githubID).Scan(&roleText, &disabled)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return &domain.ApproverNotAuthorizedError{}
	case err != nil:
		return fmt.Errorf("read approver role: %w", err)
	case disabled:
		return &domain.ApproverNotAuthorizedError{}
	}
	role, err := domain.ParseRole(roleText)
	if err != nil {
		return fmt.Errorf("read approver role: %w", err)
	}
	if err := (domain.RolePolicy{}).Authorize(role, domain.ActionApproveRollback); err != nil {
		return &domain.ApproverNotAuthorizedError{}
	}
	return nil
}

type localApproverVerifier struct {
	actor domain.Actor
}

// NewLocalApproverVerifier is the AUTH_MODE=local verifier. Local mode has no
// users rows; the orchestrator injects one fixed human actor, so only an
// approval recorded by exactly that actor is executable.
func NewLocalApproverVerifier(actor domain.Actor) (ApproverVerifier, error) {
	if !actor.Valid() || actor.Kind() != domain.ActorHuman {
		return nil, &domain.ValidationError{Field: "local_actor", Reason: "must be a human actor"}
	}
	if _, isGitHub := domain.GitHubIDOf(actor.ID()); isGitHub {
		// A github:<id> local actor would make that user's approvals
		// executable regardless of their role in users.
		return nil, &domain.ValidationError{Field: "local_actor", Reason: "must not be a github:<id> actor"}
	}
	return localApproverVerifier{actor: actor}, nil
}

func (v localApproverVerifier) verifyApprover(_ context.Context, _ pgx.Tx, approver domain.Actor) error {
	if approver != v.actor {
		return &domain.ApproverNotAuthorizedError{}
	}
	return nil
}
