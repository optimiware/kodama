//go:build e2e

package harness

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
)

// inspectResult builds one valid inspection fixture.
func inspectResult(state, cleanup string, actions ...api.ExecutionAction) api.ExecutionInspectResult {
	return api.ExecutionInspectResult{
		ExecutionID:   "exec-1",
		State:         state,
		Version:       1,
		CurrentAction: -1,
		Actions:       actions,
		Cleanup:       api.ExecutionCleanup{State: cleanup},
	}
}

// TestHarnessPollStopsOnTerminal proves the injected clock reaches a terminal
// observation without sleeping and records every observation in order.
func TestHarnessPollStopsOnTerminal(t *testing.T) {
	clock := NewFakeClock(time.Date(2026, time.November, 8, 1, 2, 3, 0, time.UTC))
	states := []string{"queued", "running", "succeeded"}
	fetches := 0
	fetch := func(context.Context) (api.ExecutionInspectResult, error) {
		state := states[fetches]
		fetches++
		return inspectResult(state, "none"), nil
	}
	final, observations, err := Poll(context.Background(), clock, PollOptions{Interval: time.Second, Timeout: 30 * time.Second}, fetch)
	if err != nil {
		t.Fatalf("poll failed: %v", err)
	}
	if final.State != "succeeded" {
		t.Fatalf("final state = %q; want succeeded", final.State)
	}
	if fetches != 3 {
		t.Fatalf("fetch count = %d; want 3", fetches)
	}
	if len(observations) != 3 {
		t.Fatalf("observation count = %d; want 3", len(observations))
	}
	if elapsed := clock.Now().Sub(time.Date(2026, time.November, 8, 1, 2, 3, 0, time.UTC)); elapsed != 2*time.Second {
		t.Fatalf("fake clock advanced %s; want 2s of polling", elapsed)
	}
}

// TestHarnessPollTimesOut proves a non-terminal observation ends with
// ErrPollTimeout after the bounded window, returning the last observation.
func TestHarnessPollTimesOut(t *testing.T) {
	clock := NewFakeClock(time.Date(2026, time.November, 8, 1, 2, 3, 0, time.UTC))
	fetches := 0
	fetch := func(context.Context) (api.ExecutionInspectResult, error) {
		fetches++
		return inspectResult("running", "none"), nil
	}
	final, observations, err := Poll(context.Background(), clock, PollOptions{Interval: time.Second, Timeout: 3 * time.Second}, fetch)
	if !errors.Is(err, ErrPollTimeout) {
		t.Fatalf("poll error = %v; want ErrPollTimeout", err)
	}
	if final.State != "running" {
		t.Fatalf("last state = %q; want running", final.State)
	}
	if len(observations) == 0 {
		t.Fatal("timed-out poll recorded no observation")
	}
	if fetches > 5 {
		t.Fatalf("timed-out poll fetched %d times; the window was not bounded", fetches)
	}
}

// TestHarnessPollCustomStop proves a caller predicate stops before terminal,
// which is how the live suite waits for cleanup=complete.
func TestHarnessPollCustomStop(t *testing.T) {
	clock := NewFakeClock(time.Date(2026, time.November, 8, 1, 2, 3, 0, time.UTC))
	cleanups := []string{"none", "pending", "complete"}
	fetches := 0
	fetch := func(context.Context) (api.ExecutionInspectResult, error) {
		cleanup := cleanups[fetches]
		fetches++
		return inspectResult("succeeded", cleanup), nil
	}
	final, _, err := Poll(context.Background(), clock, PollOptions{
		Interval: time.Millisecond,
		Timeout:  time.Second,
		Stop: func(result api.ExecutionInspectResult) bool {
			return IsTerminalState(result.State) && result.Cleanup.State == "complete"
		},
	}, fetch)
	if err != nil {
		t.Fatalf("poll failed: %v", err)
	}
	if final.Cleanup.State != "complete" {
		t.Fatalf("final cleanup = %q; want complete", final.Cleanup.State)
	}
	if fetches != 3 {
		t.Fatalf("fetch count = %d; want 3", fetches)
	}
}

// TestHarnessPollHonoursContext proves cancellation stops the loop
// immediately.
func TestHarnessPollHonoursContext(t *testing.T) {
	clock := NewFakeClock(time.Date(2026, time.November, 8, 1, 2, 3, 0, time.UTC))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fetch := func(context.Context) (api.ExecutionInspectResult, error) {
		t.Fatal("fetch ran after cancellation")
		return api.ExecutionInspectResult{}, nil
	}
	if _, _, err := Poll(ctx, clock, PollOptions{}, fetch); !errors.Is(err, context.Canceled) {
		t.Fatalf("poll error = %v; want context.Canceled", err)
	}
}
