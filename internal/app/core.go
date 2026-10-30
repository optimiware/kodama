package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/optimiware/kodama/internal/agents"
	agentsadapters "github.com/optimiware/kodama/internal/agents/adapters"
	agentshttp "github.com/optimiware/kodama/internal/agents/http"
	agentspg "github.com/optimiware/kodama/internal/agents/postgres"
	"github.com/optimiware/kodama/internal/artifacts"
	artifactspg "github.com/optimiware/kodama/internal/artifacts/postgres"
	arts3 "github.com/optimiware/kodama/internal/artifacts/s3"
	"github.com/optimiware/kodama/internal/delivery"
	deliveryadapters "github.com/optimiware/kodama/internal/delivery/adapters"
	deliverypg "github.com/optimiware/kodama/internal/delivery/postgres"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/entrypoint"
	"github.com/optimiware/kodama/internal/events"
	eventsjs "github.com/optimiware/kodama/internal/events/jetstream"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/execution"
	execadapters "github.com/optimiware/kodama/internal/execution/adapters"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/gates"
	gatesadapters "github.com/optimiware/kodama/internal/gates/adapters"
	gateshttp "github.com/optimiware/kodama/internal/gates/http"
	gatespg "github.com/optimiware/kodama/internal/gates/postgres"
	gatewayhttp "github.com/optimiware/kodama/internal/gateway/http"
	"github.com/optimiware/kodama/internal/httpapi"
	httppg "github.com/optimiware/kodama/internal/httpapi/postgres"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/identity/consumer"
	identityhttp "github.com/optimiware/kodama/internal/identity/http"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
	chatopshttp "github.com/optimiware/kodama/internal/integrations/chatops/http"
	gwadapters "github.com/optimiware/kodama/internal/integrations/gitwebhook/adapters"
	gwpg "github.com/optimiware/kodama/internal/integrations/gitwebhook/postgres"
	"github.com/optimiware/kodama/internal/logs"
	logsadapters "github.com/optimiware/kodama/internal/logs/adapters"
	logshttp "github.com/optimiware/kodama/internal/logs/http"
	logspg "github.com/optimiware/kodama/internal/logs/postgres"
	mcpproxyhttp "github.com/optimiware/kodama/internal/mcpproxy/http"
	"github.com/optimiware/kodama/internal/observation"
	obsauth "github.com/optimiware/kodama/internal/observation/authorization"
	obshttp "github.com/optimiware/kodama/internal/observation/http"
	obspg "github.com/optimiware/kodama/internal/observation/postgres"
	"github.com/optimiware/kodama/internal/pipeline"
	pipelinepg "github.com/optimiware/kodama/internal/pipeline/postgres"
	resultsadapters "github.com/optimiware/kodama/internal/results/adapters"
	rpg "github.com/optimiware/kodama/internal/results/postgres"
	"github.com/optimiware/kodama/internal/runtimeauth"
	rtjwt "github.com/optimiware/kodama/internal/runtimeauth/jwt"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
	"github.com/optimiware/kodama/internal/secrets"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/work"
	dependencieshttp "github.com/optimiware/kodama/internal/work/dependencies/http"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
	"github.com/optimiware/kodama/internal/workspace"
	gitad "github.com/optimiware/kodama/internal/workspace/git"
	workspacepg "github.com/optimiware/kodama/internal/workspace/postgres"
	wsruntime "github.com/optimiware/kodama/internal/workspace/runtime"
)

// APIPrefix is the public API path prefix.
const APIPrefix = "/api/v1"

// cancellationDurable is the JetStream durable of the work-cancellation
// consumer.
const cancellationDurable = "kodama-work-cancellation"

// gateDecisionDurable is the JetStream durable of the gate decision wakeup. The
// event only wakes the step transition; the periodic gate scan covers a missed
// one.
const gateDecisionDurable = "kodama-gates-decision"

// sentinelTables must exist for the process to report ready: they prove the
// declared migrations were applied to the database the runtime role sees.
var sentinelTables = []string{
	"tenants", "projects", "memberships", "audit", "outbox", "event_inbox", "idempotency", "work_items",
	"secrets", "pipeline_definitions", "workspaces", "artifacts", "runs", "step_attempts", "runtime_grants", "log_records",
	"agent_profiles", "gates", "environments", "deployments", "model_settings", "model_requests", "mcp_servers", "mcp_invocations",
	"work_item_dependencies", "chatops_destinations", "chatops_deliveries", "git_triggers", "git_trigger_deliveries", "git_trigger_admissions",
}

// Core is the constructed core assembly: every dependency is built and checked,
// no worker runs and nothing listens until Start. It owns its connections.
type Core struct {
	cfg CoreConfig
	log *slog.Logger

	pool   *storage.Pool
	nc     *nats.Conn
	ready  *readiness
	api    http.Handler
	router *httpapi.Router

	workers      *workerGroup
	publisher    *events.Publisher
	startWorkers func(*workerGroup)
	gitClosers   []func()
	// runtime serves the runtime API under /runtime/v1; closers release the
	// runtime's own resources after the workers stopped.
	runtime http.Handler
	// runtimeV2 serves /runtime/v2 when run_shared is composed (nil otherwise).
	runtimeV2 http.Handler
	// sharedPublic decorates the public handler with the run_shared run view and
	// resource logs (nil unless run_shared is composed).
	sharedPublic *SharedPublic
	closers      []func()
	// Exported collaborators for the integration evidence and later assembly
	// tasks (T-071 and T-072 extend this composition).
	Events    *eventspg.Store
	Consumer  *eventspg.Store
	Execution *execution.Service
	Workspace *workspace.Service
	// Slice is the assembled container execution slice.
	Slice *ExecutionSlice
	// Delivery is the manual agent/gate/delivery slice (T-072).
	Delivery *DeliverySlice
	// ModelTools is the model gateway and MCP proxy (T-039), nil unless the
	// installation selected both (model-tools.json). Triggers is the native Git
	// push trigger, nil until T-038 joins its store and admission ledger.
	ModelTools *ModelToolSlice
	Triggers   *TriggerSlice
	// BusinessLoop is the business-loop slice (T-075): the dependency graph, the
	// flow summaries, the QA mapping (nil unless the installation named its
	// principal) and ChatOps (nil unless it named its canonical origin).
	BusinessLoop *BusinessLoopSlice
	Routes       Routes
}

// Routes records what the assembly registered, so a test (and a later task)
// can prove the supported subset instead of inferring it.
type Routes struct {
	Registered []string
	// Disabled lists the capabilities that are deliberately not wired.
	Disabled []string
	// Reasons says why a capability of the M2 joins (T-101) is disabled: the
	// configuration or input that is absent. A capability reported disabled by
	// an earlier slice has its reason in that slice's source.
	Reasons map[string]string
}

// NewCore validates cfg, connects to PostgreSQL, JetStream and S3, and
// constructs every module of the core slice. It performs no migration: the
// schema must already be applied (Migrate), and readiness reports a database
// that is not.
func NewCore(ctx context.Context, cfg CoreConfig, log *slog.Logger) (*Core, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("core configuration: %w", err)
	}
	if cfg.Env == Development {
		// The development environment composes memory adapters and no service;
		// that composition is the development smoke of T-027. Nothing here
		// substitutes real services for it or mixes the two.
		return nil, fmt.Errorf("%w: the %s composition (memory adapters, Process runtime) is assembled by the development smoke, not by NewCore", errNotComposed, Development)
	}
	cons, err := NewConstructors(cfg.Env)
	if err != nil {
		return nil, err
	}
	if err := cons.Objects(cfg.S3); err != nil {
		return nil, fmt.Errorf("object store: %w", err)
	}
	// The spool directories are prepared before any connection is opened: an
	// unusable path refuses construction instead of starting a service that
	// fails every artifact completion.
	if err := prepareSpools(cfg); err != nil {
		return nil, err
	}
	c := &Core{cfg: cfg, log: log}
	ok := false
	defer func() {
		if !ok {
			c.closeConnections()
		}
	}()

	pool, err := storage.Open(ctx, storage.Config{URL: cfg.Database.RuntimeURL, MaxConns: cfg.Database.MaxConns, ApplicationName: "kodama"})
	if err != nil {
		return nil, err
	}
	c.pool = pool
	if err := verifyRuntimeRole(ctx, pool); err != nil {
		return nil, err
	}

	nc, err := connectNATS(cfg.NATS)
	if err != nil {
		return nil, err
	}
	c.nc = nc

	s3client := awss3.New(awss3.Options{
		Region: cfg.S3.Region, BaseEndpoint: aws.String(cfg.S3.Endpoint), UsePathStyle: cfg.S3.PathStyle,
		Credentials: credentials.NewStaticCredentialsProvider(cfg.S3.AccessKey, cfg.S3.SecretKey, ""),
	})

	if err := c.build(ctx, cons, s3client); err != nil {
		return nil, err
	}
	ok = true
	return c, nil
}

// closeConnections releases everything the constructor opened, in the reverse
// order of use: Git helpers, the broker connection, the database pool. It is
// the single place that does so, for a failed start and for Shutdown alike.
func (c *Core) closeConnections() {
	for _, closeFn := range c.closers {
		closeFn()
	}
	c.closers = nil
	for _, closeFn := range c.gitClosers {
		closeFn()
	}
	c.gitClosers = nil
	if c.nc != nil {
		c.nc.Close()
	}
	if c.pool != nil {
		c.pool.Close()
	}
}

func connectNATS(cfg NATSConfig) (*nats.Conn, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Host == "" {
		return nil, errors.New("the NATS URL is not valid")
	}
	token := cfg.Token
	if u.User != nil {
		if _, has := u.User.Password(); has || token != "" {
			return nil, errors.New("the NATS URL must carry at most a token, and not both a token and user-info")
		}
		token = u.User.Username()
		u.User = nil
	}
	opts := []nats.Option{nats.Name("kodama"), nats.Timeout(5 * time.Second), nats.MaxReconnects(-1), nats.ReconnectWait(200 * time.Millisecond)}
	if token != "" {
		opts = append(opts, nats.Token(token))
	}
	nc, err := nats.Connect(u.String(), opts...)
	if err != nil {
		return nil, fmt.Errorf("connect to NATS: %w", err)
	}
	return nc, nil
}

// verifyRuntimeRole refuses a serving identity that could bypass the tenant
// boundary: a superuser, a role that bypasses row level security, or one that
// owns any table of the schema (an owner is exempt from the policies unless
// they are forced, and it can alter them).
func verifyRuntimeRole(ctx context.Context, pool *storage.Pool) error {
	var super, bypass bool
	var owned int
	err := pool.QueryRow(ctx, `SELECT r.rolsuper, r.rolbypassrls,
		(SELECT count(*) FROM pg_class c WHERE c.relowner = r.oid AND c.relnamespace = 'public'::regnamespace AND c.relkind IN ('r', 'p'))
		FROM pg_roles r WHERE r.rolname = current_user`).Scan(&super, &bypass, &owned)
	if err != nil {
		return fmt.Errorf("inspect the runtime database role: %w", err)
	}
	switch {
	case super:
		return errors.New("the runtime database role is a superuser: row level security would not bind it")
	case bypass:
		return errors.New("the runtime database role bypasses row level security")
	case owned > 0:
		return errors.New("the runtime database role owns schema tables: use the separate non-owner runtime role")
	}
	return nil
}

func (c *Core) build(ctx context.Context, cons Constructors, s3client *awss3.Client) error {
	cfg, tune, pool := c.cfg, c.cfg.Tuning, c.pool

	// ---- event bus ----
	evOpts := eventspg.Options{
		Backlog: tune.Events.Backlog, Retry: tune.Events.Retry, Retention: tune.Events.Retention,
		ReplayLease: tune.Events.ReplayLease, ReplayBatch: tune.Events.ReplayBatch, Logger: c.log,
	}
	appender, err := eventspg.NewStore(pool, evOpts)
	if err != nil {
		return fmt.Errorf("event store: %w", err)
	}
	c.Events = appender
	tenants := tenantSource{appender}

	// ---- identity: the standalone authority, selected explicitly ----
	idStore, err := identitypg.NewStore(pool, appender)
	if err != nil {
		return err
	}
	idSvc, err := identity.NewService(identity.Config{Repository: idStore, Directory: idStore})
	if err != nil {
		return err
	}
	standalone, err := identity.NewStandaloneAuthority(idStore)
	if err != nil {
		return err
	}
	authority, err := identity.SelectAuthority(identity.AuthorityConfig{Profile: identity.ProfileStandalone, Standalone: standalone})
	if err != nil {
		return err
	}
	verifier, err := cons.Verifier(cfg.OIDC)
	if err != nil {
		return fmt.Errorf("identity verifier: %w", err)
	}
	authn, err := identityhttp.NewAuthenticator(verifier, idStore)
	if err != nil {
		return err
	}
	cursors, err := httpapi.NewCursorCodec(cfg.CursorKey)
	if err != nil {
		return err
	}
	idem, err := httppg.NewIdempotency(httppg.Config{DB: pool})
	if err != nil {
		return err
	}

	// ---- work ----
	workStore, err := workpg.NewStore(pool, appender, consumer.BindPermissions(execadapters.BindWriterGuard(func(tx pgx.Tx) work.TxGuards {
		// The business dependency graph (T-045, selected by T-075) replaces the M1
		// explicit "none": a ready -> active transition reads the current links
		// inside its own transaction. The review subject guard (candidate history
		// and evidence of a review transition) needs Git and object reads that must
		// stay outside this transaction, and no transaction-safe adapter exists yet
		// (T-073/T-038 qualify the joined path): a review transition or evidence
		// link fails closed (unavailable).
		return dependencyGuards(tx)
	})))
	if err != nil {
		return err
	}
	workAuthority, err := consumer.NewWorkAuthority(authority, idStore)
	if err != nil {
		return err
	}
	workSvc, err := work.NewService(work.Config{Repository: workStore, Authority: workAuthority})
	if err != nil {
		return err
	}

	// ---- secrets and variables ----
	provider, closeKeys, err := cons.KeyWrapper(ctx, cfg.Secrets)
	if err != nil {
		return fmt.Errorf("key provider: %w", err)
	}
	c.closers = append(c.closers, closeKeys)
	provider, err = Admit(cfg.Env, PortKeyWrapper, provider)
	if err != nil {
		return err
	}
	secretStore, err := secretspg.NewStore(secretspg.Config{DB: pool, Events: secretspg.EventsFrom(appender), IDs: secrets.RandomIDs{}})
	if err != nil {
		return err
	}
	secretSvc, err := secrets.NewService(secrets.ServiceConfig{InstallationID: cfg.InstallationID, Store: secretStore, Wrapper: provider, IDs: secrets.RandomIDs{}})
	if err != nil {
		return err
	}
	secretKeyed, err := secretspg.NewKeyed(idem, secretStore)
	if err != nil {
		return err
	}
	// The version source reads the exact pinned versions of delivered secrets: the
	// runtime secret route resolves through it, service secrets resolve through it
	// and, scan-capable, the prior disclosures of a run_shared run are read through
	// it for comparison only.
	secretVersions, err := secretspg.NewVersionSource(secretStore, provider, cfg.InstallationID)
	if err != nil {
		return err
	}

	// ---- agent profiles (T-072) ----
	// The profile service is constructed with the selected authority and a
	// compatibility catalogue that qualifies nothing until a wrapper image is
	// qualified for the installation (T-031/T-085), and approvals that approve no
	// model and no tool until the gateway and MCP proxy are selected. Registration
	// therefore refuses, so no agent step can be admitted: it fails closed.
	agentStore, err := agentspg.NewStore(pool, agentspg.Options{})
	if err != nil {
		return err
	}
	agentAuthority, err := agentsadapters.NewAuthority(authority, idStore)
	if err != nil {
		return err
	}
	// The catalogue comes from the installation's model-tools.json; absent or
	// empty it qualifies nothing. The approvals approve nothing until the model
	// gateway and the MCP proxy are built (bound below), and approve only what the
	// project's stored settings and registrations enable afterwards.
	catalogue, err := cfg.ModelTools.catalogue()
	if err != nil {
		return err
	}
	lateApprovals := &LateApprovals{}
	agentSvc, err := agents.NewService(agents.ServiceConfig{
		Repository: agentStore, Authority: agentAuthority, Compatibility: catalogue, Approvals: lateApprovals,
	})
	if err != nil {
		return err
	}
	profileLookup, err := agentsadapters.NewLookup(agentSvc)
	if err != nil {
		return err
	}

	// ---- M2 production joins (T-101): the joins whose adapters exist as soon as
	// the stores do are bound here; the reader and the secret route follow the
	// stores they read. An absent join leaves its capability disabled. ----
	joins := DeliveryJoins{
		Environments: execadapters.BindDeliveryEnvironments, Deployments: gatesadapters.BindDeployments,
		ConformanceOutput: ConformanceReportOutput,
	}
	ceilingProfiles, err := NewProfileCapabilities(agentSvc)
	if err != nil {
		return err
	}
	ceiling, err := NewCeilingBinding(cfg.ActionCeiling, ceilingProfiles)
	if err != nil {
		return err
	}

	// ---- pipeline definitions: containers, agent steps and human gates ----
	kinds := pipeline.EnabledKinds{pipeline.KindContainer, pipeline.KindAgent, pipeline.KindGate}
	envRefs := &lateEnvironments{}
	validator, err := pipeline.NewValidator(pipeline.Config{
		Capabilities: kinds, Profiles: profileLookup,
		References: pipelineReferences{authority: authority, catalog: secretStore, environments: envRefs},
		// kodama/v1alpha2 is registrable exactly when run_shared is composed; the
		// execution service refuses it independently when it is not.
		EnableV1Alpha2: cfg.runSharedConfigured(),
	})
	if err != nil {
		return err
	}
	pipeStore, err := pipelinepg.NewStore(pool, pipelinepg.Options{})
	if err != nil {
		return err
	}
	pipeSvc, err := pipeline.NewService(pipeline.ServiceConfig{
		Repository: pipeStore, Authority: pipelineAuthority{authority: authority, reader: idStore}, Validator: validator,
	})
	if err != nil {
		return err
	}

	// ---- artifacts (S3): the store and the objects; the service follows the
	// runtime authority it consults ----
	artStore, err := artifactspg.NewStore(artifactspg.Config{DB: pool})
	if err != nil {
		return err
	}
	objects, err := arts3.NewStore(arts3.Config{Client: s3client, Bucket: cfg.S3.Bucket, Prefix: cfg.S3.Prefix, AllowInsecure: cfg.S3.AllowInsecureHTTP})
	if err != nil {
		return err
	}
	artReconciler, err := artifacts.NewReconciler(artifacts.ReconcilerConfig{
		Store: artStore, Objects: objects,
		Policy: artifacts.CleanupPolicy{StagingGrace: tune.ArtifactStagingGrace, AbandonAfter: tune.ArtifactAbandonAfter, DeletionLease: time.Minute, Batch: tune.ReconcileBatch},
	})
	if err != nil {
		return err
	}

	// ---- execution store: the runtime-enabled one (T-062) ----
	wsStore, err := workspacepg.NewStore(pool, appender, workspacepg.Options{})
	if err != nil {
		return err
	}
	execStore, err := execpg.NewRuntimeStore(pool, appender, joins.executionPeers(execpg.Peers{
		Authority: execadapters.BindAuthority, Workspaces: wsStore, Secrets: secretStore,
	}), execadapters.RevokeAttemptGrants, execpg.Options{})
	if err != nil {
		return err
	}

	// ---- workspaces and Git ----
	gitOpts := gitad.Options{
		CAFile: cfg.Git.CAFile, CommandTimeout: cfg.Git.CommandTimeout, MaxBundleBytes: cfg.Git.MaxBundleBytes,
		MaxScratchBytes: cfg.Git.MaxScratchBytes, ScratchRoot: filepath.Join(cfg.Git.ScratchRoot, "git"),
	}
	creds := installationCredentials{creds: indexCredentials(cfg.Git.Credentials)}
	gitReader, err := gitad.NewReader(gitOpts, creds)
	if err != nil {
		return fmt.Errorf("git reader: %w", err)
	}
	c.gitClosers = append(c.gitClosers, gitReader.Close)
	gitPublisher, err := gitad.NewPublisher(gitOpts, creds)
	if err != nil {
		return fmt.Errorf("git publisher: %w", err)
	}
	c.gitClosers = append(c.gitClosers, gitPublisher.Close)
	attemptAuthority, err := execpg.NewAttemptAuthority(execStore)
	if err != nil {
		return err
	}
	bundles, err := resultsadapters.NewBundles(artStore, objects)
	if err != nil {
		return err
	}
	repositories := projectRepositories{db: pool, creds: creds.creds}
	wsDeps := workspace.Dependencies{
		Store: wsStore, Tenants: tenants, Authorizer: workspaceAuthorizer{authority: authority},
		WorkItems: workspaceItems{repo: workStore}, Repositories: repositories,
		Attempts: attemptAuthority, Bundles: bundles, Secrets: attemptNeedles{runs: execStore},
		Refs: gitReader, Source: gitReader, Publisher: gitPublisher, Logger: c.log,
	}
	wsOpts := workspace.Options{
		GitDeadline: cfg.Git.CommandTimeout, ArchiveWait: 30 * time.Second, ArchivePoll: 500 * time.Millisecond,
		DriverStaleAfter: 2 * time.Minute, RedriveAfter: time.Minute, ReconcileBatch: tune.ReconcileBatch,
	}
	wsSvc, err := workspace.NewService(wsDeps, wsOpts)
	if err != nil {
		return err
	}
	c.Workspace = wsSvc

	// ---- container execution: the runtime of the environment and the runtime
	// authority bound to it ----
	gitCA, err := readGitCA(cfg.Git.CAFile)
	if err != nil {
		return err
	}
	late := &LateRegistrar{}
	rtSet, err := cons.Runtime(RuntimeSettings{
		Config: cfg.Runtime, Registrar: late, Store: execStore, Logger: c.log,
		Checkout: checkoutProvider{repos: repositories, creds: creds, gitCA: gitCA}, Ceiling: ceiling,
		ResourceRegistrar: kubeResourceRegistrar{late: late},
	})
	if err != nil {
		return fmt.Errorf("runtime: %w", err)
	}
	if rtSet.Close != nil {
		c.closers = append(c.closers, rtSet.Close)
	}
	rtAttempts, err := rtSet.Authority(execStore)
	if err != nil {
		return err
	}
	grants, err := rtpg.NewStore(pool, rtAttempts)
	if err != nil {
		return err
	}
	rtKeys, err := loadRuntimeKeys(cfg.Runtime)
	if err != nil {
		return err
	}
	policy := runtimeauth.Policy{AdmissionTTL: tune.AdmissionTTL, TokenTTL: tune.TokenTTL, MaxSession: tune.MaxSession}
	signer, err := rtjwt.NewSigner(rtKeys, cfg.Runtime.ServerURL)
	if err != nil {
		return err
	}
	rtVerifier, err := rtjwt.NewVerifier(rtjwt.VerifierConfig{Ring: rtKeys, Issuer: cfg.Runtime.ServerURL, MaxLifetime: policy.MaxSession})
	if err != nil {
		return err
	}

	// ---- run_shared (O-30): the resource subjects' persistence and the prior
	// disclosures, composed only when the runtime contributes the model ----
	var (
		sharedInputs *SharedInputs
		disclosures  *secrets.Disclosures
	)
	if rtSet.Shared != nil {
		resAuthority, err := newResourceAuthority(execStore, nil)
		if err != nil {
			return err
		}
		resStore, err := rtpg.NewResourceStore(pool, resAuthority)
		if err != nil {
			return err
		}
		sharedInputs = &SharedInputs{Store: resStore, State: resAuthority, Tokens: signer, Verifier: rtVerifier, Secrets: secretVersions}
		if disclosures, err = secrets.NewDisclosures(runBindings{runs: execStore}, secretVersions); err != nil {
			return err
		}
	}

	// The result and source routes run over the guarded stores: every mutation
	// they make repeats the runtime session check inside its own transaction,
	// under the run lock (T-062). Trusted background work keeps the plain ones.
	guard, err := rpg.NewGuard(rtAttempts, rtKeys.Generation, nil)
	if err != nil {
		return err
	}
	guardedExec, err := rpg.NewExecutionStore(execStore, pool, guard)
	if err != nil {
		return err
	}
	guardedWS, err := rpg.NewWorkspaceStore(wsStore, pool, guard)
	if err != nil {
		return err
	}
	guardedDeps := wsDeps
	guardedDeps.Store = guardedWS
	if disclosures != nil {
		// A source proposal is scanned for the values injected into the attempt AND
		// for every value the run disclosed to its other steps and services: the
		// scratch outlives a step, so a writer can carry another's secret into source.
		if guardedDeps.Secrets, err = workspace.WithPriorDisclosures(wsDeps.Secrets, disclosures); err != nil {
			return err
		}
	}
	guardedWSSvc, err := workspace.NewService(guardedDeps, wsOpts)
	if err != nil {
		return err
	}
	proposer, err := wsruntime.NewSource(guardedWSSvc)
	if err != nil {
		return err
	}
	resultSource, err := resultsadapters.NewSource(proposer)
	if err != nil {
		return err
	}
	resultAuthority, err := resultsadapters.NewAuthority(rtVerifier, rtKeys, grants, nil)
	if err != nil {
		return err
	}
	artifactAuthority, err := resultsadapters.NewArtifactAuthority(grants, rtKeys, nil)
	if err != nil {
		return err
	}
	// A run_shared attempt's artifacts carry the workspace state the server
	// recorded at its command begin (execution's own record).
	artifactBegins, err := resultsadapters.NewExecutionBegins(execStore)
	if err != nil {
		return err
	}
	artifactAuthority = artifactAuthority.WithBegins(artifactBegins)
	// The registry verifier of `ociArtifact` outputs: the real OCI verifier over
	// the configured registries, provenance from the producing attempt's pinned
	// source. Without a registry statement every registration is refused.
	registry, registryConfigured, err := NewRegistryVerifier(cfg.Registry, cfg.Env, execStore)
	if err != nil {
		return err
	}
	artSvc, err := artifacts.NewService(artifacts.ServiceConfig{
		Repository: artStore, Authority: artifactAuthority, Reader: artifactReadAuthority{authority: authority}, Objects: objects, Registry: registry,
		UploadTTL: 10 * time.Minute, DownloadTTL: 5 * time.Minute, ClaimLease: 30 * time.Second, SpoolDir: artifactSpoolDir(cfg),
	})
	if err != nil {
		return err
	}
	resultEvidence, err := resultsadapters.NewArtifacts(artSvc)
	if err != nil {
		return err
	}

	// ---- delivery (T-072): environments, deployment records and their runtime
	// callbacks. The cleanup-run binding is built once from the execution store
	// and shared by the delivery service and the effect reconciler. ----
	cleanupBindings, err := CleanupBindings(execStore)
	if err != nil {
		return err
	}
	deliveryGuard, err := deliverypg.NewGuard(rtAttempts, rtKeys.Generation, nil)
	if err != nil {
		return err
	}
	deliveryStore, err := deliverypg.NewStore(deliverypg.Config{DB: pool, Events: deliverypg.EventsFrom(appender), Guard: deliveryGuard})
	if err != nil {
		return err
	}
	deliveryAccess, err := deliveryadapters.NewAccess(authority)
	if err != nil {
		return err
	}
	deliveryCredentials, err := deliveryadapters.NewCredentials(secretStore)
	if err != nil {
		return err
	}
	deliveryRuns, err := deliveryadapters.NewExecution(execStore)
	if err != nil {
		return err
	}
	deliveryArtifacts, err := deliveryadapters.NewArtifacts(artSvc)
	if err != nil {
		return err
	}
	deliveryAuthority, err := deliveryadapters.NewAuthority(rtVerifier, rtKeys, grants, nil)
	if err != nil {
		return err
	}
	deliverySvc, err := delivery.NewService(delivery.Config{
		Store: deliveryStore, Access: deliveryAccess, Credentials: deliveryCredentials, Contexts: deliveryRuns, Producers: deliveryRuns,
		Artifacts: deliveryArtifacts, Authority: deliveryAuthority, Cleanups: cleanupBindings, Logger: c.log,
	})
	if err != nil {
		return err
	}
	envRefs.Bind(deliverySvc)
	// The committed deployment read of a gate's preparation and the runtime
	// secret route join the delivery records and the runtime authority.
	if joins.DeploymentReader, err = gatesadapters.NewDeployments(deliveryStore); err != nil {
		return err
	}
	secretRoute, err := execadapters.NewSecretRoute(rtVerifier, rtKeys, grants, deliveryStore, secretVersions, nil)
	if err != nil {
		return err
	}
	joins.Secrets = secretRoute
	deliveryKeyed, err := deliverypg.NewKeyed(idem, deliveryStore)
	if err != nil {
		return err
	}
	// The execution store binds environments in its admission transaction through
	// the transaction-bound delivery reader (execpg.Peers.Environments, bound
	// above by T-097's join), so the environment resolver and the cleanup
	// environment are bound with it; a store without the binding would leave
	// execution to refuse an environment-declaring admission itself. The effect
	// reconciler and the cleanup authorization are bound regardless.
	ports, err := BindDelivery(DeliveryPortInputs{
		Env: cfg.Env, Delivery: deliverySvc, Records: deliveryStore, Runs: execStore, Bindings: cleanupBindings,
		AdmitsEnvironments: execStore.AdmitsEnvironments(), Tools: cfg.DeliveryTools,
	})
	if err != nil {
		return err
	}

	// ---- logs: the read API and the capture-before-removal controller ----
	logStore, err := logspg.NewStore(logspg.Config{DB: pool})
	if err != nil {
		return err
	}
	logAttempts, err := logsadapters.NewAttempts(execStore)
	if err != nil {
		return err
	}
	var placement logsadapters.Placement = noPlacement{}
	if rtSet.Placement != nil {
		placement = rtSet.Placement
	}
	logWorkloads, err := logsadapters.NewWorkloads(placement)
	if err != nil {
		return err
	}
	var live logs.LiveReader = noLive{}
	if rtSet.Live != nil {
		live = rtSet.Live
	}
	logReader, err := logsadapters.NewArtifacts(artStore, objects)
	if err != nil {
		return err
	}
	retained, err := logsadapters.NewRetained(logReader, logsadapters.WriterConfig{
		Repository: artStore, Objects: objects, SpoolDir: logSpoolDir(cfg),
		Limits: artifacts.Limits{DiagnosticBytes: tune.Limit.MaxDiagnosticBytes, DiagnosticFiles: controllerDiagnosticFiles},
	})
	if err != nil {
		return err
	}
	logAuthority, err := logsadapters.NewAuthority(authority, idStore)
	if err != nil {
		return err
	}
	lt := tune.Logs
	var redactions logs.Redactions
	if disclosures != nil {
		if redactions, err = logs.WithPriorDisclosures(noOwnRedactions{}, disclosures); err != nil {
			return err
		}
	}
	logSvc, err := logs.NewService(logs.Config{
		Redactions: redactions,
		Authority:  logAuthority, Attempts: logAttempts, Workloads: logWorkloads, Live: live, Retained: retained, Store: logStore,
		Limits: logs.Limits{MaxLines: lt.MaxLines, MaxBytes: lt.MaxBytes, MaxDuration: lt.MaxDuration, MaxStreams: lt.MaxStreams, WriteTimeout: lt.WriteTimeout},
	})
	if err != nil {
		return err
	}
	logController, err := logs.NewController(logs.ControllerConfig{
		Redactions: redactions,
		Attempts:   logAttempts, Workloads: logWorkloads, Live: live, Retained: retained, Store: logStore,
		CaptureBytes: lt.CaptureBytes, CaptureTimeout: lt.CaptureTimeout, Lease: lt.Lease, SpoolDir: logSpoolDir(cfg),
	})
	if err != nil {
		return err
	}

	// ---- execution services: admission, dispatch, cancellation and repair ----
	execAuthority, err := execadapters.NewAuthority(authority, idStore)
	if err != nil {
		return err
	}
	definitions, err := execadapters.NewDefinitions(pipeStore)
	if err != nil {
		return err
	}
	configResolver, err := secrets.NewConfigurationResolver(secretStore, secretStore, nil)
	if err != nil {
		return err
	}
	execConfig, err := execadapters.NewConfiguration(configResolver)
	if err != nil {
		return err
	}
	workContent, err := execadapters.NewWorkContent(workStore)
	if err != nil {
		return err
	}
	cancelled, err := execadapters.NewCancelledWork(workStore)
	if err != nil {
		return err
	}
	sources, err := execadapters.NewSources(wsStore, gitReader, wsSvc)
	if err != nil {
		return err
	}
	limits := execadapters.StaticLimits{Default: tune.Limit}
	if err := limits.Validate(); err != nil {
		return err
	}
	// ---- human gates (T-072): the gate store joins the execution store's
	// transactions (the claim, completion and rejection of a gate step). The
	// deployment binder (T-097) lets a deployment-bearing gate be decided and
	// consumed in its own transaction; the conformance mapping reads the report
	// output the OpenCode wrapper declares (T-031, ConformanceReportOutput), so a
	// gate of an agent-produced candidate can be created. Either join absent
	// leaves exactly its gates unavailable (DeliveryJoins). ----
	gateStore, err := gatespg.NewStore(joins.gateStore(gatespg.Config{DB: pool, Events: appender, Execution: execStore, Authority: gatesadapters.BindAuthority}))
	if err != nil {
		return err
	}
	gateAuthority, err := gatesadapters.NewAuthority(authority, idStore)
	if err != nil {
		return err
	}
	gateEvidence, err := gatesadapters.NewEvidence(artStore, objects)
	if err != nil {
		return err
	}
	gatePins, err := gatesadapters.NewPins(artStore)
	if err != nil {
		return err
	}
	gateCandidates, err := gatesadapters.NewCandidates(sources)
	if err != nil {
		return err
	}
	gateSvc, err := gates.NewService(joins.gateDependencies(gates.Dependencies{
		Store: gateStore, Authority: gateAuthority, Runs: execStore, Evidence: gateEvidence, Pins: gatePins,
		Candidates: gateCandidates, Tenants: tenants,
	}, gatesadapters.ConformanceMapping{ReportOutput: joins.ConformanceOutput}), gates.Options{GateTTL: tune.GateTTL, Batch: tune.ReconcileBatch, Logger: c.log})
	if err != nil {
		return err
	}
	gateExec, err := gatespg.NewExecutionStore(pool, execStore, execStore, gateStore)
	if err != nil {
		return err
	}
	gateKeyed, err := gatespg.NewKeyed(idem, gateStore)
	if err != nil {
		return err
	}
	gateDecisions, err := gateStore.DecisionBinding("")
	if err != nil {
		return err
	}

	// ---- agent steps (T-072): one chain binds the expanding store, the keyed
	// creator, the planner decoration and the step kinds together. ----
	runKeyed, err := execpg.NewKeyed(idem, execStore)
	if err != nil {
		return err
	}
	agentResolver, err := agentsadapters.NewResolver(agentSvc)
	if err != nil {
		return err
	}
	chain, err := agentsadapters.NewChain(agentsadapters.ChainConfig{
		Resolver: agentResolver, Store: gateExec, Creator: runKeyed, Planner: rtSet.Planner,
	})
	if err != nil {
		return err
	}

	// The effect reconciler and the cleanup authorization are bound from the
	// delivery module: an apply whose answer was lost resolves from its
	// deployment record, and a cleanup run is admitted only for a recorded,
	// cleanup-requested deployment of the scope.
	slice, err := AssembleExecution(ExecutionInputs{
		Env: cfg.Env, Log: c.log, Tuning: tune,
		Deps: execution.Dependencies{
			Authority: execAuthority, Definitions: definitions, Capabilities: chain.Capabilities(pipeline.KindGate), Work: workContent,
			Configuration: execConfig, Sources: sources, Limits: limits, Hooks: chain.Hooks,
			Tenants: tenants, CancelledWork: cancelled,
			Cleanup: ports.Cleanup, CleanupEnvironments: ports.CleanupEnvironments, EffectReconciler: ports.EffectReconciler,
		},
		Options: execution.Options{Batch: tune.ReconcileBatch, Logger: c.log},
		Plain:   Decorated(chain.Store, execStore), Guarded: guardedExec, Runtime: rtSet,
		Environments: ports.Environments, AgentPlanner: chain.Planner, Deployments: deliverySvc,
		Authority: resultAuthority, Evidence: resultEvidence, Source: resultSource, Secrets: joins.Secrets,
		Auth: runtimeauth.Config{
			Store: grants, Attempts: rtAttempts, Tokens: signer, Verifier: rtVerifier,
			Wrapper: runtimeauth.ExactWrapperPolicy{Supported: []string{entrypoint.Version}}, Policy: policy,
		},
		Late: late, Artifacts: artSvc, Logs: logSvc, Capture: logController, Shared: sharedInputs,
	})
	if err != nil {
		return fmt.Errorf("execution assembly: %w", err)
	}
	c.Slice, c.Execution, c.runtime, c.runtimeV2 = slice, slice.Trusted, slice.Runtime, slice.RuntimeV2
	if rtSet.Shared != nil {
		// The additive run view and the resource logs of the run_shared model, in
		// front of the unchanged public router.
		diagnostics, err := logsadapters.NewAuthority(authority, idStore)
		if err != nil {
			return err
		}
		if c.sharedPublic, err = NewSharedPublic(SharedPublicInputs{
			Log: c.log, Authenticator: authn, Authority: authority, Reader: diagnostics, Runs: execStore, Resources: rtSet.Shared.Resources,
			Secrets: secretVersions, RateLimit: tune.RateLimitPerMinute, Logs: tune.Logs,
		}); err != nil {
			return err
		}
	}
	execSvc := slice.Trusted

	// ---- model gateway and MCP proxy (T-039): selected together by the
	// installation's model-tools.json; absent, they stay unwired and the profile
	// approvals approve nothing (fail closed). ----
	var modelTools *ModelToolSlice
	if cfg.ModelTools.Configured() {
		mt, approvals, err := buildModelToolsPG(modelToolDeps{
			cfg: cfg.ModelTools, env: cfg.Env, installationID: cfg.InstallationID, log: c.log, tuning: tune, cursors: cursors,
			pool: pool, events: appender, authority: authority, access: idStore, secrets: secretStore, keys: provider,
			runtime: slice.Auth, attempts: rtAttempts, generation: rtKeys.Generation, executions: execStore, tenants: tenants,
		})
		if err != nil {
			return fmt.Errorf("model gateway and MCP proxy: %w", err)
		}
		lateApprovals.Bind(approvals)
		modelTools = mt
		c.ModelTools = mt
		c.closers = append(c.closers, mt.Shutdown)
	}

	delSlice, err := AssembleDelivery(DeliveryInputs{
		Env: cfg.Env, Log: c.log, Tuning: tune, Cursors: cursors, Slice: slice,
		Profiles: agentSvc, Gates: gateSvc, GateDecisions: gateKeyed, Delivery: deliverySvc, Deployments: deliveryKeyed,
		Scopes:      projectLister{tenants: tenants, projects: sqlProjects(pool)},
		Definitions: NamedCleanupDefinitions{Repo: pipeStore},
		Unavailable: deliveryUnavailableM4(joins, execStore.AdmitsEnvironments(), modelTools != nil, len(cfg.ModelTools.Catalogue) > 0),
	})
	if err != nil {
		return fmt.Errorf("delivery assembly: %w", err)
	}
	c.Delivery = delSlice

	// ---- native Git trigger (T-100 adapters, T-101 mount): selected by
	// git-triggers.json; absent, nothing is built and git-webhooks stays
	// disabled. The planner's plans are expanded like the ordinary admission
	// routes' (agent steps frozen) before the ledger admits them. ----
	hookStore, err := gwpg.NewStore(pool, gwpg.Options{})
	if err != nil {
		return err
	}
	hookLedger, err := gwpg.NewLedger(pool, execStore)
	if err != nil {
		return err
	}
	hookDefs, err := gwadapters.NewDefinitions(pipeStore)
	if err != nil {
		return err
	}
	hookSecrets, err := gwadapters.NewHookSecrets(gwadapters.HookSecretsConfig{Reader: secretStore, Wrapper: provider, Installation: cfg.InstallationID})
	if err != nil {
		return err
	}
	hookPlanner, err := gwadapters.NewExecutionPlanner(gwadapters.PlannerConfig{Preparer: execSvc, Work: workContent, Repositories: repositories, Workspaces: wsStore})
	if err != nil {
		return err
	}
	hookExpander, err := agentsadapters.NewExpander(agentResolver)
	if err != nil {
		return err
	}
	triggers, err := NewGitTriggers(GitTriggerInputs{
		Env: cfg.Env, Log: c.log, Config: cfg.GitTriggers, Store: hookStore, Secrets: hookSecrets, Definitions: hookDefs,
		Authority: authority, Repositories: repositories, Git: gitReader, Planner: hookPlanner, Ledger: hookLedger, Expander: &hookExpander,
	})
	if err != nil {
		return fmt.Errorf("git trigger: %w", err)
	}
	c.Triggers = triggers

	// ---- business loop (T-075): the dependency graph, the flow summaries, the QA
	// mapping and ChatOps, with the identity-backed work authority. ChatOps
	// contributes an event-bus binding, built with the consumer store below. ----
	loop, loopBindings, err := buildBusinessLoopPG(businessLoopDeps{
		cfg: cfg.BusinessLoop, env: cfg.Env, log: c.log, tuning: tune, cursors: cursors, pool: pool, events: appender,
		workAuthority: workAuthority, executions: execStore, deliveries: deliveryStore, artifacts: artStore, objects: objects, tenants: tenants,
	})
	if err != nil {
		return fmt.Errorf("business loop: %w", err)
	}
	c.BusinessLoop = loop

	// ---- event delivery: the cancellation consumer binds work to execution ----
	binding, err := workpg.CancellationBinding(workpg.CancellationConsumerName, execadapters.TxCancellationRequester(execStore))
	if err != nil {
		return err
	}
	consumerStore, err := eventspg.NewStore(pool, evOpts, append([]eventspg.Binding{binding, gateDecisions}, loopBindings...)...)
	if err != nil {
		return fmt.Errorf("event consumer store: %w", err)
	}
	c.Consumer = consumerStore

	streamCfg := eventsjs.Config{
		Namespace: cfg.NATS.Namespace, Stream: cfg.NATS.Stream, MaxAge: tune.Events.StreamMaxAge, MaxBytes: tune.Events.StreamMaxBytes,
		MaxMsgs: tune.Events.StreamMaxMsgs, DuplicateWindow: tune.Events.StreamDedup, Replicas: 1,
	}
	if err := streamCfg.ValidateAgainst(tune.Events.Retention); err != nil {
		return err
	}
	broker, err := eventsjs.NewPublisher(c.nc, streamCfg)
	if err != nil {
		return err
	}
	ensure, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := broker.EnsureStream(ensure); err != nil {
		return fmt.Errorf("provision the event stream: %w", err)
	}
	c.publisher, err = events.NewPublisher(tenants, appender, broker, events.PublisherConfig{
		Lease: tune.Events.PublishLease, PublishTimeout: tune.Events.PublishTimeout, BatchSize: tune.Events.PublishBatch,
		Interval: tune.Events.PublishEvery, Retry: tune.Events.Retry, Logger: c.log,
	})
	if err != nil {
		return err
	}
	cancelConsumer, err := eventsjs.NewConsumer(c.nc, streamCfg, eventsjs.ConsumerConfig{
		Consumer: workpg.CancellationConsumerName, Durable: cancellationDurable, AckWait: tune.Events.AckWait,
		MaxAckPending: tune.Events.MaxAckPending, FetchBatch: tune.Events.FetchBatch, FetchWait: tune.Events.FetchWait, RetryDelay: tune.Events.RetryDelay,
	}, consumerStore, consumerStore, c.log)
	if err != nil {
		return err
	}
	if err := cancelConsumer.Provision(ensure); err != nil {
		return fmt.Errorf("provision the cancellation consumer: %w", err)
	}
	gateConsumer, err := eventsjs.NewConsumer(c.nc, streamCfg, eventsjs.ConsumerConfig{
		Consumer: gatespg.DecisionConsumerName, Durable: gateDecisionDurable, AckWait: tune.Events.AckWait,
		MaxAckPending: tune.Events.MaxAckPending, FetchBatch: tune.Events.FetchBatch, FetchWait: tune.Events.FetchWait, RetryDelay: tune.Events.RetryDelay,
	}, consumerStore, consumerStore, c.log)
	if err != nil {
		return err
	}
	if err := gateConsumer.Provision(ensure); err != nil {
		return fmt.Errorf("provision the gate decision consumer: %w", err)
	}
	// The business-loop consumers (ChatOps) are provisioned and repaired like the
	// two above: the broker only wakes them, the inbox repair covers a lost
	// message.
	var loopConsumers []runnable
	repairs := []events.Reconciliation{
		consumerStore.RepairReconciliation(workpg.CancellationConsumerName, tune.Events.RepairGrace, tune.ReconcileBatch),
		consumerStore.RepairReconciliation(gatespg.DecisionConsumerName, tune.Events.RepairGrace, tune.ReconcileBatch),
		consumerStore.ReplayReconciliation(),
		consumerStore.PruneReconciliation(),
		{Name: "execution", Run: func(ctx context.Context, tenant domain.TenantID) error {
			_, err := execSvc.ReconcileTenant(ctx, tenant)
			return err
		}},
	}
	for _, lc := range loop.Consumers {
		jc, err := eventsjs.NewConsumer(c.nc, streamCfg, eventsjs.ConsumerConfig{
			Consumer: lc.Spec.Name, Durable: lc.Durable, AckWait: tune.Events.AckWait,
			MaxAckPending: tune.Events.MaxAckPending, FetchBatch: tune.Events.FetchBatch, FetchWait: tune.Events.FetchWait, RetryDelay: tune.Events.RetryDelay,
		}, consumerStore, consumerStore, c.log)
		if err != nil {
			return err
		}
		if err := jc.Provision(ensure); err != nil {
			return fmt.Errorf("provision the %s consumer: %w", lc.Spec.Name, err)
		}
		loopConsumers = append(loopConsumers, runnable{name: "event-consumer:" + lc.Spec.Name, run: jc.Run})
		repairs = append(repairs, consumerStore.RepairReconciliation(lc.Spec.Name, tune.Events.RepairGrace, tune.ReconcileBatch))
	}
	reconciler, err := events.NewReconciler(tenants, tune.ReconcileInterval, c.log, repairs...)
	if err != nil {
		return err
	}

	// ---- observation (SSE) ----
	obsStore, err := obspg.NewStore(pool, appender, obspg.Bindings{Work: obspg.WorkResources{}, Runs: runStates{}})
	if err != nil {
		return err
	}
	obsAccess, err := obsauth.NewAdapter(authority)
	if err != nil {
		return err
	}
	obsSvc, err := observation.NewService(observation.Config{Log: obsStore, State: obsStore, Access: obsAccess, Options: observation.Options{
		PageSize: tune.ObservationPageSize, MaxPageBytes: 1 << 20, PollInterval: tune.ObservationPollInterval, HeartbeatInterval: tune.ObservationHeartbeat,
		AccessInterval: tune.ObservationAccessInterval, MaxSnapshotResources: tune.ObservationMaxResources,
	}})
	if err != nil {
		return err
	}

	// ---- HTTP: deny-by-default router and the supported subset ----
	router, registered, err := NewPublicRouter(PublicInputs{
		Authenticator: authn, Authority: authority, Log: c.log, Tuning: tune, Cursors: cursors, IdempotencyHashKey: cfg.IdempotencyHashKey,
		Identity: idSvc, Work: workSvc, Secrets: secretSvc, SecretsKeyed: secretKeyed, Pipelines: pipeSvc,
		Runs: execSvc, RunCreator: chain.Creator, Observation: obsSvc, Artifacts: artSvc, Workspaces: wsSvc, Slice: slice, Delivery: delSlice,
		ModelTools: modelTools, BusinessLoop: loop,
	})
	if err != nil {
		return err
	}
	c.router = router
	c.Routes = Routes{Registered: registered, Disabled: DisabledCapabilitiesM3(slice, delSlice, M2Slices{ModelTools: modelTools, Triggers: c.Triggers}, loop)}
	c.Routes.Disabled, c.Routes.Reasons = m2JoinReport(c.Routes.Disabled, JoinState{
		Joins: joins, AdmitsEnvironments: execStore.AdmitsEnvironments(), Ceiling: ceiling != nil, Registry: registryConfigured,
		GitTriggers: c.Triggers != nil,
	})

	// ---- readiness ----
	c.workers = newWorkerGroup(context.Background(), c.log)
	js, err := natsjs.New(c.nc)
	if err != nil {
		return err
	}
	httpClient := cfg.OIDC.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: tune.ReadinessTimeout}
	}
	c.ready = &readiness{
		log: c.log, workers: c.workers, timeout: tune.ReadinessTimeout, ttl: tune.ReadinessTTL, now: time.Now,
		probes: []probe{
			{"postgres", func(ctx context.Context) error { return probeDatabase(ctx, pool) }},
			{"nats", func(ctx context.Context) error {
				if !c.nc.IsConnected() {
					return errors.New("the connection is down")
				}
				_, err := js.Stream(ctx, cfg.NATS.Stream)
				return err
			}},
			{"s3", func(ctx context.Context) error {
				_, err := s3client.HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: aws.String(cfg.S3.Bucket)})
				return err
			}},
			{"oidc", func(ctx context.Context) error { return probeIssuer(ctx, httpClient, cfg.OIDC.Issuer) }},
		},
	}

	// ---- workers: registered here, started by Start ----
	c.startWorkers = func(g *workerGroup) {
		g.Go("event-publisher", c.publisher.Run)
		g.Go("event-consumer:"+workpg.CancellationConsumerName, cancelConsumer.Run)
		g.Go("event-consumer:"+gatespg.DecisionConsumerName, gateConsumer.Run)
		for _, lc := range loopConsumers {
			g.Go(lc.name, lc.run)
		}
		g.Go("reconciler", reconciler.Run)
		g.Go("workspace-reconciler", func(ctx context.Context) error { return wsSvc.RunReconciler(ctx, tune.ReconcileInterval) })
		for _, w := range slice.Workers {
			g.Go(w.Name, w.Run)
		}
		for _, w := range delSlice.Workers {
			g.Go(w.Name, w.Run)
		}
		if modelTools != nil {
			for _, w := range modelTools.Workers {
				g.Go(w.Name, w.Run)
			}
		}
		for _, w := range loop.Workers {
			g.Go(w.Name, w.Run)
		}
		g.Go("artifact-reconciler", func(ctx context.Context) error {
			return artReconciler.Run(ctx, tune.ReconcileInterval, func(err error) { c.log.Warn("artifact sweep failed", "error", err) })
		})
	}
	return nil
}

// runnable is a named loop the composition starts with the workers.
type runnable struct {
	name string
	run  func(context.Context) error
}

// tenantSource lists the tenants background workers repair.
type tenantSource struct{ s *eventspg.Store }

func (t tenantSource) TenantIDs(ctx context.Context) ([]domain.TenantID, error) {
	return t.s.TenantIDs(ctx)
}

func probeDatabase(ctx context.Context, pool *storage.Pool) error {
	var found int
	err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relnamespace = 'public'::regnamespace AND relkind = 'r' AND relname = ANY($1)`, sentinelTables).Scan(&found)
	if err != nil {
		return err
	}
	if found != len(sentinelTables) {
		return fmt.Errorf("the schema is not migrated (%d of %d core tables visible)", found, len(sentinelTables))
	}
	return nil
}

func probeIssuer(ctx context.Context, client *http.Client, issuer string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("discovery answered %s", resp.Status)
	}
	return nil
}

// coreRules extends the identity module's default route rules with the actions
// of the modules this assembly registers. An action without a rule is denied,
// so a module registered without its rule here cannot serve a request.
func coreRules() map[httpapi.Action]identityhttp.Rule {
	rules := identityhttp.DefaultRules()
	for a, r := range obshttp.Rules() {
		rules[a] = r
	}
	project := func(perms ...identity.Permission) identityhttp.Rule {
		return identityhttp.Rule{Level: identityhttp.LevelProject, Any: perms}
	}
	for a, r := range map[httpapi.Action]identityhttp.Rule{
		// T-039: project model settings (read: config:read, write: project:settings)
		// and MCP registrations (list: project:read; mutation: project:settings, the
		// module also asks for credential:manage on every write).
		gatewayhttp.ActionRead: project(identity.PermConfigRead), gatewayhttp.ActionUpdate: project(identity.PermProjectSettings),
		mcpproxyhttp.ActionList: project(identity.PermProjectRead), mcpproxyhttp.ActionMutate: project(identity.PermProjectSettings),
		// T-072: agent profiles, human gates, environments and deployment records.
		agentshttp.ActionList: project(identity.PermRunRead), agentshttp.ActionRegister: project(identity.PermRunManage),
		gateshttp.ActionList: project(identity.PermEvidenceRead), gateshttp.ActionRead: project(identity.PermEvidenceRead),
		gateshttp.ActionDecide: project(identity.PermGateDecide),
		"environment.list":     project(identity.PermRunRead), "environment.read": project(identity.PermRunRead),
		"environment.create": project(identity.PermDeploymentManage), "environment.update": project(identity.PermDeploymentManage),
		"deployment.list": project(identity.PermRunRead), "deployment.read": project(identity.PermRunRead),
		"deployment.create": project(identity.PermDeploymentManage), "deployment.cleanup": project(identity.PermDeploymentManage),
		"run.list": project(identity.PermRunRead), "run.read": project(identity.PermRunRead), "run.steps": project(identity.PermRunRead),
		"run.create": project(identity.PermRunManage), "run.cancel": project(identity.PermRunManage),
		"pipeline_definition.list": project(identity.PermRunRead), "pipeline_definition.read": project(identity.PermRunRead),
		"pipeline_definition.register": project(identity.PermRunManage),
		"workspace.read":               project(identity.PermSourceRead),
		"workspace.provision":          project(identity.PermWorkspaceManage), "workspace.sync": project(identity.PermWorkspaceManage),
		"workspace.archive": project(identity.PermWorkspaceManage), "workspace.restore": project(identity.PermWorkspaceManage),
		"artifact.read": project(identity.PermEvidenceRead),
	} {
		rules[a] = r
	}
	for a, perms := range logshttp.RoutePermissions() {
		rules[a] = project(perms...)
	}
	// T-075: business dependency links (reading needs work_item:read, adding or
	// removing needs work_item:edit_spec, the permission of parent edits) and the
	// ChatOps destinations and deliveries (project:settings and project:read, with
	// the store repeating the exact permission inside each transaction).
	rules[dependencieshttp.ActionList] = project(identity.PermWorkRead)
	rules[dependencieshttp.ActionAdd] = project(identity.PermWorkEditSpec)
	rules[dependencieshttp.ActionRemove] = project(identity.PermWorkEditSpec)
	for a, r := range chatopshttp.Rules() {
		rules[a] = r
	}
	return rules
}

// deliveryUnavailable lists the capabilities of the delivery slice that need a
// join no production adapter supplies yet; execution and the gate owner fail
// closed on each (a bounded prerequisite of T-067/T-073/T-077, not a
// default).
func deliveryUnavailable(admitsEnvironments bool) []string {
	return deliveryUnavailableM2(admitsEnvironments, false, false)
}

// deliveryUnavailableM2 is deliveryUnavailable after the M2 composition: the
// model gateway and MCP proxy are no longer listed once they are wired, and
// agent steps stay unavailable until the installation also qualifies a wrapper
// tuple (its catalogue), since no profile can register without one.
func deliveryUnavailableM2(admitsEnvironments, modelTools, qualified bool) []string {
	out := []string{
		"deployment-gates",      // no transaction-bound deployment reader for the gate store
		"agent-candidate-gates", // the conformance report output name is agreed with the wrapper (T-031)
	}
	if !modelTools || !qualified {
		out = append(out, "agent-steps") // no qualified wrapper tuple, approved model or approved tool until T-031/T-085
	}
	if !modelTools {
		out = append(out, "model-gateway", "mcp-proxy")
	}
	if !admitsEnvironments {
		out = append(out, "step-environments") // no transaction-bound delivery reader for the execution store
	}
	return out
}
