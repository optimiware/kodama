package enginetest

// Persisting the Prepare-cancellation never-started witness.
//
// The performStart invocation that commits the issued launch marker knows
// whether it ever called Start. When a concurrent cancellation, expiry or
// workspace loss wins while Prepare is in flight, that invocation must persist
// affirmative NeverStarted evidence through the versioned commit before cleanup
// may remove anything. A reconstructed engine that only sees the persisted
// `cancelled + issued` pair still knows nothing and must stay uncertain: the
// proof is never inferred from the cancellation, the launch marker, Job absence
// or an empty pod list.
//
// Every runtime below reports absent/unconfirmed for the prepared identity, so
// the fake's own "prepared but never started" knowledge cannot supply the
// evidence. Only the engine's persisted proof can let cleanup finish.

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
	"github.com/optimiware/kodama/internal/kernel/flow"
)

// ---------------------------------------------------------------------------
// A runtime that cannot supply never-started proof
// ---------------------------------------------------------------------------

// prepareCancelRuntime wraps one engine.Runtime and makes the prepared identity
// look like a real adapter that has lost the object: Observe reports absence,
// Stop acknowledges without termination evidence and names no pods, while
// Remove still performs the idempotent cleanup. It also lets a test block or
// fail Prepare and counts every effect.
type prepareCancelRuntime struct {
	engine.Runtime

	mu     sync.Mutex
	target id.WorkloadID
	// observation is what Observe returns for the prepared identity. Absence is
	// the realistic shape of a prepared identity that never created a Job/Pod.
	observation kernel.Observation

	beforePrepare func(context.Context)
	afterPrepare  func(context.Context)
	prepareErr    error

	prepareCalls int
	startCalls   int
	observeCalls int
	stopCalls    int
	removeCalls  int
}

func newPrepareCancelRuntime(runtime engine.Runtime) *prepareCancelRuntime {
	return &prepareCancelRuntime{Runtime: runtime, observation: kernel.AbsentObservation()}
}

func (r *prepareCancelRuntime) Prepare(ctx context.Context, request engine.PrepareRequest) (engine.Preparation, error) {
	r.mu.Lock()
	r.prepareCalls++
	r.target = request.Identity.WorkloadID
	before := r.beforePrepare
	after := r.afterPrepare
	prepareErr := r.prepareErr
	r.mu.Unlock()
	if before != nil {
		before(ctx)
	}
	if prepareErr != nil {
		return engine.Preparation{}, prepareErr
	}
	preparation, err := r.Runtime.Prepare(ctx, request)
	if err == nil && after != nil {
		after(ctx)
	}
	return preparation, err
}

func (r *prepareCancelRuntime) Start(ctx context.Context, request engine.StartRequest) (engine.StartResult, error) {
	r.mu.Lock()
	r.startCalls++
	r.mu.Unlock()
	return r.Runtime.Start(ctx, request)
}

func (r *prepareCancelRuntime) Observe(ctx context.Context, request engine.ObserveRequest) (kernel.Observation, error) {
	r.mu.Lock()
	targeted := r.target != "" && request.Identity.WorkloadID == r.target
	observation := r.observation
	if targeted {
		r.observeCalls++
	}
	r.mu.Unlock()
	if targeted {
		return observation, nil
	}
	return r.Runtime.Observe(ctx, request)
}

func (r *prepareCancelRuntime) Stop(ctx context.Context, request engine.StopRequest) (kernel.StopEvidence, error) {
	r.mu.Lock()
	targeted := r.target != "" && request.Identity.WorkloadID == r.target
	if targeted {
		r.stopCalls++
	}
	r.mu.Unlock()
	if targeted {
		return kernel.PendingStop("stop unconfirmed: no workload pods observed"), nil
	}
	return r.Runtime.Stop(ctx, request)
}

func (r *prepareCancelRuntime) Remove(ctx context.Context, request engine.RemoveRequest) (engine.Removal, error) {
	r.mu.Lock()
	targeted := r.target != "" && request.Identity.WorkloadID == r.target
	if targeted {
		r.removeCalls++
	}
	r.mu.Unlock()
	return r.Runtime.Remove(ctx, request)
}

func (r *prepareCancelRuntime) setPrepareError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prepareErr = err
}

func (r *prepareCancelRuntime) setTarget(workloadID id.WorkloadID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.target = workloadID
}

func (r *prepareCancelRuntime) counts() (prepare, start, observe, stop, remove int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.prepareCalls, r.startCalls, r.observeCalls, r.stopCalls, r.removeCalls
}

// ---------------------------------------------------------------------------
// Commit probes
// ---------------------------------------------------------------------------

// isPrepareWitnessCommit reports whether a commit is the evidence-only witness:
// the same terminal execution/action states, no launch rewrite, no failure and
// a persisted NeverStarted observation. Retirement and preparation-failure
// commits never match it.
func isPrepareWitnessCommit(commit flow.Commit) bool {
	return commit.Launch == nil &&
		commit.Failure == nil &&
		commit.Observation != nil &&
		commit.Observation.Kind == kernel.ObservationNeverStarted &&
		commit.Observation.Proof != ""
}

// witnessCrashState applies a commit durably and then reports a storage-style
// failure, simulating a controller crash immediately after the witness commit
// and before cleanup.
type witnessCrashState struct {
	engine.State
	mu      sync.Mutex
	armed   bool
	crashed bool
	err     error
}

func (s *witnessCrashState) Commit(ctx context.Context, commit flow.Commit) (kernel.Execution, error) {
	updated, err := s.State.Commit(ctx, commit)
	if err != nil {
		return updated, err
	}
	s.mu.Lock()
	armed := s.armed && isPrepareWitnessCommit(commit)
	if armed {
		s.armed = false
		s.crashed = true
	}
	s.mu.Unlock()
	if armed {
		return updated, s.err
	}
	return updated, nil
}

func (s *witnessCrashState) didCrash() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.crashed
}

// witnessFailureState refuses the witness commit without applying it, so no
// proof (and no effect) exists when the pass ends.
type witnessFailureState struct {
	engine.State
	mu    sync.Mutex
	armed bool
	err   error
}

func (s *witnessFailureState) Commit(ctx context.Context, commit flow.Commit) (kernel.Execution, error) {
	s.mu.Lock()
	fail := s.armed && isPrepareWitnessCommit(commit)
	if fail {
		s.armed = false
	}
	s.mu.Unlock()
	if fail {
		return kernel.Execution{}, s.err
	}
	return s.State.Commit(ctx, commit)
}

// cancelledIssuedExecution reconstructs the shape a fresh controller sees after
// a cancellation won during Prepare without the witness: cancelled execution and
// action, issued launch marker, no observation and a pending cleanup record.
func cancelledIssuedExecution(executionID, workspaceID string) (kernel.Execution, engine.EffectIdentity) {
	build := containerAction("act-build", "build", 0, time.Hour)
	build.State = kernel.ActionCancelled
	build.Version = 3
	build.Container.WorkloadID = id.WorkloadID("wl-" + executionID)
	build.Container.WorkspaceID = id.WorkspaceID(workspaceID)
	build.Container.Launch = kernel.LaunchIntent{
		WorkloadID:     build.Container.WorkloadID,
		State:          kernel.LaunchIssued,
		IdempotencyKey: "idem-" + executionID,
	}
	execution := newExecution(executionID, workspaceID, build)
	execution.State = kernel.ExecutionCancelled
	execution.CurrentAction = -1
	execution.Cleanup = kernel.Cleanup{
		State:       kernel.CleanupPending,
		WorkloadID:  build.Container.WorkloadID,
		WorkspaceID: id.WorkspaceID(workspaceID),
	}
	return execution, identityFor(execution.ID, build)
}

// waitWitnessGate is waitGate's local-name-free twin for the shared gatedState.
func waitWitnessGate(t *testing.T, gate *gatedState, ctx context.Context) {
	t.Helper()
	waitGate(t, gate, ctx)
}

// ---------------------------------------------------------------------------
// The witness is persisted before removal
// ---------------------------------------------------------------------------

// TestCancelDuringPreparePersistsNeverStartedBeforeRemoval pins the ordering:
// the invocation that never called Start commits its NeverStarted witness, and
// cleanup removes the partial resources only after that proof is durable. The
// runtime cannot supply the proof itself.
func TestCancelDuringPreparePersistsNeverStartedBeforeRemoval(t *testing.T) {
	build := containerAction("act-build", "build", 0, time.Hour)
	h := newHarness(t, newExecution("exec-witness-order", "ws-witness-order", build))

	runtime := newPrepareCancelRuntime(h.runtime)
	prepared := make(chan struct{})
	resume := make(chan struct{})
	runtime.afterPrepare = func(ctx context.Context) {
		close(prepared)
		select {
		case <-resume:
		case <-ctx.Done():
		}
	}
	probe := newProbeRuntime(runtime)
	var atRemove []kernel.Execution
	probe.beforeRemove = func(engine.RemoveRequest) { atRemove = append(atRemove, h.load()) }
	h.engine = engine.New(h.fixture.State, probe, h.fixture.Workspace, h.fixture.Clock)

	ctx, cancel := context.WithTimeout(h.t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := h.engine.Reconcile(ctx, h.executionID)
		done <- err
	}()
	waitPrepared(t, prepared, ctx)
	h.cancel(flow.CancelRequested)
	close(resume)
	if err := <-done; err != nil {
		t.Fatalf("Reconcile error = %v", err)
	}

	action := mustAction(t, h.load(), "build")
	identity := identityFor(h.executionID, action)
	if len(atRemove) != 1 {
		t.Fatalf("Remove calls = %d, want exactly one for the never-started identity", len(atRemove))
	}
	assertWitnessAtRemove(t, atRemove[0], identity)

	completed := h.load()
	if completed.State != kernel.ExecutionCancelled {
		t.Fatalf("terminal state = %q, want cancelled", completed.State)
	}
	if completed.Failure != nil {
		t.Fatalf("cancellation gained a failure: %+v", completed.Failure)
	}
	if completed.Cleanup.State != kernel.CleanupComplete {
		t.Fatalf("cleanup state = %q, want complete", completed.Cleanup.State)
	}
	if !h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("cleanup did not release the workspace")
	}
	if mustAction(t, completed, "build").Stop.IsConfirmed() {
		t.Fatal("the never-started witness fabricated confirmed stop evidence")
	}
	if _, starts, observes, stops, removes := runtime.counts(); starts != 0 || observes != 0 || stops != 0 || removes != 1 {
		t.Fatalf("effects = start:%d observe:%d stop:%d remove:%d, want 0/0/0/1", starts, observes, stops, removes)
	}
}

func assertWitnessAtRemove(t *testing.T, persisted kernel.Execution, identity engine.EffectIdentity) {
	t.Helper()
	action, ok := actionByName(persisted, "build")
	if !ok || action.Container == nil {
		t.Fatalf("persisted execution at Remove has no build container: %s", actionSummary(persisted))
	}
	if action.Container.WorkloadID != identity.WorkloadID {
		t.Fatalf("persisted identity at Remove = %q, want %q", action.Container.WorkloadID, identity.WorkloadID)
	}
	if !action.Container.Observation.ProofOfNonexecution() {
		t.Fatalf("Remove ran before the never-started witness was durable: observation=%+v",
			action.Container.Observation)
	}
}

// ---------------------------------------------------------------------------
// Prepare returning an error after cancellation already won
// ---------------------------------------------------------------------------

// TestCancelWinningFailedPreparePreservesCancellationAndProof pins the error
// path: the cancellation that won while Prepare was in flight is neither
// overwritten by a preparation failure nor left without the local witness.
func TestCancelWinningFailedPreparePreservesCancellationAndProof(t *testing.T) {
	build := containerAction("act-build", "build", 0, time.Hour)
	h := newHarness(t, newExecution("exec-witness-prep-error", "ws-witness-prep-error", build))

	runtime := newPrepareCancelRuntime(h.runtime)
	runtime.setPrepareError(errors.New("credential lifetime is shorter than the minimum"))
	runtime.beforePrepare = func(context.Context) { h.cancel(flow.CancelRequested) }
	h.engine = engine.New(h.fixture.State, runtime, h.fixture.Workspace, h.fixture.Clock)

	completed, err := h.engine.Reconcile(h.t.Context(), h.executionID)
	if err != nil {
		t.Fatalf("Reconcile error = %v", err)
	}
	if completed.State != kernel.ExecutionCancelled {
		t.Fatalf("terminal state = %q, want the cancellation winner", completed.State)
	}
	if completed.Failure != nil {
		t.Fatalf("preparation failure overwrote the cancellation: %+v", completed.Failure)
	}
	action := mustAction(t, completed, "build")
	if action.State != kernel.ActionCancelled {
		t.Fatalf("action state = %q, want cancelled", action.State)
	}
	if !action.Container.Observation.ProofOfNonexecution() {
		t.Fatalf("cancellation winner has no persisted never-started evidence: observation=%+v",
			action.Container.Observation)
	}
	if action.Container.Launch.State != kernel.LaunchIssued {
		t.Fatalf("launch state = %q, want the issued attempt preserved", action.Container.Launch.State)
	}
	if completed.Cleanup.State != kernel.CleanupComplete {
		t.Fatalf("cleanup state = %q, want complete", completed.Cleanup.State)
	}
	if completed.Cleanup.WorkloadID != action.Container.WorkloadID {
		t.Fatalf("cleanup workload = %q, want %q", completed.Cleanup.WorkloadID, action.Container.WorkloadID)
	}
	if !h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("cleanup did not release the workspace")
	}
	if mustAction(t, completed, "build").Stop.IsConfirmed() {
		t.Fatal("the never-started witness fabricated confirmed stop evidence")
	}
	if _, starts, observes, stops, removes := runtime.counts(); starts != 0 || observes != 0 || stops != 0 || removes != 1 {
		t.Fatalf("effects = start:%d observe:%d stop:%d remove:%d, want 0/0/0/1", starts, observes, stops, removes)
	}
}

// ---------------------------------------------------------------------------
// Restart
// ---------------------------------------------------------------------------

// TestPrepareCancellationProofSurvivesEngineRestart proves the witness is
// durable, not controller memory: a crash after the commit and before cleanup
// leaves a fresh engine able to finish from the persisted proof alone.
func TestPrepareCancellationProofSurvivesEngineRestart(t *testing.T) {
	build := containerAction("act-build", "build", 0, time.Hour)
	h := newHarness(t, newExecution("exec-witness-restart", "ws-witness-restart", build))

	runtime := newPrepareCancelRuntime(h.runtime)
	prepared := make(chan struct{})
	resume := make(chan struct{})
	runtime.afterPrepare = func(ctx context.Context) {
		close(prepared)
		select {
		case <-resume:
		case <-ctx.Done():
		}
	}
	crashing := &witnessCrashState{State: h.fixture.State, armed: true, err: errors.New("injected crash after the witness commit")}
	h.engine = engine.New(crashing, runtime, h.fixture.Workspace, h.fixture.Clock)

	ctx, cancel := context.WithTimeout(h.t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := h.engine.Reconcile(ctx, h.executionID)
		done <- err
	}()
	waitPrepared(t, prepared, ctx)
	h.cancel(flow.CancelRequested)
	close(resume)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "injected crash after the witness commit") {
		t.Fatalf("Reconcile error = %v, want the injected post-commit crash", err)
	}
	if !crashing.didCrash() {
		t.Fatal("the witness commit was never exercised")
	}

	persisted := h.load()
	action := mustAction(t, persisted, "build")
	if !action.Container.Observation.ProofOfNonexecution() {
		t.Fatalf("witness did not survive the crash: observation=%+v", action.Container.Observation)
	}
	if persisted.Cleanup.State != kernel.CleanupPending {
		t.Fatalf("cleanup state after the crash = %q, want pending", persisted.Cleanup.State)
	}
	if h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("the crashed pass released the workspace")
	}

	// A fresh controller instance sees only persisted state.
	h.engine = engine.New(h.fixture.Restart(), runtime, h.fixture.Workspace, h.fixture.Clock)
	completed, err := h.engine.Reconcile(h.t.Context(), h.executionID)
	if err != nil {
		t.Fatalf("restart Reconcile error = %v", err)
	}
	if completed.State != kernel.ExecutionCancelled || completed.Failure != nil {
		t.Fatalf("restart terminal outcome = %q/%+v, want the preserved cancellation", completed.State, completed.Failure)
	}
	if completed.Cleanup.State != kernel.CleanupComplete {
		t.Fatalf("restart cleanup state = %q, want complete", completed.Cleanup.State)
	}
	if completed.Cleanup.WorkloadID != action.Container.WorkloadID {
		t.Fatalf("restart cleanup workload = %q, want %q", completed.Cleanup.WorkloadID, action.Container.WorkloadID)
	}
	if !h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("restart did not release the workspace")
	}
	if mustAction(t, completed, "build").Stop.IsConfirmed() {
		t.Fatal("the restarted cleanup fabricated confirmed stop evidence")
	}
	if _, starts, observes, stops, removes := runtime.counts(); starts != 0 || observes != 0 || stops != 0 || removes != 1 {
		t.Fatalf("restart effects = start:%d observe:%d stop:%d remove:%d, want 0/0/0/1", starts, observes, stops, removes)
	}
}

// ---------------------------------------------------------------------------
// A competing commit
// ---------------------------------------------------------------------------

// TestPrepareCancellationProofConflictRetainsLocalWitness pins the bounded
// reload/revalidation: a cleanup commit that wins between the witness re-read
// and its commit is a version conflict, and the attempt's local knowledge is
// retried onto the fresh revision instead of being dropped.
func TestPrepareCancellationProofConflictRetainsLocalWitness(t *testing.T) {
	build := containerAction("act-build", "build", 0, time.Hour)
	h := newHarness(t, newExecution("exec-witness-conflict", "ws-witness-conflict", build))
	runtime := newPrepareCancelRuntime(h.runtime)

	gate := newGatedState(h.fixture.State)
	gate.gate = isPrepareWitnessCommit
	h.engine = engine.New(gate, runtime, h.fixture.Workspace, h.fixture.Clock)

	prepared := make(chan struct{})
	resume := make(chan struct{})
	runtime.afterPrepare = func(ctx context.Context) {
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
	h.cancel(flow.CancelRequested)
	close(resume)
	waitWitnessGate(t, gate, ctx)

	// A competing cleanup progress commit wins the boundary while the witness
	// commit is blocked.
	current, err := h.fixture.State.Load(ctx, h.executionID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	action := mustAction(t, current, "build")
	if _, err := h.fixture.State.Commit(ctx, flow.Commit{
		ExecutionID:              h.executionID,
		ExpectedExecutionVersion: current.Version,
		NextExecutionState:       current.State,
		ActionID:                 action.ID,
		ExpectedActionVersion:    action.Version,
		NextActionState:          action.State,
		Output:                   flow.OutputChange{Action: flow.OutputNone},
		CleanupUpdate: &flow.CleanupUpdate{
			State:       kernel.CleanupPending,
			WorkloadID:  action.Container.WorkloadID,
			WorkspaceID: h.workspaceID,
			Attempts:    1,
			LastError:   "competing cleanup progress",
		},
		At: testNow.Add(time.Minute),
	}); err != nil {
		t.Fatalf("competing cleanup commit error = %v", err)
	}
	gate.releaseGate()
	if err := <-done; err != nil {
		t.Fatalf("Reconcile error = %v", err)
	}
	if gate.conflictCount() == 0 {
		t.Fatal("the witness commit never observed the competing version conflict")
	}

	completed := h.load()
	if completed.State != kernel.ExecutionCancelled || completed.Failure != nil {
		t.Fatalf("terminal outcome = %q/%+v, want the preserved cancellation", completed.State, completed.Failure)
	}
	action = mustAction(t, completed, "build")
	if !action.Container.Observation.ProofOfNonexecution() {
		t.Fatalf("the conflict lost the witness: observation=%+v", action.Container.Observation)
	}
	if completed.Cleanup.State != kernel.CleanupComplete {
		t.Fatalf("cleanup state = %q, want complete", completed.Cleanup.State)
	}
	if completed.Cleanup.Attempts != 1 {
		t.Fatalf("completed cleanup attempts = %d, want the competing commit's 1 preserved",
			completed.Cleanup.Attempts)
	}
	if !h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("cleanup did not release the workspace")
	}
	if action.Stop.IsConfirmed() {
		t.Fatal("the conflict retry fabricated confirmed stop evidence")
	}
	if _, starts, observes, stops, removes := runtime.counts(); starts != 0 || observes != 0 || stops != 0 || removes != 1 {
		t.Fatalf("effects = start:%d observe:%d stop:%d remove:%d, want 0/0/0/1", starts, observes, stops, removes)
	}
}

// ---------------------------------------------------------------------------
// A failed witness write
// ---------------------------------------------------------------------------

// TestPrepareCancellationProofWriteFailurePreventsRemoval pins the safety
// boundary: when the witness cannot be persisted, nothing is removed or
// released, and a later pass with only `cancelled + issued` stays visibly
// uncertain instead of inventing the proof.
func TestPrepareCancellationProofWriteFailurePreventsRemoval(t *testing.T) {
	build := containerAction("act-build", "build", 0, time.Hour)
	h := newHarness(t, newExecution("exec-witness-write-fail", "ws-witness-write-fail", build))
	runtime := newPrepareCancelRuntime(h.runtime)

	prepared := make(chan struct{})
	resume := make(chan struct{})
	runtime.afterPrepare = func(ctx context.Context) {
		close(prepared)
		select {
		case <-resume:
		case <-ctx.Done():
		}
	}
	failing := &witnessFailureState{
		State: h.fixture.State,
		armed: true,
		err:   errors.New("injected witness write failure"),
	}
	h.engine = engine.New(failing, runtime, h.fixture.Workspace, h.fixture.Clock)

	ctx, cancel := context.WithTimeout(h.t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := h.engine.Reconcile(ctx, h.executionID)
		done <- err
	}()
	waitPrepared(t, prepared, ctx)
	h.cancel(flow.CancelRequested)
	close(resume)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "injected witness write failure") {
		t.Fatalf("Reconcile error = %v, want the injected witness write failure", err)
	}

	pending := h.load()
	action := mustAction(t, pending, "build")
	if action.Container.Observation.ProofOfNonexecution() {
		t.Fatalf("a failed witness write still persisted proof: %+v", action.Container.Observation)
	}
	if pending.Cleanup.State != kernel.CleanupPending {
		t.Fatalf("cleanup state = %q, want pending", pending.Cleanup.State)
	}
	if h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("a failed witness write released the workspace")
	}
	if _, starts, observes, stops, removes := runtime.counts(); starts != 0 || observes != 0 || stops != 0 || removes != 0 {
		t.Fatalf("failed pass effects = start:%d observe:%d stop:%d remove:%d, want none",
			starts, observes, stops, removes)
	}

	// A later pass has only the persisted `cancelled + issued` record. The
	// runtime reports absence and an unconfirmed stop, so cleanup must stay
	// pending: absence and cancellation are not nonexecution proof.
	h.engine = engine.New(h.fixture.State, runtime, h.fixture.Workspace, h.fixture.Clock)
	uncertain, err := h.engine.Reconcile(h.t.Context(), h.executionID)
	if err != nil {
		t.Fatalf("later Reconcile error = %v", err)
	}
	if uncertain.Cleanup.State != kernel.CleanupPending {
		t.Fatalf("later cleanup state = %q, want pending", uncertain.Cleanup.State)
	}
	if h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("the later pass invented proof and released the workspace")
	}
	if _, _, observes, _, removes := runtime.counts(); observes == 0 || removes != 0 {
		t.Fatalf("later pass observe/remove = %d/%d, want observation without removal", observes, removes)
	}
	if mustAction(t, uncertain, "build").Stop.IsConfirmed() {
		t.Fatal("the later pass fabricated confirmed stop evidence")
	}
}

// ---------------------------------------------------------------------------
// A retired predecessor and an issued successor identity
// ---------------------------------------------------------------------------

// commitSuccessorDuringPrepare drives, through the durable state port, the facts
// a second controller produces while the first invocation is still inside
// Prepare for K1: (a) an affirmative NeverStarted observation retires K1, (b) the
// confirmed removal commits the deterministic replacement identity K2, (c) K2's
// launch marker becomes issued in the same commit shape the engine uses before
// it requests Start, and (d) a cancellation wins. It returns K2's identity.
func commitSuccessorDuringPrepare(t *testing.T, h *harness) engine.EffectIdentity {
	t.Helper()
	at := testNow.Add(time.Minute)

	// (a) K1 was observed never started; the observation retires that identity.
	current := h.load()
	action := mustAction(t, current, "build")
	predecessor := identityFor(current.ID, action)
	_, commit, err := flow.Reduce(current, flow.ObserveFact{
		ExecutionID: current.ID,
		ActionID:    action.ID,
		Observation: kernel.NeverStarted("second controller proved the prepared identity never ran"),
		At:          at,
	})
	if err != nil || commit == nil {
		t.Fatalf("retirement reduction = commit %v, error %v", commit, err)
	}
	if _, err := h.fixture.State.Commit(t.Context(), *commit); err != nil {
		t.Fatalf("retirement commit error = %v", err)
	}

	// (b) The confirmed removal commits the replacement identity K2.
	current = h.load()
	_, commit, err = flow.Reduce(current, flow.RetiredFact{
		ExecutionID: current.ID,
		ActionID:    action.ID,
		WorkloadID:  predecessor.WorkloadID,
		At:          at.Add(time.Minute),
	})
	if err != nil || commit == nil {
		t.Fatalf("retired-fact reduction = commit %v, error %v", commit, err)
	}
	replaced, err := h.fixture.State.Commit(t.Context(), *commit)
	if err != nil {
		t.Fatalf("retired-fact commit error = %v", err)
	}
	successor := identityFor(replaced.ID, mustAction(t, replaced, "build"))
	if successor.WorkloadID == predecessor.WorkloadID || successor.IdempotencyKey == predecessor.IdempotencyKey {
		t.Fatalf("replacement reused the retired identity: %+v", successor)
	}

	// (c) K2 becomes the issued attempt, exactly as performStart's marker commit
	// does before any Prepare call for it.
	current = h.load()
	action = mustAction(t, current, "build")
	launch := kernel.LaunchIntent{
		WorkloadID:     successor.WorkloadID,
		State:          kernel.LaunchIssued,
		IdempotencyKey: successor.IdempotencyKey,
	}
	if _, err := h.fixture.State.Commit(t.Context(), flow.Commit{
		ExecutionID:              current.ID,
		ExpectedExecutionVersion: current.Version,
		NextExecutionState:       current.State,
		ActionID:                 action.ID,
		ExpectedActionVersion:    action.Version,
		NextActionState:          action.State,
		Output:                   flow.OutputChange{Action: flow.OutputNone},
		Launch:                   &launch,
		At:                       at.Add(2 * time.Minute),
	}); err != nil {
		t.Fatalf("successor launch commit error = %v", err)
	}

	// The successor attempt may have called Start; the fake, which the engine
	// never consults for persisted state, records it as started.
	h.prepareAndStart(successor, action.Container.Definition)

	// (d) The cancellation wins the boundary before the first invocation returns.
	h.cancel(flow.CancelRequested)
	return successor
}

// TestPrepareWitnessNeverAttachesToSuccessorIdentity pins the identity barrier
// between a Start-less Prepare invocation and the persisted action. While the
// first controller is inside Prepare for K1, a second controller observes K1 as
// never started, retires it and commits the replacement identity K2, which then
// becomes issued. A cancellation that wins afterwards settles the execution as
// cancelled with an issued launch marker, which is also the shape
// prepareWitnessApplies accepts. Only the workload and idempotency identity
// separate K1 from K2, so without that comparison the stale invocation writes
// NeverStarted onto K2's action; cleanup then skips Observe and Stop for an
// attempt that may have started and removes it without confirmed stop. This
// sequence must leave K2 without any nonexecution proof and with cleanup pending.
func TestPrepareWitnessNeverAttachesToSuccessorIdentity(t *testing.T) {
	build := containerAction("act-build", "build", 0, time.Hour)
	h := newHarness(t, newExecution("exec-witness-successor", "ws-witness-successor", build))

	runtime := newPrepareCancelRuntime(h.runtime)
	var successor engine.EffectIdentity
	runtime.afterPrepare = func(context.Context) { successor = commitSuccessorDuringPrepare(t, h) }
	probe := newProbeRuntime(runtime)
	h.engine = engine.New(h.fixture.State, probe, h.fixture.Workspace, h.fixture.Clock)

	completed, err := h.engine.Reconcile(h.t.Context(), h.executionID)
	if err != nil {
		t.Fatalf("Reconcile error = %v", err)
	}
	if completed.State != kernel.ExecutionCancelled || completed.Failure != nil {
		t.Fatalf("terminal outcome = %q/%+v, want the preserved cancellation", completed.State, completed.Failure)
	}
	action := mustAction(t, completed, "build")
	if action.State != kernel.ActionCancelled {
		t.Fatalf("action state = %q, want cancelled", action.State)
	}
	if action.Container == nil {
		t.Fatalf("action %q has no container value: %s", action.Name, actionSummary(completed))
	}
	if action.Container.WorkloadID != successor.WorkloadID ||
		action.Container.Launch.IdempotencyKey != successor.IdempotencyKey {
		t.Fatalf("persisted identity = %q/%q, want successor %q/%q",
			action.Container.WorkloadID, action.Container.Launch.IdempotencyKey,
			successor.WorkloadID, successor.IdempotencyKey)
	}
	if action.Container.Launch.State != kernel.LaunchIssued {
		t.Fatalf("launch state = %q, want the successor's issued attempt", action.Container.Launch.State)
	}
	// Without an identity comparison the predecessor's witness attaches here.
	if action.Container.Observation.ProofOfNonexecution() {
		t.Fatalf("successor identity received the predecessor's witness: observation=%+v",
			action.Container.Observation)
	}
	if action.Stop.IsConfirmed() {
		t.Fatal("a proof-less successor fabricated confirmed stop evidence")
	}
	if completed.Cleanup.State != kernel.CleanupPending {
		t.Fatalf("cleanup state = %q, want pending until the successor stop is confirmed",
			completed.Cleanup.State)
	}
	if h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("cleanup released the workspace without confirming the successor stopped")
	}
	// Cleanup must Observe and Stop the possibly-started successor, and must not
	// remove it without that confirmation.
	if observes, stops := h.runtime.observeCount(), h.runtime.stopCount(); observes == 0 || stops == 0 {
		t.Fatalf("successor observe/stop = %d/%d, want the stop barrier to run", observes, stops)
	}
	if removals := h.runtime.removedWorkloads(); len(removals) != 0 {
		t.Fatalf("Remove calls = %v, want none without confirmed stop", removals)
	}
	if count := probe.count("remove", successor.WorkloadID); count != 0 {
		t.Fatalf("Remove calls for the successor = %d, want none without confirmed stop", count)
	}
	if h.runtime.startCount() != 0 {
		t.Fatalf("the stale Prepare invocation started work after the cancellation: %v",
			h.runtime.startRequests())
	}
}

// prepareWitnessConflictState refuses every witness commit with a version
// conflict until it is disarmed, applying every other transition normally. It
// records how many witness attempts the bounded retry actually made.
type prepareWitnessConflictState struct {
	engine.State

	mu      sync.Mutex
	armed   bool
	refused int
}

func (s *prepareWitnessConflictState) Commit(ctx context.Context, commit flow.Commit) (kernel.Execution, error) {
	s.mu.Lock()
	refuse := s.armed && isPrepareWitnessCommit(commit)
	if refuse {
		s.refused++
	}
	s.mu.Unlock()
	if refuse {
		return kernel.Execution{}, fault.New(fault.VersionConflict, "injected witness version conflict")
	}
	return s.State.Commit(ctx, commit)
}

func (s *prepareWitnessConflictState) arm() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armed = true
}

func (s *prepareWitnessConflictState) refusals() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refused
}

// TestPrepareWitnessConflictExhaustionStaysUncertain pins the bounded retry:
// four consecutive version conflicts spend the witness budget, and
// the pass then falls back to the persisted terminal record instead of
// inventing evidence. Cleanup observes and requests a stop for the issued
// identity, but never removes it or releases the workspace without confirmed
// stop, and a later pass stays equally uncertain.
func TestPrepareWitnessConflictExhaustionStaysUncertain(t *testing.T) {
	build := containerAction("act-build", "build", 0, time.Hour)
	h := newHarness(t, newExecution("exec-witness-exhaust", "ws-witness-exhaust", build))

	runtime := newPrepareCancelRuntime(h.runtime)
	conflicting := &prepareWitnessConflictState{State: h.fixture.State}
	runtime.afterPrepare = func(context.Context) {
		h.cancel(flow.CancelRequested)
		conflicting.arm()
	}
	h.engine = engine.New(conflicting, runtime, h.fixture.Workspace, h.fixture.Clock)

	completed, err := h.engine.Reconcile(h.t.Context(), h.executionID)
	if err != nil {
		t.Fatalf("Reconcile error = %v", err)
	}
	if refusals := conflicting.refusals(); refusals != 4 {
		t.Fatalf("witness attempts = %d, want the bounded budget of 4", refusals)
	}
	if completed.State != kernel.ExecutionCancelled {
		t.Fatalf("terminal state = %q, want cancelled", completed.State)
	}
	action := mustAction(t, completed, "build")
	if action.Container == nil || action.Container.Observation.ProofOfNonexecution() {
		t.Fatalf("an exhausted witness budget persisted proof: observation=%+v", action.Container)
	}
	if action.Container.Launch.State != kernel.LaunchIssued {
		t.Fatalf("launch state = %q, want the untouched issued attempt", action.Container.Launch.State)
	}
	if completed.Cleanup.State != kernel.CleanupPending {
		t.Fatalf("cleanup state = %q, want pending after the exhausted witness budget", completed.Cleanup.State)
	}
	if h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("an exhausted witness budget released the workspace")
	}
	if _, _, observes, stops, removes := runtime.counts(); observes == 0 || stops == 0 || removes != 0 {
		t.Fatalf("observe/stop/remove = %d/%d/%d, want a stop barrier without removal",
			observes, stops, removes)
	}

	// A later pass over the same persisted facts stays uncertain.
	h.engine = engine.New(h.fixture.State, runtime, h.fixture.Workspace, h.fixture.Clock)
	uncertain := h.reconcile()
	if uncertain.Cleanup.State != kernel.CleanupPending {
		t.Fatalf("later cleanup state = %q, want pending", uncertain.Cleanup.State)
	}
	if h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("the later pass invented proof and released the workspace")
	}
	if _, _, _, _, removes := runtime.counts(); removes != 0 {
		t.Fatalf("the later pass removed an unconfirmed identity (%d removals)", removes)
	}
	if mustAction(t, uncertain, "build").Stop.IsConfirmed() {
		t.Fatal("the later pass fabricated confirmed stop evidence")
	}
}

// ---------------------------------------------------------------------------
// Negative boundaries
// ---------------------------------------------------------------------------

// TestCancelledIssuedWithoutProofRemainsUncertain pins that a reconstructed
// engine never derives never-started evidence from `cancelled + issued`, Job
// absence or an empty pod list.
func TestCancelledIssuedWithoutProofRemainsUncertain(t *testing.T) {
	execution, identity := cancelledIssuedExecution("exec-cancelled-no-proof", "ws-cancelled-no-proof")
	h := newHarness(t, execution)
	h.retainWorkspace()
	runtime := newPrepareCancelRuntime(h.runtime)
	runtime.setTarget(identity.WorkloadID)
	h.engine = engine.New(h.fixture.State, runtime, h.fixture.Workspace, h.fixture.Clock)

	for pass := range 2 {
		pending := h.reconcile()
		if pending.State != kernel.ExecutionCancelled {
			t.Fatalf("pass %d terminal state = %q, want cancelled", pass, pending.State)
		}
		if pending.Cleanup.State != kernel.CleanupPending {
			t.Fatalf("pass %d cleanup state = %q, want pending without proof", pass, pending.Cleanup.State)
		}
		if h.fixture.Workspace.IsReleased(h.workspaceID) {
			t.Fatalf("pass %d released the workspace without proof", pass)
		}
		action := mustAction(t, pending, "build")
		if action.Container.Observation.ProofOfNonexecution() {
			t.Fatalf("pass %d inferred never-started proof from cancelled+issued: %+v",
				pass, action.Container.Observation)
		}
		if action.Stop.IsConfirmed() {
			t.Fatalf("pass %d fabricated confirmed stop", pass)
		}
	}
	if _, _, observes, _, removes := runtime.counts(); observes != 2 || removes != 0 {
		t.Fatalf("observe/remove = %d/%d, want one observation per pass and no removal", observes, removes)
	}

	// A fresh controller reading the same record still stays uncertain.
	h.engine = engine.New(h.fixture.Restart(), runtime, h.fixture.Workspace, h.fixture.Clock)
	pending := h.reconcile()
	if pending.Cleanup.State != kernel.CleanupPending || h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatalf("fresh controller cleanup = %+v released=%v, want pending and retained",
			pending.Cleanup, h.fixture.Workspace.IsReleased(h.workspaceID))
	}
}

// TestCancellationAfterStartDoesNotGainNeverStartedProof pins the opposite
// ordering: once Start was invoked, cancellation never acquires a never-started
// witness and cleanup still waits for affirmative stop evidence.
func TestCancellationAfterStartDoesNotGainNeverStartedProof(t *testing.T) {
	build := containerAction("act-build", "build", 0, time.Hour)
	h := newHarness(t, newExecution("exec-cancel-after-start", "ws-cancel-after-start", build))

	started := h.reconcileUntil("started", 8, func(candidate kernel.Execution) bool {
		return h.fixture.Runtime.StartCount() == 1
	})
	action := mustAction(t, started, "build")
	identity := identityFor(h.executionID, action)

	h.cancel(flow.CancelRequested)
	pending := h.reconcile()
	if pending.State != kernel.ExecutionCancelled {
		t.Fatalf("terminal state = %q, want cancelled", pending.State)
	}
	action = mustAction(t, pending, "build")
	if action.Container.Observation.ProofOfNonexecution() {
		t.Fatalf("cancellation after Start gained never-started proof: %+v", action.Container.Observation)
	}
	if pending.Cleanup.State != kernel.CleanupPending {
		t.Fatalf("cleanup state = %q, want pending until stop is confirmed", pending.Cleanup.State)
	}
	if h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("unconfirmed stop released the workspace")
	}
	if observes, stops, removes := h.runtime.observeCount(), h.runtime.stopCount(), h.runtime.removeCount(); observes == 0 || stops == 0 || removes != 0 {
		t.Fatalf("observe/stop/remove = %d/%d/%d, want observation, a stop request and no removal",
			observes, stops, removes)
	}
	if action.Stop.IsConfirmed() {
		t.Fatal("cancellation after Start fabricated confirmed stop")
	}

	h.fixture.Runtime.ConfirmNextStop("test: termination observed")
	completed := h.reconcileUntil("cleanup complete after confirmation", 6, func(candidate kernel.Execution) bool {
		return candidate.Cleanup.State == kernel.CleanupComplete
	})
	if !mustAction(t, completed, "build").Stop.IsConfirmed() {
		t.Fatal("cleanup did not record the confirmed stop")
	}
	if !h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("confirmed stop did not release the workspace")
	}
	workload, ok := h.fixture.Runtime.Workload(identity.WorkloadID)
	if !ok || !workload.Removed {
		t.Fatalf("workload %q was not removed: %+v", identity.WorkloadID, workload)
	}
}
