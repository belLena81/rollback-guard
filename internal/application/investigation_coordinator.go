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

var errInvestigationCancellationRequested = errors.New("investigation cancellation requested")

type investigationWatchResult struct {
	result ports.InvestigationResult
	err    error
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
		return c.cancelInvestigation(ctx, dispatch)
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
					if errors.Is(retryErr, errInvestigationCancellationRequested) {
						return c.cancelLatestInvestigation(ctx, dispatch, ref)
					}
					return errors.Join(err, retryErr)
				}
				return err
			}
			return c.fail(ctx, dispatch.IncidentID, "submission_failed", err)
		}
		dispatch, err = c.attachTaskRef(ctx, dispatch.IncidentID, ref)
		if err != nil {
			return err
		}
		if dispatch.State == ports.InvestigationTaskCancelRequested || dispatch.State == ports.InvestigationTaskCanceled {
			return c.cancelInvestigation(ctx, dispatch)
		}
	}
	result, canceled, err := c.watchInvestigation(ctx, taskCtx, dispatch, ref)
	if canceled {
		return err
	}
	if err != nil {
		latest, loadErr := c.loadDispatch(ctx, dispatch.IncidentID)
		if loadErr == nil && (latest.State == ports.InvestigationTaskCancelRequested || latest.State == ports.InvestigationTaskCanceled) {
			return c.cancelInvestigation(ctx, latest)
		}
		if !errors.Is(taskCtx.Err(), context.DeadlineExceeded) && dispatch.Attempts < c.config.MaxAttempts {
			if retryErr := c.retry(ctx, dispatch.IncidentID, true); retryErr != nil {
				if errors.Is(retryErr, errInvestigationCancellationRequested) {
					return c.cancelLatestInvestigation(ctx, dispatch, ref)
				}
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
		latest, loadErr := c.loadDispatch(ctx, dispatch.IncidentID)
		if loadErr == nil && (latest.State == ports.InvestigationTaskCancelRequested || latest.State == ports.InvestigationTaskCanceled) {
			return c.cancelInvestigation(ctx, latest)
		}
		if loadErr != nil {
			return errors.Join(err, loadErr)
		}
		return c.fail(ctx, dispatch.IncidentID, "invalid_investigation_result", err)
	}
	return nil
}

func (c *InvestigationCoordinator) watchInvestigation(ctx context.Context, taskCtx context.Context, dispatch ports.InvestigationDispatch, ref ports.InvestigationTaskRef) (ports.InvestigationResult, bool, error) {
	watchCtx, cancelWatch := context.WithCancel(taskCtx)
	defer cancelWatch()
	completed := make(chan investigationWatchResult, 1)
	go func() {
		result, err := c.gateway.Watch(watchCtx, ref, func(stage ports.InvestigationStage) error {
			return c.recordProgress(watchCtx, dispatch.IncidentID, stage)
		})
		completed <- investigationWatchResult{result: result, err: err}
	}()
	ticker := time.NewTicker(c.config.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case watchResult := <-completed:
			if errors.Is(watchResult.err, errInvestigationCancellationRequested) {
				return ports.InvestigationResult{}, true, c.cancelLatestInvestigation(ctx, dispatch, ref)
			}
			if watchResult.err != nil {
				return ports.InvestigationResult{}, false, watchResult.err
			}
			latest, err := c.loadDispatch(ctx, dispatch.IncidentID)
			if err != nil {
				return ports.InvestigationResult{}, false, err
			}
			if latest.State == ports.InvestigationTaskCancelRequested || latest.State == ports.InvestigationTaskCanceled {
				return ports.InvestigationResult{}, true, c.cancelInvestigation(ctx, latest)
			}
			return watchResult.result, false, nil
		case <-ticker.C:
			latest, err := c.loadDispatch(ctx, dispatch.IncidentID)
			if err != nil {
				cancelWatch()
				return ports.InvestigationResult{}, false, err
			}
			if latest.State == ports.InvestigationTaskCancelRequested || latest.State == ports.InvestigationTaskCanceled {
				cancelWatch()
				err := c.cancelInvestigation(ctx, latest)
				return ports.InvestigationResult{}, true, err
			}
		case <-taskCtx.Done():
			cancelWatch()
			return ports.InvestigationResult{}, false, taskCtx.Err()
		}
	}
}

func (c *InvestigationCoordinator) attachTaskRef(ctx context.Context, incidentID domain.IncidentID, ref ports.InvestigationTaskRef) (ports.InvestigationDispatch, error) {
	for {
		var updated ports.InvestigationDispatch
		now := c.clock.Now()
		err := c.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
			task, err := repositories.InvestigationTasks().GetByIncident(incidentID)
			if err != nil {
				return err
			}
			expected := task.Version
			task.Ref = ref
			if task.State != ports.InvestigationTaskCancelRequested && task.State != ports.InvestigationTaskCanceled {
				task.State = ports.InvestigationTaskSubmitted
			}
			task.Version++
			task.UpdatedAt = now
			if err := repositories.InvestigationTasks().Update(task, expected); err != nil {
				return err
			}
			updated = task
			return nil
		})
		if err == nil || !errors.Is(err, domain.ErrVersionConflict) {
			return updated, err
		}
		if err := ctx.Err(); err != nil {
			return ports.InvestigationDispatch{}, err
		}
	}
}

func (c *InvestigationCoordinator) recordProgress(ctx context.Context, incidentID domain.IncidentID, stage ports.InvestigationStage) error {
	return c.updateActive(ctx, incidentID, func(task *ports.InvestigationDispatch) {
		task.State = ports.InvestigationTaskWorking
		task.Stage = stage
	}, ports.InvestigationTaskSubmitted, ports.InvestigationTaskWorking)
}

func (c *InvestigationCoordinator) loadDispatch(ctx context.Context, incidentID domain.IncidentID) (ports.InvestigationDispatch, error) {
	var dispatch ports.InvestigationDispatch
	err := c.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		var err error
		dispatch, err = repositories.InvestigationTasks().GetByIncident(incidentID)
		return err
	})
	return dispatch, err
}

func (c *InvestigationCoordinator) cancelLatestInvestigation(ctx context.Context, fallback ports.InvestigationDispatch, ref ports.InvestigationTaskRef) error {
	latest, err := c.loadDispatch(ctx, fallback.IncidentID)
	if err != nil {
		fallback.Ref = ref
		return errors.Join(err, c.cancelInvestigation(ctx, fallback))
	}
	if latest.Ref.TaskID == "" {
		latest.Ref = ref
	}
	return c.cancelInvestigation(ctx, latest)
}

func (c *InvestigationCoordinator) cancelInvestigation(ctx context.Context, dispatch ports.InvestigationDispatch) error {
	var cancelErr error
	if dispatch.Ref.TaskID != "" {
		cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.config.CancelTimeout)
		cancelErr = c.gateway.Cancel(cancelCtx, dispatch.Ref)
		cancel()
		if cancelErr != nil && dispatch.Attempts < c.config.MaxAttempts {
			return cancelErr
		}
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.config.CancelTimeout)
	defer cancel()
	return errors.Join(cancelErr, c.markCanceled(persistCtx, dispatch.IncidentID))
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
	allowed := []ports.InvestigationTaskState{ports.InvestigationTaskSubmitting}
	if submitted {
		allowed = []ports.InvestigationTaskState{ports.InvestigationTaskSubmitted, ports.InvestigationTaskWorking}
	}
	return c.updateActive(ctx, incidentID, func(task *ports.InvestigationDispatch) {
		if submitted {
			task.State = ports.InvestigationTaskSubmitted
			task.LeaseOwner = c.config.WorkerID
			task.LeaseExpiresAt = c.clock.Now()
		} else {
			task.State = ports.InvestigationTaskQueued
			task.LeaseOwner = ""
			task.LeaseExpiresAt = time.Time{}
		}
	}, allowed...)
}

func (c *InvestigationCoordinator) updateActive(ctx context.Context, incidentID domain.IncidentID, mutate func(*ports.InvestigationDispatch), allowed ...ports.InvestigationTaskState) error {
	now := c.clock.Now()
	err := c.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		task, err := repositories.InvestigationTasks().GetByIncident(incidentID)
		if err != nil {
			return err
		}
		if task.State == ports.InvestigationTaskCancelRequested || task.State == ports.InvestigationTaskCanceled {
			return errInvestigationCancellationRequested
		}
		stateAllowed := false
		for _, state := range allowed {
			if task.State == state {
				stateAllowed = true
				break
			}
		}
		if !stateAllowed {
			return fmt.Errorf("cannot update investigation task in %s state", task.State)
		}
		expected := task.Version
		mutate(&task)
		task.Version++
		task.UpdatedAt = now
		return repositories.InvestigationTasks().Update(task, expected)
	})
	if !errors.Is(err, domain.ErrVersionConflict) {
		return err
	}
	latest, loadErr := c.loadDispatch(ctx, incidentID)
	if loadErr == nil && (latest.State == ports.InvestigationTaskCancelRequested || latest.State == ports.InvestigationTaskCanceled) {
		return errInvestigationCancellationRequested
	}
	return errors.Join(err, loadErr)
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
