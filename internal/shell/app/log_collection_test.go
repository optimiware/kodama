// Retained-log collection before cleanup: composition API and rules.
//
// Wiring only the read side of retained logs and removing the workload
// container directly would leave the retained sink empty. The composition
// therefore adds a decorator around the Kernel runtime port: before the runtime
// removes a workload it drains the adapter's log read seam into the retained
// sink. The implementation is in internal/shell/app/log_collection.go with
// this package-internal API (tests construct it directly; wireApp composes it):
//
//	type executionScopeReader interface {
//		ExecutionScope(context.Context, id.ExecutionID) (tenancy.Scope, error)
//	}
//
//	type logCursorStore interface {
//		Load(context.Context, id.ExecutionID, id.ActionID, id.WorkloadID) (int64, bool, error)
//		Save(context.Context, id.ExecutionID, id.ActionID, id.WorkloadID, int64) error
//	}
//
//	type logCollectionConfig struct {
//		Inner     kernelengine.Runtime  // runtime that performs the real Remove
//		Sink      shelllog.RetainedSink // durable retained-log sink
//		Scopes    executionScopeReader  // execution -> immutable owning scope
//		Cursors   logCursorStore        // durable opaque runtime log cursor
//		PageBytes int                   // optional; 0 means shelllog.DefaultBounds.MaxChunkBytes
//	}
//
//	func newLogCollectingRuntime(logCollectionConfig) (*logCollectingRuntime, error)
//	func (*logCollectingRuntime) Remove(context.Context, kernelengine.RemoveRequest) (kernelengine.Removal, error)
//	func (*logCollectingRuntime) Stop(context.Context, kernelengine.StopRequest) (kernel.StopEvidence, error)
//
// The decorator implements the rest of kernelengine.Runtime by delegating to
// Inner. wireApp wraps the tracked runtime with it for every profile
// (development, test and production) and passes the decorator to
// kernelengine.New; App.runtime stays the tracked runtime so an explicit stop is
// forgotten only after confirmed evidence and the process runtime still stops
// its own workloads on close (durable workloads outlive a controller close).
// The file-backed cursor store lives under
// DataDir/logs/cursors; no new Config field is required for production. The
// unit tests below construct the decorator directly with fakes, and the wiring
// test in log_collection_wiring_test.go exercises the shared wireApp path with
// the in-memory test profile and the real file sink over a temp directory. A
// production composition unit test would need the real PostgreSQL/NATS/KMS
// adapters and is out of scope for this unit suite.
//
// Stable marker values the decorator declares (each is a fixed, secret-free
// marker; the tests do not depend on the exact text):
//
//	const logCollectionUnavailableMarker = "..." // stdout, offset 0, sequence 0
//
// Rules pinned by these tests:
//
//  1. Remove resolves the execution's owning tenancy scope first. A scope
//     failure appends nothing and never calls Inner.Remove (cleanup retained).
//  2. Logs is paged with AfterOffset = the saved opaque cursor and Limit =
//     PageBytes. Each page is appended under the resolved scope; a runtime
//     chunk larger than PageBytes is split into PageBytes pieces (never a
//     sink-refused oversized chunk) and the pieces keep distinct offsets so the
//     sink identity is stable across retries.
//  3. After a page's appends succeed, the adapter's NextOffset is saved as the
//     durable cursor (append before save). Any Logs, Append or Save error
//     returns a retryable fault and never calls Inner.Remove: the cleanup
//     obligation stays pending. A lost cursor save therefore only re-reads
//     already-stored chunks, which the sink deduplicates by identity and bytes.
//     The decorator never calls Inner.Prepare or Inner.Start; collection is
//     read-only and cannot replay the container.
//  4. Oversized output is bounded by the sink's chunk and stored-chunk limits.
//     The collector never hands the sink a chunk over PageBytes, and the sink's
//     ReadResult.Truncated is the explicit truncation signal for evicted
//     content; the collector does not invent a truncation chunk.
//  5. A Logs NotFound (the workload/container is already gone) never blocks
//     cleanup. If the sink already holds any chunk for the action, no marker is
//     written; otherwise one logCollectionUnavailableMarker chunk is written to
//     stdout. Inner.Remove then proceeds. Other faults block removal.
//  6. Logs are stored only under the execution row's immutable tenant/project
//     scope; a read under another scope never sees them.
//  7. Stop makes the readable prefix durable before delegating to the
//     destructive termination. It is a bounded snapshot, not completion: a
//     temporary EOF or spent budget writes no completion token, Remove resumes
//     from the saved cursor and drains any readable tail, and a destroyed source
//     keeps the retained prefix instead of an unavailable marker. Only
//     newline-terminated live output is checkpointed; a trailing partial line is
//     left for the final drain so a split credential is never stored in
//     plaintext. An absent source or a workload that has not started does not
//     block the stop; any other checkpoint fault withholds the destructive stop
//     for a bounded number of consecutive attempts, then delegates it so a
//     persistent fault cannot leave a live workload running.
package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
	shelllog "github.com/optimiware/kodama/internal/shell/log"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// collectionOrder records the observable side-effect order of one fixture:
// "logs" (runtime log read), "append" (retained sink append), "remove"
// (Inner.Remove) and "release" (workspace release).
type collectionOrder struct {
	mu     sync.Mutex
	events []string
}

func (order *collectionOrder) record(event string) {
	order.mu.Lock()
	order.events = append(order.events, event)
	order.mu.Unlock()
}

func (order *collectionOrder) snapshot() []string {
	order.mu.Lock()
	defer order.mu.Unlock()
	return append([]string(nil), order.events...)
}

func (order *collectionOrder) first(event string) int {
	for index, candidate := range order.snapshot() {
		if candidate == event {
			return index
		}
	}
	return -1
}

func (order *collectionOrder) last(event string) int {
	events := order.snapshot()
	for index, event0 := range slices.Backward(events) {
		if event0 == event {
			return index
		}
	}
	return -1
}

func (order *collectionOrder) count(event string) int {
	count := 0
	for _, candidate := range order.snapshot() {
		if candidate == event {
			count++
		}
	}
	return count
}

// logPageRuntime is a fake Kernel runtime that serves deterministic log pages
// keyed by the opaque AfterOffset and records every Remove and log read. It
// embeds the shared no-effect fake for the operations the collector never
// changes.
type logPageRuntime struct {
	*fakeRuntime

	order *collectionOrder

	mu               sync.Mutex
	pages            map[int64]kernelengine.LogBatch
	pagesByExecution map[id.ExecutionID]map[int64]kernelengine.LogBatch
	pagesByWorkload  map[id.WorkloadID]map[int64]kernelengine.LogBatch
	logsErr          error
	errorsByWorkload map[id.WorkloadID]error
	errorsByOffset   map[int64]error
	logFailures      int
	removeErr        error
	logCalls         []int64
	logLimits        []int
	identities       []kernelengine.EffectIdentity
	removeCalls      int
	startCalls       int
	stopCalls        int
	stopErr          error
	stopEvidence     kernel.StopEvidence
	onStop           func()
	sourceDestroyed  bool
	limitPages       bool
}

func (runtime *logPageRuntime) Logs(ctx context.Context, request kernelengine.LogsRequest) (kernelengine.LogBatch, error) {
	runtime.order.record("logs")
	runtime.mu.Lock()
	runtime.logCalls = append(runtime.logCalls, request.AfterOffset)
	runtime.logLimits = append(runtime.logLimits, request.Limit)
	runtime.identities = append(runtime.identities, request.Identity)
	if runtime.sourceDestroyed {
		runtime.mu.Unlock()
		// A destructive stop removed the pod and with it the only readable
		// container log source.
		return kernelengine.LogBatch{}, fault.New(fault.NotFound, "destructive stop destroyed the workload log source")
	}
	pages := runtime.pages
	if specific, ok := runtime.pagesByExecution[request.Identity.ExecutionID]; ok {
		pages = specific
	}
	if specific, ok := runtime.pagesByWorkload[request.Identity.WorkloadID]; ok {
		pages = specific
	}
	err := runtime.logsErr
	if specific, ok := runtime.errorsByWorkload[request.Identity.WorkloadID]; ok {
		err = specific
	}
	if runtime.logFailures > 0 {
		runtime.logFailures--
		err = fault.New(fault.DependencyUnavailable, "runtime log read is temporarily unavailable")
	}
	if runtime.errorsByOffset != nil {
		if specific, ok := runtime.errorsByOffset[request.AfterOffset]; ok {
			err = specific
		}
	}
	page, ok := pages[request.AfterOffset]
	limitPages := runtime.limitPages
	runtime.mu.Unlock()
	if err != nil {
		return kernelengine.LogBatch{}, err
	}
	if !ok {
		return kernelengine.LogBatch{NextOffset: request.AfterOffset}, nil
	}
	if limitPages && request.Limit > 0 && len(page.Chunks) == 1 && len(page.Chunks[0].Data) > request.Limit {
		// Model an adapter that honours the requested page bound: the page ends
		// at the requested limit and the continuation advances by exactly those
		// bytes. Only single-stream pages are truncated; multi-stream fixtures
		// keep their explicit opaque offsets.
		chunk := page.Chunks[0]
		chunk.Data = chunk.Data[:request.Limit]
		return kernelengine.LogBatch{
			Chunks:     []kernelengine.LogChunk{chunk},
			NextOffset: request.AfterOffset + int64(request.Limit),
		}, nil
	}
	return page, nil
}

func (runtime *logPageRuntime) Remove(ctx context.Context, request kernelengine.RemoveRequest) (kernelengine.Removal, error) {
	runtime.order.record("remove")
	runtime.mu.Lock()
	runtime.removeCalls++
	err := runtime.removeErr
	runtime.mu.Unlock()
	if err != nil {
		return kernelengine.Removal{}, err
	}
	return kernelengine.Removal{Removed: true}, nil
}

func (runtime *logPageRuntime) Start(ctx context.Context, request kernelengine.StartRequest) (kernelengine.StartResult, error) {
	runtime.mu.Lock()
	runtime.startCalls++
	runtime.mu.Unlock()
	return runtime.fakeRuntime.Start(ctx, request)
}

// Stop records the destructive stop and runs the configured controller
// reaction. The reaction models the Kubernetes Job controller deleting the
// active Pod (and its container log) in response to spec.suspend.
func (runtime *logPageRuntime) Stop(ctx context.Context, request kernelengine.StopRequest) (kernel.StopEvidence, error) {
	runtime.order.record("stop")
	runtime.mu.Lock()
	runtime.stopCalls++
	hook := runtime.onStop
	err := runtime.stopErr
	evidence := runtime.stopEvidence
	runtime.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err != nil {
		return kernel.PendingStop("stop did not complete"), err
	}
	if evidence.State == "" {
		evidence = kernel.ConfirmedStop("suspended with no workload pod")
	}
	return evidence, nil
}

// destroyLogSource models suspension terminating and deleting the workload Pod:
// every later log read is NotFound even though the bytes were readable before.
func (runtime *logPageRuntime) destroyLogSource() {
	runtime.mu.Lock()
	runtime.sourceDestroyed = true
	runtime.mu.Unlock()
}

// appendPage serves one more page from offset after the current snapshot, as a
// workload that keeps writing while it handles the stop request would.
func (runtime *logPageRuntime) appendPage(offset int64, batch kernelengine.LogBatch) {
	runtime.mu.Lock()
	if runtime.pages == nil {
		runtime.pages = map[int64]kernelengine.LogBatch{}
	}
	runtime.pages[offset] = batch
	runtime.mu.Unlock()
}

func (runtime *logPageRuntime) stopCountValue() int {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.stopCalls
}

func (runtime *logPageRuntime) removeCount() int {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.removeCalls
}

func (runtime *logPageRuntime) logCallOffsets() []int64 {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return append([]int64(nil), runtime.logCalls...)
}

func (runtime *logPageRuntime) logLimitValues() []int {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return append([]int(nil), runtime.logLimits...)
}

// fakeExecutionScopes resolves an execution to its immutable owning scope.
type fakeExecutionScopes struct {
	mu     sync.Mutex
	scopes map[id.ExecutionID]tenancy.Scope
	err    error
	calls  int
}

func (reader *fakeExecutionScopes) ExecutionScope(ctx context.Context, executionID id.ExecutionID) (tenancy.Scope, error) {
	reader.mu.Lock()
	reader.calls++
	err := reader.err
	scope, ok := reader.scopes[executionID]
	reader.mu.Unlock()
	if err != nil {
		return tenancy.Scope{}, err
	}
	if !ok {
		return tenancy.Scope{}, fault.New(fault.NotFound, "execution scope is not available")
	}
	return scope, nil
}

// memoryLogCursors is the fake durable cursor store. saveFailures models a lost
// cursor write after the append effect; the stored chunk then remains and a
// retry must deduplicate it.
type memoryLogCursors struct {
	mu           sync.Mutex
	cursors      map[string]int64
	loadErr      error
	saveErr      error
	saveFailures int
	saveFailAt   int
	saveCalls    int
}

func logCursorKey(executionID id.ExecutionID, actionID id.ActionID, workloadID id.WorkloadID) string {
	return string(executionID) + "\x00" + string(actionID) + "\x00" + string(workloadID)
}

func (store *memoryLogCursors) Load(ctx context.Context, executionID id.ExecutionID, actionID id.ActionID, workloadID id.WorkloadID) (int64, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.loadErr != nil {
		return 0, false, store.loadErr
	}
	cursor, ok := store.cursors[logCursorKey(executionID, actionID, workloadID)]
	return cursor, ok, nil
}

func (store *memoryLogCursors) Save(ctx context.Context, executionID id.ExecutionID, actionID id.ActionID, workloadID id.WorkloadID, cursor int64) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.saveCalls++
	if store.saveFailAt == store.saveCalls {
		return fault.New(fault.DependencyUnavailable, "retained log cursor is unavailable")
	}
	if store.saveFailures > 0 {
		store.saveFailures--
		return fault.New(fault.DependencyUnavailable, "retained log cursor is unavailable")
	}
	if store.saveErr != nil {
		return store.saveErr
	}
	store.cursors[logCursorKey(executionID, actionID, workloadID)] = cursor
	return nil
}

// recordingSink records every append (with its scope) and can fail a bounded
// number of appends before forwarding to the real file sink.
type recordingSink struct {
	inner shelllog.RetainedSink
	order *collectionOrder

	mu             sync.Mutex
	chunks         []shelllog.Chunk
	scopes         []tenancy.Scope
	appendFailures int
	appendErr      error
}

func (sink *recordingSink) Append(ctx context.Context, scope tenancy.Scope, chunk shelllog.Chunk) (shelllog.AppendResult, error) {
	sink.order.record("append")
	sink.mu.Lock()
	sink.chunks = append(sink.chunks, chunk)
	sink.scopes = append(sink.scopes, scope)
	err := sink.appendErr
	if sink.appendFailures > 0 {
		sink.appendFailures--
		err = fault.New(fault.DependencyUnavailable, "retained log sink is unavailable")
	}
	sink.mu.Unlock()
	if err != nil {
		return shelllog.AppendResult{}, err
	}
	if sink.inner == nil {
		return shelllog.AppendResult{Offset: chunk.Offset, Sequence: chunk.Sequence}, nil
	}
	return sink.inner.Append(ctx, scope, chunk)
}

func (sink *recordingSink) Read(ctx context.Context, request shelllog.ReadRequest) (shelllog.ReadResult, error) {
	if sink.inner == nil {
		return shelllog.ReadResult{}, fault.New(fault.InternalFailure, "recording sink has no inner sink")
	}
	return sink.inner.Read(ctx, request)
}

func (sink *recordingSink) appendedChunks() []shelllog.Chunk {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]shelllog.Chunk(nil), sink.chunks...)
}

func (sink *recordingSink) appendedScopes() []tenancy.Scope {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]tenancy.Scope(nil), sink.scopes...)
}

// logCollectionFixture is one decorator over a fake runtime and a real file
// sink rooted in a temp directory.
type logCollectionFixture struct {
	order       *collectionOrder
	runtime     *logPageRuntime
	sink        *recordingSink
	fileSink    *shelllog.FileSink
	scopes      *fakeExecutionScopes
	cursors     *memoryLogCursors
	collector   *logCollectingRuntime
	scope       tenancy.Scope
	executionID id.ExecutionID
	actionID    id.ActionID
	workspaceID id.WorkspaceID
}

func newLogCollectionFixture(t *testing.T, pages map[int64]kernelengine.LogBatch) *logCollectionFixture {
	t.Helper()
	return newLogCollectionFixtureWithSink(t, pages, shelllog.SinkConfig{}, 0)
}

func newLogCollectionFixtureWithSink(t *testing.T, pages map[int64]kernelengine.LogBatch, sinkConfig shelllog.SinkConfig, pageBytes int) *logCollectionFixture {
	t.Helper()
	clock := newFakeClock()
	sinkConfig.Root = t.TempDir()
	sinkConfig.Clock = clock
	fileSink, err := shelllog.NewFileSink(sinkConfig)
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	order := &collectionOrder{}
	sink := &recordingSink{inner: fileSink, order: order}
	runtime := &logPageRuntime{
		fakeRuntime:      &fakeRuntime{},
		order:            order,
		pages:            pages,
		pagesByExecution: map[id.ExecutionID]map[int64]kernelengine.LogBatch{},
	}
	scope := tenancy.Scope{TenantID: "tenant-1", ProjectID: "project-1"}
	executionID := id.ExecutionID("execution-1")
	actionID := id.ActionID("action-1")
	scopes := &fakeExecutionScopes{scopes: map[id.ExecutionID]tenancy.Scope{executionID: scope}}
	cursors := &memoryLogCursors{cursors: map[string]int64{}}
	collector, err := newLogCollectingRuntime(logCollectionConfig{
		Inner:     runtime,
		Sink:      sink,
		Scopes:    scopes,
		Cursors:   cursors,
		PageBytes: pageBytes,
	})
	if err != nil {
		t.Fatalf("newLogCollectingRuntime: %v", err)
	}
	return &logCollectionFixture{
		order:       order,
		runtime:     runtime,
		sink:        sink,
		fileSink:    fileSink,
		scopes:      scopes,
		cursors:     cursors,
		collector:   collector,
		scope:       scope,
		executionID: executionID,
		actionID:    actionID,
		workspaceID: "workspace-1",
	}
}

func (fixture *logCollectionFixture) workloadID() id.WorkloadID {
	return id.WorkloadID("workload-" + string(fixture.executionID))
}

func (fixture *logCollectionFixture) removeRequest() kernelengine.RemoveRequest {
	return fixture.removeRequestFor(fixture.executionID, fixture.actionID)
}

func (fixture *logCollectionFixture) stopRequest() kernelengine.StopRequest {
	return kernelengine.StopRequest{
		Identity: kernelengine.EffectIdentity{
			ExecutionID:    fixture.executionID,
			ActionID:       fixture.actionID,
			WorkloadID:     fixture.workloadID(),
			IdempotencyKey: "idempotency-" + string(fixture.workloadID()),
		},
		WorkspaceID: fixture.workspaceID,
	}
}

func (fixture *logCollectionFixture) removeRequestFor(executionID id.ExecutionID, actionID id.ActionID) kernelengine.RemoveRequest {
	return fixture.removeRequestForWorkload(executionID, actionID, id.WorkloadID("workload-"+string(executionID)))
}

func (fixture *logCollectionFixture) removeRequestForWorkload(executionID id.ExecutionID, actionID id.ActionID, workloadID id.WorkloadID) kernelengine.RemoveRequest {
	return kernelengine.RemoveRequest{
		Identity: kernelengine.EffectIdentity{
			ExecutionID:    executionID,
			ActionID:       actionID,
			WorkloadID:     workloadID,
			IdempotencyKey: "idempotency-" + string(workloadID),
		},
		WorkspaceID: fixture.workspaceID,
	}
}

func readLogs(t *testing.T, sink shelllog.RetainedSink, scope tenancy.Scope, executionID id.ExecutionID, actionID id.ActionID, stream shelllog.Stream) ([]shelllog.Chunk, bool) {
	t.Helper()
	result, err := sink.Read(context.Background(), shelllog.ReadRequest{
		Scope:       scope,
		ExecutionID: executionID,
		ActionID:    actionID,
		Stream:      stream,
		Limit:       shelllog.DefaultLimits().MaxReadChunks,
	})
	if err != nil {
		t.Fatalf("read retained %s logs: %v", stream, err)
	}
	return result.Chunks, result.Truncated
}

func readAllLogs(t *testing.T, sink shelllog.RetainedSink, scope tenancy.Scope, executionID id.ExecutionID, actionID id.ActionID, stream shelllog.Stream) []shelllog.Chunk {
	t.Helper()
	chunks, _ := readLogs(t, sink, scope, executionID, actionID, stream)
	return chunks
}

func joinLogData(chunks []shelllog.Chunk) string {
	var builder strings.Builder
	for _, chunk := range chunks {
		builder.Write(chunk.Data)
	}
	return builder.String()
}

func TestLogsBeforeCleanup(t *testing.T) {
	t.Run("collects_every_page_before_remove", func(t *testing.T) {
		fixture := newLogCollectionFixture(t, map[int64]kernelengine.LogBatch{
			0: {
				Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: 0, Sequence: 1, Data: []byte("alpha\n")}},
				NextOffset: 6,
			},
			6: {
				Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStderr, Offset: 0, Sequence: 2, Data: []byte("beta\n")}},
				NextOffset: 11,
			},
			11: {NextOffset: 11},
		})

		removal, err := fixture.collector.Remove(context.Background(), fixture.removeRequest())
		if err != nil {
			t.Fatalf("Remove returned error: %v", err)
		}
		if !removal.Removed {
			t.Fatal("Remove did not confirm removal")
		}
		if fixture.runtime.removeCalls != 1 {
			t.Fatalf("Remove calls = %d; want exactly 1", fixture.runtime.removeCalls)
		}
		if got := fixture.runtime.logCalls; len(got) != 3 || got[0] != 0 || got[1] != 6 || got[2] != 11 {
			t.Fatalf("log read offsets = %v; want [0 6 11]", got)
		}
		if fixture.runtime.startCalls != 0 {
			t.Fatalf("collector started the workload %d times; log collection must not replay the container", fixture.runtime.startCalls)
		}
		events := fixture.order.snapshot()
		firstLog := fixture.order.first("logs")
		lastAppend := fixture.order.last("append")
		firstRemove := fixture.order.first("remove")
		if firstLog < 0 || lastAppend < 0 || firstRemove < 0 {
			t.Fatalf("effect order %v is missing a collect/append/remove event", events)
		}
		if firstLog >= firstRemove {
			t.Fatalf("effect order %v read logs after removing the container", events)
		}
		if lastAppend >= firstRemove {
			t.Fatalf("effect order %v appended after removing the container", events)
		}
		if got := fixture.order.count("remove"); got != 1 {
			t.Fatalf("remove events = %d; want exactly 1 in %v", got, events)
		}
		if events[len(events)-1] != "remove" {
			t.Fatalf("effect order %v does not end with the single remove", events)
		}
		if got := joinLogData(readAllLogs(t, fixture.sink, fixture.scope, fixture.executionID, fixture.actionID, shelllog.StreamStdout)); got != "alpha\n" {
			t.Fatalf("retained stdout = %q; want %q", got, "alpha\n")
		}
		if got := joinLogData(readAllLogs(t, fixture.sink, fixture.scope, fixture.executionID, fixture.actionID, shelllog.StreamStderr)); got != "beta\n" {
			t.Fatalf("retained stderr = %q; want %q", got, "beta\n")
		}
		for index, scope := range fixture.sink.appendedScopes() {
			if scope != fixture.scope {
				t.Fatalf("append %d scope = %+v; want %+v", index, scope, fixture.scope)
			}
		}
	})

	t.Run("collection_failure_retains_cleanup", func(t *testing.T) {
		fixture := newLogCollectionFixture(t, nil)
		fixture.runtime.logsErr = fault.New(fault.DependencyUnavailable, "runtime log read is unavailable")

		_, err := fixture.collector.Remove(context.Background(), fixture.removeRequest())
		if err == nil {
			t.Fatal("Remove succeeded although log collection failed")
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
			t.Fatalf("Remove error class = %v; want dependency_unavailable", class)
		}
		if fixture.runtime.removeCalls != 0 {
			t.Fatalf("Remove calls = %d; cleanup must stay retained when collection fails", fixture.runtime.removeCalls)
		}
		if got := len(fixture.sink.appendedChunks()); got != 0 {
			t.Fatalf("append calls = %d; a failed collection must not store partial output", got)
		}
	})

	t.Run("append_failure_retains_cleanup", func(t *testing.T) {
		fixture := newLogCollectionFixture(t, map[int64]kernelengine.LogBatch{
			0: {
				Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: 0, Sequence: 1, Data: []byte("alpha\n")}},
				NextOffset: 6,
			},
		})
		fixture.sink.appendFailures = 1

		_, err := fixture.collector.Remove(context.Background(), fixture.removeRequest())
		if err == nil {
			t.Fatal("Remove succeeded although the sink append failed")
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
			t.Fatalf("Remove error class = %v; want dependency_unavailable", class)
		}
		if fixture.runtime.removeCalls != 0 {
			t.Fatalf("Remove calls = %d; the container must not be removed after a sink failure", fixture.runtime.removeCalls)
		}
	})

	t.Run("scope_failure_retains_cleanup", func(t *testing.T) {
		fixture := newLogCollectionFixture(t, nil)
		fixture.scopes.err = fault.New(fault.DependencyUnavailable, "execution scope is unavailable")

		_, err := fixture.collector.Remove(context.Background(), fixture.removeRequest())
		if err == nil {
			t.Fatal("Remove succeeded although the owning scope could not be resolved")
		}
		if len(fixture.runtime.logCalls) != 0 {
			t.Fatalf("log reads = %v; a scope-less collection must never read or store logs", fixture.runtime.logCalls)
		}
		if got := len(fixture.sink.appendedChunks()); got != 0 {
			t.Fatalf("append calls = %d; a scope-less collection must never append", got)
		}
		if fixture.runtime.removeCalls != 0 {
			t.Fatalf("Remove calls = %d; cleanup must stay retained", fixture.runtime.removeCalls)
		}
	})

	t.Run("cursor_save_failure_retains_cleanup_after_the_append", func(t *testing.T) {
		fixture := newLogCollectionFixture(t, map[int64]kernelengine.LogBatch{
			0: {
				Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: 0, Sequence: 1, Data: []byte("alpha\n")}},
				NextOffset: 6,
			},
		})
		fixture.cursors.saveFailures = 1

		_, err := fixture.collector.Remove(context.Background(), fixture.removeRequest())
		if err == nil {
			t.Fatal("Remove succeeded although the durable cursor could not be saved")
		}
		if fixture.runtime.removeCalls != 0 {
			t.Fatalf("Remove calls = %d; a lost cursor write must retain cleanup", fixture.runtime.removeCalls)
		}
		if got := len(fixture.sink.appendedChunks()); got != 1 {
			t.Fatalf("append calls = %d; the append effect happened before the lost cursor write", got)
		}
		if _, ok, err := fixture.cursors.Load(context.Background(), fixture.executionID, fixture.actionID, fixture.workloadID()); err != nil || ok {
			t.Fatalf("cursor Load = (_, %v, %v); want an unsaved cursor", ok, err)
		}
	})
}

func TestLogCollectionResumesAfterRestart(t *testing.T) {
	pages := func() map[int64]kernelengine.LogBatch {
		return map[int64]kernelengine.LogBatch{
			0: {
				Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: 0, Sequence: 1, Data: []byte("one\n")}},
				NextOffset: 4,
			},
			4: {
				Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStderr, Offset: 0, Sequence: 2, Data: []byte("two\n")}},
				NextOffset: 8,
			},
			8: {NextOffset: 8},
		}
	}

	t.Run("completed_collection_skips_the_log_source_after_a_remove_failure", func(t *testing.T) {
		fixture := newLogCollectionFixture(t, pages())
		fixture.runtime.removeErr = fault.New(fault.DependencyUnavailable, "remove response was lost")
		if _, err := fixture.collector.Remove(context.Background(), fixture.removeRequest()); err == nil {
			t.Fatal("first Remove succeeded despite the injected remove failure")
		}
		if fixture.runtime.removeCalls != 1 {
			t.Fatalf("first Remove calls = %d; want 1", fixture.runtime.removeCalls)
		}
		if cursor, ok, err := fixture.cursors.Load(context.Background(), fixture.executionID, fixture.actionID, fixture.workloadID()); err != nil || !ok || cursor != logCollectionComplete {
			t.Fatalf("saved cursor = (%d, %v, %v); want the durable completion token", cursor, ok, err)
		}

		fixture.runtime.removeErr = nil
		fixture.runtime.logsErr = fault.New(fault.DependencyUnavailable, "redaction source is unavailable; log read refused")
		restarted, err := newLogCollectingRuntime(logCollectionConfig{
			Inner:   fixture.runtime,
			Sink:    fixture.sink,
			Scopes:  fixture.scopes,
			Cursors: fixture.cursors,
		})
		if err != nil {
			t.Fatalf("newLogCollectingRuntime after restart: %v", err)
		}
		before := len(fixture.runtime.logCalls)
		if _, err := restarted.Remove(context.Background(), fixture.removeRequest()); err != nil {
			t.Fatalf("restarted Remove: %v", err)
		}
		if fixture.runtime.removeCalls != 2 {
			t.Fatalf("Remove calls after restart = %d; want 2", fixture.runtime.removeCalls)
		}
		if got := len(fixture.runtime.logCalls); got != before {
			t.Fatalf("log reads after restart = %d; want no new read after completion (%d)", got, before)
		}
		if got := joinLogData(readAllLogs(t, fixture.sink, fixture.scope, fixture.executionID, fixture.actionID, shelllog.StreamStdout)); got != "one\n" {
			t.Fatalf("retained stdout = %q; want one copy of %q", got, "one\n")
		}
		if got := joinLogData(readAllLogs(t, fixture.sink, fixture.scope, fixture.executionID, fixture.actionID, shelllog.StreamStderr)); got != "two\n" {
			t.Fatalf("retained stderr = %q; want one copy of %q", got, "two\n")
		}
		if fixture.runtime.startCalls != 0 {
			t.Fatalf("restart started the workload %d times; logs must resume without replaying the container", fixture.runtime.startCalls)
		}
	})

	t.Run("lost_cursor_save_does_not_duplicate_or_gap", func(t *testing.T) {
		fixture := newLogCollectionFixture(t, pages())
		fixture.cursors.saveFailures = 1
		if _, err := fixture.collector.Remove(context.Background(), fixture.removeRequest()); err == nil {
			t.Fatal("first Remove succeeded despite the injected cursor write failure")
		}
		if fixture.runtime.removeCalls != 0 {
			t.Fatalf("Remove calls = %d; the append effect alone must not permit removal", fixture.runtime.removeCalls)
		}
		if cursor, ok, err := fixture.cursors.Load(context.Background(), fixture.executionID, fixture.actionID, fixture.workloadID()); err != nil || ok {
			t.Fatalf("cursor after the lost write = (%d, %v, %v); want unsaved", cursor, ok, err)
		}

		restarted, err := newLogCollectingRuntime(logCollectionConfig{
			Inner:   fixture.runtime,
			Sink:    fixture.sink,
			Scopes:  fixture.scopes,
			Cursors: fixture.cursors,
		})
		if err != nil {
			t.Fatalf("newLogCollectingRuntime after restart: %v", err)
		}
		before := len(fixture.runtime.logCalls)
		if _, err := restarted.Remove(context.Background(), fixture.removeRequest()); err != nil {
			t.Fatalf("restarted Remove: %v", err)
		}
		if fixture.runtime.removeCalls != 1 {
			t.Fatalf("Remove calls after restart = %d; want 1", fixture.runtime.removeCalls)
		}
		calls := fixture.runtime.logCalls
		if before >= len(calls) || calls[before] != 0 {
			t.Fatalf("first log read after the lost cursor write = %v; want a re-read from 0", calls[before:])
		}
		if got := joinLogData(readAllLogs(t, fixture.sink, fixture.scope, fixture.executionID, fixture.actionID, shelllog.StreamStdout)); got != "one\n" {
			t.Fatalf("retained stdout = %q; the re-read must deduplicate %q", got, "one\n")
		}
		if got := joinLogData(readAllLogs(t, fixture.sink, fixture.scope, fixture.executionID, fixture.actionID, shelllog.StreamStderr)); got != "two\n" {
			t.Fatalf("retained stderr = %q; want one copy of %q", got, "two\n")
		}
	})

	t.Run("retry_after_successful_remove_does_not_mark_unavailable", func(t *testing.T) {
		fixture := newLogCollectionFixture(t, pages())
		if _, err := fixture.collector.Remove(context.Background(), fixture.removeRequest()); err != nil {
			t.Fatalf("first Remove: %v", err)
		}
		if fixture.runtime.removeCalls != 1 {
			t.Fatalf("first Remove calls = %d; want 1", fixture.runtime.removeCalls)
		}
		before := len(fixture.sink.appendedChunks())

		// A crash after the remove effect but before the cleanup commit retries
		// Remove against an already-gone container.
		fixture.runtime.logsErr = fault.New(fault.NotFound, "workload container is already gone")
		if _, err := fixture.collector.Remove(context.Background(), fixture.removeRequest()); err != nil {
			t.Fatalf("retry Remove: %v", err)
		}
		if fixture.runtime.removeCalls != 2 {
			t.Fatalf("Remove calls after retry = %d; want 2", fixture.runtime.removeCalls)
		}
		if after := len(fixture.sink.appendedChunks()); after != before {
			t.Fatalf("append calls after retry = %d; want %d (no duplicate marker)", after, before)
		}
	})
}

// TestLogCollectionRetryDoesNotNeedRemovedLogSource proves the collector is
// idempotent after a completed collection: once the action's output is durably
// stored, a cleanup retry whose underlying Remove already deleted the credential
// Secret (and whose workspace Release is still pending) must not read the log
// source again. Otherwise the deleted redaction Secret turns a completed
// collection into a permanent dependency fault and exhausts the retry budget.
func TestLogCollectionRetryDoesNotNeedRemovedLogSource(t *testing.T) {
	pages := func() map[int64]kernelengine.LogBatch {
		return map[int64]kernelengine.LogBatch{
			0: {
				Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: 0, Sequence: 1, Data: []byte("alpha\n")}},
				NextOffset: 6,
			},
			6: {NextOffset: 6},
		}
	}

	t.Run("retry_after_remove_skips_the_log_source", func(t *testing.T) {
		fixture := newLogCollectionFixture(t, pages())
		removal, err := fixture.collector.Remove(context.Background(), fixture.removeRequest())
		if err != nil {
			t.Fatalf("first Remove: %v", err)
		}
		if !removal.Removed {
			t.Fatal("first Remove did not confirm removal")
		}
		readsAfterCollection := len(fixture.runtime.logCallOffsets())
		if cursor, ok, err := fixture.cursors.Load(context.Background(), fixture.executionID, fixture.actionID, fixture.workloadID()); err != nil || !ok || cursor != logCollectionComplete {
			t.Fatalf("completed collection cursor = (%d, %v, %v); want the durable completion token", cursor, ok, err)
		}

		// The destructive Remove deleted the credential Secret. A retry that
		// needs to read logs again would now fail closed forever.
		fixture.runtime.logsErr = fault.New(fault.DependencyUnavailable, "redaction source is unavailable; log read refused")

		removal, err = fixture.collector.Remove(context.Background(), fixture.removeRequest())
		if err != nil {
			t.Fatalf("retry Remove required the removed log source: %v", err)
		}
		if !removal.Removed {
			t.Fatal("retry Remove did not confirm removal")
		}
		if got := len(fixture.runtime.logCallOffsets()); got != readsAfterCollection {
			t.Fatalf("retry log reads = %d; want no new read after a completed collection (%d)", got, readsAfterCollection)
		}
		if got := fixture.runtime.removeCount(); got != 2 {
			t.Fatalf("Remove calls = %d; want 2", got)
		}
		if got := joinLogData(readAllLogs(t, fixture.sink, fixture.scope, fixture.executionID, fixture.actionID, shelllog.StreamStdout)); got != "alpha\n" {
			t.Fatalf("retained stdout = %q; want the already collected output exactly once", got)
		}
	})

	t.Run("restart_does_not_repeat_completed_collection", func(t *testing.T) {
		fixture := newLogCollectionFixture(t, pages())
		if _, err := fixture.collector.Remove(context.Background(), fixture.removeRequest()); err != nil {
			t.Fatalf("first Remove: %v", err)
		}
		readsAfterCollection := len(fixture.runtime.logCallOffsets())

		// A process restart (fresh decorator over the same durable cursor and
		// sink) sees the completion token and must not consult the log source.
		fixture.runtime.logsErr = fault.New(fault.DependencyUnavailable, "redaction source is unavailable; log read refused")
		restarted, err := newLogCollectingRuntime(logCollectionConfig{
			Inner:   fixture.runtime,
			Sink:    fixture.sink,
			Scopes:  fixture.scopes,
			Cursors: fixture.cursors,
		})
		if err != nil {
			t.Fatalf("newLogCollectingRuntime after restart: %v", err)
		}
		if _, err := restarted.Remove(context.Background(), fixture.removeRequest()); err != nil {
			t.Fatalf("restarted Remove required the removed log source: %v", err)
		}
		if got := len(fixture.runtime.logCallOffsets()); got != readsAfterCollection {
			t.Fatalf("restarted log reads = %d; want no new read after completion (%d)", got, readsAfterCollection)
		}
		if got := joinLogData(readAllLogs(t, fixture.sink, fixture.scope, fixture.executionID, fixture.actionID, shelllog.StreamStdout)); got != "alpha\n" {
			t.Fatalf("retained stdout = %q; want one durable copy", got)
		}
	})

	t.Run("completion_is_per_action", func(t *testing.T) {
		fixture := newLogCollectionFixture(t, pages())
		if _, err := fixture.collector.Remove(context.Background(), fixture.removeRequest()); err != nil {
			t.Fatalf("first action Remove: %v", err)
		}
		readsAfterFirst := len(fixture.runtime.logCallOffsets())

		secondExecution := id.ExecutionID("execution-2")
		secondAction := id.ActionID("action-2")
		fixture.scopes.scopes[secondExecution] = fixture.scope
		fixture.runtime.pagesByExecution[secondExecution] = map[int64]kernelengine.LogBatch{
			0: {
				Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: 0, Sequence: 1, Data: []byte("beta\n")}},
				NextOffset: 5,
			},
			5: {NextOffset: 5},
		}
		if _, err := fixture.collector.Remove(context.Background(), fixture.removeRequestFor(secondExecution, secondAction)); err != nil {
			t.Fatalf("second action Remove: %v", err)
		}
		if got := len(fixture.runtime.logCallOffsets()); got <= readsAfterFirst {
			t.Fatalf("second action log reads = %d; want a read for the action that has not completed (%d)", got, readsAfterFirst)
		}
		if got := joinLogData(readAllLogs(t, fixture.sink, fixture.scope, secondExecution, secondAction, shelllog.StreamStdout)); got != "beta\n" {
			t.Fatalf("retained second-action stdout = %q; want %q", got, "beta\n")
		}
	})

	t.Run("completion_waits_for_the_final_cursor", func(t *testing.T) {
		// The final empty page advances the cursor over a gap; the completion
		// token may only be written after that cursor is durable. Fail the third
		// save: page cursor, final cursor succeed and completion fails, leaving a
		// resumable (not complete) action.
		fixture := newLogCollectionFixture(t, map[int64]kernelengine.LogBatch{
			0: {
				Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: 0, Sequence: 1, Data: []byte("alpha\n")}},
				NextOffset: 6,
			},
			6: {NextOffset: 10},
		})
		fixture.cursors.saveFailAt = 3

		_, err := fixture.collector.Remove(context.Background(), fixture.removeRequest())
		if err == nil {
			t.Fatal("Remove succeeded although the completion token could not be saved")
		}
		if !strings.Contains(err.Error(), "cursor_unavailable") {
			t.Fatalf("diagnostic %q does not name cursor_unavailable", err.Error())
		}
		if fixture.runtime.removeCount() != 0 {
			t.Fatalf("Remove calls = %d; a lost completion write must retain cleanup", fixture.runtime.removeCount())
		}
		cursor, ok, loadErr := fixture.cursors.Load(context.Background(), fixture.executionID, fixture.actionID, fixture.workloadID())
		if loadErr != nil {
			t.Fatalf("Load: %v", loadErr)
		}
		if ok && cursor == logCollectionComplete {
			t.Fatal("the action was marked complete although the final cursor write did not succeed")
		}
		if !ok || cursor != 10 {
			t.Fatalf("saved cursor = (%d, %v); want the durable final cursor 10", cursor, ok)
		}

		// The retry resumes from the saved final cursor, writes the completion
		// token and only then removes.
		fixture.cursors.saveFailAt = 0
		before := len(fixture.runtime.logCallOffsets())
		if _, err := fixture.collector.Remove(context.Background(), fixture.removeRequest()); err != nil {
			t.Fatalf("retry Remove: %v", err)
		}
		calls := fixture.runtime.logCallOffsets()
		if before >= len(calls) || calls[before] != 10 {
			t.Fatalf("retry log reads = %v; want a resume from 10", calls[before:])
		}
		if cursor, ok, err := fixture.cursors.Load(context.Background(), fixture.executionID, fixture.actionID, fixture.workloadID()); err != nil || !ok || cursor != logCollectionComplete {
			t.Fatalf("completion cursor = (%d, %v, %v); want the completion token", cursor, ok, err)
		}
	})
}

// TestLogCollectionStateIsPerWorkloadIdentity pins per-workload state: one
// action can own more than one workload identity over its life (the engine
// retires a never-started workload and allocates a replacement for the same
// action). The retained-log cursor and completion token belong to one workload
// identity, so neither the retired identity's completion token nor its position
// may suppress or skip the replacement's output.
func TestLogCollectionStateIsPerWorkloadIdentity(t *testing.T) {
	const replacementOutput = "replacement output\n"
	retired := id.WorkloadID("workload-retired")
	replacement := id.WorkloadID("workload-replacement")

	t.Run("retired_marker_does_not_complete_its_replacement", func(t *testing.T) {
		fixture := newLogCollectionFixture(t, nil)
		fixture.runtime.errorsByWorkload = map[id.WorkloadID]error{
			retired: fault.New(fault.NotFound, "retired workload container is already gone"),
		}
		fixture.runtime.pagesByWorkload = map[id.WorkloadID]map[int64]kernelengine.LogBatch{
			replacement: {
				0: {
					Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: 0, Sequence: 1, Data: []byte(replacementOutput)}},
					NextOffset: int64(len(replacementOutput)),
				},
				int64(len(replacementOutput)): {NextOffset: int64(len(replacementOutput))},
			},
		}

		// The retired identity's container is gone: one explicit marker is
		// stored and the identity completes.
		if _, err := fixture.collector.Remove(context.Background(),
			fixture.removeRequestForWorkload(fixture.executionID, fixture.actionID, retired)); err != nil {
			t.Fatalf("retired identity Remove: %v", err)
		}
		before := len(fixture.runtime.logCallOffsets())

		// The replacement for the same (execution, action) must still be
		// collected from offset 0; the retired identity's completion token may
		// not suppress it.
		if _, err := fixture.collector.Remove(context.Background(),
			fixture.removeRequestForWorkload(fixture.executionID, fixture.actionID, replacement)); err != nil {
			t.Fatalf("replacement identity Remove: %v", err)
		}
		calls := fixture.runtime.logCallOffsets()
		if before >= len(calls) {
			t.Fatalf("replacement log reads = %v; want a read after the retired identity completed", calls)
		}
		if calls[before] != 0 {
			t.Fatalf("replacement first log read offset = %d; want 0 (the retired identity's completion did not leak)", calls[before])
		}
		stdout := joinLogData(readAllLogs(t, fixture.sink, fixture.scope, fixture.executionID, fixture.actionID, shelllog.StreamStdout))
		if want := logCollectionUnavailableMarker + replacementOutput; stdout != want {
			t.Fatalf("retained stdout = %q; want the marker and the replacement output %q", stdout, want)
		}
		if fixture.runtime.removeCount() != 2 {
			t.Fatalf("Remove calls = %d; want 2", fixture.runtime.removeCount())
		}
	})

	t.Run("retired_position_does_not_advance_its_replacement", func(t *testing.T) {
		fixture := newLogCollectionFixture(t, nil)
		// The retired identity's cursor ends at a real position (20). The
		// replacement's page is keyed at 0; reusing the retired position would
		// read at 20 and never fetch the replacement output at all.
		fixture.runtime.pagesByWorkload = map[id.WorkloadID]map[int64]kernelengine.LogBatch{
			retired: {
				0: {
					Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: 0, Sequence: 1, Data: []byte("retired output\n")}},
					NextOffset: 20,
				},
				20: {NextOffset: 20},
			},
			replacement: {
				0: {
					Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: 30, Sequence: 1, Data: []byte(replacementOutput)}},
					NextOffset: 30 + int64(len(replacementOutput)),
				},
				30 + int64(len(replacementOutput)): {NextOffset: 30 + int64(len(replacementOutput))},
			},
		}

		if _, err := fixture.collector.Remove(context.Background(),
			fixture.removeRequestForWorkload(fixture.executionID, fixture.actionID, retired)); err != nil {
			t.Fatalf("retired identity Remove: %v", err)
		}
		before := len(fixture.runtime.logCallOffsets())
		if _, err := fixture.collector.Remove(context.Background(),
			fixture.removeRequestForWorkload(fixture.executionID, fixture.actionID, replacement)); err != nil {
			t.Fatalf("replacement identity Remove: %v", err)
		}
		calls := fixture.runtime.logCallOffsets()
		if before >= len(calls) {
			t.Fatalf("replacement log reads = %v; want a read after the retired identity completed", calls)
		}
		if calls[before] != 0 {
			t.Fatalf("replacement first log read offset = %d; want 0 (the retired position did not leak)", calls[before])
		}
		stdout := joinLogData(readAllLogs(t, fixture.sink, fixture.scope, fixture.executionID, fixture.actionID, shelllog.StreamStdout))
		if want := "retired output\n" + replacementOutput; stdout != want {
			t.Fatalf("retained stdout = %q; want %q", stdout, want)
		}
	})

	t.Run("durable_file_store_keeps_the_replacement_separate", func(t *testing.T) {
		fixture := newLogCollectionFixture(t, nil)
		fixture.runtime.errorsByWorkload = map[id.WorkloadID]error{
			retired: fault.New(fault.NotFound, "retired workload container is already gone"),
		}
		fixture.runtime.pagesByWorkload = map[id.WorkloadID]map[int64]kernelengine.LogBatch{
			replacement: {
				0: {
					Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: 0, Sequence: 1, Data: []byte(replacementOutput)}},
					NextOffset: int64(len(replacementOutput)),
				},
				int64(len(replacementOutput)): {NextOffset: int64(len(replacementOutput))},
			},
		}
		store, err := newFileLogCursorStore(t.TempDir())
		if err != nil {
			t.Fatalf("newFileLogCursorStore: %v", err)
		}
		collector, err := newLogCollectingRuntime(logCollectionConfig{
			Inner:   fixture.runtime,
			Sink:    fixture.sink,
			Scopes:  fixture.scopes,
			Cursors: store,
		})
		if err != nil {
			t.Fatalf("newLogCollectingRuntime: %v", err)
		}

		if _, err := collector.Remove(context.Background(),
			fixture.removeRequestForWorkload(fixture.executionID, fixture.actionID, retired)); err != nil {
			t.Fatalf("retired identity Remove: %v", err)
		}
		before := len(fixture.runtime.logCallOffsets())
		if _, err := collector.Remove(context.Background(),
			fixture.removeRequestForWorkload(fixture.executionID, fixture.actionID, replacement)); err != nil {
			t.Fatalf("replacement identity Remove: %v", err)
		}
		calls := fixture.runtime.logCallOffsets()
		if before >= len(calls) || calls[before] != 0 {
			t.Fatalf("replacement log reads = %v; want a read from 0", calls)
		}
		stdout := joinLogData(readAllLogs(t, fixture.sink, fixture.scope, fixture.executionID, fixture.actionID, shelllog.StreamStdout))
		if want := logCollectionUnavailableMarker + replacementOutput; stdout != want {
			t.Fatalf("retained stdout = %q; want %q", stdout, want)
		}
		for _, workloadID := range []id.WorkloadID{retired, replacement} {
			if cursor, ok, err := store.Load(context.Background(), fixture.executionID, fixture.actionID, workloadID); err != nil || !ok || cursor != logCollectionComplete {
				t.Fatalf("workload %q cursor = (%d, %v, %v); want its own completion token", workloadID, cursor, ok, err)
			}
		}
	})

	t.Run("retry_of_a_completed_identity_still_skips_scope_and_log_reads", func(t *testing.T) {
		fixture := newLogCollectionFixture(t, nil)
		fixture.runtime.errorsByWorkload = map[id.WorkloadID]error{
			retired: fault.New(fault.NotFound, "retired workload container is already gone"),
		}
		if _, err := fixture.collector.Remove(context.Background(),
			fixture.removeRequestForWorkload(fixture.executionID, fixture.actionID, retired)); err != nil {
			t.Fatalf("retired identity Remove: %v", err)
		}
		reads := len(fixture.runtime.logCallOffsets())
		scopeCalls := fixture.scopes.calls

		// A cleanup retry whose destructive Remove already deleted the log
		// source must not consult the scope or the log source again.
		fixture.runtime.logsErr = fault.New(fault.DependencyUnavailable, "redaction source is unavailable; log read refused")
		if _, err := fixture.collector.Remove(context.Background(),
			fixture.removeRequestForWorkload(fixture.executionID, fixture.actionID, retired)); err != nil {
			t.Fatalf("retry Remove: %v", err)
		}
		if got := len(fixture.runtime.logCallOffsets()); got != reads {
			t.Fatalf("retry log reads = %d; want no new read after completion (%d)", got, reads)
		}
		if fixture.scopes.calls != scopeCalls {
			t.Fatalf("retry scope reads = %d; want no new scope read after completion (%d)", fixture.scopes.calls, scopeCalls)
		}
		if fixture.runtime.removeCount() != 2 {
			t.Fatalf("Remove calls = %d; want 2", fixture.runtime.removeCount())
		}
	})
}

// TestRetainedLogFailureCauseVocabulary pins the bounded, secret-free vocabulary
// a stuck cleanup's LastError uses to name the failed inner operation.
func TestRetainedLogFailureCauseVocabulary(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{name: "pod list", err: errors.Join(errors.New("body s3cr3t"), runtimekube.ErrPodListUnavailable), want: "pods_list_failed"},
		{name: "pod log not found", err: errors.Join(runtimekube.ErrNotFound, runtimekube.ErrPodLogNotFound), want: "pod_log_not_found"},
		{name: "pod log bad request", err: errors.Join(errors.New("body s3cr3t"), runtimekube.ErrPodLogBadRequest), want: "pod_log_bad_request"},
		{name: "pod log server error", err: errors.Join(errors.New("body s3cr3t"), runtimekube.ErrPodLogServerError), want: "pod_log_server_error"},
		{name: "redaction source", err: errors.Join(errors.New("secret name"), runtimekube.ErrRedactionSourceUnavailable), want: "redaction_source_missing"},
		{name: "unknown failure", err: errors.New("transport reset"), want: "log_read_failed"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := retainedLogFailureCause(testCase.err); got != testCase.want {
				t.Fatalf("cause = %q; want %q", got, testCase.want)
			}
		})
	}
}

// TestLogCollectionFailureNamesBoundedCause proves every retryable collection
// fault names its failed operation with a fixed token and never repeats a
// response body, secret value or object name.
func TestLogCollectionFailureNamesBoundedCause(t *testing.T) {
	pages := map[int64]kernelengine.LogBatch{
		0: {
			Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: 0, Sequence: 1, Data: []byte("alpha\n")}},
			NextOffset: 6,
		},
	}
	cases := []struct {
		name    string
		arrange func(*logCollectionFixture)
		want    string
	}{
		{
			name: "scope",
			arrange: func(fixture *logCollectionFixture) {
				fixture.scopes.err = fault.New(fault.DependencyUnavailable, "execution scope is unavailable")
			},
			want: "scope_unavailable",
		},
		{
			name: "cursor load",
			arrange: func(fixture *logCollectionFixture) {
				fixture.cursors.loadErr = fault.New(fault.DependencyUnavailable, "retained log cursor is unavailable")
			},
			want: "cursor_unavailable",
		},
		{
			name: "cursor save",
			arrange: func(fixture *logCollectionFixture) {
				fixture.runtime.pages = pages
				fixture.cursors.saveErr = fault.New(fault.DependencyUnavailable, "retained log cursor is unavailable")
			},
			want: "cursor_unavailable",
		},
		{
			name: "sink",
			arrange: func(fixture *logCollectionFixture) {
				fixture.runtime.pages = pages
				fixture.sink.appendErr = fault.New(fault.DependencyUnavailable, "retained log sink is unavailable")
			},
			want: "sink_unavailable",
		},
		{
			name: "pod list",
			arrange: func(fixture *logCollectionFixture) {
				fixture.runtime.logsErr = fault.Wrap(fault.DependencyUnavailable, "pod state is unreachable", errors.Join(errors.New("body s3cr3t"), runtimekube.ErrPodListUnavailable))
			},
			want: "pods_list_failed",
		},
		{
			name: "pod log rejected",
			arrange: func(fixture *logCollectionFixture) {
				fixture.runtime.logsErr = fault.Wrap(fault.DependencyUnavailable, "pod log read is unavailable", errors.Join(errors.New("body s3cr3t"), runtimekube.ErrPodLogBadRequest))
			},
			want: "pod_log_bad_request",
		},
		{
			name: "pod log server",
			arrange: func(fixture *logCollectionFixture) {
				fixture.runtime.logsErr = fault.Wrap(fault.DependencyUnavailable, "pod log read is unavailable", errors.Join(errors.New("body s3cr3t"), runtimekube.ErrPodLogServerError))
			},
			want: "pod_log_server_error",
		},
		{
			name: "redaction source",
			arrange: func(fixture *logCollectionFixture) {
				fixture.runtime.logsErr = fault.Wrap(fault.DependencyUnavailable, "redaction source is unavailable; log read refused", errors.Join(errors.New("credential tok-abc"), runtimekube.ErrRedactionSourceUnavailable))
			},
			want: "redaction_source_missing",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newLogCollectionFixture(t, nil)
			testCase.arrange(fixture)

			_, err := fixture.collector.Remove(context.Background(), fixture.removeRequest())
			if err == nil {
				t.Fatalf("Remove succeeded; want a retryable %s fault", testCase.want)
			}
			class, ok := fault.ClassOf(err)
			if !ok || class != fault.DependencyUnavailable {
				t.Fatalf("fault class = %v (present=%v), error %v; want %q", class, ok, err, fault.DependencyUnavailable)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("diagnostic %q does not name %q", err.Error(), testCase.want)
			}
			for _, leaked := range []string{"s3cr3t", "tok-abc", "credential"} {
				if strings.Contains(err.Error(), leaked) {
					t.Fatalf("diagnostic %q leaks %q", err.Error(), leaked)
				}
			}
			if fixture.runtime.removeCount() != 0 {
				t.Fatalf("Remove calls = %d; a collection fault must retain cleanup", fixture.runtime.removeCount())
			}
		})
	}
}

func TestLogCollectionBoundedAndRedacted(t *testing.T) {
	const pageBytes = 32
	secret := "s3cr3t"
	// The secret starts at chunk byte 31 and is split by the 32-byte page
	// bound, so the sink's cross-chunk redaction is exercised.
	data := []byte("0123456789abcdefghijklmnopqrstu" + secret + "XYZ")
	if len(data) != 40 {
		t.Fatalf("fixture data is %d bytes; want 40", len(data))
	}
	// The sink keeps only two chunks per stream, so collecting three evicts the
	// oldest and makes ReadResult.Truncated the explicit truncation signal.
	fixture := newLogCollectionFixtureWithSink(t, map[int64]kernelengine.LogBatch{
		0: {
			Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: 10, Sequence: 1, Data: data}},
			NextOffset: 50,
		},
		50: {
			Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: 50, Sequence: 2, Data: []byte("tail\n")}},
			NextOffset: 55,
		},
		55: {NextOffset: 55},
	}, shelllog.SinkConfig{
		Limits: shelllog.Limits{MaxChunkBytes: pageBytes, MaxStoredChunks: 2},
		Redact: []string{secret},
	}, pageBytes)

	if _, err := fixture.collector.Remove(context.Background(), fixture.removeRequest()); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	for index, limit := range fixture.runtime.logLimits {
		if limit != pageBytes {
			t.Fatalf("log read %d limit = %d; want the bounded page size %d", index, limit, pageBytes)
		}
	}
	for index, chunk := range fixture.sink.appendedChunks() {
		if len(chunk.Data) > pageBytes {
			t.Fatalf("appended chunk %d is %d bytes; the sink bound is %d", index, len(chunk.Data), pageBytes)
		}
	}

	chunks, truncated := readLogs(t, fixture.sink, fixture.scope, fixture.executionID, fixture.actionID, shelllog.StreamStdout)
	if !truncated {
		t.Fatalf("retained stdout was not reported as truncated after the sink evicted the oldest chunk")
	}
	stdout := joinLogData(chunks)
	if containsMaterial(stdout, secret) {
		t.Fatalf("retained stdout leaked the configured secret: %q", stdout)
	}
	if !strings.Contains(stdout, "[REDACTED]") {
		t.Fatalf("retained stdout %q has no redaction marker", stdout)
	}
	if !strings.Contains(stdout, "tail\n") {
		t.Fatalf("retained stdout %q lost the newest output", stdout)
	}
	if strings.Contains(stdout, "0123456789") {
		t.Fatalf("retained stdout %q still contains the evicted oldest chunk", stdout)
	}
}

func TestLogCollectionUnavailableContainer(t *testing.T) {
	t.Run("not_found_records_unavailable_and_removes", func(t *testing.T) {
		fixture := newLogCollectionFixture(t, nil)
		fixture.runtime.logsErr = fault.New(fault.NotFound, "workload container is already gone")

		removal, err := fixture.collector.Remove(context.Background(), fixture.removeRequest())
		if err != nil {
			t.Fatalf("Remove returned error for an already-gone container: %v", err)
		}
		if !removal.Removed {
			t.Fatal("Remove did not confirm removal of an already-gone container")
		}
		if fixture.runtime.removeCalls != 1 {
			t.Fatalf("Remove calls = %d; want 1", fixture.runtime.removeCalls)
		}
		stdout := readAllLogs(t, fixture.sink, fixture.scope, fixture.executionID, fixture.actionID, shelllog.StreamStdout)
		if len(stdout) != 1 || string(stdout[0].Data) != logCollectionUnavailableMarker {
			t.Fatalf("retained stdout = %+v; want one explicit unavailability marker", stdout)
		}
		if cursor, ok, err := fixture.cursors.Load(context.Background(), fixture.executionID, fixture.actionID, fixture.workloadID()); err != nil || !ok || cursor != logCollectionComplete {
			t.Fatalf("marker cursor = (%d, %v, %v); want the durable completion token", cursor, ok, err)
		}
		if stderr := readAllLogs(t, fixture.sink, fixture.scope, fixture.executionID, fixture.actionID, shelllog.StreamStderr); len(stderr) != 0 {
			t.Fatalf("retained stderr = %+v; want no marker", stderr)
		}

		// A retry after a lost cleanup commit must not duplicate the marker and
		// must still not block removal.
		if _, err := fixture.collector.Remove(context.Background(), fixture.removeRequest()); err != nil {
			t.Fatalf("retry Remove: %v", err)
		}
		if fixture.runtime.removeCalls != 2 {
			t.Fatalf("Remove calls after retry = %d; want 2", fixture.runtime.removeCalls)
		}
		stdout = readAllLogs(t, fixture.sink, fixture.scope, fixture.executionID, fixture.actionID, shelllog.StreamStdout)
		if len(stdout) != 1 {
			t.Fatalf("retained stdout after retry = %+v; want exactly one unavailability marker", stdout)
		}
	})

	t.Run("not_found_after_partial_collection_does_not_mark_unavailable", func(t *testing.T) {
		fixture := newLogCollectionFixture(t, map[int64]kernelengine.LogBatch{
			0: {
				Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: 0, Sequence: 1, Data: []byte("partial\n")}},
				NextOffset: 8,
			},
		})
		fixture.runtime.errorsByOffset = map[int64]error{
			8: fault.New(fault.NotFound, "workload container disappeared during collection"),
		}

		if _, err := fixture.collector.Remove(context.Background(), fixture.removeRequest()); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		if fixture.runtime.removeCalls != 1 {
			t.Fatalf("Remove calls = %d; want 1", fixture.runtime.removeCalls)
		}
		stdout := joinLogData(readAllLogs(t, fixture.sink, fixture.scope, fixture.executionID, fixture.actionID, shelllog.StreamStdout))
		if stdout != "partial\n" {
			t.Fatalf("retained stdout = %q; want the already-collected output", stdout)
		}
		if strings.Contains(stdout, logCollectionUnavailableMarker) {
			t.Fatalf("retained stdout %q marked a partially collected action as unavailable", stdout)
		}
	})

	t.Run("transient_failure_blocks_removal", func(t *testing.T) {
		fixture := newLogCollectionFixture(t, nil)
		fixture.runtime.logsErr = fault.New(fault.DependencyUnavailable, "runtime log read is unavailable")
		if _, err := fixture.collector.Remove(context.Background(), fixture.removeRequest()); err == nil {
			t.Fatal("Remove succeeded although the log read was transiently unavailable")
		}
		if fixture.runtime.removeCalls != 0 {
			t.Fatalf("Remove calls = %d; a transient log failure must retain cleanup", fixture.runtime.removeCalls)
		}
	})
}

func TestLogCollectionScopedToOwner(t *testing.T) {
	t.Run("stores_under_the_owning_scope", func(t *testing.T) {
		fixture := newLogCollectionFixture(t, nil)
		secondScope := tenancy.Scope{TenantID: "tenant-1", ProjectID: "project-2"}
		secondExecution := id.ExecutionID("execution-2")
		secondAction := id.ActionID("action-2")
		fixture.scopes.scopes[secondExecution] = secondScope
		fixture.runtime.pagesByExecution[fixture.executionID] = map[int64]kernelengine.LogBatch{
			0: {
				Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: 0, Sequence: 1, Data: []byte("one\n")}},
				NextOffset: 4,
			},
			4: {NextOffset: 4},
		}
		fixture.runtime.pagesByExecution[secondExecution] = map[int64]kernelengine.LogBatch{
			0: {
				Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: 0, Sequence: 1, Data: []byte("two\n")}},
				NextOffset: 4,
			},
			4: {NextOffset: 4},
		}

		if _, err := fixture.collector.Remove(context.Background(), fixture.removeRequestFor(fixture.executionID, fixture.actionID)); err != nil {
			t.Fatalf("Remove execution-1: %v", err)
		}
		if _, err := fixture.collector.Remove(context.Background(), fixture.removeRequestFor(secondExecution, secondAction)); err != nil {
			t.Fatalf("Remove execution-2: %v", err)
		}

		scopes := fixture.sink.appendedScopes()
		chunks := fixture.sink.appendedChunks()
		if len(scopes) != len(chunks) {
			t.Fatalf("recorded %d scopes for %d appends", len(scopes), len(chunks))
		}
		for index, scope := range scopes {
			if string(chunks[index].ExecutionID) == string(fixture.executionID) && scope != fixture.scope {
				t.Fatalf("execution-1 append %d scope = %+v; want %+v", index, scope, fixture.scope)
			}
			if string(chunks[index].ExecutionID) == string(secondExecution) && scope != secondScope {
				t.Fatalf("execution-2 append %d scope = %+v; want %+v", index, scope, secondScope)
			}
		}

		if got := joinLogData(readAllLogs(t, fixture.sink, fixture.scope, fixture.executionID, fixture.actionID, shelllog.StreamStdout)); got != "one\n" {
			t.Fatalf("project-1 execution-1 stdout = %q; want %q", got, "one\n")
		}
		if got := readAllLogs(t, fixture.sink, fixture.scope, secondExecution, secondAction, shelllog.StreamStdout); len(got) != 0 {
			t.Fatalf("project-1 read saw another project's execution logs: %+v", got)
		}
		if got := joinLogData(readAllLogs(t, fixture.sink, secondScope, secondExecution, secondAction, shelllog.StreamStdout)); got != "two\n" {
			t.Fatalf("project-2 execution-2 stdout = %q; want %q", got, "two\n")
		}
		if got := readAllLogs(t, fixture.sink, secondScope, fixture.executionID, fixture.actionID, shelllog.StreamStdout); len(got) != 0 {
			t.Fatalf("project-2 read saw another project's execution logs: %+v", got)
		}
	})

	t.Run("scope_lookup_failure_never_appends", func(t *testing.T) {
		failing := newLogCollectionFixture(t, nil)
		failing.scopes.err = fault.New(fault.DependencyUnavailable, "execution scope is unavailable")
		if _, err := failing.collector.Remove(context.Background(), failing.removeRequest()); err == nil {
			t.Fatal("Remove succeeded without an owning scope")
		}
		if len(failing.runtime.logCalls) != 0 || len(failing.sink.appendedChunks()) != 0 {
			t.Fatalf("scope-less collection read %v pages and appended %d chunks", failing.runtime.logCalls, len(failing.sink.appendedChunks()))
		}
		if failing.runtime.removeCalls != 0 {
			t.Fatalf("Remove calls = %d; cleanup must stay retained", failing.runtime.removeCalls)
		}
	})
}

// TestLogCollectionPreservesPrefixAcrossDestructiveStop pins the product race:
// Kubernetes Stop suspends the Job, and the controller then deletes the active
// Pod along with the only readable container log. The decorator must make the
// bytes that were readable before the destructive effect durable first, keep
// them when the source is gone, and still drain any readable tail at Remove.
func TestLogCollectionPreservesPrefixAcrossDestructiveStop(t *testing.T) {
	const output = "ready-marker\n"

	t.Run("prefix_is_durable_before_suspension_destroys_the_source", func(t *testing.T) {
		fixture := newLogCollectionFixture(t, map[int64]kernelengine.LogBatch{
			0: {
				Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: 0, Sequence: 1, Data: []byte(output)}},
				NextOffset: int64(len(output)),
			},
			int64(len(output)): {NextOffset: int64(len(output))},
		})

		// At the instant the destructive suspension effect runs, a reader of
		// the retained sink must already return the produced bytes.
		durableAtStop := ""
		fixture.runtime.onStop = func() {
			durableAtStop = joinLogData(readAllLogs(t, fixture.sink, fixture.scope, fixture.executionID, fixture.actionID, shelllog.StreamStdout))
			fixture.runtime.destroyLogSource()
		}

		evidence, err := fixture.collector.Stop(context.Background(), fixture.stopRequest())
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if !evidence.IsConfirmed() {
			t.Fatalf("stop evidence = %+v; want confirmed", evidence)
		}
		if got := fixture.runtime.stopCountValue(); got != 1 {
			t.Fatalf("inner Stop calls = %d; want exactly 1", got)
		}
		if durableAtStop != output {
			t.Fatalf("retained stdout at the destructive stop = %q; want the already-produced bytes %q", durableAtStop, output)
		}
		if firstAppend, firstStop := fixture.order.first("append"), fixture.order.first("stop"); firstAppend < 0 || firstStop < 0 || firstAppend >= firstStop {
			t.Fatalf("effect order %v did not append before the destructive stop", fixture.order.snapshot())
		}
		// A live prefix checkpoint is not completion: the source may still have
		// an unread tail when the suspension is only acknowledged.
		if cursor, ok, err := fixture.cursors.Load(context.Background(), fixture.executionID, fixture.actionID, fixture.workloadID()); err != nil || ok && cursor == logCollectionComplete {
			t.Fatalf("cursor after the live checkpoint = (%d, %v, %v); want a resumable position, not completion", cursor, ok, err)
		}

		// A restart loses nothing: the durable sink and cursor are the source of
		// truth, and Remove still collects from the source (now gone).
		restarted, err := newLogCollectingRuntime(logCollectionConfig{
			Inner:   fixture.runtime,
			Sink:    fixture.sink,
			Scopes:  fixture.scopes,
			Cursors: fixture.cursors,
		})
		if err != nil {
			t.Fatalf("newLogCollectingRuntime after restart: %v", err)
		}
		removal, err := restarted.Remove(context.Background(), fixture.removeRequest())
		if err != nil {
			t.Fatalf("Remove after a destructive stop: %v", err)
		}
		if !removal.Removed {
			t.Fatal("Remove did not confirm removal after a destructive stop")
		}
		stdout := joinLogData(readAllLogs(t, fixture.sink, fixture.scope, fixture.executionID, fixture.actionID, shelllog.StreamStdout))
		if stdout != output {
			t.Fatalf("retained stdout = %q; want exactly the pre-stop bytes %q", stdout, output)
		}
		if got := strings.Count(stdout, output); got != 1 {
			t.Fatalf("retained stdout contains the produced prefix %d times; want exactly 1", got)
		}
		if strings.Contains(stdout, logCollectionUnavailableMarker) {
			t.Fatalf("retained stdout = %q; an unavailable placeholder replaced the produced bytes", stdout)
		}
		if cursor, ok, err := fixture.cursors.Load(context.Background(), fixture.executionID, fixture.actionID, fixture.workloadID()); err != nil || !ok || cursor != logCollectionComplete {
			t.Fatalf("cursor after Remove = (%d, %v, %v); want the completion token", cursor, ok, err)
		}
	})

	t.Run("readable_tail_is_drained_after_the_stop_snapshot", func(t *testing.T) {
		const tail = "term-tail\n"
		pages := map[int64]kernelengine.LogBatch{
			0: {
				Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: 0, Sequence: 1, Data: []byte(output)}},
				NextOffset: int64(len(output)),
			},
		}
		fixture := newLogCollectionFixture(t, pages)
		// The workload keeps writing while it handles termination. The tail is
		// not visible to the pre-stop snapshot (which saw a temporary EOF) and
		// must be drained at Remove, not lost and not marked unavailable.
		fixture.runtime.onStop = func() {
			fixture.runtime.appendPage(int64(len(output)), kernelengine.LogBatch{
				Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: int64(len(output)), Sequence: 2, Data: []byte(tail)}},
				NextOffset: int64(len(output) + len(tail)),
			})
			fixture.runtime.appendPage(int64(len(output)+len(tail)), kernelengine.LogBatch{NextOffset: int64(len(output) + len(tail))})
		}

		if _, err := fixture.collector.Stop(context.Background(), fixture.stopRequest()); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if got := joinLogData(readAllLogs(t, fixture.sink, fixture.scope, fixture.executionID, fixture.actionID, shelllog.StreamStdout)); got != output {
			t.Fatalf("retained stdout after the pre-stop snapshot = %q; want only the pre-stop bytes %q", got, output)
		}

		// Restart the decorator over the same durable sink and cursor, and lose
		// one cursor write while draining the tail.
		restarted, err := newLogCollectingRuntime(logCollectionConfig{
			Inner:   fixture.runtime,
			Sink:    fixture.sink,
			Scopes:  fixture.scopes,
			Cursors: fixture.cursors,
		})
		if err != nil {
			t.Fatalf("newLogCollectingRuntime after restart: %v", err)
		}
		fixture.cursors.saveFailures = 1
		if _, err := restarted.Remove(context.Background(), fixture.removeRequest()); err == nil {
			t.Fatal("Remove succeeded although the tail cursor write was lost")
		}
		if fixture.runtime.removeCount() != 0 {
			t.Fatalf("Remove calls = %d; a lost cursor write must retain cleanup", fixture.runtime.removeCount())
		}
		if _, err := restarted.Remove(context.Background(), fixture.removeRequest()); err != nil {
			t.Fatalf("retry Remove: %v", err)
		}
		stdout := joinLogData(readAllLogs(t, fixture.sink, fixture.scope, fixture.executionID, fixture.actionID, shelllog.StreamStdout))
		if want := output + tail; stdout != want {
			t.Fatalf("retained stdout = %q; want the prefix and the tail exactly once %q", stdout, want)
		}
		if got := strings.Count(stdout, output); got != 1 {
			t.Fatalf("prefix count = %d; want 1", got)
		}
		if got := strings.Count(stdout, tail); got != 1 {
			t.Fatalf("tail count = %d; want 1", got)
		}
		if strings.Contains(stdout, logCollectionUnavailableMarker) {
			t.Fatalf("retained stdout = %q; a readable tail was replaced by an unavailable placeholder", stdout)
		}
	})

	t.Run("a_known_secret_split_across_the_checkpoint_stays_redacted", func(t *testing.T) {
		const secret = "supersecret"
		const prefix = "head superse" // ends inside the configured value
		const tail = "cret tail\n"    // completes it on the other side of a temporary EOF
		fixture := newLogCollectionFixtureWithSink(t, map[int64]kernelengine.LogBatch{
			0: {
				Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: 0, Sequence: 1, Data: []byte(prefix)}},
				NextOffset: int64(len(prefix)),
			},
		}, shelllog.SinkConfig{Redact: []string{secret}}, 0)

		durableAtStop := ""
		fixture.runtime.onStop = func() {
			durableAtStop = joinLogData(readAllLogs(t, fixture.sink, fixture.scope, fixture.executionID, fixture.actionID, shelllog.StreamStdout))
			fixture.runtime.appendPage(int64(len(prefix)), kernelengine.LogBatch{
				Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: int64(len(prefix)), Sequence: 2, Data: []byte(tail)}},
				NextOffset: int64(len(prefix) + len(tail)),
			})
			fixture.runtime.appendPage(int64(len(prefix)+len(tail)), kernelengine.LogBatch{NextOffset: int64(len(prefix) + len(tail))})
		}

		if _, err := fixture.collector.Stop(context.Background(), fixture.stopRequest()); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if firstLogs, firstStop := fixture.order.first("logs"), fixture.order.first("stop"); firstLogs < 0 || firstStop < 0 || firstLogs >= firstStop {
			t.Fatalf("effect order %v did not snapshot before the destructive stop", fixture.order.snapshot())
		}
		// The sink holds back a suffix that could be the start of the configured
		// value; a temporary EOF must not flush it as plaintext.
		if strings.Contains(durableAtStop, "superse") {
			t.Fatalf("retained stdout at the temporary EOF = %q; a partial known secret was stored", durableAtStop)
		}

		// Restart over the same durable sink state: the held prefix and the
		// final tail must be redacted as one value.
		restarted, err := newLogCollectingRuntime(logCollectionConfig{
			Inner:   fixture.runtime,
			Sink:    fixture.sink,
			Scopes:  fixture.scopes,
			Cursors: fixture.cursors,
		})
		if err != nil {
			t.Fatalf("newLogCollectingRuntime after restart: %v", err)
		}
		if _, err := restarted.Remove(context.Background(), fixture.removeRequest()); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		stdout := joinLogData(readAllLogs(t, fixture.sink, fixture.scope, fixture.executionID, fixture.actionID, shelllog.StreamStdout))
		if want := "head [REDACTED] tail\n"; stdout != want {
			t.Fatalf("retained stdout = %q; want %q", stdout, want)
		}
		if containsMaterial(stdout, secret) {
			t.Fatalf("retained stdout leaked the configured secret split across the boundary: %q", stdout)
		}
	})

	t.Run("a_complete_marker_line_survives_a_trailing_partial", func(t *testing.T) {
		const secret = "supersecretvalue"
		const output = "ready-marker\n"
		const partial = "head superse"
		const completed = "head supersecretvalue tail\n"
		fixture := newLogCollectionFixtureWithSink(t, map[int64]kernelengine.LogBatch{
			0: {
				Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: 0, Sequence: 1, Data: []byte(output + partial)}},
				NextOffset: int64(len(output + partial)),
			},
			int64(len(output)): {
				Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: int64(len(output)), Sequence: 2, Data: []byte(partial)}},
				NextOffset: int64(len(output) + len(partial)),
			},
		}, shelllog.SinkConfig{Redact: []string{secret}}, 0)
		fixture.runtime.limitPages = true

		durableAtStop := ""
		fixture.runtime.onStop = func() {
			durableAtStop = joinLogData(readAllLogs(t, fixture.sink, fixture.scope, fixture.executionID, fixture.actionID, shelllog.StreamStdout))
			// The workload finishes the line after the checkpoint.
			fixture.runtime.appendPage(int64(len(output)), kernelengine.LogBatch{
				Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: int64(len(output)), Sequence: 2, Data: []byte(completed)}},
				NextOffset: int64(len(output) + len(completed)),
			})
		}

		if _, err := fixture.collector.Stop(context.Background(), fixture.stopRequest()); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		// The complete marker line is retained even though the page ended in a
		// partial credential, and the partial itself is not stored.
		if durableAtStop != output {
			t.Fatalf("retained stdout at the stop snapshot = %q; want the complete marker line %q", durableAtStop, output)
		}
		if strings.Contains(durableAtStop, "superse") {
			t.Fatalf("retained stdout at the stop snapshot contains a partial credential prefix; got %d bytes", len(durableAtStop))
		}

		restarted, err := newLogCollectingRuntime(logCollectionConfig{
			Inner:   fixture.runtime,
			Sink:    fixture.sink,
			Scopes:  fixture.scopes,
			Cursors: fixture.cursors,
		})
		if err != nil {
			t.Fatalf("newLogCollectingRuntime after restart: %v", err)
		}
		if _, err := restarted.Remove(context.Background(), fixture.removeRequest()); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		stdout := joinLogData(readAllLogs(t, fixture.sink, fixture.scope, fixture.executionID, fixture.actionID, shelllog.StreamStdout))
		if want := output + "head [REDACTED] tail\n"; stdout != want {
			t.Fatalf("retained stdout = %q; want %q", stdout, want)
		}
		if containsMaterial(stdout, secret) {
			t.Fatalf("retained stdout leaked the configured secret across the checkpoint boundary")
		}
	})

	t.Run("the_live_snapshot_is_bounded", func(t *testing.T) {
		const page = 8
		const pagesServed = 200
		const maxSnapshotReads = 64
		fixture := newLogCollectionFixtureWithSink(t, nil, shelllog.SinkConfig{}, page)
		for index := range pagesServed {
			offset := int64(index * page)
			fixture.runtime.appendPage(offset, kernelengine.LogBatch{
				Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: offset, Sequence: int64(index + 1), Data: []byte("abcdefg\n")}},
				NextOffset: offset + page,
			})
		}

		if _, err := fixture.collector.Stop(context.Background(), fixture.stopRequest()); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		reads := len(fixture.runtime.logCallOffsets())
		if reads != maxSnapshotReads {
			t.Fatalf("live snapshot reads = %d; want exactly the bounded budget %d", reads, maxSnapshotReads)
		}
		for index, limit := range fixture.runtime.logLimitValues() {
			if limit != page {
				t.Fatalf("snapshot read %d limit = %d; want the bounded page size %d", index, limit, page)
			}
		}
		if fixture.runtime.stopCountValue() != 1 {
			t.Fatalf("inner Stop calls = %d; want the stop to proceed after the bounded snapshot", fixture.runtime.stopCountValue())
		}
	})

	t.Run("an_absent_log_source_does_not_block_the_stop", func(t *testing.T) {
		fixture := newLogCollectionFixture(t, nil)
		fixture.runtime.logsErr = fault.New(fault.NotFound, "workload container is already gone")

		evidence, err := fixture.collector.Stop(context.Background(), fixture.stopRequest())
		if err != nil {
			t.Fatalf("Stop for an already-gone container: %v", err)
		}
		if !evidence.IsConfirmed() {
			t.Fatalf("stop evidence = %+v; want confirmed", evidence)
		}
		if fixture.runtime.stopCountValue() != 1 {
			t.Fatalf("inner Stop calls = %d; want the stop to proceed", fixture.runtime.stopCountValue())
		}
		if got := len(fixture.sink.appendedChunks()); got != 0 {
			t.Fatalf("appends during the stop snapshot = %d; an absent source must not invent bytes", got)
		}
		if cursor, ok, err := fixture.cursors.Load(context.Background(), fixture.executionID, fixture.actionID, fixture.workloadID()); err != nil || ok {
			t.Fatalf("cursor after an absent source = (%d, %v, %v); want no completion token", cursor, ok, err)
		}
	})

	t.Run("a_not_yet_started_workload_does_not_block_the_stop", func(t *testing.T) {
		fixture := newLogCollectionFixture(t, nil)
		// The Kubernetes adapter reports a container still waiting to start as a
		// rejected pod-log request. Nothing can have been produced, so the stop
		// must proceed instead of waiting for output that cannot exist.
		fixture.runtime.logsErr = fault.Wrap(fault.DependencyUnavailable, "pod log read was rejected",
			errors.Join(errors.New("container is waiting to start"), runtimekube.ErrPodLogBadRequest))

		evidence, err := fixture.collector.Stop(context.Background(), fixture.stopRequest())
		if err != nil {
			t.Fatalf("Stop for a not-yet-started workload: %v", err)
		}
		if !evidence.IsConfirmed() {
			t.Fatalf("stop evidence = %+v; want confirmed", evidence)
		}
		if fixture.runtime.stopCountValue() != 1 {
			t.Fatalf("inner Stop calls = %d; want the stop to proceed", fixture.runtime.stopCountValue())
		}
		if got := len(fixture.sink.appendedChunks()); got != 0 {
			t.Fatalf("appends during the stop snapshot = %d; a waiting container has no output", got)
		}
	})

	t.Run("checkpoint_failure_never_reaches_the_destructive_stop", func(t *testing.T) {
		fixture := newLogCollectionFixture(t, map[int64]kernelengine.LogBatch{
			0: {
				Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: 0, Sequence: 1, Data: []byte(output)}},
				NextOffset: int64(len(output)),
			},
		})
		fixture.runtime.logsErr = fault.New(fault.DependencyUnavailable, "runtime log read is unavailable")

		_, err := fixture.collector.Stop(context.Background(), fixture.stopRequest())
		if err == nil {
			t.Fatal("Stop succeeded although the retained prefix could not be read")
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
			t.Fatalf("Stop error class = %v; want dependency_unavailable", class)
		}
		if fixture.runtime.stopCountValue() != 0 {
			t.Fatalf("inner Stop calls = %d; the destructive effect must not run before durable retention", fixture.runtime.stopCountValue())
		}
		if len(fixture.sink.appendedChunks()) != 0 {
			t.Fatalf("appends = %d; a failed read must not store partial output", len(fixture.sink.appendedChunks()))
		}
	})

	t.Run("a_failed_append_never_saves_the_cursor", func(t *testing.T) {
		fixture := newLogCollectionFixture(t, map[int64]kernelengine.LogBatch{
			0: {
				Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: 0, Sequence: 1, Data: []byte(output)}},
				NextOffset: int64(len(output)),
			},
		})
		fixture.sink.appendFailures = 1

		_, err := fixture.collector.Stop(context.Background(), fixture.stopRequest())
		if err == nil {
			t.Fatal("Stop succeeded although the checkpoint append failed")
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
			t.Fatalf("Stop error class = %v; want dependency_unavailable", class)
		}
		if fixture.runtime.stopCountValue() != 0 {
			t.Fatalf("inner Stop calls = %d; the destructive effect must not run before the append succeeds", fixture.runtime.stopCountValue())
		}
		if got := len(fixture.sink.appendedChunks()); got != 1 {
			t.Fatalf("append attempts = %d; the failed append itself must happen before the save", got)
		}
		if cursor, ok, err := fixture.cursors.Load(context.Background(), fixture.executionID, fixture.actionID, fixture.workloadID()); err != nil || ok {
			t.Fatalf("cursor after a failed append = (%d, %v, %v); a save-before-append order would have advanced it", cursor, ok, err)
		}
	})

	t.Run("a_persistent_checkpoint_fault_eventually_delegates_the_stop", func(t *testing.T) {
		page := map[int64]kernelengine.LogBatch{
			0: {
				Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: 0, Sequence: 1, Data: []byte(output)}},
				NextOffset: int64(len(output)),
			},
		}
		cases := []struct {
			name    string
			arrange func(*logCollectionFixture)
		}{
			{
				name: "log source",
				arrange: func(fixture *logCollectionFixture) {
					fixture.runtime.logsErr = fault.New(fault.DependencyUnavailable, "runtime log read is unavailable")
				},
			},
			{
				name: "sink append",
				arrange: func(fixture *logCollectionFixture) {
					fixture.runtime.pages = page
					fixture.sink.appendErr = fault.New(fault.DependencyUnavailable, "retained log sink is unavailable")
				},
			},
			{
				name: "cursor load",
				arrange: func(fixture *logCollectionFixture) {
					fixture.cursors.loadErr = fault.New(fault.DependencyUnavailable, "retained log cursor is unavailable")
				},
			},
			{
				name: "execution scope",
				arrange: func(fixture *logCollectionFixture) {
					fixture.scopes.err = fault.New(fault.DependencyUnavailable, "execution scope is unavailable")
				},
			},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				fixture := newLogCollectionFixture(t, nil)
				testCase.arrange(fixture)

				// The first fault is retryable and withholds the destructive
				// stop, preserving the output that might still be retained.
				if _, err := fixture.collector.Stop(context.Background(), fixture.stopRequest()); err == nil {
					t.Fatal("first Stop succeeded although the checkpoint fault persisted")
				}
				if got := fixture.runtime.stopCountValue(); got != 0 {
					t.Fatalf("inner Stop calls after the first fault = %d; want the destructive effect withheld", got)
				}
				if fixture.cursors.loadErr == nil {
					if cursor, ok, err := fixture.cursors.Load(context.Background(), fixture.executionID, fixture.actionID, fixture.workloadID()); err != nil || ok && cursor == logCollectionComplete {
						t.Fatalf("cursor after the first fault = (%d, %v, %v); want a resumable state", cursor, ok, err)
					}
				}

				// A persistent fault must not strand the workload: the second
				// call spends the withholding bound and delegates the stop.
				evidence, err := fixture.collector.Stop(context.Background(), fixture.stopRequest())
				if err != nil {
					t.Fatalf("second Stop did not delegate the destructive stop: %v", err)
				}
				if !evidence.IsConfirmed() {
					t.Fatalf("stop evidence = %+v; want confirmed", evidence)
				}
				if got := fixture.runtime.stopCountValue(); got != 1 {
					t.Fatalf("inner Stop calls after the bound = %d; want exactly 1", got)
				}

				// Further calls keep delegating while the fault persists.
				if _, err := fixture.collector.Stop(context.Background(), fixture.stopRequest()); err != nil {
					t.Fatalf("third Stop did not delegate: %v", err)
				}
				if got := fixture.runtime.stopCountValue(); got != 2 {
					t.Fatalf("inner Stop calls after the persistent fault = %d; want 2", got)
				}
			})
		}
	})

	t.Run("a_successful_checkpoint_resets_the_withholding_bound", func(t *testing.T) {
		fixture := newLogCollectionFixture(t, map[int64]kernelengine.LogBatch{
			0: {
				Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: 0, Sequence: 1, Data: []byte(output)}},
				NextOffset: int64(len(output)),
			},
			int64(len(output)): {NextOffset: int64(len(output))},
		})
		fixture.runtime.logsErr = fault.New(fault.DependencyUnavailable, "transient log failure")
		if _, err := fixture.collector.Stop(context.Background(), fixture.stopRequest()); err == nil {
			t.Fatal("first Stop succeeded despite the injected log failure")
		}

		// The fault clears: the checkpoint succeeds and resets the count.
		fixture.runtime.logsErr = nil
		if _, err := fixture.collector.Stop(context.Background(), fixture.stopRequest()); err != nil {
			t.Fatalf("Stop after the fault cleared: %v", err)
		}
		if got := fixture.runtime.stopCountValue(); got != 1 {
			t.Fatalf("inner Stop calls = %d; want 1", got)
		}

		// A new fault gets its own bounded retry instead of delegating at once.
		fixture.runtime.logsErr = fault.New(fault.DependencyUnavailable, "transient log failure")
		if _, err := fixture.collector.Stop(context.Background(), fixture.stopRequest()); err == nil {
			t.Fatal("Stop did not withhold after a fresh checkpoint fault")
		}
		if got := fixture.runtime.stopCountValue(); got != 1 {
			t.Fatalf("inner Stop calls after the fresh fault = %d; want the stop withheld", got)
		}
	})

	t.Run("a_completed_collection_skips_the_stop_snapshot", func(t *testing.T) {
		fixture := newLogCollectionFixture(t, map[int64]kernelengine.LogBatch{
			0: {
				Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: 0, Sequence: 1, Data: []byte(output)}},
				NextOffset: int64(len(output)),
			},
			int64(len(output)): {NextOffset: int64(len(output))},
		})
		if _, err := fixture.collector.Remove(context.Background(), fixture.removeRequest()); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		reads := len(fixture.runtime.logCallOffsets())
		fixture.runtime.logsErr = fault.New(fault.DependencyUnavailable, "log source is already gone")

		if _, err := fixture.collector.Stop(context.Background(), fixture.stopRequest()); err != nil {
			t.Fatalf("Stop after a completed collection: %v", err)
		}
		if got := len(fixture.runtime.logCallOffsets()); got != reads {
			t.Fatalf("stop snapshot reads = %d; want no read after collection completed (%d)", got, reads)
		}
		if fixture.runtime.stopCountValue() != 1 {
			t.Fatalf("inner Stop calls = %d; want the stop to proceed", fixture.runtime.stopCountValue())
		}
	})
}

const (
	kubeStopNamespace = "tenant-acme"
	kubeStopTenant    = "tenant-acme"
)

// destructiveKubeCluster is the smallest Cluster that serves the adapter's
// Stop, Logs and Remove seams. The Job is produced for whatever name the
// adapter asks for, so the test never needs the adapter's private naming.
type destructiveKubeCluster struct {
	mu sync.Mutex

	identity    kernelengine.EffectIdentity
	workspaceID id.WorkspaceID
	job         runtimekube.Job
	pods        []runtimekube.Pod
	logs        []byte
	secretData  map[string][]byte
	onSuspend   func()

	suspendCalls         int
	removePods           bool
	keepPodsAfterSuspend bool
}

func newDestructiveKubeCluster(identity kernelengine.EffectIdentity, workspaceID id.WorkspaceID, logs string) *destructiveKubeCluster {
	return &destructiveKubeCluster{
		identity:    identity,
		workspaceID: workspaceID,
		pods: []runtimekube.Pod{{
			Name:      "workload-pod-1",
			Namespace: kubeStopNamespace,
			Labels:    map[string]string{runtimekube.LabelWorkload: string(identity.WorkloadID)},
			Phase:     "Running",
			Containers: []runtimekube.ContainerStatus{
				{Name: "workload"},
			},
		}},
		logs: []byte(logs),
	}
}

func (cluster *destructiveKubeCluster) GetNamespace(context.Context, string) (runtimekube.Namespace, error) {
	return runtimekube.Namespace{
		Name:   kubeStopNamespace,
		Labels: map[string]string{runtimekube.LabelTenant: kubeStopTenant},
	}, nil
}

func (cluster *destructiveKubeCluster) CreateJob(_ context.Context, job runtimekube.Job) (runtimekube.Job, error) {
	return job, nil
}

func (cluster *destructiveKubeCluster) GetJob(_ context.Context, namespace, name string) (runtimekube.Job, error) {
	cluster.mu.Lock()
	defer cluster.mu.Unlock()
	if namespace != kubeStopNamespace {
		return runtimekube.Job{}, runtimekube.ErrNotFound
	}
	cluster.job = cluster.jobForLocked(name)
	return cluster.job, nil
}

// jobForLocked returns the stable Job the adapter expects for this identity.
// The requested name is the adapter's private stable name, so copying it keeps
// the identity checks intact without exporting the naming helper.
func (cluster *destructiveKubeCluster) jobForLocked(name string) runtimekube.Job {
	job := cluster.job
	job.Name = name
	job.Namespace = kubeStopNamespace
	if job.UID == "" {
		job.UID = "uid-workload-1"
	}
	job.Labels = map[string]string{
		runtimekube.LabelTenant:    kubeStopTenant,
		runtimekube.LabelExecution: string(cluster.identity.ExecutionID),
		runtimekube.LabelAction:    string(cluster.identity.ActionID),
		runtimekube.LabelWorkload:  string(cluster.identity.WorkloadID),
		runtimekube.LabelWorkspace: string(cluster.workspaceID),
	}
	job.Annotations = map[string]string{
		runtimekube.AnnotationIdempotencyKey: cluster.identity.IdempotencyKey,
	}
	return job
}

func (cluster *destructiveKubeCluster) DeleteJob(context.Context, string, string) error {
	return nil
}

// SuspendJob models the destructive controller reaction: the PATCH is
// acknowledged and the active Pod (with its only container log) is deleted.
func (cluster *destructiveKubeCluster) SuspendJob(_ context.Context, namespace, name, expectedUID string) (runtimekube.Job, error) {
	cluster.mu.Lock()
	cluster.suspendCalls++
	if cluster.job.UID == "" || (expectedUID != "" && expectedUID != cluster.job.UID) {
		cluster.mu.Unlock()
		return runtimekube.Job{}, runtimekube.ErrConflict
	}
	suspend := true
	job := cluster.jobForLocked(name)
	job.Spec.Suspend = &suspend
	job.Spec.TTLSecondsAfterFinished = nil
	job.Status.Active = 0
	terminating := 0
	job.Status.Terminating = &terminating
	job.Status.Conditions = []runtimekube.JobCondition{{Type: "Suspended", Status: "True"}}
	cluster.job = job
	hook := cluster.onSuspend
	cluster.mu.Unlock()

	if hook != nil {
		hook()
	}

	cluster.mu.Lock()
	if !cluster.keepPodsAfterSuspend {
		// Suspension terminates and deletes the active Pod. Absence of a pod
		// then confirms the stop barrier, but the container log is gone with it.
		cluster.pods = nil
		cluster.logs = nil
		cluster.removePods = true
	}
	cluster.mu.Unlock()
	return job, nil
}

func (cluster *destructiveKubeCluster) ListPods(context.Context, string, map[string]string) ([]runtimekube.Pod, error) {
	cluster.mu.Lock()
	defer cluster.mu.Unlock()
	return append([]runtimekube.Pod(nil), cluster.pods...), nil
}

func (cluster *destructiveKubeCluster) ReadPodLogs(_ context.Context, _ string, pod string, request runtimekube.PodLogRequest) (runtimekube.PodLogPage, error) {
	cluster.mu.Lock()
	defer cluster.mu.Unlock()
	if len(cluster.pods) == 0 || cluster.pods[0].Name != pod {
		return runtimekube.PodLogPage{}, runtimekube.ErrNotFound
	}
	start := min(max(request.Offset, 0), int64(len(cluster.logs)))
	end := min(start+int64(request.Limit), int64(len(cluster.logs)))
	return runtimekube.PodLogPage{
		Data:       append([]byte(nil), cluster.logs[start:end]...),
		NextOffset: end,
		Truncated:  end < int64(len(cluster.logs)),
	}, nil
}

func (cluster *destructiveKubeCluster) PutConfigMap(_ context.Context, configMap runtimekube.ConfigMap) (runtimekube.ConfigMap, error) {
	return configMap, nil
}

func (cluster *destructiveKubeCluster) GetConfigMap(context.Context, string, string) (runtimekube.ConfigMap, error) {
	return runtimekube.ConfigMap{}, runtimekube.ErrNotFound
}

func (cluster *destructiveKubeCluster) DeleteConfigMap(context.Context, string, string) error {
	return nil
}

func (cluster *destructiveKubeCluster) PutSecret(_ context.Context, secret runtimekube.Secret) (runtimekube.Secret, error) {
	return secret, nil
}

func (cluster *destructiveKubeCluster) GetSecret(context.Context, string, string) (runtimekube.Secret, error) {
	cluster.mu.Lock()
	defer cluster.mu.Unlock()
	data := make(map[string][]byte, len(cluster.secretData))
	for name, value := range cluster.secretData {
		data[name] = append([]byte(nil), value...)
	}
	return runtimekube.Secret{Name: "credential", Namespace: kubeStopNamespace, Data: data}, nil
}

// setSecretValues replaces the credential Secret data the adapter reads for
// redaction. The values are used only to redact emitted output.
func (cluster *destructiveKubeCluster) setSecretValues(values map[string]string) {
	cluster.mu.Lock()
	defer cluster.mu.Unlock()
	cluster.secretData = make(map[string][]byte, len(values))
	for name, value := range values {
		cluster.secretData[name] = []byte(value)
	}
}

// setLog replaces the current combined workload log and optionally marks the Pod
// as terminated, modelling a workload that keeps writing while it handles
// termination and then exits.
func (cluster *destructiveKubeCluster) setLog(log string, terminated bool) {
	cluster.mu.Lock()
	defer cluster.mu.Unlock()
	cluster.logs = []byte(log)
	if terminated && len(cluster.pods) == 1 {
		cluster.pods[0].Phase = "Succeeded"
		cluster.pods[0].Containers = []runtimekube.ContainerStatus{{Name: "workload", Terminated: true}}
	}
}

func (cluster *destructiveKubeCluster) DeleteSecret(context.Context, string, string) error {
	return nil
}

func (cluster *destructiveKubeCluster) GetClaim(context.Context, string, string) (runtimekube.Claim, error) {
	return runtimekube.Claim{}, runtimekube.ErrNotFound
}

func (cluster *destructiveKubeCluster) UpdateClaim(context.Context, runtimekube.Claim) (runtimekube.Claim, error) {
	return runtimekube.Claim{}, runtimekube.ErrNotFound
}

func (cluster *destructiveKubeCluster) DeleteClaim(context.Context, string, string) error {
	return runtimekube.ErrNotFound
}

func (cluster *destructiveKubeCluster) DeleteClaimVersioned(context.Context, string, string, string) error {
	return runtimekube.ErrNotFound
}

func (cluster *destructiveKubeCluster) ListJobs(context.Context, string, map[string]string) ([]runtimekube.Job, error) {
	return nil, nil
}

// TestLogCollectionCheckpointsBeforeKubernetesSuspension wires the decorator
// around the real Kubernetes adapter and proves the checkpoint is durable before
// SuspendJob deletes the Pod.
func TestLogCollectionCheckpointsBeforeKubernetesSuspension(t *testing.T) {
	const output = "ready-marker\n"
	identity := kernelengine.EffectIdentity{
		ExecutionID:    id.ExecutionID("execution-wiring-1"),
		ActionID:       id.ActionID("action-wiring-1"),
		WorkloadID:     id.WorkloadID("workload-wiring-1"),
		IdempotencyKey: "idempotency-wiring-1",
	}
	workspaceID := id.WorkspaceID("workspace-wiring-1")
	scope := tenancy.Scope{TenantID: "tenant-1", ProjectID: "project-1"}

	cluster := newDestructiveKubeCluster(identity, workspaceID, output)
	adapter, err := runtimekube.New(runtimekube.Config{
		Cluster:   cluster,
		Namespace: kubeStopNamespace,
		TenantID:  id.TenantID(kubeStopTenant),
		KodamaURL: "https://kodama.example.com",
		Policy: runtimekube.Policy{
			CPURequest:    "100m",
			CPULimit:      "1",
			MemoryRequest: "128Mi",
			MemoryLimit:   "256Mi",
			JobTTLSeconds: 600,
		},
		Cleanup: runtimekube.CleanupPolicy{Attempts: 1, Backoff: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("kubernetes New: %v", err)
	}

	order := &collectionOrder{}
	fileSink, err := shelllog.NewFileSink(shelllog.SinkConfig{Root: t.TempDir(), Clock: newFakeClock()})
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	sink := &recordingSink{inner: fileSink, order: order}
	scopes := &fakeExecutionScopes{scopes: map[id.ExecutionID]tenancy.Scope{identity.ExecutionID: scope}}
	cursors := &memoryLogCursors{cursors: map[string]int64{}}
	collector, err := newLogCollectingRuntime(logCollectionConfig{
		Inner:   adapter,
		Sink:    sink,
		Scopes:  scopes,
		Cursors: cursors,
	})
	if err != nil {
		t.Fatalf("newLogCollectingRuntime: %v", err)
	}

	// At the instant the controller reaction deletes the Pod, the real retained
	// sink must already serve the bytes the Pod produced.
	durableAtSuspension := ""
	cluster.onSuspend = func() {
		result, err := fileSink.Read(context.Background(), shelllog.ReadRequest{
			Scope:       scope,
			ExecutionID: identity.ExecutionID,
			ActionID:    identity.ActionID,
			Stream:      shelllog.StreamStdout,
			Limit:       shelllog.DefaultLimits().MaxReadChunks,
		})
		if err != nil {
			t.Fatalf("read retained sink at suspension: %v", err)
		}
		durableAtSuspension = joinLogData(result.Chunks)
	}

	evidence, err := collector.Stop(context.Background(), kernelengine.StopRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatalf("Stop through the real adapter: %v", err)
	}
	if !evidence.IsConfirmed() {
		t.Fatalf("stop evidence = %+v; want confirmed suspension", evidence)
	}
	if cluster.suspendCalls != 1 {
		t.Fatalf("SuspendJob calls = %d; want exactly 1", cluster.suspendCalls)
	}
	if !cluster.removePods {
		t.Fatal("the suspension reaction did not remove the workload Pod")
	}
	if durableAtSuspension != output {
		t.Fatalf("retained stdout at suspension = %q; want the produced bytes %q", durableAtSuspension, output)
	}

	// The Pod and its log are gone. A restarted decorator still retains the
	// prefix and Remove completes cleanup without inventing a placeholder.
	restarted, err := newLogCollectingRuntime(logCollectionConfig{
		Inner:   adapter,
		Sink:    sink,
		Scopes:  scopes,
		Cursors: cursors,
	})
	if err != nil {
		t.Fatalf("newLogCollectingRuntime after restart: %v", err)
	}
	removal, err := restarted.Remove(context.Background(), kernelengine.RemoveRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatalf("Remove after suspension: %v", err)
	}
	if !removal.Removed {
		t.Fatalf("Remove after suspension = %+v; want confirmed removal", removal)
	}
	stdout := joinLogData(readAllLogs(t, sink, scope, identity.ExecutionID, identity.ActionID, shelllog.StreamStdout))
	if stdout != output {
		t.Fatalf("retained stdout = %q; want exactly the produced bytes %q", stdout, output)
	}
	if strings.Count(stdout, output) != 1 {
		t.Fatalf("retained stdout contains the produced bytes %d times; want exactly 1", strings.Count(stdout, output))
	}
	if strings.Contains(stdout, logCollectionUnavailableMarker) {
		t.Fatalf("retained stdout = %q; the suspension destroyed source produced an unavailable placeholder", stdout)
	}
	if cursor, ok, err := cursors.Load(context.Background(), identity.ExecutionID, identity.ActionID, identity.WorkloadID); err != nil || !ok || cursor != logCollectionComplete {
		t.Fatalf("cursor after Remove = (%d, %v, %v); want the completion token", cursor, ok, err)
	}
}

// TestLogCollectionKubernetesStopDoesNotLeakSplitCredential pins the checkpoint
// boundary through the real Kubernetes adapter with a non-empty credential
// Secret and the production sink configuration (no sink-level Redact list). A
// live temporary EOF must not advance past a trailing partial credential: only
// complete lines are checkpointed, the trailing partial is left for the final
// drain, and a complete readiness marker line is not lost.
func TestLogCollectionKubernetesStopDoesNotLeakSplitCredential(t *testing.T) {
	const secret = "supersecretvalue"
	const output = "ready-marker\n"
	const partialLine = "head superse"
	const completedLine = "head supersecretvalue tail\n"

	identity := kernelengine.EffectIdentity{
		ExecutionID:    id.ExecutionID("execution-leak-1"),
		ActionID:       id.ActionID("action-leak-1"),
		WorkloadID:     id.WorkloadID("workload-leak-1"),
		IdempotencyKey: "idempotency-leak-1",
	}
	workspaceID := id.WorkspaceID("workspace-leak-1")
	scope := tenancy.Scope{TenantID: "tenant-1", ProjectID: "project-1"}

	cluster := newDestructiveKubeCluster(identity, workspaceID, output+partialLine)
	cluster.setSecretValues(map[string]string{"API_KEY": secret})
	// Keep the terminated Pod and its readable log after suspension so the
	// final Remove drain can complete the split credential line.
	cluster.keepPodsAfterSuspend = true
	adapter, err := runtimekube.New(runtimekube.Config{
		Cluster:   cluster,
		Namespace: kubeStopNamespace,
		TenantID:  id.TenantID(kubeStopTenant),
		KodamaURL: "https://kodama.example.com",
		Policy: runtimekube.Policy{
			CPURequest:    "100m",
			CPULimit:      "1",
			MemoryRequest: "128Mi",
			MemoryLimit:   "256Mi",
			JobTTLSeconds: 600,
		},
		Cleanup: runtimekube.CleanupPolicy{Attempts: 1, Backoff: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("kubernetes New: %v", err)
	}

	// No sink-level Redact list: production has none (compose.go, persistent.go).
	fileSink, err := shelllog.NewFileSink(shelllog.SinkConfig{Root: t.TempDir(), Clock: newFakeClock()})
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	order := &collectionOrder{}
	sink := &recordingSink{inner: fileSink, order: order}
	scopes := &fakeExecutionScopes{scopes: map[id.ExecutionID]tenancy.Scope{identity.ExecutionID: scope}}
	cursors := &memoryLogCursors{cursors: map[string]int64{}}
	newCollector := func() *logCollectingRuntime {
		collector, err := newLogCollectingRuntime(logCollectionConfig{
			Inner:   adapter,
			Sink:    sink,
			Scopes:  scopes,
			Cursors: cursors,
		})
		if err != nil {
			t.Fatalf("newLogCollectingRuntime: %v", err)
		}
		return collector
	}
	collector := newCollector()

	durableAtStop := ""
	cluster.onSuspend = func() {
		result, err := fileSink.Read(context.Background(), shelllog.ReadRequest{
			Scope:       scope,
			ExecutionID: identity.ExecutionID,
			ActionID:    identity.ActionID,
			Stream:      shelllog.StreamStdout,
			Limit:       shelllog.DefaultLimits().MaxReadChunks,
		})
		if err != nil {
			t.Fatalf("read retained sink at suspension: %v", err)
		}
		durableAtStop = joinLogData(result.Chunks)
		// The workload keeps writing after the checkpoint and then exits.
		cluster.setLog(output+completedLine, true)
	}

	evidence, err := collector.Stop(context.Background(), kernelengine.StopRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatalf("Stop through the real adapter: %v", err)
	}
	if !evidence.IsConfirmed() {
		t.Fatalf("stop evidence = %+v; want confirmed suspension", evidence)
	}
	if durableAtStop != output {
		t.Fatalf("retained stdout at the stop snapshot has %d bytes; want only the %d-byte complete marker line", len(durableAtStop), len(output))
	}
	if strings.Contains(durableAtStop, "superse") {
		t.Fatalf("retained stdout at the stop snapshot contains a partial credential prefix (%d bytes)", len(durableAtStop))
	}

	// Restart over the same durable sink and cursor: the final drain completes
	// the line, the adapter redacts the whole credential, and nothing in plain
	// text reaches the sink.
	restarted := newCollector()
	removal, err := restarted.Remove(context.Background(), kernelengine.RemoveRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatalf("Remove after suspension: %v", err)
	}
	if !removal.Removed {
		t.Fatalf("Remove after suspension = %+v; want confirmed removal", removal)
	}
	stdout := joinLogData(readAllLogs(t, sink, scope, identity.ExecutionID, identity.ActionID, shelllog.StreamStdout))
	if want := output + "head [REDACTED] tail\n"; stdout != want {
		t.Fatalf("retained stdout has %d bytes; want the %d-byte marker line with the credential redacted", len(stdout), len(want))
	}
	if strings.Count(stdout, output) != 1 {
		t.Fatalf("retained stdout contains the readiness marker %d times; want exactly 1", strings.Count(stdout, output))
	}
	if strings.Contains(stdout, secret) {
		t.Fatalf("retained stdout contains the configured credential in plaintext after Stop and Remove (%d bytes)", len(stdout))
	}
	if strings.Contains(stdout, "superse") {
		t.Fatalf("retained stdout contains a partial credential prefix after Stop and Remove (%d bytes)", len(stdout))
	}
	if strings.Contains(stdout, logCollectionUnavailableMarker) {
		t.Fatalf("retained stdout contains an unavailability placeholder after a readable final drain")
	}
	if cursor, ok, err := cursors.Load(context.Background(), identity.ExecutionID, identity.ActionID, identity.WorkloadID); err != nil || !ok || cursor != logCollectionComplete {
		t.Fatalf("cursor after Remove = (%d, %v, %v); want the completion token", cursor, ok, err)
	}
}
