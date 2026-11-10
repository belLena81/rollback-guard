package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
)

func stringEnv(name string, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a positive duration: %w", name, err)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration, got %s", name, duration)
	}
	return duration, nil
}

// boundedDurationEnv reads a duration that must lie in [lower, upper]. A
// zero lower bound means "any positive duration up to upper". The error
// names the allowed range and carries the real cause: the parse error, or
// the out-of-range value.
func boundedDurationEnv(name string, fallback, lower, upper time.Duration) (time.Duration, error) {
	requirement := fmt.Sprintf("%s must be between %s and %s", name, formatDuration(lower), formatDuration(upper))
	if lower <= 0 {
		requirement = fmt.Sprintf("%s must be positive and no greater than %s", name, formatDuration(upper))
	}
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", requirement, err)
	}
	if duration <= 0 || duration < lower || duration > upper {
		return 0, fmt.Errorf("%s, got %s", requirement, formatDuration(duration))
	}
	return duration, nil
}

// formatDuration prints 2m and 1h instead of 2m0s and 1h0m0s.
func formatDuration(duration time.Duration) string {
	text := duration.String()
	if strings.HasSuffix(text, "m0s") {
		text = strings.TrimSuffix(text, "0s")
	}
	if strings.HasSuffix(text, "h0m") {
		text = strings.TrimSuffix(text, "0m")
	}
	return text
}

func intEnv(name string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a positive integer: %w", name, err)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer, got %d", name, parsed)
	}
	return parsed, nil
}

// boundedIntEnv reads an integer that must lie in [lower, upper].
func boundedIntEnv(name string, fallback, lower, upper int) (int, error) {
	requirement := fmt.Sprintf("%s must be between %d and %d", name, lower, upper)
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", requirement, err)
	}
	if parsed < lower || parsed > upper {
		return 0, fmt.Errorf("%s, got %d", requirement, parsed)
	}
	return parsed, nil
}

func floatEnv(name string, fallback float64) (float64, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a float: %w", name, err)
	}
	return parsed, nil
}

func targetsEnv(name string, fallback string) ([]domain.DeploymentTarget, error) {
	value := stringEnv(name, fallback)
	parts := strings.Split(value, ",")
	targets := make([]domain.DeploymentTarget, 0, len(parts))
	for _, part := range parts {
		fields := strings.Split(strings.TrimSpace(part), "/")
		if len(fields) != targetFieldCount {
			return nil, fmt.Errorf("%s target %q must be cluster/namespace/deployment/service", name, part)
		}
		target, err := domain.NewDeploymentTarget(fields[0], fields[1], fields[2], fields[3])
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		targets = append(targets, target)
	}
	return targets, nil
}
