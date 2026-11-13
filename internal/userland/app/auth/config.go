// Package auth implements Kodama's built-in Userland OIDC Provider and Social
// Login Broker (`kodama auth serve`).
//
// It uses only the Go standard library and public/Userland contracts, minting
// standard ES256 JWTs that verify cleanly with both `kodactl login` and
// Kodama's S05 OIDC access-token verifier.
package auth

import (
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/principal"
)

const (
	// DefaultListenAddress is the default loopback listener address for `kodama auth serve`.
	DefaultListenAddress = "127.0.0.1:8082"
	// DefaultClientID is the default public OAuth client ID (`kodactl`).
	DefaultClientID = "kodactl"
	// DefaultAudience is the default API audience bound into minted access tokens.
	DefaultAudience = "kodama"
	// DefaultTenantID is the default tenant claim value when none is specified.
	DefaultTenantID = "default"
	// DefaultTenantClaim is the default JWT claim carrying the tenant identity.
	DefaultTenantClaim = "tenant"
	// DefaultProjectClaim is the default JWT claim carrying an optional project identity.
	DefaultProjectClaim = "project"

	defaultAccessTokenTTL     = time.Hour
	defaultRefreshTokenTTL    = 24 * time.Hour
	defaultAuthCodeTTL        = 2 * time.Minute
	defaultDeviceCodeTTL      = 10 * time.Minute
	defaultDevicePollInterval = time.Second
	defaultMaxBodyBytes       = 64 << 10
	defaultOutboundMaxBytes   = 1 << 20
	defaultOutboundTimeout    = 15 * time.Second
	defaultReadHeaderTimeout  = 10 * time.Second
	defaultReadTimeout        = 30 * time.Second
	defaultWriteTimeout       = 30 * time.Second
	defaultIdleTimeout        = 120 * time.Second
	shutdownGracePeriod       = 10 * time.Second

	defaultGitHubWebURL = "https://github.com"
	defaultGitHubAPIURL = "https://api.github.com"
)

// Config configures one OIDC Provider and Social Login Broker instance.
type Config struct {
	// Issuer is the external OIDC issuer URL (for example "https://auth.example.com"
	// or "http://127.0.0.1:8082" for local loopback development).
	Issuer string
	// ListenAddress is the TCP address Run listens on when Listener is nil.
	ListenAddress string
	// StateDir is the optional directory where the ES256 signing keypair, cookie
	// secret and 1-click GitHub credentials are persisted with 0600 permissions.
	// When empty, ephemeral in-memory keys are generated.
	StateDir string

	// TenantID is the Kodama tenant ID stamped into issued JWTs. Defaults to "default".
	TenantID id.TenantID
	// TenantClaim is the JWT claim name carrying TenantID. Defaults to "tenant".
	TenantClaim string
	// ProjectID is an optional Kodama project ID stamped into issued JWTs.
	ProjectID id.ProjectID
	// ProjectClaim is the JWT claim name carrying ProjectID when ProjectID is set.
	ProjectClaim string

	// ClientID is the primary public OAuth 2.1 client ID (defaults to "kodactl").
	ClientID string
	// AdditionalClientIDs are optional extra pre-registered public client IDs.
	AdditionalClientIDs []string
	// Audience is the API audience stamped into access_token JWTs (defaults to "kodama").
	Audience string
	// RedirectURIs are explicitly allowed redirect URIs in addition to RFC 8252
	// §7.3 dynamic loopback callbacks (http://127.0.0.1:<port>/callback and
	// http://[::1]:<port>/callback).
	RedirectURIs []string

	// LocalPersonas enables offline/day-1 Local Sign-In for the named personas
	// (for example []string{"admin", "engineer", "reviewer"}).
	LocalPersonas []string
	// AccessCode optionally gates Local Sign-In and 1-Click GitHub Setup behind a
	// shared secret compared in constant time.
	AccessCode string

	// GitHub configures "Continue with GitHub" and optional 1-Click App Manifest setup.
	GitHub GitHubConfig
	// UpstreamOIDC configures "Continue with SSO / Google" OIDC federation.
	UpstreamOIDC UpstreamOIDCConfig

	// TLSCertFile and TLSKeyFile optionally enable direct TLS serving in Run.
	TLSCertFile string
	TLSKeyFile  string

	// Token and flow lifetimes. Zero values select safe defaults.
	AccessTokenTTL     time.Duration
	RefreshTokenTTL    time.Duration
	AuthCodeTTL        time.Duration
	DeviceCodeTTL      time.Duration
	DevicePollInterval time.Duration
	MaxBodyBytes       int64

	// Injected runtime dependencies for tests and custom transports.
	HTTPClient *http.Client
	Clock      func() time.Time
	Random     io.Reader
	Listener   net.Listener
}

// GitHubConfig configures GitHub social login and optional 1-click App Manifest setup.
type GitHubConfig struct {
	ClientID     string
	ClientSecret string
	AllowSetup   bool
	WebURL       string
	APIURL       string
}

// UpstreamOIDCConfig configures upstream OIDC federation (Google, Okta, Dex, etc.).
type UpstreamOIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	Scopes       []string
	DisplayName  string
}

// UserIdentity is one authenticated human identity ready for token issuance.
type UserIdentity struct {
	Subject string
	Email   string
	Name    string
	Method  string
}

func (config Config) normalize() (Config, error) {
	config.Issuer = strings.TrimSpace(config.Issuer)
	if err := validateEndpointURL("issuer", config.Issuer, false); err != nil {
		return Config{}, err
	}
	if strings.TrimSpace(config.ListenAddress) == "" {
		config.ListenAddress = DefaultListenAddress
	} else {
		config.ListenAddress = strings.TrimSpace(config.ListenAddress)
	}
	config.StateDir = strings.TrimSpace(config.StateDir)

	if strings.TrimSpace(string(config.TenantID)) == "" {
		config.TenantID = DefaultTenantID
	}
	parsedTenant, err := id.ParseTenantID(strings.TrimSpace(string(config.TenantID)))
	if err != nil {
		return Config{}, fault.Wrap(fault.InvalidInput, "tenant id is invalid", err)
	}
	config.TenantID = parsedTenant

	config.TenantClaim = strings.TrimSpace(config.TenantClaim)
	if config.TenantClaim == "" {
		config.TenantClaim = DefaultTenantClaim
	}
	if !isValidClaimName(config.TenantClaim) || isReservedJWTClaim(config.TenantClaim) {
		return Config{}, fault.New(fault.InvalidInput, "tenant claim name is invalid or reserved")
	}

	if strings.TrimSpace(string(config.ProjectID)) != "" {
		parsedProject, err := id.ParseProjectID(strings.TrimSpace(string(config.ProjectID)))
		if err != nil {
			return Config{}, fault.Wrap(fault.InvalidInput, "project id is invalid", err)
		}
		config.ProjectID = parsedProject
		config.ProjectClaim = strings.TrimSpace(config.ProjectClaim)
		if config.ProjectClaim == "" {
			config.ProjectClaim = DefaultProjectClaim
		}
		if !isValidClaimName(config.ProjectClaim) || isReservedJWTClaim(config.ProjectClaim) || config.ProjectClaim == config.TenantClaim {
			return Config{}, fault.New(fault.InvalidInput, "project claim name is invalid or collides with tenant claim")
		}
	}

	config.ClientID = strings.TrimSpace(config.ClientID)
	if config.ClientID == "" {
		config.ClientID = DefaultClientID
	}
	if strings.ContainsAny(config.ClientID, " \t\r\n") {
		return Config{}, fault.New(fault.InvalidInput, "client id must not contain whitespace")
	}
	extraClients := make([]string, 0, len(config.AdditionalClientIDs))
	for _, cid := range config.AdditionalClientIDs {
		cid = strings.TrimSpace(cid)
		if cid == "" {
			continue
		}
		if strings.ContainsAny(cid, " \t\r\n") {
			return Config{}, fault.New(fault.InvalidInput, "additional client id must not contain whitespace")
		}
		if cid != config.ClientID && !slices.Contains(extraClients, cid) {
			extraClients = append(extraClients, cid)
		}
	}
	config.AdditionalClientIDs = extraClients

	config.Audience = strings.TrimSpace(config.Audience)
	if config.Audience == "" {
		config.Audience = DefaultAudience
	}
	if strings.ContainsAny(config.Audience, " \t\r\n") {
		return Config{}, fault.New(fault.InvalidInput, "audience must not contain whitespace")
	}

	redirects := make([]string, 0, len(config.RedirectURIs))
	for _, raw := range config.RedirectURIs {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		if err := validateRedirectURI(trimmed); err != nil {
			return Config{}, err
		}
		if !slices.Contains(redirects, trimmed) {
			redirects = append(redirects, trimmed)
		}
	}
	config.RedirectURIs = redirects

	personas := make([]string, 0, len(config.LocalPersonas))
	for _, raw := range config.LocalPersonas {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		subject, err := principal.SubjectForLocal(raw)
		if err != nil {
			return Config{}, err
		}
		if !slices.Contains(personas, subject) {
			personas = append(personas, subject)
		}
	}
	config.LocalPersonas = personas

	config.GitHub.ClientID = strings.TrimSpace(config.GitHub.ClientID)
	config.GitHub.ClientSecret = strings.TrimSpace(config.GitHub.ClientSecret)
	if (config.GitHub.ClientID == "") != (config.GitHub.ClientSecret == "") {
		return Config{}, fault.New(fault.InvalidInput, "GitHub login requires both client id and client secret")
	}
	config.GitHub.WebURL = strings.TrimRight(strings.TrimSpace(config.GitHub.WebURL), "/")
	if config.GitHub.WebURL == "" {
		config.GitHub.WebURL = defaultGitHubWebURL
	}
	if err := validateEndpointURL("GitHub web URL", config.GitHub.WebURL, false); err != nil {
		return Config{}, err
	}
	config.GitHub.APIURL = strings.TrimRight(strings.TrimSpace(config.GitHub.APIURL), "/")
	if config.GitHub.APIURL == "" {
		config.GitHub.APIURL = defaultGitHubAPIURL
	}
	if err := validateEndpointURL("GitHub API URL", config.GitHub.APIURL, false); err != nil {
		return Config{}, err
	}

	config.UpstreamOIDC.Issuer = strings.TrimSpace(config.UpstreamOIDC.Issuer)
	config.UpstreamOIDC.ClientID = strings.TrimSpace(config.UpstreamOIDC.ClientID)
	config.UpstreamOIDC.ClientSecret = strings.TrimSpace(config.UpstreamOIDC.ClientSecret)
	upstreamAny := config.UpstreamOIDC.Issuer != "" || config.UpstreamOIDC.ClientID != "" || config.UpstreamOIDC.ClientSecret != ""
	upstreamAll := config.UpstreamOIDC.Issuer != "" && config.UpstreamOIDC.ClientID != "" && config.UpstreamOIDC.ClientSecret != ""
	if upstreamAny && !upstreamAll {
		return Config{}, fault.New(fault.InvalidInput, "upstream OIDC requires issuer, client id and client secret")
	}
	if upstreamAll {
		if err := validateEndpointURL("upstream OIDC issuer", config.UpstreamOIDC.Issuer, false); err != nil {
			return Config{}, err
		}
		scopes := make([]string, 0, len(config.UpstreamOIDC.Scopes)+3)
		for _, s := range config.UpstreamOIDC.Scopes {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			if strings.ContainsAny(s, " \t\r\n") {
				return Config{}, fault.New(fault.InvalidInput, "upstream OIDC scope must not contain whitespace")
			}
			if !slices.Contains(scopes, s) {
				scopes = append(scopes, s)
			}
		}
		if len(scopes) == 0 {
			scopes = []string{"openid", "email", "profile"}
		} else if !slices.Contains(scopes, "openid") {
			scopes = append([]string{"openid"}, scopes...)
		}
		config.UpstreamOIDC.Scopes = scopes
		config.UpstreamOIDC.DisplayName = strings.TrimSpace(config.UpstreamOIDC.DisplayName)
		if config.UpstreamOIDC.DisplayName == "" {
			if strings.TrimRight(config.UpstreamOIDC.Issuer, "/") == "https://accounts.google.com" {
				config.UpstreamOIDC.DisplayName = "Google"
			} else {
				config.UpstreamOIDC.DisplayName = "SSO / Google"
			}
		}
	}

	config.TLSCertFile = strings.TrimSpace(config.TLSCertFile)
	config.TLSKeyFile = strings.TrimSpace(config.TLSKeyFile)
	if (config.TLSCertFile == "") != (config.TLSKeyFile == "") {
		return Config{}, fault.New(fault.InvalidInput, "TLS requires both --tls-cert-file and --tls-key-file")
	}

	if config.AccessTokenTTL < 0 || config.RefreshTokenTTL < 0 || config.AuthCodeTTL < 0 ||
		config.DeviceCodeTTL < 0 || config.DevicePollInterval < 0 || config.MaxBodyBytes < 0 {
		return Config{}, fault.New(fault.InvalidInput, "durations and byte limits must not be negative")
	}
	if config.AccessTokenTTL == 0 {
		config.AccessTokenTTL = defaultAccessTokenTTL
	}
	if config.RefreshTokenTTL == 0 {
		config.RefreshTokenTTL = defaultRefreshTokenTTL
	}
	if config.AuthCodeTTL == 0 {
		config.AuthCodeTTL = defaultAuthCodeTTL
	}
	if config.DeviceCodeTTL == 0 {
		config.DeviceCodeTTL = defaultDeviceCodeTTL
	}
	if config.DevicePollInterval == 0 {
		config.DevicePollInterval = defaultDevicePollInterval
	}
	if config.MaxBodyBytes == 0 {
		config.MaxBodyBytes = defaultMaxBodyBytes
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{
			Timeout: defaultOutboundTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return config, nil
}

func isValidClaimName(value string) bool {
	return value != "" && !strings.ContainsAny(value, " \t\r\n")
}

func isReservedJWTClaim(value string) bool {
	switch value {
	case "iss", "sub", "aud", "exp", "nbf", "iat", "jti", "nonce", "azp":
		return true
	default:
		return false
	}
}

// validateEndpointURL checks an https URL (or numeric loopback http URL).
func validateEndpointURL(label, raw string, allowQuery bool) error {
	if raw == "" {
		return fault.New(fault.InvalidInput, fmt.Sprintf("%s is empty", label))
	}
	if strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, " \t\r\n\\") {
		return fault.New(fault.InvalidInput, fmt.Sprintf("%s contains whitespace or invalid characters", label))
	}
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Opaque != "" || parsed.Host == "" || parsed.User != nil {
		return fault.New(fault.InvalidInput, fmt.Sprintf("%s must be an absolute URL without user information", label))
	}
	if parsed.Fragment != "" {
		return fault.New(fault.InvalidInput, fmt.Sprintf("%s must not carry a fragment", label))
	}
	if !allowQuery && (parsed.RawQuery != "" || parsed.ForceQuery) {
		return fault.New(fault.InvalidInput, fmt.Sprintf("%s must not carry a query", label))
	}
	host := parsed.Hostname()
	if host == "" {
		return fault.New(fault.InvalidInput, fmt.Sprintf("%s has an empty host", label))
	}
	if port := parsed.Port(); port != "" {
		num, err := strconv.Atoi(port)
		if err != nil || num <= 0 || num > 65535 {
			return fault.New(fault.InvalidInput, fmt.Sprintf("%s has an invalid port", label))
		}
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		return nil
	case "http":
		if host != "127.0.0.1" && host != "::1" {
			return fault.New(fault.InvalidInput, fmt.Sprintf("%s must use https outside numeric loopback", label))
		}
		return nil
	default:
		return fault.New(fault.InvalidInput, fmt.Sprintf("%s scheme %q is not supported", label, parsed.Scheme))
	}
}

// validateRedirectURI checks one OAuth 2.1 redirect URI: https or numeric
// loopback http, no userinfo and no fragment.
func validateRedirectURI(raw string) error {
	return validateEndpointURL("redirect URI", raw, true)
}

// isDynamicLoopbackCallback reports whether raw is an RFC 8252 §7.3 dynamic
// loopback callback URI: http://127.0.0.1:<port>/callback or
// http://[::1]:<port>/callback without query, fragment or userinfo.
func isDynamicLoopbackCallback(raw string) bool {
	if raw == "" || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, " \t\r\n\\") {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Opaque != "" || parsed.User != nil {
		return false
	}
	if !strings.EqualFold(parsed.Scheme, "http") {
		return false
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return false
	}
	if parsed.Path != "/callback" {
		return false
	}
	host, portStr, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		return false
	}
	if host != "127.0.0.1" && host != "::1" {
		return false
	}
	if len(portStr) == 0 || portStr[0] < '1' || portStr[0] > '9' {
		return false
	}
	for i := 0; i < len(portStr); i++ {
		if portStr[i] < '0' || portStr[i] > '9' {
			return false
		}
	}
	port, err := strconv.Atoi(portStr)
	return err == nil && port > 0 && port <= 65535
}

// matchesRegisteredRedirectURI checks whether requested matches registered,
// allowing RFC 8252 §7.3 dynamic port variation when registered is a numeric
// loopback http URI with the same host and path.
func matchesRegisteredRedirectURI(registered, requested string) bool {
	if registered == requested {
		return true
	}
	if err := validateRedirectURI(requested); err != nil {
		return false
	}
	regURL, err := url.Parse(registered)
	if err != nil {
		return false
	}
	reqURL, err := url.Parse(requested)
	if err != nil {
		return false
	}
	if !strings.EqualFold(regURL.Scheme, "http") || !strings.EqualFold(reqURL.Scheme, "http") {
		return false
	}
	regHost := regURL.Hostname()
	if regHost != "127.0.0.1" && regHost != "::1" {
		return false
	}
	if reqURL.Hostname() != regHost {
		return false
	}
	if reqURL.Port() == "" {
		return false
	}
	return regURL.Path == reqURL.Path && regURL.RawQuery == reqURL.RawQuery
}
