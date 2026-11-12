package identity_test

// Identity authority-reader vectors for the conditional operator retry. The
// adapter maps failed_task_operator_start to a real read of the task state
// and the named current execution: only an open task whose current execution is
// failed is allowed, and the observed execution version is returned so the
// commit boundary can pin it.

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// seedOperatorRetry seeds one task and its named current execution. An empty
// execution id seeds no execution row.
func (h *harness) seedOperatorRetry(scope tenancy.Scope, taskID, executionID, taskState, executionState string, lifecycle, executionVersion uint64) {
	h.t.Helper()
	values := map[string]any{
		"content_version":   int64(1),
		"lifecycle_version": int64(lifecycle),
		"state":             taskState,
		"initiator_id":      "principal-human",
		"title":             "operator retry",
		"created_at":        testNow,
	}
	if executionID != "" {
		values["current_execution_id"] = executionID
	}
	h.seed(scope, "tasks", taskID, values)
	if executionID != "" {
		h.seed(scope, "executions", executionID, map[string]any{
			"task_id":      taskID,
			"principal_id": "principal-exec",
			"state":        executionState,
			"version":      int64(executionVersion),
			"yaml_sha256":  []byte("sha256"),
			"snapshot":     []byte(`{}`),
			"created_at":   testNow,
		})
	}
}

// TestAuthorityReaderFailedTaskOperatorStart proves the conditional read maps
// to the task state and current execution state.
func TestAuthorityReaderFailedTaskOperatorStart(t *testing.T) {
	cases := []struct {
		name           string
		taskState      string
		executionState string
		executionID    string
		allowed        bool
	}{
		{name: "open and failed", taskState: "open", executionState: "failed", executionID: "execution-failed", allowed: true},
		{name: "open and running", taskState: "open", executionState: "running", executionID: "execution-failed"},
		{name: "open and succeeded", taskState: "open", executionState: "succeeded", executionID: "execution-failed"},
		{name: "waiting and failed", taskState: "waiting", executionState: "failed", executionID: "execution-failed"},
		{name: "done and failed", taskState: "done", executionState: "failed", executionID: "execution-failed"},
		{name: "cancelled and failed", taskState: "cancelled", executionState: "failed", executionID: "execution-failed"},
		{name: "no execution", taskState: "open"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h := newHarness(t)
			scope := projectScope("tenant-a", "project-a")
			h.seedOperatorRetry(scope, "task-1", testCase.executionID, testCase.taskState, testCase.executionState, 2, 3)

			read := authorize.AuthorityRead{
				Operation:       "task.start",
				Check:           authorize.CheckFailedTaskOperatorStart,
				Scope:           scope,
				PrincipalID:     "principal-operator",
				TargetID:        testCase.executionID,
				ExpectedVersion: 3,
			}
			if testCase.executionID == "" {
				read.ExpectedVersion = 0
			}
			facts, err := h.adapter.Read(context.Background(), []authorize.AuthorityRead{read})
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if facts[0].Allowed != testCase.allowed {
				t.Fatalf("allowed = %t; want %t (%+v)", facts[0].Allowed, testCase.allowed, facts[0])
			}
			if testCase.allowed && facts[0].Version != 3 {
				t.Fatalf("observed execution version = %d; want 3", facts[0].Version)
			}
		})
	}
}

// TestAuthorityReaderFailedTaskOperatorStartVersionMoved proves a moved
// execution version surfaces as a version_conflict through Confirm, never as a
// silently accepted retry.
func TestAuthorityReaderFailedTaskOperatorStartVersionMoved(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedOperatorRetry(scope, "task-1", "execution-failed", "open", "failed", 2, 3)
	read := authorize.AuthorityRead{
		Operation:       "task.start",
		Check:           authorize.CheckFailedTaskOperatorStart,
		Scope:           scope,
		PrincipalID:     "principal-operator",
		TargetID:        "execution-failed",
		ExpectedVersion: 2,
	}
	facts, err := h.adapter.Read(context.Background(), []authorize.AuthorityRead{read})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	requireFaultClass(t, authorize.Confirm([]authorize.AuthorityRead{read}, facts), fault.VersionConflict)
}

// TestAuthorityReaderFailedTaskOperatorStartRefusesStaleNamedExecution proves
// the conditional read pins the task's CURRENT execution, not merely some failed
// execution of the same task. A failed execution that is no longer the task's
// current execution is not allowed.
func TestAuthorityReaderFailedTaskOperatorStartRefusesStaleNamedExecution(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedOperatorRetry(scope, "task-1", "execution-current", "open", "failed", 2, 3)
	h.seed(scope, "executions", "execution-stale", map[string]any{
		"task_id":      "task-1",
		"principal_id": "principal-exec",
		"state":        "failed",
		"version":      int64(4),
		"yaml_sha256":  []byte("sha256"),
		"snapshot":     []byte(`{}`),
		"created_at":   testNow,
	})
	read := authorize.AuthorityRead{
		Operation:       "task.start",
		Check:           authorize.CheckFailedTaskOperatorStart,
		Scope:           scope,
		PrincipalID:     "principal-operator",
		TargetID:        "execution-stale",
		ExpectedVersion: 4,
	}
	facts, err := h.adapter.Read(context.Background(), []authorize.AuthorityRead{read})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if facts[0].Allowed {
		t.Fatalf("a stale named execution was allowed: %+v", facts[0])
	}
	requireFaultClass(t, authorize.Confirm([]authorize.AuthorityRead{read}, facts), fault.Forbidden)
}

// TestAuthorityReaderFailedTaskOperatorStartMissingExecution proves a named
// execution row that does not exist fails closed instead of allowing.
func TestAuthorityReaderFailedTaskOperatorStartMissingExecution(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedOperatorRetry(scope, "task-1", "", "open", "", 2, 0)
	read := authorize.AuthorityRead{
		Operation:   "task.start",
		Check:       authorize.CheckFailedTaskOperatorStart,
		Scope:       scope,
		PrincipalID: "principal-operator",
		TargetID:    "execution-missing",
	}
	_, err := h.adapter.Read(context.Background(), []authorize.AuthorityRead{read})
	requireFaultClass(t, err, fault.NotFound)
}
