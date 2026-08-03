package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

type IncidentSummary struct {
	Incident     domain.Incident
	Conclusion   *domain.InvestigationConclusion
	Proposal     *domain.RollbackProposal
	Approval     *domain.Approval
	Execution    *domain.RollbackExecution
	Verification *domain.RecoveryVerification
}

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

func (s *QueryService) GetIncidentSummary(ctx context.Context, incidentID domain.IncidentID) (IncidentSummary, error) {
	c := s.workflow
	var summary IncidentSummary
	err := c.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		incident, err := repositories.Incidents().Get(incidentID)
		if err != nil {
			return err
		}
		summary.Incident = incident
		if conclusion, err := repositories.Conclusions().Get(incidentID); err == nil {
			summary.Conclusion = &conclusion
		} else if !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		if proposal, err := repositories.Proposals().GetByIncident(incidentID); err == nil {
			summary.Proposal = &proposal
		} else if !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		if approval, err := repositories.Approvals().GetByIncident(incidentID); err == nil {
			summary.Approval = &approval
		} else if !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		if execution, err := repositories.Executions().GetByIncident(incidentID); err == nil {
			summary.Execution = &execution
		} else if !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		if verification, err := repositories.Verifications().GetByIncident(incidentID); err == nil {
			summary.Verification = &verification
		} else if !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		return nil
	})
	if err != nil {
		return IncidentSummary{}, fmt.Errorf("get incident summary: %w", err)
	}
	return summary, nil
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

func (c *workflow) appendEvent(ctx context.Context, repositories ports.Repositories, incidentID domain.IncidentID, kind domain.EventKind, actor domain.Actor, detail domain.EventDetail, now time.Time) error {
	sequence, err := repositories.Events().NextSequence(ctx, incidentID)
	if err != nil {
		return fmt.Errorf("get next event sequence: %w", err)
	}
	event, err := domain.NewOperationEvent(
		domain.EventID(c.ids.NewID("event")),
		incidentID,
		sequence,
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
