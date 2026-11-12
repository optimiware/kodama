// Identity-uniqueness regression tests: a duplicate receipt, event or audit id
// must roll the whole unit back exactly like the PostgreSQL primary keys, and a
// business Write may never target a publication table.
package memory

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/store"
)

func TestDuplicateCommittedEventIDRefusedWithoutEffect(t *testing.T) {
	s := New()
	scope := projectScope()
	mustExecute(t, s, scope, store.Unit{
		Writes: []store.Write{{Table: "executions", ID: testExecutionID, Insert: true, Values: executionValues(1, "queued")}},
		Events: []event.Envelope{defaultEvent(scope, "event-1", event.TypeExecutionQueued, testExecutionID)},
	})

	_, err := s.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{Table: "executions", ID: "execution-2", Insert: true, Values: executionValues(1, "queued")}},
		Events: []event.Envelope{defaultEvent(scope, "event-1", event.TypeExecutionQueued, testExecutionID)},
	})
	assertFault(t, err, fault.VersionConflict)
	assertAbsent(t, s, scope, "executions", "execution-2")
	if got := len(s.Events(scope)); got != 1 {
		t.Errorf("duplicate event id left %d committed events; want 1", got)
	}

	// A refused duplicate must not consume a publication sequence.
	mustExecute(t, s, scope, store.Unit{
		Events: []event.Envelope{defaultEvent(scope, "event-2", event.TypeExecutionQueued, testExecutionID)},
	})
	events := s.Events(scope)
	if len(events) != 2 {
		t.Fatalf("committed events = %d; want 2", len(events))
	}
	if events[1].Sequence != 2 {
		t.Errorf("event after a refused duplicate has sequence %d; want 2 (no consumed sequence)", events[1].Sequence)
	}
}

func TestDuplicateCommittedAuditIDRefusedWithoutEffect(t *testing.T) {
	s := New()
	scope := projectScope()
	mustExecute(t, s, scope, store.Unit{
		Audit: []event.Audit{defaultAudit(scope, "audit-1", "task.create")},
	})

	_, err := s.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{Table: "executions", ID: testExecutionID, Insert: true, Values: executionValues(1, "queued")}},
		Audit:  []event.Audit{defaultAudit(scope, "audit-1", "task.reply")},
	})
	assertFault(t, err, fault.VersionConflict)
	assertAbsent(t, s, scope, "executions", testExecutionID)
	if got := len(s.Audit(scope)); got != 1 {
		t.Errorf("duplicate audit id left %d committed audit rows; want 1", got)
	}
}

func TestDuplicateIDWithinOneUnitRefusedWithoutEffect(t *testing.T) {
	t.Run("events", func(t *testing.T) {
		s := New()
		scope := projectScope()
		duplicate := defaultEvent(scope, "event-1", event.TypeExecutionQueued, testExecutionID)
		_, err := s.Execute(context.Background(), scope, store.Unit{
			Writes: []store.Write{{Table: "executions", ID: testExecutionID, Insert: true, Values: executionValues(1, "queued")}},
			Events: []event.Envelope{duplicate, duplicate},
		})
		assertFault(t, err, fault.VersionConflict)
		assertAbsent(t, s, scope, "executions", testExecutionID)
		if got := len(s.Events(scope)); got != 0 {
			t.Errorf("duplicate event id published %d events; want 0", got)
		}
	})

	t.Run("audits", func(t *testing.T) {
		s := New()
		scope := projectScope()
		duplicate := defaultAudit(scope, "audit-1", "task.create")
		_, err := s.Execute(context.Background(), scope, store.Unit{
			Writes: []store.Write{{Table: "executions", ID: testExecutionID, Insert: true, Values: executionValues(1, "queued")}},
			Audit:  []event.Audit{duplicate, duplicate},
		})
		assertFault(t, err, fault.VersionConflict)
		assertAbsent(t, s, scope, "executions", testExecutionID)
		if got := len(s.Audit(scope)); got != 0 {
			t.Errorf("duplicate audit id wrote %d audit rows; want 0", got)
		}
	})
}

func TestDuplicateReceiptIDUnderAnotherKeyRefusedWithoutEffect(t *testing.T) {
	s := New()
	scope := projectScope()
	first := receiptRecord(t, "receipt-shared",
		keyFor(testTenantID, testProjectID, testPrincipalID, "task.create", "caller-1"),
		`{"a":1}`, `{"result":"first"}`)
	mustExecute(t, s, scope, store.Unit{Receipt: &first})

	second := receiptRecord(t, "receipt-shared",
		keyFor(testTenantID, testProjectID, testPrincipalID, "task.create", "caller-2"),
		`{"a":2}`, `{"result":"second"}`)
	_, err := s.Execute(context.Background(), scope, store.Unit{
		Writes:  []store.Write{{Table: "executions", ID: testExecutionID, Insert: true, Values: executionValues(1, "queued")}},
		Receipt: &second,
	})
	assertFault(t, err, fault.VersionConflict)
	assertAbsent(t, s, scope, "executions", testExecutionID)
	if _, found := s.Receipt(second.Key); found {
		t.Error("a duplicate receipt id under a different key was committed")
	}
	stored, found := s.Receipt(first.Key)
	if !found {
		t.Fatal("the original receipt disappeared")
	}
	if string(stored.Result) != `{"result":"first"}` {
		t.Errorf("original receipt result = %q; want unchanged", stored.Result)
	}
}

func TestBusinessWriteToPublicationTablesRefused(t *testing.T) {
	cases := []struct {
		name  string
		write store.Write
	}{
		{
			name: "receipts insert",
			write: store.Write{Table: "receipts", ID: "row-1", Insert: true, Values: map[string]any{
				"principal_id": testPrincipalID,
			}},
		},
		{
			name: "events insert",
			write: store.Write{Table: "events", ID: "row-1", Insert: true, Values: map[string]any{
				"type": "execution.queued",
			}},
		},
		{
			name: "events update",
			write: store.Write{Table: "events", ID: "row-1", ExpectVersion: 1, Values: map[string]any{
				"type": "execution.queued",
			}},
		},
		{
			name: "audit insert",
			write: store.Write{Table: "audit", ID: "row-1", Insert: true, Values: map[string]any{
				"actor": testPrincipalID,
			}},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			s := New()
			scope := projectScope()
			_, err := s.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{testCase.write}})
			assertFault(t, err, fault.InvalidInput)
			if row, found := s.Snapshot(scope, testCase.write.Table, testCase.write.ID); found {
				t.Errorf("a refused publication write left a row: %v", row.Values)
			}
			if got := len(s.Events(scope)); got != 0 {
				t.Errorf("a refused publication write published %d events; want 0", got)
			}
			if got := len(s.Audit(scope)); got != 0 {
				t.Errorf("a refused publication write wrote %d audit rows; want 0", got)
			}
		})
	}
}
