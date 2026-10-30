package repositoryhttp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/repository"
	repositoryhttp "github.com/optimiware/kodama/internal/repository/http"
	rt "github.com/optimiware/kodama/internal/repository/repositorytest"
)

const (
	tenantA, projectA = "t127_acme", "p127_inventory"
	tenantB, projectB = "t127_globex", "p127_inventory"
	projectA2         = "p127_other"
)

// tokens maps bearer tokens to the fixture actors; the router authenticates,
// never the request body.
var tokens = map[string]string{
	"tok-eng": rt.Engineer.ID, "tok-viewer": rt.Viewer.ID, "tok-admin": rt.Admin.ID, "tok-out": rt.Outsider.ID, "tok-sys": rt.System.ID,
}

type authenticator map[string]string

func (a authenticator) Authenticate(_ context.Context, r *http.Request) (httpapi.Principal, error) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	id, ok := a[tok]
	if !ok {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	return httpapi.Principal{ID: id}, nil
}

// routeAuthorizer records the actions it is asked about and allows all of them:
// the use cases then enforce the fine-grained rules with the current roles.
type routeAuthorizer struct {
	mu    sync.Mutex
	calls []httpapi.Action
}

func (r *routeAuthorizer) Authorize(_ context.Context, _ httpapi.Principal, a httpapi.Action, _ httpapi.Target) error {
	r.mu.Lock()
	r.calls = append(r.calls, a)
	r.mu.Unlock()
	return nil
}

// memCreator is the keyed create over the memory store: the first request of a
// key stores its response, a repeat replays it, a different body conflicts.
type memCreator struct {
	store repository.Store
	mu    sync.Mutex
	seen  map[string]stored
}

type stored struct {
	hash httpapi.RequestHash
	resp httpapi.StoredResponse
}

func (m *memCreator) CreateKeyed(ctx context.Context, id httpapi.RequestIdentity, authorizeReplay func(context.Context, httpapi.Reference) error,
	plan *repository.CreatePlan, render func(*repository.Repository, bool) (httpapi.Outcome, error)) (httpapi.StoredResponse, bool, *repository.Repository, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := string(id.Principal.ID) + "|" + id.Route + "|" + string(id.Key)
	if s, ok := m.seen[k]; ok {
		if s.hash != id.Hash {
			return httpapi.StoredResponse{}, false, nil, httpapi.IdempotencyConflictError()
		}
		return s.resp, true, nil, nil
	}
	persisted, created, err := m.store.Insert(ctx, plan.Repository, plan.Effects)
	if err != nil {
		return httpapi.StoredResponse{}, false, nil, err
	}
	r, created, err := plan.Resolve(persisted, created)
	if err != nil {
		return httpapi.StoredResponse{}, false, nil, err
	}
	out, err := render(r, created)
	if err != nil {
		return httpapi.StoredResponse{}, false, nil, err
	}
	m.seen[k] = stored{hash: id.Hash, resp: out.Response}
	return out.Response, false, r, nil
}

type api struct {
	t      testing.TB
	router *httpapi.Router
	authz  *routeAuthorizer
	kit    *rt.Kit
	env    *rt.MemEnv
}

func newAPI(t testing.TB) *api {
	t.Helper()
	env := rt.NewMemEnv(t)
	kit := rt.NewKit(t, env)
	a := &api{t: t, authz: &routeAuthorizer{}, kit: kit, env: env}
	router, err := httpapi.NewRouter(httpapi.RouterConfig{Authenticator: authenticator(tokens), Authorizer: a.authz, RateLimiter: httpapi.Unlimited(), Prefix: "/api/v1"})
	if err != nil {
		t.Fatal(err)
	}
	codec, err := httpapi.NewCursorCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	h, err := repositoryhttp.NewHandler(repositoryhttp.Config{Service: kit.Svc, Creator: &memCreator{store: env.Store(), seen: map[string]stored{}}, Cursors: codec})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.RegisterRoutes(router); err != nil {
		t.Fatal(err)
	}
	a.router = router
	return a
}

type resp struct{ *httptest.ResponseRecorder }

func (a *api) do(token, method, path, body string, headers ...string) resp {
	a.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "/api/v1"+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	a.router.ServeHTTP(rec, req)
	return resp{rec}
}

func (r resp) repo(t testing.TB) gitv1.Repository {
	t.Helper()
	got, err := gitv1.DecodeResponse[gitv1.Repository](r.Body.Bytes())
	if err != nil {
		t.Fatalf("not a repository: %v: %s", err, r.Body.String())
	}
	return got
}

func (r resp) code(t testing.TB) string {
	t.Helper()
	var e httpapi.ErrorEnvelope
	if err := json.Unmarshal(r.Body.Bytes(), &e); err != nil {
		t.Fatalf("not an error envelope: %v: %s", err, r.Body.String())
	}
	if e.Error.RequestId == "" || r.Header().Get("X-Request-ID") != e.Error.RequestId {
		t.Fatalf("an error without a matching request id: %s", r.Body.String())
	}
	return e.Error.Code
}

func (r resp) wantStatus(t testing.TB, status int) {
	t.Helper()
	if r.Code != status {
		t.Fatalf("status %d, want %d: %s", r.Code, status, r.Body.String())
	}
}

func base(tenant, project string) string {
	return "/tenants/" + tenant + "/projects/" + project + "/repositories"
}

func etag(v int64) string { return `"` + itoa(v) + `"` }

func itoa(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func idem(key string) []string { return []string{"Idempotency-Key", key} }

func hdr(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func TestRoutesAreExactlyTheRepositoryResource(t *testing.T) {
	a := newAPI(t)
	for _, c := range []struct{ method, path string }{
		{http.MethodDelete, base(tenantA, projectA) + "/repo_x"},
		{http.MethodGet, base(tenantA, projectA) + "/repo_x/pushes"},
		{http.MethodPost, base(tenantA, projectA) + "/repo_x/seed"},
		{http.MethodPut, base(tenantA, projectA) + "/repo_x"},
	} {
		if r := a.do("tok-eng", c.method, c.path, ""); r.Code != http.StatusNotFound && r.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s answered %d", c.method, c.path, r.Code)
		}
	}
	if r := a.do("", http.MethodGet, base(tenantA, projectA), ""); r.Code != http.StatusUnauthorized {
		t.Errorf("an unauthenticated list answered %d", r.Code)
	}
	if _, err := repositoryhttp.NewHandler(repositoryhttp.Config{}); err == nil {
		t.Error("a handler without collaborators was built")
	}
}

func TestCreateReadAndIdempotency(t *testing.T) {
	a := newAPI(t)
	path := base(tenantA, projectA)

	created := a.do("tok-eng", http.MethodPost, path, `{"name":"backend"}`, idem("k1")...)
	created.wantStatus(t, http.StatusCreated)
	r := created.repo(t)
	if r.Status != gitv1.StatusProvisioning || r.Version != 1 || r.Policy.DirectPushToDefault || r.DefaultBranch != "main" || r.Hosting != gitv1.HostingInternal {
		t.Fatalf("reservation: %+v", r)
	}
	if r.Reason == nil || *r.Reason != repository.ReasonProvisioningPending {
		t.Fatalf("reservation reason: %v", r.Reason)
	}
	if created.Header().Get("ETag") != etag(1) || !strings.HasSuffix(created.Header().Get("Location"), path+"/"+r.ID) {
		t.Fatalf("headers: %v", created.Header())
	}
	if r.CloneURL != "https://git.example.invalid/git/t127_acme/p127_inventory/backend.git" {
		t.Fatalf("clone URL %s", r.CloneURL)
	}

	// The worker was called after the reservation committed: the repository is
	// ready now, with the version the completion produced.
	got := a.do("tok-viewer", http.MethodGet, path+"/"+r.ID, "")
	got.wantStatus(t, http.StatusOK)
	ready := got.repo(t)
	if ready.Status != gitv1.StatusReady || ready.Reason != nil || ready.Version != 2 || !ready.Empty || got.Header().Get("ETag") != etag(2) {
		t.Fatalf("after provisioning: %+v %v", ready, got.Header())
	}

	t.Run("a repeated key replays the stored response", func(t *testing.T) {
		again := a.do("tok-eng", http.MethodPost, path, `{"name":"backend"}`, idem("k1")...)
		again.wantStatus(t, http.StatusCreated)
		if again.Header().Get(httpapi.IdempotencyReplayedHeader) != "true" || again.Body.String() != created.Body.String() {
			t.Fatalf("replay: %v %s", again.Header(), again.Body.String())
		}
		changed := a.do("tok-eng", http.MethodPost, path, `{"name":"backend","defaultBranch":"trunk"}`, idem("k1")...)
		changed.wantStatus(t, http.StatusConflict)
		if c := changed.code(t); c != "idempotency_conflict" {
			t.Fatalf("code %s", c)
		}
	})

	t.Run("another key for the same name returns the existing repository", func(t *testing.T) {
		again := a.do("tok-eng", http.MethodPost, path, `{"name":"backend","policy":{"directPushToDefault":false}}`, idem("k2")...)
		again.wantStatus(t, http.StatusOK)
		if again.repo(t).ID != r.ID || again.Header().Get("Location") != "" {
			t.Fatalf("existing: %s %v", again.Body.String(), again.Header())
		}
		other := a.do("tok-eng", http.MethodPost, path, `{"name":"backend","defaultBranch":"trunk"}`, idem("k3")...)
		other.wantStatus(t, http.StatusConflict)
		if c := other.code(t); c != "repository_name_taken" {
			t.Fatalf("code %s", c)
		}
	})

	t.Run("bodies are strict and the key is required", func(t *testing.T) {
		for name, body := range map[string]string{
			"an unknown member":    `{"name":"x1","extra":1}`,
			"a null branch":        `{"name":"x1","defaultBranch":null}`,
			"a null flag":          `{"name":"x1","policy":{"directPushToDefault":null}}`,
			"a missing name":       `{}`,
			"an upper-case name":   `{"name":"Backend"}`,
			"a reserved branch":    `{"name":"x1","defaultBranch":"kodama/work"}`,
			"a duplicate member":   `{"name":"x1","name":"x2"}`,
			"not an object":        `[]`,
			"a differently cased":  `{"Name":"x1"}`,
			"an invalid directive": `{"name":"x1","policy":{"directPushToDefault":"yes"}}`,
		} {
			r := a.do("tok-eng", http.MethodPost, path, body, idem("bad-"+strings.ReplaceAll(name, " ", "-"))...)
			if r.Code != http.StatusBadRequest || r.code(t) != "invalid_request" {
				t.Errorf("%s: %d %s", name, r.Code, r.Body.String())
			}
		}
		if r := a.do("tok-eng", http.MethodPost, path, `{"name":"nokey"}`); r.Code != http.StatusBadRequest {
			t.Errorf("no key: %d", r.Code)
		}
		if _, err := a.kit.Svc.Get(a.kit.Ctx, rt.Engineer, rt.ScopeA, "repo_t127_9999"); err == nil {
			t.Error("a refused body created a repository")
		}
	})

	t.Run("roles decide", func(t *testing.T) {
		if r := a.do("tok-viewer", http.MethodPost, path, `{"name":"viewer-made"}`, idem("v1")...); r.Code != http.StatusForbidden || r.code(t) != "forbidden" {
			t.Errorf("viewer create: %d %s", r.Code, r.Body.String())
		}
		if r := a.do("tok-out", http.MethodGet, path+"/"+r.ID, ""); r.Code != http.StatusForbidden {
			t.Errorf("outsider read: %d", r.Code)
		}
		if r := a.do("tok-eng", http.MethodPost, base(tenantA, "p127_unknown"), `{"name":"x"}`, idem("u1")...); r.Code != http.StatusForbidden {
			t.Errorf("unknown project: %d", r.Code)
		}
	})
}

func TestPatchArchiveRestore(t *testing.T) {
	a := newAPI(t)
	path := base(tenantA, projectA)
	r := a.do("tok-eng", http.MethodPost, path, `{"name":"svc"}`, idem("k")...).repo(t)
	repoPath := path + "/" + r.ID
	cur := a.do("tok-eng", http.MethodGet, repoPath, "").repo(t)

	t.Run("If-Match is required and compared", func(t *testing.T) {
		if x := a.do("tok-eng", http.MethodPatch, repoPath, `{"policy":{"directPushToDefault":true}}`); x.Code != http.StatusPreconditionRequired || x.code(t) != "version_required" {
			t.Errorf("no If-Match: %d %s", x.Code, x.Body.String())
		}
		if x := a.do("tok-eng", http.MethodPatch, repoPath, `{"policy":{"directPushToDefault":true}}`, "If-Match", etag(cur.Version+4)); x.Code != http.StatusPreconditionFailed || x.code(t) != "version_conflict" {
			t.Errorf("stale: %d %s", x.Code, x.Body.String())
		}
	})
	t.Run("strict bodies", func(t *testing.T) {
		for _, body := range []string{`{}`, `{"policy":{}}`, `{"policy":{"directPushToDefault":null}}`, `{"defaultBranch":"kodama/x"}`, `{"name":"renamed"}`} {
			if x := a.do("tok-eng", http.MethodPatch, repoPath, body, "If-Match", etag(cur.Version)); x.Code != http.StatusBadRequest {
				t.Errorf("%s: %d %s", body, x.Code, x.Body.String())
			}
		}
	})
	t.Run("the admin sets policy and nothing else", func(t *testing.T) {
		if x := a.do("tok-admin", http.MethodPatch, repoPath, `{"defaultBranch":"trunk"}`, "If-Match", etag(cur.Version)); x.Code != http.StatusForbidden {
			t.Errorf("admin branch: %d %s", x.Code, x.Body.String())
		}
		x := a.do("tok-admin", http.MethodPatch, repoPath, `{"policy":{"directPushToDefault":true}}`, "If-Match", etag(cur.Version))
		x.wantStatus(t, http.StatusOK)
		got := x.repo(t)
		if !got.Policy.DirectPushToDefault || got.Version != cur.Version+1 || x.Header().Get("ETag") != etag(got.Version) {
			t.Fatalf("policy: %+v %v", got, x.Header())
		}
		cur = got
		if v := a.do("tok-viewer", http.MethodPatch, repoPath, `{"policy":{"directPushToDefault":false}}`, "If-Match", etag(cur.Version)); v.Code != http.StatusForbidden {
			t.Errorf("viewer patch: %d", v.Code)
		}
	})
	t.Run("the engineer changes the branch", func(t *testing.T) {
		x := a.do("tok-eng", http.MethodPatch, repoPath, `{"defaultBranch":"trunk"}`, "If-Match", etag(cur.Version))
		x.wantStatus(t, http.StatusOK)
		cur = x.repo(t)
		if cur.DefaultBranch != "trunk" {
			t.Fatalf("branch: %+v", cur)
		}
	})
	t.Run("archive and restore", func(t *testing.T) {
		if x := a.do("tok-eng", http.MethodPost, repoPath+"/archive", ""); x.Code != http.StatusPreconditionRequired {
			t.Errorf("archive without If-Match: %d", x.Code)
		}
		if x := a.do("tok-eng", http.MethodPost, repoPath+"/archive", `{"x":1}`, "If-Match", etag(cur.Version)); x.Code != http.StatusBadRequest {
			t.Errorf("archive with a body: %d", x.Code)
		}
		if x := a.do("tok-viewer", http.MethodPost, repoPath+"/archive", `{}`, "If-Match", etag(cur.Version)); x.Code != http.StatusForbidden {
			t.Errorf("viewer archive: %d", x.Code)
		}
		x := a.do("tok-eng", http.MethodPost, repoPath+"/archive", `{}`, "If-Match", etag(cur.Version))
		x.wantStatus(t, http.StatusOK)
		arch := x.repo(t)
		if arch.Status != gitv1.StatusArchived || arch.Reason == nil || *arch.Reason != "archived" || arch.Version != cur.Version+1 {
			t.Fatalf("archived: %+v", arch)
		}
		if y := a.do("tok-eng", http.MethodPatch, repoPath, `{"defaultBranch":"main"}`, "If-Match", etag(arch.Version)); y.Code != http.StatusConflict || y.code(t) != "repository_archived" {
			t.Errorf("patch archived: %d %s", y.Code, y.Body.String())
		}
		z := a.do("tok-eng", http.MethodPost, repoPath+"/restore", ``, "If-Match", etag(arch.Version))
		z.wantStatus(t, http.StatusOK)
		if back := z.repo(t); back.Status != gitv1.StatusReady || back.Reason != nil || back.Version != arch.Version+1 {
			t.Fatalf("restored: %+v", back)
		}
	})
	t.Run("an external record is read-only", func(t *testing.T) {
		ext, _, err := a.kit.Svc.RegisterExternal(a.kit.Ctx, rt.System, rt.ScopeA2, repository.ExternalConfig{ID: "repo_cfg", URL: "https://git.example.test/acme/legacy.git", BaseRef: "main"})
		if err != nil {
			t.Fatal(err)
		}
		p := base(tenantA, projectA2) + "/" + string(ext.ID)
		for _, c := range []struct{ method, path, body string }{
			{http.MethodPatch, p, `{"defaultBranch":"trunk"}`},
			{http.MethodPost, p + "/archive", `{}`},
			{http.MethodPost, p + "/restore", `{}`},
		} {
			x := a.do("tok-eng", c.method, c.path, c.body, "If-Match", etag(int64(ext.Version)))
			if x.Code != http.StatusUnprocessableEntity || x.code(t) != "hosting_mismatch" {
				t.Errorf("%s %s: %d %s", c.method, c.path, x.Code, x.Body.String())
			}
		}
		if x := a.do("tok-eng", http.MethodGet, p+"/refs", ""); x.Code != http.StatusUnprocessableEntity {
			t.Errorf("external refs: %d", x.Code)
		}
		x := a.do("tok-viewer", http.MethodGet, p, "")
		x.wantStatus(t, http.StatusOK)
		if got := x.repo(t); got.Hosting != gitv1.HostingExternal || got.CloneURL != "https://git.example.test/acme/legacy.git" {
			t.Fatalf("external: %+v", got)
		}
	})
}

func TestCrossTenantIsNotFound(t *testing.T) {
	a := newAPI(t)
	r := a.do("tok-eng", http.MethodPost, base(tenantA, projectA), `{"name":"secret"}`, idem("k")...).repo(t)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, base(tenantB, projectB) + "/" + r.ID, ""},
		{http.MethodGet, base(tenantA, projectA2) + "/" + r.ID, ""},
		{http.MethodPatch, base(tenantB, projectB) + "/" + r.ID, `{"defaultBranch":"x"}`},
		{http.MethodPost, base(tenantB, projectB) + "/" + r.ID + "/archive", `{}`},
		{http.MethodPost, base(tenantB, projectB) + "/" + r.ID + "/restore", `{}`},
		{http.MethodGet, base(tenantB, projectB) + "/" + r.ID + "/refs", ""},
		{http.MethodGet, base(tenantA, projectA) + "/repo_guess", ""},
	} {
		x := a.do("tok-eng", c.method, c.path, c.body, "If-Match", `"2"`)
		if x.Code != http.StatusNotFound || x.code(t) != "not_found" {
			t.Errorf("%s %s: %d %s", c.method, c.path, x.Code, x.Body.String())
		}
	}
	list := a.do("tok-eng", http.MethodGet, base(tenantB, projectB), "")
	list.wantStatus(t, http.StatusOK)
	if !strings.Contains(list.Body.String(), `"items":[]`) {
		t.Fatalf("tenant B sees %s", list.Body.String())
	}
}

func TestListPaginationAndCursorScope(t *testing.T) {
	a := newAPI(t)
	path := base(tenantA, projectA)
	var want []string
	for i, name := range []string{"one", "two", "three", "four", "five"} {
		r := a.do("tok-eng", http.MethodPost, path, `{"name":"`+name+`"}`, idem("k"+string(rune('a'+i)))...).repo(t)
		want = append(want, r.ID)
	}
	var got []string
	next := ""
	for range 5 {
		q := path + "?limit=2"
		if next != "" {
			q += "&cursor=" + next
		}
		x := a.do("tok-viewer", http.MethodGet, q, "")
		x.wantStatus(t, http.StatusOK)
		page, err := gitv1.DecodeResponse[gitv1.RepositoryList](x.Body.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range page.Items {
			got = append(got, it.ID)
		}
		if page.NextCursor == nil {
			break
		}
		next = *page.NextCursor
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("paged %v, want %v", got, want)
	}
	// A cursor of one project is no position in another, and grants nothing.
	x := a.do("tok-viewer", http.MethodGet, path+"?limit=2", "")
	var page gitv1.RepositoryList
	if err := json.Unmarshal(x.Body.Bytes(), &page); err != nil || page.NextCursor == nil {
		t.Fatalf("first page: %v %s", err, x.Body.String())
	}
	if y := a.do("tok-eng", http.MethodGet, base(tenantA, projectA2)+"?cursor="+*page.NextCursor, ""); y.Code != http.StatusBadRequest || y.code(t) != "invalid_cursor" {
		t.Errorf("a foreign cursor: %d %s", y.Code, y.Body.String())
	}
	for _, q := range []string{"?limit=0", "?limit=201", "?filter=x", "?cursor=a&cursor=b"} {
		if y := a.do("tok-viewer", http.MethodGet, path+q, ""); y.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", q, y.Code)
		}
	}
	if y := a.do("tok-out", http.MethodGet, path, ""); y.Code != http.StatusForbidden {
		t.Errorf("outsider list: %d", y.Code)
	}
}

func TestRefsRead(t *testing.T) {
	a := newAPI(t)
	path := base(tenantA, projectA)
	r := a.do("tok-eng", http.MethodPost, path, `{"name":"code"}`, idem("k")...).repo(t)
	refs := path + "/" + r.ID + "/refs"
	next := "worker-page-2"
	a.kit.Hosts.Refs = gitv1.RefsResponse{
		Refs:       []gitv1.RefTip{{Ref: "refs/heads/main", Commit: strings.Repeat("a", 40)}},
		NextCursor: &next, Empty: false,
	}

	x := a.do("tok-viewer", http.MethodGet, refs+"?limit=1", "")
	x.wantStatus(t, http.StatusOK)
	page, err := gitv1.DecodeResponse[gitv1.RefsResponse](x.Body.Bytes())
	if err != nil || len(page.Refs) != 1 || page.NextCursor == nil || *page.NextCursor == next || page.Empty {
		t.Fatalf("refs: %+v %v", page, err)
	}
	last := a.kit.Hosts.RefsReqs[len(a.kit.Hosts.RefsReqs)-1]
	if last.Prefix != gitv1.HeadsPrefix || last.Limit != 1 || last.Cursor != nil || last.RepositoryID != r.ID {
		t.Fatalf("worker request: %+v", last)
	}

	// The signed cursor continues the worker's own.
	y := a.do("tok-viewer", http.MethodGet, refs+"?limit=1&cursor="+*page.NextCursor, "")
	y.wantStatus(t, http.StatusOK)
	last = a.kit.Hosts.RefsReqs[len(a.kit.Hosts.RefsReqs)-1]
	if last.Cursor == nil || *last.Cursor != next {
		t.Fatalf("worker cursor: %+v", last)
	}
	// A cursor of the heads listing is no position of the tags listing.
	if z := a.do("tok-viewer", http.MethodGet, refs+"?limit=1&prefix=refs/tags/&cursor="+*page.NextCursor, ""); z.Code != http.StatusBadRequest {
		t.Errorf("a cursor of another listing: %d", z.Code)
	}
	if z := a.do("tok-viewer", http.MethodGet, refs+"?prefix=refs/tags/", ""); z.Code != http.StatusOK {
		t.Errorf("tags: %d", z.Code)
	}
	for _, q := range []string{"?prefix=refs/kodama/", "?prefix=refs/", "?prefix=refs/heads/&prefix=refs/tags/", "?x=1"} {
		if z := a.do("tok-viewer", http.MethodGet, refs+q, ""); z.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", q, z.Code)
		}
	}
	a.kit.Hosts.RefsErr = rt.Unavailable()
	if z := a.do("tok-viewer", http.MethodGet, refs, ""); z.Code != http.StatusServiceUnavailable || z.code(t) != "unavailable" {
		t.Errorf("worker outage: %d %s", z.Code, z.Body.String())
	}
	a.kit.Hosts.RefsErr = nil

	// A repository that is still provisioning has no refs to read.
	a.kit.Hosts.Script(rt.ScriptedFailure{Err: rt.Unavailable()})
	p := a.do("tok-eng", http.MethodPost, path, `{"name":"young"}`, idem("k2")...).repo(t)
	if z := a.do("tok-viewer", http.MethodGet, path+"/"+p.ID+"/refs", ""); z.Code != http.StatusConflict || z.code(t) != "repository_not_ready" {
		t.Errorf("provisioning refs: %d %s", z.Code, z.Body.String())
	}
	if got := a.do("tok-viewer", http.MethodGet, path+"/"+p.ID, "").repo(t); got.Status != gitv1.StatusProvisioning || got.Reason == nil || *got.Reason != repository.ReasonWorkerUnavailable {
		t.Errorf("a failed initialization is a reason: %+v", got)
	}
}

func TestGeneratedTypesMatchTheContract(t *testing.T) {
	reason := "archived"
	cursor := "abc"
	on := true
	for name, c := range map[string]struct {
		sample any
		into   any
	}{
		"ready repository": {gitv1.Repository{ID: "repo_1", Name: "a", DefaultBranch: "main", Hosting: gitv1.HostingInternal, Status: gitv1.StatusReady, CloneURL: "https://g.test/x.git",
			Empty: true, Version: 2, CreatedAt: "2026-10-30T10:00:00.001Z"}, &repositoryhttp.Repository{}},
		"archived repository": {gitv1.Repository{ID: "repo_1", Name: "a", DefaultBranch: "main", Hosting: gitv1.HostingExternal, Status: gitv1.StatusArchived, Reason: &reason,
			CloneURL: "https://g.test/x.git", Policy: gitv1.RepositoryPolicy{DirectPushToDefault: true}, Version: 3, CreatedAt: "2026-10-30T10:00:00Z"}, &repositoryhttp.Repository{}},
		"minimal create":  {gitv1.CreateRepositoryRequest{Name: "a"}, &repositoryhttp.CreateRepositoryRequest{}},
		"explicit create": {gitv1.CreateRepositoryRequest{Name: "a", DefaultBranch: &reason, Policy: &gitv1.RepositoryPolicyInput{DirectPushToDefault: &on}}, &repositoryhttp.CreateRepositoryRequest{}},
		"branch patch":    {gitv1.PatchRepositoryRequest{DefaultBranch: &reason}, &repositoryhttp.PatchRepositoryRequest{}},
		"policy patch":    {gitv1.PatchRepositoryRequest{Policy: &gitv1.RepositoryPolicyInput{DirectPushToDefault: &on}}, &repositoryhttp.PatchRepositoryRequest{}},
		"repository list": {gitv1.RepositoryList{Items: []gitv1.Repository{}, NextCursor: &cursor}, &repositoryhttp.RepositoryList{}},
		"refs page":       {gitv1.RefsResponse{Refs: []gitv1.RefTip{{Ref: "refs/heads/main", Commit: strings.Repeat("a", 40)}}, NextCursor: nil, Empty: false}, &repositoryhttp.RefsPage{}},
		"an empty refs":   {gitv1.RefsResponse{Refs: []gitv1.RefTip{}, Empty: true}, &repositoryhttp.RefsPage{}},
	} {
		want, err := json.Marshal(c.sample)
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewReader(want))
		dec.DisallowUnknownFields() // the generated type has every member the contract has
		if err := dec.Decode(c.into); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := json.Marshal(c.into)
		if err != nil {
			t.Fatal(err)
		}
		if !jsonEqual(t, got, want) {
			t.Errorf("%s: generated %s, contract %s", name, got, want)
		}
	}
}

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatal(err)
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return bytes.Equal(xb, yb)
}
