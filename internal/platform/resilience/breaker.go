package resilience

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// BreakerConfig tunes one breaker.
type BreakerConfig struct {
	// FailureThreshold is the number of consecutive transient failures that
	// opens the breaker.
	FailureThreshold int
	// OpenTimeout is how long the breaker rejects calls before it admits a
	// single half-open probe.
	OpenTimeout time.Duration
}

// Breaker is a consecutive-failure circuit breaker for one dependency. It is
// safe for concurrent use.
type Breaker struct {
	dependency Dependency
	config     BreakerConfig
	now        func() time.Time
	observer   Observer

	mu         sync.Mutex
	state      BreakerState
	failures   int
	openedAt   time.Time
	probeTaken bool
	pending    []BreakerState
}

// NewBreaker validates its inputs; observer may be nil.
func NewBreaker(dependency Dependency, config BreakerConfig, now func() time.Time, observer Observer) (*Breaker, error) {
	if strings.TrimSpace(string(dependency)) == "" {
		return nil, errors.New("breaker dependency is required")
	}
	if config.FailureThreshold < 1 || config.OpenTimeout <= 0 {
		return nil, fmt.Errorf("breaker %s: failure threshold and open timeout must be positive", dependency)
	}
	if now == nil {
		return nil, errors.New("breaker clock is required")
	}
	return &Breaker{dependency: dependency, config: config, now: now, observer: observer}, nil
}

// State returns the current state without advancing it.
func (b *Breaker) State() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// setState must be called with b.mu held. The observer is told by flush after
// the lock is released, so a slow log writer never stalls other callers.
func (b *Breaker) setState(state BreakerState) {
	b.state = state
	if b.observer != nil {
		b.pending = append(b.pending, state)
	}
}

func (b *Breaker) flush() {
	b.mu.Lock()
	pending := b.pending
	b.pending = nil
	b.mu.Unlock()
	for _, state := range pending {
		b.observer.BreakerStateChanged(b.dependency, state)
	}
}

type verdict uint8

const (
	verdictSuccess verdict = iota
	verdictFailure
	// verdictNeutral neither helps nor hurts: the caller gave up or the call
	// never reached the dependency.
	verdictNeutral
)

// admit returns whether the call may proceed and whether it is the half-open
// probe.
func (b *Breaker) admit() (allowed, probe bool) {
	defer b.flush() // runs after the unlock below
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case StateClosed:
		return true, false
	case StateOpen:
		if b.now().Sub(b.openedAt) < b.config.OpenTimeout {
			return false, false
		}
		b.setState(StateHalfOpen)
		b.probeTaken = true
		return true, true
	case StateHalfOpen:
		if b.probeTaken {
			return false, false
		}
		b.probeTaken = true
		return true, true
	}
	return false, false
}

func (b *Breaker) record(probe bool, v verdict) {
	defer b.flush() // runs after the unlock below
	b.mu.Lock()
	defer b.mu.Unlock()
	if probe {
		b.probeTaken = false
	}
	switch v {
	case verdictNeutral:
	case verdictSuccess:
		// A call admitted while closed can finish after the breaker opened;
		// only the half-open probe may close an open breaker.
		switch {
		case b.state == StateClosed:
			b.failures = 0
		case probe && b.state == StateHalfOpen:
			b.failures = 0
			b.setState(StateClosed)
		}
	case verdictFailure:
		switch {
		case probe && b.state == StateHalfOpen:
			b.openedAt = b.now()
			b.setState(StateOpen)
		case b.state == StateClosed:
			b.failures++
			if b.failures >= b.config.FailureThreshold {
				b.openedAt = b.now()
				b.setState(StateOpen)
			}
		}
	}
}

// Guard runs fn unless the breaker is open, in which case it returns a
// *CircuitOpenError without calling fn. A transient error counts as a
// failure; any other error means the dependency answered and counts as a
// success. If the caller's context ended, the outcome is neutral, so a client
// giving up never opens the breaker.
func Guard[T any](ctx context.Context, b *Breaker, fn func(context.Context) (T, error)) (T, error) {
	allowed, probe := b.admit()
	if !allowed {
		var zero T
		return zero, &CircuitOpenError{Dependency: b.dependency}
	}
	value, err := fn(ctx)
	switch {
	case ctx.Err() != nil, errors.Is(err, ErrCircuitOpen):
		b.record(probe, verdictNeutral)
	case IsTransient(err):
		b.record(probe, verdictFailure)
	default:
		b.record(probe, verdictSuccess)
	}
	return value, err
}
