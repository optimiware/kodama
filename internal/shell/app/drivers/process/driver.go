// Package processdriver implements the Process runtime seams
// (process.Executor, process.Observer and process.Files) over os/exec and the
// operating system file API.
//
// The wrapper is deliberately thin: it launches exactly the argv it is given
// (never a shell), gives the child an explicit environment and working
// directory, starts it in its own process group, bounds both output streams and
// persists a small amount of per-group bookkeeping so a restarted composition
// can still observe the same group.
//
// Uncertainty stays visible. A spawn whose start call reported an error but
// which already produced a process is uncertain, never refused. A group with no
// bookkeeping proves nothing; only an affirmative refusal or an affirmative
// process-group termination is reported as evidence. This package starts no
// server and opens no socket; real runtime conformance belongs to the
// component suite.
package processdriver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/runtime/process"
)

const (
	// DefaultMaxOutputBytes bounds each captured stream when Config does not
	// set a smaller bound. Bytes beyond the bound are drained and discarded so
	// a chatty child never blocks on a full pipe.
	DefaultMaxOutputBytes int64 = 1 << 20
	// MaxLogReadBytes is the largest single log page this wrapper returns. It
	// matches the Process adapter's own MaxLogBytes bound.
	MaxLogReadBytes = 1 << 20
	// stateSuffix is the per-group bookkeeping file extension.
	stateSuffix = ".json"
)

// Config binds one driver instance to its durable bookkeeping directory.
type Config struct {
	// StateDir is the driver-owned directory for per-group bookkeeping. It is
	// required and absolute. It must not be workload-writable.
	StateDir string
	// MaxOutputBytes optionally bounds each captured stream. Zero uses
	// DefaultMaxOutputBytes; a negative value is refused.
	MaxOutputBytes int64
}

// groupState is the persisted spawn state of one process group.
type groupState string

const (
	stateSpawning groupState = "spawning"
	stateStarted  groupState = "started"
	stateRefused  groupState = "refused"
	stateUnknown  groupState = "unknown"
)

// record is the driver-owned bookkeeping for one process group. It carries no
// secret material: the log paths are adapter-owned resource paths.
type record struct {
	Group           process.GroupID `json:"group"`
	State           groupState      `json:"state"`
	PID             int             `json:"pid,omitempty"`
	StartTime       uint64          `json:"start_time,omitempty"`
	StdoutPath      string          `json:"stdout_path,omitempty"`
	StderrPath      string          `json:"stderr_path,omitempty"`
	StdoutTruncated bool            `json:"stdout_truncated,omitempty"`
	StderrTruncated bool            `json:"stderr_truncated,omitempty"`
	LeaderExited    bool            `json:"leader_exited,omitempty"`
	GroupGone       bool            `json:"group_gone,omitempty"`
	ExitCode        int             `json:"exit_code,omitempty"`
}

// Driver implements process.Executor, process.Observer and process.Files.
type Driver struct {
	stateDir  string
	maxOutput int64

	mu     sync.Mutex
	groups map[process.GroupID]*record
	closed bool
	wg     sync.WaitGroup

	// start is the spawn effect. Tests replace it to prove an ambiguous start
	// (a process exists but the start call reports failure).
	start func(*exec.Cmd) error
	// renameFile and syncDir are the atomic-write commit effects. Tests replace
	// them to prove that a failed commit leaves the previous file untouched.
	renameFile func(string, string) error
	syncDir    func(string) error
}

var (
	_ process.Executor = (*Driver)(nil)
	_ process.Observer = (*Driver)(nil)
	_ process.Files    = (*Driver)(nil)
)

// New validates the configuration and builds one driver. It starts no process.
func New(config Config) (*Driver, error) {
	if !filepath.IsAbs(config.StateDir) {
		return nil, fault.New(fault.InvalidInput, "process driver requires an absolute state directory")
	}
	if config.MaxOutputBytes < 0 {
		return nil, fault.New(fault.InvalidInput, "process driver output bound is negative")
	}
	maxOutput := config.MaxOutputBytes
	if maxOutput == 0 {
		maxOutput = DefaultMaxOutputBytes
	}
	stateDir := filepath.Clean(config.StateDir)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fault.Wrap(fault.DependencyUnavailable, "process driver state directory is unavailable", err)
	}
	return &Driver{
		stateDir:   stateDir,
		maxOutput:  maxOutput,
		groups:     make(map[process.GroupID]*record),
		start:      func(cmd *exec.Cmd) error { return cmd.Start() },
		renameFile: os.Rename,
		syncDir:    syncDirectory,
	}, nil
}

// Spawn starts one process group for the supplied request. The spawn intent is
// persisted before the effect. A retry for a group that already started is
// reported as started and never spawns a second process.
func (d *Driver) Spawn(ctx context.Context, request process.SpawnRequest) (process.SpawnResult, error) {
	if err := validateSpawnRequest(request); err != nil {
		return process.SpawnResult{Outcome: process.SpawnRefused, Group: request.Group, Detail: "spawn request is invalid"}, err
	}
	if err := ctx.Err(); err != nil {
		return process.SpawnResult{Outcome: process.SpawnRefused, Group: request.Group, Detail: "spawn cancelled before any effect"},
			fault.Wrap(fault.DeadlineExpired, "spawn cancelled before any effect", err)
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed {
		return process.SpawnResult{Outcome: process.SpawnRefused, Group: request.Group, Detail: "process driver is closed"},
			fault.New(fault.DependencyUnavailable, "process driver is closed")
	}
	if existing := d.groups[request.Group]; existing != nil {
		return d.reuseLocked(existing)
	}

	intent := &record{
		Group:      request.Group,
		State:      stateSpawning,
		StdoutPath: request.StdoutPath,
		StderrPath: request.StderrPath,
	}
	if err := d.persistLocked(intent); err != nil {
		// No process effect happened yet, so an affirmative refusal is safe.
		return process.SpawnResult{Outcome: process.SpawnRefused, Group: request.Group, Detail: "spawn intent could not be persisted"}, err
	}
	d.groups[request.Group] = intent

	stdout, err := openOutput(request.StdoutPath)
	if err != nil {
		intent.State = stateRefused
		_ = d.persistLocked(intent)
		return process.SpawnResult{Outcome: process.SpawnRefused, Group: request.Group, Detail: "stdout capture could not be opened"},
			fault.Wrap(fault.DependencyUnavailable, "stdout capture could not be opened", err)
	}
	stderr, err := openOutput(request.StderrPath)
	if err != nil {
		_ = stdout.Close()
		intent.State = stateRefused
		_ = d.persistLocked(intent)
		return process.SpawnResult{Outcome: process.SpawnRefused, Group: request.Group, Detail: "stderr capture could not be opened"},
			fault.Wrap(fault.DependencyUnavailable, "stderr capture could not be opened", err)
	}

	cmd := exec.Command(request.Command[0], request.Command[1:]...)
	cmd.Dir = request.Dir
	// Always pass a non-nil environment: os/exec treats a nil Cmd.Env as
	// "inherit the parent environment", which would leak host secrets when the
	// request carries an empty environment.
	cmd.Env = make([]string, 0, len(request.Env))
	cmd.Env = append(cmd.Env, request.Env...)
	cmd.Stdin = nil
	stdoutWriter := &boundedWriter{file: stdout, limit: d.maxOutput}
	stdoutWriter.onTruncate = func() {
		d.mu.Lock()
		if current := d.groups[request.Group]; current != nil {
			current.StdoutTruncated = true
		}
		d.mu.Unlock()
	}
	stderrWriter := &boundedWriter{file: stderr, limit: d.maxOutput}
	stderrWriter.onTruncate = func() {
		d.mu.Lock()
		if current := d.groups[request.Group]; current != nil {
			current.StderrTruncated = true
		}
		d.mu.Unlock()
	}
	cmd.Stdout = stdoutWriter
	cmd.Stderr = stderrWriter
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	startErr := d.start(cmd)
	if startErr != nil && cmd.Process == nil {
		_ = stdout.Close()
		_ = stderr.Close()
		intent.State = stateRefused
		_ = d.persistLocked(intent)
		return process.SpawnResult{Outcome: process.SpawnRefused, Group: request.Group, Detail: "process start was refused"},
			fault.Wrap(fault.DependencyUnavailable, "process start was refused", startErr)
	}
	if cmd.Process == nil {
		_ = stdout.Close()
		_ = stderr.Close()
		intent.State = stateUnknown
		_ = d.persistLocked(intent)
		return process.SpawnResult{Outcome: process.SpawnUncertain, Group: request.Group, Detail: "process start produced no process"},
			fault.New(fault.UncertainExternalEffect, "process start produced no process")
	}

	intent.PID = cmd.Process.Pid
	intent.StartTime = processStartTime(cmd.Process.Pid)
	intent.State = stateStarted
	d.trackWaitLocked(request.Group, cmd, stdout, stderr)

	if startErr != nil {
		intent.State = stateUnknown
		_ = d.persistLocked(intent)
		return process.SpawnResult{Outcome: process.SpawnUncertain, Group: request.Group, Detail: "process start outcome is unknown"},
			fault.Wrap(fault.UncertainExternalEffect, "process start outcome is unknown", startErr)
	}
	if err := d.persistLocked(intent); err != nil {
		intent.State = stateUnknown
		return process.SpawnResult{Outcome: process.SpawnUncertain, Group: request.Group, Detail: "started process group could not be recorded"},
			fault.Wrap(fault.UncertainExternalEffect, "started process group could not be recorded", err)
	}
	return process.SpawnResult{Outcome: process.SpawnStarted, Group: request.Group, Detail: "process group started"}, nil
}

// reuseLocked reports an existing group without spawning again.
func (d *Driver) reuseLocked(existing *record) (process.SpawnResult, error) {
	switch existing.State {
	case stateRefused:
		return process.SpawnResult{Outcome: process.SpawnRefused, Group: existing.Group, Detail: "process start was already refused"}, nil
	case stateStarted, stateUnknown:
		present, alive, gone, err := probeRecord(*existing)
		if err != nil {
			return process.SpawnResult{Outcome: process.SpawnUncertain, Group: existing.Group, Detail: "existing process group is unreachable"},
				fault.Wrap(fault.DependencyUnavailable, "existing process group is unreachable", err)
		}
		if gone && !existing.GroupGone {
			existing.GroupGone = true
			_ = d.persistLocked(existing)
		}
		if present || alive {
			return process.SpawnResult{Outcome: process.SpawnStarted, Group: existing.Group, Detail: "reused existing process group"}, nil
		}
		if existing.State == stateStarted {
			return process.SpawnResult{Outcome: process.SpawnStarted, Group: existing.Group, Detail: "reused completed process group"}, nil
		}
		return process.SpawnResult{Outcome: process.SpawnUncertain, Group: existing.Group, Detail: "existing spawn outcome is unknown"}, nil
	case stateSpawning:
		return process.SpawnResult{Outcome: process.SpawnUncertain, Group: existing.Group, Detail: "spawn intent exists but the outcome is unknown"}, nil
	default:
		return process.SpawnResult{Outcome: process.SpawnUncertain, Group: existing.Group, Detail: "existing spawn outcome is unknown"}, nil
	}
}

// SignalGroup sends SIGTERM to the whole process group. A group that is already
// gone is not an error; a canceled context performs no effect.
func (d *Driver) SignalGroup(ctx context.Context, group process.GroupID) error {
	if err := ctx.Err(); err != nil {
		return fault.Wrap(fault.DeadlineExpired, "process-group signal was canceled", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	existing := d.groups[group]
	if existing == nil {
		existing = d.loadLocked(group)
	}
	if existing == nil || existing.PID <= 0 {
		return fault.New(fault.NotFound, "process group has no recorded leader")
	}
	_, _, gone, err := probeRecord(*existing)
	if err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "process-group state is unreachable", err)
	}
	if gone {
		if !existing.GroupGone {
			existing.GroupGone = true
			_ = d.persistLocked(existing)
		}
		return nil
	}
	if err := syscall.Kill(-existing.PID, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			if !existing.GroupGone {
				existing.GroupGone = true
				_ = d.persistLocked(existing)
			}
			return nil
		}
		return fault.Wrap(fault.DependencyUnavailable, "process-group signal failed", err)
	}
	return nil
}

// RemoveGroup removes one group's bookkeeping. It refuses to forget a group
// that may still be running.
func (d *Driver) RemoveGroup(ctx context.Context, group process.GroupID) error {
	if err := ctx.Err(); err != nil {
		return fault.Wrap(fault.DeadlineExpired, "process-group removal was canceled", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	existing := d.groups[group]
	if existing == nil {
		existing = d.loadLocked(group)
	}
	if existing == nil {
		return nil
	}
	if existing.PID > 0 {
		present, alive, gone, err := probeRecord(*existing)
		if err != nil {
			return fault.Wrap(fault.DependencyUnavailable, "process-group state is unreachable", err)
		}
		if gone && !existing.GroupGone {
			existing.GroupGone = true
			_ = d.persistLocked(existing)
		}
		if present || alive {
			return fault.New(fault.Forbidden, "process group has not affirmatively terminated")
		}
	}
	if err := os.Remove(d.statePath(group)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fault.Wrap(fault.DependencyUnavailable, "process-group bookkeeping removal failed", err)
	}
	delete(d.groups, group)
	return nil
}

// Status reports the observed state of one process group. Zero evidence is
// reported as a zero status; it never claims nonexecution.
func (d *Driver) Status(ctx context.Context, group process.GroupID) (process.GroupStatus, error) {
	if err := ctx.Err(); err != nil {
		return process.GroupStatus{}, fault.Wrap(fault.DeadlineExpired, "process-group status was canceled", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	existing := d.groups[group]
	if existing == nil {
		existing = d.loadLocked(group)
	}
	if existing == nil {
		return process.GroupStatus{}, nil
	}
	if existing.State == stateRefused {
		return process.GroupStatus{NeverCreated: true}, nil
	}
	if existing.PID <= 0 {
		return process.GroupStatus{}, nil
	}
	present, alive, gone, err := probeRecord(*existing)
	if err != nil {
		return process.GroupStatus{}, fault.Wrap(fault.DependencyUnavailable, "process-group state is unreachable", err)
	}
	if gone && !existing.GroupGone {
		existing.GroupGone = true
		_ = d.persistLocked(existing)
	}
	status := process.GroupStatus{
		Present:      present,
		MembersAlive: alive,
		GroupGone:    gone,
	}
	if existing.LeaderExited {
		status.LeaderExited = true
		status.ExitCode = existing.ExitCode
	}
	return status, nil
}

// Logs reads one bounded page from one captured stream.
func (d *Driver) Logs(ctx context.Context, request process.GroupLogsRequest) (process.GroupLogPage, error) {
	if request.Limit <= 0 || request.Limit > MaxLogReadBytes {
		return process.GroupLogPage{}, fault.New(fault.InvalidInput, "log read limit is outside the bounded range")
	}
	if request.Offset < 0 {
		return process.GroupLogPage{}, fault.New(fault.InvalidInput, "log read offset is negative")
	}
	if err := ctx.Err(); err != nil {
		return process.GroupLogPage{}, fault.Wrap(fault.DeadlineExpired, "log read was canceled", err)
	}
	d.mu.Lock()
	existing := d.groups[request.Group]
	if existing == nil {
		existing = d.loadLocked(request.Group)
	}
	if existing == nil {
		d.mu.Unlock()
		return process.GroupLogPage{}, fault.New(fault.NotFound, "process group has no log capture")
	}
	snapshot := *existing
	d.mu.Unlock()

	var path string
	var truncated bool
	switch request.Stream {
	case engine.LogStdout:
		path, truncated = snapshot.StdoutPath, snapshot.StdoutTruncated
	case engine.LogStderr:
		path, truncated = snapshot.StderrPath, snapshot.StderrTruncated
	default:
		return process.GroupLogPage{}, fault.New(fault.InvalidInput, "log stream is not one of stdout/stderr")
	}
	if path == "" {
		return process.GroupLogPage{}, nil
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return process.GroupLogPage{}, nil
		}
		return process.GroupLogPage{}, fault.Wrap(fault.DependencyUnavailable, "log capture is unreadable", err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return process.GroupLogPage{}, fault.Wrap(fault.DependencyUnavailable, "log capture is unreadable", err)
	}
	size := info.Size()
	if request.Offset >= size {
		return process.GroupLogPage{Truncated: truncated}, nil
	}
	buffer := make([]byte, request.Limit)
	read, err := file.ReadAt(buffer, request.Offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return process.GroupLogPage{}, fault.Wrap(fault.DependencyUnavailable, "log capture is unreadable", err)
	}
	end := request.Offset + int64(read)
	return process.GroupLogPage{
		Data:      buffer[:read],
		Truncated: truncated || end < size,
	}, nil
}

// Close joins every process waiter. It never kills a running workload; callers
// stop workloads through the Process adapter first. A canceled context still
// joins before returning the cancellation error.
func (d *Driver) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	d.mu.Unlock()

	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		<-done
		return fault.Wrap(fault.DeadlineExpired, "process driver close was canceled", ctx.Err())
	}
}

// trackWaitLocked starts the reaper for one started process and joins it on
// Close.
func (d *Driver) trackWaitLocked(group process.GroupID, cmd *exec.Cmd, stdout, stderr *os.File) {
	d.wg.Go(func() {
		defer func() { _ = stdout.Close() }()
		defer func() { _ = stderr.Close() }()
		_ = cmd.Wait()
		d.mu.Lock()
		defer d.mu.Unlock()
		existing := d.groups[group]
		if existing == nil {
			return
		}
		existing.LeaderExited = true
		existing.ExitCode = exitCode(cmd)
		if !existing.GroupGone {
			if _, _, gone, err := probeRecord(*existing); err == nil && gone {
				existing.GroupGone = true
			}
		}
		_ = d.persistLocked(existing)
	})
}

// exitCode returns the leader's exit status. A signal termination has no
// portable status, so it stays -1 and is never reported as a successful exit.
func exitCode(cmd *exec.Cmd) int {
	if cmd.ProcessState == nil {
		return -1
	}
	return cmd.ProcessState.ExitCode()
}

// probeRecord evaluates process-group evidence for one recorded group while
// guarding against PID reuse after group exit or leader reaping.
func probeRecord(snapshot record) (present, alive, gone bool, err error) {
	if snapshot.PID <= 0 {
		return false, false, false, nil
	}
	if snapshot.GroupGone {
		return false, false, true, nil
	}
	present, alive, gone, err = probe(snapshot.PID)
	if err != nil {
		return false, false, false, err
	}
	if snapshot.LeaderExited && present {
		return false, false, true, nil
	}
	if present && snapshot.StartTime != 0 {
		current := processStartTime(snapshot.PID)
		if current != 0 && current != snapshot.StartTime {
			return false, false, true, nil
		}
		if current == 0 {
			present, alive, gone, err = probe(snapshot.PID)
			if err != nil {
				return false, false, false, err
			}
		}
	}
	return present, alive, gone, nil
}

// processStartTime reads field 22 (starttime) of /proc/<pid>/stat on Linux,
// returning 0 when unavailable.
func processStartTime(pid int) uint64 {
	if runtime.GOOS != "linux" || pid <= 0 {
		return 0
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	return parseProcStatStartTime(string(data))
}

// parseProcStatStartTime extracts the 1-based field 22 after the closing ')' of
// comm in /proc/<pid>/stat.
func parseProcStatStartTime(stat string) uint64 {
	closeParen := strings.LastIndexByte(stat, ')')
	if closeParen < 0 || closeParen+2 >= len(stat) || stat[closeParen+1] != ' ' {
		return 0
	}
	// Field 3 (state) is fields[0], so field 22 (starttime) is fields[19].
	fields := strings.Fields(stat[closeParen+2:])
	if len(fields) <= 19 {
		return 0
	}
	startTime, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0
	}
	return startTime
}

// probe reports process-group evidence. present is the leader's existence,
// alive is any member's existence and gone is affirmative absence of the whole
// group.
func probe(pid int) (present, alive, gone bool, err error) {
	if pid <= 0 {
		return false, false, false, nil
	}
	switch killErr := syscall.Kill(pid, 0); killErr {
	case nil:
		present = true
	case syscall.ESRCH:
		present = false
	case syscall.EPERM:
		present = true
	default:
		return false, false, false, killErr
	}
	switch killErr := syscall.Kill(-pid, 0); killErr {
	case nil:
		alive = true
	case syscall.ESRCH:
		alive = false
	case syscall.EPERM:
		alive = true
	default:
		return false, false, false, killErr
	}
	return present, alive, !present && !alive, nil
}

// validateSpawnRequest refuses an unusable launch before any effect.
func validateSpawnRequest(request process.SpawnRequest) error {
	if strings.TrimSpace(string(request.Group)) == "" {
		return fault.New(fault.InvalidInput, "spawn request has no process group")
	}
	if filepath.Base(string(request.Group)) != string(request.Group) || string(request.Group) == "." || string(request.Group) == ".." {
		return fault.New(fault.InvalidInput, "process group name is not a safe file name")
	}
	if len(request.Command) == 0 || strings.TrimSpace(request.Command[0]) == "" {
		return fault.New(fault.InvalidInput, "spawn request has no command")
	}
	if !filepath.IsAbs(request.Dir) {
		return fault.New(fault.InvalidInput, "spawn request working directory is not absolute")
	}
	if !filepath.IsAbs(request.StdoutPath) || !filepath.IsAbs(request.StderrPath) {
		return fault.New(fault.InvalidInput, "spawn request capture paths are not absolute")
	}
	return nil
}

// openOutput creates one bounded capture file without following a symlink.
func openOutput(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
}

// boundedWriter copies up to limit bytes and drains the rest so a chatty child
// never blocks.
type boundedWriter struct {
	file       *os.File
	limit      int64
	written    int64
	truncated  atomic.Bool
	onTruncate func()
}

func (w *boundedWriter) Write(data []byte) (int, error) {
	original := len(data)
	remaining := w.limit - w.written
	if remaining <= 0 {
		if original > 0 {
			w.markTruncated()
		}
		return original, nil
	}
	write := data
	if int64(len(write)) > remaining {
		w.markTruncated()
		write = write[:remaining]
	}
	written, err := w.file.Write(write)
	w.written += int64(written)
	if err != nil {
		return written, err
	}
	// Discard the overflow but report the full slice as consumed.
	return original, nil
}

func (w *boundedWriter) markTruncated() {
	if w.truncated.Swap(true) {
		return
	}
	if w.onTruncate != nil {
		w.onTruncate()
	}
}

// statePath returns the bookkeeping path for one group.
func (d *Driver) statePath(group process.GroupID) string {
	return filepath.Join(d.stateDir, string(group)+stateSuffix)
}

// persistLocked atomically writes one record. The caller holds d.mu.
func (d *Driver) persistLocked(value *record) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fault.Wrap(fault.InternalFailure, "process-group bookkeeping cannot be encoded", err)
	}
	temp, err := os.CreateTemp(d.stateDir, string(value.Group)+".tmp-*")
	if err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "process-group bookkeeping cannot be staged", err)
	}
	tempName := temp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tempName)
		}
	}()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fault.Wrap(fault.DependencyUnavailable, "process-group bookkeeping write failed", err)
	}
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fault.Wrap(fault.DependencyUnavailable, "process-group bookkeeping permissions failed", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fault.Wrap(fault.DependencyUnavailable, "process-group bookkeeping sync failed", err)
	}
	if err := temp.Close(); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "process-group bookkeeping close failed", err)
	}
	if err := os.Rename(tempName, d.statePath(value.Group)); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "process-group bookkeeping rename failed", err)
	}
	committed = true
	if err := d.syncDir(d.stateDir); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "process-group bookkeeping directory sync failed", err)
	}
	return nil
}

// loadLocked reads one record from disk into the in-memory map. The caller
// holds d.mu.
func (d *Driver) loadLocked(group process.GroupID) *record {
	data, err := os.ReadFile(d.statePath(group))
	if err != nil {
		return nil
	}
	var loaded record
	if err := json.Unmarshal(data, &loaded); err != nil {
		return nil
	}
	if loaded.Group != group {
		return nil
	}
	copied := loaded
	d.groups[group] = &copied
	return &copied
}
