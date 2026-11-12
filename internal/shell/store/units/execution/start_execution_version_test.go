package executionstore

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
)

// TestStartExecutionVersionPersistsAndReplays pins the committed execution
// version on the start result: a fresh execution reports version 1 even when
// the stored pipeline is version 3, and an equal replay keeps returning the
// ORIGINAL admission execution version after the execution has advanced.
func TestStartExecutionVersionPersistsAndReplays(t *testing.T) {
	ctx := context.Background()
	adapter, backend := newStore(t)
	scope := projectScope()
	seedPipeline(t, backend, scope, 3)
	unit := startUnit(scope, "exec-version", "start-version", containerAction())

	result, err := adapter.Start(ctx, unit)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var _ uint64 = result.ExecutionVersion
	if result.ExecutionVersion != 1 {
		t.Fatalf("fresh execution version = %d; want 1", result.ExecutionVersion)
	}
	if result.PipelineVersion != 3 {
		t.Fatalf("fresh pipeline version = %d; want 3", result.PipelineVersion)
	}

	// The committed receipt itself carries the admission execution version, so
	// a replay can return the original version without guessing it.
	stored, found := backend.Receipt(unit.Receipt.Key)
	if !found {
		t.Fatal("fresh Start did not commit a receipt")
	}
	if !strings.Contains(string(stored.Result), `"ExecutionVersion":1`) {
		t.Fatalf("committed receipt result = %s; want it to carry \"ExecutionVersion\":1", stored.Result)
	}

	// Advance the execution exactly like a real transition: queued -> running.
	action := snapshotdoc.DeriveActionID("exec-version", 0)
	commit := flow.Commit{
		ExecutionID:              "exec-version",
		ExpectedExecutionVersion: 1,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 action,
		ExpectedActionVersion:    1,
		NextActionState:          kernel.ActionRunning,
		Output:                   flow.OutputChange{Action: flow.OutputNone},
		Deadline:                 testNow.Add(90 * time.Second),
		At:                       testNow,
	}
	if _, err := adapter.Transition(ctx, transitionUnit(scope, "exec-version", commit, "advance-version")); err != nil {
		t.Fatalf("advance execution: %v", err)
	}
	advanced := loadExecution(t, adapter, scope, "exec-version")
	if advanced.Version != 2 {
		t.Fatalf("advanced execution version = %d; want 2", advanced.Version)
	}

	replayed, err := adapter.Start(ctx, unit)
	if err != nil {
		t.Fatalf("replayed Start: %v", err)
	}
	if !replayed.Replayed {
		t.Fatal("equal start retry did not replay")
	}
	if replayed.ExecutionID != result.ExecutionID {
		t.Fatalf("replayed execution = %q; want %q", replayed.ExecutionID, result.ExecutionID)
	}
	if replayed.ExecutionVersion != 1 {
		t.Fatalf("replayed execution version = %d; want the original admission version 1", replayed.ExecutionVersion)
	}
	if replayed.PipelineVersion != 3 {
		t.Fatalf("replayed pipeline version = %d; want 3", replayed.PipelineVersion)
	}
}

// TestStartLegacyReceiptReturnsAdmissionVersion proves a receipt written before
// StartResult carried ExecutionVersion stays readable: the missing member falls
// back to the insertion invariant, execution version 1.
func TestStartLegacyReceiptReturnsAdmissionVersion(t *testing.T) {
	ctx := context.Background()
	adapter, backend := newStore(t)
	scope := projectScope()
	seedPipeline(t, backend, scope, 3)

	legacy := testReceipt(scope, "start-legacy", "a")
	legacy.ExecutionID = "exec-legacy"
	legacy.Result = []byte(`{"ExecutionID":"exec-legacy","PipelineVersion":3,"Replayed":false}`)
	digest := testSnapshot("", nil).YAMLSHA256
	legacy.SnapshotHash = &digest
	if _, err := backend.Execute(ctx, scope, store.Unit{Receipt: &legacy}); err != nil {
		t.Fatalf("seed legacy receipt: %v", err)
	}

	unit := startUnit(scope, "exec-new", "start-legacy", containerAction())
	result, err := adapter.Start(ctx, unit)
	if err != nil {
		t.Fatalf("Start against legacy receipt: %v", err)
	}
	if !result.Replayed {
		t.Fatal("legacy receipt did not replay")
	}
	if result.ExecutionID != "exec-legacy" {
		t.Fatalf("legacy result execution = %q; want exec-legacy", result.ExecutionID)
	}
	if result.PipelineVersion != 3 {
		t.Fatalf("legacy result pipeline version = %d; want 3", result.PipelineVersion)
	}
	if result.ExecutionVersion != 1 {
		t.Fatalf("legacy result execution version = %d; want the insertion invariant 1", result.ExecutionVersion)
	}
	if _, err := adapter.Load(ctx, scope, "exec-new"); err == nil {
		t.Fatal("legacy receipt replay must not commit a second execution")
	}
}

// TestStartReceiptReplayReturnsPersistedExecutionVersion proves replay returns
// the execution version persisted in the receipt result instead of forcing the
// insertion invariant. The seeded value 2 is one no real commit produces, so a
// replay that hard-codes 1 is distinguishable from one that reads the member.
func TestStartReceiptReplayReturnsPersistedExecutionVersion(t *testing.T) {
	ctx := context.Background()
	adapter, backend := newStore(t)
	scope := projectScope()
	seedPipeline(t, backend, scope, 3)

	persisted := testReceipt(scope, "start-persisted", "a")
	persisted.ExecutionID = "exec-persisted"
	persisted.Result = []byte(`{"ExecutionID":"exec-persisted","ExecutionVersion":2,"PipelineVersion":3,"Replayed":false}`)
	digest := testSnapshot("", nil).YAMLSHA256
	persisted.SnapshotHash = &digest
	if _, err := backend.Execute(ctx, scope, store.Unit{Receipt: &persisted}); err != nil {
		t.Fatalf("seed persisted receipt: %v", err)
	}

	unit := startUnit(scope, "exec-persisted-retry", "start-persisted", containerAction())
	result, err := adapter.Start(ctx, unit)
	if err != nil {
		t.Fatalf("Start against persisted receipt: %v", err)
	}
	if !result.Replayed {
		t.Fatal("persisted receipt did not replay")
	}
	if result.ExecutionID != "exec-persisted" {
		t.Fatalf("replayed result execution = %q; want exec-persisted", result.ExecutionID)
	}
	if result.ExecutionVersion != 2 {
		t.Fatalf("replayed result execution version = %d; want the persisted member 2", result.ExecutionVersion)
	}
	if _, err := adapter.Load(ctx, scope, "exec-persisted-retry"); err == nil {
		t.Fatal("persisted receipt replay must not commit a second execution")
	}
}
