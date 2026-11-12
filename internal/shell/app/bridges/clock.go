// System clock bridge.
//
// The same value satisfies every capability clock port: authoritative time,
// delayed wake-up and cancellable sleep. Production uses the real system clock;
// tests inject their own deterministic clock through the consumer's port.
package bridges

import (
	"context"
	"time"
)

// SystemClock is the production time source. Its zero value is ready to use.
type SystemClock struct{}

// NewSystemClock returns the production system clock.
func NewSystemClock() SystemClock {
	return SystemClock{}
}

// Now returns the current system time. time.Now carries the monotonic reading
// where the platform provides one, so elapsed-time comparisons are not affected
// by wall-clock adjustments.
func (SystemClock) Now() time.Time {
	return time.Now()
}

// After returns a channel that receives the current time after duration.
func (SystemClock) After(duration time.Duration) <-chan time.Time {
	return time.After(duration)
}

// Sleep waits for duration or until the context ends. A canceled or expired
// context returns its error instead of waiting for the timer.
func (SystemClock) Sleep(ctx context.Context, duration time.Duration) error {
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
