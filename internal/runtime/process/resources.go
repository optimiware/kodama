package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/optimiware/kodama/internal/execution"
)

// ResourceCapabilities is the named set the process backend enforces for every
// run: lifecycle members only. It never advertises service isolation, network
// isolation, declared edges, named egress, default deny or builder members,
// because host processes share the host's filesystem and network. Together with
// the missing network_policy guarantee of the attempt runtime this is why no
// admission accepts a run_shared run on Process: the adapter is exercised
// directly by the companion-port contract suite and the lifecycle tests.
var ResourceCapabilities = execution.Set(
	execution.CapRunWorkspace, execution.CapWorkspaceFence, execution.CapWorkspacePreparation,
	execution.CapResourceRecovery, execution.CapRunServices,
)

// ResourceProgram decides the host command that stands in for a preparation or
// service workload. It is called once per launch, before the process starts.
// Tests supply explicit programs (a shell script, a helper binary, or the real
// `kodama prepare`); nothing is derived from the spec's image.
type ResourceProgram interface {
	Command(ctx context.Context, l ResourceLaunch) (Command, error)
}

// ResourceProgramFunc adapts a function to ResourceProgram.
type ResourceProgramFunc func(ctx context.Context, l ResourceLaunch) (Command, error)

// Command implements ResourceProgram.
func (f ResourceProgramFunc) Command(ctx context.Context, l ResourceLaunch) (Command, error) {
	return f(ctx, l)
}

// ResourceLaunch is the workload a ResourceProgram is asked for.
type ResourceLaunch struct {
	Spec   execution.ResourceSpec
	Handle execution.ResourceHandle
	// Dir is the workload's private directory (its working directory unless the
	// command says otherwise), removed with it.
	Dir string
	// WorkspaceDir is the run workspace directory (a preparation's /workspace);
	// "" for a service, which never sees the workspace.
	WorkspaceDir string
	// CredentialFile is the persisted, registered admission credential of the
	// workload; "" when the backend has no Registrar.
	CredentialFile string
	// Env is the workload's identity environment (seam S4).
	Env []string
}

// ResourcesConfig configures Resources.
type ResourcesConfig struct {
	// Dir is the root of the run resources' state and files. Required. Share it
	// with Config.ResourcesDir of the attempt Runtime so that attempts mount the
	// run workspace.
	Dir string
	// Program runs preparation and service workloads. nil refuses them with
	// ErrNotConfigured; the workspace and network need none.
	Program ResourceProgram
	// Registrar registers each workload's admission credential (seam S5). nil
	// issues none.
	Registrar ResourceRegistrar
	// ServerURL and CAFile are exported to workloads (KODAMA_SERVER_URL,
	// KODAMA_CA_FILE) when set.
	ServerURL string
	CAFile    string
	// KillGrace is the wait between SIGTERM and SIGKILL (default 2s) and
	// DrainTimeout how long output an escaped descendant holds open is read
	// (default 1s).
	KillGrace    time.Duration
	DrainTimeout time.Duration
	// MaxLogBytes bounds the stored output of each workload (default 1 MiB).
	MaxLogBytes int
}

// Resources implements execution.RunResources with directories and host
// processes. State is durable on disk: a restarted adapter adopts the
// workspaces and the still-running process groups the previous one created
// (verified by the identity the process registered itself) and never relaunches
// a workload that was ever issued.
type Resources struct {
	cfg ResourcesConfig
	st  store

	mu   sync.Mutex
	live map[string]*proc
	// sup counts the supervisors of the workloads this instance launched.
	sup sync.WaitGroup
	// hook, when set by a test, is called at the named points of a launch with the
	// pid of the child (0 before it exists); an error aborts the launch there, as
	// a controller crash would: the gate pipe closes with the adapter.
	hook func(point string, pid int) error
}

var _ execution.RunResources = (*Resources)(nil)

// NewResources validates the configuration.
func NewResources(cfg ResourcesConfig) (*Resources, error) {
	if cfg.Dir == "" {
		return nil, fmt.Errorf("%w: the process resources need a directory", execution.ErrNotConfigured)
	}
	dir, err := filepath.Abs(cfg.Dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	cfg.Dir = dir
	if cfg.KillGrace <= 0 {
		cfg.KillGrace = 2 * time.Second
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = time.Second
	}
	if cfg.MaxLogBytes <= 0 {
		cfg.MaxLogBytes = 1 << 20
	}
	return &Resources{cfg: cfg, st: store(dir), live: map[string]*proc{}}, nil
}

// proc is a workload this adapter instance launched.
type proc struct {
	pgid int
	done chan struct{}
	exit error
}

func (p *proc) ended() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// Capabilities implements execution.RunResources.
func (r *Resources) Capabilities(context.Context) execution.ResourceCapabilities {
	return execution.ResourceCapabilities{Backend: Backend, Set: ResourceCapabilities}
}

func (r *Resources) at(point string, pid int) error {
	if r.hook == nil {
		return nil
	}
	return r.hook(point, pid)
}

// find returns the record a handle names. found is false when nothing of ours
// exists at the name; a directory without a record is not ours, and a handle
// bound to an instance meeting it, or a record of another owner or instance, is
// a collision.
func (r *Resources) find(h execution.ResourceHandle) (rec resRecord, found bool, err error) {
	rec, found, err = r.st.load(h.Name)
	if err != nil {
		return resRecord{}, false, err
	}
	if found {
		if !rec.owns(h) {
			return resRecord{}, false, execution.ErrResourceCollision
		}
		return rec, true, nil
	}
	exists, err := r.st.dirExists(h.Name)
	switch {
	case err != nil:
		return resRecord{}, false, err
	case exists && h.UID != "":
		return resRecord{}, false, execution.ErrResourceCollision
	}
	return resRecord{}, false, nil
}

// Ensure implements execution.RunResources. The record, with the instance UID,
// is written before anything exists; a directory at the name without our record
// is foreign (collision); a stopping, stopped or removed record is fenced. A
// workspace is created once: its filesystem identity is recorded and verified
// afterwards, and lost scratch is never recreated.
func (r *Resources) Ensure(ctx context.Context, spec execution.ResourceSpec) (execution.ResourceObservation, error) {
	if err := spec.Validate(); err != nil {
		return execution.ResourceObservation{}, err
	}
	if spec.Backend != Backend {
		return execution.ResourceObservation{}, fmt.Errorf("%w: the spec is for backend %q", execution.ErrInvalid, spec.Backend)
	}
	fp, err := spec.Fingerprint()
	if err != nil {
		return execution.ResourceObservation{}, err
	}
	var probe *probeRec
	if spec.Kind == execution.KindService {
		if probe, err = probeOf(*spec.Service); err != nil {
			return execution.ResourceObservation{}, err
		}
	}
	unlock := r.st.lockName(spec.Name)
	defer unlock()
	rec, found, err := r.st.load(spec.Name)
	if err != nil {
		return execution.ResourceObservation{}, err
	}
	switch {
	case found:
		if rec.Scope != spec.Scope || rec.RunID != spec.RunID || rec.Kind != spec.Kind || rec.Key != spec.Key {
			return execution.ResourceObservation{}, execution.ErrResourceCollision
		}
		// A fenced name is fenced whatever the spec (a tombstone left by a stop
		// before the create has no fingerprint).
		if rec.fenced() {
			return execution.ResourceObservation{}, execution.ErrResourceFenced
		}
		if rec.Fingerprint != fp {
			return execution.ResourceObservation{}, execution.ErrResourceCollision
		}
	default:
		if exists, err := r.st.dirExists(spec.Name); err != nil {
			return execution.ResourceObservation{}, err
		} else if exists {
			return execution.ResourceObservation{}, execution.ErrResourceCollision
		}
		rec = resRecord{Name: spec.Name, Scope: spec.Scope, RunID: spec.RunID, Kind: spec.Kind, Key: spec.Key, UID: newUID(), Fingerprint: fp, Probe: probe}
		if err := r.st.create(rec); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return execution.ResourceObservation{}, fmt.Errorf("%w: a concurrent create recorded the resource", execution.ErrUnavailable)
			}
			return execution.ResourceObservation{}, err
		}
	}
	switch spec.Kind {
	case execution.KindNetwork:
		rec.Created = true
	case execution.KindWorkspace:
		err = r.ensureWorkspace(&rec)
	default:
		// A launch that was ever announced is adopted, never repeated.
		if rec.IssuedAt == nil {
			err = r.launch(ctx, &rec, spec)
		}
	}
	if err != nil {
		return execution.ResourceObservation{}, err
	}
	if err := r.st.save(rec); err != nil {
		return execution.ResourceObservation{}, err
	}
	return r.observe(ctx, rec)
}

// makeResDir creates the resource's directory, once, with the owner marker
// already inside: it is built under a temporary name and renamed into place, so
// a directory at the name either holds our marker or is not ours. An existing
// directory is accepted only if its marker is this instance's.
func (s store) makeResDir(rec resRecord) error {
	exists, err := s.dirExists(rec.Name)
	if err != nil {
		return err
	}
	if exists {
		return s.checkOwner(rec)
	}
	tmp, err := os.MkdirTemp(string(s), ".new-*")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, ownerFile), []byte(rec.UID), 0o600); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	if err := os.Rename(tmp, s.resDir(rec.Name)); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	return nil
}

// checkOwner verifies that an existing resource directory carries the record's
// owner marker; anything else is a collision and is never deleted.
func (s store) checkOwner(rec resRecord) error {
	raw, err := os.ReadFile(s.owner(rec.Name))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return execution.ErrResourceCollision
	case err != nil:
		return fmt.Errorf("%w: reading the owner marker: %w", execution.ErrUnavailable, err)
	case string(raw) != rec.UID:
		return execution.ErrResourceCollision
	}
	return nil
}

// removeOwned deletes the resource directory the record created, after
// verifying it is that directory, and confirms its absence.
func (s store) removeOwned(rec resRecord) error {
	exists, err := s.dirExists(rec.Name)
	if err != nil || !exists {
		return err
	}
	if err := s.checkOwner(rec); err != nil {
		return err
	}
	if err := os.RemoveAll(s.resDir(rec.Name)); err != nil {
		return fmt.Errorf("%w: removing %s: %w", execution.ErrUnavailable, rec.Key, err)
	}
	if exists, err := s.dirExists(rec.Name); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("%w: %s still exists", execution.ErrUnavailable, rec.Key)
	}
	return nil
}

// ensureWorkspace creates the workspace once and records its filesystem
// identity. The intent (Owned) is durable before the directory exists; a retry
// after an interruption adopts the directory it finds only if it carries this
// instance's marker. Once Created, nothing is ever recreated: the instance is
// verified, a replacement is a collision and missing scratch stays lost.
func (r *Resources) ensureWorkspace(rec *resRecord) error {
	if rec.Created {
		_, err := r.st.verifyWorkspace(*rec)
		return err
	}
	rec.Owned = true
	if err := r.st.save(*rec); err != nil {
		return err
	}
	if err := r.st.makeResDir(*rec); err != nil {
		return err
	}
	dir := r.st.workspace(rec.Name)
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return execution.ErrResourceCollision
	}
	if rec.FsDev, rec.FsIno, err = fsIdentity(fi); err != nil {
		return err
	}
	rec.Created = true
	return nil
}

// launch starts the workload of a preparation or service. The launch is
// announced (IssuedAt, durable) before the process is started, the process is
// started behind a gate (it runs nothing until the adapter writes to its pipe)
// and the adapter records the process group, durably, BEFORE it opens the gate.
// A controller that dies at any point therefore leaves either a recorded group a
// restart adopts, or a process that reads EOF at its gate and exits without ever
// running the command: no unrecorded process can run it. A launch is never
// repeated once announced. A preparation holds the run workspace: its lock is
// taken for the whole launch and its holder marker is recorded before the gate
// opens.
func (r *Resources) launch(ctx context.Context, rec *resRecord, spec execution.ResourceSpec) error {
	if r.cfg.Program == nil {
		return fmt.Errorf("%w: the process resources have no program for %s workloads", execution.ErrNotConfigured, spec.Kind)
	}
	var wsName string
	if spec.Kind == execution.KindPreparation {
		wsName = execution.ResourceName(spec.Scope, spec.RunID, execution.KindWorkspace, execution.KeyWorkspace)
		unlockWS := r.st.lockName(wsName)
		defer unlockWS()
		ws, found, err := r.st.load(wsName)
		switch {
		case err != nil:
			return err
		case !found || ws.fenced() || !ws.Created || ws.Scope != spec.Scope || ws.RunID != spec.RunID:
			return fmt.Errorf("%w: the run has no live workspace for its preparation", execution.ErrConflict)
		}
		if state, err := r.st.verifyWorkspace(ws); err != nil {
			return err
		} else if state == wsMissing {
			return fmt.Errorf("%w: the run workspace is lost", execution.ErrConflict)
		}
	}
	dir := r.st.resDir(spec.Name)
	// Everything below the directory is ours to clean, even if the launch fails.
	rec.Owned = true
	if err := r.st.save(*rec); err != nil {
		return err
	}
	if err := r.st.makeResDir(*rec); err != nil {
		return err
	}
	l := ResourceLaunch{Spec: spec, Handle: spec.Handle(rec.UID), Dir: filepath.Join(dir, "work")}
	for _, d := range []string{l.Dir, filepath.Join(dir, "identity")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	if wsName != "" {
		l.WorkspaceDir = r.st.workspace(wsName)
	}
	cred, err := r.admission(ctx, spec, filepath.Join(dir, "identity"))
	if err != nil {
		return err
	}
	l.CredentialFile = cred
	l.Env = r.identityEnv(spec, rec.UID, l)
	cmd, err := r.cfg.Program.Command(ctx, l)
	if err != nil {
		return err
	}
	// Announce before any effect.
	now := time.Now().UTC()
	rec.IssuedAt = &now
	if err := r.st.save(*rec); err != nil {
		return err
	}
	if err := r.at("issued", 0); err != nil {
		return err
	}
	return r.exec(rec, dir, wsName, gate(cmd))
}

// identityEnv is the S4 environment of a workload outside Kubernetes: no Pod
// variables, since no Pod exists.
func (r *Resources) identityEnv(spec execution.ResourceSpec, uid string, l ResourceLaunch) []string {
	env := []string{
		"KODAMA_RUN_ID=" + string(spec.RunID), "KODAMA_RESOURCE_KEY=" + spec.Key,
		"KODAMA_WORKLOAD_NAME=" + spec.Name, "KODAMA_WORKLOAD_UID=" + uid,
	}
	if r.cfg.ServerURL != "" {
		env = append(env, "KODAMA_SERVER_URL="+r.cfg.ServerURL)
	}
	if r.cfg.CAFile != "" {
		env = append(env, "KODAMA_CA_FILE="+r.cfg.CAFile)
	}
	if l.CredentialFile != "" {
		env = append(env, "KODAMA_ADMISSION_CREDENTIAL_FILE="+l.CredentialFile)
	}
	if l.WorkspaceDir != "" {
		env = append(env, "KODAMA_WORKSPACE_DIR="+l.WorkspaceDir)
	}
	return env
}

// exec starts the (already announced and gated) command in its own process
// group with its output stored in the resource directory. A start that fails
// proves nothing ran: the announcement is withdrawn. Once the process exists the
// order is fixed: the group is recorded (the record, and for a preparation its
// holder marker), and only then is the gate opened. Closing the gate pipe without
// opening it (the adapter dying, any error here) makes the process exit without
// running the command.
func (r *Resources) exec(rec *resRecord, dir, wsName string, c Command) error {
	logFile, err := os.OpenFile(filepath.Join(dir, "output.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		r.withdraw(rec)
		return err
	}
	out, in, err := os.Pipe()
	if err != nil {
		logFile.Close()
		r.withdraw(rec)
		return err
	}
	gateRead, gateWrite, err := os.Pipe()
	if err != nil {
		logFile.Close()
		out.Close()
		in.Close()
		r.withdraw(rec)
		return err
	}
	cmd := exec.Command(c.Path, c.Args...)
	cmd.Env, cmd.Dir = c.Env, c.Dir
	if cmd.Dir == "" {
		cmd.Dir = filepath.Join(dir, "work")
	}
	cmd.Stdout, cmd.Stderr = in, in
	cmd.ExtraFiles = []*os.File{gateRead}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = cmd.Start()
	in.Close()
	gateRead.Close()
	if err != nil {
		gateWrite.Close()
		out.Close()
		logFile.Close()
		r.withdraw(rec)
		return fmt.Errorf("%w: launching %s: %w", execution.ErrUnavailable, rec.Key, err)
	}
	// Whatever follows, a return before the gate opens closes it: EOF.
	defer gateWrite.Close()
	pid := cmd.Process.Pid
	p := &proc{pgid: pid, done: make(chan struct{})}
	r.mu.Lock()
	r.live[rec.Name] = p
	r.mu.Unlock()
	r.sup.Add(1)
	go r.supervise(rec.Name, p, cmd, out, logFile)
	if err := r.at("spawned", pid); err != nil {
		return err
	}
	start, gone, err := startOf(pid)
	if err == nil && gone {
		err = fmt.Errorf("%w: the gated process %d is gone", execution.ErrUnavailable, pid)
	}
	if err != nil {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		return err
	}
	rec.Pgid, rec.StartID, rec.Launch = pid, start, execution.LaunchStarted
	if err := r.st.save(*rec); err != nil {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		return err
	}
	if wsName != "" {
		if err := r.st.saveHolder(wsName, holder{Workload: rec.Name, Pgid: pid, StartID: start}); err != nil {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			return err
		}
	}
	if err := r.at("registered", pid); err != nil {
		return err
	}
	if _, err := io.WriteString(gateWrite, "go\n"); err != nil {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		return fmt.Errorf("%w: opening the gate: %w", execution.ErrUnavailable, err)
	}
	return r.at("started", pid)
}

// withdraw takes back the launch announcement after a start that failed (so
// nothing ran) and its holder marker.
func (r *Resources) withdraw(rec *resRecord) {
	prev := rec.IssuedAt
	rec.IssuedAt = nil
	if err := r.st.save(*rec); err != nil {
		rec.IssuedAt = prev // keep it announced
		return
	}
	if rec.Kind == execution.KindPreparation {
		ws := execution.ResourceName(rec.Scope, rec.RunID, execution.KindWorkspace, execution.KeyWorkspace)
		r.st.dropHolder(ws, rec.Name)
	}
}

// supervise waits for the workload, stores its bounded output and records the
// exit status. Like a container, a workload ends with its first process: the
// rest of its group is killed then.
func (r *Resources) supervise(name string, p *proc, cmd *exec.Cmd, out, logFile *os.File) {
	defer r.sup.Done()
	drained := make(chan struct{})
	go func() {
		_, _ = io.Copy(&limitWriter{w: logFile, left: r.cfg.MaxLogBytes}, out)
		close(drained)
	}()
	err := cmd.Wait()
	_ = syscall.Kill(-p.pgid, syscall.SIGKILL)
	timer := time.NewTimer(r.cfg.DrainTimeout)
	select {
	case <-drained:
		timer.Stop()
	case <-timer.C:
		out.Close()
		<-drained
	}
	out.Close()
	logFile.Close()
	p.exit = err
	close(p.done)
	r.noteExit(name, err)
}

func (r *Resources) noteExit(name string, waitErr error) {
	unlock := r.st.lockName(name)
	defer unlock()
	rec, found, err := r.st.load(name)
	if err != nil || !found || rec.ExitKnown || rec.Removed {
		return
	}
	var exit *exec.ExitError
	switch {
	case waitErr == nil:
		rec.ExitKnown, rec.ExitCode = true, 0
	case errors.As(waitErr, &exit):
		rec.ExitKnown, rec.ExitCode = true, exit.ExitCode()
	default:
		return
	}
	_ = r.st.save(rec)
}

// limitWriter keeps the first left bytes and discards the rest, so a chatty
// workload never fills the disk yet its pipe is always drained.
type limitWriter struct {
	w    io.Writer
	left int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	n := len(p)
	if l.left <= 0 {
		return n, nil
	}
	if len(p) > l.left {
		p = p[:l.left]
	}
	l.left -= len(p)
	if _, err := l.w.Write(p); err != nil {
		return n, nil
	}
	return n, nil
}

// procState is what is known about a launched workload.
type procState struct {
	running bool
	// neverRan: the launch was announced but no process group was ever recorded.
	// The gate guarantees such a process cannot have run the command: it only runs
	// it after the adapter recorded the group, and reads EOF (and exits) if the
	// adapter dies first.
	neverRan  bool
	exitKnown bool
	exitCode  int
}

// state reports whether the workload's process group still runs. An adapter
// that launched the process knows its exit; a restarted one verifies the group
// by the identity the adapter recorded before it opened the gate, and reads the
// recorded exit.
func (r *Resources) state(rec *resRecord) (procState, error) {
	r.mu.Lock()
	p := r.live[rec.Name]
	r.mu.Unlock()
	if p != nil {
		if !p.ended() {
			return procState{running: true}, nil
		}
		return procState{exitKnown: true, exitCode: exitCodeOf(p.exit)}, nil
	}
	if rec.Pgid == 0 {
		return procState{neverRan: true}, nil
	}
	alive, err := groupAlive(rec.Pgid, rec.StartID)
	if err != nil {
		return procState{}, err
	}
	return procState{running: alive, exitKnown: rec.ExitKnown, exitCode: rec.ExitCode}, nil
}

func exitCodeOf(err error) int {
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.ExitCode()
	}
	return -1
}

// Observe implements execution.RunResources. It never creates. An object that
// is verifiably not there is absent; anything the adapter cannot determine is
// unknown, never absent.
func (r *Resources) Observe(ctx context.Context, h execution.ResourceHandle) (execution.ResourceObservation, error) {
	if err := h.Validate(); err != nil {
		return execution.ResourceObservation{}, err
	}
	unlock := r.st.lockName(h.Name)
	defer unlock()
	rec, found, err := r.find(h)
	switch {
	case errors.Is(err, execution.ErrResourceCollision):
		return execution.ResourceObservation{}, err
	case err != nil:
		return unknown(h, "resource_unobservable"), nil
	case !found:
		return execution.ResourceObservation{Handle: h, Phase: execution.PhaseAbsent, ObservedAt: time.Now().UTC()}, nil
	}
	return r.observe(ctx, rec)
}

func unknown(h execution.ResourceHandle, reason execution.ResourceReason) execution.ResourceObservation {
	return execution.ResourceObservation{Handle: h, Phase: execution.PhaseUnknown, Reason: reason, ObservedAt: time.Now().UTC()}
}

// observe describes the record's resource. The caller holds its name lock. A
// workspace is verified against its recorded filesystem identity: a replacement
// is a collision, lost scratch is absent and never recreated.
func (r *Resources) observe(ctx context.Context, rec resRecord) (execution.ResourceObservation, error) {
	h := rec.handle()
	obs := execution.ResourceObservation{Handle: h, Fingerprint: rec.Fingerprint, ObservedAt: time.Now().UTC()}
	if rec.Removed {
		return execution.ResourceObservation{Handle: h, Phase: execution.PhaseAbsent, ObservedAt: obs.ObservedAt}, nil
	}
	switch rec.Kind {
	case execution.KindNetwork:
		return r.observeSimple(rec, obs), nil
	case execution.KindWorkspace:
		if !rec.Created {
			obs.Phase = execution.PhasePending
			return obs, nil
		}
		state, err := r.st.verifyWorkspace(rec)
		switch {
		case errors.Is(err, execution.ErrResourceCollision):
			return execution.ResourceObservation{}, err
		case err != nil:
			return unknown(h, "resource_unobservable"), nil
		case state == wsMissing:
			return execution.ResourceObservation{Handle: h, Phase: execution.PhaseAbsent, ObservedAt: obs.ObservedAt}, nil
		}
		held, err := r.st.held(rec.Name)
		if err != nil {
			return unknown(h, "resource_unobservable"), nil
		}
		obs.Released = !held
		return r.observeSimple(rec, obs), nil
	}
	return r.observeWorkload(ctx, rec, obs)
}

// observeSimple reports a network or workspace: ready, stopped once verified
// stopped, pending while a stop was started but not completed.
func (r *Resources) observeSimple(rec resRecord, obs execution.ResourceObservation) execution.ResourceObservation {
	switch {
	case rec.Stopped:
		obs.Phase = execution.PhaseStopped
	case rec.Stopping:
		obs.Phase, obs.Reason = execution.PhasePending, execution.ReasonStopUnconfirmed
	default:
		obs.Phase = execution.PhaseReady
		t := obs.ObservedAt
		obs.ReadyAt = &t
	}
	return obs
}

func (r *Resources) observeWorkload(ctx context.Context, rec resRecord, obs execution.ResourceObservation) (execution.ResourceObservation, error) {
	h := obs.Handle
	if rec.IssuedAt == nil {
		obs.Phase = execution.PhasePending // recorded, not launched yet: Ensure launches it
		if rec.Stopped {
			obs.Phase = execution.PhaseStopped
		}
		return obs, nil
	}
	if rec.Stopped {
		obs.Phase = execution.PhaseStopped
		return obs, nil
	}
	st, err := r.state(&rec)
	if err != nil {
		return unknown(h, "resource_unobservable"), nil
	}
	obs.LogRef = "resource-log:" + rec.Name
	switch {
	case st.neverRan && rec.Stopping:
		obs.Phase = execution.PhaseStopped
		return obs, nil
	case st.neverRan:
		obs.Phase, obs.Reason = execution.PhaseFailed, failureReason(rec.Kind)
		return obs, nil
	case rec.Stopping && st.running:
		// The stop was requested but the process has not been verified gone.
		obs.Phase, obs.Reason = execution.PhasePending, execution.ReasonStopUnconfirmed
		return obs, nil
	case rec.Stopping:
		obs.Phase = execution.PhaseStopped
		return obs, nil
	case !st.running:
		code := int64(st.exitCode)
		if rec.Kind == execution.KindPreparation && st.exitKnown && st.exitCode == 0 {
			// The preparation completed; the receipt is the server's evidence and
			// execution stops the finished workload.
			obs.Phase = execution.PhaseReady
			t := obs.ObservedAt
			obs.ReadyAt = &t
			return obs, nil
		}
		obs.Phase, obs.Reason = execution.PhaseFailed, failureReason(rec.Kind)
		if st.exitKnown {
			obs.ExitCode = &code
		}
		return obs, nil
	}
	if rec.Kind == execution.KindPreparation {
		obs.Phase = execution.PhasePending
		return obs, nil
	}
	readyAt := rec.ReadyAt
	ready, err := r.probe(ctx, rec)
	switch {
	case err != nil:
		return unknown(h, "resource_unobservable"), nil
	case !ready:
		obs.Phase = execution.PhasePending
		return obs, nil
	}
	obs.Phase = execution.PhaseReady
	obs.ProbeKind = rec.Probe.Kind
	if readyAt == nil {
		now := obs.ObservedAt
		rec.ReadyAt = &now
		if err := r.st.save(rec); err != nil {
			return unknown(h, "resource_unobservable"), nil
		}
		readyAt = &now
	}
	obs.ReadyAt = readyAt
	return obs, nil
}

func failureReason(k execution.ResourceKind) execution.ResourceReason {
	if k == execution.KindPreparation {
		return execution.ReasonPreparationFailed
	}
	return execution.ReasonServiceLost
}

// Logs implements execution.RunResources: the bounded tail of a workload's
// stored output; workspace and network have none.
func (r *Resources) Logs(_ context.Context, h execution.ResourceHandle, bound execution.LogBound) (execution.ResourceLogs, error) {
	if err := h.Validate(); err != nil {
		return execution.ResourceLogs{}, err
	}
	if !h.Kind.HasIdentity() {
		return execution.ResourceLogs{}, fmt.Errorf("%w: a %s has no logs", execution.ErrDenied, h.Kind)
	}
	if bound.MaxBytes <= 0 {
		return execution.ResourceLogs{}, fmt.Errorf("%w: the log bound must be positive", execution.ErrInvalid)
	}
	unlock := r.st.lockName(h.Name)
	rec, found, err := r.find(h)
	unlock()
	switch {
	case err != nil:
		return execution.ResourceLogs{}, err
	case !found || rec.Removed:
		return execution.ResourceLogs{}, fmt.Errorf("%w: no resource %s", execution.ErrNotFound, h.Key)
	}
	raw, err := os.ReadFile(filepath.Join(r.st.resDir(rec.Name), "output.log"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return execution.ResourceLogs{}, fmt.Errorf("%w: reading the logs: %w", execution.ErrUnavailable, err)
	}
	out := execution.ResourceLogs{}
	if int64(len(raw)) > bound.MaxBytes {
		raw, out.Truncated = raw[int64(len(raw))-bound.MaxBytes:], true
	}
	out.Content = string(raw)
	return out, nil
}

// Stop implements execution.RunResources. The stop intent is recorded first
// (a pending create or launch can no longer start: the record fences it and the
// launch holds the name's lock) and the workload's credential material is
// destroyed; only then is the process group terminated. Stopped (the verified
// stop) is recorded, and reported, only once nothing of the resource can run: a
// recorded group is terminated and verified gone, and an announced launch whose
// group was never recorded is proven unable to run by the launch gate (see
// launch); a workspace additionally reports whether a workload still holds it,
// decided by its verified identity and the holders' evidence.
func (r *Resources) Stop(ctx context.Context, h execution.ResourceHandle) (execution.ResourceStopEvidence, error) {
	if err := h.Validate(); err != nil {
		return execution.ResourceStopEvidence{}, err
	}
	unlock := r.st.lockName(h.Name)
	defer unlock()
	rec, found, err := r.find(h)
	if err != nil {
		return execution.ResourceStopEvidence{}, err
	}
	if !found {
		// Nothing of ours exists: leave the tombstone that fences a late create.
		// It keeps the identity the caller supplied and proves nothing about launch.
		rec = tombstone(h, false)
		if err := r.st.create(rec); err != nil && !errors.Is(err, fs.ErrExist) {
			return execution.ResourceStopEvidence{}, err
		}
		return execution.ResourceStopEvidence{Stopped: true, Launch: execution.LaunchUnknown, Released: true}, nil
	}
	if rec.Stopped {
		return r.stopEvidence(rec)
	}
	if !rec.Stopping {
		rec.Stopping = true
		if err := r.st.save(rec); err != nil {
			return execution.ResourceStopEvidence{}, err
		}
	}
	switch rec.Kind {
	case execution.KindNetwork:
		if rec.Launch == "" {
			rec.Launch = execution.LaunchNotStarted
			if rec.Created {
				rec.Launch = execution.LaunchStarted
			}
		}
		rec.Stopped = true
	case execution.KindWorkspace:
		if rec.Created {
			if _, err := r.st.verifyWorkspace(rec); err != nil {
				return execution.ResourceStopEvidence{}, err
			}
		}
		if rec.Launch == "" {
			rec.Launch = execution.LaunchNotStarted
			if rec.Created {
				rec.Launch = execution.LaunchStarted
			}
		}
		rec.Stopped = true
	default:
		done, err := r.stopWorkload(ctx, &rec)
		if err != nil {
			return execution.ResourceStopEvidence{Launch: rec.Launch}, err
		}
		if !done {
			if err := r.st.save(rec); err != nil {
				return execution.ResourceStopEvidence{}, err
			}
			return execution.ResourceStopEvidence{Stopped: false, Launch: execution.LaunchUnknown}, nil
		}
		rec.Stopped = true
	}
	if err := r.st.save(rec); err != nil {
		return execution.ResourceStopEvidence{}, err
	}
	return r.stopEvidence(rec)
}

// stopEvidence is the evidence of an already verified stop.
func (r *Resources) stopEvidence(rec resRecord) (execution.ResourceStopEvidence, error) {
	ev := execution.ResourceStopEvidence{Stopped: true, Launch: rec.Launch}
	if rec.Kind == execution.KindWorkspace {
		released, err := r.released(rec)
		if err != nil {
			return execution.ResourceStopEvidence{}, err
		}
		ev.Released = released
	}
	return ev, nil
}

// released reports that no workload of the workspace can still use it: the
// recorded instance is verified first (a replacement is a collision, an
// undeterminable directory an error) and then the holders' evidence decides.
func (r *Resources) released(rec resRecord) (bool, error) {
	if rec.Created {
		if _, err := r.st.verifyWorkspace(rec); err != nil {
			return false, err
		}
	}
	held, err := r.st.held(rec.Name)
	return !held, err
}

// stopWorkload terminates the workload and reports whether it is verifiably not
// running and cannot start. Credential material is removed first, whatever the
// launch reached, and a removal failure is an error.
func (r *Resources) stopWorkload(ctx context.Context, rec *resRecord) (bool, error) {
	if rec.Owned {
		if err := r.cleanIdentity(*rec); err != nil {
			return false, err
		}
	}
	if rec.IssuedAt == nil {
		// Nothing was ever announced, and we hold the lock: it never started.
		if rec.Launch == "" {
			rec.Launch = execution.LaunchNotStarted
		}
		return true, nil
	}
	r.mu.Lock()
	p := r.live[rec.Name]
	r.mu.Unlock()
	if rec.Pgid == 0 && p == nil {
		// Announced, no group ever recorded (the adapter died between the start and
		// the record): the gate was never opened, so the command cannot run, now or
		// later. The evidence stays unknown: no process was seen.
		rec.Launch = execution.LaunchUnknown
		return true, nil
	}
	if err := r.terminate(ctx, *rec); err != nil {
		return false, err
	}
	if rec.Kind == execution.KindPreparation {
		// The process is verified gone: its holder marker ends.
		r.st.dropHolder(execution.ResourceName(rec.Scope, rec.RunID, execution.KindWorkspace, execution.KeyWorkspace), rec.Name)
	}
	rec.Launch = execution.LaunchStarted
	return true, nil
}

// cleanIdentity destroys the workload's credential material, verifying the
// directory is ours first.
func (r *Resources) cleanIdentity(rec resRecord) error {
	dir := r.st.resDir(rec.Name)
	if exists, err := r.st.dirExists(rec.Name); err != nil || !exists {
		return err
	}
	if err := r.st.checkOwner(rec); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(dir, "identity")); err != nil {
		return fmt.Errorf("%w: removing the credential material: %w", execution.ErrUnavailable, err)
	}
	return nil
}

// terminate ends the workload's process group: SIGTERM, then SIGKILL after the
// grace, and returns only when the group is verifiably gone.
func (r *Resources) terminate(ctx context.Context, rec resRecord) error {
	pgid := rec.Pgid
	r.mu.Lock()
	if p := r.live[rec.Name]; p != nil {
		pgid = p.pgid // launched by this instance: known before it registered
	}
	r.mu.Unlock()
	if pgid <= 1 {
		return fmt.Errorf("%w: no process group is known for %s", execution.ErrUnavailable, rec.Key)
	}
	gone := func() (bool, error) {
		r.mu.Lock()
		p := r.live[rec.Name]
		r.mu.Unlock()
		if p != nil {
			return p.ended(), nil
		}
		alive, err := groupAlive(pgid, rec.StartID)
		return !alive, err
	}
	done, err := gone()
	if err != nil || done {
		return err
	}
	// The group is ours (verified by its registered identity): signal only now.
	signal := func(sig syscall.Signal, wait time.Duration) (bool, error) {
		_ = syscall.Kill(-pgid, sig)
		deadline := time.NewTimer(wait)
		defer deadline.Stop()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			if done, err := gone(); err != nil || done {
				return done, err
			}
			select {
			case <-deadline.C:
				return false, nil
			case <-ctx.Done():
				return false, ctx.Err()
			case <-tick.C:
			}
		}
	}
	if done, err := signal(syscall.SIGTERM, r.cfg.KillGrace); err != nil || done {
		return err
	}
	done, err = signal(syscall.SIGKILL, 10*time.Second)
	if err != nil {
		return err
	}
	if !done {
		return fmt.Errorf("%w: process group %d of %s did not end", execution.ErrUnavailable, pgid, rec.Key)
	}
	return nil
}

// Remove implements execution.RunResources. It requires a prior VERIFIED stop
// (a stop that was only requested is not one), deletes the resource's files
// after verifying their identity and confirms their absence; a workspace a
// workload still holds is not removed (Removed false keeps cleanup pending), and
// a workspace directory that is not the recorded instance is a collision that is
// never deleted.
func (r *Resources) Remove(ctx context.Context, h execution.ResourceHandle) (execution.ResourceRemoveEvidence, error) {
	if err := h.Validate(); err != nil {
		return execution.ResourceRemoveEvidence{}, err
	}
	unlock := r.st.lockName(h.Name)
	defer unlock()
	rec, found, err := r.find(h)
	if err != nil {
		return execution.ResourceRemoveEvidence{}, err
	}
	if !found {
		if err := r.st.create(tombstone(h, true)); err != nil && !errors.Is(err, fs.ErrExist) {
			return execution.ResourceRemoveEvidence{}, err
		}
		return execution.ResourceRemoveEvidence{Removed: true}, nil
	}
	if rec.Removed {
		return execution.ResourceRemoveEvidence{Removed: true}, nil
	}
	if !rec.Stopped {
		return execution.ResourceRemoveEvidence{}, fmt.Errorf("%w: %s is removed only after a verified stop", execution.ErrResourceFenced, rec.Key)
	}
	if rec.Kind.HasIdentity() && rec.Pgid != 0 {
		if err := r.terminate(ctx, rec); err != nil {
			return execution.ResourceRemoveEvidence{}, err
		}
	}
	if rec.Kind == execution.KindWorkspace && rec.Created {
		released, err := r.released(rec)
		if err != nil {
			return execution.ResourceRemoveEvidence{}, err
		}
		if !released {
			return execution.ResourceRemoveEvidence{Removed: false}, nil
		}
	}
	// Only a directory this adapter created, verified by its marker, is deleted:
	// a tombstone made without a record owns nothing, and whatever sits at its
	// name is not ours.
	if rec.Owned {
		if err := r.st.removeOwned(rec); err != nil {
			return execution.ResourceRemoveEvidence{}, err
		}
	}
	if rec.Kind == execution.KindWorkspace {
		_ = os.RemoveAll(filepath.Join(string(r.st), holdersDir, rec.Name))
	}
	if rec.Kind == execution.KindPreparation {
		// The process is verified gone: its holder marker ends with it.
		r.st.dropHolder(execution.ResourceName(rec.Scope, rec.RunID, execution.KindWorkspace, execution.KeyWorkspace), rec.Name)
	}
	rec.Removed = true
	if err := r.st.save(rec); err != nil {
		return execution.ResourceRemoveEvidence{}, err
	}
	r.mu.Lock()
	delete(r.live, rec.Name)
	r.mu.Unlock()
	return execution.ResourceRemoveEvidence{Removed: true}, nil
}

// ---- readiness ----

// probe runs the service's readiness probe once. A failing probe is "not
// ready"; only an inability to probe is an error.
func (r *Resources) probe(ctx context.Context, rec resRecord) (bool, error) {
	pr := rec.Probe
	if pr == nil {
		return false, fmt.Errorf("%w: the service has no recorded readiness", execution.ErrUnavailable)
	}
	timeout := time.Duration(pr.TimeoutSecs) * time.Second
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	switch pr.Kind {
	case "tcp":
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.FormatInt(pr.Port, 10)))
		if err != nil {
			return false, nil
		}
		conn.Close()
		return true, nil
	case "exec":
		cmd := exec.CommandContext(ctx, pr.Exec[0], pr.Exec[1:]...)
		cmd.Dir = filepath.Join(r.st.resDir(rec.Name), "work")
		cmd.Env = slices.Concat([]string{"PATH=" + os.Getenv("PATH")}, pr.Env)
		return cmd.Run() == nil, nil
	}
	return false, fmt.Errorf("%w: unsupported readiness probe", execution.ErrInvalid)
}

// probeOf validates a service's readiness contract and returns what the record
// keeps of it: the probe, its target and the service's own environment.
func probeOf(s execution.ServiceSpec) (*probeRec, error) {
	rd := s.Readiness
	pr := &probeRec{Kind: rd.Probe, TimeoutSecs: rd.ProbeTimeout}
	switch rd.Probe {
	case "tcp":
		port, ok := s.Ports[rd.Port]
		if !ok || port < 1 || port > 65535 {
			return nil, fmt.Errorf("%w: the readiness port %q is not declared", execution.ErrInvalid, rd.Port)
		}
		pr.Port = port
	case "exec":
		if len(rd.Exec) == 0 {
			return nil, fmt.Errorf("%w: the exec probe has no command", execution.ErrInvalid)
		}
		pr.Exec = slices.Clone(rd.Exec)
		for _, k := range slices.Sorted(maps.Keys(s.Env)) {
			pr.Env = append(pr.Env, k+"="+s.Env[k])
		}
	default:
		return nil, fmt.Errorf("%w: the process backend probes tcp and exec readiness only", execution.ErrInvalid)
	}
	return pr, nil
}

// idle waits until every supervisor of this instance finished, up to d, and
// reports whether they did. Tests use it so no goroutine writes into a state
// directory that is being deleted.
func (r *Resources) idle(d time.Duration) bool {
	done := make(chan struct{})
	go func() { r.sup.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}
