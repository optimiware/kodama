package enginetest

// Bounded cleanup retry regressions. S02 §6 and K-CLEANUP make the retry
// count installation policy and require exhaustion to leave visible pending
// work, never a falsely successful cleanup record.

import (
	"errors"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
)

// cleanupBoundHarness seeds a terminal failed execution with a started workload
// and confirmed stop, so every reconciliation reaches the removal attempt.
func cleanupBoundHarness(t *testing.T, executionID, workspaceID string) (*harness, engine.EffectIdentity) {
	t.Helper()
	execution, identity := terminalFailedExecutionWithCleanup(executionID, workspaceID)
	h := newHarness(t, execution)
	h.retainWorkspace()
	h.prepareAndStart(identity, execution.Actions[0].Container.Definition)
	h.confirmRuntimeStop(identity)
	return h, identity
}

// TestCleanupRetryIsBoundedByPolicy pins S02 §6/K-CLEANUP: after MaxAttempts
// failed removals the record stays visibly pending with its attempt count and
// last error, and later passes issue no further Remove.
func TestCleanupRetryIsBoundedByPolicy(t *testing.T) {
	h, identity := cleanupBoundHarness(t, "exec-cleanup-bound", "ws-cleanup-bound")
	h.engine.SetCleanupPolicy(engine.CleanupPolicy{MaxAttempts: 2})
	h.runtime.setRemoveError(errors.New("injected removal failure"))

	for range 2 {
		h.reconcile()
	}
	pending := h.load()
	if pending.Cleanup.State != kernel.CleanupPending {
		t.Fatalf("cleanup state = %q, want pending after the attempt bound", pending.Cleanup.State)
	}
	if pending.Cleanup.Attempts != 2 {
		t.Fatalf("cleanup attempts = %d, want 2", pending.Cleanup.Attempts)
	}
	if pending.Cleanup.LastError == "" {
		t.Fatal("bounded cleanup lost the last actionable error")
	}
	if pending.Cleanup.WorkloadID != identity.WorkloadID {
		t.Fatalf("cleanup workload = %q, want %q", pending.Cleanup.WorkloadID, identity.WorkloadID)
	}
	if got := h.runtime.removeCount(); got != 2 {
		t.Fatalf("Remove calls = %d, want exactly 2 before exhaustion", got)
	}
	if pending.State != kernel.ExecutionFailed {
		t.Fatalf("terminal state = %q, want failed", pending.State)
	}
	if h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("exhausted cleanup released the workspace")
	}

	// Exhaustion is a read-only visible stop: no more removal attempts and no
	// false completion, even across repeated scans.
	for range 4 {
		h.reconcile()
	}
	exhausted := h.load()
	if exhausted.Cleanup.State != kernel.CleanupPending {
		t.Fatalf("cleanup state after exhaustion = %q, want pending", exhausted.Cleanup.State)
	}
	if exhausted.Cleanup.Attempts != 2 {
		t.Fatalf("cleanup attempts after exhaustion = %d, want 2", exhausted.Cleanup.Attempts)
	}
	if !strings.Contains(exhausted.Cleanup.LastError, "cleanup retry exhausted") {
		t.Fatalf("cleanup last error = %q, want an exhaustion diagnostic", exhausted.Cleanup.LastError)
	}
	if got := h.runtime.removeCount(); got != 2 {
		t.Fatalf("Remove calls after exhaustion = %d, want 2", got)
	}
	if h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("exhausted cleanup eventually released the workspace")
	}
	if exhausted.State != kernel.ExecutionFailed {
		t.Fatalf("terminal state after exhaustion = %q, want failed", exhausted.State)
	}
}

// TestCleanupSucceedsBeforeAttemptBound pins that a successful removal before
// the bound completes cleanup normally and preserves the failed-attempt count.
func TestCleanupSucceedsBeforeAttemptBound(t *testing.T) {
	h, _ := cleanupBoundHarness(t, "exec-cleanup-recover", "ws-cleanup-recover")
	h.engine.SetCleanupPolicy(engine.CleanupPolicy{MaxAttempts: 3})
	h.runtime.setRemoveError(errors.New("injected removal failure"))

	h.reconcile()
	pending := h.load()
	if pending.Cleanup.State != kernel.CleanupPending || pending.Cleanup.Attempts != 1 {
		t.Fatalf("cleanup after one failure = %+v, want pending with 1 attempt", pending.Cleanup)
	}

	h.runtime.setRemoveError(nil)
	completed := h.reconcileUntil("cleanup complete before the bound", 4, func(candidate kernel.Execution) bool {
		return candidate.Cleanup.State == kernel.CleanupComplete
	})
	if completed.Cleanup.Attempts != 1 {
		t.Fatalf("completed cleanup attempts = %d, want the preserved 1", completed.Cleanup.Attempts)
	}
	if completed.Cleanup.LastError != "" {
		t.Fatalf("completed cleanup last error = %q, want empty", completed.Cleanup.LastError)
	}
	if completed.State != kernel.ExecutionFailed {
		t.Fatalf("terminal state = %q, want failed", completed.State)
	}
	if !h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("successful cleanup did not release the workspace")
	}
}

// TestDefaultCleanupPolicyIsBounded pins that an engine constructed with the
// pinned New signature still enforces a finite retry bound.
func TestDefaultCleanupPolicyIsBounded(t *testing.T) {
	if engine.DefaultCleanupMaxAttempts <= 0 {
		t.Fatalf("DefaultCleanupMaxAttempts = %d, want a positive bound", engine.DefaultCleanupMaxAttempts)
	}
	h, _ := cleanupBoundHarness(t, "exec-cleanup-default", "ws-cleanup-default")
	h.runtime.setRemoveError(errors.New("injected removal failure"))

	for range engine.DefaultCleanupMaxAttempts + 3 {
		h.reconcile()
	}
	exhausted := h.load()
	if exhausted.Cleanup.State != kernel.CleanupPending {
		t.Fatalf("default cleanup state = %q, want pending", exhausted.Cleanup.State)
	}
	if exhausted.Cleanup.Attempts != engine.DefaultCleanupMaxAttempts {
		t.Fatalf("default cleanup attempts = %d, want %d", exhausted.Cleanup.Attempts, engine.DefaultCleanupMaxAttempts)
	}
	if got := h.runtime.removeCount(); got != engine.DefaultCleanupMaxAttempts {
		t.Fatalf("default Remove calls = %d, want %d", got, engine.DefaultCleanupMaxAttempts)
	}
	if !strings.Contains(exhausted.Cleanup.LastError, "cleanup retry exhausted") {
		t.Fatalf("default cleanup last error = %q, want an exhaustion diagnostic", exhausted.Cleanup.LastError)
	}
	if h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("default bounded cleanup released the workspace")
	}

	before := h.runtime.removeCount()
	for range 3 {
		h.reconcile()
	}
	if h.runtime.removeCount() != before {
		t.Fatalf("default policy kept retrying: Remove calls %d -> %d", before, h.runtime.removeCount())
	}
}
