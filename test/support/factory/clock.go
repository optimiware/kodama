package factory

import (
	"context"
	"sync"
	"time"
)

// TestClock is the factory harness's adjustable authoritative clock.
//
// It is injected through app.Config.Adapters.Clock, so the Kernel engine, the
// request service, the retained log sink, the Podman adapter, the NATS broker
// and the dispatcher all read the same time. Now starts at the real current
// time in UTC; Advance moves it forward.
//
// Sleep deliberately waits REAL time for the requested duration (cancellable by
// the context) and is never woken by Advance. Composition loops use Sleep for
// their cadence, so a fake "advance" must not spin them; a deadline only becomes
// due on the next real cadence tick after Now moves past it.
type TestClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewTestClock returns an adjustable clock anchored at the real current UTC
// time.
func NewTestClock() *TestClock {
	return &TestClock{now: time.Now().UTC()}
}

// Now returns the current adjustable time.
func (clock *TestClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

// Advance moves the clock forward by duration. It never wakes a sleeper.
func (clock *TestClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(duration)
}

// Sleep waits for duration on a real timer or until the context ends. Advance
// does not complete it.
func (clock *TestClock) Sleep(ctx context.Context, duration time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
