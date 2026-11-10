package config

import (
	"fmt"
	"time"
)

// Resilience defaults and bounds. The bounds keep a typo from disabling
// retries' backoff (tiny delays) or from stalling a request for minutes.
const (
	defaultRetryMaxAttempts        = 3
	defaultLLMRetryMaxAttempts     = 2 // provider calls are slow and billed
	defaultRetryBaseDelay          = 200 * time.Millisecond
	defaultRetryMaxDelay           = 2 * time.Second
	defaultBreakerFailureThreshold = 5
	defaultBreakerOpenTimeout      = 30 * time.Second

	minRetryAttempts        = 1
	maxRetryAttempts        = 10
	minRetryDelay           = 10 * time.Millisecond
	maxRetryDelay           = time.Minute
	minBreakerThreshold     = 1
	maxBreakerThreshold     = 100
	minBreakerOpenTimeout   = time.Second
	maxBreakerOpenTimeout   = 10 * time.Minute
	retryMaxDelayEnvName    = "RESILIENCE_RETRY_MAX_DELAY"
	retryBaseDelayEnvName   = "RESILIENCE_RETRY_BASE_DELAY"
	retryAttemptsEnvName    = "RESILIENCE_RETRY_MAX_ATTEMPTS"
	llmRetryAttemptsEnvName = "RESILIENCE_LLM_RETRY_MAX_ATTEMPTS"
)

// ResilienceConfig is shared by every service. Attempts count the first call,
// so 1 disables retries.
type ResilienceConfig struct {
	RetryMaxAttempts    int
	RetryBaseDelay      time.Duration
	RetryMaxDelay       time.Duration
	LLMRetryMaxAttempts int
	// BreakerFailureThreshold is the number of consecutive transient
	// failures that opens a dependency's breaker.
	BreakerFailureThreshold int
	// BreakerOpenTimeout is how long an open breaker rejects calls before
	// it admits one probe.
	BreakerOpenTimeout time.Duration
}

// DefaultResilience returns the settings an unconfigured process uses.
func DefaultResilience() ResilienceConfig {
	return ResilienceConfig{
		RetryMaxAttempts:        defaultRetryMaxAttempts,
		RetryBaseDelay:          defaultRetryBaseDelay,
		RetryMaxDelay:           defaultRetryMaxDelay,
		LLMRetryMaxAttempts:     defaultLLMRetryMaxAttempts,
		BreakerFailureThreshold: defaultBreakerFailureThreshold,
		BreakerOpenTimeout:      defaultBreakerOpenTimeout,
	}
}

func resilienceEnv() (ResilienceConfig, error) {
	attempts, err := boundedIntEnv(retryAttemptsEnvName, defaultRetryMaxAttempts, minRetryAttempts, maxRetryAttempts)
	if err != nil {
		return ResilienceConfig{}, err
	}
	llmAttempts, err := boundedIntEnv(llmRetryAttemptsEnvName, defaultLLMRetryMaxAttempts, minRetryAttempts, maxRetryAttempts)
	if err != nil {
		return ResilienceConfig{}, err
	}
	baseDelay, err := boundedDurationEnv(retryBaseDelayEnvName, defaultRetryBaseDelay, minRetryDelay, maxRetryDelay)
	if err != nil {
		return ResilienceConfig{}, err
	}
	maxDelay, err := boundedDurationEnv(retryMaxDelayEnvName, max(defaultRetryMaxDelay, baseDelay), minRetryDelay, maxRetryDelay)
	if err != nil {
		return ResilienceConfig{}, err
	}
	if maxDelay < baseDelay {
		return ResilienceConfig{}, fmt.Errorf(
			"%s (%s) must be at least %s (%s)",
			retryMaxDelayEnvName,
			formatDuration(maxDelay),
			retryBaseDelayEnvName,
			formatDuration(baseDelay),
		)
	}
	threshold, err := boundedIntEnv("RESILIENCE_BREAKER_FAILURE_THRESHOLD", defaultBreakerFailureThreshold, minBreakerThreshold, maxBreakerThreshold)
	if err != nil {
		return ResilienceConfig{}, err
	}
	openTimeout, err := boundedDurationEnv("RESILIENCE_BREAKER_OPEN_TIMEOUT", defaultBreakerOpenTimeout, minBreakerOpenTimeout, maxBreakerOpenTimeout)
	if err != nil {
		return ResilienceConfig{}, err
	}
	return ResilienceConfig{
		RetryMaxAttempts:        attempts,
		RetryBaseDelay:          baseDelay,
		RetryMaxDelay:           maxDelay,
		LLMRetryMaxAttempts:     llmAttempts,
		BreakerFailureThreshold: threshold,
		BreakerOpenTimeout:      openTimeout,
	}, nil
}
