package a2aadapter

import (
	"context"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestIdempotencyTimingsFollowTheTaskTimeout(t *testing.T) {
	tests := []struct {
		name        string
		taskTimeout time.Duration
		wantPoll    time.Duration
		wantStale   time.Duration
	}{
		{name: "default A2A_TASK_TIMEOUT keeps the 100ms poll", taskTimeout: 45 * time.Second, wantPoll: 100 * time.Millisecond, wantStale: 50 * time.Second},
		{name: "maximum A2A_TASK_TIMEOUT", taskTimeout: 2 * time.Minute, wantPoll: 100 * time.Millisecond, wantStale: 2*time.Minute + 5*time.Second},
		{name: "poll cap boundary", taskTimeout: 10 * time.Second, wantPoll: 100 * time.Millisecond, wantStale: 15 * time.Second},
		{name: "short timeout polls proportionally", taskTimeout: 5 * time.Second, wantPoll: 50 * time.Millisecond, wantStale: 10 * time.Second},
		{name: "poll floor boundary", taskTimeout: time.Second, wantPoll: 10 * time.Millisecond, wantStale: 6 * time.Second},
		{name: "very short timeout keeps the poll floor", taskTimeout: 200 * time.Millisecond, wantPoll: 10 * time.Millisecond, wantStale: 5200 * time.Millisecond},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			poll, stale := idempotencyTimings(test.taskTimeout)
			if poll != test.wantPoll || stale != test.wantStale {
				t.Fatalf("idempotencyTimings(%s) = poll %s, stale %s; want poll %s, stale %s", test.taskTimeout, poll, stale, test.wantPoll, test.wantStale)
			}
			// A live owner is never reclaimed: it finishes within the task
			// timeout plus the terminal write.
			if stale < test.taskTimeout+terminalPersistenceTimeout {
				t.Fatalf("staleAfter %s is shorter than a live owner's lifetime", stale)
			}
		})
	}
}

func TestNewIdempotentInvestigatorValidatesDependencies(t *testing.T) {
	// The pool connects lazily, so no database is needed.
	pool, err := pgxpool.New(context.Background(), "postgres://rollback@127.0.0.1:1/rollback_guard")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	next := investigatorFunc(func(context.Context, ports.InvestigationRequest) (ports.InvestigationResult, error) {
		return ports.InvestigationResult{}, nil
	})
	tests := []struct {
		name        string
		pool        *pgxpool.Pool
		next        ports.Investigator
		taskTimeout time.Duration
		wantErr     bool
	}{
		{name: "valid", pool: pool, next: next, taskTimeout: 45 * time.Second},
		{name: "missing pool", next: next, taskTimeout: 45 * time.Second, wantErr: true},
		{name: "missing investigator", pool: pool, taskTimeout: 45 * time.Second, wantErr: true},
		{name: "zero task timeout", pool: pool, next: next, wantErr: true},
		{name: "negative task timeout", pool: pool, next: next, taskTimeout: -time.Second, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			investigator, err := NewIdempotentInvestigator(test.pool, test.next, test.taskTimeout)
			if (err != nil) != test.wantErr {
				t.Fatalf("NewIdempotentInvestigator() error = %v, wantErr %t", err, test.wantErr)
			}
			if err != nil {
				return
			}
			wantPoll, wantStale := idempotencyTimings(test.taskTimeout)
			if investigator.pollInterval != wantPoll || investigator.staleAfter != wantStale {
				t.Fatalf("intervals = poll %s, stale %s; want the derived poll %s, stale %s", investigator.pollInterval, investigator.staleAfter, wantPoll, wantStale)
			}
		})
	}
}
