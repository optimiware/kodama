// Package adapters bridges the log module's ports to its peers: current
// identity authority, execution attempts and Kubernetes placement, and the
// artifact module's repository and object store. Each adapter is explicit
// wiring for the composition root; none has a permissive default.
package adapters

import (
	"context"
	"errors"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/logs"
)

// DiagnosticPermissions are the existing permissions that together authorize
// reading an attempt's logs. No role or permission is added: a viewer holds
// both, so every role that may see runs and evidence may see their logs, and a
// principal without a binding sees nothing. The mapping is isolated here so
// the integration owner can confirm or tighten it in one place.
var DiagnosticPermissions = []identity.Permission{identity.PermRunRead, identity.PermEvidenceRead}

// Authority implements logs.Authority over the selected identity authority and
// the current-binding reader: a principal is a user ID and is hidden from
// tenants where it holds no binding. Both permissions are checked on every
// call; there is no cache and no fallback to another authority.
type Authority struct {
	authority identity.Authority
	reader    identity.AccessReader
}

var _ logs.Authority = (*Authority)(nil)

// NewAuthority requires both collaborators; there is no default grant.
func NewAuthority(a identity.Authority, r identity.AccessReader) (*Authority, error) {
	if a == nil || r == nil {
		return nil, errors.New("logs/adapters: an authority and an access reader are required")
	}
	return &Authority{authority: a, reader: r}, nil
}

func translate(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, identity.ErrHidden):
		return logs.ErrNotFound
	case errors.Is(err, identity.ErrDenied):
		return logs.ErrDenied
	}
	return logs.Unavailablef(err, "the identity authority is unavailable")
}

// AuthorizeRead implements logs.Authority.
func (a *Authority) AuthorizeRead(ctx context.Context, principal string, scope domain.Scope) error {
	user := identity.UserID(principal)
	if err := user.Validate(); err != nil || scope.Validate() != nil {
		return logs.ErrNotFound
	}
	bindings, err := a.reader.CurrentBindings(ctx, scope.TenantID, user)
	if err != nil {
		return translate(err)
	}
	target := identity.Target{TenantID: scope.TenantID, ProjectID: scope.ProjectID}
	bound := false
	for _, b := range bindings {
		if b.Applies(target) {
			bound = true
			break
		}
	}
	if !bound {
		return logs.ErrNotFound
	}
	actor := identity.Actor{ID: principal, Type: identity.ActorUser}
	for _, perm := range DiagnosticPermissions {
		if err := translate(a.authority.Authorize(ctx, actor, target, perm)); err != nil {
			return err
		}
	}
	return nil
}
