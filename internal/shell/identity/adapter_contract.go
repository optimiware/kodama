// Package identity declares the access-token verifier and OAuth client seams.
//
// OIDC is only an access-token verifier: the caller supplies the presented
// bearer token and an authoritative time, and a successful verification yields
// the existing identity.Principal value. Issuer and audience are explicit
// configuration, never inferred from the token or the request. A verified token
// is not durable authority; enablement, roles and execution state are re-read at
// the commit boundary.
//
// The Platform integration is two OAuth applications behind Auth0 SSO. Each
// application's client configuration and claim match are explicit installation
// configuration; the exact Auth0 tenant URLs, client ids and callback paths are
// not hardcoded here. RBAC belongs to each application and is not shared.
//
// No discovery request, JWKS fetch, HTTP client or provider SDK exists here.
package identity

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/fault"
)

// OAuthProtocol names the OAuth family used by one application client.
type OAuthProtocol string

const (
	// OAuthProtocolOAuth2 is an OAuth 2.0 client.
	OAuthProtocolOAuth2 OAuthProtocol = "oauth2"
	// OAuthProtocolOIDC is an OpenID Connect client.
	OAuthProtocolOIDC OAuthProtocol = "oidc"
)

var oauthProtocols = [...]OAuthProtocol{
	OAuthProtocolOAuth2,
	OAuthProtocolOIDC,
}

// OAuthProtocols returns every declared OAuth protocol in declaration order.
func OAuthProtocols() []OAuthProtocol {
	return append([]OAuthProtocol(nil), oauthProtocols[:]...)
}

// Valid reports whether the protocol is one of the declared OAuth families.
func (protocol OAuthProtocol) Valid() bool {
	for _, candidate := range oauthProtocols {
		if protocol == candidate {
			return true
		}
	}
	return false
}

// OAuthClientConfig is one application's explicit OAuth client configuration.
//
// ClientSecretRef names a stored secret resolved through the secret seam; it is
// never the secret value. RedirectURL is optional ("as needed" by the chosen
// flow); when present it must be an explicit https URL. The Auth0 tenant
// issuer, client id and callback are installation configuration.
type OAuthClientConfig struct {
	Protocol        OAuthProtocol
	Issuer          string
	ClientID        string
	ClientSecretRef string
	Audience        string
	Scopes          []string
	RedirectURL     string
}

// Validate checks the protocol, explicit HTTPS issuer, non-empty client
// identity and secret reference, audience, unique non-empty scopes and an
// optional HTTPS redirect URL. Secret material is never accepted here.
func (config OAuthClientConfig) Validate() error {
	if !config.Protocol.Valid() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("unknown OAuth protocol %q", config.Protocol))
	}
	if err := validateHTTPSURL("OAuth issuer", config.Issuer); err != nil {
		return err
	}
	if !validOAuthToken(config.ClientID) {
		return fault.New(fault.InvalidInput, "OAuth client id is empty or contains whitespace")
	}
	if !validOAuthToken(config.ClientSecretRef) {
		return fault.New(fault.InvalidInput, "OAuth client secret reference is empty or contains whitespace")
	}
	if !validOAuthToken(config.Audience) {
		return fault.New(fault.InvalidInput, "OAuth audience is empty or contains whitespace")
	}
	if len(config.Scopes) == 0 {
		return fault.New(fault.InvalidInput, "OAuth scopes are empty")
	}
	seen := make(map[string]bool, len(config.Scopes))
	for _, scope := range config.Scopes {
		if !validOAuthToken(scope) {
			return fault.New(fault.InvalidInput, fmt.Sprintf("invalid OAuth scope %q", scope))
		}
		if seen[scope] {
			return fault.New(fault.InvalidInput, fmt.Sprintf("OAuth scope %q is repeated", scope))
		}
		seen[scope] = true
	}
	if config.RedirectURL != "" {
		if err := validateHTTPSURL("OAuth redirect URL", config.RedirectURL); err != nil {
			return err
		}
	}
	return nil
}

// OAuthClaims is the declared claim match for one OAuth application. A token
// must carry the tenant claim with the configured tenant value and the role
// claim with the configured role string. Subject is optional audit context and
// is never the match by itself.
type OAuthClaims struct {
	TenantClaim string
	Tenant      string
	RoleClaim   string
	Role        string
	Subject     string
}

// Validate checks the tenant and role claim names/values and optional subject.
func (claims OAuthClaims) Validate() error {
	if !validOAuthToken(claims.TenantClaim) {
		return fault.New(fault.InvalidInput, "OAuth tenant claim name is empty or contains whitespace")
	}
	if !validOAuthToken(claims.Tenant) {
		return fault.New(fault.InvalidInput, "OAuth tenant claim value is empty or contains whitespace")
	}
	if !validOAuthToken(claims.RoleClaim) {
		return fault.New(fault.InvalidInput, "OAuth role claim name is empty or contains whitespace")
	}
	if !validOAuthToken(claims.Role) {
		return fault.New(fault.InvalidInput, "OAuth role is empty or contains whitespace")
	}
	if claims.Subject != "" && !validOAuthToken(claims.Subject) {
		return fault.New(fault.InvalidInput, "OAuth subject contains whitespace")
	}
	return nil
}

// Matches reports whether the presented claim values carry both the configured
// tenant value under TenantClaim and the configured role string under
// RoleClaim. A nil or missing claim never matches.
func (claims OAuthClaims) Matches(values map[string][]string) bool {
	if err := claims.Validate(); err != nil {
		return false
	}
	return containsClaim(values[claims.TenantClaim], claims.Tenant) &&
		containsClaim(values[claims.RoleClaim], claims.Role)
}

func validateHTTPSURL(label, value string) error {
	const scheme = "https://"
	if !strings.HasPrefix(value, scheme) || len(value) == len(scheme) {
		return fault.New(fault.InvalidInput, label+" must be an explicit https URL")
	}
	if strings.ContainsAny(value, " \t\r\n") {
		return fault.New(fault.InvalidInput, label+" contains whitespace")
	}
	return nil
}

func validOAuthToken(value string) bool {
	return value != "" && !strings.ContainsAny(value, " \t\r\n")
}

func containsClaim(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// OIDCConfig is the explicit access-token verifier configuration.
type OIDCConfig struct {
	Issuer    string
	Audience  string
	ClockSkew time.Duration
}

// Validate checks an explicit HTTPS issuer, a non-empty audience and a
// non-negative clock skew. Plain HTTP issuers are refused.
func (config OIDCConfig) Validate() error {
	const scheme = "https://"
	if !strings.HasPrefix(config.Issuer, scheme) || len(config.Issuer) == len(scheme) {
		return fault.New(fault.InvalidInput, "OIDC issuer must be an explicit https URL")
	}
	if strings.ContainsAny(config.Issuer, " \t\r\n") {
		return fault.New(fault.InvalidInput, "OIDC issuer contains whitespace")
	}
	if config.Audience == "" {
		return fault.New(fault.InvalidInput, "OIDC audience is empty")
	}
	if config.ClockSkew < 0 {
		return fault.New(fault.InvalidInput, "OIDC clock skew is negative")
	}
	return nil
}

// OIDCRequest is one access-token verification input. Now is the authoritative
// time used for expiry/not-before checks; a zero time is refused.
type OIDCRequest struct {
	AccessToken string
	Now         time.Time
}

// Validate checks that a token and an authoritative time are present. The token
// value must never be logged or persisted.
func (request OIDCRequest) Validate() error {
	if request.AccessToken == "" {
		return fault.New(fault.InvalidInput, "OIDC access token is empty")
	}
	if request.Now.IsZero() {
		return fault.New(fault.InvalidInput, "OIDC request time is zero")
	}
	return nil
}

// OIDCVerifier verifies one presented access token and returns the verified
// principal. A refusal never reveals token contents.
type OIDCVerifier interface {
	Verify(context.Context, OIDCRequest) (Principal, error)
}
