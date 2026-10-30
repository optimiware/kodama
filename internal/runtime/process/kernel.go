package process

// The kernel adapter implements the standalone pipeline runtime contract
// (internal/kernel/runtime) over host processes: kernel Runtime for step
// workloads and kernel ResourceManager for run-scoped workspaces and services.
// It is the Process backend of ADR 0008 behind the T-171 ports, with no shell,
// SQL, broker, Git, task, identity or agent dependency.
//
// # What it enforces, and what it refuses
//
// Process runs commands on the host, so it advertises only what it enforces
// (KernelCapabilities): idempotent adoption of a workload or resource by its
// stable identity, writer-fenced stop, and run-scoped services. It never
// advertises image pinning, network isolation, resource limits, non-root
// execution or persistent (project or tenant) services. A workload or resource
// that Requires any of those is refused before anything is created, with an
// error wrapping both ErrCapability and ErrNotReached, so the engine can prove
// the effect never reached the backend. Persistent-lifetime services, services
// that declare secrets and workspace sources (no fetcher is configured) are
// refused the same way. The image of a step is not interpreted: the declared
// commands run through the configured KernelProgram (a shell by default).
//
// # Identity, adoption and fencing
//
// Every object is keyed by tenant, project, type and its stable identity
// (RuntimeBinding.WorkloadID, ResourceSpec.Identity). Its record is written
// before the object exists. Start and Ensure are idempotent on the identity:
// the same specification digest adopts the existing object, another digest
// fails with ErrSpecMismatch, and a request carrying a fence older than the
// highest fence that touched the object fails with ErrStaleFence. A higher
// fence takes the object over. Stop and Remove obey the same fence rule, and
// an object that was stopped or removed is never started again: its record
// stays as a tombstone, so a delayed create after a stop is refused.
//
// A workload has ended only when its whole process group is verifiably gone,
// not when its leader's status is collected. The leader's result and the
// group's termination are separate facts: descendants a leader leaves behind
// keep the workload running, keep the workspace held, and are terminated and
// verified gone before a stop or removal is confirmed or the holder released.
// An accepted fence is persisted before any other effect of the call, and
// exit evidence is kept in memory until a record carrying it was saved.
//
// A workload is launched behind a gate: the process starts blocked, the
// adapter records its process group durably, and only then opens the gate. A
// process whose group was never recorded cannot have run the command.
//
// # Workspace exclusion
//
// One executable step holds a run's workspace at a time. Start records a
// holder marker (its process group, written before the gate opens) and a second
// Start for another workload on the same workspace is refused with
// ErrWorkspaceBusy, which wraps ErrNotReached: nothing was launched and the
// engine may start it after the holder settles. The marker is released when the
// holder's process group is verifiably gone; WorkspaceHolder reports it. A
// workspace that is held is not reported stopped and is never removed.
//
// # Recovery
//
// State is durable under the configured directory. A new Kernel over the same
// directory adopts the process groups its predecessor left running, verified by
// the leader's recorded start time, so a recycled pid is never signalled. A
// process that ended while no adapter watched has no known exit status and is
// reported lost. The adapter does not restart anything by itself: Reconcile
// stops and removes objects whose run the caller no longer owns.

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
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	kr "github.com/optimiware/kodama/internal/kernel/runtime"
)

// Kernel errors. Each wraps domain.ErrConflict.
var (
	// ErrWorkspaceBusy reports a Start refused because another workload holds the
	// run's workspace.
	ErrWorkspaceBusy = fmt.Errorf("%w: the workspace is held by another step", domain.ErrConflict)
	// ErrFenced reports an identity that was stopped or removed and is never
	// started again.
	ErrFenced = fmt.Errorf("%w: the identity was stopped and is never started again", domain.ErrConflict)

	errCollision = fmt.Errorf("%w: the directory is not the recorded instance", domain.ErrConflict)
)

// KernelCapabilities is what the Process backend enforces for every workload
// and resource. See the package comment for what it never advertises.
var KernelCapabilities = kr.CapabilitySet{kr.CapAdoptWorkload, kr.CapFencedStop, kr.CapRunServices}

// notReached marks an error as affirmatively before the backend.
func notReached(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{kr.ErrNotReached}, args...)...)
}

func notFound(format string, args ...any) error {
	return &domain.Error{Code: domain.CodeNotFound, Message: fmt.Sprintf(format, args...)}
}

// KernelLaunch is the workload a KernelProgram is asked for.
type KernelLaunch struct {
	Binding kr.RuntimeBinding
	Spec    kr.WorkloadSpec
	// Dir is the workload's private directory.
	Dir string
	// Workspace is the run workspace directory, the step's /workspace.
	Workspace string
	// Secrets is the private materialization directory, "" when none; it lies
	// outside Workspace.
	Secrets string
	// Services names the run's ready services.
	Services []string
}

// KernelProgram decides the host command that stands in for a step workload.
type KernelProgram interface {
	Command(ctx context.Context, l KernelLaunch) (Command, error)
}

// KernelProgramFunc adapts a function to KernelProgram.
type KernelProgramFunc func(ctx context.Context, l KernelLaunch) (Command, error)

// Command implements KernelProgram.
func (f KernelProgramFunc) Command(ctx context.Context, l KernelLaunch) (Command, error) {
	return f(ctx, l)
}

// HostShell runs the declared commands of a step with /bin/sh in the run
// workspace, stopping at the first failing command. The image is not
// interpreted; the declared environment sits over PATH and the identity
// variables (KODAMA_RUN_ID, KODAMA_STEP_KEY, KODAMA_WORKSPACE_DIR,
// KODAMA_SECRETS_DIR, KODAMA_SERVICES) always win.
type HostShell struct{}

// Command implements KernelProgram.
func (HostShell) Command(_ context.Context, l KernelLaunch) (Command, error) {
	if len(l.Spec.Commands) == 0 {
		return Command{}, errors.New("the workload declares no commands")
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + l.Dir, "TMPDIR=" + l.Dir}
	for _, k := range slices.Sorted(maps.Keys(l.Spec.Env)) {
		env = append(env, k+"="+l.Spec.Env[k])
	}
	env = append(env,
		"KODAMA_RUN_ID="+string(l.Binding.RunID), "KODAMA_STEP_KEY="+string(l.Binding.StepKey),
		"KODAMA_WORKSPACE_DIR="+l.Workspace, "KODAMA_SERVICES="+strings.Join(l.Services, ","))
	if l.Secrets != "" {
		env = append(env, "KODAMA_SECRETS_DIR="+l.Secrets)
	}
	return Command{Path: "/bin/sh", Args: []string{"-c", shellScript(l.Spec.Commands)}, Env: env, Dir: l.Workspace}, nil
}

func shellScript(commands []string) string { return "set -e\n" + strings.Join(commands, "\n") }

// KernelPrepare fills a workspace from pinned inputs. It must be idempotent:
// it runs again when an earlier call was interrupted before it returned.
type KernelPrepare func(ctx context.Context, workspaceDir string, in kr.PreparationInputs) error

// KernelConfig configures a Kernel.
type KernelConfig struct {
	// Dir is the dedicated state directory. Required.
	Dir string
	// Program decides what a step runs (default HostShell).
	Program KernelProgram
	// Prepare supplies the preparation of workspaces with sources; without it a
	// workspace with sources is refused, because Process fetches nothing.
	Prepare KernelPrepare
	// KillGrace is the wait between SIGTERM and SIGKILL (default 2s).
	KillGrace time.Duration
	// MaxLogBytes bounds the output returned by Logs (default 1 MiB).
	MaxLogBytes int
	// Now supplies time (default time.Now).
	Now func() time.Time
}

// Kernel is the Process backend behind the kernel ports.
type Kernel struct {
	// Runtime launches and supervises step workloads.
	Runtime *KernelRuntime
	// Resources owns run-scoped workspaces and services.
	Resources *KernelResources

	h *host
}

// KernelRuntime implements kr.Runtime.
type KernelRuntime struct{ h *host }

// KernelResources implements kr.ResourceManager.
type KernelResources struct{ h *host }

var (
	_ kr.Runtime         = (*KernelRuntime)(nil)
	_ kr.ResourceManager = (*KernelResources)(nil)
)

// NewKernel validates the configuration and opens the state directory.
func NewKernel(cfg KernelConfig) (*Kernel, error) {
	if cfg.Dir == "" {
		return nil, errors.New("process: the kernel adapter needs a state directory")
	}
	dir, err := filepath.Abs(cfg.Dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	cfg.Dir = dir
	if cfg.Program == nil {
		cfg.Program = HostShell{}
	}
	if cfg.KillGrace <= 0 {
		cfg.KillGrace = 2 * time.Second
	}
	if cfg.MaxLogBytes <= 0 {
		cfg.MaxLogBytes = 1 << 20
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	h := &host{cfg: cfg, st: kstore(dir), live: map[string]*kproc{}, killOnExit: func(pgid int) { _ = syscall.Kill(-pgid, syscall.SIGKILL) }}
	return &Kernel{Runtime: &KernelRuntime{h}, Resources: &KernelResources{h}, h: h}, nil
}

// Idle waits until every supervisor of this instance finished, up to d.
func (k *Kernel) Idle(d time.Duration) bool {
	done := make(chan struct{})
	go func() { k.h.sup.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// host is the state shared by the runtime and the resource manager.
type host struct {
	cfg KernelConfig
	st  kstore

	mu   sync.Mutex
	live map[string]*kproc // by type-key
	sup  sync.WaitGroup

	// killOnExit kills what a workload's leader left in its process group; tests
	// replace it to leave descendants behind. It is set before any launch.
	killOnExit func(pgid int)
}

// kproc is a process this adapter instance launched.
type kproc struct {
	pgid int
	// done is closed once the leader was reaped and its status collected. It says
	// nothing about the rest of the group: see host.groupGone.
	done     chan struct{}
	exit     error
	timedOut atomic.Bool

	// ev is the decided exit evidence, kept in memory until a record carrying it
	// was saved: a failed save must not turn the result into a lost workload or
	// a cancellation, so the decision is made once and replayed.
	evMu sync.Mutex
	ev   *exitEvidence
}

// exitEvidence is how the leader ended.
type exitEvidence struct {
	code      int
	timedOut  bool
	cancelled bool
}

// evidence decides, once, how the leader ended: after the stop intent it was
// cancelled, unless it timed out; before it, it ended on its own.
func (p *kproc) evidence(stopping bool) exitEvidence {
	p.evMu.Lock()
	defer p.evMu.Unlock()
	if p.ev == nil {
		timedOut := p.timedOut.Load()
		p.ev = &exitEvidence{code: exitCodeOf(p.exit), timedOut: timedOut, cancelled: stopping && !timedOut}
	}
	return *p.ev
}

func (p *kproc) ended() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (h *host) now() time.Time { return h.cfg.Now().UTC() }

func (h *host) proc(typ, key string) *kproc {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.live[typ+"-"+key]
}

func (h *host) forget(typ, key string) {
	h.mu.Lock()
	delete(h.live, typ+"-"+key)
	h.mu.Unlock()
}

// admitFence applies the fence rule to an operation of a writer holding f: an
// older fence is a superseded writer; a newer one takes the object over and is
// recorded. It reports whether the record changed.
func admitFence(rec *kRecord, f kr.Fence) error {
	if err := f.Validate(); err != nil {
		return err
	}
	if f < rec.Fence {
		return fmt.Errorf("%w: %s %s: held %d, current %d", kr.ErrStaleFence, rec.Type, rec.ID, f, rec.Fence)
	}
	rec.Fence = f
	return nil
}

// admitFence applies the fence rule and persists a newly admitted higher fence
// at once, before any fast path, refusal or uncertain termination can return:
// a writer that was accepted must supersede every lower fence even when nothing
// else about the object changed.
func (h *host) admitFence(rec *kRecord, f kr.Fence) error {
	before := rec.Fence
	if err := admitFence(rec, f); err != nil {
		return err
	}
	if rec.Fence != before {
		return h.st.save(*rec)
	}
	return nil
}

// checkBackend refuses a binding that names another backend before any record
// is read or created.
func checkBackend(b kr.RuntimeBinding) error {
	if b.Backend != Backend {
		return fmt.Errorf("%w: the binding is for backend %q", domain.ErrInvalidValue, b.Backend)
	}
	return nil
}

// ---- capabilities ----

// Capabilities implements kr.Runtime.
func (r *KernelRuntime) Capabilities(context.Context) (kr.CapabilitySet, error) {
	return slices.Clone(KernelCapabilities), nil
}

// Capabilities implements kr.ResourceManager.
func (r *KernelResources) Capabilities(context.Context) (kr.CapabilitySet, error) {
	return slices.Clone(KernelCapabilities), nil
}

// admit refuses a requirement the backend cannot enforce.
func admit(what string, requires kr.CapabilitySet) error {
	if err := KernelCapabilities.Satisfies(requires); err != nil {
		return fmt.Errorf("%w: process refuses %s: %w", kr.ErrNotReached, what, err)
	}
	return nil
}

// ---- starting a workload ----

// Start implements kr.Runtime. It is idempotent on the binding's workload id:
// the same digest adopts, another digest is ErrSpecMismatch, an older fence is
// ErrStaleFence and a stopped or removed workload is ErrFenced. A start that
// does not return a running or settled workload leaves nothing running.
func (r *KernelRuntime) Start(ctx context.Context, req kr.StartRequest) (kr.WorkloadObservation, error) {
	h := r.h
	b := req.Binding
	if err := h.validateStart(req); err != nil {
		return kr.WorkloadObservation{}, notReached("%w", err)
	}
	if err := admit("workload "+b.WorkloadID, req.Spec.Requires); err != nil {
		return kr.WorkloadObservation{}, err
	}
	key := objectKey(b.Scope, typeWorkload, b.WorkloadID)
	unlock := h.st.lock(typeWorkload, key)
	defer unlock()

	rec, found, err := h.st.load(typeWorkload, key)
	if err != nil {
		return kr.WorkloadObservation{}, err
	}
	if found {
		if err := checkWorkload(rec, b); err != nil {
			return kr.WorkloadObservation{}, notReached("%w", err)
		}
		if err := h.admitFence(&rec, b.Fence); err != nil {
			return kr.WorkloadObservation{}, notReached("%w", err)
		}
		if rec.fenced() {
			return kr.WorkloadObservation{}, notReached("%w: workload %s", ErrFenced, b.WorkloadID)
		}
		if rec.Digest != b.SpecDigest {
			return kr.WorkloadObservation{}, notReached("%w: workload %s holds digest %s", kr.ErrSpecMismatch, b.WorkloadID, rec.Digest)
		}
		if err := h.st.save(rec); err != nil {
			return kr.WorkloadObservation{}, err
		}
		return h.observeWorkload(b, &rec)
	}
	if exists, err := h.st.dirExists(typeWorkload, key); err != nil {
		return kr.WorkloadObservation{}, notReached("%w", err)
	} else if exists {
		return kr.WorkloadObservation{}, notReached("%w: a directory not recorded by this adapter holds workload %s", errCollision, b.WorkloadID)
	}
	return r.launch(ctx, req, key)
}

func (h *host) validateStart(req kr.StartRequest) error {
	b := req.Binding
	if err := b.Validate(); err != nil {
		return err
	}
	if err := checkBackend(b); err != nil {
		return err
	}
	if err := req.Spec.Validate(); err != nil {
		return err
	}
	if err := req.Workspace.Validate(); err != nil {
		return err
	}
	if req.Workspace.Kind != kr.ResourceWorkspace || req.Workspace.Scope != b.Scope || req.Workspace.RunID != b.RunID {
		return fmt.Errorf("%w: the workspace handle is not this run's workspace", domain.ErrInvalidValue)
	}
	for _, s := range req.Services {
		if err := s.Validate(); err != nil {
			return err
		}
		if s.Kind != kr.ResourceService || s.Scope != b.Scope || s.RunID != b.RunID {
			return fmt.Errorf("%w: service %s is not a service of this run", domain.ErrInvalidValue, s.Name)
		}
	}
	if m := req.Secrets; m.Directory != "" {
		if m.Scope != b.Scope || m.RunID != b.RunID || m.StepKey != b.StepKey {
			return fmt.Errorf("%w: the secret materialization belongs to another step", domain.ErrInvalidValue)
		}
		if !filepath.IsAbs(m.Directory) {
			return fmt.Errorf("%w: the secret directory must be absolute", domain.ErrInvalidValue)
		}
	}
	return nil
}

// checkWorkload verifies that the record is the workload the binding names.
func checkWorkload(rec kRecord, b kr.RuntimeBinding) error {
	if rec.Untracked {
		return nil // a tombstone has only the fence; it is refused as fenced
	}
	if rec.RunID != b.RunID || rec.StepKey != b.StepKey || rec.Attempt != b.Attempt {
		return fmt.Errorf("%w: workload %s belongs to another step attempt", domain.ErrConflict, b.WorkloadID)
	}
	return nil
}

// launch starts a new workload. The caller holds the workload lock.
func (r *KernelRuntime) launch(ctx context.Context, req kr.StartRequest, key string) (kr.WorkloadObservation, error) {
	h := r.h
	b := req.Binding
	wsKey := objectKey(b.Scope, typeWorkspace, req.Workspace.Identity)
	unlockWS := h.st.lock(typeWorkspace, wsKey)
	defer unlockWS()

	ws, found, err := h.st.load(typeWorkspace, wsKey)
	switch {
	case err != nil:
		return kr.WorkloadObservation{}, notReached("%w", err)
	case !found || ws.Untracked || ws.RunID != b.RunID:
		return kr.WorkloadObservation{}, notReached("%w: the run has no workspace", domain.ErrConflict)
	case ws.fenced():
		return kr.WorkloadObservation{}, notReached("%w: the run workspace", ErrFenced)
	case !ws.Created:
		return kr.WorkloadObservation{}, notReached("%w: the run workspace was never created", domain.ErrConflict)
	}
	state, err := h.st.verifyWorkspace(ws)
	switch {
	case err != nil:
		return kr.WorkloadObservation{}, notReached("%w", err)
	case state == wsMissing:
		return kr.WorkloadObservation{}, notReached("%w: the run workspace is lost", domain.ErrConflict)
	}
	if held, ok, err := h.st.holder(wsKey); err != nil {
		return kr.WorkloadObservation{}, notReached("%w", err)
	} else if ok {
		return kr.WorkloadObservation{}, notReached("%w: workload %s holds it", ErrWorkspaceBusy, held.WorkloadID)
	}
	services := make([]string, 0, len(req.Services))
	for _, s := range req.Services {
		name, err := h.serviceReady(ctx, b, s)
		if err != nil {
			return kr.WorkloadObservation{}, notReached("%w", err)
		}
		services = append(services, name)
	}
	wsDir := h.st.workspaceDir(wsKey)
	secrets := req.Secrets.Directory
	if secrets != "" {
		if rel, err := filepath.Rel(wsDir, filepath.Clean(secrets)); err == nil && !strings.HasPrefix(rel, "..") {
			return kr.WorkloadObservation{}, notReached("%w: the secret directory lies inside the workspace", domain.ErrInvalidValue)
		}
	}

	now := h.now()
	rec := kRecord{
		Type: typeWorkload, Key: key, Scope: b.Scope, RunID: b.RunID, StepKey: b.StepKey, Attempt: b.Attempt,
		ID: b.WorkloadID, Digest: b.SpecDigest, Fence: b.Fence, UID: newUID(), Owned: true, IssuedAt: &now,
	}
	if err := h.st.create(rec); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return kr.WorkloadObservation{}, notReached("%w: a concurrent start recorded the workload", domain.ErrConflict)
		}
		return kr.WorkloadObservation{}, notReached("%w", err)
	}
	withdraw := func(err error) (kr.WorkloadObservation, error) {
		h.st.dropHolder(wsKey, key)
		_ = os.RemoveAll(h.st.dir(typeWorkload, key))
		_ = h.st.deleteRecord(typeWorkload, key)
		h.forget(typeWorkload, key)
		return kr.WorkloadObservation{}, notReached("launching workload %s: %w", b.WorkloadID, err)
	}
	if err := h.st.makeDir(rec); err != nil {
		return withdraw(err)
	}
	work := filepath.Join(h.st.dir(typeWorkload, key), "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		return withdraw(err)
	}
	cmd, err := h.cfg.Program.Command(ctx, KernelLaunch{
		Binding: b, Spec: req.Spec, Dir: work, Workspace: wsDir, Secrets: secrets, Services: services,
	})
	if err != nil {
		return withdraw(err)
	}
	holder := func(pid int, start string) error {
		return h.st.saveHolder(wsKey, kholder{WorkloadID: b.WorkloadID, WorkloadKey: key, Pgid: pid, StartID: start})
	}
	if err := h.spawn(&rec, gate(cmd), work, req.Spec.Timeout, holder); err != nil {
		return withdraw(err)
	}
	return h.observeWorkload(b, &rec)
}

// spawn starts the (already announced) command behind the launch gate in its
// own process group, output going straight to the object's log file so a
// restarted adapter loses nothing. The group is recorded durably (and the
// holder, via register) BEFORE the gate opens; any failure before closes the
// gate, so no process the record does not know can run the command.
func (h *host) spawn(rec *kRecord, c Command, workDir string, timeout time.Duration, register func(pid int, start string) error) error {
	logFile, err := os.OpenFile(h.st.logPath(rec.Type, rec.Key), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	gateRead, gateWrite, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd := exec.Command(c.Path, c.Args...)
	cmd.Env, cmd.Dir = c.Env, c.Dir
	if cmd.Dir == "" {
		cmd.Dir = workDir
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.ExtraFiles = []*os.File{gateRead}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = cmd.Start()
	gateRead.Close()
	if err != nil {
		gateWrite.Close()
		return err
	}
	defer gateWrite.Close() // before the gate opens, closing it is the child's EOF
	pid := cmd.Process.Pid
	p := &kproc{pgid: pid, done: make(chan struct{})}
	h.mu.Lock()
	h.live[rec.Type+"-"+rec.Key] = p
	h.mu.Unlock()
	h.sup.Add(1)
	go h.supervise(rec.Type, rec.Key, p, cmd)

	start, gone, err := startOf(pid)
	if err == nil && gone {
		err = fmt.Errorf("the gated process %d is gone", pid)
	}
	if err == nil {
		rec.Pgid, rec.StartID = pid, start
		if err = h.st.save(*rec); err == nil && register != nil {
			err = register(pid, start)
		}
	}
	if err == nil {
		_, err = io.WriteString(gateWrite, "go\n")
	}
	if err != nil {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		return err
	}
	if timeout > 0 {
		h.sup.Add(1)
		go h.enforceTimeout(p, timeout)
	}
	return nil
}

// enforceTimeout terminates the process group of a workload that outlives its
// declared timeout: SIGTERM, then SIGKILL after the grace.
func (h *host) enforceTimeout(p *kproc, timeout time.Duration) {
	defer h.sup.Done()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-p.done:
		return
	case <-t.C:
	}
	p.timedOut.Store(true)
	_ = syscall.Kill(-p.pgid, syscall.SIGTERM)
	g := time.NewTimer(h.cfg.KillGrace)
	defer g.Stop()
	select {
	case <-p.done:
	case <-g.C:
		_ = syscall.Kill(-p.pgid, syscall.SIGKILL)
	}
}

// supervise waits for the leader. Like a container, a workload ends with its
// first process: the rest of its group is killed then.
func (h *host) supervise(typ, key string, p *kproc, cmd *exec.Cmd) {
	defer h.sup.Done()
	err := cmd.Wait()
	p.exit = err
	close(p.done)
	// The leader's status is collected; the group is a separate question. Kill
	// what it left and wait, bounded, for the group to be verifiably gone before
	// anything is recorded as released.
	deadline := time.Now().Add(h.cfg.KillGrace)
	for {
		h.killOnExit(p.pgid)
		if alive, _ := memberAlive(p.pgid); !alive || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.noteExit(typ, key)
}

// memberAlive reports whether any process of the group exists. Only when the
// group is gone is it verified absent; a group of another user is not ours.
func memberAlive(pgid int) (bool, error) {
	if pgid <= 1 {
		return false, nil
	}
	err := syscall.Kill(-pgid, 0)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.ESRCH), errors.Is(err, syscall.EPERM):
		return false, nil
	}
	return false, fmt.Errorf("process: probing process group %d: %w", pgid, err)
}

// noteExit records the exit of a launched process, unless a Stop did.
func (h *host) noteExit(typ, key string) {
	unlock := h.st.lock(typ, key)
	defer unlock()
	rec, found, err := h.st.load(typ, key)
	if err != nil || !found {
		return
	}
	if h.settle(&rec) {
		if err := h.st.save(rec); err != nil {
			return // the evidence stays in memory; the next operation saves it
		}
	}
	h.releaseHolder(rec)
}

// settle records, once, how the leader of a process this instance launched
// ended. A leader that ended after the stop intent was recorded was
// cancelled; one that ended before it ended on its own. The caller holds the
// object's lock, so exactly one of exit and stop decides, and the decision is
// replayed if the record that carries it could not be saved. It records the
// leader's result only: whether the group is gone, and so whether the
// workspace is released, is groupGone's question. It reports whether the
// record changed.
func (h *host) settle(rec *kRecord) bool {
	if rec.ExitKnown {
		// Durable (every caller saves a change before it asks again): the
		// in-memory evidence is no longer needed.
		h.forget(rec.Type, rec.Key)
		return false
	}
	if rec.Pgid == 0 {
		return false
	}
	p := h.proc(rec.Type, rec.Key)
	if p == nil || !p.ended() {
		return false
	}
	ev := p.evidence(rec.Stopping)
	rec.ExitKnown, rec.ExitCode, rec.TimedOut, rec.Cancelled = true, ev.code, ev.timedOut, ev.cancelled
	return true
}

// awaitLeader waits, bounded, for the supervisor to collect the status of the
// leader of a process this instance launched whose group is already gone.
func (h *host) awaitLeader(rec kRecord) {
	p := h.proc(rec.Type, rec.Key)
	if p == nil {
		return
	}
	t := time.NewTimer(5 * time.Second)
	defer t.Stop()
	select {
	case <-p.done:
	case <-t.C:
	}
}

// dropWorkloadHolder releases the workspace the workload held. Callers verified
// that its process group is gone.
func (h *host) dropWorkloadHolder(rec kRecord) {
	entries, err := os.ReadDir(filepath.Join(string(h.st), kHoldersDir))
	if err != nil {
		return
	}
	for _, e := range entries {
		h.st.dropHolder(e.Name(), rec.Key)
	}
}

// releaseHolder drops the holder marker of a workload whose process group is
// verifiably gone, never before: surviving descendants still hold the workspace.
func (h *host) releaseHolder(rec kRecord) {
	if rec.Type != typeWorkload || rec.Pgid == 0 {
		return
	}
	if gone, err := h.groupGone(rec); err == nil && gone {
		h.dropWorkloadHolder(rec)
	}
}

// groupGone reports whether the record's process group is verifiably gone: no
// member remains, not merely that the leader ended. A process this instance
// launched is probed by its group; an adopted one is verified by the leader's
// recorded start time, so a recycled pid is never mistaken for it.
func (h *host) groupGone(rec kRecord) (bool, error) {
	if p := h.proc(rec.Type, rec.Key); p != nil {
		alive, err := memberAlive(p.pgid)
		return !alive, err
	}
	if rec.Pgid == 0 {
		return true, nil
	}
	alive, err := groupAlive(rec.Pgid, rec.StartID)
	return !alive, err
}

// terminate ends the process group of the record: SIGTERM, then SIGKILL after
// the grace, and returns only when the group is verifiably gone. The group is
// signalled only while its recorded identity matches.
func (h *host) terminate(ctx context.Context, rec kRecord) error {
	pgid := rec.Pgid
	if p := h.proc(rec.Type, rec.Key); p != nil {
		pgid = p.pgid
	}
	if pgid <= 1 {
		return nil
	}
	done, err := h.groupGone(rec)
	if err != nil || done {
		return err
	}
	signal := func(sig syscall.Signal, wait time.Duration) (bool, error) {
		_ = syscall.Kill(-pgid, sig)
		deadline := time.NewTimer(wait)
		defer deadline.Stop()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			if done, err := h.groupGone(rec); err != nil || done {
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
	if done, err := signal(syscall.SIGTERM, h.cfg.KillGrace); err != nil || done {
		return err
	}
	if done, err := signal(syscall.SIGKILL, 10*time.Second); err != nil {
		return err
	} else if !done {
		return fmt.Errorf("process group %d of %s did not end", pgid, rec.ID)
	}
	return nil
}

// stopLocked stops a workload or service: it records the stop intent, which
// fences any later start, terminates the group and records the verified stop.
// A leader that ended on its own before the intent keeps its own result. The
// caller holds the lock.
func (h *host) stopLocked(ctx context.Context, rec *kRecord) error {
	if rec.Stopped {
		return nil
	}
	if h.settle(rec) {
		if err := h.st.save(*rec); err != nil {
			return err
		}
	}
	if !rec.Stopping {
		rec.Stopping = true
		if err := h.st.save(*rec); err != nil {
			return err
		}
	}
	if rec.Pgid != 0 {
		if err := h.terminate(ctx, *rec); err != nil {
			return err
		}
		h.awaitLeader(*rec)
		h.settle(rec)
		if !rec.ExitKnown {
			// An adopted group ended without an exit status anyone saw.
			rec.ExitKnown, rec.Cancelled = true, true
		}
	}
	rec.Stopped = true
	if err := h.st.save(*rec); err != nil {
		return err
	}
	if rec.Pgid != 0 {
		h.releaseHolder(*rec) // terminate verified the group gone
		h.forget(rec.Type, rec.Key)
	}
	return nil
}

// ---- observing a workload ----

// resultOf maps a recorded exit to the kernel result.
func resultOf(rec kRecord) kr.Result {
	switch {
	case rec.TimedOut:
		return kr.Result{Code: kr.ResultTimedOut, Reason: kr.ReasonStepTimeout}
	case rec.Cancelled:
		return kr.Result{Code: kr.ResultCancelled, Reason: kr.ReasonCancelled}
	case rec.ExitCode == 0:
		return kr.Result{Code: kr.ResultSucceeded}
	}
	return kr.Result{Code: kr.ResultFailed, ExitCode: rec.ExitCode, Reason: kr.ReasonCommandFailed}
}

// observeWorkload describes the record's workload. The caller holds the lock;
// an exit this instance saw is persisted first.
func (h *host) observeWorkload(b kr.RuntimeBinding, rec *kRecord) (kr.WorkloadObservation, error) {
	obs := kr.WorkloadObservation{Binding: b, ObservedAt: h.now()}
	if h.settle(rec) {
		if err := h.st.save(*rec); err != nil {
			return kr.WorkloadObservation{}, err
		}
	}
	switch {
	case rec.Removed || rec.Untracked:
		obs.Phase = kr.PhaseAbsent
	case rec.Pgid == 0 && rec.Stopped:
		obs.Phase = kr.PhaseAbsent // stopped before any process existed: it never ran
	case rec.Pgid == 0:
		// Announced, no group ever recorded: the gate was never opened, so the
		// command cannot run, now or later.
		obs.Phase, obs.Result = kr.PhaseExited, kr.Result{Code: kr.ResultLost, Reason: kr.ReasonLaunchNotReached}
	default:
		// The workload has ended only when its whole group is gone, whatever the
		// leader's status says.
		gone, err := h.groupGone(*rec)
		switch {
		case err != nil:
			obs.Phase = kr.PhaseUnknown
		case !gone:
			obs.Phase = kr.PhaseRunning
		default:
			if !rec.ExitKnown {
				// A leader this instance launched is reaped just before its status is
				// collected: wait for it rather than call the workload lost.
				h.awaitLeader(*rec)
				if h.settle(rec) {
					if err := h.st.save(*rec); err != nil {
						return kr.WorkloadObservation{}, err
					}
				}
			}
			h.releaseHolder(*rec)
			if rec.ExitKnown {
				obs.Phase, obs.Result = kr.PhaseExited, resultOf(*rec)
			} else {
				// It ended while no adapter watched: its status is unknown.
				obs.Phase, obs.Result = kr.PhaseExited, kr.Result{Code: kr.ResultLost, Reason: kr.ReasonWorkloadLost}
			}
		}
	}
	return obs, nil
}

// Observe implements kr.Runtime. It never creates. A workload that is not
// recorded for this tenant and project, or whose removal was confirmed, is
// absent; only an inability to determine the state is unknown.
func (r *KernelRuntime) Observe(_ context.Context, b kr.RuntimeBinding) (kr.WorkloadObservation, error) {
	h := r.h
	if err := b.Validate(); err != nil {
		return kr.WorkloadObservation{}, err
	}
	if err := checkBackend(b); err != nil {
		return kr.WorkloadObservation{}, err
	}
	key := objectKey(b.Scope, typeWorkload, b.WorkloadID)
	unlock := h.st.lock(typeWorkload, key)
	defer unlock()
	rec, found, err := h.st.load(typeWorkload, key)
	switch {
	case err != nil:
		return kr.WorkloadObservation{Binding: b, Phase: kr.PhaseUnknown, ObservedAt: h.now()}, nil
	case !found:
		return kr.WorkloadObservation{Binding: b, Phase: kr.PhaseAbsent, ObservedAt: h.now()}, nil
	}
	if err := checkWorkload(rec, b); err != nil {
		return kr.WorkloadObservation{}, err
	}
	return h.observeWorkload(b, &rec)
}

// Stop implements kr.Runtime. The stop intent is recorded first, so a start
// that follows is refused and a launch in progress finishes before the stop is
// decided (both take the workload's lock); only then is the group terminated.
// A stop from a stale fence is refused. A duplicate stop returns the recorded
// outcome. A workload that ended on its own before the stop keeps its own
// result. A stop of a workload nobody recorded leaves a tombstone that fences a
// late start and returns PhaseAbsent.
func (r *KernelRuntime) Stop(ctx context.Context, b kr.RuntimeBinding) (kr.WorkloadObservation, error) {
	h := r.h
	if err := b.Validate(); err != nil {
		return kr.WorkloadObservation{}, err
	}
	if err := checkBackend(b); err != nil {
		return kr.WorkloadObservation{}, notReached("%w", err)
	}
	key := objectKey(b.Scope, typeWorkload, b.WorkloadID)
	unlock := h.st.lock(typeWorkload, key)
	defer unlock()
	rec, err := h.tombstoneOr(typeWorkload, key, b, false)
	if err != nil {
		return kr.WorkloadObservation{}, err
	}
	if err := h.admitFence(&rec, b.Fence); err != nil {
		return kr.WorkloadObservation{}, err
	}
	if !rec.Removed {
		if err := h.stopLocked(ctx, &rec); err != nil {
			return kr.WorkloadObservation{Binding: b, Phase: kr.PhaseUnknown, ObservedAt: h.now()},
				fmt.Errorf("%w: stopping workload %s: %w", kr.ErrUncertain, b.WorkloadID, err)
		}
	}
	return h.observeWorkload(b, &rec)
}

// Remove implements kr.Runtime: it stops the workload, deletes its files and
// confirms with PhaseAbsent. The tombstone stays, so a delayed start of the
// same identity is refused.
func (r *KernelRuntime) Remove(ctx context.Context, b kr.RuntimeBinding) (kr.WorkloadObservation, error) {
	h := r.h
	if err := b.Validate(); err != nil {
		return kr.WorkloadObservation{}, err
	}
	if err := checkBackend(b); err != nil {
		return kr.WorkloadObservation{}, notReached("%w", err)
	}
	key := objectKey(b.Scope, typeWorkload, b.WorkloadID)
	unlock := h.st.lock(typeWorkload, key)
	defer unlock()
	rec, err := h.tombstoneOr(typeWorkload, key, b, true)
	if err != nil {
		return kr.WorkloadObservation{}, err
	}
	if err := h.admitFence(&rec, b.Fence); err != nil {
		return kr.WorkloadObservation{}, err
	}
	if err := h.removeLocked(ctx, &rec); err != nil {
		return kr.WorkloadObservation{Binding: b, Phase: kr.PhaseUnknown, ObservedAt: h.now()},
			fmt.Errorf("%w: removing workload %s: %w", kr.ErrUncertain, b.WorkloadID, err)
	}
	return h.observeWorkload(b, &rec)
}

// removeLocked stops the object, deletes its files and records the removal.
func (h *host) removeLocked(ctx context.Context, rec *kRecord) error {
	if rec.Removed {
		return h.st.save(*rec)
	}
	if err := h.stopLocked(ctx, rec); err != nil {
		return err
	}
	if rec.Owned {
		if rec.Type == typeWorkspace && rec.Created {
			if _, err := h.st.verifyWorkspace(*rec); err != nil {
				return err
			}
		}
		if err := h.st.removeOwned(*rec); err != nil {
			return err
		}
	}
	if rec.Type == typeWorkspace {
		_ = os.RemoveAll(filepath.Join(string(h.st), kHoldersDir, rec.Key))
	}
	rec.Removed = true
	if err := h.st.save(*rec); err != nil {
		return err
	}
	h.forget(rec.Type, rec.Key)
	return nil
}

// tombstoneOr loads the record of an identity, or leaves the tombstone a stop or
// remove of an unrecorded identity records: it fences a late create and proves
// nothing about launch. A record of another run, step or attempt is a conflict.
func (h *host) tombstoneOr(typ, key string, b kr.RuntimeBinding, removed bool) (kRecord, error) {
	rec, found, err := h.st.load(typ, key)
	if err != nil {
		return kRecord{}, err
	}
	if found {
		return rec, checkWorkload(rec, b)
	}
	rec = kRecord{
		Type: typ, Key: key, Scope: b.Scope, RunID: b.RunID, StepKey: b.StepKey, Attempt: b.Attempt, ID: b.WorkloadID,
		Digest: b.SpecDigest, Fence: b.Fence, Untracked: true, Stopping: true, Stopped: true, Removed: removed,
	}
	if err := h.st.create(rec); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return h.tombstoneOr(typ, key, b, removed)
		}
		return kRecord{}, err
	}
	return rec, nil
}

// ---- logs ----

// Logs implements kr.Runtime: the bounded output of the workload so far, or,
// with Follow, until its process group ended.
func (r *KernelRuntime) Logs(_ context.Context, b kr.RuntimeBinding, opts kr.LogOptions) (io.ReadCloser, error) {
	h := r.h
	if err := b.Validate(); err != nil {
		return nil, err
	}
	if err := checkBackend(b); err != nil {
		return nil, err
	}
	key := objectKey(b.Scope, typeWorkload, b.WorkloadID)
	unlock := h.st.lock(typeWorkload, key)
	rec, found, err := h.st.load(typeWorkload, key)
	unlock()
	switch {
	case err != nil:
		return nil, err
	case !found || rec.Removed || rec.Untracked:
		return nil, notFound("no workload %s", b.WorkloadID)
	}
	if err := checkWorkload(rec, b); err != nil {
		return nil, err
	}
	return h.openLog(rec, opts)
}

func (h *host) openLog(rec kRecord, opts kr.LogOptions) (io.ReadCloser, error) {
	f, err := os.Open(h.st.logPath(rec.Type, rec.Key))
	if errors.Is(err, fs.ErrNotExist) {
		return io.NopCloser(strings.NewReader("")), nil
	}
	if err != nil {
		return nil, err
	}
	fr := &followReader{f: f, stop: make(chan struct{}), left: h.cfg.MaxLogBytes}
	if opts.Follow {
		fr.done = func() bool {
			gone, err := h.groupGone(rec)
			return err != nil || gone
		}
	}
	return fr, nil
}

// followReader reads a log file up to a bound. With done set it waits for more
// output until the process group ended.
type followReader struct {
	f    *os.File
	done func() bool
	stop chan struct{}
	once sync.Once
	left int
	cut  bool
}

func (r *followReader) Read(p []byte) (int, error) {
	if r.left <= 0 {
		return r.truncated(p)
	}
	if len(p) > r.left {
		p = p[:r.left]
	}
	for {
		n, err := r.f.Read(p)
		if n > 0 {
			r.left -= n
			return n, nil
		}
		if err == nil || !errors.Is(err, io.EOF) {
			return 0, err
		}
		if r.done == nil {
			return 0, io.EOF
		}
		if r.done() {
			if n, err = r.f.Read(p); n > 0 {
				r.left -= n
				return n, nil
			}
			return 0, io.EOF
		}
		select {
		case <-r.stop:
			return 0, io.EOF
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// truncated reports, once, that output beyond the bound was cut.
func (r *followReader) truncated(p []byte) (int, error) {
	if r.cut {
		return 0, io.EOF
	}
	r.cut = true
	var probe [1]byte
	if n, _ := r.f.Read(probe[:]); n == 0 {
		return 0, io.EOF
	}
	return copy(p, "\n[kodama: output truncated]\n"), nil
}

func (r *followReader) Close() error {
	r.once.Do(func() { close(r.stop) })
	return r.f.Close()
}

// ---- reconciliation ----

// ReconcileReport lists what Reconcile did, by stable identity.
type ReconcileReport struct {
	// Removed were stopped and removed.
	Removed []string
	// Retained could not be stopped or removed and stay recorded; a later
	// Reconcile retries them.
	Retained []string
	// Skipped changed after they were selected as orphans (a writer took them
	// over with a newer fence, or the caller owns the run now) and were left
	// alone.
	Skipped []string
}

// Reconcile removes the workloads, services and workspaces whose run the caller
// no longer owns: orphans a crashed or superseded engine left behind. It stops
// each (workloads and services first, so a workspace is released) and removes
// it. Its authority is the record it observed when it selected the orphan: under
// the object's lock the record must still be that instance at that fence, so an
// object an engine adopted or took over in between (its fence advanced) is
// skipped, never stopped. Objects of an owned run, tombstones and directories
// that are not recorded here are left alone; a failure retains the object and
// the next call retries.
func (k *Kernel) Reconcile(ctx context.Context, owned func(domain.Scope, domain.RunID) bool) (ReconcileReport, error) {
	h := k.h
	recs, err := h.st.list()
	if err != nil {
		return ReconcileReport{}, err
	}
	var rep ReconcileReport
	for _, typ := range []string{typeWorkload, typeService, typeWorkspace} {
		for _, rec := range recs {
			if rec.Type != typ || rec.Removed || owned(rec.Scope, rec.RunID) {
				continue
			}
			switch err := h.reconcileOne(ctx, rec, owned); {
			case errors.Is(err, errChanged):
				rep.Skipped = append(rep.Skipped, rec.ID)
			case err != nil:
				rep.Retained = append(rep.Retained, rec.ID)
			default:
				rep.Removed = append(rep.Removed, rec.ID)
			}
		}
	}
	return rep, nil
}

// errChanged reports an orphan whose record is no longer the one that was
// selected.
var errChanged = errors.New("process: the object changed after it was selected")

func (h *host) reconcileOne(ctx context.Context, rec kRecord, owned func(domain.Scope, domain.RunID) bool) error {
	unlock := h.st.lock(rec.Type, rec.Key)
	defer unlock()
	cur, found, err := h.st.load(rec.Type, rec.Key)
	if err != nil || !found || cur.Removed {
		return err
	}
	if cur.UID != rec.UID || cur.Fence != rec.Fence || cur.Scope != rec.Scope || cur.RunID != rec.RunID || owned(cur.Scope, cur.RunID) {
		return errChanged
	}
	if cur.Type == typeWorkspace {
		held, ok, err := h.st.holder(cur.Key)
		if err != nil {
			return err
		}
		if ok {
			return fmt.Errorf("%w: held by %s", ErrWorkspaceBusy, held.WorkloadID)
		}
	}
	return h.removeLocked(ctx, &cur)
}

// WorkspaceHolder reports which workload of the run holds the workspace: the
// observable end of the exclusion. held is false once the holder's process
// group is verifiably gone.
func (r *KernelResources) WorkspaceHolder(_ context.Context, w kr.ResourceHandle) (workloadID string, held bool, err error) {
	if err := w.Validate(); err != nil {
		return "", false, err
	}
	if w.Kind != kr.ResourceWorkspace {
		return "", false, fmt.Errorf("%w: %s is not a workspace", domain.ErrInvalidValue, w.Name)
	}
	hld, ok, err := r.h.st.holder(objectKey(w.Scope, typeWorkspace, w.Identity))
	return hld.WorkloadID, ok, err
}

// portReady reports whether a TCP port accepts connections on the host.
func portReady(ctx context.Context, port int) bool {
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
