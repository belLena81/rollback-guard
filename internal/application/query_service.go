package application

import (
	"context"
	"fmt"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

func (s *QueryService) GetIncident(ctx context.Context, incidentID domain.IncidentID) (domain.Incident, error) {
	c := s.workflow
	var incident domain.Incident
	err := c.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		var err error
		incident, err = repositories.Incidents().Get(incidentID)
		return err
	})
	if err != nil {
		return domain.Incident{}, fmt.Errorf("get incident: %w", err)
	}
	return incident, nil
}

func (s *QueryService) GetProposal(ctx context.Context, incidentID domain.IncidentID) (domain.RollbackProposal, error) {
	c := s.workflow
	var proposal domain.RollbackProposal
	err := c.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		var err error
		proposal, err = repositories.Proposals().GetByIncident(incidentID)
		return err
	})
	if err != nil {
		return domain.RollbackProposal{}, fmt.Errorf("get proposal: %w", err)
	}
	return proposal, nil
}

func (s *QueryService) ListEvents(ctx context.Context, incidentID domain.IncidentID) ([]domain.OperationEvent, error) {
	c := s.workflow
	var events []domain.OperationEvent
	err := c.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		var err error
		events, err = repositories.Events().List(incidentID)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("list incident events: %w", err)
	}
	return events, nil
}

func (c *workflow) appendEvent(repositories ports.Repositories, incidentID domain.IncidentID, kind domain.EventKind, actor domain.Actor, detail domain.EventDetail, now time.Time) error {
	event, err := domain.NewOperationEvent(
		domain.EventID(c.ids.NewID("event")),
		incidentID,
		repositories.Events().NextSequence(incidentID),
		kind,
		now,
		actor,
		detail,
	)
	if err != nil {
		return err
	}
	return repositories.Events().Append(event)
}
