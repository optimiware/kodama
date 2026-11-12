// Package sessionlock implements the Userland-declared cli.SessionLock
// cross-process lock over an exclusive operating-system file lock.
//
// kodactl serializes reload -> refresh -> ReplaceFile and logout through this
// lock so two processes cannot both rotate one rotating refresh token. The
// lock is a real flock: it is shared by every process that names the same
// session file, and the operating system releases it when the holding process
// exits, including on an uncatchable kill. Unsupported platforms report
// ErrUnsupportedPlatform instead of silently degrading to a process-local lock.
package sessionlock

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/userland/cli"
)

// DefaultPollInterval is the default delay between non-blocking lock attempts
// before jitter is applied.
const DefaultPollInterval = 50 * time.Millisecond

// lockFileName is the fixed name of the lock file next to the session file.
const lockFileName = ".session.lock"

var (
	// ErrInvalidPath reports a session path that is not an absolute clean path
	// with a real session directory.
	ErrInvalidPath = errors.New("session lock: session path must be absolute, clean and free of '..'")
	// ErrUnsupportedPlatform reports a platform without a supported exclusive
	// file lock.
	ErrUnsupportedPlatform = errors.New("session lock: exclusive cross-process file locking is unsupported on this platform")
	// ErrLockFileNotRegular reports a lock path that is not a regular file,
	// such as a symlink, directory, device or named pipe.
	ErrLockFileNotRegular = errors.New("session lock: lock file is not a regular file")
)

// Options injects the retry boundary. The zero value uses DefaultPollInterval
// and a context-aware production sleeper.
type Options struct {
	// PollInterval is the base delay between lock attempts. A non-positive
	// value uses DefaultPollInterval.
	PollInterval time.Duration
	// Sleep waits for the next attempt and must return the context error when
	// ctx is cancelled. A nil value uses the production sleeper.
	Sleep func(ctx context.Context, duration time.Duration) error
}

// SessionLock is the real cross-process cli.SessionLock implementation.
type SessionLock struct {
	dir          string
	lockPath     string
	pollInterval time.Duration
	sleep        func(ctx context.Context, duration time.Duration) error
}

var _ cli.SessionLock = (*SessionLock)(nil)

// NewSessionLock builds the lock for one durable session file. The lock file
// is always <session directory>/.session.lock.
func NewSessionLock(path string) (*SessionLock, error) {
	return NewSessionLockWithOptions(path, Options{})
}

// NewSessionLockWithOptions builds the lock for one durable session file with
// an injected poll interval and sleeper.
func NewSessionLockWithOptions(path string, options Options) (*SessionLock, error) {
	if !platformSupported() {
		return nil, ErrUnsupportedPlatform
	}
	lockPath, err := lockPathForSession(path)
	if err != nil {
		return nil, err
	}
	interval := options.PollInterval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	sleeper := options.Sleep
	if sleeper == nil {
		sleeper = sleepContext
	}
	return &SessionLock{
		dir:          filepath.Dir(path),
		lockPath:     lockPath,
		pollInterval: interval,
		sleep:        sleeper,
	}, nil
}

// lockPathForSession validates one session file path and returns the lock
// file beside it.
func lockPathForSession(sessionPath string) (string, error) {
	if sessionPath == "" || !filepath.IsAbs(sessionPath) {
		return "", fmt.Errorf("%w: %q is not absolute", ErrInvalidPath, sessionPath)
	}
	if filepath.Clean(sessionPath) != sessionPath {
		return "", fmt.Errorf("%w: %q is not clean", ErrInvalidPath, sessionPath)
	}
	for _, component := range strings.Split(sessionPath, string(filepath.Separator)) {
		if component == ".." {
			return "", fmt.Errorf("%w: %q contains '..'", ErrInvalidPath, sessionPath)
		}
	}
	dir := filepath.Dir(sessionPath)
	if dir == sessionPath {
		return "", fmt.Errorf("%w: %q has no session directory", ErrInvalidPath, sessionPath)
	}
	return filepath.Join(dir, lockFileName), nil
}

// Acquire takes the exclusive cross-process lock. It creates the session
// directory 0700 when missing (an existing directory keeps its mode), opens the
// lock file with O_NOFOLLOW at 0600, and retries a non-blocking flock until it
// succeeds, ctx is cancelled or an error occurs. The returned release function
// unlocks and closes the file exactly once and is safe to call more than once.
func (lock *SessionLock) Acquire(ctx context.Context) (func() error, error) {
	if ctx == nil {
		return nil, errors.New("session lock: nil context")
	}
	if !platformSupported() {
		return nil, ErrUnsupportedPlatform
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(lock.dir, 0o700); err != nil {
		return nil, fmt.Errorf("session lock: create session directory %s: %w", lock.dir, err)
	}
	file, err := openLockFile(lock.lockPath)
	if err != nil {
		return nil, fmt.Errorf("session lock: open %s: %w", lock.lockPath, err)
	}
	held := false
	defer func() {
		if !held {
			_ = file.Close()
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("session lock: stat %s: %w", lock.lockPath, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s", ErrLockFileNotRegular, lock.lockPath)
	}
	if info.Mode().Perm() != 0o600 {
		if err := file.Chmod(0o600); err != nil {
			return nil, fmt.Errorf("session lock: set %s mode 0600: %w", lock.lockPath, err)
		}
	}
	for {
		locked, err := tryExclusiveLock(file)
		if err != nil {
			return nil, fmt.Errorf("session lock: lock %s: %w", lock.lockPath, err)
		}
		if locked {
			break
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := lock.sleep(ctx, lock.interval()); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, fmt.Errorf("session lock: wait for %s: %w", lock.lockPath, err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	held = true
	return releaseOnce(file), nil
}

// interval returns the next poll delay with half-to-full jitter so competing
// processes do not resynchronize on every attempt.
func (lock *SessionLock) interval() time.Duration {
	base := lock.pollInterval
	if base <= 0 {
		base = DefaultPollInterval
	}
	half := base / 2
	if half <= 0 {
		return base
	}
	return half + rand.N(half+1)
}

// releaseOnce returns the idempotent release function for one held file.
func releaseOnce(file *os.File) func() error {
	var once sync.Once
	var releaseErr error
	return func() error {
		once.Do(func() {
			if err := unlockFile(file); err != nil {
				releaseErr = fmt.Errorf("session lock: release: %w", err)
			}
			if err := file.Close(); err != nil && releaseErr == nil {
				releaseErr = fmt.Errorf("session lock: close lock file: %w", err)
			}
		})
		return releaseErr
	}
}

// sleepContext is the production sleeper: it never blocks past ctx.
func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
