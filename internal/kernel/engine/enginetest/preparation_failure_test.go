package enginetest

// Preparation-failure cleanup regressions. A preparation failure
// happens before Start is issued, so the persisted failure is affirmative
// pre-Start evidence for the exact failed identity. Cleanup must remove that
// identity's partial resources and release the retained workspace without
// observing an absent Kubernetes object or waiting for an unconfirmed Stop;
// already-persisted records derive the same evidence from their failure
// provenance. Genuinely ambiguous launches keep their uncertainty and earlier
// cleanup obligations are never dropped.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/flow"
)

// failedPreparationRuntime models the production shape of a preparation that
// failed after materializing partial resources: the runtime has no Job or Pod
// for the identity, so Observe reports absence, Stop acknowledges without
// termination evidence and names zero pods, and Remove deletes the partial
// resources. The target identity is learned from Prepare, or set directly for a
// persisted record reconstructed by a fresh controller.
type failedPreparationRuntime struct {
	engine.Runtime

	prepareErr error

	mu           sync.Mutex
	target       id.WorkloadID
	prepareCalls int
	startCalls   int
	observeCalls int
	stopCalls    int
	removeCalls  int
	removeErr    error
	removed      map[id.WorkloadID]bool
}

func newFailedPreparationRuntime(runtime engine.Runtime, prepareErr error) *failedPreparationRuntime {
	return &failedPreparationRuntime{
		Runtime:    runtime,
		prepareErr: prepareErr,
		removed:    make(map[id.WorkloadID]bool),
	}
}

func (r *failedPreparationRuntime) setTarget(workloadID id.WorkloadID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.target = workloadID
}

func (r *failedPreparationRuntime) setRemoveError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removeErr = err
}

func (r *failedPreparationRuntime) Prepare(ctx context.Context, request engine.PrepareRequest) (engine.Preparation, error) {
	r.mu.Lock()
	r.prepareCalls++
	r.target = request.Identity.WorkloadID
	prepareErr := r.prepareErr
	r.mu.Unlock()
	if prepareErr != nil {
		return engine.Preparation{}, prepareErr
	}
	return r.Runtime.Prepare(ctx, request)
}

func (r *failedPreparationRuntime) Start(ctx context.Context, request engine.StartRequest) (engine.StartResult, error) {
	r.mu.Lock()
	r.startCalls++
	r.mu.Unlock()
	return r.Runtime.Start(ctx, request)
}

func (r *failedPreparationRuntime) Observe(ctx context.Context, request engine.ObserveRequest) (kernel.Observation, error) {
	r.mu.Lock()
	targeted := request.Identity.WorkloadID == r.target && r.target != ""
	if targeted {
		r.observeCalls++
	}
	r.mu.Unlock()
	if targeted {
		return kernel.AbsentObservation(), nil
	}
	return r.Runtime.Observe(ctx, request)
}

func (r *failedPreparationRuntime) Stop(ctx context.Context, request engine.StopRequest) (kernel.StopEvidence, error) {
	r.mu.Lock()
	targeted := request.Identity.WorkloadID == r.target && r.target != ""
	if targeted {
		r.stopCalls++
	}
	r.mu.Unlock()
	if targeted {
		return kernel.PendingStop("stop unconfirmed: 0 workload pods observed"), nil
	}
	return r.Runtime.Stop(ctx, request)
}

func (r *failedPreparationRuntime) Remove(ctx context.Context, request engine.RemoveRequest) (engine.Removal, error) {
	r.mu.Lock()
	targeted := request.Identity.WorkloadID == r.target && r.target != ""
	if targeted {
		r.removeCalls++
	}
	removeErr := r.removeErr
	r.mu.Unlock()
	if !targeted {
		return r.Runtime.Remove(ctx, request)
	}
	if removeErr != nil {
		return engine.Removal{}, removeErr
	}
	r.mu.Lock()
	r.removed[request.Identity.WorkloadID] = true
	r.mu.Unlock()
	return engine.Removal{Removed: true, Detail: "removed partial preparation resources"}, nil
}

func (r *failedPreparationRuntime) counts() (prepare, start, observe, stop, remove int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.prepareCalls, r.startCalls, r.observeCalls, r.stopCalls, r.removeCalls
}

func (r *failedPreparationRuntime) removedTarget(workloadID id.WorkloadID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.removed[workloadID]
}

// terminalPreparationFailedExecution reconstructs the persisted preparation-failure shape: the
// failed container has an issued launch marker and no observation, the
// execution carries a preparation failure and the cleanup record names only the
// workspace (the workload identity and attempts were lost).
func terminalPreparationFailedExecution(executionID, workspaceID string) (kernel.Execution, engine.EffectIdentity) {
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
	execution.Failure = &kernel.Failure{
		Kind:   kernel.FailurePreparation,
		Reason: "preparation_failed",
		Detail: "credential lifetime is shorter than the minimum",
	}
	execution.Cleanup = kernel.Cleanup{
		State:       kernel.CleanupPending,
		WorkspaceID: id.WorkspaceID(workspaceID),
	}
	return execution, identityFor(execution.ID, build)
}

// TestPreparationFailureCleanupUsesPreStartEvidence proves a live preparation
// failure settles the execution, records the failed identity's affirmative
// pre-Start evidence, removes its partial resources and releases the workspace
// in the same reconciliation instead of observing an absent object and waiting
// for an unconfirmed Stop forever.
func TestPreparationFailureCleanupUsesPreStartEvidence(t *testing.T) {
	build := containerAction("act-build", "build", 0, time.Hour)
	h := newHarness(t, newExecution("exec-prep-live", "ws-prep-live", build))
	runtime := newFailedPreparationRuntime(h.runtime, errors.New("credential lifetime is shorter than the minimum"))
	h.engine = engine.New(h.fixture.State, runtime, h.fixture.Workspace, h.fixture.Clock)

	failed := h.reconcile()
	action := mustAction(t, failed, "build")
	if failed.State != kernel.ExecutionFailed {
		t.Fatalf("execution state = %q, want failed", failed.State)
	}
	if failed.Failure == nil || failed.Failure.Kind != kernel.FailurePreparation {
		t.Fatalf("failure = %+v, want preparation", failed.Failure)
	}
	if action.State != kernel.ActionFailed {
		t.Fatalf("action state = %q, want failed", action.State)
	}
	if !action.Container.Observation.ProofOfNonexecution() {
		t.Fatalf("failed identity lost its pre-Start proof: %+v", action.Container.Observation)
	}
	if failed.Cleanup.State != kernel.CleanupComplete {
		t.Fatalf("cleanup state = %q, want complete; the preparation failure stranded the workspace", failed.Cleanup.State)
	}
	if failed.Cleanup.WorkloadID != action.Container.WorkloadID {
		t.Fatalf("cleanup workload = %q, want the failed identity %q", failed.Cleanup.WorkloadID, action.Container.WorkloadID)
	}
	if !h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("preparation failure did not release the workspace")
	}
	prepares, starts, observes, stops, removes := runtime.counts()
	if prepares != 1 || starts != 0 {
		t.Fatalf("prepare/start calls = %d/%d, want 1/0", prepares, starts)
	}
	if observes != 0 || stops != 0 {
		t.Fatalf("cleanup observed/stopped a proven never-started identity: observe=%d stop=%d", observes, stops)
	}
	if removes != 1 {
		t.Fatalf("Remove calls = %d, want exactly one for the partial preparation", removes)
	}
	if !runtime.removedTarget(action.Container.WorkloadID) {
		t.Fatalf("partial resources of %q were not removed", action.Container.WorkloadID)
	}
}

// TestPersistedPreparationFailureCleanupOnFreshController proves an
// already-persisted preparation failure (a record whose cleanup names only
// the workspace) converges on a fresh controller from the precise failure
// provenance, without a data migration.
func TestPersistedPreparationFailureCleanupOnFreshController(t *testing.T) {
	execution, identity := terminalPreparationFailedExecution("exec-prep-persisted", "ws-prep-persisted")
	h := newHarness(t, execution)
	h.retainWorkspace()
	runtime := newFailedPreparationRuntime(h.runtime, nil)
	runtime.setTarget(identity.WorkloadID)
	h.engine = engine.New(h.fixture.State, runtime, h.fixture.Workspace, h.fixture.Clock)

	completed := h.reconcile()
	action := mustAction(t, completed, "build")
	if completed.State != kernel.ExecutionFailed {
		t.Fatalf("terminal state = %q, want failed", completed.State)
	}
	if completed.Failure == nil || completed.Failure.Kind != kernel.FailurePreparation {
		t.Fatalf("terminal failure = %+v, want the persisted preparation failure", completed.Failure)
	}
	if completed.Cleanup.State != kernel.CleanupComplete {
		t.Fatalf("cleanup state = %q, want complete: the persisted preparation-failure record never converges", completed.Cleanup.State)
	}
	if completed.Cleanup.WorkloadID != identity.WorkloadID {
		t.Fatalf("cleanup workload = %q, want %q", completed.Cleanup.WorkloadID, identity.WorkloadID)
	}
	if !h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("persisted preparation failure did not release the workspace")
	}
	_, starts, observes, stops, removes := runtime.counts()
	if starts != 0 {
		t.Fatalf("fresh controller started %d workloads, want 0", starts)
	}
	if observes != 0 || stops != 0 {
		t.Fatalf("fresh controller observed/stopped the persisted proof: observe=%d stop=%d", observes, stops)
	}
	if removes != 1 || !runtime.removedTarget(identity.WorkloadID) {
		t.Fatalf("Remove calls = %d, removed=%v; want the exact failed identity removed once",
			removes, runtime.removedTarget(identity.WorkloadID))
	}
	if action.Container.Launch.State != kernel.LaunchIssued {
		t.Fatalf("cleanup rewrote the launch marker to %q", action.Container.Launch.State)
	}
}

// TestPreparationFailureKeepsUncertaintyAndEarlierObligations pins the two
// boundaries: an ambiguous launch is never promoted to never-started, and a
// failed action never hides an earlier workload's cleanup obligation.
func TestPreparationFailureKeepsUncertaintyAndEarlierObligations(t *testing.T) {
	t.Run("ambiguous launch stays pending", func(t *testing.T) {
		execution, identity := terminalPreparationFailedExecution("exec-prep-ambiguous", "ws-prep-ambiguous")
		execution.Actions[0].Container.Launch.State = kernel.LaunchUnknown
		h := newHarness(t, execution)
		h.retainWorkspace()
		runtime := newFailedPreparationRuntime(h.runtime, nil)
		runtime.setTarget(identity.WorkloadID)
		h.engine = engine.New(h.fixture.State, runtime, h.fixture.Workspace, h.fixture.Clock)

		pending := h.reconcile()
		if pending.Cleanup.State != kernel.CleanupPending {
			t.Fatalf("cleanup state = %q, want pending for an ambiguous launch", pending.Cleanup.State)
		}
		if h.fixture.Workspace.IsReleased(h.workspaceID) {
			t.Fatal("ambiguous launch released the workspace")
		}
		if _, _, observes, _, removes := runtime.counts(); observes == 0 || removes != 0 {
			t.Fatalf("ambiguous launch observe/remove = %d/%d, want observation without removal", observes, removes)
		}
		if mustAction(t, pending, "build").Stop.IsConfirmed() {
			t.Fatal("ambiguous launch fabricated stop evidence")
		}
	})

	t.Run("earlier confirmed workload is still removed", func(t *testing.T) {
		workspaceID := "ws-prep-earlier"
		earlier := containerAction("act-earlier", "earlier", 0, time.Hour)
		earlier.State = kernel.ActionSucceeded
		earlier.Version = 3
		earlier.Stop = kernel.ConfirmedStop("earlier workload stopped")
		earlier.Container.WorkloadID = id.WorkloadID("wl-exec-prep-earlier-earlier")
		earlier.Container.WorkspaceID = id.WorkspaceID(workspaceID)
		earlier.Container.Launch = kernel.LaunchIntent{
			WorkloadID:     earlier.Container.WorkloadID,
			State:          kernel.LaunchIssued,
			IdempotencyKey: "idem-earlier",
		}
		failed := containerAction("act-build", "build", 1, time.Hour)
		failed.State = kernel.ActionFailed
		failed.Version = 3
		failed.Container.WorkloadID = id.WorkloadID("wl-exec-prep-earlier-build")
		failed.Container.WorkspaceID = id.WorkspaceID(workspaceID)
		failed.Container.Launch = kernel.LaunchIntent{
			WorkloadID:     failed.Container.WorkloadID,
			State:          kernel.LaunchIssued,
			IdempotencyKey: "idem-build",
		}
		execution := newExecution("exec-prep-earlier", workspaceID, earlier, failed)
		execution.State = kernel.ExecutionFailed
		execution.CurrentAction = -1
		execution.Failure = &kernel.Failure{Kind: kernel.FailurePreparation, Reason: "preparation_failed"}
		execution.Cleanup = kernel.Cleanup{
			State:       kernel.CleanupPending,
			WorkloadID:  earlier.Container.WorkloadID,
			WorkspaceID: id.WorkspaceID(workspaceID),
		}
		h := newHarness(t, execution)
		h.retainWorkspace()
		h.prepareAndStart(identityFor(execution.ID, earlier), earlier.Container.Definition)
		h.confirmRuntimeStop(identityFor(execution.ID, earlier))
		runtime := newFailedPreparationRuntime(h.runtime, nil)
		runtime.setTarget(failed.Container.WorkloadID)
		h.engine = engine.New(h.fixture.State, runtime, h.fixture.Workspace, h.fixture.Clock)

		completed := h.reconcile()
		if completed.Cleanup.State != kernel.CleanupComplete {
			t.Fatalf("cleanup state = %q, want complete", completed.Cleanup.State)
		}
		if !h.fixture.Workspace.IsReleased(h.workspaceID) {
			t.Fatal("cleanup did not release the workspace")
		}
		earlierWorkload, ok := h.fixture.Runtime.Workload(earlier.Container.WorkloadID)
		if !ok || !earlierWorkload.Removed {
			t.Fatalf("earlier workload %q was not removed: %+v", earlier.Container.WorkloadID, earlierWorkload)
		}
		if !runtime.removedTarget(failed.Container.WorkloadID) {
			t.Fatalf("failed identity %q was not removed", failed.Container.WorkloadID)
		}
	})
}

// TestPreparationFailureCleanupFailureAttribution pins K-CLEANUP for the
// pre-Start path: a failed removal or release is attributed, bounded and
// retryable, and never rewrites the terminal outcome.
func TestPreparationFailureCleanupFailureAttribution(t *testing.T) {
	t.Run("partial removal failure is attributed and retried", func(t *testing.T) {
		execution, identity := terminalPreparationFailedExecution("exec-prep-remove-fail", "ws-prep-remove-fail")
		h := newHarness(t, execution)
		h.retainWorkspace()
		runtime := newFailedPreparationRuntime(h.runtime, nil)
		runtime.setTarget(identity.WorkloadID)
		runtime.setRemoveError(errors.New("injected partial removal failure"))
		h.engine = engine.New(h.fixture.State, runtime, h.fixture.Workspace, h.fixture.Clock)

		pending := h.reconcile()
		if pending.Cleanup.State != kernel.CleanupPending {
			t.Fatalf("cleanup state = %q, want pending after a removal failure", pending.Cleanup.State)
		}
		if pending.Cleanup.Attempts != 1 {
			t.Fatalf("cleanup attempts = %d, want 1", pending.Cleanup.Attempts)
		}
		if !strings.Contains(pending.Cleanup.LastError, "injected partial removal failure") {
			t.Fatalf("cleanup last error = %q, want the removal failure", pending.Cleanup.LastError)
		}
		if pending.Cleanup.WorkloadID != identity.WorkloadID {
			t.Fatalf("cleanup workload = %q, want %q", pending.Cleanup.WorkloadID, identity.WorkloadID)
		}
		if h.fixture.Workspace.IsReleased(h.workspaceID) {
			t.Fatal("failed removal released the workspace")
		}
		if pending.State != kernel.ExecutionFailed || pending.Failure == nil || pending.Failure.Kind != kernel.FailurePreparation {
			t.Fatalf("terminal outcome changed under cleanup failure: state=%q failure=%+v", pending.State, pending.Failure)
		}

		runtime.setRemoveError(nil)
		// A fresh controller instance resumes from the persisted pending record.
		h.engine = engine.New(h.fixture.State, runtime, h.fixture.Workspace, h.fixture.Clock)
		completed := h.reconcile()
		if completed.Cleanup.State != kernel.CleanupComplete {
			t.Fatalf("cleanup state after retry = %q, want complete", completed.Cleanup.State)
		}
		if completed.Cleanup.Attempts != 1 {
			t.Fatalf("completed cleanup attempts = %d, want the preserved 1", completed.Cleanup.Attempts)
		}
		if !h.fixture.Workspace.IsReleased(h.workspaceID) {
			t.Fatal("successful retry did not release the workspace")
		}
		if completed.State != kernel.ExecutionFailed || completed.Failure == nil || completed.Failure.Kind != kernel.FailurePreparation {
			t.Fatalf("terminal outcome changed after cleanup: state=%q failure=%+v", completed.State, completed.Failure)
		}
	})

	t.Run("workspace release failure is attributed and retried", func(t *testing.T) {
		execution, identity := terminalPreparationFailedExecution("exec-prep-release-fail", "ws-prep-release-fail")
		h := newHarness(t, execution)
		h.retainWorkspace()
		runtime := newFailedPreparationRuntime(h.runtime, nil)
		runtime.setTarget(identity.WorkloadID)
		h.engine = engine.New(h.fixture.State, runtime, h.fixture.Workspace, h.fixture.Clock)
		h.fixture.Workspace.SetGuard(func(id.WorkspaceID) error {
			return errors.New("injected release failure")
		})

		pending := h.reconcile()
		if pending.Cleanup.State != kernel.CleanupPending {
			t.Fatalf("cleanup state = %q, want pending after a release failure", pending.Cleanup.State)
		}
		if pending.Cleanup.Attempts != 1 {
			t.Fatalf("cleanup attempts = %d, want 1", pending.Cleanup.Attempts)
		}
		if !strings.Contains(pending.Cleanup.LastError, "injected release failure") {
			t.Fatalf("cleanup last error = %q, want the release failure", pending.Cleanup.LastError)
		}
		if pending.Cleanup.WorkloadID != identity.WorkloadID {
			t.Fatalf("cleanup workload = %q, want %q", pending.Cleanup.WorkloadID, identity.WorkloadID)
		}
		if h.fixture.Workspace.IsReleased(h.workspaceID) {
			t.Fatal("failed release reported the workspace released")
		}

		h.fixture.Workspace.SetGuard(nil)
		// A fresh controller instance resumes from the persisted release failure.
		h.engine = engine.New(h.fixture.State, runtime, h.fixture.Workspace, h.fixture.Clock)
		completed := h.reconcile()
		if completed.Cleanup.State != kernel.CleanupComplete {
			t.Fatalf("cleanup state after retry = %q, want complete", completed.Cleanup.State)
		}
		if completed.Cleanup.Attempts != 1 {
			t.Fatalf("completed cleanup attempts = %d, want the preserved 1", completed.Cleanup.Attempts)
		}
		if !h.fixture.Workspace.IsReleased(h.workspaceID) {
			t.Fatal("successful retry did not release the workspace")
		}
	})
}

// TestUnconfirmedStopKeepsAttributionWithoutSpendingBound pins S02 §5's lost
// stop response rule. An unconfirmed Stop acknowledgment is recorded against the
// exact identity and its detail, but it is not a failed cleanup attempt: a
// terminating pod may acknowledge every scan pass (LoopInterval defaults to
// 20 ms) while its grace period runs. Spending the installation retry bound on
// those acknowledgments would exhaust cleanup in ~100 ms and never remove the
// workload or release the workspace after the pod finally terminates. The bound
// counts failed effects (Stop errors, Remove and Release failures) only.
func TestUnconfirmedStopKeepsAttributionWithoutSpendingBound(t *testing.T) {
	execution, identity := terminalFailedExecutionWithCleanup("exec-stop-attribution", "ws-stop-attribution")
	execution.Actions[0].Stop = kernel.StopEvidence{}
	execution.Cleanup = kernel.Cleanup{
		State:       kernel.CleanupPending,
		WorkspaceID: execution.WorkspaceID,
	}
	h := newHarness(t, execution)
	h.retainWorkspace()
	h.prepareAndStart(identity, execution.Actions[0].Container.Definition)

	pendingPasses := engine.DefaultCleanupMaxAttempts + 3
	for pass := range pendingPasses {
		pending := h.reconcile()
		if pending.Cleanup.State != kernel.CleanupPending {
			t.Fatalf("cleanup state after pass %d = %q, want pending without stop evidence", pass, pending.Cleanup.State)
		}
		if pending.Cleanup.Attempts != 0 {
			t.Fatalf("cleanup attempts after pass %d = %d, want 0: an unconfirmed Stop is not a cleanup attempt",
				pass, pending.Cleanup.Attempts)
		}
		if pending.Cleanup.WorkloadID != identity.WorkloadID {
			t.Fatalf("cleanup workload after pass %d = %q, want the stop identity %q",
				pass, pending.Cleanup.WorkloadID, identity.WorkloadID)
		}
		if !strings.Contains(pending.Cleanup.LastError, "termination is not confirmed") {
			t.Fatalf("cleanup last error after pass %d = %q, want the unconfirmed Stop detail",
				pass, pending.Cleanup.LastError)
		}
		if h.fixture.Workspace.IsReleased(h.workspaceID) {
			t.Fatalf("unconfirmed Stop released the workspace after pass %d", pass)
		}
	}
	if got := h.runtime.stopCount(); got != pendingPasses {
		t.Fatalf("Stop calls while pending = %d, want one per pass (%d): the boundary kept retrying", got, pendingPasses)
	}
	if got := h.runtime.removeCount(); got != 0 {
		t.Fatalf("Remove calls while pending = %d, want none without stop evidence", got)
	}

	// A fresh controller resumes from the persisted, attributable barrier; the
	// workload then terminates and cleanup must converge on it rather than
	// reporting an exhausted bound.
	h.restart()
	h.fixture.Runtime.ConfirmNextStop("test: termination observed")
	completed := h.reconcileUntil("cleanup complete after the stop confirms", 4, func(candidate kernel.Execution) bool {
		return candidate.Cleanup.State == kernel.CleanupComplete
	})
	if completed.Cleanup.Attempts != 0 {
		t.Fatalf("completed cleanup attempts = %d, want the preserved 0", completed.Cleanup.Attempts)
	}
	if completed.Cleanup.LastError != "" {
		t.Fatalf("completed cleanup last error = %q, want empty", completed.Cleanup.LastError)
	}
	if completed.Cleanup.WorkloadID != identity.WorkloadID {
		t.Fatalf("completed cleanup workload = %q, want %q", completed.Cleanup.WorkloadID, identity.WorkloadID)
	}
	if got := h.runtime.stopCount(); got != pendingPasses+1 {
		t.Fatalf("Stop calls = %d, want %d (one per pending pass, then the confirming call)", got, pendingPasses+1)
	}
	if got := h.runtime.removeCount(); got != 1 {
		t.Fatalf("Remove calls = %d, want exactly one after confirmation", got)
	}
	if !h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("confirmed stop did not release the workspace")
	}
	workload, ok := h.fixture.Runtime.Workload(identity.WorkloadID)
	if !ok || !workload.Removed {
		t.Fatalf("cleanup did not remove workload %q: %+v", identity.WorkloadID, workload)
	}
	if completed.State != kernel.ExecutionFailed || completed.Failure == nil || completed.Failure.Kind != kernel.FailureRuntime {
		t.Fatalf("terminal outcome changed under the stop barrier: state=%q failure=%+v", completed.State, completed.Failure)
	}
}

// failureCommitCrashState simulates a controller crash between the durable
// preparation-failure commit and terminal cleanup: the commit lands, then the
// reconciler observes a storage-style failure instead of continuing.
type failureCommitCrashState struct {
	engine.State
	mu     sync.Mutex
	armed  bool
	failed bool
}

func (s *failureCommitCrashState) Commit(ctx context.Context, commit flow.Commit) (kernel.Execution, error) {
	updated, err := s.State.Commit(ctx, commit)
	if err != nil {
		return updated, err
	}
	s.mu.Lock()
	armed := s.armed && commit.Failure != nil && commit.Failure.Kind == kernel.FailurePreparation
	if armed {
		s.armed = false
		s.failed = true
	}
	s.mu.Unlock()
	if armed {
		return updated, errors.New("injected crash after the preparation-failure commit")
	}
	return updated, nil
}

func (s *failureCommitCrashState) crashed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failed
}

// TestPreparationFailureKeepsEarlierCleanupAttribution pins failPreparation's
// non-issued branch. A preparation failure whose action never got an issued
// launch marker (an unsupported runtime capability here) owns no identity
// itself, but the pending cleanup record must still name the earlier issued
// workload so a controller that crashes before cleanup keeps an attributable
// obligation.
func TestPreparationFailureKeepsEarlierCleanupAttribution(t *testing.T) {
	workspaceID := "ws-prep-nonissued"
	earlier := containerAction("act-earlier", "earlier", 0, time.Hour)
	earlier.State = kernel.ActionSucceeded
	earlier.Version = 3
	earlier.Stop = kernel.ConfirmedStop("earlier workload stopped")
	earlier.Container.WorkloadID = id.WorkloadID("wl-exec-prep-nonissued-earlier")
	earlier.Container.WorkspaceID = id.WorkspaceID(workspaceID)
	earlier.Container.Launch = kernel.LaunchIntent{
		WorkloadID:     earlier.Container.WorkloadID,
		State:          kernel.LaunchIssued,
		IdempotencyKey: "idem-earlier",
	}
	next := containerAction("act-next", "next", 1, time.Hour)
	execution := newExecution("exec-prep-nonissued", workspaceID, earlier, next)
	// Require an adapter capability the fake runtime does not declare, so
	// activation succeeds but Start preparation fails before the launch marker.
	execution.Snapshot = definition.NewSnapshot(definition.SnapshotInput{
		Pipeline: execution.Snapshot.Pipeline(),
		YAML:     execution.Snapshot.YAML(),
		Inputs:   execution.Snapshot.Inputs(),
		Runtime:  definition.RuntimeRequirements{Capabilities: []string{"kubernetes"}},
	})

	h := newHarness(t, execution)
	h.retainWorkspace()
	h.prepareAndStart(identityFor(execution.ID, earlier), earlier.Container.Definition)
	h.confirmRuntimeStop(identityFor(execution.ID, earlier))

	crashing := &failureCommitCrashState{State: h.fixture.State, armed: true}
	h.engine = engine.New(crashing, h.runtime, h.fixture.Workspace, h.fixture.Clock)
	if _, err := h.engine.Reconcile(h.t.Context(), h.executionID); err == nil {
		t.Fatal("Reconcile error = nil, want the injected post-commit crash")
	} else if !strings.Contains(err.Error(), "injected crash after the preparation-failure commit") {
		t.Fatalf("Reconcile error = %v, want the injected post-commit crash", err)
	}
	if !crashing.crashed() {
		t.Fatal("the preparation-failure commit was never exercised")
	}

	failed := h.load()
	if failed.State != kernel.ExecutionFailed {
		t.Fatalf("execution state = %q, want failed", failed.State)
	}
	if failed.Failure == nil || failed.Failure.Kind != kernel.FailurePreparation || failed.Failure.Reason != "unsupported_capability" {
		t.Fatalf("failure = %+v, want an unsupported_capability preparation failure", failed.Failure)
	}
	if failed.Cleanup.State != kernel.CleanupPending {
		t.Fatalf("cleanup state = %q, want pending before cleanup ran", failed.Cleanup.State)
	}
	if failed.Cleanup.WorkloadID != earlier.Container.WorkloadID {
		t.Fatalf("cleanup workload = %q, want the earlier issued identity %q: the failure dropped earlier attribution",
			failed.Cleanup.WorkloadID, earlier.Container.WorkloadID)
	}
	if h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("the crashed failure commit released the workspace")
	}

	// A fresh controller converges from the persisted attribution alone.
	h.engine = engine.New(h.fixture.State, h.runtime, h.fixture.Workspace, h.fixture.Clock)
	completed := h.reconcile()
	if completed.Cleanup.State != kernel.CleanupComplete {
		t.Fatalf("cleanup state = %q, want complete", completed.Cleanup.State)
	}
	if completed.Cleanup.WorkloadID != earlier.Container.WorkloadID {
		t.Fatalf("completed cleanup workload = %q, want %q", completed.Cleanup.WorkloadID, earlier.Container.WorkloadID)
	}
	if !h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("fresh controller did not release the workspace")
	}
	earlierWorkload, ok := h.fixture.Runtime.Workload(earlier.Container.WorkloadID)
	if !ok || !earlierWorkload.Removed {
		t.Fatalf("earlier workload %q was not removed: %+v", earlier.Container.WorkloadID, earlierWorkload)
	}
}

// TestExhaustedLegacyCleanupRecordStaysParked documents a deliberate, known
// limitation: a record whose persisted Attempts already reached the bound and
// whose LastError carries the exhaustion prefix does not auto-resume. The bound
// remains a read-only visible stop for failed effects, so such a record stays
// pending and attributable; stage data is disposable, so the recovery is a
// fresh execution rather than silently re-driving the old one.
func TestExhaustedLegacyCleanupRecordStaysParked(t *testing.T) {
	execution, identity := terminalFailedExecutionWithCleanup("exec-cleanup-legacy", "ws-cleanup-legacy")
	execution.Cleanup.Attempts = engine.DefaultCleanupMaxAttempts
	execution.Cleanup.LastError = "cleanup retry exhausted: stop unconfirmed: pod terminating"
	h := newHarness(t, execution)
	h.retainWorkspace()
	h.prepareAndStart(identity, execution.Actions[0].Container.Definition)
	h.confirmRuntimeStop(identity)

	for pass := range 2 {
		parked := h.reconcile()
		if parked.Cleanup.State != kernel.CleanupPending {
			t.Fatalf("cleanup state after pass %d = %q, want pending", pass, parked.Cleanup.State)
		}
		if parked.Cleanup.Attempts != engine.DefaultCleanupMaxAttempts {
			t.Fatalf("cleanup attempts after pass %d = %d, want the preserved %d",
				pass, parked.Cleanup.Attempts, engine.DefaultCleanupMaxAttempts)
		}
		if !strings.HasPrefix(parked.Cleanup.LastError, "cleanup retry exhausted") {
			t.Fatalf("cleanup last error after pass %d = %q, want the preserved exhaustion diagnostic",
				pass, parked.Cleanup.LastError)
		}
		if h.fixture.Workspace.IsReleased(h.workspaceID) {
			t.Fatalf("parked exhausted record released the workspace on pass %d", pass)
		}
	}

	// A fresh controller reads the same exhausted record and still parks it.
	h.restart()
	for range 2 {
		parked := h.reconcile()
		if parked.Cleanup.State != kernel.CleanupPending {
			t.Fatalf("cleanup state on the fresh controller = %q, want pending", parked.Cleanup.State)
		}
		if parked.Cleanup.Attempts != engine.DefaultCleanupMaxAttempts {
			t.Fatalf("fresh controller cleanup attempts = %d, want the preserved %d",
				parked.Cleanup.Attempts, engine.DefaultCleanupMaxAttempts)
		}
		if h.fixture.Workspace.IsReleased(h.workspaceID) {
			t.Fatal("fresh controller resumed an exhausted record and released the workspace")
		}
	}
	if got := h.runtime.removeCount(); got != 0 {
		t.Fatalf("Remove calls = %d, want none for a parked exhausted record", got)
	}
	if got := h.runtime.stopCount(); got != 0 {
		t.Fatalf("Stop calls = %d, want none for a parked exhausted record", got)
	}
}
