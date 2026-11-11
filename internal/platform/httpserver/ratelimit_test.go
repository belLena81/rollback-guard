package httpserver

import (
	"testing"
	"time"
)

type fakeNow struct{ t time.Time }

func (f *fakeNow) now() time.Time { return f.t }

type rateStep struct {
	advance   time.Duration
	key       string
	wantAllow bool
}

func newTestLimiter(t *testing.T, clock *fakeNow, perMinute, burst, maxClients int) *RateLimiter {
	t.Helper()
	l, err := NewRateLimiter(RateLimitConfig{PerMinute: perMinute, Burst: burst, MaxClients: maxClients, Now: clock.now})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestRateLimiterTokenBucket(t *testing.T) {
	tests := []struct {
		name      string
		perMinute int
		burst     int
		steps     []rateStep
	}{
		{"burst then deny then refill", 60, 2, []rateStep{
			{0, "a", true}, {0, "a", true}, {0, "a", false},
			{500 * time.Millisecond, "a", false},
			{500 * time.Millisecond, "a", true},
			{0, "a", false},
		}},
		{"clients are independent", 60, 1, []rateStep{
			{0, "a", true}, {0, "a", false}, {0, "b", true}, {0, "b", false},
		}},
		{"refill is capped at burst", 60, 2, []rateStep{
			{0, "a", true}, {time.Hour, "a", true}, {0, "a", true}, {0, "a", false},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := &fakeNow{t: time.Unix(1_000_000, 0)}
			l := newTestLimiter(t, clock, tt.perMinute, tt.burst, 100)
			for i, s := range tt.steps {
				clock.t = clock.t.Add(s.advance)
				allowed, retry := l.Allow(s.key)
				if allowed != s.wantAllow {
					t.Fatalf("step %d: allowed=%v, want %v", i, allowed, s.wantAllow)
				}
				if !allowed && retry <= 0 {
					t.Fatalf("step %d: denied without a positive retry-after (%v)", i, retry)
				}
			}
		})
	}
}

func TestRateLimiterMemoryIsBounded(t *testing.T) {
	clock := &fakeNow{t: time.Unix(1_000_000, 0)}
	l := newTestLimiter(t, clock, 60, 1, 3)
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		l.Allow(k)
		clock.t = clock.t.Add(time.Millisecond)
	}
	if got := l.size(); got > 3 {
		t.Fatalf("tracked clients = %d, want at most 3", got)
	}
}

func TestRateLimiterEvictsIdleClients(t *testing.T) {
	clock := &fakeNow{t: time.Unix(1_000_000, 0)}
	l := newTestLimiter(t, clock, 60, 2, 1000)
	l.Allow("a")
	l.Allow("b")
	clock.t = clock.t.Add(time.Hour)
	l.Allow("c") // triggers the idle sweep
	if got := l.size(); got != 1 {
		t.Fatalf("tracked clients after idle sweep = %d, want 1", got)
	}
}

func TestNewRateLimiterValidates(t *testing.T) {
	now := func() time.Time { return time.Unix(0, 0) }
	for name, cfg := range map[string]RateLimitConfig{
		"zero rate":  {PerMinute: 0, Burst: 1, MaxClients: 1, Now: now},
		"zero burst": {PerMinute: 1, Burst: 0, MaxClients: 1, Now: now},
		"no clients": {PerMinute: 1, Burst: 1, MaxClients: 0, Now: now},
		"no clock":   {PerMinute: 1, Burst: 1, MaxClients: 1},
	} {
		if _, err := NewRateLimiter(cfg); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestClientKey(t *testing.T) {
	tests := map[string]string{
		"192.0.2.1:5555":            "192.0.2.1",
		"[2001:db8:1:2:3:4:5:6]:80": "2001:db8:1:2::/64",
		"[2001:db8:1:2:ffff::1]:80": "2001:db8:1:2::/64",
		"garbage":                   "garbage",
	}
	for in, want := range tests {
		if got := ClientKey(in); got != want {
			t.Errorf("ClientKey(%q) = %q, want %q", in, got, want)
		}
	}
}
