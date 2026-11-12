//go:build unix

package native

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// provisionLockPollInterval bounds how often a waiting acquisition retries the
// non-blocking flock while it observes the context.
const provisionLockPollInterval = 20 * time.Millisecond

// acquireProvisionLock takes the exclusive advisory lock on the tenant
// directory. It is context-aware: a cancelled context refuses without waiting
// and a context deadline bounds the wait. The returned release function is
// called exactly once.
//
// The lock is a regular file inside the tenant directory, opened with
// O_CLOEXEC semantics (Go opens files close-on-exec by default) and held with
// flock(2). The file is deliberately never removed: unlinking a lock while a
// waiter holds it open would let two processes believe they own the same
// tenant. It carries no provisioning state and a later attempt ignores it.
func acquireProvisionLock(ctx context.Context, tenantDirectory string) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := filepath.Join(tenantDirectory, provisioningLockName)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open tenant provisioning lock: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return nil, fmt.Errorf("set tenant provisioning lock mode: %w", err)
	}
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			file.Close()
			return nil, fmt.Errorf("acquire tenant provisioning lock: %w", err)
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-time.After(provisionLockPollInterval):
		}
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}
	return release, nil
}
