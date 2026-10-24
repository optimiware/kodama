package podman

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Test_workload_lock: the operations of one workload exclude each other across
// every Runtime sharing a state directory, a waiter gives up with its context,
// and other workloads are independent. No container engine is involved.
func Test_workload_lock(t *testing.T) {
	dir := t.TempDir()
	newRT := func() *Runtime {
		r, err := New(Config{Program: DeclaredCommand{}, StateDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	a, b := newRT(), newRT()
	ctx := context.Background()
	const name = "kdm-one"

	unlock, err := a.lock(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	waiting, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := b.lock(waiting, name); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("another runtime over the same state took the held lock: %v", err)
	}
	if other, err := b.lock(ctx, "kdm-two"); err != nil {
		t.Fatalf("an unrelated workload was blocked: %v", err)
	} else {
		other()
	}
	unlock()

	again, err := b.lock(ctx, name)
	if err != nil {
		t.Fatalf("the released lock was not available: %v", err)
	}
	again()
	if again, err := a.lock(ctx, name); err != nil {
		t.Fatalf("a waiter that gave up left the lock held: %v", err)
	} else {
		again()
	}
}
