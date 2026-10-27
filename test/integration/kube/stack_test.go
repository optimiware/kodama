package kube_test

// The composed fixture: real PostgreSQL with every dependent schema (the
// execution fixture of T-020), the real execution service, the real T-019 runtime
// admission service and HTTP routes behind a TLS listener Pods can reach, the
// real T-021 executor, observer and reconciler against the kind cluster, and the
// actual T-016 entrypoint running in the Pods. The only doubles are the Git
// observation of the T-020 fixture (no test here uses a workspace) and the
// result route, a stand-in for T-022 that authorizes through runtimeauth and
// commits through execution.CommitResult.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/entrypoint"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/adapters"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/identity"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
	"github.com/optimiware/kodama/internal/kube"
	kubeadapters "github.com/optimiware/kodama/internal/kube/adapters"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/runtimeauth"
	runtimehttp "github.com/optimiware/kodama/internal/runtimeauth/http"
	"github.com/optimiware/kodama/internal/runtimeauth/jwt"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
	"github.com/optimiware/kodama/internal/secrets"
)

// ---- fault-injecting cluster ----

// faultCluster wraps the real client. Hooks run around the calls the tests need
// to disturb: Block runs before the call (it may block), Fail returns an error
// instead of calling, and Lose applies the call and then reports a lost
// response (an unknown outcome).
type faultCluster struct {
	kube.Cluster
	mu     sync.Mutex
	counts map[string]int
	Block  func(op string, n int)
	Fail   func(op string, n int) error
	Lose   func(op string, n int) bool
	// After runs once a create has been applied, before its response returns.
	After func(op string, n int)
}

func newFaultCluster(c kube.Cluster) *faultCluster {
	return &faultCluster{Cluster: c, counts: map[string]int{}}
}

func (f *faultCluster) enter(op string) (pre error, lose bool) {
	f.mu.Lock()
	f.counts[op]++
	n := f.counts[op]
	block, fail, loseFn := f.Block, f.Fail, f.Lose
	f.mu.Unlock()
	_ = n
	if block != nil {
		block(op, n)
	}
	if fail != nil {
		if err := fail(op, n); err != nil {
			return err, false
		}
	}
	return nil, loseFn != nil && loseFn(op, n)
}

func (f *faultCluster) calls(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[op]
}

func (f *faultCluster) reset() {
	f.mu.Lock()
	f.Block, f.Fail, f.Lose, f.After = nil, nil, nil, nil
	f.mu.Unlock()
}

func lost(op string) error { return &kubernetes.Error{Op: op, Kind: kubernetes.ErrUnknownOutcome} }

func (f *faultCluster) CreateJob(ctx context.Context, ns string, j kubernetes.Job) (kubernetes.Job, error) {
	if err, lose := f.enter("create job"); err != nil {
		return kubernetes.Job{}, err
	} else if lose {
		_, _ = f.Cluster.CreateJob(ctx, ns, j)
		return kubernetes.Job{}, lost("create job")
	}
	j2, err := f.Cluster.CreateJob(ctx, ns, j)
	f.mu.Lock()
	after, n := f.After, f.counts["create job"]
	f.mu.Unlock()
	if after != nil {
		after("create job", n)
	}
	return j2, err
}

func (f *faultCluster) DeleteJob(ctx context.Context, ns, name string, o kubernetes.DeleteOptions) error {
	if err, lose := f.enter("delete job"); err != nil {
		return err
	} else if lose {
		_ = f.Cluster.DeleteJob(ctx, ns, name, o)
		return lost("delete job")
	}
	return f.Cluster.DeleteJob(ctx, ns, name, o)
}

func (f *faultCluster) GetJob(ctx context.Context, ns, name string) (kubernetes.Job, error) {
	if err, _ := f.enter("get job"); err != nil {
		return kubernetes.Job{}, err
	}
	return f.Cluster.GetJob(ctx, ns, name)
}

func (f *faultCluster) ListJobs(ctx context.Context, ns, sel string) ([]kubernetes.Job, error) {
	if err, _ := f.enter("list jobs"); err != nil {
		return nil, err
	}
	return f.Cluster.ListJobs(ctx, ns, sel)
}

func (f *faultCluster) ListPods(ctx context.Context, ns, sel string) ([]kubernetes.Pod, error) {
	if err, _ := f.enter("list pods"); err != nil {
		return nil, err
	}
	return f.Cluster.ListPods(ctx, ns, sel)
}

func (f *faultCluster) GetSecret(ctx context.Context, ns, name string) (kubernetes.Secret, error) {
	if err, _ := f.enter("get secret"); err != nil {
		return kubernetes.Secret{}, err
	}
	return f.Cluster.GetSecret(ctx, ns, name)
}

func (f *faultCluster) CreateSecret(ctx context.Context, ns string, s kubernetes.Secret) (kubernetes.Secret, error) {
	if err, lose := f.enter("create secret"); err != nil {
		return kubernetes.Secret{}, err
	} else if lose {
		_, _ = f.Cluster.CreateSecret(ctx, ns, s)
		return kubernetes.Secret{}, lost("create secret")
	}
	return f.Cluster.CreateSecret(ctx, ns, s)
}

func (f *faultCluster) UpdateSecret(ctx context.Context, ns string, s kubernetes.Secret) (kubernetes.Secret, error) {
	if err, lose := f.enter("update secret"); err != nil {
		return kubernetes.Secret{}, err
	} else if lose {
		_, _ = f.Cluster.UpdateSecret(ctx, ns, s)
		return kubernetes.Secret{}, lost("update secret")
	}
	return f.Cluster.UpdateSecret(ctx, ns, s)
}

// ---- runtime server ----

type admitEvent struct {
	Attempt domain.AttemptID
	Status  int
	Code    string
}

type runtimeServer struct {
	ln  net.Listener
	srv *http.Server
	URL string
	CA  []byte

	rt   *runtimeauth.Service
	exec *execution.Service

	mu     sync.Mutex
	admits []admitEvent
	tokens map[domain.AttemptID]string
	// AckWithoutCommit makes the result route acknowledge without committing.
	AckWithoutCommit atomic.Bool
	// RefuseResults makes the result route answer 503.
	RefuseResults atomic.Bool
	results       atomic.Int32
}

func newRuntimeServer(t *testing.T, hostIP string) *runtimeServer {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	caTpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "t021 runtime ca"}, NotBefore: time.Now().Add(-24 * time.Hour), NotAfter: time.Now().Add(48 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	must(t, err)
	caCert, err := x509.ParseCertificate(caDER)
	must(t, err)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	ip, err := netip.ParseAddr(hostIP)
	must(t, err)
	leafTpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "t021 runtime"}, NotBefore: time.Now().Add(-24 * time.Hour), NotAfter: time.Now().Add(48 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.IP(ip.AsSlice())},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTpl, caCert, &leafKey.PublicKey, caKey)
	must(t, err)
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	must(t, err)
	s := &runtimeServer{ln: ln, tokens: map[domain.AttemptID]string{}}
	s.CA = pemCert(caDER)
	s.URL = "https://" + net.JoinHostPort(hostIP, fmt.Sprint(ln.Addr().(*net.TCPAddr).Port))
	s.srv = &http.Server{
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

func pemCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// start serves; the runtime service and execution service are bound late
// because the policy (which needs URL and CA) precedes them.
func (s *runtimeServer) start(t *testing.T, rt *runtimeauth.Service, exec *execution.Service) {
	t.Helper()
	s.rt, s.exec = rt, exec
	inner, err := runtimehttp.NewHandler(runtimehttp.Config{Service: rt})
	must(t, err)
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+runtimev1.BasePath+"/result", s.result)
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		inner.ServeHTTP(rec, r)
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/admit") {
			attempt := domain.AttemptID(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/runtime/v1/attempts/"), "/admit"))
			ev := admitEvent{Attempt: attempt, Status: rec.status}
			var env runtimev1.ErrorResponse
			if rec.status != 200 && json.Unmarshal(rec.body.Bytes(), &env) == nil {
				ev.Code = string(env.Error.Code)
			}
			s.mu.Lock()
			s.admits = append(s.admits, ev)
			if rec.status == 200 {
				var resp runtimev1.AdmitResponse
				if json.Unmarshal(rec.body.Bytes(), &resp) == nil {
					s.tokens[attempt] = resp.Token.Token
				}
			}
			s.mu.Unlock()
		}
	}))
	s.srv.Handler = mux
	go func() { _ = s.srv.ServeTLS(s.ln, "", "") }()
	t.Cleanup(func() { _ = s.srv.Close() })
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

func (s *runtimeServer) admitEvents(attempt domain.AttemptID) []admitEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []admitEvent
	for _, e := range s.admits {
		if e.Attempt == attempt {
			out = append(out, e)
		}
	}
	return out
}

func (s *runtimeServer) token(attempt domain.AttemptID) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokens[attempt]
}

func contractError(w http.ResponseWriter, code domain.ErrorCode) {
	status, _ := runtimev1.Status(code)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(runtimev1.ErrorResponse{Error: runtimev1.ErrorBody{Code: code, Message: "t021 fixture", RequestID: "req_t021fixture", Details: map[string]any{}}})
}

// result is the T-022 stand-in: bearer authorization through runtimeauth, strict
// decoding, then execution.CommitResult.
func (s *runtimeServer) result(w http.ResponseWriter, r *http.Request) {
	s.results.Add(1)
	if s.RefuseResults.Load() {
		contractError(w, domain.CodeUnavailable)
		return
	}
	attempt := domain.AttemptID(r.PathValue("attemptId"))
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	req, err := runtimev1.DecodeRequest[runtimev1.ResultRequest](body)
	if err != nil {
		contractError(w, domain.CodeInvalidRequest)
		return
	}
	az, err := s.rt.Authorize(r.Context(), runtimeauth.AuthorizeRequest{AttemptID: attempt, Bearer: tok, Action: runtimeauth.ActionResultSubmit})
	if err != nil {
		switch {
		case errors.Is(err, runtimeauth.ErrAttemptRevoked):
			contractError(w, runtimev1.CodeAttemptRevoked)
		case errors.Is(err, runtimeauth.ErrInvalidCredential), errors.Is(err, runtimeauth.ErrNotFound):
			contractError(w, domain.CodeInvalidCredential)
		case errors.Is(err, runtimeauth.ErrForbidden):
			contractError(w, domain.CodeForbidden)
		default:
			contractError(w, domain.CodeUnavailable)
		}
		return
	}
	ack := execution.ResultAck{AttemptID: attempt, ResultHash: string(req.ResultHash), AttemptStatus: execution.AttemptSucceeded}
	if !s.AckWithoutCommit.Load() {
		if ack, err = s.exec.CommitResult(r.Context(), execution.ResultCommit{Scope: az.Scope, AttemptID: attempt, Request: req}); err != nil {
			switch {
			case errors.Is(err, execution.ErrAttemptRevoked):
				contractError(w, runtimev1.CodeAttemptRevoked)
			case errors.Is(err, execution.ErrResultConflict):
				contractError(w, runtimev1.CodeResultConflict)
			case errors.Is(err, execution.ErrInvalid):
				contractError(w, domain.CodeInvalidRequest)
			default:
				contractError(w, domain.CodeUnavailable)
			}
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(runtimev1.ResultResponse{
		AttemptID: attempt, ResultHash: runtimev1.Digest(ack.ResultHash), AttemptStatus: runtimev1.AttemptStatus(ack.AttemptStatus),
		AcceptedAt: runtimev1.Timestamp(time.Now().UTC().Format(time.RFC3339Nano)),
	})
}

// ---- the stack ----

type stack struct {
	t        *testing.T
	c        *cluster
	e        *executiontest.Env
	nsA, nsB string
	policy   kube.ExecutionPolicy
	cl       *faultCluster
	attempts *kubeadapters.Attempts
	registry *runtimeauth.Service
	observer *kube.JobObserver
	runtime  *kube.Runtime
	rec      *kube.Reconciler
	svc      *execution.Service
	srv      *runtimeServer
	auth     *kubeadapters.Authority
	ids      atomic.Int64
}

type stackOptions struct {
	// Workload overrides the policy's workload resource bounds.
	Workload *kube.ResourceBounds
	// InjectorCommand overrides the init container command.
	InjectorCommand []string
	// NoNetworkPolicies provisions the namespaces without NetworkPolicies, which
	// the preflight must refuse.
	NoNetworkPolicies bool
	Limits            *execution.Limit
	// ReconcilerOptions adjusts the reconciler (and executor) options.
	Options func(*kube.Options)
}

// startErr keeps only the error of a Start.
func startErr(_ execution.Workload, err error) error { return err }

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func newStack(t *testing.T, o ...stackOptions) *stack {
	t.Helper()
	var opt stackOptions
	if len(o) > 0 {
		opt = o[0]
	}
	c := requireCluster(t)
	e := executiontest.New(t)
	e.GrantAll()
	s := &stack{t: t, c: c, e: e}

	suffix := randHex(3)
	s.nsA, s.nsB = "t021-"+suffix+"-a", "t021-"+suffix+"-b"
	s.srv = newRuntimeServer(t, c.hostIP)
	port := s.srv.ln.Addr().(*net.TCPAddr).Port
	egress := []kube.EgressRule{{CIDR: c.hostIP + "/32", Protocol: "TCP", Ports: []int32{int32(port)}}}
	cfg := kube.PolicyConfig{
		Tenants: map[domain.TenantID]kube.TenantTarget{
			executiontest.ScopeA.TenantID: {Namespace: s.nsA, ServiceAccount: "t021-runner"},
			executiontest.ScopeB.TenantID: {Namespace: s.nsB, ServiceAccount: "t021-runner"},
		},
		InjectorImage:   c.injector,
		InjectorCommand: []string{"cp", "/usr/local/bin/kodama", "/kodama/bin/kodama"},
		RuntimeURL:      s.srv.URL, RuntimeCA: s.srv.CA, RuntimeEgress: egress,
		Workload:  kube.ResourceBounds{CPURequest: "50m", CPULimit: "500m", MemoryRequest: "32Mi", MemoryLimit: "256Mi", EphemeralRequest: "32Mi", EphemeralLimit: "512Mi"},
		Injector:  kube.ResourceBounds{CPURequest: "10m", CPULimit: "100m", MemoryRequest: "8Mi", MemoryLimit: "64Mi", EphemeralRequest: "16Mi", EphemeralLimit: "128Mi"},
		Volumes:   kube.VolumeSizes{Workspace: "64Mi", Outputs: "32Mi", Scratch: "64Mi", Results: "1Mi", Tools: "64Mi", Secrets: "4Mi", Identity: "1Mi"},
		RunAsUser: 65532, RunAsGroup: 65532, TerminationGrace: 20 * time.Second,
	}
	if opt.Workload != nil {
		cfg.Workload = *opt.Workload
	}
	if opt.InjectorCommand != nil {
		cfg.InjectorCommand = opt.InjectorCommand
	}
	policy, err := kube.NewExecutionPolicy(cfg)
	must(t, err)
	s.policy = policy
	s.provision(s.nsA, string(executiontest.ScopeA.TenantID), policy, opt)
	s.provision(s.nsB, string(executiontest.ScopeB.TenantID), policy, opt)
	t.Cleanup(func() {
		_, _ = run("kubectl", "--kubeconfig", c.kubeconfig, "delete", "namespace", s.nsA, s.nsB, "--wait=false", "--ignore-not-found")
	})

	s.cl = newFaultCluster(c.client)
	s.attempts, err = kubeadapters.NewAttempts(e.Store)
	must(t, err)
	s.auth, err = kubeadapters.NewAuthority(e.Store, policy)
	must(t, err)
	kopt := kube.Options{Clock: e.Clock.Now, CreateQuiescence: 3 * time.Second, OrphanGrace: 2 * time.Second, PreflightTTL: time.Second}
	if opt.Options != nil {
		opt.Options(&kopt)
	}
	deps := kube.Dependencies{Policy: policy, Cluster: s.cl, Attempts: s.attempts}
	s.observer, err = kube.NewJobObserver(deps, kopt)
	must(t, err)

	store, err := rtpg.NewStore(e.Runtime, s.auth)
	must(t, err)
	key, err := jwt.GenerateKey()
	must(t, err)
	ring, err := jwt.NewKeyRing(1, key)
	must(t, err)
	rtPolicy := runtimeauth.Policy{AdmissionTTL: 10 * time.Minute, TokenTTL: 5 * time.Minute, MaxSession: 2 * time.Hour}
	const issuer = "https://runtime.t021.invalid"
	signer, err := jwt.NewSigner(ring, issuer)
	must(t, err)
	verifier, err := jwt.NewVerifier(jwt.VerifierConfig{Ring: ring, Issuer: issuer, MaxLifetime: rtPolicy.MaxSession, Clock: e.Clock.Now})
	must(t, err)
	s.registry, err = runtimeauth.NewService(runtimeauth.Config{
		Store: store, Attempts: s.auth, Jobs: s.observer, Tokens: signer, Verifier: verifier,
		Wrapper: runtimeauth.ExactWrapperPolicy{Supported: []string{entrypoint.Version}}, Policy: rtPolicy, Clock: e.Clock.Now,
	})
	must(t, err)

	deps.Registrar = s.registry
	s.runtime, err = kube.NewRuntime(deps, kopt)
	must(t, err)
	s.svc = s.newService(opt)
	deps.Observations = s.svc
	deps.Tenants = tenantIDs{executiontest.ScopeA.TenantID, executiontest.ScopeB.TenantID}
	s.rec, err = kube.NewReconciler(deps, kopt)
	must(t, err)
	s.srv.start(t, s.registry, s.svc)
	return s
}

type tenantIDs []domain.TenantID

func (t tenantIDs) TenantIDs(context.Context) ([]domain.TenantID, error) { return t, nil }

// newService is the real execution service wired with the kube planner and
// executor (the T-020 fixture's own service uses a double executor).
func (s *stack) newService(opt stackOptions) *execution.Service {
	e := s.e
	reader, err := identitypg.NewStore(e.Runtime, e.Events)
	must(s.t, err)
	std, err := identity.NewStandaloneAuthority(reader)
	must(s.t, err)
	authority, err := adapters.NewAuthority(std, reader)
	must(s.t, err)
	resolver, err := secrets.NewConfigurationResolver(e.Secrets, e.Secrets, nil)
	must(s.t, err)
	cfg, err := adapters.NewConfiguration(resolver)
	must(s.t, err)
	defs, err := adapters.NewDefinitions(e.Pipeline)
	must(s.t, err)
	lim := executiontest.DefaultLimit()
	lim.ProjectConcurrency, lim.TenantConcurrency = 50, 100
	// A shared, loaded host can take minutes to start a Pod: only the tests that
	// exercise the startup deadline shorten it.
	lim.StartupBudget = 5 * time.Minute
	if opt.Limits != nil {
		lim = *opt.Limits
	}
	e.Limits.Set(lim)
	svc, err := execution.NewService(execution.Dependencies{
		Store: e.Store, Authority: authority, Definitions: defs, Capabilities: pipeline.EnabledKinds{pipeline.KindContainer, pipeline.KindAgent, pipeline.KindGate},
		Work: sqlWork{e}, Configuration: cfg, Sources: e.Sources, Limits: e.Limits, Planner: kube.NewPlanner(s.policy),
		Hooks: execution.EnabledKinds{pipeline.KindContainer}, Tenants: tenantIDs{executiontest.ScopeA.TenantID, executiontest.ScopeB.TenantID},
		Runtime: s.runtime, EffectReconciler: e.Effects, Clock: e.Clock.Now, Cleanup: allowCleanup{},
	}, execution.Options{ClaimLease: 2 * time.Second, Batch: 10})
	must(s.t, err)
	return svc
}

// allowCleanup authorizes the recorded deployment of an internal cleanup run
// (T-035 supplies the real one in M2).
type allowCleanup struct{}

func (allowCleanup) AuthorizeCleanup(context.Context, domain.Scope, string) error { return nil }

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

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
