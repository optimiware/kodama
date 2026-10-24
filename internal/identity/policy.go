package identity

import "github.com/optimiware/kodama/internal/domain"

// Decide answers the effective-access question for one actor, given the actor's
// current bindings in the target's tenant. It is pure: callers read the bindings
// inside the transaction or request that needs the answer.
//
// Outcomes: nil allows; ErrHidden means no binding reaches the target (the
// caller must not learn whether it exists); ErrDenied means the caller is a
// member whose composed roles lack the permission, or the actor type, the
// permission or the scope is not allowed. Unknown permissions deny by default.
//
// Bindings compose only at the tenant and the matching project; a binding of
// another tenant or project is ignored, never widened.
func Decide(bindings []Binding, actor Actor, target Target, perm Permission) error {
	if err := target.TenantID.Validate(); err != nil {
		return ErrHidden
	}
	// Only users hold roles; workloads get explicit attempt-scoped actions.
	if actor.Type != ActorUser || actor.Validate() != nil {
		return ErrDenied
	}
	inTenant := 0
	var reaching []Binding
	for _, b := range bindings {
		if b.Tenant != target.TenantID || string(b.User) != actor.ID {
			continue
		}
		inTenant++
		if b.Applies(target) {
			reaching = append(reaching, b)
		}
	}
	if inTenant == 0 {
		return ErrHidden
	}
	// A project-only member asking about another project learns nothing; asking
	// for tenant-level access they know the tenant exists and are denied.
	if len(reaching) == 0 {
		if target.ProjectID != "" {
			return ErrHidden
		}
		return ErrDenied
	}
	if !perm.Known() || (perm.HumanOnly() && !actor.IsHuman()) {
		return ErrDenied
	}
	for _, b := range reaching {
		if perm.TenantOnly() && !b.TenantWide() {
			continue
		}
		if b.Role.Grants(perm) {
			return nil
		}
	}
	return ErrDenied
}

// RolesAt lists the distinct roles the actor holds that reach the target, for
// narrowing lists and use-case decisions.
func RolesAt(bindings []Binding, target Target) []Role {
	seen := map[Role]bool{}
	var out []Role
	for _, b := range bindings {
		if b.Applies(target) && !seen[b.Role] {
			seen[b.Role] = true
			out = append(out, b.Role)
		}
	}
	return out
}

// Reach is the set of scopes of one tenant where the actor holds a permission:
// every scope (a tenant-wide binding grants it) or the listed projects.
type Reach struct {
	All      bool
	Projects []domain.ProjectID
}

// Contains reports whether the reach includes the project.
func (r Reach) Contains(p domain.ProjectID) bool {
	if r.All {
		return true
	}
	for _, x := range r.Projects {
		if x == p {
			return true
		}
	}
	return false
}

// ReachOf computes where the bindings grant the permission in the tenant.
func ReachOf(bindings []Binding, tenant domain.TenantID, perm Permission) Reach {
	var r Reach
	seen := map[domain.ProjectID]bool{}
	for _, b := range bindings {
		if b.Tenant != tenant || !b.Role.Grants(perm) {
			continue
		}
		if b.TenantWide() {
			r.All = true
			continue
		}
		if !perm.TenantOnly() && !seen[b.Project] {
			seen[b.Project] = true
			r.Projects = append(r.Projects, b.Project)
		}
	}
	return r
}
