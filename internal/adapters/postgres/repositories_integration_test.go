//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestEventAppendReportsSequenceConflictToSecondWriter(t *testing.T) {
	pool := isolatedRollbackGuardPool(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	seedTrustedRollback(t, pool, "incident-events", "key-events", now)
	uow, err := NewUnitOfWork(repositoryPool(t, pool, 4))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Both writers computed sequence 1 before either committed. The second
	// insert blocks on the unique index until the first writer commits.
	secondErr := make(chan error, 1)
	var sequenceAfterConflict uint64
	err = uow.WithinTransaction(ctx, func(first ports.Repositories) error {
		if err := first.Events().Append(testStatusEvent(t, "event-first", "incident-events", 1, now)); err != nil {
			return err
		}
		secondStarted := make(chan struct{})
		go func() {
			secondErr <- uow.WithinTransaction(ctx, func(second ports.Repositories) error {
				close(secondStarted)
				appendErr := second.Events().Append(testStatusEvent(t, "event-second", "incident-events", 1, now))
				// The transaction must stay usable after the conflict is reported.
				var sequenceErr error
				sequenceAfterConflict, sequenceErr = second.Events().NextSequence(ctx, "incident-events")
				if sequenceErr != nil {
					return fmt.Errorf("next sequence after conflict: %w (append error: %w)", sequenceErr, appendErr)
				}
				return appendErr
			})
		}()
		<-secondStarted
		time.Sleep(100 * time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatalf("first writer error = %v", err)
	}

	err = <-secondErr
	var conflict *domain.VersionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("second writer error = %v, want *domain.VersionConflictError", err)
	}
	if conflict.Expected != 2 || conflict.Actual != 1 {
		t.Fatalf("conflict = expected %d actual %d, want expected 2 actual 1", conflict.Expected, conflict.Actual)
	}
	if sequenceAfterConflict != 2 {
		t.Fatalf("next sequence after conflict = %d, want 2", sequenceAfterConflict)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM operation_events WHERE incident_id = 'incident-events'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("stored events = %d, want 1", count)
	}
}

func TestEventAppendStillRejectsDuplicateEventID(t *testing.T) {
	pool := isolatedRollbackGuardPool(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	seedTrustedRollback(t, pool, "incident-event-id", "key-event-id", now)
	uow, err := NewUnitOfWork(repositoryPool(t, pool, 4))
	if err != nil {
		t.Fatal(err)
	}

	err = uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
		if err := repositories.Events().Append(testStatusEvent(t, "event-duplicate", "incident-event-id", 1, now)); err != nil {
			return err
		}
		return repositories.Events().Append(testStatusEvent(t, "event-duplicate", "incident-event-id", 2, now))
	})
	if !errors.Is(err, domain.ErrValidation) || errors.Is(err, domain.ErrVersionConflict) {
		t.Fatalf("duplicate event id error = %v, want validation error that is not a version conflict", err)
	}
}

func TestIdempotencyCreateReportsConflictOnlyForDuplicateKey(t *testing.T) {
	pool := isolatedRollbackGuardPool(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	proposal := seedTrustedRollback(t, pool, "incident-idempotency", "key-existing", now)
	uow, err := NewUnitOfWork(repositoryPool(t, pool, 4))
	if err != nil {
		t.Fatal(err)
	}
	newRecord := func(key domain.IdempotencyKey) domain.IdempotencyRecord {
		record, recordErr := domain.NewPendingIdempotencyRecord(key, domain.IdempotencyOperationExecuteRollback, string(proposal.Hash()), "incident-idempotency", now)
		if recordErr != nil {
			t.Fatal(recordErr)
		}
		return record
	}

	t.Run("duplicate key is a conflict", func(t *testing.T) {
		err := uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
			return repositories.Idempotency().Create(newRecord("key-existing"))
		})
		var conflict *domain.IdempotencyConflictError
		if !errors.As(err, &conflict) || conflict.Key != "key-existing" {
			t.Fatalf("duplicate create error = %v, want idempotency conflict for key-existing", err)
		}
	})

	t.Run("canceled context is not a conflict", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		err := uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
			cancel()
			return repositories.Idempotency().Create(newRecord("key-canceled"))
		})
		if err == nil {
			t.Fatal("create with canceled context error = nil")
		}
		if errors.Is(err, domain.ErrIdempotencyConflict) {
			t.Fatalf("canceled create error = %v, must not be an idempotency conflict", err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled create error = %v, want context.Canceled", err)
		}
	})

	t.Run("other constraint violation is not a conflict", func(t *testing.T) {
		record, recordErr := domain.NewPendingIdempotencyRecord("key-unknown-incident", domain.IdempotencyOperationExecuteRollback, string(proposal.Hash()), "incident-missing", now)
		if recordErr != nil {
			t.Fatal(recordErr)
		}
		err := uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
			return repositories.Idempotency().Create(record)
		})
		if err == nil {
			t.Fatal("create for missing incident error = nil")
		}
		if errors.Is(err, domain.ErrIdempotencyConflict) {
			t.Fatalf("foreign key violation error = %v, must not be an idempotency conflict", err)
		}
	})
}

func TestWithinTransactionPanicLeavesPoolUsable(t *testing.T) {
	shared := isolatedRollbackGuardPool(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	seedTrustedRollback(t, shared, "incident-panic", "key-panic", now)
	pool := repositoryPool(t, shared, 1)
	uow, err := NewUnitOfWork(pool)
	if err != nil {
		t.Fatal(err)
	}

	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Fatal("callback panic was swallowed")
			}
		}()
		_ = uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
			if err := repositories.Events().Append(testStatusEvent(t, "event-panic", "incident-panic", 1, now)); err != nil {
				t.Fatal(err)
			}
			panic("callback failed")
		})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM operation_events WHERE incident_id = 'incident-panic'`).Scan(&count); err != nil {
		t.Fatalf("pool is unusable after a panicking transaction: %v", err)
	}
	if count != 0 {
		t.Fatalf("events written by the panicking transaction = %d, want 0 (rolled back)", count)
	}
}

// repositoryPool opens a pool on the isolated schema that uses pgx's default
// extended protocol, as production does; the shared helper pool uses the
// simple protocol so it can run multi-statement migrations.
func repositoryPool(t *testing.T, shared *pgxpool.Pool, maxConns int32) *pgxpool.Pool {
	t.Helper()
	config := shared.Config()
	config.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	config.MaxConns = maxConns
	config.MinConns = 0
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Close blocks while a connection is still checked out, so a leak
		// must fail the test instead of hanging it.
		closed := make(chan struct{})
		go func() {
			pool.Close()
			close(closed)
		}()
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Error("pool close blocked: a connection was leaked")
		}
	})
	return pool
}

func testStatusEvent(t *testing.T, id domain.EventID, incidentID domain.IncidentID, sequence uint64, now time.Time) domain.OperationEvent {
	t.Helper()
	actor, err := domain.NewActor("guardian", domain.ActorSystem)
	if err != nil {
		t.Fatal(err)
	}
	event, err := domain.NewOperationEvent(id, incidentID, sequence, domain.EventStatusChanged, now, actor, domain.StatusChangedDetail{From: domain.StatusApproved, To: domain.StatusExecuting})
	if err != nil {
		t.Fatal(err)
	}
	return event
}
