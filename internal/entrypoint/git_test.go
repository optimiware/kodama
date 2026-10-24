package entrypoint

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
)

func requireGit(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	return p
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitServer serves a bare repository over HTTPS through git-http-backend and
// requires Basic credentials; it records the Authorization headers it saw.
type gitServer struct {
	*httptest.Server
	CAFile string
	mu     sync.Mutex
	auths  []string
}

func newGitServer(t *testing.T, root, user, pass string) *gitServer {
	t.Helper()
	execPath := gitRun(t, ".", "--exec-path")
	backend := filepath.Join(execPath, "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git-http-backend not available")
	}
	gs := &gitServer{}
	h := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	gs.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		gs.mu.Lock()
		gs.auths = append(gs.auths, r.Header.Get("Authorization"))
		gs.mu.Unlock()
		if user != "" && (!ok || u != user || p != pass) {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(gs.Close)
	gs.CAFile = filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: gs.Certificate().Raw})
	if err := os.WriteFile(gs.CAFile, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return gs
}

// seedRepo creates root/repo.git with two commits and returns both IDs.
func seedRepo(t *testing.T, root string) (first, second string) {
	t.Helper()
	work := t.TempDir()
	gitRun(t, work, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("one"), 0o644)
	gitRun(t, work, "add", ".")
	gitRun(t, work, "commit", "-q", "-m", "one")
	first = gitRun(t, work, "rev-parse", "HEAD")
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("two"), 0o644)
	gitRun(t, work, "commit", "-qam", "two")
	second = gitRun(t, work, "rev-parse", "HEAD")
	gitRun(t, ".", "clone", "-q", "--bare", work, filepath.Join(root, "repo.git"))
	gitRun(t, filepath.Join(root, "repo.git"), "config", "http.uploadpack", "true")
	// Allow fetching an exact commit by ID.
	gitRun(t, filepath.Join(root, "repo.git"), "config", "uploadpack.allowAnySHA1InWant", "true")
	return first, second
}

func newTestGit(t *testing.T, gs *gitServer) *git {
	return &git{bin: requireGit(t), caFile: gs.CAFile, home: t.TempDir(), pathEnv: os.Getenv("PATH")}
}

func TestCheckoutPinnedCommitWithCredentialOnlyInEnvironment(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	first, _ := seedRepo(t, root)
	gs := newGitServer(t, root, "reader", "s3cr3t-token")
	g := newTestGit(t, gs)
	cred := &checkoutCredential{username: "reader", password: "s3cr3t-token"}
	g.redactors = []string{cred.password, cred.header()}
	dir := filepath.Join(t.TempDir(), "ws")
	repoURL := gs.URL + "/repo.git"

	if err := g.checkout(context.Background(), dir, repoURL, runtimev1.GitCommit(first), cred); err != nil {
		t.Fatal(err)
	}
	if head := gitRun(t, dir, "rev-parse", "HEAD"); head != first {
		t.Fatalf("HEAD = %s, want pinned %s", head, first)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "one" {
		t.Fatalf("worktree content = %q", b)
	}
	// The credential is neither in .git/config nor in any file under .git.
	var offenders []string
	filepath.Walk(filepath.Join(dir, ".git"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), "s3cr3t-token") || strings.Contains(string(b), "czNjcjN0") {
				offenders = append(offenders, p)
			}
		}
		return nil
	})
	if len(offenders) != 0 {
		t.Fatalf("credential persisted in %v", offenders)
	}
	if remote := gitRun(t, dir, "config", "--get", "remote.origin.url"); remote != repoURL || strings.Contains(remote, "@") {
		t.Fatalf("remote url = %q", remote)
	}
	// Hooks directory is not populated from a template.
	if entries, _ := os.ReadDir(filepath.Join(dir, ".git", "hooks")); len(entries) != 0 {
		t.Fatalf("hooks present: %v", entries)
	}
	gs.mu.Lock()
	defer gs.mu.Unlock()
	if len(gs.auths) == 0 {
		t.Fatal("server saw no requests")
	}
}

func TestCheckoutWithoutValidCredentialFailsWithoutLeakingIt(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	first, _ := seedRepo(t, root)
	gs := newGitServer(t, root, "reader", "right")
	g := newTestGit(t, gs)
	bad := &checkoutCredential{username: "reader", password: "wrong-password"}
	g.redactors = []string{bad.password, bad.header()}
	err := g.checkout(context.Background(), filepath.Join(t.TempDir(), "ws"), gs.URL+"/repo.git", runtimev1.GitCommit(first), bad)
	if err == nil {
		t.Fatal("checkout succeeded with a wrong credential")
	}
	if strings.Contains(err.Error(), "wrong-password") || strings.Contains(err.Error(), bad.header()) {
		t.Fatalf("credential leaked in error: %v", err)
	}
	if err := g.checkout(context.Background(), filepath.Join(t.TempDir(), "ws2"), gs.URL+"/repo.git", runtimev1.GitCommit(first), nil); err == nil {
		t.Fatal("anonymous checkout of a protected repository succeeded")
	}
}

func TestCheckoutUnknownCommitFails(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	seedRepo(t, root)
	gs := newGitServer(t, root, "", "")
	g := newTestGit(t, gs)
	err := g.checkout(context.Background(), filepath.Join(t.TempDir(), "ws"), gs.URL+"/repo.git", runtimev1.GitCommit(strings.Repeat("1", 40)), nil)
	if err == nil {
		t.Fatal("a commit that does not exist was checked out")
	}
}

func TestCheckoutRefusesNonEmptyWorkspace(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "stale"), []byte("x"), 0o644)
	g := &git{bin: "git", home: t.TempDir()}
	if err := g.checkout(context.Background(), dir, "https://127.0.0.1:1/r.git", runtimev1.GitCommit(strings.Repeat("1", 40)), nil); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("err = %v", err)
	}
}

// A hook shipped in the repository must never run, and submodules are not
// fetched.
func TestCheckoutDoesNotRunRepositoryHooks(t *testing.T) {
	requireGit(t)
	marker := filepath.Join(t.TempDir(), "hook-ran")
	root := t.TempDir()
	first, _ := seedRepo(t, root)
	bare := filepath.Join(root, "repo.git")
	os.MkdirAll(filepath.Join(bare, "hooks"), 0o755)
	os.WriteFile(filepath.Join(bare, "hooks", "post-checkout"), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755)
	gs := newGitServer(t, root, "", "")
	g := newTestGit(t, gs)
	dir := filepath.Join(t.TempDir(), "ws")
	if err := g.checkout(context.Background(), dir, gs.URL+"/repo.git", runtimev1.GitCommit(first), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a hook ran")
	}
	// Even a hook planted in the checkout's own hooks directory is ignored by
	// the hardened invocation.
	os.WriteFile(filepath.Join(dir, ".git", "hooks", "pre-commit"), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("n"), 0o644)
	if _, err := g.commitAll(context.Background(), dir, "m"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("pre-commit hook ran")
	}
}

func TestParseCheckoutCredential(t *testing.T) {
	c, err := parseCheckoutCredential([]byte("user:pa:ss\n"))
	if err != nil || c.username != "user" || c.password != "pa:ss" {
		t.Fatalf("c=%+v err=%v", c, err)
	}
	for _, bad := range []string{"", "nocolon", ":pw", "user:", "a:b\nc:d"} {
		if _, err := parseCheckoutCredential([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestGitEnvIsHardened(t *testing.T) {
	g := &git{bin: "git", home: "/h", pathEnv: "/bin"}
	env := strings.Join(g.env(), "\n")
	for _, want := range []string{"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ALLOW_PROTOCOL=https"} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %s", want)
		}
	}
	args := strings.Join(g.base(), " ")
	for _, want := range []string{"core.hooksPath=/dev/null", "submodule.recurse=false", "credential.helper="} {
		if !strings.Contains(args, want) {
			t.Errorf("args lack %s", want)
		}
	}
}

func TestContainsAnyAcrossChunkBoundary(t *testing.T) {
	needle := []byte("NEEDLE-VALUE")
	data := append(append(make([]byte, 64<<10-5), needle...), make([]byte, 1000)...)
	if !containsAny(strings.NewReader(string(data)), [][]byte{needle}) {
		t.Fatal("needle spanning a chunk boundary was missed")
	}
	if containsAny(strings.NewReader("nothing here"), [][]byte{needle}) {
		t.Fatal("false positive")
	}
}
