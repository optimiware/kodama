package enginetest

// Engine regressions for never-started cleanup and commit-boundary races.
//
// Every prepared identity is LaunchIssued before Prepare, so terminal cleanup
// can remove an affirmatively never-started identity without a Stop.
// Reconcile re-reads after a real version conflict and never replays or
// overwrites the concurrent winner. SubmitOutputs and a confirmed settlement
// serialize on the same commit boundary in both orders.

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
)

// ---------------------------------------------------------------------------
// Prepared-never-started cleanup
// ---------------------------------------------------------------------------

// TestCancelDuringPrepareLeavesNeverStartedIdentityForCleanup pins that a
// Cancel that wins while Prepare is in flight prevents the stale attempt from
// starting. The prepared identity is removed by cleanup without a Stop, and the
// proof of nonexecution is the engine's own persisted evidence: the runtime
// reports absence and an unconfirmed stop, so it cannot supply that knowledge.
func TestCancelDuringPrepareLeavesNeverStartedIdentityForCleanup(t *testing.T) {
	build := containerAction("act-build", "build", 0, time.Hour)
	h := newHarness(t, newExecution("exec-cancel-during-prepare", "ws-cancel-during-prepare", build))

	prepared := make(chan struct{})
	resume := make(chan struct{})
	h.runtime.afterPrepare = func(ctx context.Context) {
		close(prepared)
		select {
		case <-resume:
		case <-ctx.Done():
		}
	}
	runtime := newPrepareCancelRuntime(h.runtime)
	h.engine = engine.New(h.fixture.State, runtime, h.fixture.Workspace, h.fixture.Clock)

	ctx, cancel := context.WithTimeout(h.t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := h.engine.Reconcile(ctx, h.executionID)
		done <- err
	}()
	waitPrepared(t, prepared, ctx)

	// Cancel wins after the marker commit and successful Prepare but before the
	// engine issues Start.
	h.cancel(flow.CancelRequested)
	close(resume)
	if err := <-done; err != nil {
		t.Fatalf("Reconcile error = %v", err)
	}

	if h.runtime.startCount() != 0 || h.fixture.Runtime.StartCount() != 0 {
		t.Fatalf("cancelled-during-prepare execution issued a start: calls=%d effects=%d",
			h.runtime.startCount(), h.fixture.Runtime.StartCount())
	}
	action := mustAction(t, h.load(), "build")
	identity := identityFor(h.executionID, action)
	if !action.Container.Observation.ProofOfNonexecution() {
		t.Fatalf("cancellation winner has no persisted never-started proof: observation=%+v",
			action.Container.Observation)
	}
	workload, ok := h.fixture.Runtime.Workload(identity.WorkloadID)
	if !ok {
		t.Fatalf("prepared identity %q was not materialized", identity.WorkloadID)
	}
	if workload.Started {
		t.Fatalf("prepared identity %q started after Cancel won", identity.WorkloadID)
	}

	completed := h.reconcileUntil("cleanup complete", 10, func(candidate kernel.Execution) bool {
		return candidate.Cleanup.State == kernel.CleanupComplete
	})
	if completed.State != kernel.ExecutionCancelled {
		t.Fatalf("terminal state = %q, want cancelled", completed.State)
	}
	if completed.Failure != nil {
		t.Fatalf("cancel outcome was overwritten: %+v", completed.Failure)
	}
	if !h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("cleanup did not release the workspace")
	}
	if !containsWorkload(h.runtime.removedWorkloads(), identity.WorkloadID) {
		t.Fatalf("cleanup removed %v, want never-started identity %q", h.runtime.removedWorkloads(), identity.WorkloadID)
	}
	if _, _, observes, stops, removes := runtime.counts(); observes != 0 || stops != 0 || removes != 1 {
		t.Fatalf("cleanup observe/stop/remove = %d/%d/%d, want 0/0/1 from the persisted proof",
			observes, stops, removes)
	}
	workload, _ = h.fixture.Runtime.Workload(identity.WorkloadID)
	if !workload.Removed || workload.Started {
		t.Fatalf("workload after cleanup = %+v, want removed and never started", workload)
	}
	if workload.StopRequests != 0 {
		t.Fatalf("never-started cleanup requested %d stops, want none", workload.StopRequests)
	}
}

// TestExpiryDuringPrepareLeavesNeverStartedIdentityForCleanup is the expiry
// variant: the deadline wins while Prepare is in flight, so the attempt never
// starts, the timeout outcome is preserved and cleanup removes the prepared
// identity from the engine's persisted never-started proof alone.
func TestExpiryDuringPrepareLeavesNeverStartedIdentityForCleanup(t *testing.T) {
	deadline := testNow.Add(time.Hour)
	seeded := runningContainerExecution("exec-expire-during-prepare", "ws-expire-during-prepare", deadline)
	seeded.Actions[0].Container.Launch.State = kernel.LaunchNotIssued
	h := newHarness(t, seeded)
	h.retainWorkspace()
	runtime := newPrepareCancelRuntime(h.runtime)
	h.engine = engine.New(h.fixture.State, runtime, h.fixture.Workspace, h.fixture.Clock)

	prepared := make(chan struct{})
	resume := make(chan struct{})
	h.runtime.afterPrepare = func(ctx context.Context) {
		close(prepared)
		select {
		case <-resume:
		case <-ctx.Done():
		}
	}

	ctx, cancel := context.WithTimeout(h.t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := h.engine.Reconcile(ctx, h.executionID)
		done <- err
	}()
	waitPrepared(t, prepared, ctx)

	// Expiry wins while Prepare is blocked. The marker was committed before
	// Prepare, so the prepared identity is durable.
	current, err := h.fixture.State.Load(ctx, h.executionID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	h.fixture.Clock.Set(deadline)
	_, commit, err := flow.Reduce(current, flow.ExpireFact{
		ExecutionID: h.executionID,
		ActionID:    "act-build",
		At:          deadline,
	})
	if err != nil || commit == nil {
		t.Fatalf("Reduce(ExpireFact) = commit %v, error %v", commit, err)
	}
	if _, err := h.fixture.State.Commit(ctx, *commit); err != nil {
		t.Fatalf("expiry commit error = %v", err)
	}
	close(resume)
	if err := <-done; err != nil {
		t.Fatalf("Reconcile error = %v", err)
	}

	if h.runtime.startCount() != 0 || h.fixture.Runtime.StartCount() != 0 {
		t.Fatalf("expired-during-prepare execution issued a start: calls=%d effects=%d",
			h.runtime.startCount(), h.fixture.Runtime.StartCount())
	}
	identity := identityFor(h.executionID, mustAction(t, h.load(), "build"))
	workload, ok := h.fixture.Runtime.Workload(identity.WorkloadID)
	if !ok || workload.Started {
		t.Fatalf("prepared identity after expiry = %+v, want prepared and never started", workload)
	}
	expiredAction := mustAction(t, h.load(), "build")
	if !expiredAction.Container.Observation.ProofOfNonexecution() {
		t.Fatalf("expiry winner has no persisted never-started proof: observation=%+v",
			expiredAction.Container.Observation)
	}

	completed := h.reconcileUntil("cleanup complete", 10, func(candidate kernel.Execution) bool {
		return candidate.Cleanup.State == kernel.CleanupComplete
	})
	if completed.State != kernel.ExecutionFailed {
		t.Fatalf("terminal state = %q, want failed", completed.State)
	}
	if completed.Failure == nil || completed.Failure.Kind != kernel.FailureTimeout {
		t.Fatalf("failure = %+v, want timeout", completed.Failure)
	}
	if !h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("cleanup did not release the workspace")
	}
	if !containsWorkload(h.runtime.removedWorkloads(), identity.WorkloadID) {
		t.Fatalf("cleanup removed %v, want never-started identity %q", h.runtime.removedWorkloads(), identity.WorkloadID)
	}
	if _, _, observes, stops, removes := runtime.counts(); observes != 0 || stops != 0 || removes != 1 {
		t.Fatalf("cleanup observe/stop/remove = %d/%d/%d, want 0/0/1 from the persisted proof",
			observes, stops, removes)
	}
}

// TestProvenNotStartedSupersededIdentityIsRemovedAndCleanupCompletes pins that
// a proven-not-started identity is retired (retiring state plus its persisted
// proof) and removed before the fresh replacement is committed, so the
// supersede path removes A and terminal cleanup removes B and releases the
// workspace.
func TestProvenNotStartedSupersededIdentityIsRemovedAndCleanupCompletes(t *testing.T) {
	build := containerAction("act-build", "build", 0, time.Hour, "candidate")
	h := newHarness(t, newExecution("exec-superseded", "ws-superseded", build))
	h.fixture.Runtime.RefuseNextStart("runtime proved the command never ran")

	// Pin the retire-then-replace ordering at the effect boundaries.
	probe := newProbeRuntime(h.runtime)
	var atRemove []kernel.Execution
	var removalsAtStart [][]engine.RemoveRequest
	probe.beforeRemove = func(engine.RemoveRequest) { atRemove = append(atRemove, h.load()) }
	probe.beforeStart = func(engine.StartRequest) {
		removalsAtStart = append(removalsAtStart, h.fixture.Runtime.Removals())
	}
	h.engine = engine.New(h.fixture.State, probe, h.fixture.Workspace, h.fixture.Clock)

	started := h.reconcileUntil("fresh identity started", 10, func(candidate kernel.Execution) bool {
		return h.fixture.Runtime.StartCount() == 1
	})
	starts := h.runtime.startRequests()
	if len(starts) < 2 {
		t.Fatalf("Start calls = %d, want a refused attempt followed by a relaunch", len(starts))
	}
	superseded := starts[0].Identity.WorkloadID
	fresh := starts[1].Identity.WorkloadID
	if superseded == fresh {
		t.Fatalf("relaunch reused the superseded identity %q", superseded)
	}
	if len(atRemove) != 1 {
		t.Fatalf("Remove calls = %d, want exactly one removal of the superseded identity", len(atRemove))
	}
	assertRetiring(t, atRemove[0], superseded)
	if len(removalsAtStart) < 2 || !removalRequestsInclude(removalsAtStart[1], superseded) {
		t.Fatalf("removals at the replacement start = %v, want the superseded identity %q already removed",
			removalsAtStart, superseded)
	}
	action := mustAction(t, started, "build")
	if action.Container.WorkloadID != fresh {
		t.Fatalf("persisted workload = %q, want fresh %q", action.Container.WorkloadID, fresh)
	}
	old, ok := h.fixture.Runtime.Workload(superseded)
	if !ok || !old.Removed || old.Started {
		t.Fatalf("superseded identity = %+v, want affirmatively removed and never started", old)
	}

	// Drive the fresh workload to a confirmed successful settlement and cleanup.
	h.confirmRuntimeStop(identityFor(h.executionID, action))
	h.fixture.Runtime.QueueObservation(fresh, kernel.ExitedObservation(0, kernel.ConfirmedStop("build exited")))
	completed := h.reconcileUntil("terminal success and cleanup complete", 12, func(candidate kernel.Execution) bool {
		return candidate.State == kernel.ExecutionSucceeded && candidate.Cleanup.State == kernel.CleanupComplete
	})
	if completed.Failure != nil {
		t.Fatalf("terminal success gained a failure: %+v", completed.Failure)
	}
	if !h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("cleanup did not release the workspace")
	}
	removed := h.runtime.removedWorkloads()
	if !containsWorkload(removed, superseded) || !containsWorkload(removed, fresh) {
		t.Fatalf("removed workloads = %v, want %q and %q", removed, superseded, fresh)
	}
	freshWorkload, ok := h.fixture.Runtime.Workload(fresh)
	if !ok || !freshWorkload.Removed {
		t.Fatalf("fresh workload after cleanup = %+v, want removed", freshWorkload)
	}
}

// TestCrashBeforePrepareTreatsNeverPreparedIdentityAsAbsent pins that a marker
// committed before a crash in Prepare leaves no leak: the identity was never
// prepared, so cleanup treats it as absent and releases the workspace.
func TestCrashBeforePrepareTreatsNeverPreparedIdentityAsAbsent(t *testing.T) {
	build := containerAction("act-build", "build", 0, time.Hour)
	h := newHarness(t, newExecution("exec-crash-before-prepare", "ws-crash-before-prepare", build))

	ctx, cancel := context.WithCancel(h.t.Context())
	h.runtime.beforePrepare = func(context.Context) { cancel() }
	if _, err := h.engine.Reconcile(ctx, h.executionID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Reconcile error = %v, want context.Canceled", err)
	}

	action := mustAction(t, h.load(), "build")
	identity := identityFor(h.executionID, action)
	if action.Container.Launch.State != kernel.LaunchIssued {
		t.Fatalf("launch state after the crash = %q, want issued", action.Container.Launch.State)
	}
	if _, ok := h.fixture.Runtime.Workload(identity.WorkloadID); ok {
		t.Fatalf("identity %q was prepared before the crash", identity.WorkloadID)
	}

	// Cancel the running execution so its terminal cleanup owns the identity.
	h.cancel(flow.CancelRequested)
	completed := h.reconcileUntil("cleanup complete", 10, func(candidate kernel.Execution) bool {
		return candidate.Cleanup.State == kernel.CleanupComplete
	})
	if completed.State != kernel.ExecutionCancelled {
		t.Fatalf("terminal state = %q, want cancelled", completed.State)
	}
	if !h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("never-prepared identity blocked workspace release")
	}
	if h.runtime.removeCount() == 0 {
		t.Fatal("cleanup never attempted removal of the never-prepared identity")
	}
	if completed.Actions[0].Stop.IsConfirmed() {
		t.Fatal("cleanup fabricated stop evidence for a never-prepared identity")
	}
}

// TestNeverStartedCleanupFailuresStayPending pins the negative cases: a failed
// removal or missing never-started proof keeps cleanup pending with
// attribution and never releases the workspace.
func TestNeverStartedCleanupFailuresStayPending(t *testing.T) {
	t.Run("remove failure", func(t *testing.T) {
		execution, identity := terminalNeverStartedExecution("exec-never-started-remove", "ws-never-started-remove")
		h := newHarness(t, execution)
		h.retainWorkspace()
		h.prepareOnly(identity, execution.Actions[0].Container.Definition)
		h.fixture.Runtime.FailNextRemove(errors.New("injected removal failure"))

		pending := h.reconcile()
		if pending.Cleanup.State != kernel.CleanupPending {
			t.Fatalf("cleanup state = %q, want pending", pending.Cleanup.State)
		}
		if pending.Cleanup.Attempts != 1 {
			t.Fatalf("cleanup attempts = %d, want 1", pending.Cleanup.Attempts)
		}
		if pending.Cleanup.LastError == "" {
			t.Fatal("failed removal lost its attribution")
		}
		if pending.Cleanup.WorkloadID != identity.WorkloadID {
			t.Fatalf("cleanup workload = %q, want %q", pending.Cleanup.WorkloadID, identity.WorkloadID)
		}
		if h.runtime.stopCount() != 0 {
			t.Fatalf("never-started cleanup requested %d stops, want none", h.runtime.stopCount())
		}
		if h.fixture.Workspace.IsReleased(h.workspaceID) {
			t.Fatal("failed removal released the workspace")
		}
		if pending.State != kernel.ExecutionFailed {
			t.Fatalf("terminal state = %q, want failed", pending.State)
		}
	})

	t.Run("missing never-started proof", func(t *testing.T) {
		execution, identity := terminalNeverStartedExecution("exec-never-started-unknown", "ws-never-started-unknown")
		h := newHarness(t, execution)
		h.retainWorkspace()
		h.prepareOnly(identity, execution.Actions[0].Container.Definition)
		h.fixture.Runtime.QueueObservation(identity.WorkloadID, kernel.UnknownObservation())

		pending := h.reconcile()
		if pending.Cleanup.State != kernel.CleanupPending {
			t.Fatalf("cleanup state = %q, want pending", pending.Cleanup.State)
		}
		if pending.Cleanup.WorkloadID != identity.WorkloadID {
			t.Fatalf("cleanup workload = %q, want %q", pending.Cleanup.WorkloadID, identity.WorkloadID)
		}
		if h.runtime.removeCount() != 0 {
			t.Fatalf("unknown observation removed resources: %d", h.runtime.removeCount())
		}
		if h.fixture.Workspace.IsReleased(h.workspaceID) {
			t.Fatal("unknown observation released the workspace")
		}
		action := mustAction(t, pending, "build")
		if action.Stop.IsConfirmed() {
			t.Fatal("unknown observation was promoted to confirmed stop")
		}
	})
}

// terminalNeverStartedExecution seeds a terminal failure whose container
// identity is LaunchIssued but has no confirmed stop and no terminal
// observation.
func terminalNeverStartedExecution(executionID, workspaceID string) (kernel.Execution, engine.EffectIdentity) {
	build := containerAction("act-build", "build", 0, time.Hour)
	build.State = kernel.ActionFailed
	build.Version = 3
	build.Container.WorkloadID = id.WorkloadID("wl-" + executionID)
	build.Container.WorkspaceID = id.WorkspaceID(workspaceID)
	build.Container.Launch = kernel.LaunchIntent{
		WorkloadID:     build.Container.WorkloadID,
		State:          kernel.LaunchIssued,
		IdempotencyKey: "idem-" + executionID,
	}
	execution := newExecution(executionID, workspaceID, build)
	execution.State = kernel.ExecutionFailed
	execution.CurrentAction = -1
	execution.Failure = &kernel.Failure{Kind: kernel.FailureRuntime, Reason: "workload_exited"}
	execution.Cleanup = kernel.Cleanup{
		State:       kernel.CleanupPending,
		WorkloadID:  build.Container.WorkloadID,
		WorkspaceID: id.WorkspaceID(workspaceID),
	}
	return execution, identityFor(execution.ID, build)
}

// ---------------------------------------------------------------------------
// Reconcile version-conflict re-read
// ---------------------------------------------------------------------------

// gatedState is an engine.State that can block one matching commit, letting a
// test commit a concurrent winner at the exact boundary between Reconcile's
// load and its commit. It records real version conflicts returned by the
// underlying store.
type gatedState struct {
	engine.State

	mu        sync.Mutex
	gate      func(flow.Commit) bool
	blocked   bool
	reached   chan struct{}
	release   chan struct{}
	once      sync.Once
	conflicts int
}

func newGatedState(state engine.State) *gatedState {
	return &gatedState{
		State:   state,
		reached: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (s *gatedState) Commit(ctx context.Context, commit flow.Commit) (kernel.Execution, error) {
	s.mu.Lock()
	block := s.gate != nil && s.gate(commit)
	already := s.blocked
	if block && !already {
		s.blocked = true
	}
	s.mu.Unlock()

	if block && !already {
		close(s.reached)
		select {
		case <-s.release:
		case <-ctx.Done():
			return kernel.Execution{}, ctx.Err()
		}
	}

	updated, err := s.State.Commit(ctx, commit)
	if class, ok := fault.ClassOf(err); ok && class == fault.VersionConflict {
		s.mu.Lock()
		s.conflicts++
		s.mu.Unlock()
	}
	return updated, err
}

func (s *gatedState) releaseGate() {
	s.once.Do(func() { close(s.release) })
}

func (s *gatedState) conflictCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conflicts
}

func waitGate(t *testing.T, gate *gatedState, ctx context.Context) {
	t.Helper()
	// Bound the wait so a regression that never reaches the gated commit fails
	// the test with a clear message instead of hanging until the package timeout.
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	select {
	case <-gate.reached:
	case <-bounded.Done():
		if errors.Is(bounded.Err(), context.DeadlineExceeded) {
			t.Fatal("commit gate was not reached within 5s")
		}
		t.Fatalf("commit gate was not reached: %v", bounded.Err())
	}
}

func waitPrepared(t *testing.T, prepared <-chan struct{}, ctx context.Context) {
	t.Helper()
	select {
	case <-prepared:
	case <-ctx.Done():
		t.Fatalf("Prepare was not reached: %v", ctx.Err())
	}
}

// TestReconcileVersionConflictRereadsAndPreservesWinner pins S02 §3: a
// concurrent winner between Reconcile's load and commit produces a real
// version_conflict, Reconcile re-reads, and the stale transition never
// overwrites the winner.
func TestReconcileVersionConflictRereadsAndPreservesWinner(t *testing.T) {
	seeded := runningContainerExecution("exec-reconcile-conflict", "ws-reconcile-conflict", testNow)
	identity := identityFor(seeded.ID, seeded.Actions[0])
	h := newHarness(t, seeded)
	h.retainWorkspace()
	h.prepareAndStart(identity, seeded.Actions[0].Container.Definition)

	gate := newGatedState(h.fixture.State)
	gate.gate = func(commit flow.Commit) bool {
		return commit.NextActionState == kernel.ActionFailed &&
			commit.Failure != nil && commit.Failure.Kind == kernel.FailureTimeout
	}
	h.engine = engine.New(gate, h.runtime, h.fixture.Workspace, h.fixture.Clock)

	done := make(chan error, 1)
	go func() {
		_, err := h.engine.Reconcile(h.t.Context(), h.executionID)
		done <- err
	}()
	waitGate(t, gate, h.t.Context())

	// A Cancel winner commits between Reconcile's load and its expiry commit.
	current, err := h.fixture.State.Load(h.t.Context(), h.executionID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	_, cancelCommit, err := flow.Reduce(current, flow.CancelFact{
		ExecutionID: h.executionID,
		Reason:      flow.CancelRequested,
		At:          testNow,
	})
	if err != nil || cancelCommit == nil {
		t.Fatalf("Reduce(CancelFact) = commit %v, error %v", cancelCommit, err)
	}
	if _, err := h.fixture.State.Commit(h.t.Context(), *cancelCommit); err != nil {
		t.Fatalf("cancel winner commit error = %v", err)
	}

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
		t.Fatalf("stale expiry overwrote the winner with a failure: %+v", final.Failure)
	}
	action := mustAction(t, final, "build")
	if action.State != kernel.ActionCancelled {
		t.Fatalf("action state = %q, want cancelled", action.State)
	}
	if action.Container.Observation.Kind == kernel.ObservationRunning {
		t.Fatal("stale reconcile overwrote the winner with a running observation")
	}
	if final.Cleanup.State != kernel.CleanupPending {
		t.Fatalf("winner cleanup state = %q, want pending", final.Cleanup.State)
	}
	if h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("unconfirmed stop released the workspace")
	}
}

// ---------------------------------------------------------------------------
// SubmitOutputs versus confirmed settlement
// ---------------------------------------------------------------------------

// TestSubmitOutputsVersusConfirmedSettlementSettlementWins pins that a
// settlement that wins the commit boundary refuses the stale staging commit and
// neither resurrects nor promotes outputs twice.
func TestSubmitOutputsVersusConfirmedSettlementSettlementWins(t *testing.T) {
	build := containerAction("act-build", "build", 0, time.Hour, "candidate")
	h := newHarness(t, newExecution("exec-output-settle-wins", "ws-output-settle-wins", build))
	running := h.reconcileUntil("build running", 8, func(candidate kernel.Execution) bool {
		action, ok := actionByName(candidate, "build")
		return ok && action.State == kernel.ActionRunning
	})
	buildAction := mustAction(t, running, "build")
	identity := identityFor(h.executionID, buildAction)
	h.confirmRuntimeStop(identity)
	h.fixture.Runtime.QueueObservation(buildAction.Container.WorkloadID, kernel.ExitedObservation(0, kernel.ConfirmedStop("build exited")))

	gate := newGatedState(h.fixture.State)
	gate.gate = func(commit flow.Commit) bool {
		return commit.Output.Action == flow.OutputStage
	}
	h.engine = engine.New(gate, h.runtime, h.fixture.Workspace, h.fixture.Clock)

	staged := make(chan error, 1)
	go func() {
		staged <- h.engine.SubmitOutputs(h.t.Context(), h.executionID, "act-build", kernel.Outputs{"candidate": "abc"})
	}()
	waitGate(t, gate, h.t.Context())

	// Settlement wins while the staging commit is blocked.
	settled := h.reconcileUntil("build succeeded", 8, func(candidate kernel.Execution) bool {
		action, ok := actionByName(candidate, "build")
		return ok && action.State == kernel.ActionSucceeded
	})
	gate.releaseGate()
	mustClass(t, <-staged, fault.VersionConflict)

	action := mustAction(t, settled, "build")
	if action.Outputs["candidate"] != "" {
		t.Fatalf("settlement resurrected a staged value: %+v", action.Outputs)
	}
	if action.StagedOutputsSet || action.StagedOutputs != nil {
		t.Fatalf("stale staging survived settlement: set=%v values=%+v", action.StagedOutputsSet, action.StagedOutputs)
	}
	final := h.load()
	if final.State != kernel.ExecutionSucceeded {
		t.Fatalf("terminal state = %q, want succeeded", final.State)
	}
}

// TestSubmitOutputsVersusConfirmedSettlementStagingWins pins that staging that
// wins the commit boundary is promoted exactly once by the re-read settlement.
func TestSubmitOutputsVersusConfirmedSettlementStagingWins(t *testing.T) {
	build := containerAction("act-build", "build", 0, time.Hour, "candidate")
	h := newHarness(t, newExecution("exec-output-stage-wins", "ws-output-stage-wins", build))
	running := h.reconcileUntil("build running", 8, func(candidate kernel.Execution) bool {
		action, ok := actionByName(candidate, "build")
		return ok && action.State == kernel.ActionRunning
	})
	buildAction := mustAction(t, running, "build")
	identity := identityFor(h.executionID, buildAction)
	h.confirmRuntimeStop(identity)
	// The first observation drives the blocked settlement; the second drives
	// the re-read settlement after the staging conflict.
	h.fixture.Runtime.QueueObservation(buildAction.Container.WorkloadID,
		kernel.ExitedObservation(0, kernel.ConfirmedStop("build exited")),
		kernel.ExitedObservation(0, kernel.ConfirmedStop("build exited")),
	)

	gate := newGatedState(h.fixture.State)
	gate.gate = func(commit flow.Commit) bool {
		return commit.NextActionState == kernel.ActionSucceeded
	}
	h.engine = engine.New(gate, h.runtime, h.fixture.Workspace, h.fixture.Clock)

	settled := make(chan error, 1)
	go func() {
		_, err := h.engine.Reconcile(h.t.Context(), h.executionID)
		settled <- err
	}()
	waitGate(t, gate, h.t.Context())

	// Staging wins while the settlement commit is blocked.
	if err := h.engine.SubmitOutputs(h.t.Context(), h.executionID, "act-build", kernel.Outputs{"candidate": "abc"}); err != nil {
		t.Fatalf("SubmitOutputs() error = %v", err)
	}
	gate.releaseGate()
	if err := <-settled; err != nil {
		t.Fatalf("Reconcile error = %v", err)
	}
	if gate.conflictCount() == 0 {
		t.Fatal("settlement did not observe the staging version conflict")
	}

	final := h.load()
	if final.State != kernel.ExecutionSucceeded {
		t.Fatalf("terminal state = %q, want succeeded", final.State)
	}
	action := mustAction(t, final, "build")
	if action.Outputs["candidate"] != "abc" {
		t.Fatalf("promoted outputs = %+v, want candidate=abc exactly once", action.Outputs)
	}
	if action.StagedOutputsSet || action.StagedOutputs != nil {
		t.Fatalf("staging was not cleared after promotion: set=%v values=%+v", action.StagedOutputsSet, action.StagedOutputs)
	}
	if len(action.Outputs) != 1 {
		t.Fatalf("promotion produced %d values, want exactly one: %+v", len(action.Outputs), action.Outputs)
	}
}
