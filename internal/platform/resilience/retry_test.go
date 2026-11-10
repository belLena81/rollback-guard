package resilience_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/resilience"
)

var testPolicy = resilience.RetryPolicy{MaxAttempts: 4, BaseDelay: 100 * time.Millisecond, MaxDelay: 300 * time.Millisecond}

func newRetrier(t *testing.T, ft *fakeTime, policy resilience.RetryPolicy, obs resilience.Observer) *resilience.Retrier {
	t.Helper()
	r, err := resilience.NewRetrier(resilience.DependencyObservabilityMCP, policy, ft.timing(), obs)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRetryTransientThenSuccess(t *testing.T) {
	ft, obs := newFakeTime(), &recordingObserver{}
	r := newRetrier(t, ft, testPolicy, obs)
	calls := 0
	got, err := resilience.Do(context.Background(), r, resilience.IsTransient, func(context.Context) (string, error) {
		calls++
		if calls < 3 {
			return "", context.DeadlineExceeded
		}
		return "ok", nil
	})
	if err != nil || got != "ok" || calls != 3 {
		t.Fatalf("got (%q, %v) after %d calls, want ok after 3", got, err, calls)
	}
	// Full jitter 0.5: ceilings 100ms then 200ms.
	if want := []time.Duration{50 * time.Millisecond, 100 * time.Millisecond}; !equalDurations(ft.sleeps, want) {
		t.Fatalf("sleeps = %v, want %v", ft.sleeps, want)
	}
	if want := []string{"observability_mcp/retried", "observability_mcp/retried"}; !equalStrings(obs.retries, want) {
		t.Fatalf("retry metrics = %v, want %v", obs.retries, want)
	}
}

func TestRetryBackoffIsCapped(t *testing.T) {
	ft := newFakeTime()
	r := newRetrier(t, ft, resilience.RetryPolicy{MaxAttempts: 5, BaseDelay: 100 * time.Millisecond, MaxDelay: 300 * time.Millisecond}, nil)
	discard(resilience.Do(context.Background(), r, resilience.IsTransient, func(context.Context) (int, error) { return 0, context.DeadlineExceeded }))
	want := []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 150 * time.Millisecond, 150 * time.Millisecond}
	if !equalDurations(ft.sleeps, want) {
		t.Fatalf("sleeps = %v, want %v", ft.sleeps, want)
	}
}

func TestRetryExhaustionReturnsLastError(t *testing.T) {
	ft, obs := newFakeTime(), &recordingObserver{}
	r := newRetrier(t, ft, resilience.RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}, obs)
	calls := 0
	_, err := resilience.Do(context.Background(), r, resilience.IsTransient, func(context.Context) (int, error) {
		calls++
		return 0, context.DeadlineExceeded
	})
	if !errors.Is(err, context.DeadlineExceeded) || calls != 2 {
		t.Fatalf("err = %v after %d calls, want deadline error after 2", err, calls)
	}
	if want := []string{"observability_mcp/retried", "observability_mcp/exhausted"}; !equalStrings(obs.retries, want) {
		t.Fatalf("retry metrics = %v, want %v", obs.retries, want)
	}
}

func TestRetryDoesNotRepeatNonTransientErrors(t *testing.T) {
	for _, err := range []error{
		&domain.ValidationError{Field: "f", Reason: "r"},
		&domain.NotFoundError{Resource: "r", ID: "i"},
		&domain.IdempotencyConflictError{},
		&domain.PolicyDenialError{Rule: "r"},
		context.Canceled,
		errors.New("unclassified"),
	} {
		ft, obs := newFakeTime(), &recordingObserver{}
		r := newRetrier(t, ft, testPolicy, obs)
		calls := 0
		_, got := resilience.Do(context.Background(), r, resilience.IsTransient, func(context.Context) (int, error) {
			calls++
			return 0, err
		})
		if !errors.Is(got, err) || calls != 1 || len(ft.sleeps) != 0 || len(obs.retries) != 0 {
			t.Fatalf("%T: err=%v calls=%d sleeps=%v retries=%v, want one call and no retry", err, got, calls, ft.sleeps, obs.retries)
		}
	}
}

func TestRetryNeverSleepsPastTheDeadline(t *testing.T) {
	ft := newFakeTime()
	r := newRetrier(t, ft, testPolicy, nil)
	// 40ms left; the first backoff is 50ms, so no retry is attempted.
	ctx, cancel := context.WithDeadline(context.Background(), ft.Now().Add(40*time.Millisecond))
	defer cancel()
	calls := 0
	_, err := resilience.Do(ctx, r, resilience.IsTransient, func(context.Context) (int, error) {
		calls++
		return 0, context.DeadlineExceeded
	})
	if !errors.Is(err, context.DeadlineExceeded) || calls != 1 || len(ft.sleeps) != 0 {
		t.Fatalf("err=%v calls=%d sleeps=%v, want a single call and no sleep", err, calls, ft.sleeps)
	}
}

func TestRetryStopsWhenCallerContextEnds(t *testing.T) {
	ft := newFakeTime()
	r := newRetrier(t, ft, testPolicy, nil)
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	_, err := resilience.Do(ctx, r, resilience.IsTransient, func(context.Context) (int, error) {
		calls++
		cancel()
		return 0, context.DeadlineExceeded
	})
	if calls != 1 || err == nil {
		t.Fatalf("calls=%d err=%v, want one call and an error", calls, err)
	}
}

func TestRetryDeadlineBoundary(t *testing.T) {
	// Far enough in the future that the real context never expires mid-test;
	// the fake clock decides what "remaining" means.
	deadlineIn := func(ft *fakeTime, d time.Duration) (context.Context, context.CancelFunc) {
		return context.WithDeadline(context.Background(), ft.Now().Add(d))
	}
	run := func(remaining time.Duration) (calls int, sleeps []time.Duration) {
		ft := newFakeTime()
		r := newRetrier(t, ft, resilience.RetryPolicy{MaxAttempts: 2, BaseDelay: 100 * time.Millisecond, MaxDelay: 100 * time.Millisecond}, nil)
		ctx, cancel := deadlineIn(ft, remaining)
		defer cancel()
		discard(resilience.Do(ctx, r, resilience.IsTransient, func(context.Context) (int, error) {
			calls++
			return 0, context.DeadlineExceeded
		}))
		return calls, ft.sleeps
	}
	// Jitter 0.5 makes the backoff 50ms.
	if calls, sleeps := run(50 * time.Millisecond); calls != 1 || len(sleeps) != 0 {
		t.Fatalf("delay == remaining: calls=%d sleeps=%v, want no retry", calls, sleeps)
	}
	if calls, sleeps := run(10 * time.Second); calls != 2 || len(sleeps) != 1 {
		t.Fatalf("ample budget: calls=%d sleeps=%v, want one retry", calls, sleeps)
	}
}

func TestRetryReturnsOriginalErrorWhenCancelledDuringSleep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	obs := &recordingObserver{}
	timing := newFakeTime().timing()
	timing.Sleep = func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }
	r, err := resilience.NewRetrier(resilience.DependencyPrometheus, testPolicy, timing, obs)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	_, got := resilience.Do(ctx, r, resilience.IsTransient, func(context.Context) (int, error) {
		calls++
		return 0, context.DeadlineExceeded
	})
	if calls != 1 || !errors.Is(got, context.DeadlineExceeded) || errors.Is(got, context.Canceled) {
		t.Fatalf("calls=%d err=%v, want one call and the dependency's own error", calls, got)
	}
}

func TestSystemSleepEndsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := resilience.SystemTiming().Sleep(ctx, time.Hour)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Fatalf("err=%v after %v, want the context error promptly", err, time.Since(start))
	}
	if j := resilience.SystemTiming().Jitter(); j < 0 || j >= 1 {
		t.Fatalf("jitter = %v, want [0,1)", j)
	}
}

func TestSingleAttemptPolicyEmitsNoRetryMetrics(t *testing.T) {
	obs := &recordingObserver{}
	r := newRetrier(t, newFakeTime(), resilience.RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}, obs)
	discard(resilience.Do(context.Background(), r, resilience.IsTransient, func(context.Context) (int, error) { return 0, context.DeadlineExceeded }))
	if len(obs.retries) != 0 {
		t.Fatalf("retry metrics = %v, want none", obs.retries)
	}
}

func TestRetryPolicyValidation(t *testing.T) {
	tests := []resilience.RetryPolicy{
		{MaxAttempts: 0, BaseDelay: time.Second, MaxDelay: time.Second},
		{MaxAttempts: 2, BaseDelay: 0, MaxDelay: time.Second},
		{MaxAttempts: 2, BaseDelay: 2 * time.Second, MaxDelay: time.Second},
	}
	for _, policy := range tests {
		if _, err := resilience.NewRetrier(resilience.DependencyPrometheus, policy, newFakeTime().timing(), nil); err == nil {
			t.Fatalf("policy %+v must be rejected", policy)
		}
	}
	if _, err := resilience.NewRetrier("", testPolicy, newFakeTime().timing(), nil); err == nil {
		t.Fatal("empty dependency must be rejected")
	}
}
