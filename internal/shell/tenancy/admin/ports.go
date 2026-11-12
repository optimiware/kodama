package tenancyadmin

import "context"

// TenantCreatePort applies one complete tenant-creation commit unit. The
// initial encrypted derivation key commits with the tenant row.
type TenantCreatePort interface {
	CreateTenant(context.Context, TenantCreateUnit) (TenantCreateResult, error)
}

// TenantKeyRotationPort applies one complete tenant derivation-key rotation
// commit unit. It must compare the expected tenant and key versions and re-read
// the supplied current authority inside the same transaction as the key,
// tenant-version, receipt, audit and event writes.
type TenantKeyRotationPort interface {
	RotateTenantKey(context.Context, TenantKeyRotationUnit) (TenantKeyRotationResult, error)
}

// ProjectCreatePort applies one complete project-creation commit unit.
type ProjectCreatePort interface {
	CreateProject(context.Context, ProjectCreateUnit) (ProjectCreateResult, error)
}

// ProjectUpdatePort applies one complete project-update commit unit.
//
// UpdateProject must compare the expected project version and re-read the
// supplied current authority inside the same transaction as the project write,
// receipt, audit and event publication intent.
type ProjectUpdatePort interface {
	UpdateProject(context.Context, ProjectUpdateUnit) (ProjectUpdateResult, error)
}

// MembershipGrantPort applies one complete membership-grant commit unit.
type MembershipGrantPort interface {
	GrantMembership(context.Context, MembershipGrantUnit) (MembershipGrantResult, error)
}

// MembershipRevokePort applies one complete membership-revocation commit unit.
type MembershipRevokePort interface {
	RevokeMembership(context.Context, MembershipRevokeUnit) (MembershipRevokeResult, error)
}

// MembershipQueryPort returns one authoritative, stably paginated membership
// page. The adapter applies current scope and roles at query time; a query is
// never durable permission.
type MembershipQueryPort interface {
	ListMemberships(context.Context, MembershipQuery) (MembershipPage, error)
}
