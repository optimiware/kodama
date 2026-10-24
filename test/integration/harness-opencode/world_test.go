package harnessopencode_test

// The composition under test. It is a TEST-ONLY wiring of the merged modules
// (production root wiring belongs to T-027/T-039 and is not touched):
//
//   - real PostgreSQL with every dependent schema;
//   - the real execution service, the real Kubernetes executor, observer and
//     reconciler against the kind cluster, and the actual `kodama step`
//     entrypoint running inside the Jobs;
//   - the real runtime authority, results service, artifact service (real S3
//     through a Pod-reachable TLS front) and workspace publication over a real
//     HTTPS Git remote;
//   - the real agent profile service and admission chain (profile expansion
//     into the wrapper payload and design baseline);
//   - the real model gateway, forwarding to a deterministic OpenAI-compatible
//     HTTPS upstream (a fixture, never a claim about any provider or model);
//   - the real MCP proxy in front of a real Streamable HTTP MCP server.
//
// Doubles: the model upstream, the MCP server's tools (test/support/mcp), the
// hosted identity and approvals of the profile and MCP registration services.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	wspec "github.com/optimiware/kodama/images/harness-opencode/wrapper/spec"
	"github.com/optimiware/kodama/internal/agents"
	agentsadapters "github.com/optimiware/kodama/internal/agents/adapters"
	agentspg "github.com/optimiware/kodama/internal/agents/postgres"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/artifacts/artifactstest"
	artifactshttp "github.com/optimiware/kodama/internal/artifacts/http"
	artifactspg "github.com/optimiware/kodama/internal/artifacts/postgres"
	arts3 "github.com/optimiware/kodama/internal/artifacts/s3"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/entrypoint"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/execution"
	execadapters "github.com/optimiware/kodama/internal/execution/adapters"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/gateway"
	gwadapters "github.com/optimiware/kodama/internal/gateway/adapters"
	"github.com/optimiware/kodama/internal/gateway/gatewaytest"
	gatewayhttp "github.com/optimiware/kodama/internal/gateway/http"
	"github.com/optimiware/kodama/internal/gateway/openai"
	gatewaypg "github.com/optimiware/kodama/internal/gateway/postgres"
	"github.com/optimiware/kodama/internal/httpapi"
	httpapipg "github.com/optimiware/kodama/internal/httpapi/postgres"
	"github.com/optimiware/kodama/internal/identity"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
	"github.com/optimiware/kodama/internal/kube"
	kubeadapters "github.com/optimiware/kodama/internal/kube/adapters"
	"github.com/optimiware/kodama/internal/mcpproxy"
	mcpadapters "github.com/optimiware/kodama/internal/mcpproxy/adapters"
	"github.com/optimiware/kodama/internal/mcpproxy/mcptest"
	mcppg "github.com/optimiware/kodama/internal/mcpproxy/postgres"
	mcpprotocol "github.com/optimiware/kodama/internal/mcpproxy/protocol"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	pipelinepg "github.com/optimiware/kodama/internal/pipeline/postgres"
	"github.com/optimiware/kodama/internal/results"
	resultsadapters "github.com/optimiware/kodama/internal/results/adapters"
	resultshttp "github.com/optimiware/kodama/internal/results/http"
	rpg "github.com/optimiware/kodama/internal/results/postgres"
	"github.com/optimiware/kodama/internal/runtimeauth"
	runtimehttp "github.com/optimiware/kodama/internal/runtimeauth/http"
	"github.com/optimiware/kodama/internal/runtimeauth/jwt"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/internal/secrets/keyfile"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
	"github.com/optimiware/kodama/internal/storage"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
	"github.com/optimiware/kodama/internal/workspace"
	gitad "github.com/optimiware/kodama/internal/workspace/git"
	workspacepg "github.com/optimiware/kodama/internal/workspace/postgres"
	wsruntime "github.com/optimiware/kodama/internal/workspace/runtime"
	wst "github.com/optimiware/kodama/internal/workspace/workspacetest"
	gitfx "github.com/optimiware/kodama/test/support/git"
	mcpfixture "github.com/optimiware/kodama/test/support/mcp"
	s3support "github.com/optimiware/kodama/test/support/s3"
)

const (
	prefix       = "t031"
	issuer       = "https://runtime.t031.invalid"
	installation = "inst-t031"
	admin        = "u031_admin"
	// modelName is the fixture upstream's model name: a deterministic control,
	// not a production model.
	modelName   = "t031-det"
	gwSecret    = "model-key"
	gwBearer    = "sk-t031-PROVIDER-KEY-SENTINEL"
	repoName    = "t031-repo"
	baselineDoc = "docs/specs/agent-runtime.md"
)

var scope = executiontest.ScopeA

// checkoutFunc adapts a function to kube.CheckoutProvider for this test
// composition. The credential is the fixture's read token and never enters the
// Job document; the executor stores it in the immutable bootstrap Secret.
type checkoutFunc func(context.Context, kube.CheckoutRequest) (kube.CheckoutMaterial, error)

func (f checkoutFunc) Checkout(ctx context.Context, req kube.CheckoutRequest) (kube.CheckoutMaterial, error) {
	return f(ctx, req)
}

// fixtureCheckout binds the test credential to the exact committed scope,
// workspace and repository of the fixture. The executor's own equality check is
// not a substitute: returning echoed request fields for whatever workspace
// arrives would still be a scope-confused provider.
func fixtureCheckout(req kube.CheckoutRequest, wantScope domain.Scope, workspaceID, repoURL string, credential, ca []byte) (kube.CheckoutMaterial, error) {
	switch {
	case req.Scope != wantScope:
		return kube.CheckoutMaterial{}, fmt.Errorf("%w: foreign scope", kube.ErrCheckoutDenied)
	case workspaceID == "" || req.WorkspaceID != workspaceID:
		return kube.CheckoutMaterial{}, fmt.Errorf("%w: unknown workspace", kube.ErrCheckoutDenied)
	case req.RepositoryURL != repoURL:
		return kube.CheckoutMaterial{}, fmt.Errorf("%w: mismatched repository", kube.ErrCheckoutDenied)
	case len(credential) == 0 || len(ca) == 0:
		return kube.CheckoutMaterial{}, fmt.Errorf("%w: incomplete material", kube.ErrCheckoutDenied)
	}
	return kube.CheckoutMaterial{
		Scope: req.Scope, WorkspaceID: req.WorkspaceID, RepositoryURL: req.RepositoryURL,
		Credential: append([]byte(nil), credential...), GitCA: append([]byte(nil), ca...),
	}, nil
}

type world struct {
	t *testing.T
	c *cluster
	x *executiontest.Env

	ca   *testCA
	plat *listener
	s3L  *listener
	gitL *listener
	// platLog records every request path the platform listener served.
	platLog *accessLog

	nsA    string
	policy kube.ExecutionPolicy

	auth     *kubeadapters.Authority
	observer *kube.JobObserver
	runtime  *kube.Runtime
	rec      *kube.Reconciler
	rt       *runtimeauth.Service
	ring     *jwt.KeyRing
	verifier *jwt.Verifier
	grants   *rtpg.Store
	svc      *execution.Service // trusted paths: admit, claim, cancel, reconcile
	svcRes   *execution.Service // the result path, over the session-guarded store

	agentsSvc *agents.Service
	approvals *approvals
	chain     agentsadapters.Chain
	drafts    *draftCatalogue
	validator *pipeline.Validator

	secretSv *secrets.Service
	wrapper  secrets.KeyWrapper
	gwSvc    *gateway.Service
	gwStore  *gatewaypg.Store
	model    *gatewaytest.Provider
	models   *modelScript
	mcpSvc   *mcpproxy.Service
	humans   *mcptest.Humans
	mcpfx    *mcpfixture.Server
	mcpReg   mcpproxy.Registration

	s3     *s3support.Env
	artSvc *artifacts.Service
	ws     *workspace.Service
	items  *wst.Items
	git    *gitfx.Server
	repo   *gitfx.Repo
	// repoURL is the Pod-reachable URL of the Git remote.
	repoURL string
	// workspaceID is the fixture workspace provisioned for the current run;
	// the checkout provider only serves that exact workspace.
	workspaceID string
	// design is the upstream design/blueprint repository the baselines pin; its
	// branch is what a test moves.
	design    *gitfx.Repo
	designURL string

	reader *artifactstest.Reader
	tokMu  sync.Mutex
	tokens map[domain.AttemptID][]string

	ids atomic.Int64
}

// draftCatalogue lets each test pin the tuple it registers: the catalogue is
// built from the drafts the test registers, with the evidence this task owns.
type draftCatalogue struct{ entries []agents.CatalogueEntry }

// add records the entry once: every profile of the composition shares the one
// image/wrapper/harness tuple under test.
func (d *draftCatalogue) add(e agents.CatalogueEntry) {
	if len(d.entries) == 0 {
		d.entries = append(d.entries, e)
	}
}

func (d *draftCatalogue) Qualify(ctx context.Context, t agents.Tuple) (agents.Qualification, error) {
	c, err := agents.NewCatalogue(d.entries...)
	if err != nil {
		return agents.Qualification{}, err
	}
	return c.Qualify(ctx, t)
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type tenantList []domain.TenantID

func (t tenantList) TenantIDs(context.Context) ([]domain.TenantID, error) { return t, nil }

type lateHandler struct{ h atomic.Pointer[http.Handler] }

func (l *lateHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h := l.h.Load(); h != nil {
		(*h).ServeHTTP(w, r)
		return
	}
	http.Error(w, "not ready", http.StatusServiceUnavailable)
}

type gitCreds struct{ read, write string }

func (c gitCreds) Resolve(_ context.Context, s domain.Scope, ref string) (gitad.Credential, error) {
	if s != scope {
		return gitad.Credential{}, errors.New("foreign scope")
	}
	switch ref {
	case "secret://t031/checkout":
		return gitad.Credential{Username: gitfx.Username, Password: c.read}, nil
	case "secret://t031/publisher":
		return gitad.Credential{Username: gitfx.Username, Password: c.write}, nil
	}
	return gitad.Credential{}, errors.New("unknown secret reference")
}

func newWorld(t *testing.T) *world {
	t.Helper()
	c := requireCluster(t)
	x := executiontest.New(t)
	w := &world{t: t, c: c, x: x, platLog: &accessLog{}, approvals: &approvals{tools: map[string]bool{}}, drafts: &draftCatalogue{}, humans: &mcptest.Humans{},
		reader: artifactstest.NewReader(), tokens: map[domain.AttemptID][]string{}}
	w.reader.Allow("user:t031", scope)
	x.GrantAll()
	x.Grant(scope, admin, identity.RoleAdmin)
	x.Limits.Set(func() execution.Limit {
		l := executiontest.DefaultLimit()
		l.ProjectConcurrency, l.TenantConcurrency = 50, 100
		l.StartupBudget = 5 * time.Minute
		l.DefaultTimeout, l.MaxTimeout = 20*time.Minute, time.Hour
		l.MaxDiagnosticBytes = 1 << 20
		return l
	}())
	ctx := context.Background()

	// ---- schema: every module this composition uses, in dependency order ----
	contribs := []storage.Contribution{
		storage.BaseContribution(), eventspg.Migrations(), httpapipg.Migrations(), identitypg.Migrations(), workpg.Migrations(),
		secretspg.Migrations(), pipelinepg.Migrations(), workspacepg.Migrations(), artifactspg.Migrations(),
	}
	contribs = append(contribs, execpg.Migrations()...)
	contribs = append(contribs, rtpg.Migrations(), gatewaypg.Migrations(), agentspg.Migrations(), mcppg.Migrations())
	manifest, err := storage.NewManifest(contribs...)
	must(t, err)
	mctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	_, err = storage.Migrate(mctx, x.Migration, manifest, storage.MigrateOptions{RuntimeRole: x.Base.DB.RuntimeRole})
	must(t, err)

	// ---- network surface ----
	w.ca = newCA(t)
	late := &lateHandler{}
	w.plat = serveTLS(t, w.ca, c.hostIP, 2, w.platLog.wrap(late))
	s3Endpoint, err := mustEnv("KODAMA_S3_ENDPOINT")
	must(t, err)
	s3u, err := url.Parse(s3Endpoint)
	must(t, err)
	w.s3L = serveTLS(t, w.ca, c.hostIP, 3, reverse(s3u, nil, true, ""))
	w.git = gitfx.NewServer(t)
	gu, err := url.Parse(w.git.URL)
	must(t, err)
	gitCA, err := os.ReadFile(w.git.CAFile)
	must(t, err)
	writeAuth := "Basic " + b64(gitfx.Username+":"+w.git.WriteToken)
	// Pods reach the remote through a read-only front; the Job delivers the
	// read-only checkout credential and Git CA in its bootstrap Secret, so no
	// test-side authorization is injected here. Only the host-side publisher
	// pushes (with its own write token).
	podGit := reverse(gu, gitCA, false, "")
	w.gitL = serveTLS(t, w.ca, c.hostIP, 4, http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "git-receive-pack") || r.URL.Query().Get("service") == "git-receive-pack" {
			http.Error(rw, "read-only", http.StatusForbidden)
			return
		}
		podGit.ServeHTTP(rw, r)
	}))
	// The host cannot reach the Pod-side address (the container provider's host
	// alias), so the host-side Git (workspace reader and publisher) is pointed at
	// a loopback front through a url.<base>.insteadOf rewrite in the Git binary
	// the workspace adapter runs. The loopback front brings the write token.
	hostGit := serveTLSLoopback(t, w.ca, 5, reverse(gu, gitCA, false, writeAuth))
	gitBin, err := exec.LookPath("git")
	must(t, err)

	// ---- Kubernetes: policy, namespace, executor ----
	w.nsA = "t031-" + randHex(3) + "-a"
	actions := append(kube.DefaultActions(false), runtimeauth.ActionModelInvoke, runtimeauth.ActionToolInvoke)
	wactions := append(kube.DefaultActions(true), runtimeauth.ActionModelInvoke, runtimeauth.ActionToolInvoke)
	egress := []kube.EgressRule{{CIDR: c.hostIP + "/32", Protocol: "TCP", Ports: []int32{int32(w.plat.Port), int32(w.s3L.Port), int32(w.gitL.Port)}}}
	cfg := kube.PolicyConfig{
		Tenants:       map[domain.TenantID]kube.TenantTarget{scope.TenantID: {Namespace: w.nsA, ServiceAccount: "t031-runner"}},
		InjectorImage: c.injector, InjectorCommand: []string{"cp", "/usr/local/bin/kodama", "/kodama/bin/kodama"},
		RuntimeURL: w.plat.URL, RuntimeCA: w.ca.PEM, RuntimeEgress: egress,
		Workload:  kube.ResourceBounds{CPURequest: "100m", CPULimit: "2", MemoryRequest: "256Mi", MemoryLimit: "2Gi", EphemeralRequest: "64Mi", EphemeralLimit: "1Gi"},
		Injector:  kube.ResourceBounds{CPURequest: "10m", CPULimit: "100m", MemoryRequest: "8Mi", MemoryLimit: "64Mi", EphemeralRequest: "16Mi", EphemeralLimit: "128Mi"},
		Volumes:   kube.VolumeSizes{Workspace: "128Mi", Outputs: "32Mi", Scratch: "256Mi", Results: "1Mi", Tools: "64Mi", Secrets: "4Mi", Identity: "1Mi"},
		RunAsUser: 65532, RunAsGroup: 65532, TerminationGrace: 30 * time.Second,
		ReaderActions: actions, WriterActions: wactions,
	}
	w.policy, err = kube.NewExecutionPolicy(cfg)
	must(t, err)
	w.provision(w.nsA, string(scope.TenantID))
	t.Cleanup(func() {
		_, _ = run("kubectl", "--kubeconfig", c.kubeconfig, "delete", "namespace", w.nsA, "--wait=false", "--ignore-not-found")
	})
	attempts, err := kubeadapters.NewAttempts(x.Store)
	must(t, err)
	w.auth, err = kubeadapters.NewAuthority(x.Store, w.policy)
	must(t, err)
	kopt := kube.Options{Clock: x.Clock.Now, CreateQuiescence: 3 * time.Second, OrphanGrace: 2 * time.Second, PreflightTTL: time.Second}
	deps := kube.Dependencies{Policy: w.policy, Cluster: c.client, Attempts: attempts}
	w.observer, err = kube.NewJobObserver(deps, kopt)
	must(t, err)

	// ---- runtime authority ----
	w.grants, err = rtpg.NewStore(x.Runtime, w.auth)
	must(t, err)
	key, err := jwt.GenerateKey()
	must(t, err)
	w.ring, err = jwt.NewKeyRing(1, key)
	must(t, err)
	rtPolicy := runtimeauth.Policy{AdmissionTTL: 10 * time.Minute, TokenTTL: 5 * time.Minute, MaxSession: 2 * time.Hour}
	signer, err := jwt.NewSigner(w.ring, issuer)
	must(t, err)
	w.verifier, err = jwt.NewVerifier(jwt.VerifierConfig{Ring: w.ring, Issuer: issuer, MaxLifetime: rtPolicy.MaxSession, Clock: x.Clock.Now})
	must(t, err)
	w.rt, err = runtimeauth.NewService(runtimeauth.Config{
		Store: w.grants, Attempts: w.auth, Jobs: w.observer, Tokens: signer, Verifier: w.verifier,
		Wrapper: runtimeauth.ExactWrapperPolicy{Supported: []string{entrypoint.Version}}, Policy: rtPolicy, Clock: x.Clock.Now,
	})
	must(t, err)
	deps.Registrar = w.rt
	deps.Checkout = checkoutFunc(func(_ context.Context, req kube.CheckoutRequest) (kube.CheckoutMaterial, error) {
		return fixtureCheckout(req, scope, w.workspaceID, w.repoURL, []byte(gitfx.Username+":"+w.git.ReadToken), w.ca.PEM)
	})
	w.runtime, err = kube.NewRuntime(deps, kopt)
	must(t, err)

	// ---- artifacts (real S3) ----
	w.s3 = s3support.New(t)
	tcfg := w.s3.Config
	tcfg.Endpoint = w.s3L.URL
	artStore, err := artifactspg.NewStore(artifactspg.Config{DB: x.Runtime})
	must(t, err)
	objects, err := arts3.NewStore(arts3.Config{Client: w.s3.Client, TransferClient: s3support.NewClient(tcfg), Bucket: w.s3.Bucket(), Prefix: w.s3.Prefix, Clock: x.Clock.Now})
	must(t, err)
	artAuth, err := resultsadapters.NewArtifactAuthority(w.grants, w.ring, x.Clock.Now)
	must(t, err)
	w.artSvc, err = artifacts.NewService(artifacts.ServiceConfig{
		Repository: artStore, Authority: artAuth, Reader: w.reader, Objects: objects, Registry: artifactstest.NewRegistry(),
		Clock: x.Clock.Now, UploadTTL: 10 * time.Minute, DownloadTTL: 5 * time.Minute, ClaimLease: 30 * time.Second, SpoolDir: t.TempDir(),
	})
	must(t, err)

	// ---- Git and workspaces ----
	w.repo = w.git.NewRepo(t, repoName)
	w.design = w.git.NewRepo(t, "t031-design")
	w.designURL = w.design.URL
	w.repoURL = strings.Replace(w.repo.URL, w.git.URL, w.gitL.URL, 1)
	script := filepath.Join(t.TempDir(), "git-t031")
	must(t, os.WriteFile(script, []byte(fmt.Sprintf("#!/bin/sh\nexec %s -c 'url.%s/.insteadOf=%s/' \"$@\"\n", gitBin, hostGit.URL, w.gitL.URL)), 0o755))
	gopts := gitad.Options{
		Binary: script, CAFile: w.ca.File, CommandTimeout: 30 * time.Second, MaxBundleBytes: 8 << 20, MaxScratchBytes: 64 << 20,
		ScratchRoot: filepath.Join(t.TempDir(), "scratch"),
	}
	cr := gitCreds{read: w.git.ReadToken, write: w.git.WriteToken}
	reader, err := gitad.NewReader(gopts, cr)
	must(t, err)
	pub, err := gitad.NewPublisher(gopts, cr)
	must(t, err)
	t.Cleanup(reader.Close)
	t.Cleanup(pub.Close)
	authz, items, repos, wsecrets := wst.NewAuthz(), wst.NewItems(), wst.NewRepos(), wst.NewSecrets()
	w.items = items
	authz.Grant(wst.Engineer.ID, scope)
	authz.Grant("execution", scope)
	repos.Set(scope, workspace.RepositoryConfig{ID: "repo_t031", URL: w.repoURL, CheckoutSecretRef: "secret://t031/checkout", PublisherSecretRef: "secret://t031/publisher", BaseRef: w.repo.BaseRef})
	execAttempts, err := execpg.NewAttemptAuthority(x.Store)
	must(t, err)
	bundles, err := resultsadapters.NewBundles(artStore, objects)
	must(t, err)
	guard, err := rpg.NewGuard(w.auth, w.ring.Generation, x.Clock.Now)
	must(t, err)
	wsStore, err := rpg.NewWorkspaceStore(x.Workspaces, x.Runtime, guard)
	must(t, err)
	wdeps := workspace.Dependencies{
		Store: wsStore, Tenants: tenantList{scope.TenantID}, Authorizer: authz, WorkItems: items, Repositories: repos,
		Attempts: execAttempts, Bundles: bundles, Secrets: wsecrets, Refs: reader, Source: reader, Publisher: pub, Clock: x.Clock.Now,
	}
	w.ws, err = workspace.NewService(wdeps, wst.DefaultOptions())
	must(t, err)
	plain := wdeps
	plain.Store = x.Workspaces
	wsPlain, err := workspace.NewService(plain, wst.DefaultOptions())
	must(t, err)
	sources, err := execadapters.NewSources(x.Workspaces, reader, wsPlain)
	must(t, err)

	// ---- agents: profiles, approvals, admission chain ----
	idr, err := identitypg.NewStore(x.Runtime, x.Events)
	must(t, err)
	std, err := identity.NewStandaloneAuthority(idr)
	must(t, err)
	agentAuth, err := agentsadapters.NewAuthority(std, idr)
	must(t, err)
	agentRepo, err := agentspg.NewStore(x.Runtime, agentspg.Options{})
	must(t, err)
	w.agentsSvc, err = agents.NewService(agents.ServiceConfig{
		Repository: agentRepo, Authority: agentAuth, Compatibility: w.drafts, Approvals: w.approvals,
		Clock: x.Clock.Now, NewID: func() (string, error) { return x.ID("ap"), nil },
	})
	must(t, err)
	resolver, err := agentsadapters.NewResolver(w.agentsSvc)
	must(t, err)
	w.chain, err = agentsadapters.NewChain(agentsadapters.ChainConfig{Resolver: resolver, Store: x.Store, Creator: x.Keyed, Planner: kube.NewPlanner(w.policy)})
	must(t, err)

	lookup, err := agentsadapters.NewLookup(w.agentsSvc)
	must(t, err)
	w.validator, err = pipeline.NewValidator(pipeline.Config{
		Capabilities: w.chain.Capabilities(pipeline.KindGate), Profiles: lookup, References: pipelinetest.References{Secrets: map[string]bool{}},
	})
	must(t, err)

	// ---- execution services ----
	w.svc = w.newExecution(w.chain.Store, sources, w.chain.Planner, w.chain.Hooks, w.chain.Capabilities(pipeline.KindGate))
	guarded, err := rpg.NewExecutionStore(x.Store, x.Runtime, guard)
	must(t, err)
	w.svcRes = w.newExecution(guarded, sources, kube.NewPlanner(w.policy), execution.EnabledKinds{pipeline.KindContainer}, pipeline.EnabledKinds{pipeline.KindContainer, pipeline.KindAgent, pipeline.KindGate})
	deps.Observations = w.svc
	deps.Tenants = tenantList{scope.TenantID}
	w.rec, err = kube.NewReconciler(deps, kopt)
	must(t, err)

	// ---- results ----
	execAd, err := resultsadapters.NewExecution(w.svcRes, x.Store)
	must(t, err)
	ev, err := resultsadapters.NewArtifacts(w.artSvc)
	must(t, err)
	wsSource, err := wsruntime.NewSource(w.ws)
	must(t, err)
	srcAd, err := resultsadapters.NewSource(wsSource)
	must(t, err)
	rAuth, err := resultsadapters.NewAuthority(w.verifier, w.ring, w.grants, x.Clock.Now)
	must(t, err)
	resSvc, err := results.NewService(results.Dependencies{Authority: rAuth, Contexts: execAd, Evidence: ev, Finalizer: execAd, Source: srcAd}, results.Options{})
	must(t, err)
	resH, err := resultshttp.NewHandler(resultshttp.Config{Service: resSvc})
	must(t, err)

	// ---- model gateway (real) over a deterministic upstream ----
	w.model = gatewaytest.NewProvider()
	t.Cleanup(w.model.Close)
	w.models = newModelScript(w.model)
	w.buildGateway()

	// ---- MCP proxy (real) over a real MCP server ----
	w.mcpfx = mcpfixture.New(t, mcpfixture.Options{})
	w.buildMCP()

	// ---- the platform listener ----
	runtimeH, err := runtimehttp.NewHandler(runtimehttp.Config{Service: w.rt})
	must(t, err)
	rtr, err := httpapi.NewRouter(httpapi.RouterConfig{Authenticator: rtAuth{w.verifier}, Authorizer: allowAll{}, RateLimiter: httpapi.Unlimited(), Prefix: "/runtime/v1"})
	must(t, err)
	artH, err := artifactshttp.NewRuntime(w.artSvc, artifactshttp.DefaultActions())
	must(t, err)
	must(t, artH.RegisterRoutes(rtr))
	gwH, err := gatewayhttp.NewHandler(gatewayhttp.Config{Service: w.gwSvc})
	must(t, err)
	mcpH, err := mcpprotocol.NewHandler(mcpprotocol.ServerConfig{Runtime: w.mcpSvc, IdleTimeout: time.Hour, MaxRequestBytes: 1 << 20})
	must(t, err)
	mux := http.NewServeMux()
	mux.Handle("POST /runtime/v1/attempts/{attemptId}/result", resH)
	mux.Handle("POST /runtime/v1/attempts/{attemptId}/source", resH)
	mux.Handle("POST /runtime/v1/attempts/{attemptId}/artifacts", rtr)
	mux.Handle("POST /runtime/v1/attempts/{attemptId}/artifacts/{artifactId}/complete", rtr)
	mux.Handle("/runtime/v1/", w.captureTokens(runtimeH))
	mux.Handle("/gateway/v1/", gwH.Workload())
	mux.Handle("/internal/mcp/v1/", mcpH)
	var h http.Handler = mux
	late.h.Store(&h)

	t.Cleanup(w.mcpSvc.Shutdown)
	return w
}

func b64(s string) string {
	const tbl = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	in := []byte(s)
	var out []byte
	for i := 0; i < len(in); i += 3 {
		var v uint32
		n := 0
		for j := 0; j < 3; j++ {
			v <<= 8
			if i+j < len(in) {
				v |= uint32(in[i+j])
				n++
			}
		}
		for j := 0; j < 4; j++ {
			if j <= n {
				out = append(out, tbl[(v>>(18-6*uint(j)))&63])
			} else {
				out = append(out, '=')
			}
		}
	}
	return string(out)
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// newExecution builds an execution service over the shared store and executor.
func (w *world) newExecution(store execution.Store, sources execution.Sources, planner execution.Planner, hooks execution.Hooks, caps pipeline.Capabilities) *execution.Service {
	w.t.Helper()
	x := w.x
	reader, err := identitypg.NewStore(x.Runtime, x.Events)
	must(w.t, err)
	std, err := identity.NewStandaloneAuthority(reader)
	must(w.t, err)
	authority, err := execadapters.NewAuthority(std, reader)
	must(w.t, err)
	resolver, err := secrets.NewConfigurationResolver(x.Secrets, x.Secrets, nil)
	must(w.t, err)
	cfg, err := execadapters.NewConfiguration(resolver)
	must(w.t, err)
	defs, err := execadapters.NewDefinitions(x.Pipeline)
	must(w.t, err)
	svc, err := execution.NewService(execution.Dependencies{
		Store: store, Authority: authority, Definitions: defs, Capabilities: caps,
		Work: sqlWork{x}, Configuration: cfg, Sources: sources, Limits: x.Limits, Planner: planner,
		Hooks: hooks, Tenants: tenantList{scope.TenantID},
		Runtime: w.runtime, EffectReconciler: x.Effects, Clock: x.Clock.Now, Cleanup: allowCleanup{},
	}, execution.Options{ClaimLease: 2 * time.Second, Batch: 10})
	must(w.t, err)
	return svc
}

type allowCleanup struct{}

func (allowCleanup) AuthorizeCleanup(context.Context, domain.Scope, string) error { return nil }

type sqlWork struct{ e *executiontest.Env }

func (s sqlWork) Snapshot(ctx context.Context, sc domain.Scope, id domain.WorkItemID) (execution.WorkSnapshot, error) {
	var kind, state, spec string
	var rev int64
	err := s.e.Migration.QueryRow(ctx, `SELECT kind, state, spec, content_revision FROM work_items WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
		string(sc.TenantID), string(sc.ProjectID), string(id)).Scan(&kind, &state, &spec, &rev)
	if err != nil {
		return execution.WorkSnapshot{}, execution.ErrNotFound
	}
	return execution.WorkSnapshot{
		ID: id, Actionable: kind != "epic", Cancelled: state == "cancelled", ContentRevision: domain.ContentRevision(rev), Spec: spec,
		Criteria: []runtimev1.AcceptanceCriterion{{ID: "AC-1", Text: "hello.txt exists and says hello"}},
	}, nil
}

// provision creates the namespace baseline the executor requires (Pod Security
// restricted, tenant annotation, token-less service account, quota, default-deny
// and runtime-egress network policies).
func (w *world) provision(ns, tenant string) {
	t := w.t
	items := []map[string]any{
		{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{
			"name":        ns,
			"labels":      map[string]string{"pod-security.kubernetes.io/enforce": "restricted", "pod-security.kubernetes.io/enforce-version": "latest", "io.kodama.t031/fixture": "true"},
			"annotations": map[string]string{kube.AnnNamespaceTenant: tenant},
		}},
		{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": map[string]any{"name": "t031-runner", "namespace": ns}, "automountServiceAccountToken": false},
		{"apiVersion": "v1", "kind": "ResourceQuota", "metadata": map[string]any{"name": "compute", "namespace": ns}, "spec": map[string]any{
			"hard": map[string]string{"pods": "20", "limits.cpu": "16", "limits.memory": "16Gi", "limits.ephemeral-storage": "20Gi"},
		}},
	}
	var egress []map[string]any
	for _, r := range w.policy.RuntimeEgress() {
		var ports []map[string]any
		for _, pt := range r.Ports {
			ports = append(ports, map[string]any{"protocol": r.Protocol, "port": pt})
		}
		egress = append(egress, map[string]any{"to": []map[string]any{{"ipBlock": map[string]any{"cidr": r.CIDR}}}, "ports": ports})
	}
	items = append(items,
		map[string]any{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": map[string]any{"name": "default-deny", "namespace": ns},
			"spec": map[string]any{"podSelector": map[string]any{}, "policyTypes": []string{"Ingress", "Egress"}}},
		map[string]any{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": map[string]any{"name": "runtime-egress", "namespace": ns},
			"spec": map[string]any{"podSelector": map[string]any{"matchLabels": map[string]string{kube.LabelManagedBy: kube.ValueManagedBy}}, "policyTypes": []string{"Egress"}, "egress": egress}},
	)
	doc, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": items})
	cmd := exec.Command("kubectl", "--kubeconfig", w.c.kubeconfig, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(string(doc))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("provision %s: %v: %s", ns, err, out)
	}
}

// ---- gateway ---------------------------------------------------------------

func (w *world) buildGateway() {
	t, x := w.t, w.x
	dir := t.TempDir()
	p := filepath.Join(dir, "master-key-v1")
	must(t, os.WriteFile(p, make([]byte, keyfile.KeySize), 0o600))
	prov, err := keyfile.New(keyfile.Config{Development: true, Current: "v1", Files: map[string]string{"v1": p}})
	must(t, err)
	w.wrapper = prov
	w.secretSv, err = secrets.NewService(secrets.ServiceConfig{InstallationID: installation, Store: x.Secrets, Wrapper: prov, IDs: secrets.RandomIDs{}})
	must(t, err)
	roots := x509.NewCertPool()
	roots.AddCert(w.model.Srv.Certificate())
	policy, err := openai.NewPolicy(openai.PolicyConfig{
		Origins:   []openai.ApprovedOrigin{{Origin: w.model.Origin(), PathPrefix: "/v1"}},
		Addresses: openai.AllowPrefixes(openai.PublicOnly, netip.MustParsePrefix("127.0.0.0/8")),
	})
	must(t, err)
	txAuth, err := gwadapters.NewTxAuthority(w.auth, w.ring.Generation)
	must(t, err)
	pins, err := gwadapters.NewPins(x.Secrets)
	must(t, err)
	store, err := gatewaypg.NewStore(gatewaypg.Config{
		DB: x.Runtime, Events: gwadapters.EventsFrom(x.Events), Workload: txAuth, Management: gwadapters.BindManagement, Pins: pins, Clock: x.Clock.Now,
	})
	must(t, err)
	w.gwStore = store
	idr, err := identitypg.NewStore(x.Runtime, x.Events)
	must(t, err)
	std, err := identity.NewStandaloneAuthority(idr)
	must(t, err)
	authority, err := gwadapters.NewAuthority(std)
	must(t, err)
	workloads, err := gwadapters.NewRuntimeWorkloads(w.rt)
	must(t, err)
	lim := openai.DefaultStreamLimits()
	lim.IdleTimeout = 2 * time.Minute
	client, err := openai.NewClient(openai.Config{
		Policy: policy, TLS: &tls.Config{RootCAs: roots}, Stream: lim,
		Timeouts: openai.Timeouts{Connect: 5 * time.Second, Headers: 30 * time.Second, Total: 5 * time.Minute},
	})
	must(t, err)
	src, err := secretspg.NewVersionSource(x.Secrets, prov, installation)
	must(t, err)
	creds, err := gwadapters.NewCredentials(src, x.Secrets)
	must(t, err)
	// Conservative accounting for the fixture model; the accounting is a gateway
	// input, not evidence about any model.
	est, err := openai.NewEstimator(map[string]openai.ModelAccounting{modelName: openai.ConservativeAccounting})
	must(t, err)
	w.gwSvc, err = gateway.NewService(gateway.Config{
		Store: store, Workloads: workloads, Authority: authority, Credentials: creds, Provider: client,
		Estimator: est, Decoder: openai.NewDecoder(), Destinations: policy,
		Timeouts: gateway.Timeouts{Request: 5 * time.Minute, Recheck: 500 * time.Millisecond, Finalize: 10 * time.Second, FinalizeAttempts: 4, FinalizeBackoff: 20 * time.Millisecond, Record: 5 * time.Second},
		Clock:    x.Clock.Now, Tenants: tenantList{scope.TenantID},
	})
	must(t, err)

	// Project settings: the fixture upstream, an exact-version credential, the
	// fixture model, and limits that fit OpenCode's requests.
	m := secrets.Mutation{Actor: secrets.Actor{Type: secrets.ActorUser, ID: admin}, RequestID: "req_t031", Now: time.Now().UTC()}
	payload, err := json.Marshal(gwadapters.ModelCredentialPayload{Version: 1, Bearer: gwBearer, Origin: w.model.Origin()})
	must(t, err)
	prep, err := w.secretSv.PrepareSecret(context.Background(), m, scope, gwSecret, secrets.KindModel, payload)
	must(t, err)
	must(t, w.secretSv.Atomically(context.Background(), scope.TenantID, func(ctx context.Context, wr secrets.Writer) error {
		_, err := w.secretSv.CreateSecret(ctx, wr, m, prep)
		return err
	}))
	w.configureGateway(gateway.Limits{MaxRequests: 40, TokenAllowance: 4_000_000, MaxRequestBytes: 3 << 20, MaxInputTokens: 600_000, MaxOutputTokens: 8192})
}

func (w *world) configureGateway(l gateway.Limits) gateway.ModelSettings {
	w.t.Helper()
	ctx := context.Background()
	cur, err := w.gwSvc.GetSettings(ctx, gateway.Actor{ID: admin}, scope)
	must(w.t, err)
	res, err := w.gwSvc.ReplaceSettings(ctx, gateway.Actor{ID: admin}, scope, cur.Revision, gateway.SettingsEdit{
		Enabled: true, Endpoint: w.model.Endpoint(), Credential: gateway.CredentialRef{Ref: "secret://project/" + gwSecret, Version: 1},
		Models: []string{modelName}, Limits: l,
	}, "req_t031")
	must(w.t, err)
	return res.Settings
}

// ---- MCP proxy -------------------------------------------------------------

func (w *world) buildMCP() {
	t, x := w.t, w.x
	mcpAuth, err := mcpadapters.NewRuntimeAuthority(w.rt, w.auth, w.ring.Generation)
	must(t, err)
	pins, err := mcpadapters.Pins(x.Secrets)
	must(t, err)
	store, err := mcppg.NewStore(x.Runtime, mcppg.Options{Recheck: mcpAuth.Recheck, Pin: pins})
	must(t, err)
	versions, err := secretspg.NewVersionSource(x.Secrets, w.wrapper, installation)
	must(t, err)
	creds, err := mcpadapters.NewCredentials(versions)
	must(t, err)
	dest, err := mcpprotocol.NewDestinations(mcpprotocol.DestinationConfig{Allowed: []string{w.mcpfx.HostPort()}, AllowPrivate: true})
	must(t, err)
	remote, err := mcpprotocol.NewRemote(mcpprotocol.RemoteConfig{Destinations: dest, RootCAs: w.mcpfx.Roots(), ConnectTimeout: 5 * time.Second, MaxResultBytes: 1 << 20})
	must(t, err)
	steps, err := mcpadapters.NewExecutionSteps(x.Store)
	must(t, err)
	srcAt, err := agentsadapters.NewAttemptSource(x.Store)
	must(t, err)
	reader, err := agentsadapters.NewPolicyReader(srcAt)
	must(t, err)
	policy, err := mcpadapters.NewPolicy(steps, reader)
	must(t, err)
	w.mcpSvc, err = mcpproxy.NewService(mcpproxy.Config{
		Store: store, Human: w.humans, Runtime: mcpAuth, Policy: policy, Credentials: creds, Remote: remote,
		Destinations: dest, Secrets: store, Options: mcptest.DefaultOptions("inc-t031-a"), Clock: x.Clock.Now,
	})
	must(t, err)

	// One registration of the MCP server with an exact-version recipient-bound
	// credential, enabling echo, mutate and nothing else; admin_secret stays
	// unregistered ("hidden").
	m := secrets.Mutation{Actor: secrets.Actor{Type: secrets.ActorUser, ID: admin}, RequestID: "req_t031", Now: time.Now().UTC()}
	prep, err := w.secretSv.PrepareSecret(context.Background(), m, scope, "remote-token", secrets.KindMCP, mcpadapters.EncodeCredential(w.mcpfx.Origin(), w.mcpfx.Token()))
	must(t, err)
	must(t, w.secretSv.Atomically(context.Background(), scope.TenantID, func(ctx context.Context, wr secrets.Writer) error {
		_, err := w.secretSv.CreateSecret(ctx, wr, m, prep)
		return err
	}))
	w.humans.Grant(admin, scope, mcpproxy.PermList, mcpproxy.PermMutate)
	w.mcpReg, err = w.mcpSvc.Create(context.Background(), scope, mcpproxy.ManageCaller{PrincipalID: admin, RequestID: "req_t031"},
		mcpproxy.Draft{Endpoint: w.mcpfx.URL(), Secret: mcpproxy.SecretRef{Name: "remote-token", Version: 1}, Tools: []string{"echo", "mutate", "slow"}})
	must(t, err)
}

// ---- profiles and runs -------------------------------------------------------

// pin is an immutable design baseline built from references at a commit.
func pin(t testing.TB, repository, commit string, requirements, checks []string) runtimev1.DesignBaseline {
	t.Helper()
	b, err := runtimev1.NewDesignBaseline(requirements, []string{"C-15", "O-04"}, []runtimev1.SourceRef{
		{Repository: repository, Revision: runtimev1.GitCommit(commit), Path: baselineDoc},
	}, checks)
	must(t, err)
	return b
}

type profileOpts struct {
	Name         string
	Instructions string
	Baseline     runtimev1.DesignBaseline
	Tools        []string
	Record       bool
	WallSeconds  int64
	// HarnessVersion overrides the pinned harness version (an unqualified tuple).
	HarnessVersion string
}

// register registers one profile version for the wrapper image under test with
// a catalogue entry that carries this task's evidence, and returns its exact
// reference.
func (w *world) register(o profileOpts) agents.ProfileVersion {
	w.t.Helper()
	p, err := w.tryRegister(o)
	if err != nil {
		w.t.Fatalf("register profile: %v", err)
	}
	return p
}

func (w *world) tryRegister(o profileOpts) (agents.ProfileVersion, error) {
	w.t.Helper()
	if o.HarnessVersion == "" {
		o.HarnessVersion = wspec.HarnessVersion
	}
	req, tok, calls := int64(40), int64(4_000_000), int64(50)
	if o.WallSeconds == 0 {
		o.WallSeconds = 900
	}
	d := agents.Draft{
		Name:  o.Name,
		Image: w.c.wrapper,
		Wrapper: agents.Wrapper{ID: wspec.WrapperID, Version: wspec.WrapperVersion, Command: []string{wspec.WrapperPath},
			Harness: agents.Harness{ID: wspec.HarnessID, Version: o.HarnessVersion}},
		Instructions:   o.Instructions,
		DesignBaseline: o.Baseline,
		Model:          agents.Model{Name: modelName, Endpoint: w.model.Endpoint()},
		Tools:          o.Tools,
		Limits:         agents.Limits{WallSeconds: o.WallSeconds, MaxRequests: &req, MaxTokens: &tok, MaxToolCalls: &calls},
		Transcript:     agents.Transcript{Record: o.Record},
	}
	w.approvals.model = modelName
	w.approvals.endpoint = w.model.Endpoint()
	if len(o.Tools) > 0 {
		w.approvals.allow(o.Tools...)
	}
	w.drafts.add(agents.CatalogueEntry{
		Tuple: agents.Tuple{Image: d.Image, WrapperID: wspec.WrapperID, WrapperVersion: wspec.WrapperVersion, Command: []string{wspec.WrapperPath},
			HarnessID: wspec.HarnessID, HarnessVersion: wspec.HarnessVersion},
		// Only controls this composition enforces: the model allow-list and
		// request/token caps are the gateway's, the tool allow-list the MCP proxy's.
		// tool_calls and transcript are claimed only where the test proves them.
		Controls: agents.Controls(),
		Evidence: "t031-composition:" + w.c.build.PushedDigest,
	})
	res, err := w.agentsSvc.Register(context.Background(), agents.RegisterCommand{
		Scope: scope, Caller: agents.Caller{PrincipalID: executiontest.Engineer, RequestID: "req_t031"}, Draft: d,
	})
	if err != nil {
		return agents.ProfileVersion{}, err
	}
	return res.Profile, nil
}

// stepYAML is the single agent step. The platform origin is the one thing the
// wrapper needs that the entrypoint does not export; the step supplies it as
// non-secret environment.
func (w *world) pipelineYAML(name, profileRef string, record bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: kodama/v1alpha1\nkind: Pipeline\nmetadata: {name: %s}\nspec:\n  inputs: {}\n  steps:\n", name)
	fmt.Fprintf(&b, "    - id: implement\n      kind: agent\n      agent: {profile: %s}\n      workspace: {access: write, from: run}\n      timeoutSeconds: 900\n", profileRef)
	b.WriteString("      outputs:\n")
	fmt.Fprintf(&b, "        %s: {type: artifact, path: %s}\n", wspec.ConformanceOutput, wspec.DeclaredOutputPath(wspec.ConformanceFile))
	if record {
		fmt.Fprintf(&b, "        %s: {type: artifact, path: %s}\n", wspec.TranscriptOutput, wspec.DeclaredOutputPath(wspec.TranscriptFile))
	}
	return b.String()
}

type runHandle struct {
	Run    *execution.Run
	Intent *execution.AttemptSpec
	Item   domain.WorkItemID
}

// submit admits a run of the pipeline for a fresh work item with a provisioned
// workspace on the real Git remote, then claims and dispatches the step.
func (w *world) submit(p agents.ProfileVersion, record bool) *runHandle {
	t := w.t
	t.Helper()
	ctx := context.Background()
	n := w.ids.Add(1)
	item := w.x.WorkItem(scope, w.x.ID("wi"), "# Task\nCreate hello.txt containing the word hello and commit it.")
	w.items.Add(scope, item, "story")
	ws, _, err := w.ws.Provision(ctx, wst.Engineer, scope, item)
	must(t, err)
	if ws.State != workspace.StateAvailable {
		t.Fatalf("workspace %s (%s)", ws.State, ws.LastError)
	}
	w.workspaceID = string(ws.ID)
	def := w.define(w.pipelineYAML(fmt.Sprintf("t031-%d", n), p.Ref(), record))
	rev := w.x.ContentRevision(executiontest.Project{Scope: scope, Item: item})
	run, err := w.svc.Admit(ctx, execution.Caller{PrincipalID: executiontest.Engineer, RequestID: "req_t031"}, execution.AdmitCommand{
		Scope: scope, WorkItemID: item, DefinitionID: def.ID, ContentRevision: rev, SourceCommit: w.repo.Initial,
	})
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	in, err := w.svc.ClaimRunnable(ctx, scope.TenantID)
	if err != nil || in == nil {
		t.Fatalf("claim: %v %v", in, err)
	}
	return &runHandle{Run: run, Intent: in, Item: item}
}

// tick is one iteration of the periodic loops the composition root schedules.
func (w *world) tick() {
	ctx := context.Background()
	if _, err := w.svc.Reconcile(ctx); err != nil {
		w.t.Logf("execution reconcile: %v", err)
	}
	if _, err := w.rec.Pass(ctx); err != nil {
		w.t.Logf("kube pass: %v", err)
	}
}

func (w *world) attempt(h *runHandle) *execution.Attempt {
	agg, err := w.x.Store.Get(context.Background(), scope, h.Run.ID)
	must(w.t, err)
	return agg.Attempt(h.Intent.AttemptID)
}

func (w *world) aggregate(h *runHandle) *execution.Aggregate {
	agg, err := w.x.Store.Get(context.Background(), scope, h.Run.ID)
	must(w.t, err)
	return agg
}

func (w *world) terminal(h *runHandle) bool {
	switch w.attempt(h).Status {
	case execution.AttemptSucceeded, execution.AttemptFailed, execution.AttemptCancelled:
		return true
	}
	return false
}

// await ticks until the attempt is terminal and returns it.
func (w *world) await(h *runHandle, timeout time.Duration) *execution.Attempt {
	w.t.Helper()
	deadline := time.Now().Add(timeout)
	for !w.terminal(h) {
		if time.Now().After(deadline) {
			w.t.Fatalf("attempt %s is %s after %s\n%s", h.Intent.AttemptID, w.attempt(h).Status, timeout, w.podLogs(h))
		}
		w.tick()
		time.Sleep(time.Second)
	}
	return w.attempt(h)
}

func (w *world) podLogs(h *runHandle) string {
	out, _ := run("kubectl", "--kubeconfig", w.c.kubeconfig, "-n", w.nsA, "logs", "-l", kube.LabelJob+"="+h.Intent.WorkloadName, "--tail=60", "--all-containers=true")
	return out
}

// untilCond ticks the loops until cond holds.
func (w *world) until(timeout time.Duration, what string, cond func() bool) {
	w.t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			w.t.Fatalf("timed out waiting for %s", what)
		}
		w.tick()
		time.Sleep(500 * time.Millisecond)
	}
}

func (w *world) count(sql string, args ...any) int { return w.x.Count(sql, args...) }

func (w *world) one(dst any, sql string, args ...any) { w.x.Scalar(dst, sql, args...) }

// tenantTx runs fn as the runtime role inside the tenant.
func (w *world) tenantTx(fn func(ctx context.Context, tx pgx.Tx) error) error {
	return storage.WithTenant(context.Background(), w.x.Runtime, scope.TenantID, fn)
}

// callTool calls an MCP tool of the registration through the real proxy using
// the attempt's own runtime token, as the workload could.
func (w *world) callTool(token string, attempt domain.AttemptID, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "t031-direct", Version: "1"}, nil)
	ep := "https://127.0.0.1:" + fmt.Sprint(w.plat.Port) + "/internal/mcp/v1/attempts/" + string(attempt) + "/servers/" + string(w.mcpReg.ID)
	hc := &http.Client{Transport: bearerTransport{token: token, base: hostTransport(w.ca, w.c.hostIP)}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ep, HTTPClient: hc, MaxRetries: -1, DisableStandaloneSSE: true}, nil)
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	return sess.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
}

func (w *world) caPool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AppendCertsFromPEM(w.ca.PEM)
	return p
}

type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

var _ = net.JoinHostPort

// captureTokens records every runtime token the admission and renewal routes
// issue, so a test can prove none of them reached a retained artifact.
func (w *world) captureTokens(inner http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: rw, status: 200}
		inner.ServeHTTP(rec, r)
		if rec.status != 200 || r.Method != http.MethodPost {
			return
		}
		var doc struct {
			Token struct {
				Token string `json:"token"`
			} `json:"token"`
		}
		if json.Unmarshal(rec.body.Bytes(), &doc) != nil || doc.Token.Token == "" {
			return
		}
		attempt := domain.AttemptID(strings.Split(strings.TrimPrefix(r.URL.Path, "/runtime/v1/attempts/"), "/")[0])
		w.tokMu.Lock()
		w.tokens[attempt] = append(w.tokens[attempt], doc.Token.Token)
		w.tokMu.Unlock()
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (r *statusRecorder) WriteHeader(c int) { r.status = c; r.ResponseWriter.WriteHeader(c) }
func (r *statusRecorder) Write(b []byte) (int, error) {
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// runtimeTokens returns every token the attempt was issued.
func (w *world) runtimeTokens(a domain.AttemptID) []string {
	w.tokMu.Lock()
	defer w.tokMu.Unlock()
	return append([]string(nil), w.tokens[a]...)
}

// artifact reads a completed artifact's bytes through the artifact service's
// own authorized download instruction, as a human reader would.
func (w *world) artifact(id domain.ArtifactID) (artifacts.Artifact, []byte) {
	w.t.Helper()
	v, err := w.artSvc.Metadata(context.Background(), artifacts.Caller{Subject: "user:t031"}, scope, id, true)
	must(w.t, err)
	if v.Download == nil {
		w.t.Fatalf("artifact %s has no download", id)
	}
	req, err := http.NewRequest(v.Download.Method, v.Download.URL, nil)
	must(w.t, err)
	hc := &http.Client{Timeout: 30 * time.Second, Transport: hostTransport(w.ca, w.c.hostIP)}
	resp, err := hc.Do(req)
	must(w.t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	must(w.t, err)
	if resp.StatusCode != 200 {
		w.t.Fatalf("download %s: %d %s", id, resp.StatusCode, b)
	}
	if got := string(artifacts.DigestOf(b)); got != string(v.Artifact.Digest()) {
		w.t.Fatalf("artifact %s digest %s does not match its bytes (%s)", id, v.Artifact.Digest(), got)
	}
	return v.Artifact, b
}

// diagnostics returns the retained diagnostic artifacts of the attempt by name.
func (w *world) diagnostics(at *execution.Attempt) map[string][]byte {
	w.t.Helper()
	out := map[string][]byte{}
	if at.Result == nil {
		return out
	}
	for _, id := range at.Result.DiagnosticArtifactIDs {
		a, b := w.artifact(id)
		out[a.Name()] = b
	}
	return out
}

// approvals is the project's approved model and tool policy for the profile
// service: the deterministic fixture model at the fixture endpoint, and the
// MCP tools a test registered. (The hosted approval source is T-039's.)
type approvals struct {
	mu       sync.Mutex
	model    string
	endpoint string
	tools    map[string]bool
}

func (a *approvals) allow(tools ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, t := range tools {
		a.tools[t] = true
	}
}

func (a *approvals) ApproveModel(_ context.Context, _ domain.Scope, m agents.Model) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if m.Name != a.model || m.Endpoint != a.endpoint {
		return agents.ErrDenied
	}
	return nil
}

func (a *approvals) ApproveTools(_ context.Context, _ domain.Scope, tools []string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, t := range tools {
		if !a.tools[t] {
			return agents.ErrDenied
		}
	}
	return nil
}

// define validates a pipeline with agent steps enabled (profile references must
// resolve through the real profile service) and registers it.
func (w *world) define(yaml string) pipeline.Definition {
	w.t.Helper()
	actor := pipeline.Actor{ID: executiontest.Engineer, Type: pipeline.ActorUser}
	v, err := w.validator.ValidateYAML(context.Background(), scope, actor, []byte(yaml))
	if err != nil {
		w.t.Fatalf("validate definition: %v", err)
	}
	def, _, err := w.x.Pipeline.RegisterOrGet(context.Background(), pipeline.Candidate{
		Scope: scope, ID: domain.DefinitionID(w.x.ID("pd")), Content: v, CreatedBy: actor, CreatedAt: w.x.Clock.Now(),
	})
	must(w.t, err)
	return def
}
