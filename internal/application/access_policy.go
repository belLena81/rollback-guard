package application

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

// AccessPolicy decides whether actor may perform a human use case. It returns
// nil to allow, *domain.ForbiddenError when the actor's role (or kind) is
// insufficient, and *domain.UnauthenticatedError when the actor no longer
// resolves to an enabled user. Any other error is an infrastructure failure.
// Every human-initiated use case calls it first, before any state change;
// internal system transitions never do.
type AccessPolicy interface {
	Authorize(ctx context.Context, actor domain.Actor, action domain.Action) error
}

// githubActorPrefix is the ID prefix of an Actor derived from a GitHub user
// (domain.ActorFor).
const githubActorPrefix = "github:"

// RoleAccessPolicy authorizes against the live users table: on every call it
// resolves the actor's user and evaluates domain.RolePolicy on its current
// role, so a role change or disable applies to the next call.
type RoleAccessPolicy struct {
	uow    ports.AuthUnitOfWork
	policy domain.RolePolicy
}

func NewRoleAccessPolicy(uow ports.AuthUnitOfWork) (*RoleAccessPolicy, error) {
	if uow == nil {
		return nil, errors.New("role access policy requires an auth unit of work")
	}
	return &RoleAccessPolicy{uow: uow}, nil
}

func (p *RoleAccessPolicy) Authorize(ctx context.Context, actor domain.Actor, action domain.Action) error {
	if !actor.Valid() || actor.Kind() != domain.ActorHuman {
		// A system or agent actor never performs a human use case.
		return &domain.ForbiddenError{Action: action}
	}
	githubID, ok := githubIDOf(actor.ID())
	if !ok {
		return &domain.UnauthenticatedError{}
	}
	var user domain.User
	err := p.uow.WithinAuthTransaction(ctx, func(repos ports.AuthRepositories) error {
		var err error
		user, err = repos.Users().GetByGitHubID(githubID)
		return err
	})
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return &domain.UnauthenticatedError{}
	case err != nil:
		return fmt.Errorf("resolve actor role: %w", err)
	case user.Disabled():
		return &domain.UnauthenticatedError{}
	}
	return p.policy.Authorize(user.Role(), action) //nolint:wrapcheck // the typed domain denial is the answer
}

// githubIDOf parses the canonical "github:<positive decimal>" actor ID.
func githubIDOf(id domain.ActorID) (int64, bool) {
	raw, ok := strings.CutPrefix(string(id), githubActorPrefix)
	if !ok {
		return 0, false
	}
	githubID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || githubID <= 0 || strconv.FormatInt(githubID, 10) != raw {
		return 0, false
	}
	return githubID, true
}

// LocalAccessPolicy is the AUTH_MODE=local policy: the one configured local
// human actor may do everything; every other actor is denied.
type LocalAccessPolicy struct {
	actor domain.Actor
}

func NewLocalAccessPolicy(actor domain.Actor) (*LocalAccessPolicy, error) {
	if !actor.Valid() || actor.Kind() != domain.ActorHuman {
		return nil, &domain.ValidationError{Field: "local_actor", Reason: "must be a human actor"}
	}
	return &LocalAccessPolicy{actor: actor}, nil
}

func (p *LocalAccessPolicy) Authorize(_ context.Context, actor domain.Actor, action domain.Action) error {
	if !action.Valid() || actor != p.actor {
		return &domain.ForbiddenError{Action: action}
	}
	return nil
}
