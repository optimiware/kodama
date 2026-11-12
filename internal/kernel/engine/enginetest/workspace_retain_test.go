package enginetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/testkit"
)

// interruptedRetainWorkspace fails the next Retain through a one-shot hook and
// delegates everything else. The failed Retain has no effect, so the workspace
// stays exactly as the testkit fake reports it: never retained.
type interruptedRetainWorkspace struct {
	*testkit.Workspace

	mu   sync.Mutex
	next func(context.Context) error
}

func (w *interruptedRetainWorkspace) failNextRetain(hook func(context.Context) error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.next = hook
}

func (w *interruptedRetainWorkspace) Retain(ctx context.Context, workspaceID id.WorkspaceID) error {
	w.mu.Lock()
	hook := w.next
	w.next = nil
	w.mu.Unlock()
	if hook != nil {
		return hook(ctx)
	}
	return w.Workspace.Retain(ctx, workspaceID)
}

// requireStarted reconciles until the single container action runs with an
// issued launch. A terminal execution fails immediately with its diagnostic, so
// a retention problem that was settled as a failure is reported as such.
func requireStarted(t *testing.T, h *harness) {
	t.Helper()
	for step := 0; step < 4; step++ {
		execution, err := h.engine.Reconcile(t.Context(), h.executionID)
		if err != nil {
			t.Fatalf("Reconcile(%s) error = %v", h.executionID, err)
		}
		requireNotSettled(t, execution)
		action := mustAction(t, execution, "build")
		if action.State == kernel.ActionRunning && action.Container.Launch.State == kernel.LaunchIssued {
			if !h.fixture.Workspace.IsRetained(h.workspaceID) {
				t.Fatal("workload was launched without a retained workspace")
			}
			if h.fixture.Runtime.StartCount() != 1 {
				t.Fatalf("runtime start effects = %d, want 1", h.fixture.Runtime.StartCount())
			}
			return
		}
	}
	t.Fatalf("execution %s did not start its workload: %s", h.executionID, actionSummary(h.load()))
}

func requireNotSettled(t *testing.T, execution kernel.Execution) {
	t.Helper()
	if !execution.State.Terminal() {
		return
	}
	t.Fatalf("execution settled as %q with failure %+v, want it still in progress: %s",
		execution.State, execution.Failure, actionSummary(execution))
}

// TestActivationSurvivesTransientRetainFailure pins that a retryable workspace
// error during activation is not recorded as a lost workspace: the execution
// stays in progress and a later pass retains the workspace and starts the
// workload.
func TestActivationSurvivesTransientRetainFailure(t *testing.T) {
	h := newHarness(t, newExecution("exec-retain-transient", "ws-retain-transient", containerAction("act-build", "build", 0, time.Hour)))
	workspace := &interruptedRetainWorkspace{Workspace: h.fixture.Workspace}
	workspace.failNextRetain(func(context.Context) error {
		return fault.New(fault.DependencyUnavailable, "workspace backend is unreachable")
	})
	h.engine = engine.New(h.fixture.State, h.runtime, workspace, h.fixture.Clock)

	_, err := h.engine.Reconcile(t.Context(), h.executionID)
	if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
		t.Fatalf("Reconcile() error = %v, want the dependency_unavailable failure reported", err)
	}
	requireNotSettled(t, h.load())
	if h.runtime.prepareCount() != 0 || h.runtime.startCount() != 0 {
		t.Fatalf("workload was dispatched without a retained workspace: prepare=%d start=%d", h.runtime.prepareCount(), h.runtime.startCount())
	}

	requireStarted(t, h)
}

// TestActivationFailsVisiblyWhenWorkspaceIsLost pins that a workspace failure
// that is evidence about the workspace itself still fails the execution before
// any workload is dispatched.
func TestActivationFailsVisiblyWhenWorkspaceIsLost(t *testing.T) {
	h := newHarness(t, newExecution("exec-retain-lost", "ws-retain-lost", containerAction("act-build", "build", 0, time.Hour)))
	h.fixture.Workspace.Lose(h.workspaceID)

	failed := h.reconcileUntil("workspace loss failure", 4, func(candidate kernel.Execution) bool {
		return candidate.State == kernel.ExecutionFailed
	})
	if failed.Failure == nil || failed.Failure.Kind != kernel.FailureWorkspaceLost {
		t.Fatalf("failure = %+v, want workspace_lost", failed.Failure)
	}
	if h.runtime.prepareCount() != 0 || h.runtime.startCount() != 0 {
		t.Fatalf("workspace loss dispatched work: prepare=%d start=%d", h.runtime.prepareCount(), h.runtime.startCount())
	}
}

// TestLaterRequestSurvivesTransientRetainFailure pins that a retryable
// workspace error while a later request activates does not fail an execution
// whose workspace an earlier action already retained.
func TestLaterRequestSurvivesTransientRetainFailure(t *testing.T) {
	build := containerAction("act-build", "build", 0, time.Hour, "candidate")
	build.State = kernel.ActionSucceeded
	build.Version = 3
	build.Outputs = kernel.Outputs{"candidate": "abc"}
	build.Stop = kernel.ConfirmedStop("build stopped")
	build.Container.WorkloadID = id.WorkloadID("wl-retain-request")
	build.Container.Launch = kernel.LaunchIntent{
		WorkloadID:     build.Container.WorkloadID,
		State:          kernel.LaunchIssued,
		IdempotencyKey: "idem-retain-request",
	}
	review := requestAction("act-review", "review", 1, time.Hour, definitionSubjectForBuild(), "decision")
	execution := newExecution("exec-retain-request", "ws-retain-request", build, review)
	execution.State = kernel.ExecutionRunning
	execution.CurrentAction = -1

	h := newHarness(t, execution)
	h.retainWorkspace()
	workspace := &interruptedRetainWorkspace{Workspace: h.fixture.Workspace}
	workspace.failNextRetain(func(context.Context) error {
		return fault.New(fault.DependencyUnavailable, "workspace backend is unreachable")
	})
	h.engine = engine.New(h.fixture.State, h.runtime, workspace, h.fixture.Clock)

	_, err := h.engine.Reconcile(t.Context(), h.executionID)
	if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
		t.Fatalf("Reconcile() error = %v, want the dependency_unavailable failure reported", err)
	}
	requireNotAnswerable(t, h)

	requireAnswerable(t, h)
}

// TestActivationSurvivesInterruptedRetain pins that a controller stopped while
// Retain is in flight leaves recoverable state: the restarted controller
// retains the workspace and starts the workload instead of reporting a
// workspace that was never retained as lost.
func TestActivationSurvivesInterruptedRetain(t *testing.T) {
	h := newHarness(t, newExecution("exec-retain-interrupted", "ws-retain-interrupted", containerAction("act-build", "build", 0, time.Hour)))
	workspace := &interruptedRetainWorkspace{Workspace: h.fixture.Workspace}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	workspace.failNextRetain(func(ctx context.Context) error {
		cancel()
		return ctx.Err()
	})
	h.engine = engine.New(h.fixture.State, h.runtime, workspace, h.fixture.Clock)

	if _, err := h.engine.Reconcile(ctx, h.executionID); err == nil {
		t.Fatal("Reconcile() error = nil, want the interruption reported")
	}
	requireNotSettled(t, h.load())

	h.engine = engine.New(h.fixture.Restart(), h.runtime, workspace, h.fixture.Clock)
	requireStarted(t, h)
}

// requireAnswerable reconciles once and requires the request to wait for its
// answer with a resolved subject and a retained workspace.
func requireAnswerable(t *testing.T, h *harness) {
	t.Helper()
	waiting := h.reconcile()
	requireNotSettled(t, waiting)
	review := mustAction(t, waiting, "review")
	if review.State != kernel.ActionRunning {
		t.Fatalf("request state = %q, want running", review.State)
	}
	if review.Request == nil || review.Request.Subject == nil {
		t.Fatal("activated request has no resolved subject")
	}
	if !h.fixture.Workspace.IsRetained(h.workspaceID) {
		t.Fatal("request became answerable without a retained workspace")
	}
}

// requireNotAnswerable requires the request to be untouched: it must not
// become answerable before its workspace is retained.
func requireNotAnswerable(t *testing.T, h *harness) {
	t.Helper()
	execution := h.load()
	requireNotSettled(t, execution)
	if state := mustAction(t, execution, "review").State; state != kernel.ActionPending {
		t.Fatalf("request state without a retained workspace = %q, want pending", state)
	}
}

func firstRequestExecution(executionID, workspaceID string) kernel.Execution {
	review := requestAction("act-review", "review", 0, time.Hour, literalSubject(map[string]string{"operation": "review"}), "decision")
	return newExecution(executionID, workspaceID, review)
}

// TestFirstRequestSurvivesTransientRetainFailure pins that a request that is
// the first action is not answerable until its workspace is retained, and that
// a retryable workspace error only delays it.
func TestFirstRequestSurvivesTransientRetainFailure(t *testing.T) {
	h := newHarness(t, firstRequestExecution("exec-retain-first-request", "ws-retain-first-request"))
	workspace := &interruptedRetainWorkspace{Workspace: h.fixture.Workspace}
	workspace.failNextRetain(func(context.Context) error {
		return fault.New(fault.DependencyUnavailable, "workspace backend is unreachable")
	})
	h.engine = engine.New(h.fixture.State, h.runtime, workspace, h.fixture.Clock)

	_, err := h.engine.Reconcile(t.Context(), h.executionID)
	if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
		t.Fatalf("Reconcile() error = %v, want the dependency_unavailable failure reported", err)
	}
	requireNotAnswerable(t, h)

	requireAnswerable(t, h)
}

// TestFirstRequestSurvivesInterruptedRetain pins that a controller stopped
// while Retain is in flight for a first-action request leaves it pending, and
// that the restarted controller retains the workspace and activates it.
func TestFirstRequestSurvivesInterruptedRetain(t *testing.T) {
	h := newHarness(t, firstRequestExecution("exec-retain-request-interrupted", "ws-retain-request-interrupted"))
	workspace := &interruptedRetainWorkspace{Workspace: h.fixture.Workspace}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	workspace.failNextRetain(func(ctx context.Context) error {
		cancel()
		return ctx.Err()
	})
	h.engine = engine.New(h.fixture.State, h.runtime, workspace, h.fixture.Clock)

	if _, err := h.engine.Reconcile(ctx, h.executionID); err == nil {
		t.Fatal("Reconcile() error = nil, want the interruption reported")
	}
	requireNotAnswerable(t, h)

	h.engine = engine.New(h.fixture.Restart(), h.runtime, workspace, h.fixture.Clock)
	requireAnswerable(t, h)
}
