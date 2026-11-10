package application

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

// IncidentService creates and cancels incidents.
type IncidentService struct {
	audit
	uow    ports.UnitOfWork
	clock  ports.Clock
	tracer ports.Tracer
}

type CreateIncidentCommand struct {
	Target  domain.DeploymentTarget
	Symptom domain.Symptom
	Actor   domain.Actor
}

func (s *IncidentService) CreateIncidentAsync(ctx context.Context, command CreateIncidentCommand) (domain.Incident, error) {
	now := s.clock.Now()
	incident, err := domain.NewIncident(domain.IncidentID(s.ids.NewID("incident")), command.Target, command.Symptom, now)
	if err != nil {
		return domain.Incident{}, err
	}
	if err := incident.StartInvestigation(now); err != nil {
		return domain.Incident{}, err
	}
	operationID, messageID := s.ids.NewID("operation"), s.ids.NewID("message")
	// The dispatch persists the creating request's trace context so the
	// asynchronous coordinator continues the same trace.
	traceID := s.tracer.TraceCarrier(ctx)
	if traceID == "" {
		traceID = s.ids.NewID("trace")
	}
	dispatch := domain.InvestigationDispatch{
		IncidentID: incident.ID(), OperationID: operationID, MessageID: messageID, TraceID: traceID,
		IdempotencyKey: "investigation:" + string(incident.ID()), RequestHash: InvestigationRequestHash(incident),
		State: domain.InvestigationTaskQueued, Stage: domain.InvestigationStageQueued, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	err = s.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		if err := repositories.Incidents().Create(incident); err != nil {
			return err
		}
		if err := repositories.InvestigationTasks().Create(dispatch); err != nil {
			return err
		}
		return s.appendEvents(repositories, incident.ID(), now,
			incidentCreated(command),
			statusChangeEvent(s.systemActor, domain.StatusCreated, domain.StatusInvestigating))
	})
	if err != nil {
		return domain.Incident{}, fmt.Errorf("create asynchronous incident transaction: %w", err)
	}
	return incident, nil
}

type CancelIncidentCommand struct {
	IncidentID domain.IncidentID
	Actor      domain.Actor
	Reason     string
}

func (s *IncidentService) Cancel(ctx context.Context, command CancelIncidentCommand) (domain.Incident, error) {
	if strings.TrimSpace(command.Reason) == "" {
		return domain.Incident{}, &domain.ValidationError{Field: "cancel.reason", Reason: "must not be empty"}
	}
	now := s.clock.Now()
	var result domain.Incident
	err := s.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		incident, err := repositories.Incidents().Get(command.IncidentID)
		if err != nil {
			return err
		}
		incident, err = s.transition(repositories, incident, now, (*domain.Incident).Cancel, terminated(command.Actor, domain.EventIncidentCanceled, command.Reason))
		if err != nil {
			return err
		}
		dispatch, dispatchErr := repositories.InvestigationTasks().GetByIncident(command.IncidentID)
		if dispatchErr == nil && !dispatch.State.Terminal() {
			expectedDispatchVersion := dispatch.Version
			dispatch.CancelPending(now)
			if err := repositories.InvestigationTasks().Update(dispatch, expectedDispatchVersion); err != nil {
				return err
			}
		} else if dispatchErr != nil && !errors.Is(dispatchErr, domain.ErrNotFound) {
			return dispatchErr
		}
		result = incident
		return nil
	})
	if err != nil {
		return domain.Incident{}, fmt.Errorf("cancel incident: %w", err)
	}
	return result, nil
}
