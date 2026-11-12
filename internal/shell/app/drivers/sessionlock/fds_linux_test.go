//go:build linux

package sessionlock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// countOpenFileDescriptors returns the number of entries in /proc/self/fd.
func countOpenFileDescriptors(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("ReadDir(/proc/self/fd): %v", err)
	}
	return len(entries)
}

// TestAcquireDoesNotLeakFileDescriptors pins that a successful acquire owns
// exactly one descriptor, release closes it, and no failed or canceled attempt
// leaks a descriptor.
func TestAcquireDoesNotLeakFileDescriptors(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "session.json")
	baseline := countOpenFileDescriptors(t)

	lock := newLock(t, sessionPath, Options{})
	release, err := lock.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got := countOpenFileDescriptors(t); got != baseline+1 {
		release()
		t.Fatalf("descriptors while held = %d; want %d", got, baseline+1)
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if got := countOpenFileDescriptors(t); got != baseline {
		t.Fatalf("descriptors after release = %d; want %d", got, baseline)
	}

	holder := newLock(t, sessionPath, Options{})
	holderRelease, err := holder.Acquire(context.Background())
	if err != nil {
		t.Fatalf("holder Acquire: %v", err)
	}
	defer holderRelease()
	held := countOpenFileDescriptors(t)

	ctx, cancel := context.WithCancel(context.Background())
	waiter := newLock(t, sessionPath, Options{
		Sleep: func(ctx context.Context, _ time.Duration) error {
			cancel()
			<-ctx.Done()
			return ctx.Err()
		},
	})
	if _, err := waiter.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Acquire error = %v; want context.Canceled", err)
	}
	if got := countOpenFileDescriptors(t); got != held {
		t.Fatalf("descriptors after canceled wait = %d; want %d", got, held)
	}

	symlinkDir := t.TempDir()
	victim := filepath.Join(symlinkDir, "victim")
	if err := os.WriteFile(victim, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Symlink(victim, filepath.Join(symlinkDir, ".session.lock")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	symlinkLock := newLock(t, filepath.Join(symlinkDir, "session.json"), Options{})
	if _, err := symlinkLock.Acquire(context.Background()); err == nil {
		t.Fatal("Acquire followed the symlink lock file")
	}
	if got := countOpenFileDescriptors(t); got != held {
		t.Fatalf("descriptors after symlink refusal = %d; want %d", got, held)
	}

	if err := holderRelease(); err != nil {
		t.Fatalf("holder release: %v", err)
	}
	if got := countOpenFileDescriptors(t); got != baseline {
		t.Fatalf("descriptors after all releases = %d; want %d", got, baseline)
	}
}
