package domain

import "time"

// Operational bounds that configuration and every adapter on the path
// validate against, declared once so the layers cannot drift apart.
const (
	// MinMetricsWindow and MaxMetricsWindow bound the observation window of
	// one metric query.
	MinMetricsWindow = 30 * time.Second
	MaxMetricsWindow = time.Hour

	// MinRolloutWatch and MaxRolloutWatch bound how long one rollout watch
	// may block.
	MinRolloutWatch = time.Second
	MaxRolloutWatch = 2 * time.Minute
)

// ValidMetricsWindow reports whether window is within the allowed bounds.
func ValidMetricsWindow(window time.Duration) bool {
	return window >= MinMetricsWindow && window <= MaxMetricsWindow
}

// ValidRolloutWatch reports whether timeout is within the allowed bounds.
func ValidRolloutWatch(timeout time.Duration) bool {
	return timeout >= MinRolloutWatch && timeout <= MaxRolloutWatch
}
