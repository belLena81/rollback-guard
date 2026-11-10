package demoapi

import (
	"strings"
	"testing"
	"time"
)

func lookupFrom(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func TestLoadConfig(t *testing.T) {
	defaultLabels := TargetLabels{Cluster: "local", Namespace: "rollback-demo", Deployment: "demo-api", Service: "demo-api"}
	tests := []struct {
		name    string
		env     map[string]string
		want    Config
		wantErr string
	}{
		{
			name: "defaults to healthy",
			env:  map[string]string{},
			want: Config{HTTPAddr: ":8080", Mode: ModeHealthy, Version: "dev", Target: defaultLabels, WorkLatency: 10 * time.Millisecond},
		},
		{
			name: "error storm defaults to 80 percent errors",
			env:  map[string]string{"DEMO_MODE": "error-storm"},
			want: Config{HTTPAddr: ":8080", Mode: ModeErrorStorm, Version: "dev", Target: defaultLabels, WorkLatency: 10 * time.Millisecond, ErrorRate: 0.8},
		},
		{
			name: "error storm accepts explicit rate",
			env:  map[string]string{"DEMO_MODE": "error-storm", "DEMO_ERROR_RATE": "1"},
			want: Config{HTTPAddr: ":8080", Mode: ModeErrorStorm, Version: "dev", Target: defaultLabels, WorkLatency: 10 * time.Millisecond, ErrorRate: 1},
		},
		{
			name: "high latency defaults to 1500ms",
			env:  map[string]string{"DEMO_MODE": "high-latency"},
			want: Config{HTTPAddr: ":8080", Mode: ModeHighLatency, Version: "dev", Target: defaultLabels, WorkLatency: 1500 * time.Millisecond},
		},
		{
			name: "crashloop defaults to crash after 2s",
			env:  map[string]string{"DEMO_MODE": "crashloop"},
			want: Config{HTTPAddr: ":8080", Mode: ModeCrashLoop, Version: "dev", Target: defaultLabels, WorkLatency: 10 * time.Millisecond, CrashAfter: 2 * time.Second},
		},
		{
			name: "overrides address version labels and latency",
			env: map[string]string{
				"HTTP_ADDR": "127.0.0.1:9000", "DEMO_VERSION": "v1", "DEMO_WORK_LATENCY": "25ms",
				"DEMO_CLUSTER": "c1", "DEMO_NAMESPACE": "ns", "DEMO_DEPLOYMENT": "d", "DEMO_SERVICE": "s",
			},
			want: Config{HTTPAddr: "127.0.0.1:9000", Mode: ModeHealthy, Version: "v1", Target: TargetLabels{Cluster: "c1", Namespace: "ns", Deployment: "d", Service: "s"}, WorkLatency: 25 * time.Millisecond},
		},
		{name: "unknown mode", env: map[string]string{"DEMO_MODE": "flaky"}, wantErr: "DEMO_MODE"},
		{name: "error rate outside error storm", env: map[string]string{"DEMO_ERROR_RATE": "0.5"}, wantErr: "DEMO_ERROR_RATE"},
		{name: "error rate zero", env: map[string]string{"DEMO_MODE": "error-storm", "DEMO_ERROR_RATE": "0"}, wantErr: "DEMO_ERROR_RATE"},
		{name: "error rate above one", env: map[string]string{"DEMO_MODE": "error-storm", "DEMO_ERROR_RATE": "1.5"}, wantErr: "DEMO_ERROR_RATE"},
		{name: "error rate not a number", env: map[string]string{"DEMO_MODE": "error-storm", "DEMO_ERROR_RATE": "NaN"}, wantErr: "DEMO_ERROR_RATE"},
		{name: "negative latency", env: map[string]string{"DEMO_WORK_LATENCY": "-1s"}, wantErr: "DEMO_WORK_LATENCY"},
		{name: "latency too large", env: map[string]string{"DEMO_WORK_LATENCY": "31s"}, wantErr: "DEMO_WORK_LATENCY"},
		{name: "crash after outside crashloop", env: map[string]string{"DEMO_CRASH_AFTER": "1s"}, wantErr: "DEMO_CRASH_AFTER"},
		{name: "crash after too large", env: map[string]string{"DEMO_MODE": "crashloop", "DEMO_CRASH_AFTER": "2m"}, wantErr: "DEMO_CRASH_AFTER"},
		{name: "invalid label value", env: map[string]string{"DEMO_NAMESPACE": "bad\"ns"}, wantErr: "DEMO_NAMESPACE"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := LoadConfig(lookupFrom(test.env))
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("err = %v, want mention of %s", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("config = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestLoadConfigRequiresLookup(t *testing.T) {
	if _, err := LoadConfig(nil); err == nil {
		t.Fatal("expected error for nil lookup")
	}
}
