package identity_test

// Membership-union adapter tests.
//
// A project-scoped membership decision reads BOTH the exact-project row and the
// tenant-wide row and grants their union. The identity adapter therefore has to
// (a) expose both rows with their observed memberships.id identities through
// authorize.MembershipReader, (b) pin BOTH rows in the commit unit (a present
// row by id+version, an absent row by the absence guard), and (c) keep the
// tenant-wide row a union contribution instead of a "project row else
// tenant-wide row" fallback. A tenant-scoped decision reads and pins only the
// tenant-wide row, so a project-only membership grants no tenant right.
//
// These tests drive the real memory store, including its absence guard, and
// the real capability services wherever the identity package already wires
// them.

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	shellidentity "github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	tenancyadmin "github.com/optimiware/kodama/internal/shell/tenancy/admin"
)

// membershipGrantUnitFor builds one first-grant unit over the supplied
// authority reads so a test can inspect the guards the adapter builds.
func membershipGrantUnitFor(h *harness, scope tenancy.Scope, callerKey string, reads ...authorize.AuthorityRead) tenancyadmin.MembershipGrantUnit {
	h.t.Helper()
	return tenancyadmin.MembershipGrantUnit{
		Owner:           scope,
		Membership:      tenancy.Membership{Scope: scope, PrincipalID: "principal-b", Roles: []tenancy.Role{tenancy.RoleViewer}, Version: 1},
		ExpectedVersion: 0,
		At:              testNow,
		Receipt:         h.receipt(scope, "principal-admin", "membership.grant", callerKey, tenancyadmin.MembershipGrantResult{Version: 1}),
		Audit:           []event.Audit{h.audit(scope, "principal-admin", "membership.grant")},
		Events:          []event.Envelope{h.envelope(scope, "principal-admin", "principal-b", 1)},
		Authority:       reads,
	}
}

// membershipLocksOf returns the membership-table guards of one unit.
func membershipLocksOf(unit store.Unit) []store.Lock {
	locks := make([]store.Lock, 0, len(unit.Locks))
	for _, lock := range unit.Locks {
		if lock.Table == "memberships" {
			locks = append(locks, lock)
		}
	}
	return locks
}

// findPresentMembershipLock finds the guarded present-row lock for one row id.
func findPresentMembershipLock(locks []store.Lock, rowID string) (store.Lock, bool) {
	for _, lock := range locks {
		if lock.ExpectAbsent {
			continue
		}
		if len(lock.IDs) == 1 && lock.IDs[0] == rowID {
			return lock, true
		}
	}
	return store.Lock{}, false
}

// findAbsenceMembershipLock finds the absence guard for one key.
func findAbsenceMembershipLock(locks []store.Lock, project, principal string) (store.Lock, bool) {
	for _, lock := range locks {
		if !lock.ExpectAbsent {
			continue
		}
		if lock.Key["principal_id"] != principal {
			continue
		}
		projectValue, _ := lock.Key["project_id"].(string)
		if projectValue == project {
			return lock, true
		}
	}
	return store.Lock{}, false
}

// unionRead builds the membership authority read of one declared operation.
func unionRead(t *testing.T, operation string, scope tenancy.Scope, principal id.PrincipalID) authorize.AuthorityRead {
	t.Helper()
	op, ok := authorize.LookupOperation(authorize.OperationName(operation))
	if !ok {
		t.Fatalf("operation %q is not registered", operation)
	}
	return authorize.AuthorityRead{
		Operation:   op.Name,
		Required:    op.Requires,
		Check:       authorize.CheckMembership,
		Scope:       scope,
		PrincipalID: principal,
	}
}

// TestMembershipReaderReturnsBothRows proves a project-scoped read returns the
// exact project row first and the tenant-wide row, each with its committed row
// identity and version.
func TestMembershipReaderReturnsBothRows(t *testing.T) {
	h := newHarness(t)
	tenant := tenantScope("tenant-a")
	project := projectScope("tenant-a", "project-bob")
	h.seedMembership(tenant, "membership-tenant-bob", "principal-bob", 4, tenancy.RoleEngineer)
	h.seedMembership(project, "membership-project-bob", "principal-bob", 9, tenancy.RoleReviewer)

	rows, err := h.adapter.ReadMembership(context.Background(), project, "principal-bob")
	if err != nil {
		t.Fatalf("ReadMembership: %v", err)
	}
	if rows.Project.RowID != "membership-project-bob" {
		t.Fatalf("project row id = %q; want membership-project-bob", rows.Project.RowID)
	}
	if rows.Project.Membership == nil || rows.Project.Membership.Version != 9 {
		t.Fatalf("project membership = %+v; want the committed version 9 row", rows.Project.Membership)
	}
	if !reflect.DeepEqual(rows.Project.Membership.Roles, []tenancy.Role{tenancy.RoleReviewer}) {
		t.Fatalf("project roles = %v; want reviewer", rows.Project.Membership.Roles)
	}
	if rows.TenantWide.RowID != "membership-tenant-bob" {
		t.Fatalf("tenant-wide row id = %q; want membership-tenant-bob", rows.TenantWide.RowID)
	}
	if rows.TenantWide.Membership == nil || rows.TenantWide.Membership.Version != 4 {
		t.Fatalf("tenant-wide membership = %+v; want the committed version 4 row", rows.TenantWide.Membership)
	}
}

// TestMembershipReaderTenantScopeReturnsOnlyTenantWide proves a tenant-scoped
// read never observes the project row, so a project-only grant is not a
// tenant-wide right.
func TestMembershipReaderTenantScopeReturnsOnlyTenantWide(t *testing.T) {
	h := newHarness(t)
	tenant := tenantScope("tenant-a")
	project := projectScope("tenant-a", "project-bob")
	h.seedMembership(tenant, "membership-tenant-bob", "principal-bob", 2, tenancy.RoleViewer)
	h.seedMembership(project, "membership-project-bob", "principal-bob", 7, tenancy.RoleAdmin)

	rows, err := h.adapter.ReadMembership(context.Background(), tenant, "principal-bob")
	if err != nil {
		t.Fatalf("ReadMembership: %v", err)
	}
	if rows.TenantWide.RowID != "membership-tenant-bob" || rows.TenantWide.Membership == nil {
		t.Fatalf("tenant-wide row = %+v; want the committed tenant-wide row", rows.TenantWide)
	}
	if rows.Project.Membership != nil || rows.Project.RowID != "" {
		t.Fatalf("project row = %+v; a tenant-scoped read must not observe it", rows.Project)
	}
}

// TestMembershipReaderIsolatesScopeAndPrincipal proves the reader never returns
// another tenant's, another project's or another principal's row.
func TestMembershipReaderIsolatesScopeAndPrincipal(t *testing.T) {
	h := newHarness(t)
	project := projectScope("tenant-a", "project-a")
	h.seedMembership(tenantScope("tenant-a"), "membership-tenant-bob", "principal-bob", 1, tenancy.RoleViewer)
	h.seedMembership(project, "membership-project-bob", "principal-bob", 1, tenancy.RoleReviewer)
	h.seedMembership(projectScope("tenant-a", "project-other"), "membership-other-project", "principal-bob", 1, tenancy.RoleAdmin)
	h.seedMembership(tenantScope("tenant-b"), "membership-other-tenant", "principal-bob", 1, tenancy.RoleAdmin)
	h.seedMembership(project, "membership-project-alice", "principal-alice", 1, tenancy.RoleAdmin)

	rows, err := h.adapter.ReadMembership(context.Background(), project, "principal-bob")
	if err != nil {
		t.Fatalf("ReadMembership: %v", err)
	}
	if rows.Project.RowID != "membership-project-bob" || rows.TenantWide.RowID != "membership-tenant-bob" {
		t.Fatalf("rows = %+v; want only principal-bob's tenant and exact-project rows", rows)
	}

	missing, err := h.adapter.ReadMembership(context.Background(), project, "principal-nobody")
	if err != nil {
		t.Fatalf("ReadMembership missing: %v", err)
	}
	if missing.Project.Membership != nil || missing.TenantWide.Membership != nil {
		t.Fatalf("rows = %+v; an absent principal has no rows", missing)
	}
}

// TestMembershipAuthorityPinsBothRows proves a project-scoped membership
// authority check puts BOTH present rows in the commit unit at their observed
// identity and version, never a single tenant-wide lock.
func TestMembershipAuthorityPinsBothRows(t *testing.T) {
	h, observer := newObservingHarness(t, nil)
	tenant := tenantScope("tenant-a")
	project := projectScope("tenant-a", "project-a")
	h.seedMembership(tenant, "membership-tenant-admin", "principal-admin", 3, tenancy.RoleAdmin)
	h.seedMembership(project, "membership-project-admin", "principal-admin", 5, tenancy.RoleViewer)

	unit := membershipGrantUnitFor(h, project, "caller-both", membershipRead(project, "principal-admin"))
	if _, err := h.adapter.GrantMembership(context.Background(), unit); err != nil {
		t.Fatalf("GrantMembership: %v", err)
	}
	if len(observer.units) == 0 {
		t.Fatal("the adapter committed no unit")
	}
	locks := membershipLocksOf(observer.units[len(observer.units)-1])

	projectLock, ok := findPresentMembershipLock(locks, "membership-project-admin")
	if !ok {
		t.Fatalf("membership locks missing the project row pin: %+v", locks)
	}
	if projectLock.ExpectVersion != 5 || projectLock.TenantWide {
		t.Fatalf("project pin = %+v; want exact project row at version 5", projectLock)
	}
	tenantLock, ok := findPresentMembershipLock(locks, "membership-tenant-admin")
	if !ok {
		t.Fatalf("membership locks missing the tenant-wide row pin: %+v", locks)
	}
	if tenantLock.ExpectVersion != 3 || !tenantLock.TenantWide {
		t.Fatalf("tenant-wide pin = %+v; want the tenant-wide row at version 3", tenantLock)
	}
}

// TestMembershipAuthorityPinsAbsentRows proves an absent applicable row is a
// deliberate absence guard on the membership unique key, not a silent fallback.
func TestMembershipAuthorityPinsAbsentRows(t *testing.T) {
	t.Run("absent project row for a tenant-wide actor", func(t *testing.T) {
		h, observer := newObservingHarness(t, nil)
		tenant := tenantScope("tenant-a")
		project := projectScope("tenant-a", "project-a")
		h.seedMembership(tenant, "membership-tenant-admin", "principal-admin", 2, tenancy.RoleAdmin)

		unit := membershipGrantUnitFor(h, project, "caller-absent-project", membershipRead(project, "principal-admin"))
		if _, err := h.adapter.GrantMembership(context.Background(), unit); err != nil {
			t.Fatalf("GrantMembership: %v", err)
		}
		locks := membershipLocksOf(observer.units[len(observer.units)-1])
		guard, ok := findAbsenceMembershipLock(locks, "project-a", "principal-admin")
		if !ok {
			t.Fatalf("membership locks missing the project absence guard: %+v", locks)
		}
		if guard.ExpectVersion != 0 || len(guard.IDs) != 0 {
			t.Fatalf("project absence guard = %+v; want no row id and no version", guard)
		}
		if _, ok := findPresentMembershipLock(locks, "membership-tenant-admin"); !ok {
			t.Fatalf("membership locks missing the tenant-wide row pin: %+v", locks)
		}
	})

	t.Run("absent tenant-wide row for a project-only actor", func(t *testing.T) {
		h, observer := newObservingHarness(t, nil)
		project := projectScope("tenant-a", "project-a")
		h.seedMembership(project, "membership-project-admin", "principal-admin", 1, tenancy.RoleAdmin)

		unit := membershipGrantUnitFor(h, project, "caller-absent-tenant", membershipRead(project, "principal-admin"))
		if _, err := h.adapter.GrantMembership(context.Background(), unit); err != nil {
			t.Fatalf("GrantMembership: %v", err)
		}
		locks := membershipLocksOf(observer.units[len(observer.units)-1])
		guard, ok := findAbsenceMembershipLock(locks, "", "principal-admin")
		if !ok {
			t.Fatalf("membership locks missing the tenant-wide absence guard: %+v", locks)
		}
		if guard.Key["project_id"] != nil {
			t.Fatalf("tenant-wide absence guard project value = %v; want an explicit nil", guard.Key["project_id"])
		}
	})
}

// TestReadPrincipalReturnsUnionRoles proves a human principal's roles are the
// effective union of the tenant-wide and exact-project rows, in declared role
// order, and that the project row only adds.
func TestReadPrincipalReturnsUnionRoles(t *testing.T) {
	h := newHarness(t)
	tenant := tenantScope("tenant-a")
	project := projectScope("tenant-a", "project-a")
	h.seedMembership(tenant, "membership-tenant-bob", "principal-bob", 4, tenancy.RoleEngineer)
	h.seedMembership(project, "membership-project-bob", "principal-bob", 9, tenancy.RoleReviewer)
	h.seedMembership(tenant, "membership-tenant-carol", "principal-carol", 2, tenancy.RoleAdmin)
	h.seedMembership(project, "membership-project-carol", "principal-carol", 6, tenancy.RoleViewer)

	bob, err := h.adapter.ReadPrincipal(context.Background(), shellidentity.PrincipalRequest{Scope: project, PrincipalID: "principal-bob"})
	if err != nil {
		t.Fatalf("ReadPrincipal bob: %v", err)
	}
	if !reflect.DeepEqual(bob.Roles, []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleReviewer}) {
		t.Fatalf("bob roles = %v; want the engineer+reviewer union", bob.Roles)
	}
	carol, err := h.adapter.ReadPrincipal(context.Background(), shellidentity.PrincipalRequest{Scope: project, PrincipalID: "principal-carol"})
	if err != nil {
		t.Fatalf("ReadPrincipal carol: %v", err)
	}
	if !reflect.DeepEqual(carol.Roles, []tenancy.Role{tenancy.RoleAdmin, tenancy.RoleViewer}) {
		t.Fatalf("carol roles = %v; want the admin+viewer union (the project viewer must not narrow the tenant admin)", carol.Roles)
	}
}

// TestReadPrincipalServiceNeverHoldsAdmin proves the service variant filters
// Admin even from a corrupt service row, so the principal stays structurally
// valid and can never administer.
func TestReadPrincipalServiceNeverHoldsAdmin(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seed(scope, "services", "service-corrupt", map[string]any{
		"name":       "service-corrupt",
		"state":      "enabled",
		"roles":      rolesJSON(t, tenancy.RoleAdmin, tenancy.RoleEngineer),
		"version":    int64(1),
		"created_at": testNow,
	})

	principal, err := h.adapter.ReadPrincipal(context.Background(), shellidentity.PrincipalRequest{Scope: scope, PrincipalID: "service-corrupt"})
	if err != nil {
		t.Fatalf("ReadPrincipal: %v", err)
	}
	if principal.Kind != shellidentity.KindService {
		t.Fatalf("principal kind = %q; want service", principal.Kind)
	}
	if !reflect.DeepEqual(principal.Roles, []tenancy.Role{tenancy.RoleEngineer}) {
		t.Fatalf("service roles = %v; Admin must never be service-assignable", principal.Roles)
	}
	if err := principal.Validate(); err != nil {
		t.Fatalf("service principal is structurally invalid: %v", err)
	}

	facts, err := h.adapter.Read(context.Background(), []authorize.AuthorityRead{
		{Check: authorize.CheckMembership, Scope: scope, PrincipalID: "service-corrupt", Required: authorize.Requires{ServiceRoles: []tenancy.Role{tenancy.RoleAdmin}}},
		{Check: authorize.CheckMembership, Scope: scope, PrincipalID: "service-corrupt", Required: authorize.Requires{ServiceRoles: []tenancy.Role{tenancy.RoleEngineer}}},
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if facts[0].Allowed {
		t.Fatalf("service admin fact = %+v; want denied", facts[0])
	}
	if !facts[1].Allowed {
		t.Fatalf("service engineer fact = %+v; want allowed", facts[1])
	}
}

// TestAuthorityReadUnionBobPassesEngineerAndReviewer proves the union admits
// both an engineer operation and a reviewer operation for Bob (tenant engineer
// plus project reviewer).
func TestAuthorityReadUnionBobPassesEngineerAndReviewer(t *testing.T) {
	h := newHarness(t)
	project := projectScope("tenant-a", "project-a")
	h.seedMembership(tenantScope("tenant-a"), "membership-tenant-bob", "principal-bob", 1, tenancy.RoleEngineer)
	h.seedMembership(project, "membership-project-bob", "principal-bob", 1, tenancy.RoleReviewer)

	facts, err := h.adapter.Read(context.Background(), []authorize.AuthorityRead{
		unionRead(t, "pipeline.apply", project, "principal-bob"),
		unionRead(t, "execution.request.resolve", project, "principal-bob"),
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !facts[0].Allowed {
		t.Fatalf("pipeline.apply fact = %+v; want the tenant engineer union to allow it", facts[0])
	}
	if !facts[1].Allowed {
		t.Fatalf("execution.request.resolve fact = %+v; want the project reviewer union to allow it", facts[1])
	}
}

// TestAuthorityReadProjectOnlyMemberHasNoTenantFact proves a tenant-scoped read
// ignores the project row entirely.
func TestAuthorityReadProjectOnlyMemberHasNoTenantFact(t *testing.T) {
	h := newHarness(t)
	tenant := tenantScope("tenant-a")
	project := projectScope("tenant-a", "project-a")
	h.seedMembership(project, "membership-project-admin", "principal-project-only", 1, tenancy.RoleAdmin)

	facts, err := h.adapter.Read(context.Background(), []authorize.AuthorityRead{
		unionRead(t, "membership.list", tenant, "principal-project-only"),
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if facts[0].Allowed {
		t.Fatalf("tenant-scoped fact = %+v; a project-only membership grants no tenant right", facts[0])
	}
}

// TestMembershipAuthorityConfirmsCallerPins proves a commit-unit read that
// already carries the capability's observed pins is compared with the rows the
// adapter re-reads. A row change between the capability's confirmation and the
// adapter's re-read is a version_conflict, even when the current union still
// grants the role.
func TestMembershipAuthorityConfirmsCallerPins(t *testing.T) {
	h := newHarness(t)
	tenant := tenantScope("tenant-a")
	project := projectScope("tenant-a", "project-a")
	h.seedMembership(tenant, "membership-tenant-admin", "principal-admin", 1, tenancy.RoleAdmin)

	read := membershipRead(project, "principal-admin")
	rows, err := h.adapter.ReadMembership(context.Background(), project, "principal-admin")
	if err != nil {
		t.Fatalf("ReadMembership: %v", err)
	}
	fact, err := authorize.EvaluateMembership(read, false, rows)
	if err != nil {
		t.Fatalf("EvaluateMembership: %v", err)
	}
	pinned := fact.PinnedRead()
	if len(pinned.Targets) != 2 {
		t.Fatalf("pinned targets = %d; want the project absence and the tenant-wide row", len(pinned.Targets))
	}

	if _, err := h.store.Execute(context.Background(), tenant, store.Unit{Writes: []store.Write{{
		Table: "memberships", ID: "membership-tenant-admin", ExpectVersion: 1,
		Values: map[string]any{"roles": rolesJSON(t, tenancy.RoleAdmin)},
	}}}); err != nil {
		t.Fatalf("advance the tenant-wide row: %v", err)
	}

	unit := membershipGrantUnitFor(h, project, "caller-pinned", pinned)
	_, err = h.adapter.GrantMembership(context.Background(), unit)
	requireFaultClass(t, err, fault.VersionConflict)
	if _, found := h.snapshot(project, "memberships", "principal-b"); found {
		t.Fatal("a stale pinned membership read committed the grant")
	}
}

// TestFirstGrantUnitCarriesAbsenceGuard proves the create-if-absent shape of a
// first membership grant: the same unit that inserts the new row identity
// carries the absence guard on the (tenant, project, principal) key.
func TestFirstGrantUnitCarriesAbsenceGuard(t *testing.T) {
	h, observer := newObservingHarness(t, nil)
	tenant := tenantScope("tenant-a")
	project := projectScope("tenant-a", "project-a")
	h.seedMembership(tenant, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)

	if _, err := h.adapter.GrantMembership(context.Background(), membershipGrantUnitFor(h, project, "caller-first", membershipRead(project, "principal-admin"))); err != nil {
		t.Fatalf("GrantMembership: %v", err)
	}
	unit := observer.units[len(observer.units)-1]
	locks := membershipLocksOf(unit)
	guard, ok := findAbsenceMembershipLock(locks, "project-a", "principal-b")
	if !ok {
		t.Fatalf("first-grant locks missing the target absence guard: %+v", locks)
	}
	if guard.Key["tenant_id"] != "tenant-a" {
		t.Fatalf("absence guard tenant = %v; want tenant-a", guard.Key["tenant_id"])
	}
	if len(unit.Writes) != 1 || !unit.Writes[0].Insert {
		t.Fatalf("first-grant writes = %+v; want exactly one insert", unit.Writes)
	}
}

// TestGrantOverExistingRowLocksItsIdentityAndVersion proves a grant over an
// existing scoped row pins that exact row id and expected version; it never
// re-inserts and never falls back to the tenant-wide row.
func TestGrantOverExistingRowLocksItsIdentityAndVersion(t *testing.T) {
	h, observer := newObservingHarness(t, nil)
	tenant := tenantScope("tenant-a")
	project := projectScope("tenant-a", "project-a")
	h.seedMembership(tenant, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seedMembership(project, "membership-project-b", "principal-b", 2, tenancy.RoleViewer)

	unit := membershipGrantUnitFor(h, project, "caller-update", membershipRead(project, "principal-admin"))
	unit.ExpectedVersion = 2
	unit.Membership.Version = 3
	if _, err := h.adapter.GrantMembership(context.Background(), unit); err != nil {
		t.Fatalf("GrantMembership update: %v", err)
	}
	captured := observer.units[len(observer.units)-1]
	locks := membershipLocksOf(captured)
	rowLock, ok := findPresentMembershipLock(locks, "membership-project-b")
	if !ok {
		t.Fatalf("update locks missing the existing row identity: %+v", locks)
	}
	if rowLock.ExpectVersion != 2 {
		t.Fatalf("update row pin version = %d; want the expected 2", rowLock.ExpectVersion)
	}
	if len(captured.Writes) != 1 || captured.Writes[0].Insert || captured.Writes[0].ID != "membership-project-b" {
		t.Fatalf("update writes = %+v; want one guarded update of the existing row", captured.Writes)
	}
}

// registerAs builds one real resource registration command for the actor.
func registerAs(actorScope, scope tenancy.Scope, actor id.PrincipalID, binding, resourceID, caller string) resource.RegisterBindingCommand {
	return resource.RegisterBindingCommand{
		Actor:            humanPrincipal(actorScope, actor, tenancy.RoleAdmin),
		Scope:            scope,
		BindingID:        resource.BindingID(binding),
		ResourceID:       resource.ResourceID(resourceID),
		EnforcingService: "service-a",
		TrustedOrigins:   []string{"https://git.example"},
		Operations:       []resource.Operation{resource.OperationGitRefUpdate},
		ReportableEvents: []string{"git.ref.updated"},
		Metadata:         map[string]string{"repo": "kodama"},
		CallerKey:        caller,
		Content:          requestContent(caller),
	}
}

// TestRealMembershipUnionCarolKeepsAdminOperations drives the real resource
// service: Carol's tenant-wide Admin stays effective in a project where she
// holds only Viewer, so the project admin operation commits.
func TestRealMembershipUnionCarolKeepsAdminOperations(t *testing.T) {
	ctx := context.Background()
	r := newRealServices(t)
	tenant := tenantScope("tenant-a")
	project := projectScope("tenant-a", "project-a")
	r.h.seedMembership(tenant, "membership-tenant-carol", "principal-carol", 1, tenancy.RoleAdmin)
	r.h.seedMembership(project, "membership-project-carol", "principal-carol", 1, tenancy.RoleViewer)

	if _, err := r.bindings.RegisterBinding(ctx, registerAs(project, project, "principal-carol", "binding-carol", "resource-carol", "caller-carol")); err != nil {
		t.Fatalf("RegisterBinding by Carol: %v", err)
	}
	if _, found := r.h.snapshot(project, "resource_bindings", "binding-carol"); !found {
		t.Fatal("Carol's tenant-wide Admin did not authorize the project registration")
	}
}

// TestRealMembershipProjectOnlyMemberHasNoTenantOperation drives the real
// tenancy service: a principal with only a project grant cannot commit a
// tenant-scoped administration unit.
func TestRealMembershipProjectOnlyMemberHasNoTenantOperation(t *testing.T) {
	ctx := context.Background()
	r := newRealServices(t)
	tenant := tenantScope("tenant-a")
	project := projectScope("tenant-a", "project-a")
	r.h.seedMembership(project, "membership-project-admin", "principal-project-only", 1, tenancy.RoleAdmin)

	_, err := r.tenancy.CreateProject(ctx, tenancyadmin.ProjectCreateCommand{
		Actor:     humanPrincipal(tenant, "principal-project-only", tenancy.RoleAdmin),
		ProjectID: "project-new",
		Name:      "Project new",
		CallerKey: "caller-project-only",
		Content:   requestContent("project-only"),
	})
	requireFaultClass(t, err, fault.Forbidden)
	r.h.requireNoRows(tenant, "projects", "project-new")
}

// TestRealMembershipRevokedTenantWideRowConflicts proves a tenant-wide revoke
// between confirmation and commit is refused, not silently accepted.
func TestRealMembershipRevokedTenantWideRowConflicts(t *testing.T) {
	ctx := context.Background()
	tenant := tenantScope("tenant-a")
	project := projectScope("tenant-a", "project-a")
	var h *harness
	h, observer := newObservingHarness(t, func(ctx context.Context) error {
		_, err := h.store.Execute(ctx, tenant, store.Unit{Writes: []store.Write{{
			Table: "memberships", ID: "membership-tenant-admin", Delete: true, ExpectVersion: 1,
		}}})
		return err
	})
	r := newRealServicesOn(t, h)
	h.seedMembership(tenant, "membership-tenant-admin", "principal-admin", 1, tenancy.RoleAdmin)

	_, err := r.bindings.RegisterBinding(ctx, registerAs(tenant, project, "principal-admin", "binding-revoked", "resource-revoked", "caller-revoked"))
	requireFaultClass(t, err, fault.VersionConflict)
	r.h.requireNoRows(project, "resource_bindings", "binding-revoked")
	if !observer.fired {
		t.Fatal("the interposed revoke did not run before the commit unit")
	}
}

// TestRealMembershipDeletedAndRecreatedRowConflicts proves a revoked row that is
// re-created at the same version under a new identity is refused: a version
// alone is not row identity.
func TestRealMembershipDeletedAndRecreatedRowConflicts(t *testing.T) {
	ctx := context.Background()
	project := projectScope("tenant-a", "project-a")
	roles := rolesJSON(t, tenancy.RoleAdmin)
	var h *harness
	h, _ = newObservingHarness(t, func(ctx context.Context) error {
		_, err := h.store.Execute(ctx, project, store.Unit{Writes: []store.Write{
			{Table: "memberships", ID: "membership-project-admin", Delete: true, ExpectVersion: 1},
			{Table: "memberships", ID: "membership-project-admin-regranted", Insert: true, Values: map[string]any{
				"principal_id": "principal-admin",
				"roles":        roles,
				"version":      int64(1),
				"created_at":   testNow,
			}},
		}})
		return err
	})
	r := newRealServicesOn(t, h)
	h.seedMembership(project, "membership-project-admin", "principal-admin", 1, tenancy.RoleAdmin)

	_, err := r.bindings.RegisterBinding(ctx, registerAs(project, project, "principal-admin", "binding-recreated", "resource-recreated", "caller-recreated"))
	requireFaultClass(t, err, fault.VersionConflict)
	r.h.requireNoRows(project, "resource_bindings", "binding-recreated")
	if _, found := h.snapshot(project, "memberships", "membership-project-admin-regranted"); !found {
		t.Fatal("the interposed regrant row is missing; the fixture did not run")
	}
}

// TestRealMembershipAbsentProjectRowCreatedConflicts proves a project row
// created after confirmation, when the project row was absent and pinned as
// such, is a version_conflict.
func TestRealMembershipAbsentProjectRowCreatedConflicts(t *testing.T) {
	ctx := context.Background()
	tenant := tenantScope("tenant-a")
	project := projectScope("tenant-a", "project-a")
	roles := rolesJSON(t, tenancy.RoleViewer)
	var h *harness
	h, _ = newObservingHarness(t, func(ctx context.Context) error {
		_, err := h.store.Execute(ctx, project, store.Unit{Writes: []store.Write{{
			Table: "memberships", ID: "membership-project-late", Insert: true, Values: map[string]any{
				"principal_id": "principal-admin",
				"roles":        roles,
				"version":      int64(1),
				"created_at":   testNow,
			},
		}}})
		return err
	})
	r := newRealServicesOn(t, h)
	h.seedMembership(tenant, "membership-tenant-admin", "principal-admin", 1, tenancy.RoleAdmin)

	_, err := r.bindings.RegisterBinding(ctx, registerAs(tenant, project, "principal-admin", "binding-late", "resource-late", "caller-late"))
	requireFaultClass(t, err, fault.VersionConflict)
	r.h.requireNoRows(project, "resource_bindings", "binding-late")
	if _, found := h.snapshot(project, "memberships", "membership-project-late"); !found {
		t.Fatal("the interposed project row is missing; the fixture did not run")
	}
}

// TestRealFirstGrantAbsenceGuardRejectsExistingRow proves a first grant cannot
// create a second row for an existing (tenant, project, principal) key.
func TestRealFirstGrantAbsenceGuardRejectsExistingRow(t *testing.T) {
	ctx := context.Background()
	r := newRealServices(t)
	tenant := tenantScope("tenant-a")
	r.h.seedMembership(tenant, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	r.h.seedMembership(tenant, "membership-existing-bob", "principal-bob", 1, tenancy.RoleViewer)

	_, err := r.tenancy.GrantMembership(ctx, tenancyadmin.MembershipGrantCommand{
		Actor:           humanPrincipal(tenant, "principal-admin", tenancy.RoleAdmin),
		Membership:      tenancy.Membership{Scope: tenant, PrincipalID: "principal-bob", Roles: []tenancy.Role{tenancy.RoleEngineer}},
		ExpectedVersion: 0,
		CallerKey:       "caller-first-grant-existing",
		Content:         requestContent("first-grant-existing"),
	})
	requireFaultClass(t, err, fault.VersionConflict)
	if _, found := r.h.snapshot(tenant, "memberships", "membership-1"); found {
		t.Fatal("a first grant inserted a duplicate row identity")
	}
	page, err := r.h.membershipFor(tenant, "principal-bob")
	if err != nil {
		t.Fatalf("ListMemberships: %v", err)
	}
	if len(page.Memberships) != 1 || page.Memberships[0].Version != 1 {
		t.Fatalf("memberships = %+v; want the untouched existing row", page.Memberships)
	}
}

// TestRealRegrantAfterRevokeUsesNewRowIdentity proves a revoked row identity is
// never reused: a re-grant starts a new row at version 1.
func TestRealRegrantAfterRevokeUsesNewRowIdentity(t *testing.T) {
	ctx := context.Background()
	r := newRealServices(t)
	tenant := tenantScope("tenant-a")
	actor := humanPrincipal(tenant, "principal-admin", tenancy.RoleAdmin)
	r.h.seedMembership(tenant, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)

	if _, err := r.tenancy.GrantMembership(ctx, tenancyadmin.MembershipGrantCommand{
		Actor:           actor,
		Membership:      tenancy.Membership{Scope: tenant, PrincipalID: "principal-bob", Roles: []tenancy.Role{tenancy.RoleViewer}},
		ExpectedVersion: 0,
		CallerKey:       "caller-regrant-first",
		Content:         requestContent("regrant-first"),
	}); err != nil {
		t.Fatalf("first grant: %v", err)
	}
	if _, found := r.h.snapshot(tenant, "memberships", "membership-1"); !found {
		t.Fatal("the first grant did not allocate membership-1")
	}
	if _, err := r.tenancy.RevokeMembership(ctx, tenancyadmin.MembershipRevokeCommand{
		Actor:           actor,
		Scope:           tenant,
		PrincipalID:     "principal-bob",
		ExpectedVersion: 1,
		CallerKey:       "caller-regrant-revoke",
		Content:         requestContent("regrant-revoke"),
	}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, found := r.h.snapshot(tenant, "memberships", "membership-1"); found {
		t.Fatal("the revoked row identity survived")
	}
	if _, err := r.tenancy.GrantMembership(ctx, tenancyadmin.MembershipGrantCommand{
		Actor:           actor,
		Membership:      tenancy.Membership{Scope: tenant, PrincipalID: "principal-bob", Roles: []tenancy.Role{tenancy.RoleViewer}},
		ExpectedVersion: 0,
		CallerKey:       "caller-regrant-second",
		Content:         requestContent("regrant-second"),
	}); err != nil {
		t.Fatalf("regrant: %v", err)
	}
	row, found := r.h.snapshot(tenant, "memberships", "membership-2")
	if !found {
		t.Fatal("the regrant did not mint a new row identity")
	}
	if got := mustInt(t, row, "version"); got != 1 {
		t.Fatalf("regranted version = %d; want a restarted 1", got)
	}
}

// TestRealConcurrentFirstGrantsHaveOneWinner proves two create-if-absent units
// on the same membership key cannot both commit: exactly one wins and the loser
// is version_conflict.
func TestRealConcurrentFirstGrantsHaveOneWinner(t *testing.T) {
	ctx := context.Background()
	r := newRealServices(t)
	tenant := tenantScope("tenant-a")
	r.h.seedMembership(tenant, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)

	type outcome struct {
		index int
		err   error
	}
	results := make(chan outcome, 2)
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	done.Add(2)
	for index := range 2 {
		go func(index int) {
			defer done.Done()
			start.Wait()
			callerKey := fmt.Sprintf("caller-concurrent-%d", index)
			_, err := r.tenancy.GrantMembership(ctx, tenancyadmin.MembershipGrantCommand{
				Actor:           humanPrincipal(tenant, "principal-admin", tenancy.RoleAdmin),
				Membership:      tenancy.Membership{Scope: tenant, PrincipalID: "principal-bob", Roles: []tenancy.Role{tenancy.RoleViewer}},
				ExpectedVersion: 0,
				CallerKey:       callerKey,
				Content:         requestContent(callerKey),
			})
			results <- outcome{index: index, err: err}
		}(index)
	}
	start.Done()
	done.Wait()
	close(results)

	winners, conflicts := 0, 0
	for result := range results {
		if result.err == nil {
			winners++
			continue
		}
		class, ok := fault.ClassOf(result.err)
		if !ok || class != fault.VersionConflict {
			t.Fatalf("concurrent grant %d failed with %v; want version_conflict", result.index, result.err)
		}
		conflicts++
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("concurrent first grants: %d winners, %d conflicts; want exactly one of each", winners, conflicts)
	}
	page, err := r.h.membershipFor(tenant, "principal-bob")
	if err != nil {
		t.Fatalf("ListMemberships: %v", err)
	}
	if len(page.Memberships) != 1 || page.Memberships[0].Version != 1 {
		t.Fatalf("memberships = %+v; want exactly one committed first grant", page.Memberships)
	}
}
