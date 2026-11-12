// Atomic commit-unit behavior: one transaction, declared lock order, versioned
// writes, receipt identity, state/event coupling, rollback and concurrent
// one-winner settlement.
package postgres

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func TestExecuteUnitCommitsStateReceiptAuditAndEventsTogether(t *testing.T) {
	db := newFakeDB()
	connector := New(db)
	record := testReceipt(t)
	scope := testProjectScope()
	unit := store.Unit{
		Locks: []store.Lock{
			{Table: "cleanup", IDs: []string{testExecutionID}},
			{Table: "executions", IDs: []string{testExecutionID}},
		},
		Writes: []store.Write{
			{Table: "actions", ID: "action-1", Insert: true, Values: unitValues("actions")},
			{Table: "tasks", ID: "task-1", Insert: true, Values: unitValues("tasks")},
			{Table: "executions", ID: testExecutionID, Insert: true, Values: unitValues("executions")},
		},
		Receipt: &record,
		Audit:   []event.Audit{testAudit(scope)},
		Events:  []event.Envelope{testEvent(scope)},
	}
	result, err := ExecuteUnit(context.Background(), connector, scope, unit)
	if err != nil {
		t.Fatalf("ExecuteUnit: %v", err)
	}
	if result.Replayed {
		t.Fatal("a fresh commit unit was reported as a replay")
	}
	if result.Receipt.ExecutionID != record.ExecutionID {
		t.Errorf("committed receipt execution = %q; want %q", result.Receipt.ExecutionID, record.ExecutionID)
	}

	conn := db.conn(t)
	if conn.commits != 1 || conn.rollbacks != 0 {
		t.Fatalf("terminal calls: commits=%d rollbacks=%d; want one commit", conn.commits, conn.rollbacks)
	}
	calls := conn.callsCopy()

	executionsLock := indexOfTable(calls, "executions", "lock")
	cleanupLock := indexOfTable(calls, "cleanup", "lock")
	if executionsLock < 0 || cleanupLock < 0 {
		t.Fatalf("locks were not acquired: executions=%d cleanup=%d", executionsLock, cleanupLock)
	}
	if executionsLock > cleanupLock {
		t.Error("executions must be locked before cleanup")
	}

	tasksWrite := indexOfTable(calls, "tasks", "insert")
	executionsWrite := indexOfTable(calls, "executions", "insert")
	actionsWrite := indexOfTable(calls, "actions", "insert")
	if tasksWrite < 0 || executionsWrite < 0 || actionsWrite < 0 {
		t.Fatalf("unit writes missing: tasks=%d executions=%d actions=%d", tasksWrite, executionsWrite, actionsWrite)
	}
	if !(executionsLock < tasksWrite && cleanupLock < tasksWrite) {
		t.Error("a write was issued before the declared locks")
	}
	if !(tasksWrite < executionsWrite && executionsWrite < actionsWrite) {
		t.Error("writes were not applied in the declared lock order")
	}

	receiptInsert := indexOfTable(calls, "receipts", "insert")
	auditInsert := indexOfTable(calls, "audit", "insert")
	eventInsert := indexOfTable(calls, "events", "insert")
	if receiptInsert < 0 || auditInsert < 0 || eventInsert < 0 {
		t.Fatalf("commit-unit records missing: receipt=%d audit=%d event=%d", receiptInsert, auditInsert, eventInsert)
	}
	if !(actionsWrite < receiptInsert) {
		t.Error("the receipt must be inserted after the business writes")
	}
	if !(receiptInsert < auditInsert && auditInsert < eventInsert) {
		t.Error("audit/event intent must be committed after the receipt in the declared lock order")
	}
	if !eventPublishIntent(calls[eventInsert]) {
		t.Error("the event row must carry committed pending publication intent")
	}
}

func eventPublishIntent(call recordedCall) bool {
	text := strings.ToLower(call.sql)
	if strings.Contains(text, "pending") {
		return true
	}
	return argContains(call.args, "pending")
}

func indexOfTable(calls []recordedCall, table, kind string) int {
	pattern := regexp.MustCompile(`(?:^|[^a-z0-9_])` + regexp.QuoteMeta(table) + `(?:[^a-z0-9_]|$)`)
	for index, call := range calls {
		if call.kind != "exec" {
			continue
		}
		text := strings.TrimSpace(strings.ToLower(call.sql))
		switch kind {
		case "lock":
			if !strings.Contains(text, "for update") || !pattern.MatchString(text) {
				continue
			}
		case "insert":
			if !strings.HasPrefix(text, "insert into ") || !pattern.MatchString(text) {
				continue
			}
		case "update":
			if !strings.HasPrefix(text, "update ") || !pattern.MatchString(text) {
				continue
			}
		default:
			continue
		}
		return index
	}
	return -1
}

func TestExecuteUnitRollsBackOnInjectedFailure(t *testing.T) {
	db := newFakeDB()
	injected := errors.New("event write failed")
	db.script.setExec(func(sql string, args []any) (store.Result, error) {
		if indexOfTable([]recordedCall{{kind: "exec", sql: sql}}, "events", "insert") == 0 {
			return store.Result{}, injected
		}
		return store.Result{RowsAffected: 1}, nil
	})
	connector := New(db)
	record := testReceipt(t)
	scope := testProjectScope()
	unit := store.Unit{
		Writes:  []store.Write{{Table: "executions", ID: testExecutionID, Insert: true, Values: unitValues("executions")}},
		Receipt: &record,
		Audit:   []event.Audit{testAudit(scope)},
		Events:  []event.Envelope{testEvent(scope)},
	}
	if _, err := ExecuteUnit(context.Background(), connector, scope, unit); !errors.Is(err, injected) {
		t.Fatalf("ExecuteUnit error = %v; want the injected cause", err)
	}
	conn := db.conn(t)
	if conn.commits != 0 {
		t.Fatalf("a failed commit unit committed %d times", conn.commits)
	}
	if conn.rollbacks != 1 {
		t.Fatalf("a failed commit unit rolled back %d times; want 1", conn.rollbacks)
	}
}

func TestExecuteUnitCommitFailureNeverReportsSuccess(t *testing.T) {
	db := newFakeDB()
	injected := errors.New("commit outcome unknown")
	db.script.commitErr = injected
	connector := New(db)
	record := testReceipt(t)
	scope := testProjectScope()
	unit := store.Unit{
		Writes:  []store.Write{{Table: "executions", ID: testExecutionID, Insert: true, Values: unitValues("executions")}},
		Receipt: &record,
	}
	result, err := ExecuteUnit(context.Background(), connector, scope, unit)
	if !errors.Is(err, injected) {
		t.Fatalf("ExecuteUnit error = %v; want the injected commit cause", err)
	}
	if result.Replayed {
		t.Fatal("a failed commit was reported as a replay")
	}
	conn := db.conn(t)
	if conn.commits != 1 || conn.rollbacks != 1 {
		t.Fatalf("terminal calls after commit failure: commits=%d rollbacks=%d; want one attempt and one cleanup", conn.commits, conn.rollbacks)
	}
}

func TestExecuteUnitStaleVersionRefusedWithoutPartialCommit(t *testing.T) {
	db := newFakeDB()
	db.script.setExec(func(sql string, args []any) (store.Result, error) {
		if indexOfTable([]recordedCall{{kind: "exec", sql: sql}}, "executions", "update") == 0 {
			return store.Result{RowsAffected: 0}, nil
		}
		return store.Result{RowsAffected: 1}, nil
	})
	connector := New(db)
	record := testReceipt(t)
	scope := testProjectScope()
	unit := store.Unit{
		Writes:  []store.Write{{Table: "executions", ID: testExecutionID, ExpectVersion: 3, Values: map[string]any{"state": "running"}}},
		Receipt: &record,
		Events:  []event.Envelope{testEvent(scope)},
	}
	_, err := ExecuteUnit(context.Background(), connector, scope, unit)
	assertFaultClass(t, err, fault.VersionConflict)
	conn := db.conn(t)
	if conn.commits != 0 || conn.rollbacks != 1 {
		t.Fatalf("stale version terminal calls: commits=%d rollbacks=%d; want one rollback", conn.commits, conn.rollbacks)
	}
	if indexOfTable(conn.callsCopy(), "receipts", "insert") >= 0 {
		t.Error("a stale-version unit inserted its receipt")
	}
}

func TestExecuteUnitReplaysEqualReceipt(t *testing.T) {
	db := newFakeDB()
	stored := testReceipt(t)
	db.script.setQuery(queryByTable(map[string]map[string]any{"receipts": receiptFixture(t, stored)}))
	connector := New(db)
	candidate := testReceipt(t)
	scope := testProjectScope()
	unit := store.Unit{
		Writes:  []store.Write{{Table: "executions", ID: testExecutionID, Insert: true, Values: unitValues("executions")}},
		Receipt: &candidate,
		Events:  []event.Envelope{testEvent(scope)},
	}
	result, err := ExecuteUnit(context.Background(), connector, scope, unit)
	if err != nil {
		t.Fatalf("ExecuteUnit replay: %v", err)
	}
	if !result.Replayed {
		t.Fatal("equal receipt content did not replay the committed result")
	}
	if result.Receipt.ExecutionID != stored.ExecutionID {
		t.Errorf("replayed execution = %q; want the committed execution %q", result.Receipt.ExecutionID, stored.ExecutionID)
	}
	if string(result.Receipt.Result) != string(stored.Result) {
		t.Errorf("replayed result = %q; want %q", result.Receipt.Result, stored.Result)
	}
	conn := db.conn(t)
	if conn.terminalCalls() != 1 {
		t.Fatalf("replay issued %d terminal calls; want exactly 1", conn.terminalCalls())
	}
	calls := conn.callsCopy()
	if indexOfTable(calls, "executions", "insert") >= 0 {
		t.Error("a replay re-applied the business write")
	}
	if indexOfTable(calls, "events", "insert") >= 0 {
		t.Error("a replay re-published the event")
	}
	if indexOfTable(calls, "receipts", "insert") >= 0 {
		t.Error("a replay inserted a second receipt")
	}
	if !containsSQL(calls, "from receipts") {
		t.Error("the adapter did not read the existing receipt before applying the unit")
	}
}

func TestExecuteUnitRefusesChangedReceiptContent(t *testing.T) {
	db := newFakeDB()
	stored := testReceipt(t)
	stored.ContentHash = testFingerprint(t, []byte(`{"a":2}`))
	db.script.setQuery(queryByTable(map[string]map[string]any{"receipts": receiptFixture(t, stored)}))
	connector := New(db)
	candidate := testReceipt(t)
	scope := testProjectScope()
	unit := store.Unit{
		Writes:  []store.Write{{Table: "executions", ID: testExecutionID, Insert: true, Values: unitValues("executions")}},
		Receipt: &candidate,
		Events:  []event.Envelope{testEvent(scope)},
	}
	_, err := ExecuteUnit(context.Background(), connector, scope, unit)
	assertFaultClass(t, err, fault.ChangedIdempotencyContent)
	conn := db.conn(t)
	if conn.commits != 0 || conn.rollbacks != 1 {
		t.Fatalf("changed-content terminal calls: commits=%d rollbacks=%d; want one rollback", conn.commits, conn.rollbacks)
	}
	if indexOfTable(conn.callsCopy(), "executions", "insert") >= 0 {
		t.Error("changed receipt content re-applied the business write")
	}
}

func TestTenantScopedReceiptLookupIsNullSafe(t *testing.T) {
	db := newFakeDB()
	connector := New(db)
	record := testTenantReceipt(t)
	unit := store.Unit{Receipt: &record}
	if _, err := ExecuteUnit(context.Background(), connector, testTenantScope(), unit); err != nil {
		t.Fatalf("ExecuteUnit tenant-scoped receipt: %v", err)
	}
	queries := queryCalls(db.conn(t).callsCopy())
	if len(queries) == 0 {
		t.Fatal("the adapter did not look up the tenant-scoped receipt")
	}
	text := strings.ToLower(queries[0].sql)
	if !strings.Contains(text, "project_id") || !strings.Contains(text, "is not distinct from") {
		t.Fatalf("tenant-scoped receipt lookup must use a NULL-safe project comparison: %s", queries[0].sql)
	}
	if !argContains(queries[0].args, "") && !argContains(queries[0].args, nil) {
		t.Errorf("tenant-scoped receipt lookup args %v do not carry an empty project", queries[0].args)
	}
}

func TestExecuteUnitRefusesCrossScopeEventBeforeTransaction(t *testing.T) {
	db := newFakeDB()
	connector := New(db)
	record := testReceipt(t)
	foreign := testEvent(tenancy.Scope{TenantID: "tenant-2", ProjectID: testProjectID})
	unit := store.Unit{
		Receipt: &record,
		Events:  []event.Envelope{foreign},
	}
	_, err := ExecuteUnit(context.Background(), connector, testProjectScope(), unit)
	assertFaultClass(t, err, fault.Forbidden)
	if got := db.beginCount(); got != 0 {
		t.Fatalf("a cross-scope event opened %d transactions", got)
	}
}

func TestExecuteUnitRefusesInvalidUnitBeforeTransaction(t *testing.T) {
	invalidEvent := testEvent(testProjectScope())
	invalidEvent.Type = "bogus"
	foreignAudit := testAudit(tenancy.Scope{TenantID: "tenant-2", ProjectID: testProjectID})
	badReceipt := testReceipt(t)
	badReceipt.ContentHash = "not-a-hash"
	cases := []struct {
		name string
		unit store.Unit
		want fault.Class
	}{
		{"unknown write table", store.Unit{Writes: []store.Write{{Table: "not_a_table", ID: "id-1", Insert: true}}}, fault.InvalidInput},
		{"unknown lock table", store.Unit{Locks: []store.Lock{{Table: "not_a_table", IDs: []string{"id-1"}}}}, fault.InvalidInput},
		{"invalid event", store.Unit{Events: []event.Envelope{invalidEvent}}, fault.InvalidInput},
		{"cross-scope audit", store.Unit{Audit: []event.Audit{foreignAudit}}, fault.Forbidden},
		{"malformed receipt", store.Unit{Receipt: &badReceipt}, fault.InvalidInput},
		{"scope column supplied", store.Unit{Writes: []store.Write{{Table: "executions", ID: "exec-2", Insert: true, Values: map[string]any{"tenant_id": "tenant-2"}}}}, fault.InvalidInput},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			db := newFakeDB()
			connector := New(db)
			_, err := ExecuteUnit(context.Background(), connector, testProjectScope(), testCase.unit)
			assertFaultClass(t, err, testCase.want)
			if got := db.beginCount(); got != 0 {
				t.Fatalf("%s opened %d transactions before validation", testCase.name, got)
			}
		})
	}
}

func TestExecuteUnitOrdersLocksAndWritesByDeclaredLockOrder(t *testing.T) {
	db := newFakeDB()
	connector := New(db)
	scope := testProjectScope()
	unit := store.Unit{
		Locks: []store.Lock{
			{Table: "events", IDs: []string{"event-1"}},
			{Table: "cleanup", IDs: []string{testExecutionID}},
			{Table: "actions", IDs: []string{"action-1"}},
			{Table: "executions", IDs: []string{testExecutionID}},
		},
		Writes: []store.Write{
			{Table: "actions", ID: "action-1", ExpectVersion: 1, Values: map[string]any{"state": "running"}},
			{Table: "executions", ID: testExecutionID, ExpectVersion: 1, Values: map[string]any{"state": "running"}},
		},
	}
	if _, err := ExecuteUnit(context.Background(), connector, scope, unit); err != nil {
		t.Fatalf("ExecuteUnit: %v", err)
	}
	calls := db.conn(t).callsCopy()
	executionsLock := indexOfTable(calls, "executions", "lock")
	actionsLock := indexOfTable(calls, "actions", "lock")
	cleanupLock := indexOfTable(calls, "cleanup", "lock")
	eventsLock := indexOfTable(calls, "events", "lock")
	if executionsLock < 0 || actionsLock < 0 || cleanupLock < 0 || eventsLock < 0 {
		t.Fatalf("missing locks: executions=%d actions=%d cleanup=%d events=%d", executionsLock, actionsLock, cleanupLock, eventsLock)
	}
	if !(executionsLock < actionsLock && actionsLock < cleanupLock && cleanupLock < eventsLock) {
		t.Error("locks were not acquired in the declared schema lock order")
	}
	executionsWrite := indexOfTable(calls, "executions", "update")
	actionsWrite := indexOfTable(calls, "actions", "update")
	if executionsWrite < 0 || actionsWrite < 0 {
		t.Fatalf("missing writes: executions=%d actions=%d", executionsWrite, actionsWrite)
	}
	if !(executionsWrite < actionsWrite) {
		t.Error("writes were not applied in the declared schema lock order")
	}
	if !(eventsLock < executionsWrite) {
		t.Error("a write was issued before all declared locks were acquired")
	}
}

func TestExecuteUnitPlacesReceiptAfterBusinessWrites(t *testing.T) {
	db := newFakeDB()
	connector := New(db)
	scope := testProjectScope()
	record := testReceipt(t)
	unit := store.Unit{
		Writes: []store.Write{
			{Table: "verifications", ID: "verification-1", Insert: true, Values: unitValues("verifications")},
			{Table: "executions", ID: testExecutionID, ExpectVersion: 1, Values: map[string]any{"state": "running"}},
		},
		Receipt: &record,
		Audit:   []event.Audit{testAudit(scope)},
		Events:  []event.Envelope{testEvent(scope)},
	}
	if _, err := ExecuteUnit(context.Background(), connector, scope, unit); err != nil {
		t.Fatalf("ExecuteUnit: %v", err)
	}
	calls := db.conn(t).callsCopy()
	executionsWrite := indexOfTable(calls, "executions", "update")
	verificationsWrite := indexOfTable(calls, "verifications", "insert")
	receiptInsert := indexOfTable(calls, "receipts", "insert")
	auditInsert := indexOfTable(calls, "audit", "insert")
	eventInsert := indexOfTable(calls, "events", "insert")
	if executionsWrite < 0 || verificationsWrite < 0 || receiptInsert < 0 || auditInsert < 0 || eventInsert < 0 {
		t.Fatalf("missing unit records: executions=%d verifications=%d receipt=%d audit=%d event=%d",
			executionsWrite, verificationsWrite, receiptInsert, auditInsert, eventInsert)
	}
	if !(executionsWrite < verificationsWrite && verificationsWrite < receiptInsert) {
		t.Error("the receipt was not placed after every earlier commit-unit table")
	}
	if !(receiptInsert < auditInsert && auditInsert < eventInsert) {
		t.Error("audit/event intent was not placed after the receipt")
	}
}

func TestExecuteUnitCoversEveryC01CommitUnit(t *testing.T) {
	for _, unit := range c01Units() {
		t.Run(unit.name, func(t *testing.T) {
			db := newFakeDB()
			connector := New(db)
			scope := testProjectScope()
			record := testReceipt(t)
			if unit.tenantScoped {
				scope = testTenantScope()
				record = testTenantReceipt(t)
			}
			writes := make([]store.Write, 0, len(unit.tables))
			for _, table := range unit.tables {
				// A root row's identity is the scope's own tenant/project
				// identity; every other table uses a stable fixture id.
				id := table + "-1"
				switch table {
				case "tenants":
					id = string(scope.TenantID)
				case "projects":
					id = string(scope.ProjectID)
					if id == "" {
						id = "project-1"
					}
				}
				writes = append(writes, store.Write{Table: table, ID: id, Insert: true, Values: unitValues(table)})
			}
			commit := store.Unit{
				Writes:  writes,
				Receipt: &record,
				Audit:   []event.Audit{testAudit(scope)},
				Events:  []event.Envelope{testEvent(scope)},
			}
			result, err := ExecuteUnit(context.Background(), connector, scope, commit)
			if err != nil {
				t.Fatalf("ExecuteUnit %s: %v", unit.name, err)
			}
			if result.Replayed {
				t.Fatalf("%s was reported as a replay", unit.name)
			}
			conn := db.conn(t)
			if conn.commits != 1 || conn.rollbacks != 0 {
				t.Fatalf("%s terminal calls: commits=%d rollbacks=%d", unit.name, conn.commits, conn.rollbacks)
			}
			calls := conn.callsCopy()
			for _, table := range unit.tables {
				if indexOfTable(calls, table, "insert") < 0 {
					t.Errorf("%s did not write table %q", unit.name, table)
				}
			}
			if indexOfTable(calls, "receipts", "insert") < 0 {
				t.Errorf("%s did not commit its receipt", unit.name)
			}
			if indexOfTable(calls, "audit", "insert") < 0 {
				t.Errorf("%s did not commit its audit row", unit.name)
			}
			if indexOfTable(calls, "events", "insert") < 0 {
				t.Errorf("%s did not commit its event intent", unit.name)
			}
		})
	}
}

type c01Unit struct {
	name         string
	tables       []string
	tenantScoped bool
}

func c01Units() []c01Unit {
	return []c01Unit{
		{name: "tenant create", tables: []string{"tenants"}, tenantScoped: true},
		{name: "tenant key rotation", tables: []string{"tenants"}, tenantScoped: true},
		{name: "execution transition", tables: []string{"executions", "actions"}},
		{name: "request activation", tables: []string{"executions", "actions"}},
		{name: "request resolve", tables: []string{"executions", "actions"}},
		{name: "task start", tables: []string{"tasks", "executions", "actions"}},
		{name: "task ask", tables: []string{"tasks", "task_messages", "executions", "actions"}},
		{name: "task reply", tables: []string{"tasks", "task_messages", "executions", "actions"}},
		{name: "task cancel", tables: []string{"tasks", "executions", "actions"}},
		{name: "token mutation", tables: []string{"services"}},
		{name: "binding change", tables: []string{"resource_bindings"}},
		{name: "verified report", tables: []string{"verifications"}},
		{name: "push selection", tables: []string{"pipelines", "resource_bindings"}},
	}
}

// unitValues returns realistic column values for one schema table. The store
// adapter does not interpret them; the fake only proves the row write carries
// the columns and arguments a complete commit unit needs.
func unitValues(table string) map[string]any {
	switch table {
	case "tenants":
		return map[string]any{"name": "acme", "version": uint64(1), "derivation_key": []byte(`{"version":1}`)}
	case "projects":
		return map[string]any{"name": "app", "version": uint64(1)}
	case "memberships":
		return map[string]any{"principal_id": testPrincipalID, "roles": []byte(`["admin"]`), "version": uint64(1)}
	case "services":
		return map[string]any{"name": "service", "state": "enabled", "roles": []byte(`["engineer"]`), "version": uint64(1)}
	case "resource_bindings":
		return map[string]any{
			"resource_id":       testResourceID,
			"service_id":        testServiceID,
			"origins":           []byte(`[]`),
			"operations":        []byte(`[]`),
			"reportable_events": []byte(`[]`),
			"metadata":          []byte(`{}`),
			"version":           uint64(1),
		}
	case "pipelines":
		yaml := []byte("name: build\non: push\n")
		return map[string]any{"name": "build", "yaml": yaml, "yaml_sha256": testDigest(yaml), "version": uint64(1)}
	case "tasks":
		return map[string]any{
			"content_version":   uint64(1),
			"lifecycle_version": uint64(1),
			"state":             "open",
			"initiator_id":      testPrincipalID,
			"title":             "task",
		}
	case "task_messages":
		return map[string]any{"task_id": "task-1", "author_id": testPrincipalID, "at": testTime(), "body": "hello"}
	case "executions":
		return map[string]any{
			"pipeline_id":  "pipeline-1",
			"task_id":      "task-1",
			"principal_id": testPrincipalID,
			"state":        "queued",
			"version":      uint64(1),
			"yaml_sha256":  testDigest([]byte("name: build\non: push\n")),
			"snapshot":     []byte(`{"yaml_sha256":"00"}`),
		}
	case "actions":
		return map[string]any{
			"execution_id":       testExecutionID,
			"index":              int64(0),
			"name":               "action-1",
			"kind":               "container",
			"state":              "pending",
			"version":            uint64(1),
			"timeout_ms":         int64(1000),
			"staged_outputs_set": false,
		}
	case "cleanup":
		return map[string]any{"execution_id": testExecutionID, "state": "pending", "attempts": int64(0)}
	case "verifications":
		return map[string]any{
			"service_id":  testServiceID,
			"actor_id":    testPrincipalID,
			"binding_id":  "binding-1",
			"resource_id": testResourceID,
			"operation":   "git.ref.update",
			"subject":     []byte(`{}`),
			"result":      "approved",
			"approved":    true,
		}
	default:
		return map[string]any{}
	}
}

func TestExecuteUnitConcurrentOneWinner(t *testing.T) {
	db := newFakeDB()
	var counterMu sync.Mutex
	winners := 0
	db.script.setExec(func(sql string, args []any) (store.Result, error) {
		if indexOfTable([]recordedCall{{kind: "exec", sql: sql}}, "executions", "update") == 0 {
			counterMu.Lock()
			winners++
			winner := winners == 1
			counterMu.Unlock()
			if winner {
				return store.Result{RowsAffected: 1}, nil
			}
			return store.Result{RowsAffected: 0}, nil
		}
		return store.Result{RowsAffected: 1}, nil
	})
	connector := New(db)
	record := testReceipt(t)
	unit := store.Unit{
		Writes:  []store.Write{{Table: "executions", ID: testExecutionID, ExpectVersion: 1, Values: map[string]any{"state": "running"}}},
		Receipt: &record,
	}

	ready := make(chan struct{}, 2)
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			ready <- struct{}{}
			<-start
			_, err := ExecuteUnit(context.Background(), connector, testProjectScope(), unit)
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
		t.Fatalf("concurrent unit settlement: success=%d conflicts=%d; want exactly one winner", success, conflicts)
	}
	commits, rollbacks := 0, 0
	for _, conn := range db.connsCopy() {
		commits += conn.commits
		rollbacks += conn.rollbacks
	}
	if commits != 1 || rollbacks != 1 {
		t.Fatalf("concurrent settlement terminal calls: commits=%d rollbacks=%d; want one commit and one rollback", commits, rollbacks)
	}
}

func faultClassIs(err error, class fault.Class) bool {
	got, ok := fault.ClassOf(err)
	return ok && got == class
}

func TestExecuteUnitLostResponseReplaysCommittedResult(t *testing.T) {
	db := newFakeDB()
	connector := New(db)
	record := testReceipt(t)
	unit := store.Unit{
		Writes:  []store.Write{{Table: "executions", ID: testExecutionID, Insert: true, Values: unitValues("executions")}},
		Receipt: &record,
		Events:  []event.Envelope{testEvent(testProjectScope())},
	}
	first, err := ExecuteUnit(context.Background(), connector, testProjectScope(), unit)
	if err != nil {
		t.Fatalf("first ExecuteUnit: %v", err)
	}
	if first.Replayed {
		t.Fatal("the first commit was reported as a replay")
	}

	// Simulate a crash after commit and before the caller saw the response:
	// the durable receipt is the only surviving evidence.
	db.script.setQuery(queryByTable(map[string]map[string]any{"receipts": receiptFixture(t, record)}))
	second, err := ExecuteUnit(context.Background(), connector, testProjectScope(), unit)
	if err != nil {
		t.Fatalf("retry ExecuteUnit: %v", err)
	}
	if !second.Replayed {
		t.Fatal("a retry after a lost response did not replay the committed result")
	}
	if second.Receipt.ExecutionID != first.Receipt.ExecutionID {
		t.Errorf("retry execution = %q; want the original %q", second.Receipt.ExecutionID, first.Receipt.ExecutionID)
	}
	if string(second.Receipt.Result) != string(first.Receipt.Result) {
		t.Errorf("retry result = %q; want the original %q", second.Receipt.Result, first.Receipt.Result)
	}
	conns := db.connsCopy()
	if len(conns) != 2 {
		t.Fatalf("retry opened %d connections; want 2", len(conns))
	}
	if indexOfTable(conns[1].callsCopy(), "executions", "insert") >= 0 {
		t.Error("the lost-response retry re-applied the business write")
	}
}

func TestWriteRowRefusesReservedColumnsAndBadVersions(t *testing.T) {
	db := newFakeDB()
	connector := New(db)
	tx, err := connector.Begin(context.Background(), testProjectScope())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	cases := []struct {
		name  string
		write store.Write
	}{
		{"tenant column", store.Write{Table: "executions", ID: "exec-1", ExpectVersion: 1, Values: map[string]any{"tenant_id": "tenant-2"}}},
		{"project column", store.Write{Table: "executions", ID: "exec-1", ExpectVersion: 1, Values: map[string]any{"project_id": "project-2"}}},
		{"id column", store.Write{Table: "executions", ID: "exec-1", ExpectVersion: 1, Values: map[string]any{"id": "other"}}},
		{"version column on update", store.Write{Table: "executions", ID: "exec-1", ExpectVersion: 1, Values: map[string]any{"version": uint64(2)}}},
		{"unknown table", store.Write{Table: "not_a_table", ID: "id-1", Insert: true}},
		{"insert with expected version", store.Write{Table: "executions", ID: "exec-1", Insert: true, ExpectVersion: 2, Values: map[string]any{"state": "queued"}}},
		{"update without expected version", store.Write{Table: "executions", ID: "exec-1", Values: map[string]any{"state": "running"}}},
		{"project table in tenant scope", store.Write{Table: "executions", ID: "exec-1", Insert: true, Values: map[string]any{"state": "queued"}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			scope := testProjectScope()
			if testCase.name == "project table in tenant scope" {
				scope = testTenantScope()
			}
			err := WriteRow(context.Background(), tx, scope, testCase.write)
			assertFaultClass(t, err, fault.InvalidInput)
		})
	}
	if got := len(dataExecCalls(db.conn(t).callsCopy())); got != 0 {
		t.Fatalf("invalid writes issued %d statements", got)
	}

	insert := store.Write{Table: "executions", ID: "exec-2", Insert: true, Values: unitValues("executions")}
	if err := WriteRow(context.Background(), tx, testProjectScope(), insert); err != nil {
		t.Fatalf("an insert may carry the initial version: %v", err)
	}
}

func TestWriteRowReportsVersionConflict(t *testing.T) {
	db := newFakeDB()
	db.script.setExec(func(sql string, args []any) (store.Result, error) {
		return store.Result{RowsAffected: 0}, nil
	})
	connector := New(db)
	tx, err := connector.Begin(context.Background(), testProjectScope())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	err = WriteRow(context.Background(), tx, testProjectScope(), store.Write{
		Table:         "executions",
		ID:            testExecutionID,
		ExpectVersion: 7,
		Values:        map[string]any{"state": "running"},
	})
	assertFaultClass(t, err, fault.VersionConflict)
	calls := dataExecCalls(db.conn(t).callsCopy())
	if len(calls) != 1 {
		t.Fatalf("stale version issued %d statements; want 1", len(calls))
	}
	if !strings.Contains(strings.ToLower(calls[0].sql), "version") || !argContainsNumber(calls[0].args, 7) {
		t.Errorf("versioned update did not carry the expected version: %s %v", calls[0].sql, calls[0].args)
	}
}

func TestWriteRowStaleServiceAuthorityVersion(t *testing.T) {
	db := newFakeDB()
	db.script.setExec(func(sql string, args []any) (store.Result, error) {
		return store.Result{RowsAffected: 0}, nil
	})
	connector := New(db)
	tx, err := connector.Begin(context.Background(), testTenantScope())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	err = WriteRow(context.Background(), tx, testTenantScope(), store.Write{
		Table:         "services",
		ID:            testServiceID,
		ExpectVersion: 3,
		Values:        map[string]any{"state": "disabled"},
	})
	assertFaultClass(t, err, fault.VersionConflict)
}

func TestWriteRowTenantRootTableDoesNotInjectTenantColumn(t *testing.T) {
	db := newFakeDB()
	connector := New(db)
	tx, err := connector.Begin(context.Background(), testTenantScope())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := WriteRow(context.Background(), tx, testTenantScope(), store.Write{
		Table:  "tenants",
		ID:     testTenantID,
		Insert: true,
		Values: map[string]any{"name": "acme", "version": uint64(1), "derivation_key": []byte(`{"version":1}`)},
	}); err != nil {
		t.Fatalf("WriteRow tenants: %v", err)
	}
	calls := dataExecCalls(db.conn(t).callsCopy())
	if len(calls) != 1 {
		t.Fatalf("tenant insert issued %d statements", len(calls))
	}
	if strings.Contains(strings.ToLower(calls[0].sql), "tenant_id") {
		t.Errorf("the tenants table has no tenant_id column: %s", calls[0].sql)
	}
	if !argContains(calls[0].args, testTenantID) {
		t.Errorf("tenant insert args %v do not carry the tenant identity", calls[0].args)
	}
}

func TestWriteRowIsDeterministicAndParameterized(t *testing.T) {
	db := newFakeDB()
	connector := New(db)
	tx, err := connector.Begin(context.Background(), testProjectScope())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	marker := "value'; DROP TABLE tasks; --"
	write := store.Write{
		Table:  "pipelines",
		ID:     "pipeline-1",
		Insert: true,
		Values: map[string]any{
			"name":        marker,
			"yaml":        []byte("name: build\n"),
			"yaml_sha256": testDigest([]byte("name: build\n")),
			"version":     uint64(1),
		},
	}
	if err := WriteRow(context.Background(), tx, testProjectScope(), write); err != nil {
		t.Fatalf("first WriteRow: %v", err)
	}
	if err := WriteRow(context.Background(), tx, testProjectScope(), write); err != nil {
		t.Fatalf("second WriteRow: %v", err)
	}
	calls := dataExecCalls(db.conn(t).callsCopy())
	if len(calls) != 2 {
		t.Fatalf("two writes issued %d statements", len(calls))
	}
	if calls[0].sql != calls[1].sql {
		t.Errorf("SQL is not deterministic across calls:\n%s\n%s", calls[0].sql, calls[1].sql)
	}
	if !equalAny(calls[0].args, calls[1].args) {
		t.Errorf("arguments are not deterministic across calls:\n%v\n%v", calls[0].args, calls[1].args)
	}
	if strings.Contains(strings.ToLower(calls[0].sql), "drop table") {
		t.Errorf("a value was interpolated into SQL: %s", calls[0].sql)
	}
	if !argContains(calls[0].args, marker) {
		t.Errorf("the value did not travel as an argument: %v", calls[0].args)
	}
}

func TestLockRowsOrdersTablesAndRefusesUnknown(t *testing.T) {
	db := newFakeDB()
	connector := New(db)
	tx, err := connector.Begin(context.Background(), testProjectScope())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	locks := []store.Lock{
		{Table: "events", IDs: []string{"event-1"}},
		{Table: "cleanup", IDs: []string{testExecutionID}},
		{Table: "actions", IDs: []string{"action-1"}},
		{Table: "executions", IDs: []string{testExecutionID}},
	}
	if err := LockRows(context.Background(), tx, testProjectScope(), locks); err != nil {
		t.Fatalf("LockRows: %v", err)
	}
	calls := dataExecCalls(db.conn(t).callsCopy())
	order := make([]string, 0, len(calls))
	for _, call := range calls {
		text := strings.ToLower(call.sql)
		for _, table := range []string{"executions", "actions", "cleanup", "events"} {
			if regexp.MustCompile(`(?:^|[^a-z0-9_])` + table + `(?:[^a-z0-9_]|$)`).MatchString(text) {
				order = append(order, table)
				break
			}
		}
	}
	want := []string{"executions", "actions", "cleanup", "events"}
	if !equalStrings(order, want) {
		t.Fatalf("lock order = %v; want %v", order, want)
	}
	for _, call := range calls {
		if !strings.Contains(strings.ToLower(call.sql), "for update") {
			t.Errorf("lock statement does not request a row lock: %s", call.sql)
		}
	}

	if err := LockRows(context.Background(), tx, testProjectScope(), []store.Lock{{Table: "not_a_table", IDs: []string{"id-1"}}}); err == nil {
		t.Fatal("LockRows accepted an unknown table")
	} else {
		assertFaultClass(t, err, fault.InvalidInput)
	}
	if err := LockRows(context.Background(), tx, testProjectScope(), nil); err != nil {
		t.Fatalf("empty LockRows: %v", err)
	}
}

func TestLockRowsTenantScopedTableOmitsProjectPredicate(t *testing.T) {
	db := newFakeDB()
	connector := New(db)
	tx, err := connector.Begin(context.Background(), testTenantScope())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := LockRows(context.Background(), tx, testTenantScope(), []store.Lock{{Table: "services", IDs: []string{testServiceID}}}); err != nil {
		t.Fatalf("LockRows services: %v", err)
	}
	calls := dataExecCalls(db.conn(t).callsCopy())
	if len(calls) != 1 {
		t.Fatalf("tenant-scoped lock issued %d statements", len(calls))
	}
	text := strings.ToLower(calls[0].sql)
	if !strings.Contains(text, "services") || !strings.Contains(text, "for update") {
		t.Fatalf("lock statement = %s", calls[0].sql)
	}
	if regexp.MustCompile(`project_id\s*=\s*\$`).MatchString(text) {
		t.Errorf("tenant-scoped lock filters on a project value: %s", calls[0].sql)
	}
	if !argContains(calls[0].args, testTenantID) {
		t.Errorf("tenant-scoped lock args %v do not carry the tenant", calls[0].args)
	}
}

func TestExecuteUnitUnknownTableIsRejectedBeforeAnyStatement(t *testing.T) {
	db := newFakeDB()
	connector := New(db)
	_, err := ExecuteUnit(context.Background(), connector, testProjectScope(), store.Unit{
		Writes: []store.Write{{Table: "not_a_table", ID: "id-1", Insert: true}},
	})
	assertFaultClass(t, err, fault.InvalidInput)
	if got := db.beginCount(); got != 0 {
		t.Fatalf("invalid unit opened %d transactions", got)
	}
}

func TestExecuteUnitRejectsCanceledContextBeforeAnyStatement(t *testing.T) {
	db := newFakeDB()
	connector := New(db)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ExecuteUnit(ctx, connector, testProjectScope(), store.Unit{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ExecuteUnit canceled = %v; want context.Canceled", err)
	}
	if got := db.beginCount(); got != 0 {
		t.Fatalf("canceled unit opened %d transactions", got)
	}
}

func TestUnitTableNamesAreSchemaTables(t *testing.T) {
	known := make(map[string]bool, len(store.SchemaDesign().Tables))
	for _, table := range store.SchemaDesign().Tables {
		known[table.Name] = true
	}
	for _, unit := range c01Units() {
		for _, table := range unit.tables {
			if !known[table] {
				t.Errorf("commit unit %q names unknown table %q", unit.name, table)
			}
		}
	}
}
