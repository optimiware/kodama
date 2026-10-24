// Package deliverytest is the shared fixture of the delivery module's tests and
// the published component evidence for its downstream consumers: real
// PostgreSQL carrying the execution, secrets, artifact, runtime-grant and
// delivery schemas, with the module's real adapters joining them. Only the
// external seams are controlled doubles: the Kubernetes Job observation, the
// OCI registry (a bounded HTTP fixture served by httptest) and the Git sources.
//
// It is component evidence, not production wiring: nothing here activates
// delivery in the composition root (T-072) or qualifies the real run/secret-pin
// transaction (T-077) or the end-to-end path (T-067).
//
// Resource prefix t035: tenants, projects and identifiers carry it.
package deliverytest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	artifactspg "github.com/optimiware/kodama/internal/artifacts/postgres"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/delivery/adapters"
	deliverypg "github.com/optimiware/kodama/internal/delivery/postgres"
	"github.com/optimiware/kodama/internal/domain"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	httppg "github.com/optimiware/kodama/internal/httpapi/postgres"
	"github.com/optimiware/kodama/internal/identity"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	pipelinepg "github.com/optimiware/kodama/internal/pipeline/postgres"
	rpg "github.com/optimiware/kodama/internal/results/postgres"
	"github.com/optimiware/kodama/internal/runtimeauth"
	rtjwt "github.com/optimiware/kodama/internal/runtimeauth/jwt"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
	"github.com/optimiware/kodama/internal/secrets"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
	"github.com/optimiware/kodama/internal/storage"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
	workspacepg "github.com/optimiware/kodama/internal/workspace/postgres"
)

// Resource prefix and fixed scopes of the task's tests.
const (
	Prefix    = "t035"
	Namespace = "t035-runs"
	Issuer    = "https://runtime.t035.invalid"
	// TargetNamespace is the application namespace of the fixture environments.
	TargetNamespace = "t035-preview"
)

var (
	ScopeA = domain.Scope{TenantID: "t035_acme", ProjectID: "p035_inventory"}
	// ScopeA2 is a second project of ScopeA's tenant: same-tenant project
	// scoping and composite ownership are tested against it.
	ScopeA2 = domain.Scope{TenantID: "t035_acme", ProjectID: "p035_billing"}
	ScopeB  = domain.Scope{TenantID: "t035_globex", ProjectID: "p035_ops"}
)

// Users. Engineers hold deployment:manage; viewers only read.
const (
	Engineer = executiontest.Engineer
	Viewer   = executiontest.Viewer
)

// Options select fault injection and the optional dependencies a test needs.
type Options struct {
	// WrapAppender inserts fault injection before delivery events are appended.
	WrapAppender func(deliverypg.EventAppender) deliverypg.EventAppender
	// WrapDB inserts fault injection into the delivery store's transactions (a
	// lost commit acknowledgment).
	WrapDB func(storage.TxBeginner) storage.TxBeginner
	// WrapArtifacts inserts fault injection around the artifact owner port.
	WrapArtifacts func(delivery.Artifacts) delivery.Artifacts
	// WrapAuthority inserts hooks around the runtime authority (a cancellation
	// committing between authentication and the guarded mutation).
	WrapAuthority func(delivery.Authority) delivery.Authority
	// WrapCredentials inserts a spy around the secrets owner port (a protected
	// callback must never resolve current credentials).
	WrapCredentials func(delivery.Credentials) delivery.Credentials
	// WithoutDeploymentCall registers attempts' grants without deployment.call.
	WithoutDeploymentCall bool
	// DefaultPreviewTTL overrides the installation default.
	DefaultPreviewTTL time.Duration
}

// Contributions is the ordered migration manifest the component tests apply: the
// foundation, event, idempotency, identity, work, secrets, pipeline, workspace
// and artifact contributions, the execution tables, the runtime grants and then
// delivery, which references the attempts and artifacts the earlier ones create.
// It is the order the composition root must register.
func Contributions() []storage.Contribution {
	contribs := []storage.Contribution{
		storage.BaseContribution(), eventspg.Migrations(), httppg.Migrations(), identitypg.Migrations(), workpg.Migrations(),
		secretspg.Migrations(), pipelinepg.Migrations(), workspacepg.Migrations(), artifactspg.Migrations(),
	}
	contribs = append(contribs, execpg.Migrations()...)
	return append(contribs, rtpg.Migrations(), deliverypg.Migrations())
}

// Manifest builds the ordered manifest of Contributions.
func Manifest() (storage.Manifest, error) { return storage.NewManifest(Contributions()...) }

// Env is one fully wired delivery module over real PostgreSQL.
type Env struct {
	T testing.TB
	X *executiontest.Env

	Ring     *rtjwt.KeyRing
	Verifier *rtjwt.Verifier
	Jobs     *rtest.Jobs
	Auth     *runtimeauth.Service
	Grants   *rtpg.Store
	Attempts *rpg.AttemptAuthority
	Guard    *deliverypg.Guard

	Store     *deliverypg.Store
	Svc       *delivery.Service
	Execution *adapters.Execution
	Authority *adapters.Authority
	Bindings  *Bindings
	Pins      *Pins

	ArtStore *artifactspg.Store
	Registry *Registry

	opts Options
	seq  atomic.Int64
}

// ID returns a fresh valid identifier carrying the task prefix.
func (e *Env) ID(prefix string) string { return fmt.Sprintf("%s_t035_%d", prefix, e.seq.Add(1)) }

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// New builds the environment.
func New(t testing.TB, o Options) *Env {
	t.Helper()
	x := executiontest.New(t, executiontest.Options{})
	e := &Env{T: t, X: x, Bindings: NewBindings(), Pins: &Pins{edits: map[domain.AttemptID]func(runtimev1.Context) (runtimev1.Context, error){}}}
	ctx := context.Background()
	x.Base.SeedTenant(t, string(ScopeA.TenantID), string(ScopeA.ProjectID), string(ScopeA2.ProjectID))
	x.Base.SeedTenant(t, string(ScopeB.TenantID), string(ScopeB.ProjectID))
	for _, s := range []domain.Scope{ScopeA, ScopeA2, ScopeB} {
		x.Grant(s, Engineer, identity.RoleEngineer)
		x.Grant(s, Viewer, identity.RoleViewer)
	}
	lim := executiontest.DefaultLimit()
	lim.TenantConcurrency, lim.ProjectConcurrency = 200, 100
	x.Limits.Set(lim)

	// The execution fixture migrated everything the delivery schema depends on
	// but the runtime grants and delivery itself: the same manifest plus those
	// contributions is a pure append.
	manifest, err := Manifest()
	must(t, err)
	mctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	_, err = storage.Migrate(mctx, x.Migration, manifest, storage.MigrateOptions{RuntimeRole: x.Base.DB.RuntimeRole})
	must(t, err)

	// ---- runtime authority over execution's rows, granting deployment.call ----
	e.Attempts, err = rpg.NewAttemptAuthority(x.Store, func(domain.Scope) (string, error) { return Namespace, nil }, deploymentActions)
	must(t, err)
	e.Grants, err = rtpg.NewStore(x.Runtime, e.Attempts)
	must(t, err)
	key, err := rtjwt.GenerateKey()
	must(t, err)
	e.Ring, err = rtjwt.NewKeyRing(1, key)
	must(t, err)
	signer, err := rtjwt.NewSigner(e.Ring, Issuer)
	must(t, err)
	pol := runtimeauth.Policy{AdmissionTTL: 10 * time.Minute, TokenTTL: 5 * time.Minute, MaxSession: 2 * time.Hour}
	e.Verifier, err = rtjwt.NewVerifier(rtjwt.VerifierConfig{Ring: e.Ring, Issuer: Issuer, MaxLifetime: pol.MaxSession, Clock: x.Clock.Now})
	must(t, err)
	e.Jobs = &rtest.Jobs{}
	e.Auth, err = runtimeauth.NewService(runtimeauth.Config{
		Store: e.Grants, Attempts: e.Attempts, Jobs: e.Jobs, Tokens: signer, Verifier: e.Verifier,
		Wrapper: runtimeauth.ExactWrapperPolicy{Supported: []string{"1.4.0"}}, Policy: pol, Clock: x.Clock.Now,
	})
	must(t, err)
	e.Authority, err = adapters.NewAuthority(e.Verifier, e.Ring, e.Grants, x.Clock.Now)
	must(t, err)
	e.Guard, err = deliverypg.NewGuard(e.Attempts, e.Ring.Generation, x.Clock.Now)
	must(t, err)

	e.opts = o
	e.wire()

	// ---- execution wired to the real delivery provider and cleanup boundary ----
	resolver, err := adapters.NewEnvironments(e.Svc)
	must(t, err)
	cleanup, err := adapters.NewCleanup(e.Svc)
	must(t, err)
	x.Svc = x.NewService(executiontest.Options{Environments: resolver, Cleanup: cleanup})

	e.Registry = NewRegistry(t)

	// Definitions may declare only environments that really exist in the project.
	v, err := pipeline.NewValidator(pipeline.Config{
		Capabilities: pipeline.EnabledKinds{pipeline.KindContainer},
		Profiles:     pipelinetest.Profiles{"coding-v1": true},
		References:   references{e},
	})
	must(t, err)
	x.Validator = v
	return e
}

// references is the pipeline module's reference policy over the real delivery
// store: an environment reference is admissible only if it exists in the scope.
type references struct{ e *Env }

func (r references) CheckSecret(context.Context, domain.Scope, pipeline.Actor, string, pipeline.SecretUse) error {
	return nil
}

func (r references) CheckEnvironment(ctx context.Context, scope domain.Scope, _ pipeline.Actor, id domain.EnvironmentID) error {
	if _, err := r.e.Store.GetEnvironment(ctx, scope, id); err != nil {
		return pipeline.ErrReferenceNotFound
	}
	return nil
}

// wire builds the delivery store and service over the shared database. A second
// call is a restart: fresh instances, no in-memory state, the same rows.
func (e *Env) wire() {
	x := e.X
	var err error
	var db storage.TxBeginner = x.Runtime
	if e.opts.WrapDB != nil {
		db = e.opts.WrapDB(db)
	}
	var appender deliverypg.EventAppender = deliverypg.EventsFrom(x.Events)
	if e.opts.WrapAppender != nil {
		appender = e.opts.WrapAppender(appender)
	}
	e.Store, err = deliverypg.NewStore(deliverypg.Config{DB: db, Events: appender, Guard: e.Guard, Clock: x.Clock.Now})
	must(e.T, err)

	reader, err := identitypg.NewStore(x.Runtime, x.Events)
	must(e.T, err)
	std, err := identity.NewStandaloneAuthority(reader)
	must(e.T, err)
	access, err := adapters.NewAccess(std)
	must(e.T, err)
	var creds delivery.Credentials
	creds, err = adapters.NewCredentials(x.Secrets)
	must(e.T, err)
	if e.opts.WrapCredentials != nil {
		creds = e.opts.WrapCredentials(creds)
	}
	e.Execution, err = adapters.NewExecution(x.Store)
	must(e.T, err)
	e.Pins.inner = e.Execution
	e.ArtStore, err = artifactspg.NewStore(artifactspg.Config{DB: x.Runtime})
	must(e.T, err)
	var arts delivery.Artifacts
	arts, err = adapters.NewArtifacts(&artifactShim{store: e.ArtStore, clock: x.Clock.Now})
	must(e.T, err)
	if e.opts.WrapArtifacts != nil {
		arts = e.opts.WrapArtifacts(arts)
	}
	var authority delivery.Authority = e.Authority
	if e.opts.WrapAuthority != nil {
		authority = e.opts.WrapAuthority(authority)
	}
	e.Svc, err = delivery.NewService(delivery.Config{
		Store: e.Store, Access: access, Credentials: creds, Contexts: e.Pins, Producers: e.Execution, Artifacts: arts,
		Authority: authority, Cleanups: e.Bindings, Clock: x.Clock.Now, DefaultPreviewTTL: e.opts.DefaultPreviewTTL,
	})
	must(e.T, err)
}

// Restart replaces the delivery store and service with fresh instances over the
// same database, as a process restart does, and returns the new service.
func (e *Env) Restart() *delivery.Service {
	e.T.Helper()
	e.wire()
	return e.Svc
}

// deploymentActions is the action policy the composition root must use for
// deployment steps: the entrypoint's actions plus deployment.call.
func deploymentActions(access execution.Access) (runtimeauth.ActionSet, error) {
	base, err := rpg.DefaultActions(access)
	if err != nil {
		return runtimeauth.ActionSet{}, err
	}
	return DeploymentActions(base)
}

// DeploymentActions adds deployment.call to an action set.
func DeploymentActions(base runtimeauth.ActionSet) (runtimeauth.ActionSet, error) {
	names := append(base.Strings(), string(runtimeauth.ActionDeploymentCall))
	return runtimeauth.ParseActionSet(names)
}

// Caller is the human principal of the fixtures.
func Caller(user string) delivery.Caller {
	return delivery.Caller{Actor: delivery.Actor{Type: delivery.ActorUser, ID: user}, RequestID: "req_t035"}
}

// ---- secrets ---------------------------------------------------------------

// Kubeconfig creates a kubeconfig-kind secret with a first value.
func (e *Env) Kubeconfig(scope domain.Scope, name, value string) secrets.Secret {
	return e.secret(scope, name, secrets.KindKubeconfig, value)
}

// Generic creates a generic secret with a first value.
func (e *Env) Generic(scope domain.Scope, name, value string) secrets.Secret {
	return e.secret(scope, name, secrets.KindGeneric, value)
}

func (e *Env) secret(scope domain.Scope, name string, kind secrets.SecretKind, value string) secrets.Secret {
	e.T.Helper()
	ctx := context.Background()
	m := secrets.Mutation{Actor: secrets.Actor{Type: secrets.ActorUser, ID: Engineer}, RequestID: "req_t035", Now: e.X.Clock.Now()}
	p, err := e.X.SecretSvc.PrepareSecret(ctx, m, scope, name, kind, []byte(value))
	must(e.T, err)
	var out secrets.Secret
	must(e.T, e.X.SecretSvc.Atomically(ctx, scope.TenantID, func(ctx context.Context, w secrets.Writer) error {
		var err error
		out, err = e.X.SecretSvc.CreateSecret(ctx, w, m, p)
		return err
	}))
	return out
}

// ---- environments ----------------------------------------------------------

// Environment creates a preview environment bound to the named kubeconfig secret.
func (e *Env) Environment(scope domain.Scope, name, kubeconfigSecret string, extra ...delivery.Credential) delivery.Environment {
	e.T.Helper()
	env, err := e.Svc.CreateEnvironment(context.Background(), Caller(Engineer), scope, delivery.EnvironmentInput{
		Name: name, Kind: delivery.KindPreview, Target: "kind", Namespace: TargetNamespace,
		Kubeconfig: delivery.Credential{Name: delivery.DefaultKubeconfig, Ref: secrets.FormatSecretRef(kubeconfigSecret), Delivery: runtimev1.DeliveryFile},
		Secrets:    extra, Config: map[string]string{"REPLICAS": "1"},
	})
	must(e.T, err)
	return env
}

// Stage creates a stage environment (no preview TTL, never expired by a
// schedule) bound to the named kubeconfig secret, in its own namespace.
func (e *Env) Stage(scope domain.Scope, name, namespace, kubeconfigSecret string, extra ...delivery.Credential) delivery.Environment {
	e.T.Helper()
	env, err := e.Svc.CreateEnvironment(context.Background(), Caller(Engineer), scope, delivery.EnvironmentInput{
		Name: name, Kind: delivery.KindStage, Target: "kind", Namespace: namespace,
		Kubeconfig: delivery.Credential{Name: delivery.DefaultKubeconfig, Ref: secrets.FormatSecretRef(kubeconfigSecret), Delivery: runtimev1.DeliveryFile},
		Secrets:    extra, Config: map[string]string{"REPLICAS": "3"},
	})
	must(e.T, err)
	return env
}

// ---- the delivery flow -----------------------------------------------------

// FlowOptions vary the fixture flow.
type FlowOptions struct {
	// Provenance overrides the source provenance the registry fixture reports
	// for the image (a wrong repository or commit).
	Provenance *artifacts.Provenance
	// BuildFails makes the producing attempt fail instead of succeeding with the
	// image output.
	BuildFails bool
	// OutputOther makes the build's accepted result name a second image, so the
	// first (Image) is registered but not an accepted output.
	OutputOther bool
}

// Deploy is one admitted two-step run: a succeeded build that produced an OCI
// image and a started deploy attempt with runtime authority.
type Deploy struct {
	Scope       domain.Scope
	Env         delivery.Environment
	Project     executiontest.Project
	BuildRun    *execution.Run
	Run         *execution.Run
	Build       *Attempt
	Deploy      *Attempt
	Image       domain.ArtifactID
	ImageDigest runtimev1.Digest
	// Other is the second image of an OutputOther build.
	Other       domain.ArtifactID
	OtherDigest runtimev1.Digest
	Source      delivery.Source
}

// Attempt is one started attempt with runtime authority.
type Attempt struct {
	Intent  *execution.AttemptSpec
	Context runtimev1.Context
	Token   string
	Session runtimeauth.SessionID
	Scope   domain.Scope
}

// ID is the attempt identifier.
func (a *Attempt) ID() domain.AttemptID { return a.Intent.AttemptID }

// buildStep builds the image; deployStep consumes it as a pinned run input and
// declares the environment. A build and its promotion are separate runs, so the
// deploying attempt is never the producing attempt. The image reaches the
// deploy step as a pinned input value (the artifact ID): that is the context
// boundary delivery checks, whatever admission-time typing produced it.
func buildStep() string {
	return "{id: build, kind: container, workspace: {access: read, from: run}, image: " + pipelinetest.Image +
		", command: [/bin/sh, -ec], timeoutSeconds: 60, outputs: {image: {type: ociArtifact}}}"
}

func deployStep(env domain.EnvironmentID) string {
	return "{id: deploy, kind: container, workspace: {access: read, from: run}, environment: " + string(env) +
		", image: " + pipelinetest.Image + ", command: [/bin/sh, -ec], args: [\"${{ inputs.image }}\"], timeoutSeconds: 60, retry: {maxAttempts: 3, backoffSeconds: 1, on: [infrastructure]}}"
}

// NewDeploy admits a build run and completes it with a registered,
// registry-verified image, then admits the deploy run over the environment with
// that image as its pinned input and starts its attempt with a runtime token,
// as the executor and the entrypoint would.
func (e *Env) NewDeploy(scope domain.Scope, env delivery.Environment, o FlowOptions) *Deploy {
	e.T.Helper()
	ctx := context.Background()
	p := e.X.NewProject(scope)
	admit := func(def string, inputs map[string]runtimev1.Value) *execution.Run {
		d := e.X.Define(scope, def)
		run, err := e.X.Svc.Admit(ctx, execution.Caller{PrincipalID: Engineer, RequestID: "req_t035"}, execution.AdmitCommand{
			Scope: scope, WorkItemID: p.Item, DefinitionID: d.ID, ContentRevision: e.X.ContentRevision(p), SourceCommit: executiontest.CommitA, Inputs: inputs,
		})
		must(e.T, err)
		return run
	}
	buildRun := admit(pipelinetest.Doc(buildStep()), nil)
	d := &Deploy{Scope: scope, Env: env, Project: p, BuildRun: buildRun}
	d.Build = e.start(scope, buildRun, true)
	src := d.Build.Context.Workspace.Source
	if src == nil {
		e.T.Fatal("the build has no source")
	}
	d.Source = delivery.Source{RepositoryURL: src.RepositoryURL, Commit: string(src.Commit)}

	prov := artifacts.Provenance{RepositoryURL: src.RepositoryURL, Commit: string(src.Commit)}
	if o.Provenance != nil {
		prov = *o.Provenance
	}
	d.Image, d.ImageDigest = e.RegisterImage(d.Build, "t035/app", prov)
	var err error
	switch {
	case o.BuildFails:
		_, err = e.X.Fail(d.Build.Intent)
	case o.OutputOther:
		d.Other, d.OtherDigest = e.RegisterImage(d.Build, "t035/other-output", prov)
		_, err = e.CommitBuild(d.Build, d.Other)
	default:
		_, err = e.CommitBuild(d.Build, d.Image)
	}
	must(e.T, err)
	d.Run = admit(pipelinetest.DocNamed("promote", "{image: {type: string, required: true}}", deployStep(env.ID())),
		map[string]runtimev1.Value{"image": runtimev1.StringValue(string(d.Image))})
	d.Deploy = e.start(scope, d.Run, false)
	return d
}

// Promote admits another deploy run, of another work item, that deploys the same
// verified image to env: the promotion of one digest, whose deploying attempt is
// never the producing attempt.
func (e *Env) Promote(d *Deploy, env delivery.Environment) *Deploy {
	return e.PromoteImage(d, env, d.Image)
}

// PromoteImage is Promote with any artifact ID as the pinned image input.
func (e *Env) PromoteImage(d *Deploy, env delivery.Environment, image domain.ArtifactID) *Deploy {
	e.T.Helper()
	p := e.X.NewProject(d.Scope)
	def := e.X.Define(d.Scope, pipelinetest.DocNamed("promote", "{image: {type: string, required: true}}", deployStep(env.ID())))
	run, err := e.X.Svc.Admit(context.Background(), execution.Caller{PrincipalID: Engineer, RequestID: "req_t035"}, execution.AdmitCommand{
		Scope: d.Scope, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: e.X.ContentRevision(p), SourceCommit: executiontest.CommitA,
		Inputs: map[string]runtimev1.Value{"image": runtimev1.StringValue(string(image))},
	})
	must(e.T, err)
	n := *d
	n.Env, n.Project, n.Run = env, p, run
	n.Deploy = e.start(d.Scope, run, false)
	return &n
}

// Retry fails the deploy attempt as the executor does for an infrastructure
// failure that provably never launched the command, and starts the step's next
// attempt: a new attempt ID and token for the same logical action key.
func (e *Env) Retry(d *Deploy) *Attempt {
	e.T.Helper()
	must(e.T, e.X.Observe(d.Deploy.Intent, execution.Observation{Kind: execution.ObservedFailed, Class: execution.ClassInfrastructure,
		Reason: execution.ReasonInfrastructure, Launch: execution.LaunchNotStarted}))
	must(e.T, e.X.Terminate(d.Deploy.Intent, execution.LaunchNotStarted))
	e.X.Clock.Advance(time.Minute)
	return e.start(d.Scope, d.Run, false)
}

// NewPlain starts an ordinary attempt that declares no environment.
func (e *Env) NewPlain(scope domain.Scope) *Attempt {
	e.T.Helper()
	p := e.X.NewProject(scope)
	def := e.X.Define(scope, pipelinetest.Doc(pipelinetest.Container("plain", "workspace: {access: read, from: run}")))
	run, err := e.X.Svc.Admit(context.Background(), execution.Caller{PrincipalID: Engineer, RequestID: "req_t035"}, execution.AdmitCommand{
		Scope: scope, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: e.X.ContentRevision(p), SourceCommit: executiontest.CommitA,
	})
	must(e.T, err)
	return e.start(scope, run, false)
}

// CommitBuild commits the build attempt's completed result naming the image.
func (e *Env) CommitBuild(a *Attempt, image domain.ArtifactID) (execution.ResultAck, error) {
	res := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: ptr(int64(0)), DiagnosticArtifactIDs: []domain.ArtifactID{},
		Outputs: map[string]runtimev1.Value{"image": runtimev1.StringValue(string(image))}}
	h, err := runtimev1.ResultHash(res)
	must(e.T, err)
	return e.X.Svc.CommitResult(context.Background(), execution.ResultCommit{
		Scope: a.Scope, AttemptID: a.ID(), Request: runtimev1.ResultRequest{ResultHash: h, Result: res},
		Artifacts: map[domain.ArtifactID]runtimev1.ArtifactRef{image: {ID: image, Scope: a.Scope, Attempt: a.ID(), Kind: runtimev1.KindOCI, Usable: true}},
	})
}

func ptr[T any](v T) *T { return &v }

// start claims the next runnable step, starts it and exchanges its admission
// credential for a runtime token.
//
// A deploy attempt is not marked as having launched its command: execution
// retries a step only when the command provably never started, and the retry
// tests need that. (A started command that fails is never retried, by design.)
func (e *Env) start(scope domain.Scope, run *execution.Run, launched bool) *Attempt {
	e.T.Helper()
	in := e.X.MustClaim(scope.TenantID)
	if launched {
		e.X.Start(in)
	}
	agg, err := e.X.Store.Get(context.Background(), scope, run.ID)
	must(e.T, err)
	at := agg.Attempt(in.AttemptID)
	pinned, err := runtimev1.DecodeRequest[runtimev1.Context](at.Context)
	must(e.T, err)
	a := &Attempt{Intent: in, Context: pinned, Scope: scope}
	e.admit(a)
	return a
}

func (e *Env) admit(a *Attempt) {
	e.T.Helper()
	ctx := context.Background()
	cred, err := runtimeauth.GenerateCredential(nil)
	must(e.T, err)
	actions, err := deploymentActions(a.Intent.Access)
	if e.opts.WithoutDeploymentCall {
		actions, err = rpg.DefaultActions(a.Intent.Access)
	}
	must(e.T, err)
	_, err = e.Auth.Register(ctx, runtimeauth.RegisterCommand{Scope: a.Scope, AttemptID: a.ID(), Credential: cred, Actions: actions, Context: a.Context})
	must(e.T, err)
	sum := sha256.Sum256([]byte(a.ID()))
	h := hex.EncodeToString(sum[:])
	uid := func(off int) string {
		return h[off:off+8] + "-" + h[off+8:off+12] + "-4" + h[off+13:off+16] + "-8" + h[off+17:off+20] + "-" + h[off+20:off+32]
	}
	name := a.Intent.WorkloadName
	id := runtimev1.JobIdentity{Name: name, UID: uid(0), PodName: name + "-x7k2p", PodUID: uid(16)}
	e.Jobs.SetCluster(runtimeauth.JobEvidence{Namespace: Namespace, JobName: id.Name, JobUID: id.UID, PodNamespace: Namespace, PodName: id.PodName, PodUID: id.PodUID, PodOwnerUID: id.UID})
	resp, err := e.Auth.Admit(ctx, a.ID(), runtimev1.AdmitRequest{AdmissionCredential: cred.Reveal(), Nonce: "nnnnnnnnnnnnnnnnnnnnnn", ClientVersion: "1.4.0", Job: id})
	must(e.T, err)
	a.Token = resp.Token.Token
	claims, err := e.Verifier.Verify(ctx, a.Token)
	must(e.T, err)
	a.Session = claims.Session
}

// Revoke ends the attempt's authority as a run cancellation does.
func (e *Env) Revoke(a *Attempt) {
	e.T.Helper()
	// GREATEST: the database clock may trail the test process's by milliseconds,
	// and a revocation never precedes its row's creation (a table constraint).
	e.X.OwnerExec(`UPDATE attempt_controls SET revoked_at = now() WHERE tenant_id = $1 AND project_id = $2 AND attempt_id = $3`,
		string(a.Scope.TenantID), string(a.Scope.ProjectID), string(a.ID()))
	e.X.OwnerExec(`UPDATE runtime_grants SET revoked_at = GREATEST(now(), created_at) WHERE tenant_id = $1 AND attempt_id = $2`, string(a.Scope.TenantID), string(a.ID()))
}

// ---- the OCI registry fixture ----------------------------------------------

// Registry is a bounded HTTP OCI registry serving digest-addressed manifests.
type Registry struct {
	t   testing.TB
	srv *httptest.Server
	mu  sync.Mutex
	// manifests maps repository -> digest -> body.
	manifests map[string]map[artifacts.Digest][]byte
	prov      map[string]artifacts.Provenance
}

// NewRegistry starts the fixture; it stops with the test.
func NewRegistry(t testing.TB) *Registry {
	r := &Registry{t: t, manifests: map[string]map[artifacts.Digest][]byte{}, prov: map[string]artifacts.Provenance{}}
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

// Host is host:port of the registry.
func (r *Registry) Host() string { return strings.TrimPrefix(r.srv.URL, "http://") }

func (r *Registry) serve(w http.ResponseWriter, req *http.Request) {
	parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/v2/"), "/manifests/")
	if len(parts) != 2 {
		http.NotFound(w, req)
		return
	}
	r.mu.Lock()
	body, ok := r.manifests[parts[0]][artifacts.Digest(parts[1])]
	r.mu.Unlock()
	if !ok {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	w.Header().Set("Docker-Content-Digest", parts[1])
	_, _ = w.Write(body)
}

// Add publishes a minimal image manifest under the repository and returns its
// digest.
func (r *Registry) Add(repository string, nonce string) artifacts.Digest {
	cfg := artifacts.DigestOf([]byte("config-" + nonce))
	body, err := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": string(cfg), "size": 12},
		"layers": []any{},
	})
	must(r.t, err)
	d := artifacts.DigestOf(body)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.manifests[repository] == nil {
		r.manifests[repository] = map[artifacts.Digest][]byte{}
	}
	r.manifests[repository][d] = body
	return d
}

type policy struct{ r *Registry }

func (p policy) Authorize(_ context.Context, _ domain.Scope, _ domain.AttemptID, ref artifacts.RegistryRef) (ociEndpoint, error) {
	return ociEndpoint{BaseURL: p.r.srv.URL, Insecure: true}, nil
}

// ---- artifact owner shim ---------------------------------------------------

// artifactShim adapts the artifact repository to the lookup surface
// *artifacts.Service provides (the service wraps the same repository calls).
type artifactShim struct {
	store *artifactspg.Store
	clock func() time.Time
}

func (s *artifactShim) Reference(ctx context.Context, scope domain.Scope, id domain.ArtifactID) (artifacts.Reference, error) {
	a, err := s.store.Get(ctx, scope, id)
	if err != nil {
		return artifacts.Reference{}, err
	}
	return a.Reference(), nil
}

func (s *artifactShim) Pin(ctx context.Context, scope domain.Scope, id domain.ArtifactID, owner artifacts.PinOwner) error {
	return s.store.Pin(ctx, scope, id, owner, s.clock())
}

func (s *artifactShim) Unpin(ctx context.Context, scope domain.Scope, id domain.ArtifactID, owner artifacts.PinOwner) error {
	return s.store.Unpin(ctx, scope, id, owner)
}

// ---- cleanup binding -------------------------------------------------------

// Bindings is the injected cleanup-binding reader: which deployment a cleanup
// run was admitted for, per scope. A run identifier alone proves nothing across
// tenants and projects, so a binding recorded for one scope is invisible to
// another even for the same run ID. T-066/T-077 supply the production reader.
type Bindings struct {
	mu sync.Mutex
	m  map[bindingKey]delivery.DeploymentID
}

type bindingKey struct {
	scope domain.Scope
	run   domain.RunID
}

// NewBindings returns an empty reader: every cleanup callback is denied until a
// binding is recorded.
func NewBindings() *Bindings { return &Bindings{m: map[bindingKey]delivery.DeploymentID{}} }

// Bind records the deployment a cleanup run of the scope is bound to.
func (b *Bindings) Bind(scope domain.Scope, run domain.RunID, id delivery.DeploymentID) {
	b.mu.Lock()
	b.m[bindingKey{scope, run}] = id
	b.mu.Unlock()
}

// Binding implements delivery.CleanupBindings.
func (b *Bindings) Binding(_ context.Context, scope domain.Scope, run domain.RunID) (delivery.DeploymentID, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id, ok := b.m[bindingKey{scope, run}]
	return id, ok, nil
}

// ---- immutable pinned-context fixture ----------------------------------------

// Pins decorates the real pinned-context reader (the execution adapter) with
// explicit immutable-pin edits for negative cases: an absent pin, a foreign one,
// a mismatched environment or credential binding. Without an edit it returns
// exactly what execution persisted. It is the injection point the task allows
// ("component tests may inject immutable pins at the reader boundary"); the
// record, provenance and artifact adapters beneath stay real.
type Pins struct {
	mu    sync.Mutex
	inner delivery.PinnedContexts
	edits map[domain.AttemptID]func(runtimev1.Context) (runtimev1.Context, error)
}

// Edit replaces what Pinned returns for the attempt: fn receives the persisted
// context and returns the one to serve, or an error (for example
// delivery.ErrNotFound for an absent pin).
func (p *Pins) Edit(attempt domain.AttemptID, fn func(runtimev1.Context) (runtimev1.Context, error)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.edits[attempt] = fn
}

// Absent makes the attempt's pinned context unreadable (not found).
func (p *Pins) Absent(attempt domain.AttemptID) {
	p.Edit(attempt, func(runtimev1.Context) (runtimev1.Context, error) {
		return runtimev1.Context{}, delivery.Fail(delivery.ErrNotFound, nil, "not found")
	})
}

// Clear restores every attempt's persisted context.
func (p *Pins) Clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.edits = map[domain.AttemptID]func(runtimev1.Context) (runtimev1.Context, error){}
}

// Pinned implements delivery.PinnedContexts.
func (p *Pins) Pinned(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (runtimev1.Context, error) {
	c, err := p.inner.Pinned(ctx, scope, attempt)
	if err != nil {
		return runtimev1.Context{}, err
	}
	p.mu.Lock()
	fn := p.edits[attempt]
	p.mu.Unlock()
	if fn == nil {
		return c, nil
	}
	return fn(c)
}

// ---- lost commit acknowledgment --------------------------------------------

// LossyDB loses the acknowledgment of commits while armed: mode 1 applies the
// commit and reports an error (an unknown outcome), mode 2 fails the commit.
type LossyDB struct {
	Inner storage.TxBeginner
	mode  atomic.Int32
}

// Lose applies the commit and loses its acknowledgment.
func (d *LossyDB) Lose() { d.mode.Store(1) }

// Fail rolls the commit back and reports an error.
func (d *LossyDB) Fail() { d.mode.Store(2) }

// Off restores normal commits.
func (d *LossyDB) Off() { d.mode.Store(0) }

// BeginTx implements storage.TxBeginner.
func (d *LossyDB) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	tx, err := d.Inner.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	if opts.AccessMode == pgx.ReadOnly {
		return tx, nil // only mutations lose their acknowledgment
	}
	return &lossyTx{Tx: tx, db: d}, nil
}

type lossyTx struct {
	pgx.Tx
	db *LossyDB
}

func (t *lossyTx) Commit(ctx context.Context) error {
	switch t.db.mode.Load() {
	case 1:
		if err := t.Tx.Commit(ctx); err != nil {
			return err
		}
		return fmt.Errorf("connection reset by peer while waiting for the commit acknowledgment")
	case 2:
		_ = t.Tx.Rollback(ctx)
		return fmt.Errorf("server closed the connection unexpectedly during commit")
	}
	return t.Tx.Commit(ctx)
}

// Provenance returns the trusted source provenance of the deploy's candidate.
func (d *Deploy) Provenance() artifacts.Provenance {
	return artifacts.Provenance{RepositoryURL: d.Source.RepositoryURL, Commit: d.Source.Commit}
}

// ArtifactLookup returns the artifact owner's lookup and pin surface over the
// real artifact repository.
func (e *Env) ArtifactLookup() adapters.ArtifactLookup {
	return &artifactShim{store: e.ArtStore, clock: e.X.Clock.Now}
}
