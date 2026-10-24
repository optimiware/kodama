package logs_test

// The composed fixture: real PostgreSQL with every dependent schema (execution,
// artifact, runtime-grant and log tables, applied in the merged order under a
// separate owner role and non-owner runtime roles), the runtime-enabled
// execution store with the guarded result path, the real runtime admission,
// results and artifact services and HTTP handlers behind a TLS endpoint Pods
// can reach, real S3 for artifact bytes, the real T-021 executor, observer and
// reconciler against the kind cluster, and the actual entrypoint (`kodama step`,
// with the T-078 container-log mirror) running in the Pods. The log service,
// controller, Kubernetes reader and HTTP route are the merged T-023 constructors
// composed locally (product root wiring is T-071's).
//
// Test-local adapters (this file and logs_test.go) implement only existing
// ports where composition owns the answer: per-tenant namespace placement and the
// delivered-value redaction source. Authorization is never replaced: standalone
// membership, runtime sessions and scoped persistence are the real ones.
//
// Resource prefix t064.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
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
	"github.com/optimiware/kodama/internal/httpapi"
	httppg "github.com/optimiware/kodama/internal/httpapi/postgres"
	"github.com/optimiware/kodama/internal/identity"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
	"github.com/optimiware/kodama/internal/kube"
	kubeadapters "github.com/optimiware/kodama/internal/kube/adapters"
	logspg "github.com/optimiware/kodama/internal/logs/postgres"
	"github.com/optimiware/kodama/internal/pipeline"
	pipelinepg "github.com/optimiware/kodama/internal/pipeline/postgres"
	"github.com/optimiware/kodama/internal/results"
	resultsadapters "github.com/optimiware/kodama/internal/results/adapters"
	resultshttp "github.com/optimiware/kodama/internal/results/http"
	rpg "github.com/optimiware/kodama/internal/results/postgres"
	"github.com/optimiware/kodama/internal/runtimeauth"
	runtimehttp "github.com/optimiware/kodama/internal/runtimeauth/http"
	rtjwt "github.com/optimiware/kodama/internal/runtimeauth/jwt"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
	"github.com/optimiware/kodama/internal/secrets"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
	"github.com/optimiware/kodama/internal/storage"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
	workspacepg "github.com/optimiware/kodama/internal/workspace/postgres"
	s3support "github.com/optimiware/kodama/test/support/s3"
)

const issuer = "https://runtime.t064.invalid"

// startErr keeps only the error of a Start.
func startErr(_ execution.Workload, err error) error { return err }

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type tenantIDs []domain.TenantID

func (t tenantIDs) TenantIDs(context.Context) ([]domain.TenantID, error) { return t, nil }

type allowCleanup struct{}

func (allowCleanup) AuthorizeCleanup(context.Context, domain.Scope, string) error { return nil }

type noSource struct{}

func (noSource) Propose(context.Context, domain.Scope, domain.AttemptID, runtimev1.SourceProposalRequest) (results.SourceOutcome, error) {
	return results.SourceOutcome{}, fmt.Errorf("t064 runs are nonwriter attempts without source publication")
}

type sqlWork struct{ e *executiontest.Env }

func (w sqlWork) Snapshot(ctx context.Context, scope domain.Scope, id domain.WorkItemID) (execution.WorkSnapshot, error) {
	var kind, state, spec string
	var rev int64
	err := w.e.Migration.QueryRow(ctx, `SELECT kind, state, spec, content_revision FROM work_items WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
		string(scope.TenantID), string(scope.ProjectID), string(id)).Scan(&kind, &state, &spec, &rev)
	if err != nil {
		return execution.WorkSnapshot{}, execution.ErrNotFound
	}
	return execution.WorkSnapshot{
		ID: id, Actionable: kind != "epic", Cancelled: state == "cancelled", ContentRevision: domain.ContentRevision(rev), Spec: spec,
		Criteria: []runtimev1.AcceptanceCriterion{{ID: "AC-1", Text: "works"}},
	}, nil
}

// stack is one isolated composition (two tenant namespaces, one database, one S3
// subprefix).
type stack struct {
	t        testing.TB
	c        *cluster
	x        *executiontest.Env
	nsA, nsB string
	policy   kube.ExecutionPolicy

	rt       *execpg.Store // runtime-enabled execution store (grant revocation)
	attempts *kubeadapters.Attempts
	auth     *kubeadapters.Authority
	observer *kube.JobObserver
	runtime  *kube.Runtime
	rec      *kube.Reconciler
	plain    *execution.Service // trusted paths: admit, dispatch, cancel, reconcile
	guarded  *execution.Service // the result path
	commits  *countingCommitter

	ring     *rtjwt.KeyRing
	verifier *rtjwt.Verifier
	grants   *rtpg.Store
	registry *runtimeauth.Service
	srv      *runtimeServer

	s3      *s3support.Env
	objects *arts3.Store
	artRepo *artifactspg.Store
	artSvc  *artifacts.Service

	vault     *vault
	extraMu   sync.Mutex
	extra     map[domain.AttemptID][][]byte // values delivered to an attempt beyond its credentials
	sealed    map[string]bool               // secrets sealed by this stack
	validator *pipeline.Validator

	ids atomic.Int64
	kit *logsKit
}

type countingCommitter struct {
	inner *execution.Service
	n     atomic.Int64
}

func (c *countingCommitter) CommitResult(ctx context.Context, in execution.ResultCommit) (execution.ResultAck, error) {
	c.n.Add(1)
	return c.inner.CommitResult(ctx, in)
}

type stackOptions struct {
	// CompletedRetention keeps settled Jobs (default one hour) so a retained read
	// and a live read can be compared; tests that remove the workload do so
	// themselves.
	CompletedRetention time.Duration
}

func newStack(t *testing.T, o ...stackOptions) *stack {
	t.Helper()
	var opt stackOptions
	if len(o) > 0 {
		opt = o[0]
	}
	if opt.CompletedRetention == 0 {
		opt.CompletedRetention = time.Hour
	}
	c := requireCluster(t)
	x := executiontest.New(t)
	x.GrantAll()
	s := &stack{t: t, c: c, x: x}
	ctx := context.Background()

	// Quotas are not under test here: many attempts stay around across subtests.
	lim := executiontest.DefaultLimit()
	lim.TenantConcurrency, lim.ProjectConcurrency = 200, 100
	lim.StartupBudget = 5 * time.Minute // a shared, loaded host can take minutes to start a Pod
	x.Limits.Set(lim)

	// The merged migration order: execution's contribution is already applied;
	// runtime grants and logs append after it.
	contribs := []storage.Contribution{
		storage.BaseContribution(), eventspg.Migrations(), httppg.Migrations(), identitypg.Migrations(), workpg.Migrations(),
		secretspg.Migrations(), pipelinepg.Migrations(), workspacepg.Migrations(), artifactspg.Migrations(),
	}
	contribs = append(contribs, execpg.Migrations()...)
	contribs = append(contribs, rtpg.Migrations(), logspg.Migrations())
	manifest, err := storage.NewManifest(contribs...)
	must(t, err)
	mctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	_, err = storage.Migrate(mctx, x.Migration, manifest, storage.MigrateOptions{RuntimeRole: x.Base.DB.RuntimeRole})
	must(t, err)

	suffix := randHex(3)
	s.nsA, s.nsB = Prefix+"-"+suffix+"-a", Prefix+"-"+suffix+"-b"
	s.srv = newRuntimeServer(t, c.hostIP)
	port := s.srv.ln.Addr().(*net.TCPAddr).Port
	cfg := kube.PolicyConfig{
		Tenants: map[domain.TenantID]kube.TenantTarget{
			executiontest.ScopeA.TenantID: {Namespace: s.nsA, ServiceAccount: Prefix + "-runner"},
			executiontest.ScopeB.TenantID: {Namespace: s.nsB, ServiceAccount: Prefix + "-runner"},
		},
		InjectorImage:   c.injector,
		InjectorCommand: []string{"cp", "/usr/local/bin/kodama", "/kodama/bin/kodama"},
		RuntimeURL:      s.srv.URL, RuntimeCA: s.srv.CA,
		RuntimeEgress: []kube.EgressRule{{CIDR: c.hostIP + "/32", Protocol: "TCP", Ports: []int32{int32(port)}}},
		Workload:      kube.ResourceBounds{CPURequest: "50m", CPULimit: "500m", MemoryRequest: "32Mi", MemoryLimit: "256Mi", EphemeralRequest: "32Mi", EphemeralLimit: "512Mi"},
		Injector:      kube.ResourceBounds{CPURequest: "10m", CPULimit: "100m", MemoryRequest: "8Mi", MemoryLimit: "64Mi", EphemeralRequest: "16Mi", EphemeralLimit: "128Mi"},
		Volumes:       kube.VolumeSizes{Workspace: "64Mi", Outputs: "32Mi", Scratch: "64Mi", Results: "1Mi", Tools: "64Mi", Secrets: "4Mi", Identity: "1Mi"},
		RunAsUser:     65532, RunAsGroup: 65532, TerminationGrace: 20 * time.Second,
	}
	s.policy, err = kube.NewExecutionPolicy(cfg)
	must(t, err)
	s.provision(s.nsA, string(executiontest.ScopeA.TenantID))
	s.provision(s.nsB, string(executiontest.ScopeB.TenantID))
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, ns := range []string{s.nsA, s.nsB} {
			_, _, _ = c.api.do(cctx, http.MethodDelete, "/api/v1/namespaces/"+ns, nil)
		}
	})

	// ---- execution store with grant revocation, and the kube adapters over it ----
	s.rt, err = execpg.NewRuntimeStore(x.Runtime, x.Events, execpg.Peers{
		Authority: execadapters.BindAuthority, Workspaces: x.Workspaces, Secrets: x.Secrets,
	}, execadapters.RevokeAttemptGrants, execpg.Options{Clock: x.Clock.Now})
	must(t, err)
	s.attempts, err = kubeadapters.NewAttempts(s.rt)
	must(t, err)
	s.auth, err = kubeadapters.NewAuthority(s.rt, s.policy)
	must(t, err)
	kopt := kube.Options{Clock: x.Clock.Now, CreateQuiescence: 3 * time.Second, OrphanGrace: 2 * time.Second, PreflightTTL: time.Second, CompletedRetention: opt.CompletedRetention}
	deps := kube.Dependencies{Policy: s.policy, Cluster: c.client, Attempts: s.attempts}
	s.observer, err = kube.NewJobObserver(deps, kopt)
	must(t, err)

	// ---- runtime authority ----
	s.grants, err = rtpg.NewStore(x.Runtime, s.auth)
	must(t, err)
	key, err := rtjwt.GenerateKey()
	must(t, err)
	s.ring, err = rtjwt.NewKeyRing(1, key)
	must(t, err)
	signer, err := rtjwt.NewSigner(s.ring, issuer)
	must(t, err)
	pol := runtimeauth.Policy{AdmissionTTL: 10 * time.Minute, TokenTTL: 5 * time.Minute, MaxSession: 2 * time.Hour}
	s.verifier, err = rtjwt.NewVerifier(rtjwt.VerifierConfig{Ring: s.ring, Issuer: issuer, MaxLifetime: pol.MaxSession, Clock: x.Clock.Now})
	must(t, err)
	s.registry, err = runtimeauth.NewService(runtimeauth.Config{
		Store: s.grants, Attempts: s.auth, Jobs: s.observer, Tokens: signer, Verifier: s.verifier,
		Wrapper: runtimeauth.ExactWrapperPolicy{Supported: []string{entrypoint.Version}}, Policy: pol, Clock: x.Clock.Now,
	})
	must(t, err)
	guard, err := rpg.NewGuard(s.auth, s.ring.Generation, x.Clock.Now)
	must(t, err)

	// ---- execution services: plain for trusted paths, guarded for results ----
	deps.Registrar = s.registry
	s.runtime, err = kube.NewRuntime(deps, kopt)
	must(t, err)
	guardedStore, err := rpg.NewExecutionStore(s.rt, x.Runtime, guard)
	must(t, err)
	s.plain = s.newExecution(s.rt)
	s.guarded = s.newExecution(guardedStore)
	s.commits = &countingCommitter{inner: s.guarded}
	deps.Observations = s.plain
	deps.Tenants = tenantIDs{executiontest.ScopeA.TenantID, executiontest.ScopeB.TenantID}
	s.rec, err = kube.NewReconciler(deps, kopt)
	must(t, err)

	// ---- artifacts over real S3, signed for the Pod-reachable origin ----
	s.s3 = s3support.New(t)
	s.objects, err = arts3.NewStore(arts3.Config{
		Client: s.s3.Client, TransferClient: s3support.NewClient(s3support.Config{Endpoint: s.srv.URL, AccessKey: s.s3.Config.AccessKey, SecretKey: s.s3.Config.SecretKey}),
		Bucket: s.s3.Bucket(), Prefix: s.s3.Prefix, Clock: x.Clock.Now,
	})
	must(t, err)
	s.artRepo, err = artifactspg.NewStore(artifactspg.Config{DB: x.Runtime})
	must(t, err)
	artAuth, err := resultsadapters.NewArtifactAuthority(s.grants, s.ring, x.Clock.Now)
	must(t, err)
	s.artSvc, err = artifacts.NewService(artifacts.ServiceConfig{
		Repository: s.artRepo, Authority: artAuth, Reader: artifactstest.NewReader(), Objects: s.objects, Registry: artifactstest.NewRegistry(),
		Clock: x.Clock.Now, UploadTTL: 10 * time.Minute, DownloadTTL: 5 * time.Minute, ClaimLease: 30 * time.Second, SpoolDir: t.TempDir(),
	})
	must(t, err)

	// ---- results ----
	resAuth, err := resultsadapters.NewAuthority(s.verifier, s.ring, s.grants, x.Clock.Now)
	must(t, err)
	execAd, err := resultsadapters.NewExecution(s.commits, s.rt)
	must(t, err)
	evidence, err := resultsadapters.NewArtifacts(s.artSvc)
	must(t, err)
	resSvc, err := results.NewService(results.Dependencies{Authority: resAuth, Contexts: execAd, Evidence: evidence, Finalizer: execAd, Source: noSource{}}, results.Options{})
	must(t, err)
	resHandler, err := resultshttp.NewHandler(resultshttp.Config{Service: resSvc})
	must(t, err)
	rtHandler, err := runtimehttp.NewHandler(runtimehttp.Config{Service: s.registry})
	must(t, err)
	artHandler := s.artifactRouter()
	s.vault = s.newVault()
	s.srv.secrets = s.secretRoute(s.vault)
	s.srv.bind(t, rtHandler, resHandler, artHandler, s.s3.Config.Endpoint, "/"+s.s3.Bucket()+"/"+s.s3.Prefix)

	s.kit = s.newLogs(logsOptions{})
	return s
}

// newExecution builds the real execution service over the given store, wired
// with the kube planner and executor.
func (s *stack) newExecution(store execution.Store) *execution.Service {
	e := s.x
	reader, err := identitypg.NewStore(e.Runtime, e.Events)
	must(s.t, err)
	std, err := identity.NewStandaloneAuthority(reader)
	must(s.t, err)
	authority, err := execadapters.NewAuthority(std, reader)
	must(s.t, err)
	resolver, err := secrets.NewConfigurationResolver(e.Secrets, e.Secrets, nil)
	must(s.t, err)
	cfg, err := execadapters.NewConfiguration(resolver)
	must(s.t, err)
	defs, err := execadapters.NewDefinitions(e.Pipeline)
	must(s.t, err)
	svc, err := execution.NewService(execution.Dependencies{
		Store: store, Authority: authority, Definitions: defs, Capabilities: pipeline.EnabledKinds{pipeline.KindContainer, pipeline.KindAgent, pipeline.KindGate},
		Work: sqlWork{e}, Configuration: cfg, Sources: e.Sources, Limits: e.Limits, Planner: kube.NewPlanner(s.policy),
		Hooks: execution.EnabledKinds{pipeline.KindContainer}, Tenants: tenantIDs{executiontest.ScopeA.TenantID, executiontest.ScopeB.TenantID},
		Runtime: s.runtime, EffectReconciler: e.Effects, Clock: e.Clock.Now, Cleanup: allowCleanup{},
	}, execution.Options{ClaimLease: 2 * time.Second, Batch: 10})
	must(s.t, err)
	return svc
}

// provision creates one tenant namespace as the execution baseline requires
// (deployment owners do this in production): Pod Security restricted, the
// tenant binding, a service account that mounts no token and holds no
// permission, a compute ResourceQuota, default-deny networking and the single
// runtime egress.
func (s *stack) provision(ns, tenant string) {
	t := s.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	api := s.c.api
	must(t, api.create(ctx, "/api/v1/namespaces", map[string]any{
		"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]any{
			"name": ns,
			"labels": map[string]string{
				"pod-security.kubernetes.io/enforce": "restricted", "pod-security.kubernetes.io/enforce-version": "latest", "io.kodama." + Prefix + "/fixture": "true",
			},
			"annotations": map[string]string{kube.AnnNamespaceTenant: tenant},
		},
	}))
	must(t, api.create(ctx, "/api/v1/namespaces/"+ns+"/serviceaccounts", map[string]any{
		"apiVersion": "v1", "kind": "ServiceAccount", "metadata": map[string]any{"name": Prefix + "-runner", "namespace": ns}, "automountServiceAccountToken": false,
	}))
	must(t, api.create(ctx, "/api/v1/namespaces/"+ns+"/resourcequotas", map[string]any{
		"apiVersion": "v1", "kind": "ResourceQuota", "metadata": map[string]any{"name": "compute", "namespace": ns},
		"spec": map[string]any{"hard": map[string]string{"pods": "20", "limits.cpu": "8", "limits.memory": "8Gi", "limits.ephemeral-storage": "20Gi"}},
	}))
	var egress []map[string]any
	for _, r := range s.policy.RuntimeEgress() {
		var ports []map[string]any
		for _, pt := range r.Ports {
			ports = append(ports, map[string]any{"protocol": r.Protocol, "port": pt})
		}
		egress = append(egress, map[string]any{"to": []map[string]any{{"ipBlock": map[string]any{"cidr": r.CIDR}}}, "ports": ports})
	}
	np := "/apis/networking.k8s.io/v1/namespaces/" + ns + "/networkpolicies"
	must(t, api.create(ctx, np, map[string]any{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": map[string]any{"name": "default-deny", "namespace": ns},
		"spec": map[string]any{"podSelector": map[string]any{}, "policyTypes": []string{"Ingress", "Egress"}}}))
	must(t, api.create(ctx, np, map[string]any{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": map[string]any{"name": "runtime-egress", "namespace": ns},
		"spec": map[string]any{"podSelector": map[string]any{"matchLabels": map[string]string{kube.LabelManagedBy: kube.ValueManagedBy}}, "policyTypes": []string{"Egress"}, "egress": egress}}))
}

func (s *stack) ns(tenant domain.TenantID) string {
	if tenant == executiontest.ScopeA.TenantID {
		return s.nsA
	}
	return s.nsB
}

func (s *stack) count(sql string, args ...any) int { return s.x.Count(sql, args...) }

// artifactRouter serves the runtime artifact routes behind the shared router:
// the runtime token is verified, the artifact module's own authority re-reads
// the session grant on every call.
func (s *stack) artifactRouter() http.Handler {
	rt, err := httpapi.NewRouter(httpapi.RouterConfig{Authenticator: runtimeAuthn{s.verifier}, Authorizer: allowAll{}, RateLimiter: httpapi.Unlimited(), Prefix: "/runtime/v1"})
	must(s.t, err)
	h, err := artifactshttp.NewRuntime(s.artSvc, artifactshttp.DefaultActions())
	must(s.t, err)
	must(s.t, h.RegisterRoutes(rt))
	return rt
}
