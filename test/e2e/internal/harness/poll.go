//go:build e2e

package harness

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/optimiware/kodama/api"
)

// Default poll bounds for live execution observation.
const (
	DefaultPollInterval = 250 * time.Millisecond
	DefaultPollTimeout  = 5 * time.Minute
)

// ErrPollTimeout reports that the observation did not reach its stop condition
// inside the bounded poll window.
var ErrPollTimeout = errors.New("poll timed out")

// Clock is the injectable time source of the poll loop.
type Clock interface {
	Now() time.Time
	Sleep(time.Duration)
}

// RealClock is the production clock.
type RealClock struct{}

// Now returns the current local time.
func (RealClock) Now() time.Time { return time.Now() }

// Sleep waits for the requested duration.
func (RealClock) Sleep(duration time.Duration) { time.Sleep(duration) }

// FakeClock advances only when the poll loop sleeps or the test moves it, so
// hermetic poll tests never wait.
type FakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewFakeClock starts a deterministic clock at the given instant.
func NewFakeClock(at time.Time) *FakeClock { return &FakeClock{now: at} }

// Now returns the fake instant.
func (clock *FakeClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

// Sleep advances the fake instant.
func (clock *FakeClock) Sleep(duration time.Duration) { clock.Advance(duration) }

// Advance moves the fake instant forward.
func (clock *FakeClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(duration)
}

// PollOptions bounds one observation loop. Stop defaults to a terminal
// execution state; a caller that needs cleanup settlement supplies its own.
type PollOptions struct {
	Interval time.Duration
	Timeout  time.Duration
	Stop     func(api.ExecutionInspectResult) bool
}

// Poll fetches execution.inspect until Stop is true, the context ends or the
// bounded window expires. It returns the stop observation, every observation in
// order, and an error. A timed-out poll returns the last observation with
// ErrPollTimeout.
func Poll(ctx context.Context, clock Clock, options PollOptions, fetch func(context.Context) (api.ExecutionInspectResult, error)) (api.ExecutionInspectResult, []api.ExecutionInspectResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if clock == nil {
		return api.ExecutionInspectResult{}, nil, fmt.Errorf("poll has no clock")
	}
	if fetch == nil {
		return api.ExecutionInspectResult{}, nil, fmt.Errorf("poll has no fetch function")
	}
	interval := options.Interval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = DefaultPollTimeout
	}
	stop := options.Stop
	if stop == nil {
		stop = func(result api.ExecutionInspectResult) bool { return IsTerminalState(result.State) }
	}
	deadline := clock.Now().Add(timeout)
	// The attempt bound keeps a non-advancing clock from spinning forever.
	maxAttempts := int(timeout/interval) + 2
	observations := make([]api.ExecutionInspectResult, 0, 16)
	var last api.ExecutionInspectResult
	for range maxAttempts {
		if err := ctx.Err(); err != nil {
			return last, observations, err
		}
		result, err := fetch(ctx)
		if err != nil {
			return last, observations, err
		}
		last = result
		observations = append(observations, result)
		if stop(result) {
			return last, observations, nil
		}
		if !clock.Now().Add(interval).Before(deadline) {
			return last, observations, fmt.Errorf("%w after %s", ErrPollTimeout, timeout)
		}
		clock.Sleep(interval)
	}
	return last, observations, fmt.Errorf("%w after %d attempts", ErrPollTimeout, maxAttempts)
}
