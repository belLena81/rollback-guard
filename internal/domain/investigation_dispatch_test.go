package domain

import (
	"errors"
	"testing"
	"time"
)

func leasedDispatch(state InvestigationTaskState) InvestigationDispatch {
	return InvestigationDispatch{
		IncidentID:     "incident-1",
		State:          state,
		Stage:          InvestigationStageMetrics,
		LeaseOwner:     "worker-1",
		LeaseExpiresAt: time.Unix(500, 0),
		Version:        4,
		UpdatedAt:      time.Unix(1, 0),
	}
}

func TestInvestigationDispatchTerminalTransitions(t *testing.T) {
	now := time.Unix(1000, 0)
	tests := []struct {
		name      string
		apply     func(*InvestigationDispatch)
		wantState InvestigationTaskState
		wantCode  string
	}{
		{"complete", func(d *InvestigationDispatch) { d.Complete(now) }, InvestigationTaskCompleted, ""},
		{"fail", func(d *InvestigationDispatch) { d.Fail("boom", now) }, InvestigationTaskFailed, "boom"},
		{"cancel", func(d *InvestigationDispatch) { d.Cancel(now) }, InvestigationTaskCanceled, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := leasedDispatch(InvestigationTaskWorking)
			tt.apply(&d)
			if d.State != tt.wantState || d.FailureCode != tt.wantCode {
				t.Fatalf("state/code = %s/%q", d.State, d.FailureCode)
			}
			if d.Version != 5 || !d.UpdatedAt.Equal(now) || !d.CompletedAt.Equal(now) {
				t.Fatalf("version/timestamps = %d %v %v", d.Version, d.UpdatedAt, d.CompletedAt)
			}
			if d.LeaseOwner != "" || !d.LeaseExpiresAt.IsZero() {
				t.Fatalf("lease not cleared: %q %v", d.LeaseOwner, d.LeaseExpiresAt)
			}
			if !d.State.Terminal() {
				t.Fatalf("%s is not terminal", d.State)
			}
		})
	}
}

func TestInvestigationDispatchCompleteSetsFinalizingStage(t *testing.T) {
	d := leasedDispatch(InvestigationTaskWorking)
	d.Complete(time.Unix(1000, 0))
	if d.Stage != InvestigationStageFinalizing {
		t.Fatalf("stage = %s", d.Stage)
	}
}

func TestInvestigationDispatchCancelPending(t *testing.T) {
	now := time.Unix(1000, 0)
	tests := []struct {
		name  string
		state InvestigationTaskState
		task  string
		want  InvestigationTaskState
	}{
		{"queued without remote task is canceled outright", InvestigationTaskQueued, "", InvestigationTaskCanceled},
		{"queued with remote task requests cancellation", InvestigationTaskQueued, "task-1", InvestigationTaskCancelRequested},
		{"working requests cancellation", InvestigationTaskWorking, "task-1", InvestigationTaskCancelRequested},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := leasedDispatch(tt.state)
			d.Ref.TaskID = tt.task
			d.CancelPending(now)
			if d.State != tt.want || d.Version != 5 || !d.UpdatedAt.Equal(now) {
				t.Fatalf("state/version/updated = %s/%d/%v", d.State, d.Version, d.UpdatedAt)
			}
		})
	}
}

func TestInvestigationDispatchGuardedTransitions(t *testing.T) {
	now := time.Unix(1000, 0)
	tests := []struct {
		name      string
		state     InvestigationTaskState
		apply     func(*InvestigationDispatch) error
		wantState InvestigationTaskState
		wantErr   error
		wantAny   bool
	}{
		{"progress from submitted", InvestigationTaskSubmitted, func(d *InvestigationDispatch) error { return d.RecordProgress(InvestigationStageDiagnosis, now) }, InvestigationTaskWorking, nil, false},
		{"progress while submitting is refused", InvestigationTaskSubmitting, func(d *InvestigationDispatch) error { return d.RecordProgress(InvestigationStageDiagnosis, now) }, InvestigationTaskSubmitting, nil, true},
		{"progress after cancel request", InvestigationTaskCancelRequested, func(d *InvestigationDispatch) error { return d.RecordProgress(InvestigationStageDiagnosis, now) }, InvestigationTaskCancelRequested, ErrInvestigationCancellationRequested, false},
		{"progress after cancel", InvestigationTaskCanceled, func(d *InvestigationDispatch) error { return d.RecordProgress(InvestigationStageDiagnosis, now) }, InvestigationTaskCanceled, ErrInvestigationCancellationRequested, false},
		{"requeue a failed submission", InvestigationTaskSubmitting, func(d *InvestigationDispatch) error { return d.RetrySubmission(now) }, InvestigationTaskQueued, nil, false},
		{"requeue is refused once submitted", InvestigationTaskSubmitted, func(d *InvestigationDispatch) error { return d.RetrySubmission(now) }, InvestigationTaskSubmitted, nil, true},
		{"retry watch from working", InvestigationTaskWorking, func(d *InvestigationDispatch) error { return d.RetryWatch("worker-2", now) }, InvestigationTaskSubmitted, nil, false},
		{"retry watch after cancel request", InvestigationTaskCancelRequested, func(d *InvestigationDispatch) error { return d.RetryWatch("worker-2", now) }, InvestigationTaskCancelRequested, ErrInvestigationCancellationRequested, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := leasedDispatch(tt.state)
			err := tt.apply(&d)
			refused := tt.wantErr != nil || tt.wantAny
			switch {
			case tt.wantErr != nil && !errors.Is(err, tt.wantErr):
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			case tt.wantAny && err == nil:
				t.Fatal("transition succeeded, want refusal")
			case !refused && err != nil:
				t.Fatalf("unexpected error: %v", err)
			}
			if d.State != tt.wantState {
				t.Fatalf("state = %s, want %s", d.State, tt.wantState)
			}
			if refused && (d.Version != 4 || !d.UpdatedAt.Equal(time.Unix(1, 0))) {
				t.Fatalf("refused transition changed version/timestamp: %d %v", d.Version, d.UpdatedAt)
			}
			if !refused && (d.Version != 5 || !d.UpdatedAt.Equal(now)) {
				t.Fatalf("version/timestamp = %d %v", d.Version, d.UpdatedAt)
			}
		})
	}
}

func TestInvestigationDispatchRetryLeases(t *testing.T) {
	now := time.Unix(1000, 0)
	watch := leasedDispatch(InvestigationTaskWorking)
	if err := watch.RetryWatch("worker-2", now); err != nil {
		t.Fatal(err)
	}
	if watch.LeaseOwner != "worker-2" || !watch.LeaseExpiresAt.Equal(now) {
		t.Fatalf("watch lease = %q %v", watch.LeaseOwner, watch.LeaseExpiresAt)
	}
	submit := leasedDispatch(InvestigationTaskSubmitting)
	if err := submit.RetrySubmission(now); err != nil {
		t.Fatal(err)
	}
	if submit.LeaseOwner != "" || !submit.LeaseExpiresAt.IsZero() {
		t.Fatalf("submission lease not cleared: %q %v", submit.LeaseOwner, submit.LeaseExpiresAt)
	}
}

func TestInvestigationDispatchAttachRef(t *testing.T) {
	now := time.Unix(1000, 0)
	ref := InvestigationTaskRef{TaskID: "task-1", ContextID: "ctx-1"}
	tests := []struct {
		name  string
		state InvestigationTaskState
		want  InvestigationTaskState
	}{
		{"submitting becomes submitted", InvestigationTaskSubmitting, InvestigationTaskSubmitted},
		{"cancel request is kept", InvestigationTaskCancelRequested, InvestigationTaskCancelRequested},
		{"cancel is kept", InvestigationTaskCanceled, InvestigationTaskCanceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := leasedDispatch(tt.state)
			d.AttachRef(ref, now)
			if d.Ref != ref || d.State != tt.want || d.Version != 5 || !d.UpdatedAt.Equal(now) {
				t.Fatalf("ref/state/version = %+v/%s/%d", d.Ref, d.State, d.Version)
			}
		})
	}
}
