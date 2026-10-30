package api_test

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitservice"
	"github.com/optimiware/kodama/internal/kernel/store"
)

const reposPath = "/api/v1/tenants/acme/projects/p_erp/repositories"

// Test_unit_kernel_repository_create_assigns_project: the project of a
// repository comes from the requested scope, creation is idempotent on the name,
// and a path or a foreign caller never creates or reveals anything.
func Test_unit_kernel_repository_create_assigns_project(t *testing.T) {
	e := newEnv(t)

	t.Run("project scope assigns the project", func(t *testing.T) {
		repo := e.createRepo("tok-eng", "p_erp", "backend", nil)
		if repo["projectId"] != "p_erp" || repo["tenantId"] != string(tenantID) || repo["path"] != "acme/erp/backend" ||
			repo["defaultBranch"] != "main" || repo["status"] != "ready" || repo["empty"] != true {
			t.Fatalf("repository = %v", repo)
		}
		if got := str(repo["cloneUrl"]); got != "https://kodama.example.invalid/git/acme/erp/backend.git" || strings.Contains(got, "@") {
			t.Fatalf("cloneUrl = %q", got)
		}
		if p := repo["policy"].(map[string]any); p["directPushToDefault"] != false {
			t.Fatalf("policy = %v", p)
		}
		stored, err := e.mem.RepositoryByPath(t.Context(), tenantID, "erp", "backend")
		if err != nil || stored.ProjectID != projectID || stored.Status != store.RepoReady {
			t.Fatalf("stored = %+v, %v", stored, err)
		}
		// The project may also be addressed by its key.
		byKey := e.createRepo("tok-admin", "web", "site", nil)
		if byKey["projectId"] != "p_web" {
			t.Fatalf("created under the web project: %v", byKey)
		}
	})

	t.Run("retry is idempotent and a different configuration is a name conflict", func(t *testing.T) {
		first := e.createRepo("tok-eng", "p_erp", "api", map[string]any{"defaultBranch": "trunk"})
		r := e.do(http.MethodPost, reposPath, "tok-eng", map[string]any{"name": "api", "defaultBranch": "trunk"})
		if r.status != http.StatusOK || r.body["id"] != first["id"] {
			t.Fatalf("retry = %d %s", r.status, r.raw)
		}
		r = e.do(http.MethodPost, reposPath, "tok-eng", map[string]any{"name": "api", "defaultBranch": "main"})
		if r.status != http.StatusConflict || errCode(r) != "repository_name_taken" {
			t.Fatalf("conflicting retry = %d %s", r.status, r.raw)
		}
		repos, _ := e.mem.ListRepositories(t.Context(), tenantID, projectID)
		n := 0
		for _, x := range repos {
			if x.Name == "api" {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%d repositories named api", n)
		}
	})

	t.Run("tenant scope has no project and is named context", func(t *testing.T) {
		ctx := e.createRepo("tok-admin", "", "context", nil)
		if ctx["projectId"] != nil || ctx["path"] != "acme/context" {
			t.Fatalf("tenant repository = %v", ctx)
		}
		r := e.do(http.MethodPost, "/api/v1/tenants/acme/repositories", "tok-admin", map[string]any{"name": "notes"})
		if r.status != http.StatusUnprocessableEntity {
			t.Fatalf("tenant-level name = %d %s", r.status, r.raw)
		}
		// A project engineer holds no tenant-wide role.
		if r := e.do(http.MethodPost, "/api/v1/tenants/acme/repositories", "tok-eng", map[string]any{"name": "context"}); r.status == http.StatusCreated {
			t.Fatalf("a project engineer created the tenant repository: %s", r.raw)
		}
	})

	t.Run("an unknown project, a foreign caller and a viewer create nothing", func(t *testing.T) {
		before := len(e.mem.Events())
		for name, c := range map[string]struct {
			token, project string
			want           int
		}{
			"unknown project": {"tok-admin", "p_missing", http.StatusNotFound},
			"foreign tenant":  {"tok-foreign", "p_erp", http.StatusNotFound},
			"no role":         {"tok-outside", "p_erp", http.StatusNotFound},
			"viewer":          {"tok-viewer", "p_erp", http.StatusForbidden},
			"unauthenticated": {"", "p_erp", http.StatusUnauthorized},
		} {
			r := e.do(http.MethodPost, "/api/v1/tenants/acme/projects/"+c.project+"/repositories", c.token, map[string]any{"name": "ghost"})
			if r.status != c.want {
				t.Errorf("%s: %d %s, want %d", name, r.status, r.raw, c.want)
			}
		}
		if _, err := e.mem.RepositoryByPath(t.Context(), tenantID, "erp", "ghost"); err == nil {
			t.Fatal("a refused request created a repository")
		}
		if len(e.mem.Events()) != before {
			t.Fatal("a refused request committed an event")
		}
	})

	t.Run("strict bodies", func(t *testing.T) {
		for name, body := range map[string]any{
			"unknown member":       `{"name":"x1","hosting":"external"}`,
			"bad name":             map[string]any{"name": "bad name"},
			"trailing slash":       map[string]any{"name": "a/"},
			"bad branch":           map[string]any{"name": "x2", "defaultBranch": "../x"},
			"bad pipeline":         map[string]any{"name": "x3", "policy": map[string]any{"integrationPipeline": "Has Spaces"}},
			"unknown policy field": `{"name":"x4","policy":{"forcePush":true}}`,
		} {
			r := e.do(http.MethodPost, reposPath, "tok-eng", body)
			if r.status < 400 || r.status >= 500 {
				t.Errorf("%s: %d %s", name, r.status, r.raw)
			}
		}
	})

	t.Run("provisioning is finished by the worker's confirmation", func(t *testing.T) {
		e.workers.initErr = &gitv1.ContractError{Code: domain.CodeUnavailable, Message: "down"}
		repo := e.createRepo("tok-eng", "p_erp", "slow", nil)
		e.workers.initErr = nil
		if repo["status"] != "provisioning" || repo["reason"] != "provisioning" {
			t.Fatalf("repository = %v", repo)
		}
		if r := e.credential("tok-eng", repo, false); r.status != http.StatusConflict || errCode(r) != "repository_not_ready" {
			t.Fatalf("credential for a provisioning repository = %d %s", r.status, r.raw)
		}
		assign, err := e.control().Assignments(t.Context(), tenantID)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, a := range assign.Items {
			if a.RepositoryID == repo["id"] {
				found = a.Kind == gitv1.AssignInitialize
			}
		}
		if !found {
			t.Fatalf("assignments = %+v", assign)
		}
		for i := 0; i < 2; i++ { // idempotent on the repository ID
			resp, err := e.control().Initialized(t.Context(), tenantID, gitv1.InitializedRequest{RepositoryID: str(repo["id"]), DefaultBranch: "main", Empty: true})
			if err != nil || resp.Status != gitv1.StatusReady {
				t.Fatalf("initialized = %+v, %v", resp, err)
			}
		}
		created := 0
		for _, ev := range e.mem.EventsOfType("repository.created") {
			if ev.ResourceID == repo["id"] {
				created++
			}
		}
		if created != 1 {
			t.Fatalf("repository.created events = %d", created)
		}
		assign, _ = e.control().Assignments(t.Context(), tenantID)
		for _, a := range assign.Items {
			if a.RepositoryID == repo["id"] && (a.Kind != gitv1.AssignReconcile || a.JournalAck == nil) {
				t.Fatalf("a ready repository is reconciled with its acknowledged sequence: %+v", a)
			}
		}
	})

	t.Run("read, patch, archive and restore", func(t *testing.T) {
		repo := e.createRepo("tok-eng", "p_erp", "lifecycle", nil)
		path := reposPath + "/" + str(repo["id"])
		if r := e.do(http.MethodGet, path, "tok-viewer", nil); r.status != http.StatusOK || r.body["id"] != repo["id"] {
			t.Fatalf("viewer read = %d %s", r.status, r.raw)
		}
		if r := e.do(http.MethodGet, "/api/v1/tenants/acme/projects/p_web/repositories/"+str(repo["id"]), "tok-admin", nil); r.status != http.StatusNotFound {
			t.Fatalf("a repository addressed under another project = %d", r.status)
		}
		if r := e.do(http.MethodGet, path, "tok-outside", nil); r.status != http.StatusNotFound {
			t.Fatalf("outsider read = %d", r.status)
		}
		patch := map[string]any{"policy": map[string]any{"directPushToDefault": true, "integrationPipeline": "integrate"}}
		if r := e.do(http.MethodPatch, path, "tok-eng", patch); r.status != http.StatusPreconditionRequired {
			t.Fatalf("patch without If-Match = %d", r.status)
		}
		if r := e.do(http.MethodPatch, path, "tok-viewer", patch, "If-Match", "1"); r.status != http.StatusForbidden {
			t.Fatalf("viewer patch = %d", r.status)
		}
		if r := e.do(http.MethodPatch, path, "tok-eng", map[string]any{"policy": map[string]any{}}, "If-Match", "2"); r.status != http.StatusUnprocessableEntity {
			t.Fatalf("a patched policy must state the flag: %d", r.status)
		}
		r := e.do(http.MethodPatch, path, "tok-eng", patch, "If-Match", strconv.Itoa(int(repo["version"].(float64))))
		if r.status != http.StatusOK || r.body["version"] != float64(3) {
			t.Fatalf("patch = %d %s", r.status, r.raw)
		}
		if p := r.body["policy"].(map[string]any); p["directPushToDefault"] != true || p["integrationPipeline"] != "integrate" {
			t.Fatalf("policy = %v", p)
		}
		if r := e.do(http.MethodPatch, path, "tok-eng", patch, "If-Match", "2"); r.status != http.StatusPreconditionFailed {
			t.Fatalf("stale patch = %d", r.status)
		}
		if r := e.do(http.MethodPost, path+"/archive", "tok-viewer", nil); r.status != http.StatusForbidden {
			t.Fatalf("viewer archive = %d", r.status)
		}
		if r := e.do(http.MethodPost, path+"/archive", "tok-eng", nil); r.status != http.StatusOK || r.body["status"] != "archived" {
			t.Fatalf("archive = %d %s", r.status, r.raw)
		}
		if r := e.do(http.MethodPost, path+"/restore", "tok-admin", nil); r.status != http.StatusOK || r.body["status"] != "ready" {
			t.Fatalf("restore = %d %s", r.status, r.raw)
		}
		if got := len(e.mem.EventsOfType("repository.state_changed")); got < 2 {
			t.Fatalf("repository.state_changed events = %d", got)
		}
		if r := e.do(http.MethodGet, path+"/refs", "tok-viewer", nil); r.status != http.StatusOK || r.body["empty"] != false {
			t.Fatalf("refs = %d %s", r.status, r.raw)
		}
		list := e.do(http.MethodGet, reposPath, "tok-viewer", nil)
		if list.status != http.StatusOK || len(list.body["items"].([]any)) < 4 {
			t.Fatalf("list = %d %s", list.status, list.raw)
		}
	})
}

// Test_unit_kernel_git_route_resolves_path: /git/ resolves <tenant>/context to
// the tenant-level repository and <tenant>/<project>/<name> to a project's, never
// creates anything, and answers the same 404 for unknown, foreign and
// unauthorized paths.
func Test_unit_kernel_git_route_resolves_path(t *testing.T) {
	e := newEnv(t)
	backend := e.createRepo("tok-eng", "p_erp", "backend", nil)
	nested := e.createRepo("tok-eng", "p_erp", "libs/core", nil)
	tenantRepo := e.createRepo("tok-admin", "", "context", nil)
	_, backendPW := e.mustCredential("tok-eng", backend, false)
	_, nestedPW := e.mustCredential("tok-eng", nested, false)
	_, tenantPW := e.mustCredential("tok-admin", tenantRepo, false)

	refs := "/info/refs?service=git-upload-pack"
	for name, c := range map[string]struct {
		path, password string
		want           map[string]any
	}{
		"project repository": {"/git/acme/erp/backend.git" + refs, backendPW, backend},
		"nested name":        {"/git/acme/erp/libs/core.git" + refs, nestedPW, nested},
		"tenant context":     {"/git/acme/context.git" + refs, tenantPW, tenantRepo},
	} {
		before := len(e.workers.requests())
		r := e.gitGet(c.path, c.password, http.MethodGet)
		if r.status != http.StatusOK || r.raw != "worker" {
			t.Fatalf("%s: %d %s", name, r.status, r.raw)
		}
		reqs := e.workers.requests()
		if len(reqs) != before+1 {
			t.Fatalf("%s: the worker was not called", name)
		}
		got := reqs[len(reqs)-1]
		if got.URL.Path != "/"+str(c.want["id"])+".git/info/refs" || got.URL.Query().Get("service") != "git-upload-pack" {
			t.Fatalf("%s: forwarded %s?%s, want the repository ID path", name, got.URL.Path, got.URL.RawQuery)
		}
	}

	repoCount := func() int {
		a, _ := e.mem.ListRepositories(t.Context(), tenantID, projectID)
		b, _ := e.mem.ListRepositories(t.Context(), tenantID, "")
		return len(a) + len(b)
	}
	before, forwarded := repoCount(), len(e.workers.requests())
	notFound := map[string]reply{}
	for name, path := range map[string]string{
		"unknown repository":                   "/git/acme/erp/ghost.git" + refs,
		"unknown project":                      "/git/acme/nope/backend.git" + refs,
		"tenant-level not context":             "/git/acme/notes.git" + refs,
		"another repository":                   "/git/acme/erp/libs/core.git" + refs, // backend's credential, nested's path
		"tenant context by project credential": "/git/acme/context.git" + refs,
	} {
		r := e.gitGet(path, backendPW, http.MethodGet)
		if r.status != http.StatusNotFound || errCode(r) != "not_found" {
			t.Errorf("%s: %d %s", name, r.status, r.raw)
		}
		notFound[name] = r
	}
	for name, r := range notFound {
		if r.raw != notFound["unknown repository"].raw {
			t.Errorf("%s is distinguishable from an unknown repository: %q", name, r.raw)
		}
	}
	// A foreign tenant's path with a credential of this tenant is a bad credential.
	if r := e.gitGet("/git/other/erp/backend.git"+refs, backendPW, http.MethodGet); r.status != http.StatusUnauthorized {
		t.Errorf("foreign tenant path = %d", r.status)
	}
	if repoCount() != before || len(e.workers.requests()) != forwarded {
		t.Fatal("an unknown path created a repository or reached a worker")
	}

	t.Run("credential required", func(t *testing.T) {
		for name, pw := range map[string]string{"none": "", "unknown": "gitc_00000000000000000000000000000000"} {
			r := e.gitGet("/git/acme/erp/backend.git"+refs, pw, http.MethodGet)
			if r.status != http.StatusUnauthorized || !strings.HasPrefix(r.header.Get("WWW-Authenticate"), "Basic") {
				t.Errorf("%s: %d challenge %q", name, r.status, r.header.Get("WWW-Authenticate"))
			}
		}
		// A bearer token carries the credential as well.
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/git/acme/erp/backend.git"+refs, nil)
		req.Header.Set("Authorization", "Bearer "+backendPW)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("bearer credential: %v %v", resp, err)
		}
		resp.Body.Close()
	})

	t.Run("only the smart endpoints", func(t *testing.T) {
		for name, c := range map[string]struct{ method, path string }{
			"dumb HTTP HEAD":     {http.MethodGet, "/git/acme/erp/backend.git/HEAD"},
			"dumb objects":       {http.MethodGet, "/git/acme/erp/backend.git/objects/info/packs"},
			"upload-archive":     {http.MethodPost, "/git/acme/erp/backend.git/git-upload-archive"},
			"service archive":    {http.MethodGet, "/git/acme/erp/backend.git/info/refs?service=git-upload-archive"},
			"wrong method":       {http.MethodGet, "/git/acme/erp/backend.git/git-upload-pack"},
			"extra query":        {http.MethodGet, "/git/acme/erp/backend.git/info/refs?service=git-upload-pack&x=1"},
			"no .git suffix":     {http.MethodGet, "/git/acme/erp/backend/info/refs?service=git-upload-pack"},
			"single segment":     {http.MethodGet, "/git/acme.git/info/refs?service=git-upload-pack"},
			"empty segment":      {http.MethodGet, "/git/acme//backend.git/info/refs?service=git-upload-pack"},
			"server ref listing": {http.MethodGet, "/git/acme/erp/backend.git/refs/kodama/runs"},
		} {
			if r := e.gitGet(c.path, backendPW, c.method); r.status != http.StatusNotFound {
				t.Errorf("%s: %d", name, r.status)
			}
		}
	})

	t.Run("worker unavailable is retryable", func(t *testing.T) {
		e.workers.handlerErr = &gitv1.ContractError{Code: domain.CodeUnavailable, Message: "down"}
		defer func() { e.workers.handlerErr = nil }()
		r := e.gitGet("/git/acme/erp/backend.git"+refs, backendPW, http.MethodGet)
		if r.status != http.StatusServiceUnavailable || r.header.Get("Retry-After") == "" {
			t.Fatalf("worker down = %d", r.status)
		}
	})
}

// Test_unit_kernel_git_forwarding_assertion: the forwarded request carries a
// short-lived signed assertion for the tenant's worker and never the caller's
// credential; receivePack is true only for a credential that may update a ref.
func Test_unit_kernel_git_forwarding_assertion(t *testing.T) {
	e := newEnv(t)
	repo := e.createRepo("tok-eng", "p_erp", "backend", nil)
	grant, password := e.mustCredential("tok-eng", repo, false)
	viewerGrant, viewerPW := e.mustCredential("tok-viewer", repo, false)

	verify := func(r *http.Request, tenant domain.TenantID) (gitv1.ForwardingAssertion, error) {
		return e.verifier.VerifyForwarding(t.Context(), r.Header.Get(gitservice.AssertionHeader), tenant, e.clock())
	}
	last := func() *http.Request {
		reqs := e.workers.requests()
		return reqs[len(reqs)-1]
	}
	uploadRefs := "/git/acme/erp/backend.git/info/refs?service=git-upload-pack"
	receiveRefs := "/git/acme/erp/backend.git/info/refs?service=git-receive-pack"

	if r := e.gitGet(receiveRefs, password, http.MethodGet); r.status != http.StatusOK {
		t.Fatalf("engineer receive = %d %s", r.status, r.raw)
	}
	fwd := last()
	a, err := verify(fwd, tenantID)
	if err != nil {
		t.Fatalf("the assertion does not verify for the tenant's worker: %v", err)
	}
	issued, _ := a.IssuedAt.Time()
	expires, _ := a.ExpiresAt.Time()
	if a.Audience != "kodama-git-worker:t_acme" || a.TenantID != "t_acme" || a.RepositoryID != repo["id"] || a.GrantID != grant ||
		!a.ReceivePack || a.RequestID == "" || !issued.Equal(e.clock()) || expires.Sub(issued) > 5*time.Minute || !expires.After(issued) {
		t.Fatalf("assertion = %+v", a)
	}
	if fwd.Header.Get("Authorization") != "" || strings.Contains(fwd.Header.Get(gitservice.AssertionHeader), password) {
		t.Fatal("the caller's credential reached the worker")
	}
	if fwd.URL.Path != "/"+str(repo["id"])+".git/info/refs" {
		t.Fatalf("forwarded path %q", fwd.URL.Path)
	}
	if _, err := verify(fwd, otherID); err == nil {
		t.Fatal("the assertion verified for another tenant's worker")
	}
	e.advance(5 * time.Minute)
	if _, err := verify(fwd, tenantID); err == nil {
		t.Fatal("an assertion outlived five minutes")
	}
	e.advance(-5 * time.Minute)

	// Fetch with the same credential: the assertion is still receivePack only because the credential may update.
	e.gitGet(uploadRefs, password, http.MethodGet)
	if a, err := verify(last(), tenantID); err != nil || !a.ReceivePack {
		t.Fatalf("fetch assertion = %+v, %v", a, err)
	}
	if a.CheckService("git-receive-pack") != nil {
		t.Fatal("receive-pack was not permitted by the assertion")
	}

	// A read-only person: fetch is forwarded with receivePack false; a push never reaches the worker.
	n := len(e.workers.requests())
	if r := e.gitGet(uploadRefs, viewerPW, http.MethodGet); r.status != http.StatusOK {
		t.Fatalf("viewer fetch = %d %s", r.status, r.raw)
	}
	va, err := verify(last(), tenantID)
	if err != nil || va.ReceivePack || va.GrantID != viewerGrant || va.CheckService("git-receive-pack") == nil {
		t.Fatalf("viewer assertion = %+v, %v", va, err)
	}
	if r := e.gitGet(receiveRefs, viewerPW, http.MethodGet); r.status != http.StatusNotFound {
		t.Fatalf("viewer receive = %d", r.status)
	}
	if len(e.workers.requests()) != n+1 {
		t.Fatal("a push of a read-only credential reached the worker")
	}

	// Every request gets its own assertion.
	e.gitGet(uploadRefs, password, http.MethodGet)
	b, _ := verify(last(), tenantID)
	if b.RequestID == a.RequestID {
		t.Fatal("two requests share a request ID")
	}
}

// Test_unit_kernel_git_credential_current_authority: authority is current, not
// recorded. A removed role, a cancelled run, a revoked or expired credential and
// an archived repository stop working on the next request.
func Test_unit_kernel_git_credential_current_authority(t *testing.T) {
	e := newEnv(t)
	repo := e.createRepo("tok-eng", "p_erp", "backend", map[string]any{"policy": map[string]any{"directPushToDefault": true}})
	repoID := str(repo["id"])
	branch := create("refs/heads/feature", c1)
	refs := "/git/acme/erp/backend.git/info/refs?service=git-receive-pack"

	t.Run("a removed role stops a push with an unexpired credential", func(t *testing.T) {
		grant, pw := e.mustCredential("tok-eng", repo, false)
		if r := e.gitGet(refs, pw, http.MethodGet); r.status != http.StatusOK {
			t.Fatalf("before = %d", r.status)
		}
		resp, err := e.authorize(grant, repoID, []gitv1.RefUpdate{branch}, factsFor(false, branch))
		if err != nil || resp.Updates[0].Verdict != gitv1.VerdictAllow {
			t.Fatalf("engineer push = %+v, %v", resp, err)
		}
		e.mem.Revoke(tenantID, "u_eng", string(projectID), "engineer")
		e.mem.Grant(tenantID, "u_eng", string(projectID), "viewer")
		if r := e.gitGet(refs, pw, http.MethodGet); r.status != http.StatusNotFound {
			t.Fatalf("receive after the role was removed = %d", r.status)
		}
		if r := e.gitGet("/git/acme/erp/backend.git/info/refs?service=git-upload-pack", pw, http.MethodGet); r.status != http.StatusOK {
			t.Fatalf("a viewer still reads: %d", r.status)
		}
		resp, err = e.authorize(grant, repoID, []gitv1.RefUpdate{branch}, factsFor(false, branch))
		if err != nil || strings.Join(reasons(resp), ",") != gitv1.ReasonGrantScope {
			t.Fatalf("pre-receive after the role was removed = %v, %v", reasons(resp), err)
		}
		e.mem.Revoke(tenantID, "u_eng", string(projectID), "viewer")
		if r := e.gitGet("/git/acme/erp/backend.git/info/refs?service=git-upload-pack", pw, http.MethodGet); r.status != http.StatusNotFound {
			t.Fatalf("a person without any role reads: %d", r.status)
		}
		e.mem.Grant(tenantID, "u_eng", string(projectID), "engineer")
	})

	t.Run("expired and foreign credentials", func(t *testing.T) {
		_, pw := e.mustCredential("tok-eng", repo, false)
		if r := e.gitGet(refs, pw, http.MethodGet); r.status != http.StatusOK {
			t.Fatal("before expiry")
		}
		grant, _ := e.mustCredential("tok-eng", repo, false)
		e.advance(16 * time.Minute)
		defer e.advance(-16 * time.Minute)
		if r := e.gitGet(refs, pw, http.MethodGet); r.status != http.StatusUnauthorized {
			t.Fatalf("expired = %d", r.status)
		}
		if _, err := e.authorize(grant, repoID, []gitv1.RefUpdate{branch}, factsFor(false, branch)); contractCode(err) != domain.CodeInvalidCredential {
			t.Fatalf("pre-receive with an expired credential: %v", err)
		}
	})

	t.Run("a cancelled or finished run loses its credential", func(t *testing.T) {
		e.mem.SetRun(store.RunGitInfo{TenantID: tenantID, ProjectID: projectID, RunID: "run_1", PipelineName: "build", Status: "running",
			Sources: []byte(`[{"repositoryId":"` + repoID + `","commit":"` + c1 + `","path":".","access":"write","branch":"refs/heads/kodama/work/tsk_1"}]`), TaskID: "tsk_1"})
		issued, err := e.git.IssueRunCredential(t.Context(), tenantID, projectID, "run_1", repoID)
		if err != nil {
			t.Fatal(err)
		}
		runBranch := create("refs/heads/kodama/runs/run_1/work", c1)
		fetch := "/git/acme/erp/backend.git/info/refs?service=git-upload-pack"
		if r := e.gitGet(fetch, issued.Secret, http.MethodGet); r.status != http.StatusOK {
			t.Fatalf("run fetch = %d", r.status)
		}
		resp, err := e.authorize(issued.GrantID, repoID, []gitv1.RefUpdate{runBranch}, factsFor(false, runBranch))
		if err != nil || resp.Updates[0].Verdict != gitv1.VerdictAllow {
			t.Fatalf("run push = %+v, %v", resp, err)
		}
		e.mem.SetRun(store.RunGitInfo{TenantID: tenantID, ProjectID: projectID, RunID: "run_1", PipelineName: "build", Status: "running", Cancelling: true, TaskID: "tsk_1"})
		if r := e.gitGet(fetch, issued.Secret, http.MethodGet); r.status != http.StatusNotFound {
			t.Fatalf("fetch by a cancelled run = %d", r.status)
		}
		resp, err = e.authorize(issued.GrantID, repoID, []gitv1.RefUpdate{runBranch}, factsFor(false, runBranch))
		if err != nil || strings.Join(reasons(resp), ",") != gitv1.ReasonRunInactive {
			t.Fatalf("pre-receive of a cancelled run = %v, %v", reasons(resp), err)
		}
		if _, err := e.control().CommitIntent(t.Context(), tenantID, gitv1.NewReceiveID(issued.GrantID), gitv1.CommitIntentRequest{}); err == nil {
			t.Fatal("an invalid commit intent was granted")
		}
		receive := gitv1.NewReceiveID(issued.GrantID)
		_, err = e.control().CommitIntent(t.Context(), tenantID, receive, gitv1.CommitIntentRequest{ReceiveID: receive, Updates: []gitv1.RefUpdate{runBranch}})
		if contractCode(err) != gitv1.CodeRunInactive {
			t.Fatalf("commit intent of a cancelled run: %v", err)
		}
		// Running again (a restored fixture) works; the run ending revokes its credential.
		e.mem.SetRun(store.RunGitInfo{TenantID: tenantID, ProjectID: projectID, RunID: "run_1", PipelineName: "build", Status: "running", TaskID: "tsk_1"})
		if _, err := e.control().CommitIntent(t.Context(), tenantID, receive, gitv1.CommitIntentRequest{ReceiveID: receive, Updates: []gitv1.RefUpdate{runBranch}}); err != nil {
			t.Fatalf("commit intent of a live run: %v", err)
		}
		_ = e.mem.RevokeRunCredentials(t.Context(), tenantID, projectID, "run_1")
		if r := e.gitGet(fetch, issued.Secret, http.MethodGet); r.status != http.StatusUnauthorized {
			t.Fatalf("fetch with a revoked credential = %d", r.status)
		}
		if _, err := e.control().CommitIntent(t.Context(), tenantID, receive, gitv1.CommitIntentRequest{ReceiveID: receive, Updates: []gitv1.RefUpdate{runBranch}}); contractCode(err) != gitv1.CodeRunInactive {
			t.Fatalf("commit intent with a revoked credential: %v", err)
		}
		e.mem.SetRun(store.RunGitInfo{TenantID: tenantID, ProjectID: projectID, RunID: "run_2", PipelineName: "build", Status: "succeeded", Sources: []byte(`[]`)})
		if _, err := e.git.IssueRunCredential(t.Context(), tenantID, projectID, "run_2", repoID); err == nil {
			t.Fatal("a credential was issued to a finished run")
		}
	})

	t.Run("an archived repository refuses pushes but not history", func(t *testing.T) {
		grant, pw := e.mustCredential("tok-eng", repo, false)
		if r := e.do(http.MethodPost, reposPath+"/"+repoID+"/archive", "tok-eng", nil); r.status != http.StatusOK {
			t.Fatalf("archive = %d", r.status)
		}
		resp, err := e.authorize(grant, repoID, []gitv1.RefUpdate{branch}, factsFor(false, branch))
		if err != nil || strings.Join(reasons(resp), ",") != gitv1.ReasonRepositoryArchived {
			t.Fatalf("push to an archived repository = %v, %v", reasons(resp), err)
		}
		receive := gitv1.NewReceiveID(grant)
		if _, err := e.control().CommitIntent(t.Context(), tenantID, receive, gitv1.CommitIntentRequest{ReceiveID: receive, Updates: []gitv1.RefUpdate{branch}}); contractCode(err) != gitv1.CodeRepositoryArchived {
			t.Fatalf("commit intent on an archived repository: %v", err)
		}
		if r := e.gitGet("/git/acme/erp/backend.git/info/refs?service=git-upload-pack", pw, http.MethodGet); r.status != http.StatusOK {
			t.Fatalf("history of an archived repository = %d", r.status)
		}
	})
}

// Test_unit_kernel_git_seed_single_use: a seed grant authorizes one receive that
// creates the default branch of an empty repository, exactly once.
func Test_unit_kernel_git_seed_single_use(t *testing.T) {
	e := newEnv(t)
	repo := e.createRepo("tok-eng", "p_erp", "backend", nil)
	repoID := str(repo["id"])
	main := create("refs/heads/main", c1)
	other := create("refs/heads/other", c2)

	if r := e.credential("tok-viewer", repo, true); r.status != http.StatusForbidden {
		t.Fatalf("a viewer requested a seed: %d %s", r.status, r.raw)
	}
	if r := e.credential("tok-outside", repo, true); r.status != http.StatusNotFound {
		t.Fatalf("an outsider requested a seed: %d", r.status)
	}
	if r := e.credential("tok-admin", repo, true); r.status != http.StatusNotFound && r.status != http.StatusForbidden {
		t.Fatalf("an administrator without the engineer role requested a seed: %d", r.status)
	}
	grant, _ := e.mustCredential("tok-eng", repo, true)
	stored, _ := e.mem.GetCredential(t.Context(), tenantID, grant)
	if stored.Kind != store.CredSeed || stored.ExpiresAt.Sub(stored.IssuedAt) > 15*time.Minute || strings.Contains(string(stored.Claims), "gitc_") {
		t.Fatalf("seed = %+v", stored)
	}

	t.Run("anything but the one default-branch create refuses the whole receive", func(t *testing.T) {
		for name, c := range map[string]struct {
			updates []gitv1.RefUpdate
			empty   bool
		}{
			"extra ref":      {[]gitv1.RefUpdate{main, other}, true},
			"another branch": {[]gitv1.RefUpdate{other}, true},
			"a tag":          {[]gitv1.RefUpdate{create("refs/tags/v1", c1)}, true},
			"non-empty":      {[]gitv1.RefUpdate{main}, false},
			"not a create":   {[]gitv1.RefUpdate{{Ref: "refs/heads/main", Old: c1, New: c2}}, true},
			"a delete":       {[]gitv1.RefUpdate{{Ref: "refs/heads/main", Old: c1, New: z0}}, true},
			"a server ref":   {[]gitv1.RefUpdate{create("refs/kodama/runs/x/1", c1)}, true},
		} {
			facts := factsFor(c.empty, c.updates...)
			if name == "not a create" || name == "a delete" {
				facts = factsFor(false, c.updates...)
			}
			resp, err := e.authorize(grant, repoID, c.updates, facts)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			for _, r := range reasons(resp) {
				if r != gitv1.ReasonSeedOnly {
					t.Errorf("%s: reasons %v, want seed_only for every update", name, reasons(resp))
				}
			}
		}
		if after, _ := e.mem.GetCredential(t.Context(), tenantID, grant); after.ConsumedAt != nil {
			t.Fatal("a refused receive consumed the seed")
		}
	})

	t.Run("the first valid receive is authorized once", func(t *testing.T) {
		receive := gitv1.NewReceiveID(grant)
		resp, err := e.authorizeAs(receive, grant, repoID, []gitv1.RefUpdate{main}, factsFor(true, main))
		if err != nil || strings.Join(reasons(resp), ",") != "allow" {
			t.Fatalf("seed receive = %v, %v", reasons(resp), err)
		}
		// The same receive repeats idempotently; any other receive is consumed.
		if resp, err = e.authorizeAs(receive, grant, repoID, []gitv1.RefUpdate{main}, factsFor(true, main)); err != nil || reasons(resp)[0] != "allow" {
			t.Fatalf("repeat of the same receive = %v, %v", reasons(resp), err)
		}
		resp, err = e.authorize(grant, repoID, []gitv1.RefUpdate{main}, factsFor(true, main))
		if err != nil || strings.Join(reasons(resp), ",") != gitv1.ReasonSeedConsumed {
			t.Fatalf("second receive with the same grant = %v, %v", reasons(resp), err)
		}
		// Until its outcome is recorded, no new grant (seed_pending); a concurrent seed grant authorizes nothing.
		if r := e.credential("tok-eng", repo, true); r.status != http.StatusConflict || errCode(r) != "seed_pending" {
			t.Fatalf("new seed while one is unrecorded = %d %s", r.status, r.raw)
		}
		// The outcome is recorded: the repository has a ref, so it cannot be seeded again.
		if _, err := e.control().Outcome(t.Context(), tenantID, receive, gitv1.OutcomeRequest{ReceiveID: receive, Applied: []gitv1.RefUpdate{main}}); err != nil {
			t.Fatal(err)
		}
		if r := e.credential("tok-eng", repo, true); r.status != http.StatusConflict || errCode(r) != "repository_not_empty" {
			t.Fatalf("seed of a repository with a ref = %d %s", r.status, r.raw)
		}
		if resp, err = e.authorize(grant, repoID, []gitv1.RefUpdate{main}, factsFor(true, main)); err != nil || reasons(resp)[0] != gitv1.ReasonSeedConsumed {
			t.Fatalf("consumed grant after the outcome = %v, %v", reasons(resp), err)
		}
	})

	t.Run("a refused seed leaves the repository empty and a new grant possible", func(t *testing.T) {
		fresh := e.createRepo("tok-eng", "p_erp", "second", nil)
		freshID := str(fresh["id"])
		g1, _ := e.mustCredential("tok-eng", fresh, true)
		receive := gitv1.NewReceiveID(g1)
		if resp, err := e.authorizeAs(receive, g1, freshID, []gitv1.RefUpdate{main}, factsFor(true, main)); err != nil || reasons(resp)[0] != "allow" {
			t.Fatalf("seed = %v, %v", reasons(resp), err)
		}
		// Nothing was applied (Git rejected the receive): the outcome is empty.
		if _, err := e.control().Outcome(t.Context(), tenantID, receive, gitv1.OutcomeRequest{ReceiveID: receive, Applied: []gitv1.RefUpdate{}}); err != nil {
			t.Fatal(err)
		}
		g2, _ := e.mustCredential("tok-eng", fresh, true)
		if g2 == g1 {
			t.Fatal("the same grant was issued twice")
		}
		// A grant replaced by a newer request authorizes nothing.
		g3, _ := e.mustCredential("tok-eng", fresh, true)
		if _, err := e.authorize(g2, freshID, []gitv1.RefUpdate{main}, factsFor(true, main)); contractCode(err) != domain.CodeInvalidCredential {
			t.Fatalf("a replaced (revoked) grant authorized a receive: %v", err)
		}
		// Two seed grants of one repository authorize at most one receive.
		resp, err := e.authorize(g3, freshID, []gitv1.RefUpdate{main}, factsFor(true, main))
		if err != nil || reasons(resp)[0] != "allow" {
			t.Fatalf("the current grant = %v, %v", reasons(resp), err)
		}
	})

	t.Run("a seed holder who is no longer an engineer is refused", func(t *testing.T) {
		fresh := e.createRepo("tok-eng", "p_erp", "third", nil)
		g, _ := e.mustCredential("tok-eng", fresh, true)
		e.mem.Revoke(tenantID, "u_eng", string(projectID), "engineer")
		defer e.mem.Grant(tenantID, "u_eng", string(projectID), "engineer")
		resp, err := e.authorize(g, str(fresh["id"]), []gitv1.RefUpdate{main}, factsFor(true, main))
		if err != nil || reasons(resp)[0] != gitv1.ReasonGrantScope {
			t.Fatalf("seed after the role was removed = %v, %v", reasons(resp), err)
		}
	})
}
