package tenancy

// Membership union.
//
// A membership row grants roles either tenant-wide (Scope.ProjectID empty) or
// in exactly one project. A project row never replaces the tenant-wide row: a
// lookup that fell back to the tenant-wide row only when the project row was
// missing would let a project row silently remove every tenant-wide grant from
// the effective set. The rule is an explicit union:
//
//   - a project-scoped call reads BOTH the tenant-wide row and the exact
//     project row; the effective roles are their set union, deduplicated and
//     returned in the declared role declaration order;
//   - a tenant-scoped call reads ONLY the tenant-wide row; the project row is
//     ignored, so a project-only membership grants no tenant-wide right;
//   - a project row can only ADD roles, never narrow an existing grant;
//   - either row may be absent; no applicable row at all is the no-membership
//     refusal (ok false), while an applicable row that grants nothing is a
//     membership with an empty effective set (ok true);
//   - services use the same union, but Admin is never service-assignable and
//     never appears in a service's effective set even from a corrupt row.
//
// These are pure declarations. They read no clock and perform no I/O, and they
// never mutate their input rows; adapters consume them. The authority commit
// unit pins both rows by store identity AND version through
// authorize.MembershipTargets, then re-reads the current rows and re-evaluates
// this union against the required role inside the unit. A version match alone
// is not sufficient: a revoked row can be re-granted under a new identity at a
// restarted version, and a contrived same-version role rewrite still changes
// the effective set.

// EffectiveRoles returns the union of the roles granted by every membership row
// applicable to the caller's scope, deduplicated and in tenancy.Roles order.
//
// The tenantWide row contributes only when it is a tenant-wide row of the
// caller's tenant. The project row contributes only for a project-scoped call
// and only when it names exactly that project. The bool reports whether any
// applicable row exists: false is the no-membership refusal and nil roles.
func EffectiveRoles(scope Scope, tenantWide, project *Membership) ([]Role, bool) {
	return effectiveRoles(scope, tenantWide, project, false)
}

// EffectiveServiceRoles applies the same union to a service principal's rows
// and removes Admin, which is never service-assignable.
func EffectiveServiceRoles(scope Scope, tenantWide, project *Membership) ([]Role, bool) {
	return effectiveRoles(scope, tenantWide, project, true)
}

// AssignableServiceRoles returns the roles a service account may hold from one
// observed services row: Admin and every unknown value are dropped and the rest
// keep the observed order. A services row is not a membership row, so the union
// above does not apply to it, but the guarantee is the same one as
// EffectiveServiceRoles: a corrupt or mis-written row can never make a service
// an Admin or invent a role.
func AssignableServiceRoles(roles []Role) []Role {
	filtered := make([]Role, 0, len(roles))
	for _, role := range roles {
		if role.ServiceAssignable() {
			filtered = append(filtered, role)
		}
	}
	return filtered
}

// effectiveRoles is the single union implementation shared by human and service
// principals. Iterating the declared role table keeps the result in declaration
// order and drops duplicate and unknown roles.
func effectiveRoles(scope Scope, tenantWide, project *Membership, service bool) ([]Role, bool) {
	applicable := make([]*Membership, 0, 2)
	if tenantWideApplies(scope, tenantWide) {
		applicable = append(applicable, tenantWide)
	}
	if projectApplies(scope, project) {
		applicable = append(applicable, project)
	}
	if len(applicable) == 0 {
		return nil, false
	}
	var roles []Role
	for _, role := range roleTable {
		if service && role == RoleAdmin {
			continue
		}
		for _, row := range applicable {
			if row.Has(role) {
				roles = append(roles, role)
				break
			}
		}
	}
	return roles, true
}

// tenantWideApplies reports whether the row is the tenant-wide row of the
// caller's tenant. A project-scoped row is never a tenant-wide contribution.
func tenantWideApplies(scope Scope, row *Membership) bool {
	return row != nil && row.Scope.TenantScoped() && row.Scope.TenantID == scope.TenantID
}

// projectApplies reports whether the row is the exact project row of a
// project-scoped caller. A tenant-wide or foreign-project row never applies.
func projectApplies(scope Scope, row *Membership) bool {
	return scope.ProjectScoped() && row != nil && row.Scope == scope
}
