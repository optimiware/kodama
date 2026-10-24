package entrypoint_test

// Fixture: a temporary Pod layout, a contract server, optional real Git, and
// the entrypoint configuration that ties them together.

import (
	"bytes"
	"context"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/entrypoint"
)

const (
	imageRef = "registry.example.invalid/step@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	sh       = "/bin/sh"
)

// ---- Git ----

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed; Git-backed entrypoint checks skipped")
	}
}

func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=human", "GIT_AUTHOR_EMAIL=human@example.invalid", "GIT_COMMITTER_NAME=human", "GIT_COMMITTER_EMAIL=human@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(out)), &gitError{args: args, out: string(out), err: err}
	}
	return strings.TrimSpace(string(out)), nil
}

type gitError struct {
	args []string
	out  string
	err  error
}

func (e *gitError) Error() string {
	return "git " + strings.Join(e.args, " ") + ": " + e.err.Error() + "\n" + e.out
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitOut(dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// gitHTTPS serves a bare-repository root over TLS through git-http-backend
// with Basic authentication, like a forge's smart-HTTP endpoint.
type gitHTTPS struct {
	*httptest.Server
	CAFile string
	mu     sync.Mutex
	auths  []string
}

func newGitHTTPS(t *testing.T, root, user, pass string) *gitHTTPS {
	t.Helper()
	execPath := gitRun(t, ".", "--exec-path")
	backend := filepath.Join(execPath, "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git-http-backend is not available; Git-backed entrypoint checks skipped")
	}
	g := &gitHTTPS{}
	h := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	g.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		g.mu.Lock()
		g.auths = append(g.auths, r.Header.Get("Authorization"))
		g.mu.Unlock()
		if !ok || u != user || p != pass {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(g.Close)
	g.CAFile = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(g.CAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: g.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return g
}

const (
	gitUser = "reader"
	gitPass = "co-token-4f81c2d9e07b" // the checkout credential: must never be persisted or published
	branch  = "refs/heads/work"
)

// repo is a bare repository with the branch "work": c1 ("one"), c2 ("two").
type repo struct {
	root, bare string
	srv        *gitHTTPS
	c1, c2     string
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	requireGit(t)
	r := &repo{root: t.TempDir()}
	work := t.TempDir()
	gitRun(t, work, "init", "-q", "-b", "work")
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(work, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.txt", "one")
	gitRun(t, work, "add", ".")
	gitRun(t, work, "commit", "-q", "-m", "one")
	r.c1 = gitRun(t, work, "rev-parse", "HEAD")
	write("a.txt", "two")
	gitRun(t, work, "commit", "-qam", "two")
	r.c2 = gitRun(t, work, "rev-parse", "HEAD")
	r.bare = filepath.Join(r.root, "repo.git")
	gitRun(t, ".", "clone", "-q", "--bare", work, r.bare)
	gitRun(t, r.bare, "config", "http.uploadpack", "true")
	gitRun(t, r.bare, "config", "uploadpack.allowAnySHA1InWant", "true")
	r.srv = newGitHTTPS(t, r.root, gitUser, gitPass)
	return r
}

func (r *repo) url() string { return r.srv.URL + "/repo.git" }

func (r *repo) tip() string { out, _ := gitOut(r.bare, "rev-parse", "--verify", branch); return out }

// humanPush is a person pushing a fast-forward commit to the work branch.
func (r *repo) humanPush(t *testing.T, name, content string) string {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "human")
	gitRun(t, ".", "clone", "-q", "--branch", "work", r.bare, clone)
	if err := os.WriteFile(filepath.Join(clone, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, clone, "add", ".")
	gitRun(t, clone, "commit", "-q", "-m", "human change")
	gitRun(t, clone, "push", "-q", "origin", "work")
	return gitRun(t, clone, "rev-parse", "HEAD")
}

// humanPushSafe is humanPush for use inside the server goroutine.
func (r *repo) humanPushSafe(name, content string) (string, error) {
	clone, err := os.MkdirTemp("", "kodama-human-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(clone)
	for _, args := range [][]string{
		{"clone", "-q", "--branch", "work", r.bare, clone + "/h"},
	} {
		if _, err := gitOut(".", args...); err != nil {
			return "", err
		}
	}
	h := clone + "/h"
	if err := os.WriteFile(filepath.Join(h, name), []byte(content), 0o644); err != nil {
		return "", err
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-q", "-m", "human change"}, {"push", "-q", "origin", "work"}} {
		if _, err := gitOut(h, args...); err != nil {
			return "", err
		}
	}
	return gitOut(h, "rev-parse", "HEAD")
}

// ---- Pod fixture ----

// syncBuffer collects the entrypoint's structured logs.
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

type fixture struct {
	t    *testing.T
	root string
	srv  *contractServer
	repo *repo
	cfg  entrypoint.Config
	logs *syncBuffer
	pids string // directory commands write PID files into
}

// baseContext is a no-workspace step running script under /bin/sh.
func baseContext(script string) runtimev1.Context {
	return runtimev1.Context{
		TenantID: "t_acme", ProjectID: "p_inventory", RunID: "run_01", StepID: "build", AttemptID: "att_it01", ActionKey: "run_01.build",
		Definition: runtimev1.Definition{ID: "pd_01", Digest: runtimev1.Digest("sha256:" + strings.Repeat("d", 64))},
		Command:    runtimev1.Command{Image: imageRef, Command: []string{sh, "-c"}, Args: []string{script}, Env: map[string]string{}},
		Workspace:  runtimev1.Workspace{Access: runtimev1.AccessNone},
		Inputs:     map[string]runtimev1.Value{}, Outputs: []runtimev1.OutputDecl{}, Secrets: []runtimev1.SecretBinding{},
		Limits: runtimev1.Limits{TimeoutSeconds: 60, MaxOutputBytes: 1 << 20, MaxOutputFiles: 10, MaxDiagnosticBytes: 64 << 10},
	}
}

// newFixture lays out a Pod (workspace, outputs, secrets, identity, results
// file, scratch), starts the contract server and builds a fast configuration.
func newFixture(t *testing.T, script string) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{t: t, root: root, logs: &syncBuffer{}, pids: filepath.Join(root, "pids")}
	for _, d := range []string{"pids", "scratch", "identity", "outputs"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.srv = newContractServer(t, baseContext(script))
	f.srv.ctx.Command.Env["PIDDIR"] = f.pids
	if err := os.WriteFile(filepath.Join(root, "identity", "admission"), []byte(f.srv.admission+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.cfg = entrypoint.Config{
		ServerURL: f.srv.URL, AttemptID: string(f.srv.ctx.AttemptID),
		AdmissionCredentialFile: filepath.Join(root, "identity", "admission"),
		CheckoutCredentialFile:  filepath.Join(root, "identity", "checkout-credential"),
		TokenFile:               filepath.Join(root, "identity", "token"),
		WorkspaceDir:            filepath.Join(root, "workspace"),
		OutputsDir:              filepath.Join(root, "outputs"),
		SecretsDir:              filepath.Join(root, "secrets"),
		IdentityDir:             filepath.Join(root, "identity"),
		ResultsFile:             filepath.Join(root, "results.json"),
		ScratchDir:              filepath.Join(root, "scratch"),
		Job:                     f.srv.job,
		ChildEnv:                map[string]string{"PATH": os.Getenv("PATH"), "HOME": filepath.Join(root, "home")},
		Retry:                   entrypoint.RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond},
		KillGrace:               300 * time.Millisecond,
		FinalizeTimeout:         30 * time.Second,
		SettleTimeout:           15 * time.Second,
		SettleBackoff:           5 * time.Millisecond,
		RenewMinInterval:        20 * time.Millisecond,
		InterruptTimeout:        10 * time.Second,
	}
	f.cfg.Logger = newLogger(f.logs)
	return f
}

// withRepo makes the step a reader (pinned = pinned commit) or a writer of the
// repository's work branch.
func (f *fixture) withRepo(r *repo, access runtimev1.WorkspaceAccess, pinned string) {
	f.t.Helper()
	f.repo = r
	src := &runtimev1.Source{RepositoryURL: r.url(), Commit: runtimev1.GitCommit(pinned)}
	if access == runtimev1.AccessWrite {
		b, tip := branch, runtimev1.GitCommit(pinned)
		src.Branch, src.ExpectedTip = &b, &tip
		f.srv.pub = &publisher{bare: r.bare, branch: branch}
	}
	f.srv.ctx.Workspace = runtimev1.Workspace{Access: access, Source: src}
	f.cfg.GitCAFile = r.srv.CAFile
	if err := os.WriteFile(f.cfg.CheckoutCredentialFile, []byte(gitUser+":"+gitPass+"\n"), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) declareFile(name, path string) {
	p := path
	f.srv.ctx.Outputs = append(f.srv.ctx.Outputs, runtimev1.OutputDecl{Name: name, Type: runtimev1.OutputArtifact, Path: &p})
}

func (f *fixture) declare(name string, typ runtimev1.OutputType) {
	f.srv.ctx.Outputs = append(f.srv.ctx.Outputs, runtimev1.OutputDecl{Name: name, Type: typ})
}

func (f *fixture) secret(name, ref, value string, delivery runtimev1.SecretDelivery) {
	f.srv.ctx.Secrets = append(f.srv.ctx.Secrets, runtimev1.SecretBinding{Ref: ref, Version: 1, Name: name, Delivery: delivery})
	f.srv.secrets[ref] = value
}

func (f *fixture) path(parts ...string) string {
	return filepath.Join(append([]string{f.root}, parts...)...)
}

// run executes the entrypoint to completion (bounded) and returns its exit code.
func (f *fixture) run() int { return f.runCtx(context.Background()) }

func (f *fixture) runCtx(parent context.Context) int {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	return entrypoint.Execute(ctx, f.cfg)
}

// start runs the entrypoint in the background; the returned func waits for
// the exit code (failing the test after 60s).
func (f *fixture) start(ctx context.Context) func() int {
	done := make(chan int, 1)
	go func() { done <- entrypoint.Execute(ctx, f.cfg) }()
	return func() int {
		f.t.Helper()
		select {
		case code := <-done:
			return code
		case <-time.After(60 * time.Second):
			f.t.Fatal("the entrypoint did not return within 60s")
			return -1
		}
	}
}

// ---- assertions ----

func (f *fixture) wantExit(got, want int) {
	f.t.Helper()
	if got != want {
		f.t.Fatalf("exit code = %d, want %d\ncalls: %v\nlogs:\n%s", got, want, f.srv.Ops(), f.logs.String())
	}
}

func newLogger(w *syncBuffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// acceptedResult fails unless the server accepted a result and returns it.
func (f *fixture) acceptedResult() runtimev1.Result {
	f.t.Helper()
	r := f.srv.Accepted()
	if r == nil {
		f.t.Fatalf("no result was accepted; calls: %v\nlogs:\n%s", f.srv.Ops(), f.logs.String())
	}
	return r.Result
}

func (f *fixture) wantFailed(r runtimev1.Result, reason string) {
	f.t.Helper()
	if r.Outcome != runtimev1.OutcomeFailed || r.Reason == nil || *r.Reason != reason {
		f.t.Fatalf("result = %+v (reason %v), want failed/%s\nlogs:\n%s", r, deref(r.Reason), reason, f.logs.String())
	}
	if len(r.Outputs) != 0 || r.SourceCommit != nil || r.Question != nil {
		f.t.Fatalf("a failed result carries outputs, source or question: %+v", r)
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// diagnostics returns the usable diagnostic artifacts named in the result,
// keyed by artifact name.
func (f *fixture) diagnostics(r runtimev1.Result) map[string]string {
	f.t.Helper()
	byID := map[domain.ArtifactID]artifactRec{}
	for _, a := range f.srv.Artifacts(runtimev1.RoleDiagnostic) {
		byID[a.id] = a
	}
	out := map[string]string{}
	for _, id := range r.DiagnosticArtifactIDs {
		a, ok := byID[id]
		if !ok || !a.usable {
			f.t.Fatalf("result cites diagnostic %s which is not a usable diagnostic artifact", id)
		}
		out[a.req.Name] = string(a.bytes)
	}
	return out
}

// pidFiles reads PID files written by the command.
func (f *fixture) waitPIDs(names ...string) map[string]int {
	f.t.Helper()
	out := map[string]int{}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, n := range names {
			if _, ok := out[n]; ok {
				continue
			}
			b, err := os.ReadFile(filepath.Join(f.pids, n))
			if err != nil || !bytes.HasSuffix(b, []byte("\n")) {
				continue
			}
			var pid int
			if _, err := fmt.Sscan(strings.TrimSpace(string(b)), &pid); err == nil && pid > 0 {
				out[n] = pid
			}
		}
		if len(out) == len(names) {
			return out
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("command did not write PID files %v (got %v)", names, out)
	return nil
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// wantDead requires every process to be gone (reaped) shortly.
func wantDead(t *testing.T, pids map[string]int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var left []string
		for name, pid := range pids {
			if alive(pid) {
				left = append(left, name)
			}
		}
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("processes still alive: %v (%v)", left, pids)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// credentialsAbsent fails if any needle occurs in an uploaded object, in the
// server's recorded request bodies (besides the secret resolve responses,
// which are responses), in the structured logs or the extra files.
func (f *fixture) credentialsAbsent(needles ...string) {
	f.t.Helper()
	check := func(where string, data []byte) {
		for _, n := range needles {
			if n != "" && bytes.Contains(data, []byte(n)) {
				f.t.Errorf("credential %q... found in %s", n[:4], where)
			}
		}
	}
	for name, data := range f.srv.UploadedBytes() {
		check("uploaded artifact "+name, data)
	}
	for _, c := range f.srv.Calls() {
		check("request body of "+c.op, c.body)
	}
	check("entrypoint logs", []byte(f.logs.String()))
}

// fastRenewal makes runtime tokens short-lived so the renewal loop runs every
// couple of hundred milliseconds.
func (f *fixture) fastRenewal() {
	f.srv.with(func() { f.srv.ttl = 400 * time.Millisecond })
}

// treeScript starts a leader, a child and a grandchild (a subshell with its
// own sleep) that would outlive a naive kill of the leader. ignoreTerm makes
// every process ignore SIGTERM so only SIGKILL stops them.
func treeScript(ignoreTerm bool) string {
	trap := ""
	if ignoreTerm {
		trap = `trap '' TERM
`
	}
	return trap + `echo $$ > "$PIDDIR/leader.tmp" && mv "$PIDDIR/leader.tmp" "$PIDDIR/leader"
sleep 300 &
echo $! > "$PIDDIR/child.tmp" && mv "$PIDDIR/child.tmp" "$PIDDIR/child"
( sleep 300 & echo $! > "$PIDDIR/grand.tmp" && mv "$PIDDIR/grand.tmp" "$PIDDIR/grand"; wait ) &
wait
`
}

// decodeInto strictly decodes a recorded request body.
func decodeInto[T any, P interface {
	*T
	Validate() error
}](body []byte, out *T) error {
	v, err := runtimev1.DecodeRequest[T, P](body)
	if err == nil {
		*out = v
	}
	return err
}
