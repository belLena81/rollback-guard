package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

type InvestigationCoordinatorConfig struct {
	WorkerID      string
	TaskTimeout   time.Duration
	CancelTimeout time.Duration
	PollInterval  time.Duration
	LeaseTTL      time.Duration
	MaxAttempts   int
	ReportError   func(error)
}

type InvestigationCoordinator struct {
	uow            ports.UnitOfWork
	gateway        ports.InvestigationGateway
	investigations *InvestigationService
	clock          ports.Clock
	config         InvestigationCoordinatorConfig
}

func NewInvestigationCoordinator(uow ports.UnitOfWork, gateway ports.InvestigationGateway, investigations *InvestigationService, clock ports.Clock, config InvestigationCoordinatorConfig) (*InvestigationCoordinator, error) {
	if uow == nil || gateway == nil || investigations == nil || clock == nil {
		return nil, fmt.Errorf("all investigation coordinator dependencies are required")
	}
	if config.WorkerID == "" || config.TaskTimeout <= 0 || config.CancelTimeout <= 0 {
		return nil, &domain.ValidationError{Field: "investigation_coordinator", Reason: "worker and timeouts are required"}
	}
	if config.PollInterval <= 0 {
		config.PollInterval = 250 * time.Millisecond
	}
	if config.LeaseTTL <= 0 {
		config.LeaseTTL = config.TaskTimeout + config.CancelTimeout
	}
	if config.MaxAttempts <= 0 {
		config.MaxAttempts = 3
	}
	return &InvestigationCoordinator{uow: uow, gateway: gateway, investigations: investigations, clock: clock, config: config}, nil
}

func (c *InvestigationCoordinator) Run(ctx context.Context) error {
	ticker := time.NewTicker(c.config.PollInterval)
	defer ticker.Stop()
	for {
		if err := c.ProcessNext(ctx); err != nil && !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, context.Canceled) {
			if c.config.ReportError != nil {
				c.config.ReportError(err)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *InvestigationCoordinator) ProcessNext(ctx context.Context) error {
	now := c.clock.Now()
	var dispatch ports.InvestigationDispatch
	var incident domain.Incident
	err := c.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		var err error
		dispatch, err = repositories.InvestigationTasks().ClaimNext(now, c.config.WorkerID, now.Add(c.config.LeaseTTL))
		if err != nil {
			return err
		}
		incident, err = repositories.Incidents().Get(dispatch.IncidentID)
		return err
	})
	if err != nil {
		return err
	}
	if dispatch.State == ports.InvestigationTaskCancelRequested || incident.Status() == domain.StatusCanceled {
		if dispatch.Ref.TaskID != "" {
			cancelCtx, cancelRemote := context.WithTimeout(context.WithoutCancel(ctx), c.config.CancelTimeout)
			defer cancelRemote()
			if err := c.gateway.Cancel(cancelCtx, dispatch.Ref); err != nil {
				if dispatch.Attempts >= c.config.MaxAttempts {
					if updateErr := c.markCanceled(ctx, dispatch.IncidentID); updateErr != nil {
						return errors.Join(err, updateErr)
					}
				}
				return err
			}
		}
		return c.markCanceled(ctx, dispatch.IncidentID)
	}

	taskCtx, cancel := context.WithTimeout(ctx, c.config.TaskTimeout)
	defer cancel()
	ref := dispatch.Ref
	if ref.TaskID == "" {
		var err error
		ref, err = c.gateway.Submit(taskCtx, ports.InvestigationSubmission{Incident: incident, OperationID: dispatch.OperationID, IdempotencyKey: dispatch.IdempotencyKey, MessageID: dispatch.MessageID, TraceID: dispatch.TraceID, RequestHash: dispatch.RequestHash})
		if err != nil {
			if dispatch.Attempts < c.config.MaxAttempts {
				if retryErr := c.retry(ctx, dispatch.IncidentID, false); retryErr != nil {
					return errors.Join(err, retryErr)
				}
				return err
			}
			return c.fail(ctx, dispatch.IncidentID, "submission_failed", err)
		}
		if err := c.update(ctx, dispatch.IncidentID, func(task *ports.InvestigationDispatch) { task.Ref = ref; task.State = ports.InvestigationTaskSubmitted }); err != nil {
			return err
		}
	}
	result, err := c.gateway.Watch(taskCtx, ref, func(stage ports.InvestigationStage) error {
		return c.update(ctx, dispatch.IncidentID, func(task *ports.InvestigationDispatch) {
			task.State = ports.InvestigationTaskWorking
			task.Stage = stage
		})
	})
	if err != nil {
		if !errors.Is(taskCtx.Err(), context.DeadlineExceeded) && dispatch.Attempts < c.config.MaxAttempts {
			if retryErr := c.retry(ctx, dispatch.IncidentID, true); retryErr != nil {
				return errors.Join(err, retryErr)
			}
			return err
		}
		cancelCtx, cancelRemote := context.WithTimeout(context.WithoutCancel(ctx), c.config.CancelTimeout)
		defer cancelRemote()
		cancelErr := c.gateway.Cancel(cancelCtx, ref)
		code := "investigation_failed"
		if errors.Is(taskCtx.Err(), context.DeadlineExceeded) {
			code = "investigation_timeout"
		}
		return c.fail(ctx, dispatch.IncidentID, code, errors.Join(err, cancelErr))
	}
	if _, err := c.investigations.Complete(ctx, dispatch.IncidentID, result); err != nil {
		return c.fail(ctx, dispatch.IncidentID, "invalid_investigation_result", err)
	}
	return nil
}

func (c *InvestigationCoordinator) markCanceled(ctx context.Context, incidentID domain.IncidentID) error {
	return c.update(ctx, incidentID, func(task *ports.InvestigationDispatch) {
		task.State = ports.InvestigationTaskCanceled
		task.CompletedAt = c.clock.Now()
		task.LeaseOwner = ""
		task.LeaseExpiresAt = time.Time{}
	})
}

func (c *InvestigationCoordinator) retry(ctx context.Context, incidentID domain.IncidentID, submitted bool) error {
	return c.update(ctx, incidentID, func(task *ports.InvestigationDispatch) {
		if submitted {
			task.State = ports.InvestigationTaskSubmitted
			task.LeaseOwner = c.config.WorkerID
			task.LeaseExpiresAt = c.clock.Now()
		} else {
			task.State = ports.InvestigationTaskQueued
			task.LeaseOwner = ""
			task.LeaseExpiresAt = time.Time{}
		}
	})
}

func (c *InvestigationCoordinator) update(ctx context.Context, incidentID domain.IncidentID, mutate func(*ports.InvestigationDispatch)) error {
	now := c.clock.Now()
	return c.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		task, err := repositories.InvestigationTasks().GetByIncident(incidentID)
		if err != nil {
			return err
		}
		expected := task.Version
		mutate(&task)
		task.Version++
		task.UpdatedAt = now
		return repositories.InvestigationTasks().Update(task, expected)
	})
}

func (c *InvestigationCoordinator) fail(ctx context.Context, incidentID domain.IncidentID, code string, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.config.CancelTimeout)
	defer cancel()
	if err := c.investigations.Fail(cleanupCtx, incidentID, code); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}
