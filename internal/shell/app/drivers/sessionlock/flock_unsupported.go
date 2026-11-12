//go:build !linux && !darwin

package sessionlock

import (
	"os"
)

// lockOpenFlags is only needed for compilation on unsupported platforms;
// NewSessionLock refuses before a lock file is opened.
const lockOpenFlags = os.O_RDWR | os.O_CREATE

// openLockFile exists only so the common code compiles on unsupported
// platforms.
func openLockFile(path string) (*os.File, error) {
	return os.OpenFile(path, lockOpenFlags, 0o600)
}

// tryExclusiveLock always reports the platform refusal.
func tryExclusiveLock(*os.File) (bool, error) {
	return false, ErrUnsupportedPlatform
}

// unlockFile always reports the platform refusal.
func unlockFile(*os.File) error {
	return ErrUnsupportedPlatform
}

// platformSupported reports that this platform has no supported lock.
func platformSupported() bool { return false }
