package config

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testVariable = "ROLLBACK_GUARD_TEST_VALUE"

func TestBoundedDurationEnv(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		lower   time.Duration
		upper   time.Duration
		want    time.Duration
		wantErr string
	}{
		{name: "unset uses the fallback", value: "", lower: time.Second, upper: time.Minute, want: 7 * time.Second},
		{name: "blank uses the fallback", value: "  ", lower: time.Second, upper: time.Minute, want: 7 * time.Second},
		{name: "inside the range", value: "30s", lower: time.Second, upper: time.Minute, want: 30 * time.Second},
		{name: "surrounding space", value: " 30s ", lower: time.Second, upper: time.Minute, want: 30 * time.Second},
		{name: "lower bound", value: "1s", lower: time.Second, upper: time.Minute, want: time.Second},
		{name: "upper bound", value: "1m", lower: time.Second, upper: time.Minute, want: time.Minute},
		{name: "below the lower bound", value: "999ms", lower: time.Second, upper: time.Minute, wantErr: testVariable + " must be between 1s and 1m, got 999ms"},
		{name: "above the upper bound", value: "61s", lower: time.Second, upper: time.Minute, wantErr: testVariable + " must be between 1s and 1m, got 1m1s"},
		{name: "positive only: smallest", value: "1ns", upper: 30 * time.Second, want: time.Nanosecond},
		{name: "positive only: zero", value: "0s", upper: 30 * time.Second, wantErr: testVariable + " must be positive and no greater than 30s, got 0s"},
		{name: "positive only: negative", value: "-5s", upper: 30 * time.Second, wantErr: testVariable + " must be positive and no greater than 30s, got -5s"},
		{name: "positive only: above", value: "31s", upper: 30 * time.Second, wantErr: testVariable + " must be positive and no greater than 30s, got 31s"},
		{name: "hour bound prints without zero units", value: "2h", lower: 30 * time.Second, upper: time.Hour, wantErr: testVariable + " must be between 30s and 1h, got 2h"},
		{name: "missing unit reports the parse error", value: "30", lower: time.Second, upper: time.Minute, wantErr: testVariable + ` must be between 1s and 1m: time: missing unit in duration "30"`},
		{name: "garbage reports the parse error", value: "soon", upper: 30 * time.Second, wantErr: testVariable + ` must be positive and no greater than 30s: time: invalid duration "soon"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(testVariable, test.value)
			got, err := boundedDurationEnv(testVariable, 7*time.Second, test.lower, test.upper)
			if test.wantErr != "" {
				if err == nil || err.Error() != test.wantErr {
					t.Fatalf("error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("boundedDurationEnv() = %s, %v; want %s", got, err, test.want)
			}
		})
	}
}

func TestBoundedIntEnv(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    int
		wantErr string
	}{
		{name: "unset uses the fallback", value: "", want: 512},
		{name: "lower bound", value: "64", want: 64},
		{name: "upper bound", value: "2048", want: 2048},
		{name: "below", value: "63", wantErr: testVariable + " must be between 64 and 2048, got 63"},
		{name: "above", value: "2049", wantErr: testVariable + " must be between 64 and 2048, got 2049"},
		{name: "not a number reports the parse error", value: "many", wantErr: testVariable + ` must be between 64 and 2048: strconv.Atoi: parsing "many": invalid syntax`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(testVariable, test.value)
			got, err := boundedIntEnv(testVariable, 512, 64, 2048)
			if test.wantErr != "" {
				if err == nil || err.Error() != test.wantErr {
					t.Fatalf("error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("boundedIntEnv() = %d, %v; want %d", got, err, test.want)
			}
		})
	}
}

func TestUnboundedHelpersReportTheRealCause(t *testing.T) {
	t.Run("duration parse error", func(t *testing.T) {
		t.Setenv(testVariable, "soon")
		_, err := durationEnv(testVariable, time.Second)
		if err == nil || err.Error() != testVariable+` must be a positive duration: time: invalid duration "soon"` {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("duration not positive", func(t *testing.T) {
		t.Setenv(testVariable, "-1s")
		_, err := durationEnv(testVariable, time.Second)
		if err == nil || err.Error() != testVariable+" must be a positive duration, got -1s" {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("int parse error keeps its cause", func(t *testing.T) {
		t.Setenv(testVariable, "four")
		_, err := intEnv(testVariable, 4)
		var numErr *strconv.NumError
		if !errors.As(err, &numErr) || !strings.HasPrefix(err.Error(), testVariable+" must be a positive integer: ") {
			t.Fatalf("error = %v, want a wrapped strconv error", err)
		}
	})
	t.Run("int not positive", func(t *testing.T) {
		t.Setenv(testVariable, "0")
		_, err := intEnv(testVariable, 4)
		if err == nil || err.Error() != testVariable+" must be a positive integer, got 0" {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("float parse error keeps its cause", func(t *testing.T) {
		t.Setenv(testVariable, "high")
		_, err := floatEnv(testVariable, 0.8)
		var numErr *strconv.NumError
		if !errors.As(err, &numErr) || !strings.HasPrefix(err.Error(), testVariable+" must be a float: ") {
			t.Fatalf("error = %v, want a wrapped strconv error", err)
		}
	})
	t.Run("fallbacks", func(t *testing.T) {
		t.Setenv(testVariable, "")
		duration, durationErr := durationEnv(testVariable, 3*time.Second)
		integer, intErr := intEnv(testVariable, 4)
		float, floatErr := floatEnv(testVariable, 0.8)
		if durationErr != nil || intErr != nil || floatErr != nil || duration != 3*time.Second || integer != 4 || float != 0.8 {
			t.Fatalf("fallbacks = %s %d %v (%v %v %v)", duration, integer, float, durationErr, intErr, floatErr)
		}
	})
}

func TestFormatDuration(t *testing.T) {
	for duration, want := range map[time.Duration]string{
		0:                       "0s",
		250 * time.Millisecond:  "250ms",
		30 * time.Second:        "30s",
		2 * time.Minute:         "2m",
		90 * time.Second:        "1m30s",
		time.Hour:               "1h",
		time.Hour + time.Minute: "1h1m",
		time.Hour + time.Second: "1h0m1s",
		10 * time.Minute:        "10m",
		-5 * time.Second:        "-5s",
	} {
		if got := formatDuration(duration); got != want {
			t.Errorf("formatDuration(%v) = %q, want %q", duration, got, want)
		}
	}
}

// Sub-loader errors name the variable, so a mis-set shared variable is
// reported the same way by every service that reads it.
func TestSharedVariablesFailTheSameWayInEveryLoader(t *testing.T) {
	loaders := map[string]func(*testing.T) error{
		"orchestrator": func(t *testing.T) error { _, err := Load(); return err },
		"investigator": func(t *testing.T) error { _, err := LoadInvestigator(); return err },
		"kubernetes MCP rollback": func(t *testing.T) error {
			t.Setenv("KUBERNETES_MCP_MODE", "rollback")
			_, err := LoadKubernetesMCP()
			return err
		},
	}
	tests := []struct {
		key, value, want string
		skip             string
	}{
		{key: "DB_MAX_CONNS", value: "many", want: `DB_MAX_CONNS must be a positive integer: strconv.Atoi: parsing "many": invalid syntax`},
		{key: "DB_MAX_CONNS", value: "0", want: "DB_MAX_CONNS must be a positive integer, got 0"},
		{key: "DB_PING_TIMEOUT", value: "fast", want: `DB_PING_TIMEOUT must be a positive duration: time: invalid duration "fast"`},
		{key: "MCP_CONNECT_TIMEOUT", value: "31s", want: "MCP_CONNECT_TIMEOUT must be positive and no greater than 30s, got 31s", skip: "kubernetes MCP rollback"},
		{key: "MCP_CALL_TIMEOUT", value: "1m", want: "MCP_CALL_TIMEOUT must be positive and no greater than 30s, got 1m", skip: "kubernetes MCP rollback"},
		{key: "A2A_TASK_TIMEOUT", value: "3m", want: "A2A_TASK_TIMEOUT must be positive and no greater than 2m, got 3m", skip: "kubernetes MCP rollback"},
		{key: "ALLOWED_TARGETS", value: "local/demo", want: `ALLOWED_TARGETS target "local/demo" must be cluster/namespace/deployment/service`, skip: "investigator"},
	}
	for _, test := range tests {
		for name, load := range loaders {
			if name == test.skip {
				continue
			}
			t.Run(test.key+"="+test.value+"/"+name, func(t *testing.T) {
				isolateEnvironment(t, map[string]string{"DATABASE_URL": testDatabaseURL, test.key: test.value})
				if err := load(t); err == nil || err.Error() != test.want {
					t.Fatalf("error = %v, want %q", err, test.want)
				}
			})
		}
	}
}
