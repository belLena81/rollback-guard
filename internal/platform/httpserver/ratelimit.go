package httpserver

import (
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"
)

const (
	secondsPerMinute = 60
	// ipv6ClientPrefixBits groups IPv6 clients by /64, the smallest block a
	// single subscriber normally controls, so rotating addresses inside it
	// does not mint fresh buckets.
	ipv6ClientPrefixBits = 64
)

// RateLimitConfig configures a RateLimiter.
type RateLimitConfig struct {
	// PerMinute is the steady refill rate per client.
	PerMinute int
	// Burst is the bucket size: requests a quiet client may send at once.
	Burst int
	// MaxClients bounds the number of tracked clients (memory).
	MaxClients int
	// Now is the injected clock.
	Now func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// RateLimiter is an in-process per-client token bucket. It keeps no global
// state, uses only the standard library and tracks at most MaxClients keys:
// idle buckets (those that have refilled completely) are dropped first, and
// when the table is still full the least recently seen client is evicted.
// It is per process; behind several replicas the effective limit multiplies.
type RateLimiter struct {
	mu          sync.Mutex
	now         func() time.Time
	perSecond   float64
	burst       float64
	maxClients  int
	nextSweep   time.Time
	sweepPeriod time.Duration
	buckets     map[string]*bucket
}

// NewRateLimiter validates cfg and returns a limiter.
func NewRateLimiter(cfg RateLimitConfig) (*RateLimiter, error) {
	if cfg.PerMinute <= 0 || cfg.Burst <= 0 || cfg.MaxClients <= 0 || cfg.Now == nil {
		return nil, errors.New("rate limiter needs a positive rate, burst and client bound, and a clock")
	}
	return &RateLimiter{
		now:         cfg.Now,
		perSecond:   float64(cfg.PerMinute) / secondsPerMinute,
		burst:       float64(cfg.Burst),
		maxClients:  cfg.MaxClients,
		sweepPeriod: time.Minute,
		buckets:     make(map[string]*bucket),
	}, nil
}

// Allow takes one token from key's bucket. When it denies, retryAfter is how
// long until a token is available.
func (l *RateLimiter) Allow(key string) (allowed bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if !now.Before(l.nextSweep) {
		l.sweep(now)
		l.nextSweep = now.Add(l.sweepPeriod)
	}
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.maxClients {
			l.sweep(now)
		}
		if len(l.buckets) >= l.maxClients {
			l.evictOldest()
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = l.refilled(b, now)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / l.perSecond * float64(time.Second))
}

func (l *RateLimiter) refilled(b *bucket, now time.Time) float64 {
	elapsed := max(now.Sub(b.last).Seconds(), 0)
	return min(l.burst, b.tokens+elapsed*l.perSecond)
}

// sweep drops buckets that have refilled completely: forgetting them changes
// no decision.
func (l *RateLimiter) sweep(now time.Time) {
	for key, b := range l.buckets {
		if l.refilled(b, now) >= l.burst {
			delete(l.buckets, key)
		}
	}
}

func (l *RateLimiter) evictOldest() {
	var oldestKey string
	var oldest time.Time
	first := true
	for key, b := range l.buckets {
		if first || b.last.Before(oldest) {
			oldestKey, oldest, first = key, b.last, false
		}
	}
	delete(l.buckets, oldestKey)
}

func (l *RateLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// ClientKey derives the rate-limit key from an http.Request.RemoteAddr only.
// Forwarding headers are never consulted, because any client can forge them;
// behind a reverse proxy every request carries the proxy's address, so such a
// deployment must rate-limit at the proxy. An IPv6 address is reduced to its
// /64 so one host cannot rotate through its own prefix.
func ClientKey(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	addr = addr.Unmap()
	if addr.Is6() {
		if prefix, err := addr.Prefix(ipv6ClientPrefixBits); err == nil {
			return prefix.String()
		}
	}
	return addr.String()
}
