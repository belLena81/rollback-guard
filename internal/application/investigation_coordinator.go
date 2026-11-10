package application

import (
	"context"
	"errors"
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
	// ReportError receives iteration failures with a context carrying the
	// dispatch correlation and trace, so callers can log them correlated.
	ReportError func(context.Context, error)
	// Tracer resumes the trace persisted with each dispatch. Nil disables
	// tracing.
	Tracer ports.Tracer
}

type InvestigationCoordinator struct {
	uow            ports.UnitOfWork
	gateway        ports.InvestigationGateway
	investigations *InvestigationService
	clock          ports.Clock
	config         InvestigationCoordinatorConfig
}

type investigationWatchResult struct {
	result ports.InvestigationResult
	err    error
}

func NewInvestigationCoordinator(uow ports.UnitOfWork, gateway ports.InvestigationGateway, investigations *InvestigationService, clock ports.Clock, config InvestigationCoordinatorConfig) (*InvestigationCoordinator, error) {
	if uow == nil || gateway == nil || investigations == nil || clock == nil {
		return nil, errors.New("all investigation coordinator dependencies are required")
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
	if config.Tracer == nil {
		config.Tracer = ports.NoopTracer{}
	}
	return &InvestigationCoordinator{uow: uow, gateway: gateway, investigations: investigations, clock: clock, config: config}, nil
}

func (c *InvestigationCoordinator) Run(ctx context.Context) error {
	ticker := time.NewTicker(c.config.PollInterval)
	defer ticker.Stop()
	for {
		if reportCtx, err := c.processNext(ctx); err != nil && !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, context.Canceled) {
			if c.config.ReportError != nil {
				c.config.ReportError(reportCtx, err)
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
	_, err := c.processNext(ctx)
	return err
}

// processNext claims and drives one dispatch. It returns the context it ran
// under (carrying the dispatch correlation and resumed trace) so failures can
// be reported against it.
func (c *InvestigationCoordinator) processNext(ctx context.Context) (context.Context, error) {
	dispatch, incident, err := c.claim(ctx)
	if err != nil {
		return ctx, err
	}
	ctx = ports.WithCorrelation(ctx, ports.Correlation{IncidentID: dispatch.IncidentID, OperationID: dispatch.OperationID, TaskID: dispatch.Ref.TaskID, TraceID: dispatch.TraceID, IdempotencyKey: dispatch.IdempotencyKey})
	ctx, end := c.config.Tracer.StartSpan(ctx, "investigation.dispatch", dispatch.TraceID)
	err = c.drive(ctx, dispatch, incident)
	end(err)
	return ctx, err
}

func (c *InvestigationCoordinator) claim(ctx context.Context) (domain.InvestigationDispatch, domain.Incident, error) {
	now := c.clock.Now()
	var dispatch domain.InvestigationDispatch
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
		return domain.InvestigationDispatch{}, domain.Incident{}, err
	}
	return dispatch, incident, nil
}

func (c *InvestigationCoordinator) drive(ctx context.Context, dispatch domain.InvestigationDispatch, incident domain.Incident) error {
	if dispatch.State == domain.InvestigationTaskCancelRequested || incident.Status() == domain.StatusCanceled {
		return c.cancelInvestigation(ctx, dispatch)
	}

	taskCtx, cancel := context.WithTimeout(ctx, c.config.TaskTimeout)
	defer cancel()
	if dispatch.Ref.TaskID == "" {
		submitted, done, err := c.submit(ctx, taskCtx, dispatch, incident)
		if done {
			return err
		}
		dispatch = submitted
		ctx = ports.WithCorrelation(ctx, ports.Correlation{TaskID: dispatch.Ref.TaskID})
	}
	ref := dispatch.Ref
	result, canceled, err := c.watchInvestigation(ctx, taskCtx, dispatch, ref)
	if canceled {
		return err
	}
	if err != nil {
		return c.handleWatchFailure(ctx, taskCtx, dispatch, ref, err)
	}
	return c.finish(ctx, dispatch, result)
}

// submit hands the investigation to the Investigator and records the remote
// task on the dispatch. done reports that the drive is over, with err as its
// outcome: the submission failed (and was retried or failed for good) or the
// dispatch was canceled meanwhile. Otherwise it returns the updated dispatch.
func (c *InvestigationCoordinator) submit(ctx, taskCtx context.Context, dispatch domain.InvestigationDispatch, incident domain.Incident) (domain.InvestigationDispatch, bool, error) {
	// Hand the Investigator the trace context of this dispatch span so a
	// detached task execution still joins the incident's trace.
	traceID := c.config.Tracer.TraceCarrier(ctx)
	if traceID == "" {
		traceID = dispatch.TraceID
	}
	ref, err := c.gateway.Submit(taskCtx, ports.InvestigationSubmission{Incident: incident, OperationID: dispatch.OperationID, IdempotencyKey: dispatch.IdempotencyKey, MessageID: dispatch.MessageID, TraceID: traceID, RequestHash: dispatch.RequestHash})
	if err != nil {
		if dispatch.Attempts < c.config.MaxAttempts {
			return dispatch, true, c.retryOrCancel(ctx, dispatch, ref, false, err)
		}
		return dispatch, true, c.fail(ctx, dispatch.IncidentID, "submission_failed", err)
	}
	ctx = ports.WithCorrelation(ctx, ports.Correlation{TaskID: ref.TaskID})
	attached, err := c.attachTaskRef(ctx, dispatch.IncidentID, ref)
	if err != nil {
		return dispatch, true, err
	}
	if attached.CancellationRequested() {
		return attached, true, c.cancelInvestigation(ctx, attached)
	}
	return attached, false, nil
}

// retryOrCancel schedules another attempt and returns cause. If cancellation
// was requested meanwhile it cancels the investigation instead.
func (c *InvestigationCoordinator) retryOrCancel(ctx context.Context, dispatch domain.InvestigationDispatch, ref domain.InvestigationTaskRef, submitted bool, cause error) error {
	retryErr := c.retry(ctx, dispatch.IncidentID, submitted)
	if errors.Is(retryErr, domain.ErrInvestigationCancellationRequested) {
		return c.cancelLatestInvestigation(ctx, dispatch, ref)
	}
	if retryErr != nil {
		return errors.Join(cause, retryErr)
	}
	return cause
}

// handleWatchFailure decides what a failed watch means: shutdown, a
// cancellation requested meanwhile, another attempt, or a failed
// investigation (after canceling the remote task).
func (c *InvestigationCoordinator) handleWatchFailure(ctx, taskCtx context.Context, dispatch domain.InvestigationDispatch, ref domain.InvestigationTaskRef, watchErr error) error {
	if errors.Is(ctx.Err(), context.Canceled) && errors.Is(taskCtx.Err(), context.Canceled) {
		return ctx.Err()
	}
	if latest, loadErr := c.loadDispatch(ctx, dispatch.IncidentID); loadErr == nil && latest.CancellationRequested() {
		return c.cancelInvestigation(ctx, latest)
	}
	timedOut := errors.Is(taskCtx.Err(), context.DeadlineExceeded)
	if !timedOut && dispatch.Attempts < c.config.MaxAttempts {
		return c.retryOrCancel(ctx, dispatch, ref, true, watchErr)
	}
	cancelCtx, cancelRemote := context.WithTimeout(context.WithoutCancel(ctx), c.config.CancelTimeout)
	defer cancelRemote()
	cancelErr := c.gateway.Cancel(cancelCtx, ref)
	code := "investigation_failed"
	if timedOut {
		code = "investigation_timeout"
	}
	return c.fail(ctx, dispatch.IncidentID, code, errors.Join(watchErr, cancelErr))
}

// finish persists the investigation result. A result that cannot be persisted
// fails the investigation unless cancellation was requested meanwhile.
func (c *InvestigationCoordinator) finish(ctx context.Context, dispatch domain.InvestigationDispatch, result ports.InvestigationResult) error {
	_, err := c.investigations.Complete(ctx, dispatch.IncidentID, result)
	if err == nil {
		return nil
	}
	latest, loadErr := c.loadDispatch(ctx, dispatch.IncidentID)
	if loadErr != nil {
		return errors.Join(err, loadErr)
	}
	if latest.CancellationRequested() {
		return c.cancelInvestigation(ctx, latest)
	}
	return c.fail(ctx, dispatch.IncidentID, "invalid_investigation_result", err)
}

func (c *InvestigationCoordinator) watchInvestigation(ctx context.Context, taskCtx context.Context, dispatch domain.InvestigationDispatch, ref domain.InvestigationTaskRef) (ports.InvestigationResult, bool, error) {
	watchCtx, cancelWatch := context.WithCancel(taskCtx)
	defer cancelWatch()
	completed := make(chan investigationWatchResult, 1)
	go func() {
		result, err := c.gateway.Watch(watchCtx, ref, func(stage domain.InvestigationStage) error {
			if err := taskCtx.Err(); err != nil {
				return err
			}
			return c.recordProgress(watchCtx, dispatch.IncidentID, stage)
		})
		completed <- investigationWatchResult{result: result, err: err}
	}()
	ticker := time.NewTicker(c.config.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case watchResult := <-completed:
			if errors.Is(watchResult.err, domain.ErrInvestigationCancellationRequested) {
				return ports.InvestigationResult{}, true, c.cancelLatestInvestigation(ctx, dispatch, ref)
			}
			if watchResult.err != nil {
				return ports.InvestigationResult{}, false, watchResult.err
			}
			latest, err := c.loadDispatch(ctx, dispatch.IncidentID)
			if err != nil {
				return ports.InvestigationResult{}, false, err
			}
			if latest.CancellationRequested() {
				return ports.InvestigationResult{}, true, c.cancelInvestigation(ctx, latest)
			}
			return watchResult.result, false, nil
		case <-ticker.C:
			latest, err := c.loadDispatch(ctx, dispatch.IncidentID)
			if err != nil {
				cancelWatch()
				return ports.InvestigationResult{}, false, err
			}
			if latest.CancellationRequested() {
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

func (c *InvestigationCoordinator) attachTaskRef(ctx context.Context, incidentID domain.IncidentID, ref domain.InvestigationTaskRef) (domain.InvestigationDispatch, error) {
	for {
		var updated domain.InvestigationDispatch
		err := c.update(ctx, incidentID, func(task *domain.InvestigationDispatch, now time.Time) error {
			task.AttachRef(ref, now)
			updated = *task
			return nil
		})
		if err == nil || !errors.Is(err, domain.ErrVersionConflict) {
			return updated, err
		}
		if err := ctx.Err(); err != nil {
			return domain.InvestigationDispatch{}, err
		}
	}
}

func (c *InvestigationCoordinator) recordProgress(ctx context.Context, incidentID domain.IncidentID, stage domain.InvestigationStage) error {
	return c.updateActive(ctx, incidentID, func(task *domain.InvestigationDispatch, now time.Time) error {
		return task.RecordProgress(stage, now)
	})
}

func (c *InvestigationCoordinator) loadDispatch(ctx context.Context, incidentID domain.IncidentID) (domain.InvestigationDispatch, error) {
	var dispatch domain.InvestigationDispatch
	err := c.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		var err error
		dispatch, err = repositories.InvestigationTasks().GetByIncident(incidentID)
		return err
	})
	return dispatch, err
}

func (c *InvestigationCoordinator) cancelLatestInvestigation(ctx context.Context, fallback domain.InvestigationDispatch, ref domain.InvestigationTaskRef) error {
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

func (c *InvestigationCoordinator) cancelInvestigation(ctx context.Context, dispatch domain.InvestigationDispatch) error {
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
	return c.update(ctx, incidentID, func(task *domain.InvestigationDispatch, now time.Time) error {
		task.Cancel(now)
		return nil
	})
}

func (c *InvestigationCoordinator) retry(ctx context.Context, incidentID domain.IncidentID, submitted bool) error {
	return c.updateActive(ctx, incidentID, func(task *domain.InvestigationDispatch, now time.Time) error {
		if submitted {
			return task.RetryWatch(c.config.WorkerID, now)
		}
		return task.RetrySubmission(now)
	})
}

// updateActive applies a transition that the dispatch refuses once
// cancellation was requested. A version conflict is resolved by looking at
// the latest dispatch: if cancellation won the race, that is reported instead.
func (c *InvestigationCoordinator) updateActive(ctx context.Context, incidentID domain.IncidentID, transition func(*domain.InvestigationDispatch, time.Time) error) error {
	err := c.update(ctx, incidentID, transition)
	if !errors.Is(err, domain.ErrVersionConflict) {
		return err
	}
	latest, loadErr := c.loadDispatch(ctx, incidentID)
	if loadErr == nil && latest.CancellationRequested() {
		return domain.ErrInvestigationCancellationRequested
	}
	return errors.Join(err, loadErr)
}

// update loads the dispatch, applies transition, and persists it with the
// version it loaded.
func (c *InvestigationCoordinator) update(ctx context.Context, incidentID domain.IncidentID, transition func(*domain.InvestigationDispatch, time.Time) error) error {
	now := c.clock.Now()
	return c.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		task, err := repositories.InvestigationTasks().GetByIncident(incidentID)
		if err != nil {
			return err
		}
		expected := task.Version
		if err := transition(&task, now); err != nil {
			return err
		}
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
