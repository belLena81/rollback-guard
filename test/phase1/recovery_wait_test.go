package phase1_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

// observedMetrics counts Observe calls and runs onObserve (if set) before
// delegating, so a test can change the metric between recovery checks.
type observedMetrics struct {
	mu        sync.Mutex
	inner     ports.MetricsPort
	calls     int
	onObserve func(call int)
}

func (m *observedMetrics) Observe(ctx context.Context, target domain.DeploymentTarget, symptom domain.Symptom) (domain.MetricEvidence, error) {
	m.mu.Lock()
	m.calls++
	call, hook := m.calls, m.onObserve
	m.mu.Unlock()
	if hook != nil {
		hook(call)
	}
	return m.inner.Observe(ctx, target, symptom)
}

func (m *observedMetrics) watch(hook func(call int)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = 0
	m.onObserve = hook
}

func (m *observedMetrics) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// recoveryHarness returns an executed incident (status verifying) whose
// metric is still breached, with Observe calls counted from here on.
func recoveryHarness(t *testing.T, timeout, interval time.Duration) (phaseHarness, *observedMetrics) {
	t.Helper()
	metrics := &observedMetrics{}
	h := approvedHarnessWithOverrides(t, harnessOverrides{
		wrapMetrics: func(inner ports.MetricsPort) ports.MetricsPort {
			metrics.inner = inner
			return metrics
		},
		recoveryTimeout: timeout, recoveryPollInterval: interval,
	})
	if _, err := h.services.Executions.Execute(context.Background(), h.incident.ID(), "execute"); err != nil {
		t.Fatal(err)
	}
	h.infrastructure.SetHealthy(true)
	assertIncidentStatus(t, h, domain.StatusVerifying)
	metrics.watch(nil)
	return h, metrics
}

// The rollback has completed but the metric window still holds the errors
// from before it: Verify keeps checking until the metric recovers.
func TestVerificationWaitsForTheMetricToRecover(t *testing.T) {
	h, metrics := recoveryHarness(t, time.Second, time.Millisecond)
	metrics.watch(func(call int) {
		if call == 3 {
			h.infrastructure.SetRecoveryMetric(0.01)
		}
	})
	verification, err := h.services.Verifications.Verify(context.Background(), h.incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	if !verification.Passed() {
		t.Fatalf("verification failed: %s", verification.FailureReason())
	}
	if got := metrics.count(); got != 3 {
		t.Fatalf("recovery checks = %d, want 3 (stop at the first passing one)", got)
	}
	assertIncidentStatus(t, h, domain.StatusResolved)
}

// Right after the pod switch the metric can be briefly unreadable (no fresh
// series yet). A failed read is not evidence of recovery or of failure, so
// the wait continues; only a successful, passing check resolves.
func TestVerificationWaitsThroughTransientMetricReadFailures(t *testing.T) {
	h, metrics := recoveryHarness(t, time.Second, time.Millisecond)
	metrics.watch(func(call int) {
		switch call {
		case 1:
			h.infrastructure.MetricErr = errors.New("no fresh series")
		case 3:
			h.infrastructure.MetricErr = nil
			h.infrastructure.SetRecoveryMetric(0.01)
		}
	})
	verification, err := h.services.Verifications.Verify(context.Background(), h.incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	if !verification.Passed() || metrics.count() != 3 {
		t.Fatalf("passed = %v after %d checks, want passed after 3", verification.Passed(), metrics.count())
	}
	assertIncidentStatus(t, h, domain.StatusResolved)
}

// When the last allowed check cannot read the metric either, Verify returns
// that error and records nothing: the incident stays verifying and a retry of
// the approval resumes it.
func TestVerificationReturnsTheLastReadFailure(t *testing.T) {
	h, metrics := recoveryHarness(t, 5*time.Millisecond, time.Millisecond)
	h.infrastructure.MetricErr = errors.New("metrics unavailable")
	if _, err := h.services.Verifications.Verify(context.Background(), h.incident.ID()); err == nil {
		t.Fatal("Verify() error = nil, want the read failure")
	}
	if got := metrics.count(); got != 6 {
		t.Fatalf("recovery checks = %d, want 6", got)
	}
	assertIncidentStatus(t, h, domain.StatusVerifying)
}

// The deployment can still be converging on the target revision when the
// first check runs; that is a failing check, not an error, and the wait
// continues until both the rollout and the metric have recovered.
func TestVerificationWaitsForTheDeploymentToConverge(t *testing.T) {
	h, metrics := recoveryHarness(t, time.Second, time.Millisecond)
	target := h.infrastructure.Deployment.CurrentRevision
	h.infrastructure.SetCurrentRevision("revision-still-rolling")
	h.infrastructure.SetRecoveryMetric(0.01)
	// The hook runs after a check's inspection: converge during the first
	// check, so the second one sees the target revision.
	metrics.watch(func(call int) {
		if call == 1 {
			h.infrastructure.SetCurrentRevision(target)
		}
	})
	verification, err := h.services.Verifications.Verify(context.Background(), h.incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	if !verification.Passed() || metrics.count() != 2 {
		t.Fatalf("passed = %v after %d checks, want passed after 2", verification.Passed(), metrics.count())
	}
}

// A deployment read failure during the wait is treated like a metric read
// failure: not evidence, keep waiting.
func TestVerificationWaitsThroughTransientDeploymentReadFailures(t *testing.T) {
	inspector := &flakyInspector{}
	metrics := &observedMetrics{}
	h := approvedHarnessWithOverrides(t, harnessOverrides{
		wrapInspector: func(inner ports.DeploymentInspector) ports.DeploymentInspector {
			inspector.inner = inner
			return inspector
		},
		wrapMetrics: func(inner ports.MetricsPort) ports.MetricsPort {
			metrics.inner = inner
			return metrics
		},
		recoveryTimeout: time.Second, recoveryPollInterval: time.Millisecond,
	})
	if _, err := h.services.Executions.Execute(context.Background(), h.incident.ID(), "execute"); err != nil {
		t.Fatal(err)
	}
	h.infrastructure.SetHealthy(true)
	h.infrastructure.SetRecoveryMetric(0.01)
	inspector.failNext(2)
	metrics.watch(nil)
	verification, err := h.services.Verifications.Verify(context.Background(), h.incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	// Two failed inspections never reach the metric; the third check passes.
	if !verification.Passed() || metrics.count() != 1 {
		t.Fatalf("passed = %v after %d metric reads, want passed after 1", verification.Passed(), metrics.count())
	}
	assertIncidentStatus(t, h, domain.StatusResolved)
}

// flakyInspector fails the next n inspections, then delegates.
type flakyInspector struct {
	mu    sync.Mutex
	inner ports.DeploymentInspector
	fails int
}

func (f *flakyInspector) failNext(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails = n
}

func (f *flakyInspector) InspectDeployment(ctx context.Context, target domain.DeploymentTarget) (domain.DeploymentEvidence, error) {
	f.mu.Lock()
	failing := f.fails > 0
	if failing {
		f.fails--
	}
	f.mu.Unlock()
	if failing {
		return domain.DeploymentEvidence{}, errors.New("api server unavailable")
	}
	return f.inner.InspectDeployment(ctx, target)
}

// The check count is 1 + floor(timeout/interval) when the timeout is not a
// multiple of the interval.
func TestVerificationCheckCountRoundsDown(t *testing.T) {
	h, metrics := recoveryHarness(t, 10*time.Millisecond, 3*time.Millisecond)
	if _, err := h.services.Verifications.Verify(context.Background(), h.incident.ID()); err == nil {
		t.Fatal("Verify() error = nil, want recovery failure")
	}
	if got := metrics.count(); got != 4 {
		t.Fatalf("recovery checks = %d, want 4", got)
	}
}

// Slow checks cannot stretch the wait: once RecoveryTimeout has elapsed on
// the workflow clock, the current check is the last one.
func TestVerificationStopsAtTheRecoveryDeadline(t *testing.T) {
	h, metrics := recoveryHarness(t, time.Minute, time.Millisecond)
	metrics.watch(func(call int) {
		if call == 2 {
			h.clock.Advance(time.Minute)
		}
	})
	if _, err := h.services.Verifications.Verify(context.Background(), h.incident.ID()); err == nil {
		t.Fatal("Verify() error = nil, want recovery failure")
	}
	if got := metrics.count(); got != 2 {
		t.Fatalf("recovery checks = %d, want 2 (deadline reached during the second)", got)
	}
	assertIncidentStatus(t, h, domain.StatusFailed)
}

// A metric that never recovers fails the incident after 1 + timeout/interval
// checks, recording only the last result.
func TestVerificationFailsWhenTheMetricNeverRecovers(t *testing.T) {
	h, metrics := recoveryHarness(t, 10*time.Millisecond, time.Millisecond)
	// A committed failed verification is returned together with an error.
	verification, err := h.services.Verifications.Verify(context.Background(), h.incident.ID())
	if err == nil {
		t.Fatal("Verify() error = nil, want recovery failure")
	}
	if verification.Passed() {
		t.Fatal("verification passed with a breached metric")
	}
	if got := metrics.count(); got != 11 {
		t.Fatalf("recovery checks = %d, want 11", got)
	}
	assertIncidentStatus(t, h, domain.StatusFailed)
}

// Without a recovery timeout, Verify checks exactly once (the Phase 1-5
// behaviour).
func TestVerificationWithoutRecoveryTimeoutChecksOnce(t *testing.T) {
	h, metrics := recoveryHarness(t, 0, 0)
	verification, err := h.services.Verifications.Verify(context.Background(), h.incident.ID())
	if err == nil {
		t.Fatal("Verify() error = nil, want recovery failure")
	}
	if verification.Passed() {
		t.Fatal("verification passed with a breached metric")
	}
	if got := metrics.count(); got != 1 {
		t.Fatalf("recovery checks = %d, want 1", got)
	}
	assertIncidentStatus(t, h, domain.StatusFailed)
}

// A caller that goes away while Verify waits leaves the incident verifying
// with nothing recorded, so a retry of the approval resumes verification.
func TestVerificationWaitStopsOnCancellation(t *testing.T) {
	h, metrics := recoveryHarness(t, time.Hour, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	metrics.watch(func(call int) {
		if call == 1 {
			cancel()
		}
	})
	if _, err := h.services.Verifications.Verify(ctx, h.incident.ID()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Verify() error = %v, want context.Canceled", err)
	}
	assertIncidentStatus(t, h, domain.StatusVerifying)
	if summary, err := h.services.Queries.GetIncidentSummary(context.Background(), h.incident.ID()); err != nil || summary.Verification != nil {
		t.Fatalf("summary verification = %v, err = %v; want none recorded", summary.Verification, err)
	}
}

func TestWorkflowConfigRejectsInvalidRecoveryWait(t *testing.T) {
	tests := []struct {
		name              string
		timeout, interval time.Duration
	}{
		{name: "negative timeout", timeout: -time.Second, interval: time.Second},
		{name: "timeout without interval", timeout: time.Minute},
		{name: "negative interval", timeout: time.Minute, interval: -time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			systemActor, err := domain.NewActor("rollback-guardian", domain.ActorSystem)
			if err != nil {
				t.Fatal(err)
			}
			h := newHarness(t, nil)
			_, err = application.NewServices(application.Dependencies{
				UnitOfWork: h.uow, Inspector: h.infrastructure, Metrics: h.infrastructure, Diagnoser: noopDiagnoser{},
				Executor: h.infrastructure, RollbackPolicy: domain.EligibilityPolicy{}, ApprovalPolicy: domain.ApprovalAuthorizer{},
				Clock: h.clock, IDGenerator: idGenerator{},
			}, application.WorkflowConfig{
				ProposalTTL: time.Minute, ApprovalTTL: time.Minute, CleanupTimeout: time.Second, SystemActor: systemActor,
				RecoveryTimeout: test.timeout, RecoveryPollInterval: test.interval,
			})
			if err == nil {
				t.Fatal("NewServices() error = nil, want invalid recovery wait")
			}
		})
	}
}

type noopDiagnoser struct{}

func (noopDiagnoser) Diagnose(context.Context, ports.InvestigationInput) (domain.InvestigationConclusion, error) {
	return domain.InvestigationConclusion{}, errors.New("unused")
}

type idGenerator struct{}

func (idGenerator) NewID(prefix string) string { return prefix + "-id" }
