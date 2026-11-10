package bootstrap

import (
	"github.com/belLena81/rollback-guard/internal/platform/config"
	"github.com/belLena81/rollback-guard/internal/platform/resilience"
)

// maxMutationAttempts caps retries of an approved rollback regardless of the
// general retry setting: one repeat after a connection that never opened is
// all the rule in resilience.NewExecutor ever needs.
const maxMutationAttempts = 2

func retryPolicy(cfg config.ResilienceConfig, attempts int) resilience.RetryPolicy {
	return resilience.RetryPolicy{MaxAttempts: attempts, BaseDelay: cfg.RetryBaseDelay, MaxDelay: cfg.RetryMaxDelay}
}

func newProtector(dependency resilience.Dependency, cfg config.ResilienceConfig, attempts int, observer resilience.Observer) (*resilience.Protector, error) {
	timing := resilience.SystemTiming()
	retrier, err := resilience.NewRetrier(dependency, retryPolicy(cfg, attempts), timing, observer)
	if err != nil {
		return nil, err
	}
	breaker, err := resilience.NewBreaker(dependency, resilience.BreakerConfig{
		FailureThreshold: cfg.BreakerFailureThreshold,
		OpenTimeout:      cfg.BreakerOpenTimeout,
	}, timing.Now, observer)
	if err != nil {
		return nil, err
	}
	return resilience.NewProtector(retrier, breaker)
}

// NewProtector builds the retry-plus-breaker protection for one external
// dependency from the shared settings. Each call creates an independent
// breaker, so create exactly one per dependency and share it between the
// ports that call that dependency.
func NewProtector(dependency resilience.Dependency, cfg config.ResilienceConfig, observer resilience.Observer) (*resilience.Protector, error) {
	return newProtector(dependency, cfg, cfg.RetryMaxAttempts, observer)
}

// NewLLMProtector is NewProtector with the (smaller) LLM attempt budget.
func NewLLMProtector(cfg config.ResilienceConfig, observer resilience.Observer) (*resilience.Protector, error) {
	return newProtector(resilience.DependencyLLM, cfg, cfg.LLMRetryMaxAttempts, observer)
}

// NewMutationRetrier builds the retrier for resilience.NewExecutor.
func NewMutationRetrier(dependency resilience.Dependency, cfg config.ResilienceConfig, observer resilience.Observer) (*resilience.Retrier, error) {
	return resilience.NewRetrier(dependency, retryPolicy(cfg, min(cfg.RetryMaxAttempts, maxMutationAttempts)), resilience.SystemTiming(), observer)
}
