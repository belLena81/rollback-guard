package application

import (
	"context"
	"errors"
	"fmt"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

// QueryService reads incidents and their records. Every exported read is a
// human use case and authorizes its actor for domain.ActionRead first.
type QueryService struct {
	uow    ports.UnitOfWork
	access AccessPolicy
}

func (s *QueryService) authorizeRead(ctx context.Context, actor domain.Actor) error {
	if err := s.access.Authorize(ctx, actor, domain.ActionRead); err != nil {
		return fmt.Errorf("authorize read: %w", err)
	}
	return nil
}

type IncidentSummary struct {
	Incident      domain.Incident
	Investigation *domain.InvestigationDispatch
	Conclusion    *domain.InvestigationConclusion
	Proposal      *domain.RollbackProposal
	Approval      *domain.Approval
	Execution     *domain.RollbackExecution
	Verification  *domain.RecoveryVerification
}

func (s *QueryService) GetIncident(ctx context.Context, actor domain.Actor, incidentID domain.IncidentID) (domain.Incident, error) {
	if err := s.authorizeRead(ctx, actor); err != nil {
		return domain.Incident{}, err
	}
	var incident domain.Incident
	err := s.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		var err error
		incident, err = repositories.Incidents().Get(incidentID)
		return err
	})
	if err != nil {
		return domain.Incident{}, fmt.Errorf("get incident: %w", err)
	}
	return incident, nil
}

func (s *QueryService) GetIncidentSummary(ctx context.Context, actor domain.Actor, incidentID domain.IncidentID) (IncidentSummary, error) {
	if err := s.authorizeRead(ctx, actor); err != nil {
		return IncidentSummary{}, err
	}
	return s.summary(ctx, incidentID)
}

// summary reads the incident summary without an access check, for a use case
// that has already authorized its actor (ApproveAndExecuteService).
func (s *QueryService) summary(ctx context.Context, incidentID domain.IncidentID) (IncidentSummary, error) {
	var summary IncidentSummary
	err := s.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		incident, err := repositories.Incidents().Get(incidentID)
		if err != nil {
			return err
		}
		summary.Incident = incident
		if investigation, err := repositories.InvestigationTasks().GetByIncident(incidentID); err == nil {
			summary.Investigation = &investigation
		} else if !errors.Is(err, domain.ErrNotFound) {
			return err
		}
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

func (s *QueryService) ListEvents(ctx context.Context, actor domain.Actor, incidentID domain.IncidentID) ([]domain.OperationEvent, error) {
	if err := s.authorizeRead(ctx, actor); err != nil {
		return nil, err
	}
	var events []domain.OperationEvent
	err := s.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		var err error
		events, err = repositories.Events().List(incidentID)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("list incident events: %w", err)
	}
	return events, nil
}
