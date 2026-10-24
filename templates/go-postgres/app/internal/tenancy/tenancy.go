// Package tenancy owns the application's own tenant and membership policy.
//
// An application tenant is a customer or organization inside this application.
// It is independent of any factory tenant: a role or tenant claim minted
// elsewhere grants nothing here. Access is produced only by resolving a
// verified identity against this application's membership records.
package tenancy

import (
	"context"
	"errors"
	"fmt"
	"regexp"
)

// TenantID is the application tenant's UUID.
type TenantID string

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ParseTenantID validates a tenant identifier.
func ParseTenantID(s string) (TenantID, error) {
	if !uuidRE.MatchString(s) {
		return "", fmt.Errorf("%w: tenant id must be a lowercase UUID", ErrInvalid)
	}
	return TenantID(s), nil
}

// Role is a membership role.
type Role string

// Roles. Humans hold viewer, reviewer or admin; worker is a service role.
const (
	RoleViewer   Role = "viewer"
	RoleReviewer Role = "reviewer"
	RoleAdmin    Role = "admin"
	RoleWorker   Role = "worker"
)

// Valid reports whether the role is defined.
func (r Role) Valid() bool {
	switch r {
	case RoleViewer, RoleReviewer, RoleAdmin, RoleWorker:
		return true
	}
	return false
}

// Permission is an application operation.
type Permission string

// Permissions.
const (
	ExceptionRead    Permission = "exception:read"
	ExceptionCreate  Permission = "exception:create"
	ExceptionResolve Permission = "exception:resolve"
	ExceptionProcess Permission = "exception:process"
)

var grants = map[Role][]Permission{
	RoleViewer:   {ExceptionRead},
	RoleReviewer: {ExceptionRead, ExceptionCreate, ExceptionResolve},
	RoleAdmin:    {ExceptionRead, ExceptionCreate, ExceptionResolve},
	RoleWorker:   {ExceptionRead, ExceptionProcess},
}

// Errors.
var (
	ErrInvalid     = errors.New("tenancy: invalid value")
	ErrNotMember   = errors.New("tenancy: no current membership in the requested tenant")
	ErrForbidden   = errors.New("tenancy: permission not granted")
	ErrUnavailable = errors.New("tenancy: membership store unavailable")
)

// Access is a verified subject's authority in exactly one tenant. Construct it
// from a membership via Resolver; transports never build it from a header.
type Access struct {
	tenant  TenantID
	subject string
	role    Role
}

// NewAccess builds Access from an already-verified membership. Resolver is the
// production path; tests and adapters with their own proof may call this.
func NewAccess(tenant TenantID, subject string, role Role) (Access, error) {
	if _, err := ParseTenantID(string(tenant)); err != nil {
		return Access{}, err
	}
	if n := len(subject); n < 1 || n > 256 {
		return Access{}, fmt.Errorf("%w: subject must be 1..256 bytes", ErrInvalid)
	}
	if !role.Valid() {
		return Access{}, fmt.Errorf("%w: unknown role", ErrInvalid)
	}
	return Access{tenant: tenant, subject: subject, role: role}, nil
}

// Tenant is the single tenant the access is bound to.
func (a Access) Tenant() TenantID { return a.tenant }

// Subject is the attributable actor.
func (a Access) Subject() string { return a.subject }

// Role is the membership role.
func (a Access) Role() Role { return a.role }

// IsZero reports an unusable (unauthenticated) access value.
func (a Access) IsZero() bool { return a.tenant == "" }

// Can reports whether the role grants the permission.
func (a Access) Can(p Permission) bool {
	if a.IsZero() {
		return false
	}
	for _, g := range grants[a.role] {
		if g == p {
			return true
		}
	}
	return false
}

// Require returns ErrForbidden unless the permission is granted.
func (a Access) Require(p Permission) error {
	if !a.Can(p) {
		return fmt.Errorf("%w: %s", ErrForbidden, p)
	}
	return nil
}

// Memberships is the consumer-owned port to the membership records.
type Memberships interface {
	// Role returns the subject's current role in the tenant. ok is false when
	// there is no membership. Errors are outages, never denials.
	Role(ctx context.Context, tenant TenantID, subject string) (role Role, ok bool, err error)
}

// Resolver turns a verified subject and a requested tenant into Access.
type Resolver struct{ memberships Memberships }

// NewResolver requires its membership port.
func NewResolver(m Memberships) (*Resolver, error) {
	if m == nil {
		return nil, errors.New("tenancy: a membership store is required")
	}
	return &Resolver{memberships: m}, nil
}

// Resolve authorizes a human subject for the requested tenant. The requested
// tenant is only a claim of intent: membership is checked on every call, so a
// removed member loses access with the next request.
func (r *Resolver) Resolve(ctx context.Context, subject, requested string) (Access, error) {
	return r.resolve(ctx, subject, requested, false)
}

// ResolveWorker authorizes a service identity, which must hold the worker role.
func (r *Resolver) ResolveWorker(ctx context.Context, serviceID, requested string) (Access, error) {
	return r.resolve(ctx, serviceID, requested, true)
}

func (r *Resolver) resolve(ctx context.Context, subject, requested string, worker bool) (Access, error) {
	tenant, err := ParseTenantID(requested)
	if err != nil {
		return Access{}, ErrNotMember // an unparseable selector is indistinguishable from no membership
	}
	role, ok, err := r.memberships.Role(ctx, tenant, subject)
	if err != nil {
		return Access{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if !ok || (role == RoleWorker) != worker {
		return Access{}, ErrNotMember
	}
	return NewAccess(tenant, subject, role)
}
