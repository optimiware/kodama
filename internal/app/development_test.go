package app

// The development composition (ADR 0009, SD-12): the whole M1 slice assembled
// from the modules' in-memory adapters, with `KODAMA_ENV=development`, the
// Process runtime and no service of any kind. It is test code on purpose: the
// memory adapters live in the modules' `*test` packages and are injected here,
// never installed as a fallback of the executable constructors (NewCore refuses
// `development`, and `production` refuses every adapter this file builds).
//
// Every module serves its real use cases and its real HTTP handlers; the
// router, the authorizer rule table and the route registration are the ones
// NewCore uses (NewPublicRouter). What differs from the PostgreSQL composition
// is named in each module's MEMORY.md and in internal/app/memory.go: no lock
// order, row locks or isolation levels, no restart persistence, and the runtime
// API attempt authority is the memory stand-in (rtest.MemAttempts), so a
// workload's own callbacks are covered by T-071's tests and by T-026.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/agents"
	agentsadapters "github.com/optimiware/kodama/internal/agents/adapters"
	"github.com/optimiware/kodama/internal/agents/agentstest"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/artifacts/artifactstest"
	"github.com/optimiware/kodama/internal/delivery"
	deliveryadapters "github.com/optimiware/kodama/internal/delivery/adapters"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/entrypoint"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/execution"
	execadapters "github.com/optimiware/kodama/internal/execution/adapters"
	execmemory "github.com/optimiware/kodama/internal/execution/memory"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/gates"
	gatesadapters "github.com/optimiware/kodama/internal/gates/adapters"
	gatesmemory "github.com/optimiware/kodama/internal/gates/gatestest/memory"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/identity/consumer"
	identityhttp "github.com/optimiware/kodama/internal/identity/http"
	"github.com/optimiware/kodama/internal/identity/idtest"
	"github.com/optimiware/kodama/internal/integrations/chatops"
	"github.com/optimiware/kodama/internal/logs"
	logsadapters "github.com/optimiware/kodama/internal/logs/adapters"
	"github.com/optimiware/kodama/internal/logs/logstest"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/observation"
	obsauth "github.com/optimiware/kodama/internal/observation/authorization"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	resultsadapters "github.com/optimiware/kodama/internal/results/adapters"
	"github.com/optimiware/kodama/internal/runtime/process"
	"github.com/optimiware/kodama/internal/runtimeauth"
	rtjwt "github.com/optimiware/kodama/internal/runtimeauth/jwt"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/internal/secrets/secretstest"
	"github.com/optimiware/kodama/internal/work"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
	"github.com/optimiware/kodama/internal/work/worktest"
	"github.com/optimiware/kodama/internal/workspace"
	wsruntime "github.com/optimiware/kodama/internal/workspace/runtime"
	"github.com/optimiware/kodama/internal/workspace/workspacetest"
)

// Fixed identities of the development fixture. The tokens are the whole
// credential store of the static verifier.
const (
	devTenant   domain.TenantID  = "t_acme"
	devProject  domain.ProjectID = "p_inv"
	devAdmin                     = "u_admin"
	devEngineer                  = "u_engineer"
	devViewer                    = "u_viewer"
	devOther                     = "u_other"
	// devLead holds the administrator role (project settings) and the engineer
	// role (run management): installing a Git trigger needs the first, every
	// delivery it starts needs the second, and the role matrix keeps them apart.
	devLead    = "u_lead"
	devIssuer  = "https://dev.issuer.example"
	devRepoURL = "https://git.development.invalid/inv-repo.git"

	devAdminToken    = "dev-token-admin"
	devEngineerToken = "dev-token-engineer"
	devViewerToken   = "dev-token-viewer"
	devOtherToken    = "dev-token-other"
)

var devScope = domain.Scope{TenantID: devTenant, ProjectID: devProject}

// staticVerifier is the development identity port: a seeded token table. An
// unknown token is the credential failure, never an anonymous principal.
type staticVerifier map[string]identity.Principal

func (v staticVerifier) DevelopmentOnly() bool { return true }

func (v staticVerifier) Verify(_ context.Context, token string) (identity.Principal, error) {
	p, ok := v[token]
	if !ok {
		return identity.Principal{}, identity.ErrUnauthenticated
	}
	return p, nil
}

type devTenants []domain.TenantID

func (t devTenants) TenantIDs(context.Context) ([]domain.TenantID, error) { return t, nil }

// devSlice is the assembled development composition.
type devSlice struct {
	t   *testing.T
	Env Env

	DB       *memdb.Store
	Events   *eventstest.Mem
	Broker   *eventstest.InProcessBroker
	IDs      *idtest.Mem
	Work     *worktest.MemStore
	Secrets  *secretstest.MemStore
	Pipeline *pipelinetest.MemRepo
	WS       *workspacetest.MemStore
	Git      *workspacetest.LocalGit
	Repo     *workspacetest.LocalRepo
	Exec     *execmemory.Store
	Objects  *artifactstest.MemObjects
	Runtime  *process.Runtime

	WorkSvc *work.Service
	WSSvc   *workspace.Service
	ArtSvc  *artifacts.Service
	Slice   *ExecutionSlice
	// Delivery is the manual agent/gate/delivery slice (T-072).
	Delivery *DeliverySlice
	// DeliveryIn are the inputs the delivery slice was assembled from, so a test
	// can assemble a variant (the gate transitions of TestM2CompositionSmoke
	// replace only the gate service).
	DeliveryIn DeliveryInputs
	// M2 (T-039): the model gateway and MCP proxy over their memory fixtures, the
	// native Git trigger over its in-memory store with the real local Git reader,
	// and the agent profile service whose approvals read the first two.
	ModelTools *ModelToolSlice
	Triggers   *TriggerSlice
	M2         *devM2
	// BusinessLoop (T-075): the dependency graph, the flow summaries, the QA
	// mapping and ChatOps over their deterministic fixtures.
	BusinessLoop *BusinessLoopSlice
	BL           *devBusinessLoop
	Core         *Core
	Handler      http.Handler
	Server       *httptest.Server
	Routes       Routes
	Tuning       Tuning
	Idem         *memIdempotency
	Cancels      *int
	Tenants      devTenants
	// Fault, when set, makes every commit of the kernel fail (rollback tests).
	Fault atomic.Bool
	// Attempts, Jobs and Late are the runtime-authority seams a test uses to play
	// the entrypoint's admission.
	Attempts *rtest.MemAttempts
	Jobs     *rtest.Jobs
	Late     *LateRegistrar
	Bindings []eventstest.MemBinding
}

// newDevSlice assembles the slice. It fails the test on any construction error.
func newDevSlice(t *testing.T) *devSlice {
	t.Helper()
	d, err := buildDevSlice(t, Development)
	if err != nil {
		t.Fatalf("the development composition: %v", err)
	}
	return d
}

func buildDevSlice(t *testing.T, env Env) (*devSlice, error) {
	t.Helper()
	d := &devSlice{t: t, Env: env, Tuning: DefaultTuning(), Tenants: devTenants{devTenant}, Jobs: &rtest.Jobs{}}
	d.Tuning.DispatchInterval = 5 * time.Millisecond
	d.Tuning.RateLimitPerMinute = 100000
	d.Tuning.ChatOps = fastChatOps()
	log := slog.New(slog.DiscardHandler)
	ctx := context.Background()
	check := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	// Every adapter the composition injects is refused by `production` (SD-12):
	// Admit is the one guard, applied to each below through admitAll.
	var adapters []any

	// ---- the transaction kernel and the event bus ----
	d.DB = memdb.New(memdb.WithCommitFault(func() error {
		if d.Fault.Load() {
			return errors.New("injected commit failure")
		}
		return nil
	}))
	holder := &execHolder{}
	cancelBinding, err := cancellationBinding(holder)
	if err != nil {
		return nil, err
	}
	// The ChatOps consumer joins the bus with the module's own spec and planner
	// (T-075); its store is bound to the identity authority below.
	d.BL = newDevBusinessLoop(t)
	opts := eventstest.DefaultMemOptions()
	d.Events, err = eventstest.NewMem(d.DB, opts, cancelBinding, d.BL.Chat.Binding())
	check(err)
	d.Events.AddTenant(devTenant)
	adapters = append(adapters, d.DB, d.Events)
	d.Broker, err = eventstest.NewInProcessBroker(d.Events)
	check(err)
	d.Broker.Subscribe(workpg.CancellationConsumerName, d.Events)
	d.Broker.Subscribe(chatops.ConsumerName, d.Events)
	adapters = append(adapters, d.Broker)

	// ---- identity: the standalone authority over a static, seeded verifier ----
	d.IDs, err = idtest.NewMemOn(d.DB, d.Events)
	check(err)
	adapters = append(adapters, d.IDs)
	idSvc, err := identity.NewService(identity.Config{Repository: d.IDs, Directory: d.IDs})
	check(err)
	standalone, err := identity.NewStandaloneAuthority(d.IDs)
	check(err)
	authority, err := identity.SelectAuthority(identity.AuthorityConfig{Profile: identity.ProfileStandalone, Standalone: standalone})
	check(err)
	d.IDs.AddTenant(devTenant, "acme")
	d.IDs.AddProject(devTenant, devProject, "inv")
	d.IDs.AddUser(devAdmin, devIssuer, "sub-admin")
	d.IDs.AddUser(devEngineer, devIssuer, "sub-engineer")
	d.IDs.AddUser(devViewer, devIssuer, "sub-viewer")
	d.IDs.AddUser(devOther, devIssuer, "sub-other")
	d.IDs.AddUser(devLead, devIssuer, "sub-lead")
	d.IDs.AddBinding(devTenant, "mb_admin", devAdmin, "", identity.RoleAdmin)
	d.IDs.AddBinding(devTenant, "mb_engineer", devEngineer, devProject, identity.RoleEngineer)
	d.IDs.AddBinding(devTenant, "mb_viewer", devViewer, devProject, identity.RoleViewer)
	d.IDs.AddBinding(devTenant, "mb_lead_admin", devLead, "", identity.RoleAdmin)
	d.IDs.AddBinding(devTenant, "mb_lead_engineer", devLead, devProject, identity.RoleEngineer)
	verifier := staticVerifier{
		devAdminToken:    {Issuer: devIssuer, Subject: "sub-admin"},
		devEngineerToken: {Issuer: devIssuer, Subject: "sub-engineer"},
		devViewerToken:   {Issuer: devIssuer, Subject: "sub-viewer"},
		devOtherToken:    {Issuer: devIssuer, Subject: "sub-other"},
	}
	cons, err := NewConstructors(env)
	check(err)
	adapters = append(adapters, verifier)
	authn, err := identityhttp.NewAuthenticator(verifier, d.IDs)
	check(err)
	cursors, err := httpapi.NewCursorCodec(bytes32('c'))
	check(err)

	// ---- work ----
	workAuthority, err := consumer.NewWorkAuthority(authority, d.IDs)
	check(err)
	// The writer guard reads the composed execution state, which is built after
	// the work store: it is bound late and fails closed until then.
	writers := NewMemoryWriterGuard(func() WriterRuns {
		if holder.store == nil {
			return nil
		}
		return holder.store
	})
	d.Work, err = worktest.NewMemStore(d.DB, d.Events, func(tx memdb.Tx) work.TxGuards {
		perms, err := consumer.NewWorkAuthority(d.IDs.AuthorityIn(tx), d.IDs)
		if err != nil {
			return work.TxGuards{}
		}
		return work.TxGuards{Permissions: perms, Writers: writers, Dependencies: d.BL.Deps.Guard()}
	})
	check(err)
	// The work module reads the project and its assignable members through the
	// identity module's tables in PostgreSQL; its memory twin is seeded alongside.
	check(d.Work.SeedProject(ctx, devScope, 5))
	check(d.Work.AddMember(ctx, devTenant, devEngineer, devProject))
	adapters = append(adapters, d.Work)
	d.WorkSvc, err = work.NewService(work.Config{Repository: d.Work, Authority: workAuthority})
	check(err)
	d.BL.Deps.items = d.Work
	d.BL.Chat.auth = authority

	// ---- secrets and variables: the key wrapper is the ephemeral development KEK ----
	wrapper, err := secretstest.NewEphemeralWrapper()
	check(err)
	adapters = append(adapters, wrapper)
	if env == Development {
		if _, _, err := cons.KeyWrapper(context.Background(), SecretsConfig{}); !errors.Is(err, errNotComposed) {
			return nil, errors.New("the development constructors must leave the key wrapper to the composition")
		}
	}
	d.Secrets, err = secretstest.NewMemStore(secretstest.MemConfig{Store: d.DB, Events: d.Events, IDs: secrets.RandomIDs{}})
	check(err)
	adapters = append(adapters, d.Secrets)
	secretSvc, err := secrets.NewService(secrets.ServiceConfig{InstallationID: "inst_dev", Store: d.Secrets, Wrapper: wrapper, IDs: secrets.RandomIDs{}})
	check(err)
	d.Idem = newMemIdempotency(nil)
	secretKeyed := &memSecretsKeyed{store: d.Secrets, idem: d.Idem}

	// ---- pipeline definitions: containers only ----
	// T-039: the agent step kind is enabled on the composition. Profiles are
	// registered through the same service the public routes use, whose
	// compatibility catalogue is the installation's statement and whose approvals
	// read the stored gateway settings and MCP registrations (LateApprovals is
	// bound once those exist, below). Gate steps need the gate-aware execution
	// store of the PostgreSQL composition; the gate transitions of the M2
	// composition are exercised by TestM2CompositionSmoke over the T-072 gate
	// fixture and this composition's delivery slice.
	kinds := pipeline.EnabledKinds{pipeline.KindContainer, pipeline.KindAgent}
	agentAuthority, err := agentsadapters.NewAuthority(authority, d.IDs)
	check(err)
	devCatalogue, err := devModelTools().catalogue()
	check(err)
	lateApprovals := &LateApprovals{}
	agentSvc, err := agents.NewService(agents.ServiceConfig{Repository: &agentstest.MemRepo{}, Authority: agentAuthority, Compatibility: devCatalogue, Approvals: lateApprovals})
	check(err)
	profileLookup, err := agentsadapters.NewLookup(agentSvc)
	check(err)
	validator, err := pipeline.NewValidator(pipeline.Config{
		Capabilities: kinds, Profiles: profileLookup, References: pipelineReferences{authority: authority, catalog: d.Secrets},
	})
	check(err)
	d.Pipeline = &pipelinetest.MemRepo{}
	pipeSvc, err := pipeline.NewService(pipeline.ServiceConfig{
		Repository: d.Pipeline, Authority: pipelineAuthority{authority: authority, reader: d.IDs}, Validator: validator,
	})
	check(err)

	// ---- artifacts ----
	artStore, err := artifactstest.NewMemStore(d.DB)
	check(err)
	d.Objects = artifactstest.NewMemObjects(time.Now)
	if _, err := d.Objects.Serve(); err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = d.Objects.Close() })
	adapters = append(adapters, artStore, d.Objects)

	// ---- execution store over the real peers' memory adapters ----
	d.WS, err = workspacetest.NewMemStore(d.DB, d.Events, workspacetest.MemOptions{})
	check(err)
	adapters = append(adapters, d.WS)
	wsPeer, err := MemoryWorkspaces(d.WS, nil)
	check(err)
	d.Exec, err = execmemory.NewStore(d.DB, execmemory.Peers{
		Authority:  MemoryAuthorityBinder(d.IDs.AuthorityIn),
		Work:       MemoryWorkLock(worktest.LockActionable),
		Workspaces: wsPeer,
		Secrets: MemorySecretPinsFrom(
			func(tx memdb.Tx, scope domain.Scope, c secrets.ConsumerRef, b []runtimev1.SecretBinding) error {
				return d.Secrets.RegisterPins(tx, secretstest.PinRequest{Scope: scope, Consumer: c, Bindings: b})
			},
			d.Secrets.ReleasePins),
		Events: MemoryEvents(func(tx memdb.Tx, env eventsv1.Envelope) error { _, err := d.Events.Append(tx, env); return err }),
		Grants: func(tx memdb.Tx, scope domain.Scope, attempt domain.AttemptID, now time.Time) error {
			_, err := rtest.RevokeAttemptMem(tx, scope, attempt, now)
			return err
		},
	}, execmemory.Options{})
	check(err)
	holder.store = d.Exec
	adapters = append(adapters, d.Exec, wsPeer)

	// ---- workspaces over temporary bare Git ----
	d.Git, err = workspacetest.NewLocalGit()
	check(err)
	t.Cleanup(func() { _ = d.Git.Close() })
	d.Repo = d.Git.NewRepo(t, "inv-repo")
	gitReader, gitPreparer, gitPublisher, err := NewMemoryGit(d.Git.Reader(), d.Git.Preparer(), d.Git.Publisher(), map[string]string{devRepoURL: d.Repo.Path})
	check(err)
	repos := newMemRepositories()
	repos.Set(devScope, workspace.RepositoryConfig{
		ID: "repo_dev", URL: devRepoURL, CheckoutSecretRef: workspacetest.CheckoutRef, PublisherSecretRef: workspacetest.PublisherRef, BaseRef: d.Repo.BaseRef,
	})
	adapters = append(adapters, repos, gitReader, gitPreparer, gitPublisher)
	bundles, err := resultsadapters.NewBundles(artStore, d.Objects)
	check(err)
	wsDeps := workspace.Dependencies{
		Store: d.WS, Tenants: d.Tenants, Authorizer: workspaceAuthorizer{authority: authority},
		WorkItems: workspaceItems{repo: d.Work}, Repositories: repos, Attempts: MemoryAttempts(d.Exec),
		Bundles: bundles, Secrets: attemptNeedles{runs: d.Exec},
		Refs: gitReader, Source: gitPreparer, Publisher: gitPublisher, Logger: log,
	}
	d.WSSvc, err = workspace.NewService(wsDeps, workspacetest.DefaultOptions())
	check(err)

	// ---- the runtime of the environment (Process) and its authority ----
	processRT, err := process.New(process.Config{Program: process.DeclaredCommand{}, Dir: t.TempDir()})
	check(err)
	d.Runtime = processRT
	attempts := rtest.NewMemAttempts(d.DB)
	d.Attempts = attempts
	grants, err := rtest.NewMemStore(d.DB, attempts)
	check(err)
	adapters = append(adapters, attempts, grants)
	rtSet := RuntimeSet{
		// The Process runtime starts the workloads; the workload identity the
		// runtime API verifies is the controllable fixture observer, so a test can
		// play the entrypoint's admission against a real committed attempt.
		Runtime: processRT, Jobs: devJobs{d.Jobs},
		Authority: func(*execpg.Store) (rtpg.AttemptAuthority, error) {
			return nil, errors.New("the development runtime authority is the memory attempt authority")
		},
	}
	rtKey, err := rtjwt.GenerateKey()
	check(err)
	ring, err := rtjwt.NewKeyRing(1, rtKey)
	check(err)
	signer, err := rtjwt.NewSigner(ring, "http://127.0.0.1:0")
	check(err)
	rtVerifier, err := rtjwt.NewVerifier(rtjwt.VerifierConfig{Ring: ring, Issuer: "http://127.0.0.1:0", MaxLifetime: d.Tuning.MaxSession})
	check(err)
	guarded, err := NewMemoryGuardedStore(d.Exec, d.DB, d.Exec, func(tx memdb.Tx) runtimeauth.GrantTx { return rtest.BindMem(tx, attempts) }, ring.Generation)
	check(err)
	adapters = append(adapters, guarded, writers)
	resultAuthority, err := resultsadapters.NewAuthority(rtVerifier, ring, grants, nil)
	check(err)
	artifactAuthority, err := resultsadapters.NewArtifactAuthority(grants, ring, nil)
	check(err)
	artSvc, err := artifacts.NewService(artifacts.ServiceConfig{
		Repository: artStore, Authority: artifactAuthority, Reader: artifactReadAuthority{authority: authority}, Objects: d.Objects, Registry: deniedRegistry{},
		UploadTTL: 10 * time.Minute, DownloadTTL: 5 * time.Minute, ClaimLease: 30 * time.Second, SpoolDir: t.TempDir(),
	})
	check(err)
	d.ArtSvc = artSvc
	resultEvidence, err := resultsadapters.NewArtifacts(artSvc)
	check(err)
	proposer, err := wsruntime.NewSource(d.WSSvc)
	check(err)
	resultSource, err := resultsadapters.NewSource(proposer)
	check(err)

	// ---- logs: retained reads over the artifact store ----
	logStore := logstest.NewMemory()
	logAttempts, err := logsadapters.NewAttempts(d.Exec)
	check(err)
	logWorkloads, err := logsadapters.NewWorkloads(noPlacement{})
	check(err)
	logReader, err := logsadapters.NewArtifacts(artStore, d.Objects)
	check(err)
	retained, err := logsadapters.NewRetained(logReader, logsadapters.WriterConfig{
		Repository: artStore, Objects: d.Objects, SpoolDir: t.TempDir(),
		Limits: artifacts.Limits{DiagnosticBytes: d.Tuning.Limit.MaxDiagnosticBytes, DiagnosticFiles: controllerDiagnosticFiles},
	})
	check(err)
	logAuthority, err := logsadapters.NewAuthority(authority, d.IDs)
	check(err)
	lt := d.Tuning.Logs
	logSvc, err := logs.NewService(logs.Config{
		Authority: logAuthority, Attempts: logAttempts, Workloads: logWorkloads, Live: noLive{}, Retained: retained, Store: logStore.Store(),
		Limits: logs.Limits{MaxLines: lt.MaxLines, MaxBytes: lt.MaxBytes, MaxDuration: lt.MaxDuration, MaxStreams: lt.MaxStreams, WriteTimeout: lt.WriteTimeout},
	})
	check(err)
	logController, err := logs.NewController(logs.ControllerConfig{
		Attempts: logAttempts, Workloads: logWorkloads, Live: noLive{}, Retained: retained, Store: logStore.Store(),
		CaptureBytes: lt.CaptureBytes, CaptureTimeout: lt.CaptureTimeout, Lease: lt.Lease, SpoolDir: t.TempDir(),
	})
	check(err)
	adapters = append(adapters, logStore)

	// ---- execution services ----
	execAuthority, err := execadapters.NewAuthority(authority, d.IDs)
	check(err)
	definitions, err := execadapters.NewDefinitions(d.Pipeline)
	check(err)
	resolver, err := secrets.NewConfigurationResolver(d.Secrets, d.Secrets, nil)
	check(err)
	execConfig, err := execadapters.NewConfiguration(resolver)
	check(err)
	workContent, err := execadapters.NewWorkContent(d.Work)
	check(err)
	cancelled, err := execadapters.NewCancelledWork(d.Work)
	check(err)
	sources, err := execadapters.NewSources(d.WS, gitReader, d.WSSvc)
	check(err)
	limits := execadapters.StaticLimits{Default: d.Tuning.Limit}
	check(limits.Validate())
	late := &LateRegistrar{}
	d.Late = late
	// The agent chain binds the expanding store, the keyed creator, the planner
	// decoration and the step kinds together, as NewCore does.
	runCreator := &memRunCreator{db: d.DB, store: d.Exec, idem: d.Idem}
	agentResolver, err := agentsadapters.NewResolver(agentSvc)
	check(err)
	chain, err := agentsadapters.NewChain(agentsadapters.ChainConfig{Resolver: agentResolver, Store: d.Exec, Creator: runCreator, Planner: rtSet.Planner})
	check(err)
	d.Slice, err = AssembleExecution(ExecutionInputs{
		Env: env, Log: log, Tuning: d.Tuning,
		Deps: execution.Dependencies{
			Authority: execAuthority, Definitions: definitions, Capabilities: chain.Capabilities(), Work: workContent,
			Configuration: execConfig, Sources: sources, Limits: limits, Hooks: chain.Hooks,
			Tenants: d.Tenants, CancelledWork: cancelled,
		},
		Options: execution.Options{Batch: d.Tuning.ReconcileBatch, Logger: log},
		Plain:   Decorated(chain.Store, d.Exec), Guarded: guarded, Runtime: rtSet, AgentPlanner: chain.Planner,
		Authority: resultAuthority, Evidence: resultEvidence, Source: resultSource,
		Auth: runtimeauth.Config{
			Store: grants, Attempts: attempts, Tokens: signer, Verifier: rtVerifier,
			Wrapper: runtimeauth.ExactWrapperPolicy{Supported: []string{entrypoint.Version}},
			Policy:  runtimeauth.Policy{AdmissionTTL: d.Tuning.AdmissionTTL, TokenTTL: d.Tuning.TokenTTL, MaxSession: d.Tuning.MaxSession},
		},
		Late: late, Artifacts: artSvc, Logs: logSvc, Capture: logController,
	})
	if err != nil {
		return nil, err
	}
	holder.svc = d.Slice.Trusted
	adapters = append(adapters, d.Slice.Runtime)
	_ = adapters

	// ---- HTTP: the same router, rule table and registration as NewCore ----
	obsLog, obsState, err := NewMemoryObservation(devObservation{d: d})
	check(err)
	obsAccess, err := obsauth.NewAdapter(authority)
	check(err)
	obsSvc, err := observation.NewService(observation.Config{Log: obsLog, State: obsState, Access: obsAccess, Options: observation.Options{
		PageSize: d.Tuning.ObservationPageSize, MaxPageBytes: 1 << 20, PollInterval: d.Tuning.ObservationPollInterval, HeartbeatInterval: d.Tuning.ObservationHeartbeat,
		AccessInterval: d.Tuning.ObservationAccessInterval, MaxSnapshotResources: d.Tuning.ObservationMaxResources,
	}})
	check(err)

	// ---- the manual agent/gate/delivery slice (T-072): the real use cases and
	// handlers over the modules' memory fixtures; the identity authority is the
	// development one. The step kinds of the execution slice stay container-only
	// (the M1 behavior): agent steps and gate steps are enabled by the agent chain
	// and the gate-aware store of the PostgreSQL composition (NewCore). ----
	gateEnv := gatesmemory.New()
	gateDeps := gateEnv.Dependencies()
	gateAuthority, err := gatesadapters.NewAuthority(authority, d.IDs)
	check(err)
	gateDeps.Authority = gateAuthority
	gateSvc, err := gates.NewService(gateDeps, gateEnv.ServiceOptions())
	check(err)
	m2, err := newDevM2(t, env, devM2Inputs{
		Log: log, Tuning: d.Tuning, Cursors: cursors, Tenants: d.Tenants, Authority: authority, Access: d.IDs, Repos: repos, Git: gitReader,
	})
	if err != nil {
		return nil, err
	}
	d.M2, d.ModelTools, d.Triggers = m2, m2.Slice, m2.Triggers
	lateApprovals.Bind(m2.Approvals)
	m2.Profiles = agentSvc
	deliveryAccess, err := deliveryadapters.NewAccess(authority)
	check(err)
	deliverySvc, err := delivery.NewService(delivery.Config{Store: newDVStore(), Access: deliveryAccess, Credentials: dvCredentials{}})
	check(err)
	d.DeliveryIn = DeliveryInputs{
		Env: env, Log: log, Tuning: d.Tuning, Cursors: cursors, Slice: d.Slice,
		Profiles: agentSvc, Gates: gateSvc, GateDecisions: dvDecider{}, Delivery: deliverySvc, Deployments: dvRegistrar{},
		Scopes: &dvScopes{}, Definitions: dvDefinitions{}, Unavailable: deliveryUnavailableM2(false, true, true),
	}
	d.Delivery, err = AssembleDelivery(d.DeliveryIn)
	check(err)

	// ---- the business loop (T-075): the real use cases and handlers over the
	// fixtures; the authority is the development one ----
	d.BusinessLoop, err = d.BL.assemble(env, d.Tuning.ChatOps, log, cursors, d.Tenants, workAuthority)
	if err != nil {
		return nil, err
	}

	router, registered, err := NewPublicRouter(PublicInputs{
		Authenticator: authn, Authority: authority, Log: log, Tuning: d.Tuning, Cursors: cursors, IdempotencyHashKey: bytes32('h'),
		Identity: idSvc, Work: d.WorkSvc, Secrets: secretSvc, SecretsKeyed: secretKeyed, Pipelines: pipeSvc,
		Runs: d.Slice.Trusted, RunCreator: chain.Creator, Observation: obsSvc, Artifacts: artSvc, Workspaces: d.WSSvc, Slice: d.Slice, Delivery: d.Delivery,
		ModelTools: d.ModelTools, BusinessLoop: d.BusinessLoop,
	})
	if err != nil {
		return nil, err
	}
	d.Routes = Routes{Registered: registered, Disabled: DisabledCapabilitiesM3(d.Slice, d.Delivery, M2Slices{ModelTools: d.ModelTools, Triggers: d.Triggers}, d.BusinessLoop)}

	// ---- the process handler: probes, the runtime API and the public router ----
	workers := newWorkerGroup(context.Background(), log)
	d.Core = &Core{
		cfg: CoreConfig{Env: env, Tuning: d.Tuning}, log: log, router: router, runtime: d.Slice.Runtime, workers: workers, Slice: d.Slice,
		Execution: d.Slice.Trusted, Workspace: d.WSSvc, Routes: d.Routes, ModelTools: d.ModelTools, Triggers: d.Triggers, BusinessLoop: d.BusinessLoop,
		ready: &readiness{log: log, workers: workers, timeout: time.Second, ttl: time.Millisecond, now: time.Now},
	}
	d.Handler = d.Core.Handler()
	d.Server = httptest.NewServer(d.Handler)
	t.Cleanup(func() {
		d.Server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = workers.Stop(ctx)
	})
	_ = ctx
	return d, nil
}

// bytes32 is a fixed development key of the minimum length.
func bytes32(b byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = b
	}
	return k
}

// execHolder lets the cancellation consumer, which the event bus is built with,
// reach the execution store and service that are built after the bus.
type execHolder struct {
	store *execmemory.Store
	svc   *execution.Service
}

// cancellationBinding is the memory twin of work/postgres.CancellationBinding:
// a work item's cancellation event records the cancellation intent and cancels
// the item's runs inside the consumer's own inbox transaction.
func cancellationBinding(h *execHolder) (eventstest.MemBinding, error) {
	spec := events.ConsumerSpec{
		Name: workpg.CancellationConsumerName, Mode: events.ModeCurrentState, Effect: events.EffectExternalIntent,
		Types: []string{eventsv1.TypeWorkItemTransitioned},
	}
	if err := spec.Validate(); err != nil {
		return eventstest.MemBinding{}, err
	}
	return eventstest.MemBinding{Spec: spec, Handle: func(ctx context.Context, tx memdb.Tx, env eventsv1.Envelope) error {
		req, ok, err := work.CancellationFromEvent(env)
		if err != nil {
			return events.Permanent(err)
		}
		if !ok {
			return nil
		}
		return h.store.RequestCancellationInTx(ctx, tx, execution.WorkCancellation{Scope: req.Scope, WorkItemID: req.WorkItemID, Version: req.Version})
	}}, nil
}

// devObservation reads the event log and the current state of the slice's
// resources for the observation module, over the same kernel as every writer.
type devObservation struct{ d *devSlice }

func (o devObservation) eventScope(s observation.Scope) events.Scope {
	return events.Scope{TenantID: s.Tenant, ProjectID: s.Project}
}

func (o devObservation) Watermark(ctx context.Context, scope observation.Scope) (observation.Watermark, error) {
	var out observation.Watermark
	err := o.d.DB.View(ctx, scope.Tenant, func(tx memdb.Tx) error {
		hw, err := o.d.Events.Watermark(tx, o.eventScope(scope))
		out = observation.Watermark{HighWater: hw}
		return err
	})
	return out, err
}

func (o devObservation) Read(ctx context.Context, scope observation.Scope, after int64, limit int) ([]observation.Record, observation.Watermark, error) {
	var (
		recs []observation.Record
		wm   observation.Watermark
	)
	err := o.d.DB.View(ctx, scope.Tenant, func(tx memdb.Tx) error {
		obs, err := o.d.Events.ReadObservations(tx, o.eventScope(scope), after, limit)
		if err != nil {
			return err
		}
		for _, r := range obs {
			recs = append(recs, observation.Record{Sequence: r.Sequence, Envelope: r.Envelope, Size: len(r.Payload)})
		}
		hw, err := o.d.Events.Watermark(tx, o.eventScope(scope))
		wm = observation.Watermark{HighWater: hw}
		return err
	})
	return recs, wm, err
}

func (o devObservation) Exists(ctx context.Context, scope observation.Scope) (bool, error) {
	err := o.d.IDs.Transact(ctx, scope.Tenant, func(ctx context.Context, tx identity.Tx) error {
		_, err := tx.Project(ctx, scope.Project)
		return err
	})
	if err != nil {
		if errors.Is(err, identity.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if scope.Run == "" {
		return true, nil
	}
	if _, err := o.d.Exec.Get(ctx, domain.Scope{TenantID: scope.Tenant, ProjectID: scope.Project}, scope.Run); err != nil {
		if errors.Is(err, execution.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (o devObservation) Resources(ctx context.Context, scope observation.Scope, max int) ([]observation.Resource, error) {
	ds := domain.Scope{TenantID: scope.Tenant, ProjectID: scope.Project}
	var out []observation.Resource
	for _, f := range scope.Families() {
		switch f {
		case observation.FamilyWork:
			items, err := o.d.Work.List(ctx, ds, work.ListQuery{Limit: max + 1})
			if err != nil {
				return nil, err
			}
			for _, it := range items {
				out = append(out, observation.Resource{Type: f, ID: string(it.ID), Version: int64(it.Version), State: string(it.State), Kind: string(it.Kind)})
			}
		case observation.FamilyRun, observation.FamilyStepAttempt:
			runs, err := o.d.Exec.List(ctx, ds, execution.ListQuery{Limit: max + 1})
			if err != nil {
				return nil, err
			}
			for _, r := range runs {
				if scope.Run != "" && r.ID != scope.Run {
					continue
				}
				if f == observation.FamilyRun {
					out = append(out, observation.Resource{Type: f, ID: string(r.ID), Version: int64(r.Version), State: string(r.Status)})
					continue
				}
				agg, err := o.d.Exec.Get(ctx, ds, r.ID)
				if err != nil {
					return nil, err
				}
				for _, at := range agg.Attempts {
					out = append(out, observation.Resource{Type: f, ID: string(at.ID), Version: int64(at.Version), State: string(at.Status), RunID: string(r.ID), StepID: at.StepID})
				}
			}
		}
	}
	return out, nil
}

// devJobs is the fixture Job observer, flagged for the construction guard.
type devJobs struct{ *rtest.Jobs }

func (devJobs) DevelopmentOnly() bool { return true }
