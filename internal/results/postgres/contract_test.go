package postgres_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/artifacts/artifactstest"
	artifactspg "github.com/optimiware/kodama/internal/artifacts/postgres"
	arts3 "github.com/optimiware/kodama/internal/artifacts/s3"
	"github.com/optimiware/kodama/internal/domain"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/execution"
	execadapters "github.com/optimiware/kodama/internal/execution/adapters"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	httppg "github.com/optimiware/kodama/internal/httpapi/postgres"
	"github.com/optimiware/kodama/internal/identity"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	pipelinepg "github.com/optimiware/kodama/internal/pipeline/postgres"
	"github.com/optimiware/kodama/internal/results"
	"github.com/optimiware/kodama/internal/results/adapters"
	rpg "github.com/optimiware/kodama/internal/results/postgres"
	"github.com/optimiware/kodama/internal/results/resultstest"
	"github.com/optimiware/kodama/internal/runtimeauth"
	rtjwt "github.com/optimiware/kodama/internal/runtimeauth/jwt"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
	"github.com/optimiware/kodama/internal/storage"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
	"github.com/optimiware/kodama/internal/workspace"
	gitad "github.com/optimiware/kodama/internal/workspace/git"
	workspacepg "github.com/optimiware/kodama/internal/workspace/postgres"
	wsruntime "github.com/optimiware/kodama/internal/workspace/runtime"
	wst "github.com/optimiware/kodama/internal/workspace/workspacetest"
	gitfx "github.com/optimiware/kodama/test/support/git"
	"github.com/optimiware/kodama/test/support/layer"
	s3support "github.com/optimiware/kodama/test/support/s3"
)

// TestPostgresContract runs the shared resultstest.Contract against the real
// composition the results module joins: PostgreSQL carrying the execution,
// workspace, artifact and runtime-grant schemas, real S3 (LocalStack) for
// artifact bytes, a real HTTPS Git server for publication and the module's own
// adapters and transaction guards (SD-13, SD-A11). Only the Kubernetes Job
// observation and the hosted identity are controlled, as in the integration
// suite. It needs the `test` environment's services and skips in the unit layer
// and when they are absent; the memory adapter runs the same suite
// unconditionally (resultstest.Test_unit_results_logs_contract).
func TestPostgresContract(t *testing.T) {
	layer.SkipService(t, "PostgreSQL")
	resultstest.Contract(t, func(tb testing.TB) resultstest.Subject {
		w := newPGWorld(tb)
		return resultstest.Subject{Deps: w.deps, World: w, Scopes: resultstest.Scopes{A: scopeA, A2: scopeA2, B: scopeB}}
	})
}

const (
	namespace = "t081-runs"
	issuer    = "https://runtime.t081.invalid"
)

// Scopes of the contract: tenant A with two projects, and tenant B. The
// executiontest fixture seeds none of them; newPGWorld does.
var (
	scopeA  = domain.Scope{TenantID: "t081_acme", ProjectID: "p081_web"}
	scopeA2 = domain.Scope{TenantID: "t081_acme", ProjectID: "p081_api"}
	scopeB  = domain.Scope{TenantID: "t081_globex", ProjectID: "p081_ops"}
)

type creds struct{ read, write string }

func (c creds) Resolve(_ context.Context, s domain.Scope, ref string) (gitad.Credential, error) {
	switch s {
	case scopeA, scopeA2, scopeB:
	default:
		return gitad.Credential{}, errors.New("foreign scope")
	}
	switch ref {
	case "secret://t081/checkout":
		return gitad.Credential{Username: gitfx.Username, Password: c.read}, nil
	case "secret://t081/publisher":
		return gitad.Credential{Username: gitfx.Username, Password: c.write}, nil
	}
	return gitad.Credential{}, errors.New("unknown secret reference")
}

type tenants []domain.TenantID

func (t tenants) TenantIDs(context.Context) ([]domain.TenantID, error) { return t, nil }

// pgAttempt is one started attempt with runtime authority.
type pgAttempt struct {
	run     *execution.Run
	intent  *execution.AttemptSpec
	pinned  runtimev1.Context
	token   string
	session runtimeauth.SessionID
	scope   domain.Scope
}

func (a *pgAttempt) id() domain.AttemptID { return a.intent.AttemptID }

type pgWorld struct {
	t    testing.TB
	x    *executiontest.Env
	deps results.Dependencies

	auth     *runtimeauth.Service
	verifier *rtjwt.Verifier
	jobs     *rtest.Jobs
	exec     *execution.Service
	ws       *workspace.Service
	artSvc   *artifacts.Service
	repo     *gitfx.Repo
	items    *wst.Items
	http     *http.Client

	mu       sync.Mutex
	attempts map[domain.AttemptID]*pgAttempt
	seq      int
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func newPGWorld(t testing.TB) *pgWorld {
	t.Helper()
	ctx := context.Background()
	x := executiontest.New(t, executiontest.Options{})
	w := &pgWorld{t: t, x: x, attempts: map[domain.AttemptID]*pgAttempt{}, http: &http.Client{Timeout: 30 * time.Second}}

	lim := executiontest.DefaultLimit()
	lim.TenantConcurrency, lim.ProjectConcurrency = 200, 100
	x.Limits.Set(lim)
	// Seed every scope of the contract: tenants, projects and the engineer's
	// membership the admission fixtures act as.
	x.Base.SeedTenant(t, string(scopeA.TenantID), string(scopeA.ProjectID), string(scopeA2.ProjectID))
	x.Base.SeedTenant(t, string(scopeB.TenantID), string(scopeB.ProjectID))
	for _, s := range []domain.Scope{scopeA, scopeA2, scopeB} {
		x.Grant(s, executiontest.Engineer, identity.RoleEngineer)
	}

	contribs := []storage.Contribution{
		storage.BaseContribution(), eventspg.Migrations(), httppg.Migrations(), identitypg.Migrations(), workpg.Migrations(),
		secretspg.Migrations(), pipelinepg.Migrations(), workspacepg.Migrations(), artifactspg.Migrations(),
	}
	contribs = append(contribs, execpg.Migrations()...)
	contribs = append(contribs, rtpg.Migrations())
	manifest, err := storage.NewManifest(contribs...)
	must(t, err)
	mctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	_, err = storage.Migrate(mctx, x.Migration, manifest, storage.MigrateOptions{RuntimeRole: x.Base.DB.RuntimeRole})
	must(t, err)

	// ---- runtime authority over execution's rows ----
	attemptAuth, err := rpg.NewAttemptAuthority(x.Store, func(domain.Scope) (string, error) { return namespace, nil }, nil)
	must(t, err)
	grants, err := rtpg.NewStore(x.Runtime, attemptAuth)
	must(t, err)
	key, err := rtjwt.GenerateKey()
	must(t, err)
	ring, err := rtjwt.NewKeyRing(1, key)
	must(t, err)
	signer, err := rtjwt.NewSigner(ring, issuer)
	must(t, err)
	pol := runtimeauth.Policy{AdmissionTTL: 10 * time.Minute, TokenTTL: 5 * time.Minute, MaxSession: 2 * time.Hour}
	w.verifier, err = rtjwt.NewVerifier(rtjwt.VerifierConfig{Ring: ring, Issuer: issuer, MaxLifetime: pol.MaxSession, Clock: x.Clock.Now})
	must(t, err)
	w.jobs = &rtest.Jobs{}
	w.auth, err = runtimeauth.NewService(runtimeauth.Config{
		Store: grants, Attempts: attemptAuth, Jobs: w.jobs, Tokens: signer, Verifier: w.verifier,
		Wrapper: runtimeauth.ExactWrapperPolicy{Supported: []string{"1.4.0"}}, Policy: pol, Clock: x.Clock.Now,
	})
	must(t, err)
	guard, err := rpg.NewGuard(attemptAuth, ring.Generation, x.Clock.Now)
	must(t, err)
	authority, err := adapters.NewAuthority(w.verifier, ring, grants, x.Clock.Now)
	must(t, err)
	guarded, err := rpg.NewExecutionStore(x.Store, x.Runtime, guard)
	must(t, err)

	// ---- artifacts on real S3 ----
	s3 := s3support.New(t)
	artStore, err := artifactspg.NewStore(artifactspg.Config{DB: x.Runtime})
	must(t, err)
	objects, err := arts3.NewStore(arts3.Config{Client: s3.Client, Bucket: s3.Bucket(), Prefix: s3.Prefix, AllowInsecure: true, Clock: x.Clock.Now})
	must(t, err)
	artAuth, err := adapters.NewArtifactAuthority(grants, ring, x.Clock.Now)
	must(t, err)
	w.artSvc, err = artifacts.NewService(artifacts.ServiceConfig{
		Repository: artStore, Authority: artAuth, Reader: artifactstest.NewReader(), Objects: objects, Registry: artifactstest.NewRegistry(),
		Clock: x.Clock.Now, UploadTTL: 10 * time.Minute, DownloadTTL: 5 * time.Minute, ClaimLease: 30 * time.Second, SpoolDir: t.TempDir(),
	})
	must(t, err)

	// ---- Git and workspace ----
	git := gitfx.NewServer(t)
	w.repo = git.NewRepo(t, "t081-repo")
	gopts := gitad.Options{
		CAFile: git.CAFile, CommandTimeout: 30 * time.Second, MaxBundleBytes: 8 << 20, MaxScratchBytes: 64 << 20,
		ScratchRoot: filepath.Join(t.TempDir(), "scratch"),
	}
	cr := creds{read: git.ReadToken, write: git.WriteToken}
	reader, err := gitad.NewReader(gopts, cr)
	must(t, err)
	pub, err := gitad.NewPublisher(gopts, cr)
	must(t, err)
	t.Cleanup(reader.Close)
	t.Cleanup(pub.Close)
	authz, repos, secrets := wst.NewAuthz(), wst.NewRepos(), wst.NewSecrets()
	w.items = wst.NewItems()
	for _, s := range []domain.Scope{scopeA, scopeA2, scopeB} {
		authz.Grant(wst.Engineer.ID, s)
		authz.Grant("execution", s)
		repos.Set(s, workspace.RepositoryConfig{ID: "repo_t081", URL: w.repo.URL, CheckoutSecretRef: "secret://t081/checkout", PublisherSecretRef: "secret://t081/publisher", BaseRef: w.repo.BaseRef})
	}
	wsAttempts, err := execpg.NewAttemptAuthority(x.Store)
	must(t, err)
	bundles, err := adapters.NewBundles(artStore, objects)
	must(t, err)
	wsStore, err := rpg.NewWorkspaceStore(x.Workspaces, x.Runtime, guard)
	must(t, err)
	wsOpts := wst.DefaultOptions()
	deps := workspace.Dependencies{
		Store: wsStore, Tenants: tenants{scopeA.TenantID, scopeB.TenantID}, Authorizer: authz, WorkItems: w.items, Repositories: repos,
		Attempts: wsAttempts, Bundles: bundles, Secrets: secrets, Refs: reader, Source: reader, Publisher: pub, Clock: x.Clock.Now,
	}
	w.ws, err = workspace.NewService(deps, wsOpts)
	must(t, err)
	plain := deps
	plain.Store = x.Workspaces
	wsPlain, err := workspace.NewService(plain, wsOpts)
	must(t, err)
	sources, err := execadapters.NewSources(x.Workspaces, reader, wsPlain)
	must(t, err)

	// ---- execution: plain (trusted paths) and guarded (the result path) ----
	w.exec = x.NewService(executiontest.Options{Sources: sources})
	execSvc := x.NewService(executiontest.Options{Sources: sources, Store: guarded})
	x.Svc = w.exec
	execAd, err := adapters.NewExecution(execSvc, x.Store)
	must(t, err)
	evidence, err := adapters.NewArtifacts(w.artSvc)
	must(t, err)
	wsrc, err := wsruntime.NewSource(w.ws)
	must(t, err)
	source, err := adapters.NewSource(wsrc)
	must(t, err)
	w.deps = results.Dependencies{Authority: authority, Contexts: execAd, Evidence: evidence, Finalizer: execAd, Source: source}
	return w
}

func (w *pgWorld) next() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seq++
	return w.seq
}

func (w *pgWorld) attempt(a resultstest.Attempt) *pgAttempt {
	w.mu.Lock()
	defer w.mu.Unlock()
	p := w.attempts[a.ID]
	if p == nil {
		w.t.Fatalf("attempt %s was not created by this world", a.ID)
	}
	return p
}

const (
	readRun   = "workspace: {access: read, from: run}"
	writeRun  = "workspace: {access: write, from: run}"
	reportJob = "outputs: {report: {type: artifact, path: /kodama/outputs/report.json}}"
)

// NewAttempt seeds a work item and a provisioned workspace (the branch exists
// on the remote), admits a run, claims and starts its step, registers the
// attempt's grant and exchanges the admission credential for a runtime token,
// as the executor and the entrypoint would.
func (w *pgWorld) NewAttempt(tb testing.TB, scope domain.Scope, o resultstest.AttemptOptions) resultstest.Attempt {
	tb.Helper()
	ctx := context.Background()
	item := w.x.WorkItem(scope, fmt.Sprintf("wi_t081_%d", w.next()), "# Plan\nDo the thing.")
	w.items.Add(scope, item, "story")
	ws, _, err := w.ws.Provision(ctx, wst.Engineer, scope, item)
	must(tb, err)
	if ws.State != workspace.StateAvailable {
		tb.Fatalf("workspace %s (%s)", ws.State, ws.LastError)
	}
	project := executiontest.Project{Scope: scope, Item: item, Workspace: ws.ID}

	access, extra := readRun, []string{reportJob}
	if o.NoOutputs {
		extra = nil
	}
	if o.Writer {
		access = writeRun
	}
	step := "{id: build, kind: container, image: " + pipelinetest.Image + ", command: [/bin/sh, -ec], " + access
	for _, e := range extra {
		step += ", " + e
	}
	step += ", timeoutSeconds: 600}"
	def := w.x.Define(scope, pipelinetest.Doc(step))
	run, err := w.exec.Admit(ctx, execution.Caller{PrincipalID: executiontest.Engineer, RequestID: "req_t081"}, execution.AdmitCommand{
		Scope: scope, WorkItemID: item, DefinitionID: def.ID, ContentRevision: w.x.ContentRevision(project), SourceCommit: w.repo.Initial,
	})
	must(tb, err)
	in, err := w.exec.ClaimRunnable(ctx, scope.TenantID)
	must(tb, err)
	w.x.Start(in)
	agg, err := w.x.Store.Get(ctx, scope, run.ID)
	must(tb, err)
	pinned, err := runtimev1.DecodeRequest[runtimev1.Context](agg.Attempt(in.AttemptID).Context)
	must(tb, err)

	a := &pgAttempt{run: run, intent: in, pinned: pinned, scope: scope}
	cred, err := runtimeauth.GenerateCredential(nil)
	must(tb, err)
	actions, err := rpg.DefaultActions(in.Access)
	must(tb, err)
	_, err = w.auth.Register(ctx, runtimeauth.RegisterCommand{Scope: scope, AttemptID: a.id(), Credential: cred, Actions: actions, Context: pinned})
	must(tb, err)
	sum := sha256.Sum256([]byte(a.id()))
	h := hex.EncodeToString(sum[:])
	uid := func(off int) string {
		return h[off:off+8] + "-" + h[off+8:off+12] + "-4" + h[off+13:off+16] + "-8" + h[off+17:off+20] + "-" + h[off+20:off+32]
	}
	name := in.WorkloadName
	job := runtimev1.JobIdentity{Name: name, UID: uid(0), PodName: name + "-x7k2p", PodUID: uid(16)}
	w.jobs.SetCluster(runtimeauth.JobEvidence{Namespace: namespace, JobName: job.Name, JobUID: job.UID, PodNamespace: namespace, PodName: job.PodName, PodUID: job.PodUID, PodOwnerUID: job.UID})
	resp, err := w.auth.Admit(ctx, a.id(), runtimev1.AdmitRequest{AdmissionCredential: cred.Reveal(), Nonce: "nnnnnnnnnnnnnnnnnnnnnn", ClientVersion: "1.4.0", Job: job})
	must(tb, err)
	a.token = resp.Token.Token
	claims, err := w.verifier.Verify(ctx, a.token)
	must(tb, err)
	a.session = claims.Session

	w.mu.Lock()
	w.attempts[a.id()] = a
	w.mu.Unlock()
	return resultstest.Attempt{Scope: scope, Run: run.ID, ID: a.id(), Pinned: pinned}
}

// Bearer implements resultstest.World: the attempt's runtime token.
func (w *pgWorld) Bearer(tb testing.TB, a resultstest.Attempt, _ results.Action) string {
	tb.Helper()
	return w.attempt(a).token
}

func (w *pgWorld) caller(a *pgAttempt) artifacts.Caller {
	return artifacts.Caller{Subject: adapters.RuntimePrincipal(a.scope.TenantID, a.session)}
}

// upload reserves a file artifact through the artifact service, sends its
// bytes through the signed transfer and, unless incomplete, completes it.
func (w *pgWorld) upload(tb testing.TB, p *pgAttempt, name string, role artifacts.Role, data []byte, incomplete bool) domain.ArtifactID {
	tb.Helper()
	ctx := context.Background()
	mt, size := "text/plain", int64(len(data))
	out, err := w.artSvc.Reserve(ctx, w.caller(p), p.id(), artifacts.ReserveInput{
		Name: name, Kind: artifacts.KindFile, Role: role, Digest: artifacts.DigestOf(data), MediaType: &mt, Size: &size,
	}, "req_t081")
	must(tb, err)
	if out.Upload == nil {
		tb.Fatal("no upload instruction")
	}
	up := *out.Upload
	req, err := http.NewRequest(up.Method, up.URL, bytes.NewReader(data))
	must(tb, err)
	req.ContentLength = int64(len(data))
	for k, v := range up.Headers {
		if strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Host") {
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := w.http.Do(req)
	must(tb, err)
	defer resp.Body.Close()
	if b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096)); resp.StatusCode != http.StatusOK {
		tb.Fatalf("upload: %d %s", resp.StatusCode, b)
	}
	if !incomplete {
		_, err = w.artSvc.Complete(ctx, w.caller(p), p.id(), out.ID, artifacts.CompleteInput{Digest: artifacts.DigestOf(data), Size: size}, "req_t081")
		must(tb, err)
	}
	return out.ID
}

// Artifact implements resultstest.World.
func (w *pgWorld) Artifact(tb testing.TB, a resultstest.Attempt, s resultstest.ArtifactSpec) domain.ArtifactID {
	tb.Helper()
	role := artifacts.RoleOutput
	if s.Role != "" {
		role = artifacts.Role(s.Role)
	}
	n := w.next()
	name := fmt.Sprintf("artifact-%d", n)
	if role == artifacts.RoleOutput {
		name = "report" // the only declared file output of the contract's attempts
	}
	return w.upload(tb, w.attempt(a), name, role, []byte(fmt.Sprintf("contract artifact %d\n", n)), s.Incomplete)
}

// Bundle implements resultstest.World: what the entrypoint would upload for the
// writer, on top of its pinned expected tip.
func (w *pgWorld) Bundle(tb testing.TB, a resultstest.Attempt) (runtimev1.GitCommit, domain.ArtifactID) {
	tb.Helper()
	src := a.Pinned.Workspace.Source
	if src == nil || src.ExpectedTip == nil {
		tb.Fatal("not a writer attempt")
	}
	n := w.next()
	commit, bundle, _ := w.repo.Bundle(tb, string(*src.ExpectedTip), "agent change", gitfx.File{Path: fmt.Sprintf("agent-%d.txt", n), Content: fmt.Sprintf("agent %d", n)})
	return runtimev1.GitCommit(commit), w.upload(tb, w.attempt(a), fmt.Sprintf("bundle-%d", n), artifacts.RoleBundle, bundle, false)
}

// Revoke implements resultstest.World: execution's own cancellation, then the
// explicit revocation of the attempt's grants.
func (w *pgWorld) Revoke(tb testing.TB, a resultstest.Attempt) {
	tb.Helper()
	p := w.attempt(a)
	_, err := w.exec.Cancel(context.Background(), execution.Caller{PrincipalID: executiontest.Engineer, RequestID: "req_t081"}, a.Scope, p.run.ID)
	must(tb, err)
	must(tb, w.auth.Revoke(context.Background(), a.Scope, a.ID))
}

// MoveTip implements resultstest.World.
func (w *pgWorld) MoveTip(tb testing.TB, a resultstest.Attempt) runtimev1.GitCommit {
	tb.Helper()
	n := w.next()
	return runtimev1.GitCommit(w.repo.HumanPush(tb, *a.Pinned.Workspace.Source.Branch, "human change", gitfx.File{Path: fmt.Sprintf("human-%d.txt", n), Content: "human"}))
}

// Tip implements resultstest.World.
func (w *pgWorld) Tip(tb testing.TB, a resultstest.Attempt) runtimev1.GitCommit {
	tb.Helper()
	return runtimev1.GitCommit(w.repo.Tip(tb, *a.Pinned.Workspace.Source.Branch))
}
