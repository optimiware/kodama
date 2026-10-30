package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitpush"
	"github.com/optimiware/kodama/internal/gitpush/gitpushtest"
	gitpushhttp "github.com/optimiware/kodama/internal/gitpush/http"
	"github.com/optimiware/kodama/internal/gitservice"
)

const (
	workerToken  = "tok-worker"
	controlToken = "control-secret"
)

var (
	hookOnce sync.Once
	hookPath string
	hookErr  error
)

// hookBinary builds the minimal hook binary once (what `kodama git-hook` does):
// the worker's hook shims run `<binary> git-hook <name>`.
func hookBinary(t *testing.T) string {
	t.Helper()
	hookOnce.Do(func() {
		dir, err := os.MkdirTemp("", "gitpush-hook-")
		if err != nil {
			hookErr = err
			return
		}
		path := filepath.Join(dir, "kodama")
		build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-buildvcs=false", "-o", path,
			"github.com/optimiware/kodama/internal/gitservice/internal/hookmain")
		build.Env = append(os.Environ(), "GOFLAGS=")
		if out, err := build.CombinedOutput(); err != nil {
			hookErr = fmt.Errorf("building the hook binary: %v\n%s", err, out)
			return
		}
		hookPath = path
	})
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	return hookPath
}

// verifier answers the worker's two identity questions: forwarding assertions
// (the control plane's Git route would sign them) and the control plane's calls
// to the worker RPC.
type verifier struct {
	mu     sync.Mutex
	tokens map[string]gitv1.ForwardingAssertion
}

func (v *verifier) VerifyForwarding(_ context.Context, token string, _ domain.TenantID, _ time.Time) (gitv1.ForwardingAssertion, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if a, ok := v.tokens[token]; ok {
		return a, nil
	}
	return gitv1.ForwardingAssertion{}, &gitv1.ContractError{Code: domain.CodeInvalidCredential, Message: "bad signature"}
}

func (*verifier) VerifyControlCall(_ context.Context, r *http.Request) error {
	if r.Header.Get("Authorization") != "Bearer "+controlToken {
		return &gitv1.ContractError{Code: domain.CodeInvalidCredential, Message: "no"}
	}
	return nil
}

// workerClient is the control plane's read access to the real worker: the
// evidence subset of repository.Hosts over the worker RPC.
type workerClient struct{ base string }

var _ gitpush.Worker = workerClient{}

func rpc[T any, P interface {
	*T
	Validate() error
}](c workerClient, method, path string, in any) (T, error) {
	var zero T
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return zero, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, c.base+gitv1.WorkerBasePath+path, body)
	if err != nil {
		return zero, err
	}
	req.Header.Set("Authorization", "Bearer "+controlToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return zero, err
	}
	if resp.StatusCode != http.StatusOK {
		return zero, fmt.Errorf("worker answered %d: %s", resp.StatusCode, data)
	}
	return gitv1.DecodeResponse[T, P](data)
}

func (c workerClient) ReadRefValues(_ context.Context, _ domain.TenantID, req gitv1.ReadRefValuesRequest) (gitv1.ReadRefValuesResponse, error) {
	return rpc[gitv1.ReadRefValuesResponse](c, http.MethodPost, "/repositories/"+req.RepositoryID+"/refs/values", req)
}

func (c workerClient) ReadJournal(_ context.Context, _ domain.TenantID, req gitv1.ReadJournalRequest) (gitv1.ReadJournalResponse, error) {
	return rpc[gitv1.ReadJournalResponse](c, http.MethodPost, "/repositories/"+req.RepositoryID+"/journal", req)
}

func (c workerClient) ReceiveLiveness(_ context.Context, _ domain.TenantID, repo, receive string) (gitv1.ReceiveLivenessResponse, error) {
	return rpc[gitv1.ReceiveLivenessResponse](c, http.MethodGet, "/repositories/"+repo+"/receives/"+receive, nil)
}

// rig is the composition under test: the real worker, the real control-plane
// handlers over the push service of a gitpushtest fixture, and a Git client.
type rig struct {
	t        *testing.T
	f        *gitpushtest.Fixture
	worker   *gitservice.Service
	workerAt string
	root     string
	verifier *verifier
	client   workerClient
	home     string
	seq      int

	// dropOutcome makes the control plane lose every outcome confirmation (the
	// call fails before it reaches the push service).
	dropOutcome atomic.Bool
}

func newRig(t *testing.T, open gitpushtest.Open) *rig {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	f := gitpushtest.New(t, open)
	r := &rig{t: t, f: f, verifier: &verifier{tokens: map[string]gitv1.ForwardingAssertion{}}, home: t.TempDir()}

	identity := func() gitv1.WorkerIdentity {
		now := time.Now().UTC()
		ts := func(t time.Time) gitv1.Timestamp { return gitv1.Timestamp(t.Format("2006-01-02T15:04:05Z")) }
		return gitv1.WorkerIdentity{Audience: gitv1.ControlAudience, TenantID: string(gitpushtest.TenantA), WorkerInstance: "worker-1",
			IssuedAt: ts(now.Add(-time.Minute)), ExpiresAt: ts(now.Add(30 * time.Minute))}
	}
	handler, err := gitpushhttp.NewWorkerHandler(f.Svc, identityVerifier{tokens: map[string]func() gitv1.WorkerIdentity{workerToken: identity}}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	handler.Register(mux)
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if r.dropOutcome.Load() && strings.HasSuffix(req.URL.Path, "/outcome") {
			http.Error(w, "the confirmation is lost", http.StatusServiceUnavailable)
			return
		}
		mux.ServeHTTP(w, req)
	}))
	t.Cleanup(control.Close)

	cp, err := gitservice.NewControlPlaneClient(control.URL, gitpushtest.TenantA, func(context.Context) (string, error) { return workerToken, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.root = filepath.Join(t.TempDir(), "data")
	svc, err := gitservice.New(gitservice.Config{
		TenantID: gitpushtest.TenantA, DataRoot: r.root, ControlPlane: cp, Assertions: r.verifier, Callers: r.verifier, HookBinary: hookBinary(t),
		Limits: gitservice.Limits{GateWait: 3 * time.Second, ControlTimeout: 10 * time.Second, LockWait: 300 * time.Millisecond},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(svc.Handler())
	t.Cleanup(func() {
		srv.Close()
		_ = svc.Close()
	})
	r.worker, r.workerAt = svc, srv.URL
	r.client = workerClient{base: srv.URL}
	f.UseWorker(r.client)
	return r
}

type identityVerifier struct {
	tokens map[string]func() gitv1.WorkerIdentity
}

func (v identityVerifier) VerifyWorkerIdentity(_ context.Context, token string, pathTenant domain.TenantID, now time.Time) (gitv1.WorkerIdentity, error) {
	mk, ok := v.tokens[token]
	if !ok {
		return gitv1.WorkerIdentity{}, fmt.Errorf("unknown token")
	}
	id := mk()
	if err := gitv1.CheckWorkerCall(id, string(pathTenant), now); err != nil {
		return gitv1.WorkerIdentity{}, err
	}
	return id, nil
}

// initRepo initializes the repository on the worker through its RPC.
func (r *rig) initRepo(id string) {
	r.t.Helper()
	_, err := rpc[gitv1.InitializeRepositoryResponse](r.client, http.MethodPost, "/repositories/"+id+"/initialize",
		gitv1.InitializeRepositoryRequest{RepositoryID: id, DefaultBranch: "main"})
	if err != nil {
		r.t.Fatal(err)
	}
}

// token mints the forwarding assertion the control plane's Git route would send
// for the grant.
func (r *rig) token(repo, grant string) string {
	r.seq++
	now := time.Now().UTC()
	a := gitv1.ForwardingAssertion{
		Audience: gitv1.WorkerAudience(string(gitpushtest.TenantA)), TenantID: string(gitpushtest.TenantA), RepositoryID: repo, GrantID: grant, ReceivePack: true,
		IssuedAt:  gitv1.Timestamp(now.Add(-time.Second).Format(time.RFC3339Nano)),
		ExpiresAt: gitv1.Timestamp(now.Add(4 * time.Minute).Format(time.RFC3339Nano)),
		RequestID: fmt.Sprintf("req_%d", r.seq),
	}
	tok := fmt.Sprintf("fwd-%d", r.seq)
	r.verifier.mu.Lock()
	r.verifier.tokens[tok] = a
	r.verifier.mu.Unlock()
	return tok
}

func (r *rig) repoDir(id string) string { return filepath.Join(r.root, "repositories", id+".git") }

func (r *rig) url(repo string) string { return r.workerAt + "/" + repo + ".git" }

func (r *rig) env() []string {
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + r.home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=Dev", "GIT_AUTHOR_EMAIL=dev@example.invalid",
		"GIT_COMMITTER_NAME=Dev", "GIT_COMMITTER_EMAIL=dev@example.invalid", "LC_ALL=C"}
}

func (r *rig) git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = r.env()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (r *rig) mustGit(dir string, args ...string) string {
	r.t.Helper()
	out, err := r.git(dir, args...)
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(out)
}

// push runs git push with the grant's forwarding assertion.
func (r *rig) push(dir, repo, grant string, args ...string) (string, error) {
	return r.git(dir, append([]string{"-c", "http.extraHeader=" + gitservice.AssertionHeader + ": " + r.token(repo, grant), "push", r.url(repo)}, args...)...)
}

// workdir creates a local repository with one commit.
func (r *rig) workdir() string {
	dir := r.t.TempDir()
	r.mustGit(dir, "init", "-q", "-b", "main")
	r.commit(dir, "README", "hello\n")
	return dir
}

func (r *rig) commit(dir, name, content string) string {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
	r.mustGit(dir, "add", name)
	r.mustGit(dir, "commit", "-q", "-m", "add "+name)
	return r.mustGit(dir, "rev-parse", "HEAD")
}

// prefill writes refs into the bare repository directly, bypassing the worker's
// hooks (a push the worker did not start is refused by them), so a test starts
// from a repository that has refs. The journal stays empty.
func (r *rig) prefill(repo, dir string, refspecs ...string) {
	r.t.Helper()
	r.mustGit(dir, append([]string{"push", "-q", "--receive-pack=git -c core.hooksPath=" + os.DevNull + " receive-pack", r.repoDir(repo)}, refspecs...)...)
}

// ref reads a ref of the bare repository; "" when it does not exist.
func (r *rig) ref(repo, ref string) string {
	r.t.Helper()
	return r.mustGit(r.repoDir(repo), "--git-dir="+r.repoDir(repo), "for-each-ref", "--format=%(objectname)", ref)
}

func (r *rig) refs(repo string) string {
	r.t.Helper()
	return r.mustGit(r.repoDir(repo), "--git-dir="+r.repoDir(repo), "for-each-ref", "--format=%(refname)")
}

// records are the push records of the repository, newest first.
func (r *rig) records(repo string) []gitv1.PushRecord {
	r.t.Helper()
	page, err := r.f.Svc.List(context.Background(), "usr_reader", gitpushtest.ScopeA(), repo, gitpush.ListQuery{Limit: 100})
	if err != nil {
		r.t.Fatal(err)
	}
	out := make([]gitv1.PushRecord, len(page))
	for i, p := range page {
		out[i] = p.Record
	}
	return out
}

func (r *rig) events(repo string) int {
	return len(r.f.Env.Events(r.t, gitpushtest.ScopeA(), repo))
}

func (r *rig) journal(repo, receive string) []gitv1.JournalEntry {
	r.t.Helper()
	resp, err := r.client.ReadJournal(context.Background(), gitpushtest.TenantA, gitv1.ReadJournalRequest{RepositoryID: repo, ReceiveID: &receive, Limit: 100})
	if err != nil {
		r.t.Fatal(err)
	}
	return resp.Entries
}

// pushID extracts the push ID the worker prints to the client.
func pushID(output string) string {
	const marker = "kodama-push-id: "
	for _, line := range strings.Split(output, "\n") {
		if _, rest, ok := strings.Cut(line, marker); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}
