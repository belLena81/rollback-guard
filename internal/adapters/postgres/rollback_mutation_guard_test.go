package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

func TestTakeRollbackMutationLockRequiresTheOwningLease(t *testing.T) {
	key := domain.IdempotencyKey("rollback-key")
	held := &rollbackMutationLock{key: key, lease: "lease-owner"}
	tests := []struct {
		name  string
		key   domain.IdempotencyKey
		lease ports.RollbackMutationLease
		want  *rollbackMutationLock
	}{
		{name: "owning lease", key: key, lease: "lease-owner", want: held},
		{name: "another attempt's lease", key: key, lease: "lease-other"},
		{name: "empty lease of a completed attempt", key: key, lease: ""},
		{name: "owning lease under another key", key: "other-key", lease: "lease-owner"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			guard := &RollbackMutationGuard{locks: map[domain.IdempotencyKey]*rollbackMutationLock{key: held}}

			got := guard.takeRollbackMutationLock(test.key, test.lease)

			if got != test.want {
				t.Fatalf("taken lock = %p, want %p", got, test.want)
			}
			_, stillHeld := guard.locks[key]
			if stillHeld == (test.want != nil) {
				t.Fatalf("lock still retained = %v after taking %p", stillHeld, got)
			}
		})
	}
}

func TestReleaseWithoutTheOwningLeaseLeavesTheLockHeld(t *testing.T) {
	key := domain.IdempotencyKey("rollback-key")
	released := 0
	held := &rollbackMutationLock{key: key, lease: "lease-replay", releaseHook: func(context.Context) error {
		released++
		return nil
	}}
	guard := &RollbackMutationGuard{locks: map[domain.IdempotencyKey]*rollbackMutationLock{key: held}}
	mutation, err := domain.NewPendingRollbackMutation(key, "sha256-hash", "execution-1", "revision-bad", "revision-good", time.Date(2026, 8, 6, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	late := ports.RollbackMutationAttempt{Mutation: mutation, Lease: "lease-first"}

	if err := guard.Release(context.Background(), late); err != nil {
		t.Fatal(err)
	}
	if _, err := guard.Complete(context.Background(), late, false, time.Date(2026, 8, 6, 9, 1, 0, 0, time.UTC)); !errors.Is(err, ports.ErrRollbackMutationLeaseNotHeld) {
		t.Fatalf("Complete() error = %v, want lease not held", err)
	}
	if released != 0 || guard.locks[key] != held {
		t.Fatalf("replay lock released %d times, retained = %v; a stale lease must not touch it", released, guard.locks[key] == held)
	}

	owner := ports.RollbackMutationAttempt{Mutation: mutation, Lease: "lease-replay"}
	if err := guard.Release(context.Background(), owner); err != nil {
		t.Fatal(err)
	}
	if released != 1 || guard.locks[key] != nil {
		t.Fatalf("owner release: released %d, retained %v", released, guard.locks[key] != nil)
	}
}

func TestRollbackMutationLeasesAreDistinct(t *testing.T) {
	seen := make(map[ports.RollbackMutationLease]bool)
	for range 64 {
		lease := newRollbackMutationLease()
		if len(lease) != 2*rollbackMutationLeaseBytes || seen[lease] {
			t.Fatalf("lease %q is short or repeated", lease)
		}
		seen[lease] = true
	}
}

func TestRetainRollbackMutationLockIgnoresReplacedLockReleaseFailure(t *testing.T) {
	key := domain.IdempotencyKey("rollback-key")
	releaseErr := errors.New("stale lock release failed")
	previous := &rollbackMutationLock{
		key: key,
		releaseHook: func(context.Context) error {
			return releaseErr
		},
	}
	replacement := &rollbackMutationLock{key: key}
	guard := &RollbackMutationGuard{locks: map[domain.IdempotencyKey]*rollbackMutationLock{key: previous}}

	guard.retainRollbackMutationLock(t.Context(), key, replacement)

	if got := guard.locks[key]; got != replacement {
		t.Fatalf("retained lock = %p, want replacement %p", got, replacement)
	}
}
