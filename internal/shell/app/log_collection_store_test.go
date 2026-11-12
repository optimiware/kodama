// Focused tests for the retained-log collection collaborators this composition
// adds: the durable file cursor store and the "scope failure is never an absent
// removal" boundary. The behavioral collection rules live in
// log_collection_test.go and the composed path in log_collection_wiring_test.go.
package app

import (
	"context"
	"encoding/hex"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
	shelllog "github.com/optimiware/kodama/internal/shell/log"
)

func isLogCollectionClass(err error, want fault.Class) bool {
	class, ok := fault.ClassOf(err)
	return ok && class == want
}

func TestFileLogCursorStoreDurableAndScoped(t *testing.T) {
	if _, err := newFileLogCursorStore("  "); err == nil {
		t.Fatal("newFileLogCursorStore accepted an empty data directory")
	}
	dataDir := t.TempDir()
	store, err := newFileLogCursorStore(dataDir)
	if err != nil {
		t.Fatalf("newFileLogCursorStore: %v", err)
	}
	executionID := id.ExecutionID("execution-1")
	actionID := id.ActionID("action-1")
	otherAction := id.ActionID("action-2")
	workloadID := id.WorkloadID("workload-1")
	otherWorkload := id.WorkloadID("workload-2")

	if cursor, ok, err := store.Load(context.Background(), executionID, actionID, workloadID); err != nil || ok || cursor != 0 {
		t.Fatalf("Load before Save = (%d, %v, %v); want an unsaved cursor", cursor, ok, err)
	}
	if err := store.Save(context.Background(), executionID, actionID, workloadID, 42); err != nil {
		t.Fatalf("Save: %v", err)
	}
	path, err := store.cursorPath(executionID, actionID, workloadID)
	if err != nil {
		t.Fatalf("cursorPath: %v", err)
	}
	wantRoot := filepath.Join(dataDir, "logs", "cursors") + string(os.PathSeparator)
	if !strings.HasPrefix(path, wantRoot) {
		t.Fatalf("cursor path %q is outside %q", path, wantRoot)
	}

	// A fresh store over the same data directory models a composition restart.
	restarted, err := newFileLogCursorStore(dataDir)
	if err != nil {
		t.Fatalf("newFileLogCursorStore after restart: %v", err)
	}
	if cursor, ok, err := restarted.Load(context.Background(), executionID, actionID, workloadID); err != nil || !ok || cursor != 42 {
		t.Fatalf("Load after restart = (%d, %v, %v); want (42, true, nil)", cursor, ok, err)
	}
	if cursor, ok, err := restarted.Load(context.Background(), executionID, otherAction, workloadID); err != nil || ok || cursor != 0 {
		t.Fatalf("Load for another action = (%d, %v, %v); want an unsaved cursor", cursor, ok, err)
	}
	if cursor, ok, err := restarted.Load(context.Background(), executionID, actionID, otherWorkload); err != nil || ok || cursor != 0 {
		t.Fatalf("Load for another workload of the same action = (%d, %v, %v); want an unsaved cursor", cursor, ok, err)
	}

	// Identities are validated before they become file names.
	if _, _, err := restarted.Load(context.Background(), id.ExecutionID("../escape"), actionID, workloadID); !isLogCollectionClass(err, fault.InvalidInput) {
		t.Fatalf("Load with a traversal execution id error = %v; want invalid_input", err)
	}
	if _, _, err := restarted.Load(context.Background(), executionID, actionID, id.WorkloadID("../escape")); !isLogCollectionClass(err, fault.InvalidInput) {
		t.Fatalf("Load with a traversal workload id error = %v; want invalid_input", err)
	}
	if err := restarted.Save(context.Background(), executionID, actionID, workloadID, -1); !isLogCollectionClass(err, fault.InvalidInput) {
		t.Fatalf("Save with a negative cursor error = %v; want invalid_input", err)
	}
	if err := os.WriteFile(path, []byte("not-a-cursor"), 0o600); err != nil {
		t.Fatalf("corrupt cursor file: %v", err)
	}
	if _, _, err := restarted.Load(context.Background(), executionID, actionID, workloadID); !isLogCollectionClass(err, fault.InternalFailure) {
		t.Fatalf("Load of a corrupt cursor error = %v; want internal_failure", err)
	}
}

// TestFileLogCursorStoreIsKeyedByWorkloadIdentity proves one action's workloads
// keep independent cursors: a position saved for one identity never applies to
// another identity of the same (execution, action).
func TestFileLogCursorStoreIsKeyedByWorkloadIdentity(t *testing.T) {
	store, err := newFileLogCursorStore(t.TempDir())
	if err != nil {
		t.Fatalf("newFileLogCursorStore: %v", err)
	}
	executionID := id.ExecutionID("execution-1")
	actionID := id.ActionID("action-1")
	retired := id.WorkloadID("workload-retired")
	replacement := id.WorkloadID("workload-replacement")

	if err := store.Save(context.Background(), executionID, actionID, retired, 20); err != nil {
		t.Fatalf("Save retired: %v", err)
	}
	if cursor, ok, err := store.Load(context.Background(), executionID, actionID, replacement); err != nil || ok || cursor != 0 {
		t.Fatalf("replacement Load = (%d, %v, %v); want an unsaved cursor, not the retired position 20", cursor, ok, err)
	}
	if err := store.Save(context.Background(), executionID, actionID, replacement, 5); err != nil {
		t.Fatalf("Save replacement: %v", err)
	}
	if cursor, ok, err := store.Load(context.Background(), executionID, actionID, retired); err != nil || !ok || cursor != 20 {
		t.Fatalf("retired Load = (%d, %v, %v); want its own position 20", cursor, ok, err)
	}
	if cursor, ok, err := store.Load(context.Background(), executionID, actionID, replacement); err != nil || !ok || cursor != 5 {
		t.Fatalf("replacement Load = (%d, %v, %v); want its own position 5", cursor, ok, err)
	}

	retiredPath, err := store.cursorPath(executionID, actionID, retired)
	if err != nil {
		t.Fatalf("cursorPath retired: %v", err)
	}
	replacementPath, err := store.cursorPath(executionID, actionID, replacement)
	if err != nil {
		t.Fatalf("cursorPath replacement: %v", err)
	}
	if retiredPath == replacementPath {
		t.Fatalf("both workload identities map to the same cursor path %q", retiredPath)
	}
}

// TestFileLogCursorStoreIgnoresLegacyActionCursor pins the documented no-migration
// rule: a cursor file keyed by (execution, action) alone is not read for the
// workload-scoped key, so such a record cannot be misapplied.
func TestFileLogCursorStoreIgnoresLegacyActionCursor(t *testing.T) {
	dataDir := t.TempDir()
	store, err := newFileLogCursorStore(dataDir)
	if err != nil {
		t.Fatalf("newFileLogCursorStore: %v", err)
	}
	executionID := id.ExecutionID("execution-1")
	actionID := id.ActionID("action-1")
	workloadID := id.WorkloadID("workload-1")

	legacyPath := filepath.Join(dataDir, "logs", "cursors",
		hex.EncodeToString([]byte(executionID))+"-"+hex.EncodeToString([]byte(actionID))+".cursor")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o700); err != nil {
		t.Fatalf("create legacy cursor directory: %v", err)
	}
	if err := os.WriteFile(legacyPath, []byte("9223372036854775807"), 0o600); err != nil {
		t.Fatalf("write legacy completion token: %v", err)
	}

	if cursor, ok, err := store.Load(context.Background(), executionID, actionID, workloadID); err != nil || ok || cursor != 0 {
		t.Fatalf("Load = (%d, %v, %v); the legacy action-keyed cursor must be ignored", cursor, ok, err)
	}

	// The ignored legacy token must not suppress collection through the
	// decorator either.
	fixture := newLogCollectionFixture(t, map[int64]kernelengine.LogBatch{
		0: {
			Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: 0, Sequence: 1, Data: []byte("collected\n")}},
			NextOffset: 10,
		},
		10: {NextOffset: 10},
	})
	collector, err := newLogCollectingRuntime(logCollectionConfig{
		Inner:   fixture.runtime,
		Sink:    fixture.sink,
		Scopes:  fixture.scopes,
		Cursors: store,
	})
	if err != nil {
		t.Fatalf("newLogCollectingRuntime: %v", err)
	}
	if _, err := collector.Remove(context.Background(), fixture.removeRequestForWorkload(executionID, actionID, workloadID)); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	calls := fixture.runtime.logCallOffsets()
	if len(calls) == 0 || calls[0] != 0 {
		t.Fatalf("log read offsets = %v; a legacy completion token must not skip collection from 0", calls)
	}
	if got := joinLogData(readAllLogs(t, fixture.sink, fixture.scope, executionID, actionID, shelllog.StreamStdout)); got != "collected\n" {
		t.Fatalf("retained stdout = %q; want the collected output", got)
	}
	if cursor, ok, err := store.Load(context.Background(), executionID, actionID, workloadID); err != nil || !ok || cursor != logCollectionComplete {
		t.Fatalf("workload cursor = (%d, %v, %v); want the new-keyed completion token", cursor, ok, err)
	}
}

// TestLogCollectionCompletionTokenIsExact pins the persisted completion value
// to math.MaxInt64 so a mutant that changes the constant to zero cannot pass: a
// saved zero must be a resumable position, never a completion marker.
func TestLogCollectionCompletionTokenIsExact(t *testing.T) {
	if logCollectionComplete != math.MaxInt64 {
		t.Fatalf("logCollectionComplete = %d; want math.MaxInt64", int64(logCollectionComplete))
	}

	t.Run("persisted_value_is_max_int64", func(t *testing.T) {
		fixture := newLogCollectionFixture(t, map[int64]kernelengine.LogBatch{
			0: {
				Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: 0, Sequence: 1, Data: []byte("done\n")}},
				NextOffset: 5,
			},
			5: {NextOffset: 5},
		})
		if _, err := fixture.collector.Remove(context.Background(), fixture.removeRequest()); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		cursor, ok, err := fixture.cursors.Load(context.Background(), fixture.executionID, fixture.actionID, fixture.workloadID())
		if err != nil || !ok || cursor != math.MaxInt64 {
			t.Fatalf("persisted completion = (%d, %v, %v); want exactly math.MaxInt64", cursor, ok, err)
		}
	})

	t.Run("saved_zero_is_not_complete", func(t *testing.T) {
		fixture := newLogCollectionFixture(t, map[int64]kernelengine.LogBatch{
			0: {
				Chunks:     []kernelengine.LogChunk{{Stream: kernelengine.LogStdout, Offset: 0, Sequence: 1, Data: []byte("resumable\n")}},
				NextOffset: 10,
			},
			10: {NextOffset: 10},
		})
		if err := fixture.cursors.Save(context.Background(), fixture.executionID, fixture.actionID, fixture.workloadID(), 0); err != nil {
			t.Fatalf("Save zero position: %v", err)
		}
		if _, err := fixture.collector.Remove(context.Background(), fixture.removeRequest()); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		calls := fixture.runtime.logCallOffsets()
		if len(calls) == 0 || calls[0] != 0 {
			t.Fatalf("log read offsets = %v; a saved cursor of 0 must resume from 0, not skip collection", calls)
		}
		if got := joinLogData(readAllLogs(t, fixture.sink, fixture.scope, fixture.executionID, fixture.actionID, shelllog.StreamStdout)); got != "resumable\n" {
			t.Fatalf("retained stdout = %q; want the collected output", got)
		}
	})
}

func TestLogCollectionScopeFailureIsNotAnAbsentRemoval(t *testing.T) {
	fixture := newLogCollectionFixture(t, nil)
	fixture.scopes.err = fault.New(fault.NotFound, "execution row is gone")

	_, err := fixture.collector.Remove(context.Background(), fixture.removeRequest())
	if err == nil {
		t.Fatal("Remove succeeded without an owning scope")
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
		t.Fatalf("scope failure class = %v; want dependency_unavailable so cleanup never treats it as absence", class)
	}
	if fixture.runtime.removeCalls != 0 {
		t.Fatalf("Remove calls = %d; cleanup must stay retained", fixture.runtime.removeCalls)
	}
	if len(fixture.runtime.logCalls) != 0 || len(fixture.sink.appendedChunks()) != 0 {
		t.Fatalf("scope failure read %v pages and appended %d chunks", fixture.runtime.logCalls, len(fixture.sink.appendedChunks()))
	}
}

func TestLogCollectingRuntimeRefusesIncompleteConfig(t *testing.T) {
	if _, err := newLogCollectingRuntime(logCollectionConfig{}); !isLogCollectionClass(err, fault.InvalidInput) {
		t.Fatalf("empty config error = %v; want invalid_input", err)
	}
	if _, err := newLogCollectingRuntime(logCollectionConfig{
		Inner:     &logPageRuntime{fakeRuntime: &fakeRuntime{}, order: &collectionOrder{}},
		Sink:      &recordingSink{order: &collectionOrder{}},
		Scopes:    &fakeExecutionScopes{},
		Cursors:   &memoryLogCursors{},
		PageBytes: -1,
	}); !isLogCollectionClass(err, fault.InvalidInput) {
		t.Fatalf("negative page size error = %v; want invalid_input", err)
	}
}
