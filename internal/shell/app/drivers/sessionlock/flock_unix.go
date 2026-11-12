//go:build linux || darwin

package sessionlock

import (
	"os"
	"syscall"
)

// lockOpenFlags opens the lock file without following a symlink and without
// truncating an existing lock file.
const lockOpenFlags = os.O_RDWR | os.O_CREATE | syscall.O_NOFOLLOW

// openLockFile opens the lock file for the exclusive lock.
func openLockFile(path string) (*os.File, error) {
	return os.OpenFile(path, lockOpenFlags, 0o600)
}

// tryExclusiveLock performs one non-blocking exclusive flock attempt.
func tryExclusiveLock(file *os.File) (bool, error) {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
		return false, nil
	}
	return false, err
}

// unlockFile releases the exclusive flock before the descriptor is closed.
func unlockFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}

// platformSupported reports that this platform implements the lock.
func platformSupported() bool { return true }
