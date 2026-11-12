package identity_test

// resource.binding.register ownership tests.
//
// These drive the REAL resource.Service (register/update/delete/read) onto the
// identity store adapter over the memory store. They pin the register rule: no
// other operation creates the `resources` row, so a registration that required
// it would always fail the resource_ownership authority read with not_found.
// Registration creates the absent row (identity = ResourceID, owner =
// committing scope, version 1) atomically with the binding, receipt, audit and
// event; a same-scope re-register reuses it; a foreign scope gets the uniform
// not_found and writes no binding; update/delete/read keep requiring the
// existing owned row.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/store"
	identityunits "github.com/optimiware/kodama/internal/shell/store/units/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// registerCommand builds one complete real-service registration command for a
// human Admin actor in the given project scope.
func registerCommand(scope tenancy.Scope, actor id.PrincipalID, bindingID, resourceID, callerKey string) resource.RegisterBindingCommand {
	return resource.RegisterBindingCommand{
		Actor:            humanPrincipal(scope, actor, tenancy.RoleAdmin),
		Scope:            scope,
		BindingID:        resource.BindingID(bindingID),
		ResourceID:       resource.ResourceID(resourceID),
		EnforcingService: "service-a",
		TrustedOrigins:   []string{"https://git.example"},
		Operations:       []resource.Operation{resource.OperationGitRefUpdate},
		ReportableEvents: []string{"git.ref.updated"},
		Metadata:         map[string]string{"repo": "kodama"},
		CallerKey:        callerKey,
		Content:          requestContent(callerKey),
	}
}

// newBindingService builds a real resource.Service over the adapter with its
// own deterministic identity source, so a concurrent test can run two services
// without sharing a mutable ID allocator.
func newBindingService(t *testing.T, port *identityunits.Adapter, ids resource.IDSource) *resource.Service {
	t.Helper()
	service, err := resource.NewService(resource.Config{
		Clock:    fixedClock{now: testNow},
		IDs:      ids,
		Bindings: port,
		Deletes:  port,
		Reads:    port,
		Queries:  port,
	})
	if err != nil {
		t.Fatalf("resource.NewService: %v", err)
	}
	return service
}

// newBindingAdapter builds an adapter over a chosen executor and scanner so a
// test can interpose a deterministic commit barrier on the register path.
func newBindingAdapter(t *testing.T, executor store.Executor, scanner store.Scanner) *identityunits.Adapter {
	t.Helper()
	adapter, err := identityunits.New(identityunits.Config{
		Executor: executor,
		Scanner:  scanner,
		Clock:    fixedClock{now: testNow},
		IDs:      &sequenceIDs{},
	})
	if err != nil {
		t.Fatalf("identityunits.New: %v", err)
	}
	return adapter
}

// registrationProbe wraps the store executor one binding service commits
// through. It counts the registration units that reach the store, inspects
// whether they carry the conditional resources insert, and can either commit a
// competing winner immediately before the first one or fail the first one with
// a chosen error. The latter makes the "winner rolled back" branch
// deterministic without inventing a committed row.
type registrationProbe struct {
	store.Executor
	beforeFirst     func()
	failFirst       error
	attempts        int
	resourceInserts int
}

func (probe *registrationProbe) Execute(ctx context.Context, scope tenancy.Scope, unit store.Unit) (store.UnitResult, error) {
	if unit.Receipt != nil {
		if probe.attempts == 0 && probe.beforeFirst != nil {
			probe.beforeFirst()
		}
		probe.attempts++
		for _, write := range unit.Writes {
			if write.Table == "resources" && write.Insert {
				probe.resourceInserts++
			}
		}
		if probe.failFirst != nil {
			failure := probe.failFirst
			probe.failFirst = nil
			return store.UnitResult{}, failure
		}
	}
	return probe.Executor.Execute(ctx, scope, unit)
}

// barrierExecutor holds the first registration unit of one service until every
// participating service has read the resources row and reached its commit, so
// two registers of one new ResourceID deterministically race their conditional
// inserts.
type barrierExecutor struct {
	store.Executor
	arrive  chan<- struct{}
	release <-chan struct{}
	fired   bool
}

func (executor *barrierExecutor) Execute(ctx context.Context, scope tenancy.Scope, unit store.Unit) (store.UnitResult, error) {
	if !executor.fired && unit.Receipt != nil {
		executor.fired = true
		executor.arrive <- struct{}{}
		<-executor.release
	}
	return executor.Executor.Execute(ctx, scope, unit)
}

// registerReceiptKey names the durable receipt of one real register command.
func registerReceiptKey(scope tenancy.Scope, actor id.PrincipalID, callerKey string) receipt.Key {
	return receipt.Key{
		TenantID:    scope.TenantID,
		ProjectID:   scope.ProjectID,
		PrincipalID: actor,
		Operation:   "resource.binding.register",
		CallerKey:   callerKey,
	}
}

// requireResourceRow asserts the committed resources row's identity, owner and
// version. The rule pins exactly those three facts; the schema's kind and
// metadata columns are adapter-supplied and are not part of the rule.
func requireResourceRow(t *testing.T, h *harness, scope tenancy.Scope, resourceID resource.ResourceID, wantVersion uint64) {
	t.Helper()
	row, found := h.snapshot(scope, "resources", string(resourceID))
	if !found {
		t.Fatalf("resources/%s was not committed in scope %#v", resourceID, scope)
	}
	if got := mustInt(t, row, "version"); uint64(got) != wantVersion {
		t.Fatalf("resources/%s version = %d; want %d", resourceID, got, wantVersion)
	}
	if got := row.Values["tenant_id"]; got != string(scope.TenantID) {
		t.Fatalf("resources/%s tenant = %v; want %q", resourceID, got, scope.TenantID)
	}
	if got := row.Values["project_id"]; got != string(scope.ProjectID) {
		t.Fatalf("resources/%s project = %v; want %q", resourceID, got, scope.ProjectID)
	}
}

// requireResourceAbsent asserts no resources row is visible in the scope.
func requireResourceAbsent(t *testing.T, h *harness, scope tenancy.Scope, resourceID resource.ResourceID) {
	t.Helper()
	if _, found := h.snapshot(scope, "resources", string(resourceID)); found {
		t.Fatalf("resources/%s was committed in scope %#v; want no row", resourceID, scope)
	}
}

// TestRealRegisterCreatesAbsentResourceRow is a regression test: the
// first registration of a brand-new ResourceID commits and creates the
// resources row at version 1 in the same unit as the binding. It also pins the
// authority-read split: absence satisfies the register read but the report
// read (and update/delete/read) still require an existing owned row, and the
// row register creates makes that later read succeed.
func TestRealRegisterCreatesAbsentResourceRow(t *testing.T) {
	ctx := context.Background()
	r := newRealServices(t)
	scope := projectScope("tenant-a", "project-a")
	actor := humanPrincipal(scope, "principal-admin", tenancy.RoleAdmin)
	r.h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)

	resourceID := resource.ResourceID("resource-new")

	// Before registration the report ownership read must not resolve a row.
	_, err := r.h.adapter.Read(ctx, []authorize.AuthorityRead{{
		Operation:   "resource.report",
		Check:       authorize.CheckResourceOwnership,
		Scope:       scope,
		PrincipalID: actor.ID,
		TargetID:    string(resourceID),
	}})
	requireFaultClass(t, err, fault.NotFound)

	// The register ownership read is satisfied by absence.
	facts, err := r.h.adapter.Read(ctx, []authorize.AuthorityRead{{
		Operation:   "resource.binding.register",
		Check:       authorize.CheckResourceOwnership,
		Scope:       scope,
		PrincipalID: actor.ID,
		TargetID:    string(resourceID),
	}})
	if err != nil {
		t.Fatalf("register ownership read refused an absent row: %v", err)
	}
	if len(facts) != 1 || !facts[0].Allowed || facts[0].Version != 0 {
		t.Fatalf("register ownership fact = %+v; want allowed at version 0", facts)
	}

	registered, err := r.bindings.RegisterBinding(ctx, registerCommand(scope, actor.ID, "binding-a", string(resourceID), "caller-register-a"))
	if err != nil {
		t.Fatalf("RegisterBinding on a new ResourceID: %v", err)
	}
	if registered.Version != 1 || registered.Replayed {
		t.Fatalf("register result = %+v; want committed version 1", registered)
	}

	requireResourceRow(t, r.h, scope, resourceID, 1)
	if _, found := r.h.snapshot(scope, "resource_bindings", "binding-a"); !found {
		t.Fatal("register committed the resources row but not the binding")
	}

	// The row register created now satisfies the report ownership read.
	facts, err = r.h.adapter.Read(ctx, []authorize.AuthorityRead{{
		Operation:   "resource.report",
		Check:       authorize.CheckResourceOwnership,
		Scope:       scope,
		PrincipalID: actor.ID,
		TargetID:    string(resourceID),
	}})
	if err != nil {
		t.Fatalf("report ownership read after register: %v", err)
	}
	if len(facts) != 1 || !facts[0].Allowed || facts[0].Version != 1 {
		t.Fatalf("report ownership fact = %+v; want allowed at version 1", facts)
	}
}

// TestRealRegisterReusesOwnedResourceRow pins the same-scope half of the rule:
// a second registration with a new binding identity reuses the version-1 row
// (no second insert) and the created row keeps satisfying update/delete.
func TestRealRegisterReusesOwnedResourceRow(t *testing.T) {
	ctx := context.Background()
	r := newRealServices(t)
	scope := projectScope("tenant-a", "project-a")
	actor := humanPrincipal(scope, "principal-admin", tenancy.RoleAdmin)
	r.h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)

	resourceID := resource.ResourceID("resource-shared")
	if _, err := r.bindings.RegisterBinding(ctx, registerCommand(scope, actor.ID, "binding-a", string(resourceID), "caller-register-a")); err != nil {
		t.Fatalf("first RegisterBinding: %v", err)
	}
	// The schema admits one binding per (resource, service) pair in a
	// scope, so the second binding names another enforcing service.
	secondCommand := registerCommand(scope, actor.ID, "binding-b", string(resourceID), "caller-register-b")
	secondCommand.EnforcingService = "service-b"
	second, err := r.bindings.RegisterBinding(ctx, secondCommand)
	if err != nil {
		t.Fatalf("second RegisterBinding reusing the row: %v", err)
	}
	if second.Version != 1 || second.Replayed {
		t.Fatalf("second register result = %+v; want committed version 1", second)
	}
	requireResourceRow(t, r.h, scope, resourceID, 1)

	page, err := r.bindings.QueryBindings(ctx, resource.QueryBindingsCommand{Actor: actor, Scope: scope, Limit: 10})
	if err != nil {
		t.Fatalf("QueryBindings: %v", err)
	}
	if len(page.Bindings) != 2 {
		t.Fatalf("bindings = %d; want both registrations", len(page.Bindings))
	}

	// The reused row keeps the existing-row rule satisfied for update and
	// delete of the second binding.
	updated, err := r.bindings.UpdateBinding(ctx, resource.UpdateBindingCommand{
		Actor:            actor,
		Scope:            scope,
		BindingID:        "binding-b",
		ResourceID:       resourceID,
		EnforcingService: "service-b",
		TrustedOrigins:   []string{"https://git.example"},
		Operations:       []resource.Operation{resource.OperationGitRefUpdate},
		ReportableEvents: []string{"git.ref.updated"},
		Metadata:         map[string]string{"repo": "kodama", "owner": "team"},
		ExpectedVersion:  1,
		CallerKey:        "caller-update-b",
		Content:          requestContent("caller-update-b"),
	})
	if err != nil {
		t.Fatalf("UpdateBinding against the registered row: %v", err)
	}
	if updated.Version != 2 {
		t.Fatalf("updated binding version = %d; want 2", updated.Version)
	}
	if _, err := r.bindings.DeleteBinding(ctx, resource.DeleteBindingCommand{
		Actor:           actor,
		Scope:           scope,
		BindingID:       "binding-b",
		ExpectedVersion: 2,
		CallerKey:       "caller-delete-b",
		Content:         requestContent("caller-delete-b"),
	}); err != nil {
		t.Fatalf("DeleteBinding against the registered row: %v", err)
	}
	requireResourceRow(t, r.h, scope, resourceID, 1)
}

// TestRealRegisterRefusesForeignResourceRow pins the foreign-owner half: a row
// owned by another project or tenant is the uniform not_found class, the loser
// writes no binding, and the winner row is untouched.
func TestRealRegisterRefusesForeignResourceRow(t *testing.T) {
	ctx := context.Background()
	r := newRealServices(t)
	ownerScope := projectScope("tenant-a", "project-a")
	foreignProject := projectScope("tenant-a", "project-b")
	foreignTenant := projectScope("tenant-b", "project-a")
	r.h.seedMembership(ownerScope, "membership-owner", "principal-owner", 1, tenancy.RoleAdmin)
	r.h.seedMembership(foreignProject, "membership-project", "principal-project", 1, tenancy.RoleAdmin)
	r.h.seedMembership(foreignTenant, "membership-tenant", "principal-tenant", 1, tenancy.RoleAdmin)

	resourceID := resource.ResourceID("resource-owned")
	if _, err := r.bindings.RegisterBinding(ctx, registerCommand(ownerScope, "principal-owner", "binding-owner", string(resourceID), "caller-owner")); err != nil {
		t.Fatalf("owner RegisterBinding: %v", err)
	}

	for _, foreign := range []struct {
		name    string
		scope   tenancy.Scope
		actor   id.PrincipalID
		binding string
		caller  string
	}{
		{name: "other project", scope: foreignProject, actor: "principal-project", binding: "binding-foreign-project", caller: "caller-foreign-project"},
		{name: "other tenant", scope: foreignTenant, actor: "principal-tenant", binding: "binding-foreign-tenant", caller: "caller-foreign-tenant"},
	} {
		t.Run(foreign.name, func(t *testing.T) {
			_, err := r.bindings.RegisterBinding(ctx, registerCommand(foreign.scope, foreign.actor, foreign.binding, string(resourceID), foreign.caller))
			requireFaultClass(t, err, fault.NotFound)
			if _, found := r.h.snapshot(foreign.scope, "resource_bindings", foreign.binding); found {
				t.Fatalf("refused register wrote binding %q in %#v", foreign.binding, foreign.scope)
			}
			requireResourceAbsent(t, r.h, foreign.scope, resourceID)
		})
	}

	requireResourceRow(t, r.h, ownerScope, resourceID, 1)
}

// TestRealBindingUpdateDeleteReadRequireOwnedResourceRow pins the existing-row
// rule for update, delete and read: a binding whose resources row is absent is
// uniformly not_found, never version_conflict, and the refusal persists
// nothing.
func TestRealBindingUpdateDeleteReadRequireOwnedResourceRow(t *testing.T) {
	ctx := context.Background()
	r := newRealServices(t)
	scope := projectScope("tenant-a", "project-a")
	actor := humanPrincipal(scope, "principal-admin", tenancy.RoleAdmin)
	r.h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)

	// An orphan state: the binding exists but its resources row does not.
	r.h.seed(scope, "resource_bindings", "binding-orphan", map[string]any{
		"resource_id":       "resource-orphan",
		"service_id":        "service-a",
		"origins":           []byte(`["https://git.example"]`),
		"operations":        []byte(`["git.ref.update"]`),
		"reportable_events": []byte(`["git.ref.updated"]`),
		"metadata":          []byte(`{}`),
		"version":           int64(1),
		"created_at":        testNow,
	})

	_, err := r.bindings.UpdateBinding(ctx, resource.UpdateBindingCommand{
		Actor:            actor,
		Scope:            scope,
		BindingID:        "binding-orphan",
		ResourceID:       "resource-orphan",
		EnforcingService: "service-a",
		TrustedOrigins:   []string{"https://git.example"},
		Operations:       []resource.Operation{resource.OperationGitRefUpdate},
		ReportableEvents: []string{"git.ref.updated"},
		Metadata:         map[string]string{"repo": "changed"},
		ExpectedVersion:  1,
		CallerKey:        "caller-update-orphan",
		Content:          requestContent("caller-update-orphan"),
	})
	requireFaultClass(t, err, fault.NotFound)

	_, err = r.bindings.DeleteBinding(ctx, resource.DeleteBindingCommand{
		Actor:           actor,
		Scope:           scope,
		BindingID:       "binding-orphan",
		ExpectedVersion: 1,
		CallerKey:       "caller-delete-orphan",
		Content:         requestContent("caller-delete-orphan"),
	})
	requireFaultClass(t, err, fault.NotFound)

	_, err = r.bindings.ReadBinding(ctx, resource.ReadBindingCommand{Actor: actor, Scope: scope, BindingID: "binding-orphan"})
	requireFaultClass(t, err, fault.NotFound)

	if _, found := r.h.snapshot(scope, "resource_bindings", "binding-orphan"); !found {
		t.Fatal("a refused update or delete removed the binding row")
	}
	requireResourceAbsent(t, r.h, scope, "resource-orphan")
	r.h.requireReceiptAbsent(scope, "principal-admin", "resource.binding.update", "caller-update-orphan")
	r.h.requireReceiptAbsent(scope, "principal-admin", "resource.binding.delete", "caller-delete-orphan")
	if got := len(r.h.store.Events(scope)); got != 0 {
		t.Fatalf("event rows = %d; want 0", got)
	}
}

// TestRealRegisterReplayDoesNotRecreateResourceRow pins the replay rule: an
// equal register returns the original receipt result and neither re-inserts the
// resources row nor publishes a second event.
func TestRealRegisterReplayDoesNotRecreateResourceRow(t *testing.T) {
	ctx := context.Background()
	r := newRealServices(t)
	scope := projectScope("tenant-a", "project-a")
	actor := humanPrincipal(scope, "principal-admin", tenancy.RoleAdmin)
	r.h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)

	resourceID := resource.ResourceID("resource-replay")
	command := registerCommand(scope, actor.ID, "binding-replay", string(resourceID), "caller-replay")
	first, err := r.bindings.RegisterBinding(ctx, command)
	if err != nil {
		t.Fatalf("first RegisterBinding: %v", err)
	}
	second, err := r.bindings.RegisterBinding(ctx, command)
	if err != nil {
		t.Fatalf("replayed RegisterBinding: %v", err)
	}
	if !second.Replayed || second.Version != first.Version {
		t.Fatalf("replayed result = %+v; want replayed version %d", second, first.Version)
	}
	requireResourceRow(t, r.h, scope, resourceID, 1)
	if got := len(r.h.store.Events(scope)); got != 1 {
		t.Fatalf("event rows = %d after replay; want 1", got)
	}
}

// TestConcurrentRegisterSameResourceTwoScopesOneWinner pins the conditional
// insert race: two scopes both read the row absent; exactly one insert wins,
// the loser gets the uniform not_found, the winner row stays at version 1 and
// the loser writes no binding.
func TestConcurrentRegisterSameResourceTwoScopesOneWinner(t *testing.T) {
	h := newHarness(t)
	scopeA := projectScope("tenant-a", "project-a")
	scopeB := projectScope("tenant-a", "project-b")
	h.seedMembership(scopeA, "membership-a", "principal-a", 1, tenancy.RoleAdmin)
	h.seedMembership(scopeB, "membership-b", "principal-b", 1, tenancy.RoleAdmin)

	services := []*resource.Service{
		newBindingService(t, h.adapter, &commitIDs{}),
		newBindingService(t, h.adapter, &commitIDs{}),
	}
	commands := []resource.RegisterBindingCommand{
		registerCommand(scopeA, "principal-a", "binding-a", "resource-race", "caller-a"),
		registerCommand(scopeB, "principal-b", "binding-b", "resource-race", "caller-b"),
	}
	scopes := []tenancy.Scope{scopeA, scopeB}
	bindingIDs := []string{"binding-a", "binding-b"}

	start := make(chan struct{})
	errs := make([]error, len(commands))
	var wait sync.WaitGroup
	for index := range commands {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			_, errs[index] = services[index].RegisterBinding(context.Background(), commands[index])
		}(index)
	}
	close(start)
	wait.Wait()

	winner := -1
	for index, err := range errs {
		if err == nil {
			if winner >= 0 {
				t.Fatalf("both registers won: %v", errs)
			}
			winner = index
			continue
		}
		requireFaultClass(t, err, fault.NotFound)
	}
	if winner < 0 {
		t.Fatalf("no register won: %v", errs)
	}
	loser := 1 - winner

	requireResourceRow(t, h, scopes[winner], "resource-race", 1)
	requireResourceAbsent(t, h, scopes[loser], "resource-race")
	if _, found := h.snapshot(scopes[winner], "resource_bindings", bindingIDs[winner]); !found {
		t.Fatalf("the winner's binding %q was not committed", bindingIDs[winner])
	}
	if _, found := h.snapshot(scopes[loser], "resource_bindings", bindingIDs[loser]); found {
		t.Fatalf("the loser wrote binding %q", bindingIDs[loser])
	}
}

// TestRealRegisterSameScopeConflictReusesWinnerRow deterministically drives the
// same-scope-loser half of resolveRegisterConflict: a competing registration in
// the SAME scope commits its resources row and binding between the loser's
// first read and its conditional insert. The loser's insert conflicts, the
// adapter re-reads the winner row, re-applies the decision rule and reuses it
// with no second resources insert. The loser still succeeds at version 1, the
// winner row stays at version 1, and the loser's binding, audit, event and
// receipt are each committed exactly once with no conflict/not_found surfaced.
func TestRealRegisterSameScopeConflictReusesWinnerRow(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)

	resourceID := resource.ResourceID("resource-same-scope")
	// Disjoint identity ranges keep the two services' receipt/audit/event ids
	// distinct while letting this test name the loser's rows exactly.
	winnerService := newBindingService(t, h.adapter, &commitIDs{next: 100})
	var winnerErr error
	// The two bindings name different enforcing services: the schema
	// admits one binding per (resource, service) pair in a scope.
	winnerCommand := registerCommand(scope, "principal-admin", "binding-winner", string(resourceID), "caller-winner")
	winnerCommand.EnforcingService = "service-b"
	probe := &registrationProbe{Executor: h.store, beforeFirst: func() {
		_, winnerErr = winnerService.RegisterBinding(ctx, winnerCommand)
	}}
	loserService := newBindingService(t, newBindingAdapter(t, probe, h.store), &commitIDs{})

	loser, err := loserService.RegisterBinding(ctx,
		registerCommand(scope, "principal-admin", "binding-loser", string(resourceID), "caller-loser"))
	if winnerErr != nil {
		t.Fatalf("the competing same-scope winner registration: %v", winnerErr)
	}
	if err != nil {
		t.Fatalf("same-scope losing RegisterBinding: %v; want the reused-row success", err)
	}
	if loser.Version != 1 || loser.Replayed {
		t.Fatalf("loser result = %+v; want committed version 1", loser)
	}
	// The first unit carried the conditional resources insert; the reuse commit
	// must not attempt a second one.
	if probe.attempts != 2 {
		t.Fatalf("registration units = %d; want the conflicting insert plus the reuse commit", probe.attempts)
	}
	if probe.resourceInserts != 1 {
		t.Fatalf("resources inserts = %d; want exactly one (no second insert on reuse)", probe.resourceInserts)
	}

	requireResourceRow(t, h, scope, resourceID, 1)
	for _, binding := range []string{"binding-winner", "binding-loser"} {
		if _, found := h.snapshot(scope, "resource_bindings", binding); !found {
			t.Fatalf("binding %q was not committed", binding)
		}
	}
	// Loser ids: receipt-1, audit event-2, event event-3. Winner ids start at
	// 101. Each row must be present exactly once; the store keys make a
	// duplicate impossible once the count below is pinned.
	for _, audit := range []string{"event-2", "event-102"} {
		if _, found := h.snapshot(scope, "audit", audit); !found {
			t.Fatalf("audit row %q was not committed exactly once", audit)
		}
	}
	for _, event := range []string{"event-3", "event-103"} {
		if _, found := h.snapshot(scope, "events", event); !found {
			t.Fatalf("event row %q was not committed exactly once", event)
		}
	}
	if got := len(h.store.Audit(scope)); got != 2 {
		t.Fatalf("audit rows = %d; want one per registration", got)
	}
	if got := len(h.store.Events(scope)); got != 2 {
		t.Fatalf("event rows = %d; want one per registration", got)
	}
	for _, key := range []receipt.Key{
		registerReceiptKey(scope, "principal-admin", "caller-winner"),
		registerReceiptKey(scope, "principal-admin", "caller-loser"),
	} {
		if _, found := h.store.Receipt(key); !found {
			t.Fatalf("receipt for caller key %q was not committed exactly once", key.CallerKey)
		}
	}
}

// TestConcurrentSameScopeRegisterTwoBindingsOneResourceRow is the same-scope
// barrier race: two registers with different binding identities on one new
// ResourceID both read the row absent, then race their conditional inserts.
// Exactly one insert wins; the loser re-reads the same-scope winner and reuses
// it, so both succeed, one resources row at version 1 exists, both distinct
// bindings commit, and each registration writes its own audit and event once.
func TestConcurrentSameScopeRegisterTwoBindingsOneResourceRow(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)

	resourceID := resource.ResourceID("resource-same-scope-race")
	arrive := make(chan struct{}, 2)
	release := make(chan struct{})
	services := []*resource.Service{
		newBindingService(t, newBindingAdapter(t, &barrierExecutor{Executor: h.store, arrive: arrive, release: release}, h.store), &commitIDs{}),
		newBindingService(t, newBindingAdapter(t, &barrierExecutor{Executor: h.store, arrive: arrive, release: release}, h.store), &commitIDs{next: 1000}),
	}
	commands := []resource.RegisterBindingCommand{
		registerCommand(scope, "principal-admin", "binding-a", string(resourceID), "caller-a"),
		registerCommand(scope, "principal-admin", "binding-b", string(resourceID), "caller-b"),
	}
	// One binding per (resource, service) pair in a scope: the second binding
	// names another enforcing service.
	commands[1].EnforcingService = "service-b"

	errs := make([]error, len(commands))
	var wait sync.WaitGroup
	for index := range commands {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, errs[index] = services[index].RegisterBinding(context.Background(), commands[index])
		}(index)
	}
	for index := range commands {
		select {
		case <-arrive:
		case <-time.After(10 * time.Second):
			close(release)
			wait.Wait()
			t.Fatalf("register %d did not reach the commit barrier", index)
		}
	}
	close(release)
	wait.Wait()

	for index, err := range errs {
		if err != nil {
			t.Fatalf("same-scope register %d: %v; want both registers to succeed", index, err)
		}
	}
	requireResourceRow(t, h, scope, resourceID, 1)
	for _, binding := range []string{"binding-a", "binding-b"} {
		if _, found := h.snapshot(scope, "resource_bindings", binding); !found {
			t.Fatalf("binding %q was not committed", binding)
		}
	}
	if got := len(h.store.Audit(scope)); got != 2 {
		t.Fatalf("audit rows = %d; want one per registration", got)
	}
	if got := len(h.store.Events(scope)); got != 2 {
		t.Fatalf("event rows = %d; want one per registration", got)
	}
}

// TestRealRegisterRetriesWhenWinnerRolledBack pins the absent-re-read branch of
// resolveRegisterConflict: the first conditional insert conflicts but the
// winner is not visible on the re-read (it rolled back), so the adapter retries
// the conditional insert once and succeeds. No row is invented and the retry
// commits the resources row, binding, receipt, audit and event exactly once.
func TestRealRegisterRetriesWhenWinnerRolledBack(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)

	resourceID := resource.ResourceID("resource-retry")
	probe := &registrationProbe{
		Executor:  h.store,
		failFirst: fault.New(fault.VersionConflict, "the competing registration rolled back"),
	}
	service := newBindingService(t, newBindingAdapter(t, probe, h.store), &commitIDs{})

	registered, err := service.RegisterBinding(ctx,
		registerCommand(scope, "principal-admin", "binding-retry", string(resourceID), "caller-retry"))
	if err != nil {
		t.Fatalf("register after a rolled-back winner: %v; want the retry to succeed", err)
	}
	if registered.Version != 1 || registered.Replayed {
		t.Fatalf("retry result = %+v; want committed version 1", registered)
	}
	if probe.attempts != 2 {
		t.Fatalf("registration attempts = %d; want the conflict plus the one retry", probe.attempts)
	}
	if probe.resourceInserts != 2 {
		t.Fatalf("resources inserts = %d; want the conditional insert on both attempts", probe.resourceInserts)
	}
	requireResourceRow(t, h, scope, resourceID, 1)
	if _, found := h.snapshot(scope, "resource_bindings", "binding-retry"); !found {
		t.Fatal("the retried registration committed no binding")
	}
	if got := len(h.store.Audit(scope)); got != 1 {
		t.Fatalf("audit rows = %d; want exactly one", got)
	}
	if got := len(h.store.Events(scope)); got != 1 {
		t.Fatalf("event rows = %d; want exactly one", got)
	}
	if _, found := h.store.Receipt(registerReceiptKey(scope, "principal-admin", "caller-retry")); !found {
		t.Fatal("the retried registration committed no receipt")
	}
}

// TestRealRegisterDuplicateBindingWithInvisibleResourceRow pins the last
// conflict branch: the resources row stays invisible on both reads while a
// binding with the same identity already exists. The conflict is the binding
// identity, so the adapter keeps its version_conflict instead of reporting the
// uniform foreign-resource not_found, and nothing is committed.
func TestRealRegisterDuplicateBindingWithInvisibleResourceRow(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)

	// The orphan state: the binding exists, its resources row does not.
	h.seed(scope, "resource_bindings", "binding-orphan", map[string]any{
		"resource_id":       "resource-invisible",
		"service_id":        "service-a",
		"origins":           []byte(`["https://git.example"]`),
		"operations":        []byte(`["git.ref.update"]`),
		"reportable_events": []byte(`["git.ref.updated"]`),
		"metadata":          []byte(`{}`),
		"version":           int64(1),
		"created_at":        testNow,
	})
	probe := &registrationProbe{Executor: h.store}
	service := newBindingService(t, newBindingAdapter(t, probe, h.store), &commitIDs{})

	_, err := service.RegisterBinding(ctx,
		registerCommand(scope, "principal-admin", "binding-orphan", "resource-invisible", "caller-duplicate"))
	requireFaultClass(t, err, fault.VersionConflict)
	if probe.attempts != 2 {
		t.Fatalf("registration attempts = %d; want the conflict plus the one retry", probe.attempts)
	}
	requireResourceAbsent(t, h, scope, "resource-invisible")
	h.requireReceiptAbsent(scope, "principal-admin", "resource.binding.register", "caller-duplicate")
	if got := len(h.store.Audit(scope)); got != 0 {
		t.Fatalf("audit rows = %d; want none", got)
	}
	if got := len(h.store.Events(scope)); got != 0 {
		t.Fatalf("event rows = %d; want none", got)
	}
}

// TestRegisterRollbackLeavesNoResourceRow pins unit atomicity: a failure after
// the resources insert is staged, or a current-authority guard that fails
// before the write, leaves neither the resources row nor the binding.
func TestRegisterRollbackLeavesNoResourceRow(t *testing.T) {
	t.Run("staged failure", func(t *testing.T) {
		h := newHarness(t)
		scope := projectScope("tenant-a", "project-a")
		h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
		h.store.FailNextCommit(fault.New(fault.InternalFailure, "staged failure"))

		unit := h.registerUnit(scope, "principal-admin", "binding-a", "resource-new")
		_, err := h.adapter.Apply(context.Background(), unit)
		requireFaultClass(t, err, fault.InternalFailure)
		requireResourceAbsent(t, h, scope, "resource-new")
		h.requireNoRows(scope, "resource_bindings", "binding-a")
		h.requireNoPublications(scope)
		if _, found := h.store.Receipt(unit.Receipt.Key); found {
			t.Fatal("a failed register committed its receipt")
		}
	})

	t.Run("authority guard", func(t *testing.T) {
		h := newBumpingHarness(t, "memberships", "membership-admin", 1, map[string]any{
			"principal_id": "principal-admin",
			"roles":        rolesJSON(t, tenancy.RoleViewer),
			"created_at":   testNow,
		})
		scope := projectScope("tenant-a", "project-a")
		h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)

		unit := h.registerUnit(scope, "principal-admin", "binding-a", "resource-new")
		_, err := h.adapter.Apply(context.Background(), unit)
		requireFaultClass(t, err, fault.VersionConflict)
		requireResourceAbsent(t, h, scope, "resource-new")
		h.requireNoRows(scope, "resource_bindings", "binding-a")
		h.requireNoPublications(scope)
	})
}
