package domaincontract

// Decision rows: the membership union at the operation boundary.
//
// The union is only useful if it changes the authorization answer. These are
// the decision rows:
//
//   - Bob is tenant-wide Engineer and project Reviewer. In the project he holds
//     BOTH: he can create a task (Engineer-only) AND resolve a review
//     (Reviewer operation). The project Reviewer row does not hide the
//     tenant-wide Engineer grant.
//   - Carol is tenant-wide Admin and project Viewer. In the project she keeps
//     Admin: administration operations stay allowed because the project Viewer
//     row can only ADD Viewer, never narrow away Admin.
//   - A principal who holds only a project row has no tenant-wide rights: a
//     tenant-scoped operation evaluates only the tenant-wide row, which is
//     absent.
//
// These vectors join the pure `tenancy.EffectiveRoles` rule to the
// operation registry. They do not implement an authorizer; they pin the grant
// answers a consumer must derive from the effective set.

import (
	"testing"

	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func effectiveRolesFor(t *testing.T, scope tenancy.Scope, tenantWide, projectRow *tenancy.Membership) []tenancy.Role {
	t.Helper()
	roles, ok := tenancy.EffectiveRoles(scope, tenantWide, projectRow)
	if !ok {
		// A missing applicable row is the no-membership refusal, not an empty
		// grant set. Return nil so every grant check denies.
		return nil
	}
	return roles
}

// humanGranted answers the human grant question for one operation from
// an effective role set. It mirrors the registry evaluator rather than
// re-deriving the registry: the operation must exist, the target scope must
// match the operation's declared shape, and at least one required human role
// must be held.
func humanGranted(t *testing.T, roles []tenancy.Role, operation authorize.OperationName, scope tenancy.Scope) bool {
	t.Helper()
	registered, ok := authorize.LookupOperation(operation)
	if !ok {
		t.Fatalf("operation %q is not registered", operation)
	}
	switch registered.Scope {
	case authorize.ScopeTenant:
		if !scope.TenantScoped() {
			t.Fatalf("%s is tenant-scoped but was authorized in %+v", operation, scope)
		}
	case authorize.ScopeProject:
		if !scope.ProjectScoped() {
			t.Fatalf("%s is project-scoped but was authorized in %+v", operation, scope)
		}
	}
	for _, required := range registered.Requires.HumanRoles {
		for _, held := range roles {
			if held == required {
				return true
			}
		}
	}
	return false
}

// TestBobUnionAllowsEngineerAndReviewerWork pins the first decision row. Bob's
// tenant-wide Engineer grant is not hidden by his project Reviewer row, so both
// task creation and review resolution are allowed in the project, while an
// Admin-only project operation is still denied.
func TestBobUnionAllowsEngineerAndReviewerWork(t *testing.T) {
	scope := projectScope()
	roles := effectiveRolesFor(t, scope,
		member(tenantScope(), "principal-bob", 4, tenancy.RoleEngineer),
		member(scope, "principal-bob", 9, tenancy.RoleReviewer),
	)
	if !equalRoles(roles, []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleReviewer}) {
		t.Fatalf("Bob effective roles = %v; want engineer and reviewer (the project row must not hide the tenant row)", roles)
	}

	for _, allowed := range []authorize.OperationName{"task.create", "execution.request.resolve"} {
		if !humanGranted(t, roles, allowed, scope) {
			t.Errorf("Bob was denied %s; the union must allow both his Engineer and Reviewer work", allowed)
		}
	}
	if humanGranted(t, roles, "service.create", scope) {
		t.Error("Bob was allowed service.create; the union must not invent Admin")
	}
}

// TestCarolUnionKeepsAdminInTheProject pins the no-narrowing decision. Carol's
// project Viewer row ADDS Viewer; it does not remove the tenant-wide Admin
// grant, so project administration stays allowed and Viewer-only reads stay
// allowed. It also pins that the union does not invent Engineer.
func TestCarolUnionKeepsAdminInTheProject(t *testing.T) {
	scope := projectScope()
	roles := effectiveRolesFor(t, scope,
		member(tenantScope(), "principal-carol", 2, tenancy.RoleAdmin),
		member(scope, "principal-carol", 5, tenancy.RoleViewer),
	)
	if !equalRoles(roles, []tenancy.Role{tenancy.RoleAdmin, tenancy.RoleViewer}) {
		t.Fatalf("Carol effective roles = %v; want admin and viewer (the project row must not narrow away admin)", roles)
	}

	for _, allowed := range []authorize.OperationName{"service.create", "resource.binding.update", "task.read"} {
		if !humanGranted(t, roles, allowed, scope) {
			t.Errorf("Carol was denied %s; the project row must not narrow away her tenant-wide Admin grant", allowed)
		}
	}
	if humanGranted(t, roles, "pipeline.apply", scope) {
		t.Error("Carol was allowed pipeline.apply; the union must not invent Engineer")
	}
}

// TestProjectOnlyMembershipHasNoTenantRights pins the third decision row: a
// principal holding only a project row is a member in that project but has no
// tenant-wide membership at all, so every tenant-scoped operation is denied.
func TestProjectOnlyMembershipHasNoTenantRights(t *testing.T) {
	project := projectScope()
	tenant := tenantScope()
	projectOnly := member(project, "principal-dana", 6, tenancy.RoleAdmin, tenancy.RoleViewer)

	tenantRoles, ok := tenancy.EffectiveRoles(tenant, nil, projectOnly)
	if ok {
		t.Fatalf("tenant-scoped EffectiveRoles ok = true with only a project row; roles = %v", tenantRoles)
	}
	if len(tenantRoles) != 0 {
		t.Fatalf("tenant-scoped EffectiveRoles = %v; want no roles from a project row", tenantRoles)
	}
	for _, denied := range []authorize.OperationName{"tenant.create", "project.create", "membership.grant", "membership.list"} {
		if humanGranted(t, nil, denied, tenant) {
			t.Errorf("a project-only principal was allowed tenant operation %s", denied)
		}
	}

	projectRoles, ok := tenancy.EffectiveRoles(project, nil, projectOnly)
	if !ok || !equalRoles(projectRoles, []tenancy.Role{tenancy.RoleAdmin, tenancy.RoleViewer}) {
		t.Fatalf("project-scoped EffectiveRoles = %v ok=%v; want the project grant", projectRoles, ok)
	}
	if !humanGranted(t, projectRoles, "service.create", project) {
		t.Error("a project-only Admin was denied a project-scoped administration operation")
	}
}
