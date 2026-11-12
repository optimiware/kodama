package app

import (
	"context"
	"crypto/rand"
	"path/filepath"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/app/bridges"
	natsdriver "github.com/optimiware/kodama/internal/shell/app/drivers/nats"
	pgdriver "github.com/optimiware/kodama/internal/shell/app/drivers/postgres"
	retentiondriver "github.com/optimiware/kodama/internal/shell/app/drivers/retention"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	natsbroker "github.com/optimiware/kodama/internal/shell/integration/nats"
	"github.com/optimiware/kodama/internal/shell/integration/oidc"
	"github.com/optimiware/kodama/internal/shell/integration/registry"
	shelllog "github.com/optimiware/kodama/internal/shell/log"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store/postgres"
	identitystore "github.com/optimiware/kodama/internal/shell/store/units/identity"
	"github.com/optimiware/kodama/internal/shell/store/units/readers"
	"github.com/optimiware/kodama/internal/shell/token"
)

// persistentDatabases are the four least-privilege role connection strings
// every durable composition (production and the durable test profile) opens.
type persistentDatabases struct {
	runtime    string
	controller string
	publisher  string
	auth       string
	// closePools registers each pool that owns connections for reverse
	// cleanup as soon as it exists. The durable test profile sets it;
	// production keeps its own shutdown sequence.
	closePools bool
}

// persistentStores are the PostgreSQL store consumers, each holding its own
// role pool, in composition order (runtime, controller, publisher, auth).
type persistentStores struct {
	pools       []postgresPool
	executor    *postgres.Store
	controller  *postgres.Controller
	outbox      *postgres.Outbox
	credentials *postgres.Credentials
}

// openPersistentStores builds the four role pools and their store consumers.
// With closePools, each pool that owns connections is registered for reverse
// cleanup as soon as it exists, so a later construction failure or shutdown
// closes it exactly once.
func openPersistentStores(builder *builder, databases persistentDatabases) (persistentStores, error) {
	pools := make([]postgresPool, 0, 4)
	for _, database := range []struct {
		name string
		url  string
	}{
		{"runtime", databases.runtime},
		{"controller", databases.controller},
		{"publisher", databases.publisher},
		{"auth", databases.auth},
	} {
		pool, err := newPostgresPool(pgdriver.Config{ConnString: database.url})
		if err != nil {
			return persistentStores{}, err
		}
		if closer, ok := pool.(interface{ Close() }); ok && databases.closePools {
			builder.addStop("postgres-"+database.name, func(context.Context) error {
				closer.Close()
				return nil
			})
		}
		pools = append(pools, pool)
	}
	return persistentStores{
		pools:       pools,
		executor:    postgres.New(pools[0]),
		controller:  postgres.NewController(pools[1]),
		outbox:      postgres.NewOutbox(pools[2]),
		credentials: postgres.NewCredentialLookup(pools[3]),
	}, nil
}

// persistentServices are the profile-specific inputs of the shared durable
// builder: the already routed runtime and workspace, the selected key
// provider and the broker, identity and registry settings.
type persistentServices struct {
	// label names the profile in configuration refusals.
	label         string
	runtime       kernelengine.Runtime
	workspace     kernelengine.Workspace
	provider      keys.Provider
	keyVersion    secret.KeyVersion
	natsTLS       *natsdriver.TLSConfig
	natsAuth      *NATSAuthConfig
	dial          natsDial
	oidc          oidc.Config
	registry      registry.Config
	streams       *StreamConfig
	runtimeCloser func(context.Context) error
}

// startNATSCallout opens the dedicated bypass connection and starts the
// per-credential authorization service. The service connection is separate
// from the publication client so a broker failure in one never silently moves
// the other onto a weaker path.
func startNATSCallout(
	ctx context.Context,
	builder *builder,
	services persistentServices,
	brokerConfig natsdriver.Config,
	dial natsDial,
	principal authorize.CurrentPrincipal,
	validity *readers.CredentialValidity,
	clock bridges.SystemClock,
) error {
	callout := services.natsAuth.Callout
	calloutConfig := brokerConfig
	calloutConfig.Name = "kodama-auth-callout"
	calloutConfig.User = callout.User
	calloutConfig.Password = callout.Password
	client, err := dial(ctx, calloutConfig)
	if err != nil {
		return err
	}
	connection, ok := client.(natsbroker.CalloutConnection)
	if !ok {
		_ = client.Close(ctx)
		return fault.New(fault.InternalFailure, "broker client does not support authorization callout")
	}
	service, err := natsbroker.NewCallout(natsbroker.CalloutConfig{
		Connection: connection,
		IssuerSeed: callout.IssuerSeed,
		Account:    callout.Account,
		Principal:  principal,
		Authorizer: authorize.NewMatrix(),
		Validity:   validity,
		Clock:      clock,
	})
	if err != nil {
		_ = client.Close(ctx)
		return err
	}
	if err := service.Start(ctx); err != nil {
		_ = client.Close(ctx)
		return err
	}
	builder.addStop("nats-callout", func(context.Context) error {
		_ = service.Close(context.Background())
		return client.Close(context.Background())
	})
	return nil
}

// buildPersistentEnvironment composes the durable broker, the token manager
// and OIDC/token principal resolver over current database memberships, the
// real registry and the file log sink, then provisions the broker streams. It
// is shared by production and the durable test profile; each caller keeps its
// own adapter selection and validation.
func buildPersistentEnvironment(ctx context.Context, builder *builder, stores persistentStores, services persistentServices) (*environment, error) {
	cfg := builder.cfg
	clock := bridges.NewSystemClock()
	ids := bridges.NewSecureIDs()
	executor := stores.executor

	cipher, err := keys.NewCipher(services.provider, rand.Reader)
	if err != nil {
		return nil, err
	}

	brokerConfig := natsdriver.Config{URL: cfg.Installation.NATSURL, TLS: services.natsTLS}
	if services.natsAuth != nil {
		brokerConfig.User = services.natsAuth.User
		brokerConfig.Password = services.natsAuth.Password
	}
	lazy := &lazyPrincipal{}
	dial := services.dial
	brokerAdapter, err := natsbroker.NewBroker(natsbroker.BrokerConfig{
		Connect: func(ctx context.Context) (natsbroker.Client, error) {
			client, err := dial(ctx, brokerConfig)
			if err != nil {
				return nil, err
			}
			if client == nil {
				return nil, fault.New(fault.DependencyUnavailable, "broker connection returned no client")
			}
			return client, nil
		},
		Clock:               clock,
		Principal:           lazy,
		Authorizer:          authorize.NewMatrix(),
		Sink:                bridges.NewObservationSink(),
		MaxReconnectBackoff: 5 * time.Second,
		IdleValidation:      time.Minute,
		MaxPublishAttempts:  3,
	})
	if err != nil {
		return nil, err
	}
	builder.addStop("broker", brokerAdapter.Close)

	identityAdapter, err := identitystore.New(identitystore.Config{
		Executor: executor,
		Scanner:  executor,
		Clock:    clock,
		IDs:      ids,
	})
	if err != nil {
		return nil, err
	}
	receiptLookup, err := readers.NewReceiptLookup(executor)
	if err != nil {
		return nil, err
	}
	deliverer, err := bridges.NewDeliverer(services.runtime, &executionIdentities{controller: stores.controller})
	if err != nil {
		return nil, err
	}
	associated := tenantDerivationAssociated(cfg.InstallationID, secret.MinKeyVersion)
	derivationConfig := bridges.DerivationKeyConfig{
		Cipher:     cipher,
		Associated: associated,
	}
	if productionDerivationHook != nil {
		productionDerivationHook(derivationConfig)
	}
	derivation, err := bridges.NewDerivationKeyResolver(derivationConfig)
	if err != nil {
		return nil, err
	}
	manager, err := token.NewManager(token.ManagerConfig{
		Keys:          identityAdapter,
		Resolver:      derivation,
		Credentials:   identityAdapter,
		Principals:    identityAdapter,
		Services:      identityAdapter,
		Mutations:     identityAdapter,
		Receipts:      receiptLookup,
		Deliverer:     deliverer,
		Invalidations: brokerAdapter,
		Clock:         clock,
		Random:        rand.Reader,
	})
	if err != nil {
		return nil, err
	}
	verifier, err := oidc.NewVerifier(services.oidc)
	if err != nil {
		return nil, err
	}
	principal, err := token.NewResolver(token.ResolverConfig{
		Lookup:      stores.credentials,
		Credentials: manager,
		OIDC:        verifier,
		Principals:  identityAdapter,
		Clock:       clock,
	})
	if err != nil {
		return nil, err
	}
	lazy.set(principal)
	if productionLazyHook != nil {
		productionLazyHook(lazy)
	}
	if productionConsumersHook != nil {
		productionConsumersHook(productionConsumers{
			Identity:  identityAdapter,
			Receipts:  receiptLookup,
			Deliverer: deliverer,
			Manager:   manager,
			Resolver:  principal,
		})
	}
	validity, err := readers.NewCredentialValidity(executor)
	if err != nil {
		return nil, err
	}
	if services.natsAuth != nil && services.natsAuth.Callout != nil {
		if err := startNATSCallout(ctx, builder, services, brokerConfig, dial, principal, validity, clock); err != nil {
			return nil, err
		}
	}
	registryResolver, err := registry.New(services.registry)
	if err != nil {
		return nil, err
	}
	if cfg.DataDir == "" {
		return nil, fault.New(fault.InvalidInput, services.label+" retained logs require a data directory")
	}
	sink, err := shelllog.NewFileSink(shelllog.SinkConfig{
		Root:  filepath.Join(cfg.DataDir, "logs"),
		Clock: clock,
	})
	if err != nil {
		return nil, err
	}
	// Live progress is published on Core NATS through the same broker adapter
	// that carries committed events, but through the bounded asynchronous
	// publisher: a slow or failed broker drops live progress and never blocks
	// workload observation. The retained sink above stays the complete source.
	liveLog, err := shelllog.NewAsyncPublisher(shelllog.PublisherConfig{Downstream: brokerAdapter})
	if err != nil {
		return nil, err
	}
	builder.addStop("live-log-publisher", liveLog.Close)
	builder.addStop("runtime-driver", services.runtimeCloser)

	// Provision the streams the broker publishes into before any publication
	// loop can start. A refusal, conflict or dial failure is a startup failure;
	// New stops every dependency registered above in reverse order.
	if err := provisionStreams(ctx, brokerConfig, services.streams, dial); err != nil {
		return nil, err
	}

	return &environment{
		store:       executor,
		scanner:     executor,
		controller:  stores.controller,
		outbox:      stores.outbox,
		credentials: stores.credentials,
		runtime:     services.runtime,
		workspace:   services.workspace,
		keys:        services.provider,
		cipher:      cipher,
		publisher:   brokerAdapter,
		invalidator: brokerAdapter,
		clock:       clock,
		ids:         ids,
		principal:   principal,
		validity:    validity,
		registry:    registryResolver,
		logSink:     sink,
		liveLog:     liveLog,
		retention:   retentiondriver.NewRetentionStore(stores.pools[0]),
		keyVersion:  services.keyVersion,
		readiness: &readinessDependencies{
			pools:  stores.pools,
			dial:   dial,
			broker: brokerConfig,
		},
	}, nil
}
