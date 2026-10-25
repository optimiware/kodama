package entrypoint

// T-079 acceptance: the real restricted Job carries a scoped, read-only checkout
// credential and Git CA to the common entrypoint, and nothing else does.
//
// The Secret and the arguments under test are produced by the real kube.Runtime
// (dispatching against the in-memory cluster), written out exactly as the kubelet
// projects a Secret volume (one file per key, named by key), and consumed by the
// real entrypoint in a separate local process (this test binary re-executed, see
// TestMain) that talks to a loopback runtime API and clones from a real
// git-http-backend behind a TLS listener that requires Basic credentials. There is
// no proxy and no injected helper between the entrypoint and Git.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
	"github.com/optimiware/kodama/internal/kube/kubetest"
)

const jcSubprocess = "KODAMA_T079_ENTRYPOINT"

// TestMain turns this test binary into the entrypoint when it is re-executed by
// a test: the process under test then has its own argv, environment, working
// directory, signals and exit status, exactly like the container command.
func TestMain(m *testing.M) {
	if os.Getenv(jcSubprocess) == "1" {
		os.Exit(Run(context.Background(), os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

const (
	jcUser = "kodama-ro"
	jcPass = "s3cr3t-checkout-7c41d9e2"
)

// ---- loopback runtime API -------------------------------------------------

// jcRuntime is a minimal runtime API server speaking the frozen wire contract.
// It records every request so tests can prove what never crossed it.
type jcRuntime struct {
	t         *testing.T
	srv       *httptest.Server
	ctx       runtimev1.Context
	admission string
	secrets   map[string]string // ref -> value

	mu      sync.Mutex
	wire    [][]byte // method, path, authorization and body of every request
	objects map[string][]byte
	names   map[string]string
	results []runtimev1.ResultRequest
	admits  []runtimev1.AdmitRequest
	nextArt int
}

const jcToken = "rt_jc_token_0123456789abcdef"

func newJCRuntime(t *testing.T, c runtimev1.Context, admission string, secrets map[string]string) *jcRuntime {
	t.Helper()
	r := &jcRuntime{t: t, ctx: c, admission: admission, secrets: secrets, objects: map[string][]byte{}, names: map[string]string{}}
	r.srv = httptest.NewUnstartedServer(http.HandlerFunc(r.serve))
	r.srv.Start()
	t.Cleanup(r.srv.Close)
	return r
}

func (r *jcRuntime) reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (r *jcRuntime) deny(w http.ResponseWriter, status int, code domain.ErrorCode) {
	r.reply(w, status, runtimev1.ErrorResponse{Error: runtimev1.ErrorBody{Code: code, Message: string(code), RequestID: "req_jc"}})
}

func (r *jcRuntime) tokenInfo() runtimev1.TokenInfo {
	return tokenInfo(jcToken, time.Hour, 2*time.Hour)
}

func (r *jcRuntime) serve(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(req.Body, 8<<20))
	r.mu.Lock()
	r.wire = append(r.wire, []byte(req.Method+" "+req.URL.Path+"\n"+req.Header.Get("Authorization")+"\n"), body)
	r.mu.Unlock()

	if strings.HasPrefix(req.URL.Path, "/objects/") {
		id := strings.TrimPrefix(req.URL.Path, "/objects/")
		r.mu.Lock()
		r.objects[id] = body
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		return
	}
	prefix := "/runtime/v1/attempts/" + string(r.ctx.AttemptID)
	route, ok := strings.CutPrefix(req.URL.Path, prefix)
	if !ok {
		r.deny(w, http.StatusNotFound, domain.CodeNotFound)
		return
	}
	if route != "/admit" && req.Header.Get("Authorization") != "Bearer "+jcToken {
		r.deny(w, http.StatusUnauthorized, domain.CodeInvalidCredential)
		return
	}
	switch {
	case route == "/admit" && req.Method == http.MethodPost:
		var a runtimev1.AdmitRequest
		if json.Unmarshal(body, &a) != nil || a.AdmissionCredential != r.admission {
			r.deny(w, http.StatusForbidden, domain.CodeForbidden)
			return
		}
		r.mu.Lock()
		r.admits = append(r.admits, a)
		r.mu.Unlock()
		r.reply(w, http.StatusOK, runtimev1.AdmitResponse{Token: r.tokenInfo(), Context: r.ctx})
	case route == "/token":
		r.reply(w, http.StatusOK, runtimev1.TokenResponse{Token: r.tokenInfo()})
	case route == "/context":
		r.reply(w, http.StatusOK, r.ctx)
	case route == "/secrets/resolve":
		var q runtimev1.SecretResolveRequest
		_ = json.Unmarshal(body, &q)
		for _, b := range r.ctx.Secrets {
			if b.Ref == q.Ref && b.Version == q.Version {
				v, ok := r.secrets[b.Ref]
				if !ok {
					break
				}
				r.reply(w, http.StatusOK, runtimev1.SecretResolveResponse{Ref: b.Ref, Version: b.Version, Name: b.Name, Delivery: b.Delivery, ValueBase64: base64.StdEncoding.EncodeToString([]byte(v))})
				return
			}
		}
		r.deny(w, http.StatusForbidden, domain.CodeForbidden)
	case route == "/artifacts":
		var q runtimev1.ArtifactReserveRequest
		_ = json.Unmarshal(body, &q)
		r.mu.Lock()
		r.nextArt++
		id := fmt.Sprintf("art_jc%03d", r.nextArt)
		r.names[id] = q.Name
		r.mu.Unlock()
		r.reply(w, http.StatusCreated, runtimev1.ArtifactReserveResponse{ArtifactID: domain.ArtifactID(id), State: runtimev1.StateReserved, Upload: &runtimev1.Upload{
			Method: "PUT", URL: r.srv.URL + "/objects/" + id, Headers: map[string]string{}, ExpiresAt: timestamp(time.Hour),
		}})
	case strings.HasPrefix(route, "/artifacts/") && strings.HasSuffix(route, "/complete"):
		id := strings.TrimSuffix(strings.TrimPrefix(route, "/artifacts/"), "/complete")
		var q runtimev1.ArtifactCompleteRequest
		_ = json.Unmarshal(body, &q)
		r.mu.Lock()
		obj, ok := r.objects[id]
		r.mu.Unlock()
		if !ok || digestOfBytes(obj) != q.Digest || int64(len(obj)) != q.SizeBytes {
			r.deny(w, http.StatusUnprocessableEntity, domain.CodeInvalidRequest)
			return
		}
		r.reply(w, http.StatusOK, runtimev1.ArtifactCompleteResponse{ArtifactID: domain.ArtifactID(id), State: runtimev1.StateUsable, Digest: q.Digest, SizeBytes: q.SizeBytes})
	case route == "/result":
		var q runtimev1.ResultRequest
		if json.Unmarshal(body, &q) != nil {
			r.deny(w, http.StatusBadRequest, domain.CodeInvalidRequest)
			return
		}
		r.mu.Lock()
		r.results = append(r.results, q)
		r.mu.Unlock()
		status := runtimev1.AttemptFailed
		if q.Result.Outcome == runtimev1.OutcomeCompleted {
			status = runtimev1.AttemptSucceeded
		}
		r.reply(w, http.StatusOK, runtimev1.ResultResponse{AttemptID: r.ctx.AttemptID, ResultHash: q.ResultHash, AttemptStatus: status, AcceptedAt: timestamp(0)})
	default:
		r.deny(w, http.StatusNotFound, domain.CodeNotFound)
	}
}

// everything is every byte the runtime API received or stored.
func (r *jcRuntime) everything() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []byte
	for _, w := range r.wire {
		out = append(out, w...)
	}
	for _, o := range r.objects {
		out = append(out, o...)
	}
	return out
}

func (r *jcRuntime) object(name string) ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, n := range r.names {
		if n == name {
			o, ok := r.objects[id]
			return o, ok
		}
	}
	return nil, false
}

func (r *jcRuntime) result(t *testing.T) runtimev1.Result {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.results) == 0 {
		t.Fatal("the entrypoint submitted no result")
	}
	return r.results[len(r.results)-1].Result
}

// ---- the Job the real executor builds -------------------------------------

// jcProvider is the composition's CheckoutProvider.
type jcProvider struct {
	mu       sync.Mutex
	material kube.CheckoutMaterial
	calls    []kube.CheckoutRequest
}

func (p *jcProvider) Checkout(_ context.Context, req kube.CheckoutRequest) (kube.CheckoutMaterial, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, req)
	return p.material, nil
}

type jcOpts struct {
	access runtimev1.WorkspaceAccess // "" = none
	script string
	// issuedPass overrides the password the provider issues (the Git server
	// still expects jcPass); issuedCA overrides the Git CA it issues.
	issuedPass string
	issuedCA   []byte
	outputs    []runtimev1.OutputDecl
	secrets    []runtimev1.SecretBinding
	values     map[string]string
	// secretsRoot is the directory mounted at /kodama/secrets ("" = a private
	// directory of the Pod fixture).
	secretsRoot string
}

// jcPod is one dispatched attempt laid out as a Pod: the real Job's arguments,
// the real Secret projected into the bootstrap directory, and a runtime API.
type jcPod struct {
	t        *testing.T
	root     string
	rt       *jcRuntime
	gs       *gitServer
	commit   string
	job      kubernetes.Job
	secret   kubernetes.Secret
	provider *jcProvider
	args     []string // as in the Job, before path mapping
	jobName  string
	mounts   [][2]string
	// secretsRoot is the mapped /kodama/secrets.
	secretsRoot string
}

func newJCPod(t *testing.T, o jcOpts) *jcPod {
	t.Helper()
	requireGit(t)
	p := &jcPod{t: t, root: t.TempDir()}
	gitRoot := t.TempDir()
	first, _ := seedRepo(t, gitRoot)
	p.commit = first
	p.gs = newGitServer(t, gitRoot, jcUser, jcPass)
	repoURL := p.gs.URL + "/repo.git"

	policy := kubetest.Policy(t, "acme")
	tenant, _ := kubetest.Tenant("acme")
	access := o.access
	c := kubetest.Context(t, kubetest.ContextSpec{Tenant: tenant, Project: "p_inv", Run: "run_1", Step: "build", Attempt: "att_jc1", Image: kubetest.WorkloadImage, Access: access})
	if access != "" && access != runtimev1.AccessNone {
		c.Workspace.Source.RepositoryURL, c.Workspace.Source.Commit = repoURL, runtimev1.GitCommit(first)
	}
	c.Command = runtimev1.Command{Image: kubetest.WorkloadImage, Command: []string{"/bin/sh", "-c"}, Args: []string{o.script}, Env: map[string]string{}}
	if o.outputs != nil {
		c.Outputs = o.outputs
	}
	if o.secrets != nil {
		c.Secrets = o.secrets
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("context: %v", err)
	}
	rec := kubetest.Record(t, policy, c)

	pass := jcPass
	if o.issuedPass != "" {
		pass = o.issuedPass
	}
	ca := o.issuedCA
	if ca == nil {
		var err error
		if ca, err = os.ReadFile(p.gs.CAFile); err != nil {
			t.Fatal(err)
		}
	}
	p.provider = &jcProvider{material: kube.CheckoutMaterial{
		Scope: c.Scope(), WorkspaceID: kubetest.Workspace, RepositoryURL: repoURL,
		Credential: []byte(jcUser + ":" + pass), GitCA: ca,
	}}
	fake := kubetest.NewFake()
	kubetest.Baseline(fake, policy, tenant)
	x, err := kube.NewRuntime(kube.Dependencies{
		Policy: policy, Cluster: fake, Attempts: kubetest.NewAttempts(rec), Registrar: kubetest.NewRegistrar(), Checkout: p.provider,
	}, kube.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x.Start(context.Background(), kubetest.Intent(rec)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	tgt, _ := policy.Tenant(tenant)
	var ok bool
	if p.job, ok = fake.JobOf(tgt.Namespace, rec.JobName); !ok {
		t.Fatal("no Job")
	}
	if p.secret, ok = fake.SecretOf(tgt.Namespace, kube.SecretName(rec.JobName)); !ok {
		t.Fatal("no Secret")
	}
	p.jobName = rec.JobName
	for _, ctr := range p.job.Spec.Template.Spec.Containers {
		if ctr.Name == kube.PrimaryContainer {
			p.args = append([]string(nil), ctr.Args...)
		}
	}
	if len(p.args) == 0 {
		t.Fatal("the Job has no entrypoint arguments")
	}

	p.secretsRoot = o.secretsRoot
	if p.secretsRoot == "" {
		p.secretsRoot = p.dir("secrets")
	}
	p.mounts = [][2]string{
		{"/kodama/bootstrap", p.dir("bootstrap")}, {"/kodama/outputs", p.dir("outputs")}, {"/kodama/results", p.dir("results")},
		{"/kodama/secrets", p.secretsRoot}, {"/kodama/identity", p.dir("identity")}, {"/workspace", p.dir("workspace")}, {"/tmp", p.dir("tmp")},
	}
	// The kubelet projects every key of the Secret as a file named by the key.
	for k, v := range p.secret.Data {
		if err := os.WriteFile(filepath.Join(p.dir("bootstrap"), k), v, 0o440); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"outputs", "results", "identity", "tmp"} {
		_ = os.MkdirAll(p.dir(d), 0o755)
	}
	p.rt = newJCRuntime(t, c, string(p.secret.Data[kube.KeyAdmission]), o.values)
	return p
}

func (p *jcPod) dir(name string) string {
	d := filepath.Join(p.root, name)
	_ = os.MkdirAll(d, 0o755)
	return d
}

func (p *jcPod) mapPath(v string) string {
	for _, m := range p.mounts {
		if v == m[0] || strings.HasPrefix(v, m[0]+"/") {
			return m[1] + strings.TrimPrefix(v, m[0])
		}
	}
	return v
}

// mappedArgs are the Job's arguments with Pod paths mapped onto the fixture and
// the callback origin pointed at the loopback runtime, plus the identity the
// Downward API would add through the environment.
func (p *jcPod) mappedArgs(drop ...string) []string {
	var out []string
	if len(p.args) == 0 || p.args[0] != CommandName {
		p.t.Fatalf("the Job's arguments do not start with the %q subcommand: %v", CommandName, p.args)
	}
outer:
	for _, a := range p.args[1:] {
		for _, d := range drop {
			if strings.HasPrefix(a, d) {
				continue outer
			}
		}
		flag, val, ok := strings.Cut(a, "=")
		switch {
		case !ok:
			out = append(out, a)
		case flag == "--server-url":
			out = append(out, flag+"="+p.rt.srv.URL)
		default:
			out = append(out, flag+"="+p.mapPath(val))
		}
	}
	return append(out,
		"--job-name="+p.jobName, "--job-uid="+p.job.Metadata.UID,
		"--pod-name="+p.jobName+"-x7k2p", "--pod-uid=7a2d2e3f-4b5c-4d6e-9fa0-1b2c3d4e5f60")
}

func (p *jcPod) hasArg(want string) bool {
	for _, a := range p.args {
		if a == want {
			return true
		}
	}
	return false
}

type jcRun struct {
	exit           int
	stdout, stderr string
}

// run executes the real entrypoint in its own process.
func (p *jcPod) run(drop ...string) jcRun {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], p.mappedArgs(drop...)...)
	cmd.Dir = p.root
	cmd.Env = []string{jcSubprocess + "=1", "PATH=" + os.Getenv("PATH"), "HOME=" + p.dir("home"), "TMPDIR=" + p.dir("tmp")}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	res := jcRun{stdout: stdout.String(), stderr: stderr.String()}
	if ee, ok := err.(*exec.ExitError); ok {
		res.exit = ee.ExitCode()
	} else if err != nil {
		p.t.Fatalf("entrypoint process: %v", err)
	}
	return res
}

// forms are the representations of a credential that must not leak.
func jcForms(user, pass string) []string {
	b64 := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	return []string{pass, user + ":" + pass, b64, "Basic " + b64}
}

// gitKnobLeaks names the GIT_CONFIG_* variables in a captured command environment other than the
// one safe.directory entry the step receives for the kubelet-owned workspace. Anything else could
// carry the checkout credential or its CA into the command.
func gitKnobLeaks(env []byte, workspace string) []string {
	var out []string
	for _, line := range strings.Split(string(env), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "GIT_CONFIG_") {
			continue
		}
		switch line {
		case "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=safe.directory", "GIT_CONFIG_VALUE_0=" + workspace:
			continue
		}
		name, _, _ := strings.Cut(line, "=")
		out = append(out, name)
	}
	return out
}

func jcNoLeak(t *testing.T, where string, data []byte, forms []string) {
	t.Helper()
	for _, f := range forms {
		if strings.Contains(string(data), f) {
			t.Errorf("%s contains the checkout credential (%d bytes of form %q...)", where, len(f), f[:min(4, len(f))])
		}
	}
}

// jcWalk returns the content of every regular file under dir, keyed by path.
func jcWalk(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			b, _ := os.ReadFile(p)
			out[p] = b
		}
		return nil
	})
	return out
}

const jcCheckoutScript = `set -eu
test "$(cat "$KODAMA_WORKSPACE/a.txt")" = one
{ env | sort; cat "$KODAMA_WORKSPACE/.git/config"; } > "$KODAMA_OUTPUTS_DIR/env.txt"
`

var jcEnvOutput = []runtimev1.OutputDecl{{Name: "env", Type: runtimev1.OutputArtifact, Path: ptr("/kodama/outputs/env.txt")}}

func Test_job_checkout_credential(t *testing.T) {
	t.Run("private checkout authenticates from the Job Secret without a proxy", func(t *testing.T) {
		p := newJCPod(t, jcOpts{access: runtimev1.AccessRead, script: jcCheckoutScript, outputs: jcEnvOutput})
		// The arguments are exactly the supported entrypoint flags, in the
		// read-only bootstrap volume that also holds the admission credential.
		for _, want := range []string{
			"--checkout-credential-file=/kodama/bootstrap/checkout-credential",
			"--git-ca-file=/kodama/bootstrap/git-ca.crt",
			"--admission-credential-file=/kodama/bootstrap/admission",
		} {
			if !p.hasArg(want) {
				t.Fatalf("the Job does not pass %s: %v", want, p.args)
			}
		}
		run := p.run()
		if run.exit != ExitOK {
			t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", run.exit, run.stdout, run.stderr)
		}
		if r := p.rt.result(t); r.Outcome != runtimev1.OutcomeCompleted {
			t.Fatalf("result = %+v\nstderr:\n%s", r, run.stderr)
		}
		// The private repository was cloned at the pinned commit with exactly the
		// issued credential.
		if head := gitRun(t, p.dir("workspace"), "rev-parse", "HEAD"); head != p.commit {
			t.Fatalf("HEAD = %s, want %s", head, p.commit)
		}
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte(jcUser+":"+jcPass))
		p.gs.mu.Lock()
		authorized := 0
		for _, a := range p.gs.auths {
			if a == want {
				authorized++
			} else if a != "" {
				t.Errorf("the Git server saw an unexpected Authorization header")
			}
		}
		p.gs.mu.Unlock()
		if authorized == 0 {
			t.Fatal("the Git server never saw the delivered credential")
		}
		// The checkout material reaches nothing else the entrypoint or the
		// command can write or send.
		forms := jcForms(jcUser, jcPass)
		jcNoLeak(t, "entrypoint stdout", []byte(run.stdout), forms)
		jcNoLeak(t, "entrypoint stderr", []byte(run.stderr), forms)
		jcNoLeak(t, "runtime API traffic and uploads", p.rt.everything(), forms)
		for path, b := range jcWalk(t, p.dir("workspace")) {
			jcNoLeak(t, path, b, forms)
		}
		for path, b := range jcWalk(t, p.dir("identity")) {
			jcNoLeak(t, path, b, forms)
		}
		env, ok := p.rt.object("env")
		if !ok {
			t.Fatal("the command's environment was not uploaded")
		}
		jcNoLeak(t, "the command environment and .git/config", env, forms)
		for _, name := range []string{"KODAMA_CHECKOUT_CREDENTIAL_FILE", "KODAMA_GIT_CA_FILE", "GIT_SSL_CAINFO", "extraHeader"} {
			if strings.Contains(string(env), name) {
				t.Errorf("the command inherited %s", name)
			}
		}
		for _, name := range gitKnobLeaks(env, p.dir("workspace")) {
			t.Errorf("the command inherited %s", name)
		}
		// The provider was asked once, for this attempt's committed workspace.
		if n := len(p.provider.calls); n != 1 {
			t.Fatalf("provider calls = %d", n)
		}
		if req := p.provider.calls[0]; req.WorkspaceID != kubetest.Workspace || req.Access != "read" || string(req.Commit) != p.commit {
			t.Fatalf("provider request = %+v", req)
		}
	})

	t.Run("a wrong credential fails the checkout closed and leaks nothing", func(t *testing.T) {
		const wrong = "wrong-password-0000-aaaa"
		p := newJCPod(t, jcOpts{access: runtimev1.AccessRead, script: `touch "$KODAMA_WORKSPACE/command-ran"`, issuedPass: wrong})
		run := p.run()
		if run.exit != ExitOK {
			t.Fatalf("exit = %d\n%s", run.exit, run.stderr)
		}
		wantReason(t, p.rt.result(t), ReasonCheckoutFailed)
		if _, err := os.Stat(filepath.Join(p.dir("workspace"), "command-ran")); err == nil {
			t.Fatal("the command ran after a failed checkout")
		}
		forms := append(jcForms(jcUser, wrong), jcForms(jcUser, jcPass)...)
		jcNoLeak(t, "stdout", []byte(run.stdout), forms)
		jcNoLeak(t, "stderr", []byte(run.stderr), forms)
		jcNoLeak(t, "runtime API traffic and uploads", p.rt.everything(), forms)
	})

	t.Run("a Job without the credential argument cannot read the private repository", func(t *testing.T) {
		p := newJCPod(t, jcOpts{access: runtimev1.AccessRead, script: `exit 0`})
		run := p.run("--checkout-credential-file=")
		if run.exit != ExitOK {
			t.Fatalf("exit = %d\n%s", run.exit, run.stderr)
		}
		wantReason(t, p.rt.result(t), ReasonCheckoutFailed)
	})

	t.Run("a malformed stored credential fails closed", func(t *testing.T) {
		p := newJCPod(t, jcOpts{access: runtimev1.AccessRead, script: `exit 0`})
		bad := filepath.Join(p.dir("bootstrap"), kube.KeyCheckoutCredential)
		_ = os.Remove(bad) // projected files are read-only
		if err := os.WriteFile(bad, []byte("no-colon-credential-value"), 0o440); err != nil {
			t.Fatal(err)
		}
		run := p.run()
		if run.exit != ExitOK {
			t.Fatalf("exit = %d\n%s", run.exit, run.stderr)
		}
		wantReason(t, p.rt.result(t), ReasonCheckoutFailed)
		jcNoLeak(t, "stderr", []byte(run.stderr), []string{"no-colon-credential-value"})
	})

	t.Run("an attempt without a workspace has no checkout material and no checkout arguments", func(t *testing.T) {
		p := newJCPod(t, jcOpts{script: `test ! -e "$KODAMA_WORKSPACE/.git"`})
		for k := range p.secret.Data {
			if k == kube.KeyCheckoutCredential || k == kube.KeyGitCA {
				t.Fatalf("the Secret of a no-workspace attempt carries %s", k)
			}
		}
		for _, a := range p.args {
			if strings.HasPrefix(a, "--checkout-credential-file") || strings.HasPrefix(a, "--git-ca-file") {
				t.Fatalf("unexpected argument %s", a)
			}
		}
		if len(p.provider.calls) != 0 {
			t.Fatal("the provider was asked for an attempt without source")
		}
		run := p.run()
		if run.exit != ExitOK || p.rt.result(t).Outcome != runtimev1.OutcomeCompleted {
			t.Fatalf("exit = %d, result = %+v\n%s", run.exit, p.rt.result(t), run.stderr)
		}
	})
}

func Test_job_git_ca(t *testing.T) {
	t.Run("the delivered Git CA verifies the TLS Git server", func(t *testing.T) {
		p := newJCPod(t, jcOpts{access: runtimev1.AccessRead, script: `exit 0`})
		ca, _ := os.ReadFile(p.gs.CAFile)
		if got := p.secret.Data[kube.KeyGitCA]; string(got) != string(ca) {
			t.Fatal("the Secret does not carry the issued Git CA")
		}
		run := p.run()
		if run.exit != ExitOK || p.rt.result(t).Outcome != runtimev1.OutcomeCompleted {
			t.Fatalf("exit = %d, result = %+v\n%s", run.exit, p.rt.result(t), run.stderr)
		}
	})

	t.Run("without the Git CA the system roots reject the server", func(t *testing.T) {
		p := newJCPod(t, jcOpts{access: runtimev1.AccessRead, script: `exit 0`})
		run := p.run("--git-ca-file=")
		if run.exit != ExitOK {
			t.Fatalf("exit = %d\n%s", run.exit, run.stderr)
		}
		wantReason(t, p.rt.result(t), ReasonCheckoutFailed)
	})

	t.Run("a CA that did not sign the server is refused", func(t *testing.T) {
		// httptest servers share one certificate, so the foreign CA is an unrelated
		// throwaway authority.
		p := newJCPod(t, jcOpts{access: runtimev1.AccessRead, script: `exit 0`, issuedCA: kubetest.CA(t)})
		run := p.run()
		if run.exit != ExitOK {
			t.Fatalf("exit = %d\n%s", run.exit, run.stderr)
		}
		wantReason(t, p.rt.result(t), ReasonCheckoutFailed)
		// A failed handshake cannot have shown the server the credential.
		p.gs.mu.Lock()
		defer p.gs.mu.Unlock()
		if len(p.gs.auths) != 0 {
			t.Fatalf("the Git server saw %d requests through a CA that does not verify it", len(p.gs.auths))
		}
	})
}

// sha256hex is shared by the secret-directory test.
func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
