package serve_test

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/internal/kernel/api"
	"github.com/optimiware/kodama/internal/kernel/serve"
	"github.com/optimiware/kodama/internal/kernel/store"
)

func init() {
	tokens["tok-globex"] = api.Principal{UserID: "u_globex"}
}

// Test_unit_kernel_git_development_hosting: KODAMA_ENV=development hosts one
// Git worker per tenant in the server process, each on its own data root below
// the state directory, and needs no external Git credential, worker address,
// control token or signing key file. The data root is removed with the state
// directory (`make dev-down`); stopping the server stops the workers.
func Test_unit_kernel_git_development_hosting(t *testing.T) {
	e := newGitEnv(t)
	e.mem.AddTenant("t_globex", "globex")
	e.mem.AddProject("t_globex", "p_crm", "crm")
	e.mem.Grant("t_globex", "u_globex", "p_crm", "engineer")

	state := filepath.Join(e.dir, "state")
	if _, err := os.Stat(filepath.Join(state, "git")); !os.IsNotExist(err) {
		t.Fatalf("a worker was started before any request: %v", err)
	}

	acme := e.createRepo("backend", nil)
	code, globex := e.api(http.MethodPost, "/api/v1/tenants/globex/projects/p_crm/repositories", "tok-globex", map[string]any{"name": "backend"})
	if code != http.StatusCreated || globex["status"] != "ready" {
		t.Fatalf("create in the second tenant: %d %v", code, globex)
	}

	// One data root per tenant, named by the tenant ID, below the state directory
	// and nowhere else; the repositories inside are separate.
	entries, err := os.ReadDir(filepath.Join(state, "git"))
	if err != nil {
		t.Fatal(err)
	}
	var roots []string
	for _, entry := range entries {
		roots = append(roots, entry.Name())
	}
	sort.Strings(roots)
	if strings.Join(roots, ",") != "t_acme,t_globex" {
		t.Fatalf("data roots = %v, want one per tenant", roots)
	}
	for _, repo := range []map[string]any{acme, globex} {
		tenant, _ := repo["tenantId"].(string)
		found := false
		_ = filepath.WalkDir(filepath.Join(state, "git", tenant), func(path string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() && strings.Contains(path, repo["id"].(string)) {
				found = true
			}
			return nil
		})
		if !found {
			t.Errorf("the repository %v has no bare repository under its tenant's data root", repo["id"])
		}
		other := "t_globex"
		if tenant == "t_globex" {
			other = "t_acme"
		}
		_ = filepath.WalkDir(filepath.Join(state, "git", other), func(path string, d os.DirEntry, err error) error {
			if err == nil && strings.Contains(path, repo["id"].(string)) {
				t.Errorf("the repository %v appears under the other tenant's data root: %s", repo["id"], path)
			}
			return nil
		})
	}

	// A person of one tenant cannot reach the other tenant's repository, with or
	// without a credential: the same not-found either way.
	if code, _ := e.api(http.MethodGet, "/api/v1/tenants/acme/projects/p_erp/repositories/"+globex["id"].(string), "tok-eng", nil); code != http.StatusNotFound {
		t.Errorf("another tenant's repository read as %d", code)
	}
	if code, _ := e.api(http.MethodGet, "/api/v1/tenants/acme/projects/p_erp/repositories/"+acme["id"].(string), "tok-globex", nil); code != http.StatusNotFound {
		t.Errorf("a person without a role read the repository: %d", code)
	}

	// A credential works against its own tenant's worker with nothing configured.
	_, pw, status, body := e.credential("tok-eng", acme, true)
	if status != http.StatusCreated {
		t.Fatalf("seed credential: %d %v", status, body)
	}
	work, _ := e.work()
	if out, err := e.git(work, "push", e.cloneURL(acme, pw), "main:refs/heads/main"); err != nil {
		t.Fatalf("push through the in-process worker: %v\n%s", err, out)
	}

	// Stopping the server stops the workers: nothing is served afterwards.
	if err := e.workers.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.workers.GitHandler(t.Context(), "t_acme"); err == nil {
		t.Fatal("a stopped server still hands out a worker")
	}
}

// Test_unit_kernel_git_bootstrap_roundtrip is the documented bootstrap
// (dev/README.md) end to end over the assembled kernel: an in-memory store, an
// in-process worker and the real Git client. A repository is created, a seed
// credential is requested, a pipeline definition is pushed, the pipeline is
// indexed, and the run it triggers is admitted pinned to the pushed commit.
func Test_unit_kernel_git_bootstrap_roundtrip(t *testing.T) {
	d := newDispatchEnv(t)
	const unseeded = "frontend"
	repo := d.createRepo(unseeded, map[string]any{"directPushToDefault": true})
	id := repo["id"].(string)
	d.repo, d.id = repo, id
	if repo["empty"] != true {
		t.Fatalf("a created repository is not empty: %v", repo)
	}
	// Nothing is indexed and nothing runs before the first push.
	if got := d.mem.Pipelines("t_acme", id); len(got) != 0 {
		t.Fatalf("pipelines before the first push: %+v", got)
	}
	runsBefore := len(d.mem.AdmittedRuns())

	// The local working copy: one commit that defines a pipeline run on pushes to main.
	work := filepath.Join(d.dir, "bootstrap")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	d.mustGit(work, "init", "-q", "-b", "main")
	pushed := d.commitFiles(work, "bootstrap", files(
		"README.md", "# frontend\n",
		".kodama/build.yaml", pipelineDoc("build", "bootstrapped", onMain)))

	// An ordinary credential cannot create the first branch; the seed can, once.
	_, plain, _, _ := d.credential("tok-eng", repo, false)
	if out, err := d.git(work, "push", d.cloneURL(repo, plain), "main:refs/heads/main"); err == nil {
		t.Fatalf("an ordinary credential seeded an empty repository:\n%s", out)
	}
	if got := d.mem.Pipelines("t_acme", id); len(got) != 0 {
		t.Fatalf("a refused push indexed a pipeline: %+v", got)
	}
	_, seed, status, body := d.credential("tok-eng", repo, true)
	if status != http.StatusCreated {
		t.Fatalf("seed credential: %d %v", status, body)
	}
	if out, err := d.git(work, "push", d.cloneURL(repo, seed), "main:refs/heads/main"); err != nil {
		t.Fatalf("seed push: %v\n%s", err, out)
	}
	if out, err := d.git(work, "push", d.cloneURL(repo, seed), "main:refs/heads/second"); err == nil {
		t.Fatalf("the seed credential was used twice:\n%s", out)
	}

	// The push committed one event; the publisher delivers it, the dispatcher
	// reads the definition at the pushed commit, indexes it and admits a run.
	if got := len(d.refsEvents()); got != 1 {
		t.Fatalf("git.refs_updated events = %d, want 1", got)
	}
	d.drain()
	build := d.pipeline("build")
	if build.Disabled || build.RepositoryID != id || build.SourceCommit != pushed || commandOf(t, build) != "echo bootstrapped" {
		t.Fatalf("the indexed pipeline = %+v", build)
	}
	runs := d.runs("build")
	if len(runs) != 1 || len(d.mem.AdmittedRuns()) != runsBefore+1 {
		t.Fatalf("runs after the seed push = %+v", d.mem.AdmittedRuns())
	}
	sources := d.sourcesOf(runs[0])
	if len(sources) != 1 || sources[0].RepositoryID != id || sources[0].Commit != pushed || sources[0].Access != "read" {
		t.Fatalf("the run is not pinned to the pushed commit %s: %+v", pushed, sources)
	}
	if d.woken.Load() == 0 {
		t.Fatal("the admitted run did not wake the scheduler")
	}

	// The hosted repository now holds exactly that commit.
	code, refs := d.api(http.MethodGet, "/api/v1/tenants/acme/projects/p_erp/repositories/"+id+"/refs", "tok-eng", nil)
	tips, _ := refs["refs"].([]any)
	if code != http.StatusOK || len(tips) != 1 || tips[0].(map[string]any)["commit"] != pushed || refs["empty"] != false {
		t.Fatalf("refs = %d %v", code, refs)
	}

	// Redelivery of the same event neither indexes twice nor starts a second run.
	d.handle(d.refsEvents()[0])
	if got := d.runs("build"); len(got) != 1 {
		t.Fatalf("a redelivered event admitted another run: %+v", got)
	}

	// A later push to main starts the next run, pinned to its own commit.
	_, pw, _, _ := d.credential("tok-eng", repo, false)
	next := d.commitFiles(work, "second", files("src.txt", "x\n"))
	if out, err := d.git(work, "push", d.cloneURL(repo, pw), "main:refs/heads/main"); err != nil {
		t.Fatalf("second push: %v\n%s", err, out)
	}
	d.drain()
	runs = d.runs("build")
	if len(runs) != 2 || d.sourcesOf(runs[1])[0].Commit != next || d.sourcesOf(runs[0])[0].Commit != pushed {
		t.Fatalf("runs after the second push = %+v", runs)
	}
}

// Test_unit_kernel_git_refs_cursor_follows_pages: the refs cursor is the name of
// the last ref of the page, which the Git cursor pattern of the API document
// accepts (the common authenticated cursor would not), and passing it back
// returns the next page without a repeat or a gap.
func Test_unit_kernel_git_refs_cursor_follows_pages(t *testing.T) {
	e := newGitEnv(t)
	repo := e.createRepo("backend", map[string]any{"directPushToDefault": true})
	id := repo["id"].(string)
	work, _ := e.work()
	_, seed, status, body := e.credential("tok-eng", repo, true)
	if status != http.StatusCreated {
		t.Fatalf("seed credential: %d %v", status, body)
	}
	if out, err := e.git(work, "push", e.cloneURL(repo, seed), "main:refs/heads/main"); err != nil {
		t.Fatalf("seed push: %v\n%s", err, out)
	}
	_, pw, _, _ := e.credential("tok-eng", repo, false)
	for _, branch := range []string{"b1", "b2", "b3", "feature/x"} {
		if out, err := e.git(work, "push", e.cloneURL(repo, pw), "main:refs/heads/"+branch); err != nil {
			t.Fatalf("push %s: %v\n%s", branch, err, out)
		}
	}

	// The pattern is the document's own.
	raw, err := os.ReadFile("../../../api/kernel-git.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Pattern string `yaml:"pattern"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(doc.Components.Schemas["GitRefsCursorValue"].Pattern)
	if doc.Components.Schemas["GitRefsCursorValue"].Pattern == "" {
		t.Fatal("the document has no Git refs cursor pattern")
	}

	path := "/api/v1/tenants/acme/projects/p_erp/repositories/" + id + "/refs"
	var seen []string
	query := "?limit=2"
	pages := 0
	for {
		code, page := e.api(http.MethodGet, path+query, "tok-eng", nil)
		if code != http.StatusOK {
			t.Fatalf("refs%s: %d %v", query, code, page)
		}
		pages++
		tips, _ := page["refs"].([]any)
		for _, tip := range tips {
			seen = append(seen, tip.(map[string]any)["ref"].(string))
		}
		next, _ := page["nextCursor"].(string)
		if next == "" {
			if page["nextCursor"] != nil {
				t.Fatalf("nextCursor = %v", page["nextCursor"])
			}
			break
		}
		if !pattern.MatchString(next) || !strings.HasPrefix(next, "refs/heads/") {
			t.Fatalf("the cursor %q is not accepted by the document's pattern %s", next, pattern)
		}
		if len(tips) != 2 {
			t.Fatalf("a page with a next cursor has %d refs", len(tips))
		}
		query = "?limit=2&cursor=" + url.QueryEscape(next)
		if pages > 10 {
			t.Fatal("the listing does not end")
		}
	}
	want := []string{"refs/heads/b1", "refs/heads/b2", "refs/heads/b3", "refs/heads/feature/x", "refs/heads/main"}
	if pages != 3 || strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Fatalf("pages = %d, refs = %v, want %v over 3 pages", pages, seen, want)
	}
	// A cursor that is not a ref name is refused, not guessed.
	if code, body := e.api(http.MethodGet, path+"?cursor="+url.QueryEscape("not a ref"), "tok-eng", nil); code != http.StatusUnprocessableEntity {
		t.Fatalf("a malformed cursor answered %d %v, want 422", code, body)
	}
}

// Test_unit_kernel_git_worker_unreachable_is_503: when a remote worker cannot be
// reached the /git/ route answers 503 with Retry-After and the Git error body,
// not the reverse proxy's bare 502, and says nothing of the worker's address.
func Test_unit_kernel_git_worker_unreachable_is_503(t *testing.T) {
	// A port nothing listens on.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	mem := store.NewMemoryGit()
	mem.AddTenant("t_acme", "acme")
	mem.AddProject("t_acme", "p_erp", "erp")
	mem.Grant("t_acme", "u_eng", "p_erp", "engineer")
	signer, err := serve.NewGitSigner("development", "", "")
	if err != nil {
		t.Fatal(err)
	}
	workers, err := serve.NewGitWorkers(serve.GitWorkersConfig{Env: "production", WorkerURL: "http://" + addr + "/{tenantId}", ControlToken: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	git, err := api.NewGit(api.GitConfig{Store: mem, Auth: authMap(tokens), Workers: workers, Signer: signer, Identities: signer.Verifier()})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(git.Handler())
	t.Cleanup(srv.Close)

	// A ready repository (the worker confirmed it earlier) and a human credential.
	repo, _, err := mem.CreateRepository(t.Context(), store.Repository{TenantID: "t_acme", ProjectID: "p_erp", ID: "repo_down", Name: "backend", DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mem.ConfirmInitialized(t.Context(), "t_acme", repo.ID, "main", true); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/tenants/acme/git-credentials",
		strings.NewReader(`{"projectId":"p_erp","repositoryId":"repo_down","seed":false}`))
	req.Header.Set("Authorization", "Bearer tok-eng")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var cred struct{ Password string }
	_ = json.NewDecoder(resp.Body).Decode(&cred)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || cred.Password == "" {
		t.Fatalf("credential: %d", resp.StatusCode)
	}

	get, _ := http.NewRequest(http.MethodGet, srv.URL+"/git/acme/erp/backend.git/info/refs?service=git-upload-pack", nil)
	get.SetBasicAuth("kodama", cred.Password)
	resp, err = http.DefaultClient.Do(get)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("an unreachable worker answered %d (Retry-After %q): %s", resp.StatusCode, resp.Header.Get("Retry-After"), data)
	}
	var e struct {
		Error struct{ Code, Message string }
	}
	if json.Unmarshal(data, &e) != nil || e.Error.Code != "unavailable" || strings.Contains(string(data), addr) {
		t.Fatalf("body = %s", data)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type %q", ct)
	}
}
