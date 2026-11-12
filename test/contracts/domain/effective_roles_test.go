package domaincontract

// The membership union.
//
// A project-scoped membership lookup does not read the exact project row and
// fall back to the tenant-wide row only when the project row is absent: that
// rule would let a project row HIDE the tenant-wide grant (Bob tenant Engineer
// + project Reviewer would see only Reviewer; Carol tenant Admin + project
// Viewer would see only Viewer). The rule is an explicit UNION:
//
//   - a project-scoped call reads BOTH the tenant-wide row and the exact
//     project row; the effective roles are their set union, deduplicated and in
//     the role declaration order;
//   - a tenant-scoped call reads ONLY the tenant-wide row; the project row is
//     ignored, so a project-only membership grants no tenant-wide right;
//   - either row may be absent; both absent is the no-membership refusal;
//   - a project row can only ADD roles; there is no narrowing;
//   - the union applies to services too, but Admin is never service-assignable
//     and is removed from a service's effective set.
//
// `tenancy.EffectiveRoles` and `tenancy.EffectiveServiceRoles` are the pure
// declarations of that rule; this vector pins the total function table.

import (
	"testing"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func tenantScope() tenancy.Scope {
	return tenancy.Scope{TenantID: id.TenantID("tenant-1")}
}

func projectScope() tenancy.Scope {
	return tenancy.Scope{
		TenantID:  id.TenantID("tenant-1"),
		ProjectID: id.ProjectID("project-1"),
	}
}

func otherProjectScope() tenancy.Scope {
	return tenancy.Scope{
		TenantID:  id.TenantID("tenant-1"),
		ProjectID: id.ProjectID("project-2"),
	}
}

func otherTenantScope() tenancy.Scope {
	return tenancy.Scope{TenantID: id.TenantID("tenant-2")}
}

// member builds one observed membership row. Version is the optimistic version
// the row had when the caller read it; roles is the grant set.
func member(scope tenancy.Scope, principal string, version uint64, roles ...tenancy.Role) *tenancy.Membership {
	return &tenancy.Membership{
		Scope:       scope,
		PrincipalID: id.PrincipalID(principal),
		Roles:       roles,
		Version:     version,
	}
}

// TestEffectiveRolesGoldenTable pins the total union rule: which rows apply for
// each scope shape, that a project row never hides the tenant-wide row, that
// the result is deduplicated and returned in the declared role order, and that
// ok distinguishes "no applicable row" from "an applicable row that grants
// nothing".
func TestEffectiveRolesGoldenTable(t *testing.T) {
	project := projectScope()
	tenant := tenantScope()

	cases := []struct {
		name       string
		scope      tenancy.Scope
		tenantWide *tenancy.Membership
		projectRow *tenancy.Membership
		want       []tenancy.Role
		wantOK     bool
	}{
		{
			name:       "tenant-wide row only, project-scoped call",
			scope:      project,
			tenantWide: member(tenant, "principal-1", 3, tenancy.RoleEngineer),
			want:       []tenancy.Role{tenancy.RoleEngineer},
			wantOK:     true,
		},
		{
			name:       "project row only, project-scoped call",
			scope:      project,
			projectRow: member(project, "principal-1", 7, tenancy.RoleReviewer),
			want:       []tenancy.Role{tenancy.RoleReviewer},
			wantOK:     true,
		},
		{
			name:       "both rows union instead of the project row hiding the tenant row",
			scope:      project,
			tenantWide: member(tenant, "principal-1", 3, tenancy.RoleEngineer),
			projectRow: member(project, "principal-1", 7, tenancy.RoleReviewer),
			want:       []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleReviewer},
			wantOK:     true,
		},
		{
			name:       "union is deduplicated and in role declaration order",
			scope:      project,
			tenantWide: member(tenant, "principal-1", 3, tenancy.RoleEngineer, tenancy.RoleAdmin, tenancy.RoleEngineer),
			projectRow: member(project, "principal-1", 7, tenancy.RoleReviewer, tenancy.RoleAdmin, tenancy.RoleReviewer),
			want:       []tenancy.Role{tenancy.RoleAdmin, tenancy.RoleEngineer, tenancy.RoleReviewer},
			wantOK:     true,
		},
		{
			name:   "neither row, project-scoped call",
			scope:  project,
			want:   nil,
			wantOK: false,
		},
		{
			name:       "tenant-scoped call ignores the project row entirely",
			scope:      tenant,
			projectRow: member(project, "principal-1", 7, tenancy.RoleAdmin),
			want:       nil,
			wantOK:     false,
		},
		{
			name:       "tenant-scoped call reads only the tenant-wide row",
			scope:      tenant,
			tenantWide: member(tenant, "principal-1", 3, tenancy.RoleViewer),
			projectRow: member(project, "principal-1", 7, tenancy.RoleAdmin),
			want:       []tenancy.Role{tenancy.RoleViewer},
			wantOK:     true,
		},
		{
			name:       "empty project row still counts as a membership and adds nothing",
			scope:      project,
			tenantWide: member(tenant, "principal-1", 3, tenancy.RoleEngineer),
			projectRow: member(project, "principal-1", 7),
			want:       []tenancy.Role{tenancy.RoleEngineer},
			wantOK:     true,
		},
		{
			name:       "empty project row with no tenant row is a membership that grants nothing",
			scope:      project,
			projectRow: member(project, "principal-1", 7),
			want:       nil,
			wantOK:     true,
		},
		{
			name:       "a project row for another project is not applicable",
			scope:      project,
			projectRow: member(otherProjectScope(), "principal-1", 7, tenancy.RoleAdmin),
			want:       nil,
			wantOK:     false,
		},
		{
			name:       "a tenant-wide row for another tenant is not applicable",
			scope:      project,
			tenantWide: member(otherTenantScope(), "principal-1", 3, tenancy.RoleAdmin),
			want:       nil,
			wantOK:     false,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, ok := tenancy.EffectiveRoles(testCase.scope, testCase.tenantWide, testCase.projectRow)
			if ok != testCase.wantOK {
				t.Fatalf("EffectiveRoles ok = %v; want %v", ok, testCase.wantOK)
			}
			if !equalRoles(got, testCase.want) {
				t.Fatalf("EffectiveRoles = %v; want %v", got, testCase.want)
			}
		})
	}
}

// TestEffectiveServiceRolesNeverGainsAdmin pins the service variant: it applies
// the same union to a service principal's two rows, but Admin is never
// service-assignable and therefore never appears in the effective set even when
// a corrupt row carries it.
func TestEffectiveServiceRolesNeverGainsAdmin(t *testing.T) {
	project := projectScope()
	tenant := tenantScope()

	cases := []struct {
		name       string
		scope      tenancy.Scope
		tenantWide *tenancy.Membership
		projectRow *tenancy.Membership
		want       []tenancy.Role
		wantOK     bool
	}{
		{
			name:       "service union keeps the non-admin roles",
			scope:      project,
			tenantWide: member(tenant, "service-1", 2, tenancy.RoleEngineer),
			projectRow: member(project, "service-1", 4, tenancy.RoleReviewer),
			want:       []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleReviewer},
			wantOK:     true,
		},
		{
			name:       "a corrupt tenant-wide admin grant never reaches a service",
			scope:      project,
			tenantWide: member(tenant, "service-1", 2, tenancy.RoleAdmin),
			want:       nil,
			wantOK:     true,
		},
		{
			name:       "a corrupt project admin grant never reaches a service",
			scope:      project,
			tenantWide: member(tenant, "service-1", 2, tenancy.RoleReviewer),
			projectRow: member(project, "service-1", 4, tenancy.RoleAdmin),
			want:       []tenancy.Role{tenancy.RoleReviewer},
			wantOK:     true,
		},
		{
			name:       "service tenant-scoped call ignores the project row",
			scope:      tenant,
			projectRow: member(project, "service-1", 4, tenancy.RoleEngineer),
			want:       nil,
			wantOK:     false,
		},
		{
			name:   "service with neither row",
			scope:  project,
			want:   nil,
			wantOK: false,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, ok := tenancy.EffectiveServiceRoles(testCase.scope, testCase.tenantWide, testCase.projectRow)
			if ok != testCase.wantOK {
				t.Fatalf("EffectiveServiceRoles ok = %v; want %v", ok, testCase.wantOK)
			}
			if !equalRoles(got, testCase.want) {
				t.Fatalf("EffectiveServiceRoles = %v; want %v", got, testCase.want)
			}
			for _, role := range got {
				if role == tenancy.RoleAdmin {
					t.Fatalf("EffectiveServiceRoles returned Admin: %v", got)
				}
			}
		})
	}
}

// TestEffectiveRolesIsDeterministicAndReadOnly pins that repeated evaluation of
// the same rows returns the same ordered set and does not mutate either input
// row's role slice. Callers share membership values between authorization
// boundaries, so a role evaluation must not become a hidden write.
func TestEffectiveRolesIsDeterministicAndReadOnly(t *testing.T) {
	tenantWide := member(tenantScope(), "principal-1", 3, tenancy.RoleEngineer, tenancy.RoleAdmin)
	projectRow := member(projectScope(), "principal-1", 7, tenancy.RoleReviewer)
	beforeTenant := append([]tenancy.Role(nil), tenantWide.Roles...)
	beforeProject := append([]tenancy.Role(nil), projectRow.Roles...)

	first, firstOK := tenancy.EffectiveRoles(projectScope(), tenantWide, projectRow)
	second, secondOK := tenancy.EffectiveRoles(projectScope(), tenantWide, projectRow)
	if firstOK != secondOK || !equalRoles(first, second) {
		t.Fatalf("EffectiveRoles is not deterministic: %v/%v then %v/%v", first, firstOK, second, secondOK)
	}
	if !equalRoles(tenantWide.Roles, beforeTenant) || !equalRoles(projectRow.Roles, beforeProject) {
		t.Fatalf("EffectiveRoles mutated its input rows: tenant %v -> %v, project %v -> %v",
			beforeTenant, tenantWide.Roles, beforeProject, projectRow.Roles)
	}
}

func equalRoles(left, right []tenancy.Role) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
