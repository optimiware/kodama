// Regression coverage: commit-boundary authority guards (version/column
// assertions inside the unit transaction, tenant-wide guards from a project
// scope, missing-row refusal, task lifecycle/content guards) and root-row scope
// binding for tenants/projects.
package postgres

import (
	"context"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func TestGuardedLockRefusesStaleAuthorityVersion(t *testing.T) {
	db := newFakeDB()
	// The service row exists but no longer carries version 3.
	db.script.setQuery(func(sql string, args []any) (store.Rows, error) {
		return &fakeRows{index: -1}, nil
	})
	connector := New(db)
	scope := testTenantScope()
	unit := store.Unit{
		Locks: []store.Lock{{Table: "services", IDs: []string{testServiceID}, ExpectVersion: 3}},
		Writes: []store.Write{{
			Table:         "services",
			ID:            testServiceID,
			ExpectVersion: 3,
			Values:        map[string]any{"state": "disabled"},
		}},
	}
	_, err := ExecuteUnit(context.Background(), connector, scope, unit)
	assertFaultClass(t, err, fault.VersionConflict)

	conn := db.conn(t)
	if conn.commits != 0 || conn.rollbacks != 1 {
		t.Fatalf("stale guard terminal calls: commits=%d rollbacks=%d; want one rollback", conn.commits, conn.rollbacks)
	}
	calls := conn.callsCopy()
	if indexOfTable(calls, "services", "update") >= 0 {
		t.Error("a stale authority guard still applied its business write")
	}
	guards := queryCalls(calls)
	if len(guards) != 1 {
		t.Fatalf("stale guard issued %d guard queries; want 1", len(guards))
	}
	text := strings.ToLower(guards[0].sql)
	if !strings.Contains(text, "for update") || !strings.Contains(text, "version = $") {
		t.Errorf("guard query does not assert the expected version under a row lock: %s", guards[0].sql)
	}
	if !argContains(guards[0].args, testServiceID) || !argContainsNumber(guards[0].args, 3) {
		t.Errorf("guard args %v do not carry the row id and expected version", guards[0].args)
	}
}

func TestGuardedLockRefusesMissingAuthorityRow(t *testing.T) {
	db := newFakeDB()
	// The membership row is gone; a column guard alone must still fail closed.
	db.script.setQuery(func(sql string, args []any) (store.Rows, error) {
		return &fakeRows{index: -1}, nil
	})
	connector := New(db)
	scope := testProjectScope()
	unit := store.Unit{
		Locks: []store.Lock{{
			Table:  "memberships",
			IDs:    []string{"membership-1"},
			Expect: map[string]any{"principal_id": testPrincipalID},
		}},
	}
	_, err := ExecuteUnit(context.Background(), connector, scope, unit)
	assertFaultClass(t, err, fault.VersionConflict)

	conn := db.conn(t)
	if conn.commits != 0 || conn.rollbacks != 1 {
		t.Fatalf("missing guard terminal calls: commits=%d rollbacks=%d; want one rollback", conn.commits, conn.rollbacks)
	}
	guards := queryCalls(conn.callsCopy())
	if len(guards) != 1 {
		t.Fatalf("missing-row guard issued %d guard queries; want 1", len(guards))
	}
	text := strings.ToLower(guards[0].sql)
	if !strings.Contains(text, "principal_id = $") {
		t.Errorf("guard query does not assert the guarded column: %s", guards[0].sql)
	}
	if !argContains(guards[0].args, testPrincipalID) {
		t.Errorf("guard args %v do not carry the guarded principal", guards[0].args)
	}
}

func TestGuardedLockTargetsTenantWideRowFromProjectScope(t *testing.T) {
	db := newFakeDB()
	db.script.setQuery(func(sql string, args []any) (store.Rows, error) {
		return singleRow("services", sql, map[string]any{"id": testServiceID}), nil
	})
	connector := New(db)
	scope := testProjectScope()
	unit := store.Unit{
		Locks: []store.Lock{{
			Table:         "services",
			IDs:           []string{testServiceID},
			ExpectVersion: 2,
			TenantWide:    true,
		}},
	}
	if _, err := ExecuteUnit(context.Background(), connector, scope, unit); err != nil {
		t.Fatalf("ExecuteUnit tenant-wide guard: %v", err)
	}
	conn := db.conn(t)
	if conn.commits != 1 || conn.rollbacks != 0 {
		t.Fatalf("tenant-wide guard terminal calls: commits=%d rollbacks=%d", conn.commits, conn.rollbacks)
	}
	calls := conn.callsCopy()

	locks := dataExecCalls(calls)
	if len(locks) != 1 {
		t.Fatalf("tenant-wide guard issued %d lock statements; want 1", len(locks))
	}
	if text := strings.ToLower(locks[0].sql); !strings.Contains(text, "project_id is null") {
		t.Errorf("the lock did not target the tenant-wide row: %s", locks[0].sql)
	}
	guards := queryCalls(calls)
	if len(guards) != 1 {
		t.Fatalf("tenant-wide guard issued %d guard queries; want 1", len(guards))
	}
	text := strings.ToLower(guards[0].sql)
	if !strings.Contains(text, "project_id is null") {
		t.Errorf("the guard did not target the tenant-wide row: %s", guards[0].sql)
	}
	if strings.Contains(text, "project_id = $") {
		t.Errorf("the tenant-wide guard widened to a project equality: %s", guards[0].sql)
	}
	if !argContains(guards[0].args, testTenantID) || !argContainsNumber(guards[0].args, 2) {
		t.Errorf("guard args %v do not carry the tenant and expected version", guards[0].args)
	}
	if argContains(guards[0].args, testProjectID) {
		t.Errorf("tenant-wide guard args %v carry the project identity", guards[0].args)
	}
}

func TestTaskUpdateRequiresExpectVersionGuard(t *testing.T) {
	db := newFakeDB()
	connector := New(db)
	scope := testProjectScope()
	tx, err := connector.Begin(context.Background(), scope)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if err := WriteRow(context.Background(), tx, scope, store.Write{
		Table:  "tasks",
		ID:     "task-1",
		Values: map[string]any{"state": "waiting"},
	}); err == nil {
		t.Fatal("a task update without an explicit expected-column guard was accepted")
	} else {
		assertFaultClass(t, err, fault.InvalidInput)
	}
	if got := len(dataExecCalls(db.conn(t).callsCopy())); got != 0 {
		t.Fatalf("unguarded task updates issued %d statements", got)
	}

	if err := WriteRow(context.Background(), tx, scope, store.Write{
		Table:  "tasks",
		ID:     "task-1",
		Expect: map[string]any{"lifecycle_version": uint64(1)},
		Values: map[string]any{"state": "waiting"},
	}); err != nil {
		t.Fatalf("lifecycle-version task update: %v", err)
	}
	if err := WriteRow(context.Background(), tx, scope, store.Write{
		Table:  "tasks",
		ID:     "task-1",
		Expect: map[string]any{"content_version": uint64(2)},
		Values: map[string]any{"title": "edited"},
	}); err != nil {
		t.Fatalf("content-version task update: %v", err)
	}
	if err := WriteRow(context.Background(), tx, scope, store.Write{
		Table:  "tasks",
		ID:     "task-1",
		Expect: map[string]any{"question_version": uint64(3)},
		Values: map[string]any{"question_body": "answered"},
	}); err != nil {
		t.Fatalf("question-version task update: %v", err)
	}
	calls := dataExecCalls(db.conn(t).callsCopy())
	if len(calls) != 3 {
		t.Fatalf("guarded task updates issued %d statements; want 3", len(calls))
	}
	if text := strings.ToLower(calls[0].sql); !strings.Contains(text, "lifecycle_version = $") {
		t.Errorf("lifecycle guard did not travel as a predicate: %s", calls[0].sql)
	}
	if !argContainsNumber(calls[0].args, 1) {
		t.Errorf("lifecycle guard args %v do not carry the expected version", calls[0].args)
	}
}

func TestGuardsRunAfterLocksAndBeforeWrites(t *testing.T) {
	db := newFakeDB()
	db.script.setQuery(queryByTable(map[string]map[string]any{
		"executions": {"id": testExecutionID},
		"actions":    {"id": "action-1"},
	}))
	connector := New(db)
	scope := testProjectScope()
	unit := store.Unit{
		Locks: []store.Lock{
			{Table: "actions", IDs: []string{"action-1"}, ExpectVersion: 2},
			{Table: "executions", IDs: []string{testExecutionID}, ExpectVersion: 4},
		},
		Writes: []store.Write{
			{Table: "actions", ID: "action-1", ExpectVersion: 2, Values: map[string]any{"state": "running"}},
			{Table: "executions", ID: testExecutionID, ExpectVersion: 4, Values: map[string]any{"state": "running"}},
		},
	}
	if _, err := ExecuteUnit(context.Background(), connector, scope, unit); err != nil {
		t.Fatalf("ExecuteUnit: %v", err)
	}
	calls := db.conn(t).callsCopy()

	executionsLock := indexOfTable(calls, "executions", "lock")
	actionsLock := indexOfTable(calls, "actions", "lock")
	executionsWrite := indexOfTable(calls, "executions", "update")
	actionsWrite := indexOfTable(calls, "actions", "update")
	if executionsLock < 0 || actionsLock < 0 || executionsWrite < 0 || actionsWrite < 0 {
		t.Fatalf("missing calls: executionsLock=%d actionsLock=%d executionsWrite=%d actionsWrite=%d",
			executionsLock, actionsLock, executionsWrite, actionsWrite)
	}
	if !(executionsLock < actionsLock) {
		t.Error("locks were not acquired in the declared schema lock order")
	}

	guards := queryCalls(calls)
	if len(guards) != 2 {
		t.Fatalf("two guarded locks issued %d guard queries; want 2", len(guards))
	}
	guardIndex := func(table string) int {
		for index, call := range calls {
			if call.kind == "query" && strings.Contains(strings.ToLower(call.sql), "from "+table) {
				return index
			}
		}
		return -1
	}
	executionsGuard := guardIndex("executions")
	actionsGuard := guardIndex("actions")
	if executionsGuard < 0 || actionsGuard < 0 {
		t.Fatalf("missing guard queries: executions=%d actions=%d", executionsGuard, actionsGuard)
	}
	if !(actionsLock < executionsGuard) {
		t.Error("a guard ran before every declared lock was acquired")
	}
	if !(executionsGuard < actionsGuard) {
		t.Error("guards were not issued in the declared schema lock order")
	}
	if !(actionsGuard < executionsWrite && actionsGuard < actionsWrite) {
		t.Error("a guard ran after a business write")
	}
	if text := strings.ToLower(calls[executionsGuard].sql); !strings.Contains(text, "version = $") ||
		!argContainsNumber(calls[executionsGuard].args, 4) {
		t.Errorf("executions guard did not assert version 4: %s %v", calls[executionsGuard].sql, calls[executionsGuard].args)
	}
}

func TestLockRowsRefusesMissingAndStaleRows(t *testing.T) {
	t.Run("plain lock missing row", func(t *testing.T) {
		db := newFakeDB()
		db.script.setExec(func(sql string, args []any) (store.Result, error) {
			if strings.Contains(strings.ToLower(sql), "for update") {
				return store.Result{RowsAffected: 0}, nil
			}
			return store.Result{RowsAffected: 1}, nil
		})
		connector := New(db)
		tx, err := connector.Begin(context.Background(), testProjectScope())
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		err = LockRows(context.Background(), tx, testProjectScope(), []store.Lock{{Table: "executions", IDs: []string{testExecutionID}}})
		assertFaultClass(t, err, fault.VersionConflict)
	})

	t.Run("guarded lock stale row", func(t *testing.T) {
		db := newFakeDB()
		db.script.setQuery(func(sql string, args []any) (store.Rows, error) {
			return &fakeRows{index: -1}, nil
		})
		connector := New(db)
		tx, err := connector.Begin(context.Background(), testProjectScope())
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		err = LockRows(context.Background(), tx, testProjectScope(), []store.Lock{{
			Table:         "executions",
			IDs:           []string{testExecutionID},
			ExpectVersion: 2,
		}})
		assertFaultClass(t, err, fault.VersionConflict)
	})
}

func TestGuardedLockRefusesInvalidGuardSurface(t *testing.T) {
	cases := []struct {
		name  string
		scope tenancy.Scope
		lock  store.Lock
	}{
		{"version guard on a versionless table", testProjectScope(), store.Lock{Table: "tasks", IDs: []string{"task-1"}, ExpectVersion: 2}},
		{"guard names several rows", testProjectScope(), store.Lock{Table: "executions", IDs: []string{testExecutionID, "execution-2"}, ExpectVersion: 2}},
		{"tenant-wide guard on a root table", testTenantScope(), store.Lock{Table: "tenants", IDs: []string{testTenantID}, TenantWide: true}},
		{"tenant-wide guard on a project-required table", testProjectScope(), store.Lock{Table: "executions", IDs: []string{testExecutionID}, TenantWide: true}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			db := newFakeDB()
			connector := New(db)
			_, err := ExecuteUnit(context.Background(), connector, testCase.scope, store.Unit{Locks: []store.Lock{testCase.lock}})
			assertFaultClass(t, err, fault.InvalidInput)
			if got := db.beginCount(); got != 0 {
				t.Fatalf("%s opened %d transactions before refusal", testCase.name, got)
			}
		})
	}
}

func TestRootRowsAreBoundToTheUnitScope(t *testing.T) {
	tenantInsert := map[string]any{"name": "acme", "version": uint64(1), "derivation_key": []byte(`{"version":1}`)}
	projectInsert := map[string]any{"name": "app", "version": uint64(1)}

	refused := []struct {
		name  string
		scope tenancy.Scope
		unit  store.Unit
	}{
		{"cross-tenant tenant write", testTenantScope(), store.Unit{Writes: []store.Write{{Table: "tenants", ID: "tenant-2", Insert: true, Values: tenantInsert}}}},
		{"cross-tenant tenant update", testTenantScope(), store.Unit{Writes: []store.Write{{Table: "tenants", ID: "tenant-2", ExpectVersion: 1, Values: map[string]any{"name": "other"}}}}},
		{"cross-tenant tenant lock", testTenantScope(), store.Unit{Locks: []store.Lock{{Table: "tenants", IDs: []string{"tenant-2"}}}}},
		{"cross-project project write", testProjectScope(), store.Unit{Writes: []store.Write{{Table: "projects", ID: "project-2", Insert: true, Values: projectInsert}}}},
		{"cross-project project update", testProjectScope(), store.Unit{Writes: []store.Write{{Table: "projects", ID: "project-2", ExpectVersion: 1, Values: map[string]any{"name": "other"}}}}},
		{"cross-project project lock", testProjectScope(), store.Unit{Locks: []store.Lock{{Table: "projects", IDs: []string{"project-2"}}}}},
	}
	for _, testCase := range refused {
		t.Run(testCase.name, func(t *testing.T) {
			db := newFakeDB()
			connector := New(db)
			_, err := ExecuteUnit(context.Background(), connector, testCase.scope, testCase.unit)
			assertFaultClass(t, err, fault.Forbidden)
			if got := db.beginCount(); got != 0 {
				t.Fatalf("%s opened %d transactions before refusal", testCase.name, got)
			}
		})
	}

	allowed := []struct {
		name  string
		scope tenancy.Scope
		unit  store.Unit
	}{
		{"own tenant row from tenant scope", testTenantScope(), store.Unit{Writes: []store.Write{{Table: "tenants", ID: testTenantID, ExpectVersion: 1, Values: map[string]any{"name": "acme2"}}}}},
		{"own tenant row from project scope", testProjectScope(), store.Unit{Writes: []store.Write{{Table: "tenants", ID: testTenantID, ExpectVersion: 1, Values: map[string]any{"name": "acme2"}}}}},
		{"own project row from project scope", testProjectScope(), store.Unit{Writes: []store.Write{{Table: "projects", ID: testProjectID, ExpectVersion: 1, Values: map[string]any{"name": "app2"}}}}},
		{"own tenant project row from tenant scope", testTenantScope(), store.Unit{Writes: []store.Write{{Table: "projects", ID: "project-9", Insert: true, Values: projectInsert}}}},
		{"own tenant lock", testTenantScope(), store.Unit{Locks: []store.Lock{{Table: "tenants", IDs: []string{testTenantID}}}}},
		{"own project lock", testProjectScope(), store.Unit{Locks: []store.Lock{{Table: "projects", IDs: []string{testProjectID}}}}},
		{"tenant project lock from tenant scope", testTenantScope(), store.Unit{Locks: []store.Lock{{Table: "projects", IDs: []string{"project-9"}}}}},
		{"tenant root lock from project scope", testProjectScope(), store.Unit{Locks: []store.Lock{{Table: "tenants", IDs: []string{testTenantID}}}}},
	}
	for _, testCase := range allowed {
		t.Run(testCase.name, func(t *testing.T) {
			db := newFakeDB()
			connector := New(db)
			if _, err := ExecuteUnit(context.Background(), connector, testCase.scope, testCase.unit); err != nil {
				t.Fatalf("%s: %v", testCase.name, err)
			}
			calls := dataExecCalls(db.conn(t).callsCopy())
			if len(calls) == 0 {
				t.Fatalf("%s issued no statement", testCase.name)
			}
			if !argContains(calls[0].args, testTenantID) {
				t.Errorf("%s args %v do not carry the tenant scope", testCase.name, calls[0].args)
			}
		})
	}
}

func TestRootRowRefusalHappensBeforeAnyStatement(t *testing.T) {
	db := newFakeDB()
	connector := New(db)
	scope := testProjectScope()
	tx, err := connector.Begin(context.Background(), scope)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if err := WriteRow(context.Background(), tx, scope, store.Write{
		Table:  "projects",
		ID:     "project-2",
		Insert: true,
		Values: map[string]any{"name": "other", "version": uint64(1)},
	}); err == nil {
		t.Fatal("a cross-project root write was accepted")
	} else {
		assertFaultClass(t, err, fault.Forbidden)
	}
	if err := LockRows(context.Background(), tx, scope, []store.Lock{{Table: "projects", IDs: []string{"project-2"}}}); err == nil {
		t.Fatal("a cross-project root lock was accepted")
	} else {
		assertFaultClass(t, err, fault.Forbidden)
	}
	if got := len(dataExecCalls(db.conn(t).callsCopy())); got != 0 {
		t.Fatalf("refused root operations issued %d statements", got)
	}
}

func TestWriteExpectNilRendersIsNullAndMatchesInEmulator(t *testing.T) {
	t.Run("render update and delete with nil expectation", func(t *testing.T) {
		db := newFakeDB()
		connector := New(db)
		scope := testProjectScope()
		tx, err := connector.Begin(context.Background(), scope)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()

		if err := WriteRow(context.Background(), tx, scope, store.Write{
			Table: "tasks",
			ID:    "task-1",
			Expect: map[string]any{
				"current_execution_id": nil,
				"lifecycle_version":    uint64(1),
			},
			Values: map[string]any{
				"current_execution_id": testExecutionID,
				"lifecycle_version":    uint64(2),
			},
		}); err != nil {
			t.Fatalf("WriteRow update with nil Expect: %v", err)
		}

		if err := WriteRow(context.Background(), tx, scope, store.Write{
			Table:  "tasks",
			ID:     "task-1",
			Delete: true,
			Expect: map[string]any{
				"current_execution_id": nil,
				"lifecycle_version":    uint64(2),
			},
		}); err != nil {
			t.Fatalf("WriteRow delete with nil Expect: %v", err)
		}

		calls := dataExecCalls(db.conn(t).callsCopy())
		if len(calls) != 2 {
			t.Fatalf("writes issued %d statements; want 2", len(calls))
		}
		for index, call := range calls {
			text := strings.ToLower(call.sql)
			if !strings.Contains(text, "current_execution_id is null") {
				t.Errorf("call %d missing IS NULL guard: %s", index, call.sql)
			}
			if _, predicates, ok := strings.Cut(text, " where "); !ok || strings.Contains(predicates, "current_execution_id = $") {
				t.Errorf("call %d bound a parameter for a nil expectation: %s", index, call.sql)
			}
			if !strings.Contains(text, "lifecycle_version = $") {
				t.Errorf("call %d missing non-nil expectation predicate: %s", index, call.sql)
			}
			for argIndex, arg := range call.args {
				if arg == nil {
					t.Errorf("call %d arg %d is nil (%v); nil Expect must not bind a parameter", index, argIndex, call.args)
				}
			}
		}
	})

	t.Run("emulator sql equality rejects nil while update and delete Expect nil match", func(t *testing.T) {
		ctx := context.Background()
		db := newEmuDB("kodama_runtime", testTime())
		scope := testProjectScope()
		db.seed("tasks", map[string]any{
			"id":                   "task-null",
			"tenant_id":            testTenantID,
			"project_id":           testProjectID,
			"state":                "open",
			"title":                "unassigned",
			"content_version":      int64(1),
			"lifecycle_version":    int64(1),
			"current_execution_id": nil,
		})
		db.seed("tasks", map[string]any{
			"id":                   "task-bound",
			"tenant_id":            testTenantID,
			"project_id":           testProjectID,
			"state":                "running",
			"title":                "assigned",
			"content_version":      int64(1),
			"lifecycle_version":    int64(1),
			"current_execution_id": testExecutionID,
		})
		db.seed("executions", map[string]any{
			"id":           testExecutionID,
			"tenant_id":    testTenantID,
			"project_id":   testProjectID,
			"version":      int64(1),
			"state":        "queued",
			"task_id":      nil,
			"workspace_id": "ws-1",
		})

		// Verify the emulator itself enforces SQL three-valued NULL equality for
		// "=" while keeping NULL-safe equality for IS NOT DISTINCT FROM.
		rawConn, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin raw emulator conn: %v", err)
		}
		eqRows, err := rawConn.Query(ctx, "SELECT id FROM tasks WHERE current_execution_id = $1", nil)
		if err != nil {
			t.Fatalf("raw = nil query: %v", err)
		}
		if eqRows.Next() {
			t.Fatal("emulator matched NULL = NULL with ordinary '='")
		}
		_ = eqRows.Close()
		distinctRows, err := rawConn.Query(ctx, "SELECT id FROM tasks WHERE current_execution_id IS NOT DISTINCT FROM $1", nil)
		if err != nil {
			t.Fatalf("raw IS NOT DISTINCT FROM nil query: %v", err)
		}
		if !distinctRows.Next() {
			t.Fatal("emulator did not match NULL IS NOT DISTINCT FROM NULL")
		}
		_ = distinctRows.Close()
		_ = rawConn.Rollback(ctx)

		connector := New(db)

		// UPDATE with Expect nil refuses a row whose column is non-NULL.
		_, err = ExecuteUnit(ctx, connector, scope, store.Unit{
			Writes: []store.Write{{
				Table: "tasks",
				ID:    "task-bound",
				Expect: map[string]any{
					"current_execution_id": nil,
					"lifecycle_version":    uint64(1),
				},
				Values: map[string]any{"state": "closed"},
			}},
		})
		assertFaultClass(t, err, fault.VersionConflict)

		// UPDATE with Expect nil succeeds when the column is NULL.
		if _, err := ExecuteUnit(ctx, connector, scope, store.Unit{
			Writes: []store.Write{{
				Table: "tasks",
				ID:    "task-null",
				Expect: map[string]any{
					"current_execution_id": nil,
					"lifecycle_version":    uint64(1),
				},
				Values: map[string]any{
					"current_execution_id": testExecutionID,
					"lifecycle_version":    uint64(2),
				},
			}},
		}); err != nil {
			t.Fatalf("ExecuteUnit update Expect nil: %v", err)
		}
		updated := emuRowInScope(db, "tasks", "task-null", scope)
		if updated == nil || emuString(updated.values["current_execution_id"]) != testExecutionID {
			t.Fatalf("updated task-null = %+v; want current_execution_id=%q", updated, testExecutionID)
		}

		// Re-running the same Expect nil UPDATE now conflicts because the column is non-NULL.
		_, err = ExecuteUnit(ctx, connector, scope, store.Unit{
			Writes: []store.Write{{
				Table: "tasks",
				ID:    "task-null",
				Expect: map[string]any{
					"current_execution_id": nil,
					"lifecycle_version":    uint64(2),
				},
				Values: map[string]any{"lifecycle_version": uint64(3)},
			}},
		})
		assertFaultClass(t, err, fault.VersionConflict)

		// DELETE with Expect nil refuses a row whose column is non-NULL.
		_, err = ExecuteUnit(ctx, connector, scope, store.Unit{
			Writes: []store.Write{{
				Table:  "tasks",
				ID:     "task-bound",
				Delete: true,
				Expect: map[string]any{
					"current_execution_id": nil,
					"lifecycle_version":    uint64(1),
				},
			}},
		})
		assertFaultClass(t, err, fault.VersionConflict)

		// DELETE with Expect nil succeeds on a versioned row whose nullable column is NULL.
		if _, err := ExecuteUnit(ctx, connector, scope, store.Unit{
			Writes: []store.Write{{
				Table:         "executions",
				ID:            testExecutionID,
				Delete:        true,
				ExpectVersion: 1,
				Expect:        map[string]any{"task_id": nil},
			}},
		}); err != nil {
			t.Fatalf("ExecuteUnit delete Expect nil on executions: %v", err)
		}
		if rows := emuRowsFor(db, "executions"); len(rows) != 0 {
			t.Fatalf("executions remaining after guarded delete = %d; want 0", len(rows))
		}
	})
}
