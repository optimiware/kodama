package app

// The durable form of ProfileTest: `kodama server --profile test`
// composes PostgreSQL, NATS, the Podman runtime, a local keyfile, OIDC humans
// and the file log sink through the same durable builder production uses. The
// fully injected harness form (Config.Test == nil) is pinned by
// TestProfileTestRequiresAndAcceptsFullyInjectedFakes.

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/installation"
	"github.com/optimiware/kodama/internal/shell/app/bridges"
	natsdriver "github.com/optimiware/kodama/internal/shell/app/drivers/nats"
	pgdriver "github.com/optimiware/kodama/internal/shell/app/drivers/postgres"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	"github.com/optimiware/kodama/internal/shell/integration/oidc"
	"github.com/optimiware/kodama/internal/shell/integration/registry"
	shelllog "github.com/optimiware/kodama/internal/shell/log"
	runtimepodman "github.com/optimiware/kodama/internal/shell/runtime/podman"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store/postgres"
	"github.com/optimiware/kodama/internal/shell/token"
	"github.com/optimiware/kodama/internal/shell/transport/httpapi"
)

const (
	durableRuntimeDSN    = "postgres://kodama_runtime:SENTINEL-RUNTIME-PW@db.test:5432/kodama?sslmode=disable"
	durableControllerDSN = "postgres://kodama_controller:SENTINEL-CONTROLLER-PW@db.test:5432/kodama?sslmode=disable"
	durablePublisherDSN  = "postgres://kodama_publisher:SENTINEL-PUBLISHER-PW@db.test:5432/kodama?sslmode=disable"
	durableAuthDSN       = "postgres://kodama_auth:SENTINEL-AUTH-PW@db.test:5432/kodama?sslmode=disable"
	durableKeyRef        = "keyfile:kek-v1"
)

// recordingVolumes is the fresh-only volume provisioner seam.
type recordingVolumes struct {
	mu      sync.Mutex
	created []string
}

func (volumes *recordingVolumes) CreateVolume(_ context.Context, volume runtimepodman.Volume) (runtimepodman.Volume, error) {
	volumes.mu.Lock()
	defer volumes.mu.Unlock()
	volumes.created = append(volumes.created, volume.Name)
	return volume, nil
}

// writeDurableKeyfile writes one 32-byte owner-only keyfile.
func writeDurableKeyfile(t *testing.T) string {
	t.Helper()
	key := make([]byte, secret.DataKeyBytes)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "kek-v1.key")
	if err := os.WriteFile(path, key, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// durableTestConfig is one complete durable test composition over fake
// pools (installed by the caller), a fake broker and a fake Podman runtime.
func durableTestConfig(t *testing.T, server *fakeNATSServer) Config {
	t.Helper()
	runtime := &fakeRuntime{}
	return Config{
		Profile: ProfileTest,
		Installation: installation.Config{
			Environment: installation.EnvironmentTest,
			State:       installation.StatePostgres,
			Messaging:   installation.MessagingNATS,
			Runtime:     installation.RuntimePodman,
			Keys:        installation.KeyKeyfile,
			DatabaseURL: durableRuntimeDSN,
			NATSURL:     "nats://nats.test:4222",
			KeyRef:      durableKeyRef,
			PublicURL:   "https://kodama.test:8443",
		},
		InstallationID: "installation-test-1",
		DataDir:        t.TempDir(),
		LoopInterval:   time.Millisecond,
		Test: &TestConfig{
			Runtime:           runtime,
			Workspace:         runtime,
			VolumeProvisioner: &recordingVolumes{},
			RuntimeReady:      func(context.Context) error { return nil },
			KeyfilePath:       writeDurableKeyfile(t),
			OIDC: oidc.Config{
				OIDCConfig:    identity.OIDCConfig{Issuer: "https://issuer.example", Audience: "kodama"},
				JWKSURL:       "https://issuer.example/jwks",
				Algorithms:    []string{oidc.AlgorithmRS256},
				HTTPClient:    &http.Client{Transport: noNetworkTransport{}},
				MaxJWKSBytes:  1 << 20,
				MaxTokenBytes: 1 << 20,
				TenantClaim:   "tenant",
				Clock:         time.Now,
			},
			Registry:      registry.Config{Transport: noNetworkTransport{}},
			ControllerURL: durableControllerDSN,
			PublisherURL:  durablePublisherDSN,
			AuthURL:       durableAuthDSN,
			dialNATS:      server.dial,
		},
	}
}

// closingPool is a role pool whose Close is counted, so ownership of every
// pool the durable builder opens is observable.
type closingPool struct {
	readinessProbePool
	dsn    string
	closes atomic.Int32
}

func (pool *closingPool) Close() { pool.closes.Add(1) }

func installClosingPools(t *testing.T) *[]*closingPool {
	t.Helper()
	var mu sync.Mutex
	pools := &[]*closingPool{}
	previous := newPostgresPool
	newPostgresPool = func(config pgdriver.Config) (postgresPool, error) {
		mu.Lock()
		defer mu.Unlock()
		pool := &closingPool{dsn: config.ConnString}
		*pools = append(*pools, pool)
		return pool, nil
	}
	t.Cleanup(func() { newPostgresPool = previous })
	return pools
}

func TestDurableTestCompositionUsesProductAdapters(t *testing.T) {
	server := &fakeNATSServer{}
	config := durableTestConfig(t, server).resolved()
	if err := config.validate(); err != nil {
		t.Fatalf("validate a complete durable test configuration: %v", err)
	}
	pools := installClosingPools(t)

	previousConsumers := productionConsumersHook
	var consumers productionConsumers
	productionConsumersHook = func(composed productionConsumers) { consumers = composed }
	t.Cleanup(func() { productionConsumersHook = previousConsumers })
	previousLazy := productionLazyHook
	var lazy *lazyPrincipal
	productionLazyHook = func(observed *lazyPrincipal) { lazy = observed }
	t.Cleanup(func() { productionLazyHook = previousLazy })

	builder := &builder{cfg: config, logger: config.logger()}
	environment, err := buildEnvironment(context.Background(), builder)
	if err != nil {
		t.Fatalf("buildEnvironment: %v", err)
	}
	t.Cleanup(func() { builder.cleanup(context.Background()) })

	var dialed []string
	for _, pool := range *pools {
		dialed = append(dialed, pool.dsn)
	}
	if want := []string{durableRuntimeDSN, durableControllerDSN, durablePublisherDSN, durableAuthDSN}; !slices.Equal(dialed, want) {
		t.Fatalf("role pools = %q; want %q", dialed, want)
	}
	if _, ok := environment.store.(*postgres.Store); !ok {
		t.Fatalf("store = %T; want the PostgreSQL store", environment.store)
	}
	if _, ok := environment.controller.(*postgres.Controller); !ok {
		t.Fatalf("controller = %T; want the PostgreSQL controller", environment.controller)
	}
	if _, ok := environment.outbox.(*postgres.Outbox); !ok {
		t.Fatalf("outbox = %T; want the PostgreSQL outbox", environment.outbox)
	}
	if _, ok := environment.credentials.(*postgres.Credentials); !ok {
		t.Fatalf("credential lookup = %T; want the PostgreSQL auth lookup", environment.credentials)
	}
	// Each consumer holds its own role pool.
	for _, check := range []struct {
		name  string
		value any
		path  []string
		want  string
	}{
		{"store", environment.store, []string{"db"}, durableRuntimeDSN},
		{"controller", environment.controller, []string{"db"}, durableControllerDSN},
		{"outbox", environment.outbox, []string{"db"}, durablePublisherDSN},
		{"credentials", environment.credentials, []string{"db"}, durableAuthDSN},
		{"resolver credential lookup", consumers.Resolver, []string{"lookup", "db"}, durableAuthDSN},
		{"resolver principal reader", consumers.Resolver, []string{"principals", "executor", "db"}, durableRuntimeDSN},
		{"manager deliverer controller", consumers.Manager, []string{"deliverer", "identities", "controller", "db"}, durableControllerDSN},
	} {
		if got := observedClosingPoolDSN(t, check.name, check.value, check.path...); got != check.want {
			t.Fatalf("%s pool = %q; want %q", check.name, got, check.want)
		}
	}

	// No development authenticator, seed or in-process broker.
	if environment.dev != nil || environment.seed != nil {
		t.Fatal("the durable test composition installed the development authenticator or first-admin seed")
	}
	if consumers.Resolver == nil || any(environment.principal) != any(consumers.Resolver) {
		t.Fatalf("principal = %T; want the composed token/OIDC resolver", environment.principal)
	}
	if lazy == nil {
		t.Fatal("the broker principal never received its token-manager delegate")
	}
	if fmt.Sprintf("%T", environment.publisher) != "*nats.Broker" {
		t.Fatalf("publisher = %T; want the NATS broker", environment.publisher)
	}

	// Streams converge exactly once at startup, before any publication.
	if server.ensureCount() != 1 || len(server.publishedIDs()) != 0 {
		t.Fatalf("stream provisioning = %d, publications = %d; want one provisioning and no publication",
			server.ensureCount(), len(server.publishedIDs()))
	}

	// The real keyfile seals: an independently constructed keyfile provider
	// opens an envelope the composition sealed.
	associated := secret.AssociatedData{InstallationID: "installation-test-1", TenantID: "tenant-1", SecretID: "probe", Name: "PROBE", KeyVersion: secret.MinKeyVersion}
	envelope, err := environment.cipher.Seal(context.Background(), associated, []byte("plaintext"))
	if err != nil {
		t.Fatalf("seal through the composed cipher: %v", err)
	}
	if envelope.KeyRef != secret.KeyRef(durableKeyRef) || environment.keyVersion != secret.MinKeyVersion {
		t.Fatalf("key reference/version = %q/%d; want %q/%d", envelope.KeyRef, environment.keyVersion, durableKeyRef, secret.MinKeyVersion)
	}
	reconstructed, err := keys.NewKeyfile(secret.KeyRef(durableKeyRef), config.Test.KeyfilePath, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	independent, err := keys.NewCipher(reconstructed, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if opened, err := independent.Open(context.Background(), associated, envelope); err != nil || string(opened) != "plaintext" {
		t.Fatalf("independent keyfile open = %q, %v; want the sealed plaintext", opened, err)
	}

	// Retained logs go to the file sink under the data directory.
	sink, ok := environment.logSink.(*shelllog.FileSink)
	if !ok {
		t.Fatalf("log sink = %T; want the file sink", environment.logSink)
	}
	if root := reflect.ValueOf(sink).Elem().FieldByName("root").String(); root != filepath.Join(config.DataDir, "logs") {
		t.Fatalf("retained log root = %q; want <data-dir>/logs", root)
	}

	// The Podman workspace is wrapped by the fresh-only volume provisioner and
	// the configured runtime is used as is.
	provisioning, ok := environment.workspace.(*provisioningWorkspace)
	if !ok {
		t.Fatalf("workspace = %T; want the fresh-only volume provisioning decorator", environment.workspace)
	}
	if provisioning.provisioner != config.Test.VolumeProvisioner {
		t.Fatal("the provisioning decorator does not use the configured volume provisioner")
	}
	if environment.runtime != config.Test.Runtime {
		t.Fatalf("runtime = %T; want the configured Podman runtime", environment.runtime)
	}

	// Readiness declares the four pools, the broker and the local keyfile and
	// runtime probes, never a cloud KMS.
	if environment.readiness == nil || len(environment.readiness.pools) != 4 ||
		len(environment.readiness.local) != 2 || environment.readiness.kms != nil {
		t.Fatalf("readiness dependencies = %+v; want four pools, NATS and the two local probes", environment.readiness)
	}
}

// observedClosingPoolDSN walks the named fields to the closingPool a consumer
// holds and returns its DSN.
func observedClosingPoolDSN(t *testing.T, label string, value any, path ...string) string {
	t.Helper()
	current := reflect.ValueOf(value)
	if !current.IsValid() {
		t.Fatalf("%s: seam value is invalid", label)
	}
	for _, name := range path {
		current = dereferenced(t, label, current)
		field := current.FieldByName(name)
		if !field.IsValid() {
			t.Fatalf("%s: %s has no field %q", label, current.Type(), name)
		}
		current = field
	}
	current = dereferenced(t, label, current)
	if current.Type() != reflect.TypeFor[closingPool]() {
		t.Fatalf("%s: seam resolves to %s; want the closing pool", label, current.Type())
	}
	return current.FieldByName("dsn").String()
}

func TestDurableTestOIDCAndDerivedCredentials(t *testing.T) {
	installReadinessPools(t)
	server := &fakeNATSServer{}
	config := durableTestConfig(t, server)

	previous := productionDerivationHook
	var derivation bridges.DerivationKeyConfig
	productionDerivationHook = func(observed bridges.DerivationKeyConfig) { derivation = observed }
	t.Cleanup(func() { productionDerivationHook = previous })

	app, err := New(context.Background(), config)
	if err != nil {
		t.Fatalf("New durable test composition: %v", err)
	}
	t.Cleanup(func() { _ = app.Close(context.Background()) })

	// The server opens the tenant derivation key with exactly the identity the
	// first-admin bootstrap seals with.
	if want := tenantDerivationAssociated("installation-test-1", secret.MinKeyVersion); derivation.Associated != want {
		t.Fatalf("derivation identity = %+v; want %+v", derivation.Associated, want)
	}

	// A static development credential authenticates nothing: no development
	// authenticator is composed and the token resolver is not wrapped by the
	// development combined resolver.
	recorder, _, publicError := restCall(t, app.Handler(), http.MethodGet, "/api/v1/executions/execution-1", "dev-token", nil, nil, "")
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("static credential status = %d (%+v); want 401", recorder.Code, publicError)
	}
	for _, credential := range []string{"dev-token", token.ServicePrefix + "forged", token.ExecutionPrefix + "forged"} {
		recorder, _, _ := restCall(t, app.Handler(), http.MethodGet, "/api/v1/executions/execution-1", credential, nil, nil, "")
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("credential %q status = %d; want 401", credential[:min(len(credential), 6)], recorder.Code)
		}
	}
}

func TestDurableTestReadinessAndCleanup(t *testing.T) {
	t.Run("readiness", func(t *testing.T) {
		pools := installReadinessPools(t)
		server := &fakeNATSServer{}
		config := durableTestConfig(t, server)
		var runtimeErr atomic.Pointer[error]
		var pings atomic.Int32
		config.Test.RuntimeReady = func(context.Context) error {
			pings.Add(1)
			if err := runtimeErr.Load(); err != nil {
				return *err
			}
			return nil
		}
		app, err := New(context.Background(), config)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		t.Cleanup(func() { _ = app.Close(context.Background()) })
		handler := app.Handler()
		if code, _ := readinessStatus(t, handler); code != http.StatusServiceUnavailable {
			t.Fatalf("readyz before start = %d; want 503", code)
		}
		markAppStarted(t, app)
		if code, body := readinessStatus(t, handler); code != http.StatusOK {
			t.Fatalf("readyz = %d %q; want 200", code, body)
		}
		if pings.Load() != 1 {
			t.Fatalf("Podman pings = %d; want one per probe", pings.Load())
		}
		for index, pool := range pools {
			if begins, rollbacks := pool.counts(); begins != 1 || rollbacks != 1 {
				t.Fatalf("pool %d begins=%d rollbacks=%d; want 1/1", index, begins, rollbacks)
			}
		}
		breakers := []struct {
			name    string
			breakIt func()
			heal    func()
		}{
			{"runtime pool", func() { pools[0].setBeginError(errors.New("down")) }, func() { pools[0].setBeginError(nil) }},
			{"controller pool", func() { pools[1].setBeginError(errors.New("down")) }, func() { pools[1].setBeginError(nil) }},
			{"publisher pool", func() { pools[2].setBeginError(errors.New("down")) }, func() { pools[2].setBeginError(nil) }},
			{"auth pool", func() { pools[3].setBeginError(errors.New("down")) }, func() { pools[3].setBeginError(nil) }},
			{"NATS", func() { server.mu.Lock(); server.dialErr = errors.New("down"); server.mu.Unlock() },
				func() { server.mu.Lock(); server.dialErr = nil; server.mu.Unlock() }},
			{"keyfile permissions", func() { _ = os.Chmod(config.Test.KeyfilePath, 0o644) },
				func() { _ = os.Chmod(config.Test.KeyfilePath, 0o600) }},
			{"keyfile missing", func() { _ = os.Rename(config.Test.KeyfilePath, config.Test.KeyfilePath+".moved") },
				func() { _ = os.Rename(config.Test.KeyfilePath+".moved", config.Test.KeyfilePath) }},
			{"Podman", func() { failure := errors.New("podman down"); runtimeErr.Store(&failure) },
				func() { runtimeErr.Store(nil) }},
		}
		for _, breaker := range breakers {
			breaker.breakIt()
			if code, body := readinessStatus(t, handler); code != http.StatusServiceUnavailable || body != httpapi.ReadinessUnavailableBody {
				t.Fatalf("readyz with %s failing = %d %q; want 503", breaker.name, code, body)
			}
			if code, _ := livenessStatus(t, handler); code != http.StatusOK {
				t.Fatalf("healthz with %s failing = %d; want 200", breaker.name, code)
			}
			breaker.heal()
			if code, _ := readinessStatus(t, handler); code != http.StatusOK {
				t.Fatalf("readyz after %s recovered = %d; want 200", breaker.name, code)
			}
		}
	})

	t.Run("shutdown closes owned dependencies once and keeps durable workloads", func(t *testing.T) {
		pools := installClosingPools(t)
		server := &fakeNATSServer{}
		config := durableTestConfig(t, server)
		var runtimeCloses atomic.Int32
		config.Test.RuntimeCloser = func(context.Context) error { runtimeCloses.Add(1); return nil }
		app, err := New(context.Background(), config)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		for range 2 {
			if err := app.Close(context.Background()); err != nil {
				t.Fatalf("Close: %v", err)
			}
		}
		if len(*pools) != 4 {
			t.Fatalf("pools = %d; want 4", len(*pools))
		}
		for index, pool := range *pools {
			if closes := pool.closes.Load(); closes != 1 {
				t.Fatalf("pool %d closed %d times; want exactly once", index, closes)
			}
		}
		if runtimeCloses.Load() != 1 {
			t.Fatalf("runtime transport closed %d times; want once", runtimeCloses.Load())
		}
		if runtime := config.Test.Runtime.(*fakeRuntime); runtime.stopCount() != 0 || runtime.removeCount() != 0 {
			t.Fatal("shutdown stopped or removed a durable Podman workload")
		}
	})

	t.Run("partial construction closes what it opened", func(t *testing.T) {
		pools := installClosingPools(t)
		server := &fakeNATSServer{ensureErr: errors.New("stream conflict")}
		config := durableTestConfig(t, server)
		var runtimeCloses atomic.Int32
		config.Test.RuntimeCloser = func(context.Context) error { runtimeCloses.Add(1); return nil }
		if _, err := New(context.Background(), config); err == nil {
			t.Fatal("New accepted a failing stream provisioning")
		}
		if len(*pools) != 4 {
			t.Fatalf("pools = %d; want 4 before the failure", len(*pools))
		}
		for index, pool := range *pools {
			if closes := pool.closes.Load(); closes != 1 {
				t.Fatalf("pool %d closed %d times after a partial construction; want once", index, closes)
			}
		}
		if runtimeCloses.Load() != 1 {
			t.Fatalf("runtime transport closed %d times after a partial construction; want once", runtimeCloses.Load())
		}
	})
}

// TestProductionRefusesTestEntrySettings pins the explicit selection of the
// durable test form: it cannot enter development or production, cannot be
// mixed with development authentication, injected adapters or production
// adapters, and every incomplete setting is a refusal before any dial.
func TestProductionRefusesTestEntrySettings(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"production profile", func(c *Config) {
			production := validProductionConfig(t)
			test := c.Test
			*c = production
			c.Test = test
		}, "durable test"},
		{"development profile", func(c *Config) {
			development := newTestEnvironment(t).config
			test := c.Test
			*c = development
			c.Test = test
		}, "durable test"},
		{"development authentication", func(c *Config) {
			c.Dev = &DevelopmentConfig{Tokens: []DevToken{{Credential: "dev", Principal: identity.Principal{}, ExpiresAt: time.Now()}}}
		}, "development"},
		{"production adapters", func(c *Config) { c.Production = &ProductionConfig{} }, "production"},
		{"injected adapter", func(c *Config) { c.Adapters = &Adapters{Runtime: &fakeRuntime{}} }, "injected"},
		{"kubernetes runtime", func(c *Config) { c.Installation.Runtime = installation.RuntimeKubernetes }, "podman"},
		{"production environment", func(c *Config) { c.Installation.Environment = installation.EnvironmentProduction }, ""},
		{"http public URL", func(c *Config) { c.Installation.PublicURL = "http://kodama.test" }, "public URL"},
		{"relative data dir", func(c *Config) { c.DataDir = "data" }, "data directory"},
		{"no runtime", func(c *Config) { c.Test.Runtime = nil }, "runtime"},
		{"no workspace", func(c *Config) { c.Test.Workspace = nil }, "workspace"},
		{"no volume provisioner", func(c *Config) { c.Test.VolumeProvisioner = nil }, "volume provisioner"},
		{"no runtime probe", func(c *Config) { c.Test.RuntimeReady = nil }, "runtime readiness"},
		{"relative keyfile", func(c *Config) { c.Test.KeyfilePath = "kek.key" }, "keyfile"},
		{"no OIDC", func(c *Config) { c.Test.OIDC = oidc.Config{} }, "OIDC"},
		{"no registry", func(c *Config) { c.Test.Registry = registry.Config{} }, "registry"},
		{"no controller URL", func(c *Config) { c.Test.ControllerURL = "" }, "controller database URL"},
		{"no publisher URL", func(c *Config) { c.Test.PublisherURL = "" }, "publisher database URL"},
		{"no auth URL", func(c *Config) { c.Test.AuthURL = "" }, "auth database URL"},
		{"controller URL scheme", func(c *Config) { c.Test.ControllerURL = "http://db.test/kodama" }, "controller database URL"},
		{"auth URL host list", func(c *Config) { c.Test.AuthURL = "postgres://kodama_auth@a,b/kodama" }, "auth database URL"},
		{"broker certificate without key", func(c *Config) {
			c.Test.NATSTLS = &natsdriver.TLSConfig{CertFile: "/etc/tls.crt"}
		}, "broker"},
		{"workspace claim template", func(c *Config) { template := kubeTestTemplate(); c.WorkspacePVC = &template }, "claim"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			pools := installClosingPools(t)
			server := &fakeNATSServer{}
			config := durableTestConfig(t, server)
			testCase.mutate(&config)
			_, err := New(context.Background(), config)
			if err == nil {
				t.Fatalf("New accepted %s", testCase.name)
			}
			if class, _ := fault.ClassOf(err); class != fault.InvalidInput {
				t.Fatalf("refusal class = %q (%v); want invalid_input", class, err)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("refusal = %q; want it to mention %q", err, testCase.want)
			}
			if strings.Contains(err.Error(), "SENTINEL") {
				t.Fatalf("refusal rendered a credential: %q", err)
			}
			if len(*pools) != 0 || server.dialCount() != 0 {
				t.Fatalf("an invalid configuration opened %d pools and %d broker dials", len(*pools), server.dialCount())
			}
		})
	}
}
