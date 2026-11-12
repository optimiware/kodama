package factory

// Controller restart fixture.
//
// RestartController models a controller restart without recreating any
// infrastructure: the PostgreSQL server, NATS server, keyfile, retained log
// sink, workload image, Podman service or kind cluster, public edge socket and
// forwarder, and every running workload all survive. Only the controller
// process is replaced: its Shell adapters (store pools, broker connection,
// runtime/workspace driver, principal resolver) and its App are rebuilt fresh
// over the same durable state.
//
// This is the fixture the restart regression needs. A memory-backed fake
// restart cannot show a restart defect because a fake runtime is not a durable
// workload: the real proof is that the same Kubernetes Job and Pod UID,
// workspace and deadline survive the restart, the command runs exactly once,
// the execution still succeeds and its retained logs stay readable. The
// restart deliberately does not call Harness.Close, which removes this
// fixture's workload resources; a controller shutdown must leave them running
// for the successor to adopt.

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/installation"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/app"
	"github.com/optimiware/kodama/internal/shell/app/bridges"
	driverkube "github.com/optimiware/kodama/internal/shell/app/drivers/kube"
	natsdriver "github.com/optimiware/kodama/internal/shell/app/drivers/nats"
	podmandriver "github.com/optimiware/kodama/internal/shell/app/drivers/podman"
	pgdriver "github.com/optimiware/kodama/internal/shell/app/drivers/postgres"
	"github.com/optimiware/kodama/internal/shell/authorize"
	natsbroker "github.com/optimiware/kodama/internal/shell/integration/nats"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
	runtimepodman "github.com/optimiware/kodama/internal/shell/runtime/podman"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
	identitystore "github.com/optimiware/kodama/internal/shell/store/units/identity"
	"github.com/optimiware/kodama/internal/shell/store/units/readers"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// controllerRestartTimeout bounds rebuilding one controller. It covers adapter
// construction and app.New only; it never includes infrastructure provisioning.
const controllerRestartTimeout = 2 * time.Minute

// RestartController stops the served controller without removing any
// infrastructure or workload, then builds fresh Shell adapters and a fresh App
// over the same durable state and serves them on the same public edge.
//
// The caller asserts the post-restart behavior (same workload identity, one
// command execution, eventual success and retained logs) through the normal
// Harness helpers.
func (h *Harness) RestartController() {
	h.T.Helper()
	h.stopController()
	ctx, cancel := context.WithTimeout(context.Background(), controllerRestartTimeout)
	defer cancel()
	config, discard, err := h.buildControllerConfig(ctx)
	if err != nil {
		h.T.Fatalf("factory: rebuild controller adapters: %v\ncomposition log:\n%s",
			err, SanitizeDiagnostic(h.logs.String()))
	}
	composed, err := app.New(ctx, config)
	if err != nil {
		discard()
		h.T.Fatalf("factory: restart app.New: %v\ncomposition log:\n%s",
			err, SanitizeDiagnostic(h.logs.String()))
	}
	if err := h.serveController(composed); err != nil {
		_ = composed.Close(context.WithoutCancel(ctx))
		h.T.Fatalf("factory: serve restarted controller: %v\ncomposition log:\n%s",
			err, SanitizeDiagnostic(h.logs.String()))
	}
	// The replacement controller is ready when the public edge answers again.
	h.Eventually(h.T, 30*time.Second, func() bool {
		response, err := h.mcpRaw(api.MCPMethodToolsList, nil, "")
		return err == nil && response.Error == nil
	})
}

// serveController starts serving one constructed controller on the retained
// public edge. The first controller uses the socket start bound before the
// forwarder was created; a restarted controller re-binds the same address: the
// workload's KODAMA_URL and the forwarder target name that port, and every
// workload started before the restart still dials it.
func (h *Harness) serveController(composed *app.App) error {
	listener := h.infra.rawListener
	if listener == nil || h.infra.listenerRetired {
		rebound, err := net.Listen("tcp", h.infra.listenAddr)
		if err != nil {
			return fmt.Errorf("rebind public edge %s: %w", h.infra.listenAddr, err)
		}
		listener = rebound
		h.infra.rawListener = rebound
		h.infra.listenerRetired = false
	}
	if h.TLS != nil {
		listener = tls.NewListener(listener, h.TLS.ServerConfig())
	}
	h.Listener = listener

	serveCtx, cancel := context.WithCancel(context.Background())
	h.App = composed
	h.cancel = cancel
	h.served = make(chan error, 1)
	h.serveDone = make(chan struct{})
	// The serving goroutine only reports the result. stopController owns the
	// join barrier: it publishes serveDone itself after it has received the
	// result, so a non-blocking receive on ServingDone after Close is proof the
	// serving goroutine was joined, not merely that the scheduler got to it.
	served := h.served
	go func() {
		served <- composed.Serve(serveCtx, listener)
	}()
	return nil
}

// stopController stops one served controller and joins it without touching any
// workload or durable state. It closes the controller's database pools; the
// next controller builds fresh ones.
func (h *Harness) stopController() {
	cancel, served, done := h.cancel, h.served, h.serveDone
	if cancel == nil && h.App == nil {
		h.closePools()
		return
	}
	if cancel != nil {
		cancel()
	}
	if served != nil {
		select {
		case err := <-served:
			if err != nil && !strings.Contains(err.Error(), "http: Server closed") {
				h.T.Logf("factory: serve: %v", err)
			}
			if done != nil {
				close(done)
			}
		case <-time.After(30 * time.Second):
			// Leave serveDone open: the serving goroutine was not joined and a
			// caller must be able to observe that instead of a false barrier.
			h.T.Logf("factory: serve did not return after cancellation")
		}
	}
	if h.App != nil {
		if err := h.App.Wait(); err != nil {
			h.T.Logf("factory: app wait: %v", err)
		}
	}
	h.App = nil
	h.cancel = nil
	h.served = nil
	// App.Serve closes its listener when the served http.Server returns; do it
	// explicitly too so a controller that never reached Serve cannot leak the
	// socket. The next controller re-binds the same address.
	if h.infra.rawListener != nil {
		_ = h.infra.rawListener.Close()
		h.infra.rawListener = nil
	}
	h.Listener = nil
	h.infra.listenerRetired = true
	h.closePools()
}

// closePools closes the database pools owned by the stopped controller. The
// shared server and its durable rows are untouched.
func (h *Harness) closePools() {
	for _, pool := range h.infra.dbs {
		pool.Close()
	}
	h.infra.dbs = nil
}

// buildControllerConfig builds one complete Shell adapter set over the harness
// infrastructure and returns the App configuration plus a discard function the
// caller invokes if it does not construct the App. Every call creates new
// connections and adapters, so a restarted controller cannot inherit in-memory
// state from its predecessor.
func (h *Harness) buildControllerConfig(ctx context.Context) (app.Config, func(), error) {
	infra := &h.infra
	// Fresh role-separated pools: the durable data is shared, the connections
	// and the transaction identity are not.
	runtimeDB := infra.shard.Runtime(8)
	controllerDB := infra.shard.Role(store.ControllerRole, 2)
	publisherDB := infra.shard.Role(store.PublisherRole, 2)
	controllerPool := infra.shard.Role(store.ControllerRole, 4)
	publisherPool := infra.shard.Role(store.PublisherRole, 4)
	authPool := infra.shard.Role(store.AuthRole, 4)
	pools := []*pgdriver.DB{runtimeDB, controllerDB, publisherDB, controllerPool, publisherPool, authPool}
	var broker *natsbroker.Broker
	// discard releases everything this builder created. It is used on a build
	// error and by a caller whose app.New fails; on success the App owns the
	// broker and stopController owns the pools.
	discard := func() {
		if broker != nil {
			_ = broker.Close(context.WithoutCancel(ctx))
		}
		for _, pool := range pools {
			pool.Close()
		}
		h.infra.dbs = nil
	}
	fail := func(err error) (app.Config, func(), error) {
		discard()
		return app.Config{}, nil, err
	}

	executor := storepg.New(runtimeDB)
	controller := storepg.NewController(controllerPool)
	outbox := storepg.NewOutbox(publisherPool)
	credentials := storepg.NewCredentialLookup(authPool)
	validity, err := readers.NewCredentialValidity(executor)
	if err != nil {
		return fail(err)
	}

	// A fresh broker connection over the shared NATS server. The streams are
	// durable, so reprovisioning them is idempotent.
	dialNATS := func(ctx context.Context) (natsbroker.Client, error) {
		client, err := natsdriver.Dial(ctx, natsdriver.Config{
			URL:            infra.nat.URL,
			Name:           "i04-factory",
			ConnectTimeout: 5 * time.Second,
			ReconnectWait:  50 * time.Millisecond,
			MaxReconnects:  -1,
			DrainTimeout:   2 * time.Second,
		})
		if err != nil {
			return nil, err
		}
		if err := client.EnsureStreams(ctx, natsdriver.StreamConfig{}); err != nil {
			_ = client.Close(ctx)
			return nil, err
		}
		return client, nil
	}
	broker, err = natsbroker.NewBroker(natsbroker.BrokerConfig{
		Connect:             dialNATS,
		Clock:               infra.clock,
		Principal:           infra.devAuth,
		Authorizer:          authorize.NewMatrix(),
		Sink:                bridges.NewObservationSink(),
		MaxReconnectBackoff: 2 * time.Second,
		IdleValidation:      time.Minute,
		MaxPublishAttempts:  3,
	})
	if err != nil {
		return fail(err)
	}

	var (
		runtimeAdapter    kernelengine.Runtime
		workspaceAdapter  kernelengine.Workspace
		volumeProvisioner app.VolumeProvisioner
		claimProvisioner  app.ClaimProvisioner
	)
	switch infra.variant {
	case runtimeVariantPodman:
		// A fresh Libpod client and fresh-only volume provisioner over the same
		// private Podman API socket.
		files, err := podmandriver.NewFiles(h.runtimeRoot)
		if err != nil {
			return fail(err)
		}
		libpod, err := podmandriver.NewLibpod(podmandriver.LibpodConfig{SocketPath: infra.socket})
		if err != nil {
			return fail(err)
		}
		libpodClient := runtimepodman.Libpod(libpod)
		if infra.opts.StopGate != nil {
			libpodClient = gatedStopLibpod{Libpod: libpodClient, gate: infra.opts.StopGate}
		}
		scoped := labelledLibpod{Libpod: libpodClient, provisioner: libpod, fixture: h.fixture}
		adapter, err := runtimepodman.New(runtimepodman.Config{
			Libpod:    scoped,
			Files:     files,
			Root:      h.runtimeRoot,
			KodamaURL: h.WorkloadURL,
			Cleanup:   runtimepodman.CleanupPolicy{Attempts: 3, Backoff: 50 * time.Millisecond},
			Clock:     infra.clock,
		})
		if err != nil {
			return fail(err)
		}
		runtimeAdapter = adapter
		workspaceAdapter = adapter
		volumeProvisioner = scoped
	case runtimeVariantKubernetes:
		// A fresh production driver and runtime/workspace adapter over the same
		// kind cluster kubeconfig. The cluster, tenant namespace, Jobs, Pods and
		// claims are untouched.
		client, adapter, err := freshKubernetesAdapter(ctx, infra.kube, h.WorkloadURL, infra.clock)
		if err != nil {
			return fail(err)
		}
		infra.kube.Client = client
		infra.kube.Adapter = adapter
		runtimeAdapter = adapter
		workspaceAdapter = adapter
		claimProvisioner = client
		h.KubernetesClient = client
		h.KubernetesAdapter = adapter
	default:
		return fail(fmt.Errorf("factory: unknown runtime variant %d", infra.variant))
	}

	principal := authorize.CurrentPrincipal(infra.devAuth)
	if h.OIDC != nil {
		identityAdapter, err := identitystore.New(identitystore.Config{
			Executor: executor,
			Scanner:  executor,
			Clock:    infra.clock,
			IDs:      bridges.NewSecureIDs(),
		})
		if err != nil {
			return fail(err)
		}
		verifier, err := h.OIDC.Provider.NewVerifier()
		if err != nil {
			return fail(err)
		}
		resolver, err := newOIDCPrincipalResolver(oidcResolverConfig{
			Verifier:   verifier,
			Static:     infra.devAuth,
			Principals: identityAdapter,
			Now:        h.OIDC.Provider.Clock().Now,
		})
		if err != nil {
			return fail(err)
		}
		principal = resolver
	}

	runtimeDSN := fmt.Sprintf("postgres://%s@%s/%s?sslmode=disable",
		store.DefaultRuntimeRole, infra.shard.Addr, infra.shard.Database)
	imageResolver := infra.opts.ImageResolver
	if imageResolver == nil {
		imageResolver = DigestResolver{}
	}
	config := app.Config{
		Profile: app.ProfileTest,
		Installation: installation.Config{
			Environment: installation.EnvironmentTest,
			State:       installation.StatePostgres,
			Messaging:   installation.MessagingNATS,
			Runtime:     infra.runtime,
			Keys:        installation.KeyKeyfile,
			DatabaseURL: runtimeDSN,
			NATSURL:     infra.nat.URL,
			PublicURL:   "https://kodama.test",
			KeyRef:      KeyRef,
		},
		InstallationID:       InstallationID,
		DataDir:              infra.dataDir,
		DefaultActionTimeout: infra.opts.DefaultActionTimeout,
		LoopInterval:         infra.opts.LoopInterval,
		Logger:               infra.logger,
		SecretRules:          append([]secret.NameRule(nil), infra.opts.SecretRules...),
		Models:               infra.opts.composeModelsConfig(h.Scope),
		ToolProxy:            infra.opts.composeToolsConfig(h.Scope),
		WorkspacePVC:         infra.pvc,
		Dev: &app.DevelopmentConfig{
			Tokens: infra.tokens,
			Bootstrap: &app.Bootstrap{
				TenantID:       id.TenantID(TenantID),
				TenantName:     "I04 Tenant",
				ProjectID:      id.ProjectID(ProjectID),
				ProjectName:    "I04 Project",
				AdminPrincipal: id.PrincipalID(AdminPrincipal),
				AdminRoles:     []string{string(tenancy.RoleAdmin)},
			},
		},
		Adapters: &app.Adapters{
			Store:              executor,
			Scanner:            executor,
			Controller:         controller,
			Outbox:             outbox,
			Credentials:        credentials,
			Runtime:            runtimeAdapter,
			Workspace:          workspaceAdapter,
			Keys:               infra.keyfile,
			Publisher:          broker,
			Invalidator:        broker,
			Clock:              infra.clock,
			IDs:                bridges.NewSecureIDs(),
			Principal:          principal,
			CredentialValidity: composedValidity{dev: infra.devAuth, store: validity},
			Registry:           imageResolver,
			LogSink:            infra.logSink,
			VolumeProvisioner:  volumeProvisioner,
			ClaimProvisioner:   claimProvisioner,
		},
	}
	// The controller owns these pools now; stopController closes them after the
	// App has joined. The harness helpers read runtimeDB/publisherDB, so keep
	// them pointing at the current controller's connections.
	infra.dbs = pools
	h.runtimeDB = runtimeDB
	h.controllerDB = controllerDB
	h.publisherDB = publisherDB
	return config, discard, nil
}

// freshKubernetesAdapter builds a new production driver and runtime/workspace
// adapter over an existing kind cluster's kubeconfig. It mirrors the adapter
// policy newKubernetesRuntime creates for the first controller; a restart uses a
// new client so no in-memory state is shared with the stopped controller.
func freshKubernetesAdapter(ctx context.Context, runtime *kubernetesRuntime, kodamaURL string, clock *TestClock) (*driverkube.Cluster, *runtimekube.Adapter, error) {
	kubeconfig, err := runtime.Cluster.KubeconfigJSON(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("export kind kubeconfig: %w", err)
	}
	client, err := driverkube.NewCluster(driverkube.ClusterConfig{
		Kubeconfig: kubeconfig,
		Timeout:    2 * time.Minute,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("build Kubernetes driver: %w", err)
	}
	adapter, err := runtimekube.New(runtimekube.Config{
		Cluster:   client,
		Namespace: KubernetesNamespace,
		TenantID:  id.TenantID(TenantID),
		KodamaURL: kodamaURL,
		Policy: runtimekube.Policy{
			CPURequest:    "50m",
			CPULimit:      "200m",
			MemoryRequest: "32Mi",
			MemoryLimit:   "128Mi",
			JobTTLSeconds: 120,
		},
		Cleanup: runtimekube.CleanupPolicy{Attempts: 3, Backoff: 500 * time.Millisecond},
		Clock:   clock,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("build Kubernetes runtime adapter: %w", err)
	}
	return client, adapter, nil
}
