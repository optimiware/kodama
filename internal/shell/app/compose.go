package app

import (
	"context"
	"crypto/rand"
	"log/slog"
	"path/filepath"
	"slices"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/installation"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/app/bridges"
	processdriver "github.com/optimiware/kodama/internal/shell/app/drivers/process"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	shelllog "github.com/optimiware/kodama/internal/shell/log"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/runtime/process"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	"github.com/optimiware/kodama/internal/shell/store/units/readers"
	"github.com/optimiware/kodama/internal/shell/token"
)

// environment is the explicit adapter bundle every profile must supply.
type environment struct {
	store       store.Executor
	scanner     store.Scanner
	controller  store.Controller
	outbox      event.Outbox
	credentials store.CredentialLookup
	runtime     kernelengine.Runtime
	workspace   kernelengine.Workspace
	keys        keys.Provider
	cipher      secretCipher
	publisher   event.Publisher
	invalidator token.InvalidationPort
	clock       Clock
	ids         *bridges.IDs
	principal   authorize.CurrentPrincipal
	validity    event.CredentialValidity
	registry    pipeline.Resolver
	logSink     shelllog.RetainedSink
	// liveLog publishes bounded live log progress while an action runs. It is
	// nil only for a composition that injects no live-capable broker.
	liveLog shelllog.LivePublisher
	// retention is the durable store the retention sweeper reads and deletes
	// through. It is nil for compositions without a durable store.
	retention  installation.RetentionStore
	keyVersion secret.KeyVersion
	dev        *DevAuthenticator
	seed       *Bootstrap
	// readiness carries the durable dependency probes the operational
	// readiness route runs. It is nil for the development profile and the
	// injected test form, which declare no dependency checks.
	readiness *readinessDependencies
}

// secretCipher is the envelope cipher surface the composition consumes.
type secretCipher interface {
	Seal(context.Context, secret.AssociatedData, []byte) (secret.Envelope, error)
	Open(context.Context, secret.AssociatedData, secret.Envelope) ([]byte, error)
}

// builder owns the ordered startup stack and its reverse cleanup.
type builder struct {
	cfg    Config
	logger *slog.Logger
	stops  []namedStop
}

type namedStop struct {
	name string
	stop func(context.Context) error
}

func (builder *builder) before(name string) error {
	return builder.cfg.hooks.before(name)
}

func (builder *builder) addStop(name string, stop func(context.Context) error) {
	if stop == nil {
		return
	}
	builder.stops = append(builder.stops, namedStop{name: name, stop: stop})
}

// cleanup stops every registered dependency in reverse startup order.
func (builder *builder) cleanup(ctx context.Context) {
	for _, step := range slices.Backward(builder.stops) {

		builder.cfg.hooks.stoppedStep(step.name)
		_ = step.stop(ctx)
	}
	builder.stops = nil
}

// buildEnvironment constructs the profile's explicit adapter set.
func buildEnvironment(ctx context.Context, builder *builder) (*environment, error) {
	switch builder.cfg.Profile {
	case ProfileTest:
		if builder.cfg.Test != nil {
			return buildDurableTestEnvironment(ctx, builder)
		}
		return buildDevelopmentEnvironment(ctx, builder)
	case ProfileDevelopment:
		return buildDevelopmentEnvironment(ctx, builder)
	case ProfileProduction:
		return buildProductionEnvironment(ctx, builder)
	default:
		return nil, fault.New(fault.InvalidInput, "composition profile is not one of the declared profiles")
	}
}

func buildDevelopmentEnvironment(ctx context.Context, builder *builder) (*environment, error) {
	cfg := builder.cfg
	overrides := cfg.Adapters
	clock := overrides.Clock
	if clock == nil {
		clock = bridges.NewSystemClock()
	}
	ids := overrides.IDs
	if ids == nil {
		ids = bridges.NewSecureIDs()
	}
	var (
		executor    store.Executor
		scanner     store.Scanner
		controller  store.Controller
		outbox      event.Outbox
		credentials store.CredentialLookup
	)
	if overrides.Store != nil {
		executor = overrides.Store
	} else {
		memoryStore := memory.NewWithClock(clock.Now)
		executor = memoryStore
		scanner = memoryStore
		controller = memoryStore.Controller()
		outbox = memoryStore
		credentials = memoryStore
	}
	if overrides.Scanner != nil {
		scanner = overrides.Scanner
	} else if scanner == nil {
		if candidate, ok := executor.(store.Scanner); ok {
			scanner = candidate
		} else {
			return nil, fault.New(fault.InvalidInput, "development composition requires a store scanner")
		}
	}
	if overrides.Controller != nil {
		controller = overrides.Controller
	} else if controller == nil {
		if provider, ok := executor.(interface{ Controller() store.Controller }); ok {
			controller = provider.Controller()
		} else {
			return nil, fault.New(fault.InvalidInput, "development composition requires a store controller")
		}
	}
	if overrides.Outbox != nil {
		outbox = overrides.Outbox
	} else if outbox == nil {
		if candidate, ok := executor.(event.Outbox); ok {
			outbox = candidate
		} else {
			return nil, fault.New(fault.InvalidInput, "development composition requires an event outbox")
		}
	}
	if overrides.Credentials != nil {
		credentials = overrides.Credentials
	} else if credentials == nil {
		if candidate, ok := executor.(store.CredentialLookup); ok {
			credentials = candidate
		} else {
			return nil, fault.New(fault.InvalidInput, "development composition requires a credential lookup")
		}
	}

	keyProvider := overrides.Keys
	if keyProvider == nil {
		provider, err := keys.NewEphemeral(rand.Reader)
		if err != nil {
			return nil, err
		}
		keyProvider = provider
	}
	cipher, err := keys.NewCipher(keyProvider, rand.Reader)
	if err != nil {
		return nil, err
	}

	brokerImpl := overrides.Publisher
	invalidator := overrides.Invalidator
	var inProcess *inProcessBroker
	if brokerImpl == nil {
		inProcess, err = newInProcessBroker(clock)
		if err != nil {
			return nil, err
		}
		brokerImpl = inProcess
	}
	if invalidator == nil {
		if inProcess != nil {
			invalidator = inProcess
		} else if candidate, ok := brokerImpl.(token.InvalidationPort); ok {
			invalidator = candidate
		} else {
			return nil, fault.New(fault.InvalidInput, "development composition requires a token invalidation port")
		}
	}

	runtimeAdapter := overrides.Runtime
	workspace := overrides.Workspace
	runtimeStop := overrides.RuntimeCloser
	if runtimeAdapter == nil {
		if cfg.DataDir == "" {
			return nil, fault.New(fault.InvalidInput, "development process runtime requires a data directory")
		}
		driver, err := processdriver.New(processdriver.Config{
			StateDir: filepath.Join(cfg.DataDir, "process"),
		})
		if err != nil {
			return nil, err
		}
		kodamaURL := cfg.Installation.PublicURL
		if kodamaURL == "" {
			kodamaURL = "http://127.0.0.1:8080"
		}
		adapter, err := process.New(process.Config{
			Root:          filepath.Join(cfg.DataDir, "runtime"),
			WorkspaceRoot: filepath.Join(cfg.DataDir, "workspaces"),
			KodamaURL:     kodamaURL,
			Executor:      driver,
			Observer:      driver,
			Files:         driver,
			Clock:         clock,
			Cleanup:       process.CleanupPolicy{Attempts: 1},
		})
		if err != nil {
			return nil, err
		}
		runtimeAdapter = adapter
		if workspace == nil {
			workspace = adapter
		}
		runtimeStop = func(ctx context.Context) error { return driver.Close(ctx) }
	}
	if workspace == nil {
		candidate, ok := runtimeAdapter.(kernelengine.Workspace)
		if !ok {
			return nil, fault.New(fault.InvalidInput, "development composition requires a workspace adapter")
		}
		workspace = candidate
	}
	// The Podman adapter's Retain never creates a missing volume: a lost
	// workspace of an execution that already ran must surface as not_found. The
	// composition therefore provisions the deterministic owned volume only for
	// a never-activated workspace. The durable activation fact comes
	// from the store-backed controller seam unless the caller injects one.
	if (cfg.Profile == ProfileDevelopment || cfg.Profile == ProfileTest) &&
		cfg.Installation.Runtime == installation.RuntimePodman &&
		overrides.VolumeProvisioner != nil {
		activation := overrides.WorkspaceActivation
		if activation == nil {
			fact, err := newWorkspaceActivation(controller)
			if err != nil {
				return nil, err
			}
			activation = fact
		}
		decorated, err := newProvisioningWorkspace(workspace, overrides.VolumeProvisioner, activation)
		if err != nil {
			return nil, err
		}
		workspace = decorated
	}
	// The Kubernetes adapter's Retain never creates a missing claim either. The
	// composition therefore provisions the deterministic workspace claim before
	// Retain for a never-activated workspace, using the trusted
	// template. The decorator is installed only for the development/test
	// profiles on the Kubernetes runtime; production and the other runtimes are
	// untouched. A provisioner without a template refuses startup rather than
	// sending a request with empty storage fields.
	if (cfg.Profile == ProfileDevelopment || cfg.Profile == ProfileTest) &&
		cfg.Installation.Runtime == installation.RuntimeKubernetes &&
		overrides.ClaimProvisioner != nil {
		if cfg.WorkspacePVC == nil {
			return nil, fault.New(fault.InvalidInput,
				"kubernetes workspace claim provisioning requires a workspace claim template")
		}
		activation := overrides.WorkspaceActivation
		if activation == nil {
			fact, err := newWorkspaceActivation(controller)
			if err != nil {
				return nil, err
			}
			activation = fact
		}
		decorated, err := newKubeProvisioningWorkspace(workspace, overrides.ClaimProvisioner, activation, *cfg.WorkspacePVC)
		if err != nil {
			return nil, err
		}
		workspace = decorated
	}
	builder.addStop("runtime-driver", runtimeStop)

	brokerStop := overrides.PublisherCloser
	if brokerStop == nil && inProcess == nil {
		if closer, ok := brokerImpl.(interface{ Close(context.Context) error }); ok {
			brokerStop = closer.Close
		}
	}
	builder.addStop("broker", brokerStop)

	devAuthenticator, err := NewDevAuthenticator(clock, cfg.Dev.Tokens)
	if err != nil {
		return nil, err
	}
	principal := overrides.Principal
	if principal == nil {
		principal = devAuthenticator
	}
	validity := overrides.CredentialValidity
	if validity == nil {
		storeValidity, err := readers.NewCredentialValidity(executor)
		if err != nil {
			return nil, err
		}
		validity = &composedCredentialValidity{dev: devAuthenticator, store: storeValidity}
	}

	resolver := overrides.Registry
	if resolver == nil {
		resolver = devRegistry{}
	}
	sink := overrides.LogSink
	if sink == nil {
		if cfg.DataDir == "" {
			return nil, fault.New(fault.InvalidInput, "development retained logs require a data directory")
		}
		fileSink, err := shelllog.NewFileSink(shelllog.SinkConfig{
			Root:  filepath.Join(cfg.DataDir, "logs"),
			Clock: clock,
		})
		if err != nil {
			return nil, err
		}
		sink = fileSink
	}

	// Live progress is published through the same broker adapter whenever it
	// implements the live publisher seam. The bounded asynchronous
	// publisher is composed here exactly as in the durable builder, so a
	// development composition with a real broker behaves like production while
	// the in-process broker simply leaves live publication disabled.
	liveDownstream := overrides.LivePublisher
	if liveDownstream == nil {
		if candidate, ok := brokerImpl.(shelllog.LivePublisher); ok {
			liveDownstream = candidate
		}
	}
	var livePublisher shelllog.LivePublisher
	if liveDownstream != nil {
		async, err := shelllog.NewAsyncPublisher(shelllog.PublisherConfig{Downstream: liveDownstream})
		if err != nil {
			return nil, err
		}
		builder.addStop("live-log-publisher", async.Close)
		livePublisher = async
	}

	return &environment{
		store:       executor,
		scanner:     scanner,
		controller:  controller,
		outbox:      outbox,
		credentials: credentials,
		runtime:     runtimeAdapter,
		workspace:   workspace,
		keys:        keyProvider,
		cipher:      cipher,
		publisher:   brokerImpl,
		invalidator: invalidator,
		clock:       clock,
		ids:         ids,
		principal:   principal,
		validity:    validity,
		registry:    resolver,
		logSink:     sink,
		liveLog:     livePublisher,
		keyVersion:  secret.MinKeyVersion,
		dev:         devAuthenticator,
		seed:        cfg.Dev.Bootstrap,
	}, nil
}
