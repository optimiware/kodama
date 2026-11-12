package testkit

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/engine"
)

// These tests pin the runtime and workspace behavior the retire-then-replace
// sequence depends on: after a never-started identity is removed, that stable
// identity is terminal (it can never be prepared or started again), observing
// it is absent rather than never-started, removal is idempotent, and removal
// never releases or deletes the execution's retained workspace.

func TestRuntimeRemovedPreparedIdentityCannotBeReused(t *testing.T) {
	runtime, identity, workspaceID := testRuntimeWithPreparedWorkload(t)
	ctx := context.Background()

	removal, err := runtime.Remove(ctx, engine.RemoveRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatalf("Remove() for a prepared never-started identity error = %v", err)
	}
	if !removal.Removed {
		t.Fatal("Remove() for a prepared never-started identity reported not removed")
	}

	// Removal is idempotent and records one effect, not two.
	again, err := runtime.Remove(ctx, engine.RemoveRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatalf("second Remove() error = %v", err)
	}
	if !again.Removed {
		t.Fatal("idempotent Remove() reported not removed")
	}
	if len(runtime.Removals()) != 1 {
		t.Fatalf("removal effects = %d, want 1", len(runtime.Removals()))
	}

	// The stable identity is terminal: no replay of preparation or start.
	if _, err := runtime.Prepare(ctx, engine.PrepareRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
		Container: definition.Container{
			Image:    "registry.example/app@sha256:abc",
			Commands: []string{"run"},
		},
	}); err == nil {
		t.Fatal("Prepare() after removal error = nil, want refusal")
	} else {
		testClass(t, err, fault.InvalidInput)
		if !strings.Contains(err.Error(), "cannot be reused") {
			t.Fatalf("Prepare() after removal error = %v, want a stable-identity refusal", err)
		}
	}

	if _, err := runtime.Start(ctx, engine.StartRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	}); err == nil {
		t.Fatal("Start() after removal error = nil, want refusal")
	} else {
		testClass(t, err, fault.InvalidInput)
		if !strings.Contains(err.Error(), "cannot be reused") {
			t.Fatalf("Start() after removal error = %v, want a stable-identity refusal", err)
		}
	}

	// A removed identity is absent, never affirmative proof of nonexecution for
	// a replacement decision.
	observation, err := runtime.Observe(ctx, engine.ObserveRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatalf("Observe() after removal error = %v", err)
	}
	if observation.Kind != kernel.ObservationAbsent {
		t.Fatalf("observation after removal = %+v, want absent", observation)
	}
	if observation.Kind == kernel.ObservationNeverStarted || observation.ProofOfNonexecution() {
		t.Fatalf("removed identity reported proof of nonexecution: %+v", observation)
	}

	workload, ok := runtime.Workload(identity.WorkloadID)
	if !ok {
		t.Fatalf("runtime lost the removed workload record %q", identity.WorkloadID)
	}
	if !workload.Removed || workload.Started || workload.StartCount != 0 {
		t.Fatalf("workload after removal = %+v, want removed and never started", workload)
	}
	if runtime.StartCount() != 0 {
		t.Fatalf("applied start effects after removal = %d, want 0", runtime.StartCount())
	}
	if runtime.PrepareCount() != 1 {
		t.Fatalf("materializations after removal = %d, want 1", runtime.PrepareCount())
	}
}

func TestRuntimeRemoveLeavesWorkspaceReleaseToTheOwner(t *testing.T) {
	fixture := NewFixture(testNow)
	ctx := context.Background()
	workspaceID := id.WorkspaceID("ws-1")
	if err := fixture.Workspace.Retain(ctx, workspaceID); err != nil {
		t.Fatalf("Retain() error = %v", err)
	}
	identity := testIdentity("exec-1", "act-1", "wl-1")
	testPrepare(t, fixture.Runtime, identity, workspaceID)

	// While the identity still exists, it owns the workspace and the guarded
	// release is refused.
	if err := fixture.Runtime.OwnershipHeld(workspaceID); !errors.Is(err, ErrOwnershipHeld) {
		t.Fatalf("OwnershipHeld() before removal = %v, want ErrOwnershipHeld", err)
	}
	if err := fixture.Workspace.Release(ctx, workspaceID); !errors.Is(err, ErrOwnershipHeld) {
		t.Fatalf("Release() before removal = %v, want ErrOwnershipHeld", err)
	}
	if fixture.Workspace.IsReleased(workspaceID) {
		t.Fatal("refused Release() released the workspace")
	}

	if _, err := fixture.Runtime.Remove(ctx, engine.RemoveRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	}); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}

	// Removal ends ownership but never releases or deletes the workspace: that
	// is Workspace.Release's decision alone.
	if err := fixture.Runtime.OwnershipHeld(workspaceID); err != nil {
		t.Fatalf("OwnershipHeld() after removal = %v, want nil", err)
	}
	if fixture.Workspace.IsReleased(workspaceID) {
		t.Fatal("Remove() released the workspace")
	}
	if !fixture.Workspace.IsRetained(workspaceID) {
		t.Fatal("Remove() gave up workspace retention")
	}
	if fixture.Workspace.ReleaseCount() != 0 {
		t.Fatalf("release count after Remove() = %d, want 0", fixture.Workspace.ReleaseCount())
	}
	status, err := fixture.Workspace.Status(ctx, workspaceID)
	if err != nil {
		t.Fatalf("Status() after removal error = %v", err)
	}
	if !status.Available {
		t.Fatalf("workspace status after removal = %+v, want available", status)
	}

	// With every owned identity affirmatively removed, the guard now permits
	// the explicit release.
	if err := fixture.Workspace.Release(ctx, workspaceID); err != nil {
		t.Fatalf("Release() after removal error = %v", err)
	}
	if !fixture.Workspace.IsReleased(workspaceID) {
		t.Fatal("Release() after removal did not release the workspace")
	}
	if fixture.Workspace.ReleaseCount() != 1 {
		t.Fatalf("release count after explicit Release() = %d, want 1", fixture.Workspace.ReleaseCount())
	}
}

func TestRuntimeRemoveStartedUnconfirmedStillRefused(t *testing.T) {
	fixture := NewFixture(testNow)
	ctx := context.Background()
	workspaceID := id.WorkspaceID("ws-1")
	if err := fixture.Workspace.Retain(ctx, workspaceID); err != nil {
		t.Fatalf("Retain() error = %v", err)
	}
	identity := testIdentity("exec-1", "act-1", "wl-1")
	testPrepare(t, fixture.Runtime, identity, workspaceID)
	result := testStart(t, fixture.Runtime, identity, workspaceID)
	if result.Outcome != engine.StartAccepted {
		t.Fatalf("Start outcome = %q, want %q", result.Outcome, engine.StartAccepted)
	}

	if _, err := fixture.Runtime.Remove(ctx, engine.RemoveRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	}); !errors.Is(err, ErrStopUnconfirmed) {
		t.Fatalf("Remove() before confirmed stop error = %v, want ErrStopUnconfirmed", err)
	}
	workload, ok := fixture.Runtime.Workload(identity.WorkloadID)
	if !ok {
		t.Fatalf("runtime lost the started workload record %q", identity.WorkloadID)
	}
	if workload.Removed {
		t.Fatal("refused Remove() marked the started workload removed")
	}

	// A started identity that is not affirmatively stopped still holds the
	// workspace, so a release stays refused.
	if err := fixture.Runtime.OwnershipHeld(workspaceID); !errors.Is(err, ErrOwnershipHeld) {
		t.Fatalf("OwnershipHeld() after refused Remove() = %v, want ErrOwnershipHeld", err)
	}
	if err := fixture.Workspace.Release(ctx, workspaceID); !errors.Is(err, ErrOwnershipHeld) {
		t.Fatalf("Release() after refused Remove() = %v, want ErrOwnershipHeld", err)
	}
	if fixture.Workspace.IsReleased(workspaceID) {
		t.Fatal("release after a refused Remove() released the workspace")
	}
}
