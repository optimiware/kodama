package enginetest

// Engine regressions for the retire-then-replace launch protocol.
//
// A proven-not-started identity A superseded by replacement B must be
// committed as `retiring` with its persisted proof before any removal, and B
// must be committed only after Remove(A) is confirmed. A transient Remove(A)
// failure or a crash at Remove leaves A retiring and tracked, so a later
// Reconcile retries the removal and only then allocates B instead of leaving an
// untracked resource that wedges workspace release.
//
// Only ErrNotPrepared is affirmative never-prepared evidence. A generic
// fault.NotFound from Observe or from an unproven Remove must not skip Stop,
// release the workspace or fabricate stop evidence. A NotFound from Remove is
// accepted only after confirmed stop or never-started proof.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
)

// ---------------------------------------------------------------------------
// Runtime wrappers
// ---------------------------------------------------------------------------

// cancelAtRemoveRuntime cancels the driving context exactly when the engine
// issues Remove, simulating a crash after the never-started proof and before
// removal completes. The call is delegated so the recording wrapper still sees
// the attempt and the fake returns the cancelled context without removing.
type cancelAtRemoveRuntime struct {
	engine.Runtime

	once   sync.Once
	cancel context.CancelFunc
}

func (r *cancelAtRemoveRuntime) Remove(ctx context.Context, request engine.RemoveRequest) (engine.Removal, error) {
	r.once.Do(r.cancel)
	return r.Runtime.Remove(ctx, request)
}

// faultInjectingRuntime returns a generic fault.NotFound (never ErrNotPrepared)
// for the selected workload identities. removeCalls counts Remove requests the
// engine actually issued so a test can prove it never reached Remove.
type faultInjectingRuntime struct {
	engine.Runtime

	mu              sync.Mutex
	observeNotFound map[id.WorkloadID]bool
	removeNotFound  map[id.WorkloadID]bool
	removeCalls     int
}

func newFaultInjectingRuntime(runtime engine.Runtime) *faultInjectingRuntime {
	return &faultInjectingRuntime{
		Runtime:         runtime,
		observeNotFound: make(map[id.WorkloadID]bool),
		removeNotFound:  make(map[id.WorkloadID]bool),
	}
}

func (r *faultInjectingRuntime) Observe(ctx context.Context, request engine.ObserveRequest) (kernel.Observation, error) {
	r.mu.Lock()
	inject := r.observeNotFound[request.Identity.WorkloadID]
	r.mu.Unlock()
	if inject {
		return kernel.Observation{}, fault.New(fault.NotFound, "injected: runtime has no resource for the identity")
	}
	return r.Runtime.Observe(ctx, request)
}

func (r *faultInjectingRuntime) Remove(ctx context.Context, request engine.RemoveRequest) (engine.Removal, error) {
	r.mu.Lock()
	r.removeCalls++
	inject := r.removeNotFound[request.Identity.WorkloadID]
	r.mu.Unlock()
	removal, err := r.Runtime.Remove(ctx, request)
	if inject {
		return engine.Removal{}, fault.New(fault.NotFound, "injected: runtime has no resource for the identity")
	}
	return removal, err
}

func (r *faultInjectingRuntime) interceptObserve(workloadID id.WorkloadID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observeNotFound[workloadID] = true
}

func (r *faultInjectingRuntime) interceptRemove(workloadID id.WorkloadID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removeNotFound[workloadID] = true
}

func (r *faultInjectingRuntime) removeCallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.removeCalls
}

// ---------------------------------------------------------------------------
// Remove A before committing replacement B
// ---------------------------------------------------------------------------

// TestProvenNotStartedRemoveFailureKeepsIdentityTrackedThenRetries pins the
// StartProvenNotStarted path: the retirement commit (retiring state plus the
// persisted proof) precedes Remove, and a transient Remove(A) failure must not
// commit the replacement B. A stays retiring and tracked, a later Reconcile
// retries the removal and only then starts B, and cleanup completes only after
// A is gone.
func TestProvenNotStartedRemoveFailureKeepsIdentityTrackedThenRetries(t *testing.T) {
	build := containerAction("act-build", "build", 0, time.Hour)
	h := newHarness(t, newExecution("exec-p1-001-retry", "ws-p1-001-retry", build))
	h.fixture.Runtime.RefuseNextStart("runtime proved the command never ran")
	h.fixture.Runtime.FailNextRemove(errors.New("injected transient removal failure"))

	if _, err := h.engine.Reconcile(h.t.Context(), h.executionID); err == nil {
		t.Fatal("Reconcile hid the transient Remove failure")
	}

	starts := h.runtime.startRequests()
	if len(starts) != 1 {
		t.Fatalf("Start calls = %d, want the single refused attempt", len(starts))
	}
	superseded := starts[0].Identity.WorkloadID

	pending := h.load()
	action := mustAction(t, pending, "build")
	if action.Container.WorkloadID != superseded {
		t.Fatalf("persisted workload = %q, want the still-tracked superseded identity %q",
			action.Container.WorkloadID, superseded)
	}
	if action.Container.Launch.State != kernel.LaunchRetiring {
		t.Fatalf("launch state after failed removal = %q, want retiring", action.Container.Launch.State)
	}
	if action.Container.Launch.WorkloadID != superseded {
		t.Fatalf("retiring launch workload = %q, want the unchanged identity %q",
			action.Container.Launch.WorkloadID, superseded)
	}
	if !action.Container.Observation.ProofOfNonexecution() {
		t.Fatalf("retirement did not retain the never-started proof: %+v", action.Container.Observation)
	}
	if pending.State != kernel.ExecutionRunning {
		t.Fatalf("execution state = %q, want visibly running", pending.State)
	}
	if pending.Cleanup.State == kernel.CleanupComplete {
		t.Fatal("failed removal reported cleanup complete")
	}
	if pending.Cleanup.Attempts != 0 {
		t.Fatalf("cleanup attempts = %d, want 0 before exhaustion", pending.Cleanup.Attempts)
	}
	if action.Stop.IsConfirmed() {
		t.Fatal("failed removal fabricated stop evidence for a never-started identity")
	}
	old, ok := h.fixture.Runtime.Workload(superseded)
	if !ok || old.Removed || old.Started {
		t.Fatalf("superseded identity after failed removal = %+v, want prepared, not started, not removed", old)
	}

	// A later Reconcile observes the still-tracked identity, removes it, commits
	// the replacement and starts it.
	started := h.reconcileUntil("replacement started after retry", 10, func(candidate kernel.Execution) bool {
		return h.fixture.Runtime.StartCount() == 1
	})
	starts = h.runtime.startRequests()
	if len(starts) < 2 {
		t.Fatalf("Start calls = %d, want refused attempt plus relaunch", len(starts))
	}
	fresh := starts[1].Identity.WorkloadID
	if fresh == superseded {
		t.Fatalf("relaunch reused the superseded identity %q", superseded)
	}
	action = mustAction(t, started, "build")
	if action.Container.WorkloadID != fresh {
		t.Fatalf("persisted workload = %q, want replacement %q", action.Container.WorkloadID, fresh)
	}
	old, ok = h.fixture.Runtime.Workload(superseded)
	if !ok || !old.Removed || old.Started {
		t.Fatalf("superseded identity after retry = %+v, want affirmatively removed and never started", old)
	}
	if old.StopRequests != 0 {
		t.Fatalf("superseded identity received %d stops, want none", old.StopRequests)
	}
	if !containsWorkload(h.runtime.removedWorkloads(), superseded) {
		t.Fatalf("Remove calls = %v, want the superseded identity %q", h.runtime.removedWorkloads(), superseded)
	}
	if countWorkload(h.runtime.removedWorkloads(), superseded) < 2 {
		t.Fatalf("Remove attempts = %v, want the failed removal retried before the replacement was committed",
			h.runtime.removedWorkloads())
	}

	// The replacement settles successfully and cleanup completes only once both
	// identities are removed and the workspace is released.
	h.confirmRuntimeStop(identityFor(h.executionID, action))
	h.fixture.Runtime.QueueObservation(fresh, kernel.ExitedObservation(0, kernel.ConfirmedStop("build exited")))
	completed := h.reconcileUntil("terminal success and cleanup complete", 12, func(candidate kernel.Execution) bool {
		return candidate.State == kernel.ExecutionSucceeded && candidate.Cleanup.State == kernel.CleanupComplete
	})
	if completed.Failure != nil {
		t.Fatalf("terminal success gained a failure: %+v", completed.Failure)
	}
	if !h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("cleanup did not release the workspace after both removals")
	}
	removed := h.runtime.removedWorkloads()
	if !containsWorkload(removed, superseded) || !containsWorkload(removed, fresh) {
		t.Fatalf("removed workloads = %v, want superseded %q and replacement %q", removed, superseded, fresh)
	}
	if completed.Cleanup.Attempts != 0 {
		t.Fatalf("completed cleanup attempts = %d, want 0: the transient failure was retried before B existed", completed.Cleanup.Attempts)
	}
}

// TestProvenNotStartedRemoveCancellationKeepsIdentityTracked pins the crash
// variant of the StartProvenNotStarted path: the retirement is already durable,
// so a cancelled context at Remove leaves A retiring with its proof and
// prepared, and a later Reconcile retries the removal.
func TestProvenNotStartedRemoveCancellationKeepsIdentityTracked(t *testing.T) {
	build := containerAction("act-build", "build", 0, time.Hour)
	h := newHarness(t, newExecution("exec-p1-001-crash", "ws-p1-001-crash", build))
	h.fixture.Runtime.RefuseNextStart("runtime proved the command never ran")

	ctx, cancel := context.WithCancel(h.t.Context())
	defer cancel()
	h.engine = engine.New(h.fixture.State, &cancelAtRemoveRuntime{Runtime: h.runtime, cancel: cancel}, h.fixture.Workspace, h.fixture.Clock)
	if _, err := h.engine.Reconcile(ctx, h.executionID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Reconcile error = %v, want context.Canceled", err)
	}

	starts := h.runtime.startRequests()
	if len(starts) != 1 {
		t.Fatalf("Start calls = %d, want the single refused attempt", len(starts))
	}
	superseded := starts[0].Identity.WorkloadID
	pending := h.load()
	action := mustAction(t, pending, "build")
	if action.Container.WorkloadID != superseded || action.Container.Launch.State != kernel.LaunchRetiring {
		t.Fatalf("identity after crash = %q/%q, want the tracked retiring identity %q",
			action.Container.WorkloadID, action.Container.Launch.State, superseded)
	}
	if !action.Container.Observation.ProofOfNonexecution() {
		t.Fatalf("retirement did not retain the never-started proof: %+v", action.Container.Observation)
	}
	old, ok := h.fixture.Runtime.Workload(superseded)
	if !ok || old.Removed || old.Started {
		t.Fatalf("superseded identity after crash = %+v, want prepared, not started, not removed", old)
	}

	// A fresh Reconcile retries the removal and starts the replacement.
	h.engine = engine.New(h.fixture.State, h.runtime, h.fixture.Workspace, h.fixture.Clock)
	started := h.reconcileUntil("replacement started after crash", 10, func(candidate kernel.Execution) bool {
		return h.fixture.Runtime.StartCount() == 1
	})
	starts = h.runtime.startRequests()
	fresh := starts[len(starts)-1].Identity.WorkloadID
	if fresh == superseded {
		t.Fatalf("relaunch reused the superseded identity %q", superseded)
	}
	if action = mustAction(t, started, "build"); action.Container.WorkloadID != fresh {
		t.Fatalf("persisted workload = %q, want replacement %q", action.Container.WorkloadID, fresh)
	}
	old, ok = h.fixture.Runtime.Workload(superseded)
	if !ok || !old.Removed || old.Started {
		t.Fatalf("superseded identity after retry = %+v, want removed and never started", old)
	}
}

// TestObserveNeverStartedRemoveFailureKeepsIdentityTracked pins the Observe
// NeverStarted path: the retirement commit with its proof precedes Remove, a
// transient Remove(A) failure must not commit B, and a later Reconcile retries
// the removal before starting the replacement.
func TestObserveNeverStartedRemoveFailureKeepsIdentityTracked(t *testing.T) {
	seeded := runningContainerExecution("exec-p1-001-observe", "ws-p1-001-observe", testNow.Add(time.Hour))
	h := newHarness(t, seeded)
	h.retainWorkspace()
	identity := identityFor(seeded.ID, seeded.Actions[0])
	h.prepareOnly(identity, seeded.Actions[0].Container.Definition)
	h.fixture.Runtime.FailNextRemove(errors.New("injected transient removal failure"))

	if _, err := h.engine.Reconcile(h.t.Context(), h.executionID); err == nil {
		t.Fatal("Reconcile hid the transient Remove failure")
	}

	pending := h.load()
	action := mustAction(t, pending, "build")
	if action.Container.WorkloadID != identity.WorkloadID || action.Container.Launch.State != kernel.LaunchRetiring {
		t.Fatalf("identity after failed removal = %q/%q, want the tracked retiring identity %q",
			action.Container.WorkloadID, action.Container.Launch.State, identity.WorkloadID)
	}
	if !action.Container.Observation.ProofOfNonexecution() {
		t.Fatalf("retirement did not retain the never-started proof: %+v", action.Container.Observation)
	}
	if h.runtime.startCount() != 0 {
		t.Fatalf("Start calls = %d, want none before the replacement is durable", h.runtime.startCount())
	}
	old, ok := h.fixture.Runtime.Workload(identity.WorkloadID)
	if !ok || old.Removed || old.Started {
		t.Fatalf("never-started identity after failed removal = %+v, want prepared, not started, not removed", old)
	}

	started := h.reconcileUntil("replacement started after retry", 10, func(candidate kernel.Execution) bool {
		return h.fixture.Runtime.StartCount() == 1
	})
	starts := h.runtime.startRequests()
	if len(starts) != 1 {
		t.Fatalf("Start calls = %d, want only the fresh replacement", len(starts))
	}
	fresh := starts[0].Identity.WorkloadID
	if fresh == identity.WorkloadID {
		t.Fatalf("relaunch reused the superseded identity %q", identity.WorkloadID)
	}
	if action = mustAction(t, started, "build"); action.Container.WorkloadID != fresh {
		t.Fatalf("persisted workload = %q, want replacement %q", action.Container.WorkloadID, fresh)
	}
	old, ok = h.fixture.Runtime.Workload(identity.WorkloadID)
	if !ok || !old.Removed || old.Started {
		t.Fatalf("superseded identity after retry = %+v, want removed and never started", old)
	}
	if old.StopRequests != 0 {
		t.Fatalf("superseded identity received %d stops, want none", old.StopRequests)
	}
	if !containsWorkload(h.runtime.removedWorkloads(), identity.WorkloadID) {
		t.Fatalf("Remove calls = %v, want the superseded identity %q", h.runtime.removedWorkloads(), identity.WorkloadID)
	}
}

// TestObserveNeverStartedRemoveCancellationKeepsIdentityTracked pins the crash
// variant of the Observe NeverStarted path: the retirement is durable before
// Remove, so a cancelled context must not strand A or commit B, and a later
// Reconcile retries the removal from the persisted proof.
func TestObserveNeverStartedRemoveCancellationKeepsIdentityTracked(t *testing.T) {
	seeded := runningContainerExecution("exec-p1-001-observe-crash", "ws-p1-001-observe-crash", testNow.Add(time.Hour))
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

	pending := h.load()
	action := mustAction(t, pending, "build")
	if action.Container.WorkloadID != identity.WorkloadID || action.Container.Launch.State != kernel.LaunchRetiring {
		t.Fatalf("identity after crash = %q/%q, want the tracked retiring identity %q",
			action.Container.WorkloadID, action.Container.Launch.State, identity.WorkloadID)
	}
	if !action.Container.Observation.ProofOfNonexecution() {
		t.Fatalf("retirement did not retain the never-started proof: %+v", action.Container.Observation)
	}
	old, ok := h.fixture.Runtime.Workload(identity.WorkloadID)
	if !ok || old.Removed || old.Started {
		t.Fatalf("never-started identity after crash = %+v, want prepared, not started, not removed", old)
	}

	h.engine = engine.New(h.fixture.State, h.runtime, h.fixture.Workspace, h.fixture.Clock)
	started := h.reconcileUntil("replacement started after crash", 10, func(candidate kernel.Execution) bool {
		return h.fixture.Runtime.StartCount() == 1
	})
	starts := h.runtime.startRequests()
	fresh := starts[len(starts)-1].Identity.WorkloadID
	if fresh == identity.WorkloadID {
		t.Fatalf("relaunch reused the superseded identity %q", identity.WorkloadID)
	}
	if action = mustAction(t, started, "build"); action.Container.WorkloadID != fresh {
		t.Fatalf("persisted workload = %q, want replacement %q", action.Container.WorkloadID, fresh)
	}
	old, ok = h.fixture.Runtime.Workload(identity.WorkloadID)
	if !ok || !old.Removed || old.Started {
		t.Fatalf("superseded identity after retry = %+v, want removed and never started", old)
	}
}

// ---------------------------------------------------------------------------
// Generic NotFound is not absence or stop evidence
// ---------------------------------------------------------------------------

// terminalStartedUnknownExecution seeds a terminal failure whose container
// identity may have started (LaunchUnknown) and has no confirmed stop.
func terminalStartedUnknownExecution(executionID, workspaceID string) (kernel.Execution, engine.EffectIdentity) {
	execution, identity := terminalNeverStartedExecution(executionID, workspaceID)
	execution.Actions[0].Container.Launch.State = kernel.LaunchUnknown
	return execution, identity
}

// TestCleanupGenericNotFoundDoesNotProveAbsence pins that a generic NotFound
// from Observe or from an unproven Remove never releases the workspace or
// fabricates stop evidence, while a NotFound after confirmed stop or
// never-started proof is accepted as already-clean.
func TestCleanupGenericNotFoundDoesNotProveAbsence(t *testing.T) {
	t.Run("observe and remove notfound stay pending", func(t *testing.T) {
		execution, identity := terminalStartedUnknownExecution("exec-p1-002-observe", "ws-p1-002-observe")
		h := newHarness(t, execution)
		h.retainWorkspace()
		h.prepareAndStart(identity, execution.Actions[0].Container.Definition)

		injected := newFaultInjectingRuntime(h.runtime)
		injected.interceptObserve(identity.WorkloadID)
		injected.interceptRemove(identity.WorkloadID)
		h.engine = engine.New(h.fixture.State, injected, h.fixture.Workspace, h.fixture.Clock)

		pending := h.reconcile()
		if pending.Cleanup.State != kernel.CleanupPending {
			t.Fatalf("cleanup state = %q, want pending", pending.Cleanup.State)
		}
		if pending.Cleanup.Attempts != 1 {
			t.Fatalf("cleanup attempts = %d, want 1 attribution", pending.Cleanup.Attempts)
		}
		if !strings.Contains(pending.Cleanup.LastError, "not_found") {
			t.Fatalf("cleanup last error = %q, want the NotFound attribution", pending.Cleanup.LastError)
		}
		if h.fixture.Workspace.IsReleased(h.workspaceID) {
			t.Fatal("generic NotFound from Observe released the workspace")
		}
		action := mustAction(t, pending, "build")
		if action.Stop.IsConfirmed() {
			t.Fatal("generic NotFound fabricated stop evidence")
		}
		if injected.removeCallCount() != 0 {
			t.Fatalf("Remove calls = %d, want none after an unproven NotFound observation", injected.removeCallCount())
		}
		workload, ok := h.fixture.Runtime.Workload(identity.WorkloadID)
		if !ok || !workload.Started || workload.Removed {
			t.Fatalf("workload after unproven NotFound = %+v, want started and not removed", workload)
		}
	})

	t.Run("remove notfound without stop evidence stays pending", func(t *testing.T) {
		execution, identity := terminalStartedUnknownExecution("exec-p1-002-remove", "ws-p1-002-remove")
		h := newHarness(t, execution)
		h.retainWorkspace()
		h.prepareAndStart(identity, execution.Actions[0].Container.Definition)

		injected := newFaultInjectingRuntime(h.runtime)
		injected.interceptRemove(identity.WorkloadID)
		h.engine = engine.New(h.fixture.State, injected, h.fixture.Workspace, h.fixture.Clock)

		pending := h.reconcile()
		if pending.Cleanup.State != kernel.CleanupPending {
			t.Fatalf("cleanup state = %q, want pending", pending.Cleanup.State)
		}
		if h.fixture.Workspace.IsReleased(h.workspaceID) {
			t.Fatal("generic NotFound from Remove released the workspace before stop evidence")
		}
		if injected.removeCallCount() != 0 {
			t.Fatalf("Remove calls = %d, want none before confirmed stop", injected.removeCallCount())
		}
		if mustAction(t, pending, "build").Stop.IsConfirmed() {
			t.Fatal("unconfirmed stop was promoted to confirmed")
		}
	})

	t.Run("remove notfound after confirmed stop completes", func(t *testing.T) {
		execution, identity := terminalFailedExecutionWithCleanup("exec-p1-002-stopped", "ws-p1-002-stopped")
		h := newHarness(t, execution)
		h.retainWorkspace()
		h.prepareAndStart(identity, execution.Actions[0].Container.Definition)
		h.confirmRuntimeStop(identity)

		injected := newFaultInjectingRuntime(h.runtime)
		injected.interceptRemove(identity.WorkloadID)
		h.engine = engine.New(h.fixture.State, injected, h.fixture.Workspace, h.fixture.Clock)

		completed := h.reconcileUntil("cleanup complete after confirmed stop", 6, func(candidate kernel.Execution) bool {
			return candidate.Cleanup.State == kernel.CleanupComplete
		})
		if !h.fixture.Workspace.IsReleased(h.workspaceID) {
			t.Fatal("confirmed stop did not allow an already-absent removal")
		}
		if completed.Cleanup.Attempts != 0 {
			t.Fatalf("cleanup attempts = %d, want 0", completed.Cleanup.Attempts)
		}
	})

	t.Run("remove notfound after never-started proof completes", func(t *testing.T) {
		execution, identity := terminalNeverStartedExecution("exec-p1-002-never", "ws-p1-002-never")
		h := newHarness(t, execution)
		h.retainWorkspace()
		h.prepareOnly(identity, execution.Actions[0].Container.Definition)

		injected := newFaultInjectingRuntime(h.runtime)
		injected.interceptRemove(identity.WorkloadID)
		h.engine = engine.New(h.fixture.State, injected, h.fixture.Workspace, h.fixture.Clock)

		completed := h.reconcileUntil("cleanup complete after never-started proof", 6, func(candidate kernel.Execution) bool {
			return candidate.Cleanup.State == kernel.CleanupComplete
		})
		if !h.fixture.Workspace.IsReleased(h.workspaceID) {
			t.Fatal("never-started proof did not allow an already-absent removal")
		}
		action := mustAction(t, completed, "build")
		if action.Stop.IsConfirmed() {
			t.Fatal("never-started removal fabricated stop evidence")
		}
		if h.runtime.stopCount() != 0 {
			t.Fatalf("Stop calls = %d, want none for a never-started identity", h.runtime.stopCount())
		}
	})
}
