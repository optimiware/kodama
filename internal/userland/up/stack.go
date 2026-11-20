package up

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/optimiware/kodema/internal/id"
)

const (
	// LabelInstance tags every container, network and volume owned by kodema up.
	LabelInstance = "kodema.up.instance"
	// LabelComponent tags each container's logical role.
	LabelComponent = "kodema.up.component"

	DefaultTenantID  = "tenant-local"
	DefaultProjectID = "project-local"
	// KodemaImageRepository holds the released images, tagged by version.
	KodemaImageRepository = "ghcr.io/optimiware/kodema/kodema"
	DefaultPostgresImage  = "docker.io/library/postgres:18-alpine@sha256:300ebd02c6d4de9ee27ad7f6855d5d21b4d3a4a5117767c1197d284c635d9736"
	DefaultNATSImage      = "docker.io/library/nats:2.15-alpine@sha256:89c5c61b5969261e05106a98e465ccb85c7f4bf4e94c1e70786c189759d79e7e"
)

// StackDescriptor persists the non-secret runtime metadata of one `kodema up`
// instance in <state-dir>/stack.json (mode 0600).
type StackDescriptor struct {
	Instance       string          `json:"instance"`
	StateDir       string          `json:"state_dir"`
	ListenAddr     string          `json:"listen_addr"`
	PublicURL      string          `json:"public_url"`
	TenantID       string          `json:"tenant_id"`
	ProjectID      string          `json:"project_id"`
	Runtime        DetectedRuntime `json:"runtime"`
	CACertPath     string          `json:"ca_cert_path"`
	ServerUpstream string          `json:"server_upstream"`
	GitUpstream    string          `json:"git_upstream"`
	GatewayPID     int             `json:"gateway_pid,omitempty"`
}

// UpConfig configures `kodema up`.
type UpConfig struct {
	Instance              string
	StateDir              string
	ListenAddr            string
	Socket                string
	AllowUnsharedStateDir bool
	TenantID              string
	ProjectID             string
	KodemaImage           string
	PostgresImage         string
	NATSImage             string
	Foreground            bool
	// DenyProjectSecrets disables the local workload secret-name allow-list so
	// the server runs deny-by-default. By default (false) and when AllowSecrets
	// is empty, kodema up allows project-stored workload secrets for
	// (TenantID, ProjectID) while still rejecting KODEMA_TOKEN, missing secrets
	// and other scopes.
	DenyProjectSecrets bool
	// AllowSecrets restricts the local workload secret-name allow-list to these
	// exact secret names in (TenantID, ProjectID) instead of allowing all
	// project-stored secrets.
	AllowSecrets []string
	// Stderr receives concise startup progress lines (defaults to io.Discard).
	Stderr io.Writer

	// Optional test seams.
	GOOS         string
	HomeDir      string
	DetectOpts   *DetectOptions
	EngineHTTP   *http.Client
	SpawnGateway func(stateDir string) (int, error)
	Provision    func(context.Context, provisionConfig) (string, error)
}

// DownConfig configures `kodema down`.
type DownConfig struct {
	Instance              string
	StateDir              string
	Socket                string
	AllowUnsharedStateDir bool
	Purge                 bool

	GOOS       string
	HomeDir    string
	DetectOpts *DetectOptions
	EngineHTTP *http.Client
}

// StatusConfig configures `kodema status`.
type StatusConfig struct {
	Instance              string
	StateDir              string
	Socket                string
	AllowUnsharedStateDir bool

	GOOS       string
	HomeDir    string
	DetectOpts *DetectOptions
	EngineHTTP *http.Client
}

// StatusReport summarizes the observed health of a `kodema up` instance.
type StatusReport struct {
	Instance     string                   `json:"instance"`
	StateDir     string                   `json:"state_dir"`
	PublicURL    string                   `json:"public_url"`
	Runtime      DetectedRuntime          `json:"runtime"`
	GatewayAlive bool                     `json:"gateway_alive"`
	GatewayPID   int                      `json:"gateway_pid,omitempty"`
	Ready        bool                     `json:"ready"`
	Containers   map[string]ContainerInfo `json:"containers"`
}

// Healthy reports whether all managed containers, the TLS gateway and /readyz are up.
func (r StatusReport) Healthy() bool {
	if !r.GatewayAlive || !r.Ready {
		return false
	}
	for _, component := range []string{"postgres", "nats", "server", "git"} {
		info, ok := r.Containers[component]
		if !ok || !info.Running {
			return false
		}
	}
	return true
}

// kodemaContainerUser runs the Kodema containers as the container's root. The
// state directory is bind-mounted 0700 from the host; under a rootless engine
// container root maps to the invoking host user, which is the only user that
// can read it. The image's own user (65532) cannot.
const kodemaContainerUser = "0:0"

// hostMountSecurityOpt is set on every container that mounts a path of the
// host: files of the state directory, or the engine's socket. On a host with
// SELinux (Fedora, RHEL, a Podman machine) a confined container may read
// neither, and the stack does not start. The alternative, relabeling the state
// directory, is refused by file systems that keep no labels, such as the one a
// Podman machine shares with macOS. Without SELinux the option changes nothing.
var hostMountSecurityOpt = []string{"label=disable"}

// EnvFileSpec describes the operator shell environment written to <state-dir>/env.sh.
type EnvFileSpec struct {
	EnvFilePath    string
	AdminTokenPath string
	PublicURL      string
	CACertPath     string
	AdminToken     string
	TenantID       string
	ProjectID      string
	// TenantAdminTokenPath and TenantAdminToken are optional: a second admin
	// credential scoped to the tenant, for membership and project administration.
	TenantAdminTokenPath string
	TenantAdminToken     string
}

// WriteEnvFiles writes <state-dir>/admin-token and <state-dir>/env.sh with 0600 permissions.
func WriteEnvFiles(spec EnvFileSpec) error {
	if spec.AdminTokenPath != "" {
		if err := writeFileAtomic(spec.AdminTokenPath, []byte(spec.AdminToken+"\n"), 0o600); err != nil {
			return fmt.Errorf("write admin-token: %w", err)
		}
	}
	if spec.TenantAdminTokenPath != "" {
		if err := writeFileAtomic(spec.TenantAdminTokenPath, []byte(spec.TenantAdminToken+"\n"), 0o600); err != nil {
			return fmt.Errorf("write tenant-admin-token: %w", err)
		}
	}
	if spec.EnvFilePath != "" {
		var buf bytes.Buffer
		buf.WriteString("# Generated by Kodema; source this file (do not commit it).\n")
		fmt.Fprintf(&buf, "export KODEMA_URL=%s\n", shellSingleQuote(spec.PublicURL))
		fmt.Fprintf(&buf, "export KODEMA_SERVER=%s\n", shellSingleQuote(spec.PublicURL))
		fmt.Fprintf(&buf, "export KODEMA_CA_FILE=%s\n", shellSingleQuote(spec.CACertPath))
		if spec.AdminTokenPath != "" {
			fmt.Fprintf(&buf, "export KODEMA_CREDENTIAL_FILE=%s\n", shellSingleQuote(spec.AdminTokenPath))
		}
		fmt.Fprintf(&buf, "export KODEMA_TOKEN=%s\n", shellSingleQuote(spec.AdminToken))
		fmt.Fprintf(&buf, "export KODEMA_TENANT=%s\n", shellSingleQuote(spec.TenantID))
		fmt.Fprintf(&buf, "export KODEMA_PROJECT=%s\n", shellSingleQuote(spec.ProjectID))
		if spec.TenantAdminTokenPath != "" {
			buf.WriteString("# The default credential is scoped to the project. Tenant administration\n")
			buf.WriteString("# (kodema member ...) needs the tenant-scoped one:\n")
			buf.WriteString("#   kodema --credential-file \"$KODEMA_TENANT_ADMIN_CREDENTIAL_FILE\" member list\n")
			fmt.Fprintf(&buf, "export KODEMA_TENANT_ADMIN_CREDENTIAL_FILE=%s\n", shellSingleQuote(spec.TenantAdminTokenPath))
		}
		if err := writeFileAtomic(spec.EnvFilePath, buf.Bytes(), 0o600); err != nil {
			return fmt.Errorf("write env.sh: %w", err)
		}
	}
	return nil
}

func shellSingleQuote(val string) string {
	return "'" + strings.ReplaceAll(val, "'", `'\''`) + "'"
}

// DefaultListenAddr is where the gateway listens. On Linux, containers reach
// the host on its outward address, not on loopback, so the gateway listens on
// every address there and refuses peers that are not this machine.
func DefaultListenAddr() string {
	if runtime.GOOS == "linux" {
		return "0.0.0.0:8443"
	}
	return "127.0.0.1:8443"
}

// PublicOriginFromListen turns a listen address such as "127.0.0.1:8443" or ":8443"
// into its client-facing https:// origin.
func PublicOriginFromListen(listenAddr string) (string, string, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(listenAddr))
	if err != nil {
		return "", "", fmt.Errorf("invalid listen address %q: %w", listenAddr, err)
	}
	portNum, err := strconv.Atoi(port)
	if err != nil || portNum < 0 || portNum > 65535 {
		return "", "", fmt.Errorf("invalid listen port %q", port)
	}
	publicHost := strings.Trim(host, "[]")
	if publicHost == "" || publicHost == "0.0.0.0" || publicHost == "::" {
		publicHost = "127.0.0.1"
	}
	normalizedBind := net.JoinHostPort(publicHost, port)
	if host != "" {
		normalizedBind = net.JoinHostPort(strings.Trim(host, "[]"), port)
	}
	return normalizedBind, "https://" + net.JoinHostPort(publicHost, port), nil
}

// Up provisions or reconciles the container-backed Kodema stack and starts the
// single-origin HTTPS reverse-proxy gateway on cfg.ListenAddr without socat.
//
// When it fails after it started the gateway, it stops that gateway: the
// listen port is free again and `kodema status` does not report a gateway in
// front of a stack that did not come up. The containers stay for their logs.
func Up(ctx context.Context, cfg UpConfig, stdout io.Writer) (_ StackDescriptor, err error) {
	if stdout == nil {
		stdout = io.Discard
	}
	stderr := cfg.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	instance := defaultIfEmpty(cfg.Instance, DefaultInstanceName)
	listenRaw := defaultIfEmpty(cfg.ListenAddr, DefaultListenAddr())
	listenAddr, publicURL, err := PublicOriginFromListen(listenRaw)
	if err != nil {
		return StackDescriptor{}, err
	}
	tenantID := defaultIfEmpty(cfg.TenantID, DefaultTenantID)
	projectID := defaultIfEmpty(cfg.ProjectID, DefaultProjectID)
	kodemaImage := defaultIfEmpty(cfg.KodemaImage, KodemaImageRepository+":latest")
	postgresImage := defaultIfEmpty(cfg.PostgresImage, DefaultPostgresImage)
	natsImage := defaultIfEmpty(cfg.NATSImage, DefaultNATSImage)
	if cfg.DenyProjectSecrets && len(cfg.AllowSecrets) > 0 {
		return StackDescriptor{}, errors.New("cannot combine --allow-project-secrets=false with --allow-secret")
	}
	allowSecrets, err := validateAllowSecrets(cfg.AllowSecrets)
	if err != nil {
		return StackDescriptor{}, err
	}

	stateDir, err := ResolveStateDir(StateDirOptions{
		GOOS:                  cfg.GOOS,
		HomeDir:               cfg.HomeDir,
		InstanceName:          instance,
		ExplicitStateDir:      cfg.StateDir,
		AllowUnsharedStateDir: cfg.AllowUnsharedStateDir,
	})
	if err != nil {
		return StackDescriptor{}, err
	}

	detectOpts := buildDetectOpts(cfg.DetectOpts, cfg.Socket, cfg.GOOS, cfg.HomeDir, stateDir)
	detected, err := DetectRuntime(ctx, detectOpts)
	if err != nil {
		return StackDescriptor{}, err
	}

	if detected.Kind != KindPodman {
		return StackDescriptor{}, fmt.Errorf("kodema up needs Podman: Kodema runs workloads through the Podman API, which %s (%s) does not serve", detected.Provider, detected.SocketPath)
	}
	fmt.Fprintf(stderr, "using %s (%s)\n", detected.Kind, detected.Provider)

	if err := EnsureInstanceStateDir(stateDir, instance); err != nil {
		return StackDescriptor{}, err
	}
	for _, sub := range []string{"tls", "secrets", "data", "git/repos", "git/journal", "logs", "run"} {
		dir := filepath.Join(stateDir, filepath.FromSlash(sub))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return StackDescriptor{}, err
		}
		_ = os.Chmod(dir, 0o700)
	}

	// Stop any previous gateway process on this instance before rebinding :8443.
	stopExistingGateway(stateDir)

	prefix := "kodema-" + instance
	serverContainer := prefix + "-server"
	postgresContainer := prefix + "-postgres"
	natsContainer := prefix + "-nats"
	gitContainer := prefix + "-git"
	networkName := prefix + "-net"
	pgVolume := prefix + "-pgdata"
	natsVolume := prefix + "-natsdata"

	tlsBundle, err := EnsureTLS(TLSConfig{
		Dir:   filepath.Join(stateDir, "tls"),
		Hosts: []string{listenAddr, serverContainer, gitContainer},
	})
	if err != nil {
		return StackDescriptor{}, err
	}

	devIssuer, err := NewDevIssuer(DevIssuerConfig{
		Issuer:         publicURL + "/auth",
		Audience:       publicURL,
		APIURL:         publicURL,
		ClientID:       defaultOIDCClientID,
		KeyPath:        filepath.Join(stateDir, "oidc-signing.key"),
		TenantID:       tenantID,
		ProjectID:      projectID,
		AccessTokenTTL: 24 * time.Hour,
	})
	if err != nil {
		return StackDescriptor{}, err
	}
	// Project operations need a project-scoped actor and tenant operations a
	// tenant-scoped one, so the administrator gets a token for each.
	adminToken, err := devIssuer.MintAccessToken("admin", 24*time.Hour)
	if err != nil {
		return StackDescriptor{}, err
	}
	tenantAdminToken, err := devIssuer.MintTenantAdminToken(24 * time.Hour)
	if err != nil {
		return StackDescriptor{}, err
	}

	engine, err := NewEngineClient(detected.SocketPath, cfg.EngineHTTP)
	if err != nil {
		return StackDescriptor{}, err
	}

	baseLabels := map[string]string{LabelInstance: instance}
	if err := engine.EnsureNetwork(ctx, networkName, baseLabels); err != nil {
		return StackDescriptor{}, err
	}
	if err := engine.EnsureVolume(ctx, pgVolume, instance, baseLabels); err != nil {
		return StackDescriptor{}, err
	}
	if err := engine.EnsureVolume(ctx, natsVolume, instance, baseLabels); err != nil {
		return StackDescriptor{}, err
	}

	fmt.Fprintln(stderr, "checking container images...")
	for _, img := range []string{postgresImage, natsImage} {
		if err := engine.EnsureImageWithProgress(ctx, img, stderr); err != nil {
			return StackDescriptor{}, err
		}
	}
	if err := engine.EnsureImageWithProgress(ctx, kodemaImage, stderr); err != nil {
		return StackDescriptor{}, fmt.Errorf("%w; from a checkout, build it with `make image`, or name another with --image", err)
	}

	secretsDir := filepath.Join(stateDir, "secrets")
	pgSuperPass, err := ensureSecretHex(filepath.Join(secretsDir, "postgres-superuser"), 24)
	if err != nil {
		return StackDescriptor{}, err
	}
	dbAdminPass, err := ensureSecretHex(filepath.Join(secretsDir, "kodema-admin"), 24)
	if err != nil {
		return StackDescriptor{}, err
	}
	runtimePass, err := ensureSecretHex(filepath.Join(secretsDir, "runtime-pass"), 24)
	if err != nil {
		return StackDescriptor{}, err
	}
	controllerPass, err := ensureSecretHex(filepath.Join(secretsDir, "controller-pass"), 24)
	if err != nil {
		return StackDescriptor{}, err
	}
	publisherPass, err := ensureSecretHex(filepath.Join(secretsDir, "publisher-pass"), 24)
	if err != nil {
		return StackDescriptor{}, err
	}
	authPass, err := ensureSecretHex(filepath.Join(secretsDir, "auth-pass"), 24)
	if err != nil {
		return StackDescriptor{}, err
	}

	initSQLPath := filepath.Join(secretsDir, "init-admin.sql")
	initSQL := fmt.Sprintf(
		"DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'kodema_admin') THEN CREATE ROLE kodema_admin LOGIN CREATEROLE CREATEDB NOSUPERUSER NOBYPASSRLS; END IF; END $$;\nALTER ROLE kodema_admin WITH PASSWORD '%s';\n",
		dbAdminPass,
	)
	if err := writeFileAtomic(initSQLPath, []byte(initSQL), 0o600); err != nil {
		return StackDescriptor{}, err
	}

	fmt.Fprintf(stderr, "starting postgres (%s)...\n", postgresContainer)
	_ = engine.RemoveContainer(ctx, postgresContainer)
	if _, err := engine.StartDetached(ctx, ContainerSpec{
		Name:    postgresContainer,
		Image:   postgresImage,
		Env:     []string{"POSTGRES_PASSWORD=" + pgSuperPass},
		Labels:  map[string]string{LabelInstance: instance, LabelComponent: "postgres"},
		Network: networkName,
		Mounts: []MountBind{
			{Source: pgVolume, Target: "/var/lib/postgresql"},
			{Source: initSQLPath, Target: "/tmp/kodema-init.sql", ReadOnly: true},
		},
		SecurityOpt: hostMountSecurityOpt,
	}); err != nil {
		return StackDescriptor{}, err
	}
	fmt.Fprintln(stderr, "waiting for postgres readiness...")
	if err := waitPostgresReady(ctx, engine, postgresContainer); err != nil {
		return StackDescriptor{}, err
	}
	if code, err := engine.Exec(ctx, postgresContainer, []string{"psql", "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", "postgres", "-f", "/tmp/kodema-init.sql"}); err != nil {
		return StackDescriptor{}, fmt.Errorf("initialize kodema_admin role: %w", err)
	} else if code != 0 {
		return StackDescriptor{}, fmt.Errorf("initialize kodema_admin role: psql exited %d", code)
	}

	fmt.Fprintf(stderr, "starting nats (%s)...\n", natsContainer)
	_ = engine.RemoveContainer(ctx, natsContainer)
	if _, err := engine.StartDetached(ctx, ContainerSpec{
		Name:    natsContainer,
		Image:   natsImage,
		Cmd:     []string{"-js", "-sd", "/data"},
		Labels:  map[string]string{LabelInstance: instance, LabelComponent: "nats"},
		Network: networkName,
		Mounts:  []MountBind{{Source: natsVolume, Target: "/data"}},
	}); err != nil {
		return StackDescriptor{}, err
	}

	dbURL := fmt.Sprintf("postgres://kodema_runtime:%s@%s:5432/kodema?sslmode=disable", runtimePass, postgresContainer)
	ctrlURL := fmt.Sprintf("postgres://kodema_controller:%s@%s:5432/kodema?sslmode=disable", controllerPass, postgresContainer)
	pubURL := fmt.Sprintf("postgres://kodema_publisher:%s@%s:5432/kodema?sslmode=disable", publisherPass, postgresContainer)
	authDBURL := fmt.Sprintf("postgres://kodema_auth:%s@%s:5432/kodema?sslmode=disable", authPass, postgresContainer)
	adminDBURL := fmt.Sprintf("postgres://kodema_admin:%s@%s:5432/postgres?sslmode=disable", dbAdminPass, postgresContainer)

	fmt.Fprintln(stderr, "running database migrations...")
	if _, err := engine.RunOneShot(ctx, ContainerSpec{
		Name:    prefix + "-migrate",
		Image:   kodemaImage,
		User:    kodemaContainerUser,
		Cmd:     []string{"migrate"},
		Network: networkName,
		Labels:  map[string]string{LabelInstance: instance, LabelComponent: "migrate"},
		Env: []string{
			"KODEMA_PROFILE=test",
			"KODEMA_ADMIN_DATABASE_URL=" + adminDBURL,
			"KODEMA_DATABASE_URL=" + dbURL,
			"KODEMA_CONTROLLER_DATABASE_URL=" + ctrlURL,
			"KODEMA_PUBLISHER_DATABASE_URL=" + pubURL,
			"KODEMA_AUTH_DATABASE_URL=" + authDBURL,
		},
	}); err != nil {
		return StackDescriptor{}, fmt.Errorf("database migration: %w", err)
	}

	_, publicPort, _ := net.SplitHostPort(listenAddr)
	workloadOrigin := "https://" + net.JoinHostPort(detected.HostAlias, publicPort)
	disc := devIssuer.DiscoveryDocument()
	containerSocket := containerSocketMountSource(cfg.GOOS, detected, engine.RemoteSocketPath(ctx))
	keyfilePath := filepath.Join(secretsDir, "keys", "kek-v1.key")
	if err := os.MkdirAll(filepath.Dir(keyfilePath), 0o700); err != nil {
		return StackDescriptor{}, err
	}

	tenantAdminTokenPath := filepath.Join(stateDir, "tenant-admin-token")
	envFilePath := filepath.Join(stateDir, "env.sh")
	if err := WriteEnvFiles(EnvFileSpec{
		EnvFilePath:          envFilePath,
		AdminTokenPath:       filepath.Join(stateDir, "admin-token"),
		PublicURL:            publicURL,
		CACertPath:           tlsBundle.CACertPath,
		AdminToken:           adminToken,
		TenantID:             tenantID,
		ProjectID:            projectID,
		TenantAdminTokenPath: tenantAdminTokenPath,
		TenantAdminToken:     tenantAdminToken,
	}); err != nil {
		return StackDescriptor{}, err
	}

	serverPort, err := allocateLoopbackPort()
	if err != nil {
		return StackDescriptor{}, err
	}
	gitPort, err := allocateLoopbackPort()
	if err != nil {
		return StackDescriptor{}, err
	}

	desc := StackDescriptor{
		Instance:       instance,
		StateDir:       stateDir,
		ListenAddr:     listenAddr,
		PublicURL:      publicURL,
		TenantID:       tenantID,
		ProjectID:      projectID,
		Runtime:        detected,
		CACertPath:     tlsBundle.CACertPath,
		ServerUpstream: fmt.Sprintf("https://127.0.0.1:%d", serverPort),
		GitUpstream:    fmt.Sprintf("http://127.0.0.1:%d", gitPort),
	}
	if err := saveStackDescriptor(stateDir, desc); err != nil {
		return StackDescriptor{}, err
	}

	fmt.Fprintf(stderr, "starting TLS gateway on %s...\n", listenAddr)
	stopTempGateway := func() {}
	defer func() { stopTempGateway() }()
	if cfg.Foreground {
		// Serve /auth/jwks.json during bootstrap-admin, then hand the port to
		// the foreground gateway once containers are up.
		ln, listenErr := net.Listen("tcp", listenAddr)
		if listenErr != nil {
			return StackDescriptor{}, fmt.Errorf("listen %q: %w", listenAddr, listenErr)
		}
		tlsCfg, tlsErr := tlsBundle.ServerTLSConfig()
		if tlsErr != nil {
			_ = ln.Close()
			return StackDescriptor{}, tlsErr
		}
		gwCtx, gwCancel := context.WithCancel(ctx)
		gwDone := make(chan struct{})
		pool, _ := tlsBundle.CertPool()
		gw, gwErr := NewGateway(GatewayConfig{
			ServerUpstream:  desc.ServerUpstream,
			AuthHandler:     devIssuer,
			GitUpstream:     desc.GitUpstream,
			UpstreamRootCAs: pool,
		})
		if gwErr != nil {
			_ = ln.Close()
			gwCancel()
			return StackDescriptor{}, gwErr
		}
		go func() {
			defer close(gwDone)
			_ = gw.ServeTLS(gwCtx, ln, tlsCfg)
		}()
		stopTempGateway = func() {
			gwCancel()
			<-gwDone
		}
	} else {
		spawner := cfg.SpawnGateway
		if spawner == nil {
			spawner = defaultSpawnGatewayDaemon
		}
		pid, spawnErr := spawner(stateDir)
		if spawnErr != nil {
			return StackDescriptor{}, fmt.Errorf("start single-origin TLS gateway: %w", spawnErr)
		}
		defer func() {
			if err != nil {
				stopExistingGateway(stateDir)
			}
		}()
		desc.GatewayPID = pid
		if err := saveStackDescriptor(stateDir, desc); err != nil {
			return StackDescriptor{}, err
		}
		_ = writeFileAtomic(filepath.Join(stateDir, "gateway.pid"), []byte(strconv.Itoa(pid)+"\n"), 0o600)
		// bootstrap-admin reads /auth/jwks.json from the gateway just spawned.
		for i := 0; i < 50 && !probeReadyz(ctx, publicURL+"/auth/healthz", tlsBundle.CACertPath); i++ {
			time.Sleep(100 * time.Millisecond)
		}
	}

	bootstrapMarker := filepath.Join(stateDir, "bootstrap.done")
	if _, statErr := os.Stat(bootstrapMarker); os.IsNotExist(statErr) {
		fmt.Fprintf(stderr, "bootstrapping tenant %s...\n", tenantID)
		bootstrapCmd := []string{
			"bootstrap-admin",
			"--profile", "test",
			"--tenant", tenantID,
			"--tenant-name", tenantID,
			"--token-file", tenantAdminTokenPath,
		}
		if _, kfErr := os.Stat(keyfilePath); os.IsNotExist(kfErr) {
			bootstrapCmd = append(bootstrapCmd, "--init-keyfile")
		}
		if _, err := engine.RunOneShot(ctx, ContainerSpec{
			Name:        prefix + "-bootstrap",
			Image:       kodemaImage,
			User:        kodemaContainerUser,
			Cmd:         bootstrapCmd,
			Network:     networkName,
			Labels:      map[string]string{LabelInstance: instance, LabelComponent: "bootstrap"},
			Mounts:      []MountBind{{Source: stateDir, Target: stateDir}},
			SecurityOpt: hostMountSecurityOpt,
			Env: []string{
				"KODEMA_PROFILE=test",
				"KODEMA_INSTALLATION_ID=installation-" + instance,
				"KODEMA_DATABASE_URL=" + dbURL,
				"KODEMA_KEY_ADAPTER=keyfile",
				"KODEMA_KEY_REF=keyfile:kek-v1",
				"KODEMA_KEYFILE=" + keyfilePath,
				"KODEMA_OIDC_ISSUER=" + disc.Issuer,
				"KODEMA_OIDC_AUDIENCE=" + disc.Audience,
				"KODEMA_OIDC_JWKS_URL=" + workloadOrigin + "/auth/jwks.json",
				"KODEMA_OIDC_CA_FILE=" + tlsBundle.CACertPath,
				"KODEMA_OIDC_TENANT_CLAIM=org_id",
			},
		}); err != nil {
			return StackDescriptor{}, fmt.Errorf("bootstrap-admin: %w", err)
		}
		_ = writeFileAtomic(bootstrapMarker, []byte("bootstrapped\n"), 0o600)
	}

	secretRulesPath, err := reconcileSecretRulesFile(stateDir, tenantID, projectID, cfg.DenyProjectSecrets, allowSecrets)
	if err != nil {
		return StackDescriptor{}, err
	}

	// Start the server and git companion on dedicated 127.0.0.1 loopback ports;
	// only the single-origin TLS gateway binds the public listen port (:8443).
	serverEnv := []string{
		"KODEMA_PROFILE=test",
		"KODEMA_INSTALLATION_ID=installation-" + instance,
		"KODEMA_DATA_DIR=" + filepath.Join(stateDir, "data"),
		"KODEMA_DATABASE_URL=" + dbURL,
		"KODEMA_CONTROLLER_DATABASE_URL=" + ctrlURL,
		"KODEMA_PUBLISHER_DATABASE_URL=" + pubURL,
		"KODEMA_AUTH_DATABASE_URL=" + authDBURL,
		"KODEMA_NATS_URL=nats://" + natsContainer + ":4222",
		"KODEMA_PODMAN_SOCKET=/var/run/kodema-container.sock",
		"KODEMA_KEY_ADAPTER=keyfile",
		"KODEMA_KEY_REF=keyfile:kek-v1",
		"KODEMA_KEYFILE=" + keyfilePath,
		"KODEMA_TLS_DIR=" + filepath.Join(stateDir, "tls"),
		"KODEMA_OIDC_ISSUER=" + disc.Issuer,
		"KODEMA_OIDC_AUDIENCE=" + disc.Audience,
		"KODEMA_OIDC_JWKS_URL=" + workloadOrigin + "/auth/jwks.json",
		"KODEMA_OIDC_CA_FILE=" + tlsBundle.CACertPath,
		"KODEMA_OIDC_TENANT_CLAIM=org_id",
		"KODEMA_OIDC_PROJECT_CLAIM=project_id",
		"KODEMA_PUBLIC_URL=" + publicURL,
		"KODEMA_WORKLOAD_URL=" + workloadOrigin,
		// The gateway serves Git under /git/ of the same origin.
		"KODEMA_WORKLOAD_GIT_URL=" + workloadOrigin + "/git",
		"KODEMA_ADDRESS=0.0.0.0:8443",
		"KODEMA_DISCOVERY_ISSUER=" + disc.Issuer,
		"KODEMA_DISCOVERY_CLIENT_ID=" + disc.ClientID,
		"KODEMA_DISCOVERY_AUDIENCE=" + disc.Audience,
		"KODEMA_DISCOVERY_AUTHORIZATION_ENDPOINT=" + disc.AuthorizationEndpoint,
		"KODEMA_DISCOVERY_TOKEN_ENDPOINT=" + disc.TokenEndpoint,
		"KODEMA_DISCOVERY_DEVICE_AUTHORIZATION_ENDPOINT=" + disc.DeviceAuthorizationEndpoint,
		"KODEMA_DISCOVERY_REVOCATION_ENDPOINT=" + disc.RevocationEndpoint,
		"KODEMA_DISCOVERY_SCOPES=" + strings.Join(disc.Scopes, ","),
		"KODEMA_DISCOVERY_API_URL=" + disc.APIURL,
	}
	if secretRulesPath != "" {
		serverEnv = append(serverEnv, "KODEMA_SECRET_RULES_FILE="+secretRulesPath)
	}

	fmt.Fprintf(stderr, "starting kodema server (%s)...\n", serverContainer)
	gitDir := filepath.Join(stateDir, "git")
	_ = engine.RemoveContainer(ctx, serverContainer)
	if _, err := engine.StartDetached(ctx, ContainerSpec{
		Name:    serverContainer,
		Image:   kodemaImage,
		User:    kodemaContainerUser,
		Cmd:     []string{"serve", "--profile", "test"},
		Env:     serverEnv,
		Labels:  map[string]string{LabelInstance: instance, LabelComponent: "server"},
		Network: networkName,
		Mounts: []MountBind{
			{Source: stateDir, Target: stateDir},
			{Source: containerSocket, Target: "/var/run/kodema-container.sock"},
		},
		SecurityOpt: hostMountSecurityOpt,
		Ports:       []PortPublish{{ContainerPort: 8443, HostIP: "127.0.0.1", HostPort: serverPort}},
	}); err != nil {
		return StackDescriptor{}, err
	}

	pool, err := tlsBundle.CertPool()
	if err != nil {
		return StackDescriptor{}, err
	}
	provisionStack := cfg.Provision
	if provisionStack == nil {
		provisionStack = provision
	}
	fmt.Fprintf(stderr, "waiting for kodema server readiness (%s/readyz)...\n", publicURL)
	gitToken, err := provisionStack(ctx, provisionConfig{
		PublicURL:    publicURL,
		RootCAs:      pool,
		Issuer:       disc.Issuer,
		TenantID:     tenantID,
		ProjectID:    projectID,
		TenantToken:  tenantAdminToken,
		ProjectToken: adminToken,
		GitOrigins:   []string{publicURL, workloadOrigin},
	})
	if err != nil {
		if tail := engine.LogTail(ctx, serverContainer, 5); tail != "" {
			return StackDescriptor{}, fmt.Errorf("%w; the log of container %s ends with:\n%s", err, serverContainer, tail)
		}
		return StackDescriptor{}, fmt.Errorf("%w (see the logs of container %s)", err, serverContainer)
	}
	gitTokenPath := filepath.Join(secretsDir, "git-service-token")
	if err := writeFileAtomic(gitTokenPath, []byte(gitToken+"\n"), 0o600); err != nil {
		return StackDescriptor{}, err
	}

	fmt.Fprintf(stderr, "starting git service (%s)...\n", gitContainer)
	if _, err := engine.RunOneShot(ctx, ContainerSpec{
		Name:        prefix + "-git-init",
		Image:       kodemaImage,
		User:        kodemaContainerUser,
		Network:     networkName,
		Labels:      map[string]string{LabelInstance: instance, LabelComponent: "git-init"},
		Mounts:      []MountBind{{Source: gitDir, Target: gitDir}},
		SecurityOpt: hostMountSecurityOpt,
		Cmd: []string{
			"git", "provision-tenant",
			"--tenant", tenantID,
			"--storage-root", filepath.Join(stateDir, "git", "repos"),
			"--journal-dir", filepath.Join(stateDir, "git", "journal"),
		},
	}); err != nil {
		return StackDescriptor{}, fmt.Errorf("git provision-tenant: %w", err)
	}

	_ = engine.RemoveContainer(ctx, gitContainer)
	if _, err := engine.StartDetached(ctx, ContainerSpec{
		Name:    gitContainer,
		Image:   kodemaImage,
		User:    kodemaContainerUser,
		Labels:  map[string]string{LabelInstance: instance, LabelComponent: "git"},
		Network: networkName,
		Cmd: []string{
			"git", "serve",
			"--tenant", tenantID,
			"--listen", "0.0.0.0:8081",
			"--storage-root", filepath.Join(stateDir, "git", "repos"),
			"--journal-dir", filepath.Join(stateDir, "git", "journal"),
			"--base-url", "https://" + serverContainer + ":8443",
			"--credential-file", gitTokenPath,
			"--resource", "admin=git-admin",
		},
		Env: []string{
			"SSL_CERT_FILE=" + tlsBundle.CACertPath,
		},
		// Git serves pushed content, so it sees only its repositories, its
		// credential and the CA, never the installation's keys.
		Mounts: []MountBind{
			{Source: gitDir, Target: gitDir},
			{Source: gitTokenPath, Target: gitTokenPath, ReadOnly: true},
			{Source: tlsBundle.CACertPath, Target: tlsBundle.CACertPath, ReadOnly: true},
		},
		SecurityOpt: hostMountSecurityOpt,
		Ports:       []PortPublish{{ContainerPort: 8081, HostIP: "127.0.0.1", HostPort: gitPort}},
	}); err != nil {
		return StackDescriptor{}, err
	}

	if cfg.Foreground {
		stopTempGateway()
		fmt.Fprintf(stdout, "Kodema stack %q ready at %s (Ctrl-C to stop gateway)\n", instance, publicURL)
		return desc, ServeStackGateway(ctx, stateDir)
	}

	fmt.Fprintf(stdout, "Kodema stack %q is up on single origin %s (%s via %s)\n",
		instance, publicURL, detected.Kind, detected.Provider)
	fmt.Fprintf(stdout, "  API & MCP:  %s  (%s/mcp)\n", publicURL, publicURL)
	fmt.Fprintf(stdout, "  OIDC Auth:  %s/auth/.well-known/openid-configuration\n", publicURL)
	fmt.Fprintf(stdout, "  Git HTTP:   %s/git/\n", publicURL)
	fmt.Fprintf(stdout, "  Shell env:  source %s\n", shellSingleQuote(envFilePath))
	fmt.Fprintf(stdout, "  CLI login:  kodema --server %s --ca-file %s login\n",
		shellSingleQuote(publicURL), shellSingleQuote(tlsBundle.CACertPath))
	return desc, nil
}

// ServeStackGateway runs the single-origin HTTPS gateway + /auth/ OIDC issuer
// configured by <state-dir>/stack.json.
func ServeStackGateway(ctx context.Context, stateDir string) error {
	desc, err := loadStackDescriptor(stateDir)
	if err != nil {
		return err
	}
	// The same names as in Up: a reissued leaf must still cover the containers.
	prefix := "kodema-" + desc.Instance
	tlsBundle, err := EnsureTLS(TLSConfig{
		Dir:   filepath.Join(stateDir, "tls"),
		Hosts: []string{desc.ListenAddr, prefix + "-server", prefix + "-git"},
	})
	if err != nil {
		return err
	}
	pool, err := tlsBundle.CertPool()
	if err != nil {
		return err
	}
	tlsCfg, err := tlsBundle.ServerTLSConfig()
	if err != nil {
		return err
	}
	devIssuer, err := NewDevIssuer(DevIssuerConfig{
		Issuer:         desc.PublicURL + "/auth",
		Audience:       desc.PublicURL,
		APIURL:         desc.PublicURL,
		ClientID:       defaultOIDCClientID,
		KeyPath:        filepath.Join(stateDir, "oidc-signing.key"),
		TenantID:       desc.TenantID,
		ProjectID:      desc.ProjectID,
		AccessTokenTTL: 24 * time.Hour,
	})
	if err != nil {
		return err
	}
	gw, err := NewGateway(GatewayConfig{
		ServerUpstream:  desc.ServerUpstream,
		AuthHandler:     devIssuer,
		GitUpstream:     desc.GitUpstream,
		UpstreamRootCAs: pool,
	})
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", desc.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen %q: %w", desc.ListenAddr, err)
	}
	return gw.ServeTLS(ctx, ln, tlsCfg)
}

// Down stops the single-origin gateway and removes containers for cfg.Instance.
// When cfg.Purge is true, managed Postgres/NATS volumes and stateDir are removed.
func Down(ctx context.Context, cfg DownConfig, stdout io.Writer) error {
	if stdout == nil {
		stdout = io.Discard
	}
	instance := defaultIfEmpty(cfg.Instance, DefaultInstanceName)
	stateDir, err := ResolveStateDir(StateDirOptions{
		GOOS:                  cfg.GOOS,
		HomeDir:               cfg.HomeDir,
		InstanceName:          instance,
		ExplicitStateDir:      cfg.StateDir,
		AllowUnsharedStateDir: cfg.AllowUnsharedStateDir,
	})
	if err != nil {
		return err
	}

	stopExistingGateway(stateDir)

	detectOpts := buildDetectOpts(cfg.DetectOpts, cfg.Socket, cfg.GOOS, cfg.HomeDir, stateDir)
	if detected, detectErr := DetectRuntime(ctx, detectOpts); detectErr == nil {
		if engine, engineErr := NewEngineClient(detected.SocketPath, cfg.EngineHTTP); engineErr == nil {
			prefix := "kodema-" + instance
			if containers, listErr := engine.ListInstanceContainers(ctx, instance); listErr == nil {
				for _, c := range containers {
					_ = engine.RemoveContainer(ctx, c.Name)
				}
			}
			for _, known := range []string{prefix + "-git", prefix + "-server", prefix + "-nats", prefix + "-postgres"} {
				_ = engine.RemoveContainer(ctx, known)
			}
			_ = engine.RemoveNetwork(ctx, prefix+"-net")
			if cfg.Purge {
				_ = engine.RemoveWorkloads(ctx, stateDir)
				_ = engine.RemoveVolume(ctx, prefix+"-pgdata")
				_ = engine.RemoveVolume(ctx, prefix+"-natsdata")
			}
		}
	} else if cfg.Socket != "" {
		return detectErr
	}

	if cfg.Purge {
		if err := PurgeInstanceStateDir(stateDir, instance); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Kodema stack %q stopped and purged (%s)\n", instance, stateDir)
		return nil
	}
	fmt.Fprintf(stdout, "Kodema stack %q stopped (state kept in %s)\n", instance, stateDir)
	return nil
}

// Status inspects the gateway, containers and /readyz health of cfg.Instance.
func Status(ctx context.Context, cfg StatusConfig) (StatusReport, error) {
	instance := defaultIfEmpty(cfg.Instance, DefaultInstanceName)
	stateDir, err := ResolveStateDir(StateDirOptions{
		GOOS:                  cfg.GOOS,
		HomeDir:               cfg.HomeDir,
		InstanceName:          instance,
		ExplicitStateDir:      cfg.StateDir,
		AllowUnsharedStateDir: cfg.AllowUnsharedStateDir,
	})
	if err != nil {
		return StatusReport{}, err
	}

	detectOpts := buildDetectOpts(cfg.DetectOpts, cfg.Socket, cfg.GOOS, cfg.HomeDir, stateDir)
	detected, err := DetectRuntime(ctx, detectOpts)
	if err != nil {
		return StatusReport{}, err
	}
	engine, err := NewEngineClient(detected.SocketPath, cfg.EngineHTTP)
	if err != nil {
		return StatusReport{}, err
	}

	report := StatusReport{
		Instance:   instance,
		StateDir:   stateDir,
		PublicURL:  "https://127.0.0.1:8443",
		Runtime:    detected,
		Containers: make(map[string]ContainerInfo, 4),
	}
	if desc, descErr := loadStackDescriptor(stateDir); descErr == nil {
		if desc.PublicURL != "" {
			report.PublicURL = desc.PublicURL
		}
		report.GatewayPID = desc.GatewayPID
		if desc.GatewayPID > 0 && processAlive(desc.GatewayPID) {
			report.GatewayAlive = true
		}
	}
	if pid := readPIDFile(filepath.Join(stateDir, "gateway.pid")); pid > 0 && processAlive(pid) {
		report.GatewayPID = pid
		report.GatewayAlive = true
	}

	prefix := "kodema-" + instance
	for _, component := range []string{"postgres", "nats", "server", "git"} {
		info, inspectErr := engine.InspectContainer(ctx, prefix+"-"+component)
		if inspectErr != nil {
			return report, inspectErr
		}
		report.Containers[component] = info
	}

	caPath := filepath.Join(stateDir, "tls", "ca.crt")
	if probeReadyz(ctx, report.PublicURL+"/readyz", caPath) {
		report.Ready = true
		report.GatewayAlive = true
	}
	return report, nil
}

func probeReadyz(ctx context.Context, readyURL, caPath string) bool {
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return false
	}
	bundle := TLSBundle{CACertPath: caPath, CACertPEM: caPEM}
	pool, err := bundle.CertPool()
	if err != nil {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	tr := &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 2 * time.Second}
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, readyURL, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode == http.StatusOK
}

// containerSocketMountSource is the engine socket the server container mounts.
// remoteSocket is the path Podman reports for its own socket, or "".
func containerSocketMountSource(goos string, detected DetectedRuntime, remoteSocket string) string {
	effectiveOS := strings.TrimSpace(goos)
	if effectiveOS == "" {
		effectiveOS = runtime.GOOS
	}
	// On macOS Linux VMs (OrbStack, Docker Desktop, Colima, Podman Machine), the
	// host socket is a host-side tunnel outside the VM filesystem; inside the VM
	// /var/run/docker.sock (or the Podman socket) is the local Unix socket. A
	// rootless Podman machine, the default, serves /run/user/<uid>/podman/podman.sock
	// and a rootful one /run/podman/podman.sock, so Podman is asked.
	if effectiveOS == "darwin" {
		if detected.Kind == KindPodman {
			if remoteSocket != "" {
				return remoteSocket
			}
			return "/run/podman/podman.sock"
		}
		return "/var/run/docker.sock"
	}
	return detected.SocketPath
}

// buildDetectOpts assembles the socket discovery options of one command. The
// runtime recorded in stateDir's stack.json, if any, is offered to discovery.
func buildDetectOpts(override *DetectOptions, explicitSocket, goos, homeDir, stateDir string) DetectOptions {
	var recorded DetectedRuntime
	if desc, err := loadStackDescriptor(stateDir); err == nil {
		recorded = desc.Runtime
	}
	if override != nil {
		copied := *override
		copied.Recorded = recorded
		if explicitSocket != "" {
			copied.ExplicitSocket = explicitSocket
		}
		if goos != "" && copied.GOOS == "" {
			copied.GOOS = goos
		}
		if homeDir != "" && copied.HomeDir == "" {
			copied.HomeDir = homeDir
		}
		return copied
	}
	return DetectOptions{
		ExplicitSocket: explicitSocket,
		Recorded:       recorded,
		GOOS:           goos,
		HomeDir:        homeDir,
		UID:            os.Getuid(),
	}
}

func ensureSecretHex(path string, numBytes int) (string, error) {
	if data, err := os.ReadFile(path); err == nil {
		trimmed := strings.TrimSpace(string(data))
		if len(trimmed) >= numBytes*2 {
			_ = os.Chmod(path, 0o600)
			return trimmed, nil
		}
	}
	val, err := randomTokenHex(numBytes)
	if err != nil {
		return "", err
	}
	if err := writeFileAtomic(path, []byte(val+"\n"), 0o600); err != nil {
		return "", err
	}
	return val, nil
}

func waitPostgresReady(ctx context.Context, engine *EngineClient, containerName string) error {
	deadline := time.Now().Add(45 * time.Second)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Over TCP: on a new volume the image first runs a temporary server
		// that answers on the socket only, then stops it. A probe over the
		// socket can reach that one, and the next statement then fails.
		code, err := engine.Exec(ctx, containerName, []string{"psql", "-h", "127.0.0.1", "-U", "postgres", "-d", "postgres", "-Atc", "SELECT 1"})
		if err == nil && code == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("PostgreSQL container did not become ready within 45s")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func saveStackDescriptor(stateDir string, desc StackDescriptor) error {
	data, err := json.MarshalIndent(desc, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(stateDir, "stack.json"), append(data, '\n'), 0o600)
}

// DiscoverDefaultInstance returns the StackDescriptor of the local "default"
// `kodema up` instance under $HOME/.kodema/instances/default when its stack
// metadata is present and its TLS gateway is alive. When no live default
// instance exists, ok is false.
func DiscoverDefaultInstance(homeDir string) (StackDescriptor, bool) {
	home := strings.TrimSpace(homeDir)
	if home == "" {
		detected, err := os.UserHomeDir()
		if err != nil || strings.TrimSpace(detected) == "" {
			return StackDescriptor{}, false
		}
		home = detected
	}
	stateDir, err := DefaultStateDir(home, DefaultInstanceName)
	if err != nil {
		return StackDescriptor{}, false
	}
	desc, err := loadStackDescriptor(stateDir)
	if err != nil || strings.TrimSpace(desc.PublicURL) == "" {
		return StackDescriptor{}, false
	}
	if !instanceGatewayAlive(stateDir, desc) {
		return StackDescriptor{}, false
	}
	return desc, true
}

// instanceGatewayAlive reports whether the local instance gateway is running,
// checking gateway.pid first (background daemon mode) and falling back to a
// fast loopback TCP dial (foreground mode).
func instanceGatewayAlive(stateDir string, desc StackDescriptor) bool {
	if pid := readPIDFile(filepath.Join(stateDir, "gateway.pid")); pid > 0 && processAlive(pid) {
		return true
	}
	parsed, err := url.Parse(strings.TrimSpace(desc.PublicURL))
	if err != nil || parsed.Host == "" {
		return false
	}
	host := parsed.Hostname()
	if host != "127.0.0.1" && host != "::1" && host != "localhost" {
		return false
	}
	port := parsed.Port()
	if port == "" {
		if strings.EqualFold(parsed.Scheme, "https") {
			port = "443"
		} else {
			port = "80"
		}
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func loadStackDescriptor(stateDir string) (StackDescriptor, error) {
	data, err := os.ReadFile(filepath.Join(stateDir, "stack.json"))
	if err != nil {
		return StackDescriptor{}, err
	}
	var desc StackDescriptor
	if err := json.Unmarshal(data, &desc); err != nil {
		return StackDescriptor{}, err
	}
	return desc, nil
}

func defaultSpawnGatewayDaemon(stateDir string) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	logPath := filepath.Join(stateDir, "gateway.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer logFile.Close()

	cmd := exec.Command(exe, "up", "--gateway-only", "--state-dir", stateDir)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

func stopExistingGateway(stateDir string) {
	pidPath := filepath.Join(stateDir, "gateway.pid")
	pid := readPIDFile(pidPath)
	if pid <= 0 {
		if desc, err := loadStackDescriptor(stateDir); err == nil {
			pid = desc.GatewayPID
		}
	}
	if pid > 0 && pid != os.Getpid() && processAlive(pid) && gatewayProcess(pid) {
		_ = syscall.Kill(pid, syscall.SIGTERM)
		deadline := time.Now().Add(3 * time.Second)
		for processAlive(pid) && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
	}
	_ = os.Remove(pidPath)
}

// gatewayProcess reports whether pid runs `kodema up --gateway-only`: after a
// reboot the recorded PID may belong to another process. When ps cannot tell,
// the PID is trusted.
func gatewayProcess(pid int) bool {
	out, err := exec.Command("ps", "-ww", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	return err != nil || strings.Contains(string(out), "--gateway-only")
}

func readPIDFile(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func allocateLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("allocate loopback port: %w", err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	port := addr.Port
	_ = ln.Close()
	return port, nil
}

func defaultIfEmpty(val, fallback string) string {
	if strings.TrimSpace(val) == "" {
		return fallback
	}
	return strings.TrimSpace(val)
}

const secretRulesFileName = "secret-rules.json"

type localSecretRulesFile struct {
	SecretRules []localSecretRule `json:"secret_rules"`
}

type localSecretRule struct {
	TenantID  string   `json:"tenant_id"`
	ProjectID string   `json:"project_id,omitempty"`
	Names     []string `json:"names,omitempty"`
	AllowAll  bool     `json:"allow_all,omitempty"`
}

// validateAllowSecrets validates and deduplicates explicit --allow-secret
// names before any container or file state is touched.
func validateAllowSecrets(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, entry := range raw {
		trimmed := strings.TrimSpace(entry)
		parsed, err := id.ParseSecretName(trimmed)
		if err != nil {
			return nil, fmt.Errorf("invalid --allow-secret %q: %w", entry, err)
		}
		name := string(parsed)
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out, nil
}

// reconcileSecretRulesFile writes or removes <state-dir>/secret-rules.json for
// the local stack's server container. When denyAll is true it removes any stale
// file and returns an empty path so the server stays deny-by-default.
func reconcileSecretRulesFile(stateDir, tenantID, projectID string, denyAll bool, allowSecrets []string) (string, error) {
	path := filepath.Join(stateDir, secretRulesFileName)
	if denyAll {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("remove %s: %w", secretRulesFileName, err)
		}
		return "", nil
	}
	rule := localSecretRule{
		TenantID:  tenantID,
		ProjectID: projectID,
	}
	if len(allowSecrets) > 0 {
		rule.Names = allowSecrets
	} else {
		rule.AllowAll = true
	}
	data, err := json.MarshalIndent(localSecretRulesFile{SecretRules: []localSecretRule{rule}}, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal %s: %w", secretRulesFileName, err)
	}
	if err := writeFileAtomic(path, append(data, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", secretRulesFileName, err)
	}
	return path, nil
}
