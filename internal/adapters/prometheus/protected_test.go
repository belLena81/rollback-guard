package prometheus

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/platform/resilience"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

type scriptedAPI struct {
	errs  []error
	calls int
}

func (s *scriptedAPI) Query(context.Context, string, time.Time, ...v1.Option) (model.Value, v1.Warnings, error) {
	s.calls++
	if s.calls <= len(s.errs) {
		return nil, nil, s.errs[s.calls-1]
	}
	return model.Vector{}, v1.Warnings{"w"}, nil
}

func protector(t *testing.T) *resilience.Protector {
	t.Helper()
	timing := resilience.Timing{
		Now:    time.Now,
		Sleep:  func(context.Context, time.Duration) error { return nil },
		Jitter: func() float64 { return 0 },
	}
	retrier, err := resilience.NewRetrier(resilience.DependencyPrometheus, resilience.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}, timing, nil)
	if err != nil {
		t.Fatal(err)
	}
	breaker, err := resilience.NewBreaker(resilience.DependencyPrometheus, resilience.BreakerConfig{FailureThreshold: 5, OpenTimeout: time.Minute}, time.Now, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := resilience.NewProtector(retrier, breaker)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestQueryErrorTransience(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"server error", &v1.Error{Type: v1.ErrServer, Msg: "503"}, true},
		{"timeout", &v1.Error{Type: v1.ErrTimeout, Msg: "t"}, true},
		{"bad response", &v1.Error{Type: v1.ErrBadResponse, Msg: "x"}, true},
		{"bad query", &v1.Error{Type: v1.ErrBadData, Msg: "parse"}, false},
		{"client error", &v1.Error{Type: v1.ErrClient, Msg: "401"}, false},
		{"execution error", &v1.Error{Type: v1.ErrExec, Msg: "e"}, false},
		{"deadline", context.DeadlineExceeded, true},
		{"invalid result", ErrInvalidResult, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &scriptedAPI{errs: []error{tt.err}}
			protected, err := NewProtectedQueryAPI(api, protector(t))
			if err != nil {
				t.Fatal(err)
			}
			_, _, got := protected.Query(context.Background(), "q", time.Now())
			retried := api.calls > 1
			if retried != tt.want {
				t.Fatalf("retried = %v after %v, want %v", retried, got, tt.want)
			}
			if tt.want && got != nil {
				t.Fatalf("err = %v, want success after the retry", got)
			}
			if !tt.want && !errors.Is(got, tt.err) {
				t.Fatalf("err = %v, want %v surfaced unchanged", got, tt.err)
			}
		})
	}
}

func TestProtectedQueryKeepsValueAndWarnings(t *testing.T) {
	protected, err := NewProtectedQueryAPI(&scriptedAPI{}, protector(t))
	if err != nil {
		t.Fatal(err)
	}
	value, warnings, err := protected.Query(context.Background(), "q", time.Now())
	if err != nil || value == nil || len(warnings) != 1 {
		t.Fatalf("value=%v warnings=%v err=%v", value, warnings, err)
	}
}

func TestNewProtectedQueryAPIRequiresDependencies(t *testing.T) {
	if _, err := NewProtectedQueryAPI(nil, protector(t)); err == nil {
		t.Fatal("nil API accepted")
	}
	if _, err := NewProtectedQueryAPI(&scriptedAPI{}, nil); err == nil {
		t.Fatal("nil protector accepted")
	}
}
