package domain

import (
	"errors"
	"testing"
	"time"
)

func TestRollbackMutationCompletionIsImmutable(t *testing.T) {
	startedAt := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	record, err := NewPendingRollbackMutation(
		"key-1",
		"hash-1",
		"mutation-1",
		"revision-bad",
		"revision-stable",
		startedAt,
	)
	if err != nil {
		t.Fatal(err)
	}

	completed, err := record.Complete(true, startedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if completed.State() != RollbackMutationCompleted || !completed.AlreadyApplied() {
		t.Fatalf("completed mutation = %#v", completed.Snapshot())
	}
	if _, err := completed.Complete(false, startedAt.Add(2*time.Second)); !errors.Is(err, ErrValidation) {
		t.Fatalf("second completion error = %v", err)
	}
}

func TestRollbackMutationRejectsInvalidBindings(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		key  IdempotencyKey
		hash ProposalHash
		from Revision
		to   Revision
	}{
		{name: "empty key", hash: "hash", from: "a", to: "b"},
		{name: "empty hash", key: "key", from: "a", to: "b"},
		{name: "same revision", key: "key", hash: "hash", from: "a", to: "a"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewPendingRollbackMutation(test.key, test.hash, "mutation", test.from, test.to, now); !errors.Is(err, ErrValidation) {
				t.Fatalf("NewPendingRollbackMutation() error = %v", err)
			}
		})
	}
}
