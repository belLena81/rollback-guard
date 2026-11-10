package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// TelemetryConfig is shared by every service.
type TelemetryConfig struct {
	ServiceName string
	// OTLPEndpoint is the OTLP/HTTP collector base URL; empty disables span
	// export (tracing stays on in-process for log correlation).
	OTLPEndpoint    string
	SampleRatio     float64
	ShutdownTimeout time.Duration
	LogLevel        slog.Level
}

func telemetryEnv(defaultServiceName string) (TelemetryConfig, error) {
	endpoint := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	if endpoint != "" {
		parsed, err := url.Parse(endpoint)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" ||
			parsed.Fragment != "" {
			// The value may carry credentials, so neither it nor the parse
			// error is reported.
			return TelemetryConfig{}, errors.New("OTEL_EXPORTER_OTLP_ENDPOINT must be an http(s) URL without credentials, query, or fragment")
		}
	}
	ratio := defaultTraceSampleRatio
	if value := strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLE_RATIO")); value != "" {
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return TelemetryConfig{}, fmt.Errorf("OTEL_TRACES_SAMPLE_RATIO must be between 0 and 1: %w", err)
		}
		if parsed < 0 || parsed > 1 {
			return TelemetryConfig{}, fmt.Errorf("OTEL_TRACES_SAMPLE_RATIO must be between 0 and 1, got %v", parsed)
		}
		ratio = parsed
	}
	shutdownTimeout, err := boundedDurationEnv("TELEMETRY_SHUTDOWN_TIMEOUT", defaultTelemetryShutdownTimeout, 0, maxTelemetryShutdownTimeout)
	if err != nil {
		return TelemetryConfig{}, err
	}
	levels := map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError}
	level, ok := levels[strings.ToLower(stringEnv("LOG_LEVEL", defaultLogLevel))]
	if !ok {
		return TelemetryConfig{}, errors.New("LOG_LEVEL must be debug, info, warn, or error")
	}
	return TelemetryConfig{
		ServiceName:     stringEnv("OTEL_SERVICE_NAME", defaultServiceName),
		OTLPEndpoint:    endpoint,
		SampleRatio:     ratio,
		ShutdownTimeout: shutdownTimeout,
		LogLevel:        level,
	}, nil
}
