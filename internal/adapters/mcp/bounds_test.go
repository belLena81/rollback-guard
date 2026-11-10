package mcp

import (
	"math"
	"testing"
)

func TestValidWindowSeconds(t *testing.T) {
	// 1<<55 + 30 seconds overflows time.Duration to exactly 30s.
	for seconds, want := range map[int]bool{-1: false, 0: false, 29: false, 30: true, 300: true, 3600: true, 3601: false, 1<<55 + 30: false, math.MaxInt: false} {
		if got := ValidWindowSeconds(seconds); got != want {
			t.Errorf("ValidWindowSeconds(%d) = %t, want %t", seconds, got, want)
		}
	}
}

func TestValidWatchSeconds(t *testing.T) {
	// 1<<55 + 1 seconds overflows time.Duration to exactly 1s.
	for seconds, want := range map[int]bool{-1: false, 0: false, 1: true, 90: true, 120: true, 121: false, 1<<55 + 1: false, math.MaxInt: false} {
		if got := ValidWatchSeconds(seconds); got != want {
			t.Errorf("ValidWatchSeconds(%d) = %t, want %t", seconds, got, want)
		}
	}
}
