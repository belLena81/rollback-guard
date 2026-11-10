package memory_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/memory"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

func TestTransactionRollsBackAllWrites(t *testing.T) {
	uow := memory.NewUnitOfWork()
	incident := repositoryIncident(t)
	wantErr := errors.New("stop transaction")
	err := uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
		if err := repositories.Incidents().Create(incident); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("WithinTransaction() error = %v", err)
	}
	err = uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
		_, getErr := repositories.Incidents().Get(incident.ID())
		return getErr
	})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("rolled back incident lookup error = %v, want not found", err)
	}
}

func TestCanceledContextCannotCommitTransaction(t *testing.T) {
	uow := memory.NewUnitOfWork()
	incident := repositoryIncident(t)
	ctx, cancel := context.WithCancel(context.Background())
	err := uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		if err := repositories.Incidents().Create(incident); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WithinTransaction() error = %v, want canceled", err)
	}
	err = uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
		_, getErr := repositories.Incidents().Get(incident.ID())
		return getErr
	})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("canceled transaction committed incident: %v", err)
	}
}

func TestNextEventSequenceHonorsCancellation(t *testing.T) {
	uow := memory.NewUnitOfWork()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
		_, sequenceErr := repositories.Events().NextSequence(ctx, "incident")
		return sequenceErr
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("NextSequence() error = %v, want context canceled", err)
	}
}

func TestOptimisticUpdateRejectsStaleVersion(t *testing.T) {
	uow := memory.NewUnitOfWork()
	incident := repositoryIncident(t)
	if err := uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
		return repositories.Incidents().Create(incident)
	}); err != nil {
		t.Fatal(err)
	}
	updated := incident
	if err := updated.StartInvestigation(time.Now()); err != nil {
		t.Fatal(err)
	}
	err := uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
		return repositories.Incidents().Update(updated, 99)
	})
	if !errors.Is(err, domain.ErrVersionConflict) {
		t.Fatalf("Update() error = %v, want version conflict", err)
	}
}

func TestConcurrentTransactionsAreRaceSafe(t *testing.T) {
	uow := memory.NewUnitOfWork()
	incident := repositoryIncident(t)
	if err := uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
		return repositories.Incidents().Create(incident)
	}); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for index := 0; index < 32; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
				_, err := repositories.Incidents().Get(incident.ID())
				return err
			}); err != nil {
				t.Errorf("read transaction: %v", err)
			}
		}()
	}
	group.Wait()
}

func TestClaimNextRespectsActiveCancellationLease(t *testing.T) {
	uow := memory.NewUnitOfWork()
	incident := repositoryIncident(t)
	now := time.Now().UTC()
	dispatch := domain.InvestigationDispatch{
		IncidentID:     incident.ID(),
		State:          domain.InvestigationTaskCancelRequested,
		Stage:          domain.InvestigationStageQueued,
		LeaseOwner:     "worker-1",
		LeaseExpiresAt: now.Add(time.Minute),
		Version:        1,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
		if err := repositories.Incidents().Create(incident); err != nil {
			return err
		}
		return repositories.InvestigationTasks().Create(dispatch)
	}); err != nil {
		t.Fatal(err)
	}
	err := uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
		_, claimErr := repositories.InvestigationTasks().ClaimNext(now, "worker-2", now.Add(time.Minute))
		return claimErr
	})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("active cancellation lease claim error = %v, want not found", err)
	}
	err = uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
		claimed, claimErr := repositories.InvestigationTasks().ClaimNext(now.Add(2*time.Minute), "worker-2", now.Add(3*time.Minute))
		if claimErr != nil {
			return claimErr
		}
		if claimed.State != domain.InvestigationTaskCancelRequested || claimed.LeaseOwner != "worker-2" {
			t.Fatalf("claimed dispatch = %#v", claimed)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentStaleUpdatesYieldOneVersionConflict(t *testing.T) {
	uow := memory.NewUnitOfWork()
	incident := repositoryIncident(t)
	if err := uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
		return repositories.Incidents().Create(incident)
	}); err != nil {
		t.Fatal(err)
	}
	first := incident
	second := incident
	if err := first.StartInvestigation(time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := second.Cancel(time.Now()); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for _, value := range []domain.Incident{first, second} {
		value := value
		go func() {
			results <- uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
				return repositories.Incidents().Update(value, incident.Version())
			})
		}()
	}
	conflicts := 0
	for index := 0; index < 2; index++ {
		if err := <-results; errors.Is(err, domain.ErrVersionConflict) {
			conflicts++
		}
	}
	if conflicts != 1 {
		t.Fatalf("version conflicts = %d, want 1", conflicts)
	}
}

func TestIdempotencyKeyCannotBeReusedForDifferentInput(t *testing.T) {
	uow := memory.NewUnitOfWork()
	now := time.Now()
	first, err := domain.NewPendingIdempotencyRecord("key", domain.IdempotencyOperationExecuteRollback, "hash-1", "incident-1", now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := domain.NewPendingIdempotencyRecord("key", domain.IdempotencyOperationExecuteRollback, "hash-2", "incident-2", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
		return repositories.Idempotency().Create(first)
	}); err != nil {
		t.Fatal(err)
	}
	err = uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
		return repositories.Idempotency().Create(second)
	})
	if !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("Save(reused key) error = %v, want idempotency conflict", err)
	}
}

func TestIdempotencyKeyCannotBeReusedForDifferentResource(t *testing.T) {
	uow := memory.NewUnitOfWork()
	now := time.Now()
	first, err := domain.NewPendingIdempotencyRecord("key", domain.IdempotencyOperationExecuteRollback, "hash", "incident-1", now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := domain.NewPendingIdempotencyRecord("key", domain.IdempotencyOperationExecuteRollback, "hash", "incident-2", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
		return repositories.Idempotency().Create(first)
	}); err != nil {
		t.Fatal(err)
	}
	err = uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
		return repositories.Idempotency().Create(second)
	})
	if !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("Save(reused key) error = %v, want idempotency conflict", err)
	}
}

func repositoryIncident(t *testing.T) domain.Incident {
	t.Helper()
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	target, err := domain.NewDeploymentTarget("cluster", "namespace", "api", "service")
	if err != nil {
		t.Fatal(err)
	}
	symptom, err := domain.NewSymptom("http_error_rate", 0.2, 0.1, "ratio", now)
	if err != nil {
		t.Fatal(err)
	}
	incident, err := domain.NewIncident("incident", target, symptom, now)
	if err != nil {
		t.Fatal(err)
	}
	return incident
}
