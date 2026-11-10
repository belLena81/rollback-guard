package mcp

import (
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
)

// The tool contracts carry durations as whole seconds. These checks compare
// in seconds, so an oversized untrusted value cannot overflow a
// time.Duration into the valid range.

// ValidWindowSeconds reports whether a tool's windowSeconds input is within
// the domain metrics window bounds.
func ValidWindowSeconds(seconds int) bool {
	return seconds >= int(domain.MinMetricsWindow/time.Second) && seconds <= int(domain.MaxMetricsWindow/time.Second)
}

// ValidWatchSeconds reports whether watch_rollout's timeoutSeconds input is
// within the domain rollout watch bounds.
func ValidWatchSeconds(seconds int) bool {
	return seconds >= int(domain.MinRolloutWatch/time.Second) && seconds <= int(domain.MaxRolloutWatch/time.Second)
}
