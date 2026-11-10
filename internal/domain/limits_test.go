package domain

import (
	"testing"
	"time"
)

func TestValidMetricsWindow(t *testing.T) {
	tests := []struct {
		window time.Duration
		want   bool
	}{
		{window: 0, want: false},
		{window: 30*time.Second - time.Nanosecond, want: false},
		{window: 30 * time.Second, want: true},
		{window: 5 * time.Minute, want: true},
		{window: time.Hour, want: true},
		{window: time.Hour + time.Nanosecond, want: false},
		{window: -time.Minute, want: false},
	}
	for _, test := range tests {
		if got := ValidMetricsWindow(test.window); got != test.want {
			t.Errorf("ValidMetricsWindow(%s) = %t, want %t", test.window, got, test.want)
		}
	}
}

func TestValidRolloutWatch(t *testing.T) {
	tests := []struct {
		timeout time.Duration
		want    bool
	}{
		{timeout: 0, want: false},
		{timeout: time.Second - time.Nanosecond, want: false},
		{timeout: time.Second, want: true},
		{timeout: 90 * time.Second, want: true},
		{timeout: 2 * time.Minute, want: true},
		{timeout: 2*time.Minute + time.Nanosecond, want: false},
		{timeout: -time.Second, want: false},
	}
	for _, test := range tests {
		if got := ValidRolloutWatch(test.timeout); got != test.want {
			t.Errorf("ValidRolloutWatch(%s) = %t, want %t", test.timeout, got, test.want)
		}
	}
}
