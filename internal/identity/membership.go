package identity

import (
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// Binding is one fixed-role binding of a user at tenant scope (Project empty) or
// project scope. Tenant, User and Project are immutable once granted; only the
// role changes, through ChangeRole. Several bindings of one user compose.
type Binding struct {
	ID        BindingID
	Tenant    domain.TenantID
	User      UserID
	Project   domain.ProjectID // empty: tenant-wide
	Role      Role
	Version   domain.Version
	CreatedAt time.Time
}

// Target is where access is requested. ProjectID is empty for tenant-level
// access. AnyProject asks whether the permission is held at tenant scope or at
// any project of the tenant; it opens administrative routes whose use case then
// narrows the result to the scopes the caller administers.
type Target struct {
	TenantID   domain.TenantID
	ProjectID  domain.ProjectID
	AnyProject bool
}

// TenantWide reports whether the binding applies to the whole tenant.
func (b Binding) TenantWide() bool { return b.Project == "" }

// Applies reports whether the binding reaches the target.
func (b Binding) Applies(t Target) bool {
	if b.Tenant != t.TenantID {
		return false
	}
	if t.AnyProject || b.TenantWide() {
		return true
	}
	return t.ProjectID != "" && b.Project == t.ProjectID
}

// Validate checks the binding's invariants.
func (b Binding) Validate() error {
	if err := b.ID.Validate(); err != nil {
		return err
	}
	if err := b.Tenant.Validate(); err != nil {
		return invalid("%v", err)
	}
	if err := b.User.Validate(); err != nil {
		return err
	}
	if b.Project != "" {
		if err := b.Project.Validate(); err != nil {
			return invalid("%v", err)
		}
	}
	if !b.Role.Valid() {
		return invalid("role must be one of admin, analyst, engineer, reviewer, viewer")
	}
	return b.Version.Validate()
}

// Grant creates a binding at version 1. A custom role is rejected.
func Grant(id BindingID, tenant domain.TenantID, user UserID, project domain.ProjectID, role Role, now time.Time) (Binding, error) {
	b := Binding{ID: id, Tenant: tenant, User: user, Project: project, Role: role, Version: 1, CreatedAt: now.UTC()}
	if err := b.Validate(); err != nil {
		return Binding{}, err
	}
	return b, nil
}

// ChangeRole returns the binding with another fixed role and the next version.
// Changing to the same role is ErrNoChange.
func (b Binding) ChangeRole(role Role) (Binding, error) {
	if !role.Valid() {
		return Binding{}, invalid("role must be one of admin, analyst, engineer, reviewer, viewer")
	}
	if role == b.Role {
		return Binding{}, ErrNoChange
	}
	next := b
	next.Role = role
	next.Version = b.Version + 1
	if err := next.Version.Validate(); err != nil {
		return Binding{}, invalid("%v", err)
	}
	return next, nil
}

// Duplicates reports whether other holds the same user, scope and role.
func (b Binding) Duplicates(other Binding) bool {
	return b.ID != other.ID && b.Tenant == other.Tenant && b.User == other.User && b.Project == other.Project && b.Role == other.Role
}

// CheckNoDuplicate rejects a candidate that duplicates an existing binding.
func CheckNoDuplicate(candidate Binding, existing []Binding) error {
	for _, e := range existing {
		if candidate.Duplicates(e) {
			return ErrConflict
		}
	}
	return nil
}

// Revoked is the record of a removed binding. The binding's identity and final
// version remain in audit and events.
type Revoked struct {
	Binding Binding
}

// Revoke ends the binding.
func (b Binding) Revoke() Revoked { return Revoked{Binding: b} }
