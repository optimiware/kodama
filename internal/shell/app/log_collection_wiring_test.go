// Composed retained-log collection: the shared wireApp path.
//
// This is the wiring counterpart of log_collection_test.go. It composes the
// in-memory test profile (memory store, in-process broker, ephemeral keys, fake
// runtime) with the real file-backed retained sink over a temp directory. It
// proves the composed execution.logs.read operation returns the lines that were
// collected before the fake container was removed, that a failed collection
// keeps cleanup pending and retries instead of removing first, and that the
// observable order is collect -> append -> remove -> release.
//
// Production and any other profile: wireApp is shared, so production composes
// the same decorator with environment.logSink and environment.controller. A
// production composition unit test would need the real PostgreSQL/NATS/KMS
// adapters and is out of scope here; this test exercises the shared wireApp
// path with ProfileTest.
package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/id"
	kernelengine "github.com/optimiware/kodema/internal/kernel/engine"
	shelllog "github.com/optimiware/kodema/internal/shell/log"
)

// logReleaseWorkspace records the workspace release that must follow the
// workload removal and delegates the rest of the workspace port to the shared
// fake runtime.
type logReleaseWorkspace struct {
	*fakeRuntime
	order *collectionOrder

	mu           sync.Mutex
	releaseCalls int
}

func (workspace *logReleaseWorkspace) Release(ctx context.Context, workspaceID id.WorkspaceID) error {
	workspace.order.record("release")
	workspace.mu.Lock()
	workspace.releaseCalls++
	workspace.mu.Unlock()
	return workspace.fakeRuntime.Release(ctx, workspaceID)
}

func (workspace *logReleaseWorkspace) releaseCount() int {
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	return workspace.releaseCalls
}

const composedLogOutput = "composed container output\n"

// composedLogFixture is one fully composed in-memory test profile with a fake
// runtime that serves composedLogOutput and a real file sink over a temp dir.
type composedLogFixture struct {
	app       *App
	handler   http.Handler
	order     *collectionOrder
	runtime   *logPageRuntime
	workspace *logReleaseWorkspace
	sink      *recordingSink
	clock     *fakeClock
}

func newComposedLogFixture(t *testing.T, logFailures int) *composedLogFixture {
	t.Helper()
	config := fullTestProfileConfig(t)
	clock, ok := config.Adapters.Clock.(*fakeClock)
	if !ok {
		t.Fatalf("test profile clock is %T; want *fakeClock", config.Adapters.Clock)
	}
	order := &collectionOrder{}
	fileSink, err := shelllog.NewFileSink(shelllog.SinkConfig{Root: t.TempDir(), Clock: clock})
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	sink := &recordingSink{inner: fileSink, order: order}
	runtime := &logPageRuntime{
		fakeRuntime: &fakeRuntime{},
		order:       order,
		logFailures: logFailures,
		pages: map[int64]kernelengine.LogBatch{
			0: {
				Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: 0, Sequence: 1, Data: []byte(composedLogOutput)}},
				NextOffset: int64(len(composedLogOutput)),
			},
			int64(len(composedLogOutput)): {NextOffset: int64(len(composedLogOutput))},
		},
	}
	workspace := &logReleaseWorkspace{fakeRuntime: runtime.fakeRuntime, order: order}
	config.Adapters.Runtime = runtime
	config.Adapters.Workspace = workspace
	config.Adapters.LogSink = sink

	environment := &testEnvironment{clock: clock, runtime: runtime.fakeRuntime, logs: &bytes.Buffer{}, config: config}
	app := environment.startApp(t)
	handler := app.Handler()
	grantProjectEngineer(t, handler, "dev-admin-token")
	return &composedLogFixture{
		app:       app,
		handler:   handler,
		order:     order,
		runtime:   runtime,
		workspace: workspace,
		sink:      sink,
		clock:     clock,
	}
}

// runExecution applies the smoke pipeline, starts one task and waits until the
// execution succeeded and its cleanup is complete.
func (fixture *composedLogFixture) runExecution(t *testing.T) (string, string, api.ExecutionInspectResult) {
	t.Helper()
	handler := fixture.handler
	projectToken := "dev-project-token"
	recorder, result, publicError := restCall(t, handler, http.MethodPut,
		"/api/v1/projects/project-1/pipelines/build", projectToken,
		api.PipelineApplyRequest{Name: "build", YAML: []byte(smokePipeline)}, nil, "log-pipeline-1")
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("pipeline.apply status=%d error=%+v", recorder.Code, publicError)
	}
	applied := decodeBody[api.PipelineApplyResult](t, result)
	recorder, result, publicError = restCall(t, handler, http.MethodPost,
		"/api/v1/projects/project-1/tasks/create-and-start", projectToken,
		api.TaskCreateAndStartRequest{Title: "logs", PipelineID: applied.PipelineID}, nil, "log-task-1")
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("task.create_and_start status=%d error=%+v", recorder.Code, publicError)
	}
	started := decodeBody[api.TaskCreateAndStartResult](t, result)

	// The test profile freezes the fake clock while the recovery loop paces
	// itself on real time. A failed collection persists
	// next_retry_at = commit.At + the retry backoff, so the retry is due only
	// once the fixture clock advances past it. Advance a fixed step only while
	// such a retry is pending, so preparation and credential issuance still see
	// a stable clock, and bound the wait by the same real deadline as every
	// other wait in this package; one failed attempt backs off 1s, far inside
	// that bound.
	const settleTimeout = 5 * time.Second
	const retryClockStep = 100 * time.Millisecond
	var inspected api.ExecutionInspectResult
	deadline := time.Now().Add(settleTimeout)
	for time.Now().Before(deadline) {
		_, inspectedResult, _ := restCall(t, handler, http.MethodGet,
			"/api/v1/executions/"+started.ExecutionID, projectToken, nil, nil, "")
		if err := json.Unmarshal(inspectedResult.Body, &inspected); err != nil {
			time.Sleep(2 * time.Millisecond)
			continue
		}
		if inspected.State == "succeeded" && inspected.Cleanup.State == "complete" {
			break
		}
		if inspected.Cleanup.State == "pending" && inspected.Cleanup.Attempts > 0 {
			fixture.clock.Advance(retryClockStep)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if inspected.State != "succeeded" || inspected.Cleanup.State != "complete" {
		t.Fatalf("execution did not settle within %s: state=%q failure=%+v cleanup=%+v", settleTimeout, inspected.State, inspected.Failure, inspected.Cleanup)
	}
	if len(inspected.Actions) == 0 || inspected.Actions[0].ActionID == "" {
		t.Fatalf("execution inspect returned no action: %+v", inspected)
	}
	return started.ExecutionID, inspected.Actions[0].ActionID, inspected
}

func (fixture *composedLogFixture) readStream(t *testing.T, executionID, actionID, stream string) string {
	t.Helper()
	recorder, result, publicError := restCall(t, fixture.handler, http.MethodGet,
		"/api/v1/executions/"+executionID+"/logs", "dev-project-token",
		api.ExecutionLogsReadRequest{ActionID: actionID, Stream: stream}, nil, "")
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("execution.logs.read (%s) status=%d error=%+v", stream, recorder.Code, publicError)
	}
	page := decodeBody[api.ExecutionLogsReadResult](t, result)
	var collected strings.Builder
	for _, chunk := range page.Chunks {
		collected.Write(chunk.Data)
	}
	return collected.String()
}

func (fixture *composedLogFixture) readStdout(t *testing.T, executionID, actionID string) string {
	t.Helper()
	return fixture.readStream(t, executionID, actionID, "stdout")
}

func TestComposedLogsReadAfterContainerRemoval(t *testing.T) {
	fixture := newComposedLogFixture(t, 0)
	executionID, actionID, inspected := fixture.runExecution(t)

	// The container is already removed and the workspace released at this
	// point; the retained read must still return the collected lines.
	collected := fixture.readStdout(t, executionID, actionID)
	if !strings.Contains(collected, strings.TrimSpace(composedLogOutput)) {
		t.Fatalf("execution.logs.read returned %q; want the collected container output", collected)
	}
	if got := fixture.runtime.removeCount(); got != 1 {
		t.Fatalf("runtime Remove calls = %d; want exactly 1", got)
	}
	if got := fixture.workspace.releaseCount(); got != 1 {
		t.Fatalf("workspace Release calls = %d; want exactly 1", got)
	}
	if inspected.Cleanup.Attempts != 0 {
		t.Fatalf("cleanup attempts = %d; want 0 for a first-pass collection", inspected.Cleanup.Attempts)
	}

	events := fixture.order.snapshot()
	firstLog := fixture.order.first("logs")
	lastAppend := fixture.order.last("append")
	firstRemove := fixture.order.first("remove")
	firstRelease := fixture.order.first("release")
	if firstLog < 0 || lastAppend < 0 || firstRemove < 0 || firstRelease < 0 {
		t.Fatalf("effect order %v is missing a collect/append/remove/release event", events)
	}
	if firstLog >= firstRemove {
		t.Fatalf("effect order %v read logs after removing the container", events)
	}
	if lastAppend >= firstRemove {
		t.Fatalf("effect order %v appended after removing the container", events)
	}
	if firstRemove >= firstRelease {
		t.Fatalf("effect order %v released the workspace before removing the container", events)
	}
	if got := fixture.order.count("remove"); got != 1 {
		t.Fatalf("remove events = %d; want 1 in %v", got, events)
	}
	for index, scope := range fixture.sink.appendedScopes() {
		if scope.TenantID != "tenant-1" || scope.ProjectID != "project-1" {
			t.Fatalf("append %d scope = %+v; want the owning tenant-1/project-1 scope", index, scope)
		}
	}
}

func TestComposedLogCollectionRetriesBeforeRemoval(t *testing.T) {
	// Inject two consecutive log read failures: the first fails the post-exit
	// drain on Observe (which preserves exit settlement and defers to Remove),
	// and the second fails the first Remove attempt so terminal cleanup records
	// a pending attempt and retries before deleting the workload.
	fixture := newComposedLogFixture(t, 2)
	executionID, actionID, inspected := fixture.runExecution(t)

	collected := fixture.readStdout(t, executionID, actionID)
	if strings.Count(collected, strings.TrimSpace(composedLogOutput)) != 1 {
		t.Fatalf("execution.logs.read returned %q; want exactly one copy of the collected output", collected)
	}
	if got := fixture.runtime.removeCount(); got != 1 {
		t.Fatalf("runtime Remove calls = %d; a failed collection must retry before removing exactly once", got)
	}
	if got := len(fixture.runtime.logCallOffsets()); got < 2 {
		t.Fatalf("log read calls = %d; want a retry after the first collection failure", got)
	}
	if inspected.Cleanup.Attempts < 1 {
		t.Fatalf("cleanup attempts = %d; want the failed collection attempt to be retained", inspected.Cleanup.Attempts)
	}
	if got := fixture.workspace.releaseCount(); got != 1 {
		t.Fatalf("workspace Release calls = %d; want exactly 1 after the successful retry", got)
	}

	events := fixture.order.snapshot()
	lastAppend := fixture.order.last("append")
	firstRemove := fixture.order.first("remove")
	firstRelease := fixture.order.first("release")
	if lastAppend < 0 || firstRemove < 0 || firstRelease < 0 {
		t.Fatalf("effect order %v is missing an append/remove/release event", events)
	}
	if lastAppend >= firstRemove {
		t.Fatalf("effect order %v appended after removing the container", events)
	}
	if firstRemove >= firstRelease {
		t.Fatalf("effect order %v released the workspace before removing the container", events)
	}
}

func TestComposedLogsReadRefusesUnknownExecutionWithNotFound(t *testing.T) {
	fixture := newComposedLogFixture(t, 0)
	executionID, actionID, _ := fixture.runExecution(t)

	recorder, _, publicError := restCall(t, fixture.handler, http.MethodGet,
		"/api/v1/executions/execution-missing/logs", "dev-project-token",
		api.ExecutionLogsReadRequest{ActionID: actionID, Stream: "stdout"}, nil, "")
	if recorder.Code != http.StatusNotFound || publicError.Code != api.CodeNotFound {
		t.Fatalf("unknown execution logs status=%d error=%+v; want 404 not_found", recorder.Code, publicError)
	}

	recorder, _, publicError = restCall(t, fixture.handler, http.MethodGet,
		"/api/v1/executions/"+executionID+"/logs", "dev-project-token",
		api.ExecutionLogsReadRequest{ActionID: "act-missing", Stream: "stdout"}, nil, "")
	if recorder.Code != http.StatusNotFound || publicError.Code != api.CodeNotFound {
		t.Fatalf("unknown action logs status=%d error=%+v; want 404 not_found", recorder.Code, publicError)
	}
}

func TestComposedLogsReadableDuringRequestWait(t *testing.T) {
	const composedLogStderr = "composed container stderr\n"
	fixture := newComposedLogFixture(t, 0)
	fixture.runtime.mu.Lock()
	fixture.runtime.pages = map[int64]kernelengine.LogBatch{
		0: {
			Chunks: []kernelengine.LogChunk{
				{Stream: kernelengine.LogStdout, Offset: 0, Sequence: 1, Data: []byte(composedLogOutput)},
				{Stream: kernelengine.LogStderr, Offset: 0, Sequence: 1, Data: []byte(composedLogStderr)},
			},
			NextOffset: int64(len(composedLogOutput) + len(composedLogStderr)),
		},
		int64(len(composedLogOutput) + len(composedLogStderr)): {
			NextOffset: int64(len(composedLogOutput) + len(composedLogStderr)),
		},
	}
	fixture.runtime.mu.Unlock()

	// Grant reviewer alongside admin/engineer so the test can resolve the wait.
	membershipVersion := uint64(1)
	recorder, _, publicError := restCall(t, fixture.handler, http.MethodPut,
		"/api/v1/projects/project-1/memberships/admin-1", "dev-admin-token",
		api.MembershipGrantRequest{Roles: []string{"admin", "engineer", "reviewer"}}, &membershipVersion, "membership-reviewer")
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("membership.grant reviewer status=%d error=%+v", recorder.Code, publicError)
	}

	const requestPipeline = `actions:
  - name: prepare
    image: busybox:latest
    commands:
      - echo prepare
  - name: approve
    request: role/reviewer
    subject:
      stage: prepare
  - name: integrate
    image: busybox:latest
    commands:
      - echo integrate
`
	projectToken := "dev-project-token"
	recorder, result, publicError := restCall(t, fixture.handler, http.MethodPut,
		"/api/v1/projects/project-1/pipelines/review-flow", projectToken,
		api.PipelineApplyRequest{Name: "review-flow", YAML: []byte(requestPipeline)}, nil, "review-pipeline-1")
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("pipeline.apply status=%d error=%+v", recorder.Code, publicError)
	}
	applied := decodeBody[api.PipelineApplyResult](t, result)
	recorder, result, publicError = restCall(t, fixture.handler, http.MethodPost,
		"/api/v1/projects/project-1/tasks/create-and-start", projectToken,
		api.TaskCreateAndStartRequest{Title: "review-logs", PipelineID: applied.PipelineID}, nil, "review-task-1")
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("task.create_and_start status=%d error=%+v", recorder.Code, publicError)
	}
	started := decodeBody[api.TaskCreateAndStartResult](t, result)

	var waiting api.ExecutionInspectResult
	eventually(t, 5*time.Second, func() bool {
		_, inspectedResult, _ := restCall(t, fixture.handler, http.MethodGet,
			"/api/v1/executions/"+started.ExecutionID, projectToken, nil, nil, "")
		if err := json.Unmarshal(inspectedResult.Body, &waiting); err != nil {
			return false
		}
		return waiting.State == "running" && len(waiting.Actions) == 3 &&
			waiting.Actions[0].State == "succeeded" && waiting.Actions[1].State == "running"
	})

	prepareAction := waiting.Actions[0]
	integrateAction := waiting.Actions[2]

	// Container removal and workspace release must NOT have happened yet while
	// the request action is waiting, yet prepare's stdout and stderr must
	// already be durably readable.
	if got := fixture.runtime.removeCount(); got != 0 {
		t.Fatalf("runtime Remove calls during request wait = %d; want 0", got)
	}
	if got := fixture.workspace.releaseCount(); got != 0 {
		t.Fatalf("workspace Release calls during request wait = %d; want 0", got)
	}
	if got := fixture.readStream(t, started.ExecutionID, prepareAction.ActionID, "stdout"); got != composedLogOutput {
		t.Fatalf("prepare stdout during request wait = %q; want %q", got, composedLogOutput)
	}
	if got := fixture.readStream(t, started.ExecutionID, prepareAction.ActionID, "stderr"); got != composedLogStderr {
		t.Fatalf("prepare stderr during request wait = %q; want %q", got, composedLogStderr)
	}

	// Approve the request so the execution completes and runs terminal cleanup.
	// Re-inspect on version_conflict because the served inspect view can straddle
	// an activation/credential-revocation commit (same pattern as requests_test.go).
	resolved := false
	for range 20 {
		_, currentResult, _ := restCall(t, fixture.handler, http.MethodGet,
			"/api/v1/executions/"+started.ExecutionID, projectToken, nil, nil, "")
		var current api.ExecutionInspectResult
		if err := json.Unmarshal(currentResult.Body, &current); err != nil {
			t.Fatalf("unmarshal execution inspect: %v", err)
		}
		currentApprove := current.Actions[1]
		expectedVersion := current.Version
		recorder, _, publicError = restCall(t, fixture.handler, http.MethodPost,
			"/api/v1/executions/"+started.ExecutionID+"/actions/"+currentApprove.ActionID+"/resolve",
			projectToken,
			api.ExecutionRequestResolveRequest{
				ExecutionID:           started.ExecutionID,
				ActionID:              currentApprove.ActionID,
				ExpectedActionVersion: currentApprove.Version,
				Decision:              "pass",
			},
			&expectedVersion,
			"resolve-approve-1",
		)
		if publicError.Code == api.CodeVersionConflict {
			continue
		}
		if recorder.Code != api.HTTPStatusSuccess {
			t.Fatalf("execution.request.resolve status=%d error=%+v", recorder.Code, publicError)
		}
		resolved = true
		break
	}
	if !resolved {
		t.Fatal("execution.request.resolve did not converge on a current execution version")
	}

	var finished api.ExecutionInspectResult
	eventually(t, 5*time.Second, func() bool {
		_, inspectedResult, _ := restCall(t, fixture.handler, http.MethodGet,
			"/api/v1/executions/"+started.ExecutionID, projectToken, nil, nil, "")
		if err := json.Unmarshal(inspectedResult.Body, &finished); err != nil {
			return false
		}
		return finished.State == "succeeded" && finished.Cleanup.State == "complete"
	})

	if got := fixture.runtime.removeCount(); got != 2 {
		t.Fatalf("runtime Remove calls after completion = %d; want 2 (prepare + integrate)", got)
	}
	if got := fixture.workspace.releaseCount(); got != 1 {
		t.Fatalf("workspace Release calls after completion = %d; want 1", got)
	}
	// Terminal cleanup must not duplicate prepare's already-drained chunks, and
	// integrate's logs must also be retained.
	if got := fixture.readStream(t, started.ExecutionID, prepareAction.ActionID, "stdout"); got != composedLogOutput {
		t.Fatalf("prepare stdout after cleanup = %q; want exact single copy %q", got, composedLogOutput)
	}
	if got := fixture.readStream(t, started.ExecutionID, prepareAction.ActionID, "stderr"); got != composedLogStderr {
		t.Fatalf("prepare stderr after cleanup = %q; want exact single copy %q", got, composedLogStderr)
	}
	if got := fixture.readStream(t, started.ExecutionID, integrateAction.ActionID, "stdout"); got != composedLogOutput {
		t.Fatalf("integrate stdout after cleanup = %q; want %q", got, composedLogOutput)
	}
}
