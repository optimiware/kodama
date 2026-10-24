package kube_test

// T-079 acceptance: the real restricted Job carries a scoped, read-only checkout
// credential and Git CA to the common entrypoint only through the attempt's
// immutable bootstrap Secret.
//
// Every cluster call below goes through a wire layer that serializes each Job and
// Secret to the JSON the API would carry (and decodes what it returns), records
// those bytes, and fails the test if checkout material appears anywhere but the
// Secret's data keys.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
	"github.com/optimiware/kodama/internal/kube/kubetest"
)

// ---- wire layer ------------------------------------------------------------

// wireCluster implements kube.Cluster over a fake API server, round-tripping
// every Job and Secret through its JSON form in both directions and keeping the
// bytes it sent.
type wireCluster struct {
	kube.Cluster
	mu   sync.Mutex
	jobs [][]byte // every Job document sent
	secs [][]byte // every Secret document sent
}

func (w *wireCluster) viaJSON(v, out any, record *[][]byte) {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	if record != nil {
		w.mu.Lock()
		*record = append(*record, b)
		w.mu.Unlock()
	}
	if err := json.Unmarshal(b, out); err != nil {
		panic(err)
	}
}

func (w *wireCluster) CreateJob(ctx context.Context, ns string, j kubernetes.Job) (kubernetes.Job, error) {
	var sent kubernetes.Job
	w.viaJSON(j, &sent, &w.jobs)
	return w.Cluster.CreateJob(ctx, ns, sent)
}

func (w *wireCluster) CreateSecret(ctx context.Context, ns string, s kubernetes.Secret) (kubernetes.Secret, error) {
	var sent kubernetes.Secret
	w.viaJSON(s, &sent, &w.secs)
	out, err := w.Cluster.CreateSecret(ctx, ns, sent)
	return w.back(out), err
}

func (w *wireCluster) UpdateSecret(ctx context.Context, ns string, s kubernetes.Secret) (kubernetes.Secret, error) {
	var sent kubernetes.Secret
	w.viaJSON(s, &sent, &w.secs)
	out, err := w.Cluster.UpdateSecret(ctx, ns, sent)
	return w.back(out), err
}

func (w *wireCluster) GetSecret(ctx context.Context, ns, name string) (kubernetes.Secret, error) {
	out, err := w.Cluster.GetSecret(ctx, ns, name)
	return w.back(out), err
}

func (w *wireCluster) back(s kubernetes.Secret) kubernetes.Secret {
	var out kubernetes.Secret
	w.viaJSON(s, &out, nil)
	return out
}

// logBuffer captures every log line the executor writes.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *logBuffer) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// ---- fixtures --------------------------------------------------------------

type srcEnv struct {
	policy   kube.ExecutionPolicy
	tenant   domain.TenantID
	ctx      runtimev1.Context
	rec      kube.AttemptRecord
	exp      kube.Expectation
	fake     *kubetest.Fake
	wire     *wireCluster
	attempts *kubetest.Attempts
	reg      *kubetest.Registrar
	logs     *logBuffer
	// everErr collects the text of every error returned to the caller.
	errs []string
}

// newSrcEnv builds an attempt over a fresh policy; pass a policy to plan
// several attempts under the same configuration (the policy's CA is random).
func newSrcEnv(t *testing.T, access runtimev1.WorkspaceAccess, policy ...kube.ExecutionPolicy) *srcEnv {
	t.Helper()
	var p kube.ExecutionPolicy
	if len(policy) > 0 {
		p = policy[0]
	} else {
		p = kubetest.Policy(t, "acme", "globex")
	}
	tid, _ := kubetest.Tenant("acme")
	c := kubetest.Context(t, kubetest.ContextSpec{Tenant: tid, Project: "p_inv", Run: "run_1", Step: "build", Attempt: "att_1", Image: kubetest.WorkloadImage, Access: access})
	rec := kubetest.Record(t, p, c)
	exp, err := p.Expect(rec)
	if err != nil {
		t.Fatal(err)
	}
	f := kubetest.NewFake()
	kubetest.Baseline(f, p, tid)
	return &srcEnv{
		policy: p, tenant: tid, ctx: c, rec: rec, exp: exp, fake: f, wire: &wireCluster{Cluster: f},
		attempts: kubetest.NewAttempts(rec), reg: kubetest.NewRegistrar(), logs: &logBuffer{},
	}
}

func (e *srcEnv) runtime(t *testing.T, p kube.CheckoutProvider) *kube.Runtime {
	t.Helper()
	x, err := kube.NewRuntime(kube.Dependencies{
		Policy: e.policy, Cluster: e.wire, Attempts: e.attempts, Registrar: e.reg, Checkout: p,
	}, kube.Options{Logger: slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func (e *srcEnv) dispatch(t *testing.T, p kube.CheckoutProvider) error {
	t.Helper()
	_, err := e.runtime(t, p).Start(context.Background(), kubetest.Intent(e.rec))
	if err != nil {
		e.errs = append(e.errs, err.Error())
	}
	return err
}

func (e *srcEnv) ns() string { return e.exp.Identity.Namespace }

func (e *srcEnv) secret(t *testing.T) kubernetes.Secret {
	t.Helper()
	s, ok := e.fake.SecretOf(e.ns(), kube.SecretName(e.rec.JobName))
	if !ok {
		t.Fatal("no bootstrap Secret")
	}
	return s
}

func (e *srcEnv) hasSecret() bool {
	_, ok := e.fake.SecretOf(e.ns(), kube.SecretName(e.rec.JobName))
	return ok
}

func (e *srcEnv) job(t *testing.T) kubernetes.Job {
	t.Helper()
	j, ok := e.fake.JobOf(e.ns(), e.rec.JobName)
	if !ok {
		t.Fatal("no Job")
	}
	return j
}

func (e *srcEnv) hasJob() bool { _, ok := e.fake.JobOf(e.ns(), e.rec.JobName); return ok }

// nothingCreated asserts that a refused dispatch left the cluster and runtime
// admission untouched.
func (e *srcEnv) nothingCreated(t *testing.T) {
	t.Helper()
	if e.hasSecret() || e.hasJob() {
		t.Fatalf("a refused dispatch created objects (secret %v, job %v)", e.hasSecret(), e.hasJob())
	}
	if e.reg.Calls != 0 {
		t.Fatalf("a refused dispatch registered a credential (%d calls)", e.reg.Calls)
	}
	if e.fake.Calls("create secret") != 0 || e.fake.Calls("create job") != 0 {
		t.Fatal("a refused dispatch issued creates")
	}
}

// material is checkout material scoped to the attempt.
func (e *srcEnv) material(t *testing.T, user, pass string) kube.CheckoutMaterial {
	t.Helper()
	repo := "https://git.example.invalid/none.git"
	if src := e.ctx.Workspace.Source; src != nil {
		repo = src.RepositoryURL
	}
	return kube.CheckoutMaterial{
		Scope: e.ctx.Scope(), WorkspaceID: kubetest.Workspace, RepositoryURL: repo,
		Credential: []byte(user + ":" + pass), GitCA: kubetest.CA(t),
	}
}

// recProvider is a scriptable CheckoutProvider.
type recProvider struct {
	mu    sync.Mutex
	calls []kube.CheckoutRequest
	fn    func(kube.CheckoutRequest) (kube.CheckoutMaterial, error)
}

func (p *recProvider) Checkout(_ context.Context, req kube.CheckoutRequest) (kube.CheckoutMaterial, error) {
	p.mu.Lock()
	p.calls = append(p.calls, req)
	p.mu.Unlock()
	return p.fn(req)
}

func (p *recProvider) count() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.calls) }

func constProvider(m kube.CheckoutMaterial) *recProvider {
	return &recProvider{fn: func(kube.CheckoutRequest) (kube.CheckoutMaterial, error) { return m, nil }}
}

// forms are the representations of material that must never appear outside the
// Secret's data keys. The base64 forms are what a Secret's data holds.
func forms(m kube.CheckoutMaterial) []string {
	return []string{
		string(m.Credential), strings.SplitN(string(m.Credential), ":", 2)[1],
		base64.StdEncoding.EncodeToString(m.Credential), base64.StdEncoding.EncodeToString(m.GitCA), string(m.GitCA),
	}
}

func mustNotContain(t *testing.T, where string, data []byte, fs []string) {
	t.Helper()
	for _, f := range fs {
		if f != "" && bytes.Contains(data, []byte(f)) {
			t.Errorf("%s contains checkout material (%d-byte form starting %q)", where, len(f), f[:min(6, len(f))])
		}
	}
}

// assertNoLeak checks every place except the Secret's data keys.
func (e *srcEnv) assertNoLeak(t *testing.T, m kube.CheckoutMaterial) {
	t.Helper()
	fs := forms(m)
	for i, j := range e.wire.jobs {
		mustNotContain(t, fmt.Sprintf("Job document %d sent to the API", i), j, fs)
	}
	if job, ok := e.fake.JobOf(e.ns(), e.rec.JobName); ok {
		mustNotContain(t, "the stored Job", job.Raw, fs)
		doc, _ := json.Marshal(job)
		mustNotContain(t, "the Job's typed document", doc, fs)
	}
	if e.hasSecret() {
		s := e.secret(t)
		meta, _ := json.Marshal(s.Metadata)
		mustNotContain(t, "the Secret's metadata (labels, annotations)", meta, fs)
	}
	// A Secret document carries the credential base64-encoded in its data and
	// nowhere in plaintext.
	for i, s := range e.wire.secs {
		mustNotContain(t, fmt.Sprintf("Secret document %d (plaintext forms)", i), s, []string{string(m.Credential), string(m.GitCA)})
	}
	mustNotContain(t, "executor logs", []byte(e.logs.String()), fs)
	mustNotContain(t, "returned errors", []byte(strings.Join(e.errs, "\n")), fs)
}

func (e *srcEnv) workload(t *testing.T) kubernetes.Container {
	t.Helper()
	for _, c := range e.job(t).Spec.Template.Spec.Containers {
		if c.Name == kube.PrimaryContainer {
			return c
		}
	}
	t.Fatal("no primary container")
	return kubernetes.Container{}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func sameSet(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			found = found || g == w
		}
		if !found {
			t.Fatalf("keys = %v, want %v", got, want)
		}
	}
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func hasArgPrefix(args []string, prefix string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

var errOutage = errors.New("provider outage")

// ---- Test_job_checkout_credential -------------------------------------------

func Test_job_checkout_credential(t *testing.T) {
	t.Run("a reader's Job carries the credential only in the immutable bootstrap Secret", func(t *testing.T) {
		for _, access := range []runtimev1.WorkspaceAccess{runtimev1.AccessRead, runtimev1.AccessWrite} {
			t.Run(string(access), func(t *testing.T) {
				e := newSrcEnv(t, access)
				m := e.material(t, "kodama-ro", "s3cr3t-checkout-credential-0001")
				prov := constProvider(m)
				if err := e.dispatch(t, prov); err != nil {
					t.Fatal(err)
				}
				s := e.secret(t)
				sameSet(t, keys(s.Data), kube.KeyAdmission, kube.KeyCA, kube.KeyCheckoutCredential, kube.KeyGitCA)
				if string(s.Data[kube.KeyCheckoutCredential]) != string(m.Credential) || string(s.Data[kube.KeyGitCA]) != string(m.GitCA) {
					t.Fatal("the Secret does not hold the issued material")
				}
				if string(s.Data[kube.KeyCA]) != string(e.policy.RuntimeCA()) {
					t.Fatal("the runtime CA changed")
				}
				if s.Immutable == nil || !*s.Immutable {
					t.Fatal("the Secret is not immutable")
				}
				c := e.workload(t)
				for _, want := range []string{
					"--checkout-credential-file=/kodama/bootstrap/checkout-credential",
					"--git-ca-file=/kodama/bootstrap/git-ca.crt",
					"--admission-credential-file=/kodama/bootstrap/admission",
					"--ca-file=/kodama/bootstrap/ca.crt",
				} {
					if !hasArg(c.Args, want) {
						t.Errorf("missing argument %s in %v", want, c.Args)
					}
				}
				// The file names the Job passes are exactly the Secret's keys.
				for _, arg := range c.Args {
					for _, flag := range []string{"--checkout-credential-file=", "--git-ca-file="} {
						if path, ok := strings.CutPrefix(arg, flag); ok {
							key := strings.TrimPrefix(path, "/kodama/bootstrap/")
							if _, ok := s.Data[key]; !ok {
								t.Errorf("%s names %q, which is not a Secret key", flag, key)
							}
						}
					}
				}
				// The bootstrap volume is the attempt's Secret, mounted read-only and
				// shared with no writable volume.
				var vol *kubernetes.Volume
				for i, v := range e.job(t).Spec.Template.Spec.Volumes {
					if v.Name == "bootstrap" {
						vol = &e.job(t).Spec.Template.Spec.Volumes[i]
					}
				}
				if vol == nil || vol.Secret == nil || vol.Secret.SecretName != kube.SecretName(e.rec.JobName) {
					t.Fatalf("bootstrap volume = %+v", vol)
				}
				mounted := 0
				for _, vm := range c.VolumeMounts {
					if vm.Name == "bootstrap" {
						mounted++
						if !vm.ReadOnly || vm.MountPath != "/kodama/bootstrap" {
							t.Errorf("bootstrap mount = %+v", vm)
						}
					}
				}
				if mounted != 1 {
					t.Fatalf("bootstrap mounted %d times", mounted)
				}
				// No environment variable, command or init container sees the material.
				for _, env := range c.Env {
					if env.Value != "" && strings.Contains(strings.Join(forms(m), "\x00"), env.Value) {
						t.Errorf("environment variable %s carries material", env.Name)
					}
				}
				if len(c.Command) != 1 || c.Command[0] != "/kodama/bin/kodama" {
					t.Errorf("command = %v", c.Command)
				}
				// Only a read-only credential is requested, once, for the committed
				// workspace; publisher material is not part of the seam.
				if n := prov.count(); n != 1 {
					t.Fatalf("provider calls = %d", n)
				}
				req := prov.calls[0]
				if req.Scope != e.ctx.Scope() || req.WorkspaceID != kubetest.Workspace || req.AttemptID != e.rec.AttemptID ||
					string(req.Access) != string(access) || req.RepositoryURL != e.ctx.Workspace.Source.RepositoryURL || req.Commit != e.ctx.Workspace.Source.Commit {
					t.Fatalf("provider request = %+v", req)
				}
				e.assertNoLeak(t, m)
				if e.reg.Calls < 1 {
					t.Fatal("the admission credential was not registered")
				}
			})
		}
	})

	t.Run("an attempt without a workspace keeps the two-key layout and needs no provider", func(t *testing.T) {
		e := newSrcEnv(t, "")
		prov := constProvider(e.material(t, "u", "p"))
		if err := e.dispatch(t, nil); err != nil {
			t.Fatal(err)
		}
		sameSet(t, keys(e.secret(t).Data), kube.KeyAdmission, kube.KeyCA)
		for _, a := range e.workload(t).Args {
			if strings.HasPrefix(a, "--checkout-credential-file") || strings.HasPrefix(a, "--git-ca-file") {
				t.Fatalf("unexpected argument %s", a)
			}
		}
		if prov.count() != 0 {
			t.Fatal("provider consulted")
		}
		// A provider that is configured is still not asked.
		e2 := newSrcEnv(t, "")
		if err := e2.dispatch(t, prov); err != nil {
			t.Fatal(err)
		}
		if prov.count() != 0 {
			t.Fatal("provider consulted for an attempt without source")
		}
		sameSet(t, keys(e2.secret(t).Data), kube.KeyAdmission, kube.KeyCA)
	})

	t.Run("the Job fingerprint and document do not depend on the issued material", func(t *testing.T) {
		a := newSrcEnv(t, runtimev1.AccessRead)
		b := newSrcEnv(t, runtimev1.AccessRead, a.policy)
		ma, mb := a.material(t, "reader-a", "password-aaaa-1111"), b.material(t, "reader-b", "password-bbbb-2222")
		if a.rec.SpecFingerprint != b.rec.SpecFingerprint {
			t.Fatal("the planned fingerprint differs between identical attempts")
		}
		if err := a.dispatch(t, constProvider(ma)); err != nil {
			t.Fatal(err)
		}
		if err := b.dispatch(t, constProvider(mb)); err != nil {
			t.Fatal(err)
		}
		ja, jb := a.job(t), b.job(t)
		if ja.Metadata.Annotations[kube.AnnFingerprint] != a.rec.SpecFingerprint || jb.Metadata.Annotations[kube.AnnFingerprint] != a.rec.SpecFingerprint {
			t.Fatal("a Job does not carry the committed fingerprint")
		}
		sa, _ := json.Marshal(ja.Spec)
		sb, _ := json.Marshal(jb.Spec)
		// Server-generated selector UIDs differ; the rest must not.
		if len(sa) != len(sb) {
			t.Fatalf("Job specs differ in size: %d vs %d", len(sa), len(sb))
		}
		if _, err := kube.VerifyJob(a.exp, ja); err != nil {
			t.Fatalf("the created Job fails verification: %v", err)
		}
		// Checkout flags are part of the fingerprinted recipe: a source attempt's
		// Job differs from the same step without them.
		none := newSrcEnv(t, "", a.policy)
		if none.rec.SpecFingerprint == a.rec.SpecFingerprint {
			t.Fatal("source and no-workspace recipes share a fingerprint")
		}
	})

	t.Run("a restart or redrive reads the stored Secret and never asks the provider again", func(t *testing.T) {
		e := newSrcEnv(t, runtimev1.AccessRead)
		first := e.material(t, "reader-one", "password-one-0001")
		p1 := constProvider(first)
		if err := e.dispatch(t, p1); err != nil {
			t.Fatal(err)
		}
		stored := e.secret(t)
		// A restarted controller whose provider now answers differently.
		second := e.material(t, "reader-two", "password-two-0002")
		p2 := constProvider(second)
		for i := 0; i < 3; i++ {
			if err := e.dispatch(t, p2); err != nil {
				t.Fatalf("redrive %d: %v", i, err)
			}
		}
		if p2.count() != 0 || p1.count() != 1 {
			t.Fatalf("provider calls: first %d, second %d", p1.count(), p2.count())
		}
		now := e.secret(t)
		if string(now.Data[kube.KeyCheckoutCredential]) != string(first.Credential) || string(now.Data[kube.KeyGitCA]) != string(first.GitCA) {
			t.Fatal("the stored material changed")
		}
		for k, v := range stored.Data {
			if !bytes.Equal(v, now.Data[k]) {
				t.Fatalf("Secret data %q changed across redrives", k)
			}
		}
		if e.fake.Calls("create secret") != 1 || e.fake.Calls("create job") != 1 {
			t.Fatalf("creates: secret %d, job %d", e.fake.Calls("create secret"), e.fake.Calls("create job"))
		}
		e.assertNoLeak(t, first)
		e.assertNoLeak(t, second)
	})

	t.Run("a crash between the Secret and the Job redrives onto the stored material", func(t *testing.T) {
		e := newSrcEnv(t, runtimev1.AccessRead)
		first := e.material(t, "reader-one", "password-one-0001")
		e.fake.Fault = func(op string, n int) *kubetest.Fault {
			if op == "create job" && n == 1 {
				return &kubetest.Fault{Err: &kubernetes.Error{Op: op, Kind: kubernetes.ErrInvalid}}
			}
			return nil
		}
		p1 := constProvider(first)
		if err := e.dispatch(t, p1); err == nil {
			t.Fatal("the refused create was reported as success")
		}
		if !e.hasSecret() || e.hasJob() {
			t.Fatalf("secret %v job %v", e.hasSecret(), e.hasJob())
		}
		second := e.material(t, "reader-two", "password-two-0002")
		p2 := constProvider(second)
		if err := e.dispatch(t, p2); err != nil {
			t.Fatal(err)
		}
		if p2.count() != 0 {
			t.Fatal("the provider was asked again though the Secret exists")
		}
		if string(e.secret(t).Data[kube.KeyCheckoutCredential]) != string(first.Credential) {
			t.Fatal("the stored credential was replaced")
		}
		e.assertNoLeak(t, first)
		e.assertNoLeak(t, second)
	})

	t.Run("a lost Secret-create response is resolved by reading, and the provider is not asked again", func(t *testing.T) {
		e := newSrcEnv(t, runtimev1.AccessRead)
		first := e.material(t, "reader-one", "password-one-0001")
		e.fake.Fault = func(op string, n int) *kubetest.Fault {
			if op == "create secret" && n == 1 {
				return &kubetest.Fault{Err: &kubernetes.Error{Op: op, Kind: kubernetes.ErrUnknownOutcome}, After: true}
			}
			return nil
		}
		p1 := constProvider(first)
		if err := e.dispatch(t, p1); err != nil {
			t.Fatal(err)
		}
		if p1.count() != 1 || e.fake.Calls("create secret") != 1 {
			t.Fatalf("provider calls %d, secret creates %d", p1.count(), e.fake.Calls("create secret"))
		}
		if string(e.secret(t).Data[kube.KeyCheckoutCredential]) != string(first.Credential) {
			t.Fatal("the stored credential is not the issued one")
		}
		e.assertNoLeak(t, first)
	})

	t.Run("a concurrent dispatcher's stored material wins and ours is discarded", func(t *testing.T) {
		e := newSrcEnv(t, runtimev1.AccessRead)
		winner := e.material(t, "reader-win", "password-win-0001")
		loser := e.material(t, "reader-lose", "password-lose-0002")
		pw := constProvider(winner)
		// While the first dispatcher is inside its provider call, a second one
		// completes the whole dispatch with its own provider.
		pl := &recProvider{fn: func(kube.CheckoutRequest) (kube.CheckoutMaterial, error) {
			if _, err := e.runtime(t, pw).Start(context.Background(), kubetest.Intent(e.rec)); err != nil {
				t.Errorf("concurrent dispatcher: %v", err)
			}
			return loser, nil
		}}
		if err := e.dispatch(t, pl); err != nil {
			t.Fatal(err)
		}
		if got := string(e.secret(t).Data[kube.KeyCheckoutCredential]); got != string(winner.Credential) {
			t.Fatalf("the stored credential is not the winner's")
		}
		if e.fake.Calls("create job") != 1 {
			t.Fatalf("created %d Jobs", e.fake.Calls("create job"))
		}
		e.assertNoLeak(t, winner)
		e.assertNoLeak(t, loser)
	})

	t.Run("replays create nothing and never rewrite the Secret's data", func(t *testing.T) {
		e := newSrcEnv(t, runtimev1.AccessWrite)
		m := e.material(t, "reader", "password-replay-0001")
		if err := e.dispatch(t, constProvider(m)); err != nil {
			t.Fatal(err)
		}
		before := e.secret(t)
		for i := 0; i < 5; i++ {
			if err := e.dispatch(t, nil); err != nil { // replay needs no provider
				t.Fatalf("replay %d: %v", i, err)
			}
		}
		after := e.secret(t)
		for k, v := range before.Data {
			if !bytes.Equal(v, after.Data[k]) {
				t.Fatalf("data %q changed", k)
			}
		}
		if after.Immutable == nil || !*after.Immutable {
			t.Fatal("the Secret lost immutability")
		}
		// Every Secret document sent after the create is the same data (record
		// updates only change annotations).
		for i, doc := range e.wire.secs {
			var s kubernetes.Secret
			if err := json.Unmarshal(doc, &s); err != nil {
				t.Fatal(err)
			}
			for k, v := range before.Data {
				if !bytes.Equal(v, s.Data[k]) {
					t.Fatalf("Secret document %d changed data %q", i, k)
				}
			}
		}
		e.assertNoLeak(t, m)
	})

	t.Run("a missing provider denies a source attempt and creates nothing", func(t *testing.T) {
		e := newSrcEnv(t, runtimev1.AccessRead)
		err := e.dispatch(t, nil)
		if !errors.Is(err, kube.ErrCheckoutDenied) {
			t.Fatalf("err = %v", err)
		}
		e.nothingCreated(t)
	})

	t.Run("a refusing or failing provider creates nothing and a later retry succeeds", func(t *testing.T) {
		e := newSrcEnv(t, runtimev1.AccessRead)
		m := e.material(t, "reader", "password-retry-0001")
		deny := &recProvider{fn: func(kube.CheckoutRequest) (kube.CheckoutMaterial, error) {
			return kube.CheckoutMaterial{}, fmt.Errorf("%w: repo %s is not readable with %s", kube.ErrCheckoutDenied, "x", m.Credential)
		}}
		if err := e.dispatch(t, deny); !errors.Is(err, kube.ErrCheckoutDenied) {
			t.Fatalf("err = %v", err)
		}
		e.nothingCreated(t)
		outage := &recProvider{fn: func(kube.CheckoutRequest) (kube.CheckoutMaterial, error) {
			return kube.CheckoutMaterial{}, fmt.Errorf("%w: upstream said %s", errOutage, m.Credential)
		}}
		err := e.dispatch(t, outage)
		if !errors.Is(err, kube.ErrUnavailable) || !errors.Is(err, errOutage) {
			t.Fatalf("err = %v", err)
		}
		e.nothingCreated(t)
		if err := e.dispatch(t, constProvider(m)); err != nil {
			t.Fatalf("retry: %v", err)
		}
		e.assertNoLeak(t, m)
	})

	t.Run("an answer for another scope, workspace or repository is refused", func(t *testing.T) {
		for name, edit := range map[string]func(*kube.CheckoutMaterial){
			"foreign tenant":     func(m *kube.CheckoutMaterial) { m.Scope.TenantID = "t021_globex" },
			"foreign project":    func(m *kube.CheckoutMaterial) { m.Scope.ProjectID = "p_other" },
			"foreign workspace":  func(m *kube.CheckoutMaterial) { m.WorkspaceID = "ws_other" },
			"foreign repository": func(m *kube.CheckoutMaterial) { m.RepositoryURL = "https://git.example.invalid/other/repo.git" },
			"no workspace":       func(m *kube.CheckoutMaterial) { m.WorkspaceID = "" },
		} {
			t.Run(name, func(t *testing.T) {
				e := newSrcEnv(t, runtimev1.AccessRead)
				m := e.material(t, "reader", "password-foreign-0001")
				edit(&m)
				err := e.dispatch(t, constProvider(m))
				if !errors.Is(err, kube.ErrCheckoutDenied) {
					t.Fatalf("err = %v", err)
				}
				e.nothingCreated(t)
				e.assertNoLeak(t, m)
			})
		}
	})

	t.Run("malformed material is refused before anything is created", func(t *testing.T) {
		good := kubetest.CA(t)
		for name, edit := range map[string]func(*kube.CheckoutMaterial){
			"no colon":          func(m *kube.CheckoutMaterial) { m.Credential = []byte("justonevalue") },
			"empty user":        func(m *kube.CheckoutMaterial) { m.Credential = []byte(":pass") },
			"empty password":    func(m *kube.CheckoutMaterial) { m.Credential = []byte("user:") },
			"newline":           func(m *kube.CheckoutMaterial) { m.Credential = []byte("user:pass\nextra:line") },
			"trailing newline":  func(m *kube.CheckoutMaterial) { m.Credential = []byte("user:pass\n") },
			"leading space":     func(m *kube.CheckoutMaterial) { m.Credential = []byte(" user:pass") },
			"control character": func(m *kube.CheckoutMaterial) { m.Credential = []byte("user:pa\x00ss") },
			"oversized":         func(m *kube.CheckoutMaterial) { m.Credential = []byte("user:" + strings.Repeat("p", 5000)) },
			"empty CA":          func(m *kube.CheckoutMaterial) { m.GitCA = nil },
			"CA is not PEM":     func(m *kube.CheckoutMaterial) { m.GitCA = []byte("not a certificate") },
			"CA with a private key": func(m *kube.CheckoutMaterial) {
				m.GitCA = append(append([]byte(nil), good...), []byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n")...)
			},
		} {
			t.Run(name, func(t *testing.T) {
				e := newSrcEnv(t, runtimev1.AccessRead)
				m := e.material(t, "reader", "password-bad-0001")
				edit(&m)
				err := e.dispatch(t, constProvider(m))
				if !errors.Is(err, kube.ErrCheckoutDenied) {
					t.Fatalf("err = %v", err)
				}
				e.nothingCreated(t)
				// Even refused material never appears in the error text.
				for _, s := range []string{string(m.Credential)} {
					if len(s) > 8 && strings.Contains(strings.Join(e.errs, "\n")+e.logs.String(), s) {
						t.Fatal("refused material was echoed")
					}
				}
			})
		}
	})

	t.Run("the committed attempt must bind a workspace", func(t *testing.T) {
		e := newSrcEnv(t, runtimev1.AccessRead)
		e.attempts.Edit(e.rec.AttemptID, func(r *kube.AttemptRecord) { r.WorkspaceID = "" })
		prov := constProvider(e.material(t, "reader", "password-unbound-0001"))
		if err := e.dispatch(t, prov); !errors.Is(err, kube.ErrNotDispatchable) {
			t.Fatalf("err = %v", err)
		}
		if prov.count() != 0 {
			t.Fatal("the provider was asked without a committed workspace")
		}
		e.nothingCreated(t)
	})

	t.Run("the stored context must agree with the committed access", func(t *testing.T) {
		e := newSrcEnv(t, runtimev1.AccessRead)
		e.attempts.Edit(e.rec.AttemptID, func(r *kube.AttemptRecord) { r.Access = execution.AccessNone })
		in := kubetest.Intent(e.rec)
		in.Access = execution.AccessNone
		_, err := e.runtime(t, constProvider(e.material(t, "r", "password-x-0001"))).Start(context.Background(), in)
		if err == nil {
			t.Fatal("a dispatch with a mismatched access was accepted")
		}
		e.nothingCreated(t)
	})

	t.Run("a tampered Secret is never adopted", func(t *testing.T) {
		type tamper struct {
			name string
			want error
			edit func(*kubernetes.Secret, srcEnvKit)
		}
		cases := []tamper{
			{"extra key", kube.ErrForeignWorkload, func(s *kubernetes.Secret, _ srcEnvKit) { s.Data["publisher-credential"] = []byte("x") }},
			{"checkout credential removed", kube.ErrForeignWorkload, func(s *kubernetes.Secret, _ srcEnvKit) { delete(s.Data, kube.KeyCheckoutCredential) }},
			{"Git CA removed", kube.ErrForeignWorkload, func(s *kubernetes.Secret, _ srcEnvKit) { delete(s.Data, kube.KeyGitCA) }},
			{"both checkout keys removed", kube.ErrForeignWorkload, func(s *kubernetes.Secret, _ srcEnvKit) {
				delete(s.Data, kube.KeyCheckoutCredential)
				delete(s.Data, kube.KeyGitCA)
			}},
			{"credential malformed", kube.ErrForeignWorkload, func(s *kubernetes.Secret, _ srcEnvKit) { s.Data[kube.KeyCheckoutCredential] = []byte("no-colon") }},
			{"Git CA holds a key", kube.ErrForeignWorkload, func(s *kubernetes.Secret, _ srcEnvKit) {
				s.Data[kube.KeyGitCA] = []byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n")
			}},
			{"mutable", kube.ErrForeignWorkload, func(s *kubernetes.Secret, _ srcEnvKit) { s.Immutable = nil }},
			{"owner reference", kube.ErrForeignWorkload, func(s *kubernetes.Secret, _ srcEnvKit) {
				s.Metadata.OwnerReferences = []kubernetes.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "x", UID: "u"}}
			}},
			{"foreign attempt annotation", kube.ErrForeignWorkload, func(s *kubernetes.Secret, _ srcEnvKit) { s.Metadata.Annotations[kube.AnnAttempt] = "att_other" }},
			{"fingerprint", kube.ErrForeignWorkload, func(s *kubernetes.Secret, _ srcEnvKit) { s.Metadata.Annotations[kube.AnnFingerprint] = "sha256:00" }},
			{"runtime CA replaced", kube.ErrPolicyDrift, func(s *kubernetes.Secret, k srcEnvKit) { s.Data[kube.KeyCA] = kubetest.CA(k.t) }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				e := newSrcEnv(t, runtimev1.AccessRead)
				m := e.material(t, "reader", "password-tamper-0001")
				if err := e.dispatch(t, constProvider(m)); err != nil {
					t.Fatal(err)
				}
				s := e.secret(t)
				tc.edit(&s, srcEnvKit{t})
				e.fake.PutSecret(e.ns(), s)
				// Redrive with the Job present, and again after the Job is gone (the
				// Secret is still the attempt's creation-recovery record).
				err := e.dispatch(t, nil)
				if !errors.Is(err, tc.want) {
					t.Fatalf("redrive over an existing Job: err = %v, want %v", err, tc.want)
				}
				e.fake.DropJob(e.ns(), e.rec.JobName)
				err = e.dispatch(t, nil)
				if !errors.Is(err, tc.want) {
					t.Fatalf("redrive without a Job: err = %v, want %v", err, tc.want)
				}
				if e.hasJob() {
					t.Fatal("a Job was created over a tampered Secret")
				}
				e.assertNoLeak(t, m)
			})
		}
	})

	t.Run("a no-workspace Secret that gained checkout keys is refused", func(t *testing.T) {
		e := newSrcEnv(t, "")
		if err := e.dispatch(t, nil); err != nil {
			t.Fatal(err)
		}
		s := e.secret(t)
		m := e.material(t, "reader", "password-gain-0001")
		s.Data[kube.KeyCheckoutCredential], s.Data[kube.KeyGitCA] = m.Credential, m.GitCA
		e.fake.PutSecret(e.ns(), s)
		if err := e.dispatch(t, nil); !errors.Is(err, kube.ErrForeignWorkload) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a Job that lost its checkout arguments or gained material is foreign", func(t *testing.T) {
		for name, edit := range map[string]func(map[string]any, kube.CheckoutMaterial){
			"credential flag removed": func(doc map[string]any, _ kube.CheckoutMaterial) {
				c := workloadDoc(doc)
				var kept []any
				for _, a := range c["args"].([]any) {
					if !strings.HasPrefix(a.(string), "--checkout-credential-file") {
						kept = append(kept, a)
					}
				}
				c["args"] = kept
			},
			"Git CA flag removed": func(doc map[string]any, _ kube.CheckoutMaterial) {
				c := workloadDoc(doc)
				var kept []any
				for _, a := range c["args"].([]any) {
					if !strings.HasPrefix(a.(string), "--git-ca-file") {
						kept = append(kept, a)
					}
				}
				c["args"] = kept
			},
			"credential in an argument": func(doc map[string]any, m kube.CheckoutMaterial) {
				c := workloadDoc(doc)
				c["args"] = append(c["args"].([]any), "--token="+string(m.Credential))
			},
			"credential in the environment": func(doc map[string]any, m kube.CheckoutMaterial) {
				c := workloadDoc(doc)
				c["env"] = append(c["env"].([]any), map[string]any{"name": "GIT_CREDENTIAL", "value": string(m.Credential)})
			},
			"credential in an annotation": func(doc map[string]any, m kube.CheckoutMaterial) {
				doc["metadata"].(map[string]any)["annotations"].(map[string]any)["x"] = string(m.Credential)
			},
		} {
			t.Run(name, func(t *testing.T) {
				e := newSrcEnv(t, runtimev1.AccessRead)
				m := e.material(t, "reader", "password-job-0001")
				if err := e.dispatch(t, constProvider(m)); err != nil {
					t.Fatal(err)
				}
				e.fake.MutateJobRaw(e.ns(), e.rec.JobName, func(doc map[string]any) { edit(doc, m) })
				_, verr := kube.VerifyJob(e.exp, e.job(t))
				// An annotation outside the expected set is not part of the spec
				// comparison; the other mutations are.
				if name != "credential in an annotation" && !errors.Is(verr, kube.ErrForeignWorkload) {
					t.Fatalf("VerifyJob = %v", verr)
				}
				if verr != nil {
					mustNotContain(t, "the verification error", []byte(verr.Error()), forms(m))
				}
				if name != "credential in an annotation" {
					if err := e.dispatch(t, nil); !errors.Is(err, kube.ErrForeignWorkload) {
						t.Fatalf("redrive over the tampered Job: %v", err)
					}
				}
			})
		}
	})
}

type srcEnvKit struct{ t *testing.T }

func workloadDoc(doc map[string]any) map[string]any {
	spec := doc["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	for _, c := range spec["containers"].([]any) {
		if c.(map[string]any)["name"] == kube.PrimaryContainer {
			return c.(map[string]any)
		}
	}
	panic("no workload container")
}

// ---- Test_job_git_ca ---------------------------------------------------------

func Test_job_git_ca(t *testing.T) {
	t.Run("the Git CA is its own key beside, never instead of, the runtime CA", func(t *testing.T) {
		e := newSrcEnv(t, runtimev1.AccessRead)
		m := e.material(t, "reader", "password-ca-0001")
		if err := e.dispatch(t, constProvider(m)); err != nil {
			t.Fatal(err)
		}
		s := e.secret(t)
		if !bytes.Equal(s.Data[kube.KeyGitCA], m.GitCA) {
			t.Fatal("the Git CA is not the issued one")
		}
		if !bytes.Equal(s.Data[kube.KeyCA], e.policy.RuntimeCA()) || bytes.Equal(s.Data[kube.KeyCA], s.Data[kube.KeyGitCA]) {
			t.Fatal("the runtime CA and the Git CA were mixed")
		}
		c := e.workload(t)
		if !hasArg(c.Args, "--ca-file=/kodama/bootstrap/ca.crt") || !hasArg(c.Args, "--git-ca-file=/kodama/bootstrap/git-ca.crt") {
			t.Fatalf("args = %v", c.Args)
		}
		e.assertNoLeak(t, m)
	})

	t.Run("an attempt without a workspace has no Git CA", func(t *testing.T) {
		e := newSrcEnv(t, "")
		if err := e.dispatch(t, nil); err != nil {
			t.Fatal(err)
		}
		if _, ok := e.secret(t).Data[kube.KeyGitCA]; ok {
			t.Fatal("a Git CA was stored for an attempt that checks out nothing")
		}
		if hasArgPrefix(e.workload(t).Args, "--git-ca-file") {
			t.Fatal("a Git CA argument was passed")
		}
	})

	t.Run("a changed provider CA cannot alter a stored attempt", func(t *testing.T) {
		e := newSrcEnv(t, runtimev1.AccessRead)
		m := e.material(t, "reader", "password-ca-0002")
		if err := e.dispatch(t, constProvider(m)); err != nil {
			t.Fatal(err)
		}
		rotated := m
		rotated.GitCA = kubetest.CA(t)
		p := constProvider(rotated)
		if err := e.dispatch(t, p); err != nil {
			t.Fatal(err)
		}
		if p.count() != 0 || !bytes.Equal(e.secret(t).Data[kube.KeyGitCA], m.GitCA) {
			t.Fatal("the stored Git CA changed")
		}
	})
}

// ---- Test_job_secret_dir (the Job side) ------------------------------------

// The declared-secret directory is materialized by the entrypoint inside the
// Pod's memory-backed secrets volume; the Job must leave the volume writable by
// the non-root user (the entrypoint creates its own private child and never
// changes the volume root) and keep the checkout material out of that volume.
func Test_job_secret_dir(t *testing.T) {
	e := newSrcEnv(t, runtimev1.AccessRead)
	m := e.material(t, "reader", "password-dir-0001")
	if err := e.dispatch(t, constProvider(m)); err != nil {
		t.Fatal(err)
	}
	j := e.job(t)
	pod := j.Spec.Template.Spec
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot ||
		pod.SecurityContext.FSGroup == nil || *pod.SecurityContext.FSGroup != *pod.SecurityContext.RunAsGroup || *pod.SecurityContext.RunAsUser == 0 {
		t.Fatalf("pod security = %+v", pod.SecurityContext)
	}
	var secrets *kubernetes.Volume
	for i, v := range pod.Volumes {
		if v.Name == "secrets" {
			secrets = &pod.Volumes[i]
		}
	}
	if secrets == nil || secrets.EmptyDir == nil || secrets.EmptyDir.Medium != "Memory" || secrets.Secret != nil {
		t.Fatalf("secrets volume = %+v", secrets)
	}
	c := e.workload(t)
	if !hasArg(c.Args, "--secrets-dir=/kodama/secrets") {
		t.Fatalf("args = %v", c.Args)
	}
	for _, vm := range c.VolumeMounts {
		if vm.Name == "secrets" && (vm.ReadOnly || vm.MountPath != "/kodama/secrets") {
			t.Fatalf("secrets mount = %+v", vm)
		}
		if vm.MountPath == "/kodama/secrets" && vm.Name != "secrets" {
			t.Fatalf("%s is mounted over the secrets directory", vm.Name)
		}
	}
	// No argument pre-selects a child directory: the entrypoint owns that choice.
	for _, a := range c.Args {
		if strings.HasPrefix(a, "--secrets-dir=") && a != "--secrets-dir=/kodama/secrets" {
			t.Fatalf("unexpected %s", a)
		}
	}
	// The checkout material is not placed in the writable secrets volume.
	if !hasArg(c.Args, "--checkout-credential-file=/kodama/bootstrap/checkout-credential") {
		t.Fatal("the checkout credential is not in the bootstrap volume")
	}
	e.assertNoLeak(t, m)
}
