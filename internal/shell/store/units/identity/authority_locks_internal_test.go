package identity

// Internal vectors for the identity adapter's failed_task_operator_start lock
// mapping. The task.start unit is handled by the work adapter, but the identity
// adapter shares the authority-lock translation and must fail closed if it is
// ever handed the conditional read: the observed execution must still be the
// task's current failed execution, and its observed version must match.

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func TestAuthorityLocksFailedTaskOperatorStart(t *testing.T) {
	scope := tenancy.Scope{TenantID: id.TenantID("tenant-a"), ProjectID: id.ProjectID("project-a")}
	now := time.Date(2026, time.November, 4, 12, 0, 0, 0, time.UTC)
	instance := memory.NewWithClock(func() time.Time { return now })
	adapter := &Adapter{executor: instance, scanner: instance}
	seed := func(table, identity string, values map[string]any) {
		t.Helper()
		if _, err := instance.Execute(context.Background(), scope, store.Unit{
			Writes: []store.Write{{Table: table, ID: identity, Insert: true, Values: values}},
		}); err != nil {
			t.Fatalf("seed %s/%s: %v", table, identity, err)
		}
	}
	seed("tasks", "task-1", map[string]any{
		"content_version":      int64(1),
		"lifecycle_version":    int64(2),
		"state":                "open",
		"initiator_id":         "principal-human",
		"title":                "operator retry",
		"created_at":           now,
		"current_execution_id": "execution-failed",
	})
	seed("executions", "execution-failed", map[string]any{
		"task_id":      "task-1",
		"principal_id": "principal-exec",
		"state":        "failed",
		"version":      int64(3),
		"yaml_sha256":  []byte("sha256"),
		"snapshot":     []byte(`{}`),
		"created_at":   now,
	})
	read := authorize.AuthorityRead{
		Operation:       "task.start",
		Check:           authorize.CheckFailedTaskOperatorStart,
		Scope:           scope,
		PrincipalID:     "principal-operator",
		TargetID:        "execution-failed",
		ExpectedVersion: 3,
	}

	locks, err := adapter.authorityLocks(context.Background(), []authorize.AuthorityRead{read})
	if err != nil {
		t.Fatalf("allowed authorityLocks: %v", err)
	}
	if len(locks) != 2 {
		t.Fatalf("locks = %+v; want the task and execution pins", locks)
	}
	if locks[0].Table != "tasks" || locks[1].Table != "executions" {
		t.Fatalf("lock order = %s/%s; want tasks/executions", locks[0].Table, locks[1].Table)
	}
	if locks[1].ExpectVersion != 3 || locks[1].Expect["state"] != "failed" {
		t.Fatalf("execution lock = %+v; want version 3 failed", locks[1])
	}

	// A moved observed version is a version_conflict.
	moved := read
	moved.ExpectedVersion = 2
	if _, err := adapter.authorityLocks(context.Background(), []authorize.AuthorityRead{moved}); err == nil {
		t.Fatal("a moved execution version was accepted")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
		t.Fatalf("moved version error = %v; want version_conflict", err)
	}

	// An unversioned read pins the observed version without comparing.
	unversioned := read
	unversioned.ExpectedVersion = 0
	if _, err := adapter.authorityLocks(context.Background(), []authorize.AuthorityRead{unversioned}); err != nil {
		t.Fatalf("unversioned authorityLocks: %v", err)
	}

	// A non-failed execution fails closed.
	if _, err := instance.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:         "executions",
			ID:            "execution-failed",
			ExpectVersion: 3,
			Values:        map[string]any{"state": "running"},
		}},
	}); err != nil {
		t.Fatalf("move execution state: %v", err)
	}
	if _, err := adapter.authorityLocks(context.Background(), []authorize.AuthorityRead{read}); err == nil {
		t.Fatal("a running current execution was accepted as a retry")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("running execution error = %v; want forbidden", err)
	}
}
