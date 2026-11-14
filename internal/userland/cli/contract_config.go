package cli

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/optimiware/kodama/client"
)

// Validate checks the trusted server URL, optional session file, optional CA
// bundle path, output format and registered trusted origins. Insecure,
// ambiguous and unknown configuration refuses.
func (config Config) Validate() error {
	if _, err := parseServerURL(config.ServerURL); err != nil {
		return err
	}
	if config.CredentialFile != "" && !filepath.IsAbs(config.CredentialFile) {
		return fmt.Errorf("credential file %q must be an absolute path", config.CredentialFile)
	}
	if config.CAFile != "" && strings.TrimSpace(config.CAFile) == "" {
		return fmt.Errorf("ca_file must not be blank")
	}
	if config.Output != "" && !config.Output.Valid() {
		return fmt.Errorf("unknown output format %q", config.Output)
	}
	for _, raw := range config.TrustedOrigins {
		if _, err := ParseOrigin(raw); err != nil {
			return err
		}
	}
	return nil
}

// ParseCABundle loads PEM-encoded CA certificates into a root certificate pool
// backed by the system trust store (or an empty pool when the system roots are
// unavailable). Empty or non-certificate PEM bundles refuse.
func ParseCABundle(pem []byte) (*x509.CertPool, error) {
	if len(bytes.TrimSpace(pem)) == 0 {
		return nil, fmt.Errorf("CA certificate bundle is empty")
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("CA certificate bundle contains no usable PEM certificates")
	}
	return pool, nil
}

// ParseConfig decodes and validates one kodactl configuration. Unknown fields,
// a non-object body and an invalid configuration refuse.
func ParseConfig(data []byte) (Config, error) {
	var config Config
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("invalid kodactl config: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Config{}, fmt.Errorf("invalid kodactl config: trailing data")
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

// parseServerURL validates one configured server URL.
func parseServerURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, fmt.Errorf("server_url is empty")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("server_url is invalid: %w", err)
	}
	if parsed.Opaque != "" {
		return nil, fmt.Errorf("server_url must not be opaque")
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("server_url must not carry user information")
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("server_url must be absolute")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, fmt.Errorf("server_url must not carry a query or fragment")
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		if !isLoopbackHost(parsed.Hostname()) {
			return nil, fmt.Errorf("server_url must use https outside loopback")
		}
	default:
		return nil, fmt.Errorf("server_url scheme %q is not supported", parsed.Scheme)
	}
	if err := validateCleanPath(parsed.Path); err != nil {
		return nil, err
	}
	return parsed, nil
}

// isLoopbackHost reports whether host is a development loopback host.
func isLoopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

// validateCleanPath refuses a path with an empty, dot or parent segment.
func validateCleanPath(path string) error {
	if path == "" || path == "/" {
		return nil
	}
	if strings.Contains(path, "//") {
		return fmt.Errorf("path %q has an empty segment", path)
	}
	trimmed := strings.TrimSuffix(path, "/")
	for _, segment := range strings.Split(strings.TrimPrefix(trimmed, "/"), "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("path %q has an unsafe segment", path)
		}
	}
	return nil
}

// ParseOrigin validates and parses one registered trusted Git origin. Only
// https, exact host and a clean path are accepted; userinfo, query, fragment,
// parent segments and insecure schemes refuse.
func ParseOrigin(raw string) (Origin, error) {
	if raw == "" {
		return Origin{}, fmt.Errorf("trusted origin is empty")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return Origin{}, fmt.Errorf("trusted origin %q is invalid: %w", raw, err)
	}
	if parsed.Scheme != "https" {
		return Origin{}, fmt.Errorf("trusted origin %q must use https", raw)
	}
	if parsed.Opaque != "" {
		return Origin{}, fmt.Errorf("trusted origin %q must not be opaque", raw)
	}
	if parsed.User != nil {
		return Origin{}, fmt.Errorf("trusted origin %q must not carry user information", raw)
	}
	if parsed.Host == "" {
		return Origin{}, fmt.Errorf("trusted origin %q must be absolute", raw)
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return Origin{}, fmt.Errorf("trusted origin %q must not carry a query or fragment", raw)
	}
	if err := validateCleanPath(parsed.Path); err != nil {
		return Origin{}, fmt.Errorf("trusted origin %q: %w", raw, err)
	}
	return Origin{Scheme: "https", Host: parsed.Host, Path: strings.TrimSuffix(parsed.Path, "/")}, nil
}

// String renders the origin.
func (origin Origin) String() string {
	return origin.Scheme + "://" + origin.Host + origin.Path
}

// Matches reports whether target is under this trusted origin. The scheme and
// host must match exactly and the path must match at a segment boundary.
func (origin Origin) Matches(target string) bool {
	parsed, err := ParseOrigin(target)
	if err != nil {
		return false
	}
	if parsed.Scheme != origin.Scheme || parsed.Host != origin.Host {
		return false
	}
	if origin.Path == "" {
		return true
	}
	return parsed.Path == origin.Path || strings.HasPrefix(parsed.Path, origin.Path+"/")
}

// Validate checks that the session uses a human or execution source, an
// absolute mounted token path and an expiry. An opaque mounted execution
// session may omit the expiry because the token file never carries one; a human
// session must carry an explicit expiry. A service credential is never a helper
// session.
func (session Session) Validate() error {
	if session.Source != client.KindSession && session.Source != client.KindExecution {
		return fmt.Errorf("session source %q must be session or execution", session.Source)
	}
	if session.TokenPath == "" || !filepath.IsAbs(session.TokenPath) {
		return fmt.Errorf("session token path %q must be absolute", session.TokenPath)
	}
	if session.ExpiresAt.IsZero() && session.Source != client.KindExecution {
		return fmt.Errorf("session expiry is zero")
	}
	return nil
}

// Usable reports whether the session is valid and current at now. An
// unknown-expiry mounted execution session is current; any explicit expiry
// equal to now is expired.
func (session Session) Usable(now time.Time) bool {
	if session.Validate() != nil {
		return false
	}
	if session.ExpiresAt.IsZero() {
		return session.Source == client.KindExecution
	}
	return now.Before(session.ExpiresAt)
}

// Validate checks the helper protocol and absolute request URL.
func (request HelperRequest) Validate() error {
	switch request.Protocol {
	case "get", "store", "erase":
	default:
		return fmt.Errorf("unsupported helper protocol %q", request.Protocol)
	}
	if request.URL == "" {
		return fmt.Errorf("helper URL is empty")
	}
	parsed, err := url.Parse(request.URL)
	if err != nil {
		return fmt.Errorf("helper URL is invalid: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("helper URL must be absolute")
	}
	return nil
}

// String renders the helper response without the password.
func (response HelperResponse) String() string {
	return fmt.Sprintf("username=%s password=<redacted>", response.Username)
}

// HelperDecision answers one Git credential-helper request. A token is returned
// as the password for username kodama only for a registered trusted origin, a
// current human or mounted execution credential and a `get` request;
// store/erase, unregistered targets, insecure schemes, expired credentials and
// every service credential produce an empty refusal.
func HelperDecision(config Config, request HelperRequest, credential client.Credential, now time.Time) (HelperResponse, error) {
	if err := request.Validate(); err != nil {
		return HelperResponse{}, err
	}
	if request.Protocol != "get" {
		return HelperResponse{}, nil
	}
	target, err := ParseOrigin(request.URL)
	if err != nil {
		return HelperResponse{}, nil
	}
	trusted := false
	for _, raw := range config.TrustedOrigins {
		origin, err := ParseOrigin(raw)
		if err != nil {
			continue
		}
		if origin.Matches(target.String()) {
			trusted = true
			break
		}
	}
	if !trusted {
		return HelperResponse{}, nil
	}
	if credential.Kind == client.KindService {
		return HelperResponse{}, nil
	}
	if !credential.Usable(now) {
		return HelperResponse{}, nil
	}
	return HelperResponse{Username: "kodama", Password: credential.Token}, nil
}
