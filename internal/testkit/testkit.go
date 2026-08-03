package testkit

import (
	"fmt"
	"sync"
	"time"
)

type Clock struct {
	mu  sync.RWMutex
	now time.Time
}

func NewClock(now time.Time) *Clock {
	return &Clock{now: now.UTC()}
}

func (c *Clock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.now
}

func (c *Clock) Advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(duration)
}

type IDGenerator struct {
	mu   sync.Mutex
	next uint64
}

func (g *IDGenerator) NewID(prefix string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.next++
	return fmt.Sprintf("%s-%d", prefix, g.next)
}
