// PostgreSQL absence-guard plan.
//
// The adapter must hold the absence guard under READ COMMITTED: SELECT ... FOR
// UPDATE on zero rows locks no gap, so the guard and every INSERT into a table
// with registered absence-guardable unique keys take the same
// transaction-scoped advisory lock on a deterministic hash of (table,
// unique-key values), then verify that no row matches, and map a unique-key
// violation to version_conflict.
//
// These in-process vectors prove the rendered statement sequence, the
// parameterization, the lock order and the error mapping. The two-session
// first-grant vector drives the real adapter against a read-committed test
// double that models the advisory-lock block and statement-level visibility of
// a committed row. They do not prove actual PostgreSQL/pgx semantics.
package postgres

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/store"
)

// enableAbsenceGuard turns the declaration-derived absence plan on for one
// vector without changing the declaration itself.
func enableAbsenceGuard(t *testing.T) {
	t.Helper()
	previous := absenceGuard
	absenceGuard = true
	t.Cleanup(func() { absenceGuard = previous })
}

// membershipAbsenceLock names one memberships unique-key identity. A tenant-wide
// row carries an explicit NULL project_id, never an empty string.
func membershipAbsenceLock(tenant, project, principal string) store.Lock {
	var projectValue any
	if project != "" {
		projectValue = project
	}
	return store.Lock{
		Table:        "memberships",
		ExpectAbsent: true,
		Key: map[string]any{
			"tenant_id":    tenant,
			"project_id":   projectValue,
			"principal_id": principal,
		},
	}
}

// membershipInsertWrite is one first-grant membership insert.
func membershipInsertWrite(rowID, principal string) store.Write {
	return store.Write{
		Table:  "memberships",
		ID:     rowID,
		Insert: true,
		Values: map[string]any{
			"principal_id": principal,
			"roles":        []byte(`["engineer"]`),
			"version":      uint64(1),
		},
	}
}

// absenceSequence renders the ordered data statements one unit issued. The
// transaction-local scope statements are omitted because they are not part of
// the lock/write plan.
func absenceSequence(calls []recordedCall) []string {
	sequence := make([]string, 0, len(calls))
	for _, call := range calls {
		text := strings.ToLower(strings.TrimSpace(call.sql))
		switch call.kind {
		case "commit":
			sequence = append(sequence, "commit")
		case "rollback":
			sequence = append(sequence, "rollback")
		case "exec":
			switch {
			case strings.Contains(text, "set_config"):
			case strings.Contains(text, "pg_advisory_xact_lock"):
				sequence = append(sequence, "advisory")
			case strings.HasPrefix(text, "insert into memberships"):
				sequence = append(sequence, "insert:memberships")
			case strings.HasPrefix(text, "insert into tasks"):
				sequence = append(sequence, "insert:tasks")
			case strings.HasPrefix(text, "insert into services"):
				sequence = append(sequence, "insert:services")
			case strings.Contains(text, " from tasks") && strings.Contains(text, "for update"):
				sequence = append(sequence, "lock:tasks")
			default:
				sequence = append(sequence, "exec")
			}
		case "query":
			if strings.Contains(text, "is not distinct from") {
				sequence = append(sequence, "guard:absent")
			} else {
				sequence = append(sequence, "query")
			}
		}
	}
	return sequence
}

func advisoryCalls(calls []recordedCall) []recordedCall {
	filtered := make([]recordedCall, 0, len(calls))
	for _, call := range calls {
		if call.kind == "exec" && strings.Contains(strings.ToLower(call.sql), "pg_advisory_xact_lock") {
			filtered = append(filtered, call)
		}
	}
	return filtered
}

func absenceGuardCalls(calls []recordedCall) []recordedCall {
	filtered := make([]recordedCall, 0, len(calls))
	for _, call := range calls {
		if call.kind == "query" && strings.Contains(strings.ToLower(call.sql), "is not distinct from") {
			filtered = append(filtered, call)
		}
	}
	return filtered
}

// TestExecuteUnitAbsenceGuardStatementSequence pins the plan: the scope is
// established first, absence advisory locks are taken in the declared lock
// order, the absence SELECT runs before any write, every membership insert
// takes the same advisory lock immediately before it inserts, and the unit
// commits last.
func TestExecuteUnitAbsenceGuardStatementSequence(t *testing.T) {
	enableAbsenceGuard(t)
	db := newFakeDB()
	scope := testProjectScope()
	lock := membershipAbsenceLock(testTenantID, testProjectID, testPrincipalID)
	unit := store.Unit{
		Locks: []store.Lock{lock, {Table: "tasks", IDs: []string{"task-1"}}},
		Writes: []store.Write{
			membershipInsertWrite("membership-1", testPrincipalID),
			{Table: "tasks", ID: "task-1", Insert: true, Values: unitValues("tasks")},
		},
	}
	if _, err := ExecuteUnit(context.Background(), New(db), scope, unit); err != nil {
		t.Fatalf("ExecuteUnit absence-guarded first grant: %v", err)
	}

	conn := db.conn(t)
	if conn.commits != 1 || conn.rollbacks != 0 {
		t.Fatalf("terminal calls: commits=%d rollbacks=%d; want one commit", conn.commits, conn.rollbacks)
	}
	calls := conn.callsCopy()
	got := absenceSequence(calls)
	want := []string{
		"advisory",           // memberships absence guard
		"advisory",           // the insert's same lock, in the declared lock order
		"lock:tasks",         // the later-ranked row lock stays after them
		"guard:absent",       // absence verification before any write
		"insert:memberships", // the lock phase already serialized this insert
		"insert:tasks",       // writes keep the declared write order
		"commit",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("data statement sequence = %v; want %v", got, want)
	}

	advisories := advisoryCalls(calls)
	if len(advisories) != 2 {
		t.Fatalf("advisory lock statements = %d; want 2 (guard + insert)", len(advisories))
	}
	guards := absenceGuardCalls(calls)
	if len(guards) != 1 {
		t.Fatalf("absence guard SELECTs = %d; want 1", len(guards))
	}

	guardSQL := strings.ToLower(guards[0].sql)
	if !strings.HasPrefix(strings.TrimSpace(guardSQL), "select 1 from memberships") {
		t.Errorf("absence guard SELECT = %q; want a parameterized SELECT 1 FROM memberships", guards[0].sql)
	}
	for index, wantArgument := range []any{testTenantID, testProjectID, testPrincipalID} {
		if !argContains(guards[0].args, wantArgument) {
			t.Errorf("absence guard args %v are missing key value %v", guards[0].args, wantArgument)
		}
		if index >= len(guards[0].args) || guards[0].args[index] != wantArgument {
			t.Errorf("absence guard arg %d = %v; want %v (declared key column order)", index, guards[0].args[index], wantArgument)
		}
	}
	if strings.Contains(guardSQL, testTenantID) || strings.Contains(guardSQL, testPrincipalID) {
		t.Errorf("absence guard SELECT carries a key value in its text instead of its arguments: %s", guards[0].sql)
	}

	for _, advisory := range advisories {
		sql := strings.ToLower(advisory.sql)
		if !strings.Contains(sql, "pg_advisory_xact_lock") || !strings.Contains(sql, "hashtextextended") {
			t.Errorf("advisory statement = %q; want pg_advisory_xact_lock(hashtextextended($1, 0))", advisory.sql)
		}
		if !strings.Contains(sql, "$1") || strings.Count(sql, "$") != 1 {
			t.Errorf("advisory statement = %q; want one parameterized argument", advisory.sql)
		}
		if len(advisory.args) != 1 {
			t.Fatalf("advisory statement has %d arguments; want 1", len(advisory.args))
		}
		if strings.Contains(sql, testTenantID) || strings.Contains(sql, testPrincipalID) || strings.Contains(sql, testProjectID) {
			t.Errorf("advisory statement carries key values in its text: %s", advisory.sql)
		}
	}
	if len(advisories[0].args) == 1 && len(advisories[1].args) == 1 {
		if advisories[0].args[0] != advisories[1].args[0] {
			t.Errorf("guard advisory key %v != insert advisory key %v; both paths must take the same lock",
				advisories[0].args[0], advisories[1].args[0])
		}
		canonical, ok := advisories[1].args[0].(string)
		if !ok {
			t.Fatalf("advisory key is %T; want the canonical string", advisories[1].args[0])
		}
		for _, fragment := range []string{"memberships", testTenantID, testProjectID, testPrincipalID} {
			if !strings.Contains(canonical, fragment) {
				t.Errorf("canonical advisory key %q is missing %q", canonical, fragment)
			}
		}
	}
}

// TestExecuteUnitAbsenceGuardPresentRowConflicts: a matching row at guard time
// is version_conflict, nothing is inserted and the transaction rolls back.
func TestExecuteUnitAbsenceGuardPresentRowConflicts(t *testing.T) {
	enableAbsenceGuard(t)
	db := newFakeDB()
	db.script.setQuery(func(sql string, args []any) (store.Rows, error) {
		if strings.Contains(strings.ToLower(sql), "is not distinct from") {
			return &fakeRows{rows: [][]any{{int64(1)}}, index: -1}, nil
		}
		return &fakeRows{index: -1}, nil
	})
	unit := store.Unit{
		Locks:  []store.Lock{membershipAbsenceLock(testTenantID, testProjectID, testPrincipalID)},
		Writes: []store.Write{membershipInsertWrite("membership-new", testPrincipalID)},
	}
	_, err := ExecuteUnit(context.Background(), New(db), testProjectScope(), unit)
	assertFaultClass(t, err, fault.VersionConflict)

	conn := db.conn(t)
	if conn.commits != 0 || conn.rollbacks != 1 {
		t.Fatalf("present-row conflict terminal calls: commits=%d rollbacks=%d; want one rollback", conn.commits, conn.rollbacks)
	}
	calls := conn.callsCopy()
	if indexOfTable(calls, "memberships", "insert") >= 0 {
		t.Error("a refused absence guard still inserted its business row")
	}
	if got := absenceSequence(calls); !reflect.DeepEqual(got, []string{"advisory", "advisory", "guard:absent", "rollback"}) {
		t.Fatalf("conflict statement sequence = %v; want the locks, the failed guard and a rollback", got)
	}
}

// TestExecuteUnitAbsenceGuardsSortByCanonicalKey: same-table guards are ordered
// by their canonical unique key (declared column order, NULL before any value)
// and take their advisory locks in that order.
func TestExecuteUnitAbsenceGuardsSortByCanonicalKey(t *testing.T) {
	enableAbsenceGuard(t)
	db := newFakeDB()
	unit := store.Unit{Locks: []store.Lock{
		membershipAbsenceLock(testTenantID, "project-2", testPrincipalID),
		membershipAbsenceLock(testTenantID, "", testPrincipalID),
		membershipAbsenceLock(testTenantID, "project-1", testPrincipalID),
	}}
	if _, err := ExecuteUnit(context.Background(), New(db), testProjectScope(), unit); err != nil {
		t.Fatalf("ExecuteUnit ordered absence guards: %v", err)
	}

	calls := db.conn(t).callsCopy()
	advisories := advisoryCalls(calls)
	if len(advisories) != 3 {
		t.Fatalf("advisory lock statements = %d; want 3", len(advisories))
	}
	keys := make([]string, len(advisories))
	for index, advisory := range advisories {
		key, ok := advisory.args[0].(string)
		if !ok {
			t.Fatalf("advisory %d key is %T; want a canonical string", index, advisory.args[0])
		}
		keys[index] = key
	}
	if !(keys[0] < keys[1] && keys[1] < keys[2]) {
		t.Fatalf("advisory keys are not in canonical order: %q", keys)
	}

	guards := absenceGuardCalls(calls)
	if len(guards) != 3 {
		t.Fatalf("absence guard SELECTs = %d; want 3", len(guards))
	}
	wantProjects := []any{nil, "project-1", "project-2"}
	for index, guard := range guards {
		if len(guard.args) < 2 {
			t.Fatalf("guard %d args = %v; want tenant, project and principal", index, guard.args)
		}
		if guard.args[1] != wantProjects[index] {
			t.Errorf("guard %d project arg = %v; want %v (NULL before any value)", index, guard.args[1], wantProjects[index])
		}
	}
	// The same table's guards must precede the write phase entirely.
	if insert := indexOfTable(calls, "memberships", "insert"); insert >= 0 {
		t.Fatal("a guard-only unit inserted a membership row")
	}
}

// TestExecuteUnitMembershipInsertWithoutGuardStillLocks: every membership
// insert takes the same advisory lock even when the unit carries no absence
// guard, so an unguarded grant serializes against a guarded first grant.
func TestExecuteUnitMembershipInsertWithoutGuardStillLocks(t *testing.T) {
	enableAbsenceGuard(t)
	db := newFakeDB()
	unit := store.Unit{Writes: []store.Write{membershipInsertWrite("membership-1", testPrincipalID)}}
	if _, err := ExecuteUnit(context.Background(), New(db), testProjectScope(), unit); err != nil {
		t.Fatalf("ExecuteUnit unguarded membership insert: %v", err)
	}
	calls := db.conn(t).callsCopy()
	if got := absenceSequence(calls); !reflect.DeepEqual(got, []string{"advisory", "insert:memberships", "commit"}) {
		t.Fatalf("unguarded insert sequence = %v; want advisory lock before the insert", got)
	}
	advisories := advisoryCalls(calls)
	if len(advisories) != 1 {
		t.Fatalf("advisory lock statements = %d; want 1", len(advisories))
	}
	guarded, err := absenceKeyText("memberships", map[string]any{
		"tenant_id": testTenantID, "project_id": testProjectID, "principal_id": testPrincipalID,
	})
	if err != nil {
		t.Fatalf("canonical guard key: %v", err)
	}
	if advisories[0].args[0] != guarded {
		t.Errorf("unguarded insert advisory key %v != guarded key %q", advisories[0].args[0], guarded)
	}

	// A table with no registered absence key takes no advisory lock.
	plain := newFakeDB()
	plainUnit := store.Unit{Writes: []store.Write{
		{Table: "tasks", ID: "task-1", Insert: true, Values: unitValues("tasks")},
		{Table: "services", ID: testServiceID, Insert: true, Values: unitValues("services")},
	}}
	if _, err := ExecuteUnit(context.Background(), New(plain), testProjectScope(), plainUnit); err != nil {
		t.Fatalf("ExecuteUnit without absence tables: %v", err)
	}
	if got := len(advisoryCalls(plain.conn(t).callsCopy())); got != 0 {
		t.Fatalf("tables without a registered absence key issued %d advisory locks; want 0", got)
	}
}

// TestExecuteUnitAbsenceKeyDistinguishesNullProject: a tenant-wide NULL project
// is a different identity from any project value and sorts before it.
func TestExecuteUnitAbsenceKeyDistinguishesNullProject(t *testing.T) {
	nullKey, err := absenceKeyText("memberships", map[string]any{
		"tenant_id": testTenantID, "project_id": nil, "principal_id": testPrincipalID,
	})
	if err != nil {
		t.Fatalf("canonical tenant-wide key: %v", err)
	}
	projectKey, err := absenceKeyText("memberships", map[string]any{
		"tenant_id": testTenantID, "project_id": testProjectID, "principal_id": testPrincipalID,
	})
	if err != nil {
		t.Fatalf("canonical project key: %v", err)
	}
	if nullKey == projectKey {
		t.Fatal("a tenant-wide key and a project key render the same canonical text")
	}
	if !(nullKey < projectKey) {
		t.Fatalf("tenant-wide key %q does not sort before project key %q", nullKey, projectKey)
	}
	otherTenant, err := absenceKeyText("memberships", map[string]any{
		"tenant_id": "tenant-2", "project_id": nil, "principal_id": testPrincipalID,
	})
	if err != nil {
		t.Fatalf("canonical other-tenant key: %v", err)
	}
	if nullKey == otherTenant {
		t.Fatal("keys that differ only in tenant render the same canonical text")
	}
}

// TestExecuteUnitAbsenceGuardGateFailsClosed: the single declaration-derived
// switch still refuses the unit before any transaction when it is off.
func TestExecuteUnitAbsenceGuardGateFailsClosed(t *testing.T) {
	previous := absenceGuard
	absenceGuard = false
	t.Cleanup(func() { absenceGuard = previous })

	db := newFakeDB()
	unit := store.Unit{
		Locks:  []store.Lock{membershipAbsenceLock(testTenantID, testProjectID, testPrincipalID)},
		Writes: []store.Write{membershipInsertWrite("membership-1", testPrincipalID)},
	}
	_, err := ExecuteUnit(context.Background(), New(db), testProjectScope(), unit)
	assertFaultClass(t, err, fault.InvalidInput)
	if got := db.beginCount(); got != 0 {
		t.Fatalf("a gated absence unit opened %d transactions before refusal", got)
	}
}

// sqlStateTestError models a pgx/pgconn SQLSTATE error at the seam.
type sqlStateTestError string

func (err sqlStateTestError) Error() string    { return "sqlstate " + string(err) }
func (err sqlStateTestError) SQLState() string { return string(err) }

// TestExecuteUnitMapsMembershipUniqueViolation: a 23505 from the membership
// insert or the unit commit is version_conflict, never a raw driver error.
func TestExecuteUnitMapsMembershipUniqueViolation(t *testing.T) {
	t.Run("insert conflict", func(t *testing.T) {
		enableAbsenceGuard(t)
		db := newFakeDB()
		db.script.setExec(func(sql string, args []any) (store.Result, error) {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(sql)), "insert into memberships") {
				return store.Result{}, sqlStateTestError("23505")
			}
			return store.Result{RowsAffected: 1}, nil
		})
		unit := store.Unit{Writes: []store.Write{membershipInsertWrite("membership-1", testPrincipalID)}}
		_, err := ExecuteUnit(context.Background(), New(db), testProjectScope(), unit)
		assertFaultClass(t, err, fault.VersionConflict)
		conn := db.conn(t)
		if conn.commits != 0 || conn.rollbacks != 1 {
			t.Fatalf("unique-violation terminal calls: commits=%d rollbacks=%d; want one rollback", conn.commits, conn.rollbacks)
		}
	})

	t.Run("commit conflict", func(t *testing.T) {
		enableAbsenceGuard(t)
		db := newFakeDB()
		db.script.commitErr = sqlStateTestError("23505")
		unit := store.Unit{Writes: []store.Write{membershipInsertWrite("membership-1", testPrincipalID)}}
		_, err := ExecuteUnit(context.Background(), New(db), testProjectScope(), unit)
		assertFaultClass(t, err, fault.VersionConflict)
		if conn := db.conn(t); conn.rollbacks == 0 {
			t.Fatal("a unique violation on commit left the transaction without a rollback")
		}
	})
}

// TestControllerRefusesAbsenceGuard: the cross-scope controller authority never
// performs a membership authority check, so it still refuses the guard.
func TestControllerRefusesAbsenceGuard(t *testing.T) {
	enableAbsenceGuard(t)
	db := newFakeDB()
	unit := store.Unit{Locks: []store.Lock{membershipAbsenceLock(testTenantID, testProjectID, testPrincipalID)}}
	_, err := NewController(db).Execute(context.Background(), testExecutionID, unit)
	assertFaultClass(t, err, fault.Forbidden)
	if got := db.beginCount(); got != 0 {
		t.Fatalf("the controller opened %d transactions before refusing an absence guard", got)
	}
}

// absenceRaceDB is a deterministic in-process READ COMMITTED double for the
// first-grant race. Committed rows become visible to a later statement, an
// advisory lock is transaction-scoped, and a second session blocks on the same
// advisory key. It is not a PostgreSQL implementation and is no real
// two-session proof.
type absenceRaceDB struct {
	mu        sync.Mutex
	committed []map[string]any
	locks     map[string]chan struct{}

	guardReached  chan struct{}
	secondWaiting chan struct{}
	releaseGuard  chan struct{}

	blockedFirstGuard bool
	signaledWait      bool
}

func newAbsenceRaceDB() *absenceRaceDB {
	return &absenceRaceDB{
		locks:         make(map[string]chan struct{}),
		guardReached:  make(chan struct{}),
		secondWaiting: make(chan struct{}),
		releaseGuard:  make(chan struct{}),
	}
}

func (db *absenceRaceDB) Begin(ctx context.Context) (Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &absenceRaceConn{
		db:      db,
		working: make([]map[string]any, 0, 1),
		heldSet: make(map[string]bool, 1),
	}, nil
}

func (db *absenceRaceDB) lockChannel(key string) chan struct{} {
	db.mu.Lock()
	defer db.mu.Unlock()
	channel, ok := db.locks[key]
	if !ok {
		channel = make(chan struct{}, 1)
		db.locks[key] = channel
	}
	return channel
}

func (db *absenceRaceDB) acquire(ctx context.Context, key string) error {
	channel := db.lockChannel(key)
	select {
	case channel <- struct{}{}:
		return nil
	default:
	}
	db.mu.Lock()
	if !db.signaledWait {
		db.signaledWait = true
		close(db.secondWaiting)
	}
	db.mu.Unlock()
	select {
	case channel <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (db *absenceRaceDB) release(key string) {
	channel := db.lockChannel(key)
	select {
	case <-channel:
	default:
	}
}

type absenceRaceConn struct {
	db      *absenceRaceDB
	working []map[string]any
	held    []string
	heldSet map[string]bool
	closed  bool
}

func (conn *absenceRaceConn) Exec(ctx context.Context, sql string, args ...any) (store.Result, error) {
	if err := ctx.Err(); err != nil {
		return store.Result{}, err
	}
	if conn.closed {
		return store.Result{}, ErrNoTransaction
	}
	lower := strings.ToLower(strings.TrimSpace(sql))
	switch {
	case strings.Contains(lower, "set_config"):
		return store.Result{RowsAffected: 1}, nil
	case strings.Contains(lower, "pg_advisory_xact_lock"):
		if len(args) != 1 {
			return store.Result{}, fmt.Errorf("advisory lock with %d arguments", len(args))
		}
		key, ok := args[0].(string)
		if !ok {
			return store.Result{}, fmt.Errorf("advisory key is %T; want string", args[0])
		}
		// PostgreSQL advisory locks are reentrant within a session: the guard
		// lock and the insert lock are the same transaction-scoped lock, so
		// the second acquisition must not block on the session's own hold.
		if conn.heldSet[key] {
			return store.Result{RowsAffected: 1}, nil
		}
		if err := conn.db.acquire(ctx, key); err != nil {
			return store.Result{}, err
		}
		conn.heldSet[key] = true
		conn.held = append(conn.held, key)
		return store.Result{RowsAffected: 1}, nil
	case strings.HasPrefix(lower, "insert into memberships"):
		row, err := absenceRaceRow(sql, args)
		if err != nil {
			return store.Result{}, err
		}
		conn.db.mu.Lock()
		duplicate := absenceRaceFind(conn.db.committed, row) || absenceRaceFind(conn.working, row)
		conn.db.mu.Unlock()
		if duplicate {
			return store.Result{}, sqlStateTestError("23505")
		}
		conn.working = append(conn.working, row)
		return store.Result{RowsAffected: 1}, nil
	default:
		return store.Result{}, fmt.Errorf("absence race double: unsupported exec %q", sql)
	}
}

func (conn *absenceRaceConn) Query(ctx context.Context, sql string, args ...any) (store.Rows, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if conn.closed {
		return nil, ErrNoTransaction
	}
	lower := strings.ToLower(sql)
	if !strings.Contains(lower, "is not distinct from") {
		return nil, fmt.Errorf("absence race double: unsupported query %q", sql)
	}
	if len(args) != 3 {
		return nil, fmt.Errorf("absence guard query with %d arguments", len(args))
	}
	conn.db.mu.Lock()
	block := !conn.db.blockedFirstGuard
	if block {
		conn.db.blockedFirstGuard = true
	}
	conn.db.mu.Unlock()
	if block {
		close(conn.db.guardReached)
		select {
		case <-conn.db.releaseGuard:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	conn.db.mu.Lock()
	found := absenceRaceFindKey(conn.db.committed, args)
	conn.db.mu.Unlock()
	if found {
		return &fakeRows{rows: [][]any{{int64(1)}}, index: -1}, nil
	}
	return &fakeRows{index: -1}, nil
}

func (conn *absenceRaceConn) Commit(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	conn.db.mu.Lock()
	conn.db.committed = append(conn.db.committed, conn.working...)
	conn.db.mu.Unlock()
	conn.working = nil
	conn.releaseHeld()
	conn.closed = true
	return nil
}

func (conn *absenceRaceConn) Rollback(ctx context.Context) error {
	if conn.closed {
		return ErrNoTransaction
	}
	conn.working = nil
	conn.releaseHeld()
	conn.closed = true
	return nil
}

func (conn *absenceRaceConn) releaseHeld() {
	for _, key := range conn.held {
		conn.db.release(key)
	}
	conn.held = nil
	conn.heldSet = make(map[string]bool, 1)
}

// absenceRaceRow decodes one rendered membership insert. The rendered values are
// placeholders in column order, so the recorded arguments line up with the
// parsed column list.
func absenceRaceRow(sql string, args []any) (map[string]any, error) {
	match := insertPattern.FindStringSubmatch(strings.TrimSpace(sql))
	if match == nil {
		return nil, fmt.Errorf("absence race double: unparsable insert %q", sql)
	}
	columns := splitTop(match[2], ',')
	if len(columns) != len(args) {
		return nil, fmt.Errorf("absence race double: %d columns for %d arguments", len(columns), len(args))
	}
	row := make(map[string]any, len(columns))
	for index, column := range columns {
		row[trimIdent(column)] = args[index]
	}
	return row, nil
}

func absenceRaceFind(rows []map[string]any, candidate map[string]any) bool {
	for _, row := range rows {
		if row["tenant_id"] == candidate["tenant_id"] &&
			row["project_id"] == candidate["project_id"] &&
			row["principal_id"] == candidate["principal_id"] {
			return true
		}
	}
	return false
}

func absenceRaceFindKey(rows []map[string]any, key []any) bool {
	for _, row := range rows {
		if row["tenant_id"] == key[0] && row["project_id"] == key[1] && row["principal_id"] == key[2] {
			return true
		}
	}
	return false
}

// TestExecuteUnitConcurrentFirstGrantsExactlyOneWins drives two real adapter
// sessions through the double. Both first grants absence-guard the same
// (tenant, project, principal) key and insert different row ids; exactly one
// commits and the loser is version_conflict with no row.
func TestExecuteUnitConcurrentFirstGrantsExactlyOneWins(t *testing.T) {
	enableAbsenceGuard(t)
	db := newAbsenceRaceDB()
	scope := testProjectScope()
	first := store.Unit{
		Locks:  []store.Lock{membershipAbsenceLock(testTenantID, testProjectID, testPrincipalID)},
		Writes: []store.Write{membershipInsertWrite("membership-a", testPrincipalID)},
	}
	second := store.Unit{
		Locks:  []store.Lock{membershipAbsenceLock(testTenantID, testProjectID, testPrincipalID)},
		Writes: []store.Write{membershipInsertWrite("membership-b", testPrincipalID)},
	}

	firstResult := make(chan error, 1)
	go func() {
		_, err := ExecuteUnit(context.Background(), New(db), scope, first)
		firstResult <- err
	}()
	select {
	case <-db.guardReached:
	case <-time.After(5 * time.Second):
		t.Fatal("the first grant never reached its absence guard")
	}

	secondResult := make(chan error, 1)
	go func() {
		_, err := ExecuteUnit(context.Background(), New(db), scope, second)
		secondResult <- err
	}()
	select {
	case <-db.secondWaiting:
	case <-time.After(5 * time.Second):
		t.Fatal("the second grant never blocked on the shared advisory lock")
	}
	close(db.releaseGuard)

	firstErr := <-firstResult
	secondErr := <-secondResult
	if firstErr != nil {
		t.Fatalf("the first grant failed: %v", firstErr)
	}
	assertFaultClass(t, secondErr, fault.VersionConflict)

	if len(db.committed) != 1 {
		t.Fatalf("committed membership rows = %d; want exactly one winner", len(db.committed))
	}
	if got := db.committed[0]["id"]; got != "membership-a" {
		t.Fatalf("committed row id = %v; want the first grant's row", got)
	}
}
