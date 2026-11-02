package kernelstage

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// GitRepo is a local working tree used to seed and inspect a kernel repository.
type GitRepo struct {
	t    testing.TB
	dir  string
	url  string
	auth string
}

// NewGitRepo creates a working tree and its authenticated endpoint. The
// credential is passed as an HTTP Basic header, never in the URL or a file.
func NewGitRepo(t testing.TB, dir, baseURL, tenantKey, projectKey, name, password string) *GitRepo {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	auth := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("kodama:"+password))
	return &GitRepo{t: t, dir: dir, url: strings.TrimSuffix(baseURL, "/") + "/git/" + tenantKey + "/" + projectKey + "/" + name + ".git", auth: auth}
}

func (g *GitRepo) git(ctx context.Context, args ...string) string {
	g.t.Helper()
	all := append([]string{"-c", "http.extraHeader=" + g.auth, "-c", "user.name=kodama", "-c", "user.email=kodama@stage.invalid"}, args...)
	cmd := exec.CommandContext(ctx, "git", all...)
	cmd.Dir = g.dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		g.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (g *GitRepo) Init(ctx context.Context) {
	g.t.Helper()
	g.git(ctx, "init", "-q", "-b", "main")
}

func (g *GitRepo) Write(path, content string) {
	g.t.Helper()
	full := filepath.Join(g.dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		g.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		g.t.Fatal(err)
	}
}

func (g *GitRepo) Commit(ctx context.Context, message string) string {
	g.t.Helper()
	g.git(ctx, "add", "-A")
	g.git(ctx, "commit", "-q", "--allow-empty", "-m", message)
	return g.git(ctx, "rev-parse", "HEAD")
}

func (g *GitRepo) Push(ctx context.Context, refspec string) {
	g.t.Helper()
	g.git(ctx, "push", "-q", g.url, refspec)
}

func (g *GitRepo) Head(ctx context.Context, ref string) string {
	g.t.Helper()
	out := g.git(ctx, "ls-remote", g.url, ref)
	if out == "" {
		return ""
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func (g *GitRepo) Clone(ctx context.Context, dir, branch string) *GitRepo {
	g.t.Helper()
	cmd := exec.CommandContext(ctx, "git", "-c", "http.extraHeader="+g.auth, "clone", "-q", "--branch", branch, g.url, dir)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		g.t.Fatalf("clone: %v\n%s", err, out)
	}
	return &GitRepo{t: g.t, dir: dir, url: g.url, auth: g.auth}
}

func (g *GitRepo) String() string { return fmt.Sprintf("git worktree %s", filepath.Base(g.dir)) }
