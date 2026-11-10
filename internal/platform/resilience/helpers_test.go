package resilience_test

import (
	"context"
	"sync"
	"time"

	"github.com/belLena81/rollback-guard/internal/platform/resilience"
)

// fakeTime is a manual clock whose Sleep advances time instead of waiting.
type fakeTime struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

// newFakeTime starts at the real time so context deadlines derived from it
// are in the real future.
func newFakeTime() *fakeTime { return &fakeTime{now: time.Now()} }

func (f *fakeTime) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeTime) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func (f *fakeTime) timing() resilience.Timing {
	return resilience.Timing{
		Now: f.Now,
		Sleep: func(ctx context.Context, d time.Duration) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			f.mu.Lock()
			f.sleeps = append(f.sleeps, d)
			f.now = f.now.Add(d)
			f.mu.Unlock()
			return nil
		},
		Jitter: func() float64 { return 0.5 },
	}
}

type recordingObserver struct {
	mu      sync.Mutex
	retries []string
	states  []string
}

func (o *recordingObserver) RetryAttempted(dep resilience.Dependency, outcome resilience.RetryOutcome) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.retries = append(o.retries, string(dep)+"/"+string(outcome))
}

func (o *recordingObserver) BreakerStateChanged(dep resilience.Dependency, state resilience.BreakerState) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.states = append(o.states, string(dep)+"/"+state.String())
}

// must fails the test run on a constructor error; the arguments are
// constants, so an error here is a bug in the test.
func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

// ignore and discard drop a result the test deliberately does not inspect.
func ignore(error) {}

func discard[T any](T, error) {}

func equalDurations(a, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
