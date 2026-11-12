package restorequal

// Q03 restore-ordering rules over the product's own installation.Recovery
// orchestration. These are pure drills: they prove that fencing precedes state
// changes, reconciliation precedes dispatch, and an unresolved external effect
// blocks dispatch without replaying a restore.

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/optimiware/kodama/internal/installation"
)

type orderedRestore struct {
	calls      *[]string
	evidence   installation.BackupEvidence
	reconcile  installation.ReconcileResult
	restoreErr error
	restores   int
}

func (port *orderedRestore) Preflight(context.Context, installation.BackupIdentity) (installation.BackupEvidence, error) {
	*port.calls = append(*port.calls, "preflight")
	return port.evidence, nil
}

func (port *orderedRestore) Restore(_ context.Context, plan installation.RestorePlan) (installation.RestoreResult, error) {
	*port.calls = append(*port.calls, "restore")
	port.restores++
	if port.restoreErr != nil {
		return installation.RestoreResult{}, port.restoreErr
	}
	return installation.RestoreResult{Fenced: plan.Fenced}, nil
}

func (port *orderedRestore) Reconcile(context.Context) (installation.ReconcileResult, error) {
	*port.calls = append(*port.calls, "reconcile")
	return port.reconcile, nil
}

type orderedFence struct {
	calls *[]string
	err   error
}

func (port orderedFence) Fence(context.Context) error {
	*port.calls = append(*port.calls, "fence")
	return port.err
}

type orderedGate struct {
	calls *[]string
}

func (gate orderedGate) EnableDispatch(context.Context) error {
	*gate.calls = append(*gate.calls, "dispatch")
	return nil
}

func assertOrder(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("restore order = %v; want %v", got, want)
	}
}

func TestCoordinatedRestoreOrdering(t *testing.T) {
	scope := installation.DefaultBackupScope()
	evidence := installation.BackupEvidence{
		Identity:          installation.BackupIdentity{ID: "backup-q03"},
		Scope:             scope,
		KeysPresent:       true,
		Preserved:         installation.PreservedState{Receipts: 1, Decisions: 1, QueuedExecutions: 1},
		MissingWorkspaces: []string{"workspace-q03"},
	}

	t.Run("fence then restore then reconcile then dispatch", func(t *testing.T) {
		var calls []string
		restore := &orderedRestore{
			calls:     &calls,
			evidence:  evidence,
			reconcile: installation.ReconcileResult{Reconciled: true},
		}
		result, err := (installation.Recovery{
			Restore:  restore,
			Fence:    orderedFence{calls: &calls},
			Dispatch: orderedGate{calls: &calls},
		}).RunRestore(context.Background(), evidence.Identity, installation.RestorePlan{Scope: scope})
		if err != nil {
			t.Fatalf("RunRestore: %v", err)
		}
		assertOrder(t, calls, []string{"preflight", "fence", "restore", "reconcile", "dispatch"})
		if !result.Fenced || !result.Reconciled {
			t.Fatalf("result = %+v; want fenced and reconciled", result)
		}
		if !reflect.DeepEqual(result.UnrecoverableWorkspaces, evidence.MissingWorkspaces) {
			t.Fatalf("unrecoverable workspaces = %v; want %v", result.UnrecoverableWorkspaces, evidence.MissingWorkspaces)
		}
	})

	t.Run("an unresolved external effect blocks dispatch without replay", func(t *testing.T) {
		var calls []string
		restore := &orderedRestore{
			calls:     &calls,
			evidence:  evidence,
			reconcile: installation.ReconcileResult{Unresolved: []string{"workload-unknown"}},
		}
		_, err := (installation.Recovery{
			Restore:  restore,
			Fence:    orderedFence{calls: &calls},
			Dispatch: orderedGate{calls: &calls},
		}).RunRestore(context.Background(), evidence.Identity, installation.RestorePlan{Scope: scope})
		if err == nil {
			t.Fatal("RunRestore enabled dispatch with an unresolved external effect")
		}
		assertOrder(t, calls, []string{"preflight", "fence", "restore", "reconcile"})
		if restore.restores != 1 {
			t.Fatalf("restore ran %d times; want exactly 1 (never replayed)", restore.restores)
		}
	})

	t.Run("a failed fence blocks the state change", func(t *testing.T) {
		var calls []string
		restore := &orderedRestore{
			calls:     &calls,
			evidence:  evidence,
			reconcile: installation.ReconcileResult{Reconciled: true},
		}
		if _, err := (installation.Recovery{
			Restore:  restore,
			Fence:    orderedFence{calls: &calls, err: errors.New("fence failed")},
			Dispatch: orderedGate{calls: &calls},
		}).RunRestore(context.Background(), evidence.Identity, installation.RestorePlan{Scope: scope}); err == nil {
			t.Fatal("RunRestore continued after a failed fence")
		}
		assertOrder(t, calls, []string{"preflight", "fence"})
		if restore.restores != 0 {
			t.Fatalf("restore ran %d times after a failed fence", restore.restores)
		}
	})

	t.Run("a failed restore blocks reconciliation and dispatch without replay", func(t *testing.T) {
		var calls []string
		restore := &orderedRestore{
			calls:      &calls,
			evidence:   evidence,
			reconcile:  installation.ReconcileResult{Reconciled: true},
			restoreErr: errors.New("restore failed"),
		}
		if _, err := (installation.Recovery{
			Restore:  restore,
			Fence:    orderedFence{calls: &calls},
			Dispatch: orderedGate{calls: &calls},
		}).RunRestore(context.Background(), evidence.Identity, installation.RestorePlan{Scope: scope}); err == nil {
			t.Fatal("RunRestore continued after a failed restore")
		}
		assertOrder(t, calls, []string{"preflight", "fence", "restore"})
		if restore.restores != 1 {
			t.Fatalf("restore ran %d times; want exactly 1 (never replayed)", restore.restores)
		}
	})

	t.Run("missing keys block fencing and restore", func(t *testing.T) {
		var calls []string
		noKeys := evidence
		noKeys.KeysPresent = false
		restore := &orderedRestore{calls: &calls, evidence: noKeys}
		if _, err := (installation.Recovery{
			Restore:  restore,
			Fence:    orderedFence{calls: &calls},
			Dispatch: orderedGate{calls: &calls},
		}).RunRestore(context.Background(), evidence.Identity, installation.RestorePlan{Scope: scope}); err == nil {
			t.Fatal("RunRestore proceeded without backup keys")
		}
		assertOrder(t, calls, []string{"preflight"})
		if restore.restores != 0 {
			t.Fatalf("restore ran %d times without keys", restore.restores)
		}
	})
}
