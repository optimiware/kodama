//go:build integration

package composition

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
)

// lifecycleFacts is the bounded payload of one projected lifecycle event.
type lifecycleFacts struct {
	ExecutionID   string `json:"execution_id"`
	ActionID      string `json:"action_id"`
	Index         int    `json:"index"`
	Kind          string `json:"kind"`
	PreviousState string `json:"previous_state"`
	State         string `json:"state"`
	CleanupState  string `json:"cleanup_state"`
	Version       uint64 `json:"version"`
}

// TestFirstTaskFlowThroughTheComposedApp proves the bootstrap-to-first-task
// flow: REST tenancy, pipeline.apply, task.create_and_start, Kernel progression
// through the injected fake runtime to terminal success with its atomic
// lifecycle events, the settlement loop marking the task done, cursor-resumed
// event reads, and the authorized subscribe surface.
func TestFirstTaskFlowThroughTheComposedApp(t *testing.T) {
	harness := newHarness(t)
	runtime := newFakeRuntime(modeComplete)
	environment := harness.start(runtime)

	environment.grantEngineer("admin-1", projectID)
	pipelineID := environment.applyPipeline("build")

	started := environment.mustInvoke(projectToken, "task.create_and_start", api.TaskCreateAndStartRequest{
		ProjectID:  projectID,
		Title:      "first task",
		PipelineID: pipelineID,
	}, nil, "task-create-and-start-1")
	var task api.TaskCreateAndStartResult
	environment.decode(started, &task)
	if task.TaskID == "" || task.ExecutionID == "" {
		t.Fatalf("task.create_and_start = %+v", task)
	}

	// The Kernel reconcile loop drives the queued execution through the fake
	// executor to terminal success and complete cleanup; the composition owns
	// the goroutines.
	environment.eventually(10*time.Second, func() bool {
		result, publicError := environment.call(projectToken, "execution.inspect", api.ExecutionInspectRequest{
			ExecutionID: task.ExecutionID,
		}, nil, "")
		if publicError.Code != "" {
			return false
		}
		var inspected api.ExecutionInspectResult
		environment.decode(result, &inspected)
		return inspected.State == "succeeded" && inspected.Cleanup.State == "complete"
	})
	if runtime.startCount() != 1 {
		t.Fatalf("fake runtime starts = %d; want exactly one", runtime.startCount())
	}

	// The settlement loop reports the current execution success to the task
	// capability; task.read then exposes the settled state.
	environment.eventually(10*time.Second, func() bool {
		result, publicError := environment.call(projectToken, "task.read", api.TaskReadRequest{TaskID: task.TaskID}, nil, "")
		if publicError.Code != "" {
			return false
		}
		var view api.TaskReadResult
		environment.decode(result, &view)
		return view.State == "done"
	})
	read := environment.mustInvoke(projectToken, "task.read", api.TaskReadRequest{TaskID: task.TaskID}, nil, "")
	var view api.TaskReadResult
	environment.decode(read, &view)
	if view.State != "done" {
		t.Fatalf("settled task state = %q; want done", view.State)
	}
	if view.CurrentExecution != task.ExecutionID {
		t.Fatalf("task current execution = %q; want %q", view.CurrentExecution, task.ExecutionID)
	}
	if len(view.ExecutionHistory) != 1 || view.ExecutionHistory[0] != task.ExecutionID {
		t.Fatalf("task execution history = %v; want [%s]", view.ExecutionHistory, task.ExecutionID)
	}

	history := environment.mustInvoke(projectToken, "task.history", api.TaskHistoryRequest{TaskID: task.TaskID}, nil, "")
	var entries api.TaskHistoryResult
	environment.decode(history, &entries)
	settled := false
	for _, entry := range entries.Entries {
		if entry.Kind == "execution" && entry.ExecutionID == task.ExecutionID && entry.State == "succeeded" {
			settled = true
		}
	}
	if !settled {
		t.Fatalf("task history has no succeeded execution entry: %+v", entries.Entries)
	}

	// The lifecycle projection commits exactly one queued, started and
	// succeeded event, with the container action's two transitions bracketing
	// them in order.
	all := readAllEventPage(t, environment)
	assertLifecycleSequence(t, all.Events)

	// Cursor resume reconstructs the same ordered identity list with no
	// duplicate.
	fullIDs := eventIDs(all.Events)
	pagedIDs := pagedEventIDs(t, environment, 2)
	if len(pagedIDs) != len(fullIDs) {
		t.Fatalf("paged reads saw %d events; full read saw %d", len(pagedIDs), len(fullIDs))
	}
	for index := range fullIDs {
		if pagedIDs[index] != fullIDs[index] {
			t.Fatalf("paged event %d = %q; want %q", index, pagedIDs[index], fullIDs[index])
		}
	}

	// event.subscribe grants only the caller's scope-qualified subjects and
	// exposes the authenticated human session expiry.
	subscribe := environment.mustInvoke(projectToken, "event.subscribe", api.EventSubscribeRequest{
		ProjectID: projectID,
	}, nil, "")
	var grant api.EventSubscribeResult
	environment.decode(subscribe, &grant)
	if len(grant.Subjects) != 1 || grant.Subjects[0] != "kodama.v1.tenant-1.project-1.events" {
		t.Fatalf("subscribe subjects = %v; want the project-1 subject only", grant.Subjects)
	}
	if grant.Cursor == "" {
		t.Fatal("subscribe grant has no resume cursor")
	}
	if want := harness.clock.Now().Add(24 * time.Hour); !grant.ExpiresAt.Equal(want) {
		t.Fatalf("subscribe expiry = %s; want %s", grant.ExpiresAt, want)
	}

	// A graceful restart over the same store must not re-deliver or duplicate
	// any committed event, and the broker sees each identity at most once.
	// Wait for the outbox to finish publishing before the graceful close so the
	// restart cannot race an in-flight publication mark.
	environment.eventually(10*time.Second, func() bool {
		return len(harness.publisher.publishedIDs()) >= len(fullIDs)
	})
	environment.close()
	restarted := harness.start(newFakeRuntime(modeComplete))
	restartedPage := readAllEventPage(t, restarted)
	if got := eventIDs(restartedPage.Events); !equalStrings(got, fullIDs) {
		t.Fatalf("restart event identities = %v; want %v", got, fullIDs)
	}
	assertUnique(t, harness.publisher.publishedIDs(), "broker publication")
}

// TestOlderSuccessDoesNotSettleANewerCurrentExecution proves the settlement
// loop reports only the task's current execution: a newer current execution
// keeps the task open after an older execution succeeds, and only the newer
// success settles it. Under the handoff gate the replacement is current but
// stays queued, without a workload, until its running predecessor is terminal
// with confirmed stop evidence.
//
// The test does not wait for elapsed time. It holds the settlement loop inside
// one open-task page so that page was snapshotted while the older execution was
// still the task's current execution, lets the newer execution become current
// (queued behind the running predecessor) and the older one succeed, and only
// then releases the page. The settlement pass therefore observes the older
// success against a page that still names the older execution, which is the
// only way the current-execution guard can be exercised. Only after further
// settlement passes have observed the older success is the task state asserted,
// so a delayed scheduler cannot make the assertion vacuous.
func TestOlderSuccessDoesNotSettleANewerCurrentExecution(t *testing.T) {
	harness := newHarness(t)
	runtime := newFakeRuntime(modeStayRunning)
	environment := harness.start(runtime)
	environment.grantEngineer("admin-1", projectID)
	pipelineID := environment.applyPipeline("build")

	// A warm-up task gives the settlement loop a succeeded execution in the
	// project scope, so its open-task scan is already running when the gated
	// page is armed. Without that, scope discovery would begin only after the
	// older execution succeeded, too late to snapshot the older current
	// execution.
	warmup := environment.mustInvoke(projectToken, "task.create_and_start", api.TaskCreateAndStartRequest{
		ProjectID:  projectID,
		Title:      "scope warm-up",
		PipelineID: pipelineID,
	}, nil, "older-success-warmup")
	var warmupTask api.TaskCreateAndStartResult
	environment.decode(warmup, &warmupTask)
	environment.eventually(10*time.Second, func() bool { return runtime.startCount() == 1 })

	created := environment.mustInvoke(projectToken, "task.create_and_start", api.TaskCreateAndStartRequest{
		ProjectID:  projectID,
		Title:      "two executions",
		PipelineID: pipelineID,
	}, nil, "older-success-first")
	var first api.TaskCreateAndStartResult
	environment.decode(created, &first)
	environment.eventually(10*time.Second, func() bool { return runtime.startCount() == 2 })

	read := environment.mustInvoke(projectToken, "task.read", api.TaskReadRequest{TaskID: first.TaskID}, nil, "")
	var view api.TaskReadResult
	environment.decode(read, &view)
	if view.State != "open" {
		t.Fatalf("task state before the second start = %q; want open", view.State)
	}
	if view.CurrentExecution != first.ExecutionID {
		t.Fatalf("task current execution before the second start = %q; want %q", view.CurrentExecution, first.ExecutionID)
	}
	version := view.LifecycleVersion

	// Arm the pass counter and the page gate before the older execution can
	// succeed. The captured page must name the older execution as current.
	probe := harness.settlement
	probe.watchExecution(id.ExecutionID(first.ExecutionID))
	gate := probe.armTaskGate(id.TaskID(first.TaskID))

	// The warm-up succeeds so discovery reaches the project scope and takes the
	// gated open-task page. That page is snapshotted now, while the older
	// execution is still current and running.
	workloads := runtime.startedIDs()
	runtime.release(workloads[0])
	select {
	case <-gate.captured:
	case <-time.After(10 * time.Second):
		t.Fatal("settlement did not capture the open-task page")
	}
	// The held page must be stale relative to the live task: it still names the
	// older execution. Without this the rest of the test could pass against a
	// page that was captured after the newer execution became current.
	if got := gate.taskCurrentExecution(id.TaskID(first.TaskID)); got != first.ExecutionID {
		t.Fatalf("held settlement page current execution = %q; want the older %q", got, first.ExecutionID)
	}

	// The first execution is still running, so the second start commits a
	// queued execution without dispatching it. The handoff gate keeps it
	// queued while its predecessor is non-terminal, even though it is already
	// the task's current execution.
	secondResult := environment.mustInvoke(projectToken, "task.start", api.TaskStartRequest{
		TaskID: first.TaskID,
	}, &version, "older-success-second")
	var second api.TaskStartResult
	environment.decode(secondResult, &second)
	if second.ExecutionID == "" || second.ExecutionID == first.ExecutionID {
		t.Fatalf("second execution = %q; want a new execution", second.ExecutionID)
	}
	inspectSecond := func() (api.ExecutionInspectResult, bool) {
		result, publicError := environment.call(projectToken, "execution.inspect", api.ExecutionInspectRequest{
			ExecutionID: second.ExecutionID,
		}, nil, "")
		if publicError.Code != "" {
			return api.ExecutionInspectResult{}, false
		}
		var inspected api.ExecutionInspectResult
		environment.decode(result, &inspected)
		return inspected, true
	}
	environment.eventually(10*time.Second, func() bool {
		inspected, ok := inspectSecond()
		return ok && inspected.State == "queued"
	})

	// The Kernel scan runs every LoopInterval. Observing the replacement still
	// queued, with its primary action never activated and the start count
	// unchanged, across several scan intervals proves the handoff gate held the
	// workload rather than the scan not having run yet.
	observationDeadline := time.Now().Add(20 * harness.config.LoopInterval)
	for time.Now().Before(observationDeadline) {
		if starts := runtime.startCount(); starts != 2 {
			t.Fatalf("replacement workload dispatched while its predecessor was running: starts = %d; want 2", starts)
		}
		inspected, ok := inspectSecond()
		if !ok {
			t.Fatal("execution.inspect of the replacement was refused")
		}
		if inspected.State != "queued" {
			t.Fatalf("replacement execution state = %q while its predecessor is running; want queued", inspected.State)
		}
		if len(inspected.Actions) != 0 && inspected.Actions[0].ActivatedAt != nil {
			t.Fatalf("replacement action activated at %s while its predecessor was running", inspected.Actions[0].ActivatedAt)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if starts := runtime.startCount(); starts != 2 {
		t.Fatalf("replacement workload dispatched while its predecessor was running: starts = %d; want 2", starts)
	}
	read = environment.mustInvoke(projectToken, "task.read", api.TaskReadRequest{TaskID: first.TaskID}, nil, "")
	var queuedView api.TaskReadResult
	environment.decode(read, &queuedView)
	if queuedView.State != "open" || queuedView.CurrentExecution != second.ExecutionID {
		t.Fatalf("task while the replacement is queued = state %q current %q; want open with the newer %q",
			queuedView.State, queuedView.CurrentExecution, second.ExecutionID)
	}

	// The older execution succeeds while the newer one is the task's current
	// execution, still queued until the older one is terminal with confirmed
	// stop evidence. The held settlement page still names the older execution,
	// so the pass will observe the older success.
	workloads = runtime.startedIDs()
	runtime.release(workloads[1])
	environment.eventually(10*time.Second, func() bool {
		result, publicError := environment.call(projectToken, "execution.inspect", api.ExecutionInspectRequest{
			ExecutionID: first.ExecutionID,
		}, nil, "")
		if publicError.Code != "" {
			return false
		}
		var inspected api.ExecutionInspectResult
		environment.decode(result, &inspected)
		return inspected.State == "succeeded"
	})

	// The predecessor's success carries confirmed stop evidence, so only now
	// may the queued replacement activate and start its workload.
	environment.eventually(10*time.Second, func() bool { return runtime.startCount() == 3 })
	workloads = runtime.startedIDs()

	// Release the held page only after the older execution is durably succeeded,
	// then require observed settlement passes rather than elapsed time. The loop
	// is a single goroutine, so a later discovery pass can only run after the
	// released page was fully observed; every counted pass saw the older success
	// among the succeeded executions and still declined to settle.
	before := probe.passesFor(id.ExecutionID(first.ExecutionID))
	gate.releaseGate()
	environment.eventually(10*time.Second, func() bool {
		return probe.passesFor(id.ExecutionID(first.ExecutionID)) >= before+3
	})

	read = environment.mustInvoke(projectToken, "task.read", api.TaskReadRequest{TaskID: first.TaskID}, nil, "")
	environment.decode(read, &view)
	if view.State != "open" {
		t.Fatalf("an older success settled the task: state = %q, current = %q", view.State, view.CurrentExecution)
	}
	if view.CurrentExecution != second.ExecutionID {
		t.Fatalf("task current execution = %q; want the newer %q", view.CurrentExecution, second.ExecutionID)
	}

	// The newer execution's success is the only one that settles the task.
	runtime.release(workloads[2])
	environment.eventually(10*time.Second, func() bool {
		result, publicError := environment.call(projectToken, "task.read", api.TaskReadRequest{TaskID: first.TaskID}, nil, "")
		if publicError.Code != "" {
			return false
		}
		var current api.TaskReadResult
		environment.decode(result, &current)
		return current.State == "done" && current.CurrentExecution == second.ExecutionID
	})
}

// assertLifecycleSequence pins the projected event order for one successful
// container execution.
func assertLifecycleSequence(t *testing.T, events []api.Event) {
	t.Helper()
	var (
		queued, started, succeeded int
		transitions                []lifecycleFacts
		phase                      []string
	)
	for _, envelope := range events {
		switch envelope.Type {
		case "execution.queued":
			queued++
			phase = append(phase, "queued")
		case "execution.started":
			started++
			phase = append(phase, "started")
		case "execution.succeeded":
			succeeded++
			phase = append(phase, "succeeded")
		case "action.transition":
			var facts lifecycleFacts
			if err := json.Unmarshal(envelope.Data, &facts); err != nil {
				t.Fatalf("decode action.transition facts: %v", err)
			}
			if facts.Kind != "container" || facts.Index != 0 {
				continue
			}
			transitions = append(transitions, facts)
			if facts.PreviousState == "pending" && facts.State == "running" {
				phase = append(phase, "action-running")
			}
			if facts.PreviousState == "running" && facts.State == "succeeded" {
				phase = append(phase, "action-succeeded")
			}
		}
	}
	if queued != 1 || started != 1 || succeeded != 1 {
		t.Fatalf("lifecycle counts queued=%d started=%d succeeded=%d; want one each", queued, started, succeeded)
	}
	if len(transitions) != 2 {
		t.Fatalf("container action transitions = %+v; want exactly two", transitions)
	}
	want := []string{"queued", "started", "action-running", "succeeded", "action-succeeded"}
	if !equalStrings(phase, want) {
		t.Fatalf("lifecycle phase order = %v; want %v", phase, want)
	}
}

// readAllEventPage reads the complete committed event page for project-1.
func readAllEventPage(t *testing.T, environment *runningEnv) api.EventReadResult {
	t.Helper()
	result := environment.mustInvoke(projectToken, "event.read", api.EventReadRequest{
		ProjectID: projectID,
		Page:      &api.Page{Limit: 200},
	}, nil, "")
	var page api.EventReadResult
	environment.decode(result, &page)
	for _, envelope := range page.Events {
		if err := envelope.Validate(); err != nil {
			t.Fatalf("event.read returned an invalid envelope: %v", err)
		}
	}
	return page
}

// pagedEventIDs walks the event read with a small page size and returns the
// ordered identity list, refusing any duplicate or repeated cursor.
func pagedEventIDs(t *testing.T, environment *runningEnv, pageSize int) []string {
	t.Helper()
	var (
		identities []string
		cursor     string
		seen       = make(map[string]bool)
		seenCursor = make(map[string]bool)
	)
	for {
		request := api.EventReadRequest{ProjectID: projectID, Page: &api.Page{Limit: pageSize}}
		if cursor != "" {
			request.Page.Cursor = cursor
		}
		result := environment.mustInvoke(projectToken, "event.read", request, nil, "")
		var page api.EventReadResult
		environment.decode(result, &page)
		for _, envelope := range page.Events {
			if seen[envelope.ID] {
				t.Fatalf("paged event read repeated identity %q", envelope.ID)
			}
			seen[envelope.ID] = true
			identities = append(identities, envelope.ID)
		}
		if !page.HasMore {
			return identities
		}
		if page.Next == "" || seenCursor[page.Next] {
			t.Fatalf("event read did not advance its cursor (next=%q)", page.Next)
		}
		seenCursor[page.Next] = true
		cursor = page.Next
	}
}

func eventIDs(events []api.Event) []string {
	identities := make([]string, len(events))
	for index, envelope := range events {
		identities[index] = envelope.ID
	}
	return identities
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func assertUnique(t *testing.T, identities []id.EventID, what string) {
	t.Helper()
	seen := make(map[id.EventID]bool, len(identities))
	for _, identity := range identities {
		if seen[identity] {
			t.Fatalf("%s repeated identity %q", what, identity)
		}
		seen[identity] = true
	}
}
