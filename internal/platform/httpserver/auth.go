package httpserver

import (
	"context"
	"errors"
	"net/http"

	"github.com/belLena81/rollback-guard/internal/domain"
)

type AuthMiddleware interface {
	Wrap(http.Handler) http.Handler
}

type actorContextKey struct{}

type LocalAuthMiddleware struct {
	actor domain.Actor
}

func NewLocalAuthMiddleware(actor domain.Actor) (LocalAuthMiddleware, error) {
	if !actor.IsHuman() {
		return LocalAuthMiddleware{}, errors.New("local auth actor must be human")
	}
	return LocalAuthMiddleware{actor: actor}, nil
}

func (m LocalAuthMiddleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), actorContextKey{}, m.actor)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func ActorFromContext(ctx context.Context) (domain.Actor, bool) {
	actor, ok := ctx.Value(actorContextKey{}).(domain.Actor)
	if !ok || !actor.Valid() {
		return domain.Actor{}, false
	}
	return actor, true
}
