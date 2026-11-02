package process

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// State of the run resources of the process backend, on disk under one
// directory so that a restarted adapter finds what the previous one created:
//
//	.records/NAME.json   the durable intent and fence of every resource, written
//	                     BEFORE the object exists and kept as a tombstone after
//	                     it is removed, so a stopped or removed resource is
//	                     never created again
//	.holders/WS/WL.json  a workload that uses the run workspace (a preparation
//	                     or an attempt) and the process group it was recorded
//	                     under, written before the workload is let run
//	NAME/                the resource's own files (workspace/, .owner, work/,
//	                     identity/, output.log); a directory without a record is
//	                     not ours
//
// A name never authorizes anything: only a record of this installation whose
// scope, run, kind, key and instance UID match owns the directory, and a
// workspace's directory is additionally pinned by its filesystem identity.
//
// An effect is announced before it happens. A workload is launched behind a
// gate: its process starts, blocked, the adapter records its process group
// durably, and only then opens the gate. A process whose group was never recorded
// can therefore never run the command (it reads EOF at the gate when the adapter
// dies and exits), so "no recorded group" is a proof, not a guess, and no time
// window is needed to read it.

const (
	recordsDir = ".records"
	holdersDir = ".holders"
	// workspaceSub is the run workspace inside its resource directory: the
	// directory an attempt sees as /workspace.
	workspaceSub = "workspace"
	// ownerFile names, beside the workspace, the marker that holds the instance
	// UID. It lies outside the directory a workload is given.
	ownerFile = ".owner"
)

// resRecord is the durable state of one run resource.
type resRecord struct {
	Name  string                 `json:"name"`
	Scope domain.Scope           `json:"scope"`
	RunID domain.RunID           `json:"runId"`
	Kind  execution.ResourceKind `json:"kind"`
	Key   string                 `json:"key"`
	// UID is the instance identity. A tombstone made without a record keeps the
	// UID the caller supplied (possibly empty) and is Untracked: its identity was
	// never verified, and it never yields a not-started proof.
	UID         string `json:"uid"`
	Fingerprint string `json:"fingerprint"`
	Untracked   bool   `json:"untracked,omitempty"`
	// Stopping is the stop intent (the fence); Stopped is the verified stop.
	Stopping bool                     `json:"stopping,omitempty"`
	Stopped  bool                     `json:"stopped,omitempty"`
	Removed  bool                     `json:"removed,omitempty"`
	Launch   execution.LaunchEvidence `json:"launch,omitempty"`
	// Owned is true once the creation of the resource's directory was issued: it
	// is ours to clean, even if the object never materialized.
	Owned bool `json:"owned,omitempty"`
	// Created is true once the object exists and its identity is recorded: for a
	// workspace its filesystem identity, for a workload that its launch was
	// issued (see IssuedAt).
	Created bool `json:"created,omitempty"`
	// FsDev and FsIno pin the workspace directory instance.
	FsDev uint64 `json:"fsDev,omitempty"`
	FsIno uint64 `json:"fsIno,omitempty"`
	// IssuedAt is when a workload's launch was announced, before the process was
	// started. Pgid and StartID identify the launched process group (leader pid
	// and its start time) once resolved, from the process's own registration, so
	// a restarted adapter adopts it and never signals a recycled pid.
	IssuedAt *time.Time `json:"issuedAt,omitempty"`
	Pgid     int        `json:"pgid,omitempty"`
	StartID  string     `json:"startId,omitempty"`
	ReadyAt  *time.Time `json:"readyAt,omitempty"`
	// Probe is a service's readiness contract, kept so Observe needs only the
	// handle.
	Probe *probeRec `json:"probe,omitempty"`
	// ExitKnown/ExitCode record the leader's exit status once supervised.
	ExitKnown bool `json:"exitKnown,omitempty"`
	ExitCode  int  `json:"exitCode,omitempty"`
}

// probeRec is what a record keeps of a service's readiness contract.
type probeRec struct {
	Kind        string   `json:"kind"`
	Port        int64    `json:"port,omitempty"`
	Exec        []string `json:"exec,omitempty"`
	Env         []string `json:"env,omitempty"`
	TimeoutSecs int64    `json:"timeoutSeconds,omitempty"`
}

func (r resRecord) handle() execution.ResourceHandle {
	return execution.ResourceHandle{Scope: r.Scope, RunID: r.RunID, Kind: r.Kind, Key: r.Key, Name: r.Name, UID: r.UID}
}

// fenced reports that the resource may never be created or started again.
func (r resRecord) fenced() bool { return r.Stopping || r.Stopped || r.Removed }

// owns reports whether the record is the resource a handle names: scope, run,
// kind and key, and (when bound) the instance UID. A tombstone of a lost
// record that never had a UID accepts any.
func (r resRecord) owns(h execution.ResourceHandle) bool {
	if r.Scope != h.Scope || r.RunID != h.RunID || r.Kind != h.Kind || r.Key != h.Key || r.Name != h.Name {
		return false
	}
	return h.UID == "" || h.UID == r.UID || (r.Untracked && r.UID == "")
}

// tombstone is the record a stop or remove leaves for a resource nobody
// recorded: it keeps the identity the caller supplied and never claims the
// resource was not started.
func tombstone(h execution.ResourceHandle, removed bool) resRecord {
	return resRecord{
		Name: h.Name, Scope: h.Scope, RunID: h.RunID, Kind: h.Kind, Key: h.Key, UID: h.UID, Untracked: true,
		Stopping: true, Stopped: true, Removed: removed, Launch: execution.LaunchUnknown,
	}
}

// store is the on-disk state under one directory.
type store string

func (s store) record(name string) string    { return filepath.Join(string(s), recordsDir, name+".json") }
func (s store) resDir(name string) string    { return filepath.Join(string(s), name) }
func (s store) workspace(name string) string { return filepath.Join(s.resDir(name), workspaceSub) }
func (s store) owner(name string) string     { return filepath.Join(s.resDir(name), ownerFile) }

// load reads the record of name; found is false when there is none.
func (s store) load(name string) (resRecord, bool, error) {
	raw, err := os.ReadFile(s.record(name))
	if errors.Is(err, fs.ErrNotExist) {
		return resRecord{}, false, nil
	}
	if err != nil {
		return resRecord{}, false, fmt.Errorf("%w: reading the record of %s: %w", execution.ErrUnavailable, name, err)
	}
	var rec resRecord
	if err := json.Unmarshal(raw, &rec); err != nil || rec.Name != name {
		return resRecord{}, false, fmt.Errorf("%w: the record of %s is unreadable", execution.ErrUnavailable, name)
	}
	return rec, true, nil
}

// create durably writes a new record and fails with fs.ErrExist when one
// exists (it is linked into place, which never replaces a file).
func (s store) create(rec resRecord) error { return s.write(rec, os.Link) }

// save durably replaces the record.
func (s store) save(rec resRecord) error { return s.write(rec, os.Rename) }

func (s store) write(rec resRecord, place func(from, to string) error) error {
	dir := filepath.Dir(s.record(rec.Name))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return writeFileDurably(dir, s.record(rec.Name), raw, place)
}

// writeFileDurably writes data to a temporary file in dir, syncs it, places it
// at file and syncs dir.
func writeFileDurably(dir, file string, data []byte, place func(from, to string) error) error {
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := place(tmp.Name(), file); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// dirExists reports whether name's directory exists; an error other than
// absence is undeterminable.
func (s store) dirExists(name string) (bool, error) {
	_, err := os.Lstat(s.resDir(name))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, fmt.Errorf("%w: inspecting %s: %w", execution.ErrUnavailable, name, err)
}

// ---- workspace identity ----

// wsState is what verifying a workspace instance found.
type wsState int

const (
	wsPresent wsState = iota
	// wsMissing: the directory is verifiably not there (lost scratch).
	wsMissing
)

// fsIdentity is the device and inode of the workspace directory.
func fsIdentity(fi os.FileInfo) (dev, ino uint64, err error) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("%w: the filesystem reports no identity", execution.ErrUnavailable)
	}
	return uint64(st.Dev), uint64(st.Ino), nil //nolint:unconvert // Dev is int32 on darwin
}

// verifyWorkspace checks that the workspace directory is the instance the record
// pinned: the same device and inode, a directory (never a symlink), and the
// owner marker outside it holding the instance UID. A replacement is a
// collision; a verifiably missing directory is lost scratch (wsMissing), which
// is never recreated; anything undeterminable is an error.
func (s store) verifyWorkspace(rec resRecord) (wsState, error) {
	fi, err := os.Lstat(s.workspace(rec.Name))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return wsMissing, nil
	case err != nil:
		return wsPresent, fmt.Errorf("%w: inspecting the workspace: %w", execution.ErrUnavailable, err)
	case !fi.IsDir():
		return wsPresent, execution.ErrResourceCollision
	}
	dev, ino, err := fsIdentity(fi)
	if err != nil {
		return wsPresent, err
	}
	if dev != rec.FsDev || ino != rec.FsIno {
		return wsPresent, execution.ErrResourceCollision
	}
	raw, err := os.ReadFile(s.owner(rec.Name))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return wsPresent, execution.ErrResourceCollision
	case err != nil:
		return wsPresent, fmt.Errorf("%w: reading the owner marker: %w", execution.ErrUnavailable, err)
	case strings.TrimSpace(string(raw)) != rec.UID:
		return wsPresent, execution.ErrResourceCollision
	}
	return wsPresent, nil
}

// ---- holders ----

// holder is the marker of a workload using a run workspace: its process group,
// recorded before the workload was let run.
type holder struct {
	Workload string `json:"workload"`
	Pgid     int    `json:"pgid"`
	StartID  string `json:"startId"`
}

func (s store) holderFile(ws, workload string) string {
	return filepath.Join(string(s), holdersDir, ws, workload+".json")
}

func (s store) saveHolder(ws string, h holder) error {
	file := s.holderFile(ws, h.Workload)
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(h)
	if err != nil {
		return err
	}
	return writeFileDurably(dir, file, raw, os.Rename)
}

func (s store) dropHolder(ws, workload string) {
	_ = os.Remove(s.holderFile(ws, workload))
}

// held reports whether any workload of the workspace can still use it: a holder
// is held while its recorded process group lives. A marker without a group
// cannot be one that runs (see the gate). An undeterminable marker or group is an
// error, never "released".
func (s store) held(ws string) (bool, error) {
	entries, err := os.ReadDir(filepath.Join(string(s), holdersDir, ws))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: reading the holders of %s: %w", execution.ErrUnavailable, ws, err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(string(s), holdersDir, ws, e.Name()))
		var h holder
		if err != nil || json.Unmarshal(raw, &h) != nil {
			return false, fmt.Errorf("%w: a holder marker of %s is unreadable", execution.ErrUnavailable, ws)
		}
		if h.Pgid == 0 {
			continue
		}
		alive, err := groupAlive(h.Pgid, h.StartID)
		if err != nil {
			return false, err
		}
		if alive {
			return true, nil
		}
	}
	return false, nil
}

// ---- the launch gate ----

// gateScript is the wrapper every workload is launched through. It blocks reading
// the gate (fd 3, a pipe whose write end only the adapter holds) and execs the
// command only after the adapter wrote to it; end of file (the adapter closed it
// without opening the gate, or died) is an exit without the command.
const gateScript = `read -r _ <&3 || exit 125
exec 3<&-
exec "$@"`

// gate returns c run behind the launch gate (the caller passes the read end as
// fd 3 and opens the gate once the process group is recorded).
func gate(c Command) Command {
	return Command{Path: "/bin/sh", Args: append([]string{"-c", gateScript, "sh", c.Path}, c.Args...), Env: c.Env, Dir: c.Dir}
}

// ---- locks ----

var (
	lockMu sync.Mutex
	locks  = map[string]*sync.Mutex{}
)

// lockName serializes the operations of one resource name under one state
// directory across every adapter of this process, so a Stop waits for an
// Ensure or attempt launch that is deciding to start and a start that follows
// a confirmed Stop reads the fence.
func (s store) lockName(name string) (unlock func()) {
	key := filepath.Join(string(s), name)
	lockMu.Lock()
	m, ok := locks[key]
	if !ok {
		m = &sync.Mutex{}
		locks[key] = m
	}
	lockMu.Unlock()
	m.Lock()
	return m.Unlock
}

// ---- process identity ----

// startOf returns the start time of pid as the OS reports it; gone is true when
// no such process exists.
func startOf(pid int) (start string, gone bool, err error) {
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 && strings.TrimSpace(string(out)) == "" {
			return "", true, nil
		}
		return "", false, fmt.Errorf("%w: ps: %w", execution.ErrUnavailable, err)
	}
	start = strings.TrimSpace(string(out))
	return start, start == "", nil
}

// groupAlive reports whether the process group of a launched resource still has
// a member that is OURS: the leader identified by its start time, or (after the
// leader exited) a surviving member of the group, since a pid that is a live
// group cannot be recycled. A group of another user, or one led by a process
// with another start time (a recycled pid), is not ours and is never signaled.
// An undeterminable answer is an error.
func groupAlive(pgid int, startID string) (bool, error) {
	if pgid <= 1 {
		return false, nil
	}
	if err := syscall.Kill(-pgid, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM) {
			return false, nil
		}
		return false, fmt.Errorf("%w: probing process group %d: %w", execution.ErrUnavailable, pgid, err)
	}
	start, gone, err := startOf(pgid)
	switch {
	case err != nil:
		return false, err
	case gone:
		return true, nil
	}
	return startID != "" && start == startID, nil
}
