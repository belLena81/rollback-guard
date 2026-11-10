package domain

import "testing"

func TestDeterministicExecutionIDIsStableAndBoundToKeyAndHash(t *testing.T) {
	first := DeterministicExecutionID("approval:approval-1", "hash-1")
	if again := DeterministicExecutionID("approval:approval-1", "hash-1"); again != first {
		t.Fatalf("same inputs gave %q and %q", first, again)
	}
	const prefix = "execution-"
	if len(first) != len(prefix)+32 || string(first[:len(prefix)]) != prefix {
		t.Fatalf("id = %q, want %q plus 32 hex characters", first, prefix)
	}
	others := map[string]ExecutionID{
		"different key":  DeterministicExecutionID("approval:approval-2", "hash-1"),
		"different hash": DeterministicExecutionID("approval:approval-1", "hash-2"),
		// A key/hash pair must not be re-splittable into another pair.
		"shifted boundary": DeterministicExecutionID("approval:approval-1\nhash", "-1"),
	}
	for name, other := range others {
		if other == first {
			t.Fatalf("%s produced the same id %q", name, first)
		}
	}
}
