package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/installation"
	shellapp "github.com/optimiware/kodama/internal/shell/app"
	natsdriver "github.com/optimiware/kodama/internal/shell/app/drivers/nats"
	podmandriver "github.com/optimiware/kodama/internal/shell/app/drivers/podman"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/integration/oidc"
	"github.com/optimiware/kodama/internal/shell/integration/registry"
	runtimepodman "github.com/optimiware/kodama/internal/shell/runtime/podman"
)

// testProfileLoopInterval is the durable test profile's default background
// scan cadence. The development default (20ms) is a busy loop against
// PostgreSQL. An explicit --loop-interval or KODAMA_LOOP_INTERVAL overrides it
// within the same bound as production.
const testProfileLoopInterval = time.Second

// hostAliases are the container-runtime names for the host. A workload URL
// naming one of them only works when the server listens on an interface the
// container network forwards to, never on loopback alone.
var hostAliases = map[string]bool{"host.containers.internal": true, "host.docker.internal": true}

// buildTestProfile assembles KODAMA.md's single-host `test` environment from
// the parsed options and the process environment: PostgreSQL and NATS, the
// Podman runtime over an explicit API socket, an existing local keyfile, OIDC
// humans and HTTPS. Every setting is explicit; nothing is auto-detected, no
// key is created and no dependency is dialed here, so a wrong installation is
// an exit-2 refusal whose text names settings, never their values.
func buildTestProfile(options serverOptions, getenv productionEnv) (productionBuild, error) {
	if len(options.DevTokens) > 0 {
		return productionBuild{}, fmt.Errorf("test profile refuses development tokens; humans sign in through OIDC")
	}
	if strings.TrimSpace(options.InstallationID) == "" || options.InstallationID == "installation-dev" {
		return productionBuild{}, fmt.Errorf("test profile requires an explicit installation id (--installation-id or KODAMA_INSTALLATION_ID)")
	}
	if strings.TrimSpace(options.DataDir) == "" || !filepath.IsAbs(options.DataDir) {
		return productionBuild{}, fmt.Errorf("test profile requires an absolute persistent data directory (--data-dir or KODAMA_DATA_DIR)")
	}
	loopInterval, err := resolvedLoopInterval(options.LoopInterval, testProfileLoopInterval)
	if err != nil {
		return productionBuild{}, err
	}
	gitStorageRoot, err := resolveGitStorageRoot(options, getenv)
	if err != nil {
		return productionBuild{}, err
	}
	workloadURL, workloadHost, err := validateWorkloadURL(options.WorkloadURL)
	if err != nil {
		return productionBuild{}, err
	}
	if hostAliases[strings.ToLower(workloadHost)] && loopbackListener(options.Address) {
		return productionBuild{}, fmt.Errorf("the workload URL names the container host alias but --address listens on loopback only; workloads cannot reach it")
	}

	values := map[string]string{}
	for _, name := range []string{
		"KODAMA_DATABASE_URL", "KODAMA_CONTROLLER_DATABASE_URL", "KODAMA_PUBLISHER_DATABASE_URL", "KODAMA_AUTH_DATABASE_URL",
		"KODAMA_NATS_URL", "KODAMA_PODMAN_SOCKET", "KODAMA_TLS_DIR",
		"KODAMA_OIDC_ISSUER", "KODAMA_OIDC_AUDIENCE", "KODAMA_OIDC_JWKS_URL", "KODAMA_OIDC_TENANT_CLAIM",
	} {
		value := strings.TrimSpace(getenv(name))
		if value == "" {
			return productionBuild{}, fmt.Errorf("test profile requires %s", name)
		}
		values[name] = value
	}
	keyRef, keyfilePath, err := testKeySettings(getenv)
	if err != nil {
		return productionBuild{}, err
	}

	installationConfig := installation.Config{
		Environment: installation.EnvironmentTest,
		State:       installation.StatePostgres,
		Messaging:   installation.MessagingNATS,
		Runtime:     installation.RuntimePodman,
		Keys:        installation.KeyKeyfile,
		DatabaseURL: values["KODAMA_DATABASE_URL"],
		NATSURL:     values["KODAMA_NATS_URL"],
		KeyRef:      string(keyRef),
		PublicURL:   options.PublicURL,
	}
	// Validate everything but the database URLs first, so a database refusal
	// below is attributed to its own setting.
	general := installationConfig
	general.DatabaseURL = "postgres://kodama@database.invalid/kodama"
	if err := general.Preflight(); err != nil {
		return productionBuild{}, fmt.Errorf("test profile configuration: %w", err)
	}
	if err := validateTestDatabases(installationConfig, values); err != nil {
		return productionBuild{}, err
	}
	if err := installationConfig.Preflight(); err != nil {
		return productionBuild{}, fmt.Errorf("test profile configuration: %w", err)
	}
	natsTLS, err := testBrokerTLS(getenv)
	if err != nil {
		return productionBuild{}, err
	}
	socket := values["KODAMA_PODMAN_SOCKET"]
	if !filepath.IsAbs(socket) {
		return productionBuild{}, fmt.Errorf("KODAMA_PODMAN_SOCKET must be an absolute path")
	}
	if info, err := os.Stat(socket); err != nil || info.Mode()&os.ModeSocket == 0 {
		return productionBuild{}, fmt.Errorf("KODAMA_PODMAN_SOCKET does not name an existing Unix socket")
	}
	// The server reads an existing key only; `bootstrap-admin --profile test
	// --init-keyfile` is the one explicit way to create it.
	if _, err := loadTestKeys(getenv); err != nil {
		return productionBuild{}, err
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(values["KODAMA_TLS_DIR"], "tls.crt"), filepath.Join(values["KODAMA_TLS_DIR"], "tls.key"))
	if err != nil {
		return productionBuild{}, fmt.Errorf("server certificate in KODAMA_TLS_DIR is unusable")
	}
	oidcClient, err := oidcHTTPClient(getenv("KODAMA_OIDC_CA_FILE"))
	if err != nil {
		return productionBuild{}, err
	}
	discovery, err := options.discoveryConfig()
	if err != nil {
		return productionBuild{}, err
	}
	models, toolProxy, err := options.gatewayConfigurations(shellapp.ProfileTest)
	if err != nil {
		return productionBuild{}, err
	}
	natsAuth, err := natsAuthFromEnv(getenv)
	if err != nil {
		return productionBuild{}, err
	}
	if discovery != nil {
		if discovery.Issuer != "" && discovery.Issuer != values["KODAMA_OIDC_ISSUER"] {
			return productionBuild{}, fmt.Errorf("discovery issuer differs from KODAMA_OIDC_ISSUER")
		}
		if discovery.Audience != "" && discovery.Audience != values["KODAMA_OIDC_AUDIENCE"] {
			return productionBuild{}, fmt.Errorf("discovery audience differs from KODAMA_OIDC_AUDIENCE")
		}
	}

	libpod, err := podmandriver.NewLibpod(podmandriver.LibpodConfig{SocketPath: socket})
	if err != nil {
		return productionBuild{}, fmt.Errorf("podman client: %w", err)
	}
	runtimeRoot := filepath.Join(options.DataDir, "runtime")
	files, err := podmandriver.NewFiles(runtimeRoot)
	if err != nil {
		return productionBuild{}, fmt.Errorf("podman runtime root under the data directory: %w", err)
	}
	adapter, err := runtimepodman.New(runtimepodman.Config{
		Libpod:    libpod,
		Files:     files,
		Root:      runtimeRoot,
		KodamaURL: workloadURL,
		Cleanup:   runtimepodman.CleanupPolicy{Attempts: 3, Backoff: time.Second},
	})
	if err != nil {
		return productionBuild{}, fmt.Errorf("podman runtime: %w", err)
	}

	config := shellapp.Config{
		Profile:              shellapp.ProfileTest,
		Installation:         installationConfig,
		InstallationID:       options.InstallationID,
		DataDir:              options.DataDir,
		MCPPath:              options.MCPPath,
		LoopInterval:         loopInterval,
		NATSAuth:             natsAuth,
		GitStorageRoot:       gitStorageRoot,
		DefaultActionTimeout: shellapp.DefaultActionTimeout,
		Discovery:            discovery,
		Models:               models,
		ToolProxy:            toolProxy,
		Test: &shellapp.TestConfig{
			Runtime:           adapter,
			Workspace:         adapter,
			VolumeProvisioner: libpod,
			RuntimeReady:      libpod.Ping,
			RuntimeCloser:     libpod.Close,
			KeyfilePath:       keyfilePath,
			OIDC: oidc.Config{
				OIDCConfig:    identity.OIDCConfig{Issuer: values["KODAMA_OIDC_ISSUER"], Audience: values["KODAMA_OIDC_AUDIENCE"], ClockSkew: time.Minute},
				JWKSURL:       values["KODAMA_OIDC_JWKS_URL"],
				Algorithms:    []string{oidc.AlgorithmRS256, oidc.AlgorithmES256},
				HTTPClient:    oidcClient,
				MaxJWKSBytes:  1 << 20,
				MaxTokenBytes: 1 << 20,
				CacheTTL:      10 * time.Minute,
				SubjectClaim:  strings.TrimSpace(getenv("KODAMA_OIDC_SUBJECT_CLAIM")),
				TenantClaim:   values["KODAMA_OIDC_TENANT_CLAIM"],
				ProjectClaim:  strings.TrimSpace(getenv("KODAMA_OIDC_PROJECT_CLAIM")),
			},
			Registry:      registry.Config{Transport: http.DefaultTransport},
			NATSTLS:       natsTLS,
			ControllerURL: values["KODAMA_CONTROLLER_DATABASE_URL"],
			PublisherURL:  values["KODAMA_PUBLISHER_DATABASE_URL"],
			AuthURL:       values["KODAMA_AUTH_DATABASE_URL"],
		},
	}
	return productionBuild{Config: config, TLS: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}, nil
}

// validateWorkloadURL checks the explicit base URL workloads reach the server
// through. It is HTTPS with one named host, an optional valid port and no
// userinfo, query, fragment or path, and it never names a loopback or
// unspecified destination: inside a container those are the container itself.
// The refusal never renders the value, which could carry a credential.
func validateWorkloadURL(raw string) (string, string, error) {
	refuse := func(rule string) (string, string, error) {
		return "", "", fmt.Errorf("test profile workload URL (--workload-url or KODAMA_WORKLOAD_URL) %s", rule)
	}
	value := strings.TrimSpace(raw)
	if value == "" {
		return refuse("is required")
	}
	if strings.ContainsAny(value, "#?@") {
		return refuse("must not carry userinfo, a query or a fragment")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Opaque != "" || parsed.User != nil {
		return refuse("must be an explicit https:// URL")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return refuse("must be a base URL without a path")
	}
	host := parsed.Hostname()
	if host == "" {
		return refuse("must name a host")
	}
	if strings.HasSuffix(parsed.Host, ":") {
		return refuse("has an empty port")
	}
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return refuse("has an invalid port")
		}
	}
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") {
		return refuse("must not name a loopback destination")
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
		return refuse("must not name a loopback or unspecified destination")
	}
	return strings.TrimSuffix(value, "/"), host, nil
}

// loopbackListener reports whether a listen address binds loopback only.
func loopbackListener(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// validateTestDatabases applies the test endpoint rule to every role URL and
// requires the four roles to target the same server and database. Refusals
// name the setting only.
func validateTestDatabases(base installation.Config, values map[string]string) error {
	runtimeURL, err := url.Parse(values["KODAMA_DATABASE_URL"])
	if err != nil {
		return fmt.Errorf("KODAMA_DATABASE_URL is not a valid postgres:// URL")
	}
	for _, name := range []string{"KODAMA_DATABASE_URL", "KODAMA_CONTROLLER_DATABASE_URL", "KODAMA_PUBLISHER_DATABASE_URL", "KODAMA_AUTH_DATABASE_URL"} {
		probe := base
		probe.DatabaseURL = values[name]
		if err := probe.Preflight(); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		parsed, err := url.Parse(values[name])
		if err != nil {
			return fmt.Errorf("%s is not a valid postgres:// URL", name)
		}
		if parsed.Host != runtimeURL.Host || strings.Trim(parsed.Path, "/") != strings.Trim(runtimeURL.Path, "/") {
			return fmt.Errorf("%s must target the same database server and database as KODAMA_DATABASE_URL", name)
		}
	}
	return nil
}

// testBrokerTLS reads the optional broker trust material once. A CA bundle
// alone verifies the broker; a client certificate requires its key and vice
// versa. Messages never render file content.
func testBrokerTLS(getenv productionEnv) (*natsdriver.TLSConfig, error) {
	caFile := strings.TrimSpace(getenv("KODAMA_NATS_CA_FILE"))
	certFile := strings.TrimSpace(getenv("KODAMA_NATS_CERT_FILE"))
	keyFile := strings.TrimSpace(getenv("KODAMA_NATS_KEY_FILE"))
	if caFile == "" && certFile == "" && keyFile == "" {
		return nil, nil
	}
	if (certFile == "") != (keyFile == "") {
		return nil, fmt.Errorf("broker TLS needs both KODAMA_NATS_CERT_FILE and KODAMA_NATS_KEY_FILE")
	}
	if caFile != "" {
		caPEM, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("broker TLS certificate authority is unreadable")
		}
		if !x509.NewCertPool().AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("broker TLS certificate authority is not a usable certificate")
		}
	}
	if certFile != "" {
		if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
			return nil, fmt.Errorf("broker TLS client certificate is unusable")
		}
	}
	return &natsdriver.TLSConfig{CAFile: caFile, CertFile: certFile, KeyFile: keyFile}, nil
}

// runTestProfile serves the durable test composition over TLS with the same
// drain, exit-code and fixed-diagnostic behavior as production.
func runTestProfile(ctx context.Context, options serverOptions, stderr io.Writer, deps serverDeps) int {
	build := deps.buildTest
	if build == nil {
		build = func(options serverOptions) (productionBuild, error) {
			return buildTestProfile(options, os.Getenv)
		}
	}
	built, err := build(options)
	if err != nil {
		fmt.Fprintf(stderr, "kodama: %v\n", err)
		return 2
	}
	fmt.Fprintln(stderr, "kodama: test profile: single host, local keyfile; not a production installation")
	// A dependency failure may quote a credential or a DSN in its private
	// diagnostic, so these branches render fixed text naming only the step.
	app, err := deps.newApp(ctx, built.Config)
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
	if err := app.Serve(ctx, tls.NewListener(listener, built.TLS)); err != nil {
		fmt.Fprintln(stderr, "kodama: server failed")
		return 1
	}
	return 0
}
