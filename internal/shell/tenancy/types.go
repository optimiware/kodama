// Package tenancy declares the tenant, project and membership vocabulary the
// Shell authorizes against.
//
// A tenant owns an isolated organization; a project groups software, work,
// configuration and resources inside that tenant. Membership is the current
// relational grant of human roles, read again at every authorization boundary
// rather than cached in a token or request body. This package declares the
// values and the role vocabulary; the admin subpackage owns every
// administration behavior and authorize.AuthorityReader supplies current
// membership facts to the authorizer.
//
// This package imports only neutral contract packages (id, fault); it never
// imports a store, transport, identity provider or another capability's
// implementation.
package tenancy

import (
	"fmt"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
)

// Role is one current human or service role in a membership.
//
// Admin and Reviewer are deliberately distinct authorities: Admin configures
// the factory, Reviewer decides assigned requests. Neither implies the other.
type Role string

const (
	// RoleAdmin administers tenants, projects, memberships, bindings,
	// services and credentials. Admin is never a service-assignable role.
	RoleAdmin Role = "admin"
	// RoleEngineer authors pipelines and works with tasks and executions.
	RoleEngineer Role = "engineer"
	// RoleReviewer decides requests assigned to its human role.
	RoleReviewer Role = "reviewer"
	// RoleOperator performs authorized runtime follow-up, retries and
	// cancellation and works the operational inbox.
	RoleOperator Role = "operator"
	// RoleViewer has authorized read access only.
	RoleViewer Role = "viewer"
)

// roleTable is the single ordered role vocabulary. Roles, HumanRoles and the
// validators all derive from it so the lists cannot drift.
var roleTable = [...]Role{
	RoleAdmin,
	RoleEngineer,
	RoleReviewer,
	RoleOperator,
	RoleViewer,
}

// Roles returns every role in declaration order.
func Roles() []Role {
	return append([]Role(nil), roleTable[:]...)
}

// HumanRoles returns the roles a verified human membership may hold. It is the
// same vocabulary as Roles; Admin is human-only rather than a different kind.
func HumanRoles() []Role {
	return Roles()
}

// ServiceRoles returns the roles a service account may hold. Admin is absent
// because service accounts never administer the factory.
func ServiceRoles() []Role {
	return []Role{RoleEngineer, RoleReviewer, RoleOperator, RoleViewer}
}

// Valid reports whether the role is one of the declared roles.
func (role Role) Valid() bool {
	for _, candidate := range roleTable {
		if role == candidate {
			return true
		}
	}
	return false
}

// ServiceAssignable reports whether a service account may hold the role.
func (role Role) ServiceAssignable() bool {
	return role.Valid() && role != RoleAdmin
}

// Scope is the ownership scope of one entity or operation.
//
// TenantID is always required. An empty ProjectID means the entity is
// tenant-scoped; a present ProjectID means it is project-scoped. There is no
// third "global" scope in the Shell domain.
type Scope struct {
	TenantID  id.TenantID  `json:"tenant_id"`
	ProjectID id.ProjectID `json:"project_id,omitempty"`
}

// TenantScoped reports whether the scope covers the whole tenant.
func (scope Scope) TenantScoped() bool {
	return scope.ProjectID == ""
}

// ProjectScoped reports whether the scope is narrowed to one project.
func (scope Scope) ProjectScoped() bool {
	return scope.ProjectID != ""
}

// Validate checks that the scope names a usable tenant and, when present, a
// usable project. Failures use the declared invalid_input class.
func (scope Scope) Validate() error {
	if _, err := id.ParseTenantID(string(scope.TenantID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "scope tenant id is invalid", err)
	}
	if scope.ProjectID != "" {
		if _, err := id.ParseProjectID(string(scope.ProjectID)); err != nil {
			return fault.Wrap(fault.InvalidInput, "scope project id is invalid", err)
		}
	}
	return nil
}

// Tenant is one isolated organization.
type Tenant struct {
	ID      id.TenantID `json:"id"`
	Name    string      `json:"name"`
	Version uint64      `json:"version"`
}

// Validate checks the tenant identity and display name.
func (tenant Tenant) Validate() error {
	if _, err := id.ParseTenantID(string(tenant.ID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "tenant id is invalid", err)
	}
	if tenant.Name == "" {
		return fault.New(fault.InvalidInput, "tenant name is empty")
	}
	return nil
}

// Project is one project inside a tenant.
type Project struct {
	ID       id.ProjectID `json:"id"`
	TenantID id.TenantID  `json:"tenant_id"`
	Name     string       `json:"name"`
	Version  uint64       `json:"version"`
}

// Validate checks the project identity, owning tenant and display name.
func (project Project) Validate() error {
	if _, err := id.ParseProjectID(string(project.ID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "project id is invalid", err)
	}
	if _, err := id.ParseTenantID(string(project.TenantID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "project tenant id is invalid", err)
	}
	if project.Name == "" {
		return fault.New(fault.InvalidInput, "project name is empty")
	}
	return nil
}

// Membership is the current relational grant of roles to one principal in one
// scope. Version advances on every grant or revoke so a stale administration
// command is refused rather than silently applied.
type Membership struct {
	Scope       Scope          `json:"scope"`
	PrincipalID id.PrincipalID `json:"principal_id"`
	Roles       []Role         `json:"roles"`
	Version     uint64         `json:"version"`
}

// Validate checks the membership scope, principal and roles.
func (membership Membership) Validate() error {
	if err := membership.Scope.Validate(); err != nil {
		return err
	}
	if _, err := id.ParsePrincipalID(string(membership.PrincipalID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "membership principal id is invalid", err)
	}
	for _, role := range membership.Roles {
		if !role.Valid() {
			return fault.New(fault.InvalidInput, fmt.Sprintf("unknown membership role %q", role))
		}
	}
	return nil
}

// Has reports whether the membership currently holds the role.
func (membership Membership) Has(role Role) bool {
	for _, candidate := range membership.Roles {
		if candidate == role {
			return true
		}
	}
	return false
}
