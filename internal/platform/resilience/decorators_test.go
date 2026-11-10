package resilience_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/resilience"
	"github.com/belLena81/rollback-guard/internal/ports"
)

func newProtector(t *testing.T, ft *fakeTime, dep resilience.Dependency, obs resilience.Observer) (*resilience.Protector, *resilience.Breaker) {
	t.Helper()
	retrier, err := resilience.NewRetrier(dep, resilience.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 4 * time.Millisecond}, ft.timing(), obs)
	if err != nil {
		t.Fatal(err)
	}
	breaker, err := resilience.NewBreaker(dep, resilience.BreakerConfig{FailureThreshold: 3, OpenTimeout: time.Minute}, ft.Now, obs)
	if err != nil {
		t.Fatal(err)
	}
	protector, err := resilience.NewProtector(retrier, breaker)
	if err != nil {
		t.Fatal(err)
	}
	return protector, breaker
}

type scriptedInspector struct {
	errs  []error
	calls int
}

func (s *scriptedInspector) InspectDeployment(context.Context, domain.DeploymentTarget) (domain.DeploymentEvidence, error) {
	s.calls++
	if s.calls <= len(s.errs) && s.errs[s.calls-1] != nil {
		return domain.DeploymentEvidence{}, s.errs[s.calls-1]
	}
	return domain.DeploymentEvidence{CurrentRevision: "rev"}, nil
}

func TestInspectorRetriesTransientThenSucceeds(t *testing.T) {
	ft, obs := newFakeTime(), &recordingObserver{}
	protector, _ := newProtector(t, ft, resilience.DependencyKubernetesReadonlyMCP, obs)
	next := &scriptedInspector{errs: []error{context.DeadlineExceeded, resilience.MarkTransient(errors.New("dependency_unavailable"))}}
	decorated, err := resilience.NewInspector(next, protector)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decorated.InspectDeployment(context.Background(), domain.DeploymentTarget{})
	if err != nil || got.CurrentRevision != "rev" || next.calls != 3 {
		t.Fatalf("got (%v, %v) after %d calls, want success on the third", got, err, next.calls)
	}
	if want := []string{"kubernetes_readonly_mcp/retried", "kubernetes_readonly_mcp/retried"}; !equalStrings(obs.retries, want) {
		t.Fatalf("retry metrics = %v, want %v", obs.retries, want)
	}
}

func TestInspectorDoesNotRetryPermanentErrors(t *testing.T) {
	for _, permanent := range []error{
		&domain.ValidationError{Field: "f", Reason: "r"},
		&domain.NotFoundError{Resource: "deployment", ID: "x"},
		&domain.ApprovalMismatchError{Reason: "drift"},
	} {
		ft := newFakeTime()
		protector, breaker := newProtector(t, ft, resilience.DependencyKubernetesReadonlyMCP, nil)
		next := &scriptedInspector{errs: []error{permanent}}
		decorated := must(resilience.NewInspector(next, protector))
		_, err := decorated.InspectDeployment(context.Background(), domain.DeploymentTarget{})
		if !errors.Is(err, permanent) || next.calls != 1 || breaker.State() != resilience.StateClosed {
			t.Fatalf("%T: err=%v calls=%d state=%v, want a single call and a closed breaker", permanent, err, next.calls, breaker.State())
		}
	}
}

func TestInspectorFailsFastWhileBreakerIsOpen(t *testing.T) {
	ft, obs := newFakeTime(), &recordingObserver{}
	protector, breaker := newProtector(t, ft, resilience.DependencyKubernetesReadonlyMCP, obs)
	next := &scriptedInspector{errs: []error{context.DeadlineExceeded, context.DeadlineExceeded, context.DeadlineExceeded}}
	decorated := must(resilience.NewInspector(next, protector))
	if _, err := decorated.InspectDeployment(context.Background(), domain.DeploymentTarget{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if breaker.State() != resilience.StateOpen || next.calls != 3 {
		t.Fatalf("state=%v calls=%d, want open after 3 consecutive transient failures", breaker.State(), next.calls)
	}
	_, err := decorated.InspectDeployment(context.Background(), domain.DeploymentTarget{})
	var open *resilience.CircuitOpenError
	if !errors.As(err, &open) || next.calls != 3 {
		t.Fatalf("err=%v calls=%d, want a typed open error without another call", err, next.calls)
	}
	// After the open timeout one probe goes through and closes the breaker.
	ft.advance(time.Minute)
	if _, err := decorated.InspectDeployment(context.Background(), domain.DeploymentTarget{}); err != nil || breaker.State() != resilience.StateClosed {
		t.Fatalf("probe err=%v state=%v, want recovery", err, breaker.State())
	}
	if want := []string{"kubernetes_readonly_mcp/open", "kubernetes_readonly_mcp/half_open", "kubernetes_readonly_mcp/closed"}; !equalStrings(obs.states, want) {
		t.Fatalf("breaker metrics = %v, want %v", obs.states, want)
	}
}

type scriptedMetrics struct {
	err   error
	calls int
}

func (s *scriptedMetrics) Observe(context.Context, domain.DeploymentTarget, domain.Symptom) (domain.MetricEvidence, error) {
	s.calls++
	if s.calls == 1 {
		return domain.MetricEvidence{}, s.err
	}
	return domain.MetricEvidence{}, nil
}

func TestMetricsRetriesOnlyTransient(t *testing.T) {
	ft := newFakeTime()
	protector, _ := newProtector(t, ft, resilience.DependencyObservabilityMCP, nil)
	transientNext := &scriptedMetrics{err: context.DeadlineExceeded}
	decorated := must(resilience.NewMetrics(transientNext, protector))
	if _, err := decorated.Observe(context.Background(), domain.DeploymentTarget{}, domain.Symptom{}); err != nil || transientNext.calls != 2 {
		t.Fatalf("err=%v calls=%d, want one retry then success", err, transientNext.calls)
	}
	validationNext := &scriptedMetrics{err: &domain.ValidationError{Field: "kind", Reason: "unsupported"}}
	decorated = must(resilience.NewMetrics(validationNext, protector))
	if _, err := decorated.Observe(context.Background(), domain.DeploymentTarget{}, domain.Symptom{}); !errors.Is(err, domain.ErrValidation) || validationNext.calls != 1 {
		t.Fatalf("err=%v calls=%d, want no retry for validation", err, validationNext.calls)
	}
}

type scriptedReasoner struct {
	err   error
	calls int
}

func (s *scriptedReasoner) Reason(context.Context, ports.ReasoningRequest) (ports.ReasoningResult, error) {
	s.calls++
	return ports.ReasoningResult{}, s.err
}

func TestReasonerOpenBreakerReportsUnavailableSoRuleFallbackStaysInCharge(t *testing.T) {
	ft := newFakeTime()
	protector, breaker := newProtector(t, ft, resilience.DependencyLLM, nil)
	next := &scriptedReasoner{err: ports.ErrReasonerUnavailable}
	decorated := must(resilience.NewReasoner(next, protector))
	if _, err := decorated.Reason(context.Background(), ports.ReasoningRequest{}); !errors.Is(err, ports.ErrReasonerUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if breaker.State() != resilience.StateOpen {
		t.Fatalf("state = %v, want open", breaker.State())
	}
	calls := next.calls
	_, err := decorated.Reason(context.Background(), ports.ReasoningRequest{})
	if !errors.Is(err, ports.ErrReasonerUnavailable) || !errors.Is(err, resilience.ErrCircuitOpen) || next.calls != calls {
		t.Fatalf("err=%v calls=%d, want unavailable + open without calling the provider", err, next.calls)
	}
}

func TestReasonerDoesNotRetryRefusalOrInvalidOutput(t *testing.T) {
	for _, reasonErr := range []error{ports.ErrReasonerRefused, ports.ErrReasonerInvalidResponse} {
		protector, breaker := newProtector(t, newFakeTime(), resilience.DependencyLLM, nil)
		next := &scriptedReasoner{err: reasonErr}
		decorated := must(resilience.NewReasoner(next, protector))
		if _, err := decorated.Reason(context.Background(), ports.ReasoningRequest{}); !errors.Is(err, reasonErr) || next.calls != 1 || breaker.State() != resilience.StateClosed {
			t.Fatalf("%v: err=%v calls=%d, want one call", reasonErr, err, next.calls)
		}
	}
}

type countingGateway struct {
	ports.InvestigationGateway
	submitErrs []error
	submits    int
	watches    int
	watchErr   error
}

func (g *countingGateway) Submit(context.Context, ports.InvestigationSubmission) (ports.InvestigationTaskRef, error) {
	g.submits++
	if g.submits <= len(g.submitErrs) {
		return ports.InvestigationTaskRef{}, g.submitErrs[g.submits-1]
	}
	return ports.InvestigationTaskRef{TaskID: "t", ContextID: "c"}, nil
}

func (g *countingGateway) Watch(context.Context, ports.InvestigationTaskRef, func(ports.InvestigationStage) error) (ports.InvestigationResult, error) {
	g.watches++
	return ports.InvestigationResult{}, g.watchErr
}

func TestGatewayRetriesSubmitButNeverWatch(t *testing.T) {
	ft := newFakeTime()
	protector, _ := newProtector(t, ft, resilience.DependencyA2AInvestigator, nil)
	next := &countingGateway{submitErrs: []error{context.DeadlineExceeded}, watchErr: context.DeadlineExceeded}
	decorated := must(resilience.NewGateway(next, protector))
	if ref, err := decorated.Submit(context.Background(), ports.InvestigationSubmission{}); err != nil || ref.TaskID != "t" || next.submits != 2 {
		t.Fatalf("ref=%v err=%v submits=%d, want success on the second submit", ref, err, next.submits)
	}
	if _, err := decorated.Watch(context.Background(), ports.InvestigationTaskRef{}, nil); !errors.Is(err, context.DeadlineExceeded) || next.watches != 1 {
		t.Fatalf("err=%v watches=%d, want exactly one watch (the coordinator owns re-watching)", err, next.watches)
	}
}

// ledgerExecutor models the rollback server: the first call per key applies
// the mutation, later calls with the same key return the recorded result.
type ledgerExecutor struct {
	before  []error // returned before the mutation is applied
	after   error   // returned after the mutation is applied (reply lost)
	calls   int
	applied map[domain.IdempotencyKey]int
	keys    []domain.IdempotencyKey
}

func (l *ledgerExecutor) ExecuteApprovedRollback(_ context.Context, _ domain.ApprovedRollback, key domain.IdempotencyKey) (domain.RollbackExecution, error) {
	l.calls++
	l.keys = append(l.keys, key)
	if l.calls <= len(l.before) {
		return domain.RollbackExecution{}, l.before[l.calls-1]
	}
	if l.applied == nil {
		l.applied = map[domain.IdempotencyKey]int{}
	}
	if l.applied[key] == 0 {
		l.applied[key] = 1
	}
	return domain.RollbackExecution{}, l.after
}

func newExecutorRetrier(t *testing.T, ft *fakeTime, obs resilience.Observer) *resilience.Retrier {
	t.Helper()
	r, err := resilience.NewRetrier(resilience.DependencyKubernetesRollbackMCP, resilience.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}, ft.timing(), obs)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

var dialRefused = &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}

func TestExecutorRetriesOnlyWhenRequestWasProvablyNotSent(t *testing.T) {
	ft, obs := newFakeTime(), &recordingObserver{}
	next := &ledgerExecutor{before: []error{fmt.Errorf("call MCP tool: %w", dialRefused)}}
	decorated, err := resilience.NewExecutor(next, newExecutorRetrier(t, ft, obs))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decorated.ExecuteApprovedRollback(context.Background(), domain.ApprovedRollback{}, "key-1"); err != nil {
		t.Fatalf("err = %v", err)
	}
	if next.calls != 2 || next.applied["key-1"] != 1 || next.keys[0] != next.keys[1] {
		t.Fatalf("calls=%d applied=%v keys=%v, want one retry with the same key and one mutation", next.calls, next.applied, next.keys)
	}
	if want := []string{"kubernetes_rollback_mcp/retried"}; !equalStrings(obs.retries, want) {
		t.Fatalf("retry metrics = %v, want %v", obs.retries, want)
	}
}

func TestExecutorNeverRetriesAmbiguousOrPermanentErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"outcome unknown", ports.ErrRollbackOutcomeUnknown},
		{"outcome unknown wrapping a dial failure", fmt.Errorf("%w: %w", ports.ErrRollbackOutcomeUnknown, dialRefused)},
		{"timeout", context.DeadlineExceeded},
		{"reset after connect", &net.OpError{Op: "read", Err: syscall.ECONNRESET}},
		{"dependency unavailable tool error", resilience.MarkTransient(errors.New("dependency_unavailable"))},
		{"validation", &domain.ValidationError{Field: "f", Reason: "r"}},
		{"policy denied", &domain.PolicyDenialError{Rule: "r"}},
		{"idempotency conflict", &domain.IdempotencyConflictError{}},
		{"unclassified", errors.New("boom")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ft, obs := newFakeTime(), &recordingObserver{}
			next := &ledgerExecutor{after: tt.err}
			decorated := must(resilience.NewExecutor(next, newExecutorRetrier(t, ft, obs)))
			_, err := decorated.ExecuteApprovedRollback(context.Background(), domain.ApprovedRollback{}, "key-1")
			if !errors.Is(err, tt.err) {
				t.Fatalf("err = %v, want it surfaced unchanged", err)
			}
			if next.calls != 1 || next.applied["key-1"] != 1 || len(ft.sleeps) != 0 || len(obs.retries) != 0 {
				t.Fatalf("calls=%d applied=%v sleeps=%v retries=%v, want exactly one call and one mutation", next.calls, next.applied, ft.sleeps, obs.retries)
			}
		})
	}
}

// rawServerExecutor has no idempotency ledger at all: every call that gets
// past the dial applies the mutation again. Only the decorator can keep the
// count at one, so the assertions measure real double-application.
type rawServerExecutor struct {
	dialFailures int   // leading calls that fail before reaching the server
	replyErr     error // returned by calls that reached the server (mutation applied)
	reached      int
	applied      int
}

func (r *rawServerExecutor) ExecuteApprovedRollback(context.Context, domain.ApprovedRollback, domain.IdempotencyKey) (domain.RollbackExecution, error) {
	if r.dialFailures > 0 {
		r.dialFailures--
		return domain.RollbackExecution{}, dialRefused
	}
	r.reached++
	r.applied++
	return domain.RollbackExecution{}, r.replyErr
}

func TestExecutorNeverAppliesTwiceWithoutServerLedger(t *testing.T) {
	tests := []struct {
		name        string
		server      rawServerExecutor
		wantApplied int
		wantReached int
	}{
		{"dial refused once then success", rawServerExecutor{dialFailures: 1}, 1, 1},
		{"applied but reply lost to a read reset", rawServerExecutor{replyErr: &net.OpError{Op: "read", Err: syscall.ECONNRESET}}, 1, 1},
		{"applied but reply timed out", rawServerExecutor{replyErr: context.DeadlineExceeded}, 1, 1},
		{"applied but adapter reports unknown outcome", rawServerExecutor{replyErr: fmt.Errorf("%w: %w", ports.ErrRollbackOutcomeUnknown, dialRefused)}, 1, 1},
		{"applied but tool said dependency unavailable", rawServerExecutor{replyErr: resilience.MarkTransient(errors.New("dependency_unavailable"))}, 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := tt.server
			decorated := must(resilience.NewExecutor(&server, newExecutorRetrier(t, newFakeTime(), nil)))
			discard(decorated.ExecuteApprovedRollback(context.Background(), domain.ApprovedRollback{}, "key-1"))
			if server.applied != tt.wantApplied || server.reached != tt.wantReached {
				t.Fatalf("applied=%d reached=%d, want %d and %d", server.applied, server.reached, tt.wantApplied, tt.wantReached)
			}
		})
	}
}

func TestExecutorRetriesAreBounded(t *testing.T) {
	ft := newFakeTime()
	next := &ledgerExecutor{before: []error{dialRefused, dialRefused, dialRefused, dialRefused, dialRefused}}
	decorated := must(resilience.NewExecutor(next, newExecutorRetrier(t, ft, nil)))
	_, err := decorated.ExecuteApprovedRollback(context.Background(), domain.ApprovedRollback{}, "key-1")
	if !errors.Is(err, syscall.ECONNREFUSED) || next.calls != 3 || len(next.applied) != 0 {
		t.Fatalf("err=%v calls=%d applied=%v, want three bounded attempts and no mutation", err, next.calls, next.applied)
	}
}
