package entrypoint

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

type procSpec struct {
	argv  []string
	dir   string
	env   []string
	out   io.Writer
	grace time.Duration
}

type procResult struct {
	started  bool
	startErr error
	// exitCode is nil when the process was killed by a signal or its status
	// could not be observed.
	exitCode *int64
	// drainErr is a read failure of the output pipe; drainIncomplete means the
	// pipe was still open (an escaped descendant kept it) when the grace period
	// for draining ended. Either way part of the output may be missing.
	drainErr        error
	drainIncomplete bool
}

// runProcess starts argv in its own process group, copies stdout and stderr
// (one shared stream) to spec.out, and waits for it. When ctx ends, the whole
// group receives SIGTERM, then SIGKILL after the grace period. After the
// leader exits, remaining group members (grandchildren) are always killed, so
// nothing the command spawned outlives the call.
func runProcess(ctx context.Context, spec procSpec) procResult {
	cmd := exec.Command(spec.argv[0], spec.argv[1:]...)
	cmd.Dir = spec.dir
	cmd.Env = spec.env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	pr, pw, err := os.Pipe()
	if err != nil {
		return procResult{startErr: err}
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return procResult{startErr: err}
	}
	pw.Close()
	pid := cmd.Process.Pid
	_ = cmd.Process.Release() // we reap with wait4 ourselves

	copied := make(chan error, 1)
	go func() {
		_, err := io.Copy(spec.out, pr)
		copied <- err
	}()

	waited := make(chan *int64, 1)
	go func() { waited <- wait4Leader(pid) }()

	var code *int64
	select {
	case code = <-waited:
		killGroup(pid, syscall.SIGKILL)
		reapGroup(pid, 2*time.Second)
	case <-ctx.Done():
		code = terminate(pid, spec.grace, waited)
	}
	res := procResult{started: true, exitCode: code}
	select {
	case res.drainErr = <-copied:
	case <-time.After(spec.grace):
		// Something still holds the write end. Stop reading: the capture is
		// incomplete, and the copier must be gone before the caller freezes it.
		res.drainIncomplete = true
		pr.Close()
		select {
		case <-copied:
		case <-time.After(spec.grace):
		}
	}
	pr.Close()
	return res
}

func wait4Leader(pid int) *int64 {
	var ws syscall.WaitStatus
	for {
		_, err := syscall.Wait4(pid, &ws, 0, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil || !ws.Exited() {
			return nil
		}
		c := int64(ws.ExitStatus())
		return &c
	}
}

func killGroup(pgid int, sig syscall.Signal) { _ = syscall.Kill(-pgid, sig) }

// groupGone reaps any orphaned zombies of the group that are children of this
// process (when running as PID 1 orphans are reparented here) and reports
// whether no member remains.
func groupGone(pgid int) bool {
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-pgid, &ws, syscall.WNOHANG, nil)
		if pid <= 0 || err != nil {
			break
		}
	}
	return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH)
}

func reapGroup(pgid int, wait time.Duration) {
	deadline := time.Now().Add(wait)
	for !groupGone(pgid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}

// terminate stops the group: SIGTERM, up to grace for the leader and the rest
// of the group, then SIGKILL.
func terminate(pgid int, grace time.Duration, waited <-chan *int64) *int64 {
	killGroup(pgid, syscall.SIGTERM)
	deadline := time.Now().Add(grace)
	var code *int64
	select {
	case code = <-waited:
	case <-time.After(grace):
		killGroup(pgid, syscall.SIGKILL)
		code = <-waited
	}
	for !groupGone(pgid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !groupGone(pgid) {
		killGroup(pgid, syscall.SIGKILL)
		reapGroup(pgid, 2*time.Second)
	}
	return code
}

// boundedLog is an io.Writer that keeps the first part of a stream in a file
// and discards the rest, so a runaway command cannot exhaust scratch space. It
// always accepts the whole write so the pipe keeps draining, and records what
// it could not keep: dropped output, a failed write, or writes after it was
// frozen. Only a log with none of these may be called a complete capture.
type boundedLog struct {
	mu        sync.Mutex
	f         *os.File
	capacity  int64
	written   int64
	truncated bool
	writeErr  error
	frozen    bool
}

const truncationMarker = "\n[kodama: output truncated]\n"

func newBoundedLog(dir, pattern string, limit int64) (*boundedLog, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	return &boundedLog{f: f, capacity: dataCapacity(limit)}, nil
}

func (b *boundedLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.frozen || b.writeErr != nil {
		b.truncated = true
		return len(p), nil
	}
	room := b.capacity - b.written
	if room > 0 {
		n := min(int64(len(p)), room)
		w, err := b.f.Write(p[:n])
		b.written += int64(w)
		if err != nil {
			b.writeErr = err
			b.truncated = true
			return len(p), nil
		}
	}
	if int64(len(p)) > room {
		b.truncated = true
	}
	return len(p), nil
}

// captureState is what a frozen capture can say about itself.
type captureState struct {
	truncated bool  // output was dropped (size, failed write, or after freeze)
	writeErr  error // a write to the file failed
	closeErr  error // the file could not be closed cleanly
}

// freeze closes the log without a marker and returns its path, size and
// state. Later writes are dropped (and counted as truncation). The caller
// removes the file.
func (b *boundedLog) freeze() (string, int64, captureState) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.frozen = true
	err := b.f.Close()
	return b.f.Name(), b.written, captureState{truncated: b.truncated, writeErr: b.writeErr, closeErr: err}
}

// finish freezes the log and appends the truncation marker if output was
// dropped. It returns the file path and size. The caller removes the file.
func (b *boundedLog) finish(limit int64) (string, int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.truncated && !b.frozen && b.writeErr == nil && b.written+int64(len(truncationMarker)) <= limit {
		if _, err := b.f.WriteString(truncationMarker); err == nil {
			b.written += int64(len(truncationMarker))
		}
	}
	b.frozen = true
	path := b.f.Name()
	return path, b.written, b.f.Close()
}
