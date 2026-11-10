package applicationtest

import (
	"context"

	"github.com/belLena81/rollback-guard/internal/domain"
)

// AllowAll is a permissive application.AccessPolicy for tests that exercise
// the workflow rather than authorization. It allows any valid actor to
// perform any valid action. Production wires RoleAccessPolicy or
// LocalAccessPolicy.
type AllowAll struct{}

func (AllowAll) Authorize(_ context.Context, actor domain.Actor, action domain.Action) error {
	if !actor.Valid() || !action.Valid() {
		return &domain.ForbiddenError{Action: action}
	}
	return nil
}

// Reader returns the human actor tests use for reads.
func Reader() domain.Actor {
	actor, err := domain.NewActor("test-reader", domain.ActorHuman)
	if err != nil {
		panic(err)
	}
	return actor
}
