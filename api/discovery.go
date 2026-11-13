package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// DiscoveryPath is the one unauthenticated public discovery route. It is a
// fixed root path outside the 58-operation registry: it is not returned by
// Routes, not resolvable by Match or LookupRoute, and carries no credential.
const DiscoveryPath = "/.well-known/kodama-configuration"

// OAuthProtectedResourcePath is the RFC 9728 OAuth 2.0 Protected Resource
// Metadata route served alongside DiscoveryPath when public discovery is
// configured.
const OAuthProtectedResourcePath = "/.well-known/oauth-protected-resource"

// OAuthProtectedResourceMetadata is the RFC 9728 protected-resource metadata
// document MCP clients discover on an unauthenticated /mcp response.
type OAuthProtectedResourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
}

// MaxDiscoveryBytes bounds one discovery document body. A body of exactly the
// bound is accepted; one byte more is refused.
const MaxDiscoveryBytes = 16 << 10

// DiscoveryDocument is the public, unauthenticated OAuth client configuration a
// kodactl login reads from the API origin. It carries no secret and no JWKS
// material: the issuer JWKS is fetched separately from the issuer origin.
type DiscoveryDocument struct {
	Issuer                      string   `json:"issuer"`
	ClientID                    string   `json:"client_id"`
	Audience                    string   `json:"audience"`
	AuthorizationEndpoint       string   `json:"authorization_endpoint"`
	TokenEndpoint               string   `json:"token_endpoint"`
	DeviceAuthorizationEndpoint string   `json:"device_authorization_endpoint"`
	RevocationEndpoint          string   `json:"revocation_endpoint"`
	Scopes                      []string `json:"scopes"`
	APIURL                      string   `json:"api_url"`
}

// DecodeDiscoveryDocument strictly decodes and validates one bounded discovery
// document. Unknown members, trailing data, a non-object body, an empty body,
// wrong JSON types and an oversize body are refused, as are missing required
// members and invalid endpoint origins.
func DecodeDiscoveryDocument(data []byte) (DiscoveryDocument, error) {
	if len(data) == 0 {
		return DiscoveryDocument{}, fmt.Errorf("discovery document is empty")
	}
	if len(data) > MaxDiscoveryBytes {
		return DiscoveryDocument{}, fmt.Errorf("discovery document is %d bytes; at most %d are allowed", len(data), MaxDiscoveryBytes)
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return DiscoveryDocument{}, fmt.Errorf("discovery document must be a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	var document DiscoveryDocument
	if err := decoder.Decode(&document); err != nil {
		return DiscoveryDocument{}, fmt.Errorf("invalid discovery document: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return DiscoveryDocument{}, fmt.Errorf("invalid discovery document: trailing data")
	}
	if err := document.Validate(); err != nil {
		return DiscoveryDocument{}, err
	}
	return document, nil
}

// Validate checks the required members and the transport-origin rules: the
// issuer and every authorization endpoint must be https, except a development
// fixture on the literal numeric loopback hosts 127.0.0.1 or ::1; every
// authorization endpoint must share the issuer's origin. The API URL is a
// client configuration target, not an authorization endpoint, so it may live on
// another https origin.
func (document DiscoveryDocument) Validate() error {
	if document.Issuer == "" {
		return fmt.Errorf("discovery issuer is empty")
	}
	if document.ClientID == "" {
		return fmt.Errorf("discovery client_id is empty")
	}
	if document.Audience == "" {
		return fmt.Errorf("discovery audience is empty")
	}
	if len(document.Scopes) == 0 {
		return fmt.Errorf("discovery scopes are empty")
	}
	for _, scope := range document.Scopes {
		if scope == "" {
			return fmt.Errorf("discovery scope is empty")
		}
	}
	issuerScheme, issuerAuthority, err := splitDiscoveryURL(document.Issuer)
	if err != nil {
		return fmt.Errorf("discovery issuer: %w", err)
	}
	endpoints := map[string]string{
		"authorization_endpoint":        document.AuthorizationEndpoint,
		"token_endpoint":                document.TokenEndpoint,
		"device_authorization_endpoint": document.DeviceAuthorizationEndpoint,
		"revocation_endpoint":           document.RevocationEndpoint,
	}
	for name, raw := range endpoints {
		if raw == "" {
			return fmt.Errorf("discovery %s is empty", name)
		}
		scheme, authority, err := splitDiscoveryURL(raw)
		if err != nil {
			return fmt.Errorf("discovery %s: %w", name, err)
		}
		if !sameOrigin(issuerScheme, issuerAuthority, scheme, authority) {
			return fmt.Errorf("discovery %s does not share the issuer origin", name)
		}
	}
	if document.APIURL == "" {
		return fmt.Errorf("discovery api_url is empty")
	}
	if _, _, err := splitDiscoveryURL(document.APIURL); err != nil {
		return fmt.Errorf("discovery api_url: %w", err)
	}
	return nil
}

// splitDiscoveryURL validates one absolute URL and returns its lowercase scheme
// and raw authority. Only https is allowed, except a plain-HTTP URL bound to
// the literal numeric loopback host. User information, an opaque URL, a query,
// a fragment, whitespace and a malformed authority are refused.
func splitDiscoveryURL(raw string) (string, string, error) {
	if strings.ContainsAny(raw, " \t\r\n\\") {
		return "", "", fmt.Errorf("must not contain whitespace or a backslash")
	}
	index := strings.Index(raw, "://")
	if index <= 0 {
		return "", "", fmt.Errorf("must be absolute")
	}
	scheme := strings.ToLower(raw[:index])
	if scheme != "https" && scheme != "http" {
		return "", "", fmt.Errorf("scheme %q is not supported", scheme)
	}
	remainder := raw[index+3:]
	authority := remainder
	if end := strings.IndexByte(remainder, '/'); end >= 0 {
		authority = remainder[:end]
	}
	if strings.Contains(remainder, "#") {
		return "", "", fmt.Errorf("must not carry a fragment")
	}
	if strings.Contains(remainder, "?") {
		return "", "", fmt.Errorf("must not carry a query")
	}
	if authority == "" {
		return "", "", fmt.Errorf("must be absolute")
	}
	if strings.Contains(authority, "@") {
		return "", "", fmt.Errorf("must not carry user information")
	}
	host, ok := discoveryHostname(authority)
	if !ok {
		return "", "", fmt.Errorf("has an invalid host")
	}
	if scheme == "http" && !numericLoopbackHost(host) {
		return "", "", fmt.Errorf("must use https outside numeric loopback")
	}
	return scheme, authority, nil
}

// discoveryHostname extracts a non-empty host from one authority, validating a
// present port. The bracketed IPv6 form is supported.
func discoveryHostname(authority string) (string, bool) {
	if strings.HasPrefix(authority, "[") {
		end := strings.IndexByte(authority, ']')
		if end < 0 {
			return "", false
		}
		host := authority[1:end]
		rest := authority[end+1:]
		if rest != "" {
			if !strings.HasPrefix(rest, ":") {
				return "", false
			}
			if _, ok := discoveryPort(rest[1:]); !ok {
				return "", false
			}
		}
		return host, host != ""
	}
	host := authority
	if colon := strings.LastIndexByte(host, ':'); colon >= 0 {
		if _, ok := discoveryPort(host[colon+1:]); !ok {
			return "", false
		}
		host = host[:colon]
	}
	return host, host != ""
}

// discoveryPort validates one explicit port.
func discoveryPort(raw string) (int, bool) {
	port, err := strconv.Atoi(raw)
	if err != nil || port <= 0 || port > 65535 {
		return 0, false
	}
	return port, true
}

// numericLoopbackHost reports whether host is exactly one of the literal
// numeric loopback hosts. The name localhost is deliberately not accepted.
func numericLoopbackHost(host string) bool {
	switch host {
	case "127.0.0.1", "::1":
		return true
	}
	return false
}

// sameOrigin reports whether both URLs share scheme, host and port.
func sameOrigin(leftScheme, leftAuthority, rightScheme, rightAuthority string) bool {
	return strings.EqualFold(leftScheme, rightScheme) && strings.EqualFold(leftAuthority, rightAuthority)
}

// SameOrigin reports whether endpoint obeys the discovery transport-origin rule
// relative to issuer: both must be absolute URLs with the same scheme, host and
// port, and the only plain-HTTP form is a literal numeric loopback fixture.
// Consumers use it to pin a recorded OAuth endpoint to the issuer that minted
// the session instead of trusting a stored endpoint blindly.
func SameOrigin(issuer, endpoint string) bool {
	issuerScheme, issuerAuthority, err := splitDiscoveryURL(issuer)
	if err != nil {
		return false
	}
	endpointScheme, endpointAuthority, err := splitDiscoveryURL(endpoint)
	if err != nil {
		return false
	}
	return sameOrigin(issuerScheme, issuerAuthority, endpointScheme, endpointAuthority)
}
