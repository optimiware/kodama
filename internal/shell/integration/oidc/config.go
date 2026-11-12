// Package oidc implements the configured access-token verifier declared by the
// identity.OIDCVerifier seam.
//
// It verifies only access tokens. Issuer, audience, signing algorithm, JWKS
// location and claim names are explicit configuration; nothing is discovered
// from the presented token. A successful verification returns a structurally
// valid human principal whose id is derived from the issuer and subject. Token
// principal-id or role claims are never trusted, and current roles and
// enablement are re-read by the authorization boundary.
package oidc

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/identity"
)

const (
	// AlgorithmRS256 is the supported RSA SHA-256 JWS algorithm.
	AlgorithmRS256 = "RS256"
	// AlgorithmES256 is the supported ECDSA P-256 SHA-256 JWS algorithm.
	AlgorithmES256 = "ES256"

	defaultSubjectClaim = "sub"
)

// Config is the explicit OIDC access-token verifier configuration.
//
// The embedded identity.OIDCConfig carries the issuer, audience and clock skew.
// The remaining fields configure transport, signing algorithms, key caching and
// the claim names used to derive the verified ownership scope.
type Config struct {
	identity.OIDCConfig
	JWKSURL            string
	Algorithms         []string
	HTTPClient         *http.Client
	MaxJWKSBytes       int64
	MaxTokenBytes      int
	CacheTTL           time.Duration
	MinRefreshInterval time.Duration
	Clock              func() time.Time
	SubjectClaim       string
	TenantClaim        string
	ProjectClaim       string
	Logf               func(format string, args ...any)
}

// Validate checks the explicit verifier configuration.
//
// A plain-HTTP issuer or JWKS URL, an empty audience, a negative skew or cache
// TTL, an empty algorithm list, the "none" or any HS* algorithm, a
// non-positive body bound, a nil HTTP client and an empty tenant claim are all
// refused as invalid input.
func (config Config) Validate() error {
	if err := config.OIDCConfig.Validate(); err != nil {
		return err
	}
	if err := validateHTTPSURL("OIDC JWKS URL", config.JWKSURL); err != nil {
		return err
	}
	if len(config.Algorithms) == 0 {
		return fault.New(fault.InvalidInput, "OIDC algorithms are empty")
	}
	seen := make(map[string]bool, len(config.Algorithms))
	for _, algorithm := range config.Algorithms {
		if !supportedAlgorithm(algorithm) {
			return fault.New(fault.InvalidInput, fmt.Sprintf("unsupported OIDC algorithm %q", algorithm))
		}
		if seen[algorithm] {
			return fault.New(fault.InvalidInput, fmt.Sprintf("OIDC algorithm %q is repeated", algorithm))
		}
		seen[algorithm] = true
	}
	if config.MaxJWKSBytes <= 0 {
		return fault.New(fault.InvalidInput, "OIDC JWKS body bound is not positive")
	}
	if config.MaxTokenBytes <= 0 {
		return fault.New(fault.InvalidInput, "OIDC token body bound is not positive")
	}
	if config.CacheTTL < 0 {
		return fault.New(fault.InvalidInput, "OIDC key cache TTL is negative")
	}
	if config.MinRefreshInterval < 0 {
		return fault.New(fault.InvalidInput, "OIDC minimum key refresh interval is negative")
	}
	if config.HTTPClient == nil {
		return fault.New(fault.InvalidInput, "OIDC HTTP client is nil")
	}
	if !validClaimName(config.TenantClaim) {
		return fault.New(fault.InvalidInput, "OIDC tenant claim name is empty or contains whitespace")
	}
	if config.SubjectClaim != "" && !validClaimName(config.SubjectClaim) {
		return fault.New(fault.InvalidInput, "OIDC subject claim name contains whitespace")
	}
	if config.ProjectClaim != "" && !validClaimName(config.ProjectClaim) {
		return fault.New(fault.InvalidInput, "OIDC project claim name contains whitespace")
	}
	return nil
}

func supportedAlgorithm(algorithm string) bool {
	return algorithm == AlgorithmRS256 || algorithm == AlgorithmES256
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

func validClaimName(value string) bool {
	return value != "" && !strings.ContainsAny(value, " \t\r\n")
}
