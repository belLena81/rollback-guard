package resilience_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/resilience"
)

func newBreaker(t *testing.T, ft *fakeTime, obs resilience.Observer) *resilience.Breaker {
	t.Helper()
	b, err := resilience.NewBreaker(resilience.DependencyLLM, resilience.BreakerConfig{FailureThreshold: 3, OpenTimeout: 10 * time.Second}, ft.Now, obs)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fail(b *resilience.Breaker, err error) error {
	_, got := resilience.Guard(context.Background(), b, func(context.Context) (int, error) { return 0, err })
	return got
}

func succeed(b *resilience.Breaker) error {
	_, got := resilience.Guard(context.Background(), b, func(context.Context) (int, error) { return 1, nil })
	return got
}

func TestBreakerOpensAfterConsecutiveTransientFailures(t *testing.T) {
	ft, obs := newFakeTime(), &recordingObserver{}
	b := newBreaker(t, ft, obs)
	for range 3 {
		if err := fail(b, context.DeadlineExceeded); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v", err)
		}
	}
	if b.State() != resilience.StateOpen {
		t.Fatalf("state = %v, want open", b.State())
	}
	calls := 0
	_, err := resilience.Guard(context.Background(), b, func(context.Context) (int, error) { calls++; return 0, nil })
	var open *resilience.CircuitOpenError
	if !errors.As(err, &open) || !errors.Is(err, resilience.ErrCircuitOpen) || open.Dependency != resilience.DependencyLLM || calls != 0 {
		t.Fatalf("err = %v calls=%d, want typed open error without calling", err, calls)
	}
	if want := []string{"llm/open"}; !equalStrings(obs.states, want) {
		t.Fatalf("state metrics = %v, want %v", obs.states, want)
	}
}

func TestBreakerIgnoresNonTransientFailuresAndResetsOnSuccess(t *testing.T) {
	ft := newFakeTime()
	b := newBreaker(t, ft, nil)
	ignore(fail(b, context.DeadlineExceeded))
	ignore(fail(b, context.DeadlineExceeded))
	// A validation error proves the dependency answered: the streak resets.
	ignore(fail(b, &domain.ValidationError{Field: "f", Reason: "r"}))
	ignore(fail(b, context.DeadlineExceeded))
	ignore(fail(b, context.DeadlineExceeded))
	if b.State() != resilience.StateClosed {
		t.Fatalf("state = %v, want closed (failures were not consecutive)", b.State())
	}
	ignore(succeed(b))
	ignore(fail(b, context.DeadlineExceeded))
	if b.State() != resilience.StateClosed {
		t.Fatalf("state = %v, want closed after success reset", b.State())
	}
}

func TestBreakerCallerCancellationIsNeutral(t *testing.T) {
	ft := newFakeTime()
	b := newBreaker(t, ft, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 5 {
		discard(resilience.Guard(ctx, b, func(context.Context) (int, error) { return 0, context.DeadlineExceeded }))
	}
	if b.State() != resilience.StateClosed {
		t.Fatalf("state = %v: failures caused by the caller's own cancellation must not trip the breaker", b.State())
	}
}

func TestBreakerHalfOpenProbeClosesOnSuccess(t *testing.T) {
	ft, obs := newFakeTime(), &recordingObserver{}
	b := newBreaker(t, ft, obs)
	for range 3 {
		ignore(fail(b, context.DeadlineExceeded))
	}
	ft.advance(9 * time.Second)
	if err := succeed(b); !errors.Is(err, resilience.ErrCircuitOpen) {
		t.Fatalf("err = %v, want still open before the timeout", err)
	}
	ft.advance(time.Second)
	if err := succeed(b); err != nil {
		t.Fatalf("probe err = %v", err)
	}
	if b.State() != resilience.StateClosed {
		t.Fatalf("state = %v, want closed", b.State())
	}
	if want := []string{"llm/open", "llm/half_open", "llm/closed"}; !equalStrings(obs.states, want) {
		t.Fatalf("state metrics = %v, want %v", obs.states, want)
	}
}

func TestBreakerHalfOpenProbeFailureReopens(t *testing.T) {
	ft, obs := newFakeTime(), &recordingObserver{}
	b := newBreaker(t, ft, obs)
	for range 3 {
		ignore(fail(b, context.DeadlineExceeded))
	}
	ft.advance(10 * time.Second)
	if err := fail(b, context.DeadlineExceeded); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("probe err = %v", err)
	}
	if b.State() != resilience.StateOpen {
		t.Fatalf("state = %v, want open again", b.State())
	}
	if err := succeed(b); !errors.Is(err, resilience.ErrCircuitOpen) {
		t.Fatalf("err = %v, want fail-fast for a fresh open period", err)
	}
	if want := []string{"llm/open", "llm/half_open", "llm/open"}; !equalStrings(obs.states, want) {
		t.Fatalf("state metrics = %v, want %v", obs.states, want)
	}
}

func TestBreakerAllowsOnlyOneHalfOpenProbe(t *testing.T) {
	ft := newFakeTime()
	b := newBreaker(t, ft, nil)
	for range 3 {
		ignore(fail(b, context.DeadlineExceeded))
	}
	ft.advance(10 * time.Second)
	var concurrent error
	_, err := resilience.Guard(context.Background(), b, func(context.Context) (int, error) {
		concurrent = succeed(b) // second caller while the probe is in flight
		return 1, nil
	})
	if err != nil || !errors.Is(concurrent, resilience.ErrCircuitOpen) {
		t.Fatalf("probe err=%v concurrent err=%v, want the second caller rejected", err, concurrent)
	}
}

func TestBreakerCancelledProbeReleasesTheSlot(t *testing.T) {
	ft := newFakeTime()
	b := newBreaker(t, ft, nil)
	for range 3 {
		ignore(fail(b, context.DeadlineExceeded))
	}
	ft.advance(10 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	discard(resilience.Guard(ctx, b, func(context.Context) (int, error) { cancel(); return 0, context.Canceled }))
	if err := succeed(b); err != nil {
		t.Fatalf("next probe err = %v, want the slot to be free after a cancelled probe", err)
	}
}

// A call admitted while the breaker was closed can finish after it opened.
// Only the half-open probe may change an open breaker's state.
func TestBreakerStaleCallsCannotCloseOrReopen(t *testing.T) {
	ft := newFakeTime()
	b := newBreaker(t, ft, nil)
	var staleSuccess, staleFailure func()
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	done := make(chan struct{}, 2)
	go func() {
		discard(resilience.Guard(context.Background(), b, func(context.Context) (int, error) { started <- struct{}{}; <-release; return 1, nil }))
		done <- struct{}{}
	}()
	go func() {
		discard(resilience.Guard(context.Background(), b, func(context.Context) (int, error) {
			started <- struct{}{}
			<-release
			return 0, context.DeadlineExceeded
		}))
		done <- struct{}{}
	}()
	_, _ = staleSuccess, staleFailure
	<-started
	<-started
	for range 3 {
		ignore(fail(b, context.DeadlineExceeded))
	}
	if b.State() != resilience.StateOpen {
		t.Fatalf("state = %v, want open", b.State())
	}
	close(release)
	<-done
	<-done
	if b.State() != resilience.StateOpen {
		t.Fatalf("state = %v: a stale call changed an open breaker", b.State())
	}
	ft.advance(10 * time.Second)
	ignore(fail(b, context.DeadlineExceeded)) // probe fails
	// A stale success during the new open period must not close it either.
	if b.State() != resilience.StateOpen {
		t.Fatalf("state = %v, want open after failed probe", b.State())
	}
}

func TestBreakerStaysClosedBelowThresholdAndIgnoresDeadlines(t *testing.T) {
	ft := newFakeTime()
	b := newBreaker(t, ft, nil)
	ignore(fail(b, context.DeadlineExceeded))
	ignore(fail(b, context.DeadlineExceeded))
	if b.State() != resilience.StateClosed {
		t.Fatalf("state = %v, want closed at threshold-1", b.State())
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	for range 5 {
		discard(resilience.Guard(ctx, b, func(context.Context) (int, error) { return 0, context.DeadlineExceeded }))
	}
	if b.State() != resilience.StateClosed {
		t.Fatalf("state = %v: the caller's own expired deadline must be neutral", b.State())
	}
}

func TestBreakerAdmitsExactlyOneConcurrentProbe(t *testing.T) {
	ft := newFakeTime()
	b := newBreaker(t, ft, nil)
	for range 3 {
		ignore(fail(b, context.DeadlineExceeded))
	}
	ft.advance(10 * time.Second)
	var admitted atomic.Int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			discard(resilience.Guard(context.Background(), b, func(context.Context) (int, error) {
				admitted.Add(1)
				<-release
				return 1, nil
			}))
		})
	}
	// Rejected callers return immediately; wait until only the probe is left.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && admitted.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if got := admitted.Load(); got != 1 {
		t.Fatalf("admitted = %d, want exactly one half-open probe", got)
	}
}

// reentrantObserver reads the breaker from inside the callback; that would
// deadlock if the callback ran while the breaker's lock was held.
type reentrantObserver struct {
	b      *resilience.Breaker
	states []resilience.BreakerState
}

func (o *reentrantObserver) RetryAttempted(resilience.Dependency, resilience.RetryOutcome) {}

func (o *reentrantObserver) BreakerStateChanged(_ resilience.Dependency, state resilience.BreakerState) {
	o.states = append(o.states, o.b.State())
	_ = state
}

func TestBreakerNotifiesObserverOutsideItsLock(t *testing.T) {
	ft := newFakeTime()
	obs := &reentrantObserver{}
	b := must(resilience.NewBreaker(resilience.DependencyLLM, resilience.BreakerConfig{FailureThreshold: 1, OpenTimeout: time.Second}, ft.Now, obs))
	obs.b = b
	finished := make(chan struct{})
	go func() {
		ignore(fail(b, context.DeadlineExceeded))
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("breaker deadlocked: observer was called while holding the lock")
	}
	if len(obs.states) != 1 || obs.states[0] != resilience.StateOpen {
		t.Fatalf("observer saw %v, want one open state", obs.states)
	}
}

func TestBreakerConfigValidation(t *testing.T) {
	for _, cfg := range []resilience.BreakerConfig{{FailureThreshold: 0, OpenTimeout: time.Second}, {FailureThreshold: 1, OpenTimeout: 0}} {
		if _, err := resilience.NewBreaker(resilience.DependencyLLM, cfg, newFakeTime().Now, nil); err == nil {
			t.Fatalf("config %+v must be rejected", cfg)
		}
	}
	if _, err := resilience.NewBreaker(resilience.DependencyLLM, resilience.BreakerConfig{FailureThreshold: 1, OpenTimeout: time.Second}, nil, nil); err == nil {
		t.Fatal("nil clock must be rejected")
	}
}
