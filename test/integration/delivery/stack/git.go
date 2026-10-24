package stack

import (
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepo is the candidate's repository: a bare repository served anonymously over
// the runtime server's TLS by git-http-backend, and the working clone commits
// are made in. The step entrypoint inside the container clones the admitted
// commit from it, as it does from the project's repository.
type gitRepo struct {
	t       testing.TB
	root    string
	bare    string
	work    string
	handler *cgi.Handler
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

func newGitRepo(t testing.TB, dir string) *gitRepo {
	t.Helper()
	r := &gitRepo{t: t, root: filepath.Join(dir, "git"), work: filepath.Join(dir, "work")}
	r.bare = filepath.Join(r.root, "repo.git")
	must(t, os.MkdirAll(r.root, 0o755))
	must(t, os.MkdirAll(r.work, 0o755))
	git(t, r.work, "init", "-q", "-b", "work")
	must(t, os.WriteFile(filepath.Join(r.work, "README.md"), []byte("candidate\n"), 0o644))
	git(t, r.work, "add", ".")
	git(t, r.work, "commit", "-q", "-m", "initial")
	git(t, ".", "clone", "-q", "--bare", r.work, r.bare)
	git(t, r.bare, "config", "http.uploadpack", "true")
	git(t, r.bare, "config", "uploadpack.allowAnySHA1InWant", "true")
	backend := filepath.Join(git(t, ".", "--exec-path"), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Fatalf("git-http-backend is required to serve the candidate: %v", err)
	}
	r.handler = &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + r.root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1"}}
	return r
}

func (r *gitRepo) serve(w http.ResponseWriter, req *http.Request) { r.handler.ServeHTTP(w, req) }

// commit replaces the repository's deploy/ directory with files (name to
// content) and returns the new commit.
func (r *gitRepo) commit(files map[string]string) string {
	t := r.t
	t.Helper()
	dir := filepath.Join(r.work, "deploy")
	must(t, os.RemoveAll(dir))
	must(t, os.MkdirAll(dir, 0o755))
	for name, content := range files {
		must(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
	}
	git(t, r.work, "add", "-A")
	git(t, r.work, "commit", "-q", "--allow-empty", "-m", "candidate "+randSuffix())
	git(t, r.work, "push", "-q", r.bare, "HEAD:refs/heads/work")
	return git(t, r.work, "rev-parse", "HEAD")
}

func randSuffix() string { return ID("c") }
