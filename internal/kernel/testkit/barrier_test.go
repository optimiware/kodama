package testkit

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestBarrierBlocksUntilReleased(t *testing.T) {
	barrier := NewBarrier()
	done := make(chan error, 1)
	go func() {
		done <- barrier.Wait(context.Background())
	}()

	<-barrier.Reached()

	select {
	case err := <-done:
		t.Fatalf("Wait returned before Release: %v", err)
	default:
	}

	barrier.Release()
	if err := <-done; err != nil {
		t.Fatalf("Wait after Release error = %v, want nil", err)
	}
}

func TestBarrierHonorsContextCancellation(t *testing.T) {
	barrier := NewBarrier()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- barrier.Wait(ctx)
	}()

	<-barrier.Reached()
	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait after cancel error = %v, want context.Canceled", err)
	}
	// Release keeps a mistakenly blocked peer from leaking into another test.
	barrier.Release()
}

func TestBarrierReleaseBeforeWait(t *testing.T) {
	barrier := NewBarrier()
	barrier.Release()

	done := make(chan error, 1)
	go func() {
		done <- barrier.Wait(context.Background())
	}()
	if err := <-done; err != nil {
		t.Fatalf("Wait after early Release error = %v, want nil", err)
	}
}

func TestBarrierReleasesEveryWaiter(t *testing.T) {
	barrier := NewBarrier()
	const waiters = 4

	var wait sync.WaitGroup
	wait.Add(waiters)
	errs := make(chan error, waiters)
	for range waiters {
		go func() {
			defer wait.Done()
			errs <- barrier.Wait(context.Background())
		}()
	}

	<-barrier.Reached()
	barrier.Release()
	wait.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("waiter error = %v, want nil", err)
		}
	}
}
