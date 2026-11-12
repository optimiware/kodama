package readers_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	executionstore "github.com/optimiware/kodama/internal/shell/store/units/execution"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
	"github.com/optimiware/kodama/internal/shell/store/units/readers"
	"github.com/optimiware/kodama/internal/shell/store/units/work"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// executionFixture is one store with the real work and execution adapters.
type executionFixture struct {
	store    *memory.Store
	tasks    *work.Task
	pipes    *work.Pipelines
	exec     *executionstore.Store
	reader   *readers.ExecutionReader
	receipts *receiptCounter
	scope    tenancy.Scope
	taskID   id.TaskID
	execID   id.ExecutionID
}

func newExecutionFixture(t *testing.T, scope tenancy.Scope) *executionFixture {
	t.Helper()
	backend := newStore()
	tasks, err := work.NewTask(backend, backend)
	if err != nil {
		t.Fatalf("NewTask: %v", err)
	}
	pipes, err := work.NewPipelines(backend, backend, nil)
	if err != nil {
		t.Fatalf("NewPipelines: %v", err)
	}
	reader, err := readers.NewExecutionReader(backend, backend)
	if err != nil {
		t.Fatalf("NewExecutionReader: %v", err)
	}
	seedPipeline(t, backend, scope, testPipeline, "build", []byte("actions:\n  - name: run\n"), 1)
	receipts := &receiptCounter{}
	taskID, execID := startedTask(t, tasks, pipes, receipts, scope)
	return &executionFixture{
		store:    backend,
		tasks:    tasks,
		pipes:    pipes,
		exec:     executionstore.New(backend, backend),
		reader:   reader,
		receipts: receipts,
		scope:    scope,
		taskID:   taskID,
		execID:   execID,
	}
}

// writeCleanup commits one cleanup progress row through the real execution
// adapter and returns the next execution version. next is the execution state
// the same commit persists, so a fixture can represent cleanup progress on a
// terminal execution without inventing a state the Kernel never writes.
func (fixture *executionFixture) writeCleanup(t *testing.T, version uint64, state kernel.CleanupState, next kernel.ExecutionState, workload id.WorkloadID) uint64 {
	t.Helper()
	result, err := fixture.exec.Transition(context.Background(), execution.TransitionUnit{
		Owner:                    fixture.scope,
		ExecutionID:              fixture.execID,
		ExpectedExecutionVersion: version,
		Commit: flow.Commit{
			ExecutionID:              fixture.execID,
			ExpectedExecutionVersion: version,
			NextExecutionState:       next,
			Output:                   flow.OutputChange{Action: flow.OutputNone},
			CleanupUpdate: &flow.CleanupUpdate{
				State:       state,
				WorkloadID:  workload,
				WorkspaceID: snapshotdoc.DeriveWorkspaceID(fixture.execID),
				Attempts:    1,
			},
			At: testNow,
		},
	})
	if err != nil {
		t.Fatalf("commit cleanup %q: %v", state, err)
	}
	return result.ExecutionVersion
}

// writeStop commits one persisted stop barrier on the execution's first action.
// next is the execution state the same commit persists, so the fixture can
// represent a barrier recorded while the execution is still running or after it
// reached a terminal state.
func (fixture *executionFixture) writeStop(t *testing.T, version, actionVersion uint64, evidence kernel.StopEvidence, next kernel.ExecutionState) uint64 {
	t.Helper()
	return writeStopEvidence(t, fixture.exec, fixture.scope, fixture.execID, version, actionVersion, evidence, next)
}

// writeStopEvidence commits one persisted stop barrier on the execution's first
// action together with the execution state the same Kernel commit persists, and
// returns the new execution version.
func writeStopEvidence(t *testing.T, exec *executionstore.Store, scope tenancy.Scope, execID id.ExecutionID, version, actionVersion uint64, evidence kernel.StopEvidence, next kernel.ExecutionState) uint64 {
	t.Helper()
	actionID := snapshotdoc.DeriveActionID(execID, 0)
	result, err := exec.Transition(context.Background(), execution.TransitionUnit{
		Owner:                    scope,
		ExecutionID:              execID,
		ExpectedExecutionVersion: version,
		ExpectedActionVersion:    actionVersion,
		Commit: flow.Commit{
			ExecutionID:              execID,
			ExpectedExecutionVersion: version,
			NextExecutionState:       next,
			ActionID:                 actionID,
			ExpectedActionVersion:    actionVersion,
			NextActionState:          kernel.ActionPending,
			Output:                   flow.OutputChange{Action: flow.OutputNone},
			StopEvidence:             &evidence,
			At:                       testNow,
		},
	})
	if err != nil {
		t.Fatalf("commit stop evidence: %v", err)
	}
	return result.ExecutionVersion
}

// TestExecutionReaderReadsTheQueuedExecutionView proves the reader returns the
// committed identity, version and state and never invents stop evidence for an
// execution that has no cleanup or stop row.
func TestExecutionReaderReadsTheQueuedExecutionView(t *testing.T) {
	fixture := newExecutionFixture(t, testScope())
	view, err := fixture.reader.Get(context.Background(), fixture.scope, fixture.execID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if view.ID != fixture.execID || view.Version != 1 || view.State != kernel.ExecutionQueued {
		t.Fatalf("view = %+v; want the queued execution at version 1", view)
	}
	if view.StopEvidence.IsConfirmed() {
		t.Fatalf("stop evidence = %+v; an absent cleanup/stop row must not be confirmed", view.StopEvidence)
	}
}

// TestExecutionReaderConfirmsStopFromCompletedCleanup proves cleanup complete is
// affirmative persisted evidence once the execution is terminal. The cleanup row
// is written by a commit that also persists the terminal state, matching what
// the Kernel does.
func TestExecutionReaderConfirmsStopFromCompletedCleanup(t *testing.T) {
	fixture := newExecutionFixture(t, testScope())
	version := fixture.writeCleanup(t, 1, kernel.CleanupComplete, kernel.ExecutionSucceeded, "workload-1")
	view, err := fixture.reader.Get(context.Background(), fixture.scope, fixture.execID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if view.Version != version {
		t.Fatalf("version = %d; want %d", view.Version, version)
	}
	if !view.StopEvidence.IsConfirmed() {
		t.Fatalf("stop evidence = %+v; completed cleanup must confirm stop", view.StopEvidence)
	}
}

// TestExecutionReaderConfirmsStopFromPersistedActionBarrier proves a confirmed
// stop barrier on the execution's possibly started action is used while cleanup
// is still owed.
func TestExecutionReaderConfirmsStopFromPersistedActionBarrier(t *testing.T) {
	fixture := newExecutionFixture(t, testScope())
	version := fixture.writeCleanup(t, 1, kernel.CleanupPending, kernel.ExecutionFailed, "workload-1")
	version = fixture.writeStop(t, version, 1, kernel.ConfirmedStop("runtime confirmed termination"), kernel.ExecutionFailed)
	view, err := fixture.reader.Get(context.Background(), fixture.scope, fixture.execID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if view.Version != version {
		t.Fatalf("version = %d; want %d", view.Version, version)
	}
	if !view.StopEvidence.IsConfirmed() {
		t.Fatalf("stop evidence = %+v; a confirmed persisted barrier must confirm stop", view.StopEvidence)
	}
}

// TestExecutionReaderNeverConfirmsAPendingOrAbsentBarrier proves an owed cleanup
// with a pending stop barrier stays unconfirmed, and that a cleanup row alone
// without complete state is not confirmation.
func TestExecutionReaderNeverConfirmsAPendingOrAbsentBarrier(t *testing.T) {
	cases := map[string]struct {
		pending bool
	}{
		"cleanup pending with no barrier":     {pending: false},
		"cleanup pending with a pending stop": {pending: true},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newExecutionFixture(t, testScope())
			version := fixture.writeCleanup(t, 1, kernel.CleanupPending, kernel.ExecutionCancelled, "workload-1")
			if testCase.pending {
				fixture.writeStop(t, version, 1, kernel.PendingStop("stop response lost"), kernel.ExecutionCancelled)
			}
			view, err := fixture.reader.Get(context.Background(), fixture.scope, fixture.execID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if view.StopEvidence.IsConfirmed() {
				t.Fatalf("stop evidence = %+v; pending/absent evidence must stay unconfirmed", view.StopEvidence)
			}
		})
	}
}

// TestExecutionReaderNeverConfirmsANonTerminalExecution proves the reader is
// the guard against dispatching a replacement over a live predecessor: a queued
// or running execution can still start another action, so even a confirmed
// per-action barrier or completed cleanup is not confirmation until the
// execution itself reaches a terminal state.
func TestExecutionReaderNeverConfirmsANonTerminalExecution(t *testing.T) {
	for _, state := range []kernel.ExecutionState{kernel.ExecutionQueued, kernel.ExecutionRunning} {
		t.Run(string(state)+"/confirmed action barrier", func(t *testing.T) {
			fixture := newExecutionFixture(t, testScope())
			version := fixture.writeCleanup(t, 1, kernel.CleanupPending, state, "workload-1")
			fixture.writeStop(t, version, 1, kernel.ConfirmedStop("runtime confirmed termination"), state)
			view, err := fixture.reader.Get(context.Background(), fixture.scope, fixture.execID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if view.State != state {
				t.Fatalf("state = %q; want %q", view.State, state)
			}
			if view.StopEvidence.IsConfirmed() {
				t.Fatalf("stop evidence = %+v; a %s execution may still start another action", view.StopEvidence, state)
			}
		})
		t.Run(string(state)+"/cleanup complete", func(t *testing.T) {
			fixture := newExecutionFixture(t, testScope())
			fixture.writeCleanup(t, 1, kernel.CleanupComplete, state, "workload-1")
			view, err := fixture.reader.Get(context.Background(), fixture.scope, fixture.execID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if view.State != state {
				t.Fatalf("state = %q; want %q", view.State, state)
			}
			if view.StopEvidence.IsConfirmed() {
				t.Fatalf("stop evidence = %+v; a %s execution may still start another action", view.StopEvidence, state)
			}
		})
	}
}

// TestExecutionReaderScopeIsolationAndAbsence proves a foreign scope and an
// unknown execution are not_found.
func TestExecutionReaderScopeIsolationAndAbsence(t *testing.T) {
	fixture := newExecutionFixture(t, testScope())
	if _, err := fixture.reader.Get(context.Background(), otherScope(), fixture.execID); !isClass(err, fault.NotFound) {
		t.Fatalf("cross-scope read error = %v; want not_found", err)
	}
	if _, err := fixture.reader.Get(context.Background(), fixture.scope, "execution-missing"); !isClass(err, fault.NotFound) {
		t.Fatalf("absent read error = %v; want not_found", err)
	}
}
