// Package applicationtest drives the synchronous create -> investigate path
// for tests. Production creates an incident asynchronously and lets the
// InvestigationCoordinator delegate the investigation over A2A; tests that
// only need an incident that is awaiting approval, or an investigation that
// fails at a chosen point, use Flow instead of running a coordinator.
//
// It is built on the exported application services and ports only, so it
// cannot bypass them.
package applicationtest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

// investigationFailureReason is the audit reason of an investigation that
// failed before it produced a result.
const investigationFailureReason = "investigation failed"

// Flow runs the synchronous path against the same services, store, clock, and
// ID source a test built them with.
type Flow struct {
	Services       application.Services
	UnitOfWork     ports.UnitOfWork
	Clock          ports.Clock
	IDs            ports.IDGenerator
	Runner         *application.InvestigationRunner
	SystemActor    domain.Actor
	CleanupTimeout time.Duration
}

// CreateIncident stores a new incident in the created state and audits it.
func (f Flow) CreateIncident(ctx context.Context, command application.CreateIncidentCommand) (domain.Incident, error) {
	now := f.Clock.Now()
	incident, err := domain.NewIncident(domain.IncidentID(f.IDs.NewID("incident")), command.Target, command.Symptom, now)
	if err != nil {
		return domain.Incident{}, err
	}
	err = f.UnitOfWork.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		if err := repositories.Incidents().Create(incident); err != nil {
			return err
		}
		return f.appendEvent(repositories, incident.ID(), domain.EventIncidentCreated, command.Actor, domain.IncidentCreatedDetail{Target: command.Target}, now)
	})
	if err != nil {
		return domain.Incident{}, fmt.Errorf("create incident transaction: %w", err)
	}
	return incident, nil
}

// Investigate starts the incident's investigation if it has not started, runs
// the investigation in process, and persists its result. A failed
// investigation fails the incident.
func (f Flow) Investigate(ctx context.Context, incidentID domain.IncidentID) (domain.Incident, error) {
	incident, err := f.beginOrResume(ctx, incidentID)
	if err != nil {
		return domain.Incident{}, err
	}
	result, err := f.Runner.Investigate(ctx, ports.InvestigationRequest{Incident: incident})
	if err != nil {
		return domain.Incident{}, f.failAfter(ctx, incidentID, err)
	}
	return f.Services.Investigations.Complete(ctx, incidentID, result)
}

// Proposal returns the incident's rollback proposal, or a not-found error.
func (f Flow) Proposal(ctx context.Context, incidentID domain.IncidentID) (domain.RollbackProposal, error) {
	summary, err := f.Services.Queries.GetIncidentSummary(ctx, incidentID)
	if err != nil {
		return domain.RollbackProposal{}, err
	}
	if summary.Proposal == nil {
		return domain.RollbackProposal{}, &domain.NotFoundError{Resource: "proposal", ID: string(incidentID)}
	}
	return *summary.Proposal, nil
}

func (f Flow) beginOrResume(ctx context.Context, incidentID domain.IncidentID) (domain.Incident, error) {
	now := f.Clock.Now()
	var result domain.Incident
	err := f.UnitOfWork.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		incident, err := repositories.Incidents().Get(incidentID)
		if err != nil {
			return err
		}
		if incident.Status() == domain.StatusInvestigating {
			result = incident
			return nil
		}
		expected := incident.Version()
		if err := incident.StartInvestigation(now); err != nil {
			return err
		}
		if err := repositories.Incidents().Update(incident, expected); err != nil {
			return err
		}
		result = incident
		return f.appendEvent(
			repositories,
			incidentID,
			domain.EventStatusChanged,
			f.SystemActor,
			domain.StatusChangedDetail{From: domain.StatusCreated, To: domain.StatusInvestigating},
			now,
		)
	})
	if err != nil {
		return domain.Incident{}, fmt.Errorf("begin investigation: %w", err)
	}
	return result, nil
}

// failAfter fails the incident on a context that outlives the caller's, and
// returns cause (joined with the cleanup error, if any).
func (f Flow) failAfter(ctx context.Context, incidentID domain.IncidentID, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), f.CleanupTimeout)
	defer cancel()
	now := f.Clock.Now()
	err := f.UnitOfWork.WithinTransaction(cleanupCtx, func(repositories ports.Repositories) error {
		incident, err := repositories.Incidents().Get(incidentID)
		if err != nil {
			return err
		}
		expected := incident.Version()
		if err := incident.Fail(now); err != nil {
			return err
		}
		if err := repositories.Incidents().Update(incident, expected); err != nil {
			return err
		}
		return f.appendEvent(
			repositories,
			incidentID,
			domain.EventIncidentFailed,
			f.SystemActor,
			domain.TerminalDetail{Reason: investigationFailureReason},
			now,
		)
	})
	if err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func (f Flow) appendEvent(
	repositories ports.Repositories,
	incidentID domain.IncidentID,
	kind domain.EventKind,
	actor domain.Actor,
	detail domain.EventDetail,
	now time.Time,
) error {
	sequence, err := repositories.Events().NextSequence(incidentID)
	if err != nil {
		return fmt.Errorf("get next event sequence: %w", err)
	}
	event, err := domain.NewOperationEvent(domain.EventID(f.IDs.NewID("event")), incidentID, sequence, kind, now, actor, detail)
	if err != nil {
		return err
	}
	return repositories.Events().Append(event)
}
