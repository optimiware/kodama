package app

import (
	"context"
	"crypto/rand"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/installation"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/app/bridges"
	"github.com/optimiware/kodama/internal/shell/app/drivers/gitkeep"
	opsapp "github.com/optimiware/kodama/internal/shell/app/ops"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	shellExecution "github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/inbox"
	shellLog "github.com/optimiware/kodama/internal/shell/log"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/service"
	executionstore "github.com/optimiware/kodama/internal/shell/store/units/execution"
	identitystore "github.com/optimiware/kodama/internal/shell/store/units/identity"
	"github.com/optimiware/kodama/internal/shell/store/units/platform"
	"github.com/optimiware/kodama/internal/shell/store/units/readers"
	"github.com/optimiware/kodama/internal/shell/store/units/work"
	"github.com/optimiware/kodama/internal/shell/task"
	tenancyadmin "github.com/optimiware/kodama/internal/shell/tenancy/admin"
	"github.com/optimiware/kodama/internal/shell/token"
	"github.com/optimiware/kodama/internal/shell/transport/httpapi"
	"github.com/optimiware/kodama/internal/shell/transport/mcp"
	transportops "github.com/optimiware/kodama/internal/shell/transport/ops"
	"github.com/optimiware/kodama/internal/shell/trigger"
	"github.com/optimiware/kodama/internal/shell/verify"
)

// wireApp connects the completed capability services to the neutral store
// adapters and the single operation router.
func wireApp(ctx context.Context, builder *builder, environment *environment) (*App, error) {
	cfg := builder.cfg
	clock := environment.clock
	ids := environment.ids
	logger := builder.logger
	authorizer := authorize.NewMatrix()
	// The operations counters are composed before every decorated port so the
	// health/operations listener can expose refusals and dependency errors.
	operations := newOperationsMetrics()

	identityAdapter, err := identitystore.New(identitystore.Config{
		Executor: environment.store,
		Scanner:  environment.scanner,
		Clock:    clock,
		IDs:      ids,
	})
	if err != nil {
		return nil, err
	}
	receiptLookup, err := readers.NewReceiptLookup(environment.scanner)
	if err != nil {
		return nil, err
	}
	bindingReader, err := readers.NewBindingReader(environment.scanner)
	if err != nil {
		return nil, err
	}
	cursorReader, err := readers.NewCursorReader(environment.scanner)
	if err != nil {
		return nil, err
	}
	executionReader, err := readers.NewExecutionReader(environment.store, environment.scanner)
	if err != nil {
		return nil, err
	}
	ownershipReader, err := readers.NewOwnershipReader(environment.store)
	if err != nil {
		return nil, err
	}
	verificationReader, err := readers.NewVerificationReader(environment.store)
	if err != nil {
		return nil, err
	}

	eventReader, err := platform.NewEventReader(environment.scanner)
	if err != nil {
		return nil, err
	}
	ingressAdapter, err := platform.NewIngressAdapter(environment.store, environment.scanner)
	if err != nil {
		return nil, err
	}
	recordAdapter, err := platform.NewRecordAdapter(environment.store, environment.scanner)
	if err != nil {
		return nil, err
	}
	reportAdapter, err := platform.NewReportAdapter(environment.store, environment.scanner)
	if err != nil {
		return nil, err
	}
	evidenceReader, err := platform.NewEvidenceReader(environment.scanner)
	if err != nil {
		return nil, err
	}
	secretLookup, err := platform.NewSecretLookup(environment.scanner)
	if err != nil {
		return nil, err
	}
	workloadPolicy, err := bridges.NewWorkloadNamePolicy(cfg.SecretRules)
	if err != nil {
		return nil, err
	}
	activationSecrets, err := secret.NewResolver(secret.ResolverConfig{
		InstallationID: cfg.InstallationID,
		Policy:         workloadPolicy,
		Lookup:         secretLookup,
		Cipher:         environment.cipher,
	})
	if err != nil {
		return nil, err
	}

	taskAdapter, err := work.NewTask(environment.store, environment.scanner)
	if err != nil {
		return nil, err
	}
	inboxAdapter, err := work.NewInbox(environment.store, environment.scanner)
	if err != nil {
		return nil, err
	}
	pipelinesAdapter, err := work.NewPipelines(environment.store, environment.scanner, nil)
	if err != nil {
		return nil, err
	}
	triggersAdapter, err := work.NewTriggers(environment.store, environment.scanner, environment.controller, clock.Now, ids.NewEventID)
	if err != nil {
		return nil, err
	}

	executionStore := executionstore.New(environment.store, environment.scanner)
	engineState := executionstore.NewEngine(environment.controller)

	factsPermission, err := bridges.NewPermissionSource(authorize.ExecOwnTaskContent, clock)
	if err != nil {
		return nil, err
	}
	executionFacts, err := platform.NewExecutionReader(environment.controller, factsPermission)
	if err != nil {
		return nil, err
	}
	ownership, err := opsapp.NewOwnership(executionFacts, opsapp.RegistryPermissions{})
	if err != nil {
		return nil, err
	}

	tenancyAdmin, err := tenancyadmin.NewService(tenancyadmin.Config{
		Clock:             clock,
		IDs:               ids,
		TenantCreate:      identityAdapter,
		TenantKeyRotation: identityAdapter,
		ProjectCreate:     identityAdapter,
		ProjectUpdate:     identityAdapter,
		MembershipGrant:   identityAdapter,
		MembershipRevoke:  identityAdapter,
		MembershipQuery:   identityAdapter,
	})
	if err != nil {
		return nil, err
	}
	bindingService, err := resource.NewService(resource.Config{
		Clock:    clock,
		IDs:      ids,
		Bindings: identityAdapter,
		Deletes:  identityAdapter,
		Reads:    identityAdapter,
		Queries:  identityAdapter,
	})
	if err != nil {
		return nil, err
	}
	pipelineManagement, err := pipeline.NewManagement(pipeline.ManagementConfig{
		Clock:      clock,
		IDs:        ids,
		Authorizer: authorizer,
		Authority:  identityAdapter,
		Changes:    pipelinesAdapter,
		Reader:     pipelinesAdapter,
	})
	if err != nil {
		return nil, err
	}
	admission, err := pipeline.NewAdmissionService(pipeline.AdmissionConfig{
		Resolver: environment.registry,
		Reader:   pipelinesAdapter,
	})
	if err != nil {
		return nil, err
	}
	executionService, err := shellExecution.NewService(shellExecution.Config{
		Clock:       clock,
		IDs:         ids,
		Authorizer:  authorizer,
		Authority:   identityAdapter,
		Reader:      executionStore,
		State:       executionStore,
		Transitions: executionStore,
		Starts:      executionStore,
	})
	if err != nil {
		return nil, err
	}
	requestService, err := request.NewService(request.Config{
		Clock:       clock,
		IDs:         ids,
		Authorizer:  authorizer,
		Authority:   identityAdapter,
		State:       executionStore,
		Activations: executionStore,
		Resolutions: executionStore,
	})
	if err != nil {
		return nil, err
	}
	secretManager, err := secret.NewManager(secret.ManagerConfig{
		InstallationID: cfg.InstallationID,
		KeyVersion:     environment.keyVersion,
		Clock:          clock,
		IDs:            ids,
		Executor:       environment.store,
		Scanner:        environment.scanner,
		Cipher:         environment.cipher,
		Lookup:         secretLookup,
	})
	if err != nil {
		return nil, err
	}
	serviceInvalidator, err := bridges.NewInvalidator(environment.invalidator)
	if err != nil {
		return nil, err
	}
	serviceAdmin, err := service.NewAdmin(service.AdminConfig{
		Authorizer:    authorizer,
		Authority:     identityAdapter,
		Principals:    identityAdapter,
		Services:      identityAdapter,
		Port:          identityAdapter,
		Invalidations: serviceInvalidator,
		Clock:         clock,
		IDs:           ids,
	})
	if err != nil {
		return nil, err
	}
	executionIdentities := &executionIdentities{controller: environment.controller}
	runtimeDeliverer, err := bridges.NewDeliverer(environment.runtime, executionIdentities)
	if err != nil {
		return nil, err
	}
	// A preparation owns its initial credential delivery directly in the
	// PrepareRequest; the wrapper intercepts only that post-commit delivery.
	deliverer := &activationDeliverer{inner: runtimeDeliverer}
	derivation, err := bridges.NewDerivationKeyResolver(bridges.DerivationKeyConfig{
		Cipher: environment.cipher,
		Associated: secret.AssociatedData{
			InstallationID: cfg.InstallationID,
			SecretID:       secret.SecretID("tenant-derivation-key"),
			Name:           id.SecretName("TENANT_DERIVATION_KEY"),
			KeyVersion:     secret.MinKeyVersion,
		},
	})
	if err != nil {
		return nil, err
	}
	tokenManager, err := token.NewManager(token.ManagerConfig{
		Keys:          identityAdapter,
		Resolver:      derivation,
		Credentials:   identityAdapter,
		Principals:    identityAdapter,
		Services:      identityAdapter,
		Mutations:     identityAdapter,
		Receipts:      receiptLookup,
		Deliverer:     deliverer,
		Invalidations: environment.invalidator,
		Clock:         clock,
		Random:        rand.Reader,
	})
	if err != nil {
		return nil, err
	}
	// The renewal loop is composed for every profile that mints workload
	// credentials at activation (development, test and production all wire the
	// activation materializer above). It reads durable running executions, so a
	// composition with no workload credential simply has nothing to renew.
	renewal, err := newRenewalLoop(renewalLoopConfig{
		Controller:  environment.controller,
		Credentials: identityAdapter,
		Manager:     tokenManager,
		Deliverer:   deliverer,
		Clock:       clock,
		IDs:         ids,
		RenewBefore: cfg.RenewBefore,
		Interval:    cfg.RenewInterval,
		Logger:      logger,
	})
	if err != nil {
		return nil, err
	}

	// Retention enforcement. Durable compositions own a store the sweeper
	// can read and delete through; the shared persistent builder installs it.
	// The loop runs immediately at startup and then once per interval.
	var retention *retentionLoop
	if environment.retention != nil || cfg.GitStorageRoot != "" {
		if environment.retention == nil {
			return nil, fault.Wrap(fault.InvalidInput,
				"retained-commit keep refs are configured but this composition has no durable retention store",
				ErrKeepRefsWithoutRetainedCommits)
		}
		retentionSweeper, err := installation.NewRetentionSweeper(
			installation.RetentionPolicy{RawContentOptIn: hasRecordedModelContent(cfg.Models)},
			environment.retention,
		)
		if err != nil {
			return nil, err
		}
		var commits retainedCommitReader
		var keepRefs retainedCommitKeeper
		if cfg.GitStorageRoot != "" {
			reader, ok := environment.retention.(retainedCommitReader)
			if !ok {
				// A retention store that cannot enumerate retained commits would
				// either skip the sweep or be mistaken for "nothing retained",
				// releasing every keep ref. Refuse startup instead.
				return nil, fault.Wrap(fault.InvalidInput,
					"retained-commit keep refs are configured but the durable store cannot supply retained commits",
					ErrKeepRefsWithoutRetainedCommits)
			}
			commits = reader
			keeper, err := gitkeep.New(gitkeep.Config{
				StorageRoot:  cfg.GitStorageRoot,
				Repositories: cfg.GitRepositories,
			})
			if err != nil {
				return nil, err
			}
			keepRefs = keeper
		}
		retention, err = newRetentionLoop(retentionLoopConfig{
			Sweeper: retentionSweeper,
			Scopes: controllerRetentionScopes{
				controller: environment.controller,
				tenants:    configuredRetentionTenants(cfg),
				projects:   configuredRetentionProjects(cfg),
			},
			Commits:  commits,
			KeepRefs: keepRefs,
			Clock:    clock,
			Interval: DefaultRetentionInterval,
			Logger:   logger,
		})
		if err != nil {
			return nil, err
		}
	}
	// Development and the injected test form authenticate the derived
	// execution credential the composition delivers. The durable compositions
	// (production and the durable test profile) keep the token resolver and
	// never construct the static development authenticator.
	principals := environment.principal
	if !cfg.durable() {
		combined, err := newCombinedPrincipalResolver(environment.principal, tokenManager, environment.credentials, clock)
		if err != nil {
			return nil, err
		}
		principals = combined
	}
	activation, err := newActivationMaterializer(activationMaterializerConfig{
		Resolver:    activationSecrets,
		Lookup:      environment.credentials,
		Credentials: identityAdapter,
		TenantKeys:  identityAdapter,
		Derivation:  derivation,
		Manager:     tokenManager,
		Controller:  environment.controller,
		IDs:         ids,
		Clock:       clock,
	})
	if err != nil {
		return nil, err
	}
	credentialAdmin, err := token.NewCredentialAdmin(token.CredentialAdminConfig{
		Authorizer:  authorizer,
		Authority:   identityAdapter,
		Principals:  identityAdapter,
		Services:    identityAdapter,
		Credentials: identityAdapter,
		Manager:     tokenManager,
		Receipts:    receiptLookup,
		Clock:       clock,
		IDs:         ids,
	})
	if err != nil {
		return nil, err
	}
	taskService, err := task.NewService(task.ServiceConfig{
		Clock:          clock,
		IDs:            ids,
		Authorizer:     authorizer,
		Authority:      identityAdapter,
		Principals:     identityAdapter,
		Pipelines:      pipelinesAdapter,
		Reader:         taskAdapter,
		Executions:     executionReader,
		Aggregates:     executionStore,
		Admission:      admission,
		Creates:        taskAdapter,
		Edits:          taskAdapter,
		Starts:         taskAdapter,
		Asks:           taskAdapter,
		Replies:        taskAdapter,
		Cancels:        taskAdapter,
		Progress:       taskAdapter,
		DefaultTimeout: cfg.DefaultActionTimeout,
	})
	if err != nil {
		return nil, err
	}
	inboxService, err := inbox.NewService(inbox.ServiceConfig{
		Authorizer: authorizer,
		Authority:  identityAdapter,
		Principals: identityAdapter,
		Queries:    inboxAdapter,
	})
	if err != nil {
		return nil, err
	}
	logReader, err := shellLog.NewReader(shellLog.ReaderConfig{
		Authorizer: authorizer,
		Authority:  identityAdapter,
		Principals: identityAdapter,
		Ownership:  ownershipReader,
		Sink:       environment.logSink,
		Clock:      clock,
	})
	if err != nil {
		return nil, err
	}
	verifier, err := verify.New(verify.Config{
		Principals: principals,
		Bindings:   bindingReader,
		Evidence:   evidenceReader,
		Membership: identityAdapter,
		Records:    recordAdapter,
		Clock:      clock,
		NewID:      identitySource{IDs: ids}.NewVerificationID,
	})
	if err != nil {
		return nil, err
	}
	reporter, err := verify.NewReporter(verify.ReportConfig{
		Authorizer:    authorizer,
		Authority:     identityAdapter,
		Bindings:      bindingReader,
		Verifications: verificationReader,
		Reports:       reportAdapter,
		Clock:         clock,
		IDs:           ids,
	})
	if err != nil {
		return nil, err
	}

	bindingAuth, err := newBindingAuthenticator(principals, clock)
	if err != nil {
		return nil, err
	}
	// The optional configured model gateway is constructed before the operation
	// bindings so an incomplete enabled configuration refuses startup. A nil
	// configuration returns the disabled port without touching any dependency.
	// The composition adopts the provider transport and stops it at shutdown.
	modelInvocation, stopModels, err := newConfiguredModels(modelGatewayDependencies{
		Config:         cfg.Models,
		Profile:        cfg.Profile,
		InstallationID: cfg.InstallationID,
		Controller:     environment.controller,
		Credentials:    bindingAuth,
		Authorizer:     authorizer,
		Authority:      identityAdapter,
		Lookup:         secretLookup,
		Cipher:         environment.cipher,
		Store:          environment.store,
		IDs:            ids,
		Clock:          clock,
	})
	if err != nil {
		return nil, err
	}
	builder.addStop("model-gateway-transport", stopModels)
	// The optional external tool proxy is constructed before the operation
	// bindings so an incomplete enabled configuration refuses startup. A nil
	// configuration returns the disabled port without touching any dependency.
	toolInvocation, err := newConfiguredTools(toolProxyDependencies{
		Config:         cfg.ToolProxy,
		InstallationID: cfg.InstallationID,
		Controller:     environment.controller,
		Credentials:    bindingAuth,
		Authorizer:     authorizer,
		Authority:      identityAdapter,
		Lookup:         secretLookup,
		Cipher:         environment.cipher,
		Store:          environment.store,
		IDs:            ids,
		Clock:          clock,
	})
	if err != nil {
		return nil, err
	}
	eventIngress, err := event.NewIngress(event.IngressConfig{
		Credentials:  bindingAuth,
		Authority:    identityAdapter,
		Authorizer:   authorizer,
		Port:         ingressAdapter,
		Clock:        clock,
		NewEventID:   ids.NewEventID,
		NewReceiptID: ids.NewReceiptID,
	})
	if err != nil {
		return nil, err
	}
	eventReadService, err := event.NewReader(event.ReadConfig{
		Authorizer: authorizer,
		Authority:  identityAdapter,
		Principals: identityAdapter,
		Port:       eventReader,
	})
	if err != nil {
		return nil, err
	}
	eventSubscriber, err := event.NewSubscriber(event.SubscribeConfig{
		Authorizer: authorizer,
		Authority:  identityAdapter,
		Principals: identityAdapter,
		Cursor:     cursorReader,
		Validity:   environment.validity,
		Clock:      clock,
	})
	if err != nil {
		return nil, err
	}

	tenantKeys := tenantKeySource{
		cipher:     environment.cipher,
		keyVersion: environment.keyVersion,
		associated: secret.AssociatedData{
			InstallationID: cfg.InstallationID,
			SecretID:       secret.SecretID("tenant-derivation-key"),
			Name:           id.SecretName("TENANT_DERIVATION_KEY"),
			KeyVersion:     environment.keyVersion,
		},
	}
	bindings, err := opsapp.Bindings(opsapp.Dependencies{
		Tenancy:        tenancyAdmin,
		Bindings:       bindingService,
		Pipelines:      pipelineManagement,
		Admission:      admission,
		Executions:     executionService,
		Receipts:       receiptLookup,
		Secrets:        secretManager,
		Authority:      identityAdapter,
		Services:       serviceAdmin,
		Credentials:    credentialAdmin,
		Tasks:          taskService,
		Inbox:          inboxService,
		Requests:       requestService,
		Logs:           logReader,
		Verifier:       verifier,
		ActorActions:   actorActions{principals: principals, identities: executionIdentities},
		Reports:        &metricsReports{inner: reporter, metrics: operations},
		Events:         &eventPublication{ingress: eventIngress, credentials: bindingAuth},
		EventRead:      eventReadService,
		EventSubscribe: eventSubscriber,
		Models:         modelInvocation,
		Tools:          toolInvocation,
		IDs:            identitySource{IDs: ids},
		TenantKeys:     tenantKeys,
		ActionTimeout:  cfg.DefaultActionTimeout,
	})
	if err != nil {
		return nil, err
	}

	if err := builder.before("router"); err != nil {
		return nil, err
	}
	router, err := transportops.NewRouter(transportops.Config{
		Principals: principals,
		Scopes:     ownership,
		Executions: ownership,
		Bindings:   bindings,
	})
	if err != nil {
		return nil, err
	}
	// The optional public discovery document is validated at construction and
	// served from a static provider that copies it on every call. Without it the
	// fixed route stays a clean 404, so kodactl login reports that the login
	// configuration is unavailable instead of inventing OAuth endpoints.
	restConfig := httpapi.Config{Router: router}
	document, discoveryConfigured, err := cfg.discoveryDocument()
	if err != nil {
		return nil, err
	}
	if discoveryConfigured {
		provider, err := newStaticDiscovery(document)
		if err != nil {
			return nil, err
		}
		restConfig.Discovery = provider
	} else if cfg.Profile == ProfileProduction && cfg.Production != nil &&
		cfg.Production.OIDC.JWKSURL != "" {
		// Production human authentication is configured but interactive login
		// has no published client configuration. Service-only deployments are
		// legitimate, so this is one startup warning rather than a refusal.
		logger.Warn("public login discovery is not configured; interactive login is unavailable",
			"profile", string(cfg.Profile),
			"route", api.DiscoveryPath)
	}
	restEdge, err := httpapi.NewEdgeWithConfig(restConfig)
	if err != nil {
		return nil, err
	}
	mcpEdge, err := mcp.NewEdge(mcp.Config{
		Router:     router,
		ServerInfo: api.MCPImplementation{Name: "kodama", Version: "1.0.0"},
	})
	if err != nil {
		return nil, err
	}
	handler := newPublicHandler(restEdge, mcpEdge, cfg.MCPPath)

	tracked, err := newTrackedRuntime(environment.runtime, closePolicyForRuntime(cfg.Installation.Runtime))
	if err != nil {
		return nil, err
	}
	tracked.activation = activation
	// The Kernel removes workloads directly, so the engine sees a decorator
	// that drains the runtime log seam into the retained sink before the real
	// removal. App.runtime stays the tracked runtime so an explicit stop is
	// forgotten only after confirmed evidence, and the process runtime still
	// stops its own workloads on close. Durable Podman/Kubernetes workloads stay
	// running across a controller close and are adopted by the next controller.
	executionScopes, err := newControllerExecutionScopes(environment.controller)
	if err != nil {
		return nil, err
	}
	logCursors, err := newFileLogCursorStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	collecting, err := newLogCollectingRuntime(logCollectionConfig{
		Inner:     tracked,
		Sink:      environment.logSink,
		Live:      environment.liveLog,
		Scopes:    executionScopes,
		Cursors:   logCursors,
		PageBytes: shellLog.DefaultBounds.MaxChunkBytes,
	})
	if err != nil {
		return nil, err
	}
	builder.addStop("live-log-runtime", collecting.Close)
	engine := kernelengine.New(newPreparationState(engineState, activation), collecting, environment.workspace, clock)
	dispatcher, err := event.NewDispatcher(event.DispatcherConfig{
		Outbox:    environment.outbox,
		Publisher: &metricsPublisher{inner: environment.publisher, metrics: operations},
		BatchSize: 64,
		Idle: func(ctx context.Context) error {
			return clock.Sleep(ctx, cfg.LoopInterval)
		},
	})
	if err != nil {
		return nil, err
	}
	processor, err := trigger.NewProcessor(trigger.ProcessorConfig{
		Events:         triggersAdapter,
		Candidates:     pipelinesAdapter,
		Selections:     triggersAdapter,
		Select:         triggersAdapter,
		Admission:      admission,
		Start:          &metricsStart{inner: triggersAdapter, metrics: operations},
		Completion:     triggersAdapter,
		Clock:          clock,
		IDs:            ids,
		DefaultTimeout: cfg.DefaultActionTimeout,
		Idle: func(ctx context.Context) error {
			return clock.Sleep(ctx, cfg.LoopInterval)
		},
	})
	if err != nil {
		return nil, err
	}

	if err := builder.before("seed"); err != nil {
		return nil, err
	}
	if err := seedFirstAdmin(ctx, environment.store, environment.scanner, ids, environment.cipher,
		cfg.InstallationID, environment.keyVersion, environment.seed, clock); err != nil {
		return nil, err
	}

	if cfg.Profile == ProfileTest && cfg.Test != nil {
		logger.Warn("test composition: single host with a local keyfile; never use this profile in production",
			"profile", string(cfg.Profile),
			"store", "postgres",
			"runtime", string(cfg.Installation.Runtime),
			"keys", string(cfg.Installation.Keys),
			"messaging", "nats")
	} else if cfg.Profile != ProfileProduction {
		logger.Warn("development composition uses explicitly ephemeral adapters",
			"profile", string(cfg.Profile),
			"store", "memory",
			"runtime", "process",
			"keys", "ephemeral",
			"messaging", "in-process",
			"authenticator", "DevAuthenticator",
			"first_admin_seed", environment.seed != nil)
	}

	return &App{
		cfg:           cfg,
		logger:        logger,
		handler:       handler,
		engine:        engine,
		events:        dispatcher,
		trigger:       processor,
		metrics:       operations,
		metricsSource: newMetricsSource(environment.outbox, triggersAdapter, environment.controller, operations),
		settlement:    newTaskSettlement(environment.controller, environment.controller, environment.scanner, taskService),
		runtime:       tracked,
		renewal:       renewal,
		retention:     retention,
		clock:         clock,
		gate:          newDispatchGate(!cfg.dispatchDisabled),
		done:          make(chan struct{}),
	}, nil
}
