package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
)

func TestActorKindIsValidatedEnum(t *testing.T) {
	actor, err := domain.NewActor("alice", domain.ActorHuman)
	if err != nil {
		t.Fatalf("NewActor() error = %v", err)
	}
	if actor.Kind().String() != "human" {
		t.Fatalf("Kind().String() = %q", actor.Kind().String())
	}
	_, err = domain.NewActor("alice", domain.ActorKind(99))
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("NewActor(unknown) error = %v, want validation", err)
	}
}

func TestOnlyHumanMayCreateApproval(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	for _, kind := range []domain.ActorKind{domain.ActorAgent, domain.ActorSystem, domain.ActorService} {
		t.Run(kind.String(), func(t *testing.T) {
			actor, err := domain.NewActor("actor", kind)
			if err != nil {
				t.Fatal(err)
			}
			_, err = domain.NewApproval("approval", actor, domain.ApprovalApproved, "action", "hash", now, now.Add(time.Minute), "")
			if !errors.Is(err, domain.ErrNonHumanApproval) {
				t.Fatalf("NewApproval() error = %v, want non-human approval", err)
			}
		})
	}
}

func TestIncidentHappyPathAndVersion(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	incident := newIncident(t, now)
	transitions := []struct {
		want  domain.Status
		apply func(*domain.Incident) error
	}{
		{want: domain.StatusInvestigating, apply: func(value *domain.Incident) error { return value.StartInvestigation(now.Add(time.Minute)) }},
		{want: domain.StatusAwaitingApproval, apply: func(value *domain.Incident) error { return value.AwaitApproval(now.Add(2 * time.Minute)) }},
		{want: domain.StatusApproved, apply: func(value *domain.Incident) error { return value.Approve(now.Add(3 * time.Minute)) }},
		{want: domain.StatusExecuting, apply: func(value *domain.Incident) error { return value.StartExecution(now.Add(4 * time.Minute)) }},
		{want: domain.StatusVerifying, apply: func(value *domain.Incident) error { return value.StartVerification(now.Add(5 * time.Minute)) }},
		{want: domain.StatusResolved, apply: func(value *domain.Incident) error { return value.Resolve(now.Add(6 * time.Minute)) }},
	}
	wantVersion := uint64(1)
	for _, transition := range transitions {
		if err := transition.apply(&incident); err != nil {
			t.Fatalf("transition to %s: %v", transition.want, err)
		}
		wantVersion++
		if incident.Status() != transition.want || incident.Version() != wantVersion {
			t.Fatalf("after transition status/version = %s/%d, want %s/%d", incident.Status(), incident.Version(), transition.want, wantVersion)
		}
	}
	if err := incident.Cancel(now.Add(7 * time.Minute)); !errors.Is(err, domain.ErrIllegalTransition) {
		t.Fatalf("terminal Cancel() error = %v, want illegal transition", err)
	}
}

func TestIncidentRejectsCancelAfterMutationStarts(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	incident := newIncident(t, now)
	applyTransitions(t, &incident,
		func(value *domain.Incident) error { return value.StartInvestigation(now) },
		func(value *domain.Incident) error { return value.AwaitApproval(now) },
		func(value *domain.Incident) error { return value.Approve(now) },
		func(value *domain.Incident) error { return value.StartExecution(now) },
	)
	if err := incident.Cancel(now); !errors.Is(err, domain.ErrIllegalTransition) {
		t.Fatalf("Cancel() error = %v, want illegal transition", err)
	}
}

func newIncident(t *testing.T, now time.Time) domain.Incident {
	t.Helper()
	target, err := domain.NewDeploymentTarget("cluster", "namespace", "api", "api-service")
	if err != nil {
		t.Fatal(err)
	}
	symptom, err := domain.NewSymptom("http_error_rate", 0.25, 0.10, "ratio", now)
	if err != nil {
		t.Fatal(err)
	}
	incident, err := domain.NewIncident("incident", target, symptom, now)
	if err != nil {
		t.Fatal(err)
	}
	return incident
}

func applyTransitions(t *testing.T, incident *domain.Incident, transitions ...func(*domain.Incident) error) {
	t.Helper()
	for _, transition := range transitions {
		if err := transition(incident); err != nil {
			t.Fatal(err)
		}
	}
}
