// Atomic commit-unit behavior for the memory adapter: one shared committed
// state, whole-unit commit and rollback, receipt identity, multi-row
// reply-and-admit / ask-and-cancel units, and committed-only event sequencing.
package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func TestExecuteCommitsWholeUnitAtomically(t *testing.T) {
	s := New()
	scope, unit := replyAndAdmitUnit(t)
	seedRow(t, s, scope, "tasks", testTaskID, taskValues(1, "open"))

	result := mustExecute(t, s, scope, unit)
	if result.Replayed {
		t.Fatal("a fresh commit unit was reported as a replay")
	}
	if result.Receipt.Key.CallerKey != "reply-key" {
		t.Errorf("committed receipt caller key = %q; want reply-key", result.Receipt.Key.CallerKey)
	}

	task := mustSnapshot(t, s, scope, "tasks", testTaskID)
	if got := rowString(t, task, "state"); got != "open" {
		t.Errorf("task state = %q; want open", got)
	}
	if got := rowVersion(t, task, "lifecycle_version"); got != 2 {
		t.Errorf("task lifecycle version = %d; want 2", got)
	}
	if got := rowString(t, task, "current_execution_id"); got != testExecutionID {
		t.Errorf("task current execution = %q; want %q", got, testExecutionID)
	}
	if row, found := s.Snapshot(scope, "task_messages", "message-1"); !found {
		t.Error("the reply message was not committed with the unit")
	} else if got := rowString(t, row, "body"); got != "answer" {
		t.Errorf("message body = %q; want answer", got)
	}
	execution := mustSnapshot(t, s, scope, "executions", testExecutionID)
	if got := rowString(t, execution, "state"); got != "queued" {
		t.Errorf("execution state = %q; want queued", got)
	}
	if got := rowVersion(t, execution, "version"); got != 1 {
		t.Errorf("execution version = %d; want the inserted initial version 1", got)
	}
	action := mustSnapshot(t, s, scope, "actions", testActionID)
	if got := rowString(t, action, "state"); got != "pending" {
		t.Errorf("action state = %q; want pending", got)
	}

	record, found := s.Receipt(unit.Receipt.Key)
	if !found {
		t.Fatal("the committed receipt is not readable")
	}
	if string(record.Result) != string(unit.Receipt.Result) {
		t.Errorf("receipt result = %q; want %q", record.Result, unit.Receipt.Result)
	}

	events := s.Events(scope)
	if got, want := eventIDs(events), []string{"event-reply", "event-queued"}; !equalStrings(got, want) {
		t.Fatalf("committed event ids = %v; want %v", got, want)
	}
	if events[0].Sequence != 1 || events[1].Sequence != 2 {
		t.Errorf("event sequences = %d,%d; want 1,2", events[0].Sequence, events[1].Sequence)
	}
	if got, want := auditOperations(s.Audit(scope)), []string{"task.reply"}; !equalStrings(got, want) {
		t.Errorf("committed audit operations = %v; want %v", got, want)
	}
}

func TestExecuteFailureLeavesNoPartialVisibility(t *testing.T) {
	s := New()
	scope, unit := replyAndAdmitUnit(t)
	seedRow(t, s, scope, "tasks", testTaskID, taskValues(1, "open"))

	injected := errors.New("publish failed")
	s.FailNextCommit(injected)
	if _, err := s.Execute(context.Background(), scope, unit); !errors.Is(err, injected) {
		t.Fatalf("Execute error = %v; want the armed failure", err)
	}

	task := mustSnapshot(t, s, scope, "tasks", testTaskID)
	if got := rowString(t, task, "state"); got != "open" {
		t.Errorf("task state after rollback = %q; want open", got)
	}
	if got := rowVersion(t, task, "lifecycle_version"); got != 1 {
		t.Errorf("task lifecycle version after rollback = %d; want 1", got)
	}
	if raw, present := task.Values["current_execution_id"]; !present || raw != nil {
		t.Errorf("task current execution after rollback = %v (present=%t); want present nil", raw, present)
	}
	assertAbsent(t, s, scope, "task_messages", "message-1")
	assertAbsent(t, s, scope, "executions", testExecutionID)
	assertAbsent(t, s, scope, "actions", testActionID)
	if _, found := s.Receipt(unit.Receipt.Key); found {
		t.Error("a rolled-back unit left its receipt visible")
	}
	if events := s.Events(scope); len(events) != 0 {
		t.Errorf("a rolled-back unit left %d events visible", len(events))
	}
	if audits := s.Audit(scope); len(audits) != 0 {
		t.Errorf("a rolled-back unit left %d audit rows visible", len(audits))
	}

	// The failure hook is one-shot: the same unit commits once it is disarmed.
	retry := mustExecute(t, s, scope, unit)
	if retry.Replayed {
		t.Fatal("the retry after a crash before the effect was reported as a replay")
	}
	if row, found := s.Snapshot(scope, "executions", testExecutionID); !found {
		t.Error("the retry did not commit the execution")
	} else if got := rowString(t, row, "state"); got != "queued" {
		t.Errorf("retried execution state = %q; want queued", got)
	}
}

func TestExecuteStaleVersionRefusedWithoutPartialCommit(t *testing.T) {
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "executions", testExecutionID, executionValues(1, "queued"))
	record := defaultReceipt(t)
	unit := store.Unit{
		Writes: []store.Write{
			{Table: "executions", ID: testExecutionID, ExpectVersion: 7, Values: map[string]any{"state": "running"}},
			{Table: "actions", ID: testActionID, Insert: true, Values: insertValues("actions")},
		},
		Receipt: &record,
		Events:  []event.Envelope{defaultEvent(scope, "event-1", event.TypeExecutionStarted, testExecutionID)},
	}

	_, err := s.Execute(context.Background(), scope, unit)
	assertFault(t, err, fault.VersionConflict)

	execution := mustSnapshot(t, s, scope, "executions", testExecutionID)
	if got := rowString(t, execution, "state"); got != "queued" {
		t.Errorf("execution state = %q; want queued", got)
	}
	if got := rowVersion(t, execution, "version"); got != 1 {
		t.Errorf("execution version = %d; want 1", got)
	}
	assertAbsent(t, s, scope, "actions", testActionID)
	if _, found := s.Receipt(record.Key); found {
		t.Error("a stale-version unit inserted its receipt")
	}
	if events := s.Events(scope); len(events) != 0 {
		t.Errorf("a stale-version unit published %d events", len(events))
	}
}

func TestExecuteEqualReceiptReplaysOriginalResult(t *testing.T) {
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "executions", testExecutionID, executionValues(1, "queued"))
	record := defaultReceipt(t)
	unit := store.Unit{
		Writes:  []store.Write{{Table: "executions", ID: testExecutionID, ExpectVersion: 1, Values: map[string]any{"state": "running"}}},
		Receipt: &record,
		Audit:   []event.Audit{defaultAudit(scope, "audit-1", "execution.start")},
		Events:  []event.Envelope{defaultEvent(scope, "event-1", event.TypeExecutionStarted, testExecutionID)},
	}

	first := mustExecute(t, s, scope, unit)
	if first.Replayed {
		t.Fatal("the first commit was reported as a replay")
	}
	// The retry carries the now-stale expected version; the receipt must be
	// consulted before locks and writes so the retry replays instead.
	second := mustExecute(t, s, scope, unit)
	if !second.Replayed {
		t.Fatal("equal receipt content did not replay the committed result")
	}
	if string(second.Receipt.Result) != string(first.Receipt.Result) {
		t.Errorf("replayed result = %q; want the original %q", second.Receipt.Result, first.Receipt.Result)
	}
	if second.Receipt.ExecutionID != first.Receipt.ExecutionID {
		t.Errorf("replayed execution = %q; want the original %q", second.Receipt.ExecutionID, first.Receipt.ExecutionID)
	}

	execution := mustSnapshot(t, s, scope, "executions", testExecutionID)
	if got := rowVersion(t, execution, "version"); got != 2 {
		t.Errorf("execution version = %d; want exactly one applied update (2)", got)
	}
	if events := s.Events(scope); len(events) != 1 {
		t.Errorf("a replay published %d events; want 1", len(events))
	}
	if audits := s.Audit(scope); len(audits) != 1 {
		t.Errorf("a replay wrote %d audit rows; want 1", len(audits))
	}
}

// TestIncompleteReceiptPersistsInProgressResultAndReplaysWithoutApplying proves
// the receipt completion rule: an incomplete insert MAY carry the in-progress
// selection, an equal-content replay returns that stored payload and applies
// nothing, and the receipt still has a NULL completion marker.
func TestIncompleteReceiptPersistsInProgressResultAndReplaysWithoutApplying(t *testing.T) {
	s := New()
	scope := projectScope()
	key := keyFor(testTenantID, testProjectID, testServiceID, "pipeline.push", "event-1")
	record := receiptRecord(t, "receipt-selection", key, `{"event":"event-1"}`, "selection-v1")
	unit := store.Unit{
		Writes: []store.Write{{
			Table: "executions", ID: testExecutionID, Insert: true,
			Values: executionValues(1, "queued"),
		}},
		Receipt:     &record,
		ReceiptMode: store.ReceiptInsertIncomplete,
		Audit:       []event.Audit{defaultAudit(scope, "audit-1", "execution.select")},
		Events:      []event.Envelope{defaultEvent(scope, "event-1", event.TypeExecutionStarted, testExecutionID)},
	}
	first := mustExecute(t, s, scope, unit)
	if first.Replayed {
		t.Fatal("the first incomplete insert was reported as a replay")
	}
	if got := string(first.Receipt.Result); got != "selection-v1" {
		t.Fatalf("incomplete insert result = %q; want selection-v1", got)
	}
	stored, found := s.Receipt(key)
	if !found {
		t.Fatal("the incomplete receipt is missing")
	}
	if got := string(stored.Result); got != "selection-v1" {
		t.Fatalf("stored incomplete result = %q; want selection-v1", got)
	}
	row := receiptScanRow(t, s, scope, string(record.ID))
	if row["completed_at"] != nil {
		t.Fatalf("incomplete receipt completed_at = %v; want NULL", row["completed_at"])
	}
	if got, _ := row["result"].([]byte); string(got) != "selection-v1" {
		t.Fatalf("incomplete receipt row result = %q; want selection-v1", got)
	}

	// The replay carries a different in-progress payload and a write that would
	// change the execution. Equal content must replay the stored payload and
	// apply no write, audit row or event.
	replayRecord := record
	replayRecord.Result = []byte("selection-v2")
	replay := unit
	replay.Receipt = &replayRecord
	replay.Writes = []store.Write{{
		Table: "executions", ID: testExecutionID, ExpectVersion: 1,
		Values: map[string]any{"state": "running"},
	}}
	second := mustExecute(t, s, scope, replay)
	if !second.Replayed {
		t.Fatal("an equal-content incomplete replay was not reported as a replay")
	}
	if got := string(second.Receipt.Result); got != "selection-v1" {
		t.Fatalf("replayed incomplete result = %q; want the stored selection-v1", got)
	}
	execution := mustSnapshot(t, s, scope, "executions", testExecutionID)
	if got := rowString(t, execution, "state"); got != "queued" {
		t.Errorf("execution state after replay = %q; want queued", got)
	}
	if got := rowVersion(t, execution, "version"); got != 1 {
		t.Errorf("execution version after replay = %d; want 1", got)
	}
	if got := len(s.Audit(scope)); got != 1 {
		t.Errorf("audit rows after replay = %d; want 1", got)
	}
	if got := len(s.Events(scope)); got != 1 {
		t.Errorf("events after replay = %d; want 1", got)
	}
}

// TestReceiptCompletionOverwritesInProgressResultExactlyOnce proves completion
// compare-and-sets on the NULL marker and content hash, replaces the in-progress
// selection with the final result, and replays the first result afterwards.
func TestReceiptCompletionOverwritesInProgressResultExactlyOnce(t *testing.T) {
	s := New()
	scope := projectScope()
	key := keyFor(testTenantID, testProjectID, testServiceID, "pipeline.push", "event-1")
	incomplete := receiptRecord(t, "receipt-selection", key, `{"event":"event-1"}`, "selection-v1")
	mustExecute(t, s, scope, store.Unit{Receipt: &incomplete, ReceiptMode: store.ReceiptInsertIncomplete})

	completed := incomplete
	completed.Result = []byte("started")
	completion := store.Unit{
		Receipt:       &completed,
		ReceiptMode:   store.ReceiptComplete,
		ReceiptExpect: &store.ReceiptExpect{ContentHash: incomplete.ContentHash},
	}
	first := mustExecute(t, s, scope, completion)
	if first.Replayed {
		t.Fatal("the first completion was reported as a replay")
	}
	if got := string(first.Receipt.Result); got != "started" {
		t.Fatalf("completion result = %q; want started", got)
	}
	row := receiptScanRow(t, s, scope, string(incomplete.ID))
	if row["completed_at"] == nil {
		t.Fatal("completed receipt completed_at = NULL; want a completion marker")
	}
	if got, _ := row["result"].([]byte); string(got) != "started" {
		t.Fatalf("completed receipt row result = %q; want started", got)
	}

	second := mustExecute(t, s, scope, completion)
	if !second.Replayed {
		t.Fatal("a second completion of the same receipt was not reported as a replay")
	}
	if got := string(second.Receipt.Result); got != "started" {
		t.Fatalf("replayed completion result = %q; want the first started", got)
	}
	row = receiptScanRow(t, s, scope, string(incomplete.ID))
	if got, _ := row["result"].([]byte); string(got) != "started" {
		t.Fatalf("stored result after replay = %q; want started", got)
	}
}

// TestReceiptModeShapeEnforcesUnitValidate proves the adapter calls the shared
// Unit.Validate: a completed insert without a result is refused before any
// effect, while an incomplete insert without a result stays valid.
func TestReceiptModeShapeEnforcesUnitValidate(t *testing.T) {
	s := New()
	scope := projectScope()
	missing := receiptRecord(t, "receipt-missing-result",
		keyFor(testTenantID, testProjectID, testServiceID, "pipeline.push", "event-1"),
		`{"event":"event-1"}`, "")
	missing.Result = nil
	_, err := s.Execute(context.Background(), scope, store.Unit{Receipt: &missing})
	assertFault(t, err, fault.InvalidInput)
	if _, found := s.Receipt(missing.Key); found {
		t.Fatal("a completed insert without a result was committed")
	}

	incomplete := receiptRecord(t, "receipt-incomplete-nil",
		keyFor(testTenantID, testProjectID, testServiceID, "pipeline.push", "event-2"),
		`{"event":"event-2"}`, "")
	incomplete.Result = nil
	mustExecute(t, s, scope, store.Unit{Receipt: &incomplete, ReceiptMode: store.ReceiptInsertIncomplete})
	stored, found := s.Receipt(incomplete.Key)
	if !found {
		t.Fatal("an incomplete insert without a result was refused")
	}
	if len(stored.Result) != 0 {
		t.Fatalf("incomplete receipt result = %q; want empty", stored.Result)
	}
}

// receiptScanRow reads one committed receipt through the scan seam, the only
// reader that materializes the receipts table's durable completion marker.
func receiptScanRow(t *testing.T, s *Store, scope tenancy.Scope, id string) map[string]any {
	t.Helper()
	page := queryScanner(t, s, scope, store.ScanQuery{
		Table: "receipts", Equals: map[string]any{"id": id}, Limit: 10,
	})
	if len(page.Rows) != 1 {
		t.Fatalf("receipt rows for %q = %d; want 1", id, len(page.Rows))
	}
	return page.Rows[0].Values
}

func TestExecuteRefusesChangedReceiptContentBeforeAnyWrite(t *testing.T) {
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "executions", testExecutionID, executionValues(1, "queued"))
	record := defaultReceipt(t)
	first := store.Unit{
		Writes:  []store.Write{{Table: "executions", ID: testExecutionID, ExpectVersion: 1, Values: map[string]any{"state": "running"}}},
		Receipt: &record,
		Events:  []event.Envelope{defaultEvent(scope, "event-1", event.TypeExecutionStarted, testExecutionID)},
	}
	mustExecute(t, s, scope, first)

	changed := defaultReceipt(t)
	changed.ContentHash = fingerprintOf(t, `{"task":"task-1","different":true}`)
	second := store.Unit{
		Writes:  []store.Write{{Table: "executions", ID: testExecutionID, ExpectVersion: 2, Values: map[string]any{"state": "succeeded"}}},
		Receipt: &changed,
		Events:  []event.Envelope{defaultEvent(scope, "event-2", event.TypeExecutionSucceeded, testExecutionID)},
	}
	_, err := s.Execute(context.Background(), scope, second)
	assertFault(t, err, fault.ChangedIdempotencyContent)

	execution := mustSnapshot(t, s, scope, "executions", testExecutionID)
	if got := rowString(t, execution, "state"); got != "running" {
		t.Errorf("execution state after changed-content refusal = %q; want running", got)
	}
	if got := rowVersion(t, execution, "version"); got != 2 {
		t.Errorf("execution version after changed-content refusal = %d; want 2", got)
	}
	if events := s.Events(scope); len(events) != 1 {
		t.Errorf("changed-content refusal published %d events; want 1", len(events))
	}
	stored, found := s.Receipt(record.Key)
	if !found {
		t.Fatal("the original receipt disappeared")
	}
	if string(stored.Result) != string(record.Result) {
		t.Errorf("stored receipt result = %q; want the original %q", stored.Result, record.Result)
	}
}

func TestTenantScopedReceiptIdentityReplays(t *testing.T) {
	s := New()
	scope := tenantScope()
	record := receiptRecord(t, "receipt-tenant",
		keyFor(testTenantID, "", testPrincipalID, "tenant.rotate", "caller-tenant"),
		`{"tenant":"tenant-1"}`,
		`{"rotated":true}`)
	unit := store.Unit{Receipt: &record}

	first := mustExecute(t, s, scope, unit)
	if first.Replayed {
		t.Fatal("the first tenant-scoped receipt was reported as a replay")
	}
	second := mustExecute(t, s, scope, unit)
	if !second.Replayed {
		t.Fatal("a tenant-scoped receipt with equal content did not replay")
	}
	if string(second.Receipt.Result) != `{"rotated":true}` {
		t.Errorf("tenant-scoped replay result = %q", second.Receipt.Result)
	}
	if _, found := s.Receipt(record.Key); !found {
		t.Error("the tenant-scoped receipt is not readable by identity")
	}
}

func TestExecuteRefusesInvalidUnitsBeforeAnyEffect(t *testing.T) {
	badEvent := defaultEvent(projectScope(), "event-1", event.TypeTaskCreated, testTaskID)
	badEvent.Type = "bogus"
	badHash := defaultReceipt(t)
	badHash.ContentHash = "not-a-hash"
	foreignReceipt := defaultReceipt(t)
	foreignReceipt.Key.TenantID = otherTenantID

	cases := []struct {
		name  string
		scope tenancy.Scope
		unit  store.Unit
		want  fault.Class
	}{
		{"unknown write table", projectScope(), store.Unit{Writes: []store.Write{{Table: "not_a_table", ID: "id-1", Insert: true}}}, fault.InvalidInput},
		{"unknown lock table", projectScope(), store.Unit{Locks: []store.Lock{{Table: "not_a_table", IDs: []string{"id-1"}}}}, fault.InvalidInput},
		{"invalid event", projectScope(), store.Unit{Events: []event.Envelope{badEvent}}, fault.InvalidInput},
		{"cross-scope event", projectScope(), store.Unit{Events: []event.Envelope{defaultEvent(scopeFor(otherTenantID, testProjectID), "event-1", event.TypeTaskCreated, testTaskID)}}, fault.Forbidden},
		{"cross-scope audit", projectScope(), store.Unit{Audit: []event.Audit{defaultAudit(scopeFor(otherTenantID, testProjectID), "audit-1", "x")}}, fault.Forbidden},
		{"malformed receipt hash", projectScope(), store.Unit{Receipt: &badHash}, fault.InvalidInput},
		{"receipt identity scope mismatch", projectScope(), store.Unit{Receipt: &foreignReceipt}, fault.Forbidden},
		{"reserved column", projectScope(), store.Unit{Writes: []store.Write{{Table: "executions", ID: "exec-2", Insert: true, Values: map[string]any{"tenant_id": "tenant-2"}}}}, fault.InvalidInput},
		{"project table in tenant scope", tenantScope(), store.Unit{Writes: []store.Write{{Table: "executions", ID: "exec-2", Insert: true, Values: insertValues("executions")}}}, fault.InvalidInput},
		{"cross-tenant root write", tenantScope(), store.Unit{Writes: []store.Write{{Table: "tenants", ID: "tenant-2", Insert: true, Values: insertValues("tenants")}}}, fault.Forbidden},
		{"cross-project root lock", projectScope(), store.Unit{Locks: []store.Lock{{Table: "projects", IDs: []string{"project-2"}}}}, fault.Forbidden},
		{"tenant-wide guard on project-required table", projectScope(), store.Unit{Locks: []store.Lock{{Table: "executions", IDs: []string{testExecutionID}, TenantWide: true}}}, fault.InvalidInput},
		{"guarded lock names two rows", projectScope(), store.Unit{Locks: []store.Lock{{Table: "executions", IDs: []string{testExecutionID, "execution-2"}, ExpectVersion: 1}}}, fault.InvalidInput},
		{"version guard on versionless table", projectScope(), store.Unit{Locks: []store.Lock{{Table: "tasks", IDs: []string{testTaskID}, ExpectVersion: 1}}}, fault.InvalidInput},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			s := New()
			_, err := s.Execute(context.Background(), testCase.scope, testCase.unit)
			assertFault(t, err, testCase.want)
			if events := s.Events(testCase.scope); len(events) != 0 {
				t.Errorf("a refused unit published %d events", len(events))
			}
			if audits := s.Audit(testCase.scope); len(audits) != 0 {
				t.Errorf("a refused unit wrote %d audit rows", len(audits))
			}
		})
	}
}

func TestExecuteValidatesTheWholeUnitBeforeAnyEffect(t *testing.T) {
	s := New()
	scope := projectScope()
	unit := store.Unit{Writes: []store.Write{
		{Table: "executions", ID: testExecutionID, Insert: true, Values: insertValues("executions")},
		{Table: "not_a_table", ID: "id-1", Insert: true},
	}}
	_, err := s.Execute(context.Background(), scope, unit)
	assertFault(t, err, fault.InvalidInput)
	assertAbsent(t, s, scope, "executions", testExecutionID)
}

func TestExecuteCanceledContextLeavesStoreUntouched(t *testing.T) {
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "executions", testExecutionID, executionValues(1, "queued"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.Execute(ctx, scope, store.Unit{
		Writes: []store.Write{{Table: "executions", ID: testExecutionID, ExpectVersion: 1, Values: map[string]any{"state": "running"}}},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute with a canceled context = %v; want context.Canceled", err)
	}
	execution := mustSnapshot(t, s, scope, "executions", testExecutionID)
	if got := rowString(t, execution, "state"); got != "queued" {
		t.Errorf("execution state = %q; want queued", got)
	}
}

func TestReplyAndAdmitUnitCommitsOrVanishes(t *testing.T) {
	t.Run("commit", func(t *testing.T) {
		s := New()
		scope, unit := replyAndAdmitUnit(t)
		seedRow(t, s, scope, "tasks", testTaskID, taskValues(1, "open"))
		mustExecute(t, s, scope, unit)

		task := mustSnapshot(t, s, scope, "tasks", testTaskID)
		if got := rowVersion(t, task, "lifecycle_version"); got != 2 {
			t.Errorf("task lifecycle version = %d; want 2", got)
		}
		if _, found := s.Snapshot(scope, "executions", testExecutionID); !found {
			t.Error("the admitted execution is missing")
		}
		if _, found := s.Snapshot(scope, "actions", testActionID); !found {
			t.Error("the admitted action is missing")
		}
		if _, found := s.Snapshot(scope, "task_messages", "message-1"); !found {
			t.Error("the reply message is missing")
		}
		if events := s.Events(scope); len(events) != 2 {
			t.Errorf("committed events = %d; want 2", len(events))
		}
	})

	t.Run("failure", func(t *testing.T) {
		s := New()
		scope, unit := replyAndAdmitUnit(t)
		seedRow(t, s, scope, "tasks", testTaskID, taskValues(1, "open"))
		s.FailNextCommit(errors.New("publish failed"))
		if _, err := s.Execute(context.Background(), scope, unit); err == nil {
			t.Fatal("the armed failure did not fail the reply-and-admit unit")
		}

		task := mustSnapshot(t, s, scope, "tasks", testTaskID)
		if got := rowVersion(t, task, "lifecycle_version"); got != 1 {
			t.Errorf("task lifecycle version = %d; want 1", got)
		}
		if raw, present := task.Values["current_execution_id"]; !present || raw != nil {
			t.Errorf("task current execution = %v (present=%t); want present nil", raw, present)
		}
		assertAbsent(t, s, scope, "executions", testExecutionID)
		assertAbsent(t, s, scope, "actions", testActionID)
		assertAbsent(t, s, scope, "task_messages", "message-1")
		if events := s.Events(scope); len(events) != 0 {
			t.Errorf("a failed reply-and-admit unit left %d events", len(events))
		}
	})
}

func TestAskAndCancelUnitCommitsOrVanishes(t *testing.T) {
	seed := func(t *testing.T, s *Store, scope tenancy.Scope) {
		t.Helper()
		task := taskValues(1, "open")
		task["current_execution_id"] = testExecutionID
		seedRow(t, s, scope, "tasks", testTaskID, task)
		execution := executionValues(1, "running")
		execution["task_id"] = testTaskID
		seedRow(t, s, scope, "executions", testExecutionID, execution)
		seedRow(t, s, scope, "actions", testActionID, actionValues(1, "running"))
	}

	t.Run("commit", func(t *testing.T) {
		s := New()
		scope, unit := askAndCancelUnit(t)
		seed(t, s, scope)
		mustExecute(t, s, scope, unit)

		task := mustSnapshot(t, s, scope, "tasks", testTaskID)
		if got := rowString(t, task, "state"); got != "cancelled" {
			t.Errorf("task state = %q; want cancelled", got)
		}
		if got := rowVersion(t, task, "lifecycle_version"); got != 2 {
			t.Errorf("task lifecycle version = %d; want 2", got)
		}
		execution := mustSnapshot(t, s, scope, "executions", testExecutionID)
		if got := rowString(t, execution, "state"); got != "cancelled" {
			t.Errorf("execution state = %q; want cancelled", got)
		}
		if got := rowVersion(t, execution, "version"); got != 2 {
			t.Errorf("execution version = %d; want 2", got)
		}
		action := mustSnapshot(t, s, scope, "actions", testActionID)
		if got := rowString(t, action, "state"); got != "cancelled" {
			t.Errorf("action state = %q; want cancelled", got)
		}
		if got := rowVersion(t, action, "version"); got != 2 {
			t.Errorf("action version = %d; want 2", got)
		}
		if _, found := s.Snapshot(scope, "task_messages", "message-cancel"); !found {
			t.Error("the cancellation message is missing")
		}
		if events := s.Events(scope); len(events) != 2 {
			t.Errorf("committed events = %d; want 2", len(events))
		}
	})

	t.Run("failure", func(t *testing.T) {
		s := New()
		scope, unit := askAndCancelUnit(t)
		seed(t, s, scope)
		s.FailNextCommit(errors.New("publish failed"))
		if _, err := s.Execute(context.Background(), scope, unit); err == nil {
			t.Fatal("the armed failure did not fail the ask-and-cancel unit")
		}

		task := mustSnapshot(t, s, scope, "tasks", testTaskID)
		if got := rowString(t, task, "state"); got != "open" {
			t.Errorf("task state = %q; want open", got)
		}
		execution := mustSnapshot(t, s, scope, "executions", testExecutionID)
		if got := rowString(t, execution, "state"); got != "running" {
			t.Errorf("execution state = %q; want running", got)
		}
		action := mustSnapshot(t, s, scope, "actions", testActionID)
		if got := rowString(t, action, "state"); got != "running" {
			t.Errorf("action state = %q; want running", got)
		}
		assertAbsent(t, s, scope, "task_messages", "message-cancel")
		if events := s.Events(scope); len(events) != 0 {
			t.Errorf("a failed ask-and-cancel unit left %d events", len(events))
		}
	})
}

func TestEventSequenceIsMonotonicAcrossCommittedUnitsOnly(t *testing.T) {
	s := New()
	scope := projectScope()
	mustExecute(t, s, scope, store.Unit{Events: []event.Envelope{
		defaultEvent(scope, "event-a1", event.TypeTaskCreated, testTaskID),
		defaultEvent(scope, "event-a2", event.TypeTaskQuestion, testTaskID),
	}})

	injected := errors.New("publish failed")
	s.FailNextCommit(injected)
	if _, err := s.Execute(context.Background(), scope, store.Unit{Events: []event.Envelope{
		defaultEvent(scope, "event-b", event.TypeTaskReply, testTaskID),
	}}); !errors.Is(err, injected) {
		t.Fatalf("failed unit error = %v; want the armed failure", err)
	}

	mustExecute(t, s, scope, store.Unit{Events: []event.Envelope{
		defaultEvent(scope, "event-c", event.TypeTaskCancelled, testTaskID),
	}})

	events := s.Events(scope)
	if got, want := eventIDs(events), []string{"event-a1", "event-a2", "event-c"}; !equalStrings(got, want) {
		t.Fatalf("committed event order = %v; want %v", got, want)
	}
	for index, committed := range events {
		if want := uint64(index + 1); committed.Sequence != want {
			t.Fatalf("event %q sequence = %d; want %d (a failed unit must not consume a sequence)", committed.Envelope.ID, committed.Sequence, want)
		}
	}

	other := otherProjectScope()
	mustExecute(t, s, other, store.Unit{Events: []event.Envelope{
		defaultEvent(other, "event-other", event.TypeTaskCreated, testTaskID),
	}})
	if got := len(s.Events(scope)); got != 3 {
		t.Errorf("project scope sees %d events; want 3", got)
	}
	otherEvents := s.Events(other)
	if len(otherEvents) != 1 {
		t.Fatalf("other project sees %d events; want 1", len(otherEvents))
	}
	if otherEvents[0].Sequence != 4 {
		t.Errorf("other project event sequence = %d; want the next global sequence 4", otherEvents[0].Sequence)
	}
}

func TestCrashBeforeEffectRetryApplies(t *testing.T) {
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "executions", testExecutionID, executionValues(1, "queued"))
	record := defaultReceipt(t)
	unit := store.Unit{
		Writes:  []store.Write{{Table: "executions", ID: testExecutionID, ExpectVersion: 1, Values: map[string]any{"state": "running"}}},
		Receipt: &record,
		Events:  []event.Envelope{defaultEvent(scope, "event-1", event.TypeExecutionStarted, testExecutionID)},
	}

	injected := errors.New("crash before publish")
	s.FailNextCommit(injected)
	if _, err := s.Execute(context.Background(), scope, unit); !errors.Is(err, injected) {
		t.Fatalf("Execute error = %v; want the armed failure", err)
	}
	execution := mustSnapshot(t, s, scope, "executions", testExecutionID)
	if got := rowString(t, execution, "state"); got != "queued" {
		t.Errorf("execution state after crash = %q; want queued", got)
	}
	if _, found := s.Receipt(record.Key); found {
		t.Error("a crash before the effect left a receipt")
	}

	retry := mustExecute(t, s, scope, unit)
	if retry.Replayed {
		t.Fatal("a retry after a crash before the effect was reported as a replay")
	}
	execution = mustSnapshot(t, s, scope, "executions", testExecutionID)
	if got := rowString(t, execution, "state"); got != "running" {
		t.Errorf("retried execution state = %q; want running", got)
	}
	if got := rowVersion(t, execution, "version"); got != 2 {
		t.Errorf("retried execution version = %d; want 2", got)
	}
	if events := s.Events(scope); len(events) != 1 {
		t.Errorf("retry published %d events; want 1", len(events))
	}
}

func TestLostResponseRetryReplaysCommittedResult(t *testing.T) {
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "executions", testExecutionID, executionValues(1, "queued"))
	record := defaultReceipt(t)
	unit := store.Unit{
		Writes:  []store.Write{{Table: "executions", ID: testExecutionID, ExpectVersion: 1, Values: map[string]any{"state": "running"}}},
		Receipt: &record,
		Events:  []event.Envelope{defaultEvent(scope, "event-1", event.TypeExecutionStarted, testExecutionID)},
	}

	first := mustExecute(t, s, scope, unit)
	// Simulate the caller losing the response after the commit: the durable
	// receipt is the only surviving evidence.
	second := mustExecute(t, s, scope, unit)
	if !second.Replayed {
		t.Fatal("a retry after a lost response did not replay the committed result")
	}
	if string(second.Receipt.Result) != string(first.Receipt.Result) {
		t.Errorf("retry result = %q; want the original %q", second.Receipt.Result, first.Receipt.Result)
	}
	if second.Receipt.ExecutionID != first.Receipt.ExecutionID {
		t.Errorf("retry execution = %q; want the original %q", second.Receipt.ExecutionID, first.Receipt.ExecutionID)
	}
	execution := mustSnapshot(t, s, scope, "executions", testExecutionID)
	if got := rowVersion(t, execution, "version"); got != 2 {
		t.Errorf("execution version = %d; want one applied update (2)", got)
	}
	if events := s.Events(scope); len(events) != 1 {
		t.Errorf("a lost-response retry published %d events; want 1", len(events))
	}
}

func replyAndAdmitUnit(t *testing.T) (tenancy.Scope, store.Unit) {
	t.Helper()
	scope := projectScope()
	record := receiptRecord(t, "receipt-reply",
		keyFor(testTenantID, testProjectID, testPrincipalID, "task.reply", "reply-key"),
		`{"task":"task-1","answer":"yes"}`,
		`{"execution_id":"execution-1"}`)
	return scope, store.Unit{
		Locks: []store.Lock{
			{Table: "tasks", IDs: []string{testTaskID}, Expect: map[string]any{"lifecycle_version": uint64(1)}},
		},
		Writes: []store.Write{
			{Table: "tasks", ID: testTaskID, Expect: map[string]any{"lifecycle_version": uint64(1)}, Values: map[string]any{
				"state": "open", "lifecycle_version": uint64(2), "current_execution_id": testExecutionID,
			}},
			{Table: "task_messages", ID: "message-1", Insert: true, Values: map[string]any{
				"task_id": testTaskID, "author_id": testPrincipalID, "at": fixedTime(), "body": "answer",
			}},
			{Table: "executions", ID: testExecutionID, Insert: true, Values: insertValues("executions")},
			{Table: "actions", ID: testActionID, Insert: true, Values: insertValues("actions")},
		},
		Receipt: &record,
		Audit:   []event.Audit{defaultAudit(scope, "audit-reply", "task.reply")},
		Events: []event.Envelope{
			defaultEvent(scope, "event-reply", event.TypeTaskReply, testTaskID),
			defaultEvent(scope, "event-queued", event.TypeExecutionQueued, testExecutionID),
		},
	}
}

func askAndCancelUnit(t *testing.T) (tenancy.Scope, store.Unit) {
	t.Helper()
	scope := projectScope()
	record := receiptRecord(t, "receipt-cancel",
		keyFor(testTenantID, testProjectID, testPrincipalID, "task.cancel", "cancel-key"),
		`{"task":"task-1"}`,
		`{"cancelled":true}`)
	return scope, store.Unit{
		Locks: []store.Lock{
			{Table: "tasks", IDs: []string{testTaskID}, Expect: map[string]any{"lifecycle_version": uint64(1)}},
			{Table: "executions", IDs: []string{testExecutionID}, ExpectVersion: 1},
			{Table: "actions", IDs: []string{testActionID}, ExpectVersion: 1},
		},
		Writes: []store.Write{
			{Table: "tasks", ID: testTaskID, Expect: map[string]any{"lifecycle_version": uint64(1)}, Values: map[string]any{
				"state": "cancelled", "lifecycle_version": uint64(2),
			}},
			{Table: "executions", ID: testExecutionID, ExpectVersion: 1, Values: map[string]any{"state": "cancelled"}},
			{Table: "actions", ID: testActionID, ExpectVersion: 1, Values: map[string]any{"state": "cancelled"}},
			{Table: "task_messages", ID: "message-cancel", Insert: true, Values: map[string]any{
				"task_id": testTaskID, "author_id": testPrincipalID, "at": fixedTime(), "body": "cancelled",
			}},
		},
		Receipt: &record,
		Audit:   []event.Audit{defaultAudit(scope, "audit-cancel", "task.cancel")},
		Events: []event.Envelope{
			defaultEvent(scope, "event-cancel", event.TypeTaskCancelled, testTaskID),
			defaultEvent(scope, "event-exec-cancel", event.TypeExecutionCancelled, testExecutionID),
		},
	}
}
