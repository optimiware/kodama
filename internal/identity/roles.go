// Package identity implements Kodama's standalone identity authority: the
// mapping of verified OIDC principals to users, tenant/project bindings of five
// fixed roles, the current-access decision every module asks, and the one-time
// operator bootstrap (docs/specs/identity-tenancy-and-rbac.md).
//
// The package holds entities, the role policy, use cases and small
// consumer-owned ports. It imports neither pgx, net/http, go-oidc nor a broker;
// the OIDC verifier, PostgreSQL store, HTTP handlers and consumer bridges are
// adapters in sub-packages, wired by the composition root.
//
// Authority profiles: this package implements the standalone profile only. An
// Optimi-hosted installation selects a different Authority (O-18/T-061); the
// standalone grants are never consulted as a fallback (see SelectAuthority).
package identity

// Role is one of the five fixed roles. There are no custom roles.
type Role string

const (
	RoleAdmin    Role = "admin"
	RoleAnalyst  Role = "analyst"
	RoleEngineer Role = "engineer"
	RoleReviewer Role = "reviewer"
	RoleViewer   Role = "viewer"
)

// Roles lists the fixed roles in a stable order.
func Roles() []Role {
	return []Role{RoleAdmin, RoleAnalyst, RoleEngineer, RoleReviewer, RoleViewer}
}

// Valid reports whether r is one of the fixed roles.
func (r Role) Valid() bool {
	_, ok := rolePermissions[r]
	return ok
}

// Permission names an application-level action a role may grant. Permissions
// are distinct from HTTP route actions such as "work_item.transition": a route
// action maps to one or more permissions, and a field- or transition-specific
// check still needs its own permission (for example work_item:complete).
type Permission string

// The permission set. The work_item:* names equal the work module's values.
const (
	PermProjectRead      Permission = "project:read"
	PermProjectCreate    Permission = "project:create" // tenant scope only
	PermProjectSettings  Permission = "project:settings"
	PermMembershipManage Permission = "membership:manage"
	PermConfigRead       Permission = "config:read"  // nonsecret project variables
	PermConfigWrite      Permission = "config:write" // nonsecret project variables
	PermCredentialRead   Permission = "credential:read"
	PermCredentialManage Permission = "credential:manage" // metadata and versions; never plaintext

	PermWorkRead          Permission = "work_item:read"
	PermWorkCreate        Permission = "work_item:create"
	PermWorkEditSpec      Permission = "work_item:edit_spec"
	PermWorkEditTechnical Permission = "work_item:edit_technical"
	PermWorkRefine        Permission = "work_item:refine"
	PermWorkWork          Permission = "work_item:work"
	PermWorkComplete      Permission = "work_item:complete" // human only
	PermWorkReopen        Permission = "work_item:reopen"
	PermWorkCancel        Permission = "work_item:cancel"

	PermWorkspaceManage  Permission = "workspace:manage"
	PermRunRead          Permission = "run:read"
	PermRunManage        Permission = "run:manage"
	PermDeploymentManage Permission = "deployment:manage"
	PermGateDecide       Permission = "gate:decide" // human only
	PermEvidenceRead     Permission = "evidence:read"
	PermSourceRead       Permission = "source:read"
)

// No role grants secret plaintext: there is deliberately no permission for it.
// Plaintext is an internal attempt interface resolved by the secrets module.

var viewerPermissions = []Permission{
	PermProjectRead, PermConfigRead, PermWorkRead, PermRunRead, PermEvidenceRead, PermSourceRead,
}

func union(base []Permission, more ...Permission) []Permission {
	return append(append([]Permission(nil), base...), more...)
}

// rolePermissions is the fixed role matrix. Admin is deliberately not a
// reviewer, engineer or analyst: composing roles is an explicit, audited grant.
var rolePermissions = func() map[Role]map[Permission]bool {
	m := map[Role][]Permission{
		RoleAdmin: {
			PermProjectRead, PermProjectCreate, PermProjectSettings, PermMembershipManage,
			PermConfigRead, PermConfigWrite, PermCredentialRead, PermCredentialManage,
		},
		RoleAnalyst: union(viewerPermissions, PermWorkCreate, PermWorkEditSpec, PermWorkRefine),
		RoleEngineer: union(viewerPermissions, PermWorkCreate, PermWorkEditSpec, PermWorkEditTechnical, PermWorkRefine,
			PermWorkWork, PermWorkCancel, PermWorkReopen, PermWorkspaceManage, PermRunManage, PermDeploymentManage,
			PermConfigWrite, PermCredentialRead),
		RoleReviewer: union(viewerPermissions, PermWorkComplete, PermWorkReopen, PermWorkCancel, PermGateDecide),
		RoleViewer:   viewerPermissions,
	}
	out := make(map[Role]map[Permission]bool, len(m))
	for r, perms := range m {
		set := make(map[Permission]bool, len(perms))
		for _, p := range perms {
			set[p] = true
		}
		out[r] = set
	}
	return out
}()

// knownPermissions is every permission some role may hold. A permission outside
// this set is unknown and always denied.
var knownPermissions = func() map[Permission]bool {
	s := map[Permission]bool{}
	for _, set := range rolePermissions {
		for p := range set {
			s[p] = true
		}
	}
	return s
}()

// Grants reports whether the role carries the permission.
func (r Role) Grants(p Permission) bool { return rolePermissions[r][p] }

// Known reports whether p is a permission of the fixed matrix.
func (p Permission) Known() bool { return knownPermissions[p] }

// HumanOnly reports whether the permission needs a verified human actor in
// addition to the role: gate decisions and work completion.
func (p Permission) HumanOnly() bool { return p == PermWorkComplete || p == PermGateDecide }

// TenantOnly reports whether the permission can be exercised only through a
// tenant-wide binding (a project binding never grants it).
func (p Permission) TenantOnly() bool { return p == PermProjectCreate }

// PermissionsOf lists the role's permissions (sorted by name) for documentation
// and tests.
func PermissionsOf(r Role) []Permission {
	var out []Permission
	for p := range rolePermissions[r] {
		out = append(out, p)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
