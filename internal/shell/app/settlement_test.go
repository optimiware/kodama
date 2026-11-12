package app

import (
	"context"
	"crypto/rand"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
)

// controlledRuntime observes workloads by execution identity. An execution
// that has not been released stays running, so a test can decide when (and
// with which exit code) an execution reaches its terminal state. It also
// records which execution every accepted workload start belongs to, because
// the execution's activated state is committed before its workload is started
// and a start count alone cannot attribute a late start.
type controlledRuntime struct {
	*fakeRuntime
	mu        sync.Mutex
	outcomes  map[id.ExecutionID]int
	startedBy []id.ExecutionID
}

var _ kernelengine.Runtime = (*controlledRuntime)(nil)

func newControlledRuntime() *controlledRuntime {
	return &controlledRuntime{
		fakeRuntime: &fakeRuntime{},
		outcomes:    make(map[id.ExecutionID]int),
	}
}

func (runtime *controlledRuntime) Start(ctx context.Context, request kernelengine.StartRequest) (kernelengine.StartResult, error) {
	result, err := runtime.fakeRuntime.Start(ctx, request)
	runtime.mu.Lock()
	runtime.startedBy = append(runtime.startedBy, request.Identity.ExecutionID)
	runtime.mu.Unlock()
	return result, err
}

func (runtime *controlledRuntime) Observe(_ context.Context, request kernelengine.ObserveRequest) (kernel.Observation, error) {
	runtime.mu.Lock()
	code, released := runtime.outcomes[request.Identity.ExecutionID]
	runtime.mu.Unlock()
	if !released {
		return kernel.RunningObservation(), nil
	}
	return kernel.ExitedObservation(code, kernel.ConfirmedStop("process exited")), nil
}

func (runtime *controlledRuntime) release(executionID id.ExecutionID, code int) {
	runtime.mu.Lock()
	runtime.outcomes[executionID] = code
	runtime.mu.Unlock()
}

// startedExecutions returns a stable copy of the execution identities whose
// workloads the runtime was asked to start, in start order.
func (runtime *controlledRuntime) startedExecutions() []id.ExecutionID {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return append([]id.ExecutionID(nil), runtime.startedBy...)
}

// startedFor counts runtime starts for one execution.
func (runtime *controlledRuntime) startedFor(executionID id.ExecutionID) int {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	count := 0
	for _, started := range runtime.startedBy {
		if started == executionID {
			count++
		}
	}
	return count
}

// applySettlementPipeline admits the smoke pipeline used by every settlement
// scenario.
func applySettlementPipeline(t *testing.T, handler http.Handler, token string) api.PipelineApplyResult {
	t.Helper()
	return applyProjectPipeline(t, handler, token, "project-1", []byte(smokePipeline), "settlement-pipeline")
}

// createAndStartSettlementTask creates and starts one task for the settlement
// scenarios.
func createAndStartSettlementTask(t *testing.T, handler http.Handler, token, pipelineID, key string) api.TaskCreateAndStartResult {
	t.Helper()
	recorder, result, publicError := restCall(t, handler, http.MethodPost,
		"/api/v1/projects/project-1/tasks/create-and-start", token,
		api.TaskCreateAndStartRequest{Title: "settlement", PipelineID: pipelineID}, nil, key)
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("task.create_and_start status=%d error=%+v", recorder.Code, publicError)
	}
	return decodeBody[api.TaskCreateAndStartResult](t, result)
}

func readSettlementTask(t *testing.T, handler http.Handler, token, taskID string) api.TaskReadResult {
	t.Helper()
	recorder, result, publicError := restCall(t, handler, http.MethodGet,
		"/api/v1/tasks/"+taskID, token, nil, nil, "")
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("task.read status=%d error=%+v", recorder.Code, publicError)
	}
	return decodeBody[api.TaskReadResult](t, result)
}

func readSettlementHistory(t *testing.T, handler http.Handler, token, taskID string) api.TaskHistoryResult {
	t.Helper()
	recorder, result, publicError := restCall(t, handler, http.MethodGet,
		"/api/v1/tasks/"+taskID+"/history", token, nil, nil, "")
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("task.history status=%d error=%+v", recorder.Code, publicError)
	}
	return decodeBody[api.TaskHistoryResult](t, result)
}

// settlementWait bounds each settlement observation below. A wait names the
// durable condition it needs; when the bound expires it reports the last
// observed state instead of the caller's line alone.
const settlementWait = 5 * time.Second

// awaitExecutionState waits for one execution to reach want.
func awaitExecutionState(t *testing.T, handler http.Handler, token, executionID, want, role string) api.ExecutionInspectResult {
	t.Helper()
	var last api.ExecutionInspectResult
	deadline := time.Now().Add(settlementWait)
	for {
		last = inspectExecution(t, handler, token, executionID)
		if last.State == want {
			return last
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s execution %s did not reach state %q within %s: state=%q version=%d current_action=%d actions=%+v cleanup=%+v failure=%+v",
				role, executionID, want, settlementWait, last.State, last.Version, last.CurrentAction,
				last.Actions, last.Cleanup, last.Failure)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// awaitTaskCurrentExecution waits for the task to name want as its current
// execution.
func awaitTaskCurrentExecution(t *testing.T, handler http.Handler, token, taskID, want string) api.TaskReadResult {
	t.Helper()
	var last api.TaskReadResult
	deadline := time.Now().Add(settlementWait)
	for {
		last = readSettlementTask(t, handler, token, taskID)
		if last.CurrentExecution == want {
			return last
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s did not make execution %s current within %s: state=%q lifecycle_version=%d current_execution=%q history=%v",
				taskID, want, settlementWait, last.State, last.LifecycleVersion, last.CurrentExecution, last.ExecutionHistory)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// awaitTaskState waits for the task to reach want.
func awaitTaskState(t *testing.T, handler http.Handler, token, taskID, want, reason string) api.TaskReadResult {
	t.Helper()
	var last api.TaskReadResult
	deadline := time.Now().Add(settlementWait)
	for {
		last = readSettlementTask(t, handler, token, taskID)
		if last.State == want {
			return last
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: task %s did not reach state %q within %s: state=%q lifecycle_version=%d current_execution=%q history=%v",
				reason, taskID, want, settlementWait, last.State, last.LifecycleVersion,
				last.CurrentExecution, last.ExecutionHistory)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// awaitStartedExecution waits until the runtime was asked to start the named
// execution's workload. An execution reaches "running" before its workload is
// started, so this is the only deterministic baseline for "the predecessor is
// actually running its workload".
func awaitStartedExecution(t *testing.T, adapter *controlledRuntime, executionID id.ExecutionID, role string) {
	t.Helper()
	deadline := time.Now().Add(settlementWait)
	for {
		if adapter.startedFor(executionID) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s execution %s never started a workload within %s; the runtime started %v",
				role, executionID, settlementWait, adapter.startedExecutions())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestSettlementCompletesTaskWhenCurrentExecutionSucceeds proves the composed
// app settles a task after its current execution reaches succeeded, exposes the
// completed state through task.read/history, and does not repeat the settlement
// commit on later scans.
func TestSettlementCompletesTaskWhenCurrentExecutionSucceeds(t *testing.T) {
	environment := newTestEnvironment(t)
	app := environment.startApp(t)
	handler := app.Handler()
	token := "dev-project-token"
	grantProjectEngineer(t, handler, "dev-admin-token")
	pipeline := applySettlementPipeline(t, handler, token)
	started := createAndStartSettlementTask(t, handler, token, pipeline.PipelineID, "settlement-success")

	eventually(t, 5*time.Second, func() bool {
		return inspectExecution(t, handler, token, started.ExecutionID).State == "succeeded"
	})
	eventually(t, 5*time.Second, func() bool {
		return readSettlementTask(t, handler, token, started.TaskID).State == "done"
	})

	task := readSettlementTask(t, handler, token, started.TaskID)
	if task.CurrentExecution != started.ExecutionID {
		t.Fatalf("task current execution = %q; want %q", task.CurrentExecution, started.ExecutionID)
	}
	history := readSettlementHistory(t, handler, token, started.TaskID)
	found := false
	for _, entry := range history.Entries {
		if entry.Kind == "execution" && entry.ExecutionID == started.ExecutionID {
			found = true
			if entry.State != "succeeded" {
				t.Fatalf("history execution state = %q; want succeeded", entry.State)
			}
		}
	}
	if !found {
		t.Fatalf("history has no execution entry for %q: %+v", started.ExecutionID, history.Entries)
	}

	// Repeated settlement scans must not commit a second transition.
	settledVersion := task.LifecycleVersion
	time.Sleep(10 * environment.config.LoopInterval)
	again := readSettlementTask(t, handler, token, started.TaskID)
	if again.State != "done" || again.LifecycleVersion != settledVersion {
		t.Fatalf("repeated settlement changed the task: state=%q version=%d; want done/%d",
			again.State, again.LifecycleVersion, settledVersion)
	}

	closeApp(t, app)
	if logged := environment.logs.String(); containsAll(logged, "settlement", "retrying") {
		t.Fatalf("the settlement loop reported a fault in the success path: %s", logged)
	}
}

// TestSettlementIgnoresOlderExecutionSuccess proves an older execution that
// succeeds after a newer current execution exists does not settle the task, and
// that the newer current execution's success still does.
func TestSettlementIgnoresOlderExecutionSuccess(t *testing.T) {
	runtimeAdapter := newControlledRuntime()
	environment := newTestEnvironment(t)
	environment.config.Adapters.Runtime = runtimeAdapter
	app := environment.startApp(t)
	handler := app.Handler()
	token := "dev-project-token"
	grantProjectEngineer(t, handler, "dev-admin-token")
	pipeline := applySettlementPipeline(t, handler, token)
	started := createAndStartSettlementTask(t, handler, token, pipeline.PipelineID, "settlement-older")

	olderID := id.ExecutionID(started.ExecutionID)

	// Deterministic baseline: an execution is committed "running" before its
	// workload is started, so wait until the predecessor's own workload has
	// actually been started. Without this the subsequent start-count snapshot
	// can precede the predecessor's own late Start and misattribute it to the
	// replacement.
	awaitExecutionState(t, handler, token, started.ExecutionID, "running", "predecessor")
	awaitStartedExecution(t, runtimeAdapter, olderID, "predecessor")

	// The two admissions carry distinct creation instants so this test stays
	// on the ordinary ordering path; the same-instant order is covered by the
	// execution store's barrier tests.
	environment.clock.Advance(time.Millisecond)

	// A second start replaces the current execution while the first execution
	// is still running. It is admitted as the new current execution even though
	// it cannot dispatch until the predecessor is confirmed stopped.
	recorder, result, publicError := restCall(t, handler, http.MethodPost,
		"/api/v1/tasks/"+started.TaskID+"/start", token, api.TaskStartRequest{},
		&started.LifecycleVersion, "settlement-start-newer")
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("second task.start status=%d error=%+v", recorder.Code, publicError)
	}
	newer := decodeBody[api.TaskStartResult](t, result)
	newerID := id.ExecutionID(newer.ExecutionID)
	awaitTaskCurrentExecution(t, handler, token, started.TaskID, newer.ExecutionID)

	// The predecessor is still running and therefore has no confirmed stop
	// evidence, so the admitted replacement must stay queued: it is neither
	// activated nor dispatched, and the runtime is never asked to start its
	// workload.
	baselineStarts := runtimeAdapter.startedExecutions()
	time.Sleep(10 * environment.config.LoopInterval)
	if state := inspectExecution(t, handler, token, started.ExecutionID).State; state != "running" {
		t.Fatalf("predecessor execution state = %q; want running", state)
	}
	if state := inspectExecution(t, handler, token, newer.ExecutionID).State; state != "queued" {
		t.Fatalf("replacement execution state = %q while its predecessor is running; want queued", state)
	}
	if starts := runtimeAdapter.startedExecutions(); !slices.Equal(starts, baselineStarts) {
		t.Fatalf("workloads were dispatched before the predecessor was confirmed stopped: started executions %v -> %v (predecessor %s, replacement %s; replacement starts=%d)",
			baselineStarts, starts, olderID, newerID, runtimeAdapter.startedFor(newerID))
	}
	if starts := runtimeAdapter.startedFor(newerID); starts != 0 {
		t.Fatalf("the replacement execution %s started %d workloads before its predecessor was confirmed stopped",
			newerID, starts)
	}
	if starts := runtimeAdapter.startedFor(olderID); starts != 1 {
		t.Fatalf("the predecessor execution %s started %d workloads; want exactly one", olderID, starts)
	}

	// The older execution succeeds late. It is no longer current, so the task
	// must remain open across many settlement scans, and only now may the
	// queued replacement activate and reach running.
	runtimeAdapter.release(olderID, 0)
	awaitExecutionState(t, handler, token, started.ExecutionID, "succeeded", "predecessor after release")
	awaitExecutionState(t, handler, token, newer.ExecutionID, "running", "replacement after predecessor stop")
	awaitStartedExecution(t, runtimeAdapter, newerID, "replacement")
	time.Sleep(10 * environment.config.LoopInterval)
	task := readSettlementTask(t, handler, token, started.TaskID)
	if task.State != "open" {
		t.Fatalf("older execution success settled the task: state=%q; want open", task.State)
	}
	if task.CurrentExecution != newer.ExecutionID {
		t.Fatalf("task current execution = %q; want the newer %q", task.CurrentExecution, newer.ExecutionID)
	}

	// The newer current execution succeeding settles the task.
	runtimeAdapter.release(newerID, 0)
	awaitTaskState(t, handler, token, started.TaskID, "done", "the newer current execution success did not settle the task")
}

// TestSettlementLeavesTaskOpenOnFailedExecution proves a failed current
// execution does not write a task transition: the task stays open with a
// stable lifecycle version and the settlement loop does not report failure to
// the progress port (only a current success can complete a task).
func TestSettlementLeavesTaskOpenOnFailedExecution(t *testing.T) {
	runtimeAdapter := newControlledRuntime()
	environment := newTestEnvironment(t)
	environment.config.Adapters.Runtime = runtimeAdapter
	app := environment.startApp(t)
	handler := app.Handler()
	token := "dev-project-token"
	grantProjectEngineer(t, handler, "dev-admin-token")
	pipeline := applySettlementPipeline(t, handler, token)
	started := createAndStartSettlementTask(t, handler, token, pipeline.PipelineID, "settlement-failed")

	eventually(t, 5*time.Second, func() bool {
		return inspectExecution(t, handler, token, started.ExecutionID).State == "running"
	})
	runtimeAdapter.release(id.ExecutionID(started.ExecutionID), 1)
	eventually(t, 5*time.Second, func() bool {
		return inspectExecution(t, handler, token, started.ExecutionID).State == "failed"
	})
	time.Sleep(10 * environment.config.LoopInterval)
	task := readSettlementTask(t, handler, token, started.TaskID)
	if task.State != "open" {
		t.Fatalf("failed execution changed the task state to %q; want open", task.State)
	}
	if task.LifecycleVersion != started.LifecycleVersion {
		t.Fatalf("failed execution changed the lifecycle version %d -> %d",
			started.LifecycleVersion, task.LifecycleVersion)
	}
	closeApp(t, app)
}

// TestSettlementSkipsCancelledExecution proves a conversation handoff's
// cancelled predecessor does not fault the settlement loop and does not settle
// the waiting task.
func TestSettlementSkipsCancelledExecution(t *testing.T) {
	runtimeAdapter := newControlledRuntime()
	environment := newTestEnvironment(t)
	environment.config.Adapters.Runtime = runtimeAdapter
	app := environment.startApp(t)
	handler := app.Handler()
	token := "dev-project-token"
	grantProjectEngineer(t, handler, "dev-admin-token")
	pipeline := applySettlementPipeline(t, handler, token)
	started := createAndStartSettlementTask(t, handler, token, pipeline.PipelineID, "settlement-cancelled")

	eventually(t, 5*time.Second, func() bool {
		return inspectExecution(t, handler, token, started.ExecutionID).State == "running"
	})
	// Wait for the engine's first running observation to be persisted and the
	// execution version to settle so the guarded ask does not race it.
	stable := false
	for attempt := 0; attempt < 100 && !stable; attempt++ {
		before := inspectExecution(t, handler, token, started.ExecutionID).Version
		time.Sleep(5 * environment.config.LoopInterval)
		stable = inspectExecution(t, handler, token, started.ExecutionID).Version == before
	}
	if !stable {
		t.Fatal("the running execution version never stabilized")
	}
	recorder, _, publicError := restCall(t, handler, http.MethodPost,
		"/api/v1/tasks/"+started.TaskID+"/ask", token,
		api.TaskAskRequest{Question: "need an operator decision", Assignment: "role/operator"},
		&started.LifecycleVersion, "settlement-ask")
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("task.ask status=%d error=%+v", recorder.Code, publicError)
	}
	eventually(t, 5*time.Second, func() bool {
		return inspectExecution(t, handler, token, started.ExecutionID).State == "cancelled"
	})
	time.Sleep(10 * environment.config.LoopInterval)
	task := readSettlementTask(t, handler, token, started.TaskID)
	if task.State != "waiting" {
		t.Fatalf("cancelled execution changed the task state to %q; want waiting", task.State)
	}
	closeApp(t, app)
	if logged := environment.logs.String(); containsAll(logged, "settlement", "retrying") {
		t.Fatalf("a cancelled execution faulted the settlement loop: %s", logged)
	}
}

// failingSettlementScanner fails a configured number of leading scans and then
// delegates to the composed controller seam.
type failingSettlementScanner struct {
	inner    settlementScanner
	mu       sync.Mutex
	failures int
	calls    int
}

func (scanner *failingSettlementScanner) Scan(ctx context.Context, scan store.ControllerScan) (store.QueryPage, error) {
	scanner.mu.Lock()
	scanner.calls++
	fail := scanner.failures > 0
	if fail {
		scanner.failures--
	}
	scanner.mu.Unlock()
	if fail {
		return store.QueryPage{}, fault.New(fault.DependencyUnavailable, "transient settlement scan failure")
	}
	return scanner.inner.Scan(ctx, scan)
}

// TestSettlementLoopResumesAfterTransientScanFailure proves a failing
// discovery pass is logged once per pause and the supervised loop resumes, so a
// later terminal execution still settles its task.
func TestSettlementLoopResumesAfterTransientScanFailure(t *testing.T) {
	environment := newTestEnvironment(t)
	environment.config.LoopInterval = 10 * time.Millisecond
	app, err := New(context.Background(), environment.config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { closeApp(t, app) })
	if app.settlement == nil {
		t.Fatal("the composition did not wire the task settlement loop")
	}
	app.settlement.scanner = &failingSettlementScanner{inner: app.settlement.scanner, failures: 1}
	if err := app.start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	handler := app.Handler()
	token := "dev-project-token"
	grantProjectEngineer(t, handler, "dev-admin-token")
	pipeline := applySettlementPipeline(t, handler, token)
	started := createAndStartSettlementTask(t, handler, token, pipeline.PipelineID, "settlement-scan-retry")
	eventually(t, 5*time.Second, func() bool {
		return readSettlementTask(t, handler, token, started.TaskID).State == "done"
	})
	closeApp(t, app)
	logged := environment.logs.String()
	if !containsAll(logged, "settlement", "retrying") {
		t.Fatalf("the settlement scan failure was not logged for retry: %s", logged)
	}
}

// hidingSettlementScanner observes no terminal execution, simulating a crash
// before the settlement consumer ever discovered the persisted outcome.
type hidingSettlementScanner struct{}

func (hidingSettlementScanner) Scan(context.Context, store.ControllerScan) (store.QueryPage, error) {
	return store.QueryPage{}, nil
}

// TestSettlementRestartRecoversPersistedSuccess proves settlement is crash-safe:
// the outcome is persisted with the execution, so a new app over the same store
// discovers the still-unsettled succeeded execution and completes the task.
func TestSettlementRestartRecoversPersistedSuccess(t *testing.T) {
	environment := newTestEnvironment(t)
	shared := memory.NewWithClock(environment.clock.Now)
	provider, err := keys.NewEphemeral(rand.Reader)
	if err != nil {
		t.Fatalf("keys.NewEphemeral: %v", err)
	}
	environment.config.Adapters = &Adapters{
		Store:       shared,
		Scanner:     shared,
		Controller:  shared.Controller(),
		Outbox:      shared,
		Credentials: shared,
		Runtime:     environment.runtime,
		Clock:       environment.clock,
		Keys:        provider,
	}

	first, err := New(context.Background(), environment.config)
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	if first.settlement == nil {
		t.Fatal("the composition did not wire the task settlement loop")
	}
	first.settlement.scanner = hidingSettlementScanner{}
	if err := first.start(context.Background()); err != nil {
		t.Fatalf("first start: %v", err)
	}
	token := "dev-project-token"
	grantProjectEngineer(t, first.Handler(), "dev-admin-token")
	pipeline := applySettlementPipeline(t, first.Handler(), token)
	started := createAndStartSettlementTask(t, first.Handler(), token, pipeline.PipelineID, "settlement-restart")
	eventually(t, 5*time.Second, func() bool {
		return inspectExecution(t, first.Handler(), token, started.ExecutionID).State == "succeeded"
	})
	time.Sleep(10 * environment.config.LoopInterval)
	if state := readSettlementTask(t, first.Handler(), token, started.TaskID).State; state != "open" {
		t.Fatalf("hidden settlement unexpectedly changed the task to %q", state)
	}
	closeApp(t, first)

	second, err := New(context.Background(), environment.config)
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	if err := second.start(context.Background()); err != nil {
		t.Fatalf("second start: %v", err)
	}
	t.Cleanup(func() { closeApp(t, second) })
	eventually(t, 5*time.Second, func() bool {
		return readSettlementTask(t, second.Handler(), token, started.TaskID).State == "done"
	})
	closeApp(t, second)
}

// countingSettlementScanner counts discovery passes.
type countingSettlementScanner struct {
	inner settlementScanner
	mu    sync.Mutex
	calls int
}

func (scanner *countingSettlementScanner) Scan(ctx context.Context, scan store.ControllerScan) (store.QueryPage, error) {
	scanner.mu.Lock()
	scanner.calls++
	scanner.mu.Unlock()
	return scanner.inner.Scan(ctx, scan)
}

func (scanner *countingSettlementScanner) count() int {
	scanner.mu.Lock()
	defer scanner.mu.Unlock()
	return scanner.calls
}

// TestSettlementLoopIsJoinedOnClose proves the app owns the settlement loop and
// Close joins it with the other background loops.
func TestSettlementLoopIsJoinedOnClose(t *testing.T) {
	baseline := runtime.NumGoroutine()
	environment := newTestEnvironment(t)
	app, err := New(context.Background(), environment.config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if app.settlement == nil {
		t.Fatal("the composition did not wire the task settlement loop")
	}
	scanner := &countingSettlementScanner{inner: app.settlement.scanner}
	app.settlement.scanner = scanner
	if err := app.start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	eventually(t, 5*time.Second, func() bool { return scanner.count() > 0 })
	closeApp(t, app)
	eventually(t, 5*time.Second, func() bool {
		return runtime.NumGoroutine() <= baseline+1
	})
}

// containsAll reports whether the text contains every marker.
func containsAll(text string, markers ...string) bool {
	for _, marker := range markers {
		if !strings.Contains(text, marker) {
			return false
		}
	}
	return true
}
