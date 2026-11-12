//go:build qualification

package production

// The cleanup-crash attribution probe keeps two decisions pure so they can be
// pinned without a stage: what the cleanup state observed after the crash means,
// and whether the observed record is the obligation the terminal commit
// persisted. Both probe rounds regressed here once: a vacuum skip and an
// identity comparison that skipped when the workspace identity was empty.

import (
	"fmt"
	"testing"

	"github.com/optimiware/kodama/api"
)

// resilienceCleanupCrashNotAttributable is the named skip reason for a cleanup
// obligation that was already complete when the replacement could first serve
// an inspection. The probe must not pass unless the replacement resumed an
// interrupted cleanup.
const resilienceCleanupCrashNotAttributable = "resilience-cleanup-crash-not-attributable"

// resilienceCleanupCrashVerdict classifies the cleanup state observed after the
// crashed controller pod is gone. "pending" is the interrupted obligation the
// probe requires. "complete" means cleanup finished in the gap between the
// crash and the first post-crash read; that completion cannot be attributed to
// the replacement, so the probe skips with the named reason. Any other state is
// not an obligation this probe can explain and fails.
func resilienceCleanupCrashVerdict(state string) (string, error) {
	switch state {
	case "pending":
		return "", nil
	case "complete":
		return resilienceCleanupCrashNotAttributable, nil
	default:
		return "", fmt.Errorf("cleanup state %q is not a pending obligation", state)
	}
}

// resilienceCleanupObligationError proves an observed cleanup record is the
// obligation the pre-crash terminal commit persisted.
//
// A pending record always carries the workspace identity: every cleanup write
// in internal/kernel/engine/cleanup.go passes execution.WorkspaceID, so an
// empty pre-crash workspace identity is corrupt and must fail, never skip the
// comparison. The workload identity may be empty while no owned identity is
// being removed (commitCleanupProgress is called with an empty workload id when
// the execution never issued one), but a non-empty persisted identity must
// survive unchanged. The observed record may be the pending obligation after
// the crash or the completed record after the replacement finished it.
func resilienceCleanupObligationError(pending, observed api.ExecutionCleanup) error {
	if pending.State != "pending" {
		return fmt.Errorf("pre-crash cleanup state %q is not pending", pending.State)
	}
	if pending.WorkspaceID == "" {
		return fmt.Errorf("pre-crash pending cleanup carries no workspace identity")
	}
	if observed.WorkspaceID != pending.WorkspaceID {
		return fmt.Errorf("workspace identity changed across the controller crash: pre-crash %q, observed %q",
			pending.WorkspaceID, observed.WorkspaceID)
	}
	if pending.WorkloadID != "" && observed.WorkloadID != pending.WorkloadID {
		return fmt.Errorf("workload identity changed across the controller crash: pre-crash %q, observed %q",
			pending.WorkloadID, observed.WorkloadID)
	}
	return nil
}

// TestHarnessCleanupCrashAttributionIdentity pins the identity comparison the
// cleanup-crash attribution probe uses. Nothing here needs a stage.
func TestHarnessCleanupCrashAttributionIdentity(t *testing.T) {
	obligation := api.ExecutionCleanup{State: "pending", WorkspaceID: "workspace-rw-1", WorkloadID: "workload-1"}
	cases := []struct {
		name     string
		pending  api.ExecutionCleanup
		observed api.ExecutionCleanup
		accepted bool
	}{
		{
			name:     "the pending obligation is unchanged after the crash",
			pending:  obligation,
			observed: api.ExecutionCleanup{State: "pending", WorkspaceID: "workspace-rw-1", WorkloadID: "workload-1"},
			accepted: true,
		},
		{
			name:     "the completed record is the same obligation",
			pending:  obligation,
			observed: api.ExecutionCleanup{State: "complete", WorkspaceID: "workspace-rw-1", WorkloadID: "workload-1"},
			accepted: true,
		},
		{
			name:     "an empty persisted workload identity is allowed",
			pending:  api.ExecutionCleanup{State: "pending", WorkspaceID: "workspace-rw-1"},
			observed: api.ExecutionCleanup{State: "complete", WorkspaceID: "workspace-rw-1", WorkloadID: "workload-1"},
			accepted: true,
		},
		{
			name:     "an empty pending workspace identity is corrupt, not a skip",
			pending:  api.ExecutionCleanup{State: "pending", WorkloadID: "workload-1"},
			observed: api.ExecutionCleanup{State: "pending", WorkloadID: "workload-1"},
			accepted: false,
		},
		{
			name:     "a changed workspace identity is not the same cleanup",
			pending:  obligation,
			observed: api.ExecutionCleanup{State: "complete", WorkspaceID: "workspace-rw-2", WorkloadID: "workload-1"},
			accepted: false,
		},
		{
			name:     "a changed workload identity is not the same cleanup",
			pending:  obligation,
			observed: api.ExecutionCleanup{State: "complete", WorkspaceID: "workspace-rw-1", WorkloadID: "workload-2"},
			accepted: false,
		},
		{
			name:     "a completed record without a workspace identity is not the same cleanup",
			pending:  obligation,
			observed: api.ExecutionCleanup{State: "complete"},
			accepted: false,
		},
		{
			name:     "a record that was never pending is not an obligation",
			pending:  api.ExecutionCleanup{State: "complete", WorkspaceID: "workspace-rw-1", WorkloadID: "workload-1"},
			observed: api.ExecutionCleanup{State: "complete", WorkspaceID: "workspace-rw-1", WorkloadID: "workload-1"},
			accepted: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := resilienceCleanupObligationError(tc.pending, tc.observed)
			if tc.accepted && err != nil {
				t.Fatalf("obligation %+v observed as %+v was refused: %v", tc.pending, tc.observed, err)
			}
			if !tc.accepted && err == nil {
				t.Fatalf("obligation %+v observed as %+v was accepted; want a named refusal", tc.pending, tc.observed)
			}
		})
	}
}

// TestHarnessCleanupCrashVerdict pins the post-crash state classification.
func TestHarnessCleanupCrashVerdict(t *testing.T) {
	reason, err := resilienceCleanupCrashVerdict("pending")
	if err != nil || reason != "" {
		t.Fatalf("pending cleanup verdict = %q, err = %v; want attributable", reason, err)
	}
	reason, err = resilienceCleanupCrashVerdict("complete")
	if err != nil {
		t.Fatalf("completed cleanup verdict failed: %v", err)
	}
	if reason != resilienceCleanupCrashNotAttributable {
		t.Fatalf("completed cleanup verdict = %q, want the named reason %q", reason, resilienceCleanupCrashNotAttributable)
	}
	for _, state := range []string{"none", ""} {
		if _, err := resilienceCleanupCrashVerdict(state); err == nil {
			t.Fatalf("cleanup state %q was accepted; want a refusal", state)
		}
	}
}
