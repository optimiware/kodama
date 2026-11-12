package testkit

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/engine"
)

var _ engine.Workspace = (*Workspace)(nil)

func TestWorkspaceRetainAndStatus(t *testing.T) {
	workspace := NewWorkspace()
	workspaceID := id.WorkspaceID("ws-1")

	observation, err := workspace.Status(context.Background(), workspaceID)
	if err != nil {
		t.Fatalf("Status(unretained) error = %v", err)
	}
	if observation.Available {
		t.Fatal("unretained workspace reported available")
	}
	if observation.WorkspaceID != workspaceID || observation.Detail == "" {
		t.Fatalf("unretained observation = %+v", observation)
	}

	if err := workspace.Retain(context.Background(), workspaceID); err != nil {
		t.Fatalf("Retain() error = %v", err)
	}
	if !workspace.IsRetained(workspaceID) {
		t.Fatal("Retain() did not record retention")
	}
	observation, err = workspace.Status(context.Background(), workspaceID)
	if err != nil {
		t.Fatalf("Status(retained) error = %v", err)
	}
	if !observation.Available {
		t.Fatalf("retained workspace unavailable: %+v", observation)
	}

	if err := workspace.Retain(context.Background(), workspaceID); err != nil {
		t.Fatalf("idempotent Retain() error = %v", err)
	}
	if workspace.RetainCount() != 1 {
		t.Fatalf("RetainCount() = %d, want 1", workspace.RetainCount())
	}
}

func TestWorkspaceLossIsNotRecreation(t *testing.T) {
	workspace := NewWorkspace()
	workspaceID := id.WorkspaceID("ws-1")
	if err := workspace.Retain(context.Background(), workspaceID); err != nil {
		t.Fatalf("Retain() error = %v", err)
	}

	workspace.Lose(workspaceID)

	for attempt := range 3 {
		observation, err := workspace.Status(context.Background(), workspaceID)
		if err != nil {
			t.Fatalf("Status(lost) error = %v", err)
		}
		if observation.Available {
			t.Fatalf("Status(lost) attempt %d reported available", attempt)
		}
		if observation.Detail == "" {
			t.Fatalf("Status(lost) attempt %d had no detail", attempt)
		}
	}
	if !workspace.IsRetained(workspaceID) {
		t.Fatal("loss silently dropped ownership")
	}
	if workspace.RetainCount() != 1 {
		t.Fatalf("Status(lost) recreated the workspace: RetainCount() = %d", workspace.RetainCount())
	}

	if err := workspace.Retain(context.Background(), workspaceID); !errors.Is(err, ErrWorkspaceLost) {
		t.Fatalf("Retain(lost) error = %v, want ErrWorkspaceLost", err)
	}
	observation, _ := workspace.Status(context.Background(), workspaceID)
	if observation.Available {
		t.Fatal("failed Retain(lost) recreated the workspace")
	}
}

func TestWorkspaceReleaseGuardHoldsUntilConfirmedStop(t *testing.T) {
	workspace := NewWorkspace()
	workspaceID := id.WorkspaceID("ws-1")
	if err := workspace.Retain(context.Background(), workspaceID); err != nil {
		t.Fatalf("Retain() error = %v", err)
	}

	workspace.SetGuard(func(id.WorkspaceID) error {
		return ErrOwnershipHeld
	})
	if err := workspace.Release(context.Background(), workspaceID); !errors.Is(err, ErrOwnershipHeld) {
		t.Fatalf("guarded Release() error = %v, want ErrOwnershipHeld", err)
	}
	if workspace.IsReleased(workspaceID) {
		t.Fatal("guarded Release() released the workspace")
	}
	observation, _ := workspace.Status(context.Background(), workspaceID)
	if !observation.Available {
		t.Fatal("guarded Release() changed workspace availability")
	}

	workspace.SetGuard(func(id.WorkspaceID) error {
		return nil
	})
	if err := workspace.Release(context.Background(), workspaceID); err != nil {
		t.Fatalf("unguarded Release() error = %v", err)
	}
	if !workspace.IsReleased(workspaceID) {
		t.Fatal("Release() did not record release")
	}
	if workspace.IsRetained(workspaceID) {
		t.Fatal("Release() kept the workspace retained")
	}
	observation, _ = workspace.Status(context.Background(), workspaceID)
	if observation.Available {
		t.Fatal("released workspace reported available")
	}

	if err := workspace.Release(context.Background(), workspaceID); err != nil {
		t.Fatalf("idempotent Release() error = %v", err)
	}
	if workspace.ReleaseCount() != 1 {
		t.Fatalf("ReleaseCount() = %d, want 1", workspace.ReleaseCount())
	}
}

func TestWorkspaceReleaseUnknownIsNotFound(t *testing.T) {
	workspace := NewWorkspace()
	err := workspace.Release(context.Background(), id.WorkspaceID("ws-missing"))
	testClass(t, err, fault.NotFound)
}

func TestWorkspaceContextCancellation(t *testing.T) {
	workspace := NewWorkspace()
	workspaceID := id.WorkspaceID("ws-1")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := workspace.Status(ctx, workspaceID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Status(canceled) error = %v, want context.Canceled", err)
	}
	if err := workspace.Retain(ctx, workspaceID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Retain(canceled) error = %v, want context.Canceled", err)
	}
	if workspace.IsRetained(workspaceID) {
		t.Fatal("canceled Retain() retained the workspace")
	}

	if err := workspace.Retain(context.Background(), workspaceID); err != nil {
		t.Fatalf("Retain() error = %v", err)
	}
	if err := workspace.Release(ctx, workspaceID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Release(canceled) error = %v, want context.Canceled", err)
	}
	if workspace.IsReleased(workspaceID) {
		t.Fatal("canceled Release() released the workspace")
	}
}

func TestWorkspaceInstancesAreIndependent(t *testing.T) {
	first := NewWorkspace()
	second := NewWorkspace()
	workspaceID := id.WorkspaceID("ws-1")

	if err := first.Retain(context.Background(), workspaceID); err != nil {
		t.Fatalf("first Retain() error = %v", err)
	}
	if second.IsRetained(workspaceID) {
		t.Fatal("second workspace saw the first workspace's retention")
	}
	observation, err := second.Status(context.Background(), workspaceID)
	if err != nil {
		t.Fatalf("second Status() error = %v", err)
	}
	if observation.Available {
		t.Fatal("second workspace reported the first workspace's availability")
	}
}
