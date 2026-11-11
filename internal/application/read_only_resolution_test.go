package application

import (
	"context"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/fake"
	"github.com/belLena81/rollback-guard/internal/adapters/memory"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/belLena81/rollback-guard/internal/testkit"
)

// writeCountingStore counts read-write transactions on top of a memory store.
type writeCountingStore struct {
	*memory.AuthUnitOfWork
	writes, reads int
}

func (s *writeCountingStore) WithinAuthTransaction(ctx context.Context, fn func(ports.AuthRepositories) error) error {
	s.writes++
	return s.AuthUnitOfWork.WithinAuthTransaction(ctx, fn)
}

func (s *writeCountingStore) WithinAuthReadTransaction(ctx context.Context, fn func(ports.AuthReadRepositories) error) error {
	s.reads++
	return s.AuthUnitOfWork.WithinAuthReadTransaction(ctx, fn)
}

func TestSessionResolveAndRoleLookupUseReadOnlyTransactions(t *testing.T) {
	store := &writeCountingStore{AuthUnitOfWork: memory.NewAuthUnitOfWork()}
	clock := testkit.NewClock(time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC))
	sessions, err := NewSessionService(SessionDependencies{
		UnitOfWork: store, Clock: clock, IDGenerator: &testkit.IDGenerator{}, Random: &fake.RandomSource{},
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	user, err := domain.NewUser("user-1", 4242, "octocat", "octo@example.com", []string{"acme"}, domain.RoleOperator, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AuthUnitOfWork.WithinAuthTransaction(context.Background(), func(r ports.AuthRepositories) error {
		return r.Users().Create(user)
	}); err != nil {
		t.Fatal(err)
	}
	token, _, err := sessions.Issue(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	store.writes, store.reads = 0, 0

	principal, err := sessions.Resolve(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := NewRoleAccessPolicy(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := policy.Authorize(context.Background(), principal.Actor(), domain.ActionRead); err != nil {
		t.Fatal(err)
	}
	if store.writes != 0 || store.reads != 2 {
		t.Fatalf("read-write transactions = %d, read-only = %d; want 0 and 2", store.writes, store.reads)
	}
}
