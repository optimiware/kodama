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

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
	shelllog "github.com/optimiware/kodama/internal/shell/log"
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

func (fixture *composedLogFixture) readStdout(t *testing.T, executionID, actionID string) string {
	t.Helper()
	recorder, result, publicError := restCall(t, fixture.handler, http.MethodGet,
		"/api/v1/executions/"+executionID+"/logs", "dev-project-token",
		api.ExecutionLogsReadRequest{ActionID: actionID, Stream: "stdout"}, nil, "log-read-1")
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("execution.logs.read status=%d error=%+v", recorder.Code, publicError)
	}
	page := decodeBody[api.ExecutionLogsReadResult](t, result)
	var collected strings.Builder
	for _, chunk := range page.Chunks {
		collected.Write(chunk.Data)
	}
	return collected.String()
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
	fixture := newComposedLogFixture(t, 1)
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
