package serve_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/shell/api"
	"github.com/optimiware/kodama/internal/shell/bus"
	"github.com/optimiware/kodama/internal/shell/serve"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/test/support/postgres"
)

// Test_unit_kernel_git_bootstrap_roundtrip_postgres is the bootstrap roundtrip
// over the assembled kernel against a real PostgreSQL (skipped without
// KODAMA_TEST_POSTGRES_ADMIN_URL; KODAMA_REQUIRE_POSTGRES=1 fails instead): the
// migrated schema, `kodama seed` with the admin and engineer roles, the kernel's
// own HTTP routes for the project, the repository and the credential, the real
// Git client, an in-process tenant worker, the outbox publisher and the
// dispatcher, and every read through the API. It is the in-memory
// Test_unit_kernel_git_bootstrap_roundtrip with the store replaced.
func Test_unit_kernel_git_bootstrap_roundtrip_postgres(t *testing.T) {
	ctx := t.Context()
	db := postgres.New(t)
	mig := db.MigrationPool(ctx, 2)
	manifest, err := storage.NewManifest(storage.BaseContribution())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Migrate(ctx, mig, manifest, storage.MigrateOptions{RuntimeRole: db.RuntimeRole}); err != nil {
		t.Fatal(err)
	}
	// The operator command, with the roles the documented bootstrap names.
	tenant, err := serve.Seed(ctx, db.MigrationURL, "acme", "u_dev", "kodama:dev", "u_dev", "Developer", "admin", "engineer")
	if err != nil || tenant != "t_acme" {
		t.Fatalf("seed = %q, %v", tenant, err)
	}
	st := store.New(db.RuntimePool(ctx, 8))

	signer, err := serve.NewGitSigner("development", "", "")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	workers, err := serve.NewGitWorkers(serve.GitWorkersConfig{Env: "development", StateDir: filepath.Join(dir, "state"), Verifier: signer.Verifier(), HookBinary: hookBinary(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = workers.Close() })
	auth := api.StaticToken{Token: "tok-dev", UserID: "u_dev", Name: "Developer"}
	e := &gitEnv{t: t, dir: dir}
	git, err := api.NewGit(api.GitConfig{Store: st, Auth: auth, Workers: workers, Signer: signer, Identities: signer.Verifier()})
	if err != nil {
		t.Fatal(err)
	}
	workers.BindControl(git.Control())
	server, err := api.New(api.Config{Store: st, Auth: auth, InstallationID: "test", Git: git})
	if err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewServer(server.Handler())
	t.Cleanup(e.srv.Close)
	events, err := serve.NewEvents(ctx, serve.EventsConfig{Store: st, Files: workers, Bus: bus.NewMemory()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(events.Close)

	get := func(path string) map[string]any {
		t.Helper()
		code, body := e.api(http.MethodGet, path, "tok-dev", nil)
		if code != http.StatusOK {
			t.Fatalf("GET %s: %d %v", path, code, body)
		}
		return body
	}

	items := func(path string) []any {
		t.Helper()
		list, _ := get(path)["items"].([]any) // an empty list is null on the wire
		return list
	}

	// 1. A project and 2. a repository, over the kernel's HTTP routes.
	code, project := e.api(http.MethodPost, "/api/v1/tenants/acme/projects", "tok-dev", map[string]any{"key": "demo", "displayName": "Demo"})
	projectID, _ := project["id"].(string)
	if code != http.StatusCreated || projectID == "" {
		t.Fatalf("create project: %d %v", code, project)
	}
	code, repo := e.api(http.MethodPost, "/api/v1/tenants/acme/projects/demo/repositories", "tok-dev", map[string]any{"name": "hello"})
	if code != http.StatusCreated || repo["status"] != "ready" || repo["empty"] != true {
		t.Fatalf("create repository: %d %v", code, repo)
	}
	repoID := repo["id"].(string)

	// The local working copy with one commit that defines a pipeline run on main.
	work := filepath.Join(dir, "hello")
	if err := os.MkdirAll(filepath.Join(work, ".kodama"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.mustGit(work, "init", "-q", "-b", "main")
	doc := pipelineDoc("build", "bootstrapped", onMain)
	if err := os.WriteFile(filepath.Join(work, ".kodama", "build.yaml"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("# hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.mustGit(work, "add", "-A")
	e.mustGit(work, "commit", "-q", "-m", "first commit")
	pushed := e.mustGit(work, "rev-parse", "HEAD")

	// 3. An ordinary credential cannot create the first branch; the seed can, once.
	_, plain, status, body := e.credential("tok-dev", repo, false)
	if status != http.StatusCreated {
		t.Fatalf("credential: %d %v", status, body)
	}
	if out, err := e.git(work, "push", e.cloneURL(repo, plain), "main:refs/heads/main"); err == nil {
		t.Fatalf("an ordinary credential seeded an empty repository:\n%s", out)
	}
	if got := items("/api/v1/tenants/acme/projects/" + projectID + "/pipelines"); len(got) != 0 {
		t.Fatalf("a refused push indexed a pipeline: %v", got)
	}
	_, seed, status, body := e.credential("tok-dev", repo, true)
	if status != http.StatusCreated {
		t.Fatalf("seed credential: %d %v", status, body)
	}
	// 4. git push of the pipeline definition with the seed credential.
	if out, err := e.git(work, "push", e.cloneURL(repo, seed), "main:refs/heads/main"); err != nil {
		t.Fatalf("seed push: %v\n%s", err, out)
	}
	if out, err := e.git(work, "push", e.cloneURL(repo, seed), "main:refs/heads/second"); err == nil {
		t.Fatalf("the seed credential was used twice:\n%s", out)
	}

	// 5. The receive is recorded as one event; publishing it indexes the pipeline
	// from the pushed commit and admits the run pinned to it.
	res, err := events.Publisher.Drain(ctx)
	if err != nil || res.Failed != 0 {
		t.Fatalf("drain = %+v, %v", res, err)
	}
	pipelines := items("/api/v1/tenants/acme/projects/" + projectID + "/pipelines")
	if len(pipelines) != 1 {
		t.Fatalf("pipelines = %v", pipelines)
	}
	built := pipelines[0].(map[string]any)
	if built["name"] != "build" || built["source"] != "git" || built["sourceCommit"] != pushed || built["repositoryId"] != repoID || built["disabled"] == true {
		t.Fatalf("the indexed pipeline = %v", built)
	}
	runs := items("/api/v1/tenants/acme/projects/" + projectID + "/runs")
	if len(runs) != 1 {
		t.Fatalf("runs = %v", runs)
	}
	run := runs[0].(map[string]any)
	raw, _ := json.Marshal(run["sources"])
	var sources []gitv1.Source
	if err := json.Unmarshal(raw, &sources); err != nil || len(sources) != 1 || sources[0].RepositoryID != repoID || sources[0].Commit != pushed {
		t.Fatalf("the run is not pinned to the pushed commit %s: %s (%v)", pushed, raw, err)
	}
	if run["pipelineName"] != "build" || run["status"] != "queued" {
		t.Fatalf("run = %v", run)
	}

	// The hosted repository holds exactly that commit.
	refs := get("/api/v1/tenants/acme/projects/demo/repositories/" + repoID + "/refs")
	tips, _ := refs["refs"].([]any)
	if len(tips) != 1 || tips[0].(map[string]any)["commit"] != pushed || refs["empty"] != false {
		t.Fatalf("refs = %v", refs)
	}

	// Publishing again neither indexes twice nor starts a second run.
	if _, err := events.Publisher.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if got := items("/api/v1/tenants/acme/projects/" + projectID + "/runs"); len(got) != 1 {
		t.Fatalf("runs after a second drain = %v", got)
	}
}
