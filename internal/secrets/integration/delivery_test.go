// Package integration_test exercises secret delivery through the merged
// `kodama step` entrypoint (T-016) via its exported interfaces: the Runtime and
// Uploader ports, real local processes and a real Git repository served over
// TLS. Secret resolution goes through the module's real Resolver and runtime
// adapter; the encrypted store itself is covered by the PostgreSQL tests, so no
// PostgreSQL, NATS or S3 is needed here.
package integration_test

import (
	"bytes"
	"context"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http/cgi"
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
	"github.com/optimiware/kodama/internal/entrypoint"
	"github.com/optimiware/kodama/internal/secrets"
	secretsruntime "github.com/optimiware/kodama/internal/secrets/runtime"
)

const (
	imageRef = "registry.example.invalid/step@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	attempt  = domain.AttemptID("att_it01")
	branch   = "refs/heads/work"
	// secretValue is the sentinel every check looks for.
	secretValue = "delivery-SENTINEL-8e41c07b"
)

var scope = domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}

// ---- Git ----

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed; Git-backed delivery checks skipped")
	}
}

func git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=human", "GIT_AUTHOR_EMAIL=human@example.invalid", "GIT_COMMITTER_NAME=human", "GIT_COMMITTER_EMAIL=human@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// repo is a bare repository with branch "work" at one commit, served over TLS
// through git-http-backend.
type repo struct {
	url, caFile, commit, bare string
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	requireGit(t)
	root, work := t.TempDir(), t.TempDir()
	git(t, work, "init", "-q", "-b", "work")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("project\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "one")
	commit := git(t, work, "rev-parse", "HEAD")
	bare := filepath.Join(root, "repo.git")
	git(t, ".", "clone", "-q", "--bare", work, bare)
	git(t, bare, "config", "http.uploadpack", "true")
	git(t, bare, "config", "uploadpack.allowAnySHA1InWant", "true")

	backend := filepath.Join(git(t, ".", "--exec-path"), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git-http-backend is not available; Git-backed delivery checks skipped")
	}
	h := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return &repo{url: srv.URL + "/repo.git", caFile: ca, commit: commit, bare: bare}
}

// ---- secret resolution through the module's real resolver and adapter ----

// vault stands in for the version source: values by reference, with failures
// injectable per reference, and a count of delivered (audited) resolutions.
type vault struct {
	mu     sync.Mutex
	values map[string]string
	errs   map[string]error
	opened []string
	fin    []string
}

type vaultValue struct {
	v   *vault
	ref string
	val []byte
}

func (v *vault) Open(_ context.Context, req secrets.VersionRequest) (secrets.PendingValue, error) {
	ref := secrets.FormatSecretRef(req.SecretName)
	v.mu.Lock()
	defer v.mu.Unlock()
	v.opened = append(v.opened, ref)
	if err := v.errs[ref]; err != nil {
		return nil, err
	}
	val, ok := v.values[ref]
	if !ok {
		return nil, secrets.ErrNotFound
	}
	return &vaultValue{v: v, ref: ref, val: []byte(val)}, nil
}

func (p *vaultValue) Value() []byte { return p.val }
func (p *vaultValue) Discard()      { clear(p.val) }
func (p *vaultValue) Finalize(_ context.Context, info secrets.FinalizeInfo) error {
	p.v.mu.Lock()
	defer p.v.mu.Unlock()
	p.v.fin = append(p.v.fin, p.ref+"/"+string(info.Delivery))
	return nil
}

// grantAuthority declares exactly the context's secret bindings.
type grantAuthority struct{ c *runtimev1.Context }

func (g grantAuthority) Grant(_ context.Context, id domain.AttemptID) (secrets.AttemptGrant, error) {
	return secrets.AttemptGrant{Scope: g.c.Scope(), RunID: g.c.RunID, AttemptID: id, Bindings: g.c.Secrets}, nil
}

// ---- the runtime fake (the Runtime and Uploader ports) ----

type artifact struct {
	req   runtimev1.ArtifactReserveRequest
	bytes []byte
}

type runtimeFake struct {
	t       *testing.T
	ctx     runtimev1.Context
	adapter *secretsruntime.Adapter

	mu        sync.Mutex
	artifacts map[domain.ArtifactID]*artifact
	order     []domain.ArtifactID
	proposals []runtimev1.SourceProposalRequest
	results   []runtimev1.ResultRequest
}

func (r *runtimeFake) token() runtimev1.TokenInfo {
	at := runtimev1.Timestamp(time.Now().Add(time.Hour).UTC().Format("2006-01-02T15:04:05Z"))
	return runtimev1.TokenInfo{Token: "tok-0123456789abcdef", TokenType: "Bearer", Audience: "kodama-runtime", ExpiresAt: at, MaxExpiresAt: at}
}

func (r *runtimeFake) Admit(context.Context, runtimev1.AdmitRequest) (runtimev1.AdmitResponse, error) {
	return runtimev1.AdmitResponse{Token: r.token(), Context: r.ctx}, nil
}
func (r *runtimeFake) RenewToken(context.Context) (runtimev1.TokenResponse, error) {
	return runtimev1.TokenResponse{Token: r.token()}, nil
}
func (r *runtimeFake) Context(context.Context) (runtimev1.Context, error) { return r.ctx, nil }

func (r *runtimeFake) ResolveSecret(ctx context.Context, req runtimev1.SecretResolveRequest) (runtimev1.SecretResolveResponse, error) {
	resp, err := secretsruntime.Bound{Adapter: r.adapter, Attempt: attempt, RequestID: "req_it"}.ResolveSecret(ctx, req)
	if err != nil {
		ce := err.(*runtimev1.ContractError)
		status, _ := runtimev1.Status(ce.Code)
		return runtimev1.SecretResolveResponse{}, &entrypoint.APIError{Op: "resolveSecret", Status: status, Code: ce.Code, Message: ce.Message}
	}
	return resp, nil
}

func (r *runtimeFake) ReserveArtifact(_ context.Context, req runtimev1.ArtifactReserveRequest) (runtimev1.ArtifactReserveResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := domain.ArtifactID(fmt.Sprintf("art_%d", len(r.order)+1))
	r.artifacts[id] = &artifact{req: req}
	r.order = append(r.order, id)
	up := runtimev1.Upload{Method: "PUT", URL: "https://upload.invalid/" + string(id), Headers: map[string]string{},
		ExpiresAt: runtimev1.Timestamp(time.Now().Add(time.Hour).UTC().Format("2006-01-02T15:04:05Z"))}
	return runtimev1.ArtifactReserveResponse{ArtifactID: id, State: runtimev1.StateReserved, Upload: &up}, nil
}

func (r *runtimeFake) CompleteArtifact(_ context.Context, id domain.ArtifactID, req runtimev1.ArtifactCompleteRequest) (runtimev1.ArtifactCompleteResponse, error) {
	return runtimev1.ArtifactCompleteResponse{ArtifactID: id, State: runtimev1.StateUsable, Digest: req.Digest, SizeBytes: req.SizeBytes}, nil
}

func (r *runtimeFake) ProposeSource(_ context.Context, req runtimev1.SourceProposalRequest) (runtimev1.SourceProposalResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.proposals = append(r.proposals, req)
	c := req.Commit
	return runtimev1.SourceProposalResponse{Status: runtimev1.PublishAccepted, Commit: req.Commit, AcceptedCommit: &c}, nil
}

func (r *runtimeFake) SubmitResult(_ context.Context, req runtimev1.ResultRequest) (runtimev1.ResultResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.results = append(r.results, req)
	status := runtimev1.AttemptSucceeded
	if req.Result.Outcome != runtimev1.OutcomeCompleted {
		status = runtimev1.AttemptFailed
	}
	return runtimev1.ResultResponse{AttemptID: attempt, ResultHash: req.ResultHash, AttemptStatus: status,
		AcceptedAt: runtimev1.Timestamp(time.Now().UTC().Format("2006-01-02T15:04:05Z"))}, nil
}

// Upload implements entrypoint.Uploader: it keeps the bytes the entrypoint
// would have sent to object storage.
func (r *runtimeFake) Upload(_ context.Context, target runtimev1.Upload, body io.ReaderAt, size int64) error {
	b := make([]byte, size)
	if _, err := body.ReadAt(b, 0); err != nil && err != io.EOF {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	id := domain.ArtifactID(strings.TrimPrefix(target.URL, "https://upload.invalid/"))
	r.artifacts[id].bytes = b
	return nil
}

func (r *runtimeFake) byRole(role runtimev1.ArtifactRole) []*artifact {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*artifact
	for _, id := range r.order {
		if a := r.artifacts[id]; a.req.Role == role {
			out = append(out, a)
		}
	}
	return out
}

// ---- the Pod fixture ----

type pod struct {
	t     *testing.T
	root  string
	cfg   entrypoint.Config
	rt    *runtimeFake
	vault *vault
	logs  *syncBuffer
	repo  *repo
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// newPod lays out a Pod and a context running script as a writer on the repo.
func newPod(t *testing.T, r *repo, script string, bindings ...runtimev1.SecretBinding) *pod {
	t.Helper()
	root := t.TempDir()
	p := &pod{t: t, root: root, logs: &syncBuffer{}, repo: r}
	for _, d := range []string{"scratch", "identity", "outputs"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "identity", "admission"), []byte("admission-credential-0123456789\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, tip := branch, runtimev1.GitCommit(r.commit)
	c := runtimev1.Context{
		TenantID: scope.TenantID, ProjectID: scope.ProjectID, RunID: "run_01", StepID: "build", AttemptID: attempt, ActionKey: "run_01.build",
		Definition: runtimev1.Definition{ID: "pd_01", Digest: runtimev1.Digest("sha256:" + strings.Repeat("d", 64))},
		Command:    runtimev1.Command{Image: imageRef, Command: []string{"/bin/sh", "-c"}, Args: []string{script}, Env: map[string]string{}},
		Workspace: runtimev1.Workspace{Access: runtimev1.AccessWrite, Source: &runtimev1.Source{
			RepositoryURL: r.url, Commit: runtimev1.GitCommit(r.commit), Branch: &b, ExpectedTip: &tip}},
		Inputs: map[string]runtimev1.Value{}, Outputs: []runtimev1.OutputDecl{}, Secrets: bindings,
		Limits: runtimev1.Limits{TimeoutSeconds: 60, MaxOutputBytes: 1 << 20, MaxOutputFiles: 10, MaxDiagnosticBytes: 64 << 10},
	}
	if c.Secrets == nil {
		c.Secrets = []runtimev1.SecretBinding{}
	}
	p.vault = &vault{values: map[string]string{}, errs: map[string]error{}}
	p.rt = &runtimeFake{t: t, ctx: c, artifacts: map[domain.ArtifactID]*artifact{}}
	// The authority reads the live context, so bindings added by a test are the
	// declared ones.
	resolver, err := secrets.NewResolver(grantAuthority{&p.rt.ctx}, p.vault)
	if err != nil {
		t.Fatal(err)
	}
	if p.rt.adapter, err = secretsruntime.NewAdapter(resolver); err != nil {
		t.Fatal(err)
	}
	p.cfg = entrypoint.Config{
		ServerURL: "https://runtime.invalid", AttemptID: string(attempt),
		AdmissionCredentialFile: filepath.Join(root, "identity", "admission"),
		CheckoutCredentialFile:  filepath.Join(root, "identity", "checkout-credential"),
		TokenFile:               filepath.Join(root, "identity", "token"),
		WorkspaceDir:            filepath.Join(root, "workspace"),
		OutputsDir:              filepath.Join(root, "outputs"),
		SecretsDir:              filepath.Join(root, "secrets"),
		IdentityDir:             filepath.Join(root, "identity"),
		ResultsFile:             filepath.Join(root, "results.json"),
		ScratchDir:              filepath.Join(root, "scratch"),
		GitCAFile:               r.caFile,
		Job:                     runtimev1.JobIdentity{Name: "job-1", UID: "0b1d2c3e-4f50-4a61-8b72-93a4b5c6d7e8", PodName: "pod-1", PodUID: "1c2d3e4f-5061-4a72-8b83-a4b5c6d7e8f9"},
		ChildEnv:                map[string]string{"PATH": os.Getenv("PATH"), "HOME": filepath.Join(root, "home")},
		Retry:                   entrypoint.RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond},
		KillGrace:               300 * time.Millisecond,
		FinalizeTimeout:         30 * time.Second,
		SettleTimeout:           15 * time.Second,
		SettleBackoff:           5 * time.Millisecond,
		RenewMinInterval:        20 * time.Millisecond,
		InterruptTimeout:        10 * time.Second,
		Logger:                  slog.New(slog.NewJSONHandler(p.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	return p
}

func (p *pod) bind(name, ref string, delivery runtimev1.SecretDelivery, value string) {
	p.t.Helper()
	p.rt.ctx.Secrets = append(p.rt.ctx.Secrets, runtimev1.SecretBinding{Ref: ref, Version: 1, Name: name, Delivery: delivery})
	p.vault.values[ref] = value
}

func (p *pod) output(name, path string) {
	pth := path
	p.rt.ctx.Outputs = append(p.rt.ctx.Outputs, runtimev1.OutputDecl{Name: name, Type: runtimev1.OutputArtifact, Path: &pth})
}

func (p *pod) run() int {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	// The resolver reads the context's bindings at call time.
	return entrypoint.NewStep(p.cfg, p.rt, p.rt).Run(ctx)
}

func (p *pod) result() runtimev1.Result {
	p.t.Helper()
	p.rt.mu.Lock()
	defer p.rt.mu.Unlock()
	if len(p.rt.results) != 1 {
		p.t.Fatalf("%d results were submitted\nlogs:\n%s", len(p.rt.results), p.logs.String())
	}
	return p.rt.results[0].Result
}

// secretFilesLeft lists what remains in the secrets directory.
func (p *pod) secretFilesLeft() []string {
	entries, err := os.ReadDir(p.cfg.SecretsDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// everything returns every byte the Pod uploaded or proposed.
func (p *pod) uploaded() []byte {
	var b []byte
	p.rt.mu.Lock()
	defer p.rt.mu.Unlock()
	for _, id := range p.rt.order {
		b = append(b, p.rt.artifacts[id].bytes...)
	}
	return b
}

func wantReason(t *testing.T, r runtimev1.Result, reason string) {
	t.Helper()
	if r.Outcome != runtimev1.OutcomeFailed || r.Reason == nil || *r.Reason != reason {
		t.Fatalf("result %+v reason %v, want failed/%s", r, deref(r.Reason), reason)
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// bundleFiles unbundles a proposed source bundle into a clone of the original
// repository and returns the files of the candidate commit plus whether any
// object of it contains needle.
func bundleFiles(t *testing.T, r *repo, bundle []byte, needle string) (files []string, leaked bool) {
	t.Helper()
	dir := t.TempDir()
	clone := filepath.Join(dir, "clone")
	git(t, ".", "clone", "-q", r.bare, clone)
	bpath := filepath.Join(dir, "source.bundle")
	if err := os.WriteFile(bpath, bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	heads := git(t, clone, "bundle", "unbundle", bpath)
	fields := strings.Fields(heads)
	if len(fields) == 0 {
		t.Fatalf("the bundle holds no commit: %q", heads)
	}
	candidate := fields[0]
	files = strings.Fields(git(t, clone, "ls-tree", "-r", "--name-only", candidate))
	// Every object reachable from the candidate, not only the current tree.
	cmd := exec.Command("git", "grep", "-F", "-l", "--", needle, candidate)
	cmd.Dir = clone
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.Output()
	if err == nil && len(out) > 0 {
		leaked = true
	}
	for _, obj := range strings.Fields(git(t, clone, "rev-list", "--objects", candidate+"^.."+candidate)) {
		if len(obj) != 40 {
			continue
		}
		raw := exec.Command("git", "cat-file", "-p", obj)
		raw.Dir = clone
		if b, err := raw.Output(); err == nil && bytes.Contains(b, []byte(needle)) {
			leaked = true
		}
	}
	return files, leaked
}

const deliveryScript = `set -eu
f="$KODAMA_SECRETS_DIR/erp-token"
test -f "$f"
case "$KODAMA_SECRETS_DIR" in "$KODAMA_WORKSPACE"|"$KODAMA_WORKSPACE"/*|"$KODAMA_OUTPUTS_DIR"|"$KODAMA_OUTPUTS_DIR"/*) echo inside >&2; exit 9;; esac
[ "$(cat "$f")" = "$ERP_ENV" ] || exit 8
printf 'file=%s dir=%s size=%s\n' "$(ls -ld "$f" | cut -c1-10)" "$(ls -ld "$KODAMA_SECRETS_DIR" | cut -c1-10)" "$(wc -c < "$f" | tr -d ' ')" > report.txt
echo change > change.txt
echo ok > "$KODAMA_OUTPUTS_DIR/out.txt"
`

func twoSecrets(p *pod) {
	p.bind("erp-token", "secret://project/erp-token", runtimev1.DeliveryFile, secretValue)
	p.bind("erp-env", "secret://project/erp-env", runtimev1.DeliveryEnv, secretValue)
}

// Test_secret_file_exclusion covers VS-05, VS-11, VS-A02 and the delivery half
// of VS-A08: secrets are delivered as separate protected files outside the
// source and output roots, are removed afterwards, and never enter the source
// bundle or any uploaded artifact. It checks file placement and exclusion only:
// command diagnostics (access and redaction of what a command prints) are a
// separate control and are not part of this guarantee.
func Test_secret_file_exclusion(t *testing.T) {
	r := newRepo(t)

	t.Run("delivered as protected files, cleaned up, excluded from the bundle and uploads", func(t *testing.T) {
		p := newPod(t, r, deliveryScript)
		twoSecrets(p)
		p.output("out", "/kodama/outputs/out.txt")
		if code := p.run(); code != entrypoint.ExitOK {
			t.Fatalf("exit %d\nlogs:\n%s", code, p.logs.String())
		}
		res := p.result()
		if res.Outcome != runtimev1.OutcomeCompleted || res.SourceCommit == nil {
			t.Fatalf("result %+v reason %v\nlogs:\n%s", res, deref(res.Reason), p.logs.String())
		}
		// Only the declared references were requested, once each, and each was audited.
		if got := strings.Join(p.vault.opened, ","); got != "secret://project/erp-token,secret://project/erp-env" {
			t.Fatalf("resolutions: %s", got)
		}
		if got := strings.Join(p.vault.fin, ","); got != "secret://project/erp-token/file,secret://project/erp-env/env" {
			t.Fatalf("audited deliveries: %s", got)
		}
		// Files were separate, protected and outside the source/output roots (the command asserted placement).
		bundles := p.rt.byRole(runtimev1.RoleBundle)
		if len(bundles) != 1 || len(p.rt.proposals) != 1 {
			t.Fatalf("bundles %d proposals %d", len(bundles), len(p.rt.proposals))
		}
		files, leaked := bundleFiles(t, r, bundles[0].bytes, secretValue)
		if leaked {
			t.Fatal("the source bundle contains the injected secret")
		}
		want := map[string]bool{"change.txt": true, "report.txt": true}
		for _, f := range files {
			delete(want, f)
			if strings.Contains(f, "erp-token") || strings.Contains(f, "secrets") {
				t.Errorf("the source change contains %q", f)
			}
		}
		if len(want) != 0 {
			t.Fatalf("the legitimate changes are missing from the bundle: %v (files %v)", want, files)
		}
		clone := filepath.Join(t.TempDir(), "c")
		git(t, ".", "clone", "-q", r.bare, clone)
		bpath := filepath.Join(t.TempDir(), "b.bundle")
		_ = os.WriteFile(bpath, bundles[0].bytes, 0o600)
		heads := strings.Fields(git(t, clone, "bundle", "unbundle", bpath))
		report := git(t, clone, "show", heads[0]+":report.txt")
		if report != "file=-rw------- dir=drwx------ size="+fmt.Sprint(len(secretValue)) {
			t.Fatalf("delivery evidence from the command: %q", report)
		}
		// Nothing uploaded carries the value in the clear.
		if bytes.Contains(p.uploaded(), []byte(secretValue)) {
			t.Fatal("an uploaded artifact contains the injected secret")
		}
		outs := p.rt.byRole(runtimev1.RoleOutput)
		if len(outs) != 1 || strings.TrimSpace(string(outs[0].bytes)) != "ok" {
			t.Fatalf("outputs: %d", len(outs))
		}
		// Cleanup: no secret file survives the step.
		if left := p.secretFilesLeft(); len(left) != 0 {
			t.Fatalf("secret files left behind: %v", left)
		}
		if _, err := os.Stat(filepath.Join(p.cfg.WorkspaceDir, "erp-token")); err == nil {
			t.Fatal("a secret file appeared in the workspace")
		}
	})

	t.Run("a secret copied into the workspace is refused and nothing is published", func(t *testing.T) {
		p := newPod(t, r, `set -eu
cp "$KODAMA_SECRETS_DIR/erp-token" leak.txt
`)
		twoSecrets(p)
		if code := p.run(); code != entrypoint.ExitOK {
			t.Fatalf("exit %d\nlogs:\n%s", code, p.logs.String())
		}
		wantReason(t, p.result(), entrypoint.ReasonSecretInSource)
		if len(p.rt.proposals) != 0 || len(p.rt.byRole(runtimev1.RoleBundle)) != 0 {
			t.Fatal("a bundle containing a secret was uploaded or proposed")
		}
		if bytes.Contains(p.uploaded(), []byte(secretValue)) {
			t.Fatal("an uploaded artifact contains the injected secret")
		}
		if left := p.secretFilesLeft(); len(left) != 0 {
			t.Fatalf("secret files left behind: %v", left)
		}
	})

	t.Run("output paths cannot reach the injected files", func(t *testing.T) {
		for name, link := range map[string]string{
			"symlink":   `ln -s "$KODAMA_SECRETS_DIR/erp-token" "$KODAMA_OUTPUTS_DIR/report.bin"`,
			"hard link": `ln "$KODAMA_SECRETS_DIR/erp-token" "$KODAMA_OUTPUTS_DIR/report.bin"`,
		} {
			t.Run(name, func(t *testing.T) {
				p := newPod(t, r, "set -eu\n"+link+"\n")
				twoSecrets(p)
				p.output("report", "/kodama/outputs/report.bin")
				if code := p.run(); code != entrypoint.ExitOK {
					t.Fatalf("exit %d\nlogs:\n%s", code, p.logs.String())
				}
				wantReason(t, p.result(), entrypoint.ReasonOutputInvalid)
				if len(p.rt.byRole(runtimev1.RoleOutput)) != 0 {
					t.Fatal("an output artifact was uploaded for an escaping path")
				}
				if bytes.Contains(p.uploaded(), []byte(secretValue)) {
					t.Fatal("an uploaded artifact contains the injected secret")
				}
				if left := p.secretFilesLeft(); len(left) != 0 {
					t.Fatalf("secret files left behind: %v", left)
				}
			})
		}
	})

	t.Run("failures and timeouts still clean up", func(t *testing.T) {
		p := newPod(t, r, `[ -f "$KODAMA_SECRETS_DIR/erp-token" ] && exit 3`)
		twoSecrets(p)
		if code := p.run(); code != entrypoint.ExitOK {
			t.Fatalf("exit %d", code)
		}
		wantReason(t, p.result(), entrypoint.ReasonExitNonzero)
		if left := p.secretFilesLeft(); len(left) != 0 {
			t.Fatalf("after a failing command: %v", left)
		}

		slow := newPod(t, r, `sleep 30`)
		twoSecrets(slow)
		slow.rt.ctx.Limits.TimeoutSeconds = 1
		if code := slow.run(); code != entrypoint.ExitOK {
			t.Fatalf("exit %d", code)
		}
		wantReason(t, slow.result(), entrypoint.ReasonTimeout)
		if left := slow.secretFilesLeft(); len(left) != 0 {
			t.Fatalf("after a timeout: %v", left)
		}
	})

	t.Run("an unresolvable secret fails the step before the command runs", func(t *testing.T) {
		for name, injected := range map[string]error{
			"revoked": secrets.ErrRevoked, "missing": secrets.ErrNotFound, "not pinned": secrets.ErrNotPinned,
			"key outage": &secrets.KeyError{Class: secrets.KeyUnavailable, Op: "unwrap"},
		} {
			t.Run(name, func(t *testing.T) {
				p := newPod(t, r, `touch "$KODAMA_OUTPUTS_DIR/launched"`)
				p.bind("erp-token", "secret://project/erp-token", runtimev1.DeliveryFile, secretValue)
				p.bind("second", "secret://project/second", runtimev1.DeliveryFile, "other-value")
				p.vault.errs["secret://project/second"] = injected
				if code := p.run(); code != entrypoint.ExitOK {
					t.Fatalf("exit %d\nlogs:\n%s", code, p.logs.String())
				}
				wantReason(t, p.result(), entrypoint.ReasonSecretUnavailable)
				if _, err := os.Stat(filepath.Join(p.cfg.OutputsDir, "launched")); err == nil {
					t.Fatal("the command ran although a declared secret could not be resolved")
				}
				// The first secret had been delivered to disk; it is removed again.
				if left := p.secretFilesLeft(); len(left) != 0 {
					t.Fatalf("a partially delivered secret was left behind: %v", left)
				}
				if strings.Contains(p.logs.String(), secretValue) {
					t.Fatal("the entrypoint logs contain the secret")
				}
			})
		}
	})

	t.Run("undeclared references are never resolved", func(t *testing.T) {
		p := newPod(t, r, `true`)
		p.bind("erp-token", "secret://project/erp-token", runtimev1.DeliveryFile, secretValue)
		p.vault.values["secret://project/other"] = "must-not-be-read"
		if code := p.run(); code != entrypoint.ExitOK {
			t.Fatalf("exit %d", code)
		}
		for _, ref := range p.vault.opened {
			if ref != "secret://project/erp-token" {
				t.Fatalf("an undeclared secret was read: %s", ref)
			}
		}
		// Asking the adapter directly for the undeclared reference is refused.
		_, err := p.rt.adapter.Resolve(context.Background(), attempt, runtimev1.SecretResolveRequest{Ref: "secret://project/other", Version: 1}, "r")
		if ce, ok := err.(*runtimev1.ContractError); !ok || ce.Code != domain.CodeForbidden {
			t.Fatalf("undeclared resolution: %v", err)
		}
	})
}
