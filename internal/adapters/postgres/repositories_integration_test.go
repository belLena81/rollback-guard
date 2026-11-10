//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	sequenceAfterAppend, secondErr := appendWhileFirstWriterHoldsSequence(ctx, t, pool, uow, "incident-events", now, nil)

	var conflict *domain.VersionConflictError
	if !errors.As(secondErr, &conflict) {
		t.Fatalf("second writer error = %v, want *domain.VersionConflictError", secondErr)
	}
	if conflict.Expected != 2 || conflict.Actual != 1 {
		t.Fatalf("conflict = expected %d actual %d, want expected 2 actual 1", conflict.Expected, conflict.Actual)
	}
	if sequenceAfterAppend != 2 {
		t.Fatalf("next sequence after conflict = %d, want 2", sequenceAfterAppend)
	}
	if got := storedEventIDs(ctx, t, pool, "incident-events"); len(got) != 1 || got[0] != "event-first" {
		t.Fatalf("stored events = %v, want [event-first]", got)
	}
}

func TestEventAppendSucceedsWhenFirstWriterRollsBack(t *testing.T) {
	pool := isolatedRollbackGuardPool(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	seedTrustedRollback(t, pool, "incident-rollback", "key-rollback", now)
	uow, err := NewUnitOfWork(repositoryPool(t, pool, 4))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	firstFailure := errors.New("first writer failed")
	sequenceAfterAppend, secondErr := appendWhileFirstWriterHoldsSequence(ctx, t, pool, uow, "incident-rollback", now, firstFailure)

	if secondErr != nil {
		t.Fatalf("second writer error = %v, want nil after the first writer rolled back", secondErr)
	}
	if sequenceAfterAppend != 2 {
		t.Fatalf("next sequence after append = %d, want 2", sequenceAfterAppend)
	}
	if got := storedEventIDs(ctx, t, pool, "incident-rollback"); len(got) != 1 || got[0] != "event-second" {
		t.Fatalf("stored events = %v, want [event-second]", got)
	}
}

// appendWhileFirstWriterHoldsSequence appends sequence 1 in a first
// transaction, starts a second transaction appending the same sequence, waits
// until PostgreSQL reports that insert blocked on a lock, and then ends the
// first transaction with firstResult. It returns the next sequence the second
// transaction saw after its append, and the second transaction's error.
func appendWhileFirstWriterHoldsSequence(ctx context.Context, t *testing.T, observer *pgxpool.Pool, uow *UnitOfWork, incidentID domain.IncidentID, now time.Time, firstResult error) (uint64, error) {
	t.Helper()
	firstEvent := testStatusEvent(t, "event-first", incidentID, 1, now)
	secondEvent := testStatusEvent(t, "event-second", incidentID, 1, now)
	secondErr := make(chan error, 1)
	var sequenceAfterAppend uint64
	firstErr := uow.WithinTransaction(ctx, func(first ports.Repositories) error {
		if err := first.Events().Append(firstEvent); err != nil {
			return err
		}
		go func() {
			secondErr <- uow.WithinTransaction(ctx, func(second ports.Repositories) error {
				appendErr := second.Events().Append(secondEvent)
				// The transaction must stay usable whatever Append reported.
				var sequenceErr error
				sequenceAfterAppend, sequenceErr = second.Events().NextSequence(ctx, incidentID)
				if sequenceErr != nil {
					return fmt.Errorf("next sequence after append: %w (append error: %w)", sequenceErr, appendErr)
				}
				return appendErr
			})
		}()
		for {
			var blocked int
			if err := observer.QueryRow(ctx, `
				SELECT count(*) FROM pg_stat_activity
				WHERE datname = current_database() AND pid <> pg_backend_pid()
					AND wait_event_type = 'Lock' AND query LIKE '%INSERT INTO operation_events%'
			`).Scan(&blocked); err != nil {
				return fmt.Errorf("observe blocked insert: %w", err)
			}
			if blocked > 0 {
				return firstResult
			}
			select {
			case err := <-secondErr:
				return fmt.Errorf("second writer finished before the first: %w", err)
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
		}
	})
	if !errors.Is(firstErr, firstResult) {
		t.Fatalf("first writer error = %v, want %v", firstErr, firstResult)
	}
	select {
	case err := <-secondErr:
		return sequenceAfterAppend, err
	case <-ctx.Done():
		t.Fatalf("second writer did not finish: %v", ctx.Err())
		return 0, nil
	}
}

func storedEventIDs(ctx context.Context, t *testing.T, pool *pgxpool.Pool, incidentID domain.IncidentID) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT id FROM operation_events WHERE incident_id = $1 ORDER BY sequence`, string(incidentID))
	if err != nil {
		t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return ids
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
	if !errors.Is(err, domain.ErrAlreadyExists) || errors.Is(err, domain.ErrVersionConflict) {
		t.Fatalf("duplicate event id error = %v, want already exists that is not a version conflict", err)
	}
}

func TestDuplicateInsertsAreConflictsNotValidationErrors(t *testing.T) {
	pool := isolatedRollbackGuardPool(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	proposal := seedTrustedRollback(t, pool, "incident-duplicate", "key-duplicate", now)
	uow, err := NewUnitOfWork(repositoryPool(t, pool, 4))
	if err != nil {
		t.Fatal(err)
	}
	symptom, err := domain.NewSymptom("errors", 0.2, 0.1, "ratio", now)
	if err != nil {
		t.Fatal(err)
	}
	incident, err := domain.NewIncident("incident-duplicate", proposal.Target(), symptom, now)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := domain.NewPolicyDecision("policy-incident-duplicate", incident.ID(), []domain.RuleResult{{Name: "eligible", Passed: true}}, now)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		insert func(ports.Repositories) error
	}{
		{"incident", func(r ports.Repositories) error { return r.Incidents().Create(incident) }},
		{"policy decision", func(r ports.Repositories) error { return r.PolicyDecisions().Save(decision) }},
		{"proposal", func(r ports.Repositories) error { return r.Proposals().Save(proposal) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := uow.WithinTransaction(context.Background(), func(r ports.Repositories) error { return tt.insert(r) })
			if !errors.Is(err, domain.ErrAlreadyExists) {
				t.Fatalf("duplicate insert error = %v, want already exists", err)
			}
			if errors.Is(err, domain.ErrValidation) {
				t.Fatalf("duplicate insert error = %v, must not be a validation error", err)
			}
		})
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
		err := uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
			if err := repositories.Events().Append(testStatusEvent(t, "event-panic", "incident-panic", 1, now)); err != nil {
				t.Fatal(err)
			}
			panic("callback failed")
		})
		t.Errorf("WithinTransaction returned %v instead of propagating the panic", err)
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

func TestWithinTransactionClassifiesBeginCallbackAndCommitErrors(t *testing.T) {
	shared := isolatedRollbackGuardPool(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	seedTrustedRollback(t, shared, "incident-classify", "key-classify", now)
	pool := repositoryPool(t, shared, 2)
	uow, err := NewUnitOfWork(pool)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("callback error is returned unchanged and rolled back", func(t *testing.T) {
		callbackErr := errors.New("callback failed")
		err := uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
			if err := repositories.Events().Append(testStatusEvent(t, "event-callback", "incident-classify", 1, now)); err != nil {
				return err
			}
			return callbackErr
		})
		if err != callbackErr { //nolint:errorlint // identity: the callback error must not be wrapped or joined
			t.Fatalf("WithinTransaction() error = %v, want the callback error itself", err)
		}
		if got := storedEventIDs(context.Background(), t, shared, "incident-classify"); len(got) != 0 {
			t.Fatalf("events after a failed callback = %v, want none", got)
		}
	})

	t.Run("commit failure is wrapped and rolled back", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		err := uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
			if err := repositories.Events().Append(testStatusEvent(t, "event-commit", "incident-classify", 1, now)); err != nil {
				return err
			}
			cancel()
			return nil
		})
		if !errors.Is(err, context.Canceled) || !strings.HasPrefix(fmt.Sprint(err), "commit transaction: ") {
			t.Fatalf("WithinTransaction() error = %v, want a wrapped commit failure", err)
		}
		if got := storedEventIDs(context.Background(), t, shared, "incident-classify"); len(got) != 0 {
			t.Fatalf("events after a failed commit = %v, want none", got)
		}
	})

	t.Run("begin failure is wrapped and the callback does not run", func(t *testing.T) {
		closed := repositoryPool(t, shared, 1)
		closed.Close()
		closedUOW, err := NewUnitOfWork(closed)
		if err != nil {
			t.Fatal(err)
		}
		called := false
		err = closedUOW.WithinTransaction(context.Background(), func(ports.Repositories) error {
			called = true
			return nil
		})
		if err == nil || !strings.HasPrefix(err.Error(), "begin transaction: ") {
			t.Fatalf("WithinTransaction() error = %v, want a wrapped begin failure", err)
		}
		if called {
			t.Fatal("callback ran without a transaction")
		}
	})
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
