// PostgreSQL controller engine commit: an engine unit may carry
// execution-bound lifecycle event rows and audit rows atomically with its
// Kernel state writes. These tests drive the in-process SQL emulator and the
// recording fake connection seam; they prove statement order and application
// refusals, never real PostgreSQL locking, RLS or constraint behavior.
package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// withControllerEventCommit sets the overridable declaration-derived switch on
// so the event/audit write path is explicitly active in this test. It is the
// same switch the declaration turns on in production.
func withControllerEventCommit(t *testing.T) {
	t.Helper()
	previous := controllerEventCommit
	controllerEventCommit = true
	t.Cleanup(func() { controllerEventCommit = previous })
}

func controllerEventClock() time.Time {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
}

func controllerEventSeedRows() []seamSeedRow {
	return []seamSeedRow{
		{
			Table: "executions", TenantID: "tenant-1", ProjectID: "project-1", ID: "execution-1", Version: 2,
			Values: map[string]any{"state": "queued", "principal_id": "principal-1"},
		},
		{
			Table: "actions", TenantID: "tenant-1", ProjectID: "project-1", ID: "action-1", Version: 1,
			Values: map[string]any{
				"execution_id": "execution-1", "index": 0, "kind": "container", "state": "pending",
			},
		},
	}
}

func newControllerEventHarness(t *testing.T, rows []seamSeedRow) (*Controller, *emuDB) {
	t.Helper()
	now := controllerEventClock()
	db := newEmuDB("kodama_controller", now)
	seedEmulator(t, db, seamSeed{Rows: rows}, now)
	return NewController(db), db
}

func controllerEventEnvelope(eventID string, eventType event.Type, subject string, subjectVersion uint64, scope tenancy.Scope) event.Envelope {
	return event.Envelope{
		ID:             id.EventID(eventID),
		Type:           eventType,
		Source:         event.SystemSource,
		At:             controllerEventClock(),
		Scope:          scope,
		SubjectID:      subject,
		SubjectVersion: subjectVersion,
		Data:           []byte(`{"subject":"` + subject + `"}`),
	}
}

func controllerAuditRow(auditID string, scope tenancy.Scope) event.Audit {
	return event.Audit{
		ID:        id.EventID(auditID),
		Scope:     scope,
		Actor:     id.PrincipalID("system"),
		Operation: "execution.engine.commit",
		At:        controllerEventClock(),
		Detail:    "engine commit",
	}
}

func controllerEventUnit(scope tenancy.Scope) store.Unit {
	return store.Unit{
		Locks: []store.Lock{
			{Table: "executions", IDs: []string{"execution-1"}, ExpectVersion: 2},
			{Table: "actions", IDs: []string{"action-1"}, ExpectVersion: 1},
		},
		Writes: []store.Write{
			{Table: "executions", ID: "execution-1", ExpectVersion: 2, Values: map[string]any{"state": "running"}},
			{Table: "actions", ID: "action-1", ExpectVersion: 1, Values: map[string]any{"state": "running"}},
		},
		Events: []event.Envelope{
			controllerEventEnvelope("event-1", event.TypeExecutionStarted, "execution-1", 3, scope),
			controllerEventEnvelope("event-2", event.TypeCleanup, "execution-1", 3, scope),
			controllerEventEnvelope("event-3", event.TypeActionTransition, "action-1", 2, scope),
		},
		Audit: []event.Audit{controllerAuditRow("audit-1", scope)},
	}
}

func emuRowsFor(db *emuDB, table string) []*emuRow {
	db.mu.Lock()
	defer db.mu.Unlock()
	return append([]*emuRow(nil), db.rows[table]...)
}

func emuRowInScope(db *emuDB, table, rowID string, scope tenancy.Scope) *emuRow {
	for _, row := range emuRowsFor(db, table) {
		if emuString(row.values["id"]) != rowID {
			continue
		}
		if emuString(row.values["tenant_id"]) != string(scope.TenantID) {
			continue
		}
		project, _ := row.values["project_id"].(string)
		if project != string(scope.ProjectID) {
			continue
		}
		return row
	}
	return nil
}

// The gate switch derives its default from the declaration, so this
// adapter has no second source of truth.
func TestControllerEventCommitSwitchDerivesFromDeclaration(t *testing.T) {
	if controllerEventCommit != store.ControllerEventCommitImplemented {
		t.Fatal("the controller event commit switch must derive from the declaration")
	}
}

// With the switch forced off the adapter still fails closed: an event or audit
// row is forbidden even with a guarded execution lock.
func TestControllerEventCommitGateOffRefusesEventAndAudit(t *testing.T) {
	previous := controllerEventCommit
	controllerEventCommit = false
	t.Cleanup(func() { controllerEventCommit = previous })

	controller, _ := newControllerEventHarness(t, controllerEventSeedRows())
	unit := controllerEventUnit(testProjectScope())
	if _, err := controller.Execute(context.Background(), "execution-1", unit); err == nil {
		t.Fatal("event commit succeeded while the declaration switch was off")
	} else {
		assertFaultClass(t, err, fault.Forbidden)
	}
}

// A successful engine unit commits its state writes, lifecycle events and audit
// row together, with the event publication intent and sequence assigned by the
// adapter.
func TestControllerEventCommitWritesStateEventsAndAudit(t *testing.T) {
	withControllerEventCommit(t)
	scope := testProjectScope()
	controller, db := newControllerEventHarness(t, controllerEventSeedRows())

	if _, err := controller.Execute(context.Background(), "execution-1", controllerEventUnit(scope)); err != nil {
		t.Fatalf("controller event commit: %v", err)
	}

	execution := emuRowInScope(db, "executions", "execution-1", scope)
	if execution == nil {
		t.Fatal("the execution row is missing after commit")
	}
	if emuString(execution.values["state"]) != "running" || emuInt(execution.values["version"]) != 3 {
		t.Errorf("execution state/version = %v/%v; want running/3", execution.values["state"], execution.values["version"])
	}
	action := emuRowInScope(db, "actions", "action-1", scope)
	if action == nil {
		t.Fatal("the action row is missing after commit")
	}
	if emuString(action.values["state"]) != "running" || emuInt(action.values["version"]) != 2 {
		t.Errorf("action state/version = %v/%v; want running/2", action.values["state"], action.values["version"])
	}

	events := emuRowsFor(db, "events")
	if len(events) != 3 {
		t.Fatalf("committed events = %d; want 3", len(events))
	}
	for index, row := range events {
		if emuString(row.values["tenant_id"]) != string(scope.TenantID) || emuString(row.values["project_id"]) != string(scope.ProjectID) {
			t.Errorf("event %d scope = %v/%v; want tenant-1/project-1", index, row.values["tenant_id"], row.values["project_id"])
		}
		if emuString(row.values["publish_state"]) != "pending" {
			t.Errorf("event %d publish_state = %v; want pending", index, row.values["publish_state"])
		}
		if emuInt(row.values["publish_sequence"]) != int64(index+1) {
			t.Errorf("event %d publish_sequence = %v; want %d", index, row.values["publish_sequence"], index+1)
		}
	}

	audit := emuRowInScope(db, "audit", "audit-1", scope)
	if audit == nil {
		t.Fatal("the audit row is missing after commit")
	}
	if emuString(audit.values["operation"]) != "execution.engine.commit" {
		t.Errorf("audit operation = %v; want execution.engine.commit", audit.values["operation"])
	}
}

// A foreign event scope refuses the whole unit before anything is staged: the
// state writes and the same-scope first event roll back too.
func TestControllerEventCommitForeignEventRefusesWholeUnit(t *testing.T) {
	withControllerEventCommit(t)
	scope := testProjectScope()
	controller, db := newControllerEventHarness(t, controllerEventSeedRows())

	unit := controllerEventUnit(scope)
	unit.Events[1].Scope = tenancy.Scope{TenantID: "tenant-2", ProjectID: "project-2"}

	if _, err := controller.Execute(context.Background(), "execution-1", unit); err == nil {
		t.Fatal("a foreign-scope event was committed")
	} else {
		assertFaultClass(t, err, fault.Forbidden)
	}

	execution := emuRowInScope(db, "executions", "execution-1", scope)
	if emuString(execution.values["state"]) != "queued" || emuInt(execution.values["version"]) != 2 {
		t.Errorf("execution changed after a refused unit: %v/%v", execution.values["state"], execution.values["version"])
	}
	action := emuRowInScope(db, "actions", "action-1", scope)
	if emuString(action.values["state"]) != "pending" || emuInt(action.values["version"]) != 1 {
		t.Errorf("action changed after a refused unit: %v/%v", action.values["state"], action.values["version"])
	}
	if rows := emuRowsFor(db, "events"); len(rows) != 0 {
		t.Errorf("committed events = %d after a refused unit; want 0", len(rows))
	}
	if rows := emuRowsFor(db, "audit"); len(rows) != 0 {
		t.Errorf("committed audit rows = %d after a refused unit; want 0", len(rows))
	}
}

// A foreign audit scope refuses the whole unit the same way.
func TestControllerEventCommitForeignAuditRefusesWholeUnit(t *testing.T) {
	withControllerEventCommit(t)
	scope := testProjectScope()
	controller, db := newControllerEventHarness(t, controllerEventSeedRows())

	unit := controllerEventUnit(scope)
	unit.Audit[0].Scope = tenancy.Scope{TenantID: "tenant-2", ProjectID: "project-2"}

	if _, err := controller.Execute(context.Background(), "execution-1", unit); err == nil {
		t.Fatal("a foreign-scope audit row was committed")
	} else {
		assertFaultClass(t, err, fault.Forbidden)
	}
	if rows := emuRowsFor(db, "events"); len(rows) != 0 {
		t.Errorf("committed events = %d after a refused unit; want 0", len(rows))
	}
	if rows := emuRowsFor(db, "audit"); len(rows) != 0 {
		t.Errorf("committed audit rows = %d after a refused unit; want 0", len(rows))
	}
}

// An invalid second event refuses the whole unit; the valid first event is not
// staged either.
func TestControllerEventCommitInvalidSecondEventRefusesWholeUnit(t *testing.T) {
	withControllerEventCommit(t)
	scope := testProjectScope()
	controller, db := newControllerEventHarness(t, controllerEventSeedRows())

	unit := controllerEventUnit(scope)
	unit.Events[1].Type = event.Type("not.a.lifecycle.event")

	if _, err := controller.Execute(context.Background(), "execution-1", unit); err == nil {
		t.Fatal("an invalid event was committed")
	} else {
		assertFaultClass(t, err, fault.InvalidInput)
	}
	if rows := emuRowsFor(db, "events"); len(rows) != 0 {
		t.Errorf("committed events = %d after a refused unit; want 0", len(rows))
	}
	if rows := emuRowsFor(db, "audit"); len(rows) != 0 {
		t.Errorf("committed audit rows = %d after a refused unit; want 0", len(rows))
	}
}

// A stale expected execution version is a version_conflict and stages nothing.
func TestControllerEventCommitStaleExecutionVersion(t *testing.T) {
	withControllerEventCommit(t)
	scope := testProjectScope()
	controller, db := newControllerEventHarness(t, controllerEventSeedRows())

	unit := controllerEventUnit(scope)
	unit.Locks[0].ExpectVersion = 1
	unit.Writes[0].ExpectVersion = 1

	if _, err := controller.Execute(context.Background(), "execution-1", unit); err == nil {
		t.Fatal("a stale execution version was committed")
	} else {
		assertFaultClass(t, err, fault.VersionConflict)
	}
	execution := emuRowInScope(db, "executions", "execution-1", scope)
	if emuString(execution.values["state"]) != "queued" || emuInt(execution.values["version"]) != 2 {
		t.Errorf("execution changed after a stale unit: %v/%v", execution.values["state"], execution.values["version"])
	}
	if rows := emuRowsFor(db, "events"); len(rows) != 0 {
		t.Errorf("committed events = %d after a stale unit; want 0", len(rows))
	}
}

// An event or audit unit without a guarded execution row is refused. An
// execution lock that does not carry an expected version is not a guard.
func TestControllerEventCommitRequiresGuardedExecutionRow(t *testing.T) {
	withControllerEventCommit(t)
	scope := testProjectScope()

	t.Run("no execution lock", func(t *testing.T) {
		controller, db := newControllerEventHarness(t, controllerEventSeedRows())
		unit := controllerEventUnit(scope)
		unit.Locks = unit.Locks[1:]
		if _, err := controller.Execute(context.Background(), "execution-1", unit); err == nil {
			t.Fatal("an event unit without an execution lock was committed")
		} else {
			assertFaultClass(t, err, fault.Forbidden)
		}
		if rows := emuRowsFor(db, "events"); len(rows) != 0 {
			t.Errorf("committed events = %d; want 0", len(rows))
		}
	})

	t.Run("unguarded execution lock", func(t *testing.T) {
		controller, _ := newControllerEventHarness(t, controllerEventSeedRows())
		unit := controllerEventUnit(scope)
		unit.Locks[0].ExpectVersion = 0
		if _, err := controller.Execute(context.Background(), "execution-1", unit); err == nil {
			t.Fatal("an event unit with an unguarded execution lock was committed")
		} else {
			assertFaultClass(t, err, fault.Forbidden)
		}
	})

	t.Run("audit only", func(t *testing.T) {
		controller, _ := newControllerEventHarness(t, controllerEventSeedRows())
		unit := controllerEventUnit(scope)
		unit.Events = nil
		unit.Locks = nil
		if _, err := controller.Execute(context.Background(), "execution-1", unit); err == nil {
			t.Fatal("an audit unit without an execution lock was committed")
		} else {
			assertFaultClass(t, err, fault.Forbidden)
		}
	})

	t.Run("raw insert write", func(t *testing.T) {
		controller, _ := newControllerEventHarness(t, controllerEventSeedRows())
		unit := store.Unit{Writes: []store.Write{{
			Table: "events", ID: "event-9", Insert: true,
			Values: map[string]any{
				"type": "execution.started", "subject_id": "execution-1",
				"source_kind": "shell", "source_principal": "system",
			},
		}}}
		if _, err := controller.Execute(context.Background(), "execution-1", unit); err == nil {
			t.Fatal("a raw event insert without an execution lock was committed")
		} else {
			assertFaultClass(t, err, fault.Forbidden)
		}
	})
}

// The controller still refuses a receipt even when the unit also carries events.
func TestControllerEventCommitStillRefusesReceipt(t *testing.T) {
	withControllerEventCommit(t)
	controller, _ := newControllerEventHarness(t, controllerEventSeedRows())
	unit := controllerEventUnit(testProjectScope())
	unit.Receipt = &receipt.Record{
		Key: receipt.Key{
			TenantID: "tenant-1", ProjectID: "project-1", PrincipalID: "principal-1",
			Operation: "execution.transition", CallerKey: "execution-1",
		},
		ContentHash: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		Result:      []byte("ok"),
		ExecutionID: "execution-1",
		At:          controllerEventClock(),
	}
	if _, err := controller.Execute(context.Background(), "execution-1", unit); err == nil {
		t.Fatal("a controller receipt was committed")
	} else {
		assertFaultClass(t, err, fault.Forbidden)
	}
}

// Events and audit are INSERT-only: an update or delete through the generic
// write path is refused before any statement.
func TestControllerEventCommitRefusesEventAndAuditMutation(t *testing.T) {
	withControllerEventCommit(t)
	scope := testProjectScope()
	cases := []struct {
		name  string
		write store.Write
	}{
		{"event update", store.Write{Table: "events", ID: "event-1", Expect: map[string]any{"publish_state": "pending"}, Values: map[string]any{"publish_state": "published"}}},
		{"event delete", store.Write{Table: "events", ID: "event-1", Delete: true, Expect: map[string]any{"publish_state": "pending"}}},
		{"audit update", store.Write{Table: "audit", ID: "audit-1", Expect: map[string]any{"operation": "execution.engine.commit"}, Values: map[string]any{"operation": "changed"}}},
		{"audit delete", store.Write{Table: "audit", ID: "audit-1", Delete: true, Expect: map[string]any{"operation": "execution.engine.commit"}}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			controller, db := newControllerEventHarness(t, controllerEventSeedRows())
			unit := store.Unit{
				Locks:  []store.Lock{{Table: "executions", IDs: []string{"execution-1"}, ExpectVersion: 2}},
				Writes: []store.Write{test.write},
				Events: []event.Envelope{controllerEventEnvelope("event-9", event.TypeExecutionStarted, "execution-1", 3, scope)},
			}
			if _, err := controller.Execute(context.Background(), "execution-1", unit); err == nil {
				t.Fatal("an event/audit mutation was committed")
			} else {
				assertFaultClass(t, err, fault.Forbidden)
			}
			if rows := emuRowsFor(db, "events"); len(rows) != 0 {
				t.Errorf("committed events = %d; want 0", len(rows))
			}
		})
	}
}

// controllerEventScript serves the queries the controller event path issues: the
// plain execution scope derivation, the scoped execution FOR UPDATE re-read,
// the other scoped lock/guard selects and the same-scope reclassify lookup.
func controllerEventScript() func(sql string, args []any) (store.Rows, error) {
	return func(sql string, args []any) (store.Rows, error) {
		lower := strings.ToLower(sql)
		switch {
		case strings.Contains(lower, "select tenant_id, project_id, version from executions"):
			return &fakeRows{rows: [][]any{{testTenantID, testProjectID, int64(2)}}, index: -1}, nil
		case strings.Contains(lower, "not ("):
			// The row exists in the same scope, so the cross-scope existence
			// predicate matches no row and the outcome is a version conflict.
			return &fakeRows{index: -1}, nil
		case strings.Contains(lower, "from executions"):
			return &fakeRows{rows: [][]any{{testExecutionID}}, index: -1}, nil
		case strings.Contains(lower, "from actions"):
			return &fakeRows{rows: [][]any{{"action-1"}}, index: -1}, nil
		default:
			return &fakeRows{index: -1}, nil
		}
	}
}

// The controller derives the execution scope with a plain SELECT (no lock),
// sets the transaction-local scope, then takes the execution FOR UPDATE lock and
// re-reads its scope/version under that scope before inserting the event/audit
// rows and committing the Kernel state writes once.
func TestControllerEventCommitStatementOrder(t *testing.T) {
	withControllerEventCommit(t)
	db := newFakeDB()
	db.script.setQuery(controllerEventScript())
	controller := NewController(db)

	if _, err := controller.Execute(context.Background(), "execution-1", controllerEventUnit(testProjectScope())); err != nil {
		t.Fatalf("controller event commit: %v", err)
	}
	conn := db.conn(t)
	if conn.commits != 1 || conn.rollbacks != 0 {
		t.Fatalf("terminal calls: commits=%d rollbacks=%d; want one commit", conn.commits, conn.rollbacks)
	}
	calls := conn.callsCopy()

	derive := indexOfExecutionDerive(calls)
	executionLock := indexOfExecutionLock(calls)
	scopeSet := indexOfSQL(calls, "set_config")
	eventsInsert := indexOfTable(calls, "events", "insert")
	auditInsert := indexOfTable(calls, "audit", "insert")
	executionWrite := indexOfTable(calls, "executions", "update")
	actionWrite := indexOfTable(calls, "actions", "update")
	commit := indexOfKind(calls, "commit")
	if derive < 0 || executionLock < 0 || scopeSet < 0 || eventsInsert < 0 || auditInsert < 0 ||
		executionWrite < 0 || actionWrite < 0 || commit < 0 {
		t.Fatalf("missing calls: derive=%d lock=%d scope=%d events=%d audit=%d execWrite=%d actionWrite=%d commit=%d",
			derive, executionLock, scopeSet, eventsInsert, auditInsert, executionWrite, actionWrite, commit)
	}
	if !(derive < scopeSet) {
		t.Error("the execution scope must be derived before the transaction-local scope is set")
	}
	if !(scopeSet < executionLock) {
		t.Error("the execution FOR UPDATE must follow the transaction-local scope")
	}
	// PostgreSQL applies the UPDATE policy's USING expression to a locking read,
	// so any FOR UPDATE before the scope is set is filtered to zero rows.
	for index := range scopeSet {
		if strings.Contains(strings.ToLower(calls[index].sql), "for update") {
			t.Errorf("a FOR UPDATE was issued before the transaction-local scope was set: %s", calls[index].sql)
		}
	}
	if !(executionLock < eventsInsert && executionLock < auditInsert) {
		t.Error("the locked execution row must be re-read before the event/audit inserts")
	}
	if !(eventsInsert < executionWrite && auditInsert < executionWrite) {
		t.Error("the event/audit inserts must precede the Kernel state writes")
	}
	if !(executionWrite < commit && actionWrite < commit) {
		t.Error("the state writes must precede the commit")
	}
	deriveText := strings.ToLower(calls[derive].sql)
	if strings.Contains(deriveText, "for update") {
		t.Error("the scope derivation must be a plain SELECT, not a locking read")
	}
	for _, column := range []string{"tenant_id", "project_id", "version"} {
		if !strings.Contains(deriveText, column) {
			t.Errorf("the scope derivation does not select %q: %s", column, calls[derive].sql)
		}
	}
	lockText := strings.ToLower(calls[executionLock].sql)
	if !strings.Contains(lockText, "for update") {
		t.Error("the scoped execution re-read did not lock the row FOR UPDATE")
	}
	for _, column := range []string{"tenant_id", "project_id", "version"} {
		if !strings.Contains(lockText, column) {
			t.Errorf("the locked re-read does not re-verify %q: %s", column, calls[executionLock].sql)
		}
	}
	if indexOfSQL(calls, "execution-1") >= 0 {
		t.Error("an identity was interpolated into the SQL text instead of travelling as an argument")
	}
}

// indexOfExecutionDerive returns the first plain (non-locking) execution scope
// SELECT, which is the pre-scope derivation.
func indexOfExecutionDerive(calls []recordedCall) int {
	for index, call := range calls {
		text := strings.ToLower(call.sql)
		if strings.Contains(text, "from executions") && !strings.Contains(text, "for update") {
			return index
		}
	}
	return -1
}

// indexOfExecutionLock returns the first execution FOR UPDATE re-read.
func indexOfExecutionLock(calls []recordedCall) int {
	for index, call := range calls {
		text := strings.ToLower(call.sql)
		if strings.Contains(text, "from executions") && strings.Contains(text, "for update") {
			return index
		}
	}
	return -1
}

// The emulator models the controller UPDATE policy on locking reads by default:
// a FOR UPDATE on executions issued before the transaction-local scope is set
// returns zero rows, exactly as PostgreSQL's RLS UPDATE-policy filter does.
func TestEmulatorControllerLockPolicyRequiresTransactionScope(t *testing.T) {
	now := controllerEventClock()
	db := newEmuDB("kodama_controller", now)
	seedEmulator(t, db, seamSeed{Rows: controllerEventSeedRows()}, now)
	ctx := context.Background()
	conn, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin emulator transaction: %v", err)
	}
	defer func() { _ = conn.Rollback(ctx) }()

	rows, err := conn.Query(ctx, `SELECT tenant_id, project_id, version FROM executions WHERE id = $1 FOR UPDATE`, "execution-1")
	if err != nil {
		t.Fatalf("unscoped execution FOR UPDATE: %v", err)
	}
	if rows.Next() {
		t.Fatal("a locking read on executions returned a row before the transaction-local scope was set")
	}
	_ = rows.Close()

	if _, err := conn.Exec(ctx, establishScopeSQL, "tenant-1", "project-1"); err != nil {
		t.Fatalf("establish scope: %v", err)
	}
	rows, err = conn.Query(ctx, `SELECT tenant_id, project_id, version FROM executions WHERE id = $1 FOR UPDATE`, "execution-1")
	if err != nil {
		t.Fatalf("scoped execution FOR UPDATE: %v", err)
	}
	if !rows.Next() {
		t.Fatal("a locking read on executions did not see the in-scope row after the scope was set")
	}
	_ = rows.Close()
}

// A row that changes scope between the plain derivation and the scoped lock is
// refused: the lock is filtered by the new scope and the whole unit stages
// nothing.
func TestControllerEventCommitScopeChangeBetweenDeriveAndLockRefused(t *testing.T) {
	withControllerEventCommit(t)
	scope := testProjectScope()
	controller, db := newControllerEventHarness(t, controllerEventSeedRows())

	var moved bool
	db.onStatement = func(conn *emuConn, sql string) {
		text := strings.ToLower(sql)
		if moved || !strings.Contains(text, "from executions") || strings.Contains(text, "for update") {
			return
		}
		moved = true
		for _, row := range conn.working["executions"] {
			if emuString(row.values["id"]) == "execution-1" {
				row.values["project_id"] = "project-2"
			}
		}
	}

	if _, err := controller.Execute(context.Background(), "execution-1", controllerEventUnit(scope)); err == nil {
		t.Fatal("an execution row that changed scope between derivation and lock was committed")
	} else {
		assertFaultClass(t, err, fault.Forbidden)
	}
	if !moved {
		t.Fatal("the test hook did not move the execution row between derivation and lock")
	}

	execution := emuRowInScope(db, "executions", "execution-1", scope)
	if execution == nil {
		t.Fatal("the execution row is missing after the refused unit")
	}
	if emuString(execution.values["state"]) != "queued" || emuInt(execution.values["version"]) != 2 {
		t.Errorf("execution changed after a refused scope move: %v/%v", execution.values["state"], execution.values["version"])
	}
	if rows := emuRowsFor(db, "events"); len(rows) != 0 {
		t.Errorf("committed events = %d after a refused scope move; want 0", len(rows))
	}
	if rows := emuRowsFor(db, "audit"); len(rows) != 0 {
		t.Errorf("committed audit rows = %d after a refused scope move; want 0", len(rows))
	}
}

// A version change between the plain derivation and the scoped lock is caught
// by the locked re-read against the guard, and nothing is staged.
func TestControllerEventCommitVersionChangeBetweenDeriveAndLockRefused(t *testing.T) {
	withControllerEventCommit(t)
	scope := testProjectScope()
	controller, db := newControllerEventHarness(t, controllerEventSeedRows())

	var bumped bool
	db.onStatement = func(conn *emuConn, sql string) {
		text := strings.ToLower(sql)
		if bumped || !strings.Contains(text, "from executions") || strings.Contains(text, "for update") {
			return
		}
		bumped = true
		for _, row := range conn.working["executions"] {
			if emuString(row.values["id"]) == "execution-1" {
				row.values["version"] = int64(5)
			}
		}
	}

	if _, err := controller.Execute(context.Background(), "execution-1", controllerEventUnit(scope)); err == nil {
		t.Fatal("an execution row that changed version between derivation and lock was committed")
	} else {
		assertFaultClass(t, err, fault.VersionConflict)
	}
	if !bumped {
		t.Fatal("the test hook did not bump the execution version between derivation and lock")
	}
	execution := emuRowInScope(db, "executions", "execution-1", scope)
	if execution == nil {
		t.Fatal("the execution row is missing after the refused unit")
	}
	if emuString(execution.values["state"]) != "queued" || emuInt(execution.values["version"]) != 2 {
		t.Errorf("execution changed after a refused version bump: %v/%v", execution.values["state"], execution.values["version"])
	}
	if rows := emuRowsFor(db, "events"); len(rows) != 0 {
		t.Errorf("committed events = %d after a refused version bump; want 0", len(rows))
	}
	if rows := emuRowsFor(db, "audit"); len(rows) != 0 {
		t.Errorf("committed audit rows = %d after a refused version bump; want 0", len(rows))
	}
}

func indexOfKind(calls []recordedCall, kind string) int {
	for index, call := range calls {
		if call.kind == kind {
			return index
		}
	}
	return -1
}

// Event data and audit detail travel as statement arguments, never as SQL text,
// so a payload cannot inject or rewrite the statement.
func TestControllerEventCommitParameterizesPayloads(t *testing.T) {
	withControllerEventCommit(t)
	scope := testProjectScope()
	db := newFakeDB()
	db.script.setQuery(controllerEventScript())
	controller := NewController(db)

	unit := controllerEventUnit(scope)
	payload := `{"note":"'; DROP TABLE events; --"}`
	unit.Events[0].Data = []byte(payload)
	unit.Audit[0].Detail = payload

	if _, err := controller.Execute(context.Background(), "execution-1", unit); err != nil {
		t.Fatalf("controller event commit: %v", err)
	}
	calls := db.conn(t).callsCopy()
	eventInsert := indexOfTable(calls, "events", "insert")
	if eventInsert < 0 {
		t.Fatal("the event insert was not issued")
	}
	if strings.Contains(calls[eventInsert].sql, "DROP TABLE") {
		t.Error("the event payload was interpolated into the SQL text")
	}
	if !argContains(calls[eventInsert].args, []byte(payload)) {
		t.Error("the event payload did not travel as a statement argument")
	}
	auditInsert := indexOfTable(calls, "audit", "insert")
	if auditInsert < 0 {
		t.Fatal("the audit insert was not issued")
	}
	if strings.Contains(calls[auditInsert].sql, "DROP TABLE") {
		t.Error("the audit detail was interpolated into the SQL text")
	}
	if !argContains(calls[auditInsert].args, payload) {
		t.Error("the audit detail did not travel as a statement argument")
	}
}

// Two concurrent engine commits for the same expected execution version settle
// with exactly one winner; the loser is a version_conflict and rolls back its
// already-staged event/audit inserts.
func TestControllerEventCommitConcurrentOneWinner(t *testing.T) {
	withControllerEventCommit(t)
	db := newFakeDB()
	db.script.setQuery(controllerEventScript())
	var updates atomic.Int32
	db.script.setExec(func(sql string, args []any) (store.Result, error) {
		if indexOfTable([]recordedCall{{kind: "exec", sql: sql}}, "executions", "update") == 0 {
			if updates.Add(1) == 1 {
				return store.Result{RowsAffected: 1}, nil
			}
			return store.Result{RowsAffected: 0}, nil
		}
		return store.Result{RowsAffected: 1}, nil
	})
	controller := NewController(db)
	unit := controllerEventUnit(testProjectScope())

	ready := make(chan struct{}, 2)
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			ready <- struct{}{}
			<-start
			_, err := controller.Execute(context.Background(), "execution-1", unit)
			results <- err
		}()
	}
	<-ready
	<-ready
	close(start)

	var success, conflicts int
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			success++
		case faultClassIs(err, fault.VersionConflict):
			conflicts++
		default:
			t.Fatalf("unexpected concurrent result: %v", err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("concurrent event commits: success=%d conflicts=%d; want exactly one winner", success, conflicts)
	}
	commits, rollbacks := 0, 0
	for _, conn := range db.connsCopy() {
		commits += conn.commits
		rollbacks += conn.rollbacks
	}
	if commits != 1 || rollbacks != 1 {
		t.Fatalf("concurrent event commit terminal calls: commits=%d rollbacks=%d; want one commit and one rollback", commits, rollbacks)
	}
}

// The controller contract vectors run against the PostgreSQL adapter over the
// emulator. They are the same semantic inputs the contract reference controller
// runs; they prove no real PostgreSQL isolation or RLS behavior.
type controllerVectorDocument struct {
	Version int                `json:"version"`
	Vectors []controllerVector `json:"vectors"`
}

type controllerVector struct {
	Name  string                 `json:"name"`
	Clock string                 `json:"clock"`
	Seed  seamSeed               `json:"seed"`
	Steps []controllerVectorStep `json:"steps"`
}

type controllerVectorStep struct {
	Op            string                  `json:"op"`
	Table         string                  `json:"table"`
	ID            string                  `json:"id"`
	ExecutionID   string                  `json:"execution_id"`
	Scope         *seamScope              `json:"scope"`
	Locks         []seamLock              `json:"locks"`
	Writes        []seamWrite             `json:"writes"`
	Events        []controllerVectorEvent `json:"events"`
	Audit         []controllerVectorAudit `json:"audit"`
	Receipt       *seamSeedReceipt        `json:"receipt"`
	ReceiptMode   string                  `json:"receipt_mode"`
	ReceiptExpect *seamReceiptExpect      `json:"receipt_expect"`
	Equals        map[string]any          `json:"equals"`
	Range         []scanRangeJSON         `json:"range"`
	OrderBy       []scanOrderJSON         `json:"order_by"`
	Limit         int                     `json:"limit"`
	WantValues    map[string]any          `json:"want_values"`
	WantVersion   *uint64                 `json:"want_version"`
	WantAbsent    bool                    `json:"want_absent"`
	WantIDs       []string                `json:"want_ids"`
	WantError     string                  `json:"want_error"`
}

type controllerVectorEvent struct {
	ID              string     `json:"id"`
	Type            string     `json:"type"`
	ReportType      string     `json:"report_type"`
	SourceKind      string     `json:"source_kind"`
	SourcePrincipal string     `json:"source_principal"`
	At              string     `json:"at"`
	Scope           *seamScope `json:"scope"`
	SubjectID       string     `json:"subject_id"`
	SubjectVersion  uint64     `json:"subject_version"`
	CorrelationID   string     `json:"correlation_id"`
	CausationID     string     `json:"causation_id"`
	Data            string     `json:"data"`
}

type controllerVectorAudit struct {
	ID        string     `json:"id"`
	Scope     *seamScope `json:"scope"`
	Actor     string     `json:"actor"`
	Operation string     `json:"operation"`
	At        string     `json:"at"`
	Detail    string     `json:"detail"`
}

func TestControllerContractVectors(t *testing.T) {
	withControllerEventCommit(t)
	document := loadControllerVectors(t)
	if document.Version != 1 || len(document.Vectors) == 0 {
		t.Fatalf("controller vector document version=%d vectors=%d; want 1 and non-empty", document.Version, len(document.Vectors))
	}
	for _, vector := range document.Vectors {
		t.Run(vector.Name, func(t *testing.T) {
			now := seamClock(vector.Clock)
			db := newEmuDB("kodama_controller", now)
			seedEmulator(t, db, vector.Seed, now)
			controller := NewController(db)
			for index, step := range vector.Steps {
				if err := runControllerVectorStep(controller, db, step, now); err != nil {
					t.Fatalf("step %d: %v", index, err)
				}
			}
		})
	}
}

func runControllerVectorStep(controller *Controller, db *emuDB, step controllerVectorStep, now time.Time) error {
	switch step.Op {
	case "execute":
		unit, err := controllerVectorUnit(step, now)
		if err != nil {
			return err
		}
		_, err = controller.Execute(context.Background(), step.ExecutionID, unit)
		return checkControllerVectorError(step.WantError, err)
	case "scan":
		scan := store.ControllerScan{
			Table: step.Table, Equals: step.Equals, Range: scannerRangeFilters(step.Range),
			OrderBy: scannerOrderTerms(step.OrderBy), Limit: step.Limit}
		page, err := controller.Scan(context.Background(), scan)
		if err := checkControllerVectorError(step.WantError, err); err != nil {
			return err
		}
		if step.WantError != "" {
			return nil
		}
		got := make([]string, 0, len(page.Rows))
		for _, row := range page.Rows {
			got = append(got, emuString(row.Values["id"]))
		}
		if step.WantIDs != nil && !equalStrings(got, step.WantIDs) {
			return fmt.Errorf("ids = %v; want %v", got, step.WantIDs)
		}
		return nil
	case "inspect":
		if step.Scope == nil {
			return fmt.Errorf("inspect step has no scope")
		}
		row := emuRowInScope(db, step.Table, step.ID, seamScopeOf(*step.Scope))
		if row == nil {
			if step.WantAbsent {
				return nil
			}
			return fmt.Errorf("row %s/%s is absent; want present", step.Table, step.ID)
		}
		if step.WantAbsent {
			return fmt.Errorf("row %s/%s is present; want absent", step.Table, step.ID)
		}
		if step.WantVersion != nil {
			if version := emuInt(row.values["version"]); version != int64(*step.WantVersion) {
				return fmt.Errorf("row %s/%s version = %d; want %d", step.Table, step.ID, version, *step.WantVersion)
			}
		}
		for column, want := range step.WantValues {
			if emuCompare(row.values[column], want) != 0 {
				return fmt.Errorf("row %s/%s column %q = %v; want %v", step.Table, step.ID, column, row.values[column], want)
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown op %q", step.Op)
	}
}

func controllerVectorUnit(step controllerVectorStep, now time.Time) (store.Unit, error) {
	unit := store.Unit{ReceiptMode: store.ReceiptMode(step.ReceiptMode)}
	unit.Locks = seamLocks(step.Locks)
	unit.Writes = seamWrites(step.Writes)
	for _, spec := range step.Events {
		source := event.SystemSource
		if spec.SourceKind != "" {
			source.Kind = event.SourceKind(spec.SourceKind)
		}
		if spec.SourcePrincipal != "" {
			source.PrincipalID = id.PrincipalID(spec.SourcePrincipal)
		}
		scope := seamScope{}
		if spec.Scope != nil {
			scope = *spec.Scope
		}
		unit.Events = append(unit.Events, event.Envelope{
			ID:             id.EventID(spec.ID),
			Type:           event.Type(spec.Type),
			ReportType:     event.ReportType(spec.ReportType),
			Source:         source,
			At:             seamClock(spec.At),
			Scope:          seamScopeOf(scope),
			SubjectID:      spec.SubjectID,
			SubjectVersion: spec.SubjectVersion,
			CorrelationID:  spec.CorrelationID,
			CausationID:    spec.CausationID,
			Data:           []byte(spec.Data),
		})
	}
	for _, spec := range step.Audit {
		scope := seamScope{}
		if spec.Scope != nil {
			scope = *spec.Scope
		}
		unit.Audit = append(unit.Audit, event.Audit{
			ID:        id.EventID(spec.ID),
			Scope:     seamScopeOf(scope),
			Actor:     id.PrincipalID(spec.Actor),
			Operation: spec.Operation,
			At:        seamClock(spec.At),
			Detail:    spec.Detail,
		})
	}
	if step.Receipt != nil {
		record := receipt.Record{
			ID: "receipt-vector",
			Key: receipt.Key{
				TenantID:    id.TenantID(step.Receipt.TenantID),
				ProjectID:   id.ProjectID(step.Receipt.ProjectID),
				PrincipalID: id.PrincipalID(step.Receipt.PrincipalID),
				Operation:   step.Receipt.Operation,
				CallerKey:   step.Receipt.CallerKey,
			},
			ContentHash: step.Receipt.ContentHash,
			ExecutionID: id.ExecutionID(step.Receipt.ExecutionID),
			At:          now,
		}
		if step.Receipt.Result != "" {
			record.Result = []byte(step.Receipt.Result)
		}
		unit.Receipt = &record
	}
	if step.ReceiptExpect != nil {
		unit.ReceiptExpect = &store.ReceiptExpect{Completed: step.ReceiptExpect.Completed, ContentHash: step.ReceiptExpect.ContentHash}
	}
	return unit, nil
}

func checkControllerVectorError(want string, err error) error {
	if want != "" {
		if err == nil {
			return fmt.Errorf("succeeded; want error class %q", want)
		}
		class, ok := fault.ClassOf(err)
		if !ok || string(class) != want {
			return fmt.Errorf("error = %v (class=%q); want %q", err, class, want)
		}
		return nil
	}
	return err
}

func loadControllerVectors(t *testing.T) controllerVectorDocument {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "test", "contracts", "adapters", "testdata", "controller_events.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read controller vectors: %v", err)
	}
	var document controllerVectorDocument
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode controller vectors: %v", err)
	}
	return document
}

// A committed event or audit row reaches its table only through the unit's
// publication records (Unit.Events, Unit.Audit). A raw business insert into
// either table is refused by the shared write rules even under a guarded
// execution row, exactly as the memory controller refuses it, so no row can be
// written without the scope, shape and sequence checks of the publication path.
func TestControllerEventCommitRefusesRawPublicationInsert(t *testing.T) {
	withControllerEventCommit(t)
	cases := []struct {
		name  string
		write store.Write
	}{
		{"event insert", store.Write{Table: "events", ID: "event-9", Insert: true, Values: map[string]any{
			"type": "execution.started", "subject_id": "execution-1", "source_kind": "shell", "source_principal": "system",
		}}},
		{"audit insert", store.Write{Table: "audit", ID: "audit-9", Insert: true, Values: map[string]any{
			"actor": "system", "operation": "execution.engine.commit",
		}}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			controller, db := newControllerEventHarness(t, controllerEventSeedRows())
			unit := store.Unit{
				Locks:  []store.Lock{{Table: "executions", IDs: []string{"execution-1"}, ExpectVersion: 2}},
				Writes: []store.Write{test.write},
			}
			_, err := controller.Execute(context.Background(), "execution-1", unit)
			assertFaultClass(t, err, fault.InvalidInput)
			if rows := emuRowsFor(db, test.write.Table); len(rows) != 0 {
				t.Errorf("committed %s rows = %d; want 0", test.write.Table, len(rows))
			}
		})
	}
}
