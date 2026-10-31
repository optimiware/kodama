package runner_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runner"
	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/shell/runstate"
)

var (
	t0    = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	scope = domain.Scope{TenantID: "t1", ProjectID: "p1"}
)

const (
	leaseTTL = 30 * time.Second
	poll     = time.Second
)

// testClock is the one controllable clock of the engine, the state store and the
// fakes.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Fake runtime
// ---------------------------------------------------------------------------

// script says how a step's workload behaves: it reports running for polls
// observations, then exits with result (succeeded when zero).
type script struct {
	polls  int
	result runtime.Result
}

type fakeWorkload struct {
	b      runtime.RuntimeBinding
	phase  runtime.WorkloadPhase
	result runtime.Result
	left   int
}

// fakeRuntime is a controlled runtime.Runtime. It is idempotent on the workload
// identity, refuses a stale fence like a real adapter, and records every call so
// tests can prove what was and was not issued.
type fakeRuntime struct {
	mu    sync.Mutex
	clock *testClock
	caps  runtime.CapabilitySet

	workloads map[string]*fakeWorkload
	scripts   map[domain.StepID]script

	starts, stops, removes, observes []string
	live, maxLive                    int
	startOrder                       []domain.StepID

	// Hooks decide, per call, whether the effect is applied and what the engine
	// sees. A call that applies the effect and returns an error is a lost
	// response: the effect happened, the caller was not told.
	startHook   func(n int, req runtime.StartRequest) (apply bool, err error)
	stopHook    func(n int, b runtime.RuntimeBinding) (apply bool, err error)
	removeHook  func(n int, b runtime.RuntimeBinding) (apply bool, err error)
	observeHook func(n int, b runtime.RuntimeBinding) error
	onStart     func(req runtime.StartRequest)
	onEffect    func(kind string, b runtime.RuntimeBinding)
}

func newFakeRuntime(c *testClock) *fakeRuntime {
	return &fakeRuntime{clock: c, workloads: map[string]*fakeWorkload{}, scripts: map[domain.StepID]script{}}
}

func (f *fakeRuntime) Capabilities(context.Context) (runtime.CapabilitySet, error) {
	return f.caps, nil
}

func (f *fakeRuntime) obs(w *fakeWorkload, b runtime.RuntimeBinding) runtime.WorkloadObservation {
	return runtime.WorkloadObservation{Binding: b, Phase: w.phase, Result: w.result, ObservedAt: f.clock.Now()}
}

func (f *fakeRuntime) absent(b runtime.RuntimeBinding) runtime.WorkloadObservation {
	return runtime.WorkloadObservation{Binding: b, Phase: runtime.PhaseAbsent, ObservedAt: f.clock.Now()}
}

func (f *fakeRuntime) Start(_ context.Context, req runtime.StartRequest) (runtime.WorkloadObservation, error) {
	if f.onStart != nil {
		f.onStart(req)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	b := req.Binding
	f.starts = append(f.starts, b.WorkloadID)
	apply, err := true, error(nil)
	if f.startHook != nil {
		apply, err = f.startHook(len(f.starts), req)
	}
	if apply {
		if w, ok := f.workloads[b.WorkloadID]; ok {
			if w.b.SpecDigest != b.SpecDigest {
				return runtime.WorkloadObservation{}, runtime.ErrSpecMismatch
			}
		} else {
			sc := f.scripts[b.StepKey]
			f.workloads[b.WorkloadID] = &fakeWorkload{b: b, phase: runtime.PhaseRunning, left: sc.polls, result: sc.result}
			f.startOrder = append(f.startOrder, b.StepKey)
			f.live++
			f.maxLive = max(f.maxLive, f.live)
		}
	}
	if err != nil {
		return runtime.WorkloadObservation{}, err
	}
	return f.obs(f.workloads[b.WorkloadID], b), nil
}

func (f *fakeRuntime) Observe(_ context.Context, b runtime.RuntimeBinding) (runtime.WorkloadObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.observes = append(f.observes, b.WorkloadID)
	if f.observeHook != nil {
		if err := f.observeHook(len(f.observes), b); err != nil {
			return runtime.WorkloadObservation{}, err
		}
	}
	w, ok := f.workloads[b.WorkloadID]
	if !ok {
		return f.absent(b), nil
	}
	if w.phase == runtime.PhaseRunning {
		if w.left > 0 {
			w.left--
		} else {
			w.phase = runtime.PhaseExited
			if w.result == (runtime.Result{}) {
				w.result = runtime.Result{Code: runtime.ResultSucceeded}
			}
		}
	}
	return f.obs(w, b), nil
}

func (f *fakeRuntime) Logs(context.Context, runtime.RuntimeBinding, runtime.LogOptions) (io.ReadCloser, error) {
	return nil, errors.New("not used")
}

func (f *fakeRuntime) Stop(_ context.Context, b runtime.RuntimeBinding) (runtime.WorkloadObservation, error) {
	if f.onEffect != nil {
		f.onEffect("stop", b)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, b.WorkloadID)
	apply, err := true, error(nil)
	if f.stopHook != nil {
		apply, err = f.stopHook(len(f.stops), b)
	}
	w, ok := f.workloads[b.WorkloadID]
	if ok && b.Fence < w.b.Fence {
		return runtime.WorkloadObservation{}, fmt.Errorf("%w: stop from fence %d", runtime.ErrStaleFence, b.Fence)
	}
	if apply && ok && w.phase == runtime.PhaseRunning {
		w.phase, w.result = runtime.PhaseExited, runtime.Result{Code: runtime.ResultCancelled, Reason: runtime.ReasonCancelled}
	}
	if err != nil {
		return runtime.WorkloadObservation{}, err
	}
	if !ok {
		return f.absent(b), nil
	}
	return f.obs(w, b), nil
}

func (f *fakeRuntime) Remove(_ context.Context, b runtime.RuntimeBinding) (runtime.WorkloadObservation, error) {
	if f.onEffect != nil {
		f.onEffect("remove", b)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removes = append(f.removes, b.WorkloadID)
	apply, err := true, error(nil)
	if f.removeHook != nil {
		apply, err = f.removeHook(len(f.removes), b)
	}
	if w, ok := f.workloads[b.WorkloadID]; ok && apply {
		if b.Fence < w.b.Fence {
			return runtime.WorkloadObservation{}, fmt.Errorf("%w: remove from fence %d", runtime.ErrStaleFence, b.Fence)
		}
		delete(f.workloads, b.WorkloadID)
		f.live--
	}
	if err != nil {
		return runtime.WorkloadObservation{}, err
	}
	return f.absent(b), nil
}

func (f *fakeRuntime) count(list []string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(list)
}

func (f *fakeRuntime) liveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.workloads)
}

// ---------------------------------------------------------------------------
// Fake resource manager
// ---------------------------------------------------------------------------

type fakeResources struct {
	mu    sync.Mutex
	clock *testClock
	caps  runtime.CapabilitySet

	state   map[string]runtime.ResourceState
	names   map[string]string
	ensures []string
	stops   []string
	removes []string

	ensureHook func(n int, spec runtime.ResourceSpec) (apply bool, err error)
	removeHook func(n int, h runtime.ResourceHandle) (apply bool, err error)
	// starting is how many observations a new service reports starting.
	starting map[string]int
}

func newFakeResources(c *testClock) *fakeResources {
	return &fakeResources{clock: c, caps: runtime.CapabilitySet{runtime.CapRunServices, runtime.CapPersistentService},
		state: map[string]runtime.ResourceState{}, names: map[string]string{}, starting: map[string]int{}}
}

func (f *fakeResources) Capabilities(context.Context) (runtime.CapabilitySet, error) {
	return f.caps, nil
}

func (f *fakeResources) obs(h runtime.ResourceHandle, s runtime.ResourceState) runtime.ResourceObservation {
	return runtime.ResourceObservation{Handle: h, State: s, ObservedAt: f.clock.Now()}
}

func (f *fakeResources) Ensure(_ context.Context, spec runtime.ResourceSpec) (runtime.ResourceObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensures = append(f.ensures, spec.Name)
	f.names[spec.Name] = spec.Identity
	apply, err := true, error(nil)
	if f.ensureHook != nil {
		apply, err = f.ensureHook(len(f.ensures), spec)
	}
	if apply {
		if _, ok := f.state[spec.Identity]; !ok {
			if n := f.starting[spec.Name]; n > 0 {
				f.state[spec.Identity] = runtime.ResourceStarting
			} else {
				f.state[spec.Identity] = runtime.ResourceReady
			}
		}
	}
	if err != nil {
		return runtime.ResourceObservation{}, err
	}
	return f.obs(spec.Handle(), f.state[spec.Identity]), nil
}

func (f *fakeResources) Observe(_ context.Context, h runtime.ResourceHandle) (runtime.ResourceObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.state[h.Identity]
	if !ok {
		return f.obs(h, runtime.ResourceRemoved), nil
	}
	if s == runtime.ResourceStarting {
		if f.starting[h.Name] > 1 {
			f.starting[h.Name]--
		} else {
			f.starting[h.Name], s = 0, runtime.ResourceReady
			f.state[h.Identity] = s
		}
	}
	return f.obs(h, s), nil
}

func (f *fakeResources) Logs(context.Context, runtime.ResourceHandle, runtime.LogOptions) (io.ReadCloser, error) {
	return nil, errors.New("not used")
}

func (f *fakeResources) Stop(_ context.Context, h runtime.ResourceHandle) (runtime.ResourceObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, h.Name)
	if _, ok := f.state[h.Identity]; ok {
		f.state[h.Identity] = runtime.ResourceStopped
	}
	return f.obs(h, f.state[h.Identity]), nil
}

func (f *fakeResources) Remove(_ context.Context, h runtime.ResourceHandle) (runtime.ResourceObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removes = append(f.removes, h.Name)
	apply, err := true, error(nil)
	if f.removeHook != nil {
		apply, err = f.removeHook(len(f.removes), h)
	}
	if apply {
		delete(f.state, h.Identity)
	}
	if err != nil {
		return runtime.ResourceObservation{}, err
	}
	return f.obs(h, runtime.ResourceRemoved), nil
}

// lose makes a resource disappear behind the engine's back.
func (f *fakeResources) lose(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.state, f.names[name])
}

// ---------------------------------------------------------------------------
// Fake outputs and secrets
// ---------------------------------------------------------------------------

type fakeOutputs struct {
	mu      sync.Mutex
	values  map[domain.StepID]map[string]string
	collect []domain.StepID
}

func newFakeOutputs() *fakeOutputs {
	return &fakeOutputs{values: map[domain.StepID]map[string]string{}}
}

// produce sets what a step's workload "wrote"; it is recorded only on Collect.
func (f *fakeOutputs) produce(step domain.StepID, kv map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[step] = kv
}

func (f *fakeOutputs) Collect(_ context.Context, ref runner.StepRef, declared []string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.collect = append(f.collect, ref.StepKey)
	return f.values[ref.StepKey], nil
}

func (f *fakeOutputs) Read(_ context.Context, ref runner.StepRef) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.collect {
		if k == ref.StepKey {
			return f.values[ref.StepKey], nil
		}
	}
	return nil, nil
}

type fakeSecrets struct {
	mu        sync.Mutex
	resolved  []domain.StepID
	released  []domain.StepID
	resolveFn func(step domain.StepID) error
}

func (f *fakeSecrets) Resolve(_ context.Context, req runtime.ResolveRequest) (runtime.Materialization, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resolveFn != nil {
		if err := f.resolveFn(req.StepKey); err != nil {
			return runtime.Materialization{}, err
		}
	}
	f.resolved = append(f.resolved, req.StepKey)
	return runtime.Materialization{Scope: req.Scope, RunID: req.RunID, StepKey: req.StepKey, Directory: "/run/secrets/" + string(req.StepKey), Refs: req.Refs}, nil
}

func (f *fakeSecrets) Release(_ context.Context, m runtime.Materialization) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, m.StepKey)
	return nil
}

// ---------------------------------------------------------------------------
// Environment
// ---------------------------------------------------------------------------

type env struct {
	t       *testing.T
	clock   *testClock
	state   *runstate.Memory
	rt      *fakeRuntime
	res     *fakeResources
	outputs *fakeOutputs
	secrets *fakeSecrets
	// onSleep runs at every pause of an engine, after the clock moved.
	onSleep func() error
}

var errCrash = errors.New("simulated crash")

func newEnv(t *testing.T) *env {
	t.Helper()
	c := &testClock{now: t0}
	e := &env{t: t, clock: c, state: runstate.NewMemory(c), rt: newFakeRuntime(c), res: newFakeResources(c),
		outputs: newFakeOutputs(), secrets: &fakeSecrets{}}
	return e
}

// engine builds an engine instance with its own owner identity, all wired to the
// environment's fakes and clock.
func (e *env) engine(owner string, mutate ...func(*runner.EngineConfig)) *runner.Engine {
	e.t.Helper()
	cfg := runner.EngineConfig{State: e.state, Runtime: e.rt, Resources: e.res, Secrets: e.secrets, Outputs: e.outputs,
		Clock: e.clock, Owner: owner, Backend: "fake", LeaseTTL: leaseTTL, PollInterval: poll,
		StepTimeout: 15 * time.Minute, ReadyTimeout: time.Minute, CleanupBackoff: time.Second,
		Sleep: func(ctx context.Context, d time.Duration) error {
			e.clock.Advance(d)
			if e.onSleep != nil {
				return e.onSleep()
			}
			return ctx.Err()
		}}
	for _, m := range mutate {
		m(&cfg)
	}
	eng, err := runner.NewEngine(cfg)
	if err != nil {
		e.t.Fatalf("build the engine: %v", err)
	}
	return eng
}

func ctr(key domain.StepID, deps ...domain.StepID) runtime.PlanStep {
	return runtime.PlanStep{Key: key, Kind: runtime.StepContainer, DependsOn: deps,
		Workload: &runtime.WorkloadSpec{Image: "alpine:3", Commands: []string{"run " + string(key)}}}
}

func plan(digest string, steps ...runtime.PlanStep) runtime.Plan {
	return runtime.Plan{Digest: "sha256:" + digest, Steps: steps}
}

func (e *env) admit(id domain.RunID, p runtime.Plan, maxCleanup int) runtime.RunRecord {
	e.t.Helper()
	run, err := runtime.NewRun(scope, id, "pipe1", p, maxCleanup)
	if err != nil {
		e.t.Fatalf("build the run: %v", err)
	}
	a := runtime.Admission{Run: run, Plan: p}
	for _, ps := range p.Steps {
		if ps.Kind != runtime.StepWait {
			continue
		}
		w, err := runtime.NewWait(scope, waitID(id, ps.Key), id, ps.Key, *ps.Wait)
		if err != nil {
			e.t.Fatalf("build the wait: %v", err)
		}
		a.Waits = append(a.Waits, w)
	}
	a.Events = []runtime.Event{{ID: "run.created:" + string(id), Scope: scope, RunID: id, Kind: runtime.EventRunCreated, Version: 1, At: e.clock.Now()}}
	rec, err := e.state.Admit(context.Background(), a)
	if err != nil {
		e.t.Fatalf("admit: %v", err)
	}
	return rec
}

func waitID(run domain.RunID, step domain.StepID) runtime.WaitID {
	return runtime.WaitID("w-" + string(run) + "-" + string(step))
}

func (e *env) ref(id domain.RunID, purpose runtime.ClaimPurpose) runtime.RunRef {
	return runtime.RunRef{Scope: scope, RunID: id, Purpose: purpose}
}

// drive runs one engine over a run with an execute claim and fails the test on error.
func (e *env) drive(eng *runner.Engine, id domain.RunID) runner.Report {
	e.t.Helper()
	rep, err := eng.Drive(context.Background(), e.ref(id, runtime.ClaimExecute))
	if err != nil {
		e.t.Fatalf("drive %s: %v", id, err)
	}
	return rep
}

func (e *env) load(id domain.RunID) runtime.RunRecord {
	e.t.Helper()
	rec, err := e.state.Load(context.Background(), scope, id)
	if err != nil {
		e.t.Fatalf("load %s: %v", id, err)
	}
	if err := rec.Validate(); err != nil {
		e.t.Fatalf("the stored record is not coherent: %v", err)
	}
	return rec
}

func (e *env) outbox(id domain.RunID) []runtime.Event {
	e.t.Helper()
	ev, err := e.state.Outbox(context.Background(), scope, id)
	if err != nil {
		e.t.Fatalf("outbox: %v", err)
	}
	return ev
}

func stepOf(rec runtime.RunRecord, key domain.StepID) runtime.Step {
	for _, s := range rec.Run.Steps {
		if s.Key == key {
			return s
		}
	}
	return runtime.Step{}
}

func states(rec runtime.RunRecord) map[domain.StepID]runtime.StepState {
	out := map[domain.StepID]runtime.StepState{}
	for _, s := range rec.Run.Steps {
		out[s.Key] = s.State
	}
	return out
}

func (e *env) expect(cond bool, format string, args ...any) {
	e.t.Helper()
	if !cond {
		e.t.Fatalf(format, args...)
	}
}

// settle advances the clock past the lease so another claimant may take over.
func (e *env) expire() { e.clock.Advance(leaseTTL + time.Second) }

func countKind(events []runtime.Event, k runtime.EventKind) int {
	n := 0
	for _, ev := range events {
		if ev.Kind == k {
			n++
		}
	}
	return n
}
