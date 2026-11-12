package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/app/bridges"
	natsdriver "github.com/optimiware/kodama/internal/shell/app/drivers/nats"
	pgdriver "github.com/optimiware/kodama/internal/shell/app/drivers/postgres"
	shellidentity "github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	shelllog "github.com/optimiware/kodama/internal/shell/log"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	storepostgres "github.com/optimiware/kodama/internal/shell/store/postgres"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	tenancyadmin "github.com/optimiware/kodama/internal/shell/tenancy/admin"
	"github.com/optimiware/kodama/internal/shell/token"
)

// TestProductionPassesBrokerTLSConfigurationToEveryDial proves the broker TLS
// configuration reaches both the startup stream-provisioning connection and the
// broker client that publishes later. A production install whose stream
// provisioning silently dialed a different configuration would fail only at the
// first publication.
func TestProductionPassesBrokerTLSConfigurationToEveryDial(t *testing.T) {
	server := &fakeNATSServer{}
	config := productionConfigWithServer(t, server).resolved()
	tlsConfig := &natsdriver.TLSConfig{
		CAFile:   "/etc/kodama/broker-tls/ca.crt",
		CertFile: "/etc/kodama/broker-tls/tls.crt",
		KeyFile:  "/etc/kodama/broker-tls/tls.key",
	}
	config.Production.NATSTLS = tlsConfig

	builder := &builder{cfg: config, logger: config.logger()}
	environment, err := buildEnvironment(context.Background(), builder)
	if err != nil {
		t.Fatalf("buildEnvironment: %v", err)
	}
	t.Cleanup(func() { builder.cleanup(context.Background()) })

	provisioned := server.dialConfigs()
	if len(provisioned) != 1 {
		t.Fatalf("stream provisioning dials = %d; want 1", len(provisioned))
	}
	if provisioned[0].TLS != tlsConfig {
		t.Fatalf("stream provisioning TLS = %+v; want the configured broker TLS", provisioned[0].TLS)
	}
	if provisioned[0].URL != config.Installation.NATSURL {
		t.Fatalf("stream provisioning URL = %q; want %q", provisioned[0].URL, config.Installation.NATSURL)
	}

	if err := environment.publisher.Publish(context.Background(), streamTestEnvelope(t)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	dials := server.dialConfigs()
	if len(dials) != 2 {
		t.Fatalf("dials after publication = %d; want 2", len(dials))
	}
	if dials[1].TLS != tlsConfig || dials[1].URL != config.Installation.NATSURL {
		t.Fatalf("broker dial configuration = %+v; want the configured TLS and URL", dials[1])
	}
}

// TestProductionClaimProvisioningUsesBothHalves proves the production profile
// accepts the workspace claim template and claim provisioner together and
// composes the claim-provisioning decorator, so an execution's first activation
// creates its claim instead of requiring an externally pre-created one.
func TestProductionClaimProvisioningUsesBothHalves(t *testing.T) {
	server := &fakeNATSServer{}
	config := productionConfigWithServer(t, server)
	template := kubeTestTemplate()
	config.WorkspacePVC = &template
	config.Production.ClaimProvisioner = &recordingClaimProvisioner{}

	builder := &builder{cfg: config.resolved(), logger: config.logger()}
	environment, err := buildEnvironment(context.Background(), builder)
	if err != nil {
		t.Fatalf("buildEnvironment: %v", err)
	}
	t.Cleanup(func() { builder.cleanup(context.Background()) })

	if _, ok := environment.workspace.(*kubeProvisioningWorkspace); !ok {
		t.Fatalf("production workspace = %T; want the claim-provisioning decorator", environment.workspace)
	}
}

// TestProductionRefusesBrokerTLSWithoutAClientCertificate pins the app-level
// half of the mTLS requirement: even a caller that bypasses cmd/** cannot build
// a production composition whose broker has no client certificate.
func TestProductionRefusesBrokerTLSWithoutAClientCertificate(t *testing.T) {
	server := &fakeNATSServer{}
	config := productionConfigWithServer(t, server)
	config.Production.NATSTLS = nil

	if _, err := New(context.Background(), config); err == nil {
		t.Fatal("New accepted a production composition without broker TLS")
	}

	config = productionConfigWithServer(t, server)
	config.Production.NATSTLS.CertFile = ""
	if _, err := New(context.Background(), config); err == nil {
		t.Fatal("New accepted a production broker without a client certificate")
	}

	// The shared fixture is still valid: the refusal above is about the TLS
	// half, not an unrelated configuration error.
	config = productionConfigWithServer(t, server)
	app, err := New(context.Background(), config)
	if err != nil {
		t.Fatalf("New with broker TLS: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// recordingPool is the test's per-role connection seam. Its identity is the
// DSN it was built with; Begin records that identity and then fails, so the
// consumer that reached it is observable without a live server even when the
// consumer wraps the cause in a typed fault that never renders it.
type recordingPool struct {
	dsn   string
	touch func(string)
}

func (pool *recordingPool) Begin(context.Context) (storepostgres.Conn, error) {
	if pool.touch != nil {
		pool.touch(pool.dsn)
	}
	return nil, fmt.Errorf("recording pool %s", pool.dsn)
}

// TestProductionBuildsEachPoolFromItsOwnRoleDSN proves the four pools carry
// their own role's connection string AND that every consumer holds the pool of
// its own role. Call order alone is not enough: swapping two constructor
// arguments survives it. Each recording pool reports every Begin to one shared
// recorder, so one operation per consumer proves which pool that consumer
// actually reaches, including consumers that wrap their cause in a typed fault
// and would otherwise reveal nothing. The runtime, controller, publisher and
// auth roles have disjoint least-privilege grants, so a swap would silently
// give one consumer another role's rights. The table enumerates every consumer
// production.go builds from a pool; the positive control is per row: an
// operation that never reaches a recording pool fails instead of passing
// vacuously. It also pins that the broker's lazy principal receives its
// token-manager delegate during composition.
func TestProductionBuildsEachPoolFromItsOwnRoleDSN(t *testing.T) {
	server := &fakeNATSServer{}
	config := productionConfigWithServer(t, server).resolved()
	const (
		runtimeDSN    = "postgres://kodama_runtime:SENTINEL-RUNTIME-PW@runtime-host:5432/kodama?sslmode=verify-full"
		controllerDSN = "postgres://kodama_controller:SENTINEL-CONTROLLER-PW@controller-host:5432/kodama?sslmode=verify-full"
		publisherDSN  = "postgres://kodama_publisher:SENTINEL-PUBLISHER-PW@publisher-host:5432/kodama?sslmode=verify-full"
		authDSN       = "postgres://kodama_auth:SENTINEL-AUTH-PW@auth-host:5432/kodama?sslmode=verify-full"
	)
	config.Installation.DatabaseURL = runtimeDSN
	config.Production.ControllerURL = controllerDSN
	config.Production.PublisherURL = publisherDSN
	config.Production.AuthURL = authDSN

	var mu sync.Mutex
	var dialed []string
	var touched []string
	record := func(dsn string) {
		mu.Lock()
		touched = append(touched, dsn)
		mu.Unlock()
	}
	resetTouched := func() {
		mu.Lock()
		touched = nil
		mu.Unlock()
	}
	touchedPools := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), touched...)
	}
	previousPool := newPostgresPool
	newPostgresPool = func(poolConfig pgdriver.Config) (postgresPool, error) {
		mu.Lock()
		dialed = append(dialed, poolConfig.ConnString)
		mu.Unlock()
		return &recordingPool{dsn: poolConfig.ConnString, touch: record}, nil
	}
	t.Cleanup(func() { newPostgresPool = previousPool })

	previousHook := productionLazyHook
	var observed *lazyPrincipal
	productionLazyHook = func(lazy *lazyPrincipal) { observed = lazy }
	t.Cleanup(func() { productionLazyHook = previousHook })

	previousConsumers := productionConsumersHook
	var consumers productionConsumers
	productionConsumersHook = func(composed productionConsumers) { consumers = composed }
	t.Cleanup(func() { productionConsumersHook = previousConsumers })

	// Claim provisioning is part of the production composition and adds the
	// store-backed workspace activation consumer, so compose it here.
	template := kubeTestTemplate()
	config.WorkspacePVC = &template
	config.Production.ClaimProvisioner = &recordingClaimProvisioner{}

	builder := &builder{cfg: config, logger: config.logger()}
	environment, err := buildEnvironment(context.Background(), builder)
	if err != nil {
		t.Fatalf("buildEnvironment: %v", err)
	}
	t.Cleanup(func() { builder.cleanup(context.Background()) })

	if want := []string{runtimeDSN, controllerDSN, publisherDSN, authDSN}; !slices.Equal(dialed, want) {
		t.Fatalf("pool constructor DSNs = %q; want %q (runtime, controller, publisher, auth)", dialed, want)
	}

	if consumers.Identity == nil || consumers.Receipts == nil || consumers.Deliverer == nil {
		t.Fatal("the production composition did not build every store-backed consumer")
	}
	provisioning, ok := environment.workspace.(*kubeProvisioningWorkspace)
	if !ok {
		t.Fatalf("production workspace = %T; want the claim-provisioning decorator", environment.workspace)
	}
	activation := provisioning.activation

	// One operation per consumer: every argument is valid enough to reach the
	// consumer's first database transaction, so an early refusal cannot mask a
	// mis-wired consumer and a consumer that never reaches its pool fails the
	// per-row positive control.
	ctx := context.Background()
	scope := tenancy.Scope{TenantID: "tenant-1"}
	servicePrincipal := shellidentity.Principal{ID: "service-1", Kind: shellidentity.KindService, Scope: scope}
	operations := []struct {
		name string
		run  func() error
		want string
	}{
		{
			name: "store executor (runtime)",
			run: func() error {
				_, err := environment.store.Read(ctx, scope, "tenants", "tenant-1")
				return err
			},
			want: runtimeDSN,
		},
		{
			name: "environment scanner (runtime)",
			run: func() error {
				_, err := environment.scanner.Query(ctx, scope, store.ScanQuery{Table: "tenants", Limit: 1})
				return err
			},
			want: runtimeDSN,
		},
		{
			name: "controller (controller)",
			run: func() error {
				_, err := environment.controller.Read(ctx, "executions", "execution-1")
				return err
			},
			want: controllerDSN,
		},
		{
			name: "event outbox (publisher)",
			run: func() error {
				_, err := environment.outbox.Pending(ctx, 0, 1)
				return err
			},
			want: publisherDSN,
		},
		{
			name: "credential lookup (auth)",
			run: func() error {
				_, err := environment.credentials.LookupCredential(ctx, store.CredentialExecution, "credential-1")
				return err
			},
			want: authDSN,
		},
		{
			name: "identity store executor (runtime)",
			run: func() error {
				_, err := consumers.Identity.ReadTenantKey(ctx, token.TenantKeyRequest{TenantID: "tenant-1"})
				return err
			},
			want: runtimeDSN,
		},
		{
			name: "identity store scanner (runtime)",
			run: func() error {
				_, err := consumers.Identity.ListMemberships(ctx, tenancyadmin.MembershipQuery{Scope: scope, Limit: 1})
				return err
			},
			want: runtimeDSN,
		},
		{
			name: "receipt lookup (runtime)",
			run: func() error {
				_, _, err := consumers.Receipts.Lookup(ctx, receipt.Key{
					TenantID:    "tenant-1",
					PrincipalID: "principal-1",
					Operation:   "task.read",
					CallerKey:   "caller-1",
				})
				return err
			},
			want: runtimeDSN,
		},
		{
			name: "credential validity (runtime)",
			run: func() error {
				_, err := environment.validity.ValidUntil(ctx, servicePrincipal)
				return err
			},
			want: runtimeDSN,
		},
		{
			name: "deliverer execution identity (controller)",
			run: func() error {
				return consumers.Deliverer.Deliver(ctx, token.Delivery{
					Scope:       scope,
					PrincipalID: "principal-1",
					ExecutionID: "execution-1",
					Kind:        token.MutationIssue,
					Token:       "delivery-token",
					Target:      "delivery-target",
				})
			},
			want: controllerDSN,
		},
		{
			name: "workspace activation (controller)",
			run: func() error {
				_, err := activation.PreviouslyActivated(ctx, "workspace-1")
				return err
			},
			want: controllerDSN,
		},
		{
			name: "token resolver credential lookup (auth)",
			run: func() error {
				_, err := environment.principal.Resolve(ctx, token.ExecutionPrefix+"credential-1.signature")
				return err
			},
			want: authDSN,
		},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			resetTouched()
			err := operation.run()
			reached := touchedPools()
			if len(reached) == 0 {
				t.Fatalf("%s never reached a recording pool (positive control): err=%v", operation.name, err)
			}
			if !slices.Equal(reached, []string{operation.want}) {
				t.Fatalf("%s reached pools %q; want exactly [%q] (err=%v)", operation.name, reached, operation.want, err)
			}
			if err == nil {
				t.Fatalf("%s unexpectedly succeeded against a failing recording pool", operation.name)
			}
		})
	}

	// The behavioral table proves the reachable consumers. These seams prove
	// the composed credential boundaries were built with the same pools: a
	// manager or resolver that was handed a different deliverer, receipt
	// lookup or identity adapter than the ones above would otherwise stay
	// invisible behind a successful path. Every walk fails loudly when a seam
	// field is renamed, so the assertion cannot become vacuous.
	seams := []struct {
		name  string
		value any
		path  []string
		want  string
	}{
		{"manager tenant keys (identity executor, runtime)", consumers.Manager, []string{"keys", "executor", "db"}, runtimeDSN},
		{"manager credential reader (identity executor, runtime)", consumers.Manager, []string{"credentials", "executor", "db"}, runtimeDSN},
		{"manager principal reader (identity executor, runtime)", consumers.Manager, []string{"principals", "executor", "db"}, runtimeDSN},
		{"manager service reader (identity executor, runtime)", consumers.Manager, []string{"services", "executor", "db"}, runtimeDSN},
		{"manager mutation port (identity executor, runtime)", consumers.Manager, []string{"mutations", "executor", "db"}, runtimeDSN},
		{"manager scanner seam (identity scanner, runtime)", consumers.Manager, []string{"keys", "scanner", "db"}, runtimeDSN},
		{"manager receipt lookup (runtime)", consumers.Manager, []string{"receipts", "scanner", "db"}, runtimeDSN},
		{"manager deliverer controller (controller)", consumers.Manager, []string{"deliverer", "identities", "controller", "db"}, controllerDSN},
		{"resolver credential lookup (auth)", consumers.Resolver, []string{"lookup", "db"}, authDSN},
		{"resolver credential manager (identity executor, runtime)", consumers.Resolver, []string{"credentials", "keys", "executor", "db"}, runtimeDSN},
		{"resolver principal reader (identity executor, runtime)", consumers.Resolver, []string{"principals", "executor", "db"}, runtimeDSN},
	}
	for _, seam := range seams {
		t.Run(seam.name, func(t *testing.T) {
			if got := observedPoolDSN(t, seam.name, seam.value, seam.path...); got != seam.want {
				t.Fatalf("%s resolves to pool %q; want %q", seam.name, got, seam.want)
			}
		})
	}

	if observed == nil {
		t.Fatal("the production composition never installed the broker's lazy principal delegate")
	}
	observed.mu.RLock()
	installed := observed.delegate
	observed.mu.RUnlock()
	if installed == nil {
		t.Fatal("the broker's lazy principal was left without a delegate")
	}
	if environment.principal != installed {
		t.Fatal("the lazy principal's delegate is not the composed token resolver")
	}
}

// TestProductionInstallsTheBootstrapDerivationIdentity proves the production
// composition opens tenant derivation keys with the exact associated identity
// bootstrap-admin seals them with: the configured installation id plus the
// fixed secret id and name. A changed installation wiring, secret id or name
// leaves the sealed key unopenable, so it fails here instead of at first use.
func TestProductionInstallsTheBootstrapDerivationIdentity(t *testing.T) {
	server := &fakeNATSServer{}
	config := productionConfigWithServer(t, server).resolved()
	config.InstallationID = "installation-sentinel"

	previousHook := productionDerivationHook
	var observed secret.AssociatedData
	var receivedCipher secretCipher
	productionDerivationHook = func(derivation bridges.DerivationKeyConfig) {
		observed = derivation.Associated
		receivedCipher = derivation.Cipher
	}
	t.Cleanup(func() { productionDerivationHook = previousHook })

	builder := &builder{cfg: config, logger: config.logger()}
	if _, err := buildEnvironment(context.Background(), builder); err != nil {
		t.Fatalf("buildEnvironment: %v", err)
	}
	t.Cleanup(func() { builder.cleanup(context.Background()) })

	// The hook observes the exact configuration handed to the resolver
	// constructor, not a separately recomputed value, so changing the literal
	// the resolver receives fails here.
	if receivedCipher == nil {
		t.Fatal("the derivation-key resolver was constructed without the envelope cipher")
	}
	if observed.InstallationID != "installation-sentinel" {
		t.Fatalf("associated installation = %q; want the composed installation id", observed.InstallationID)
	}
	if observed.SecretID != secret.SecretID("tenant-derivation-key") {
		t.Fatalf("associated secret id = %q; want tenant-derivation-key", observed.SecretID)
	}
	if observed.Name != id.SecretName("TENANT_DERIVATION_KEY") {
		t.Fatalf("associated secret name = %q; want TENANT_DERIVATION_KEY", observed.Name)
	}
	if observed.KeyVersion != secret.MinKeyVersion {
		t.Fatalf("associated key version = %d; want %d", observed.KeyVersion, secret.MinKeyVersion)
	}
}

// TestTenantDerivationAssociatedIdentityIsFixed pins the associated data the
// first-admin bootstrap seals a tenant derivation key with and the production
// server opens it with. Both sides call tenantDerivationAssociated; these
// literals are the fixed identities, so changing either the shared constant or
// the installation wiring is caught here rather than at first unseal.
func TestTenantDerivationAssociatedIdentityIsFixed(t *testing.T) {
	associated := tenantDerivationAssociated("installation-sentinel", secret.MinKeyVersion)
	if associated.InstallationID != "installation-sentinel" {
		t.Fatalf("associated installation = %q; want the configured installation id", associated.InstallationID)
	}
	if associated.SecretID != secret.SecretID("tenant-derivation-key") {
		t.Fatalf("associated secret id = %q; want tenant-derivation-key", associated.SecretID)
	}
	if associated.Name != id.SecretName("TENANT_DERIVATION_KEY") {
		t.Fatalf("associated secret name = %q; want TENANT_DERIVATION_KEY", associated.Name)
	}
	if associated.KeyVersion != secret.MinKeyVersion {
		t.Fatalf("associated key version = %d; want %d", associated.KeyVersion, secret.MinKeyVersion)
	}
}

// recordingPrincipal is a CurrentPrincipal delegate that records resolution.
type recordingPrincipal struct {
	calls int
}

func (principal *recordingPrincipal) Resolve(context.Context, string) (shellidentity.Principal, error) {
	principal.calls++
	return shellidentity.Principal{ID: "principal-1"}, nil
}

// TestLazyPrincipalFailsClosedBeforeItsDelegateExists pins that the production
// broker principal resolver refuses until the token manager delegate is
// installed. It never returns a zero principal or an empty error, and it never
// renders the presented credential.
func TestLazyPrincipalFailsClosedBeforeItsDelegateExists(t *testing.T) {
	lazy := &lazyPrincipal{}
	const credential = "SENTINEL-PRESENTED-CREDENTIAL"
	if _, err := lazy.Resolve(context.Background(), credential); err == nil {
		t.Fatal("the lazy principal resolver accepted a credential before its delegate exists")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.Unauthenticated {
		t.Fatalf("refusal class = %v (ok=%v); want unauthenticated", class, ok)
	} else if strings.Contains(err.Error(), credential) {
		t.Fatalf("refusal rendered the credential: %v", err)
	}

	delegate := &recordingPrincipal{}
	lazy.set(delegate)
	principal, err := lazy.Resolve(context.Background(), credential)
	if err != nil {
		t.Fatalf("Resolve after set: %v", err)
	}
	if principal.ID != "principal-1" || delegate.calls != 1 {
		t.Fatalf("principal = %+v, delegate calls = %d; want the delegate's principal", principal, delegate.calls)
	}
}

// TestProductionPinsTheConfiguredKeyVersion proves the composed environment
// carries the configured cloud key version, not a hard-coded minimum: the
// tenant derivation keys sealed by bootstrap-admin and opened by the server are
// versioned with it.
func TestProductionPinsTheConfiguredKeyVersion(t *testing.T) {
	server := &fakeNATSServer{}
	config := productionConfigWithServer(t, server).resolved()
	const configured = secret.KeyVersion(9)
	config.Production.Cloud = keys.CloudConfig{
		KeyRef:   config.Production.Cloud.KeyRef,
		Current:  configured,
		Versions: map[secret.KeyVersion][]byte{configured: make([]byte, 32)},
	}

	builder := &builder{cfg: config, logger: config.logger()}
	environment, err := buildEnvironment(context.Background(), builder)
	if err != nil {
		t.Fatalf("buildEnvironment: %v", err)
	}
	t.Cleanup(func() { builder.cleanup(context.Background()) })

	if environment.keyVersion != configured {
		t.Fatalf("environment key version = %d; want the configured %d", environment.keyVersion, configured)
	}
	if configured == secret.MinKeyVersion {
		t.Fatal("the test key version must differ from the minimum to prove it is not hard-coded")
	}
}

// TestProductionRetainsLogsUnderTheDataDirLogsSubdirectory proves the retained
// log sink is rooted in the documented `logs` subdirectory of the configured
// data directory. The sink writes its state file under the root it was built
// with, so the exact path is observable without a live installation.
func TestProductionRetainsLogsUnderTheDataDirLogsSubdirectory(t *testing.T) {
	server := &fakeNATSServer{}
	config := productionConfigWithServer(t, server).resolved()
	dataDir := t.TempDir()
	config.DataDir = dataDir

	builder := &builder{cfg: config, logger: config.logger()}
	environment, err := buildEnvironment(context.Background(), builder)
	if err != nil {
		t.Fatalf("buildEnvironment: %v", err)
	}
	t.Cleanup(func() { builder.cleanup(context.Background()) })

	if _, err := environment.logSink.Append(context.Background(),
		tenancy.Scope{TenantID: "tenant-1"},
		shelllog.Chunk{ExecutionID: "execution-1", ActionID: "action-1", Stream: shelllog.StreamStdout, Data: []byte("retained-sentinel")},
	); err != nil {
		t.Fatalf("Append: %v", err)
	}
	retained := filepath.Join(dataDir, "logs", "retained-log.json")
	info, err := os.Stat(retained)
	if err != nil {
		t.Fatalf("retained log state is not under the data directory's logs subdirectory: %v", err)
	}
	if info.IsDir() {
		t.Fatalf("retained log state path %q is a directory", retained)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "retained-log.json")); err == nil {
		t.Fatalf("retained log state was written at the data directory root instead of its logs subdirectory")
	}
}

// TestProductionFailsClosedWhenAPoolConstructorFails proves every one of the
// four role pools is mandatory: a constructor error at any position fails the
// composition instead of continuing with a nil pool. The error is returned
// unchanged, so the operator sees which pool could not be built.
func TestProductionFailsClosedWhenAPoolConstructorFails(t *testing.T) {
	for failAt := range 4 {
		t.Run(fmt.Sprintf("pool %d fails", failAt), func(t *testing.T) {
			server := &fakeNATSServer{}
			config := productionConfigWithServer(t, server).resolved()

			var constructed int
			previousPool := newPostgresPool
			newPostgresPool = func(pgdriver.Config) (postgresPool, error) {
				index := constructed
				constructed++
				if index == failAt {
					return nil, fmt.Errorf("pool constructor sentinel %d", index)
				}
				return &recordingPool{dsn: fmt.Sprintf("pool-%d", index)}, nil
			}
			t.Cleanup(func() { newPostgresPool = previousPool })

			builder := &builder{cfg: config, logger: config.logger()}
			environment, err := buildEnvironment(context.Background(), builder)
			if err == nil {
				t.Fatalf("buildEnvironment accepted a failing pool constructor at position %d", failAt)
			}
			if environment != nil {
				t.Fatal("a failed pool construction still returned a partial environment")
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("pool constructor sentinel %d", failAt)) {
				t.Fatalf("refusal = %q; want the pool constructor's own error", err)
			}
		})
	}
}

// observedPoolDSN walks value through the named unexported fields (through
// interfaces and pointers) and returns the DSN of the recordingPool the seam
// actually reaches. A missing field or an unexpected type fails the test
// loudly, so renaming a seam cannot make the assertion vacuous.
func observedPoolDSN(t *testing.T, label string, value any, path ...string) string {
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
	if current.Type() != reflect.TypeFor[recordingPool]() {
		t.Fatalf("%s: seam resolves to %s; want the recording pool", label, current.Type())
	}
	dsn := current.FieldByName("dsn")
	if !dsn.IsValid() || dsn.Kind() != reflect.String {
		t.Fatalf("%s: recording pool has no dsn string field", label)
	}
	return dsn.String()
}

// dereferenced unwraps pointers and interfaces, refusing a nil the test would
// otherwise read as an empty seam.
func dereferenced(t *testing.T, label string, value reflect.Value) reflect.Value {
	t.Helper()
	for value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface {
		if value.IsNil() {
			t.Fatalf("%s: seam is nil at %s", label, value.Type())
		}
		value = value.Elem()
	}
	return value
}

// TestProductionComposesMultiTenantRouting proves the production composition
// builds the per-tenant dispatchers and installs them as the environment's
// runtime and workspace, so the Kernel, the credential deliverer, the tracked
// runtime and the log collector all route by persisted ownership instead of
// one installation-wide tenant. Each tenant's workspace is wrapped in the
// existing claim-provisioning decorator with that tenant's own template.
func TestProductionComposesMultiTenantRouting(t *testing.T) {
	server := &fakeNATSServer{}
	config := productionConfigWithServer(t, server)
	config.Production.Runtime = nil
	config.Production.Workspace = nil
	config.WorkspacePVC = nil
	provisioner := &recordingClaimProvisioner{}
	config.Production.ClaimProvisioner = provisioner

	template := func(tenant id.TenantID) *WorkspacePVC {
		value := kubeTestTemplate()
		value.TenantID = tenant
		value.Namespace = "kodama-tenant-" + string(tenant)
		return &value
	}
	runtimeA, runtimeB := &fakeRuntime{}, &fakeRuntime{}
	workspaceA, workspaceB := &fakeRuntime{}, &fakeRuntime{}
	config.Production.Tenants = []ProductionTenant{
		{TenantID: "tenant-a", Runtime: runtimeA, Workspace: workspaceA, WorkspacePVC: template("tenant-a")},
		{TenantID: "tenant-b", Runtime: runtimeB, Workspace: workspaceB, WorkspacePVC: template("tenant-b")},
	}

	builder := &builder{cfg: config.resolved(), logger: config.logger()}
	environment, err := buildEnvironment(context.Background(), builder)
	if err != nil {
		t.Fatalf("buildEnvironment: %v", err)
	}
	t.Cleanup(func() { builder.cleanup(context.Background()) })

	dispatcher, ok := environment.runtime.(*tenantRuntime)
	if !ok {
		t.Fatalf("environment runtime = %T; want the multi-tenant dispatcher", environment.runtime)
	}
	if len(dispatcher.tenants) != 2 {
		t.Fatalf("routed runtime tenants = %d; want one per configured tenant", len(dispatcher.tenants))
	}
	if dispatcher.tenants["tenant-a"] != runtimeA || dispatcher.tenants["tenant-b"] != runtimeB {
		t.Fatal("the dispatch table does not hold each tenant's own runtime adapter")
	}
	if _, ok := dispatcher.scopes.(*controllerExecutionScopes); !ok {
		t.Fatalf("runtime ownership reader = %T; want the durable controller scope reader", dispatcher.scopes)
	}

	workspaceDispatcher, ok := environment.workspace.(*tenantWorkspace)
	if !ok {
		t.Fatalf("environment workspace = %T; want the multi-tenant dispatcher", environment.workspace)
	}
	if len(workspaceDispatcher.tenants) != 2 {
		t.Fatalf("routed workspace tenants = %d; want one per configured tenant", len(workspaceDispatcher.tenants))
	}
	originalWorkspaces := map[id.TenantID]kernelengine.Workspace{"tenant-a": workspaceA, "tenant-b": workspaceB}
	for tenant, inner := range workspaceDispatcher.tenants {
		provisioning, ok := inner.(*kubeProvisioningWorkspace)
		if !ok {
			t.Fatalf("tenant %s workspace = %T; want the claim-provisioning decorator", tenant, inner)
		}
		if provisioning.claim.TenantID != tenant {
			t.Fatalf("tenant %s claim template tenant = %s; want its own tenant", tenant, provisioning.claim.TenantID)
		}
		if provisioning.claim.Namespace != "kodama-tenant-"+string(tenant) {
			t.Fatalf("tenant %s claim namespace = %s; want its own namespace", tenant, provisioning.claim.Namespace)
		}
		if provisioning.provisioner != provisioner {
			t.Fatalf("tenant %s claim provisioner = %T; want the composed cluster client", tenant, provisioning.provisioner)
		}
		if provisioning.inner != originalWorkspaces[tenant] {
			t.Fatalf("tenant %s decorator does not wrap that tenant's workspace", tenant)
		}
	}

	// Capabilities is installation-wide and must stay the safe intersection of
	// the served tenants' adapters.
	capabilities, err := environment.runtime.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if !capabilities.Supports("process") {
		t.Fatalf("capabilities = %v; want the capability both fake adapters support", capabilities.Names)
	}
}
