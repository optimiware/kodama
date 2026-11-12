package enginetest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/testkit"
)

// observeFailingRuntime fails every Observe of one workload identity and
// delegates everything else. It models one execution whose workload stays
// unreachable while the rest of the runtime is healthy.
type observeFailingRuntime struct {
	engine.Runtime
	workload id.WorkloadID
	err      error
}

func (r *observeFailingRuntime) Observe(ctx context.Context, request engine.ObserveRequest) (kernel.Observation, error) {
	if request.Identity.WorkloadID == r.workload {
		return kernel.UnknownObservation(), r.err
	}
	return r.Runtime.Observe(ctx, request)
}

// TestScanContinuesPastFailingExecution pins that one execution whose
// reconciliation keeps failing does not stop the recovery pass: an execution
// that sorts after it is still reconciled in the same pass, and the failure is
// still reported.
func TestScanContinuesPastFailingExecution(t *testing.T) {
	unreachable := runningContainerExecution("exec-scan-a-unreachable", "ws-scan-a-unreachable", testNow.Add(time.Hour))
	healthy := newExecution("exec-scan-b-healthy", "ws-scan-b-healthy", containerAction("act-build", "build", 0, time.Hour))

	h := newHarness(t, unreachable)
	h.fixture.Store.Seed(healthy)
	h.retainWorkspace()
	h.engine = engine.New(h.fixture.State, &observeFailingRuntime{
		Runtime:  h.runtime,
		workload: unreachable.Actions[0].Container.WorkloadID,
		err:      fault.New(fault.DependencyUnavailable, "workload state is unreachable"),
	}, h.fixture.Workspace, h.fixture.Clock)

	err := h.engine.Scan(t.Context())
	if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
		t.Fatalf("Scan() error = %v, want the dependency_unavailable failure reported", err)
	}

	reconciled := h.loadID(healthy.ID)
	if reconciled.State != kernel.ExecutionRunning {
		t.Fatalf("execution after the failing one was not reconciled: %s", actionSummary(reconciled))
	}
	if state := mustAction(t, reconciled, "build").State; state != kernel.ActionRunning {
		t.Fatalf("action state of the execution after the failing one = %q, want running", state)
	}
	if h.fixture.Runtime.StartCount() != 1 {
		t.Fatalf("runtime start effects = %d, want 1", h.fixture.Runtime.StartCount())
	}
}

// TestScanReachesEveryUnfinishedExecution pins that recovery passes reach every
// unfinished execution of a large backlog. Executions that stay unfinished
// after being reconciled (running containers here) must not keep the rest out
// of every later pass, which a bounded selection in a stable order would do.
func TestScanReachesEveryUnfinishedExecution(t *testing.T) {
	const executions = 300
	const passes = 4

	fixture := testkit.NewFixture(testNow)
	seeded := make([]kernel.Execution, 0, executions)
	for index := range executions {
		name := fmt.Sprintf("exec-scan-window-%03d", index)
		seeded = append(seeded, newExecution(name, "ws-"+name, containerAction("act-build", "build", 0, time.Hour)))
	}
	fixture.Store.Seed(seeded...)
	subject := engine.New(fixture.State, fixture.Runtime, fixture.Workspace, fixture.Clock)

	for pass := range passes {
		if err := subject.Scan(t.Context()); err != nil {
			t.Fatalf("Scan() pass %d error = %v", pass+1, err)
		}
	}

	var stranded []id.ExecutionID
	for _, execution := range seeded {
		current, ok := fixture.Store.Execution(execution.ID)
		if !ok {
			t.Fatalf("execution %s disappeared from the store", execution.ID)
		}
		if current.State == kernel.ExecutionQueued {
			stranded = append(stranded, execution.ID)
		}
	}
	if len(stranded) > 0 {
		t.Fatalf("%d of %d executions were never reconciled after %d passes; first stranded: %s",
			len(stranded), executions, passes, stranded[0])
	}
}
