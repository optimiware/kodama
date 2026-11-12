//go:build linux || darwin

package sessionlock

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// newLock constructs one lock for a test.
func newLock(t *testing.T, sessionPath string, options Options) *SessionLock {
	t.Helper()
	lock, err := NewSessionLockWithOptions(sessionPath, options)
	if err != nil {
		t.Fatalf("NewSessionLockWithOptions(%q): %v", sessionPath, err)
	}
	return lock
}

// TestNewSessionLockValidatesSessionPath pins the constructor's path rules.
func TestNewSessionLockValidatesSessionPath(t *testing.T) {
	for _, path := range []string{
		"",
		"session.json",
		"relative/session.json",
		"/a/../b/session.json",
		"/a/./b/session.json",
		"/a//b/session.json",
		"/a/../session.json",
		"/",
	} {
		if _, err := NewSessionLock(path); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("NewSessionLock(%q) error = %v; want ErrInvalidPath", path, err)
		}
	}

	lock, err := NewSessionLock("/home/engineer/.config/kodama/session.json")
	if err != nil {
		t.Fatalf("NewSessionLock(valid): %v", err)
	}
	if got, want := lock.lockPath, "/home/engineer/.config/kodama/.session.lock"; got != want {
		t.Fatalf("lock path = %q; want %q", got, want)
	}
	if got, want := lock.dir, "/home/engineer/.config/kodama"; got != want {
		t.Fatalf("session directory = %q; want %q", got, want)
	}
}

// TestNewSessionLockRejectsNilContext pins the defensive context check.
func TestNewSessionLockRejectsNilContext(t *testing.T) {
	lock := newLock(t, filepath.Join(t.TempDir(), "session.json"), Options{})
	var nilContext context.Context
	release, err := lock.Acquire(nilContext)
	if err == nil || release != nil {
		t.Fatalf("Acquire(nil) returned release set=%v err=%v; want a refusal", release != nil, err)
	}
}

// TestAcquireCreatesPrivateLockFileAndSessionDirectory pins directory and file
// modes: missing session directories are created 0700, an existing directory
// keeps its mode, and the lock file is 0600.
func TestAcquireCreatesPrivateLockFileAndSessionDirectory(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "config")
	if err := os.Mkdir(existing, 0o750); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.Chmod(existing, 0o750); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	sessionPath := filepath.Join(existing, "kodama", "nested", "session.json")
	lock := newLock(t, sessionPath, Options{})
	release, err := lock.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer release()

	for _, dir := range []string{
		filepath.Join(existing, "kodama"),
		filepath.Join(existing, "kodama", "nested"),
	} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("Stat(%s): %v", dir, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", dir)
		}
		if info.Mode().Perm() != 0o700 {
			t.Errorf("created directory %s mode = %o; want 700", dir, info.Mode().Perm())
		}
	}
	info, err := os.Stat(existing)
	if err != nil {
		t.Fatalf("Stat(existing): %v", err)
	}
	if info.Mode().Perm() != 0o750 {
		t.Fatalf("existing directory mode = %o; want 750 untouched", info.Mode().Perm())
	}

	lockPath := filepath.Join(existing, "kodama", "nested", ".session.lock")
	info, err = os.Stat(lockPath)
	if err != nil {
		t.Fatalf("Stat(lock): %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("lock file is not regular: %v", info.Mode())
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("lock file mode = %o; want 600", info.Mode().Perm())
	}
}

// TestAcquireTightensExistingLockFileMode pins that an existing looser lock
// file is forced back to 0600 before it is used.
func TestAcquireTightensExistingLockFileMode(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "session.json")
	lockPath := filepath.Join(dir, ".session.lock")
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(lockPath, 0o644); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	release, err := newLock(t, sessionPath, Options{}).Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer release()
	info, err := os.Stat(lockPath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("lock file mode = %o; want 600", info.Mode().Perm())
	}
}

// TestAcquireIsMutuallyExclusiveInProcess pins that a second Acquire on a
// separate descriptor blocks until the first release, using an injected
// sleeper as a deterministic barrier instead of a timing sleep.
func TestAcquireIsMutuallyExclusiveInProcess(t *testing.T) {
	sessionPath := filepath.Join(t.TempDir(), "session.json")
	first := newLock(t, sessionPath, Options{})
	firstRelease, err := first.Acquire(context.Background())
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	conflicted := make(chan struct{})
	var conflictOnce sync.Once
	unblock := make(chan struct{})
	var unblockOnce sync.Once
	openSleeper := func() { unblockOnce.Do(func() { close(unblock) }) }
	defer openSleeper()

	second := newLock(t, sessionPath, Options{
		Sleep: func(ctx context.Context, _ time.Duration) error {
			conflictOnce.Do(func() { close(conflicted) })
			select {
			case <-unblock:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
	acquired := make(chan func() error, 1)
	failed := make(chan error, 1)
	go func() {
		release, err := second.Acquire(context.Background())
		if err != nil {
			failed <- err
			return
		}
		acquired <- release
	}()

	select {
	case <-conflicted:
	case err := <-failed:
		t.Fatalf("second Acquire failed before blocking: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("second Acquire never observed the held lock")
	}

	// The conflict signal proves the second Acquire is waiting, not holding.
	select {
	case release := <-acquired:
		release()
		t.Fatal("second Acquire succeeded while the first lock was held")
	default:
	}

	if err := firstRelease(); err != nil {
		t.Fatalf("first release: %v", err)
	}
	openSleeper()

	select {
	case release := <-acquired:
		if err := release(); err != nil {
			t.Fatalf("second release: %v", err)
		}
	case err := <-failed:
		t.Fatalf("second Acquire failed after release: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("second Acquire did not acquire after the first release")
	}
}

// TestAcquireCanceledWhileWaitingReturnsContextError pins cancellation while
// waiting: the ctx error is returned, no lock is stolen, and the held lock is
// still held afterwards.
func TestAcquireCanceledWhileWaitingReturnsContextError(t *testing.T) {
	sessionPath := filepath.Join(t.TempDir(), "session.json")
	holder := newLock(t, sessionPath, Options{})
	holderRelease, err := holder.Acquire(context.Background())
	if err != nil {
		t.Fatalf("holder Acquire: %v", err)
	}
	defer holderRelease()

	ctx, cancel := context.WithCancel(context.Background())
	waiter := newLock(t, sessionPath, Options{
		Sleep: func(ctx context.Context, _ time.Duration) error {
			cancel()
			<-ctx.Done()
			return ctx.Err()
		},
	})
	release, err := waiter.Acquire(ctx)
	if release != nil {
		release()
		t.Fatal("canceled Acquire returned a release function")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire error = %v; want context.Canceled", err)
	}

	// The holder must still own the lock after the canceled wait.
	conflict := make(chan struct{})
	var conflictOnce sync.Once
	probeCtx, probeCancel := context.WithCancel(context.Background())
	defer probeCancel()
	probe := newLock(t, sessionPath, Options{
		Sleep: func(ctx context.Context, _ time.Duration) error {
			conflictOnce.Do(func() { close(conflict) })
			<-ctx.Done()
			return ctx.Err()
		},
	})
	probeResult := make(chan error, 1)
	go func() {
		_, err := probe.Acquire(probeCtx)
		probeResult <- err
	}()
	select {
	case <-conflict:
		probeCancel()
	case err := <-probeResult:
		t.Fatalf("probe Acquire returned early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("probe never observed the held lock")
	}
	if err := <-probeResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("probe error = %v; want context.Canceled", err)
	}

	if err := holderRelease(); err != nil {
		t.Fatalf("holder release: %v", err)
	}
	release, err = waiter.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
}

// TestReleaseIsIdempotent pins that the release function may be called twice.
func TestReleaseIsIdempotent(t *testing.T) {
	sessionPath := filepath.Join(t.TempDir(), "session.json")
	lock := newLock(t, sessionPath, Options{})
	release, err := lock.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("second release: %v", err)
	}
	again, err := lock.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	if err := again(); err != nil {
		t.Fatalf("release again: %v", err)
	}
}

// TestReleaseIsSafeUnderConcurrentCalls pins the sync.Once release boundary
// under the race detector: exactly one call unlocks and closes.
func TestReleaseIsSafeUnderConcurrentCalls(t *testing.T) {
	sessionPath := filepath.Join(t.TempDir(), "session.json")
	lock := newLock(t, sessionPath, Options{})
	release, err := lock.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	var wait sync.WaitGroup
	errs := make(chan error, 8)
	for index := 0; index < 8; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errs <- release()
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent release: %v", err)
		}
	}
	again, err := lock.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire after concurrent release: %v", err)
	}
	if err := again(); err != nil {
		t.Fatalf("release again: %v", err)
	}
}

// TestAcquireRefusesSymlinkLockFile pins that a symlink at the lock path is
// refused and its destination is untouched.
func TestAcquireRefusesSymlinkLockFile(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("do-not-touch"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	lockPath := filepath.Join(dir, ".session.lock")
	if err := os.Symlink(victim, lockPath); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	release, err := newLock(t, filepath.Join(dir, "session.json"), Options{}).Acquire(context.Background())
	if err == nil {
		release()
		t.Fatal("Acquire followed a symlink lock file")
	}
	if !errors.Is(err, ErrLockFileNotRegular) && !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("Acquire error = %v; want a clear symlink refusal", err)
	}
	if data, err := os.ReadFile(victim); err != nil || string(data) != "do-not-touch" {
		t.Fatalf("symlink destination changed: data=%q err=%v", data, err)
	}
	info, err := os.Lstat(lockPath)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("Acquire replaced the symlink")
	}
}

// TestAcquireRefusesDirectoryAtLockPath pins that a directory at the lock path
// is refused and never removed or replaced.
func TestAcquireRefusesDirectoryAtLockPath(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, ".session.lock")
	if err := os.Mkdir(lockPath, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	release, err := newLock(t, filepath.Join(dir, "session.json"), Options{}).Acquire(context.Background())
	if err == nil {
		release()
		t.Fatal("Acquire accepted a directory at the lock path")
	}
	info, err := os.Stat(lockPath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("Acquire replaced the directory")
	}
}

// helperEnv marks the re-executed helper process that holds the lock.
const helperEnv = "KODAMA_SESSIONLOCK_HELPER_PATH"

// TestSessionLockHelperProcess is not a normal test: when the environment
// names a session path it acquires that lock and holds it until the parent
// kills the process.
func TestSessionLockHelperProcess(t *testing.T) {
	sessionPath := os.Getenv(helperEnv)
	if sessionPath == "" {
		t.Skip("helper process only")
	}
	lock, err := NewSessionLock(sessionPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper NewSessionLock: %v\n", err)
		os.Exit(3)
	}
	release, err := lock.Acquire(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper Acquire: %v\n", err)
		os.Exit(4)
	}
	_ = release
	fmt.Println("locked")
	for {
		time.Sleep(time.Hour)
	}
}

// startLockHelper starts the re-executed helper and waits until it reports the
// held lock.
func startLockHelper(t *testing.T, sessionPath string) *exec.Cmd {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestSessionLockHelperProcess$")
	command.Env = append(os.Environ(), helperEnv+"="+sessionPath)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	reader := bufio.NewReader(stdout)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("helper did not report the lock: %v (stderr: %s)", err, stderr.String())
	}
	if got := strings.TrimSpace(line); got != "locked" {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("helper first line = %q; want locked (stderr: %s)", got, stderr.String())
	}
	return command
}

// TestLockHolderDeathReleasesLock proves cross-process exclusion and that the
// OS releases a flock when its holder dies: while the helper lives the parent
// observes a conflict; after SIGKILL the parent acquires without any release
// call from the helper.
func TestLockHolderDeathReleasesLock(t *testing.T) {
	sessionPath := filepath.Join(t.TempDir(), "session.json")
	helper := startLockHelper(t, sessionPath)

	conflict := make(chan struct{})
	var conflictOnce sync.Once
	waitCtx, waitCancel := context.WithCancel(context.Background())
	waiter := newLock(t, sessionPath, Options{
		Sleep: func(ctx context.Context, _ time.Duration) error {
			conflictOnce.Do(func() { close(conflict) })
			<-ctx.Done()
			return ctx.Err()
		},
	})
	waitResult := make(chan error, 1)
	go func() {
		_, err := waiter.Acquire(waitCtx)
		waitResult <- err
	}()
	select {
	case <-conflict:
		waitCancel()
	case err := <-waitResult:
		waitCancel()
		t.Fatalf("parent Acquire returned while the helper held the lock: %v", err)
	case <-time.After(10 * time.Second):
		waitCancel()
		t.Fatal("parent never observed the helper's lock")
	}
	if err := <-waitResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting Acquire error = %v; want context.Canceled", err)
	}

	if err := helper.Process.Kill(); err != nil {
		t.Fatalf("kill helper: %v", err)
	}
	_ = helper.Wait()

	acquireCtx, acquireCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer acquireCancel()
	release, err := newLock(t, sessionPath, Options{PollInterval: 5 * time.Millisecond}).Acquire(acquireCtx)
	if err != nil {
		t.Fatalf("Acquire after helper death: %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release after helper death: %v", err)
	}
}
