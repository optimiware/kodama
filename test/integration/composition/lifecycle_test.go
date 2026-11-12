//go:build integration

package composition

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
)

// TestCancelRunningExecutionConfirmsStop proves a cancellation settles through
// the runtime port: the fake executor is asked to stop the live workload and
// the execution reaches the cancelled terminal state.
func TestCancelRunningExecutionConfirmsStop(t *testing.T) {
	harness := newHarness(t)
	runtime := newFakeRuntime(modeStayRunning)
	environment := harness.start(runtime)
	environment.grantEngineer("admin-1", projectID)
	pipelineID := environment.applyPipeline("build")

	started := environment.mustInvoke(projectToken, "task.create_and_start", api.TaskCreateAndStartRequest{
		ProjectID:  projectID,
		Title:      "cancel me",
		PipelineID: pipelineID,
	}, nil, "cancel-task-start")
	var task api.TaskCreateAndStartResult
	environment.decode(started, &task)
	environment.eventually(10*time.Second, func() bool { return runtime.startCount() == 1 })

	// The reconcile loop may commit an unrelated transition between the read
	// and the cancel, so retry the optimistic compare-and-set with the fresh
	// version and a fresh receipt key.
	cancelDeadline := time.Now().Add(10 * time.Second)
	for {
		if time.Now().After(cancelDeadline) {
			t.Fatal("execution.cancel never won the version boundary")
		}
		inspect, publicError := environment.call(projectToken, "execution.inspect", api.ExecutionInspectRequest{
			ExecutionID: task.ExecutionID,
		}, nil, "")
		if publicError.Code != "" {
			t.Fatalf("execution.inspect: %+v", publicError)
		}
		var view api.ExecutionInspectResult
		environment.decode(inspect, &view)
		version := view.Version
		// The cancel is issued by an operator-only credential: the registry
		// admits the operator role for execution.cancel.
		_, cancelError := environment.call(operatorToken, "execution.cancel", api.ExecutionCancelRequest{
			ExecutionID: task.ExecutionID,
		}, &version, "cancel-execution-"+view.ExecutionID[len(view.ExecutionID)-8:])
		switch cancelError.Code {
		case "":
		case api.CodeVersionConflict:
			time.Sleep(2 * time.Millisecond)
			continue
		default:
			t.Fatalf("execution.cancel refused: %+v", cancelError)
		}
		break
	}

	environment.eventually(10*time.Second, func() bool {
		result, publicError := environment.call(projectToken, "execution.inspect", api.ExecutionInspectRequest{
			ExecutionID: task.ExecutionID,
		}, nil, "")
		if publicError.Code != "" {
			return false
		}
		var current api.ExecutionInspectResult
		environment.decode(result, &current)
		return current.State == "cancelled"
	})
	environment.eventually(10*time.Second, func() bool { return runtime.stopCount() > 0 })
	stopped := runtime.stoppedIDs()
	if len(stopped) != 1 {
		t.Fatalf("stopped workloads = %v; want one", stopped)
	}
	environment.eventually(10*time.Second, func() bool {
		result, publicError := environment.call(projectToken, "execution.inspect", api.ExecutionInspectRequest{
			ExecutionID: task.ExecutionID,
		}, nil, "")
		if publicError.Code != "" {
			return false
		}
		var current api.ExecutionInspectResult
		environment.decode(result, &current)
		return current.Cleanup.State == "complete"
	})

	// The cancellation phase is projected exactly once each, in order, with no
	// duplicate cleanup step and no success/failure event.
	var (
		cancelled            int
		succeeded            int
		failed               int
		pending              int
		complete             int
		cancelledTransition  int
		activationTransition int
		phase                []string
	)
	for _, envelope := range readAllEventPage(t, environment).Events {
		switch envelope.Type {
		case "execution.cancelled":
			cancelled++
			phase = append(phase, "cancelled")
		case "execution.succeeded":
			succeeded++
		case "execution.failed":
			failed++
		case "execution.cleanup":
			var facts lifecycleFacts
			if err := json.Unmarshal(envelope.Data, &facts); err != nil {
				t.Fatalf("decode execution.cleanup facts: %v", err)
			}
			switch facts.CleanupState {
			case "pending":
				pending++
				phase = append(phase, "cleanup-pending")
			case "complete":
				complete++
				phase = append(phase, "cleanup-complete")
			}
		case "action.transition":
			var facts lifecycleFacts
			if err := json.Unmarshal(envelope.Data, &facts); err != nil {
				t.Fatalf("decode action.transition facts: %v", err)
			}
			if facts.Kind != "container" || facts.Index != 0 {
				continue
			}
			switch {
			case facts.PreviousState == "pending" && facts.State == "running":
				activationTransition++
			case facts.PreviousState == "running" && facts.State == "cancelled":
				cancelledTransition++
				phase = append(phase, "action-cancelled")
			}
		}
	}
	if cancelled != 1 || succeeded != 0 || failed != 0 {
		t.Fatalf("terminal event counts cancelled=%d succeeded=%d failed=%d", cancelled, succeeded, failed)
	}
	if activationTransition != 1 || cancelledTransition != 1 {
		t.Fatalf("action transitions activation=%d cancelled=%d; want one each", activationTransition, cancelledTransition)
	}
	if pending != 1 || complete != 1 {
		t.Fatalf("cleanup events pending=%d complete=%d; want one each", pending, complete)
	}
	wantPhase := []string{"cancelled", "cleanup-pending", "action-cancelled", "cleanup-complete"}
	if !equalStrings(phase, wantPhase) {
		t.Fatalf("cancel event order = %v; want %v", phase, wantPhase)
	}
}

// TestBrokerOutageKeepsProgressionAndDrainsOriginalEvents proves publication is
// broker-independent: the execution still succeeds while the broker is down and
// the durable outbox later publishes the same committed event identities.
func TestBrokerOutageKeepsProgressionAndDrainsOriginalEvents(t *testing.T) {
	harness := newHarness(t)
	harness.publisher.setDown(true)
	runtime := newFakeRuntime(modeComplete)
	environment := harness.start(runtime)
	environment.grantEngineer("admin-1", projectID)
	pipelineID := environment.applyPipeline("build")

	started := environment.mustInvoke(projectToken, "task.create_and_start", api.TaskCreateAndStartRequest{
		ProjectID:  projectID,
		Title:      "broker outage",
		PipelineID: pipelineID,
	}, nil, "broker-outage-start")
	var task api.TaskCreateAndStartResult
	environment.decode(started, &task)

	environment.eventually(10*time.Second, func() bool {
		result, publicError := environment.call(projectToken, "execution.inspect", api.ExecutionInspectRequest{
			ExecutionID: task.ExecutionID,
		}, nil, "")
		if publicError.Code != "" {
			return false
		}
		var view api.ExecutionInspectResult
		environment.decode(result, &view)
		return view.State == "succeeded"
	})
	if published := harness.publisher.publishedIDs(); len(published) != 0 {
		t.Fatalf("broker was down but %d events were published: %v", len(published), published)
	}

	// Committed events are already readable while the broker is down.
	committed := readAllEvents(t, environment, projectID)
	if len(committed) == 0 {
		t.Fatal("no committed events are readable during the broker outage")
	}

	harness.publisher.setDown(false)
	environment.eventually(10*time.Second, func() bool {
		published := harness.publisher.publishedIDs()
		if len(published) < len(committed) {
			return false
		}
		seen := make(map[id.EventID]bool, len(published))
		for _, identity := range published {
			seen[identity] = true
		}
		for identity := range committed {
			if !seen[identity] {
				return false
			}
		}
		return true
	})
}

// TestGracefulRestartResumesWithoutReplayingAnUncertainLaunch proves a restart
// over the same memory store reconciles persisted work by observation and never
// re-issues an ambiguous launch.
func TestGracefulRestartResumesWithoutReplayingAnUncertainLaunch(t *testing.T) {
	harness := newHarness(t)
	firstRuntime := newFakeRuntime(modeUnknownLaunch)
	first := harness.start(firstRuntime)
	first.grantEngineer("admin-1", projectID)
	pipelineID := first.applyPipeline("build")

	started := first.mustInvoke(projectToken, "task.create_and_start", api.TaskCreateAndStartRequest{
		ProjectID:  projectID,
		Title:      "restart me",
		PipelineID: pipelineID,
	}, nil, "restart-task-start")
	var task api.TaskCreateAndStartResult
	first.decode(started, &task)
	first.eventually(10*time.Second, func() bool { return firstRuntime.startCount() == 1 })

	// Graceful shutdown mid-flight: the composition stops the live workload and
	// joins its goroutines without inventing a terminal state.
	first.close()

	secondRuntime := newFakeRuntime(modeComplete)
	second := harness.start(secondRuntime)
	second.eventually(10*time.Second, func() bool {
		result, publicError := second.call(projectToken, "execution.inspect", api.ExecutionInspectRequest{
			ExecutionID: task.ExecutionID,
		}, nil, "")
		if publicError.Code != "" {
			return false
		}
		var view api.ExecutionInspectResult
		second.decode(result, &view)
		return view.State == "succeeded"
	})
	if secondRuntime.startCount() != 0 {
		t.Fatalf("restart replayed the uncertain launch: %d starts", secondRuntime.startCount())
	}

	// Persisted state stayed consistent: the task and its single execution are
	// still readable after the restart.
	read := second.mustInvoke(projectToken, "task.read", api.TaskReadRequest{TaskID: task.TaskID}, nil, "")
	var view api.TaskReadResult
	second.decode(read, &view)
	if view.CurrentExecution != task.ExecutionID || len(view.ExecutionHistory) != 1 {
		t.Fatalf("restarted task = %+v; want execution %q", view, task.ExecutionID)
	}
}

// readAllEvents reads every committed event id for one project.
func readAllEvents(t *testing.T, environment *runningEnv, project string) map[id.EventID]bool {
	t.Helper()
	result := environment.mustInvoke(projectToken, "event.read", api.EventReadRequest{
		ProjectID: project,
		Page:      &api.Page{Limit: 200},
	}, nil, "")
	var page api.EventReadResult
	environment.decode(result, &page)
	seen := make(map[id.EventID]bool, len(page.Events))
	for _, envelope := range page.Events {
		seen[id.EventID(envelope.ID)] = true
	}
	return seen
}
