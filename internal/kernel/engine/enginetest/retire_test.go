package enginetest

// Retire-then-replace launch protocol.
//
// A start attempt that is affirmatively proven never to have executed is
// retired before it is replaced: the `retiring` launch state and the
// never-started proof are committed in one payload first, Remove follows
// idempotently, and only a confirmed removal permits the `retired` fact that
// allocates the replacement identity. These tests pin the durable ordering,
// recovery from persisted state only, cancellation/expiry during retirement and
// the corrupt-retirement and absence boundaries. No sleep, real process or
// network is involved: every fact is driven through the injected clock, fakes
// and explicit probes.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/kernel/testkit"
)

// ---------------------------------------------------------------------------
// Probes
// ---------------------------------------------------------------------------

// probedCall is one recorded runtime effect.
type probedCall struct {
	kind     string
	workload id.WorkloadID
}

// probeRuntime records the ordered runtime effects the engine performs and runs
// an optional hook immediately before each removal or start. A hook inspects the
// persisted execution at the exact boundary where the contract orders a durable
// commit before an effect, so ordering is never inferred from call counts.
type probeRuntime struct {
	engine.Runtime

	mu           sync.Mutex
	calls        []probedCall
	beforeRemove func(engine.RemoveRequest)
	beforeStart  func(engine.StartRequest)
}

func newProbeRuntime(runtime engine.Runtime) *probeRuntime {
	return &probeRuntime{Runtime: runtime}
}

func (r *probeRuntime) record(kind string, workload id.WorkloadID) {
	r.mu.Lock()
	r.calls = append(r.calls, probedCall{kind: kind, workload: workload})
	r.mu.Unlock()
}

func (r *probeRuntime) Prepare(ctx context.Context, request engine.PrepareRequest) (engine.Preparation, error) {
	r.record("prepare", request.Identity.WorkloadID)
	return r.Runtime.Prepare(ctx, request)
}

func (r *probeRuntime) Start(ctx context.Context, request engine.StartRequest) (engine.StartResult, error) {
	r.record("start", request.Identity.WorkloadID)
	if r.beforeStart != nil {
		r.beforeStart(request)
	}
	return r.Runtime.Start(ctx, request)
}

func (r *probeRuntime) Observe(ctx context.Context, request engine.ObserveRequest) (kernel.Observation, error) {
	r.record("observe", request.Identity.WorkloadID)
	return r.Runtime.Observe(ctx, request)
}

func (r *probeRuntime) Stop(ctx context.Context, request engine.StopRequest) (kernel.StopEvidence, error) {
	r.record("stop", request.Identity.WorkloadID)
	return r.Runtime.Stop(ctx, request)
}

func (r *probeRuntime) Remove(ctx context.Context, request engine.RemoveRequest) (engine.Removal, error) {
	r.record("remove", request.Identity.WorkloadID)
	if r.beforeRemove != nil {
		r.beforeRemove(request)
	}
	return r.Runtime.Remove(ctx, request)
}

func (r *probeRuntime) snapshot() []probedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]probedCall(nil), r.calls...)
}

func (r *probeRuntime) count(kind string, workload id.WorkloadID) int {
	count := 0
	for _, call := range r.snapshot() {
		if call.kind == kind && call.workload == workload {
			count++
		}
	}
	return count
}

// assertNoEffectAfter fails when the engine performs one of the named effects on
// the workload at or after the given call index.
func assertNoEffectAfter(t *testing.T, calls []probedCall, from int, workload id.WorkloadID, kinds ...string) {
	t.Helper()
	for index := from; index < len(calls); index++ {
		call := calls[index]
		if call.workload != workload {
			continue
		}
		for _, kind := range kinds {
			if call.kind == kind {
				t.Fatalf("effect %q on retiring identity %q at call %d, after retirement: %v",
					call.kind, workload, index, calls)
			}
		}
	}
}

// replacementFailingState fails exactly the commit that allocates a replacement
// workload identity: a not_issued launch whose identity differs from the one
// already persisted for the action. Every other transition is delegated, so the
// retirement commit and removal still happen.
type replacementFailingState struct {
	engine.State

	executionID id.ExecutionID
	failure     error

	mu       sync.Mutex
	attempts int
}

func (s *replacementFailingState) Commit(ctx context.Context, commit flow.Commit) (kernel.Execution, error) {
	if commit.Launch != nil && commit.Launch.State == kernel.LaunchNotIssued && s.allocatesReplacement(ctx, commit) {
		s.mu.Lock()
		s.attempts++
		s.mu.Unlock()
		return kernel.Execution{}, s.failure
	}
	return s.State.Commit(ctx, commit)
}

func (s *replacementFailingState) allocatesReplacement(ctx context.Context, commit flow.Commit) bool {
	current, err := s.State.Load(ctx, s.executionID)
	if err != nil {
		return false
	}
	for _, action := range current.Actions {
		if action.ID != commit.ActionID || action.Container == nil {
			continue
		}
		// The activation commit allocates the first identity from empty; only a
		// different already-persisted identity makes this a replacement.
		return action.Container.WorkloadID != "" &&
			commit.Launch.WorkloadID != action.Container.WorkloadID
	}
	return false
}

func (s *replacementFailingState) attemptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

// cancelAfterRemoveRuntime cancels the driving context after the removal effect
// is applied, reproducing a controller that confirmed removal but crashed
// before committing the replacement identity.
type cancelAfterRemoveRuntime struct {
	engine.Runtime

	once   sync.Once
	cancel context.CancelFunc
}

func (r *cancelAfterRemoveRuntime) Remove(ctx context.Context, request engine.RemoveRequest) (engine.Removal, error) {
	removal, err := r.Runtime.Remove(ctx, request)
	r.once.Do(r.cancel)
	return removal, err
}

// refuseOnStartRuntime injects a proven-not-started result on the Nth Start call
// by arming the testkit fake's refusal immediately before that effect.
type refuseOnStartRuntime struct {
	engine.Runtime

	fixture  *testkit.Runtime
	refuseOn int
	detail   string

	mu     sync.Mutex
	starts int
}

func (r *refuseOnStartRuntime) Start(ctx context.Context, request engine.StartRequest) (engine.StartResult, error) {
	r.mu.Lock()
	r.starts++
	call := r.starts
	r.mu.Unlock()
	if call == r.refuseOn {
		r.fixture.RefuseNextStart(r.detail)
	}
	return r.Runtime.Start(ctx, request)
}

// ---------------------------------------------------------------------------
// Assertions
// ---------------------------------------------------------------------------

// assertRetiring pins the durable retirement shape: the same workload identity
// and idempotency key, the retiring launch state and the persisted never-started
// proof, with no fabricated stop evidence.
func assertRetiring(t *testing.T, execution kernel.Execution, workload id.WorkloadID) {
	t.Helper()
	action := mustAction(t, execution, "build")
	if action.Container == nil {
		t.Fatalf("action %q has no container value: %s", action.Name, actionSummary(execution))
	}
	if action.Container.WorkloadID != workload {
		t.Fatalf("persisted workload = %q, want the retiring identity %q: %s",
			action.Container.WorkloadID, workload, actionSummary(execution))
	}
	if action.Container.Launch.WorkloadID != workload {
		t.Fatalf("retiring launch workload = %q, want the unchanged identity %q",
			action.Container.Launch.WorkloadID, workload)
	}
	if action.Container.Launch.State != kernel.LaunchRetiring {
		t.Fatalf("launch state = %q, want retiring: %s",
			action.Container.Launch.State, actionSummary(execution))
	}
	if action.Container.Launch.IdempotencyKey == "" {
		t.Fatal("retiring launch lost its idempotency key")
	}
	if !action.Container.Observation.ProofOfNonexecution() {
		t.Fatalf("retirement did not persist the never-started proof: %+v", action.Container.Observation)
	}
	if action.Stop.IsConfirmed() {
		t.Fatal("retiring identity fabricated stop evidence")
	}
}

// assertIssuedAtStart pins that the persisted launch marker is already issued
// when the start effect for that identity runs.
func assertIssuedAtStart(t *testing.T, execution kernel.Execution, workload id.WorkloadID) {
	t.Helper()
	action := mustAction(t, execution, "build")
	if action.Container == nil || action.Container.WorkloadID != workload {
		t.Fatalf("persisted workload at start = %+v, want %q", action.Container, workload)
	}
	if action.Container.Launch.State != kernel.LaunchIssued {
		t.Fatalf("launch state at start = %q, want issued", action.Container.Launch.State)
	}
}

func removalRequestsInclude(removals []engine.RemoveRequest, workload id.WorkloadID) bool {
	for _, removal := range removals {
		if removal.Identity.WorkloadID == workload {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Retirement commit precedes the removal effect
// ---------------------------------------------------------------------------

func TestRetirementCommitPrecedesRemovalEffect(t *testing.T) {
	build := containerAction("act-build", "build", 0, time.Hour)
	h := newHarness(t, newExecution("exec-retire-order", "ws-retire-order", build))
	h.fixture.Runtime.RefuseNextStart("runtime proved the command never ran")

	probe := newProbeRuntime(h.runtime)
	var atRemove []kernel.Execution
	probe.beforeRemove = func(engine.RemoveRequest) { atRemove = append(atRemove, h.load()) }
	h.engine = engine.New(h.fixture.State, probe, h.fixture.Workspace, h.fixture.Clock)

	started := h.reconcileUntil("replacement started", 12, func(candidate kernel.Execution) bool {
		return h.fixture.Runtime.StartCount() == 1
	})

	starts := h.runtime.startRequests()
	if len(starts) != 2 {
		t.Fatalf("Start calls = %d, want the refused attempt plus one replacement", len(starts))
	}
	superseded := starts[0].Identity.WorkloadID
	fresh := starts[1].Identity.WorkloadID
	if superseded == fresh {
		t.Fatalf("replacement reused the retiring identity %q", superseded)
	}
	if len(atRemove) != 1 {
		t.Fatalf("Remove calls = %d, want exactly one removal of the retiring identity", len(atRemove))
	}
	// The retirement, with its proof, is durable before the removal effect.
	assertRetiring(t, atRemove[0], superseded)
	if atRemove[0].State != kernel.ExecutionRunning {
		t.Fatalf("execution state at removal = %q, want running", atRemove[0].State)
	}

	// A retiring identity is never observed, prepared, started or re-issued.
	calls := probe.snapshot()
	firstRemove := -1
	for index, call := range calls {
		if call.kind == "remove" && call.workload == superseded {
			firstRemove = index
			break
		}
	}
	if firstRemove < 0 {
		t.Fatal("the engine never issued Remove for the retiring identity")
	}
	assertNoEffectAfter(t, calls, firstRemove, superseded, "observe", "prepare", "start")
	if probe.count("stop", superseded) != 0 {
		t.Fatalf("retiring identity received %d stops, want none", probe.count("stop", superseded))
	}

	action := mustAction(t, started, "build")
	if action.Container.WorkloadID != fresh {
		t.Fatalf("persisted workload = %q, want replacement %q", action.Container.WorkloadID, fresh)
	}
	if action.Container.Launch.State != kernel.LaunchIssued {
		t.Fatalf("replacement launch state = %q, want issued", action.Container.Launch.State)
	}
	old, ok := h.fixture.Runtime.Workload(superseded)
	if !ok || !old.Removed || old.Started || old.StopRequests != 0 {
		t.Fatalf("retiring identity after replacement = %+v, want removed, never started, no stop", old)
	}
}

// ---------------------------------------------------------------------------
// Crash after Remove, before the replacement commit
// ---------------------------------------------------------------------------

func TestCrashAfterRemovalBeforeReplacementCommitRecovers(t *testing.T) {
	seeded := runningContainerExecution("exec-retire-crash", "ws-retire-crash", testNow.Add(time.Hour))
	h := newHarness(t, seeded)
	h.retainWorkspace()
	identity := identityFor(seeded.ID, seeded.Actions[0])
	h.prepareOnly(identity, seeded.Actions[0].Container.Definition)

	injected := errors.New("injected replacement commit failure")
	failing := &replacementFailingState{State: h.fixture.State, executionID: h.executionID, failure: injected}
	h.engine = engine.New(failing, h.runtime, h.fixture.Workspace, h.fixture.Clock)

	if _, err := h.engine.Reconcile(h.t.Context(), h.executionID); err == nil {
		t.Fatal("Reconcile hid the failed replacement commit")
	}
	if failing.attemptCount() == 0 {
		t.Fatal("the replacement commit was never attempted")
	}

	crashed := h.load()
	assertRetiring(t, crashed, identity.WorkloadID)
	if crashed.State != kernel.ExecutionRunning {
		t.Fatalf("execution state after the crash = %q, want running", crashed.State)
	}
	if h.runtime.startCount() != 0 {
		t.Fatalf("Start calls = %d, want none before the replacement is durable", h.runtime.startCount())
	}
	old, ok := h.fixture.Runtime.Workload(identity.WorkloadID)
	if !ok || !old.Removed || old.Started {
		t.Fatalf("retiring identity after the crash = %+v, want removed and never started", old)
	}

	// A fresh controller recovers from persisted state only.
	h.restart()
	probe := newProbeRuntime(h.runtime)
	h.engine = engine.New(h.fixture.State, probe, h.fixture.Workspace, h.fixture.Clock)
	started := h.reconcileUntil("replacement started after restart", 12, func(candidate kernel.Execution) bool {
		return h.fixture.Runtime.StartCount() == 1
	})
	for _, kind := range []string{"observe", "prepare", "start"} {
		if probe.count(kind, identity.WorkloadID) != 0 {
			t.Fatalf("recovery issued %q for the retiring identity %q, want no new decision effect",
				kind, identity.WorkloadID)
		}
	}
	if probe.count("remove", identity.WorkloadID) != 1 {
		t.Fatalf("Remove calls for the retiring identity = %d, want one idempotent retry",
			probe.count("remove", identity.WorkloadID))
	}
	if probe.count("stop", identity.WorkloadID) != 0 || h.runtime.stopCount() != 0 {
		t.Fatalf("Stop calls = %d, want none for an affirmatively never-started identity", h.runtime.stopCount())
	}
	if countWorkload(h.runtime.removedWorkloads(), identity.WorkloadID) < 2 {
		t.Fatalf("Remove attempts = %v, want the removal retried idempotently", h.runtime.removedWorkloads())
	}
	starts := h.runtime.startRequests()
	if len(starts) != 1 {
		t.Fatalf("Start calls = %d, want exactly the replacement", len(starts))
	}
	fresh := starts[0].Identity.WorkloadID
	if fresh == identity.WorkloadID {
		t.Fatalf("replacement reused the retiring identity %q", identity.WorkloadID)
	}
	action := mustAction(t, started, "build")
	if action.Container.WorkloadID != fresh {
		t.Fatalf("persisted workload = %q, want replacement %q", action.Container.WorkloadID, fresh)
	}
	if action.Container.Launch.State != kernel.LaunchIssued {
		t.Fatalf("replacement launch state = %q, want issued", action.Container.Launch.State)
	}
	old, ok = h.fixture.Runtime.Workload(identity.WorkloadID)
	if !ok || !old.Removed || old.Started || old.StopRequests != 0 {
		t.Fatalf("retiring identity after recovery = %+v, want removed, never started, no stop", old)
	}
	if replacement, ok := h.fixture.Runtime.Workload(fresh); !ok || replacement.StartCount != 1 {
		t.Fatalf("replacement workload = %+v, want started exactly once", replacement)
	}
}

// ---------------------------------------------------------------------------
// Crash after the retiring commit, before Remove
// ---------------------------------------------------------------------------

func TestCrashAfterRetiringCommitBeforeRemovalRecovers(t *testing.T) {
	seeded := runningContainerExecution("exec-retire-before-remove", "ws-retire-before-remove", testNow.Add(time.Hour))
	h := newHarness(t, seeded)
	h.retainWorkspace()
	identity := identityFor(seeded.ID, seeded.Actions[0])
	h.prepareOnly(identity, seeded.Actions[0].Container.Definition)

	ctx, cancel := context.WithCancel(h.t.Context())
	defer cancel()
	h.engine = engine.New(h.fixture.State, &cancelAtRemoveRuntime{Runtime: h.runtime, cancel: cancel}, h.fixture.Workspace, h.fixture.Clock)
	if _, err := h.engine.Reconcile(ctx, h.executionID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Reconcile error = %v, want context.Canceled", err)
	}

	crashed := h.load()
	assertRetiring(t, crashed, identity.WorkloadID)
	if crashed.State != kernel.ExecutionRunning {
		t.Fatalf("execution state after the crash = %q, want running", crashed.State)
	}
	old, ok := h.fixture.Runtime.Workload(identity.WorkloadID)
	if !ok || old.Removed || old.Started {
		t.Fatalf("retiring identity after the crash = %+v, want prepared and not removed", old)
	}

	probe := newProbeRuntime(h.runtime)
	h.engine = engine.New(h.fixture.State, probe, h.fixture.Workspace, h.fixture.Clock)
	started := h.reconcileUntil("replacement started after restart", 12, func(candidate kernel.Execution) bool {
		return h.fixture.Runtime.StartCount() == 1
	})
	for _, kind := range []string{"observe", "prepare", "start"} {
		if probe.count(kind, identity.WorkloadID) != 0 {
			t.Fatalf("recovery issued %q for the retiring identity %q, want no new decision effect",
				kind, identity.WorkloadID)
		}
	}
	if probe.count("stop", identity.WorkloadID) != 0 || h.runtime.stopCount() != 0 {
		t.Fatalf("Stop calls = %d, want none", h.runtime.stopCount())
	}
	starts := h.runtime.startRequests()
	if len(starts) != 1 {
		t.Fatalf("Start calls = %d, want exactly the replacement", len(starts))
	}
	fresh := starts[0].Identity.WorkloadID
	if fresh == identity.WorkloadID {
		t.Fatalf("replacement reused the retiring identity %q", identity.WorkloadID)
	}
	if action := mustAction(t, started, "build"); action.Container.WorkloadID != fresh {
		t.Fatalf("persisted workload = %q, want replacement %q", action.Container.WorkloadID, fresh)
	}
	old, ok = h.fixture.Runtime.Workload(identity.WorkloadID)
	if !ok || !old.Removed || old.Started || old.StopRequests != 0 {
		t.Fatalf("retiring identity after recovery = %+v, want removed, never started, no stop", old)
	}
}

// ---------------------------------------------------------------------------
// Transient Remove failure retries before the replacement
// ---------------------------------------------------------------------------

func TestTransientRemovalFailureRetriesBeforeReplacement(t *testing.T) {
	seeded := runningContainerExecution("exec-retire-retry", "ws-retire-retry", testNow.Add(time.Hour))
	h := newHarness(t, seeded)
	h.retainWorkspace()
	identity := identityFor(seeded.ID, seeded.Actions[0])
	h.prepareOnly(identity, seeded.Actions[0].Container.Definition)
	h.fixture.Runtime.FailNextRemove(errors.New("injected transient removal failure"))

	if _, err := h.engine.Reconcile(h.t.Context(), h.executionID); err == nil {
		t.Fatal("Reconcile hid the transient removal failure")
	}
	failed := h.load()
	assertRetiring(t, failed, identity.WorkloadID)
	if failed.State != kernel.ExecutionRunning {
		t.Fatalf("execution state after the failed removal = %q, want running", failed.State)
	}
	if failed.Cleanup.State == kernel.CleanupComplete || failed.Cleanup.Attempts != 0 {
		t.Fatalf("cleanup after the failed retirement removal = %+v, want no attempts and no completion", failed.Cleanup)
	}
	if h.runtime.startCount() != 0 {
		t.Fatalf("Start calls = %d, want none before the removal succeeds", h.runtime.startCount())
	}
	old, ok := h.fixture.Runtime.Workload(identity.WorkloadID)
	if !ok || old.Removed || old.Started {
		t.Fatalf("retiring identity after the failed removal = %+v, want prepared and not removed", old)
	}

	probe := newProbeRuntime(h.runtime)
	var atStart []kernel.Execution
	var removalsAtStart [][]engine.RemoveRequest
	probe.beforeStart = func(engine.StartRequest) {
		atStart = append(atStart, h.load())
		removalsAtStart = append(removalsAtStart, h.fixture.Runtime.Removals())
	}
	h.engine = engine.New(h.fixture.State, probe, h.fixture.Workspace, h.fixture.Clock)

	started := h.reconcileUntil("replacement started after the removal retry", 12, func(candidate kernel.Execution) bool {
		return h.fixture.Runtime.StartCount() == 1
	})
	if len(atStart) != 1 {
		t.Fatalf("Start calls = %d, want exactly the replacement", len(atStart))
	}
	replacementAtStart := mustAction(t, atStart[0], "build")
	if replacementAtStart.Container == nil || replacementAtStart.Container.WorkloadID == identity.WorkloadID {
		t.Fatalf("persisted workload at start = %+v, want a fresh replacement", replacementAtStart.Container)
	}
	assertIssuedAtStart(t, atStart[0], replacementAtStart.Container.WorkloadID)
	if !removalRequestsInclude(removalsAtStart[0], identity.WorkloadID) {
		t.Fatalf("replacement started before the retiring identity %q was removed", identity.WorkloadID)
	}
	if countWorkload(h.runtime.removedWorkloads(), identity.WorkloadID) < 2 {
		t.Fatalf("Remove attempts = %v, want the transient failure retried", h.runtime.removedWorkloads())
	}
	action := mustAction(t, started, "build")
	if action.Container.WorkloadID == identity.WorkloadID {
		t.Fatal("replacement did not allocate a fresh identity")
	}
	old, ok = h.fixture.Runtime.Workload(identity.WorkloadID)
	if !ok || !old.Removed || old.Started || old.StopRequests != 0 {
		t.Fatalf("retiring identity after the retry = %+v, want removed, never started, no stop", old)
	}
	if started.Cleanup.Attempts != 0 {
		t.Fatalf("cleanup attempts = %d, want 0; the removal was retried before the replacement existed", started.Cleanup.Attempts)
	}
	if started.State.Terminal() {
		t.Fatalf("the retirement retry settled the execution as %q", started.State)
	}
}

// ---------------------------------------------------------------------------
// Cancel while retiring
// ---------------------------------------------------------------------------

func TestCancelWhileRetiringRemovesIdentityWithoutStop(t *testing.T) {
	seeded := runningContainerExecution("exec-retire-cancel", "ws-retire-cancel", testNow.Add(time.Hour))
	h := newHarness(t, seeded)
	h.retainWorkspace()
	identity := identityFor(seeded.ID, seeded.Actions[0])
	h.prepareOnly(identity, seeded.Actions[0].Container.Definition)

	ctx, cancel := context.WithCancel(h.t.Context())
	defer cancel()
	h.engine = engine.New(h.fixture.State, &cancelAfterRemoveRuntime{Runtime: h.runtime, cancel: cancel}, h.fixture.Workspace, h.fixture.Clock)
	if _, err := h.engine.Reconcile(ctx, h.executionID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Reconcile error = %v, want context.Canceled", err)
	}
	retiring := h.load()
	assertRetiring(t, retiring, identity.WorkloadID)
	old, ok := h.fixture.Runtime.Workload(identity.WorkloadID)
	if !ok || !old.Removed || old.Started {
		t.Fatalf("retiring identity after the crash = %+v, want removed and never started", old)
	}
	observes := h.runtime.observeCount()

	h.cancel(flow.CancelRequested)
	cancelled := h.load()
	if cancelled.State != kernel.ExecutionCancelled {
		t.Fatalf("state after Cancel = %q, want cancelled", cancelled.State)
	}
	assertRetiring(t, cancelled, identity.WorkloadID)

	completed := h.reconcileUntil("cancelled cleanup complete", 12, func(candidate kernel.Execution) bool {
		return candidate.Cleanup.State == kernel.CleanupComplete
	})
	if completed.State != kernel.ExecutionCancelled {
		t.Fatalf("terminal state = %q, want cancelled", completed.State)
	}
	if completed.Failure != nil {
		t.Fatalf("cancel outcome was overwritten: %+v", completed.Failure)
	}
	assertRetiring(t, completed, identity.WorkloadID)
	if h.runtime.startCount() != 0 {
		t.Fatalf("a replacement was started after cancellation: %v", h.runtime.startRequests())
	}
	if h.runtime.observeCount() != observes {
		t.Fatalf("cleanup observed the retiring identity: %d -> %d", observes, h.runtime.observeCount())
	}
	if h.runtime.stopCount() != 0 {
		t.Fatalf("cleanup requested %d stops for an affirmatively never-started identity", h.runtime.stopCount())
	}
	if mustAction(t, completed, "build").Stop.IsConfirmed() {
		t.Fatal("cleanup fabricated stop evidence for the retiring identity")
	}
	if completed.Cleanup.Attempts != 0 {
		t.Fatalf("cleanup attempts = %d, want 0", completed.Cleanup.Attempts)
	}
	if !h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("cleanup did not release the workspace after removal")
	}
	workload, ok := h.fixture.Runtime.Workload(identity.WorkloadID)
	if !ok || !workload.Removed || workload.Started || workload.StopRequests != 0 {
		t.Fatalf("retiring identity after cleanup = %+v, want removed, never started, no stop", workload)
	}
}

// ---------------------------------------------------------------------------
// Expiry while retiring
// ---------------------------------------------------------------------------

func TestExpiryWhileRetiringRemovesIdentityWithoutStop(t *testing.T) {
	deadline := testNow.Add(time.Hour)
	seeded := runningContainerExecution("exec-retire-expire", "ws-retire-expire", deadline)
	h := newHarness(t, seeded)
	h.retainWorkspace()
	identity := identityFor(seeded.ID, seeded.Actions[0])
	h.prepareOnly(identity, seeded.Actions[0].Container.Definition)

	ctx, cancel := context.WithCancel(h.t.Context())
	defer cancel()
	h.engine = engine.New(h.fixture.State, &cancelAfterRemoveRuntime{Runtime: h.runtime, cancel: cancel}, h.fixture.Workspace, h.fixture.Clock)
	if _, err := h.engine.Reconcile(ctx, h.executionID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Reconcile error = %v, want context.Canceled", err)
	}
	assertRetiring(t, h.load(), identity.WorkloadID)
	observes := h.runtime.observeCount()

	// The persisted deadline expires while the identity is retiring.
	h.fixture.Clock.Set(deadline)
	completed := h.reconcileUntil("expired cleanup complete", 12, func(candidate kernel.Execution) bool {
		return candidate.State == kernel.ExecutionFailed && candidate.Cleanup.State == kernel.CleanupComplete
	})
	if completed.Failure == nil || completed.Failure.Kind != kernel.FailureTimeout {
		t.Fatalf("failure = %+v, want timeout", completed.Failure)
	}
	assertRetiring(t, completed, identity.WorkloadID)
	if h.runtime.startCount() != 0 {
		t.Fatalf("a replacement was started after expiry: %v", h.runtime.startRequests())
	}
	if h.runtime.observeCount() != observes {
		t.Fatalf("cleanup observed the retiring identity: %d -> %d", observes, h.runtime.observeCount())
	}
	if h.runtime.stopCount() != 0 {
		t.Fatalf("cleanup requested %d stops for an affirmatively never-started identity", h.runtime.stopCount())
	}
	if mustAction(t, completed, "build").Stop.IsConfirmed() {
		t.Fatal("cleanup fabricated stop evidence for the retiring identity")
	}
	if completed.Cleanup.Attempts != 0 {
		t.Fatalf("cleanup attempts = %d, want 0", completed.Cleanup.Attempts)
	}
	if !h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("cleanup did not release the workspace after removal")
	}
	workload, ok := h.fixture.Runtime.Workload(identity.WorkloadID)
	if !ok || !workload.Removed || workload.Started || workload.StopRequests != 0 {
		t.Fatalf("retiring identity after cleanup = %+v, want removed, never started, no stop", workload)
	}
}

// ---------------------------------------------------------------------------
// Cancel wins the replacement-commit race
// ---------------------------------------------------------------------------

func TestCancelWinsReplacementCommitRace(t *testing.T) {
	seeded := runningContainerExecution("exec-retire-race", "ws-retire-race", testNow.Add(time.Hour))
	h := newHarness(t, seeded)
	h.retainWorkspace()
	identity := identityFor(seeded.ID, seeded.Actions[0])
	h.prepareOnly(identity, seeded.Actions[0].Container.Definition)

	gate := newGatedState(h.fixture.State)
	gate.gate = func(commit flow.Commit) bool {
		return commit.Launch != nil && commit.Launch.State == kernel.LaunchNotIssued
	}
	h.engine = engine.New(gate, h.runtime, h.fixture.Workspace, h.fixture.Clock)

	done := make(chan error, 1)
	go func() {
		_, err := h.engine.Reconcile(h.t.Context(), h.executionID)
		done <- err
	}()
	waitGate(t, gate, h.t.Context())

	// The replacement commit is gated; retirement and removal are durable.
	gated := h.load()
	assertRetiring(t, gated, identity.WorkloadID)
	old, ok := h.fixture.Runtime.Workload(identity.WorkloadID)
	if !ok || !old.Removed || old.Started {
		t.Fatalf("retiring identity at the gated replacement commit = %+v, want removed and never started", old)
	}

	// Cancel commits first and wins the same boundary.
	h.cancel(flow.CancelRequested)
	gate.releaseGate()
	if err := <-done; err != nil {
		t.Fatalf("Reconcile error = %v", err)
	}
	if gate.conflictCount() == 0 {
		t.Fatal("Reconcile never observed a real version conflict")
	}

	final := h.load()
	if final.State != kernel.ExecutionCancelled {
		t.Fatalf("terminal state = %q, want the cancel winner", final.State)
	}
	if final.Failure != nil {
		t.Fatalf("stale replacement overwrote the winner with a failure: %+v", final.Failure)
	}
	action := mustAction(t, final, "build")
	if action.Container.WorkloadID != identity.WorkloadID {
		t.Fatalf("a replacement identity %q was committed after the cancel winner", action.Container.WorkloadID)
	}
	if action.Container.Launch.State != kernel.LaunchRetiring {
		t.Fatalf("launch state = %q, want the untouched retiring marker", action.Container.Launch.State)
	}
	if h.runtime.startCount() != 0 {
		t.Fatalf("Start calls = %d, want none", h.runtime.startCount())
	}
	observes := h.runtime.observeCount()

	completed := h.reconcileUntil("cancelled cleanup complete", 12, func(candidate kernel.Execution) bool {
		return candidate.Cleanup.State == kernel.CleanupComplete
	})
	if completed.State != kernel.ExecutionCancelled {
		t.Fatalf("terminal state = %q, want cancelled", completed.State)
	}
	if h.runtime.observeCount() != observes {
		t.Fatalf("cleanup observed the retiring identity: %d -> %d", observes, h.runtime.observeCount())
	}
	if h.runtime.stopCount() != 0 {
		t.Fatalf("cleanup requested %d stops for an affirmatively never-started identity", h.runtime.stopCount())
	}
	if completed.Cleanup.Attempts != 0 {
		t.Fatalf("cleanup attempts = %d, want 0", completed.Cleanup.Attempts)
	}
	if !h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("cleanup did not release the workspace after removal")
	}
}

// ---------------------------------------------------------------------------
// A retiring action without a persisted proof is corrupt
// ---------------------------------------------------------------------------

func TestRetiringWithoutPersistedProofPerformsNoEffect(t *testing.T) {
	seeded := runningContainerExecution("exec-retire-corrupt", "ws-retire-corrupt", testNow.Add(time.Hour))
	seeded.Actions[0].Container.Launch.State = kernel.LaunchRetiring
	seeded.Actions[0].Container.Observation = kernel.UnknownObservation()
	h := newHarness(t, seeded)
	h.retainWorkspace()
	identity := identityFor(seeded.ID, seeded.Actions[0])

	assertNoRuntimeEffects := func(stage string) {
		t.Helper()
		if h.runtime.removeCount() != 0 || h.runtime.stopCount() != 0 || h.runtime.observeCount() != 0 ||
			h.runtime.prepareCount() != 0 || h.runtime.startCount() != 0 {
			t.Fatalf("%s: corrupt retirement performed runtime effects: removes=%d stops=%d observes=%d prepares=%d starts=%d",
				stage, h.runtime.removeCount(), h.runtime.stopCount(), h.runtime.observeCount(),
				h.runtime.prepareCount(), h.runtime.startCount())
		}
	}

	// A corrupt retirement stops visibly without touching the runtime.
	_, _ = h.engine.Reconcile(h.t.Context(), h.executionID)
	assertNoRuntimeEffects("first reconcile")

	unchanged := h.load()
	if unchanged.State != kernel.ExecutionRunning {
		t.Fatalf("execution state = %q, want the visible running state", unchanged.State)
	}
	action := mustAction(t, unchanged, "build")
	if action.Container.WorkloadID != identity.WorkloadID || action.Container.Launch.State != kernel.LaunchRetiring {
		t.Fatalf("persisted launch = %q/%q, want unchanged %q/retiring",
			action.Container.WorkloadID, action.Container.Launch.State, identity.WorkloadID)
	}
	if action.Container.Observation.Kind != kernel.ObservationUnknown {
		t.Fatalf("observation = %+v, want the persisted unknown evidence", action.Container.Observation)
	}
	if unchanged.Cleanup.State == kernel.CleanupComplete {
		t.Fatal("corrupt retirement reported cleanup complete")
	}
	if h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("corrupt retirement released the workspace")
	}

	for range 3 {
		_, _ = h.engine.Reconcile(h.t.Context(), h.executionID)
	}
	assertNoRuntimeEffects("repeated reconcile")
}

// ---------------------------------------------------------------------------
// Absence is never proof
// ---------------------------------------------------------------------------

func TestAbsentObservationNeverRetiresIssuedIdentity(t *testing.T) {
	seeded := runningContainerExecution("exec-absent-issued", "ws-absent-issued", testNow.Add(time.Hour))
	h := newHarness(t, seeded)
	h.retainWorkspace()
	identity := identityFor(seeded.ID, seeded.Actions[0])
	h.prepareOnly(identity, seeded.Actions[0].Container.Definition)
	// Absence may be reported repeatedly; none of it is affirmative proof.
	for range 8 {
		h.fixture.Runtime.QueueObservation(identity.WorkloadID, kernel.AbsentObservation())
	}

	observed := h.reconcile()
	action := mustAction(t, observed, "build")
	if action.Container.Observation.Kind != kernel.ObservationAbsent {
		t.Fatalf("observation = %+v, want absent", action.Container.Observation)
	}
	if action.Container.Launch.State != kernel.LaunchIssued {
		t.Fatalf("absence retired or replaced the identity: launch state = %q, want issued",
			action.Container.Launch.State)
	}
	if action.Container.WorkloadID != identity.WorkloadID {
		t.Fatalf("persisted workload = %q, want the unchanged identity %q",
			action.Container.WorkloadID, identity.WorkloadID)
	}
	if observed.State != kernel.ExecutionRunning || observed.CurrentAction != 0 {
		t.Fatalf("execution = %s, want the action still visibly pending", actionSummary(observed))
	}
	if h.runtime.startCount() != 0 || h.runtime.removeCount() != 0 {
		t.Fatalf("absence triggered effects: starts=%d removes=%d", h.runtime.startCount(), h.runtime.removeCount())
	}

	for range 4 {
		_, _ = h.engine.Reconcile(h.t.Context(), h.executionID)
	}
	after := h.load()
	if after.State != kernel.ExecutionRunning {
		t.Fatalf("execution state = %q, want running", after.State)
	}
	if mustAction(t, after, "build").Container.Launch.State != kernel.LaunchIssued {
		t.Fatalf("repeated absence changed the launch state to %q",
			mustAction(t, after, "build").Container.Launch.State)
	}
	if h.runtime.startCount() != 0 {
		t.Fatalf("absence triggered a relaunch: %v", h.runtime.startRequests())
	}
	if h.runtime.removeCount() != 0 {
		t.Fatalf("absence triggered a removal: %v", h.runtime.removedWorkloads())
	}
}

// ---------------------------------------------------------------------------
// Two successive relaunches each retire the previous identity
// ---------------------------------------------------------------------------

func TestSuccessiveRelaunchesRetireEachIdentity(t *testing.T) {
	build := containerAction("act-build", "build", 0, time.Hour)
	h := newHarness(t, newExecution("exec-retire-chain", "ws-retire-chain", build))
	h.fixture.Runtime.RefuseNextStart("runtime proved the first command never ran")

	refusing := &refuseOnStartRuntime{
		Runtime:  h.runtime,
		fixture:  h.fixture.Runtime,
		refuseOn: 2,
		detail:   "runtime proved the second command never ran",
	}
	probe := newProbeRuntime(refusing)
	var removeSnapshots []kernel.Execution
	var startSnapshots []kernel.Execution
	var removalsAtStart [][]engine.RemoveRequest
	probe.beforeRemove = func(engine.RemoveRequest) { removeSnapshots = append(removeSnapshots, h.load()) }
	probe.beforeStart = func(engine.StartRequest) {
		startSnapshots = append(startSnapshots, h.load())
		removalsAtStart = append(removalsAtStart, h.fixture.Runtime.Removals())
	}
	h.engine = engine.New(h.fixture.State, probe, h.fixture.Workspace, h.fixture.Clock)

	h.reconcileUntil("third identity started", 20, func(candidate kernel.Execution) bool {
		return h.fixture.Runtime.StartCount() == 1
	})
	starts := h.runtime.startRequests()
	if len(starts) != 3 {
		t.Fatalf("Start calls = %d, want two refused attempts plus one accepted start", len(starts))
	}
	first := starts[0].Identity.WorkloadID
	second := starts[1].Identity.WorkloadID
	third := starts[2].Identity.WorkloadID
	if first == second || second == third || first == third {
		t.Fatalf("relaunch identities are not distinct: %q, %q, %q", first, second, third)
	}

	// Each relaunch retires its predecessor before the next is committed.
	if len(removeSnapshots) != 2 {
		t.Fatalf("Remove calls = %d, want one removal per retired identity", len(removeSnapshots))
	}
	assertRetiring(t, removeSnapshots[0], first)
	assertRetiring(t, removeSnapshots[1], second)

	if len(startSnapshots) != 3 || len(removalsAtStart) != 3 {
		t.Fatalf("start observations = %d/%d, want three", len(startSnapshots), len(removalsAtStart))
	}
	assertIssuedAtStart(t, startSnapshots[0], first)
	assertIssuedAtStart(t, startSnapshots[1], second)
	assertIssuedAtStart(t, startSnapshots[2], third)
	if len(removalsAtStart[0]) != 0 {
		t.Fatalf("the first start happened after a removal: %v", removalsAtStart[0])
	}
	if !removalRequestsInclude(removalsAtStart[1], first) {
		t.Fatalf("the second start happened before %q was removed", first)
	}
	if !removalRequestsInclude(removalsAtStart[2], first) || !removalRequestsInclude(removalsAtStart[2], second) {
		t.Fatalf("the third start happened before both predecessors were removed: %v", removalsAtStart[2])
	}

	if h.fixture.Runtime.StartCount() != 1 {
		t.Fatalf("runtime start effects = %d, want only the last identity", h.fixture.Runtime.StartCount())
	}
	for _, retired := range []id.WorkloadID{first, second} {
		workload, ok := h.fixture.Runtime.Workload(retired)
		if !ok || !workload.Removed || workload.Started || workload.StopRequests != 0 {
			t.Fatalf("retired identity %q = %+v, want removed, never started, no stop", retired, workload)
		}
	}
	if workload, ok := h.fixture.Runtime.Workload(third); !ok || workload.StartCount != 1 {
		t.Fatalf("last identity %q = %+v, want started exactly once", third, workload)
	}
}

// ---------------------------------------------------------------------------
// Stale markUnknownAfterStart never clobbers a replaced or settled action
// ---------------------------------------------------------------------------

// stopAfterReplacementState cancels its context immediately after a replacement
// not_issued identity is committed, leaving the replacement in LaunchNotIssued
// before its own launch marker is issued.
type stopAfterReplacementState struct {
	engine.State

	executionID id.ExecutionID
	cancel      context.CancelFunc
	once        sync.Once
}

func (s *stopAfterReplacementState) Commit(ctx context.Context, commit flow.Commit) (kernel.Execution, error) {
	isReplacement := commit.Launch != nil && commit.Launch.State == kernel.LaunchNotIssued && s.replacesExisting(ctx, commit)
	updated, err := s.State.Commit(ctx, commit)
	if err == nil && isReplacement {
		s.once.Do(s.cancel)
	}
	return updated, err
}

func (s *stopAfterReplacementState) replacesExisting(ctx context.Context, commit flow.Commit) bool {
	current, err := s.State.Load(ctx, s.executionID)
	if err != nil {
		return false
	}
	for _, action := range current.Actions {
		if action.ID != commit.ActionID || action.Container == nil {
			continue
		}
		return action.Container.WorkloadID != "" &&
			commit.Launch.WorkloadID != action.Container.WorkloadID
	}
	return false
}

// conflictThenNotIssuedState simulates version conflicts on LaunchUnknown
// commits while keeping the same unissued attempt identity, exercising the
// bounded reload+commit loop in markUnknownAfterStart.
type conflictThenNotIssuedState struct {
	engine.State

	store *testkit.Store

	mu                sync.Mutex
	unknownCollisions int
}

func (s *conflictThenNotIssuedState) Commit(ctx context.Context, commit flow.Commit) (kernel.Execution, error) {
	if commit.Launch != nil && commit.Launch.State == kernel.LaunchUnknown {
		s.mu.Lock()
		s.unknownCollisions++
		attempt := s.unknownCollisions
		s.mu.Unlock()
		if attempt <= 2 {
			current, err := s.State.Load(ctx, commit.ExecutionID)
			if err != nil {
				return kernel.Execution{}, err
			}
			current.Version++
			for i := range current.Actions {
				if current.Actions[i].ID == commit.ActionID && current.Actions[i].Container != nil {
					current.Actions[i].Version++
					container := *current.Actions[i].Container
					container.Launch.State = kernel.LaunchNotIssued
					current.Actions[i].Container = &container
				}
			}
			s.store.Seed(current)
			return kernel.Execution{}, fault.New(fault.VersionConflict, "injected concurrent version bump")
		}
	}
	return s.State.Commit(ctx, commit)
}

func (s *conflictThenNotIssuedState) collisionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unknownCollisions
}

func TestConcurrentRetireAndReplaceNotClobberedByStaleUnknownLaunch(t *testing.T) {
	t.Run("retire-then-replace to workload B is preserved", func(t *testing.T) {
		build := containerAction("act-build", "build", 0, time.Hour)
		h := newHarness(t, newExecution("exec-stale-unknown-replace", "ws-stale-unknown-replace", build))
		h.fixture.Runtime.FailNextStartBeforeEffect(errors.New("injected start transport failure"))

		gate := newGatedState(h.fixture.State)
		gate.gate = func(commit flow.Commit) bool {
			return commit.Launch != nil && commit.Launch.State == kernel.LaunchUnknown
		}
		staleEngine := engine.New(gate, h.runtime, h.fixture.Workspace, h.fixture.Clock)

		ctx, cancel := context.WithTimeout(h.t.Context(), 10*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := staleEngine.Reconcile(ctx, h.executionID)
			done <- err
		}()
		waitGate(t, gate, ctx)

		atGate := mustAction(t, h.load(), "build")
		workloadA := atGate.Container.WorkloadID
		if workloadA == "" || atGate.Container.Launch.State != kernel.LaunchIssued {
			t.Fatalf("state at gate = %+v, want issued identity A", atGate.Container)
		}

		// Concurrently observe A as never-started, retire and remove A, and
		// commit replacement B in LaunchNotIssued before releasing the stale
		// LaunchUnknown commit for A.
		replaceCtx, stopReplace := context.WithCancel(ctx)
		defer stopReplace()
		replaceState := &stopAfterReplacementState{
			State:       h.fixture.State,
			executionID: h.executionID,
			cancel:      stopReplace,
		}
		replaceEngine := engine.New(replaceState, h.runtime, h.fixture.Workspace, h.fixture.Clock)
		if _, err := replaceEngine.Reconcile(replaceCtx, h.executionID); !errors.Is(err, context.Canceled) {
			t.Fatalf("replaceEngine.Reconcile error = %v, want context.Canceled after replacement commit", err)
		}

		replaced := mustAction(t, h.load(), "build")
		workloadB := replaced.Container.WorkloadID
		keyB := replaced.Container.Launch.IdempotencyKey
		if workloadB == "" || workloadB == workloadA {
			t.Fatalf("replacement workload = %q, want a distinct identity from %q", workloadB, workloadA)
		}
		if replaced.Container.Launch.State != kernel.LaunchNotIssued {
			t.Fatalf("replacement launch state before stale release = %q, want not_issued", replaced.Container.Launch.State)
		}

		gate.releaseGate()
		if err := <-done; err != nil {
			t.Fatalf("stale Reconcile error = %v, want nil", err)
		}
		if gate.conflictCount() == 0 {
			t.Fatal("stale LaunchUnknown commit never observed a real version conflict")
		}

		afterStale := mustAction(t, h.load(), "build")
		if afterStale.Container.WorkloadID != workloadB {
			t.Fatalf("markUnknownAfterStart clobbered replacement %q back to %q", workloadB, afterStale.Container.WorkloadID)
		}
		if afterStale.Container.Launch.WorkloadID != workloadB {
			t.Fatalf("launch workload = %q, want replacement %q", afterStale.Container.Launch.WorkloadID, workloadB)
		}
		if afterStale.Container.Launch.IdempotencyKey != keyB {
			t.Fatalf("launch idempotency key = %q, want replacement %q", afterStale.Container.Launch.IdempotencyKey, keyB)
		}
		if afterStale.Container.Launch.State != kernel.LaunchNotIssued {
			t.Fatalf("launch state = %q, want untouched not_issued for replacement %q", afterStale.Container.Launch.State, workloadB)
		}

		// Replacement B still starts cleanly on the next pass.
		started := h.reconcileUntil("replacement B started", 8, func(candidate kernel.Execution) bool {
			return h.fixture.Runtime.StartCount() == 1
		})
		startedAction := mustAction(t, started, "build")
		if startedAction.Container.WorkloadID != workloadB || startedAction.Container.Launch.State != kernel.LaunchIssued {
			t.Fatalf("started replacement = %+v, want issued %q", startedAction.Container, workloadB)
		}
	})

	t.Run("same not_issued identity retries version conflict", func(t *testing.T) {
		build := containerAction("act-build", "build", 0, time.Hour)
		h := newHarness(t, newExecution("exec-unknown-retry", "ws-unknown-retry", build))
		h.fixture.Runtime.FailNextStartBeforeEffect(errors.New("injected start transport failure"))

		retrying := &conflictThenNotIssuedState{
			State: h.fixture.State,
			store: h.fixture.Store,
		}
		h.engine = engine.New(retrying, h.runtime, h.fixture.Workspace, h.fixture.Clock)

		reconciled := h.reconcile()
		if retrying.collisionCount() != 3 {
			t.Fatalf("LaunchUnknown commit attempts = %d, want 3 (initial + 2 markUnknownAfterStart attempts)", retrying.collisionCount())
		}
		action := mustAction(t, reconciled, "build")
		if action.Container.Launch.State != kernel.LaunchUnknown {
			t.Fatalf("launch state after retry = %q, want unknown", action.Container.Launch.State)
		}
	})
}

// ---------------------------------------------------------------------------
// Running-action observe maps ErrNotPrepared / fault.NotFound to absent
// ---------------------------------------------------------------------------

type observeErrorRuntime struct {
	engine.Runtime

	mu     sync.Mutex
	err    error
	cancel context.CancelFunc
	calls  int
}

func (r *observeErrorRuntime) Observe(ctx context.Context, request engine.ObserveRequest) (kernel.Observation, error) {
	r.mu.Lock()
	r.calls++
	err := r.err
	cancel := r.cancel
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if err != nil {
		return kernel.Observation{}, err
	}
	return r.Runtime.Observe(ctx, request)
}

func (r *observeErrorRuntime) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func TestRunningObserveMapsNotPreparedAndNotFoundToAbsent(t *testing.T) {
	absenceErrors := []struct {
		name string
		tag  string
		err  error
	}{
		{name: "ErrNotPrepared", tag: "not-prepared", err: engine.ErrNotPrepared},
		{name: "fault.NotFound", tag: "not-found", err: fault.New(fault.NotFound, "runtime object missing")},
	}

	for _, launchState := range []kernel.LaunchState{kernel.LaunchIssued, kernel.LaunchUnknown} {
		for _, tc := range absenceErrors {
			t.Run(string(launchState)+"/"+tc.name, func(t *testing.T) {
				execID := "exec-obs-" + string(launchState) + "-" + tc.tag
				wsID := "ws-obs-" + string(launchState) + "-" + tc.tag
				seeded := runningContainerExecution(execID, wsID, testNow.Add(time.Hour))
				seeded.Actions[0].Container.Launch.State = launchState
				workloadID := seeded.Actions[0].Container.WorkloadID
				h := newHarness(t, seeded)
				h.retainWorkspace()

				wrapper := &observeErrorRuntime{Runtime: h.runtime, err: tc.err}
				h.engine = engine.New(h.fixture.State, wrapper, h.fixture.Workspace, h.fixture.Clock)

				observed, err := h.engine.Reconcile(h.t.Context(), h.executionID)
				if err != nil {
					t.Fatalf("Reconcile() error = %v, want nil absent mapping", err)
				}
				if wrapper.callCount() != 1 {
					t.Fatalf("Observe calls = %d, want 1", wrapper.callCount())
				}
				if observed.State != kernel.ExecutionRunning || observed.CurrentAction != 0 {
					t.Fatalf("execution = %s, want running current=0", actionSummary(observed))
				}
				action := mustAction(t, observed, "build")
				if action.State != kernel.ActionRunning {
					t.Fatalf("action state = %q, want running", action.State)
				}
				if action.Container.Observation.Kind != kernel.ObservationAbsent {
					t.Fatalf("observation = %+v, want absent", action.Container.Observation)
				}
				if action.Container.Observation.ProofOfNonexecution() {
					t.Fatalf("absent mapping fabricated never-started proof: %+v", action.Container.Observation)
				}
				if action.Container.Launch.State != launchState {
					t.Fatalf("launch state = %q, want unchanged %q", action.Container.Launch.State, launchState)
				}
				if action.Container.WorkloadID != workloadID {
					t.Fatalf("workload = %q, want unchanged %q", action.Container.WorkloadID, workloadID)
				}
				if h.runtime.startCount() != 0 || h.runtime.stopCount() != 0 || h.runtime.removeCount() != 0 {
					t.Fatalf("absent observe triggered effects: starts=%d stops=%d removes=%d",
						h.runtime.startCount(), h.runtime.stopCount(), h.runtime.removeCount())
				}

				commitsBefore := h.fixture.Store.CommitCount()
				again, err := h.engine.Reconcile(h.t.Context(), h.executionID)
				if err != nil {
					t.Fatalf("second Reconcile() error = %v, want nil", err)
				}
				if h.fixture.Store.CommitCount() != commitsBefore {
					t.Fatalf("duplicate absent observation committed again: %d -> %d", commitsBefore, h.fixture.Store.CommitCount())
				}
				if mustAction(t, again, "build").Container.Observation.Kind != kernel.ObservationAbsent {
					t.Fatalf("observation after second reconcile = %+v, want absent", mustAction(t, again, "build").Container.Observation)
				}
			})
		}
	}

	t.Run("cancelled context returns ctx error without recording absent", func(t *testing.T) {
		seeded := runningContainerExecution("exec-obs-ctx-cancel", "ws-obs-ctx-cancel", testNow.Add(time.Hour))
		h := newHarness(t, seeded)
		h.retainWorkspace()

		ctx, cancel := context.WithCancel(h.t.Context())
		wrapper := &observeErrorRuntime{
			Runtime: h.runtime,
			err:     engine.ErrNotPrepared,
			cancel:  cancel,
		}
		h.engine = engine.New(h.fixture.State, wrapper, h.fixture.Workspace, h.fixture.Clock)

		if _, err := h.engine.Reconcile(ctx, h.executionID); !errors.Is(err, context.Canceled) {
			t.Fatalf("Reconcile() error = %v, want context.Canceled", err)
		}
		if h.fixture.Store.CommitCount() != 0 {
			t.Fatalf("cancelled observe committed %d times, want 0", h.fixture.Store.CommitCount())
		}
	})

	t.Run("non-absence observe error still surfaces", func(t *testing.T) {
		seeded := runningContainerExecution("exec-obs-dep-err", "ws-obs-dep-err", testNow.Add(time.Hour))
		h := newHarness(t, seeded)
		h.retainWorkspace()

		wrapper := &observeErrorRuntime{
			Runtime: h.runtime,
			err:     fault.New(fault.DependencyUnavailable, "runtime unreachable"),
		}
		h.engine = engine.New(h.fixture.State, wrapper, h.fixture.Workspace, h.fixture.Clock)

		_, err := h.engine.Reconcile(h.t.Context(), h.executionID)
		mustClass(t, err, fault.DependencyUnavailable)
		if h.fixture.Store.CommitCount() != 0 {
			t.Fatalf("failed observe committed %d times, want 0", h.fixture.Store.CommitCount())
		}
	})
}
