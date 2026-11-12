package work

// Membership-union authority at the work adapter boundary. A project-scoped
// membership check reads BOTH the tenant-wide row and the exact-project row and
// evaluates their union; a project row can only add roles and never shadows a
// tenant-wide grant. A tenant-scoped check reads only the tenant-wide row, so a
// project-only membership grants no tenant-wide right. Both present rows are
// pinned by memberships.id identity AND version, and an absent row is pinned by
// the absence guard on (tenant_id, project_id, principal_id). There is no
// single-row/TenantWide-fallback lookup.

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

const (
	unionPrincipal  = "principal-bob"
	carolPrincipal  = "principal-carol"
	viewerPrincipal = "principal-viewer"
)

func tenantScope() tenancy.Scope {
	return tenancy.Scope{TenantID: testTenant}
}

// seedUnionMembership seeds one membership row at an explicit scope with its
// observed identity and version.
func seedUnionMembership(t *testing.T, fixture *fixture, scope tenancy.Scope, rowID, principal string, version uint64, roles ...tenancy.Role) {
	t.Helper()
	fixture.seedMembership(t, scope, rowID, principal, version, roles...)
}

// seatMembershipRead builds one membership authority read for an explicit
// principal and required role.
func seatMembershipRead(scope tenancy.Scope, operation authorize.OperationName, principal id.PrincipalID, roles ...tenancy.Role) authorize.AuthorityRead {
	return authorize.AuthorityRead{
		Operation:   operation,
		Required:    authorize.Requires{HumanRoles: roles},
		Check:       authorize.CheckMembership,
		Scope:       scope,
		PrincipalID: principal,
	}
}

// locksByRow indexes the membership locks of one authority mapping by row id.
func locksByRow(t *testing.T, locks []store.Lock) map[string]store.Lock {
	t.Helper()
	present := make(map[string]store.Lock, len(locks))
	for _, lock := range locks {
		if lock.Table != "memberships" || lock.ExpectAbsent {
			continue
		}
		if len(lock.IDs) != 1 {
			t.Fatalf("a present membership lock carries %d ids: %+v", len(lock.IDs), lock)
		}
		present[lock.IDs[0]] = lock
	}
	return present
}

// absenceLock returns the single membership absence guard, if any.
func absenceLock(t *testing.T, locks []store.Lock) (store.Lock, bool) {
	t.Helper()
	for _, lock := range locks {
		if lock.Table == "memberships" && lock.ExpectAbsent {
			return lock, true
		}
	}
	return store.Lock{}, false
}

// requireValidLocks proves the mapper produced a well-formed unit shape against
// the declared schema, including the absence key columns.
func requireValidLocks(t *testing.T, locks []store.Lock) {
	t.Helper()
	if err := (store.Unit{Locks: locks}).Validate(); err != nil {
		t.Fatalf("authority locks are not a valid unit shape: %v", err)
	}
}

// TestAuthorityMembershipUnionPinsBothRows proves Bob (tenant-wide Engineer,
// project Reviewer) is granted the union and that BOTH rows, at their observed
// identities and versions, are carried as present pins. A shadowing
// lookup would return only the project row and lose the Engineer grant.
func TestAuthorityMembershipUnionPinsBothRows(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	seedUnionMembership(t, fixture, tenantScope(), "membership-tenant-bob", unionPrincipal, 3, tenancy.RoleEngineer)
	seedUnionMembership(t, fixture, scope, "membership-project-bob", unionPrincipal, 7, tenancy.RoleReviewer)

	locks, err := authorityLocks(context.Background(), fixture.store, fixture.store,
		[]authorize.AuthorityRead{seatMembershipRead(scope, "task.create", unionPrincipal, tenancy.RoleEngineer)},
		authorityContext{})
	if err != nil {
		t.Fatalf("tenant Engineer + project Reviewer did not grant task.create: %v", err)
	}
	requireValidLocks(t, locks)
	present := locksByRow(t, locks)
	if len(present) != 2 || len(locks) != 2 {
		t.Fatalf("membership union pinned %d locks (%d present); want exactly both rows: %+v", len(locks), len(present), locks)
	}
	project, ok := present["membership-project-bob"]
	if !ok || project.ExpectVersion != 7 || project.TenantWide {
		t.Fatalf("project pin = %+v; want identity membership-project-bob at version 7 and not tenant-wide", project)
	}
	tenant, ok := present["membership-tenant-bob"]
	if !ok || tenant.ExpectVersion != 3 || !tenant.TenantWide {
		t.Fatalf("tenant pin = %+v; want identity membership-tenant-bob at version 3 and tenant-wide", tenant)
	}
	if _, absent := absenceLock(t, locks); absent {
		t.Fatal("a present union produced an absence pin")
	}

	// The project row can only add roles: a Reviewer-only grant also passes.
	if _, err := authorityLocks(context.Background(), fixture.store, fixture.store,
		[]authorize.AuthorityRead{seatMembershipRead(scope, "task.reply", unionPrincipal, tenancy.RoleReviewer)},
		authorityContext{}); err != nil {
		t.Fatalf("the project Reviewer row did not grant a reviewer-only operation: %v", err)
	}

	// And the union does not fabricate Admin.
	_, err = authorityLocks(context.Background(), fixture.store, fixture.store,
		[]authorize.AuthorityRead{seatMembershipRead(scope, "pipeline.apply", unionPrincipal, tenancy.RoleAdmin)},
		authorityContext{})
	mustClass(t, err, fault.Forbidden)
}

// TestAuthorityMembershipAbsentRowPinsAbsenceGuard proves the union pins a
// missing project row by its registered unique key rather than silently
// omitting it, so a grant created after confirmation is a version_conflict.
func TestAuthorityMembershipAbsentRowPinsAbsenceGuard(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	seedUnionMembership(t, fixture, tenantScope(), "membership-tenant-bob", unionPrincipal, 3, tenancy.RoleEngineer)

	locks, err := authorityLocks(context.Background(), fixture.store, fixture.store,
		[]authorize.AuthorityRead{seatMembershipRead(scope, "task.create", unionPrincipal, tenancy.RoleEngineer)},
		authorityContext{})
	if err != nil {
		t.Fatalf("the tenant-wide Engineer row did not grant the project read: %v", err)
	}
	requireValidLocks(t, locks)
	if len(locks) != 2 {
		t.Fatalf("the union pinned %d locks; want one absent project row and one present tenant row: %+v", len(locks), locks)
	}
	absent, ok := absenceLock(t, locks)
	if !ok {
		t.Fatal("the absent project row was not pinned")
	}
	if absent.Key["tenant_id"] != testTenant ||
		absent.Key["project_id"] != testProject ||
		absent.Key["principal_id"] != unionPrincipal {
		t.Fatalf("absence key = %+v; want the exact project unique key", absent.Key)
	}
	present := locksByRow(t, locks)
	if tenant, ok := present["membership-tenant-bob"]; !ok || tenant.ExpectVersion != 3 || !tenant.TenantWide {
		t.Fatalf("tenant pin = %+v; want the observed tenant-wide row", tenant)
	}
}

// TestAuthorityMembershipTenantScopePinsOnlyTenantRow proves a tenant-scoped
// read never observes or pins the project row, and that a project-only
// membership therefore grants no tenant-wide right.
func TestAuthorityMembershipTenantScopePinsOnlyTenantRow(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	tenant := tenantScope()
	seedUnionMembership(t, fixture, tenant, "membership-tenant-bob", unionPrincipal, 3, tenancy.RoleEngineer)
	seedUnionMembership(t, fixture, scope, "membership-project-bob", unionPrincipal, 7, tenancy.RoleReviewer)

	locks, err := authorityLocks(context.Background(), fixture.store, fixture.store,
		[]authorize.AuthorityRead{seatMembershipRead(tenant, "project.create", unionPrincipal, tenancy.RoleEngineer)},
		authorityContext{})
	if err != nil {
		t.Fatalf("the tenant-scoped Engineer row was refused: %v", err)
	}
	requireValidLocks(t, locks)
	if len(locks) != 1 || locks[0].IDs[0] != "membership-tenant-bob" || !locks[0].TenantWide {
		t.Fatalf("tenant-scoped pins = %+v; want only the tenant-wide row", locks)
	}
	if _, absent := absenceLock(t, locks); absent {
		t.Fatal("a tenant-scoped read pinned the project row")
	}

	// A project-only Admin row grants nothing at tenant scope.
	seedUnionMembership(t, fixture, scope, "membership-project-viewer", viewerPrincipal, 1, tenancy.RoleAdmin)
	_, err = authorityLocks(context.Background(), fixture.store, fixture.store,
		[]authorize.AuthorityRead{seatMembershipRead(tenant, "project.create", viewerPrincipal, tenancy.RoleAdmin)},
		authorityContext{})
	mustClass(t, err, fault.Forbidden)
}

// TestAuthorityMembershipConfirmsCallerPins proves a membership read that
// already carries the pins an earlier observation captured is confirmed against
// the rows read now: unchanged rows yield exactly those pins, while a changed
// row is version_conflict.
func TestAuthorityMembershipConfirmsCallerPins(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	seedUnionMembership(t, fixture, tenantScope(), "membership-tenant-bob", unionPrincipal, 3, tenancy.RoleEngineer)
	seedUnionMembership(t, fixture, scope, "membership-project-bob", unionPrincipal, 7, tenancy.RoleReviewer)

	read := seatMembershipRead(scope, "task.create", unionPrincipal, tenancy.RoleEngineer)
	rows := authorize.MembershipRows{
		TenantWide: authorize.MembershipRow{
			RowID: "membership-tenant-bob",
			Membership: &tenancy.Membership{
				Scope: tenantScope(), PrincipalID: unionPrincipal,
				Roles: []tenancy.Role{tenancy.RoleEngineer}, Version: 3,
			},
		},
		Project: authorize.MembershipRow{
			RowID: "membership-project-bob",
			Membership: &tenancy.Membership{
				Scope: scope, PrincipalID: unionPrincipal,
				Roles: []tenancy.Role{tenancy.RoleReviewer}, Version: 7,
			},
		},
	}
	fact, err := authorize.EvaluateMembership(read, false, rows)
	if err != nil {
		t.Fatalf("evaluate pinned rows: %v", err)
	}
	read.Targets = fact.Targets

	locks, err := authorityLocks(context.Background(), fixture.store, fixture.store, []authorize.AuthorityRead{read}, authorityContext{})
	if err != nil {
		t.Fatalf("unchanged pinned rows were refused: %v", err)
	}
	if len(locks) != 2 {
		t.Fatalf("caller pins produced %d locks; want both rows: %+v", len(locks), locks)
	}

	fixture.setMembershipRoles(t, scope, "membership-project-bob", 7, tenancy.RoleReviewer, tenancy.RoleOperator)
	_, err = authorityLocks(context.Background(), fixture.store, fixture.store, []authorize.AuthorityRead{read}, authorityContext{})
	mustClass(t, err, fault.VersionConflict)
}

// TestAuthorityServiceMembershipNeverGainsAdmin proves the service branch keeps
// the union guarantee that a service account never holds Admin, even from a
// corrupt services row.
func TestAuthorityServiceMembershipNeverGainsAdmin(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedServiceWithRoles(t, scope, "service-corrupt", serviceStateEnabled, 1, tenancy.RoleAdmin)
	read := authorize.AuthorityRead{
		Operation:   "pipeline.start",
		Required:    authorize.Requires{ServiceRoles: []tenancy.Role{tenancy.RoleAdmin}},
		Check:       authorize.CheckMembership,
		Scope:       scope,
		PrincipalID: "service-corrupt",
	}
	_, err := authorityLocks(context.Background(), fixture.store, fixture.store, []authorize.AuthorityRead{read}, authorityContext{})
	mustClass(t, err, fault.Forbidden)

	fixture.setServiceRoles(t, scope, "service-corrupt", 1, tenancy.RoleEngineer, tenancy.RoleAdmin)
	read.Required.ServiceRoles = []tenancy.Role{tenancy.RoleEngineer}
	locks, err := authorityLocks(context.Background(), fixture.store, fixture.store, []authorize.AuthorityRead{read}, authorityContext{})
	if err != nil {
		t.Fatalf("an enabled service Engineer was refused: %v", err)
	}
	if len(locks) != 1 || locks[0].Table != "services" || locks[0].IDs[0] != "service-corrupt" || locks[0].ExpectVersion != 2 {
		t.Fatalf("service locks = %+v; want the pin of the service row at version 2", locks)
	}
}

// TestAuthorityPushServiceAdminUsesMembershipUnion proves the conditional Admin
// check uses the same union: Carol (tenant-wide Admin, project Viewer) keeps
// Admin and both rows are pinned, while demoting the tenant row drops the grant.
func TestAuthorityPushServiceAdminUsesMembershipUnion(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	seedUnionMembership(t, fixture, tenantScope(), "membership-tenant-carol", carolPrincipal, 2, tenancy.RoleAdmin)
	seedUnionMembership(t, fixture, scope, "membership-project-carol", carolPrincipal, 5, tenancy.RoleViewer)

	read := authorize.AuthorityRead{
		Operation:   "pipeline.update",
		Required:    engineerRequires(),
		Check:       authorize.CheckPushServiceAdmin,
		Scope:       scope,
		PrincipalID: carolPrincipal,
	}
	locks, err := authorityLocks(context.Background(), fixture.store, fixture.store, []authorize.AuthorityRead{read}, authorityContext{})
	if err != nil {
		t.Fatalf("tenant Admin + project Viewer lost the Admin grant: %v", err)
	}
	requireValidLocks(t, locks)
	present := locksByRow(t, locks)
	if len(present) != 2 {
		t.Fatalf("push-admin union pinned %d present rows; want both: %+v", len(present), locks)
	}
	if got := present["membership-tenant-carol"]; got.ExpectVersion != 2 || !got.TenantWide {
		t.Fatalf("tenant Admin pin = %+v; want identity+version with TenantWide", got)
	}
	if got := present["membership-project-carol"]; got.ExpectVersion != 5 || got.TenantWide {
		t.Fatalf("project Viewer pin = %+v; want identity+version without TenantWide", got)
	}

	// Demoting the tenant-wide row drops Admin even though the project row stays.
	fixture.setMembershipRoles(t, tenancy.Scope{TenantID: testTenant}, "membership-tenant-carol", 2, tenancy.RoleViewer)
	_, err = authorityLocks(context.Background(), fixture.store, fixture.store, []authorize.AuthorityRead{read}, authorityContext{})
	mustClass(t, err, fault.Forbidden)
}

// TestAuthorityPushServiceAdminDeniesServicePrincipal proves the conditional
// Admin check resolves a service principal through its service row, never
// through a membership row: an Admin membership row under the same identity
// cannot make a service an administrator.
func TestAuthorityPushServiceAdminDeniesServicePrincipal(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedServiceWithRoles(t, scope, "service-admin-claim", serviceStateEnabled, 1, tenancy.RoleEngineer)
	seedUnionMembership(t, fixture, scope, "membership-service-admin-claim", "service-admin-claim", 4, tenancy.RoleAdmin)

	read := authorize.AuthorityRead{
		Operation:   "pipeline.update",
		Required:    engineerRequires(),
		Check:       authorize.CheckPushServiceAdmin,
		Scope:       scope,
		PrincipalID: "service-admin-claim",
	}
	_, err := authorityLocks(context.Background(), fixture.store, fixture.store, []authorize.AuthorityRead{read}, authorityContext{})
	mustClass(t, err, fault.Forbidden)
}

// TestAuthorityPushServiceAdminConfirmsCallerPins proves a push-admin read that
// carries the pins an earlier confirmation captured is compared, not
// re-derived: a same-version role rewrite between confirmation and commit is a
// version_conflict.
func TestAuthorityPushServiceAdminConfirmsCallerPins(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	seedUnionMembership(t, fixture, tenantScope(), "membership-tenant-carol", carolPrincipal, 2, tenancy.RoleAdmin)
	seedUnionMembership(t, fixture, scope, "membership-project-carol", carolPrincipal, 5, tenancy.RoleViewer)

	read := authorize.AuthorityRead{
		Operation:   "pipeline.update",
		Required:    engineerRequires(),
		Check:       authorize.CheckPushServiceAdmin,
		Scope:       scope,
		PrincipalID: carolPrincipal,
	}
	evaluated := authorize.MembershipRead(read)
	fact, err := authorize.EvaluateMembership(evaluated, false, authorize.MembershipRows{
		TenantWide: authorize.MembershipRow{
			RowID: "membership-tenant-carol",
			Membership: &tenancy.Membership{
				Scope: tenantScope(), PrincipalID: carolPrincipal,
				Roles: []tenancy.Role{tenancy.RoleAdmin}, Version: 2,
			},
		},
		Project: authorize.MembershipRow{
			RowID: "membership-project-carol",
			Membership: &tenancy.Membership{
				Scope: scope, PrincipalID: carolPrincipal,
				Roles: []tenancy.Role{tenancy.RoleViewer}, Version: 5,
			},
		},
	})
	if err != nil {
		t.Fatalf("evaluate the Admin union: %v", err)
	}
	read.Targets = fact.Targets

	if _, err := authorityLocks(context.Background(), fixture.store, fixture.store, []authorize.AuthorityRead{read}, authorityContext{}); err != nil {
		t.Fatalf("unchanged pinned rows were refused: %v", err)
	}
	fixture.setMembershipRoles(t, tenantScope(), "membership-tenant-carol", 2, tenancy.RoleViewer)
	_, err = authorityLocks(context.Background(), fixture.store, fixture.store, []authorize.AuthorityRead{read}, authorityContext{})
	mustClass(t, err, fault.VersionConflict)
}

// TestAuthorityPushServiceAdminDeniesExecutionPrincipal proves the conditional
// Admin check is never satisfied by an execution permission. Admin is a human
// membership role: a running execution that the operation otherwise grants an
// execution permission must still be refused.
func TestAuthorityPushServiceAdminDeniesExecutionPrincipal(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedExecutionForPrincipal(t, scope, testExecution, "", executionPrincipal, executionStateRunning, 1)

	read := executionMembershipRead(scope, authorize.ExecOwnTaskContent)
	read.Check = authorize.CheckPushServiceAdmin
	_, err := authorityLocks(context.Background(), fixture.store, fixture.store, []authorize.AuthorityRead{read}, authorityContext{})
	mustClass(t, err, fault.Forbidden)
}
