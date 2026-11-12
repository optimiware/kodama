// Column-type regression tests: the memory adapter must store and return the
// same dynamic types the PostgreSQL reference decodes, refuse a value whose Go
// type does not match the declared column type, and read an unwritten column as
// SQL NULL rather than omitting it.
package memory

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/store"
)

func requireType[T any](t *testing.T, row store.Row, column string) T {
	t.Helper()
	raw, present := row.Values[column]
	if !present {
		t.Fatalf("%s row has no %q column: %v", row.Table, column, row.Values)
	}
	value, ok := raw.(T)
	if !ok {
		var zero T
		t.Fatalf("%s row %q is %T; want %T", row.Table, column, raw, zero)
	}
	return value
}

func TestReadAndSnapshotReturnContractColumnTypes(t *testing.T) {
	s := New()
	scope := projectScope()
	yaml := []byte("name: build\non: push\n")
	seedRow(t, s, scope, "pipelines", "pipeline-1", map[string]any{
		"name": "build", "yaml": yaml, "yaml_sha256": sha256Sum(yaml),
		"version": uint64(1), "created_at": fixedTime(),
	})

	row, err := s.Read(context.Background(), scope, "pipelines", "pipeline-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := requireType[int64](t, row, "version"); got != 1 {
		t.Errorf("Read version = %d; want int64 1", got)
	}
	if got := requireType[[]byte](t, row, "yaml"); string(got) != string(yaml) {
		t.Errorf("Read yaml = %q; want %q", got, yaml)
	}
	requireType[[]byte](t, row, "yaml_sha256")
	requireType[time.Time](t, row, "created_at")

	snapshot := mustSnapshot(t, s, scope, "pipelines", "pipeline-1")
	if got := requireType[int64](t, snapshot, "version"); got != 1 {
		t.Errorf("Snapshot version = %d; want int64 1", got)
	}
	requireType[[]byte](t, snapshot, "yaml")
	requireType[[]byte](t, snapshot, "yaml_sha256")

	// One and then a second versioned update must keep returning int64 and
	// advance the version exactly like the PostgreSQL bigint column.
	mustExecute(t, s, scope, store.Unit{Writes: []store.Write{
		{Table: "pipelines", ID: "pipeline-1", ExpectVersion: 1, Values: map[string]any{
			"yaml": []byte("name: release\n"), "name": "release",
		}},
	}})
	row, err = s.Read(context.Background(), scope, "pipelines", "pipeline-1")
	if err != nil {
		t.Fatalf("Read after first update: %v", err)
	}
	if got := requireType[int64](t, row, "version"); got != 2 {
		t.Errorf("version after first update = %d; want int64 2", got)
	}
	requireType[[]byte](t, row, "yaml")

	mustExecute(t, s, scope, store.Unit{Writes: []store.Write{
		{Table: "pipelines", ID: "pipeline-1", ExpectVersion: 2, Values: map[string]any{"name": "final"}},
	}})
	snapshot = mustSnapshot(t, s, scope, "pipelines", "pipeline-1")
	if got := requireType[int64](t, snapshot, "version"); got != 3 {
		t.Errorf("version after second update = %d; want int64 3", got)
	}
}

func TestIntegerAndJSONBColumnsNormalizeToInt64AndBytes(t *testing.T) {
	s := New()
	scope := projectScope()
	values := actionValues(1, "pending")
	values["index"] = int(0)
	values["timeout_ms"] = uint32(1000)
	values["staged_outputs"] = []byte(`{"staged":true}`)
	seedRow(t, s, scope, "actions", testActionID, values)

	row := mustSnapshot(t, s, scope, "actions", testActionID)
	if got := requireType[int64](t, row, "index"); got != 0 {
		t.Errorf("integer index = %d; want int64 0", got)
	}
	if got := requireType[int64](t, row, "timeout_ms"); got != 1000 {
		t.Errorf("bigint timeout_ms = %d; want int64 1000", got)
	}
	requireType[[]byte](t, row, "staged_outputs")

	mustExecute(t, s, scope, store.Unit{Writes: []store.Write{
		{Table: "actions", ID: testActionID, ExpectVersion: 1, Values: map[string]any{
			"index": 2, "timeout_ms": uint64(2000),
		}},
	}})
	row = mustSnapshot(t, s, scope, "actions", testActionID)
	if got := requireType[int64](t, row, "index"); got != 2 {
		t.Errorf("updated integer index = %d; want int64 2", got)
	}
	if got := requireType[int64](t, row, "timeout_ms"); got != 2000 {
		t.Errorf("updated bigint timeout_ms = %d; want int64 2000", got)
	}
	if got := requireType[int64](t, row, "version"); got != 2 {
		t.Errorf("updated version = %d; want int64 2", got)
	}
}

func TestUnwrittenColumnsReadAsNil(t *testing.T) {
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "executions", testExecutionID, executionValues(1, "queued"))

	meta, ok := lookupTable("executions")
	if !ok {
		t.Fatal("the declared schema has no executions table")
	}
	row := mustSnapshot(t, s, scope, "executions", testExecutionID)
	for _, name := range meta.columnOrder {
		if _, present := row.Values[name]; !present {
			t.Fatalf("Snapshot omitted schema column %q: %v", name, row.Values)
		}
	}
	for _, name := range []string{"pipeline_id", "task_id", "token_nonce", "token_expires_at", "workspace_id", "created_at"} {
		value, present := row.Values[name]
		if !present || value != nil {
			t.Errorf("unwritten column %q = %v (present=%t); want present nil", name, value, present)
		}
	}

	read, err := s.Read(context.Background(), scope, "executions", testExecutionID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	for _, name := range meta.columnOrder {
		if _, present := read.Values[name]; !present {
			t.Fatalf("Read omitted schema column %q: %v", name, read.Values)
		}
	}
	if value, present := read.Values["task_id"]; !present || value != nil {
		t.Errorf("Read unwritten column task_id = %v (present=%t); want present nil", value, present)
	}
}

func TestEventAndAuditSnapshotsReturnContractTypes(t *testing.T) {
	s := New()
	scope := projectScope()
	envelope := defaultEvent(scope, "event-1", event.TypeExecutionQueued, testExecutionID)
	envelope.SubjectVersion = 7
	envelope.Data = []byte(`{"a":1}`)
	mustExecute(t, s, scope, store.Unit{Events: []event.Envelope{envelope}})

	eventRow, found := s.Snapshot(scope, "events", "event-1")
	if !found {
		t.Fatal("the committed event is not snapshotted")
	}
	if got := requireType[int64](t, eventRow, "subject_version"); got != 7 {
		t.Errorf("event subject_version = %d; want int64 7", got)
	}
	if got := requireType[int64](t, eventRow, "publish_sequence"); got != 1 {
		t.Errorf("event publish_sequence = %d; want int64 1", got)
	}
	requireType[[]byte](t, eventRow, "data")
	requireType[time.Time](t, eventRow, "at")

	mustExecute(t, s, scope, store.Unit{Audit: []event.Audit{defaultAudit(scope, "audit-1", "task.create")}})
	auditRow, found := s.Snapshot(scope, "audit", "audit-1")
	if !found {
		t.Fatal("the committed audit row is not snapshotted")
	}
	if got := requireType[string](t, auditRow, "operation"); got != "task.create" {
		t.Errorf("audit operation = %q; want task.create", got)
	}
	requireType[time.Time](t, auditRow, "at")
}

func TestWrongTypedColumnValuesRefusedWithoutChange(t *testing.T) {
	t.Run("insert text column with an integer", func(t *testing.T) {
		s := New()
		scope := projectScope()
		values := executionValues(1, "queued")
		values["state"] = 42
		_, err := s.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{
			{Table: "executions", ID: testExecutionID, Insert: true, Values: values},
		}})
		assertFault(t, err, fault.InvalidInput)
		assertAbsent(t, s, scope, "executions", testExecutionID)
	})

	t.Run("update text column with an integer", func(t *testing.T) {
		s := New()
		scope := projectScope()
		seedRow(t, s, scope, "executions", testExecutionID, executionValues(1, "queued"))
		_, err := s.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{
			{Table: "executions", ID: testExecutionID, ExpectVersion: 1, Values: map[string]any{"state": 42}},
		}})
		assertFault(t, err, fault.InvalidInput)
		row := mustSnapshot(t, s, scope, "executions", testExecutionID)
		if got := rowString(t, row, "state"); got != "queued" {
			t.Errorf("refused update changed state to %q; want queued", got)
		}
		if got := rowVersion(t, row, "version"); got != 1 {
			t.Errorf("refused update changed version to %d; want 1", got)
		}
	})

	t.Run("insert bigint column with a string", func(t *testing.T) {
		s := New()
		scope := projectScope()
		values := executionValues(1, "queued")
		values["version"] = "1"
		_, err := s.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{
			{Table: "executions", ID: testExecutionID, Insert: true, Values: values},
		}})
		assertFault(t, err, fault.InvalidInput)
		assertAbsent(t, s, scope, "executions", testExecutionID)
	})

	t.Run("insert integer column with a string", func(t *testing.T) {
		s := New()
		scope := projectScope()
		values := actionValues(1, "pending")
		values["index"] = "0"
		_, err := s.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{
			{Table: "actions", ID: testActionID, Insert: true, Values: values},
		}})
		assertFault(t, err, fault.InvalidInput)
		assertAbsent(t, s, scope, "actions", testActionID)
	})

	t.Run("insert bytea column with a string", func(t *testing.T) {
		s := New()
		scope := projectScope()
		values := insertValues("pipelines")
		values["yaml"] = "not bytes"
		_, err := s.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{
			{Table: "pipelines", ID: "pipeline-1", Insert: true, Values: values},
		}})
		assertFault(t, err, fault.InvalidInput)
		assertAbsent(t, s, scope, "pipelines", "pipeline-1")
	})

	t.Run("insert jsonb column with a string", func(t *testing.T) {
		s := New()
		scope := projectScope()
		values := insertValues("memberships")
		values["roles"] = `["admin"]`
		_, err := s.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{
			{Table: "memberships", ID: "membership-1", Insert: true, Values: values},
		}})
		assertFault(t, err, fault.InvalidInput)
		assertAbsent(t, s, scope, "memberships", "membership-1")
	})

	t.Run("insert boolean column with a string", func(t *testing.T) {
		s := New()
		scope := projectScope()
		values := actionValues(1, "pending")
		values["staged_outputs_set"] = "false"
		_, err := s.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{
			{Table: "actions", ID: testActionID, Insert: true, Values: values},
		}})
		assertFault(t, err, fault.InvalidInput)
		assertAbsent(t, s, scope, "actions", testActionID)
	})

	t.Run("insert timestamptz column with a string", func(t *testing.T) {
		s := New()
		scope := projectScope()
		values := insertValues("task_messages")
		values["at"] = "2026-11-04T12:00:00Z"
		_, err := s.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{
			{Table: "task_messages", ID: "message-1", Insert: true, Values: values},
		}})
		assertFault(t, err, fault.InvalidInput)
		assertAbsent(t, s, scope, "task_messages", "message-1")
	})

	t.Run("update guard with the wrong type", func(t *testing.T) {
		s := New()
		scope := projectScope()
		seedRow(t, s, scope, "tasks", testTaskID, taskValues(1, "open"))
		_, err := s.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{
			{Table: "tasks", ID: testTaskID, Expect: map[string]any{"lifecycle_version": "1"}, Values: map[string]any{"state": "waiting"}},
		}})
		assertFault(t, err, fault.InvalidInput)
		row := mustSnapshot(t, s, scope, "tasks", testTaskID)
		if got := rowString(t, row, "state"); got != "open" {
			t.Errorf("refused guarded update changed state to %q; want open", got)
		}
		if got := rowVersion(t, row, "lifecycle_version"); got != 1 {
			t.Errorf("refused guarded update changed lifecycle version to %d; want 1", got)
		}
	})

	t.Run("lock guard with the wrong type", func(t *testing.T) {
		s := New()
		scope := projectScope()
		seedRow(t, s, scope, "tasks", testTaskID, taskValues(1, "open"))
		_, err := s.Execute(context.Background(), scope, store.Unit{
			Locks: []store.Lock{{Table: "tasks", IDs: []string{testTaskID}, Expect: map[string]any{"lifecycle_version": "1"}}},
		})
		assertFault(t, err, fault.InvalidInput)
		row := mustSnapshot(t, s, scope, "tasks", testTaskID)
		if got := rowString(t, row, "state"); got != "open" {
			t.Errorf("refused lock guard changed state to %q; want open", got)
		}
	})

	t.Run("text lock guard with an integer", func(t *testing.T) {
		s := New()
		scope := projectScope()
		seedRow(t, s, scope, "memberships", "membership-1", insertValues("memberships"))
		_, err := s.Execute(context.Background(), scope, store.Unit{
			Locks: []store.Lock{{Table: "memberships", IDs: []string{"membership-1"}, Expect: map[string]any{"principal_id": 42}}},
		})
		assertFault(t, err, fault.InvalidInput)
	})
}
