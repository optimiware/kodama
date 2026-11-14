package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/installation"
	shellapp "github.com/optimiware/kodama/internal/shell/app"
	driverkube "github.com/optimiware/kodama/internal/shell/app/drivers/kube"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// serverApp is the constructed Shell composition surface the entrypoint drives.
type serverApp interface {
	Handler() http.Handler
	Serve(context.Context, net.Listener) error
	Close(context.Context) error
}

// serverDeps are the injectable server collaborators.
type serverDeps struct {
	newApp func(context.Context, shellapp.Config) (serverApp, error)
	listen func(string, string) (net.Listener, error)
	// build assembles the production composition from its configuration. It is
	// a seam so the serving path (TLS wrapping, exit codes, drain and cleanup)
	// is testable without a cluster service account; nil uses buildProduction
	// over the process environment.
	build func(serverOptions) (productionBuild, error)
	// buildTest assembles the durable test composition; nil uses
	// buildTestProfile over the process environment.
	buildTest func(serverOptions) (productionBuild, error)
}

func defaultServerDeps() serverDeps {
	return serverDeps{
		newApp: func(ctx context.Context, config shellapp.Config) (serverApp, error) {
			return shellapp.New(ctx, config)
		},
		listen: net.Listen,
		build: func(options serverOptions) (productionBuild, error) {
			return buildProduction(options, os.Getenv, driverkube.InClusterConfig{})
		},
		buildTest: func(options serverOptions) (productionBuild, error) {
			return buildTestProfile(options, os.Getenv)
		},
	}
}

// runServerEntry is the production entrypoint for `kodama server`.
func runServerEntry(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runServer(ctx, args, stdout, stderr, defaultServerDeps())
}

// serverOptions is the merged config-file/flag/environment input.
type serverOptions struct {
	Profile        string
	Address        string
	DataDir        string
	InstallationID string
	PublicURL      string
	// WorkloadURL is the test profile's base URL workloads reach the server
	// through, separate from the client-facing public URL.
	WorkloadURL string
	// WorkloadCAFile optionally names the PEM CA bundle mounted read-only into
	// Podman workloads at /kodama/ca.crt (with KODAMA_CA_FILE=/kodama/ca.crt).
	WorkloadCAFile string
	MCPPath        string
	// LoopInterval is the durable profiles' background scan cadence. Zero
	// selects the profile default (one second); the value is bounded by
	// resolvedLoopInterval before any adapter is built.
	LoopInterval time.Duration
	// GitStorageRoot is the mounted tenant Git storage root. When set the
	// durable profiles reconcile retained-commit keep refs for every
	// repository under each tenant directory of this root. An empty value
	// keeps the sweep disabled; a partial setting is a named refusal.
	GitStorageRoot string
	DevTokens      []string
	DevPrincipal   string
	DevTenant      string
	DevProject     string
	DevRoles       string
	DevExpiry      time.Duration
	Discovery      discoveryOptions
	// ModelsFile and ToolProxyFile name the trusted files the optional model
	// gateway and external MCP tool proxy are read from. An empty path keeps
	// that optional surface disabled; a named file is never defaulted.
	ModelsFile    string
	ToolProxyFile string
}

// The durable entries' background scan cadence. The development composition
// keeps its explicit 20 ms loop; production and the durable test profile scan
// PostgreSQL and the broker once a second by default instead of inheriting the
// development busy loop.
const (
	productionLoopInterval = time.Second
	minLoopInterval        = 100 * time.Millisecond
	maxLoopInterval        = time.Minute
)

// resolvedLoopInterval returns the configured scan cadence or the profile
// default, refusing an interval outside the supported bound before any
// composition is built. Zero means "not configured".
func resolvedLoopInterval(configured, fallback time.Duration) (time.Duration, error) {
	interval := configured
	if interval == 0 {
		interval = fallback
	}
	if interval < minLoopInterval || interval > maxLoopInterval {
		return 0, fmt.Errorf("loop interval must be between %s and %s", minLoopInterval, maxLoopInterval)
	}
	return interval, nil
}

// discoveryOptions are the optional Shell public login discovery settings. The
// document is public: it carries the public client id, the OAuth endpoints, the
// granted scopes and the public API base URL, never a secret.
type discoveryOptions struct {
	Issuer                      string
	ClientID                    string
	Audience                    string
	AuthorizationEndpoint       string
	TokenEndpoint               string
	DeviceAuthorizationEndpoint string
	RevocationEndpoint          string
	Scopes                      []string
	APIURL                      string
}

// configured reports whether any discovery setting was supplied.
func (options discoveryOptions) configured() bool {
	return options.Issuer != "" || options.ClientID != "" || options.Audience != "" ||
		options.AuthorizationEndpoint != "" || options.TokenEndpoint != "" ||
		options.DeviceAuthorizationEndpoint != "" || options.RevocationEndpoint != "" ||
		len(options.Scopes) > 0 || options.APIURL != ""
}

func defaultServerOptions() serverOptions {
	return serverOptions{
		Profile:        "",
		Address:        "127.0.0.1:8080",
		DataDir:        "",
		InstallationID: "installation-dev",
		PublicURL:      "http://127.0.0.1:8080",
		MCPPath:        shellapp.DefaultMCPPath,
		DevPrincipal:   "admin-1",
		DevTenant:      "tenant-1",
		DevProject:     "project-1",
		DevRoles:       "admin,engineer,reviewer,operator",
		DevExpiry:      24 * time.Hour,
	}
}

// applyServerEnv overlays the environment on the file/default options. A
// malformed loop interval is a configuration refusal rather than a silent
// fallback to a cadence the operator did not choose.
func applyServerEnv(options serverOptions) (serverOptions, error) {
	options.Profile = envOr("KODAMA_PROFILE", options.Profile)
	options.Address = envOr("KODAMA_ADDRESS", options.Address)
	options.DataDir = envOr("KODAMA_DATA_DIR", options.DataDir)
	options.InstallationID = envOr("KODAMA_INSTALLATION_ID", options.InstallationID)
	options.PublicURL = envOr("KODAMA_PUBLIC_URL", options.PublicURL)
	options.WorkloadURL = envOr("KODAMA_WORKLOAD_URL", options.WorkloadURL)
	options.WorkloadCAFile = envOr("KODAMA_WORKLOAD_CA_FILE", options.WorkloadCAFile)
	options.MCPPath = envOr("KODAMA_MCP_PATH", options.MCPPath)
	options.GitStorageRoot = envOr("KODAMA_GIT_STORAGE_ROOT", options.GitStorageRoot)
	interval, err := envDurationStrict("KODAMA_LOOP_INTERVAL", options.LoopInterval)
	if err != nil {
		return serverOptions{}, err
	}
	options.LoopInterval = interval
	if tokens := envList("KODAMA_DEV_TOKEN"); len(tokens) > 0 {
		options.DevTokens = tokens
	}
	options.DevPrincipal = envOr("KODAMA_DEV_PRINCIPAL", options.DevPrincipal)
	options.DevTenant = envOr("KODAMA_DEV_TENANT", options.DevTenant)
	options.DevProject = envOr("KODAMA_DEV_PROJECT", options.DevProject)
	options.DevRoles = envOr("KODAMA_DEV_ROLES", options.DevRoles)
	options.DevExpiry = envDuration("KODAMA_DEV_EXPIRY", options.DevExpiry)
	options.ModelsFile = envOr(shellapp.ModelsFileSetting, options.ModelsFile)
	options.ToolProxyFile = envOr(shellapp.ToolProxyFileSetting, options.ToolProxyFile)
	options.Discovery.Issuer = envOr("KODAMA_DISCOVERY_ISSUER", options.Discovery.Issuer)
	options.Discovery.ClientID = envOr("KODAMA_DISCOVERY_CLIENT_ID", options.Discovery.ClientID)
	options.Discovery.Audience = envOr("KODAMA_DISCOVERY_AUDIENCE", options.Discovery.Audience)
	options.Discovery.AuthorizationEndpoint = envOr("KODAMA_DISCOVERY_AUTHORIZATION_ENDPOINT", options.Discovery.AuthorizationEndpoint)
	options.Discovery.TokenEndpoint = envOr("KODAMA_DISCOVERY_TOKEN_ENDPOINT", options.Discovery.TokenEndpoint)
	options.Discovery.DeviceAuthorizationEndpoint = envOr("KODAMA_DISCOVERY_DEVICE_AUTHORIZATION_ENDPOINT", options.Discovery.DeviceAuthorizationEndpoint)
	options.Discovery.RevocationEndpoint = envOr("KODAMA_DISCOVERY_REVOCATION_ENDPOINT", options.Discovery.RevocationEndpoint)
	options.Discovery.APIURL = envOr("KODAMA_DISCOVERY_API_URL", options.Discovery.APIURL)
	if scopes := envScopes("KODAMA_DISCOVERY_SCOPES"); len(scopes) > 0 {
		options.Discovery.Scopes = scopes
	}
	return options, nil
}

// envScopes reads one comma- or whitespace-separated scope list.
func envScopes(name string) []string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return nil
	}
	return strings.FieldsFunc(value, func(character rune) bool {
		return character == ',' || unicode.IsSpace(character)
	})
}

func envOr(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok {
		return value
	}
	return fallback
}

func envList(name string) []string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return nil
	}
	return []string{value}
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

// envDurationStrict reads one optional duration setting and refuses a value it
// cannot parse. The diagnostic names the setting, never its value.
func envDurationStrict(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a Go duration such as 250ms or 5s", name)
	}
	return parsed, nil
}

// serverFile is the optional JSON configuration file shape.
type serverFile struct {
	Profile        string               `json:"profile"`
	Address        string               `json:"address"`
	DataDir        string               `json:"data_dir"`
	InstallationID string               `json:"installation_id"`
	PublicURL      string               `json:"public_url"`
	WorkloadURL    string               `json:"workload_url"`
	WorkloadCAFile string               `json:"workload_ca_file"`
	MCPPath        string               `json:"mcp_path"`
	LoopInterval   string               `json:"loop_interval"`
	GitStorageRoot string               `json:"git_storage_root"`
	DevTokens      []string             `json:"dev_tokens"`
	DevPrincipal   string               `json:"dev_principal"`
	DevTenant      string               `json:"dev_tenant"`
	DevProject     string               `json:"dev_project"`
	DevRoles       string               `json:"dev_roles"`
	DevExpiry      string               `json:"dev_expiry"`
	Discovery      *serverDiscoveryFile `json:"discovery"`
	ModelsFile     string               `json:"models_file"`
	ToolProxyFile  string               `json:"tool_proxy_file"`
}

// serverDiscoveryFile is the optional public discovery configuration nested as
// `discovery` in the server configuration file.
type serverDiscoveryFile struct {
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

// stringList is a repeatable string flag.
type stringList []string

func (list *stringList) String() string { return strings.Join(*list, ",") }

func (list *stringList) Set(value string) error {
	*list = append(*list, value)
	return nil
}

// parseServerOptions merges the config file, environment and flags. Precedence
// is flags > environment > config file > defaults.
func parseServerOptions(args []string, stderr io.Writer) (serverOptions, int) {
	configPath := scanConfigPath(args)
	options := defaultServerOptions()
	if strings.TrimSpace(configPath) != "" {
		loaded, err := loadServerFile(configPath)
		if err != nil {
			fmt.Fprintf(stderr, "kodama: %v\n", err)
			return serverOptions{}, 2
		}
		merged, err := mergeServerFile(loaded, options)
		if err != nil {
			fmt.Fprintf(stderr, "kodama: %v\n", err)
			return serverOptions{}, 2
		}
		options = merged
	}
	options, err := applyServerEnv(options)
	if err != nil {
		fmt.Fprintf(stderr, "kodama: %v\n", err)
		return serverOptions{}, 2
	}

	flags := flag.NewFlagSet("server", flag.ContinueOnError)
	flags.SetOutput(stderr)
	tokens := stringList(options.DevTokens)
	flags.StringVar(&configPath, "config", configPath, "JSON configuration file")
	flags.StringVar(&options.Profile, "profile", options.Profile, "composition profile (development, test or production)")
	flags.StringVar(&options.Address, "address", options.Address, "listen address")
	flags.StringVar(&options.DataDir, "data-dir", options.DataDir, "data directory")
	flags.StringVar(&options.InstallationID, "installation-id", options.InstallationID, "installation identity")
	flags.StringVar(&options.PublicURL, "public-url", options.PublicURL, "public URL")
	flags.StringVar(&options.WorkloadURL, "workload-url", options.WorkloadURL, "test profile: base URL workloads reach the server through")
	flags.StringVar(&options.WorkloadCAFile, "workload-ca-file", options.WorkloadCAFile, "test profile: PEM CA bundle mounted read-only into workloads at /kodama/ca.crt")
	flags.StringVar(&options.MCPPath, "mcp-path", options.MCPPath, "MCP JSON-RPC path")
	flags.DurationVar(&options.LoopInterval, "loop-interval", options.LoopInterval, "durable profiles' background scan interval (default 1s, 100ms to 1m)")
	flags.StringVar(&options.GitStorageRoot, "git-storage-root", options.GitStorageRoot, "mounted tenant Git storage root for retained-commit keep refs")
	flags.Var(&tokens, "dev-token", "development bearer token (repeatable)")
	flags.StringVar(&options.DevPrincipal, "dev-principal", options.DevPrincipal, "development principal identity")
	flags.StringVar(&options.DevTenant, "dev-tenant", options.DevTenant, "development tenant identity")
	flags.StringVar(&options.DevProject, "dev-project", options.DevProject, "development project identity")
	flags.StringVar(&options.DevRoles, "dev-roles", options.DevRoles, "development role list")
	flags.DurationVar(&options.DevExpiry, "dev-expiry", options.DevExpiry, "development credential expiry")
	discoveryScopes := stringList(options.Discovery.Scopes)
	flags.StringVar(&options.Discovery.Issuer, "discovery-issuer", options.Discovery.Issuer, "public discovery issuer")
	flags.StringVar(&options.Discovery.ClientID, "discovery-client-id", options.Discovery.ClientID, "public discovery client id")
	flags.StringVar(&options.Discovery.Audience, "discovery-audience", options.Discovery.Audience, "public discovery audience")
	flags.StringVar(&options.Discovery.AuthorizationEndpoint, "discovery-authorization-endpoint", options.Discovery.AuthorizationEndpoint, "public discovery authorization endpoint")
	flags.StringVar(&options.Discovery.TokenEndpoint, "discovery-token-endpoint", options.Discovery.TokenEndpoint, "public discovery token endpoint")
	flags.StringVar(&options.Discovery.DeviceAuthorizationEndpoint, "discovery-device-authorization-endpoint", options.Discovery.DeviceAuthorizationEndpoint, "public discovery device authorization endpoint")
	flags.StringVar(&options.Discovery.RevocationEndpoint, "discovery-revocation-endpoint", options.Discovery.RevocationEndpoint, "public discovery revocation endpoint")
	flags.Var(&discoveryScopes, "discovery-scope", "public discovery scope (repeatable)")
	flags.StringVar(&options.Discovery.APIURL, "discovery-api-url", options.Discovery.APIURL, "public discovery API base URL")
	if err := flags.Parse(args); err != nil {
		return serverOptions{}, 2
	}
	if flags.NArg() != 0 {
		usage(stderr, fmt.Sprintf("unexpected argument %q", flags.Arg(0)))
		return serverOptions{}, 2
	}
	options.DevTokens = tokens
	options.Discovery.Scopes = discoveryScopes
	return options, 0
}

// scanConfigPath finds the --config value before the main flag parse so the
// file can supply defaults.
func scanConfigPath(args []string) string {
	for index := range args {
		argument := args[index]
		switch {
		case argument == "--config" || argument == "-config":
			if index+1 < len(args) {
				return args[index+1]
			}
		case strings.HasPrefix(argument, "--config="):
			return strings.TrimPrefix(argument, "--config=")
		case strings.HasPrefix(argument, "-config="):
			return strings.TrimPrefix(argument, "-config=")
		}
	}
	return ""
}

func loadServerFile(path string) (serverFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return serverFile{}, fmt.Errorf("read server configuration %q: %w", path, err)
	}
	var file serverFile
	if err := json.Unmarshal(data, &file); err != nil {
		return serverFile{}, fmt.Errorf("decode server configuration %q: %w", path, err)
	}
	return file, nil
}

// mergeServerFile fills fields the flags left at their environment defaults.
// A flag value that differs from the environment default is preserved. A
// malformed loop interval is refused rather than silently ignored.
func mergeServerFile(file serverFile, options serverOptions) (serverOptions, error) {
	if file.Profile != "" {
		options.Profile = file.Profile
	}
	if file.Address != "" {
		options.Address = file.Address
	}
	if file.DataDir != "" {
		options.DataDir = file.DataDir
	}
	if file.InstallationID != "" {
		options.InstallationID = file.InstallationID
	}
	if file.PublicURL != "" {
		options.PublicURL = file.PublicURL
	}
	if file.WorkloadURL != "" {
		options.WorkloadURL = file.WorkloadURL
	}
	if file.WorkloadCAFile != "" {
		options.WorkloadCAFile = file.WorkloadCAFile
	}
	if file.MCPPath != "" {
		options.MCPPath = file.MCPPath
	}
	if file.LoopInterval != "" {
		parsed, err := time.ParseDuration(file.LoopInterval)
		if err != nil {
			return serverOptions{}, fmt.Errorf("server configuration loop_interval must be a Go duration such as 250ms or 5s")
		}
		options.LoopInterval = parsed
	}
	if file.GitStorageRoot != "" {
		options.GitStorageRoot = file.GitStorageRoot
	}
	if len(file.DevTokens) > 0 {
		options.DevTokens = append([]string(nil), file.DevTokens...)
	}
	if file.DevPrincipal != "" {
		options.DevPrincipal = file.DevPrincipal
	}
	if file.DevTenant != "" {
		options.DevTenant = file.DevTenant
	}
	if file.DevProject != "" {
		options.DevProject = file.DevProject
	}
	if file.DevRoles != "" {
		options.DevRoles = file.DevRoles
	}
	if file.DevExpiry != "" {
		if parsed, err := time.ParseDuration(file.DevExpiry); err == nil {
			options.DevExpiry = parsed
		}
	}
	if file.ModelsFile != "" {
		options.ModelsFile = file.ModelsFile
	}
	if file.ToolProxyFile != "" {
		options.ToolProxyFile = file.ToolProxyFile
	}
	if file.Discovery != nil {
		if file.Discovery.Issuer != "" {
			options.Discovery.Issuer = file.Discovery.Issuer
		}
		if file.Discovery.ClientID != "" {
			options.Discovery.ClientID = file.Discovery.ClientID
		}
		if file.Discovery.Audience != "" {
			options.Discovery.Audience = file.Discovery.Audience
		}
		if file.Discovery.AuthorizationEndpoint != "" {
			options.Discovery.AuthorizationEndpoint = file.Discovery.AuthorizationEndpoint
		}
		if file.Discovery.TokenEndpoint != "" {
			options.Discovery.TokenEndpoint = file.Discovery.TokenEndpoint
		}
		if file.Discovery.DeviceAuthorizationEndpoint != "" {
			options.Discovery.DeviceAuthorizationEndpoint = file.Discovery.DeviceAuthorizationEndpoint
		}
		if file.Discovery.RevocationEndpoint != "" {
			options.Discovery.RevocationEndpoint = file.Discovery.RevocationEndpoint
		}
		if len(file.Discovery.Scopes) > 0 {
			options.Discovery.Scopes = append([]string(nil), file.Discovery.Scopes...)
		}
		if file.Discovery.APIURL != "" {
			options.Discovery.APIURL = file.Discovery.APIURL
		}
	}
	return options, nil
}

// runServer validates the explicit profile, prints the loud development banner
// and serves the Shell composition until cancellation.
func runServer(ctx context.Context, args []string, stdout, stderr io.Writer, deps serverDeps) int {
	options, code := parseServerOptions(args, stderr)
	if code != 0 {
		return code
	}
	switch strings.TrimSpace(options.Profile) {
	case "":
		usage(stderr, "server requires an explicit --profile (development, test or production)")
		return 2
	case "production":
		return runProduction(ctx, options, stderr, deps)
	case "test":
		return runTestProfile(ctx, options, stderr, deps)
	case "development":
	default:
		usage(stderr, fmt.Sprintf("unknown profile %q", options.Profile))
		return 2
	}
	fmt.Fprintln(stderr, "WARNING: development composition only: static dev tokens, in-memory state, ephemeral keys and the process runtime. Never use this profile in production.")
	config, err := options.config()
	if err != nil {
		fmt.Fprintf(stderr, "kodama: %v\n", err)
		return 2
	}
	app, err := deps.newApp(ctx, config)
	if err != nil {
		fmt.Fprintf(stderr, "kodama: %v\n", err)
		return 1
	}
	listener, err := deps.listen("tcp", options.Address)
	if err != nil {
		_ = app.Close(context.Background())
		fmt.Fprintf(stderr, "kodama: listen %q: %v\n", options.Address, err)
		return 1
	}
	if err := app.Serve(ctx, listener); err != nil {
		fmt.Fprintf(stderr, "kodama: %v\n", err)
		return 1
	}
	return 0
}

// runProduction serves the production composition over TLS. Its configuration
// comes from the environment the Helm chart renders (see buildProduction).
func runProduction(ctx context.Context, options serverOptions, stderr io.Writer, deps serverDeps) int {
	build := deps.build
	if build == nil {
		build = func(options serverOptions) (productionBuild, error) {
			return buildProduction(options, os.Getenv, driverkube.InClusterConfig{})
		}
	}
	production, err := build(options)
	if err != nil {
		fmt.Fprintf(stderr, "kodama: %v\n", err)
		return 2
	}
	// A dependency failure may quote a credential, a DSN or a request body in
	// its private diagnostic, so these branches render a fixed message naming
	// only the failed step. Configuration refusals from build above keep their
	// own already-secret-free text.
	app, err := deps.newApp(ctx, production.Config)
	if err != nil {
		fmt.Fprintln(stderr, "kodama: composition failed to start")
		return 1
	}
	listener, err := deps.listen("tcp", options.Address)
	if err != nil {
		_ = app.Close(context.Background())
		fmt.Fprintf(stderr, "kodama: listen %q failed\n", options.Address)
		return 1
	}
	if err := app.Serve(ctx, tls.NewListener(listener, production.TLS)); err != nil {
		fmt.Fprintln(stderr, "kodama: server failed")
		return 1
	}
	return 0
}

// config builds the explicit development Shell composition input.
func (options serverOptions) config() (shellapp.Config, error) {
	if strings.TrimSpace(options.GitStorageRoot) != "" {
		// The development composition has no durable retention store to supply
		// the retained commit set, so keeping its root would be a silent no-op.
		return shellapp.Config{}, fmt.Errorf("%w: the development profile has no durable retained-commit source",
			shellapp.ErrKeepRefsWithoutRetainedCommits)
	}
	if strings.TrimSpace(options.DataDir) == "" {
		return shellapp.Config{}, fmt.Errorf("server requires --data-dir")
	}
	if len(options.DevTokens) == 0 {
		return shellapp.Config{}, fmt.Errorf("server requires at least one --dev-token")
	}
	if options.DevExpiry <= 0 {
		return shellapp.Config{}, fmt.Errorf("server requires a positive --dev-expiry")
	}
	roles, err := parseRoles(options.DevRoles)
	if err != nil {
		return shellapp.Config{}, err
	}
	if _, err := id.ParseTenantID(options.DevTenant); err != nil {
		return shellapp.Config{}, fmt.Errorf("development tenant: %w", err)
	}
	if _, err := id.ParseProjectID(options.DevProject); err != nil {
		return shellapp.Config{}, fmt.Errorf("development project: %w", err)
	}
	if _, err := id.ParsePrincipalID(options.DevPrincipal); err != nil {
		return shellapp.Config{}, fmt.Errorf("development principal: %w", err)
	}
	principal := identity.Principal{
		ID:      id.PrincipalID(options.DevPrincipal),
		Kind:    identity.KindHuman,
		Scope:   tenancy.Scope{TenantID: id.TenantID(options.DevTenant), ProjectID: id.ProjectID(options.DevProject)},
		Roles:   roles,
		Enabled: true,
		Version: 1,
	}
	if err := principal.Validate(); err != nil {
		return shellapp.Config{}, err
	}
	discovery, err := options.discoveryConfig()
	if err != nil {
		return shellapp.Config{}, err
	}
	models, toolProxy, err := options.gatewayConfigurations(shellapp.ProfileDevelopment)
	if err != nil {
		return shellapp.Config{}, err
	}
	expiresAt := time.Now().Add(options.DevExpiry)
	tokens := make([]shellapp.DevToken, len(options.DevTokens))
	for index, token := range options.DevTokens {
		if strings.TrimSpace(token) == "" {
			return shellapp.Config{}, fmt.Errorf("development token %d is empty", index+1)
		}
		tokens[index] = shellapp.DevToken{Credential: token, Principal: principal, ExpiresAt: expiresAt}
	}
	return shellapp.Config{
		Profile: shellapp.ProfileDevelopment,
		Installation: installation.Config{
			Environment: installation.EnvironmentDevelopment,
			State:       installation.StateMemory,
			Messaging:   installation.MessagingMemory,
			Runtime:     installation.RuntimeProcess,
			Keys:        installation.KeyEphemeral,
			PublicURL:   options.PublicURL,
		},
		InstallationID:       options.InstallationID,
		DataDir:              options.DataDir,
		DefaultActionTimeout: 5 * time.Minute,
		LoopInterval:         20 * time.Millisecond,
		MCPPath:              options.MCPPath,
		Discovery:            discovery,
		Models:               models,
		WorkloadURL:          options.WorkloadURL,
		ToolProxy:            toolProxy,
		Dev: &shellapp.DevelopmentConfig{
			Tokens: tokens,
			Bootstrap: &shellapp.Bootstrap{
				TenantID:       id.TenantID(options.DevTenant),
				TenantName:     options.DevTenant,
				ProjectID:      id.ProjectID(options.DevProject),
				ProjectName:    options.DevProject,
				AdminPrincipal: id.PrincipalID(options.DevPrincipal),
				AdminRoles:     []string{string(tenancy.RoleAdmin)},
			},
		},
	}, nil
}

// gatewayConfigurations loads the optional model gateway and external MCP tool
// proxy from the trusted files the settings name. An empty path keeps that
// optional surface disabled; a named file is loaded for the composition
// profile and never defaulted, so an unreadable or malformed file is a startup
// refusal instead of a silently weakened surface.
func (options serverOptions) gatewayConfigurations(profile shellapp.Profile) (*shellapp.ModelsConfig, *shellapp.ToolProxyConfig, error) {
	var models *shellapp.ModelsConfig
	var toolProxy *shellapp.ToolProxyConfig
	if path := strings.TrimSpace(options.ModelsFile); path != "" {
		loaded, err := shellapp.LoadModelsConfigFile(path, profile)
		if err != nil {
			return nil, nil, err
		}
		models = loaded
	}
	if path := strings.TrimSpace(options.ToolProxyFile); path != "" {
		loaded, err := shellapp.LoadToolProxyConfigFile(path)
		if err != nil {
			return nil, nil, err
		}
		toolProxy = loaded
	}
	return models, toolProxy, nil
}

// discoveryConfig validates and maps the optional public discovery settings to
// the Shell composition input. A partially or invalidly configured document is
// a startup configuration refusal and its diagnostic names a member or origin
// rule, never a secret.
func (options serverOptions) discoveryConfig() (*shellapp.DiscoveryConfig, error) {
	if !options.Discovery.configured() {
		return nil, nil
	}
	document := api.DiscoveryDocument{
		Issuer:                      options.Discovery.Issuer,
		ClientID:                    options.Discovery.ClientID,
		Audience:                    options.Discovery.Audience,
		AuthorizationEndpoint:       options.Discovery.AuthorizationEndpoint,
		TokenEndpoint:               options.Discovery.TokenEndpoint,
		DeviceAuthorizationEndpoint: options.Discovery.DeviceAuthorizationEndpoint,
		RevocationEndpoint:          options.Discovery.RevocationEndpoint,
		Scopes:                      append([]string(nil), options.Discovery.Scopes...),
		APIURL:                      options.Discovery.APIURL,
	}
	if err := document.Validate(); err != nil {
		return nil, fmt.Errorf("discovery configuration is invalid: %w", err)
	}
	return &shellapp.DiscoveryConfig{
		Issuer:                      document.Issuer,
		ClientID:                    document.ClientID,
		Audience:                    document.Audience,
		AuthorizationEndpoint:       document.AuthorizationEndpoint,
		TokenEndpoint:               document.TokenEndpoint,
		DeviceAuthorizationEndpoint: document.DeviceAuthorizationEndpoint,
		RevocationEndpoint:          document.RevocationEndpoint,
		Scopes:                      append([]string(nil), document.Scopes...),
		APIURL:                      document.APIURL,
	}, nil
}

func parseRoles(value string) ([]tenancy.Role, error) {
	parts := strings.Split(value, ",")
	roles := make([]tenancy.Role, 0, len(parts))
	for _, part := range parts {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		role := tenancy.Role(name)
		if !role.Valid() {
			return nil, fmt.Errorf("unknown development role %q", name)
		}
		roles = append(roles, role)
	}
	if len(roles) == 0 {
		return nil, fmt.Errorf("development roles are empty")
	}
	return roles, nil
}
