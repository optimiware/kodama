// Package qatest is the shared fixture of the QA mapping's tests and the
// published component evidence for T-069 and T-046: real PostgreSQL carrying
// the execution, secrets, artifact, runtime-grant, delivery and work schemas;
// a real S3 prefix holding the artifact bytes; and the module's real adapters
// joining them. The only controlled doubles are fault injection at owner
// boundaries (a lost commit acknowledgment, a failing event append) and the
// model/browser, which is replaced by the test writing the report and the
// screenshots a QA attempt would upload.
//
// It is component evidence, not production wiring and not the real browser,
// image or profile qualification (T-069). Resource prefix t040.
package qatest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/artifacts/artifactstest"
	arts3 "github.com/optimiware/kodama/internal/artifacts/s3"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/delivery/deliverytest"
	"github.com/optimiware/kodama/internal/domain"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/identity"
	identityconsumer "github.com/optimiware/kodama/internal/identity/consumer"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	"github.com/optimiware/kodama/internal/qa"
	"github.com/optimiware/kodama/internal/qa/adapters"
	resultsadapters "github.com/optimiware/kodama/internal/results/adapters"
	rpg "github.com/optimiware/kodama/internal/results/postgres"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/work"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
	s3support "github.com/optimiware/kodama/test/support/s3"
)

// Resource prefix of this task's test fixtures.
const Prefix = "t040"

// ReportOutput is the declared output name of the report in the fixture steps.
const ReportOutput = "qa-report"

// DeploymentInput is the pinned run input that carries the deployment the QA
// step is to inspect (the trusted binding; the report only repeats it).
const DeploymentInput = "deployment"

// Mapper is the identity-module principal the mapping runs as: a user-typed
// principal bound as analyst (read, create and edit-spec work, nothing else) in
// every fixture project. Revoking its binding (Env.RevokeMapper) is revoking
// the mapping authority; the QA work store further narrows it to read, create
// and evidence links.
const Mapper = "u_t040_qa_mapper"

var (
	ScopeA  = deliverytest.ScopeA
	ScopeA2 = deliverytest.ScopeA2
	ScopeB  = deliverytest.ScopeB
)

// Options select fault injection.
type Options struct {
	// WrapWorkDB inserts fault injection into the QA work store's transactions.
	WrapWorkDB func(storage.TxBeginner) storage.TxBeginner
	// WrapWorkAppender inserts fault injection before work events are appended.
	WrapWorkAppender func(workpg.EventAppender) workpg.EventAppender
}

// Env is one fully wired QA module over real services.
type Env struct {
	*deliverytest.Env

	S3      *s3support.Env
	Objects *arts3.Store
	ArtSvc  *artifacts.Service
	Http    *http.Client

	// WorkStore is the QA-restricted work store (create and evidence links only).
	WorkStore *workpg.Store
	// WorkAuthority is the identity-backed work authority the mapping runs under
	// (the real identity store and standalone authority over the fixture database).
	WorkAuthority *identityconsumer.WorkAuthority
	Attempts      *adapters.Attempts
	Deploys       *adapters.Deployments
	Evidence      *adapters.Artifacts
	Defects       *adapters.Work
	Service       *qa.Service

	seq atomic.Int64
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// New builds the environment. PostgreSQL and S3 are real; a missing service
// fails the test when KODAMA_REQUIRE_POSTGRES / KODAMA_REQUIRE_S3 are set, as
// the harness sets them.
func New(t testing.TB, o Options) *Env {
	t.Helper()
	d := deliverytest.New(t, deliverytest.Options{})
	e := &Env{Env: d, Http: &http.Client{Timeout: 30 * time.Second}}
	x := d.X

	e.S3 = s3support.New(t)
	var err error
	e.Objects, err = arts3.NewStore(arts3.Config{Client: e.S3.Client, Bucket: e.S3.Bucket(), Prefix: e.S3.Prefix, AllowInsecure: true, Clock: x.Clock.Now})
	must(t, err)
	artAuth, err := resultsadapters.NewArtifactAuthority(d.Grants, d.Ring, x.Clock.Now)
	must(t, err)
	e.ArtSvc, err = artifacts.NewService(artifacts.ServiceConfig{
		Repository: d.ArtStore, Authority: artAuth, Reader: artifactstest.NewReader(), Objects: e.Objects,
		Registry: artifactstest.NewRegistry(), Clock: x.Clock.Now, UploadTTL: 10 * time.Minute, DownloadTTL: 5 * time.Minute,
		ClaimLease: 30 * time.Second, SpoolDir: t.TempDir(),
	})
	must(t, err)

	var db storage.TxBeginner = x.Runtime
	if o.WrapWorkDB != nil {
		db = o.WrapWorkDB(db)
	}
	var appender workpg.EventAppender = x.Events
	if o.WrapWorkAppender != nil {
		appender = o.WrapWorkAppender(appender)
	}
	for _, sc := range []domain.Scope{ScopeA, ScopeA2, ScopeB} {
		x.Grant(sc, Mapper, identity.RoleAnalyst)
	}
	reader, err := identitypg.NewStore(x.Runtime, x.Events)
	must(t, err)
	std, err := identity.NewStandaloneAuthority(reader)
	must(t, err)
	e.WorkAuthority, err = identityconsumer.NewWorkAuthority(std, reader)
	must(t, err)
	e.WorkStore, err = workpg.NewStore(db, appender, adapters.WorkGuards(d.ArtStore, func(tx pgx.Tx) work.PermissionChecker {
		return identityconsumer.NewTxPermissions(tx)
	}))
	must(t, err)

	e.Attempts, err = adapters.NewAttempts(x.Store)
	must(t, err)
	e.Deploys, err = adapters.NewDeployments(d.Store)
	must(t, err)
	e.Evidence, err = adapters.NewArtifacts(d.ArtStore, e.Objects, x.Clock.Now)
	must(t, err)
	e.Defects, err = adapters.NewWork(e.WorkStore, e.WorkAuthority, Mapper, x.Clock.Now)
	must(t, err)
	e.Service, err = qa.NewService(qa.Config{Attempts: e.Attempts, Deployments: e.Deploys, Artifacts: e.Evidence, Defects: e.Defects, ReportOutput: ReportOutput, DeploymentInput: DeploymentInput})
	must(t, err)
	return e
}

// Name returns a fresh identifier suffix carrying the task prefix.
func (e *Env) Name(prefix string) string { return fmt.Sprintf("%s_t040_%d", prefix, e.seq.Add(1)) }

// ---- fault injection ---------------------------------------------------------

// FaultAppender fails event appends while armed, so a work mutation's
// transaction rolls back after its rows were written.
type FaultAppender struct {
	Inner workpg.EventAppender
	skip  atomic.Int64
	fail  atomic.Int64
}

// FailNext makes the next n appends fail.
func (f *FaultAppender) FailNext(n int64) { f.fail.Store(n) }

// Append implements workpg.EventAppender.
func (f *FaultAppender) Append(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) (eventspg.AppendResult, error) {
	if f.skip.Load() > 0 && f.skip.Add(-1) >= 0 {
		return f.Inner.Append(ctx, tx, env)
	}
	if f.fail.Load() > 0 && f.fail.Add(-1) >= 0 {
		return eventspg.AppendResult{}, fmt.Errorf("injected event append failure")
	}
	return f.Inner.Append(ctx, tx, env)
}

// ---- deployments ---------------------------------------------------------------

// Deployed is a succeeded deployment of a verified image to a preview
// environment, recorded through the real delivery service.
type Deployed struct {
	Scope      domain.Scope
	Env        delivery.Environment
	Flow       *deliverytest.Deploy
	Deployment delivery.Deployment
}

// ID is the deployment identifier.
func (d *Deployed) ID() string { return string(d.Deployment.ID()) }

// Facts returns the deployment's recorded facts as the QA report must state them.
func (d *Deployed) Facts() qa.DeploymentRef {
	rec := d.Deployment.Record()
	return qa.DeploymentRef{
		ID: string(rec.ID), EnvironmentID: rec.Environment, ConfigDigest: rec.ConfigDigest,
		CandidateCommit: runtimev1.GitCommit(rec.Source.Commit), ImageDigest: rec.Image.Digest,
	}
}

func objects(names ...string) []delivery.ObjectRef {
	out := make([]delivery.ObjectRef, 0, len(names))
	for _, n := range names {
		out = append(out, delivery.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Namespace: deliverytest.TargetNamespace, Name: n})
	}
	return out
}

func observed(objs []delivery.ObjectRef) []delivery.ObjectRef {
	out := append([]delivery.ObjectRef(nil), objs...)
	for i := range out {
		b := []byte("00000000-0000-4000-8000-000000000000")
		b[35] = "0123456789abcdef"[i%16]
		out[i].UID, out[i].Ready = string(b), true
	}
	return out
}

// Deploy runs the real delivery flow in scope (build, image registration,
// deploy attempt, pre-apply deployment row, apply, readiness) and returns the
// succeeded deployment. The environment is created in the same project.
func (e *Env) Deploy(scope domain.Scope, envName string) *Deployed {
	e.T.Helper()
	e.Kubeconfig(scope, "kubeconfig", "KUBE-SENTINEL")
	return e.DeployTo(scope, e.Environment(scope, envName, "kubeconfig"), deliverytest.FlowOptions{}, true)
}

// DeployTo runs the flow against an existing environment. With succeed false
// the deployment row is registered and applying but never settled.
func (e *Env) DeployTo(scope domain.Scope, env delivery.Environment, f deliverytest.FlowOptions, succeed bool) *Deployed {
	e.T.Helper()
	return e.finishDeploy(scope, env, e.NewDeploy(scope, env, f), succeed)
}

// DeployCandidate runs the same flow for a build of another source commit, so
// the environment holds a second succeeded deployment of the same
// configuration whose candidate differs.
func (e *Env) DeployCandidate(scope domain.Scope, env delivery.Environment, commit string) *Deployed {
	e.T.Helper()
	return e.finishDeploy(scope, env, e.newDeployAt(scope, env, commit), true)
}

func (e *Env) finishDeploy(scope domain.Scope, env delivery.Environment, d *deliverytest.Deploy, succeed bool) *Deployed {
	e.T.Helper()
	ctx := context.Background()
	rc, err := e.Svc.RegisterRuntime(ctx, d.Deploy.ID(), d.Deploy.Token, delivery.RegistrationRequest{
		Source: d.Source, Image: delivery.Image{ArtifactID: d.Image, Digest: d.ImageDigest},
	})
	must(e.T, err)
	id := rc.Deployment.ID()
	intended := objects("app")
	rc, err = e.Svc.ReportOutcome(ctx, d.Deploy.ID(), d.Deploy.Token, id, delivery.OutcomeReport{State: delivery.OutcomeApplying, Objects: intended})
	must(e.T, err)
	if succeed {
		rc, err = e.Svc.ReportOutcome(ctx, d.Deploy.ID(), d.Deploy.Token, id, delivery.OutcomeReport{State: delivery.OutcomeSucceeded, Objects: observed(intended)})
		must(e.T, err)
	}
	return &Deployed{Scope: scope, Env: env, Flow: d, Deployment: rc.Deployment}
}

func qaBuildStep() string {
	return "{id: build, kind: container, workspace: {access: read, from: run}, image: " + pipelinetest.Image +
		", command: [/bin/sh, -ec], timeoutSeconds: 60, outputs: {image: {type: ociArtifact}}}"
}

func qaDeployStep(env domain.EnvironmentID) string {
	return "{id: deploy, kind: container, workspace: {access: read, from: run}, environment: " + string(env) +
		", image: " + pipelinetest.Image + ", command: [/bin/sh, -ec], args: [\"${{ inputs.image }}\"], timeoutSeconds: 60, retry: {maxAttempts: 3, backoffSeconds: 1, on: [infrastructure]}}"
}

// newDeployAt is the delivery fixture's build-and-promote flow with the source
// commit of both runs chosen by the caller (the shared fixture builds only the
// default commit). The runs, attempts, image registration and runtime
// authority are the real execution, artifact and runtimeauth components.
func (e *Env) newDeployAt(scope domain.Scope, env delivery.Environment, commit string) *deliverytest.Deploy {
	e.T.Helper()
	ctx := context.Background()
	p := e.X.NewProject(scope)
	admit := func(def string, inputs map[string]runtimev1.Value) *execution.Run {
		d := e.X.Define(scope, def)
		run, err := e.X.Svc.Admit(ctx, execution.Caller{PrincipalID: deliverytest.Engineer, RequestID: "req_t040"}, execution.AdmitCommand{
			Scope: scope, WorkItemID: p.Item, DefinitionID: d.ID, ContentRevision: e.X.ContentRevision(p), SourceCommit: commit, Inputs: inputs,
		})
		must(e.T, err)
		return run
	}
	start := func(run *execution.Run, launched bool) *deliverytest.Attempt {
		in := e.X.MustClaim(scope.TenantID)
		if launched {
			e.X.Start(in)
		}
		agg, err := e.X.Store.Get(ctx, scope, run.ID)
		must(e.T, err)
		pinned, err := runtimev1.DecodeRequest[runtimev1.Context](agg.Attempt(in.AttemptID).Context)
		must(e.T, err)
		a := &deliverytest.Attempt{Intent: in, Context: pinned, Scope: scope}
		e.admitWith(a, func(base runtimeauth.ActionSet) (runtimeauth.ActionSet, error) {
			return deliverytest.DeploymentActions(base)
		})
		return a
	}
	buildRun := admit(pipelinetest.Doc(qaBuildStep()), nil)
	d := &deliverytest.Deploy{Scope: scope, Env: env, Project: p, BuildRun: buildRun}
	d.Build = start(buildRun, true)
	src := d.Build.Context.Workspace.Source
	if src == nil || string(src.Commit) != commit {
		e.T.Fatalf("the build is not pinned to commit %s: %+v", commit, src)
	}
	d.Source = delivery.Source{RepositoryURL: src.RepositoryURL, Commit: string(src.Commit)}
	d.Image, d.ImageDigest = e.RegisterImage(d.Build, "t035/app", artifacts.Provenance{RepositoryURL: src.RepositoryURL, Commit: string(src.Commit)})
	if _, err := e.CommitBuild(d.Build, d.Image); err != nil {
		e.T.Fatal(err)
	}
	d.Run = admit(pipelinetest.DocNamed("promote", "{image: {type: string, required: true}}", qaDeployStep(env.ID())),
		map[string]runtimev1.Value{"image": runtimev1.StringValue(string(d.Image))})
	d.Deploy = start(d.Run, false)
	return d
}

// FailedDeployment registers a deployment that failed.
func (e *Env) FailedDeployment(scope domain.Scope, env delivery.Environment) *Deployed {
	e.T.Helper()
	ctx := context.Background()
	d := e.NewDeploy(scope, env, deliverytest.FlowOptions{})
	rc, err := e.Svc.RegisterRuntime(ctx, d.Deploy.ID(), d.Deploy.Token, delivery.RegistrationRequest{
		Source: d.Source, Image: delivery.Image{ArtifactID: d.Image, Digest: d.ImageDigest},
	})
	must(e.T, err)
	id := rc.Deployment.ID()
	intended := objects("app")
	_, err = e.Svc.ReportOutcome(ctx, d.Deploy.ID(), d.Deploy.Token, id, delivery.OutcomeReport{State: delivery.OutcomeApplying, Objects: intended})
	must(e.T, err)
	rc, err = e.Svc.ReportOutcome(ctx, d.Deploy.ID(), d.Deploy.Token, id, delivery.OutcomeReport{State: delivery.OutcomeFailed, Reason: "rollout_failed", Objects: observed(intended)})
	must(e.T, err)
	return &Deployed{Scope: scope, Env: env, Flow: d, Deployment: rc.Deployment}
}

// ---- QA attempts ---------------------------------------------------------------

// QAOptions shape the QA step of a fixture run.
type QAOptions struct {
	// Screenshots is the number of declared screenshot outputs shot-1..n
	// (default 2).
	Screenshots int
	// TrustedDeployment overrides the deployment identifier pinned as the run's
	// DeploymentInput; by default it is the deployment passed to StartQA.
	TrustedDeployment string
}

// QARun is a started QA attempt pinned to an environment, with a work item
// under test and a runtime session, ready to upload its artifacts.
type QARun struct {
	e       *Env
	Scope   domain.Scope
	Env     delivery.Environment
	Project executiontest.Project
	Run     *execution.Run
	Attempt *deliverytest.Attempt
	Outputs map[string]domain.ArtifactID
	// Diagnostics are diagnostic artifacts to list in the accepted result.
	Diagnostics []domain.ArtifactID
	shots       int
}

// ID is the attempt identifier.
func (q *QARun) ID() domain.AttemptID { return q.Attempt.ID() }

// Subject is the work item under test.
func (q *QARun) Subject() domain.WorkItemID { return q.Project.Item }

// Pinned is the attempt's immutable context.
func (q *QARun) Pinned() runtimev1.Context { return q.Attempt.Context }

func qaStep(env domain.EnvironmentID, shots int) string {
	var outs []string
	outs = append(outs, ReportOutput+": {type: artifact, path: /kodama/outputs/qa-report.json}")
	for i := 1; i <= shots; i++ {
		outs = append(outs, fmt.Sprintf("shot-%d: {type: artifact, path: /kodama/outputs/shot-%d.png}", i, i))
	}
	return "{id: qa, kind: container, workspace: {access: read, from: run}, environment: " + string(env) +
		", image: " + pipelinetest.Image + ", command: [/bin/sh, -ec], timeoutSeconds: 60, outputs: {" + strings.Join(outs, ", ") + "}}"
}

// StartQA admits a QA run of a new work item under test in scope, declaring the
// deployment's environment (so its immutable pin is carried into the context),
// pinning the deployment identifier as the run's trusted DeploymentInput, and
// declaring the report and screenshot outputs; it starts the attempt with a
// runtime session.
func (e *Env) StartQA(scope domain.Scope, dep *Deployed, o QAOptions) *QARun {
	e.T.Helper()
	p := e.X.NewProject(scope)
	return e.StartQAFor(p, dep, o)
}

// StartQAFor starts a QA run for an existing work item under test.
func (e *Env) StartQAFor(p executiontest.Project, dep *Deployed, o QAOptions) *QARun {
	e.T.Helper()
	env := dep.Env
	trusted := o.TrustedDeployment
	if trusted == "" {
		trusted = dep.ID()
	}
	shots := o.Screenshots
	if shots == 0 {
		shots = 2
	}
	def := e.X.Define(p.Scope, pipelinetest.DocNamed("qa", "{"+DeploymentInput+": {type: string, required: true}}", qaStep(env.ID(), shots)))
	run, err := e.X.Svc.Admit(context.Background(), execution.Caller{PrincipalID: deliverytest.Engineer, RequestID: "req_t040"}, execution.AdmitCommand{
		Scope: p.Scope, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: e.X.ContentRevision(p), SourceCommit: executiontest.CommitA,
		Inputs: map[string]runtimev1.Value{DeploymentInput: runtimev1.StringValue(trusted)},
	})
	must(e.T, err)
	in := e.X.MustClaim(p.Scope.TenantID)
	e.X.Start(in)
	agg, err := e.X.Store.Get(context.Background(), p.Scope, run.ID)
	must(e.T, err)
	at := agg.Attempt(in.AttemptID)
	pinned, err := runtimev1.DecodeRequest[runtimev1.Context](at.Context)
	must(e.T, err)
	a := &deliverytest.Attempt{Intent: in, Context: pinned, Scope: p.Scope}
	e.admitWith(a, func(base runtimeauth.ActionSet) (runtimeauth.ActionSet, error) { return base, nil })
	return &QARun{e: e, Scope: p.Scope, Env: env, Project: p, Run: run, Attempt: a, Outputs: map[string]domain.ArtifactID{}, shots: shots}
}

// admit exchanges the attempt's admission credential for a runtime token, as
// the executor and the entrypoint do (the same steps as the delivery fixture).
func (e *Env) admitWith(a *deliverytest.Attempt, extend func(runtimeauth.ActionSet) (runtimeauth.ActionSet, error)) {
	e.T.Helper()
	ctx := context.Background()
	cred, err := runtimeauth.GenerateCredential(nil)
	must(e.T, err)
	actions, err := rpg.DefaultActions(a.Intent.Access)
	must(e.T, err)
	actions, err = extend(actions)
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
	e.Jobs.SetCluster(runtimeauth.JobEvidence{Namespace: deliverytest.Namespace, JobName: id.Name, JobUID: id.UID, PodNamespace: deliverytest.Namespace, PodName: id.PodName, PodUID: id.PodUID, PodOwnerUID: id.UID})
	resp, err := e.Auth.Admit(ctx, a.ID(), runtimev1.AdmitRequest{AdmissionCredential: cred.Reveal(), Nonce: "nnnnnnnnnnnnnnnnnnnnnn", ClientVersion: "1.4.0", Job: id})
	must(e.T, err)
	a.Token = resp.Token.Token
	claims, err := e.Verifier.Verify(ctx, a.Token)
	must(e.T, err)
	a.Session = claims.Session
}

// Caller is the runtime artifact caller of the attempt's session.
func (q *QARun) Caller() artifacts.Caller {
	return artifacts.Caller{Subject: resultsadapters.RuntimePrincipal(q.Scope.TenantID, q.Attempt.Session)}
}

// Upload reserves, uploads and completes a file artifact of the attempt through
// the real artifact service and object store, and returns its identity and
// digest. role is output for a declared output name and diagnostic otherwise.
func (q *QARun) Upload(name string, role artifacts.Role, mediaType string, data []byte) (domain.ArtifactID, runtimev1.Digest) {
	q.e.T.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	size := int64(len(data))
	in := artifacts.ReserveInput{Name: name, Kind: artifacts.KindFile, Role: role, Digest: artifacts.DigestOf(data), MediaType: &mediaType, Size: &size}
	out, err := q.e.ArtSvc.Reserve(ctx, q.Caller(), q.ID(), in, "req_t040")
	must(q.e.T, err)
	if out.Upload == nil {
		q.e.T.Fatalf("reserve %s: no upload instruction", name)
	}
	req, err := http.NewRequestWithContext(ctx, out.Upload.Method, out.Upload.URL, strings.NewReader(string(data)))
	must(q.e.T, err)
	req.ContentLength = size
	for k, v := range out.Upload.Headers {
		if strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Host") {
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := q.e.Http.Do(req)
	must(q.e.T, err)
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
	resp.Body.Close()
	if resp.StatusCode != 200 {
		q.e.T.Fatalf("upload %s: %d %s", name, resp.StatusCode, body)
	}
	if _, err := q.e.ArtSvc.Complete(ctx, q.Caller(), q.ID(), out.ID, artifacts.CompleteInput{Digest: artifacts.DigestOf(data), Size: size}, "req_t040"); err != nil {
		q.e.T.Fatalf("complete %s: %v", name, err)
	}
	return out.ID, runtimev1.Digest(artifacts.DigestOf(data))
}

// PNG is deterministic image content (not a decodable image: the fixture only
// needs distinct bytes with an image media type).
func PNG(tag string) []byte { return []byte("\x89PNG\r\n\x1a\nkodama-qa-" + tag) }

// Screenshot uploads shot-<n> as a declared output of image/png and records it.
func (q *QARun) Screenshot(n int) (domain.ArtifactID, runtimev1.Digest) {
	q.e.T.Helper()
	name := fmt.Sprintf("shot-%d", n)
	id, d := q.Upload(name, artifacts.RoleOutput, "image/png", PNG(string(q.ID())+name))
	q.Outputs[name] = id
	return id, d
}

// Diagnostic uploads a diagnostic artifact and lists it in the accepted result.
func (q *QARun) Diagnostic(name, mediaType string, data []byte) (domain.ArtifactID, runtimev1.Digest) {
	q.e.T.Helper()
	id, d := q.Upload(name, artifacts.RoleDiagnostic, mediaType, data)
	q.Diagnostics = append(q.Diagnostics, id)
	return id, d
}

// ReportFor is the correct report of this run: the deployment facts, the pinned
// subject and the given observations.
func (q *QARun) ReportFor(dep *Deployed, obs ...qa.Observation) qa.Report {
	return qa.Report{
		Format:     qa.ReportFormat,
		Subject:    qa.Subject{WorkItemID: q.Project.Item, ContentRevision: q.Attempt.Context.WorkItem.ContentRevision},
		Deployment: dep.Facts(), Summary: "Explored the preview deployment against the pinned acceptance criteria.",
		Observations: obs,
	}
}

// WriteReport uploads the report bytes as the declared report output.
func (q *QARun) WriteReport(r qa.Report) (domain.ArtifactID, runtimev1.Digest) {
	q.e.T.Helper()
	raw, err := r.Canonical()
	must(q.e.T, err)
	return q.WriteRaw(raw)
}

// WriteRaw uploads arbitrary bytes as the declared report output.
func (q *QARun) WriteRaw(raw []byte) (domain.ArtifactID, runtimev1.Digest) {
	q.e.T.Helper()
	id, d := q.Upload(ReportOutput, artifacts.RoleOutput, "application/json", raw)
	q.Outputs[ReportOutput] = id
	return id, d
}

// Finish commits the attempt's completed result naming every uploaded output.
// Every declared screenshot output must have been uploaded.
func (q *QARun) Finish() error {
	q.e.T.Helper()
	outputs := map[string]runtimev1.Value{}
	refs := map[domain.ArtifactID]runtimev1.ArtifactRef{}
	for name, id := range q.Outputs {
		outputs[name] = runtimev1.StringValue(string(id))
		refs[id] = runtimev1.ArtifactRef{ID: id, Scope: q.Scope, Attempt: q.ID(), Kind: runtimev1.KindFile, Usable: true}
	}
	diag := []domain.ArtifactID{}
	for _, id := range q.Diagnostics {
		diag = append(diag, id)
		refs[id] = runtimev1.ArtifactRef{ID: id, Scope: q.Scope, Attempt: q.ID(), Kind: runtimev1.KindFile, Usable: true}
	}
	zero := int64(0)
	res := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: &zero, DiagnosticArtifactIDs: diag, Outputs: outputs}
	h, err := runtimev1.ResultHash(res)
	must(q.e.T, err)
	_, err = q.e.X.Svc.CommitResult(context.Background(), execution.ResultCommit{
		Scope: q.Scope, AttemptID: q.ID(), Request: runtimev1.ResultRequest{ResultHash: h, Result: res}, Artifacts: refs,
	})
	return err
}

// Complete uploads every missing screenshot and the report, then finishes.
func (q *QARun) Complete(r qa.Report) {
	q.e.T.Helper()
	q.WriteReport(r)
	must(q.e.T, q.Finish())
}

// UploadAllShots uploads every declared screenshot output and returns them in order.
func (q *QARun) UploadAllShots() (ids []domain.ArtifactID, digests []runtimev1.Digest) {
	q.e.T.Helper()
	for i := 1; i <= q.shots; i++ {
		id, d := q.Screenshot(i)
		ids, digests = append(ids, id), append(digests, d)
	}
	return ids, digests
}

// Evidence builds an evidence reference.
func Evidence(id domain.ArtifactID, d runtimev1.Digest, kind qa.EvidenceKind, note string) qa.EvidenceRef {
	e := qa.EvidenceRef{ArtifactID: id, Digest: d, Kind: kind}
	if note != "" {
		e.Note = &note
	}
	return e
}

// Observation builds a valid observation with the evidence.
func Observation(title, location string, ev ...qa.EvidenceRef) qa.Observation {
	crit := "AC-1"
	return qa.Observation{
		Title: title, Severity: qa.SeverityMajor, CriterionID: &crit, Location: location,
		Steps: []string{"Open " + location, "Submit the form"}, Expected: "The order is saved and confirmed.", Actual: "A blank page is shown.",
		Evidence: ev,
	}
}

// Command is the Record command a trusted control-plane caller issues.
func (q *QARun) Command() qa.RecordCommand {
	return qa.RecordCommand{Scope: q.Scope, Attempt: q.ID(), Caller: qa.Caller{Actor: qa.Actor{Type: qa.ActorSystem, ID: "qa-reconciler"}, RequestID: "req_t040"}}
}

// ---- inspection -------------------------------------------------------------------

// Count runs a count query as the owner (every tenant visible).
func (e *Env) Count(sql string, args ...any) int { return e.X.Count(sql, args...) }

// DefectRows counts the work items of kind defect in the scope.
func (e *Env) DefectRows(scope domain.Scope) int {
	return e.Count(`SELECT count(*) FROM work_items WHERE tenant_id = $1 AND project_id = $2 AND kind = 'defect'`, string(scope.TenantID), string(scope.ProjectID))
}

// Pins counts the artifact pins held for a defect.
func (e *Env) Pins(scope domain.Scope, defect domain.WorkItemID) int {
	return e.Count(`SELECT count(*) FROM artifact_pins WHERE tenant_id = $1 AND owner_kind = $2 AND owner_id = $3`,
		string(scope.TenantID), adapters.PinOwnerKind, string(defect))
}

// RevokeMapper removes the mapping principal's bindings in the scope's tenant, as
// the identity module's revocation transaction does: the mapping authority is
// gone from the next check and the next mutation on.
func (e *Env) RevokeMapper(scope domain.Scope) { e.X.Revoke(scope, Mapper) }

// Work reads an item through the QA work store.
func (e *Env) Work(scope domain.Scope, id domain.WorkItemID) (work.Item, error) {
	return e.WorkStore.Get(context.Background(), scope, id)
}

// ---- fault injection: lost commit acknowledgment --------------------------------

// CommitFault injects commit failures into transactions that wrote: mode Lose
// applies the commit and reports an error (an unknown outcome), mode Fail rolls
// it back. Read-only transactions (and transactions that only read) are never
// affected, so a retry's reread observes the true state.
type CommitFault struct {
	Inner storage.TxBeginner
	mode  atomic.Int32
}

// Lose applies commits of writing transactions and loses their acknowledgment.
func (d *CommitFault) Lose() { d.mode.Store(1) }

// Fail rolls back writing transactions at commit.
func (d *CommitFault) Fail() { d.mode.Store(2) }

// Off restores normal commits.
func (d *CommitFault) Off() { d.mode.Store(0) }

// BeginTx implements storage.TxBeginner.
func (d *CommitFault) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	tx, err := d.Inner.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &faultTx{Tx: tx, db: d}, nil
}

type faultTx struct {
	pgx.Tx
	db    *CommitFault
	wrote atomic.Bool
}

func (t *faultTx) note(sql string) {
	s := strings.ToUpper(strings.TrimSpace(sql))
	if strings.HasPrefix(s, "INSERT") || strings.HasPrefix(s, "UPDATE") || strings.HasPrefix(s, "DELETE") {
		t.wrote.Store(true)
	}
}

func (t *faultTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	t.note(sql)
	return t.Tx.Exec(ctx, sql, args...)
}

func (t *faultTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	t.note(sql)
	return t.Tx.Query(ctx, sql, args...)
}

func (t *faultTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	t.note(sql)
	return t.Tx.QueryRow(ctx, sql, args...)
}

func (t *faultTx) Commit(ctx context.Context) error {
	if !t.wrote.Load() {
		return t.Tx.Commit(ctx)
	}
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

// SkipFail makes a FaultAppender let skip appends pass, then fail the next fail.
func (f *FaultAppender) SkipFail(skip, fail int64) {
	f.skip.Store(skip)
	f.fail.Store(fail)
}
