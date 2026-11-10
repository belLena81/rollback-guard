package resilience

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"
)

// RetryOutcome labels the retry metric.
type RetryOutcome string

// Retry outcomes.
const (
	// RetryRetried counts a failed attempt that was followed by another.
	RetryRetried RetryOutcome = "retried"
	// RetryExhausted counts a call that failed on its last allowed attempt
	// after at least one retry.
	RetryExhausted RetryOutcome = "exhausted"
)

// BreakerState is a circuit-breaker state.
type BreakerState uint8

// Breaker states.
const (
	StateClosed BreakerState = iota
	StateOpen
	StateHalfOpen
)

// String returns the metric label for the state.
func (s BreakerState) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half_open"
	default:
		return "unknown"
	}
}

// Observer receives resilience events. Implementations must be cheap and must
// not block. A nil Observer is allowed everywhere.
type Observer interface {
	RetryAttempted(Dependency, RetryOutcome)
	BreakerStateChanged(Dependency, BreakerState)
}

// Timing injects the clock, sleeping, and jitter so tests never wait.
type Timing struct {
	Now func() time.Time
	// Sleep waits for d or returns the context error first.
	Sleep func(ctx context.Context, d time.Duration) error
	// Jitter returns a value in [0, 1).
	Jitter func() float64
}

// SystemTiming is the production Timing.
func SystemTiming() Timing {
	return Timing{
		Now: time.Now,
		Sleep: func(ctx context.Context, d time.Duration) error {
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
		// Backoff jitter only de-synchronizes retries; it is not a secret.
		Jitter: rand.Float64, //nolint:gosec // non-cryptographic jitter
	}
}

// RetryPolicy bounds the attempts and the backoff between them.
type RetryPolicy struct {
	// MaxAttempts is the total number of calls, including the first.
	MaxAttempts int
	// BaseDelay is the backoff ceiling before the first retry; it doubles
	// each retry up to MaxDelay. The actual wait is a uniform "full jitter"
	// draw from [0, ceiling).
	BaseDelay time.Duration
	MaxDelay  time.Duration
}

// Validate rejects a policy that could retry without bound or wait zero.
func (p RetryPolicy) Validate() error {
	switch {
	case p.MaxAttempts < 1:
		return errors.New("retry attempts must be at least 1")
	case p.BaseDelay <= 0:
		return errors.New("retry base delay must be positive")
	case p.MaxDelay < p.BaseDelay:
		return errors.New("retry max delay must be at least the base delay")
	}
	return nil
}

// Retrier runs a call with bounded, jittered, deadline-aware retries.
type Retrier struct {
	dependency Dependency
	policy     RetryPolicy
	timing     Timing
	observer   Observer
}

// NewRetrier validates its inputs; observer may be nil.
func NewRetrier(dependency Dependency, policy RetryPolicy, timing Timing, observer Observer) (*Retrier, error) {
	if strings.TrimSpace(string(dependency)) == "" {
		return nil, errors.New("retrier dependency is required")
	}
	if err := policy.Validate(); err != nil {
		return nil, fmt.Errorf("retrier %s: %w", dependency, err)
	}
	if timing.Now == nil || timing.Sleep == nil || timing.Jitter == nil {
		return nil, errors.New("retrier timing is incomplete")
	}
	return &Retrier{dependency: dependency, policy: policy, timing: timing, observer: observer}, nil
}

func (r *Retrier) backoff(retry int) time.Duration {
	ceiling := r.policy.BaseDelay
	for range retry - 1 {
		ceiling *= 2
		if ceiling >= r.policy.MaxDelay {
			ceiling = r.policy.MaxDelay
			break
		}
	}
	return time.Duration(float64(ceiling) * r.timing.Jitter())
}

func (r *Retrier) report(outcome RetryOutcome) {
	if r.observer != nil {
		r.observer.RetryAttempted(r.dependency, outcome)
	}
}

// Do calls fn, repeating it while retryable(err) holds, attempts remain, the
// caller's context is live, and the backoff would end before the context
// deadline. It returns the last result and error. The caller owns the
// decision of what is safe to repeat by choosing retryable and fn.
func Do[T any](ctx context.Context, r *Retrier, retryable func(error) bool, fn func(context.Context) (T, error)) (T, error) {
	for attempt := 1; ; attempt++ {
		value, err := fn(ctx)
		if err == nil || !retryable(err) || ctx.Err() != nil {
			return value, err
		}
		if attempt >= r.policy.MaxAttempts {
			if attempt > 1 {
				r.report(RetryExhausted)
			}
			return value, err
		}
		delay := r.backoff(attempt)
		if deadline, ok := ctx.Deadline(); ok && !r.timing.Now().Add(delay).Before(deadline) {
			return value, err
		}
		r.report(RetryRetried)
		if sleepErr := r.timing.Sleep(ctx, delay); sleepErr != nil {
			return value, err
		}
	}
}
