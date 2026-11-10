package application

import (
	"context"

	"github.com/belLena81/rollback-guard/internal/domain"
)

// allowAll is the in-package twin of applicationtest.AllowAll (which this
// package's internal tests cannot import without a cycle).
type allowAll struct{}

func (allowAll) Authorize(_ context.Context, actor domain.Actor, action domain.Action) error {
	if !actor.Valid() || !action.Valid() {
		return &domain.ForbiddenError{Action: action}
	}
	return nil
}

func testReader() domain.Actor {
	actor, err := domain.NewActor("test-reader", domain.ActorHuman)
	if err != nil {
		panic(err)
	}
	return actor
}
