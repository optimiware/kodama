package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"unicode/utf8"

	"github.com/optimiware/kodama/internal/domain"
)

// Principal is a verified caller identity. The value is whatever stable,
// opaque identifier the injected Authenticator verified; this package never
// parses it and treats an empty or oversized value as an unauthenticated
// request. In a hosted installation the platform integration defines how it
// maps to platform identity; nothing here grants access from it.
type Principal struct {
	ID string
}

// MaxPrincipalBytes bounds Principal.ID (it is stored in the idempotency key).
const MaxPrincipalBytes = 512

// Validate rejects empty, oversized, non-UTF-8 and control-character IDs.
func (p Principal) Validate() error {
	if p.ID == "" || len(p.ID) > MaxPrincipalBytes || !utf8.ValidString(p.ID) {
		return fmt.Errorf("%w: principal id must be 1..%d bytes of UTF-8", domain.ErrInvalidValue, MaxPrincipalBytes)
	}
	for _, r := range p.ID {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: principal id contains a control character", domain.ErrInvalidValue)
		}
	}
	return nil
}

// Action names an authorization decision such as "work.create". The value is
// registered with each route and passed to the Authorizer; this package defines
// no role policy.
type Action string

// Reference names a resource inside a scope: its kind and opaque ID.
type Reference struct {
	Kind string
	ID   string
}

// Target is everything the Authorizer is told about the access. TenantID is
// empty only for principal-level routes such as GET /me; ProjectID is empty for
// tenant-level routes. Resource is empty for collection routes.
type Target struct {
	TenantID  domain.TenantID
	ProjectID domain.ProjectID
	Resource  Reference
}

// Sentinels returned by the injected ports. Any other error is a dependency
// failure and results in 503, never in access.
var (
	// ErrUnauthenticated: the credential is missing, invalid or expired.
	ErrUnauthenticated = errors.New("httpapi: unauthenticated")
	// ErrDenied: authenticated but not permitted; rendered as 403.
	ErrDenied = errors.New("httpapi: access denied")
	// ErrHidden: not permitted and the caller must not learn that the
	// reference exists; rendered as 404 identical to a missing resource.
	ErrHidden = errors.New("httpapi: reference hidden")
)

// Authenticator verifies the bearer credential of a request. It returns
// ErrUnauthenticated for a bad credential and any other error for an outage.
type Authenticator interface {
	Authenticate(ctx context.Context, r *http.Request) (Principal, error)
}

// Authorizer decides whether the principal may perform the action on the
// target, using current state on every call (a cursor or an idempotency replay
// is never a permission). It returns nil to allow, ErrDenied or ErrHidden to
// refuse, and any other error for an outage, which is treated as a refusal.
type Authorizer interface {
	Authorize(ctx context.Context, p Principal, action Action, t Target) error
}

// authenticationError maps an Authenticator failure to its response.
func authenticationError(err error) *APIError {
	if errors.Is(err, ErrUnauthenticated) {
		return UnauthorizedError()
	}
	return UnavailableError(0, err)
}

// authorizationError maps an Authorizer failure to its response.
func authorizationError(err error) *APIError {
	switch {
	case errors.Is(err, ErrDenied):
		return ForbiddenError()
	case errors.Is(err, ErrHidden):
		return NotFoundError()
	default:
		return UnavailableError(0, err)
	}
}
