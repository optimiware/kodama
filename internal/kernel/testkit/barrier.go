package testkit

import (
	"context"
	"sync"
)

// Barrier is an explicit, release-once rendezvous used to stop a fake effect at
// a named point. A test waits on Reached, inspects the fake state, then either
// releases the effect or cancels its context. No sleeps are involved.
//
// Release is idempotent and may happen before any Wait: every current and
// future waiter is released. Reached closes when the first waiter arrives, so
// Reached-before-Release orders the test after the fake reached the effect
// boundary.
type Barrier struct {
	reached     chan struct{}
	released    chan struct{}
	reachOnce   sync.Once
	releaseOnce sync.Once
}

// NewBarrier returns an unreleased barrier.
func NewBarrier() *Barrier {
	return &Barrier{
		reached:  make(chan struct{}),
		released: make(chan struct{}),
	}
}

// Reached returns a channel closed once at least one waiter has arrived.
func (b *Barrier) Reached() <-chan struct{} {
	return b.reached
}

// Release unblocks every waiter, now or in the future. It is safe to call more
// than once.
func (b *Barrier) Release() {
	b.releaseOnce.Do(func() {
		close(b.released)
	})
}

// Wait blocks until the barrier is released or ctx ends. A canceled context
// returns ctx.Err() without waiting for Release.
func (b *Barrier) Wait(ctx context.Context) error {
	b.reachOnce.Do(func() {
		close(b.reached)
	})
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-b.released:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
