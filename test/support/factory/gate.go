package factory

// Test-owned runtime effect gate.
//
// Some races cannot be forced from outside the composition: the runtime stop of
// a cancelled workload happens inside the composition's real Libpod client, and
// the test must decide the exact moment it completes so the stop-confirmation
// commit and the competing lifecycle commit can be queued on the same database
// lock. StopGate is the harness's Libpod seam for that: when armed, the next
// StopContainer call publishes that it was reached and blocks until the test
// releases it; every later call passes through unchanged.

import (
	"context"
	"sync"

	runtimepodman "github.com/optimiware/kodama/internal/shell/runtime/podman"
)

// StopGate gates exactly one composition Libpod StopContainer call per Arm.
// The zero value is not usable; construct one with NewStopGate.
type StopGate struct {
	mu       sync.Mutex
	armed    bool
	fired    bool
	released bool
	reached  chan struct{}
	release  chan struct{}
}

// NewStopGate returns one unarmed stop gate.
func NewStopGate() *StopGate { return &StopGate{} }

// Arm makes the next StopContainer call block. It is safe to call between race
// subtests when no stop is in flight.
func (gate *StopGate) Arm() {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	gate.armed = true
	gate.fired = false
	gate.released = false
	gate.reached = make(chan struct{})
	gate.release = make(chan struct{})
}

// Reached reports that the armed StopContainer call has been entered. A gate
// that is not armed returns nil, which a select never reports ready.
func (gate *StopGate) Reached() <-chan struct{} {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return gate.reached
}

// Release lets the armed StopContainer call proceed. It is idempotent, so test
// cleanup may call it after an explicit release and before any Arm.
func (gate *StopGate) Release() {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.release == nil || gate.released {
		return
	}
	gate.released = true
	close(gate.release)
}

// wait blocks the armed call until Release or context cancellation. It reports
// that it gated the call so the wrapper can decide whether to wait; exactly one
// call is gated per Arm.
func (gate *StopGate) wait(ctx context.Context) error {
	gate.mu.Lock()
	if !gate.armed || gate.fired {
		gate.mu.Unlock()
		return nil
	}
	gate.fired = true
	reached := gate.reached
	release := gate.release
	close(reached)
	gate.mu.Unlock()

	select {
	case <-release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// gatedStopLibpod is the composition's Libpod client with the test-owned stop
// gate in front of StopContainer. Every other effect passes through.
type gatedStopLibpod struct {
	runtimepodman.Libpod
	gate *StopGate
}

var _ runtimepodman.Libpod = gatedStopLibpod{}

// StopContainer waits for the test release when the gate is armed, then issues
// the real stop. A cancelled context releases the wait with its error.
func (client gatedStopLibpod) StopContainer(ctx context.Context, name string) error {
	if err := client.gate.wait(ctx); err != nil {
		return err
	}
	return client.Libpod.StopContainer(ctx, name)
}
