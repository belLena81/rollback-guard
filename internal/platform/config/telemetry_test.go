package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestTelemetryDefaultsPerService(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		service func(t *testing.T) TelemetryConfig
		want    string
	}{
		{name: "orchestrator", env: map[string]string{"DATABASE_URL": "postgres://rollback@database/rollback_guard"}, want: "orchestrator", service: func(t *testing.T) TelemetryConfig {
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			return cfg.Telemetry
		}},
		{name: "investigator", env: map[string]string{"DATABASE_URL": "postgres://rollback@database/rollback_guard"}, want: "investigator", service: func(t *testing.T) TelemetryConfig {
			cfg, err := LoadInvestigator()
			if err != nil {
				t.Fatal(err)
			}
			return cfg.Telemetry
		}},
		{name: "kubernetes readonly MCP", want: "kubernetes-mcp-readonly", service: func(t *testing.T) TelemetryConfig {
			cfg, err := LoadKubernetesMCP()
			if err != nil {
				t.Fatal(err)
			}
			return cfg.Telemetry
		}},
		{name: "kubernetes rollback MCP", env: map[string]string{"KUBERNETES_MCP_MODE": "rollback", "DATABASE_URL": "postgres://rollback@database/rollback_guard"}, want: "kubernetes-mcp-rollback", service: func(t *testing.T) TelemetryConfig {
			cfg, err := LoadKubernetesMCP()
			if err != nil {
				t.Fatal(err)
			}
			return cfg.Telemetry
		}},
		{name: "observability MCP", want: "observability-mcp", service: func(t *testing.T) TelemetryConfig {
			cfg, err := LoadObservabilityMCP()
			if err != nil {
				t.Fatal(err)
			}
			return cfg.Telemetry
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearConfigEnvironment(t)
			for key, value := range test.env {
				t.Setenv(key, value)
			}
			got := test.service(t)
			want := TelemetryConfig{ServiceName: test.want, SampleRatio: 1, ShutdownTimeout: 5 * time.Second, LogLevel: slog.LevelInfo}
			if got != want {
				t.Fatalf("telemetry = %+v, want %+v (export disabled by default)", got, want)
			}
		})
	}
}

func TestTelemetrySettingsValidation(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr bool
		check   func(TelemetryConfig) bool
	}{
		{name: "otlp endpoint enables export", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://otel-collector:4318"}, check: func(c TelemetryConfig) bool { return c.OTLPEndpoint == "http://otel-collector:4318" }},
		{name: "https endpoint", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector.example/otlp"}, check: func(c TelemetryConfig) bool { return c.OTLPEndpoint == "https://collector.example/otlp" }},
		{name: "service name override", env: map[string]string{"OTEL_SERVICE_NAME": "observability-mcp-2"}, check: func(c TelemetryConfig) bool { return c.ServiceName == "observability-mcp-2" }},
		{name: "sample ratio", env: map[string]string{"OTEL_TRACES_SAMPLE_RATIO": "0.25"}, check: func(c TelemetryConfig) bool { return c.SampleRatio == 0.25 }},
		{name: "zero sample ratio", env: map[string]string{"OTEL_TRACES_SAMPLE_RATIO": "0"}, check: func(c TelemetryConfig) bool { return c.SampleRatio == 0 }},
		{name: "debug logs", env: map[string]string{"LOG_LEVEL": "debug"}, check: func(c TelemetryConfig) bool { return c.LogLevel == slog.LevelDebug }},
		{name: "shutdown timeout", env: map[string]string{"TELEMETRY_SHUTDOWN_TIMEOUT": "2s"}, check: func(c TelemetryConfig) bool { return c.ShutdownTimeout == 2*time.Second }},
		{name: "endpoint without scheme", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "otel-collector:4318"}, wantErr: true},
		{name: "endpoint with credentials", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://user:secret@collector:4318"}, wantErr: true},
		{name: "endpoint with query", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318?token=x"}, wantErr: true},
		{name: "grpc scheme", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "grpc://collector:4317"}, wantErr: true},
		{name: "ratio above one", env: map[string]string{"OTEL_TRACES_SAMPLE_RATIO": "1.5"}, wantErr: true},
		{name: "negative ratio", env: map[string]string{"OTEL_TRACES_SAMPLE_RATIO": "-0.1"}, wantErr: true},
		{name: "ratio not a number", env: map[string]string{"OTEL_TRACES_SAMPLE_RATIO": "half"}, wantErr: true},
		{name: "shutdown timeout too long", env: map[string]string{"TELEMETRY_SHUTDOWN_TIMEOUT": "31s"}, wantErr: true},
		{name: "shutdown timeout invalid", env: map[string]string{"TELEMETRY_SHUTDOWN_TIMEOUT": "soon"}, wantErr: true},
		{name: "unknown log level", env: map[string]string{"LOG_LEVEL": "verbose"}, wantErr: true},
		{name: "offset log level", env: map[string]string{"LOG_LEVEL": "INFO+2"}, wantErr: true},
		{name: "endpoint with fragment", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318#x"}, wantErr: true},
		{name: "endpoint without host", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearConfigEnvironment(t)
			for key, value := range test.env {
				t.Setenv(key, value)
			}
			cfg, err := LoadObservabilityMCP()
			if test.wantErr {
				if err == nil {
					t.Fatalf("LoadObservabilityMCP() = %+v, want error", cfg.Telemetry)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !test.check(cfg.Telemetry) {
				t.Fatalf("telemetry = %+v", cfg.Telemetry)
			}
		})
	}
}

func TestTelemetryEndpointErrorDoesNotEchoCredentials(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://user:supersecret@collector:4318")
	_, err := LoadObservabilityMCP()
	if err == nil {
		t.Fatal("expected error")
	}
	if got := err.Error(); strings.Contains(got, "supersecret") || strings.Contains(got, "user:") {
		t.Fatalf("error leaks endpoint credentials: %q", got)
	}
}
