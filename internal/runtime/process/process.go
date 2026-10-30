// Package process is the test runtime that runs attempt workloads as host
// processes. It isolates nothing, runs no image and enforces no limits, so its
// Capabilities report no guarantee: only runs admitted under an explicit
// no-requirements policy (runtimetest.NoRequirements) are claimed for it, and
// it never claims image parity.
//
// What runs is an explicit Program the test chooses: DeclaredCommand runs the
// pinned context's command on the host; Entrypoint runs the real step
// entrypoint, which is admitted, runs the command and submits the result
// through the runtime API as it does in a container.
//
// State lives in memory: a restarted runtime knows none of its workloads,
// which is why it does not claim CrashRecovery.
//
// Run resources (T-115): Resources implements execution.RunResources with a
// run-owned workspace directory shared by successive run_shared attempts
// (Config.ResourcesDir; the attempt sees it as its /workspace equivalent through
// Launch.Workspace and KODAMA_WORKSPACE_DIR), explicit host-program preparation
// and services (ResourceProgram) with tcp and exec readiness, durable on-disk
// fences and records that a restarted adapter adopts, a launch gate (a workload
// runs nothing until the adapter has recorded its process group, so no
// unrecorded process can run a command), and S5 admission credentials. It advertises lifecycle members only (ResourceCapabilities) and
// never isolation, network, egress or builder members, and the runtime still
// lacks network_policy: no run_shared admission accepts Process; the contract
// suite exercises the adapter directly.
package process

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/runtime/bootstrap"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// Backend is the identity of the process runtime.
const Backend = "process"

// Namespace is where runtime admission expects this runtime's workloads.
const Namespace = "kodama-process"

// Program decides the host command that stands in for an attempt's workload.
// It is called once per launch, before the process starts, and may provision
// the workload's private directory.
type Program interface {
	Command(ctx context.Context, l Launch) (Command, error)
}

// ProgramFunc adapts a function to Program.
type ProgramFunc func(ctx context.Context, l Launch) (Command, error)

// Command implements Program.
func (f ProgramFunc) Command(ctx context.Context, l Launch) (Command, error) { return f(ctx, l) }

// Launch is the workload a Program is asked for.
type Launch struct {
	Spec     execution.AttemptSpec
	Context  runtimev1.Context
	Workload execution.Workload
	// Dir is the workload's private directory, removed with it.
	Dir string
	// Workspace is the run workspace directory of a run_shared attempt (its
	// /workspace equivalent); "" for an attempt-private workload.
	Workspace string
}

// Command is a host command. Path without a separator is looked up in PATH;
// Dir defaults to the workload's directory.
type Command struct {
	Path string
	Args []string
	Env  []string
	Dir  string
}

// Config configures a Runtime.
type Config struct {
	// Program decides what runs. Required.
	Program Program
	// Dir holds one private directory per workload. Required.
	Dir string
	// KillGrace is how long Stop waits between SIGTERM and SIGKILL (default 2s).
	KillGrace time.Duration
	// DrainTimeout bounds how long output a descendant that escaped the
	// workload's process group still holds open is read after the workload
	// ended (default 1s). Output cut off then is marked incomplete.
	DrainTimeout time.Duration
	// MaxLogBytes bounds the captured output of each workload (default 1 MiB).
	MaxLogBytes int
	// ResourcesDir is the state directory of the run resources the attempts of
	// run_shared runs share (ResourcesConfig.Dir). When set, an attempt whose
	// spec carries a SharedLaunch runs with the run's workspace directory as its
	// /workspace equivalent and the Runtime declares the run-resources marker;
	// when empty such an attempt is refused (execution.ErrNotConfigured).
	ResourcesDir string
}

// Runtime implements execution.Runtime with host processes, and
// runtimeauth.JobObserver for the workloads it runs.
type Runtime struct {
	cfg Config

	mu        sync.Mutex
	workloads map[string]*workload

	res store // the run resources' state; "" without ResourcesDir
}

var (
	_ execution.Runtime       = (*Runtime)(nil)
	_ runtimeauth.JobObserver = (*Runtime)(nil)
)

// New validates the configuration.
func New(cfg Config) (*Runtime, error) {
	if cfg.Program == nil || cfg.Dir == "" {
		return nil, fmt.Errorf("%w: the process runtime needs a program and a directory", execution.ErrNotConfigured)
	}
	dir, err := filepath.Abs(cfg.Dir)
	if err != nil {
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
	rt := &Runtime{cfg: cfg, workloads: map[string]*workload{}}
	if cfg.ResourcesDir != "" {
		res, err := filepath.Abs(cfg.ResourcesDir)
		if err != nil {
			return nil, err
		}
		rt.res = store(res)
	}
	return rt, nil
}

// workload is one attempt's workload, or the tombstone Stop or Remove leaves
// so that a later Start never launches it. Fields are guarded by Runtime.mu.
type workload struct {
	handle execution.Workload
	spec   *execution.AttemptSpec // nil for a tombstone of a never-started workload
	dir    string
	// workspace is the run workspace directory of a run_shared attempt.
	workspace string

	// resolved is closed once Start launched the process or gave up.
	resolved chan struct{}
	launched bool
	stopped  bool
	removed  bool

	pgid int
	out  *capture
	// done is closed once the process exited and its output was drained;
	// exit is Wait's result.
	done chan struct{}
	exit error
}

func (w *workload) ended() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

// Capabilities implements execution.Runtime: no container guarantee. A stop
// before the start is fenced in this process, so it proves the command never
// started.
//
// With ResourcesDir the Runtime also declares the run-resources marker (it
// pairs with Resources); that adds no isolation, network or builder guarantee,
// and the missing network_policy keeps every run_shared admission away from it.
func (r *Runtime) Capabilities() execution.Capabilities {
	c := execution.Capabilities{Backend: Backend, ProvesNeverStarted: true}
	if r.res != "" {
		c.Guarantees = c.Guarantees.WithRunResources()
	}
	return c
}

// Start implements execution.Runtime. A repeated Start of the same spec waits
// for a concurrent one and adopts its workload; a stopped or removed workload
// is never relaunched (ErrConflict).
func (r *Runtime) Start(ctx context.Context, spec execution.AttemptSpec) (execution.Workload, error) {
	c, err := spec.DecodeContext()
	if err != nil {
		return execution.Workload{}, err
	}
	var workspace string
	if spec.Shared != nil {
		// The run workspace is verified and fenced against its Stop for the whole
		// start: a Stop of the workspace waits for the launch decision, and a
		// launch that follows a confirmed Stop reads the fence.
		var unlock func()
		if workspace, unlock, err = r.holdWorkspace(spec); err != nil {
			return execution.Workload{}, err
		}
		defer unlock()
	}
	for {
		r.mu.Lock()
		w, ok := r.workloads[spec.WorkloadName]
		if !ok {
			w = &workload{
				handle: spec.Workload(newUID()), spec: &spec, dir: filepath.Join(r.cfg.Dir, spec.WorkloadName), workspace: workspace,
				resolved: make(chan struct{}), done: make(chan struct{}), out: &capture{max: r.cfg.MaxLogBytes},
			}
			r.workloads[spec.WorkloadName] = w
			r.mu.Unlock()
			return r.launch(ctx, w, c)
		}
		err := adoptable(w, spec)
		resolved := w.resolved
		r.mu.Unlock()
		if err != nil {
			return execution.Workload{}, err
		}
		select {
		case <-resolved:
		case <-ctx.Done():
			return execution.Workload{}, ctx.Err()
		}
		r.mu.Lock()
		launched := w.launched
		r.mu.Unlock()
		if launched {
			return w.handle, nil
		}
		// The concurrent start gave up: decide again.
	}
}

func adoptable(w *workload, spec execution.AttemptSpec) error {
	switch {
	case w.stopped || w.removed:
		return fmt.Errorf("%w: the workload was stopped and is never relaunched", execution.ErrConflict)
	case w.spec.Scope != spec.Scope || w.spec.AttemptID != spec.AttemptID || w.spec.RunID != spec.RunID ||
		w.spec.StepID != spec.StepID || w.spec.SpecFingerprint != spec.SpecFingerprint || w.spec.ContextDigest != spec.ContextDigest:
		return fmt.Errorf("%w: the workload name holds another spec", execution.ErrConflict)
	}
	return nil
}

// launch asks the program for the command and starts it, unless the workload
// was stopped meanwhile. A failed launch is forgotten, so a retry starts
// afresh, but the directory stays: what the program provisioned there (an
// admission credential) is reused.
func (r *Runtime) launch(ctx context.Context, w *workload, c runtimev1.Context) (execution.Workload, error) {
	cmd, err := r.command(ctx, w, c)
	r.mu.Lock()
	defer r.mu.Unlock()
	defer close(w.resolved)
	switch {
	case w.stopped:
		return execution.Workload{}, fmt.Errorf("%w: the workload was stopped before it launched", execution.ErrConflict)
	case err == nil:
		if w.workspace != "" {
			// A shared attempt uses the run workspace: it is launched behind the
			// gate (see gate) and its process group is recorded as a holder before
			// the gate opens, so a controller that dies mid-launch leaves either a
			// recorded holder or a process that can never run the command.
			err = r.exec(w, gate(cmd), func(pid int, start string) error {
				return r.res.saveHolder(w.spec.Shared.Workspace.Name, holder{Workload: w.handle.Name, Pgid: pid, StartID: start})
			})
			if err != nil {
				r.withdrawHolder(w)
			}
		} else {
			err = r.exec(w, cmd, nil)
		}
	}
	if err != nil {
		delete(r.workloads, w.handle.Name)
		return execution.Workload{}, fmt.Errorf("%w: launching the workload: %w", execution.ErrUnavailable, err)
	}
	w.launched = true
	return w.handle, nil
}

func (r *Runtime) command(ctx context.Context, w *workload, c runtimev1.Context) (Command, error) {
	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		return Command{}, err
	}
	return r.cfg.Program.Command(ctx, Launch{Spec: *w.spec, Context: c, Workload: w.handle, Dir: w.dir, Workspace: w.workspace})
}

// exec starts the command in its own process group with its output captured.
// It runs under r.mu, so no Stop can slip between the decision and the launch.
// With register, the command is gated: register records the process group and
// only then is the gate opened; any failure before closes it, and the process
// exits without running the command.
func (r *Runtime) exec(w *workload, c Command, register func(pid int, start string) error) error {
	out, in, err := os.Pipe()
	if err != nil {
		return err
	}
	var gateRead, gateWrite *os.File
	if register != nil {
		if gateRead, gateWrite, err = os.Pipe(); err != nil {
			out.Close()
			in.Close()
			return err
		}
	}
	cmd := exec.Command(c.Path, c.Args...)
	cmd.Env, cmd.Dir = c.Env, c.Dir
	if cmd.Dir == "" {
		cmd.Dir = w.dir
	}
	cmd.Stdout, cmd.Stderr = in, in
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if gateRead != nil {
		cmd.ExtraFiles = []*os.File{gateRead}
	}
	err = cmd.Start()
	in.Close()
	if gateRead != nil {
		gateRead.Close()
	}
	if err != nil {
		out.Close()
		if gateWrite != nil {
			gateWrite.Close()
		}
		return err
	}
	w.pgid = cmd.Process.Pid
	go r.supervise(w, cmd, out)
	if register == nil {
		return nil
	}
	defer gateWrite.Close() // before the gate opens, closing it is the child's EOF
	start, gone, err := startOf(w.pgid)
	if err == nil && gone {
		err = fmt.Errorf("the gated process %d is gone", w.pgid)
	}
	if err == nil {
		err = register(w.pgid, start)
	}
	if err == nil {
		_, err = io.WriteString(gateWrite, "go\n")
	}
	if err != nil {
		_ = syscall.Kill(-w.pgid, syscall.SIGKILL)
	}
	return err
}

// supervise waits for the workload and drains its output before reporting
// the exit. Like a container, a workload ends with its first process: the
// rest of its process group is killed then. Output a descendant that escaped
// the group still holds open is read for DrainTimeout and then cut off and
// marked incomplete.
func (r *Runtime) supervise(w *workload, cmd *exec.Cmd, out *os.File) {
	drained := make(chan struct{})
	go func() {
		_, _ = io.Copy(w.out, out)
		close(drained)
	}()
	err := cmd.Wait()
	_ = syscall.Kill(-w.pgid, syscall.SIGKILL)
	timer := time.NewTimer(r.cfg.DrainTimeout)
	select {
	case <-drained:
		timer.Stop()
	case <-timer.C:
		out.Close() // unblocks the copy
		<-drained
		w.out.markIncomplete()
	}
	out.Close()
	r.mu.Lock()
	w.exit = err
	r.mu.Unlock()
	close(w.done)
}

// find returns the workload a handle names: ErrNotFound when there is none
// (or only a tombstone), ErrConflict when the handle names another attempt or
// instance. It runs under r.mu.
func (r *Runtime) find(h execution.Workload) (*workload, error) {
	w, ok := r.workloads[h.Name]
	if !ok || w.removed || (w.stopped && !w.launched && w.spec == nil) {
		return nil, fmt.Errorf("%w: no workload %s", execution.ErrNotFound, h.Name)
	}
	if err := w.matches(h); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *workload) matches(h execution.Workload) error {
	if w.handle.Scope != h.Scope || w.handle.AttemptID != h.AttemptID || (h.UID != "" && w.handle.UID != "" && h.UID != w.handle.UID) {
		return fmt.Errorf("%w: the workload is another attempt's or instance's", execution.ErrConflict)
	}
	return nil
}

// Observe implements execution.Runtime. A process that exited non-zero
// without being stopped failed; one that exited zero only ran: success is an
// accepted result, never an exit code.
func (r *Runtime) Observe(_ context.Context, h execution.Workload) (execution.Observation, error) {
	if err := h.Validate(); err != nil {
		return execution.Observation{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	w, err := r.find(h)
	if err != nil {
		return execution.Observation{}, err
	}
	if !w.launched && w.stopped {
		return execution.Observation{}, fmt.Errorf("%w: the workload never launched", execution.ErrNotFound)
	}
	obs := execution.Observation{
		Kind: execution.ObservedDispatched, WorkloadName: w.handle.Name, SpecFingerprint: w.spec.SpecFingerprint,
		WorkloadUID: w.handle.UID, ObservedAt: time.Now().UTC(),
	}
	switch {
	case !w.launched:
	case w.ended() && w.exit != nil && !w.stopped:
		obs.Kind, obs.Class, obs.Reason, obs.Launch = execution.ObservedFailed, execution.ClassInfrastructure, execution.ReasonWorkloadFailed, execution.LaunchStarted
	default:
		obs.Kind, obs.Launch = execution.ObservedStarted, execution.LaunchStarted
	}
	return obs, nil
}

// Logs implements execution.Runtime with the captured output so far.
func (r *Runtime) Logs(_ context.Context, h execution.Workload, dst io.Writer) error {
	if err := h.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	w, err := r.find(h)
	r.mu.Unlock()
	if err != nil {
		return err
	}
	_, err = dst.Write(w.out.bytes())
	return err
}

// Stop implements execution.Runtime. The workload is marked stopped first, so
// a pending launch gives up; Stop then waits until no launch can arrive and
// terminates the process group with SIGTERM, then SIGKILL after KillGrace.
// Stopping a workload that never existed leaves a tombstone.
func (r *Runtime) Stop(ctx context.Context, h execution.Workload) (execution.StopEvidence, error) {
	if err := h.Validate(); err != nil {
		return execution.StopEvidence{}, err
	}
	r.mu.Lock()
	w, ok := r.workloads[h.Name]
	if !ok {
		w = &workload{handle: h, resolved: make(chan struct{}), done: make(chan struct{}), out: &capture{}}
		close(w.resolved)
		r.workloads[h.Name] = w
	} else if err := w.matches(h); err != nil {
		r.mu.Unlock()
		return execution.StopEvidence{}, err
	}
	w.stopped = true
	resolved := w.resolved
	r.mu.Unlock()

	select {
	case <-resolved:
	case <-ctx.Done():
		return execution.StopEvidence{Launch: execution.LaunchUnknown}, ctx.Err()
	}
	r.mu.Lock()
	launched := w.launched
	r.mu.Unlock()
	if !launched {
		return execution.StopEvidence{Stopped: true, Launch: execution.LaunchNotStarted}, nil
	}
	if err := r.terminate(ctx, w); err != nil {
		return execution.StopEvidence{Launch: execution.LaunchStarted}, err
	}
	if w.workspace != "" {
		// The attempt's process is verified gone: it holds the workspace no more.
		r.withdrawHolder(w)
	}
	return execution.StopEvidence{Stopped: true, Launch: execution.LaunchStarted}, nil
}

func (r *Runtime) terminate(ctx context.Context, w *workload) error {
	if w.ended() {
		return nil
	}
	_ = syscall.Kill(-w.pgid, syscall.SIGTERM)
	grace := time.NewTimer(r.cfg.KillGrace)
	defer grace.Stop()
	select {
	case <-w.done:
		return nil
	case <-grace.C:
	case <-ctx.Done():
		return ctx.Err()
	}
	_ = syscall.Kill(-w.pgid, syscall.SIGKILL)
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Remove implements execution.Runtime: it stops the workload, deletes its
// directory and keeps a tombstone.
func (r *Runtime) Remove(ctx context.Context, h execution.Workload) error {
	if _, err := r.Stop(ctx, h); err != nil {
		return err
	}
	r.mu.Lock()
	w := r.workloads[h.Name]
	w.removed = true
	dir := w.dir
	r.mu.Unlock()
	if dir == "" {
		return nil
	}
	return os.RemoveAll(dir)
}

// ObserveJob implements runtimeauth.JobObserver for the entrypoint of a
// running workload: one host process stands for both the Job and its Pod.
func (r *Runtime) ObserveJob(_ context.Context, obs runtimeauth.JobObservation) (runtimeauth.JobEvidence, error) {
	if obs.Expected.Namespace != Namespace {
		return runtimeauth.JobEvidence{Namespace: Namespace}, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.workloads[obs.Expected.Name]
	if !ok || w.removed || !w.launched || w.ended() {
		return runtimeauth.JobEvidence{}, runtimeauth.ErrNotObserved
	}
	if w.handle.Scope != obs.Scope || w.handle.AttemptID != obs.AttemptID {
		return runtimeauth.JobEvidence{Namespace: Namespace}, nil
	}
	return bootstrap.Evidence(Namespace, w.handle), nil
}

// Wait blocks until the workload's process exited and its output was
// drained. Tests use it instead of polling.
func (r *Runtime) Wait(ctx context.Context, h execution.Workload) error {
	r.mu.Lock()
	w, err := r.find(h)
	r.mu.Unlock()
	if err != nil {
		return err
	}
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// newUID returns a random version 4 UUID.
func newUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// capture keeps the first max bytes of a workload's output.
type capture struct {
	mu                    sync.Mutex
	buf                   []byte
	max                   int
	truncated, incomplete bool
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	room := c.max - len(c.buf)
	if len(p) > room {
		c.truncated = true
		c.buf = append(c.buf, p[:max(room, 0)]...)
		return len(p), nil
	}
	c.buf = append(c.buf, p...)
	return len(p), nil
}

func (c *capture) markIncomplete() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.incomplete = true
}

func (c *capture) bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := append([]byte(nil), c.buf...)
	if c.truncated {
		out = append(out, "\n[kodama: output truncated]\n"...)
	}
	if c.incomplete {
		out = append(out, "\n[kodama: output incomplete]\n"...)
	}
	return out
}

var errNoCommand = errors.New("the pinned context declares no command")
