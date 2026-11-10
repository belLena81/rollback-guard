package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/memory"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/belLena81/rollback-guard/internal/testkit"
)

func newTransitionFixture(t *testing.T) (*workflow, *memory.UnitOfWork, domain.Incident, time.Time) {
	t.Helper()
	now := time.Date(2026, 8, 3, 13, 0, 0, 0, time.UTC)
	uow := memory.NewUnitOfWork()
	incident, err := domain.NewIncident("incident-1",
		mustTarget(t, "cluster", "shop", "checkout", "checkout"),
		mustSymptom(t, "error_rate", 0.4, 0.05, "ratio", now), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := incident.StartInvestigation(now); err != nil {
		t.Fatal(err)
	}
	if err := uow.WithinTransaction(context.Background(), func(r ports.Repositories) error { return r.Incidents().Create(incident) }); err != nil {
		t.Fatal(err)
	}
	w := &workflow{uow: uow, clock: testkit.NewClock(now), ids: &testkit.IDGenerator{}, systemActor: mustActor(t, "guardian", domain.ActorSystem)}
	return w, uow, incident, now
}

func listEvents(t *testing.T, uow *memory.UnitOfWork, id domain.IncidentID) []domain.OperationEvent {
	t.Helper()
	var events []domain.OperationEvent
	err := uow.WithinTransaction(context.Background(), func(r ports.Repositories) error {
		var err error
		events, err = r.Events().List(id)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestTransitionPersistsIncidentAndAuditEventTogether(t *testing.T) {
	t.Parallel()
	w, uow, incident, now := newTransitionFixture(t)
	later := now.Add(time.Minute)

	err := uow.WithinTransaction(context.Background(), func(r ports.Repositories) error {
		moved, err := w.transition(context.Background(), r, incident, later, (*domain.Incident).Decline, statusChanged(w.systemActor))
		if err == nil && moved.Status() != domain.StatusDeclined {
			t.Errorf("returned status = %s", moved.Status())
		}
		return err
	})
	if err != nil {
		t.Fatalf("transition error = %v", err)
	}

	stored, err := loadIncident(t, uow, incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status() != domain.StatusDeclined || stored.Version() != incident.Version()+1 {
		t.Fatalf("stored status/version = %s/%d", stored.Status(), stored.Version())
	}
	events := listEvents(t, uow, incident.ID())
	if len(events) != 1 || events[0].Kind() != domain.EventStatusChanged {
		t.Fatalf("events = %+v, want one status change", events)
	}
	detail, ok := events[0].Detail().(domain.StatusChangedDetail)
	if !ok || detail.From != domain.StatusInvestigating || detail.To != domain.StatusDeclined {
		t.Fatalf("detail = %+v, want investigating -> declined", events[0].Detail())
	}
}

func TestTransitionRejectsStaleIncidentAndWritesNoEvent(t *testing.T) {
	t.Parallel()
	w, uow, incident, now := newTransitionFixture(t)
	// Another request moves the incident on first.
	err := uow.WithinTransaction(context.Background(), func(r ports.Repositories) error {
		_, err := w.transition(context.Background(), r, incident, now, (*domain.Incident).Fail, terminated(w.systemActor, domain.EventIncidentFailed, "first"))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	err = uow.WithinTransaction(context.Background(), func(r ports.Repositories) error {
		_, err := w.transition(context.Background(), r, incident, now, (*domain.Incident).Decline, statusChanged(w.systemActor))
		return err
	})
	if !errors.Is(err, domain.ErrVersionConflict) {
		t.Fatalf("stale transition error = %v, want version conflict", err)
	}
	if events := listEvents(t, uow, incident.ID()); len(events) != 1 {
		t.Fatalf("events = %d, want only the first request's", len(events))
	}
}

func TestTransitionRefusesIllegalMoveBeforeWriting(t *testing.T) {
	t.Parallel()
	w, uow, incident, now := newTransitionFixture(t)
	err := uow.WithinTransaction(context.Background(), func(r ports.Repositories) error {
		_, err := w.transition(context.Background(), r, incident, now, (*domain.Incident).Resolve, statusChanged(w.systemActor))
		return err
	})
	if !errors.Is(err, domain.ErrIllegalTransition) {
		t.Fatalf("error = %v, want illegal transition", err)
	}
	stored, err := loadIncident(t, uow, incident.ID())
	if err != nil || stored.Version() != incident.Version() {
		t.Fatalf("stored version = %d, err = %v; want unchanged", stored.Version(), err)
	}
}
