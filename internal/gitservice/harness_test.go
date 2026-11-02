package gitservice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// TestMain lets the test binary be the hook binary: the hook shims run
// `<test binary> git-hook <name>`.
func TestMain(m *testing.M) {
	if len(os.Args) >= 3 && os.Args[1] == "git-hook" {
		os.Exit(HookMain(os.Args[2:], os.Stdin, os.Stdout, os.Stderr, os.Environ()))
	}
	if root := os.Getenv(envTestWorker); root != "" {
		os.Exit(runTestWorker(root))
	}
	os.Exit(m.Run())
}

// The worker as a separate process, for tests that kill it: the test binary
// re-executes itself with these variables and serves a worker with the fake
// control plane on a random port, whose address it writes to the address file.
const (
	envTestWorker   = "GITSERVICE_TEST_WORKER_ROOT"
	envTestHook     = "GITSERVICE_TEST_WORKER_HOOK"
	envTestAddrFile = "GITSERVICE_TEST_WORKER_ADDR"
	subprocessToken = "tok-subprocess"
)

func runTestWorker(root string) int {
	now := time.Now().UTC()
	verifier := &fakeVerifier{claims: map[string]gitv1.ForwardingAssertion{subprocessToken: {
		Audience: gitv1.WorkerAudience(testTenant), TenantID: testTenant, RepositoryID: "repo_1", GrantID: "gg_1", ReceivePack: true,
		IssuedAt:  gitv1.Timestamp(now.Add(-time.Second).Format(time.RFC3339Nano)),
		ExpiresAt: gitv1.Timestamp(now.Add(4 * time.Minute).Format(time.RFC3339Nano)),
		RequestID: "req_sub",
	}}}
	cmd, err := ServeCommand(Config{TenantID: testTenant, DataRoot: root, ControlPlane: newFakeControl(), Assertions: verifier, Callers: fakeCallers{},
		HookBinary: os.Getenv(envTestHook)})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := os.WriteFile(os.Getenv(envTestAddrFile), []byte(ln.Addr().String()), 0o600); err != nil {
		return 1
	}
	if err := cmd.Serve(context.Background(), ln); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

var (
	hookBinaryOnce sync.Once
	hookBinaryPath string
)

// testHookBinary builds the minimal hook binary once (internal/hookmain, what
// `kodama git-hook` does), without the race detector: a freshly built
// race-instrumented test binary starts slowly on some systems, and a push runs
// several hooks. It falls back to a copy of the test binary (which serves
// git-hook from TestMain).
func testHookBinary(t *testing.T) string {
	t.Helper()
	hookBinaryOnce.Do(func() {
		dir, err := os.MkdirTemp("", "gitservice-hook-")
		if err != nil {
			return
		}
		path := filepath.Join(dir, "kodama")
		goBin := filepath.Join(runtime.GOROOT(), "bin", "go")
		build := exec.Command(goBin, "build", "-buildvcs=false", "-o", path, "./internal/hookmain")
		build.Env = append(os.Environ(), "GOFLAGS=")
		if out, err := build.CombinedOutput(); err == nil {
			hookBinaryPath = path
			return
		} else {
			t.Logf("building the hook binary failed, using the test binary: %v\n%s", err, out)
		}
		self, err := os.Executable()
		if err != nil {
			return
		}
		if data, err := os.ReadFile(self); err == nil && os.WriteFile(path, data, 0o755) == nil {
			hookBinaryPath = path
		}
	})
	return hookBinaryPath
}

const (
	testTenant   = "t_acme"
	controlToken = "control-secret"
)

// ---- fakes ----

// fakeControl is the control plane: it records every call and answers with
// configurable behavior.
type fakeControl struct {
	mu            sync.Mutex
	authorizeReqs []gitv1.PushAuthorizeRequest
	intents       []gitv1.CommitIntentRequest
	outcomes      []gitv1.OutcomeRequest
	initialized   []gitv1.InitializedRequest
	assignments   []gitv1.Assignment

	deadline     func() time.Time
	deny         map[string]string
	authorizeErr error
	intentFn     func(ctx context.Context, pushID string, req gitv1.CommitIntentRequest) error
	outcomeFn    func() error
	authorizeFn  func(ctx context.Context, req gitv1.PushAuthorizeRequest) error
}

func newFakeControl() *fakeControl {
	return &fakeControl{deny: map[string]string{}}
}

func (f *fakeControl) Authorize(ctx context.Context, req gitv1.PushAuthorizeRequest) (gitv1.PushAuthorizeResponse, error) {
	f.mu.Lock()
	f.authorizeReqs = append(f.authorizeReqs, req)
	deadline, deny, err, fn := f.deadline, f.deny, f.authorizeErr, f.authorizeFn
	f.mu.Unlock()
	if err != nil {
		return gitv1.PushAuthorizeResponse{}, err
	}
	if fn != nil {
		if err := fn(ctx, req); err != nil {
			return gitv1.PushAuthorizeResponse{}, err
		}
	}
	d := time.Now().Add(time.Minute)
	if deadline != nil {
		d = deadline()
	}
	resp := gitv1.PushAuthorizeResponse{PushID: "push_" + req.ReceiveID, Deadline: gitv1.Timestamp(d.UTC().Format(time.RFC3339Nano))}
	for _, u := range req.Updates {
		v := gitv1.UpdateVerdict{Ref: u.Ref, Verdict: gitv1.VerdictAllow}
		if reason, ok := deny[u.Ref]; ok {
			v.Verdict, v.Reason = gitv1.VerdictDeny, &reason
		}
		resp.Updates = append(resp.Updates, v)
	}
	return resp, nil
}

func (f *fakeControl) CommitIntent(ctx context.Context, pushID string, req gitv1.CommitIntentRequest) (gitv1.CommitIntentResponse, error) {
	f.mu.Lock()
	f.intents = append(f.intents, req)
	fn := f.intentFn
	f.mu.Unlock()
	if fn != nil {
		if err := fn(ctx, pushID, req); err != nil {
			return gitv1.CommitIntentResponse{}, err
		}
	}
	return gitv1.CommitIntentResponse{Committing: true}, nil
}

func (f *fakeControl) Outcome(_ context.Context, pushID string, req gitv1.OutcomeRequest) (gitv1.OutcomeResponse, error) {
	f.mu.Lock()
	f.outcomes = append(f.outcomes, req)
	fn := f.outcomeFn
	f.mu.Unlock()
	if fn != nil {
		if err := fn(); err != nil {
			return gitv1.OutcomeResponse{}, err
		}
	}
	return gitv1.OutcomeResponse{PushID: pushID, Status: gitv1.PushApplied}, nil
}

func (f *fakeControl) Initialized(_ context.Context, req gitv1.InitializedRequest) (gitv1.InitializedResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.initialized = append(f.initialized, req)
	return gitv1.InitializedResponse{RepositoryID: req.RepositoryID, Status: gitv1.StatusReady, Version: 2}, nil
}

func (f *fakeControl) Assignments(context.Context) (gitv1.AssignmentsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return gitv1.AssignmentsResponse{Items: append([]gitv1.Assignment{}, f.assignments...)}, nil
}

func (f *fakeControl) snapshot() (auth []gitv1.PushAuthorizeRequest, intents []gitv1.CommitIntentRequest, outcomes []gitv1.OutcomeRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append(auth, f.authorizeReqs...), append(intents, f.intents...), append(outcomes, f.outcomes...)
}

func (f *fakeControl) calls() int {
	a, i, o := f.snapshot()
	return len(a) + len(i) + len(o)
}

// fakeVerifier stands in for gitauth's signature verification: tokens map to
// claims, so the tests choose exactly what the worker is told.
type fakeVerifier struct {
	mu     sync.Mutex
	claims map[string]gitv1.ForwardingAssertion
}

func (v *fakeVerifier) VerifyForwarding(_ context.Context, token string, _ domain.TenantID, _ time.Time) (gitv1.ForwardingAssertion, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if a, ok := v.claims[token]; ok {
		return a, nil
	}
	return gitv1.ForwardingAssertion{}, &gitv1.ContractError{Code: domain.CodeInvalidCredential, Message: "bad signature"}
}

type fakeCallers struct{}

func (fakeCallers) VerifyControlCall(_ context.Context, r *http.Request) error {
	if r.Header.Get("Authorization") != "Bearer "+controlToken {
		return &gitv1.ContractError{Code: domain.CodeInvalidCredential, Message: "no"}
	}
	return nil
}

// ---- environment ----

type testEnv struct {
	t        *testing.T
	root     string
	svc      *Service
	srv      *httptest.Server
	control  *fakeControl
	verifier *fakeVerifier
	home     string
	seq      int
}

func newTestEnv(t *testing.T, mods ...func(*Config)) *testEnv {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := filepath.Join(t.TempDir(), "data")
	e := &testEnv{t: t, root: root, control: newFakeControl(), verifier: &fakeVerifier{claims: map[string]gitv1.ForwardingAssertion{}}, home: t.TempDir()}
	cfg := Config{
		TenantID: testTenant, DataRoot: root, ControlPlane: e.control, Assertions: e.verifier, Callers: fakeCallers{},
		Limits: Limits{GateWait: 3 * time.Second, ControlTimeout: 10 * time.Second, LockWait: 300 * time.Millisecond},
	}
	cfg.HookBinary = testHookBinary(t)
	cfg.Logger = slog.New(slog.NewTextHandler(testLog{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	for _, m := range mods {
		m(&cfg)
	}
	svc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.svc = svc
	e.srv = httptest.NewServer(svc.Handler())
	t.Cleanup(func() {
		e.srv.Close()
		_ = svc.Close()
	})
	return e
}

// token registers a valid forwarding assertion for the repository and returns
// its token.
func (e *testEnv) token(repo string, receive bool) string {
	return e.tokenWith(repo, receive, func(*gitv1.ForwardingAssertion) {})
}

func (e *testEnv) tokenWith(repo string, receive bool, mod func(*gitv1.ForwardingAssertion)) string {
	e.seq++
	now := time.Now().UTC()
	a := gitv1.ForwardingAssertion{
		Audience: gitv1.WorkerAudience(testTenant), TenantID: testTenant, RepositoryID: repo, GrantID: "gg_1", ReceivePack: receive,
		IssuedAt:  gitv1.Timestamp(now.Add(-time.Second).Format(time.RFC3339Nano)),
		ExpiresAt: gitv1.Timestamp(now.Add(4 * time.Minute).Format(time.RFC3339Nano)),
		RequestID: fmt.Sprintf("req_%d", e.seq),
	}
	mod(&a)
	token := fmt.Sprintf("tok-%d", e.seq)
	e.verifier.mu.Lock()
	e.verifier.claims[token] = a
	e.verifier.mu.Unlock()
	return token
}

func (e *testEnv) repoDir(id string) string {
	return filepath.Join(e.root, "repositories", id+".git")
}

// rpc posts to the worker RPC with the control credential.
func (e *testEnv) rpc(path string, body any) (int, []byte) {
	e.t.Helper()
	return e.rpcWith(http.MethodPost, path, body, "Bearer "+controlToken)
}

func (e *testEnv) rpcWith(method, path string, body any, auth string) (int, []byte) {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		data, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, e.srv.URL+gitv1.WorkerBasePath+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

// call runs an RPC that must succeed and decodes its response.
func rpcOK[T any, P interface {
	*T
	Validate() error
}](e *testEnv, path string, body any) T {
	e.t.Helper()
	status, data := e.rpc(path, body)
	if status != http.StatusOK {
		e.t.Fatalf("POST %s: %d %s", path, status, data)
	}
	v, err := gitv1.DecodeResponse[T, P](data)
	if err != nil {
		e.t.Fatalf("POST %s: %v\n%s", path, err, data)
	}
	return v
}

func (e *testEnv) initRepo(id, branch string) gitv1.InitializeRepositoryResponse {
	e.t.Helper()
	return rpcOK[gitv1.InitializeRepositoryResponse](e, "/repositories/"+id+"/initialize", gitv1.InitializeRepositoryRequest{RepositoryID: id, DefaultBranch: branch})
}

func rpcError(e *testEnv, path string, body any, wantStatus int, wantCode domain.ErrorCode) {
	e.t.Helper()
	status, data := e.rpc(path, body)
	if status != wantStatus {
		e.t.Fatalf("POST %s: status %d, want %d: %s", path, status, wantStatus, data)
	}
	var resp gitv1.ErrorResponse
	if err := json.Unmarshal(data, &resp); err != nil || resp.Error.Code != wantCode {
		e.t.Fatalf("POST %s: error %s, want code %s", path, data, wantCode)
	}
	if err := resp.Validate(); err != nil {
		e.t.Fatalf("error body is not the contract's: %v", err)
	}
}

// ---- git client ----

func (e *testEnv) clientEnv() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + e.home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=Dev", "GIT_AUTHOR_EMAIL=dev@example.invalid",
		"GIT_COMMITTER_NAME=Dev", "GIT_COMMITTER_EMAIL=dev@example.invalid", "LC_ALL=C",
	}
}

// git runs the Git client in dir with the forwarding assertion token as an
// extra header (token "" sends none).
func (e *testEnv) git(dir, token string, args ...string) (string, error) {
	e.t.Helper()
	full := []string{}
	if token != "" {
		full = append(full, "-c", "http.extraHeader="+AssertionHeader+": "+token)
	}
	return e.gitPlain(dir, append(full, args...)...)
}

func (e *testEnv) gitPlain(dir string, args ...string) (string, error) {
	e.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = e.clientEnv()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// gitTrace runs the Git client with packet tracing enabled and returns its
// combined output.
func (e *testEnv) gitTrace(dir, token string, args ...string) (string, error) {
	e.t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "http.extraHeader=" + AssertionHeader + ": " + token}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(e.clientEnv(), "GIT_TRACE_PACKET=1")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (e *testEnv) mustGit(dir, token string, args ...string) string {
	e.t.Helper()
	out, err := e.git(dir, token, args...)
	if err != nil {
		e.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (e *testEnv) mustPlain(dir string, args ...string) string {
	e.t.Helper()
	out, err := e.gitPlain(dir, args...)
	if err != nil {
		e.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(out)
}

// workdir creates a local repository with an initial commit.
func (e *testEnv) workdir() string {
	e.t.Helper()
	dir := e.t.TempDir()
	e.mustPlain(dir, "init", "-q", "-b", "main")
	e.commit(dir, "README", "hello\n")
	return dir
}

func (e *testEnv) commit(dir, name, content string) string {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		e.t.Fatal(err)
	}
	e.mustPlain(dir, "add", name)
	e.mustPlain(dir, "commit", "-q", "-m", "add "+name)
	return e.mustPlain(dir, "rev-parse", "HEAD")
}

func (e *testEnv) url(repo string) string { return e.srv.URL + "/" + repo + ".git" }

// seed pushes refs into a repository directly (bypassing the worker's hooks,
// which would refuse a push the worker did not start) for tests of other
// behavior. refspecs are "<local>:<remote>".
func (e *testEnv) seed(repo, workdir string, refspecs ...string) {
	e.t.Helper()
	e.mustPlain(workdir, append([]string{"push", "-q", "--receive-pack=git -c core.hooksPath=" + os.DevNull + " receive-pack", e.repoDir(repo)}, refspecs...)...)
}

// bareRef reads a ref of the repository on disk.
func (e *testEnv) bareRef(repo, ref string) string {
	e.t.Helper()
	out, err := e.gitPlain(e.repoDir(repo), "--git-dir="+e.repoDir(repo), "for-each-ref", "--format=%(objectname)", ref)
	if err != nil {
		e.t.Fatalf("for-each-ref: %v\n%s", err, out)
	}
	return strings.TrimSpace(out)
}

// bareRefAt reads a ref of a repository under another data root.
func (e *testEnv) bareRefAt(root, repo, ref string) string {
	e.t.Helper()
	dir := filepath.Join(root, "repositories", repo+".git")
	out, err := e.gitPlain(dir, "--git-dir="+dir, "for-each-ref", "--format=%(objectname)", ref)
	if err != nil {
		e.t.Fatalf("for-each-ref: %v\n%s", err, out)
	}
	return strings.TrimSpace(out)
}

// journal reads the repository's journal through the RPC.
func (e *testEnv) journal(repo string, since int64, receive, ref *string) gitv1.ReadJournalResponse {
	e.t.Helper()
	return rpcOK[gitv1.ReadJournalResponse](e, "/repositories/"+repo+"/journal", gitv1.ReadJournalRequest{
		RepositoryID: repo, ReceiveID: receive, Ref: ref, SinceSequence: since, Limit: 1000})
}

func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func strp(s string) *string { return &s }

// testLog sends the worker's log to the test output.
type testLog struct{ t *testing.T }

func (l testLog) Write(p []byte) (int, error) {
	l.t.Helper()
	l.t.Log("worker: " + strings.TrimSpace(string(p)))
	return len(p), nil
}
