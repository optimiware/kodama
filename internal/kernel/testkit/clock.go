package testkit

import (
	"sync"
	"time"
)

// Clock is a deterministic engine.Clock. Time only moves when a test calls
// Advance or Set, so deadlines and transaction times never depend on the wall
// clock. Every access is guarded, so a concurrent schedule observes one
// consistent time value.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock returns a clock frozen at base.
func NewClock(base time.Time) *Clock {
	return &Clock{now: base}
}

// Now returns the current fake time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock by delta and returns the new time. A negative delta
// is accepted so a test can reproduce clock skew explicitly.
func (c *Clock) Advance(delta time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(delta)
	return c.now
}

// Set replaces the fake time and returns it.
func (c *Clock) Set(at time.Time) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = at
	return c.now
}
