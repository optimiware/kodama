package adaptercontract

// Absence-guard declaration and reference vectors.
//
// The authority commit unit pins both membership rows: a present row by its
// observed membership identity AND version, an ABSENT row by an absence pin.
// A lock on a row id can pin only a present row, so the commit-unit
// declaration also carries an absence guard:
//
//	store.Lock{
//	    Table:        "memberships",
//	    ExpectAbsent: true, // json:"expect_absent,omitempty"
//	    Key: map[string]any{ // json:"key,omitempty"
//	        "tenant_id": "tenant-1", "project_id": nil, "principal_id": "principal-1",
//	    },
//	}
//
// ExpectAbsent names a row by its registered UNIQUE-KEY identity rather than by
// a row id, because an absent row has no id. The registered key for
// memberships is (tenant_id, project_id, principal_id) with project_id NULL for
// a tenant-wide row; store.AbsenceKeyColumns declares it. The unit commits only
// when no row of that table matches the key. A matching row is
// version_conflict (never forbidden or not_found) and nothing is persisted.
//
// ExpectAbsent is exclusive: it cannot be combined with IDs, ExpectVersion,
// Expect, TenantWide or Deadline, and Key is only meaningful with ExpectAbsent.
// Unit.Validate refuses every malformed combination with invalid_input before
// an adapter starts a transaction, so a bad guard can never reach a statement.
// The cross-scope controller authority never performs a membership authority
// check, so a controller unit carrying an absence guard is refused forbidden.
//
// A first membership grant (MembershipGrantUnit ExpectedVersion zero) is a
// create-if-absent: its commit unit carries the same absence guard on the
// (tenant, project, principal) key as its insert, so two concurrent first
// grants cannot both win; exactly one commits and the loser is
// version_conflict. A revoked row identity is never reused, so revoke/regrant
// mints a new row id and the absence guard, not a version, is what serializes
// the create.
//
// The PostgreSQL adapter must hold the guard under READ COMMITTED, where
// SELECT ... FOR UPDATE on zero rows locks no gap. The guard and every insert
// that can create a matching row take the same transaction-scoped lock -
// a deterministic advisory lock over (table, unique-key values), an
// insert-conflict with in-transaction recheck, or a sentinel row lock. These
// vectors are semantic inputs, not a real two-session proof.
//
// The reference interpreter below is deliberately trivial and deterministic:
// it splits guard time from commit time so a vector can create the matching row
// in the barrier between them, exactly the race an adapter must close. A
// deliberately broken reference that drops the commit-time recheck must fail at
// least one vector.

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// TestLockAbsenceDeclaration pins the absence fields, their wire names and
// the registered membership unique key.
func TestLockAbsenceDeclaration(t *testing.T) {
	lockType := reflect.TypeOf(store.Lock{})
	expectAbsent, ok := lockType.FieldByName("ExpectAbsent")
	if !ok {
		t.Fatal("store.Lock has no ExpectAbsent field; the absence guard is not declared")
	}
	if expectAbsent.Type.Kind() != reflect.Bool {
		t.Fatalf("store.Lock.ExpectAbsent type = %s; want bool", expectAbsent.Type)
	}
	if got := expectAbsent.Tag.Get("json"); got != "expect_absent,omitempty" {
		t.Fatalf("store.Lock.ExpectAbsent json tag = %q; want %q", got, "expect_absent,omitempty")
	}

	keyField, ok := lockType.FieldByName("Key")
	if !ok {
		t.Fatal("store.Lock has no Key field; an absence guard cannot name its unique-key identity")
	}
	if keyField.Type != reflect.TypeOf(map[string]any{}) {
		t.Fatalf("store.Lock.Key type = %s; want map[string]any", keyField.Type)
	}
	if got := keyField.Tag.Get("json"); got != "key,omitempty" {
		t.Fatalf("store.Lock.Key json tag = %q; want %q", got, "key,omitempty")
	}

	columns, registered := store.AbsenceKeyColumns("memberships")
	if !registered {
		t.Fatal("memberships does not register an absence-guardable unique key")
	}
	if got := strings.Join(columns, ","); got != "tenant_id,project_id,principal_id" {
		t.Fatalf("memberships absence key columns = %q; want the declared unique key tenant_id,project_id,principal_id", got)
	}
	if _, registered := store.AbsenceKeyColumns("tasks"); registered {
		t.Fatal("tasks registers an absence-guardable unique key; only memberships is registered")
	}

	// The declared wire names are the contract names; a tenant-wide identity
	// stays an explicit NULL project_id rather than disappearing from the map.
	encoded, err := json.Marshal(store.Lock{
		Table:        "memberships",
		ExpectAbsent: true,
		Key: map[string]any{
			"tenant_id":    "tenant-1",
			"project_id":   nil,
			"principal_id": "principal-1",
		},
	})
	if err != nil {
		t.Fatalf("marshal an absence lock: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatalf("unmarshal an absence lock: %v", err)
	}
	if absent, ok := wire["expect_absent"].(bool); !ok || !absent {
		t.Fatalf("marshalled lock expect_absent = %v; want true", wire["expect_absent"])
	}
	key, ok := wire["key"].(map[string]any)
	if !ok {
		t.Fatalf("marshalled lock has no key object: %v", wire["key"])
	}
	if _, present := key["project_id"]; !present {
		t.Fatal("a tenant-wide absence key must carry project_id: null, not omit the column")
	}
	if key["project_id"] != nil {
		t.Fatalf("tenant-wide project_id = %v; want null", key["project_id"])
	}
}

// TestUnitValidateRefusesMalformedAbsenceGuard proves a malformed guard shape
// is refused with invalid_input before any adapter starts a statement. A
// malformed guard is never treated as a present-row wildcard.
func TestUnitValidateRefusesMalformedAbsenceGuard(t *testing.T) {
	projectKey := func() map[string]any {
		return map[string]any{"tenant_id": "tenant-1", "project_id": "project-1", "principal_id": "principal-1"}
	}
	tenantWideKey := func() map[string]any {
		return map[string]any{"tenant_id": "tenant-1", "project_id": nil, "principal_id": "principal-1"}
	}
	cases := []struct {
		name  string
		lock  store.Lock
		valid bool
	}{
		{name: "project absence", valid: true, lock: store.Lock{Table: "memberships", ExpectAbsent: true, Key: projectKey()}},
		{name: "tenant-wide absence", valid: true, lock: store.Lock{Table: "memberships", ExpectAbsent: true, Key: tenantWideKey()}},
		{name: "absence with ids", lock: store.Lock{Table: "memberships", ExpectAbsent: true, Key: tenantWideKey(), IDs: []string{"membership-1"}}},
		{name: "absence with expect version", lock: store.Lock{Table: "memberships", ExpectAbsent: true, Key: tenantWideKey(), ExpectVersion: 3}},
		{name: "absence with expect values", lock: store.Lock{Table: "memberships", ExpectAbsent: true, Key: tenantWideKey(), Expect: map[string]any{"roles": "engineer"}}},
		{name: "absence tenant wide flag", lock: store.Lock{Table: "memberships", ExpectAbsent: true, Key: tenantWideKey(), TenantWide: true}},
		{name: "absence with deadline", lock: store.Lock{Table: "memberships", ExpectAbsent: true, Key: tenantWideKey(), Deadline: &store.DeadlineGuard{Column: "created_at"}}},
		{name: "absence without key", lock: store.Lock{Table: "memberships", ExpectAbsent: true}},
		{name: "key without absence", lock: store.Lock{Table: "memberships", Key: tenantWideKey()}},
		{name: "unregistered table", lock: store.Lock{Table: "tasks", ExpectAbsent: true, Key: tenantWideKey()}},
		{name: "missing project column", lock: store.Lock{Table: "memberships", ExpectAbsent: true, Key: map[string]any{"tenant_id": "tenant-1", "principal_id": "principal-1"}}},
		{name: "missing principal column", lock: store.Lock{Table: "memberships", ExpectAbsent: true, Key: map[string]any{"tenant_id": "tenant-1", "project_id": "project-1"}}},
		{name: "extra key column", lock: store.Lock{Table: "memberships", ExpectAbsent: true, Key: map[string]any{"tenant_id": "tenant-1", "project_id": "project-1", "principal_id": "principal-1", "roles": "engineer"}}},
		{name: "empty tenant", lock: store.Lock{Table: "memberships", ExpectAbsent: true, Key: map[string]any{"tenant_id": "", "project_id": nil, "principal_id": "principal-1"}}},
		{name: "empty principal", lock: store.Lock{Table: "memberships", ExpectAbsent: true, Key: map[string]any{"tenant_id": "tenant-1", "project_id": nil, "principal_id": ""}}},
		{name: "non-string tenant", lock: store.Lock{Table: "memberships", ExpectAbsent: true, Key: map[string]any{"tenant_id": 7, "project_id": nil, "principal_id": "principal-1"}}},
		{name: "non-string project", lock: store.Lock{Table: "memberships", ExpectAbsent: true, Key: map[string]any{"tenant_id": "tenant-1", "project_id": 7, "principal_id": "principal-1"}}},
	}
	for _, test := range cases {
		test := test
		t.Run(test.name, func(t *testing.T) {
			err := (store.Unit{Locks: []store.Lock{test.lock}}).Validate()
			if test.valid {
				if err != nil {
					t.Fatalf("a valid absence guard was refused: %v", err)
				}
				return
			}
			assertFaultClass(t, err, fault.InvalidInput, "malformed absence guard")
		})
	}
}

// TestControllerRefusesAbsenceGuard pins the controller allow-list
// and proves an engine controller cannot use the absence guard. The controller
// commits only Kernel state and never performs a membership authority check, so
// absence is not in its authority.
func TestControllerRefusesAbsenceGuard(t *testing.T) {
	if got := store.ControllerReadTables(); !reflect.DeepEqual(got, []string{"executions", "actions", "cleanup", "events"}) {
		t.Fatalf("ControllerReadTables = %v; the absence guard must not widen the controller read authority", got)
	}
	if got := store.ControllerWriteTables(); !reflect.DeepEqual(got, []string{"executions", "actions", "cleanup", "events", "audit"}) {
		t.Fatalf("ControllerWriteTables = %v; the absence guard must not widen the controller write authority", got)
	}

	reference := newReferenceSeams(seamSeed{}, "")
	unit := store.Unit{Locks: []store.Lock{membershipAbsenceLock("tenant-1", "project-1", "principal-1")}}
	_, err := reference.apply(absenceScope("tenant-1", "project-1"), unit, true)
	assertFaultClass(t, err, fault.Forbidden, "controller absence guard")
}

// TestAbsenceGuardCommitsOnAbsentRow: an absence guard on a key with no row
// commits, and a first grant inserts exactly one row.
func TestAbsenceGuardCommitsOnAbsentRow(t *testing.T) {
	reference := newAbsenceReference()
	txn, err := reference.begin(absenceScope("tenant-1", "project-1"), membershipGrantUnit("membership-1", "principal-1"))
	if err != nil {
		t.Fatalf("guard an absent membership: %v", err)
	}
	if err := txn.commit(); err != nil {
		t.Fatalf("commit an absent-guarded grant: %v", err)
	}
	if got := reference.count("memberships"); got != 1 {
		t.Fatalf("memberships rows = %d; want 1", got)
	}
	if !reference.hasID("memberships", "membership-1") {
		t.Fatal("the granted membership row is missing")
	}
}

// TestAbsenceGuardPresentRowConflictsWithoutEffect: a row already present at
// guard time is a version_conflict, never forbidden/not_found, and nothing is
// persisted.
func TestAbsenceGuardPresentRowConflictsWithoutEffect(t *testing.T) {
	reference := newAbsenceReference(membershipSeedRow("membership-existing", "tenant-1", "project-1", "principal-1"))
	_, err := reference.begin(absenceScope("tenant-1", "project-1"), membershipGrantUnit("membership-new", "principal-1"))
	assertFaultClass(t, err, fault.VersionConflict, "absence guard on a present membership")
	if got := reference.count("memberships"); got != 1 {
		t.Fatalf("memberships rows = %d; want the seeded row only", got)
	}
	if reference.hasID("memberships", "membership-new") {
		t.Fatal("a refused absence-guarded grant persisted a row")
	}
}

// TestAbsenceGuardCreatedBetweenGuardAndCommitConflicts is the crash-safe race:
// the row is absent at guard time and created by a concurrent unit before the
// guarded unit commits. The guarded unit must be a version_conflict and must
// not persist its unrelated write.
func TestAbsenceGuardCreatedBetweenGuardAndCommitConflicts(t *testing.T) {
	reference := newAbsenceReference()
	scope := absenceScope("tenant-1", "project-1")

	guarded, err := reference.begin(scope, store.Unit{
		Locks:  []store.Lock{membershipAbsenceLock("tenant-1", "project-1", "principal-1")},
		Writes: []store.Write{membershipInsert("membership-other", "principal-2")},
	})
	if err != nil {
		t.Fatalf("guard the absent key: %v", err)
	}

	concurrent, err := reference.begin(scope, membershipGrantUnit("membership-creator", "principal-1"))
	if err != nil {
		t.Fatalf("guard the concurrent creator: %v", err)
	}
	if err := concurrent.commit(); err != nil {
		t.Fatalf("commit the concurrent creator: %v", err)
	}

	err = guarded.commit()
	assertFaultClass(t, err, fault.VersionConflict, "guarded commit after a concurrent create")
	if reference.hasID("memberships", "membership-other") {
		t.Fatal("the refused guarded unit persisted its unrelated write")
	}
	if !reference.hasID("memberships", "membership-creator") {
		t.Fatal("the concurrent creator's row is missing")
	}
}

// TestConcurrentFirstGrantsExactlyOneWins: two first grants (ExpectedVersion
// zero) both absence-guard the same key and both insert. Exactly one commits;
// the loser is a version_conflict and its row id never appears.
func TestConcurrentFirstGrantsExactlyOneWins(t *testing.T) {
	reference := newAbsenceReference()
	scope := absenceScope("tenant-1", "project-1")

	first, err := reference.begin(scope, membershipGrantUnit("membership-a", "principal-1"))
	if err != nil {
		t.Fatalf("guard the first grant: %v", err)
	}
	second, err := reference.begin(scope, membershipGrantUnit("membership-b", "principal-1"))
	if err != nil {
		t.Fatalf("guard the second grant: %v", err)
	}
	if err := first.commit(); err != nil {
		t.Fatalf("commit the first grant: %v", err)
	}
	err = second.commit()
	assertFaultClass(t, err, fault.VersionConflict, "the losing first grant")
	if got := reference.count("memberships"); got != 1 {
		t.Fatalf("memberships rows = %d; want exactly one winner", got)
	}
	if !reference.hasID("memberships", "membership-a") {
		t.Fatal("the winning first grant's row is missing")
	}
	if reference.hasID("memberships", "membership-b") {
		t.Fatal("the losing first grant persisted its row")
	}
}

// TestAbsenceGuardTenantWideAndProjectKeysDiffer proves the key includes the
// project scope: a tenant-wide row (project_id NULL) and a project row are
// different identities, so neither absence guard conflicts with the other.
func TestAbsenceGuardTenantWideAndProjectKeysDiffer(t *testing.T) {
	projectScope := absenceScope("tenant-1", "project-1")

	// A tenant-wide row does not satisfy a project-scoped absence guard.
	projectView := newAbsenceReference(membershipSeedRow("membership-tenant", "tenant-1", "", "principal-1"))
	txn, err := projectView.begin(projectScope, membershipGrantUnit("membership-project", "principal-1"))
	if err != nil {
		t.Fatalf("a project absence guard conflicted with a tenant-wide row: %v", err)
	}
	if err := txn.commit(); err != nil {
		t.Fatalf("commit a project grant with a tenant-wide row present: %v", err)
	}
	if got := projectView.count("memberships"); got != 2 {
		t.Fatalf("memberships rows = %d; want the tenant-wide row plus the project row", got)
	}

	// A project row does not satisfy a tenant-wide absence guard.
	tenantView := newAbsenceReference(membershipSeedRow("membership-project", "tenant-1", "project-1", "principal-1"))
	tenantScope := tenancy.Scope{TenantID: id.TenantID("tenant-1")}
	txn, err = tenantView.begin(tenantScope, tenantWideGrantUnit("membership-tenant-2", "principal-1"))
	if err != nil {
		t.Fatalf("a tenant-wide absence guard conflicted with a project row: %v", err)
	}
	if err := txn.commit(); err != nil {
		t.Fatalf("commit a tenant-wide grant with a project row present: %v", err)
	}
	if got := tenantView.count("memberships"); got != 2 {
		t.Fatalf("memberships rows = %d; want the project row plus the tenant-wide row", got)
	}
}

// TestAbsenceVectorsDetectBrokenReference proves the behavior vectors are not
// vacuous: a reference that drops the commit-time absence recheck commits the
// guarded unit and persists its unrelated write, so the create-between vector
// fails it.
func TestAbsenceVectorsDetectBrokenReference(t *testing.T) {
	reference := newAbsenceReference()
	reference.enforce = false
	scope := absenceScope("tenant-1", "project-1")

	guarded, err := reference.begin(scope, store.Unit{
		Locks:  []store.Lock{membershipAbsenceLock("tenant-1", "project-1", "principal-1")},
		Writes: []store.Write{membershipInsert("membership-other", "principal-2")},
	})
	if err != nil {
		t.Fatalf("the broken reference refused at guard time: %v", err)
	}
	concurrent, err := reference.begin(scope, membershipGrantUnit("membership-creator", "principal-1"))
	if err != nil {
		t.Fatalf("the broken reference refused the concurrent creator: %v", err)
	}
	if err := concurrent.commit(); err != nil {
		t.Fatalf("the broken reference refused the concurrent commit: %v", err)
	}
	if err := guarded.commit(); err != nil {
		t.Fatalf("the deliberately broken reference already refused the guarded commit: %v", err)
	}
	if !reference.hasID("memberships", "membership-other") {
		t.Fatal("the broken reference did not commit the guarded unit; the vector would be vacuous")
	}
}

// TestAbsenceGuardsFollowFrozenLockOrder pins the ordering rule: an absence
// guard ranks with its table's write rank in the frozen lock order, and two
// guards on the same table are ordered by their canonical unique key (declared
// column order, NULL before any value).
func TestAbsenceGuardsFollowFrozenLockOrder(t *testing.T) {
	order := store.SchemaDesign().LockOrder
	membershipRank := indexOfString(order, "memberships")
	taskRank := indexOfString(order, "tasks")
	if membershipRank < 0 || taskRank < 0 || membershipRank >= taskRank {
		t.Fatalf("frozen lock order = %v; want memberships before tasks", order)
	}

	// The input lists the later table first; the plan must reorder.
	plan, err := referenceAbsencePlan([]store.Lock{
		{Table: "tasks", IDs: []string{"task-1"}},
		membershipAbsenceLock("tenant-1", "project-1", "principal-1"),
	})
	if err != nil {
		t.Fatalf("plan absence guards: %v", err)
	}
	if got := []string{plan[0].table, plan[1].table}; !reflect.DeepEqual(got, []string{"memberships", "tasks"}) {
		t.Fatalf("planned lock order = %v; want [memberships tasks]", got)
	}
	if plan[0].rank != membershipRank {
		t.Fatalf("absence guard rank = %d; want the memberships write rank %d", plan[0].rank, membershipRank)
	}

	// A present-row lock on memberships has the same rank as its absence guard.
	present, err := referenceAbsencePlan([]store.Lock{{Table: "memberships", IDs: []string{"membership-1"}, ExpectVersion: 1}})
	if err != nil {
		t.Fatalf("plan a present membership lock: %v", err)
	}
	if present[0].rank != plan[0].rank {
		t.Fatalf("present membership rank = %d; absence rank = %d; they must share the table write rank", present[0].rank, plan[0].rank)
	}

	// Two guards on the same table sort by canonical key, tenant-wide NULL first.
	plan, err = referenceAbsencePlan([]store.Lock{
		membershipAbsenceLock("tenant-1", "project-2", "principal-1"),
		membershipAbsenceLock("tenant-1", "", "principal-1"),
		membershipAbsenceLock("tenant-1", "project-1", "principal-1"),
	})
	if err != nil {
		t.Fatalf("plan two same-table absence guards: %v", err)
	}
	want := []string{
		absenceKeyTextFor("tenant-1", "", "principal-1"),
		absenceKeyTextFor("tenant-1", "project-1", "principal-1"),
		absenceKeyTextFor("tenant-1", "project-2", "principal-1"),
	}
	for position, key := range want {
		if plan[position].key != key {
			t.Fatalf("guard %d key = %q; want %q (canonical key order)", position, plan[position].key, key)
		}
	}
}

// TestAbsenceGuardImplementedInStores proves store.AbsenceGuardImplemented is
// the single switch the concrete adapters consult: with the declaration true
// the memory and PostgreSQL adapters execute an absence-guarded unit through
// its first-grant plan rather than refusing it before planning. Both adapters
// commit the first grant and refuse a second grant on the same key with
// version_conflict even though it carries a different row id. The PostgreSQL
// statement plan and its two-session vector live in the adapter unit suite;
// this test does not connect to a real PostgreSQL server.
func TestAbsenceGuardImplementedInStores(t *testing.T) {
	if !store.AbsenceGuardImplemented {
		t.Fatal("store.AbsenceGuardImplemented is false; the store adapters must implement the absence plans")
	}
	scope := absenceScope("tenant-1", "project-1")
	unit := membershipGrantUnit("membership-1", "principal-1")
	if !unit.HasAbsenceGuard() {
		t.Fatal("a unit carrying ExpectAbsent is not reported as absence-guarded")
	}

	storage := memory.New()
	if _, err := storage.Execute(context.Background(), scope, unit); err != nil {
		t.Fatalf("memory adapter refused the implemented first grant: %v", err)
	}
	duplicate := membershipGrantUnit("membership-2", "principal-1")
	_, err := storage.Execute(context.Background(), scope, duplicate)
	assertFaultClass(t, err, fault.VersionConflict, "memory absence guard on a present membership")

	pgConn := &recordingAbsenceConnection{memberships: make(map[string]bool)}
	if _, err := storepg.ExecuteUnit(context.Background(), pgConn, scope, unit); err != nil {
		t.Fatalf("PostgreSQL adapter refused the implemented first grant: %v", err)
	}
	wantFirstSteps := []string{"advisory", "advisory", "absence_select", "insert_memberships", "commit"}
	if len(pgConn.txs) != 1 || !reflect.DeepEqual(pgConn.txs[0].steps, wantFirstSteps) {
		t.Fatalf("PostgreSQL first grant steps = %v; want %v", pgConn.stepsOf(0), wantFirstSteps)
	}

	_, err = storepg.ExecuteUnit(context.Background(), pgConn, scope, duplicate)
	assertFaultClass(t, err, fault.VersionConflict, "PostgreSQL absence guard on a present membership")
	wantDuplicateSteps := []string{"advisory", "advisory", "absence_select", "rollback"}
	if len(pgConn.txs) != 2 || !reflect.DeepEqual(pgConn.txs[1].steps, wantDuplicateSteps) {
		t.Fatalf("PostgreSQL duplicate grant steps = %v; want %v", pgConn.stepsOf(1), wantDuplicateSteps)
	}
}

type recordingAbsenceConnection struct {
	memberships map[string]bool
	txs         []*recordingAbsenceTx
}

func (c *recordingAbsenceConnection) Begin(_ context.Context, scope tenancy.Scope) (store.Transaction, error) {
	tx := &recordingAbsenceTx{conn: c, scope: scope}
	c.txs = append(c.txs, tx)
	return tx, nil
}

func (c *recordingAbsenceConnection) stepsOf(index int) []string {
	if index < 0 || index >= len(c.txs) {
		return nil
	}
	return c.txs[index].steps
}

type recordingAbsenceTx struct {
	conn   *recordingAbsenceConnection
	scope  tenancy.Scope
	steps  []string
	staged []string
	closed bool
}

func (tx *recordingAbsenceTx) Exec(_ context.Context, stmt store.Statement) (store.Result, error) {
	sql := strings.ToLower(strings.TrimSpace(stmt.SQL))
	switch {
	case strings.Contains(sql, "pg_advisory_xact_lock"):
		tx.steps = append(tx.steps, "advisory")
		return store.Result{}, nil
	case strings.HasPrefix(sql, "insert into memberships"):
		tx.steps = append(tx.steps, "insert_memberships")
		if len(stmt.Args) >= 4 {
			project := ""
			if stmt.Args[2] != nil {
				project = stringValue(stmt.Args[2])
			}
			key := absenceKeyTextFor(stringValue(stmt.Args[1]), project, stringValue(stmt.Args[3]))
			tx.staged = append(tx.staged, key)
		}
		return store.Result{RowsAffected: 1}, nil
	default:
		tx.steps = append(tx.steps, "exec")
		return store.Result{RowsAffected: 1}, nil
	}
}

func (tx *recordingAbsenceTx) Query(_ context.Context, query store.Query) (store.Rows, error) {
	sql := strings.ToLower(strings.TrimSpace(query.SQL))
	if strings.HasPrefix(sql, "select 1 from memberships") && strings.Contains(sql, "is not distinct from") && len(query.Args) >= 3 {
		tx.steps = append(tx.steps, "absence_select")
		project := ""
		if query.Args[1] != nil {
			project = stringValue(query.Args[1])
		}
		key := absenceKeyTextFor(stringValue(query.Args[0]), project, stringValue(query.Args[2]))
		if tx.conn.memberships[key] {
			return &recordingAbsenceRows{remaining: 1}, nil
		}
		for _, stagedKey := range tx.staged {
			if stagedKey == key {
				return &recordingAbsenceRows{remaining: 1}, nil
			}
		}
		return &recordingAbsenceRows{remaining: 0}, nil
	}
	tx.steps = append(tx.steps, "query")
	return &recordingAbsenceRows{remaining: 0}, nil
}

func (tx *recordingAbsenceTx) Commit(context.Context) error {
	if tx.closed {
		return nil
	}
	tx.closed = true
	tx.steps = append(tx.steps, "commit")
	for _, key := range tx.staged {
		tx.conn.memberships[key] = true
	}
	return nil
}

func (tx *recordingAbsenceTx) Rollback(context.Context) error {
	if tx.closed {
		return nil
	}
	tx.closed = true
	tx.steps = append(tx.steps, "rollback")
	tx.staged = nil
	return nil
}

type recordingAbsenceRows struct {
	remaining int
}

func (r *recordingAbsenceRows) Next() bool {
	if r.remaining > 0 {
		r.remaining--
		return true
	}
	return false
}

func (r *recordingAbsenceRows) Scan(...any) error { return nil }
func (r *recordingAbsenceRows) Err() error        { return nil }
func (r *recordingAbsenceRows) Close() error      { r.remaining = 0; return nil }

// assertFaultClass requires err to be a fault of the exact expected class.
func assertFaultClass(t *testing.T, err error, want fault.Class, context string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: succeeded; want fault class %q", context, want)
	}
	class, ok := fault.ClassOf(err)
	if !ok || class != want {
		t.Fatalf("%s: fault class = %q (classified=%v, err=%v); want %q", context, class, ok, err, want)
	}
}

func indexOfString(values []string, want string) int {
	for position, value := range values {
		if value == want {
			return position
		}
	}
	return -1
}

func absenceScope(tenant, project string) tenancy.Scope {
	scope := tenancy.Scope{TenantID: id.TenantID(tenant)}
	if project != "" {
		scope.ProjectID = id.ProjectID(project)
	}
	return scope
}

func membershipProject(project string) any {
	if project == "" {
		return nil
	}
	return project
}

func membershipAbsenceLock(tenant, project, principal string) store.Lock {
	return store.Lock{
		Table:        "memberships",
		ExpectAbsent: true,
		Key: map[string]any{
			"tenant_id":    tenant,
			"project_id":   membershipProject(project),
			"principal_id": principal,
		},
	}
}

func membershipInsert(rowID, principal string) store.Write {
	return store.Write{
		Table:  "memberships",
		ID:     rowID,
		Insert: true,
		Values: map[string]any{
			"principal_id": principal,
			"roles":        []byte(`["engineer"]`),
			"version":      int64(1),
		},
	}
}

func membershipGrantUnit(rowID, principal string) store.Unit {
	return store.Unit{
		Locks:  []store.Lock{membershipAbsenceLock("tenant-1", "project-1", principal)},
		Writes: []store.Write{membershipInsert(rowID, principal)},
	}
}

func tenantWideGrantUnit(rowID, principal string) store.Unit {
	return store.Unit{
		Locks:  []store.Lock{membershipAbsenceLock("tenant-1", "", principal)},
		Writes: []store.Write{membershipInsert(rowID, principal)},
	}
}

func membershipSeedRow(rowID, tenant, project, principal string) store.Row {
	return store.Row{
		Table: "memberships",
		Values: map[string]any{
			"id":           rowID,
			"tenant_id":    tenant,
			"project_id":   membershipProject(project),
			"principal_id": principal,
			"roles":        "engineer",
			"version":      uint64(1),
		},
	}
}

// absenceReference is the deterministic reference interpreter for the absence
// guard. Guard time and commit time are separate so a vector can create the
// matching row in the barrier between them.
type absenceReference struct {
	enforce bool
	rows    map[string]absenceRow
}

type absenceRow struct {
	table string
	id    string
	key   string
}

func newAbsenceReference(seed ...store.Row) *absenceReference {
	reference := &absenceReference{enforce: true, rows: make(map[string]absenceRow, len(seed))}
	for _, row := range seed {
		key, err := absenceRowKey(row.Table, row.Values)
		if err != nil {
			panic(fmt.Sprintf("seed row %s/%v: %v", row.Table, row.Values["id"], err))
		}
		reference.rows[key] = absenceRow{table: row.Table, id: stringValue(row.Values["id"]), key: key}
	}
	return reference
}

func (reference *absenceReference) count(table string) int {
	count := 0
	for _, row := range reference.rows {
		if row.table == table {
			count++
		}
	}
	return count
}

func (reference *absenceReference) hasID(table, id string) bool {
	for _, row := range reference.rows {
		if row.table == table && row.id == id {
			return true
		}
	}
	return false
}

func (reference *absenceReference) begin(scope tenancy.Scope, unit store.Unit) (*absenceTransaction, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if err := unit.Validate(); err != nil {
		return nil, err
	}
	if err := reference.assertAbsent(unit.Locks); err != nil {
		return nil, err
	}
	return &absenceTransaction{reference: reference, scope: scope, unit: unit}, nil
}

func (reference *absenceReference) assertAbsent(locks []store.Lock) error {
	if !reference.enforce {
		return nil
	}
	for _, lock := range locks {
		if !lock.ExpectAbsent {
			continue
		}
		key, err := absenceKeyText(lock)
		if err != nil {
			return fault.New(fault.InvalidInput, err.Error())
		}
		if row, present := reference.rows[key]; present {
			return fault.New(fault.VersionConflict, fmt.Sprintf("an absence guard matched row %q", row.id))
		}
	}
	return nil
}

type absenceTransaction struct {
	reference *absenceReference
	scope     tenancy.Scope
	unit      store.Unit
}

func (txn *absenceTransaction) commit() error {
	// Re-check every absence guard at the commit barrier: a row created
	// between guard time and commit is a version_conflict, and no write of the
	// unit is persisted.
	if err := txn.reference.assertAbsent(txn.unit.Locks); err != nil {
		return err
	}
	staged := make(map[string]absenceRow, len(txn.unit.Writes))
	for _, write := range txn.unit.Writes {
		if !write.Insert {
			return fault.New(fault.InvalidInput, "the reference absence interpreter models inserts only")
		}
		values := map[string]any{
			"tenant_id":  string(txn.scope.TenantID),
			"project_id": membershipProject(string(txn.scope.ProjectID)),
			"id":         write.ID,
		}
		for column, value := range write.Values {
			values[column] = value
		}
		key, err := absenceRowKey(write.Table, values)
		if err != nil {
			return fault.New(fault.InvalidInput, err.Error())
		}
		if existing, present := txn.reference.rows[key]; present {
			return fault.New(fault.VersionConflict, fmt.Sprintf("a unique-key row %q already exists", existing.id))
		}
		if existing, present := staged[key]; present {
			return fault.New(fault.VersionConflict, fmt.Sprintf("two inserts in one unit share a unique key with row %q", existing.id))
		}
		staged[key] = absenceRow{table: write.Table, id: write.ID, key: key}
	}
	for key, row := range staged {
		txn.reference.rows[key] = row
	}
	return nil
}

// absenceRowKey renders the registered unique-key identity of one row or guard
// key. All registered columns must be present; project_id may be nil for a
// tenant-wide row.
func absenceRowKey(table string, values map[string]any) (string, error) {
	columns, ok := store.AbsenceKeyColumns(table)
	if !ok {
		return "", fmt.Errorf("table %q registers no absence-guardable unique key", table)
	}
	parts := make([]string, 0, len(columns)+1)
	parts = append(parts, table)
	for _, column := range columns {
		value, present := values[column]
		if !present {
			return "", fmt.Errorf("absence key for table %q is missing column %q", table, column)
		}
		parts = append(parts, absenceKeyValue(value))
	}
	// The NUL separator sorts below every value byte, so a NULL project sorts
	// before any project name and two same-table guards order deterministically.
	return strings.Join(parts, "\x00"), nil
}

func absenceKeyText(lock store.Lock) (string, error) {
	return absenceRowKey(lock.Table, lock.Key)
}

func absenceKeyTextFor(tenant, project, principal string) string {
	key, err := absenceRowKey("memberships", map[string]any{
		"tenant_id":    tenant,
		"project_id":   membershipProject(project),
		"principal_id": principal,
	})
	if err != nil {
		panic(err)
	}
	return key
}

func absenceKeyValue(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprintf("%v", value)
}

func stringValue(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprintf("%v", value)
}

// absencePlanEntry is one planned lock after the frozen table order and the
// canonical unique-key order have been applied.
type absencePlanEntry struct {
	table    string
	absence  bool
	key      string
	rank     int
	position int
}

// referenceAbsencePlan orders absence guards with the frozen lock order. An
// absence guard ranks with its table's write rank; two guards on the same table
// are ordered by their canonical unique key.
func referenceAbsencePlan(locks []store.Lock) ([]absencePlanEntry, error) {
	ranks := make(map[string]int, len(store.SchemaDesign().LockOrder))
	for position, table := range store.SchemaDesign().LockOrder {
		ranks[table] = position
	}
	entries := make([]absencePlanEntry, 0, len(locks))
	for position, lock := range locks {
		rank, ok := ranks[lock.Table]
		if !ok {
			return nil, fmt.Errorf("table %q is not in the frozen lock order", lock.Table)
		}
		entry := absencePlanEntry{table: lock.Table, absence: lock.ExpectAbsent, rank: rank, position: position}
		if lock.ExpectAbsent {
			key, err := absenceKeyText(lock)
			if err != nil {
				return nil, err
			}
			entry.key = key
		}
		entries = append(entries, entry)
	}
	sort.SliceStable(entries, func(left, right int) bool {
		if entries[left].rank != entries[right].rank {
			return entries[left].rank < entries[right].rank
		}
		if entries[left].absence && entries[right].absence && entries[left].key != entries[right].key {
			return entries[left].key < entries[right].key
		}
		return entries[left].position < entries[right].position
	})
	return entries, nil
}
