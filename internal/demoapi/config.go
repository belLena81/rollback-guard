// Package demoapi implements the deliberately breakable demo workload that
// Rollback Guardian watches in the local Kubernetes demo. One binary backs
// every demo image tag; the failure behaviour is selected by configuration.
package demoapi

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Mode selects the demo failure behaviour.
type Mode string

const (
	ModeHealthy     Mode = "healthy"
	ModeCrashLoop   Mode = "crashloop"
	ModeErrorStorm  Mode = "error-storm"
	ModeHighLatency Mode = "high-latency"
)

const (
	defaultWorkLatency        = 10 * time.Millisecond
	defaultHighLatency        = 1500 * time.Millisecond
	defaultErrorStormRate     = 0.8
	defaultCrashAfter         = 2 * time.Second
	maxWorkLatency            = 30 * time.Second
	maxCrashAfter             = time.Minute
	defaultHTTPAddr           = ":8080"
	defaultVersion            = "dev"
	defaultClusterLabel       = "local"
	defaultNamespaceLabel     = "rollback-demo"
	defaultDeploymentLabel    = "demo-api"
	defaultServiceLabel       = "demo-api"
	maxTargetLabelValueLength = 253
)

// targetLabelPattern accepts Kubernetes-style names so the emitted labels can
// match the cluster/namespace/deployment/service allow-list entries.
var targetLabelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

// TargetLabels are attached to every request metric. They must equal the
// deployment target the observability MCP is allowed to query, because the
// Prometheus adapter selects series by exactly these four labels.
type TargetLabels struct {
	Cluster    string
	Namespace  string
	Deployment string
	Service    string
}

// Config is the validated demo-api runtime configuration.
type Config struct {
	HTTPAddr    string
	Mode        Mode
	Version     string
	Target      TargetLabels
	WorkLatency time.Duration
	// ErrorRate is the probability in (0, 1] that /work returns 500. Only
	// non-zero in error-storm mode.
	ErrorRate float64
	// CrashAfter is how long the process serves before exiting with an
	// error. Only non-zero in crashloop mode.
	CrashAfter time.Duration
}

// LoadConfig reads configuration through lookup (os.LookupEnv in production).
func LoadConfig(lookup func(string) (string, bool)) (Config, error) {
	if lookup == nil {
		return Config{}, errors.New("demo-api config lookup is required")
	}
	get := func(name, fallback string) string {
		if value, ok := lookup(name); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
		return fallback
	}
	_, hasErrorRate := lookup("DEMO_ERROR_RATE")
	_, hasCrashAfter := lookup("DEMO_CRASH_AFTER")

	cfg := Config{
		HTTPAddr: get("HTTP_ADDR", defaultHTTPAddr),
		Mode:     Mode(get("DEMO_MODE", string(ModeHealthy))),
		Version:  get("DEMO_VERSION", defaultVersion),
		Target: TargetLabels{
			Cluster:    get("DEMO_CLUSTER", defaultClusterLabel),
			Namespace:  get("DEMO_NAMESPACE", defaultNamespaceLabel),
			Deployment: get("DEMO_DEPLOYMENT", defaultDeploymentLabel),
			Service:    get("DEMO_SERVICE", defaultServiceLabel),
		},
	}
	if err := cfg.Mode.validate(); err != nil {
		return Config{}, err
	}

	latencyDefault := defaultWorkLatency
	if cfg.Mode == ModeHighLatency {
		latencyDefault = defaultHighLatency
	}
	latency, err := time.ParseDuration(get("DEMO_WORK_LATENCY", latencyDefault.String()))
	if err != nil || latency < 0 || latency > maxWorkLatency {
		return Config{}, fmt.Errorf("DEMO_WORK_LATENCY must be a duration between 0 and %s", maxWorkLatency)
	}
	cfg.WorkLatency = latency

	if cfg.ErrorRate, err = loadErrorRate(cfg.Mode, get, hasErrorRate); err != nil {
		return Config{}, err
	}
	if cfg.CrashAfter, err = loadCrashAfter(cfg.Mode, get, hasCrashAfter); err != nil {
		return Config{}, err
	}

	if err := cfg.Target.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// loadErrorRate reads DEMO_ERROR_RATE, which only error-storm mode accepts.
func loadErrorRate(mode Mode, get func(name, fallback string) string, present bool) (float64, error) {
	if mode != ModeErrorStorm {
		if present {
			return 0, errors.New("DEMO_ERROR_RATE is only valid with DEMO_MODE=error-storm")
		}
		return 0, nil
	}
	rate, err := strconv.ParseFloat(get("DEMO_ERROR_RATE", strconv.FormatFloat(defaultErrorStormRate, 'f', -1, 64)), 64)
	if err != nil || math.IsNaN(rate) || rate <= 0 || rate > 1 {
		return 0, errors.New("DEMO_ERROR_RATE must be greater than 0 and at most 1")
	}
	return rate, nil
}

// loadCrashAfter reads DEMO_CRASH_AFTER, which only crashloop mode accepts.
func loadCrashAfter(mode Mode, get func(name, fallback string) string, present bool) (time.Duration, error) {
	if mode != ModeCrashLoop {
		if present {
			return 0, errors.New("DEMO_CRASH_AFTER is only valid with DEMO_MODE=crashloop")
		}
		return 0, nil
	}
	crashAfter, err := time.ParseDuration(get("DEMO_CRASH_AFTER", defaultCrashAfter.String()))
	if err != nil || crashAfter <= 0 || crashAfter > maxCrashAfter {
		return 0, fmt.Errorf("DEMO_CRASH_AFTER must be a positive duration no greater than %s", maxCrashAfter)
	}
	return crashAfter, nil
}

func (m Mode) validate() error {
	switch m {
	case ModeHealthy, ModeCrashLoop, ModeErrorStorm, ModeHighLatency:
		return nil
	default:
		return fmt.Errorf("DEMO_MODE must be one of %s, %s, %s, %s", ModeHealthy, ModeCrashLoop, ModeErrorStorm, ModeHighLatency)
	}
}

func (t TargetLabels) validate() error {
	for _, item := range []struct{ env, value string }{
		{"DEMO_CLUSTER", t.Cluster},
		{"DEMO_NAMESPACE", t.Namespace},
		{"DEMO_DEPLOYMENT", t.Deployment},
		{"DEMO_SERVICE", t.Service},
	} {
		if len(item.value) > maxTargetLabelValueLength || !targetLabelPattern.MatchString(item.value) {
			return fmt.Errorf("%s must be a lowercase Kubernetes-style name", item.env)
		}
	}
	return nil
}
