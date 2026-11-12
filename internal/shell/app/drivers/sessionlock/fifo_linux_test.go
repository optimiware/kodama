//go:build linux

package sessionlock

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestAcquireRefusesNonRegularLockFile pins that a FIFO at the lock path is
// refused as a non-regular file rather than locked.
func TestAcquireRefusesNonRegularLockFile(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, ".session.lock")
	if err := syscall.Mkfifo(lockPath, 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}
	lock := newLock(t, filepath.Join(dir, "session.json"), Options{})
	release, err := lock.Acquire(context.Background())
	if err == nil {
		release()
		t.Fatal("Acquire accepted a named pipe at the lock path")
	}
	info, statErr := os.Lstat(lockPath)
	if statErr != nil {
		t.Fatalf("Lstat: %v", statErr)
	}
	if info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatal("Acquire replaced the named pipe")
	}
}
