// Principal verification orchestration.
//
// OIDC is only an access-token verifier. A successful verification yields a
// structurally valid identity.Principal, never durable authority: this boundary
// immediately re-reads the current membership/service state and returns the
// current roles, enablement and version. A disabled or removed principal, a
// cross-scope token and a changed id/kind/execution identity all fail closed.
//
// The access token is never part of an error or a returned value. The verifier's
// private cause stays reachable through errors.Is/errors.As but is never
// rendered by the public fault message.
package identity

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// AuthenticateRequest is one bearer-token authentication input. Now is the
// authoritative time used for token expiry; a zero time is refused.
type AuthenticateRequest struct {
	AccessToken string        `json:"-"`
	Scope       tenancy.Scope `json:"scope"`
	Now         time.Time     `json:"-"`
}

// Validate checks the token, the authoritative time and the caller scope. A
// malformed request is refused before any verifier or reader call.
func (request AuthenticateRequest) Validate() error {
	if request.AccessToken == "" {
		return fault.New(fault.InvalidInput, "access token is empty")
	}
	if request.Now.IsZero() {
		return fault.New(fault.InvalidInput, "authentication time is zero")
	}
	if err := request.Scope.Validate(); err != nil {
		return err
	}
	return nil
}

// AuthenticatorConfig binds the verifier and the current-state reader.
type AuthenticatorConfig struct {
	Verifier   OIDCVerifier
	Principals PrincipalReader
}

// Authenticator verifies an access token and then re-reads current authority.
type Authenticator struct {
	verifier   OIDCVerifier
	principals PrincipalReader
}

// NewAuthenticator builds an authenticator with both dependencies present.
func NewAuthenticator(config AuthenticatorConfig) (*Authenticator, error) {
	if config.Verifier == nil {
		return nil, fault.New(fault.InvalidInput, "authenticator requires an OIDC verifier")
	}
	if config.Principals == nil {
		return nil, fault.New(fault.InvalidInput, "authenticator requires a principal reader")
	}
	return &Authenticator{verifier: config.Verifier, principals: config.Principals}, nil
}

// Authenticate verifies the presented access token and returns the current
// principal. The token's claims are only a lookup hint: current roles and
// enablement always come from the committed reader, and a failed current read
// is a refusal rather than a fallback to the token.
func (authenticator *Authenticator) Authenticate(ctx context.Context, request AuthenticateRequest) (Principal, error) {
	if err := request.Validate(); err != nil {
		return Principal{}, err
	}
	verified, err := authenticator.verifier.Verify(ctx, OIDCRequest{
		AccessToken: request.AccessToken,
		Now:         request.Now,
	})
	if err != nil {
		return Principal{}, fault.Wrap(fault.Unauthenticated, "access token verification failed", err)
	}
	if err := verified.Validate(); err != nil {
		return Principal{}, fault.Wrap(fault.Unauthenticated, "verified principal is invalid", err)
	}
	if verified.Scope != request.Scope {
		return Principal{}, fault.New(fault.Forbidden, "verified principal is outside the requested scope")
	}
	if !verified.Enabled {
		return Principal{}, fault.New(fault.Unauthenticated, "verified principal is disabled")
	}
	current, err := Current(ctx, authenticator.principals, PrincipalRequest{
		Scope:       verified.Scope,
		PrincipalID: verified.ID,
	})
	if err != nil {
		return Principal{}, err
	}
	if err := CheckCurrent(verified, current); err != nil {
		return Principal{}, err
	}
	return current, nil
}

// Current reads one principal's committed state and refuses an unavailable,
// structurally invalid, mismatched or disabled principal. The returned value is
// current authority evidence for one call, never durable permission.
func Current(ctx context.Context, reader PrincipalReader, request PrincipalRequest) (Principal, error) {
	if reader == nil {
		return Principal{}, fault.New(fault.Unauthenticated, "current principal reader is unavailable")
	}
	if err := request.Scope.Validate(); err != nil {
		return Principal{}, err
	}
	if _, err := id.ParsePrincipalID(string(request.PrincipalID)); err != nil {
		return Principal{}, fault.Wrap(fault.InvalidInput, "current principal id is invalid", err)
	}
	current, err := reader.ReadPrincipal(ctx, request)
	if err != nil {
		return Principal{}, fault.Wrap(fault.Unauthenticated, "current principal read failed", err)
	}
	if err := current.Validate(); err != nil {
		return Principal{}, fault.Wrap(fault.Unauthenticated, "current principal is invalid", err)
	}
	if current.Scope != request.Scope || current.ID != request.PrincipalID {
		return Principal{}, fault.New(fault.Unauthenticated, "current principal does not match the request")
	}
	if !current.Enabled {
		return Principal{}, fault.New(fault.Unauthenticated, "current principal is disabled")
	}
	return current, nil
}

// CheckCurrent proves that the current committed principal is the same
// identity the verified token described and is still enabled. It deliberately
// ignores roles: current roles are read from the current value, not compared to
// a stale token.
func CheckCurrent(verified, current Principal) error {
	if err := verified.Validate(); err != nil {
		return fault.Wrap(fault.Unauthenticated, "verified principal is invalid", err)
	}
	if err := current.Validate(); err != nil {
		return fault.Wrap(fault.Unauthenticated, "current principal is invalid", err)
	}
	if !current.Enabled {
		return fault.New(fault.Unauthenticated, "current principal is disabled")
	}
	if verified.ID != current.ID ||
		verified.Kind != current.Kind ||
		verified.Scope != current.Scope ||
		verified.ExecutionID != current.ExecutionID {
		return fault.New(fault.Forbidden, "current principal does not match the verified identity")
	}
	return nil
}
