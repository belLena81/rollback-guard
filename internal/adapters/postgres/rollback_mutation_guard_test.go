package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/belLena81/rollback-guard/internal/domain"
)

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

	guard.retainRollbackMutationLock(key, replacement)

	if got := guard.locks[key]; got != replacement {
		t.Fatalf("retained lock = %p, want replacement %p", got, replacement)
	}
}
