package domain

import (
	"errors"
	"fmt"
	"time"
)

// ErrInvestigationCancellationRequested reports that a dispatch update was
// refused because cancellation of the investigation has been requested.
var ErrInvestigationCancellationRequested = errors.New("investigation cancellation requested")

type InvestigationTaskState uint8

const (
	InvestigationTaskQueued InvestigationTaskState = iota + 1
	InvestigationTaskSubmitting
	InvestigationTaskSubmitted
	InvestigationTaskWorking
	InvestigationTaskCancelRequested
	InvestigationTaskCompleted
	InvestigationTaskFailed
	InvestigationTaskCanceled
)

func (s InvestigationTaskState) String() string {
	names := [...]string{unknownName, "queued", "submitting", "submitted", "working", "cancel_requested", "completed", "failed", "canceled"}
	if int(s) >= len(names) {
		return names[0]
	}
	return names[s]
}

func ParseInvestigationTaskState(value string) (InvestigationTaskState, error) {
	return parseEnum("investigation task state", value, InvestigationTaskQueued, InvestigationTaskCanceled)
}

func (s InvestigationTaskState) Valid() bool {
	return s >= InvestigationTaskQueued && s <= InvestigationTaskCanceled
}

func (s InvestigationTaskState) Terminal() bool {
	return s == InvestigationTaskCompleted || s == InvestigationTaskFailed || s == InvestigationTaskCanceled
}

type InvestigationStage uint8

const (
	InvestigationStageQueued InvestigationStage = iota + 1
	InvestigationStageDeployment
	InvestigationStageMetrics
	InvestigationStageDiagnosis
	InvestigationStageFinalizing
)

func (s InvestigationStage) String() string {
	names := [...]string{unknownName, "queued", "deployment", "metrics", "diagnosis", "finalizing"}
	if int(s) >= len(names) {
		return names[0]
	}
	return names[s]
}

func ParseInvestigationStage(value string) (InvestigationStage, error) {
	return parseEnum("investigation stage", value, InvestigationStageQueued, InvestigationStageFinalizing)
}

func (s InvestigationStage) Valid() bool {
	return s >= InvestigationStageQueued && s <= InvestigationStageFinalizing
}

type InvestigationTaskRef struct {
	TaskID    string
	ContextID string
}

// InvestigationDispatch is the durable record of one incident's delegated
// investigation. Its transition methods own the state, lease, timestamps, and
// Version: each successful transition bumps Version once, so a caller passes
// the Version it loaded as the expected version when persisting.
type InvestigationDispatch struct {
	IncidentID     IncidentID
	OperationID    string
	IdempotencyKey string
	MessageID      string
	TraceID        string
	RequestHash    string
	Ref            InvestigationTaskRef
	State          InvestigationTaskState
	Stage          InvestigationStage
	Attempts       int
	LeaseOwner     string
	LeaseExpiresAt time.Time
	FailureCode    string
	Version        uint64
	CreatedAt      time.Time
	UpdatedAt      time.Time
	CompletedAt    time.Time
}

// CancellationRequested reports whether the dispatch is being or has been
// canceled.
func (d InvestigationDispatch) CancellationRequested() bool {
	return d.State == InvestigationTaskCancelRequested || d.State == InvestigationTaskCanceled
}

func (d *InvestigationDispatch) touch(now time.Time) {
	d.Version++
	d.UpdatedAt = now
}

func (d *InvestigationDispatch) clearLease() {
	d.LeaseOwner = ""
	d.LeaseExpiresAt = time.Time{}
}

func (d *InvestigationDispatch) finish(state InvestigationTaskState, now time.Time) {
	d.State = state
	d.CompletedAt = now
	d.clearLease()
	d.touch(now)
}

// requireState refuses the update when cancellation was requested and
// otherwise when the dispatch is not in one of the allowed states.
func (d InvestigationDispatch) requireState(allowed ...InvestigationTaskState) error {
	if d.CancellationRequested() {
		return ErrInvestigationCancellationRequested
	}
	for _, state := range allowed {
		if d.State == state {
			return nil
		}
	}
	return fmt.Errorf("cannot update investigation task in %s state", d.State)
}

// AttachRef records the remote task. A requested cancellation is kept.
func (d *InvestigationDispatch) AttachRef(ref InvestigationTaskRef, now time.Time) {
	d.Ref = ref
	if !d.CancellationRequested() {
		d.State = InvestigationTaskSubmitted
	}
	d.touch(now)
}

// RecordProgress moves a submitted or working dispatch to working at stage.
func (d *InvestigationDispatch) RecordProgress(stage InvestigationStage, now time.Time) error {
	if err := d.requireState(InvestigationTaskSubmitted, InvestigationTaskWorking); err != nil {
		return err
	}
	d.State = InvestigationTaskWorking
	d.Stage = stage
	d.touch(now)
	return nil
}

// RetrySubmission returns a dispatch whose submission failed to the queue.
func (d *InvestigationDispatch) RetrySubmission(now time.Time) error {
	if err := d.requireState(InvestigationTaskSubmitting); err != nil {
		return err
	}
	d.State = InvestigationTaskQueued
	d.clearLease()
	d.touch(now)
	return nil
}

// RetryWatch makes a submitted dispatch claimable again at once, so another
// worker (or this one) resumes watching the existing remote task.
func (d *InvestigationDispatch) RetryWatch(worker string, now time.Time) error {
	if err := d.requireState(InvestigationTaskSubmitted, InvestigationTaskWorking); err != nil {
		return err
	}
	d.State = InvestigationTaskSubmitted
	d.LeaseOwner = worker
	d.LeaseExpiresAt = now
	d.touch(now)
	return nil
}

// Complete marks the investigation finished.
func (d *InvestigationDispatch) Complete(now time.Time) {
	d.Stage = InvestigationStageFinalizing
	d.finish(InvestigationTaskCompleted, now)
}

// Fail marks the investigation failed with a stable failure code.
func (d *InvestigationDispatch) Fail(code string, now time.Time) {
	d.FailureCode = code
	d.finish(InvestigationTaskFailed, now)
}

// Cancel marks the investigation canceled and releases its lease.
func (d *InvestigationDispatch) Cancel(now time.Time) {
	d.finish(InvestigationTaskCanceled, now)
}

// RequestCancel asks the worker that owns the remote task to cancel it.
func (d *InvestigationDispatch) RequestCancel(now time.Time) {
	d.State = InvestigationTaskCancelRequested
	d.touch(now)
}

// CancelPending cancels a dispatch that no worker has started (queued, no
// remote task) outright, and requests cancellation of any other.
func (d *InvestigationDispatch) CancelPending(now time.Time) {
	if d.State == InvestigationTaskQueued && d.Ref.TaskID == "" {
		d.Cancel(now)
		return
	}
	d.RequestCancel(now)
}
