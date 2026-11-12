package tenancyadmin

import (
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

// TenantCreateCommand asks to create one tenant and its initial encrypted
// derivation key. The actor's verified scope supplies authority only; it is not
// the tenant identity, because a tenant is created before any member exists.
type TenantCreateCommand struct {
	Actor     authorize.Principal `json:"actor"`
	TenantID  id.TenantID         `json:"tenant_id"`
	Name      string              `json:"name"`
	Key       token.TenantKey     `json:"key"`
	CallerKey string              `json:"caller_key"`
	Content   receipt.Content     `json:"content"`
}

// TenantKeyRotationCommand asks to rotate one tenant's derivation key. Key is
// the next encrypted key; its version must advance ExpectedVersion by one.
type TenantKeyRotationCommand struct {
	Actor           authorize.Principal `json:"actor"`
	TenantID        id.TenantID         `json:"tenant_id"`
	Name            string              `json:"name"`
	ExpectedVersion uint64              `json:"expected_version"`
	Key             token.TenantKey     `json:"key"`
	CallerKey       string              `json:"caller_key"`
	Content         receipt.Content     `json:"content"`
}

// ProjectCreateCommand asks to create one project in the actor's tenant.
type ProjectCreateCommand struct {
	Actor     authorize.Principal `json:"actor"`
	ProjectID id.ProjectID        `json:"project_id"`
	Name      string              `json:"name"`
	CallerKey string              `json:"caller_key"`
	Content   receipt.Content     `json:"content"`
}

// ProjectUpdateCommand asks to rename one project at an expected version.
type ProjectUpdateCommand struct {
	Actor           authorize.Principal `json:"actor"`
	ProjectID       id.ProjectID        `json:"project_id"`
	ExpectedVersion uint64              `json:"expected_version"`
	Name            string              `json:"name"`
	CallerKey       string              `json:"caller_key"`
	Content         receipt.Content     `json:"content"`
}

// MembershipGrantCommand asks to create or replace one membership grant. The
// target membership carries the owning scope, principal and complete role set;
// its version is derived from ExpectedVersion.
type MembershipGrantCommand struct {
	Actor           authorize.Principal `json:"actor"`
	Membership      tenancy.Membership  `json:"membership"`
	ExpectedVersion uint64              `json:"expected_version"`
	CallerKey       string              `json:"caller_key"`
	Content         receipt.Content     `json:"content"`
}

// MembershipRevokeCommand asks to remove one membership grant.
type MembershipRevokeCommand struct {
	Actor           authorize.Principal `json:"actor"`
	Scope           tenancy.Scope       `json:"scope"`
	PrincipalID     id.PrincipalID      `json:"principal_id"`
	ExpectedVersion uint64              `json:"expected_version"`
	CallerKey       string              `json:"caller_key"`
	Content         receipt.Content     `json:"content"`
}

// MembershipListCommand asks for one scoped membership page.
type MembershipListCommand struct {
	Actor       authorize.Principal `json:"actor"`
	Scope       tenancy.Scope       `json:"scope"`
	PrincipalID id.PrincipalID      `json:"principal_id,omitempty"`
	Cursor      string              `json:"cursor,omitempty"`
	Limit       int                 `json:"limit"`
}

// withinScope reports whether target is inside the actor's verified scope. A
// tenant-scoped actor covers every project of its tenant; a project-scoped
// actor covers exactly its own project and never widens to the tenant.
func withinScope(actor, target tenancy.Scope) bool {
	if actor.TenantID == "" || target.TenantID == "" {
		return false
	}
	if actor.TenantID != target.TenantID {
		return false
	}
	if actor.ProjectID == "" {
		return true
	}
	return actor.ProjectID == target.ProjectID
}

// cloneTenantKey returns an independent copy of the encrypted key so a caller
// cannot mutate committed key material after the unit was built.
func cloneTenantKey(key token.TenantKey) token.TenantKey {
	key.Ciphertext = append([]byte(nil), key.Ciphertext...)
	key.WrappedKey = append([]byte(nil), key.WrappedKey...)
	key.WrapNonce = append([]byte(nil), key.WrapNonce...)
	return key
}

// cloneMembershipPage returns an independent copy of a membership page so a
// caller cannot mutate port-owned rows.
func cloneMembershipPage(page MembershipPage) MembershipPage {
	cloned := MembershipPage{
		Memberships: make([]tenancy.Membership, len(page.Memberships)),
		NextCursor:  page.NextCursor,
	}
	for index, membership := range page.Memberships {
		cloned.Memberships[index] = tenancy.Membership{
			Scope:       membership.Scope,
			PrincipalID: membership.PrincipalID,
			Roles:       append([]tenancy.Role(nil), membership.Roles...),
			Version:     membership.Version,
		}
	}
	return cloned
}
