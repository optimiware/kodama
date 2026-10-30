package mcpserver

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the MCP server's complete configuration. It deliberately contains
// no Kodama database, broker, object-store or KMS setting: the server is a
// client of the public API. Its only credential is the dedicated token-exchange
// client secret, read from a mounted file.
type Config struct {
	APIURL   string
	Resource string
	Audience string
	Issuer   string
	Listen   string

	// APIAudience and APIIssuer describe the separate resource the MCP server
	// calls after exchanging its incoming token. APIResource defaults to
	// APIAudience.
	APIAudience string
	APIIssuer   string
	APIResource string

	// Exchange is the RFC 8693 token-exchange endpoint and the confidential
	// client identity used to obtain the delegated API credential.
	ExchangeURL              string
	ExchangeClientID         string
	ExchangeClientSecretFile string
	ExchangeClientAuth       string
	ExchangeScopes           []string
	ExchangeCAFile           string

	Scopes                []string
	ReadOnly              bool
	Toolsets              []string
	EnableSecretWrites    bool
	EnableCredentialTools bool

	APICAFile  string
	OIDCCAFile string

	AllowInsecureHTTP    bool
	AllowInsecureAPIHTTP bool
	DefaultTenant        string
	DefaultProject       string

	HTTPTimeout      time.Duration
	MaxResponseBytes int64
}

// ParseServeConfig parses `kodama mcp serve` flags with environment fallbacks.
func ParseServeConfig(args []string, getenv func(string) string, output io.Writer) (Config, error) {
	fs := flag.NewFlagSet("kodama mcp serve", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.Usage = func() {
		fmt.Fprintln(output, "Usage: kodama mcp serve [flags]\n\nExpose the Kodama public API as an OAuth-protected MCP server:")
		fs.PrintDefaults()
	}
	str := func(name, env, fallback, usage string) *string {
		return fs.String(name, envString(getenv, env, fallback), usage+" ("+env+")")
	}
	boolFlag := func(name, env string, fallback bool, usage string) *bool {
		return fs.Bool(name, envBool(getenv, env, fallback), usage+" ("+env+")")
	}
	cfg := Config{}
	apiURL := str("api-url", "KODAMA_MCP_API_URL", "", "Kodama API base URL including /api/v1")
	resource := str("resource", "KODAMA_MCP_RESOURCE", "", "exact public MCP resource URL advertised in OAuth metadata")
	audience := str("audience", "KODAMA_MCP_AUDIENCE", "", "audience of tokens issued for this MCP server (defaults to --resource)")
	issuer := str("issuer", "KODAMA_MCP_ISSUER", "", "OAuth/OIDC issuer that issues MCP access tokens")
	apiAudience := str("api-audience", "KODAMA_MCP_API_AUDIENCE", "", "audience of the delegated Kodama API token")
	apiIssuer := str("api-issuer", "KODAMA_MCP_API_ISSUER", "", "issuer of the delegated Kodama API token (defaults to --issuer)")
	apiResource := str("api-resource", "KODAMA_MCP_API_RESOURCE", "", "RFC 8707 resource for the delegated API token (defaults to --api-audience)")
	exchangeURL := str("exchange-url", "KODAMA_MCP_EXCHANGE_URL", "", "RFC 8693 token-exchange endpoint")
	exchangeClientID := str("exchange-client-id", "KODAMA_MCP_EXCHANGE_CLIENT_ID", "", "confidential OAuth client id used for token exchange")
	exchangeSecret := str("exchange-client-secret-file", "KODAMA_MCP_EXCHANGE_CLIENT_SECRET_FILE", "", "file containing the token-exchange client secret")
	exchangeClientAuth := str("exchange-client-auth", "KODAMA_MCP_EXCHANGE_CLIENT_AUTH", "client_secret_basic", "exchange client authentication: client_secret_basic or client_secret_post")
	exchangeScopes := str("exchange-scopes", "KODAMA_MCP_EXCHANGE_SCOPES", "", "comma-separated scopes requested from token exchange")
	exchangeCA := str("exchange-ca-file", "KODAMA_MCP_EXCHANGE_CA_FILE", "", "PEM CA bundle for the token-exchange endpoint")
	listen := str("listen", "KODAMA_MCP_LISTEN", ":8090", "HTTP listen address")
	scopes := str("scopes", "KODAMA_MCP_SCOPES", "openid", "comma-separated scopes advertised by the protected-resource metadata")
	toolsets := str("toolsets", "KODAMA_MCP_TOOLSETS", "", "comma-separated tool groups; empty means all")
	apiCA := str("api-ca-file", "KODAMA_MCP_API_CA_FILE", "", "PEM CA bundle for the Kodama API")
	oidcCA := str("oidc-ca-file", "KODAMA_MCP_OIDC_CA_FILE", "", "PEM CA bundle for issuer discovery and JWKS")
	defaultTenant := str("default-tenant", "KODAMA_MCP_DEFAULT_TENANT", "", "optional default tenant ID")
	defaultProject := str("default-project", "KODAMA_MCP_DEFAULT_PROJECT", "", "optional default project ID")
	readOnly := boolFlag("read-only", "KODAMA_MCP_READ_ONLY", false, "expose only read-only API operations")
	secretWrites := boolFlag("enable-secret-writes", "KODAMA_MCP_ENABLE_SECRET_WRITES", false, "expose secret-value write tools")
	credentialTools := boolFlag("enable-credential-tools", "KODAMA_MCP_ENABLE_CREDENTIAL_TOOLS", false, "expose tools that return Git credentials")
	allowInsecure := boolFlag("allow-insecure-http", "KODAMA_MCP_ALLOW_INSECURE_HTTP", false, "allow http for public/issuer/exchange endpoints (development only)")
	allowInsecureAPI := boolFlag("allow-insecure-api-http", "KODAMA_MCP_ALLOW_INSECURE_API_HTTP", false, "allow the in-cluster Kodama API to use plain http")
	timeout := fs.Duration("http-timeout", envDuration(getenv, "KODAMA_MCP_HTTP_TIMEOUT", 30*time.Second), "per-call HTTP timeout (KODAMA_MCP_HTTP_TIMEOUT)")
	maxBytes := fs.Int64("max-response-bytes", envInt64(getenv, "KODAMA_MCP_MAX_RESPONSE_BYTES", 4<<20), "maximum API response bytes (KODAMA_MCP_MAX_RESPONSE_BYTES)")

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() != 0 {
		return Config{}, errors.New("kodama mcp serve accepts flags only, not positional arguments")
	}
	cfg = Config{
		APIURL: strings.TrimSpace(*apiURL), Resource: strings.TrimRight(strings.TrimSpace(*resource), "/"),
		Audience: strings.TrimSpace(*audience), Issuer: strings.TrimSpace(*issuer), Listen: strings.TrimSpace(*listen),
		APIAudience: strings.TrimSpace(*apiAudience), APIIssuer: strings.TrimSpace(*apiIssuer), APIResource: strings.TrimSpace(*apiResource),
		ExchangeURL: strings.TrimSpace(*exchangeURL), ExchangeClientID: strings.TrimSpace(*exchangeClientID),
		ExchangeClientSecretFile: strings.TrimSpace(*exchangeSecret), ExchangeClientAuth: strings.TrimSpace(*exchangeClientAuth), ExchangeCAFile: strings.TrimSpace(*exchangeCA),
		ReadOnly: *readOnly, EnableSecretWrites: *secretWrites, EnableCredentialTools: *credentialTools,
		APICAFile: strings.TrimSpace(*apiCA), OIDCCAFile: strings.TrimSpace(*oidcCA),
		AllowInsecureHTTP: *allowInsecure, AllowInsecureAPIHTTP: *allowInsecureAPI,
		DefaultTenant: strings.TrimSpace(*defaultTenant), DefaultProject: strings.TrimSpace(*defaultProject),
		HTTPTimeout: *timeout, MaxResponseBytes: *maxBytes,
	}
	if cfg.Audience == "" {
		cfg.Audience = cfg.Resource
	}
	if cfg.APIIssuer == "" {
		cfg.APIIssuer = cfg.Issuer
	}
	if cfg.APIResource == "" {
		cfg.APIResource = cfg.APIAudience
	}
	for _, item := range strings.Split(*scopes, ",") {
		if item = strings.TrimSpace(item); item != "" {
			cfg.Scopes = append(cfg.Scopes, item)
		}
	}
	for _, item := range strings.Split(*toolsets, ",") {
		if item = strings.TrimSpace(item); item != "" {
			cfg.Toolsets = append(cfg.Toolsets, item)
		}
	}
	for _, item := range strings.Split(*exchangeScopes, ",") {
		if item = strings.TrimSpace(item); item != "" {
			cfg.ExchangeScopes = append(cfg.ExchangeScopes, item)
		}
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate rejects configurations that would create an ambiguous resource,
// audience or API endpoint.
func (c Config) Validate() error {
	switch {
	case c.APIURL == "":
		return errors.New("kodama mcp: --api-url is required")
	case c.Resource == "":
		return errors.New("kodama mcp: --resource is required")
	case c.Audience == "":
		return errors.New("kodama mcp: --audience is required")
	case c.Issuer == "":
		return errors.New("kodama mcp: --issuer is required")
	case c.APIAudience == "":
		return errors.New("kodama mcp: --api-audience is required")
	case c.APIIssuer == "":
		return errors.New("kodama mcp: --api-issuer is required")
	case c.APIResource == "":
		return errors.New("kodama mcp: --api-resource is required")
	case c.ExchangeURL == "":
		return errors.New("kodama mcp: --exchange-url is required")
	case c.ExchangeClientID == "":
		return errors.New("kodama mcp: --exchange-client-id is required")
	case c.ExchangeClientSecretFile == "":
		return errors.New("kodama mcp: --exchange-client-secret-file is required")
	case c.ExchangeClientAuth != "" && c.ExchangeClientAuth != "client_secret_basic" && c.ExchangeClientAuth != "client_secret_post":
		return errors.New("kodama mcp: --exchange-client-auth must be client_secret_basic or client_secret_post")
	case c.Listen == "":
		return errors.New("kodama mcp: --listen is required")
	case c.HTTPTimeout <= 0:
		return errors.New("kodama mcp: --http-timeout must be positive")
	case c.MaxResponseBytes < 1024:
		return errors.New("kodama mcp: --max-response-bytes must be at least 1024")
	case c.DefaultProject != "" && c.DefaultTenant == "":
		return errors.New("kodama mcp: --default-project requires --default-tenant")
	}
	if err := validateURL("api-url", c.APIURL, c.AllowInsecureAPIHTTP || c.AllowInsecureHTTP); err != nil {
		return err
	}
	if err := validateURL("resource", c.Resource, c.AllowInsecureHTTP); err != nil {
		return err
	}
	if err := validateURL("audience", c.Audience, c.AllowInsecureHTTP); err != nil {
		return err
	}
	if err := validateURL("issuer", c.Issuer, c.AllowInsecureHTTP); err != nil {
		return err
	}
	if err := validateURL("api-audience", c.APIAudience, c.AllowInsecureHTTP); err != nil {
		return err
	}
	if err := validateURL("api-issuer", c.APIIssuer, c.AllowInsecureHTTP); err != nil {
		return err
	}
	if err := validateURL("api-resource", c.APIResource, c.AllowInsecureHTTP); err != nil {
		return err
	}
	if err := validateURL("exchange-url", c.ExchangeURL, c.AllowInsecureHTTP); err != nil {
		return err
	}
	u, _ := url.Parse(c.APIURL)
	if u.Path == "" || u.Path == "/" {
		return errors.New("kodama mcp: --api-url must include the API path, for example /api/v1")
	}
	return nil
}

func validateURL(name, raw string, allowInsecure bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("kodama mcp: --%s must be an absolute URL without credentials, query or fragment", name)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme != "http" {
		return fmt.Errorf("kodama mcp: --%s must use https", name)
	}
	if allowInsecure || isLoopback(u.Hostname()) {
		return nil
	}
	return fmt.Errorf("kodama mcp: --%s must use https (http is allowed only for loopback or with --allow-insecure-http)", name)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func envString(getenv func(string) string, name, fallback string) string {
	if v := getenv(name); v != "" {
		return v
	}
	return fallback
}

func envBool(getenv func(string) string, name string, fallback bool) bool {
	v := getenv(name)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}

func envDuration(getenv func(string) string, name string, fallback time.Duration) time.Duration {
	v := getenv(name)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}

func envInt64(getenv func(string) string, name string, fallback int64) int64 {
	v := getenv(name)
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fallback
	}
	return n
}

// ReadCA reads an optional PEM bundle. An empty path is not an error.
func ReadCA(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read CA %s: %w", path, err)
	}
	return b, nil
}
