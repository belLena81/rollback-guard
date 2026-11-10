package telemetry

import "github.com/belLena81/rollback-guard/internal/platform/resilience"

// RetryAttempted counts a retry decision. The Provider implements
// resilience.Observer so the decorators report into this process's registry.
// Both labels come from closed sets (resilience.Dependency constants and
// RetryOutcome).
func (p *Provider) RetryAttempted(dependency resilience.Dependency, outcome resilience.RetryOutcome) {
	p.metrics.retries.WithLabelValues(string(dependency), string(outcome)).Inc()
}

// BreakerStateChanged counts a breaker entering state.
func (p *Provider) BreakerStateChanged(dependency resilience.Dependency, state resilience.BreakerState) {
	p.metrics.breakerTransitions.WithLabelValues(string(dependency), state.String()).Inc()
	p.logger.Warn("circuit breaker state changed", "dependency", string(dependency), "state", state.String())
}

var _ resilience.Observer = (*Provider)(nil)
