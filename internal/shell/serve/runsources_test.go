package serve_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runner"
	"github.com/optimiware/kodama/internal/shell/api"
	"github.com/optimiware/kodama/internal/shell/serve"
	"github.com/optimiware/kodama/internal/shell/store"
)

// srcEnv is the real Git hosting (the in-process gitservice worker behind the
// kernel's Git routes, over a temporary bare repository per repository) with the
// pieces a run's source preparation needs.
type srcEnv struct {
	*gitEnv
	seq    int
	clock  atomic.Int64 // unix nanoseconds
	creds  string
	wsRoot string
	live   liveness
}

// liveness reads the run's liveness from the Git authority's own record, as the
// kernel does; unknown makes the answer fail, as an outage would.
type liveness struct {
	e       *srcEnv
	unknown atomic.Bool
}

func (l *liveness) RunActive(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, runID string) (bool, error) {
	if l.unknown.Load() {
		return false, errors.New("the database is unavailable")
	}
	info, err := l.e.mem.RunGitInfo(ctx, tenant, project, runID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil && info.Active(), err
}

const (
	tenantID  domain.TenantID  = "t_acme"
	projectID domain.ProjectID = "p_erp"
)

func newSrcEnv(t *testing.T) *srcEnv {
	t.Helper()
	e := &srcEnv{gitEnv: newGitEnv(t)}
	e.live.e = e
	e.clock.Store(time.Now().UnixNano())
	e.creds = filepath.Join(e.dir, "git-credentials")
	e.wsRoot = filepath.Join(e.dir, "workspaces")
	// The knowledge base is written by an administrator who is also an engineer.
	e.mem.Grant(tenantID, "u_admin", "", "admin")
	e.mem.Grant(tenantID, "u_admin", "", "engineer")
	return e
}

// publish creates a repository (project scope, or the tenant's when project is
// empty), bootstraps it with a seed grant and returns it with its first commit.
// The default branch accepts direct pushes so a test can move it.
func (e *srcEnv) publish(project, name string, files map[string]string) (repo map[string]any, work, commit string) {
	e.t.Helper()
	path, token := "/api/v1/tenants/acme/projects/p_erp/repositories", "tok-eng"
	if project == "" {
		path, token = "/api/v1/tenants/acme/repositories", "tok-admin"
	}
	code, repo := e.api(http.MethodPost, path, token, map[string]any{"name": name, "policy": map[string]any{"directPushToDefault": true}})
	if code != http.StatusCreated || repo["status"] != "ready" {
		e.t.Fatalf("create %s: %d %v", name, code, repo)
	}
	e.seq++
	work = filepath.Join(e.dir, "work", name+string(rune('a'+e.seq)))
	if err := os.MkdirAll(work, 0o755); err != nil {
		e.t.Fatal(err)
	}
	e.mustGit(work, "init", "-q", "-b", "main")
	commit = e.commit(work, "first", files)
	_, seed, status, body := e.credential(token, repo, true)
	if status != http.StatusCreated {
		e.t.Fatalf("seed credential: %d %v", status, body)
	}
	if out, err := e.git(work, "push", e.cloneURL(repo, seed), "main:refs/heads/main"); err != nil {
		e.t.Fatalf("seed push: %v\n%s", err, out)
	}
	return repo, work, commit
}

func (e *srcEnv) commit(work, message string, files map[string]string) string {
	e.t.Helper()
	for name, content := range files {
		p := filepath.Join(work, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			e.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			e.t.Fatal(err)
		}
	}
	e.mustGit(work, "add", "-A")
	e.mustGit(work, "commit", "-q", "--allow-empty", "-m", message)
	return e.mustGit(work, "rev-parse", "HEAD")
}

// advance moves the repository's default branch past the commit a run pinned.
func (e *srcEnv) advance(repo map[string]any, work, token string, files map[string]string) string {
	e.t.Helper()
	commit := e.commit(work, "later", files)
	_, pw, _, _ := e.credential(token, repo, false)
	if out, err := e.git(work, "push", e.cloneURL(repo, pw), "main:refs/heads/main"); err != nil {
		e.t.Fatalf("moving the branch: %v\n%s", err, out)
	}
	return commit
}

// issuer is the real Git server's run credential issuance, with switches for
// the failures a test provokes.
type issuer struct {
	e       *srcEnv
	deny    map[string]bool // repository id -> the run has no authority over it
	server  *api.GitServer  // issues instead of the environment's, when set
	calls   atomic.Int32
	failing atomic.Bool // the control plane cannot be reached
}

func (i *issuer) IssueRunCredential(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, runID, repositoryID string) (runner.IssuedCredential, error) {
	i.calls.Add(1)
	if i.failing.Load() {
		return runner.IssuedCredential{}, errors.New("the control plane is down")
	}
	if i.deny[repositoryID] {
		return runner.IssuedCredential{}, store.ErrNotFound
	}
	server := i.e.server
	if i.server != nil {
		server = i.server
	}
	cred, err := server.IssueRunCredential(ctx, tenant, project, runID, repositoryID)
	if err != nil {
		return runner.IssuedCredential{}, err
	}
	return runner.IssuedCredential{GrantID: cred.GrantID, Secret: cred.Secret, ExpiresAt: cred.ExpiresAt}, nil
}

func (e *srcEnv) sources(baseURL string, is runner.CredentialIssuer) *runner.GitSources {
	if baseURL == "" {
		baseURL = e.srv.URL
	}
	return &runner.GitSources{Checkout: runner.Process{}, Repositories: e.mem, Credentials: is, Liveness: &e.live,
		BaseURL: baseURL, Dir: e.creds, Now: func() time.Time { return time.Unix(0, e.clock.Load()) }}
}

// register makes the run live for the Git authority and returns it as the kernel
// store would load it.
func (e *srcEnv) register(runID, status string, cancelling bool, sources ...gitv1.Source) store.Run {
	e.t.Helper()
	raw, err := json.Marshal(sources)
	if err != nil {
		e.t.Fatal(err)
	}
	e.mem.SetRun(store.RunGitInfo{TenantID: tenantID, ProjectID: projectID, RunID: runID, PipelineName: "build", Status: status,
		Cancelling: cancelling, Sources: raw, TaskID: "tsk_1"})
	return store.Run{TenantID: tenantID, ProjectID: projectID, ID: runID, TenantKey: "acme", ProjectKey: "erp", Sources: sources}
}

func (e *srcEnv) workspace(name string) string {
	e.t.Helper()
	ws := filepath.Join(e.wsRoot, name, "workspace")
	if err := os.MkdirAll(ws, 0o750); err != nil {
		e.t.Fatal(err)
	}
	return ws
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func readSecret(t *testing.T, rg *runner.RunGit, repoID string) string {
	t.Helper()
	return strings.TrimSpace(read(t, filepath.Join(rg.Dir(), repoID+".credential")))
}

func readSource(repoID, commit, path string) gitv1.Source {
	return gitv1.Source{RepositoryID: repoID, Commit: commit, Path: path, Access: gitv1.AccessRead}
}

// mustNotContain fails when any file under root, including .git directories,
// holds the secret.
func mustNotContain(t *testing.T, root string, secrets ...string) {
	t.Helper()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, s := range secrets {
			if s != "" && bytes.Contains(b, []byte(s)) {
				t.Errorf("%s holds a Git credential", p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Test_unit_kernel_run_checkout_pinned_commit: the run starts from exactly its
// pinned commits, honoring each path, even after the branches moved; a missing
// commit and an unreachable Git service fail with a stable error and leave
// nothing behind.
func Test_unit_kernel_run_checkout_pinned_commit(t *testing.T) {
	e := newSrcEnv(t)
	back, backWork, backPinned := e.publish("erp", "backend", map[string]string{"README.md": "backend v1\n", "src/main.go": "package main\n"})
	front, frontWork, frontPinned := e.publish("erp", "frontend", map[string]string{"README.md": "frontend v1\n"})
	backID, frontID := back["id"].(string), front["id"].(string)

	// The branches move after the run pinned its sources.
	backTip := e.advance(back, backWork, "tok-eng", map[string]string{"README.md": "backend v2\n"})
	e.advance(front, frontWork, "tok-eng", map[string]string{"README.md": "frontend v2\n"})

	t.Run("each source at its recorded commit and path", func(t *testing.T) {
		run := e.register("run_pin", "running", false, readSource(backID, backPinned, "backend"), readSource(frontID, frontPinned, "frontend"))
		ws := e.workspace("pin")
		rg, err := e.sources("", &issuer{e: e}).Prepare(t.Context(), run, ws)
		if err != nil || rg == nil {
			t.Fatalf("prepare: %v", err)
		}
		defer rg.Close()
		if got := read(t, filepath.Join(ws, "backend", "README.md")); got != "backend v1\n" {
			t.Fatalf("backend README = %q: the moved branch was checked out", got)
		}
		if got := read(t, filepath.Join(ws, "frontend", "README.md")); got != "frontend v1\n" {
			t.Fatalf("frontend README = %q", got)
		}
		for path, want := range map[string]string{"backend": backPinned, "frontend": frontPinned} {
			if head := e.mustGit(filepath.Join(ws, path), "rev-parse", "HEAD"); head != want {
				t.Fatalf("%s HEAD = %s, want the pinned %s", path, head, want)
			}
			if out := e.mustGit(filepath.Join(ws, path), "status", "--porcelain"); out != "" {
				t.Fatalf("%s is not clean: %s", path, out)
			}
			if out, err := e.git(filepath.Join(ws, path), "symbolic-ref", "-q", "HEAD"); err == nil {
				t.Fatalf("%s is on branch %s: a pinned commit is checked out detached", path, out)
			}
			if remote := e.mustGit(filepath.Join(ws, path), "remote", "get-url", "origin"); !strings.HasPrefix(remote, e.srv.URL+"/git/acme/erp/") || strings.Contains(remote, "@") {
				t.Fatalf("%s remote = %q", path, remote)
			}
		}
		if backTip == backPinned {
			t.Fatal("the test did not move the branch")
		}
	})

	t.Run("a single source at . keeps the repository root layout", func(t *testing.T) {
		run := e.register("run_root", "running", false, readSource(backID, backPinned, "."))
		ws := e.workspace("root")
		rg, err := e.sources("", &issuer{e: e}).Prepare(t.Context(), run, ws)
		if err != nil || rg == nil {
			t.Fatalf("prepare: %v", err)
		}
		defer rg.Close()
		if got := read(t, filepath.Join(ws, "src", "main.go")); got != "package main\n" {
			t.Fatalf("src/main.go = %q", got)
		}
		if head := e.mustGit(ws, "rev-parse", "HEAD"); head != backPinned {
			t.Fatalf("HEAD = %s", head)
		}
	})

	t.Run("a missing commit fails before any step and removes the credentials", func(t *testing.T) {
		missing := strings.Repeat("d", 40)
		run := e.register("run_missing", "running", false, readSource(backID, backPinned, "backend"), readSource(frontID, missing, "frontend"))
		ws := e.workspace("missing")
		rg, err := e.sources("", &issuer{e: e}).Prepare(t.Context(), run, ws)
		if !errors.Is(err, runner.ErrSourceUnavailable) || rg != nil {
			t.Fatalf("prepare = %v, %v; want ErrSourceUnavailable", rg, err)
		}
		if _, statErr := os.Stat(filepath.Join(e.creds, "t_acme_p_erp_run_missing")); !os.IsNotExist(statErr) {
			t.Fatalf("the credential directory of a failed preparation remains: %v", statErr)
		}
	})

	t.Run("an unreachable Git service fails the run's sources", func(t *testing.T) {
		dead := httptest.NewServer(http.NotFoundHandler())
		url := dead.URL
		dead.Close()
		run := e.register("run_down", "running", false, readSource(backID, backPinned, "backend"))
		rg, err := e.sources(url, &issuer{e: e}).Prepare(t.Context(), run, e.workspace("down"))
		if !errors.Is(err, runner.ErrSourceUnavailable) || rg != nil {
			t.Fatalf("prepare = %v, %v; want ErrSourceUnavailable", rg, err)
		}
	})

	t.Run("an unpinned repository has no credential and no checkout", func(t *testing.T) {
		run := e.register("run_foreign", "running", false, readSource(backID, backPinned, "backend"))
		run.Sources = []gitv1.Source{readSource(frontID, frontPinned, "frontend")} // the record pins only backend
		rg, err := e.sources("", &issuer{e: e}).Prepare(t.Context(), run, e.workspace("foreign"))
		if !errors.Is(err, runner.ErrSourceUnavailable) || !errors.Is(err, store.ErrNotFound) || rg != nil {
			t.Fatalf("prepare = %v, %v", rg, err)
		}
	})
}

// Test_unit_kernel_run_checkout_context_repos: the tenant and project context
// repositories are checked out at their current tips under .context/ when they
// exist, an absent one is not an error, and a context never overwrites a path of
// the run's sources.
func Test_unit_kernel_run_checkout_context_repos(t *testing.T) {
	e := newSrcEnv(t)
	back, _, backPinned := e.publish("erp", "backend", map[string]string{"README.md": "backend\n"})
	backID := back["id"].(string)
	projCtx, projWork, _ := e.publish("erp", "context", map[string]string{"architecture.md": "project v1\n"})
	tenantCtx, tenantWork, _ := e.publish("", "context", map[string]string{"guidelines.md": "tenant v1\n"})
	// The contexts' tips are past their first commits.
	e.advance(projCtx, projWork, "tok-eng", map[string]string{"architecture.md": "project v2\n"})
	e.advance(tenantCtx, tenantWork, "tok-admin", map[string]string{"guidelines.md": "tenant v2\n"})
	is := func() *issuer { return &issuer{e: e} }

	t.Run("both contexts at their tips beside the sources", func(t *testing.T) {
		run := e.register("run_ctx", "running", false, readSource(backID, backPinned, "backend"))
		ws := e.workspace("ctx")
		rg, err := e.sources("", is()).Prepare(t.Context(), run, ws)
		if err != nil || rg == nil {
			t.Fatalf("prepare: %v", err)
		}
		defer rg.Close()
		if got := read(t, filepath.Join(ws, ".context", "tenant", "guidelines.md")); got != "tenant v2\n" {
			t.Fatalf("tenant context = %q: not the current tip", got)
		}
		if got := read(t, filepath.Join(ws, ".context", "project", "architecture.md")); got != "project v2\n" {
			t.Fatalf("project context = %q: not the current tip", got)
		}
		if got := read(t, filepath.Join(ws, "backend", "README.md")); got != "backend\n" {
			t.Fatalf("source = %q", got)
		}
		mustNotContain(t, ws, readSecret(t, rg, projCtx["id"].(string)), readSecret(t, rg, tenantCtx["id"].(string)), readSecret(t, rg, backID))
	})

	t.Run("a run without sources still gets its contexts", func(t *testing.T) {
		run := e.register("run_ctx_only", "running", false)
		ws := e.workspace("ctxonly")
		rg, err := e.sources("", is()).Prepare(t.Context(), run, ws)
		if err != nil || rg == nil {
			t.Fatalf("prepare: %v", err)
		}
		defer rg.Close()
		if got := read(t, filepath.Join(ws, ".context", "project", "architecture.md")); got != "project v2\n" {
			t.Fatalf("project context = %q", got)
		}
	})

	t.Run("a context that exists and cannot be read fails the run's sources", func(t *testing.T) {
		for name, denied := range map[string]string{"tenant": tenantCtx["id"].(string), "project": projCtx["id"].(string)} {
			run := e.register("run_noauth_"+name, "running", false, readSource(backID, backPinned, "backend"))
			ws := e.workspace("noauth_" + name)
			rg, err := e.sources("", &issuer{e: e, deny: map[string]bool{denied: true}}).Prepare(t.Context(), run, ws)
			if !errors.Is(err, runner.ErrSourceUnavailable) || !errors.Is(err, store.ErrNotFound) || rg != nil {
				t.Fatalf("%s context refused: prepare = %v, %v", name, rg, err)
			}
			if _, statErr := os.Stat(filepath.Join(e.creds, "t_acme_p_erp_run_noauth_"+name)); !os.IsNotExist(statErr) {
				t.Fatalf("%s: the credential directory of a failed preparation remains: %v", name, statErr)
			}
		}
	})

	t.Run("the run's credentials for a context are read-only", func(t *testing.T) {
		run := e.register("run_ro", "running", false, readSource(backID, backPinned, "backend"))
		ws := e.workspace("ro")
		rg, err := e.sources("", is()).Prepare(t.Context(), run, ws)
		if err != nil || rg == nil {
			t.Fatalf("prepare: %v", err)
		}
		defer rg.Close()
		_, work, _ := e.publish("erp", "scratch", map[string]string{"x": "x"})
		for name, repo := range map[string]map[string]any{"tenant": tenantCtx, "project": projCtx} {
			id := repo["id"].(string)
			url := e.cloneURL(repo, readSecret(t, rg, id))
			before := e.mustGit(work, "ls-remote", url)
			e.mustGit(work, "commit", "-q", "--allow-empty", "-m", "by the run")
			for _, ref := range []string{"HEAD:refs/heads/kodama/runs/run_ro/work", "HEAD:refs/heads/main", "HEAD:refs/heads/feature/x", "HEAD:refs/tags/v9"} {
				if out, err := e.git(work, "push", url, ref); err == nil {
					t.Fatalf("a run pushed %s to the %s context\n%s", ref, name, out)
				}
			}
			if after := e.mustGit(work, "ls-remote", url); after != before {
				t.Fatalf("the %s context changed:\n%s\n%s", name, before, after)
			}
		}
	})

	// A source that tracks a symbolic link where the knowledge base goes must not
	// redirect it: no component of a context's destination may be a link.
	for name, c := range map[string]struct {
		at     string // where the source tracks the link
		target func(outside string) string
	}{
		"an ancestor link to a directory":        {".context", func(o string) string { return o }},
		"a dangling ancestor link":               {".context", func(o string) string { return filepath.Join(o, "missing") }},
		"a link in the final component":          {".context/tenant", func(o string) string { return o }},
		"a dangling link in the final component": {".context/project", func(o string) string { return filepath.Join(o, "missing") }},
	} {
		t.Run("a symbolic link in the destination is refused: "+name, func(t *testing.T) {
			outside := filepath.Join(t.TempDir(), "elsewhere")
			if err := os.MkdirAll(filepath.Join(outside, "tenant"), 0o755); err != nil { // an existing "tenant" behind the link
				t.Fatal(err)
			}
			e.seq++
			repoName := "link" + strconv.Itoa(e.seq)
			repo, work, _ := e.publish("erp", repoName, map[string]string{"README.md": "x\n"})
			target := c.target(outside)
			if err := os.MkdirAll(filepath.Dir(filepath.Join(work, c.at)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(work, c.at)); err != nil {
				t.Fatal(err)
			}
			e.mustGit(work, "add", "-A")
			pinned := e.commit(work, "link", nil)
			_, pw, _, _ := e.credential("tok-eng", repo, false)
			if out, err := e.git(work, "push", e.cloneURL(repo, pw), "main:refs/heads/main"); err != nil {
				t.Fatalf("push: %v\n%s", err, out)
			}
			run := e.register("run_link", "running", false, readSource(repo["id"].(string), pinned, "."))
			ws := e.workspace(repoName)
			rg, err := e.sources("", is()).Prepare(t.Context(), run, ws)
			if !errors.Is(err, runner.ErrSourceUnavailable) || rg != nil {
				t.Fatalf("prepare = %v, %v; want ErrSourceUnavailable", rg, err)
			}
			if _, statErr := os.Stat(filepath.Join(e.creds, "t_acme_p_erp_run_link")); !os.IsNotExist(statErr) {
				t.Fatalf("credentials of the failed preparation remain: %v", statErr)
			}
			// Nothing was written through the link.
			entries, _ := os.ReadDir(outside)
			for _, ent := range entries {
				inner, _ := os.ReadDir(filepath.Join(outside, ent.Name()))
				if len(inner) != 0 {
					t.Fatalf("a context was written through the link into %s/%s", outside, ent.Name())
				}
			}
			if _, statErr := os.Stat(filepath.Join(target, "project")); statErr == nil {
				t.Fatal("a context directory appeared behind the link")
			}
		})
	}

	t.Run("a context never overwrites a source path", func(t *testing.T) {
		// This repository tracks a file where the tenant context would be placed.
		clash, _, clashPinned := e.publish("erp", "clash", map[string]string{".context/tenant/mine.md": "from the source\n", "README.md": "clash\n"})
		run := e.register("run_clash", "running", false, readSource(clash["id"].(string), clashPinned, "."))
		ws := e.workspace("clash")
		rg, err := e.sources("", is()).Prepare(t.Context(), run, ws)
		if err != nil || rg == nil {
			t.Fatalf("prepare: %v", err)
		}
		defer rg.Close()
		if got := read(t, filepath.Join(ws, ".context", "tenant", "mine.md")); got != "from the source\n" {
			t.Fatalf("the source's file = %q", got)
		}
		if _, err := os.Stat(filepath.Join(ws, ".context", "tenant", "guidelines.md")); !os.IsNotExist(err) {
			t.Fatalf("the context was written over the source's path: %v", err)
		}
		// The project context has its own path and is placed; the checkout keeps
		// the source's working tree clean of it.
		if got := read(t, filepath.Join(ws, ".context", "project", "architecture.md")); got != "project v2\n" {
			t.Fatalf("project context = %q", got)
		}
		if out := e.mustGit(ws, "status", "--porcelain"); out != "" {
			t.Fatalf("the source's working tree is dirty: %q", out)
		}
	})
}

// Test_unit_kernel_run_checkout_context_absent: a missing context repository is
// not an error.
func Test_unit_kernel_run_checkout_context_absent(t *testing.T) {
	e := newSrcEnv(t)
	back, _, pinned := e.publish("erp", "backend", map[string]string{"README.md": "backend\n"})
	// A context repository that exists but is empty has nothing to read.
	_ = e.createRepo("context", nil)
	run := e.register("run_none", "running", false, readSource(back["id"].(string), pinned, "backend"))
	ws := e.workspace("none")
	rg, err := e.sources("", &issuer{e: e}).Prepare(t.Context(), run, ws)
	if err != nil || rg == nil {
		t.Fatalf("prepare: %v", err)
	}
	defer rg.Close()
	if _, err := os.Stat(filepath.Join(ws, ".context")); !os.IsNotExist(err) {
		t.Fatalf("a context appeared: %v", err)
	}
	// With no sources and no context there is nothing in Git at all.
	bare := e.register("run_bare", "running", false)
	nothing := store.NewMemoryGit()
	nothing.AddTenant(tenantID, "acme")
	nothing.AddProject(tenantID, projectID, "erp")
	gs := e.sources("", &issuer{e: e})
	gs.Repositories = nothing
	rg2, err := gs.Prepare(t.Context(), bare, e.workspace("bare"))
	if err != nil || rg2 != nil {
		t.Fatalf("a run with nothing in Git: %v, %v", rg2, err)
	}
}

// Test_unit_kernel_run_git_credential_outside_workspace: the run's credential is
// a 0600 file in a directory outside the workspace, never in the workspace, a URL
// or a repository's configuration; steps use ordinary Git with it, and a rotation
// replaces the file atomically.
func Test_unit_kernel_run_git_credential_outside_workspace(t *testing.T) {
	e := newSrcEnv(t)
	back, backWork, pinned := e.publish("erp", "backend", map[string]string{"README.md": "backend\n"})
	backID := back["id"].(string)
	branch := "refs/heads/kodama/work/tsk_1"
	run := e.register("run_cred", "running", false, gitv1.Source{RepositoryID: backID, Commit: pinned, Path: "backend", Access: gitv1.AccessWrite, Branch: &branch})
	ws := e.workspace("cred")
	is := &issuer{e: e}
	gs := e.sources("", is)
	rg, err := gs.Prepare(t.Context(), run, ws)
	if err != nil || rg == nil {
		t.Fatalf("prepare: %v", err)
	}
	defer rg.Close()

	dir := rg.Dir()
	if rel, err := filepath.Rel(ws, dir); err == nil && !strings.HasPrefix(rel, "..") {
		t.Fatalf("the credential directory %s is inside the workspace %s", dir, ws)
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("credential directory = %v, %v", info, err)
	}
	file := filepath.Join(dir, backID+".credential")
	if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credential file = %v, %v", info, err)
	}
	secret := readSecret(t, rg, backID)
	if !strings.HasPrefix(secret, "gitc_") {
		t.Fatalf("credential = %q", secret)
	}
	mustNotContain(t, ws, secret)
	config := read(t, filepath.Join(dir, runner.GitConfigFile))
	if strings.Contains(config, secret) {
		t.Fatal("the generated Git configuration holds the credential")
	}
	repoConfig := read(t, filepath.Join(ws, "backend", ".git", "config"))
	if strings.Contains(repoConfig, secret) || strings.Contains(repoConfig, "@") || strings.Contains(repoConfig, "extraheader") || strings.Contains(repoConfig, "helper") {
		t.Fatalf(".git/config carries authority:\n%s", repoConfig)
	}
	if entries, _ := os.ReadDir(ws); len(entries) != 1 || entries[0].Name() != "backend" {
		t.Fatalf("the workspace holds more than the source: %v", entries)
	}

	// A step uses ordinary Git: the exported environment and the helper answer for
	// this repository only, reading the file when Git asks.
	spec := runner.Spec{Scope: domain.Scope{TenantID: tenantID, ProjectID: projectID}, RunID: "run_cred", StepKey: "push",
		Workspace: ws, GitDir: dir, Commands: []string{
			"cd backend",
			"test \"$KODAMA_GIT_DIR\" = '" + dir + "'",
			"git ls-remote origin refs/heads/main",
			"git fetch -q origin main",
			"git -c user.name=T -c user.email=t@example.invalid commit -q --allow-empty -m step",
			"git push -q origin HEAD:refs/heads/kodama/runs/run_cred/step",
		}}
	var logs bytes.Buffer
	if code, err := (runner.Process{}).Execute(t.Context(), spec, &logs); err != nil || code != 0 {
		t.Fatalf("step = %d, %v\n%s", code, err, logs.String())
	}
	if got := e.mustGit(backWork, "ls-remote", e.cloneURL(back, secret), "refs/heads/kodama/runs/run_cred/step"); got == "" {
		t.Fatal("the step's push did not reach the repository")
	}
	// The same credential does not open another repository: the helper offers it
	// to its own URL only.
	other, _, _ := e.publish("erp", "other", map[string]string{"x": "x"})
	otherURL := strings.TrimSuffix(e.mustGit(filepath.Join(ws, "backend"), "remote", "get-url", "origin"), "backend.git") + "other.git"
	spec.Commands = []string{"git ls-remote " + otherURL}
	logs.Reset()
	if code, _ := (runner.Process{}).Execute(t.Context(), spec, &logs); code == 0 {
		t.Fatalf("the run's credential answered for a repository outside its sources (%v)\n%s", other["id"], logs.String())
	}

	// Rotation after half the lifetime replaces the file atomically, in place.
	if err := rg.Refresh(t.Context()); err != nil || readSecret(t, rg, backID) != secret {
		t.Fatalf("a fresh credential was rotated early: %v", err)
	}
	e.clock.Add(int64(45 * time.Minute))
	if err := rg.Refresh(t.Context()); err != nil {
		t.Fatalf("rotation: %v", err)
	}
	rotated := readSecret(t, rg, backID)
	if rotated == secret || !strings.HasPrefix(rotated, "gitc_") {
		t.Fatalf("credential after rotation = %q", rotated)
	}
	if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("rotated file = %v, %v", info, err)
	}
	entries, _ := os.ReadDir(dir)
	for _, ent := range entries {
		if strings.HasPrefix(ent.Name(), ".tmp-") {
			t.Fatalf("a temporary file of the rotation remains: %s", ent.Name())
		}
	}
	spec.Commands = []string{"cd backend", "git ls-remote origin refs/heads/main"}
	if code, err := (runner.Process{}).Execute(t.Context(), spec, &logs); err != nil || code != 0 {
		t.Fatalf("the step does not use the rotated credential: %d %v\n%s", code, err, logs.String())
	}
	mustNotContain(t, ws, secret, rotated)

	// The files end with the run.
	if err := rg.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the credential directory outlives the run: %v", err)
	}
	if err := rg.Close(); err != nil {
		t.Fatalf("closing twice: %v", err)
	}
}

// Test_unit_kernel_run_git_credential_revoked: a cancelled or finished run's
// credential is refused by the Git service, the runner removes its file instead
// of keeping a stale one, and a run that is over cannot be issued another.
func Test_unit_kernel_run_git_credential_revoked(t *testing.T) {
	e := newSrcEnv(t)
	back, backWork, pinned := e.publish("erp", "backend", map[string]string{"README.md": "backend\n"})
	backID := back["id"].(string)
	src := readSource(backID, pinned, "backend")

	prepared := func(runID string) (*runner.RunGit, store.Run, string) {
		e.clock.Store(time.Now().UnixNano()) // earlier cases moved the clock
		run := e.register(runID, "running", false, src)
		rg, err := e.sources("", &issuer{e: e}).Prepare(t.Context(), run, e.workspace(runID))
		if err != nil || rg == nil {
			t.Fatalf("prepare: %v", err)
		}
		t.Cleanup(func() { _ = rg.Close() })
		return rg, run, readSecret(t, rg, backID)
	}
	opens := func(secret string) bool {
		_, err := e.git(backWork, "ls-remote", e.cloneURL(back, secret), "refs/heads/main")
		return err == nil
	}

	t.Run("a cancelled run", func(t *testing.T) {
		rg, _, secret := prepared("run_cancel")
		if !opens(secret) {
			t.Fatal("the live run's credential is refused")
		}
		// RequestCancel: the run is cancelling and its credentials are revoked.
		e.register("run_cancel", "running", true, src)
		if err := e.mem.RevokeRunCredentials(t.Context(), tenantID, projectID, "run_cancel"); err != nil {
			t.Fatal(err)
		}
		if opens(secret) {
			t.Fatal("a cancelled run's credential still opens the repository")
		}
		e.clock.Add(int64(45 * time.Minute))
		err := rg.Refresh(t.Context())
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("rotation of a cancelled run's credential = %v, want a refusal", err)
		}
		if _, statErr := os.Stat(filepath.Join(rg.Dir(), backID+".credential")); !os.IsNotExist(statErr) {
			t.Fatalf("the runner keeps the stale credential file: %v", statErr)
		}
		// A step that still runs has no credential to use.
		var logs bytes.Buffer
		spec := runner.Spec{Workspace: e.workspace("run_cancel"), GitDir: rg.Dir(), Commands: []string{"cd backend", "git ls-remote origin refs/heads/main"}}
		if code, _ := (runner.Process{}).Execute(t.Context(), spec, &logs); code == 0 {
			t.Fatalf("a step used a credential of a cancelled run\n%s", logs.String())
		}
	})

	t.Run("a finished run", func(t *testing.T) {
		rg, _, secret := prepared("run_done")
		e.register("run_done", "succeeded", false, src)
		if err := e.mem.RevokeRunCredentials(t.Context(), tenantID, projectID, "run_done"); err != nil {
			t.Fatal(err)
		}
		if opens(secret) {
			t.Fatal("a finished run's credential still opens the repository")
		}
		e.clock.Add(int64(45 * time.Minute))
		if err := rg.Refresh(t.Context()); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("rotation after the run finished = %v", err)
		}
		if _, err := os.Stat(filepath.Join(rg.Dir(), backID+".credential")); !os.IsNotExist(err) {
			t.Fatalf("stale credential file: %v", err)
		}
		// Nothing is issued once the run is over, so a late preparation fails.
		run := store.Run{TenantID: tenantID, ProjectID: projectID, ID: "run_done", TenantKey: "acme", ProjectKey: "erp", Sources: []gitv1.Source{src}}
		late, err := e.sources("", &issuer{e: e}).Prepare(t.Context(), run, e.workspace("run_done_late"))
		if !errors.Is(err, store.ErrConflict) || !errors.Is(err, runner.ErrSourceUnavailable) || late != nil {
			t.Fatalf("a finished run was prepared: %v, %v", late, err)
		}
	})

	t.Run("a transient failure keeps the credential that is still valid", func(t *testing.T) {
		run := e.register("run_blip", "running", false, src)
		is := &issuer{e: e}
		rg, err := e.sources("", is).Prepare(t.Context(), run, e.workspace("run_blip"))
		if err != nil || rg == nil {
			t.Fatalf("prepare: %v", err)
		}
		defer rg.Close()
		secret := readSecret(t, rg, backID)
		// A rotation that fails for a reason other than a refusal leaves the old
		// file, valid until it expires.
		is.failing.Store(true)
		e.clock.Add(int64(45 * time.Minute))
		if err := rg.Refresh(t.Context()); err == nil || errors.Is(err, store.ErrConflict) {
			t.Fatalf("rotation during an outage = %v", err)
		}
		if got := readSecret(t, rg, backID); got != secret {
			t.Fatal("a failed rotation replaced the credential")
		}
	})

	t.Run("cancelled right after issuance: the files go at once, without rotation being due", func(t *testing.T) {
		rg, _, secret := prepared("run_instant")
		file := filepath.Join(rg.Dir(), backID+".credential")
		if err := rg.Refresh(t.Context()); err != nil || readSecret(t, rg, backID) != secret {
			t.Fatalf("refresh of a live run: %v", err)
		}
		// The clock does not move: the one-hour credential has an hour to live.
		e.register("run_instant", "running", true, src)
		err := rg.Refresh(t.Context())
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("refresh of a cancelled run = %v, want a refusal", err)
		}
		if _, statErr := os.Stat(file); !os.IsNotExist(statErr) {
			t.Fatalf("the credential file of a cancelled run remains: %v", statErr)
		}
		spec := runner.Spec{Workspace: e.workspace("run_instant"), GitDir: rg.Dir(), Commands: []string{"cd backend", "git ls-remote origin refs/heads/main"}}
		var logs bytes.Buffer
		if code, _ := (runner.Process{}).Execute(t.Context(), spec, &logs); code == 0 {
			t.Fatalf("a step used the credential of a cancelled run\n%s", logs.String())
		}
		// The finished case is the same.
		rg2, _, _ := prepared("run_instant_done")
		e.register("run_instant_done", "failed", false, src)
		if err := rg2.Refresh(t.Context()); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("refresh of a finished run = %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(rg2.Dir(), backID+".credential")); !os.IsNotExist(statErr) {
			t.Fatalf("the credential file of a finished run remains: %v", statErr)
		}
	})

	t.Run("not knowing whether the run is live keeps the credentials and retries", func(t *testing.T) {
		rg, _, secret := prepared("run_unknown")
		e.live.unknown.Store(true)
		err := rg.Refresh(t.Context())
		e.live.unknown.Store(false)
		if err == nil || errors.Is(err, store.ErrConflict) {
			t.Fatalf("refresh during an outage = %v", err)
		}
		if readSecret(t, rg, backID) != secret {
			t.Fatal("an outage replaced or removed the credential")
		}
		e.register("run_unknown", "running", true, src)
		if err := rg.Refresh(t.Context()); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("once the answer is known: %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(rg.Dir(), backID+".credential")); !os.IsNotExist(statErr) {
			t.Fatal("the cancelled run kept its file")
		}
	})

	t.Run("an outage of the repository read is not revocation and recovery rotates", func(t *testing.T) {
		signer, err := serve.NewGitSigner("development", "", "")
		if err != nil {
			t.Fatal(err)
		}
		flaky := &flakyRepos{GitBackend: e.mem}
		server, err := api.NewGit(api.GitConfig{Store: flaky, Auth: authMap(tokens), Workers: e.workers, Signer: signer, Identities: signer.Verifier()})
		if err != nil {
			t.Fatal(err)
		}
		e.clock.Store(time.Now().UnixNano())
		run := e.register("run_outage", "running", false, src)
		rg, err := e.sources("", &issuer{e: e, server: server}).Prepare(t.Context(), run, e.workspace("run_outage"))
		if err != nil || rg == nil {
			t.Fatalf("prepare: %v", err)
		}
		defer rg.Close()
		secret := readSecret(t, rg, backID)
		e.clock.Add(int64(45 * time.Minute)) // rotation is due
		flaky.down.Store(true)
		err = rg.Refresh(t.Context())
		if err == nil || errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
			t.Fatalf("refresh during the outage = %v", err)
		}
		if readSecret(t, rg, backID) != secret {
			t.Fatal("the outage removed or replaced a still-valid credential")
		}
		if !opens(secret) {
			t.Fatal("the kept credential no longer opens the repository")
		}
		flaky.down.Store(false)
		if err := rg.Refresh(t.Context()); err != nil {
			t.Fatalf("refresh after recovery: %v", err)
		}
		if rotated := readSecret(t, rg, backID); rotated == secret || !opens(rotated) {
			t.Fatal("the credential was not rotated after the recovery")
		}
	})

	t.Run("the keeper rotates while a step runs and stops with it", func(t *testing.T) {
		rg, _, secret := prepared("run_keep")
		e.clock.Add(int64(45 * time.Minute))
		stop := rg.Keep(t.Context(), 10*time.Millisecond)
		deadline := time.Now().Add(5 * time.Second)
		for readSecret(t, rg, backID) == secret && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		stop()
		if readSecret(t, rg, backID) == secret {
			t.Fatal("the keeper never rotated the credential")
		}
	})
}

// flakyRepos fails repository reads while down is set.
type flakyRepos struct {
	store.GitBackend
	down atomic.Bool
}

func (f *flakyRepos) GetRepository(ctx context.Context, tenant domain.TenantID, id string) (store.Repository, error) {
	if f.down.Load() {
		return store.Repository{}, errors.New("the database is unavailable")
	}
	return f.GitBackend.GetRepository(ctx, tenant, id)
}

// Test_unit_kernel_run_checkout_context_states: a context repository that does not
// exist, or exists and is empty, is skipped; one that exists with readable history
// is read (an archived repository refuses pushes, not history); one that exists and
// is not usable fails the run's sources instead of starting without it.
func Test_unit_kernel_run_checkout_context_states(t *testing.T) {
	e := newSrcEnv(t)
	back, _, pinned := e.publish("erp", "backend", map[string]string{"README.md": "backend\n"})
	backSrc := readSource(back["id"].(string), pinned, "backend")
	projCtx, _, _ := e.publish("erp", "context", map[string]string{"architecture.md": "project\n"})
	projID := projCtx["id"].(string)

	t.Run("an archived context is read like any other", func(t *testing.T) {
		if _, err := e.mem.SetRepositoryStatus(t.Context(), tenantID, projID, store.RepoArchived, "archived", "user", "u_eng"); err != nil {
			t.Fatal(err)
		}
		run := e.register("run_archived", "running", false, backSrc)
		ws := e.workspace("archived")
		rg, err := e.sources("", &issuer{e: e}).Prepare(t.Context(), run, ws)
		if err != nil || rg == nil {
			t.Fatalf("prepare: %v", err)
		}
		defer rg.Close()
		if got := read(t, filepath.Join(ws, ".context", "project", "architecture.md")); got != "project\n" {
			t.Fatalf("archived context = %q", got)
		}
	})

	t.Run("an archived context whose history cannot be read fails the run", func(t *testing.T) {
		run := e.register("run_archived_denied", "running", false, backSrc)
		rg, err := e.sources("", &issuer{e: e, deny: map[string]bool{projID: true}}).Prepare(t.Context(), run, e.workspace("archived_denied"))
		if !errors.Is(err, runner.ErrSourceUnavailable) || rg != nil {
			t.Fatalf("prepare = %v, %v", rg, err)
		}
	})

	t.Run("a context that is still provisioning fails the run", func(t *testing.T) {
		other := newSrcEnv(t)
		backOther, _, pin := other.publish("erp", "backend", map[string]string{"README.md": "backend\n"})
		if _, _, err := other.mem.CreateRepository(t.Context(), store.Repository{TenantID: tenantID, ProjectID: projectID, ID: "repo_prov", Name: "context", DefaultBranch: "main"}); err != nil {
			t.Fatal(err)
		}
		run := other.register("run_prov", "running", false, readSource(backOther["id"].(string), pin, "backend"))
		rg, err := other.sources("", &issuer{e: other}).Prepare(t.Context(), run, other.workspace("prov"))
		if !errors.Is(err, runner.ErrSourceUnavailable) || !strings.Contains(err.Error(), "provisioning") || rg != nil {
			t.Fatalf("prepare = %v, %v", rg, err)
		}
	})

	t.Run("a failed lookup is not an absent context", func(t *testing.T) {
		gs := e.sources("", &issuer{e: e})
		gs.Repositories = brokenDirectory{e.mem}
		run := e.register("run_lookup", "running", false, backSrc)
		rg, err := gs.Prepare(t.Context(), run, e.workspace("lookup"))
		if !errors.Is(err, runner.ErrSourceUnavailable) || rg != nil {
			t.Fatalf("prepare = %v, %v", rg, err)
		}
	})
}

// brokenDirectory fails the context lookup as a database outage would.
type brokenDirectory struct{ *store.MemoryGit }

func (brokenDirectory) RepositoryByPath(context.Context, domain.TenantID, string, string) (store.Repository, error) {
	return store.Repository{}, errors.New("the database is unavailable")
}
