// Absence-guard conformance for the memory adapter. The shared declaration and
// its reference vectors live in test/contracts/adapters; these tests exercise
// the same semantics against the real Store. The production gate follows
// store.AbsenceGuardImplemented, which is true because both adapters implement
// the guard. Most tests here set the adapter-local gate explicitly to state
// their intent; the default-gate test proves the declared default and the
// forced-off refusal.
//
// The memory adapter is one shared state behind one mutex, so "guard time" and
// "commit time" are the same atomic section: a guarded unit re-evaluates its
// absence guards against the committed state before any write and publishes the
// whole staged copy at once. The concurrent first-grant test is therefore the
// create-between-guard-and-commit proof for this adapter: two units that both
// observe the key absent cannot both commit.
package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/store"
)

// enableAbsenceGuard sets the adapter-local gate on for one test and
// restores it afterwards. Production follows the store declaration.
func enableAbsenceGuard(t *testing.T) {
	t.Helper()
	previous := absenceGuard
	absenceGuard = true
	t.Cleanup(func() { absenceGuard = previous })
}

// absenceMembershipLock names the registered membership unique key. An empty
// project is the tenant-wide identity (project_id NULL), never an omitted
// column.
func absenceMembershipLock(project, principal string) store.Lock {
	var projectValue any
	if project != "" {
		projectValue = project
	}
	return store.Lock{
		Table:        "memberships",
		ExpectAbsent: true,
		Key: map[string]any{
			"tenant_id":    testTenantID,
			"project_id":   projectValue,
			"principal_id": principal,
		},
	}
}

// membershipValues returns one membership row for the named principal.
func membershipValues(principal string) map[string]any {
	values := insertValues("memberships")
	values["principal_id"] = principal
	return values
}

// projectFirstGrantUnit is the create-if-absent shape: the absence guard on
// the membership key and the insert of the same key in one commit unit.
func projectFirstGrantUnit(rowID, principal string) store.Unit {
	return store.Unit{
		Locks:  []store.Lock{absenceMembershipLock(testProjectID, principal)},
		Writes: []store.Write{{Table: "memberships", ID: rowID, Insert: true, Values: membershipValues(principal)}},
	}
}

// tenantWideFirstGrantUnit is the tenant-wide create-if-absent shape
// (project_id NULL).
func tenantWideFirstGrantUnit(rowID, principal string) store.Unit {
	return store.Unit{
		Locks:  []store.Lock{absenceMembershipLock("", principal)},
		Writes: []store.Write{{Table: "memberships", ID: rowID, Insert: true, Values: membershipValues(principal)}},
	}
}

// TestAbsenceGuardCommitsOnAbsentRow proves an absence guard on a key with no
// matching row commits, and the first grant inserts exactly one row.
func TestAbsenceGuardCommitsOnAbsentRow(t *testing.T) {
	enableAbsenceGuard(t)
	s := New()
	scope := projectScope()

	mustExecute(t, s, scope, projectFirstGrantUnit("membership-1", testPrincipalID))

	row := mustSnapshot(t, s, scope, "memberships", "membership-1")
	if got := rowString(t, row, "principal_id"); got != testPrincipalID {
		t.Errorf("granted principal_id = %q; want %q", got, testPrincipalID)
	}
	if got := rowString(t, row, "project_id"); got != testProjectID {
		t.Errorf("granted project_id = %q; want %q", got, testProjectID)
	}
}

// TestAbsenceGuardPresentRowConflictsWithoutEffect proves a matching row at
// commit time is version_conflict (never forbidden or not_found) and the whole
// unit rolls back: business row, receipt, event and audit.
func TestAbsenceGuardPresentRowConflictsWithoutEffect(t *testing.T) {
	enableAbsenceGuard(t)
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "memberships", "membership-existing", membershipValues(testPrincipalID))

	record := receiptRecord(t, "receipt-guard",
		keyFor(testTenantID, testProjectID, testPrincipalID, "membership.grant", "caller-guard"),
		`{"guard":"present"}`, `{"committed":false}`)
	unit := store.Unit{
		Locks:   []store.Lock{absenceMembershipLock(testProjectID, testPrincipalID)},
		Writes:  []store.Write{{Table: "executions", ID: "execution-2", Insert: true, Values: executionValues(1, "queued")}},
		Receipt: &record,
		Events:  []event.Envelope{defaultEvent(scope, "event-guard", event.TypeExecutionQueued, "execution-2")},
		Audit:   []event.Audit{defaultAudit(scope, "audit-guard", "membership.grant")},
	}

	_, err := s.Execute(context.Background(), scope, unit)
	assertFault(t, err, fault.VersionConflict)

	assertAbsent(t, s, scope, "executions", "execution-2")
	assertAbsent(t, s, scope, "memberships", "membership-1")
	if _, found := s.Receipt(record.Key); found {
		t.Fatal("a refused absence-guarded unit persisted its receipt")
	}
	if got := s.Events(scope); len(got) != 0 {
		t.Errorf("committed events = %v; want none", eventIDs(got))
	}
	if got := s.Audit(scope); len(got) != 0 {
		t.Errorf("committed audit rows = %v; want none", auditOperations(got))
	}
	existing := mustSnapshot(t, s, scope, "memberships", "membership-existing")
	if got := rowVersion(t, existing, "version"); got != 1 {
		t.Errorf("seeded membership version = %d; want 1", got)
	}
}

// TestAbsenceGuardFirstGrantThenBlocksTheNextUnit proves the create-if-absent
// unit commits when the key is absent, the insert makes the key present, and the
// next first grant on the same key is a version_conflict with no row.
func TestAbsenceGuardFirstGrantThenBlocksTheNextUnit(t *testing.T) {
	enableAbsenceGuard(t)
	s := New()
	scope := projectScope()

	mustExecute(t, s, scope, projectFirstGrantUnit("membership-1", testPrincipalID))

	_, err := s.Execute(context.Background(), scope, projectFirstGrantUnit("membership-2", testPrincipalID))
	assertFault(t, err, fault.VersionConflict)
	assertAbsent(t, s, scope, "memberships", "membership-2")
	mustSnapshot(t, s, scope, "memberships", "membership-1")
}

// TestConcurrentFirstGrantsExactlyOneWins is the adapter's
// create-between-guard-and-commit proof: concurrent units that both see the key
// absent serialize under the one store mutex, so exactly one commits and the
// loser leaves no row, receipt, event or audit row behind.
func TestConcurrentFirstGrantsExactlyOneWins(t *testing.T) {
	enableAbsenceGuard(t)
	s := New()
	scope := projectScope()
	ids := []string{"membership-a", "membership-b", "membership-c", "membership-d"}

	units := make([]store.Unit, len(ids))
	for index, rowID := range ids {
		units[index] = projectFirstGrantUnit(rowID, testPrincipalID)
	}
	errs := runConcurrently(t, len(units), func(index int) error {
		_, err := s.Execute(context.Background(), scope, units[index])
		return err
	})
	if got := countNil(errs); got != 1 {
		t.Fatalf("concurrent first-grant winners = %d; want exactly 1 (%s)", got, describeErrors(errs))
	}
	if got := countFault(t, errs, fault.VersionConflict); got != len(units)-1 {
		t.Fatalf("concurrent first-grant conflicts = %d; want %d (%s)", got, len(units)-1, describeErrors(errs))
	}

	winner := -1
	for index, err := range errs {
		if err == nil {
			winner = index
			continue
		}
		assertAbsent(t, s, scope, "memberships", ids[index])
	}
	if winner < 0 {
		t.Fatal("no concurrent first grant committed")
	}
	mustSnapshot(t, s, scope, "memberships", ids[winner])

	// Scoped reads expose only the winner; another project sees nothing.
	if _, err := s.Read(context.Background(), otherProjectScope(), "memberships", ids[winner]); err == nil {
		t.Fatal("another project read the winning membership")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		t.Fatalf("cross-project read fault = %v; want not_found", err)
	}

	// The key stays present for every later unit.
	_, err := s.Execute(context.Background(), scope, projectFirstGrantUnit("membership-late", testPrincipalID))
	assertFault(t, err, fault.VersionConflict)
	assertAbsent(t, s, scope, "memberships", "membership-late")
}

// TestAbsenceGuardTenantWideAndProjectKeysDiffer proves project_id is part of
// the identity: a tenant-wide row (NULL project) and a project row are different
// keys, so neither absence guard conflicts with the other.
func TestAbsenceGuardTenantWideAndProjectKeysDiffer(t *testing.T) {
	enableAbsenceGuard(t)

	// A tenant-wide row does not satisfy a project absence guard.
	projectView := New()
	tenant := tenantScope()
	project := projectScope()
	seedRow(t, projectView, tenant, "memberships", "membership-tenant", membershipValues(testPrincipalID))
	mustExecute(t, projectView, project, projectFirstGrantUnit("membership-project", testPrincipalID))
	mustSnapshot(t, projectView, tenant, "memberships", "membership-tenant")
	mustSnapshot(t, projectView, project, "memberships", "membership-project")

	// A project row does not satisfy a tenant-wide absence guard.
	tenantView := New()
	seedRow(t, tenantView, project, "memberships", "membership-project", membershipValues(testPrincipalID))
	mustExecute(t, tenantView, tenant, tenantWideFirstGrantUnit("membership-tenant-2", testPrincipalID))
	mustSnapshot(t, tenantView, project, "memberships", "membership-project")
	tenantRow := mustSnapshot(t, tenantView, tenant, "memberships", "membership-tenant-2")
	if got := tenantRow.Values["project_id"]; got != nil {
		t.Errorf("tenant-wide membership project_id = %v; want NULL", got)
	}
}

// TestAbsenceGuardRevokeThenRegrant proves a revoked row makes the key absent
// again while the revoked identity is never reused: the regrant mints a new row
// id and succeeds.
func TestAbsenceGuardRevokeThenRegrant(t *testing.T) {
	enableAbsenceGuard(t)
	s := New()
	scope := projectScope()

	mustExecute(t, s, scope, projectFirstGrantUnit("membership-old", testPrincipalID))
	mustExecute(t, s, scope, store.Unit{Writes: []store.Write{
		{Table: "memberships", ID: "membership-old", Delete: true, ExpectVersion: 1},
	}})
	assertAbsent(t, s, scope, "memberships", "membership-old")

	mustExecute(t, s, scope, projectFirstGrantUnit("membership-new", testPrincipalID))
	row := mustSnapshot(t, s, scope, "memberships", "membership-new")
	if got := rowVersion(t, row, "version"); got != 1 {
		t.Errorf("regranted membership version = %d; want 1", got)
	}
	assertAbsent(t, s, scope, "memberships", "membership-old")
}

// TestAbsenceGuardTwoGuardsInOneUnit proves one unit may carry several absence
// guards: both are evaluated before any write, so two absent keys both insert,
// and a later unit conflicts on either one.
func TestAbsenceGuardTwoGuardsInOneUnit(t *testing.T) {
	enableAbsenceGuard(t)
	s := New()
	scope := projectScope()

	unit := store.Unit{
		Locks: []store.Lock{
			absenceMembershipLock(testProjectID, "principal-2"),
			absenceMembershipLock(testProjectID, "principal-1"),
		},
		Writes: []store.Write{
			{Table: "memberships", ID: "membership-2", Insert: true, Values: membershipValues("principal-2")},
			{Table: "memberships", ID: "membership-1", Insert: true, Values: membershipValues("principal-1")},
		},
	}
	mustExecute(t, s, scope, unit)
	mustSnapshot(t, s, scope, "memberships", "membership-1")
	mustSnapshot(t, s, scope, "memberships", "membership-2")

	_, err := s.Execute(context.Background(), scope, store.Unit{
		Locks:  []store.Lock{absenceMembershipLock(testProjectID, "principal-1")},
		Writes: []store.Write{{Table: "memberships", ID: "membership-3", Insert: true, Values: membershipValues("principal-1")}},
	})
	assertFault(t, err, fault.VersionConflict)
	assertAbsent(t, s, scope, "memberships", "membership-3")
}

// TestAbsenceGuardOrdersWithOtherLocks proves an absence guard validates and
// applies alongside an unrelated row lock regardless of listing order, and that
// the planned order is the declared lock order with the guard ranked as a
// memberships lock and same-table guards ordered by their canonical key.
func TestAbsenceGuardOrdersWithOtherLocks(t *testing.T) {
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "tasks", testTaskID, taskValues(1, "open"))

	plan, err := planLocks(scope, []store.Lock{
		{Table: "tasks", IDs: []string{testTaskID}},
		absenceMembershipLock(testProjectID, testPrincipalID),
	})
	if err != nil {
		t.Fatalf("plan an absence guard with a task lock: %v", err)
	}
	if len(plan) != 2 {
		t.Fatalf("planned locks = %d; want 2 (the absence guard must be planned)", len(plan))
	}
	if plan[0].table != "memberships" || plan[1].table != "tasks" {
		t.Fatalf("planned lock order = [%s %s]; want [memberships tasks]", plan[0].table, plan[1].table)
	}
	if plan[0].rank != writeRank("memberships") {
		t.Fatalf("absence guard rank = %v; want the memberships write rank %v", plan[0].rank, writeRank("memberships"))
	}

	// Same-table guards sort by canonical key: feed principal-2 first and the
	// plan must put principal-1 (the smaller key) first.
	plan, err = planLocks(scope, []store.Lock{
		absenceMembershipLock(testProjectID, "principal-2"),
		absenceMembershipLock(testProjectID, "principal-1"),
	})
	if err != nil {
		t.Fatalf("plan two same-table absence guards: %v", err)
	}
	if len(plan) != 2 {
		t.Fatalf("planned guards = %d; want 2", len(plan))
	}
	if plan[0].position != 1 || plan[1].position != 0 {
		t.Fatalf("granted guard positions = [%d %d]; want [1 0] (smaller key first)", plan[0].position, plan[1].position)
	}

	// A NULL project sorts before any project value.
	plan, err = planLocks(tenantScope(), []store.Lock{
		absenceMembershipLock(testProjectID, testPrincipalID),
		absenceMembershipLock("", testPrincipalID),
	})
	if err != nil {
		t.Fatalf("plan tenant-wide and project absence guards: %v", err)
	}
	if len(plan) != 2 {
		t.Fatalf("planned guards = %d; want 2", len(plan))
	}
	if plan[0].position != 1 {
		t.Fatalf("tenant-wide guard position = %d; want 1 (NULL project sorts first)", plan[0].position)
	}

	enableAbsenceGuard(t)
	unit := store.Unit{
		Locks: []store.Lock{
			{Table: "tasks", IDs: []string{testTaskID}},
			absenceMembershipLock(testProjectID, testPrincipalID),
		},
		Writes: []store.Write{{Table: "memberships", ID: "membership-1", Insert: true, Values: membershipValues(testPrincipalID)}},
	}
	mustExecute(t, s, scope, unit)
	mustSnapshot(t, s, scope, "memberships", "membership-1")
}

// TestAbsenceGuardRollbackLeavesNoEffectAndReleasesTheKey proves a failure after
// the unit is fully staged publishes nothing, including the receipt, event and
// audit row, and leaves the guarded key absent for a later retry.
func TestAbsenceGuardRollbackLeavesNoEffectAndReleasesTheKey(t *testing.T) {
	enableAbsenceGuard(t)
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "tasks", testTaskID, taskValues(1, "open"))

	record := receiptRecord(t, "receipt-guard",
		keyFor(testTenantID, testProjectID, testPrincipalID, "membership.grant", "caller-guard"),
		`{"guard":"rollback"}`, `{"committed":true}`)
	unit := store.Unit{
		Locks: []store.Lock{
			absenceMembershipLock(testProjectID, testPrincipalID),
			{Table: "tasks", IDs: []string{testTaskID}},
		},
		Writes:  []store.Write{{Table: "memberships", ID: "membership-1", Insert: true, Values: membershipValues(testPrincipalID)}},
		Receipt: &record,
		Events:  []event.Envelope{defaultEvent(scope, "event-guard", event.TypeExecutionQueued, testExecutionID)},
		Audit:   []event.Audit{defaultAudit(scope, "audit-guard", "membership.grant")},
	}

	s.FailNextCommit(errAborted)
	if _, err := s.Execute(context.Background(), scope, unit); !errors.Is(err, errAborted) {
		t.Fatalf("injected commit failure = %v; want %v", err, errAborted)
	}
	assertAbsent(t, s, scope, "memberships", "membership-1")
	if _, found := s.Receipt(record.Key); found {
		t.Fatal("a failed absence-guarded unit persisted its receipt")
	}
	if got := s.Events(scope); len(got) != 0 {
		t.Errorf("committed events after a failed unit = %v; want none", eventIDs(got))
	}
	if got := s.Audit(scope); len(got) != 0 {
		t.Errorf("committed audit rows after a failed unit = %v; want none", auditOperations(got))
	}

	// The key is still absent, so the same unit commits after the failure.
	mustExecute(t, s, scope, unit)
	mustSnapshot(t, s, scope, "memberships", "membership-1")
	if _, found := s.Receipt(record.Key); !found {
		t.Fatal("the retried absence-guarded unit did not persist its receipt")
	}
}

// TestAbsenceGuardMalformedRefusedBeforeEffect proves every malformed guard is
// invalid_input before any effect. A malformed guard is never a wildcard.
func TestAbsenceGuardMalformedRefusedBeforeEffect(t *testing.T) {
	enableAbsenceGuard(t)
	projectKey := func() map[string]any {
		return map[string]any{"tenant_id": testTenantID, "project_id": testProjectID, "principal_id": testPrincipalID}
	}
	tenantWideKey := func() map[string]any {
		return map[string]any{"tenant_id": testTenantID, "project_id": nil, "principal_id": testPrincipalID}
	}
	cases := []struct {
		name string
		lock store.Lock
		// seed keeps a matching membership row present, so a case that was
		// wrongly accepted would fail the guard instead of committing.
		seed bool
	}{
		{name: "absence with ids", seed: true, lock: store.Lock{Table: "memberships", ExpectAbsent: true, Key: tenantWideKey(), IDs: []string{"membership-1"}}},
		{name: "absence with expect version", seed: true, lock: store.Lock{Table: "memberships", ExpectAbsent: true, Key: tenantWideKey(), ExpectVersion: 3}},
		{name: "absence with expect values", seed: true, lock: store.Lock{Table: "memberships", ExpectAbsent: true, Key: tenantWideKey(), Expect: map[string]any{"roles": "engineer"}}},
		{name: "absence tenant wide flag", seed: true, lock: store.Lock{Table: "memberships", ExpectAbsent: true, Key: tenantWideKey(), TenantWide: true}},
		{name: "absence with deadline", seed: true, lock: store.Lock{Table: "memberships", ExpectAbsent: true, Key: tenantWideKey(), Deadline: &store.DeadlineGuard{Column: "created_at"}}},
		{name: "absence without key", seed: true, lock: store.Lock{Table: "memberships", ExpectAbsent: true}},
		{name: "key without absence", seed: true, lock: store.Lock{Table: "memberships", Key: tenantWideKey()}},
		{name: "unregistered table", seed: true, lock: store.Lock{Table: "tasks", ExpectAbsent: true, Key: tenantWideKey()}},
		{name: "missing project column", seed: true, lock: store.Lock{Table: "memberships", ExpectAbsent: true, Key: map[string]any{"tenant_id": testTenantID, "principal_id": testPrincipalID}}},
		{name: "extra key column", seed: true, lock: store.Lock{Table: "memberships", ExpectAbsent: true, Key: map[string]any{"tenant_id": testTenantID, "project_id": testProjectID, "principal_id": testPrincipalID, "roles": "engineer"}}},
		{name: "empty tenant", seed: true, lock: store.Lock{Table: "memberships", ExpectAbsent: true, Key: map[string]any{"tenant_id": "", "project_id": nil, "principal_id": testPrincipalID}}},
		{name: "non-string project", seed: true, lock: store.Lock{Table: "memberships", ExpectAbsent: true, Key: map[string]any{"tenant_id": testTenantID, "project_id": 7, "principal_id": testPrincipalID}}},
		{name: "valid project absence", lock: store.Lock{Table: "memberships", ExpectAbsent: true, Key: projectKey()}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			s := New()
			scope := projectScope()
			if test.seed {
				seedRow(t, s, scope, "memberships", "membership-existing", membershipValues(testPrincipalID))
			}
			unit := store.Unit{
				Locks:  []store.Lock{test.lock},
				Writes: []store.Write{{Table: "memberships", ID: "membership-new", Insert: true, Values: membershipValues("principal-2")}},
			}
			_, err := s.Execute(context.Background(), scope, unit)
			if !test.seed {
				if err != nil {
					t.Fatalf("a valid absence guard was refused: %v", err)
				}
				mustSnapshot(t, s, scope, "memberships", "membership-new")
				return
			}
			assertFault(t, err, fault.InvalidInput)
			assertAbsent(t, s, scope, "memberships", "membership-new")
		})
	}
}

// TestAbsenceGuardControllerRefused proves the cross-scope controller never
// performs a membership authority check, so its unit carrying an absence guard
// is forbidden and nothing is staged.
func TestAbsenceGuardControllerRefused(t *testing.T) {
	enableAbsenceGuard(t)
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "executions", testExecutionID, executionValues(1, "queued"))

	_, err := s.Controller().Execute(context.Background(), testExecutionID, store.Unit{
		Locks:  []store.Lock{absenceMembershipLock(testProjectID, testPrincipalID)},
		Writes: []store.Write{{Table: "memberships", ID: "membership-1", Insert: true, Values: membershipValues(testPrincipalID)}},
	})
	assertFault(t, err, fault.Forbidden)
	assertAbsent(t, s, scope, "memberships", "membership-1")

	// Even a controller-writable table cannot carry an absence guard.
	_, err = s.Controller().Execute(context.Background(), testExecutionID, store.Unit{
		Locks: []store.Lock{{Table: "executions", IDs: []string{testExecutionID}, ExpectAbsent: true, Key: map[string]any{
			"tenant_id": testTenantID, "project_id": testProjectID, "principal_id": testPrincipalID,
		}}},
	})
	assertFault(t, err, fault.Forbidden)
}

// TestAbsenceGuardDefaultFollowsTheContractGate pins the adapter default to the
// declaration and proves a forced-off gate still fails closed with
// invalid_input before any effect.
func TestAbsenceGuardDefaultFollowsTheContractGate(t *testing.T) {
	if absenceGuard != store.AbsenceGuardImplemented {
		t.Fatalf("the memory absence gate = %v; want the declaration %v", absenceGuard, store.AbsenceGuardImplemented)
	}

	previous := absenceGuard
	absenceGuard = false
	t.Cleanup(func() { absenceGuard = previous })

	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "memberships", "membership-existing", membershipValues("principal-2"))

	_, err := s.Execute(context.Background(), scope, projectFirstGrantUnit("membership-1", testPrincipalID))
	assertFault(t, err, fault.InvalidInput)
	assertAbsent(t, s, scope, "memberships", "membership-1")

	// A unit without an absence guard is unaffected by the gate.
	mustExecute(t, s, scope, store.Unit{Writes: []store.Write{
		{Table: "executions", ID: "execution-gate", Insert: true, Values: executionValues(1, "queued")},
	}})
	mustSnapshot(t, s, scope, "executions", "execution-gate")
}
