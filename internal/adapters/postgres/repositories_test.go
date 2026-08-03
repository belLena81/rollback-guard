package postgres

import (
	"os"
	"strings"
	"testing"
)

func TestIdempotencyUpdateDoesNotUseTimestampEquality(t *testing.T) {
	source, err := os.ReadFile("repositories.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	start := strings.Index(text, "func (repo idempotencyRepo) Update")
	if start < 0 {
		t.Fatal("idempotency update function not found")
	}
	end := strings.Index(text[start:], "type eventRepo struct")
	if end < 0 {
		t.Fatal("event repository marker not found after idempotency update")
	}
	updateSource := text[start : start+end]
	if strings.Contains(updateSource, "started_at =") {
		t.Fatalf("idempotency update must not compare started_at for optimistic concurrency:\n%s", updateSource)
	}
}
