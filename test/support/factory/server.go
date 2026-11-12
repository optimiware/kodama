package factory

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/installation"
	"github.com/optimiware/kodama/internal/shell/app"
	driverkube "github.com/optimiware/kodama/internal/shell/app/drivers/kube"
	pgdriver "github.com/optimiware/kodama/internal/shell/app/drivers/postgres"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	shelllog "github.com/optimiware/kodama/internal/shell/log"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
	runtimepodman "github.com/optimiware/kodama/internal/shell/runtime/podman"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	kubesupport "github.com/optimiware/kodama/test/support/infrastructure/kubernetes"
	"github.com/optimiware/kodama/test/support/infrastructure/messaging"
	pgtest "github.com/optimiware/kodama/test/support/infrastructure/postgres"
	runtimesupport "github.com/optimiware/kodama/test/support/infrastructure/runtime"
)

// Fixture identities. Credentials are never compile-time constants: Start
// generates a fresh unpredictable token for each harness (see harnessTokenBytes).
const (
	TenantID       = "tenant-i04"
	ProjectID      = "project-i04"
	AdminPrincipal = "admin-i04"

	// InstallationID is the composition installation identity.
	InstallationID = "installation-i04"
	// KeyRef is the persistent keyfile reference the test composition uses.
	KeyRef = "keyfile:kodama"
)

// retiredFixtureTokens are the literal credentials an earlier harness revision
// committed to the repository and registered with the served composition. They
// are not credentials any more: Start asserts every generated token differs
// from them, and AssertRetiredFixtureTokensRefused proves the served edge
// refuses them. They are kept only as a denylist so a regression that
// reintroduces a published credential is caught.
var retiredFixtureTokens = []string{
	"fixture-i04-admin-token-3a1c9e",
	"fixture-i04-project-token-7b2d4f",
}

// harnessTokenBytes is the entropy of one generated harness credential. 24
// random bytes are 192 bits, comfortably above the 128-bit minimum.
const harnessTokenBytes = 24

// newHarnessToken returns one fresh unpredictable harness credential. The
// value is generated with crypto/rand and rendered as unpadded URL-safe base64
// so it is safe in a bearer header and never a committed literal.
func newHarnessToken(t testing.TB) string {
	t.Helper()
	material := make([]byte, harnessTokenBytes)
	if _, err := rand.Read(material); err != nil {
		t.Fatalf("factory: generate harness token: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(material)
}

// newHarnessCredentials returns the two independent per-run credentials and
// refuses a value that collides with the other credential or with any retired
// published literal.
func newHarnessCredentials(t testing.TB) (admin, project string) {
	t.Helper()
	admin = newHarnessToken(t)
	project = newHarnessToken(t)
	if admin == project {
		t.Fatalf("factory: generated admin and project tokens are identical")
	}
	for _, retired := range retiredFixtureTokens {
		if admin == retired || project == retired {
			t.Fatalf("factory: generated a retired published fixture credential")
		}
	}
	return admin, project
}

// workloadListenHost returns the narrowest host address a rootless Podman
// workload can reach, and which the factory edge therefore binds.
//
// Observed behaviour (Podman 5.4.2, rootless, pasta):
// `podman --log-level=debug run ...` shows pasta started with
//
//	--config-net --dns-forward 169.254.1.1 -t none -u none -T none -U none
//	--no-map-gw --map-guest-addr 169.254.1.2
//
// and a container resolves host.containers.internal to 169.254.1.2. With
// --no-map-gw and no --map-host-loopback, pasta does not forward to the host
// loopback: it maps 169.254.1.2 to the host address assigned to the container
// (pasta copies the host's default-route address into the container). Measured
// directly: a listener on 127.0.0.1:39001 refused the container's request while
// a listener on the host's default-route address answered
// it. Binding loopback would break the workload path, so the narrowest working
// bind is that single default-route interface address, never the unspecified
// wildcard.
//
// A connected UDP socket performs the same route lookup the kernel uses for the
// default route without sending a packet; its local address is exactly the
// address pasta assigns the workload. A host without a usable IPv4 default
// route cannot serve the workload path at all, so Start fails closed instead of
// widening the bind.
//
// The address is resolved once when Start runs. A harness is short lived and a
// mid-run default-route change is not repaired: the workload alias would stop
// matching this listener and the affected test fails visibly rather than the
// edge silently widening to a wildcard. The non-fatal defaultRouteHost variant
// exists so the build-tag-free security tests can skip honestly on a host with
// no route instead of failing the default unit baseline.
func workloadListenHost(t testing.TB) string {
	t.Helper()
	host, err := defaultRouteHost()
	if err != nil {
		t.Fatalf("factory: resolve the host default-route address for the workload edge: %v", err)
	}
	return host
}

// defaultRouteHost returns the host's IPv4 default-route address, or an error
// when the host has no usable route. It is the error-returning form shared by
// the harness Start path (which fails closed) and the route-dependent security
// test (which skips unless KODAMA_REQUIRE_INFRA=1). It never widens the bind.
func defaultRouteHost() (string, error) {
	connection, err := net.Dial("udp4", "1.1.1.1:53")
	if err != nil {
		return "", fmt.Errorf("%w: %v", errNoDefaultRoute, err)
	}
	defer func() { _ = connection.Close() }()
	address, ok := connection.LocalAddr().(*net.UDPAddr)
	if !ok || address.IP == nil || address.IP.IsUnspecified() || address.IP.IsLoopback() {
		return "", fmt.Errorf("default-route address %v is not a specific host interface address", connection.LocalAddr())
	}
	return address.IP.String(), nil
}

// errNoDefaultRoute identifies the only probe failure that may be skipped: the
// kernel could not select an IPv4 source address because the host has no usable
// default route. Any other probe failure is a real defect and is never skipped.
var errNoDefaultRoute = errors.New("no IPv4 default route")

// hostOnlyListener accepts only connections whose peer address is the host
// address the edge is bound to. Pasta's --map-guest-addr forwarding and
// host-local clients both appear with that source address, while any other
// network peer is refused before the HTTP server sees the connection. This is
// defense in depth on top of the per-run random credentials: even though the
// narrowest working bind is a routable interface address, a remote peer can
// never reach the served API.
type hostOnlyListener struct {
	net.Listener
	host net.IP
}

// Accept implements net.Listener.
func (listener hostOnlyListener) Accept() (net.Conn, error) {
	for {
		connection, err := listener.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if peerAllowed(connection.RemoteAddr(), listener.host) {
			return connection, nil
		}
		_ = connection.Close()
	}
}

// peerAllowed reports whether one accepted peer is the host itself.
func peerAllowed(peer net.Addr, host net.IP) bool {
	address, ok := peer.(*net.TCPAddr)
	return ok && host != nil && address.IP.Equal(host)
}

// newWorkloadEdge builds the workload-facing edge: it binds one concrete host
// address, wraps that listener in the host-only peer filter, and relays the
// filtered stream to the loopback-bound composition target. It returns the
// bound port so the caller can publish the workload URL.
//
// The host must already be a single concrete interface address. An empty,
// unspecified, or non-IP host is refused before any listener exists, so a route
// probe that degrades to "" can never turn net.JoinHostPort(host, "0") into the
// ":0" wildcard bind. This is the only place the harness creates a listener on
// a non-loopback address, so the refusal and the host-only wrapper live here
// where the infra-free unit tests can exercise them.
func newWorkloadEdge(listenHost, target string) (*edgeForwarder, int, error) {
	ip := net.ParseIP(listenHost)
	if ip == nil {
		return nil, 0, fmt.Errorf("workload edge host %q is not an IP address", listenHost)
	}
	if ip.IsUnspecified() {
		return nil, 0, fmt.Errorf("workload edge host %q is the unspecified wildcard", listenHost)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(listenHost, "0"))
	if err != nil {
		return nil, 0, fmt.Errorf("listen on workload edge host %s: %w", listenHost, err)
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		_ = listener.Close()
		return nil, 0, fmt.Errorf("workload edge listener address %v is not a TCP address", listener.Addr())
	}
	return newEdgeForwarder(hostOnlyListener{Listener: listener, host: ip}, target), address.Port, nil
}

// Options configures one factory harness.
type Options struct {
	// SecretRules is the trusted workload-consumer secret-name allow-list.
	// An empty list authorizes no activation secret, exactly like the
	// composition default.
	SecretRules []secret.NameRule
	// Secrets are written through secret.write before the harness returns.
	Secrets map[string]string
	// DefaultActionTimeout is the admitted fallback action timeout.
	DefaultActionTimeout time.Duration
	// LoopInterval is the composition background scan cadence.
	LoopInterval time.Duration
	// StopGate, when set, gates the composition's next Libpod StopContainer
	// call so a test can control when a runtime stop completes.
	StopGate *StopGate
	// TLS enables the opt-in private-CA HTTPS edge. The default harness keeps
	// serving plain HTTP on loopback.
	TLS bool
	// OIDC enables the opt-in real-verifier OIDC human authentication path.
	// The default harness keeps the static development authenticator only.
	OIDC bool
	// GitService enables the optional host Git composition. It implies the
	// private-CA TLS edge; see WithGitService.
	GitService bool
	// ModelFixture, when set, enables the optional configured model gateway
	// against the deterministic upstream provider. The default factory stays
	// model-disabled; see WithModelFixture.
	ModelFixture *ModelFixture
	// ToolFixture, when set, enables the optional external MCP tool proxy
	// against the deterministic bounded upstream. The default factory stays
	// tool-disabled; see WithToolFixture.
	ToolFixture *ToolFixture
	// ImageResolver overrides the test composition's image resolver. The
	// default is DigestResolver, which accepts only already digest-pinned
	// references. A test may supply a labelled fault-injection resolver to make
	// one admission attempt fail transiently or pause at the registry boundary;
	// it never fabricates an admitted image.
	ImageResolver pipeline.Resolver
	// GitTransport wraps the optional Git service's outbound HTTP transport.
	// It is applied only to the Git service's verification/report client, so a
	// test can hold or fail one labelled report call without touching the
	// harness's own public-edge requests. The wrapper must delegate to the inner
	// transport for every request it does not intercept.
	GitTransport func(inner http.RoundTripper) http.RoundTripper
}

// Option configures one factory harness.
type Option func(*Options)

// apply runs one option. A nil option is an explicit no-op, so a caller can
// pass a conditional or unset security option without changing the selection.
func (options *Options) apply(option Option) {
	if option != nil {
		option(options)
	}
}

// WithSecretRules supplies the workload secret-name allow-list.
func WithSecretRules(rules ...secret.NameRule) Option {
	return func(options *Options) {
		options.SecretRules = append([]secret.NameRule(nil), rules...)
	}
}

// WithSecrets writes the named secrets through the public API before the
// harness returns. Values are never rendered by the harness.
func WithSecrets(secrets map[string]string) Option {
	return func(options *Options) {
		options.Secrets = make(map[string]string, len(secrets))
		maps.Copy(options.Secrets, secrets)
	}
}

// WithLoopInterval overrides the composition background scan cadence.
func WithLoopInterval(interval time.Duration) Option {
	return func(options *Options) { options.LoopInterval = interval }
}

// WithStopGate gates the composition's next Libpod StopContainer call. The test
// keeps the gate and controls when the stop completes.
func WithStopGate(gate *StopGate) Option {
	return func(options *Options) { options.StopGate = gate }
}

// WithImageResolver overrides the composition's image resolver with one
// test-owned implementation. It is a labelled fault-injection seam: a resolver
// that fails or pauses a call proves the trigger processor's selection and
// retry behavior, and never supplies an image the product did not admit.
func WithImageResolver(resolver pipeline.Resolver) Option {
	return func(options *Options) { options.ImageResolver = resolver }
}

// WithGitTransport wraps the optional Git service's outbound HTTP transport so
// one test can hold or fail a labelled verification/report request. It affects
// only the Git service client, not the harness's own public-edge traffic.
func WithGitTransport(wrap func(inner http.RoundTripper) http.RoundTripper) Option {
	return func(options *Options) { options.GitTransport = wrap }
}

// WithTLS enables the opt-in private-CA HTTPS edge.
func WithTLS() Option {
	return func(options *Options) { options.TLS = true }
}

// WithOIDC enables the opt-in real-verifier OIDC human authentication path.
func WithOIDC() Option {
	return func(options *Options) { options.OIDC = true }
}

// Harness is one served real factory composition plus the isolated
// infrastructure it owns.
type Harness struct {
	T *testing.T

	App         *app.App
	Listener    net.Listener
	BaseURL     string
	WorkloadURL string
	// WorkloadAddress is the concrete host:port a workload or a client dials
	// through the forwarder. It is populated for every harness; under WithTLS
	// the same address serves HTTPS.
	WorkloadAddress string
	DataDir         string

	// TLS is non-nil when WithTLS or WithGitService selected the private-CA
	// HTTPS edge. It carries the private CA and the service certificate the
	// Shell and Git listeners serve.
	TLS *TLSFixture
	// OIDC is non-nil only when WithOIDC was selected. It carries the HTTPS
	// identity provider and the bootstrapped administrator identity.
	OIDC *OIDCFixture
	// Git is non-nil only when WithGitService was selected. It composes the
	// host service only; EnsureWorkloadImage adds container consumption.
	Git *GitFixture
	// Model is non-nil only when WithModelFixture was selected. It is the
	// deterministic upstream provider the configured model gateway calls.
	Model *ModelFixture
	// Tool is non-nil only when WithToolFixture was selected. It is the
	// deterministic bounded upstream the configured MCP tool proxy calls.
	Tool *ToolFixture

	Postgres *pgtest.Shard
	NATS     *messaging.NATS
	Runtime  *runtimesupport.Service
	Image    runtimesupport.PinnedImage

	// Kubernetes is non-nil only for the StartKubernetes variant. Cluster is
	// the private kind cluster this harness created; Client is the production
	// REST driver and Adapter the production runtime/workspace adapter bound to
	// Namespace.
	Kubernetes          *kubernetesRuntime
	Cluster             *kubesupport.Cluster
	KubernetesClient    *driverkube.Cluster
	KubernetesAdapter   *runtimekube.Adapter
	KubernetesNamespace string

	// Clock is the adjustable clock every composition capability consumes.
	Clock *TestClock

	AdminToken   string
	ProjectToken string
	// ReviewerToken, OperatorToken and EngineerToken are role-limited human
	// credentials generated per run. They prove assignment authority and role
	// inbox filtering without borrowing another role from a shared token, and
	// the engineer token proves an admission by a project-only membership whose
	// tenant-wide row is genuinely absent.
	ReviewerToken string
	OperatorToken string
	EngineerToken string
	// AdmissionToken is project-scoped and intentionally has no project
	// membership; the test seeds its tenant-wide membership with
	// SeedTenantWideMembership.
	AdmissionToken string
	TenantID       id.TenantID
	ProjectID      id.ProjectID
	Scope          tenancy.Scope

	runtimeDB    *pgdriver.DB
	controllerDB *pgdriver.DB
	publisherDB  *pgdriver.DB
	runtimeRoot  string
	fixture      FixtureScope
	forwarder    *edgeForwarder
	logs         *safeBuffer

	// clientTransport is the transport every harness helper uses. It is the
	// process default for the plain-HTTP harness and the fixture's
	// CA-trusting transport under WithTLS. It is never installed process-wide.
	clientTransport http.RoundTripper

	cancel    context.CancelFunc
	served    chan error
	serveDone chan struct{}
	closeOnce sync.Once

	// infra is the durable infrastructure a controller restart preserves.
	// RestartController rebuilds only the Shell adapters and the App over it.
	infra fixtureInfrastructure
}

// fixtureInfrastructure is everything outside the controller that survives a
// controller restart: the real PostgreSQL and NATS servers, the pinned keyfile,
// retained log sink, runtime platform (Podman socket or kind cluster), the
// public edge socket/forwarder and the per-run credentials. It is built once by
// start and never rebuilt by RestartController.
type fixtureInfrastructure struct {
	variant runtimeVariant
	opts    Options
	dataDir string
	clock   *TestClock
	logger  *slog.Logger
	shard   *pgtest.Shard
	nat     *messaging.NATS
	keyfile keys.Provider
	logSink shelllog.RetainedSink
	tokens  []app.DevToken
	devAuth *app.DevAuthenticator
	socket  string
	kube    *kubernetesRuntime
	pvc     *app.WorkspacePVC
	runtime installation.RuntimeAdapter
	// listenAddr is the public edge address. The listener itself is created by
	// start and re-created by a restart on the same address, because the
	// workload's KODAMA_URL and the forwarder target both depend on it.
	listenAddr      string
	rawListener     net.Listener
	listenerRetired bool
	// dbs are the database pools owned by the current controller. They are
	// recreated by every controller and closed after it has joined.
	dbs []*pgdriver.DB
}

// safeBuffer is a concurrency-safe composition log sink.
type safeBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

// Write implements io.Writer.
func (buffer *safeBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(data)
}

// String returns the captured composition log.
func (buffer *safeBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}

// Start builds one isolated real factory composition over the Podman runtime
// and serves it on the narrowest host address a rootless Podman workload can
// reach. Both fixture credentials are generated per run with crypto/rand.
// Infrastructure failures follow the shared KODAMA_REQUIRE_INFRA policy: a
// missing server skips unless the environment requires infrastructure, in
// which case it fails. A composition or wiring failure is always a test
// failure, never a skip.
func Start(t *testing.T, options ...Option) *Harness {
	t.Helper()
	return start(t, runtimeVariantPodman, options...)
}

// StartKubernetes builds the same isolated real factory composition over the
// production Kubernetes runtime/workspace adapters and a private rootless kind
// cluster. The workload is a Kubernetes Job with a PersistentVolumeClaim
// workspace. It has the same skip/fail policy as Start.
func StartKubernetes(t *testing.T, options ...Option) *Harness {
	t.Helper()
	return start(t, runtimeVariantKubernetes, options...)
}

func start(t *testing.T, variant runtimeVariant, options ...Option) *Harness {
	t.Helper()

	opts := Options{
		DefaultActionTimeout: app.DefaultActionTimeout,
		LoopInterval:         app.DefaultLoopInterval,
	}
	for _, option := range options {
		opts.apply(option)
	}
	if opts.DefaultActionTimeout <= 0 {
		t.Fatalf("factory: default action timeout must be positive")
	}
	if opts.LoopInterval <= 0 {
		t.Fatalf("factory: loop interval must be positive")
	}
	// The Git composition is bounded to the Podman runtime. Refuse a Kubernetes
	// factory before provisioning anything: its workload alias cannot reach a
	// host-local Git listener, and advertising one would be a false contract.
	if err := checkGitRuntimeVariant(variant, opts.GitService); err != nil {
		t.Fatalf("%v", err)
	}
	// The optional model fixture is started by the caller and must outlive the
	// controllers. Refuse it early, before any infrastructure exists, so a
	// closed or unstarted fixture is never mistaken for a provider.
	if opts.ModelFixture != nil {
		opts.ModelFixture.assertUsable(t)
		if modelFixtureSecretConflicts(opts.Secrets) {
			t.Fatalf("factory: secrets already configure the model fixture credential name %s", ModelSecretName)
		}
	}
	// The optional bounded MCP tool fixture is started by the caller and must
	// outlive every controller, like the model fixture. Refuse an unusable
	// fixture before any infrastructure exists.
	if opts.ToolFixture != nil {
		opts.ToolFixture.assertUsable(t)
		if toolFixtureSecretConflicts(opts.Secrets) {
			t.Fatalf("factory: secrets already configure the tool fixture credential name %s", ToolSecretName)
		}
	}

	// The fixture identity is minted before any resource exists and travels on
	// every container and workspace volume this harness creates. A second
	// fixture in the same test process therefore has an independent cleanup
	// selector while the process-wide run labels remain for crash recovery.
	fixture := newFixtureScope()
	phaseRunner := newStartupPhaseRunner(t.Context())
	setupCtx, cancelSetup := phaseRunner.context(phaseSetup)
	defer cancelSetup()

	dataDir := t.TempDir()
	clock := NewTestClock()
	logs := &safeBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Real PostgreSQL with role-separated adapters.
	shard := pgtest.Start(t)
	if _, err := pgtest.NewMigrationRunner(shard).Apply(setupCtx, 0); err != nil {
		t.Fatalf("factory: apply migrations: %v", err)
	}
	// Real NATS with the streams the composition publishes into.
	nat := messaging.StartNATS(t)

	// One persistent keyfile provider.
	keyPath := filepath.Join(dataDir, "keyfile")
	keyMaterial := make([]byte, secret.DataKeyBytes)
	if _, err := rand.Read(keyMaterial); err != nil {
		t.Fatalf("factory: generate keyfile material: %v", err)
	}
	if err := os.WriteFile(keyPath, keyMaterial, 0o600); err != nil {
		t.Fatalf("factory: write keyfile: %v", err)
	}
	provider, err := keys.NewKeyfile(secret.KeyRef(KeyRef), keyPath, rand.Reader)
	if err != nil {
		t.Fatalf("factory: keyfile provider: %v", err)
	}

	// Real retained log sink under the temp data directory.
	logSink, err := shelllog.NewFileSink(shelllog.SinkConfig{
		Root:  filepath.Join(dataDir, "logs"),
		Clock: clock,
	})
	if err != nil {
		t.Fatalf("factory: retained log sink: %v", err)
	}

	// Explicit development/test authentication: one tenant-wide admin token and
	// one project-scoped token for the same human. Both are generated per run
	// with crypto/rand; no credential is a repository literal.
	adminToken, projectToken := newHarnessCredentials(t)
	expiresAt := clock.Now().Add(24 * time.Hour)
	reviewer := newLimitedPrincipal(t, ReviewerPrincipal, tenancy.RoleReviewer, expiresAt)
	operator := newLimitedPrincipal(t, OperatorPrincipal, tenancy.RoleOperator, expiresAt)
	engineer := newLimitedPrincipal(t, EngineerPrincipal, tenancy.RoleEngineer, expiresAt)
	admission := newLimitedPrincipal(t, AdmissionPrincipal, tenancy.RoleEngineer, expiresAt)
	tokens := []app.DevToken{
		{
			Credential: adminToken,
			Principal: identity.Principal{
				ID:      id.PrincipalID(AdminPrincipal),
				Kind:    identity.KindHuman,
				Scope:   tenancy.Scope{TenantID: id.TenantID(TenantID)},
				Roles:   []tenancy.Role{tenancy.RoleAdmin},
				Enabled: true,
				Version: 1,
			},
			ExpiresAt: expiresAt,
		},
		{
			Credential: projectToken,
			Principal: identity.Principal{
				ID:    id.PrincipalID(AdminPrincipal),
				Kind:  identity.KindHuman,
				Scope: tenancy.Scope{TenantID: id.TenantID(TenantID), ProjectID: id.ProjectID(ProjectID)},
				Roles: []tenancy.Role{
					tenancy.RoleAdmin, tenancy.RoleEngineer,
					tenancy.RoleReviewer, tenancy.RoleOperator,
				},
				Enabled: true,
				Version: 1,
			},
			ExpiresAt: expiresAt,
		},
		reviewer.DevToken(),
		operator.DevToken(),
		engineer.DevToken(),
		admission.DevToken(),
	}
	// Opt-in OIDC human authentication: the real verifier against the
	// HTTPS identity fixture, backed by the current database membership reader.
	// The token is only an identity hint; the resolver re-reads the committed
	// membership on every call. The default harness keeps the static
	// development authenticator unchanged. The provider is infrastructure; the
	// membership adapter and resolver are rebuilt by every controller.
	var oidcFixture *OIDCFixture
	if opts.OIDC {
		oidcFixture = newOIDCFixture(t, clock.Now())
	}

	// One pinned workload image, resolved from the real Podman image store.
	// The digest-pinned reference is what the pipeline YAML carries: the
	// Podman runtime uses it directly, and the kind node pulls the same
	// docker.io manifest by digest.
	podmanService := runtimesupport.StartService(t)
	image := runtimesupport.ResolveImage(setupCtx, t, podmanService.Podman(), runtimesupport.ImageAlpineTag)
	t.Logf("factory: workload image %s digest %s", image.Tag, image.Digest)

	// The workload reaches the host through the runtime-specific host alias.
	// Podman forwards host.containers.internal to the host's default-route
	// address. A pod in the rootless kind cluster has no DNS and reaches the
	// host only at 169.254.1.2, which pasta maps to that same address. The
	// composition itself stays on loopback; only a host-sourced forwarder
	// listens on the narrow routable address (see workloadListenHost).
	listenHost := workloadListenHost(t)
	apiListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("factory: listen on loopback: %v", err)
	}
	apiPort := apiListener.Addr().(*net.TCPAddr).Port

	// Opt-in TLS: one private per-run CA and a service certificate covering
	// loopback and every real workload alias. The plain-HTTP default is
	// unchanged. The fixture client is used by the harness helpers only; the
	// process-wide default transport is never touched.
	scheme := "http"
	var tlsFixture *TLSFixture
	if tlsSelected(opts) {
		tlsFixture = newTLSFixture(t, serviceTLSHosts(listenHost)...)
		scheme = "https"
	}
	baseURL := fmt.Sprintf("%s://127.0.0.1:%d", scheme, apiPort)

	forwarder, edgePort, err := newWorkloadEdge(listenHost, net.JoinHostPort("127.0.0.1", strconv.Itoa(apiPort)))
	if err != nil {
		_ = apiListener.Close()
		t.Fatalf("factory: workload edge: %v", err)
	}
	// The forwarder and listener outlive several fallible fixture steps below,
	// including the runtime variant construction that can call t.Fatalf or skip.
	// Register their close now so no such exit path leaks the listener; both
	// closes are idempotent and h.close repeats them.
	t.Cleanup(func() {
		forwarder.Close()
		_ = apiListener.Close()
	})
	workloadHost := "host.containers.internal"
	if variant == runtimeVariantKubernetes {
		workloadHost = KubernetesHostAlias
	}
	workloadURL := fmt.Sprintf("%s://%s:%d", scheme, workloadHost, edgePort)
	workloadAddress := net.JoinHostPort(listenHost, strconv.Itoa(edgePort))

	installationRuntime := installation.RuntimePodman
	var kubeRuntime *kubernetesRuntime
	workspacePVC := (*app.WorkspacePVC)(nil)
	podmanSocket := ""
	runtimeRoot := filepath.Join(dataDir, "runtime")
	if variant == runtimeVariantKubernetes {
		installationRuntime = installation.RuntimeKubernetes
		workspacePVC = &app.WorkspacePVC{
			Namespace:    KubernetesNamespace,
			TenantID:     id.TenantID(TenantID),
			StorageClass: KubernetesStorageClass,
			Size:         KubernetesWorkspaceSize,
			AccessMode:   KubernetesWorkspaceAccessMode,
		}
	} else {
		podmanSocket = podmanService.Socket()
	}

	// Infrastructure provisioning and readiness have their own bound: a kind
	// cluster may legitimately outlast the setup phase. The application phase
	// below gets a fresh context. The phase callback receives the phase
	// context, so the real call sites can only use that bounded context.
	if err := phaseRunner.run(phaseInfrastructure, func(infrastructureCtx context.Context) error {
		switch variant {
		case runtimeVariantPodman:
			// The private Podman API socket and the runtime root are the
			// infrastructure. Every controller builds its own Libpod client and
			// fresh-only volume provisioner over them, and every container and
			// volume carries the run cleanup labels.
		case runtimeVariantKubernetes:
			// Real rootless kind cluster. The cluster and its kubeconfig are
			// infrastructure; every controller builds its own production
			// driver/adapter over them.
			kubeRuntime = newKubernetesRuntime(t, infrastructureCtx, KubernetesNamespace, id.TenantID(TenantID), workloadURL, clock)
		default:
			forwarder.Close()
			_ = apiListener.Close()
			t.Fatalf("factory: unknown runtime variant %d", variant)
		}
		return nil
	}); err != nil {
		forwarder.Close()
		_ = apiListener.Close()
		t.Fatalf("factory: infrastructure phase: %v", err)
	}

	devAuthenticator, err := app.NewDevAuthenticator(clock, tokens)
	if err != nil {
		forwarder.Close()
		_ = apiListener.Close()
		t.Fatalf("factory: development authenticator: %v", err)
	}
	h := &Harness{
		T:               t,
		BaseURL:         baseURL,
		WorkloadURL:     workloadURL,
		WorkloadAddress: workloadAddress,
		DataDir:         dataDir,
		TLS:             tlsFixture,
		OIDC:            oidcFixture,
		Postgres:        shard,
		NATS:            nat,
		Runtime:         podmanService,
		Image:           image,
		Kubernetes:      kubeRuntime,
		Clock:           clock,
		Model:           opts.ModelFixture,
		Tool:            opts.ToolFixture,
		AdminToken:      adminToken,
		ProjectToken:    projectToken,
		ReviewerToken:   reviewer.Token,
		OperatorToken:   operator.Token,
		EngineerToken:   engineer.Token,
		AdmissionToken:  admission.Token,
		TenantID:        id.TenantID(TenantID),
		ProjectID:       id.ProjectID(ProjectID),
		Scope:           tenancy.Scope{TenantID: id.TenantID(TenantID), ProjectID: id.ProjectID(ProjectID)},
		runtimeRoot:     runtimeRoot,
		fixture:         fixture,
		logs:            logs,
		forwarder:       forwarder,
		infra: fixtureInfrastructure{
			variant:     variant,
			opts:        opts,
			dataDir:     dataDir,
			clock:       clock,
			logger:      logger,
			shard:       shard,
			nat:         nat,
			keyfile:     provider,
			logSink:     logSink,
			tokens:      tokens,
			devAuth:     devAuthenticator,
			socket:      podmanSocket,
			kube:        kubeRuntime,
			pvc:         workspacePVC,
			runtime:     installationRuntime,
			listenAddr:  apiListener.Addr().String(),
			rawListener: apiListener,
		},
	}
	if tlsFixture != nil {
		h.clientTransport = tlsFixture.Client().Transport
	}
	if kubeRuntime != nil {
		h.Cluster = kubeRuntime.Cluster
		h.KubernetesClient = kubeRuntime.Client
		h.KubernetesAdapter = kubeRuntime.Adapter
		h.KubernetesNamespace = KubernetesNamespace
	}
	// Register teardown before the first controller is built so a composition
	// failure still closes any partial controller state. It is idempotent.
	t.Cleanup(h.close)

	// Building the composition has its own bound; it must not inherit an
	// infrastructure context that already spent its budget on kind. The
	// callback parameter is the only context app.New receives.
	if err := phaseRunner.run(phaseApplication, func(applicationCtx context.Context) error {
		config, discard, err := h.buildControllerConfig(applicationCtx)
		if err != nil {
			return err
		}
		composed, err := app.New(applicationCtx, config)
		if err != nil {
			discard()
			return err
		}
		return h.serveController(composed)
	}); err != nil {
		forwarder.Close()
		_ = apiListener.Close()
		t.Fatalf("factory: app.New: %v\ncomposition log:\n%s", err, SanitizeDiagnostic(logs.String()))
	}

	// The public edge is ready when MCP tools/list answers without a transport
	// or protocol error. No fixed sleep decides readiness.
	h.Eventually(t, 30*time.Second, func() bool {
		response, err := h.mcpRaw(api.MCPMethodToolsList, nil, "")
		return err == nil && response.Error == nil
	})

	// The bootstrap seed is tenant-wide only; grant the project roles the
	// project-scoped fixture credential needs, then write the configured
	// activation secrets through the public API.
	h.MustREST(adminToken, "membership.grant", api.MembershipGrantRequest{
		ProjectID:   ProjectID,
		PrincipalID: AdminPrincipal,
		Roles:       []string{"admin", "engineer", "reviewer", "operator"},
	}, nil, "grant-"+AdminPrincipal+"-"+ProjectID)

	// Grant each role-limited principal exactly its one role. membership.grant
	// replaces the complete role set, so a later accidental broad grant would
	// show up as a changed version rather than silently widening the fixture.
	for _, limited := range []LimitedPrincipal{reviewer, operator, engineer} {
		h.MustREST(adminToken, "membership.grant", api.MembershipGrantRequest{
			ProjectID:   ProjectID,
			PrincipalID: string(limited.ID),
			Roles:       []string{string(limited.Role)},
		}, nil, "grant-"+string(limited.ID)+"-"+ProjectID)
	}

	// The OIDC administrator's authority is a committed tenant-wide membership,
	// not the token and not a static-token substitute. The public API exposes
	// only project-scoped membership writes, so the tenant-wide row is seeded
	// through the same store seam the composition's reader consumes.
	if oidcFixture != nil {
		// The seed runs after infrastructure and composition construction, so
		// it gets a fresh bounded setup context rather than the setup context
		// that may already have been spent on a long kind phase. The callback
		// parameter is the only context the seed receives.
		if err := phaseRunner.run(phaseSetup, func(seedCtx context.Context) error {
			return h.SeedTenantWideMembership(seedCtx, string(oidcFixture.AdminPrincipal), []string{string(tenancy.RoleAdmin)})
		}); err != nil {
			t.Fatalf("factory: bootstrap OIDC administrator membership: %v", err)
		}
	}

	names := make([]string, 0, len(opts.Secrets))
	for name := range opts.Secrets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		h.MustREST(projectToken, "secret.write", api.SecretWriteRequest{
			ProjectID: ProjectID,
			SecretID:  name,
			Value:     opts.Secrets[name],
		}, nil, "secret-"+name)
	}

	// The generated model provider credential is written through the same
	// public secret.write operation, so the gateway resolves it from the real
	// project-scoped secret store. It is never logged or returned.
	if opts.ModelFixture != nil {
		opts.ModelFixture.writeCredential(h)
	}
	// The generated MCP upstream credential is written through the same public
	// operation, so the proxy resolves it from the real project-scoped secret
	// store. It is never logged or returned.
	if opts.ToolFixture != nil {
		opts.ToolFixture.writeCredential(h)
	}

	// The optional host Git service is composed last, after the public edge,
	// membership and secrets are ready. It owns its binary, storage, journal,
	// credential, bindings and listener; Close joins it before the Shell.
	if opts.GitService {
		startGitService(t, h)
	}

	return h
}

// Logs returns the captured composition log.
func (h *Harness) Logs() string { return h.logs.String() }

// ServingDone returns a channel that Close closes only after it has received
// the served composition's result. It is a join barrier: a non-blocking
// receive after Close succeeds exactly when the serving goroutine was joined,
// so it cannot be satisfied by a goroutine that merely has not run yet.
func (h *Harness) ServingDone() <-chan struct{} { return h.serveDone }

// Close stops the served composition, joins its goroutines and removes every
// run-labelled container and workspace volume. It joins the public edge, the
// workload forwarder, the OIDC provider and the composition. It is idempotent.
func (h *Harness) Close() { h.close() }

func (h *Harness) close() {
	h.closeOnce.Do(func() {
		if h.Git == nil {
			h.closeShell()
			return
		}
		// The Git fixture phases are ordered: stop its workload-side support,
		// join Git and its relay while the Shell still serves, then stop the
		// Shell and its workloads, and only then remove image names. A failed
		// join is reported, never hidden, and never skips the later cleanup.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		completed, err := (gitFixtureShutdown{
			StopWorkload: h.Git.stopWorkload,
			CloseEdge:    h.Git.closeEdge,
			CloseShell: func(context.Context) error {
				h.closeShell()
				return nil
			},
			RemoveImages: h.Git.removeWorkloadImages,
		}).run(ctx)
		if err != nil {
			h.T.Errorf("factory: close Git fixture (phases %s): %v", strings.Join(completed, ","), err)
		}
	})
}

// closeShell stops and joins the public edge, the OIDC provider, the
// composition and every run-labelled workload resource. It is reached only once
// per close, after the Git service has joined while the Shell was still
// serving.
func (h *Harness) closeShell() {
	// Stop the current controller exactly like a restart does, but without
	// starting a successor: a graceful shutdown must not stop a durable
	// workload, and teardown must not claim one was stopped. stopController
	// cancels, joins the serving goroutine (publishing the join barrier only
	// when the join really happened), waits the App and closes the listener
	// and the controller's database pools.
	h.stopController()
	h.forwarder.Close()
	if h.OIDC != nil {
		h.OIDC.Provider.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	// Remove only this fixture's resources. The process-wide run labels
	// remain on every resource for crash recovery, but a sibling fixture in
	// the same process must keep running.
	if err := h.removeOwnedResources(ctx); err != nil {
		h.T.Logf("factory: remove fixture resources: %v", err)
	}
}

// Eventually polls one condition with a bounded deadline. It is a barrier, not
// a fixed sleep. The timeout is reported on the given test, so a helper invoked
// from a subtest fails that subtest instead of its parent.
func (h *Harness) Eventually(t testing.TB, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition was not satisfied within %s\ncomposition log:\n%s", timeout, SanitizeDiagnostic(h.logs.String()))
}

// ExecutionRow reads the exact admitted YAML bytes and SHA-256 of one execution
// through the runtime role and its transaction-local scope. The admitted YAML
// lives base64-encoded in the executions.snapshot JSONB document (`yaml`), and
// the digest is the bytea yaml_sha256 column, so both are decoded to raw bytes.
func (h *Harness) ExecutionRow(ctx context.Context, executionID string) ([]byte, []byte, error) {
	h.T.Helper()
	rows, err := pgtest.ScopedQuery(ctx, h.runtimeDB, h.Scope, 2,
		"SELECT snapshot->>'yaml', encode(yaml_sha256, 'hex') FROM executions WHERE id = $1", executionID)
	if err != nil {
		return nil, nil, err
	}
	if len(rows) != 1 {
		return nil, nil, fmt.Errorf("execution %s returned %d rows; want 1", executionID, len(rows))
	}
	yamlBytes, err := base64.StdEncoding.DecodeString(rows[0][0])
	if err != nil {
		return nil, nil, fmt.Errorf("decode admitted yaml: %w", err)
	}
	digest, err := hex.DecodeString(rows[0][1])
	if err != nil {
		return nil, nil, fmt.Errorf("decode yaml digest: %w", err)
	}
	return yamlBytes, digest, nil
}

// ExecutionCount counts the executions of one pipeline in the fixture project.
func (h *Harness) ExecutionCount(ctx context.Context, pipelineID string) (int64, error) {
	h.T.Helper()
	return h.scopedCount(ctx, "SELECT count(*) FROM executions WHERE pipeline_id = $1", pipelineID)
}

// TaskCount counts the tasks in the fixture project through the runtime role's
// transaction-local scope. It is evidence that a refused idempotent retry
// admitted no replacement task.
func (h *Harness) TaskCount(ctx context.Context) (int64, error) {
	h.T.Helper()
	return h.scopedCount(ctx, "SELECT count(*) FROM tasks")
}

// SeedTenantWideMembership writes one fixture tenant-wide membership row through
// the store seam as the database owner. The public API exposes only
// project-scoped membership writes, so this is the setup needed to give a
// project-scoped principal authority without an exact-project row; the race
// under test then uses the public project-scoped first grant. The row has the
// same shape and versioning as the bootstrap seed.
func (h *Harness) SeedTenantWideMembership(ctx context.Context, principalID string, roles []string) error {
	h.T.Helper()
	if strings.TrimSpace(principalID) == "" {
		return errors.New("factory: tenant-wide membership seed needs a principal")
	}
	if len(roles) == 0 {
		return errors.New("factory: tenant-wide membership seed needs roles")
	}
	encoded, err := json.Marshal(roles)
	if err != nil {
		return fmt.Errorf("factory: encode seeded membership roles: %w", err)
	}
	owner := storepg.New(h.Postgres.Owner())
	_, err = owner.Execute(ctx, tenancy.Scope{TenantID: h.TenantID}, store.Unit{
		Writes: []store.Write{{
			Table:  "memberships",
			ID:     "membership-fixture-" + principalID,
			Insert: true,
			Values: map[string]any{
				"principal_id": principalID,
				"roles":        encoded,
				"version":      uint64(1),
				"created_at":   h.Clock.Now(),
			},
		}},
	})
	if err != nil {
		return fmt.Errorf("factory: seed tenant-wide membership for %s: %w", principalID, err)
	}
	return nil
}

// ReceiptCount counts the receipts recorded for one idempotency key.
func (h *Harness) ReceiptCount(ctx context.Context, callerKey string) (int64, error) {
	h.T.Helper()
	return h.scopedCount(ctx, "SELECT count(*) FROM receipts WHERE caller_key = $1", callerKey)
}

// ExecutionCredentialCleared reports whether the execution row no longer
// carries credential material. It never reads or renders the nonce itself: the
// caller only needs the atomic revocation fact, so a terminal commit that
// clears the nonce/expiry pair is distinguishable from one that merely relies
// on the terminal state to disable the credential.
func (h *Harness) ExecutionCredentialCleared(ctx context.Context, executionID string) (bool, error) {
	h.T.Helper()
	count, err := h.scopedCount(ctx,
		`SELECT count(*) FROM executions
		  WHERE id = $1 AND token_nonce IS NULL AND token_expires_at IS NULL`, executionID)
	if err != nil {
		return false, err
	}
	switch count {
	case 0:
		return false, nil
	case 1:
		return true, nil
	default:
		return false, fmt.Errorf("execution %s matched %d rows", executionID, count)
	}
}

// ActionRow is the durable database evidence of one admitted action. Outputs
// is the promoted response column; StagedOutputs is the container-submitted
// staging column; an empty string means the SQL value is NULL.
type ActionRow struct {
	ActionID         string
	State            string
	Outputs          string
	StagedOutputs    string
	StagedOutputsSet bool
	WorkloadID       string
	// ObservationSet reports that the action already carries a persisted
	// runtime observation. A running container's first Running observation is
	// the last engine commit before an exit observation, so this is the
	// quiescence barrier a race hold takes before it releases the workload.
	ObservationSet bool
}

// ActionRow reads one action row by admitted name through the controller role.
// The runtime role has no SELECT grant on actions, so this evidence read uses
// the reviewed cross-scope controller role and its transaction-local scope.
func (h *Harness) ActionRow(ctx context.Context, executionID, name string) (ActionRow, error) {
	h.T.Helper()
	rows, err := pgtest.ScopedQuery(ctx, h.controllerDB, h.Scope, 7,
		`SELECT id, state, coalesce(outputs::text, ''), coalesce(staged_outputs::text, ''),
		        staged_outputs_set, coalesce(workload_id, ''), observation IS NOT NULL
		   FROM actions WHERE execution_id = $1 AND name = $2`, executionID, name)
	if err != nil {
		return ActionRow{}, err
	}
	if len(rows) != 1 || len(rows[0]) != 7 {
		return ActionRow{}, fmt.Errorf("action %s/%s returned %d rows", executionID, name, len(rows))
	}
	return ActionRow{
		ActionID:         rows[0][0],
		State:            rows[0][1],
		Outputs:          rows[0][2],
		StagedOutputs:    rows[0][3],
		StagedOutputsSet: rows[0][4] == "true",
		WorkloadID:       rows[0][5],
		ObservationSet:   rows[0][6] == "true",
	}, nil
}

// ExecutionRuntimeEvidence renders the persisted launch, observation and stop
// evidence of every action of one execution and the live runtime state of its
// Podman containers. It is failure diagnostics only. The public execution view
// reports "running" for an accepted launch and for a launch whose outcome is
// unknown alike, so a wait that times out needs this evidence to tell a slow
// workload from a start the runtime refused. A failed read is rendered as
// evidence instead of failing the test. Nothing here carries a secret.
func (h *Harness) ExecutionRuntimeEvidence(ctx context.Context, executionID string) string {
	h.T.Helper()
	var evidence strings.Builder
	rows, err := pgtest.ScopedQuery(ctx, h.controllerDB, h.Scope, 7,
		`SELECT name, state, version::text, launch_state, coalesce(workload_id, ''),
		        coalesce(observation::text, 'none'), coalesce(stop::text, 'none')
		   FROM actions WHERE execution_id = $1 ORDER BY index`, executionID)
	if err != nil {
		fmt.Fprintf(&evidence, "action evidence read failed: %v", err)
	}
	for _, row := range rows {
		if len(row) != 7 {
			continue
		}
		fmt.Fprintf(&evidence, "action %s state=%s version=%s launch=%s workload=%s observation=%s stop=%s; ",
			row[0], row[1], row[2], row[3], row[4], row[5], row[6])
	}
	containers, err := h.Containers(ctx, runtimepodman.LabelExecution, executionID)
	if err != nil {
		fmt.Fprintf(&evidence, "container list failed: %v", err)
		return evidence.String()
	}
	if len(containers) == 0 {
		evidence.WriteString("no Podman container carries the execution label")
		return evidence.String()
	}
	for _, name := range containers {
		result, err := runtimesupport.RunPodman(ctx, "inspect", "--format",
			"status={{.State.Status}} exit_code={{.State.ExitCode}} started_at={{.State.StartedAt}} error={{printf \"%q\" .State.Error}}", name)
		if err != nil {
			fmt.Fprintf(&evidence, "container %s inspect failed: %v; ", name, err)
			continue
		}
		fmt.Fprintf(&evidence, "container %s %s; ", name, strings.TrimSpace(string(result.Stdout)))
	}
	return evidence.String()
}

// OutputMaterializations returns every host workload root under this harness
// that materialized an earlier output directory for the named action. The
// adapter prepares /kodama/outputs/<action>/ under the workload's activation
// root, so a non-empty result proves a following action was prepared with that
// output; an empty result proves no such materialization exists.
func (h *Harness) OutputMaterializations(actionName string) ([]string, error) {
	h.T.Helper()
	entries, err := os.ReadDir(h.runtimeRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var found []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(h.runtimeRoot, entry.Name(), "kodama", "outputs", actionName)
		if _, err := os.Stat(path); err == nil {
			found = append(found, path)
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	return found, nil
}

// AssertRetiredFixtureTokensRefused proves the served edge does not
// authenticate the repository-published credentials an earlier harness
// revision registered.
func (h *Harness) AssertRetiredFixtureTokensRefused(t *testing.T) {
	t.Helper()
	for _, retired := range retiredFixtureTokens {
		_, publicError := h.REST(retired, "execution.inspect", api.ExecutionInspectRequest{
			ExecutionID: "execution-not-used",
		}, nil, "")
		if publicError.Code != api.CodeUnauthenticated {
			t.Fatalf("retired published fixture credential was not refused: code=%q message=%q",
				publicError.Code, publicError.Message)
		}
	}
}

func (h *Harness) scopedCount(ctx context.Context, query string, args ...any) (int64, error) {
	rows, err := pgtest.ScopedQuery(ctx, h.runtimeDB, h.Scope, 1, query, args...)
	if err != nil {
		return 0, err
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return 0, fmt.Errorf("count query returned %d rows", len(rows))
	}
	var count int64
	if _, err := fmt.Sscan(rows[0][0], &count); err != nil {
		return 0, fmt.Errorf("parse count %q: %w", rows[0][0], err)
	}
	return count, nil
}

// Containers lists this fixture's containers carrying one exact label pair.
// The fixture selector is applied as well, so a sibling fixture's containers are
// never enumerated as this fixture's own.
func (h *Harness) Containers(ctx context.Context, key, value string) ([]string, error) {
	h.T.Helper()
	return listFixtureOwned(ctx, newFixtureCommandRunner(), fixtureContainers, h.fixture,
		[]string{"label=" + key + "=" + value})
}

// labelledLibpod is the real Libpod client with the run cleanup labels added to
// every container and workspace volume it creates. It is both the adapter's
// Libpod seam and the composition's fresh-only VolumeProvisioner.
type labelledLibpod struct {
	runtimepodman.Libpod
	provisioner app.VolumeProvisioner
	fixture     FixtureScope
}

var _ runtimepodman.Libpod = labelledLibpod{}
var _ app.VolumeProvisioner = labelledLibpod{}

// CreateContainer adds the fixture and process cleanup labels without changing
// the product identity labels or any other field.
func (client labelledLibpod) CreateContainer(ctx context.Context, spec runtimepodman.ContainerSpec) (runtimepodman.ContainerState, error) {
	spec.Labels = mergeLabels(spec.Labels, client.fixture.labels())
	return client.Libpod.CreateContainer(ctx, spec)
}

// CreateVolume adds the fixture and process cleanup labels to the deterministic
// workspace volume.
func (client labelledLibpod) CreateVolume(ctx context.Context, volume runtimepodman.Volume) (runtimepodman.Volume, error) {
	volume.Labels = mergeLabels(volume.Labels, client.fixture.labels())
	return client.provisioner.CreateVolume(ctx, volume)
}

func mergeLabels(base, added map[string]string) map[string]string {
	merged := make(map[string]string, len(base)+len(added))
	maps.Copy(merged, base)
	maps.Copy(merged, added)
	return merged
}

// composedValidity is the development/test credential validity reader: human
// session expiry comes from the explicit development authenticator and every
// other principal kind from the store-backed reader.
type composedValidity struct {
	dev   *app.DevAuthenticator
	store event.CredentialValidity
}

var _ event.CredentialValidity = composedValidity{}

// ValidUntil implements event.CredentialValidity.
func (validity composedValidity) ValidUntil(ctx context.Context, principal identity.Principal) (time.Time, error) {
	if principal.Kind == identity.KindHuman {
		return validity.dev.ValidUntil(ctx, principal)
	}
	return validity.store.ValidUntil(ctx, principal)
}

// DigestResolver is the test composition's image resolver. The workload image
// digest is resolved from the real Podman image store before the pipeline is
// authored, so the pipeline YAML is digest-pinned. This resolver returns that
// pinned reference unchanged and refuses a mutable tag: it never synthesizes a
// digest, so a test cannot accidentally run a pipeline whose digest did not come
// from the image the runtime will use.
type DigestResolver struct{}

var _ pipeline.Resolver = DigestResolver{}

// Resolve implements pipeline.Resolver.
func (DigestResolver) Resolve(ctx context.Context, request pipeline.ResolveRequest) (pipeline.ResolvedImage, error) {
	if err := ctx.Err(); err != nil {
		return pipeline.ResolvedImage{}, err
	}
	reference := strings.TrimSpace(request.Reference)
	index := strings.LastIndex(reference, "@sha256:")
	if index <= 0 || index+len("@sha256:") == len(reference) {
		return pipeline.ResolvedImage{}, fmt.Errorf("factory: image %q is not digest-pinned; resolve it from the Podman image store", reference)
	}
	return pipeline.ResolvedImage{
		Reference: reference[:index],
		Digest:    reference[index+1:],
	}, nil
}
