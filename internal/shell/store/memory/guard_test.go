// Versioned writes, guarded deletes and commit-boundary lock guards for the
// memory adapter. The adapter has no SQL text, so these tests assert the
// observable behavior the PostgreSQL reference adapter produces: versions
// advance by one, stale guards refuse the whole unit, scoped locks cannot see a
// foreign row, and an unguarded delete never removes anything.
package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/store"
)

func TestVersionedWritesAdvanceByOne(t *testing.T) {
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "executions", testExecutionID, executionValues(1, "queued"))

	mustExecute(t, s, scope, store.Unit{Writes: []store.Write{
		{Table: "executions", ID: testExecutionID, ExpectVersion: 1, Values: map[string]any{"state": "running"}},
	}})
	execution := mustSnapshot(t, s, scope, "executions", testExecutionID)
	if got := rowVersion(t, execution, "version"); got != 2 {
		t.Errorf("execution version = %d; want 2", got)
	}
	if got := rowString(t, execution, "state"); got != "running" {
		t.Errorf("execution state = %q; want running", got)
	}

	_, err := s.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{
		{Table: "executions", ID: testExecutionID, ExpectVersion: 1, Values: map[string]any{"state": "succeeded"}},
	}})
	assertFault(t, err, fault.VersionConflict)
	execution = mustSnapshot(t, s, scope, "executions", testExecutionID)
	if got := rowVersion(t, execution, "version"); got != 2 {
		t.Errorf("stale write advanced version to %d; want 2", got)
	}
	if got := rowString(t, execution, "state"); got != "running" {
		t.Errorf("stale write changed state to %q; want running", got)
	}

	mustExecute(t, s, scope, store.Unit{Writes: []store.Write{
		{Table: "executions", ID: testExecutionID, ExpectVersion: 2, Values: map[string]any{"state": "succeeded"}},
	}})
	execution = mustSnapshot(t, s, scope, "executions", testExecutionID)
	if got := rowVersion(t, execution, "version"); got != 3 {
		t.Errorf("execution version = %d; want 3", got)
	}
	if got := rowString(t, execution, "state"); got != "succeeded" {
		t.Errorf("execution state = %q; want succeeded", got)
	}
}

func TestVersionlessTaskWritesUseNamedOptimisticColumns(t *testing.T) {
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "tasks", testTaskID, taskValues(1, "open"))

	mustExecute(t, s, scope, store.Unit{Writes: []store.Write{
		{Table: "tasks", ID: testTaskID, Expect: map[string]any{"lifecycle_version": uint64(1)}, Values: map[string]any{
			"state": "waiting", "lifecycle_version": uint64(2),
		}},
	}})
	task := mustSnapshot(t, s, scope, "tasks", testTaskID)
	if got := rowString(t, task, "state"); got != "waiting" {
		t.Errorf("task state = %q; want waiting", got)
	}
	if got := rowVersion(t, task, "lifecycle_version"); got != 2 {
		t.Errorf("task lifecycle version = %d; want 2", got)
	}

	_, err := s.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{
		{Table: "tasks", ID: testTaskID, Expect: map[string]any{"lifecycle_version": uint64(1)}, Values: map[string]any{"state": "done"}},
	}})
	assertFault(t, err, fault.VersionConflict)
	task = mustSnapshot(t, s, scope, "tasks", testTaskID)
	if got := rowString(t, task, "state"); got != "waiting" {
		t.Errorf("stale task write changed state to %q; want waiting", got)
	}
}

func TestInsertDuplicateRefusedWithoutChange(t *testing.T) {
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "executions", testExecutionID, executionValues(1, "queued"))

	_, err := s.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{
		{Table: "executions", ID: testExecutionID, Insert: true, Values: executionValues(1, "running")},
	}})
	assertFault(t, err, fault.VersionConflict)
	execution := mustSnapshot(t, s, scope, "executions", testExecutionID)
	if got := rowString(t, execution, "state"); got != "queued" {
		t.Errorf("duplicate insert changed the row state to %q; want queued", got)
	}
	if got := rowVersion(t, execution, "version"); got != 1 {
		t.Errorf("duplicate insert changed the row version to %d; want 1", got)
	}

	// A duplicate insert inside one unit must be detected while staging, before
	// any part of the unit is published.
	_, err = s.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{
		{Table: "executions", ID: "execution-2", Insert: true, Values: executionValues(1, "queued")},
		{Table: "executions", ID: "execution-2", Insert: true, Values: executionValues(1, "queued")},
	}})
	assertFault(t, err, fault.VersionConflict)
	assertAbsent(t, s, scope, "executions", "execution-2")
}

func TestWriteShapeRefusalsLeaveTheStoreUnchanged(t *testing.T) {
	cases := []struct {
		name  string
		write store.Write
	}{
		{"insert with expected version", store.Write{Table: "executions", ID: "execution-2", Insert: true, ExpectVersion: 1, Values: executionValues(1, "queued")}},
		{"update without expected version", store.Write{Table: "executions", ID: testExecutionID, Values: map[string]any{"state": "running"}}},
		{"update replaces the version column", store.Write{Table: "executions", ID: testExecutionID, ExpectVersion: 1, Values: map[string]any{"version": uint64(2)}}},
		{"update sets no column", store.Write{Table: "executions", ID: testExecutionID, ExpectVersion: 1}},
		{"unknown column", store.Write{Table: "executions", ID: testExecutionID, ExpectVersion: 1, Values: map[string]any{"not_a_column": "x"}}},
		{"null in a non-nullable column", store.Write{Table: "executions", ID: testExecutionID, ExpectVersion: 1, Values: map[string]any{"state": nil}}},
		{"insert and delete", store.Write{Table: "executions", ID: testExecutionID, Insert: true, Delete: true, ExpectVersion: 1}},
		{"delete with values", store.Write{Table: "executions", ID: testExecutionID, Delete: true, ExpectVersion: 1, Values: map[string]any{"state": "running"}}},
		{"versioned delete without expected version", store.Write{Table: "executions", ID: testExecutionID, Delete: true}},
		{"versionless delete without a column guard", store.Write{Table: "tasks", ID: testTaskID, Delete: true}},
		{"versionless delete with an expected version", store.Write{Table: "tasks", ID: testTaskID, Delete: true, ExpectVersion: 1}},
		{"invalid row id", store.Write{Table: "executions", ID: "bad id", Insert: true, Values: executionValues(1, "queued")}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			s := New()
			scope := projectScope()
			seedRow(t, s, scope, "executions", testExecutionID, executionValues(1, "queued"))
			seedRow(t, s, scope, "tasks", testTaskID, taskValues(1, "open"))

			_, err := s.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{testCase.write}})
			assertFault(t, err, fault.InvalidInput)

			execution := mustSnapshot(t, s, scope, "executions", testExecutionID)
			if got := rowString(t, execution, "state"); got != "queued" {
				t.Errorf("refused write changed execution state to %q; want queued", got)
			}
			task := mustSnapshot(t, s, scope, "tasks", testTaskID)
			if got := rowString(t, task, "state"); got != "open" {
				t.Errorf("refused write changed task state to %q; want open", got)
			}
		})
	}
}

func TestLockGuardsRefuseStaleMissingAndMismatchedRows(t *testing.T) {
	t.Run("stale expected version", func(t *testing.T) {
		s := New()
		scope := tenantScope()
		values := insertValues("services")
		values["version"] = uint64(2)
		seedRow(t, s, scope, "services", testServiceID, values)

		unit := store.Unit{
			Locks:  []store.Lock{{Table: "services", IDs: []string{testServiceID}, ExpectVersion: 3}},
			Writes: []store.Write{{Table: "services", ID: testServiceID, ExpectVersion: 3, Values: map[string]any{"state": "disabled"}}},
			Events: []event.Envelope{defaultEvent(scope, "event-1", event.TypeServiceChanged, testServiceID)},
		}
		_, err := s.Execute(context.Background(), scope, unit)
		assertFault(t, err, fault.VersionConflict)

		service := mustSnapshot(t, s, scope, "services", testServiceID)
		if got := rowString(t, service, "state"); got != "enabled" {
			t.Errorf("service state = %q; want enabled", got)
		}
		if events := s.Events(scope); len(events) != 0 {
			t.Errorf("a stale guard published %d events", len(events))
		}
	})

	t.Run("missing plain lock", func(t *testing.T) {
		s := New()
		scope := projectScope()
		unit := store.Unit{
			Locks:  []store.Lock{{Table: "executions", IDs: []string{testExecutionID}}},
			Writes: []store.Write{{Table: "actions", ID: testActionID, Insert: true, Values: insertValues("actions")}},
		}
		_, err := s.Execute(context.Background(), scope, unit)
		assertFault(t, err, fault.VersionConflict)
		assertAbsent(t, s, scope, "actions", testActionID)
	})

	t.Run("column guard mismatch", func(t *testing.T) {
		s := New()
		scope := projectScope()
		seedRow(t, s, scope, "memberships", "membership-1", insertValues("memberships"))

		unit := store.Unit{
			Locks:  []store.Lock{{Table: "memberships", IDs: []string{"membership-1"}, Expect: map[string]any{"principal_id": "principal-2"}}},
			Writes: []store.Write{{Table: "actions", ID: testActionID, Insert: true, Values: insertValues("actions")}},
		}
		_, err := s.Execute(context.Background(), scope, unit)
		assertFault(t, err, fault.VersionConflict)
		assertAbsent(t, s, scope, "actions", testActionID)

		matched := store.Unit{
			Locks:  []store.Lock{{Table: "memberships", IDs: []string{"membership-1"}, Expect: map[string]any{"principal_id": testPrincipalID}}},
			Writes: []store.Write{{Table: "actions", ID: testActionID, Insert: true, Values: insertValues("actions")}},
		}
		mustExecute(t, s, scope, matched)
		if _, found := s.Snapshot(scope, "actions", testActionID); !found {
			t.Error("a matching column guard did not let the unit commit")
		}
	})

	t.Run("row in another project is out of scope", func(t *testing.T) {
		s := New()
		seedRow(t, s, otherProjectScope(), "executions", testExecutionID, executionValues(1, "queued"))
		_, err := s.Execute(context.Background(), projectScope(), store.Unit{
			Locks: []store.Lock{{Table: "executions", IDs: []string{testExecutionID}}},
		})
		assertFault(t, err, fault.VersionConflict)
	})
}

func TestTenantWideGuardTargetsTenantRowFromProjectScope(t *testing.T) {
	s := New()
	values := insertValues("services")
	values["version"] = uint64(2)
	seedRow(t, s, tenantScope(), "services", testServiceID, values)
	scope := projectScope()

	guarded := store.Unit{Locks: []store.Lock{{
		Table:         "services",
		IDs:           []string{testServiceID},
		ExpectVersion: 2,
		TenantWide:    true,
	}}}
	mustExecute(t, s, scope, guarded)

	_, err := s.Execute(context.Background(), scope, store.Unit{Locks: []store.Lock{{
		Table:         "services",
		IDs:           []string{testServiceID},
		ExpectVersion: 2,
	}}})
	assertFault(t, err, fault.VersionConflict)

	_, err = s.Execute(context.Background(), scope, store.Unit{Locks: []store.Lock{{
		Table:         "services",
		IDs:           []string{testServiceID},
		ExpectVersion: 1,
		TenantWide:    true,
	}}})
	assertFault(t, err, fault.VersionConflict)

	_, err = s.Execute(context.Background(), tenantScope(), store.Unit{Locks: []store.Lock{{
		Table:      "tenants",
		IDs:        []string{testTenantID},
		TenantWide: true,
	}}})
	assertFault(t, err, fault.InvalidInput)
}

func TestGuardedDeleteCommitsWithItsReceiptAndEvent(t *testing.T) {
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "executions", testExecutionID, executionValues(2, "running"))
	record := defaultReceipt(t)
	unit := store.Unit{
		Writes:  []store.Write{{Table: "executions", ID: testExecutionID, Delete: true, ExpectVersion: 2}},
		Receipt: &record,
		Events:  []event.Envelope{defaultEvent(scope, "event-1", event.TypeExecutionCancelled, testExecutionID)},
	}
	mustExecute(t, s, scope, unit)

	assertAbsent(t, s, scope, "executions", testExecutionID)
	if _, found := s.Receipt(record.Key); !found {
		t.Error("the delete unit did not commit its receipt")
	}
	if events := s.Events(scope); len(events) != 1 {
		t.Errorf("the delete unit committed %d events; want 1", len(events))
	}
}

func TestGuardedDeleteRefusesStaleMissingAndOutOfScopeRows(t *testing.T) {
	t.Run("stale version", func(t *testing.T) {
		s := New()
		scope := projectScope()
		seedRow(t, s, scope, "executions", testExecutionID, executionValues(2, "running"))
		record := defaultReceipt(t)
		_, err := s.Execute(context.Background(), scope, store.Unit{
			Writes:  []store.Write{{Table: "executions", ID: testExecutionID, Delete: true, ExpectVersion: 1}},
			Receipt: &record,
		})
		assertFault(t, err, fault.VersionConflict)
		if _, found := s.Snapshot(scope, "executions", testExecutionID); !found {
			t.Error("a stale delete removed the row")
		}
		if _, found := s.Receipt(record.Key); found {
			t.Error("a stale delete committed its receipt")
		}
	})

	t.Run("missing row", func(t *testing.T) {
		s := New()
		_, err := s.Execute(context.Background(), projectScope(), store.Unit{
			Writes: []store.Write{{Table: "executions", ID: testExecutionID, Delete: true, ExpectVersion: 1}},
		})
		assertFault(t, err, fault.VersionConflict)
	})

	t.Run("row in another project", func(t *testing.T) {
		s := New()
		seedRow(t, s, otherProjectScope(), "executions", testExecutionID, executionValues(2, "running"))
		_, err := s.Execute(context.Background(), projectScope(), store.Unit{
			Writes: []store.Write{{Table: "executions", ID: testExecutionID, Delete: true, ExpectVersion: 2}},
		})
		assertFault(t, err, fault.VersionConflict)
		if _, found := s.Snapshot(otherProjectScope(), "executions", testExecutionID); !found {
			t.Error("a cross-scope delete removed the foreign row")
		}
	})

	t.Run("tenant-wide row from project scope", func(t *testing.T) {
		s := New()
		values := insertValues("services")
		values["version"] = uint64(2)
		seedRow(t, s, tenantScope(), "services", testServiceID, values)

		_, err := s.Execute(context.Background(), projectScope(), store.Unit{
			Writes: []store.Write{{Table: "services", ID: testServiceID, Delete: true, ExpectVersion: 2}},
		})
		assertFault(t, err, fault.VersionConflict)
		if _, found := s.Snapshot(tenantScope(), "services", testServiceID); !found {
			t.Error("a project-scoped delete removed a tenant-wide row")
		}

		mustExecute(t, s, tenantScope(), store.Unit{
			Writes: []store.Write{{Table: "services", ID: testServiceID, Delete: true, ExpectVersion: 2}},
		})
		if _, found := s.Snapshot(tenantScope(), "services", testServiceID); found {
			t.Error("a tenant-scoped guarded delete did not remove the tenant-wide row")
		}
	})

	t.Run("versionless delete with column guard", func(t *testing.T) {
		s := New()
		scope := projectScope()
		seedRow(t, s, scope, "tasks", testTaskID, taskValues(1, "open"))
		mustExecute(t, s, scope, store.Unit{
			Writes: []store.Write{{Table: "tasks", ID: testTaskID, Delete: true, Expect: map[string]any{"lifecycle_version": uint64(1)}}},
		})
		assertAbsent(t, s, scope, "tasks", testTaskID)
	})
}

func TestGuardedDeleteRollsBackWhenTheUnitFails(t *testing.T) {
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "executions", testExecutionID, executionValues(1, "running"))
	record := defaultReceipt(t)
	unit := store.Unit{
		Writes:  []store.Write{{Table: "executions", ID: testExecutionID, Delete: true, ExpectVersion: 1}},
		Receipt: &record,
	}
	injected := errors.New("receipt write failed")
	s.FailNextCommit(injected)
	if _, err := s.Execute(context.Background(), scope, unit); !errors.Is(err, injected) {
		t.Fatalf("Execute error = %v; want the armed failure", err)
	}
	if _, found := s.Snapshot(scope, "executions", testExecutionID); !found {
		t.Error("a failed delete unit removed the row")
	}
	if _, found := s.Receipt(record.Key); found {
		t.Error("a failed delete unit committed its receipt")
	}
}
