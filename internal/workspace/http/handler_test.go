package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdhttp "net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/workspace"
	gitad "github.com/optimiware/kodama/internal/workspace/git"
	wshttp "github.com/optimiware/kodama/internal/workspace/http"
	wt "github.com/optimiware/kodama/internal/workspace/workspacetest"
	gitfx "github.com/optimiware/kodama/test/support/git"
)

// tokenAuth authenticates "Bearer <principal>".
type tokenAuth struct{}

func (tokenAuth) Authenticate(_ context.Context, r *stdhttp.Request) (httpapi.Principal, error) {
	id, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || id == "" {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	return httpapi.Principal{ID: id}, nil
}

// routerAuthz is the router-level, deny-by-default authorizer: engineers act in
// every fixture project, the other tenant's user only in tenant B (hidden
// elsewhere) and everyone else is refused.
type routerAuthz struct{}

func (routerAuthz) Authorize(_ context.Context, p httpapi.Principal, _ httpapi.Action, t httpapi.Target) error {
	switch p.ID {
	case wt.Engineer.ID:
		return nil
	case "usr_other_tenant":
		if t.TenantID == wt.ScopeB.TenantID {
			return nil
		}
		return httpapi.ErrHidden
	}
	return httpapi.ErrDenied
}

type harness struct {
	t   *testing.T
	e   *wt.Env
	rt  *httpapi.Router
	api *schemas
}

func newHarness(t *testing.T, cfg wt.Config) *harness {
	t.Helper()
	e := wt.New(t, cfg)
	rt, err := httpapi.NewRouter(httpapi.RouterConfig{Authenticator: tokenAuth{}, Authorizer: routerAuthz{}, RateLimiter: httpapi.Unlimited(), Prefix: "/api/v1"})
	if err != nil {
		t.Fatal(err)
	}
	h, err := wshttp.NewHandler(e.Svc)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Register(rt); err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, e: e, rt: rt, api: loadSchemas(t)}
}

type response struct {
	*httptest.ResponseRecorder
}

func (h *harness) do(method, path, principal string, headers map[string]string, body string) response {
	h.t.Helper()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, "/api/v1"+path, rd)
	if principal != "" {
		req.Header.Set("Authorization", "Bearer "+principal)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.rt.ServeHTTP(rec, req)
	return response{rec}
}

func (r response) errorCode(t *testing.T) string {
	t.Helper()
	var env httpapi.ErrorEnvelope
	if err := json.Unmarshal(r.Body.Bytes(), &env); err != nil {
		t.Fatalf("not an error envelope: %s", r.Body)
	}
	return env.Error.Code
}

func (r response) workspace(t *testing.T) wshttp.Workspace {
	t.Helper()
	var w wshttp.Workspace
	dec := json.NewDecoder(bytes.NewReader(r.Body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		t.Fatalf("not a workspace: %v: %s", err, r.Body)
	}
	return w
}

const (
	pA = "/tenants/" + "t013_acme" + "/projects/" + "p013_inventory"
	pB = "/tenants/" + "t013_globex" + "/projects/" + "p013_billing"
)

func ifMatch(v int64) map[string]string { return map[string]string{"If-Match": fmt.Sprintf(`"%d"`, v)} }

// schemas validates payloads against api/workspace.yaml and api/common.yaml.
type schemas struct {
	c   *jsonschema.Compiler
	raw map[string]any
}

func loadSchemas(t *testing.T) *schemas {
	t.Helper()
	read := func(path string) map[string]any {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		if err := yaml.Unmarshal(data, &raw); err != nil {
			t.Fatal(err)
		}
		var fix func(any)
		fix = func(n any) {
			switch v := n.(type) {
			case map[string]any:
				if nb, _ := v["nullable"].(bool); nb {
					if ty, ok := v["type"].(string); ok {
						v["type"] = []any{ty, "null"}
					}
				}
				delete(v, "nullable")
				for _, c := range v {
					fix(c)
				}
			case []any:
				for _, c := range v {
					fix(c)
				}
			}
		}
		fix(raw)
		return raw
	}
	c := jsonschema.NewCompiler()
	var out map[string]any
	for name, path := range map[string]string{"common.json": "../../../api/common.yaml", "workspace.json": "../../../api/workspace.yaml"} {
		raw := read(path)
		js, _ := json.Marshal(raw)
		js = bytes.ReplaceAll(js, []byte("common.yaml#"), []byte("common.json#"))
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(js))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.AddResource(name, doc); err != nil {
			t.Fatal(err)
		}
		if name == "workspace.json" {
			out = raw
		}
	}
	return &schemas{c: c, raw: out}
}

func (s *schemas) validate(t *testing.T, resource, schema string, doc []byte) error {
	t.Helper()
	sch, err := s.c.Compile(resource + "#/components/schemas/" + schema)
	if err != nil {
		t.Fatalf("schema %s: %v", schema, err)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		t.Fatalf("not JSON: %s", doc)
	}
	return sch.Validate(v)
}

// Test_workspace_http_contract: authorization denials, hidden foreign IDs,
// required/stale ETags, natural provisioning retries, safe errors, and the
// OpenAPI fragment's examples against the handlers' real output.
func Test_workspace_http_contract(t *testing.T) {
	t.Run("provisioning is natural, idempotent and returns the resource", func(t *testing.T) {
		h := newHarness(t, wt.Config{})
		h.e.Items.Add(wt.ScopeA, "wi_1", "story")
		first := h.do("POST", pA+"/work-items/wi_1/workspace", wt.Engineer.ID, nil, "")
		if first.Code != 201 {
			t.Fatalf("%d %s", first.Code, first.Body)
		}
		w := first.workspace(t)
		if w.State != wshttp.Available || w.Branch != "refs/heads/kodama/work/wi_1" || w.AcceptedCommit == nil || *w.AcceptedCommit != h.e.Repo.Initial || w.Publication != nil {
			t.Fatalf("%+v", w)
		}
		if err := h.api.validate(t, "workspace.json", "Workspace", first.Body.Bytes()); err != nil {
			t.Fatalf("response does not match the schema: %v", err)
		}
		if tag := first.Header().Get("ETag"); tag != fmt.Sprintf(`"%d"`, w.Version) {
			t.Fatalf("ETag %q for version %d", tag, w.Version)
		}
		// An empty JSON object body is accepted; no Idempotency-Key is needed.
		again := h.do("POST", pA+"/work-items/wi_1/workspace", wt.Engineer.ID, nil, `{}`)
		if again.Code != 200 || again.workspace(t).Id != w.Id {
			t.Fatalf("%d %s", again.Code, again.Body)
		}
		// Concurrent first requests for another item still yield one workspace.
		h.e.Items.Add(wt.ScopeA, "wi_2", "story")
		var wg sync.WaitGroup
		ids := make([]string, 6)
		codes := make([]int, 6)
		for i := range ids {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := h.do("POST", pA+"/work-items/wi_2/workspace", wt.Engineer.ID, nil, "")
				codes[i] = r.Code
				if r.Code < 300 {
					ids[i] = r.workspace(t).Id
				}
			}()
		}
		wg.Wait()
		created := 0
		for i := range ids {
			if ids[i] != ids[0] || ids[i] == "" {
				t.Fatalf("different workspaces: %v (%v)", ids, codes)
			}
			if codes[i] == 201 {
				created++
			}
		}
		if created != 1 {
			t.Fatalf("%d callers saw 201: %v", created, codes)
		}
		// A body with content is refused, never silently ignored.
		if r := h.do("POST", pA+"/work-items/wi_1/workspace", wt.Engineer.ID, nil, `{"branch":"x"}`); r.Code != 400 {
			t.Fatalf("%d", r.Code)
		}
	})

	t.Run("a workspace that cannot finish provisioning is returned as accepted with a safe reason", func(t *testing.T) {
		h := newHarness(t, wt.Config{})
		h.e.Items.Add(wt.ScopeA, "wi_1", "story")
		h.e.Repo.SetRef(t, "refs/heads/kodama/work/wi_1", h.e.Repo.OrphanCommit(t, "independent"))
		r := h.do("POST", pA+"/work-items/wi_1/workspace", wt.Engineer.ID, nil, "")
		if r.Code != 202 {
			t.Fatalf("%d %s", r.Code, r.Body)
		}
		w := r.workspace(t)
		if w.State != wshttp.Provisioning || w.LastError == nil || *w.LastError != "branch_exists" || w.AcceptedCommit != nil {
			t.Fatalf("%+v", w)
		}
		if err := h.api.validate(t, "workspace.json", "Workspace", r.Body.Bytes()); err != nil {
			t.Fatal(err)
		}
		for _, leak := range []string{h.e.Git.URL, h.e.Git.ReadToken, h.e.Git.WriteToken, "secret://"} {
			if strings.Contains(r.Body.String(), leak) {
				t.Fatalf("response leaks %q", leak)
			}
		}
	})

	t.Run("authentication, authorization and hidden foreign references", func(t *testing.T) {
		h := newHarness(t, wt.Config{})
		h.e.Items.Add(wt.ScopeA, "wi_1", "story")
		h.e.Items.Add(wt.ScopeA, "epic_1", "epic")
		h.e.Items.Add(wt.ScopeB, "wi_b", "story")
		ws := h.e.Ready(wt.ScopeA, "wi_ready")

		if r := h.do("POST", pA+"/work-items/wi_1/workspace", "", nil, ""); r.Code != 401 {
			t.Fatalf("anonymous: %d", r.Code)
		}
		if r := h.do("POST", pA+"/work-items/wi_1/workspace", "usr_nobody", nil, ""); r.Code != 403 {
			t.Fatalf("ungranted: %d", r.Code)
		}
		other := h.do("GET", pA+"/workspaces/"+string(ws.ID), "usr_other_tenant", nil, "")
		missing := h.do("GET", pA+"/workspaces/ws_does_not_exist", wt.Engineer.ID, nil, "")
		if other.Code != 404 || missing.Code != 404 || other.errorCode(t) != "not_found" {
			t.Fatalf("hidden: %d / %d", other.Code, missing.Code)
		}
		var a, b httpapi.ErrorEnvelope
		_ = json.Unmarshal(other.Body.Bytes(), &a)
		_ = json.Unmarshal(missing.Body.Bytes(), &b)
		if a.Error.Message != b.Error.Message {
			t.Fatal("a hidden reference is distinguishable from a missing one")
		}
		// The same workspace id under another tenant/project path does not exist.
		if r := h.do("GET", pB+"/workspaces/"+string(ws.ID), wt.Engineer.ID, nil, ""); r.Code != 404 {
			t.Fatalf("foreign path: %d", r.Code)
		}
		// A work item of another tenant cannot be provisioned through this one.
		if r := h.do("POST", pA+"/work-items/wi_b/workspace", wt.Engineer.ID, nil, ""); r.Code != 404 {
			t.Fatalf("foreign work item: %d %s", r.Code, r.Body)
		}
		epic := h.do("POST", pA+"/work-items/epic_1/workspace", wt.Engineer.ID, nil, "")
		if epic.Code != 409 || epic.errorCode(t) != "work_item_not_actionable" {
			t.Fatalf("epic: %d %s", epic.Code, epic.Body)
		}
		if r := h.do("GET", pA+"/workspaces/bad%20id", wt.Engineer.ID, nil, ""); r.Code != 400 {
			t.Fatalf("malformed id: %d", r.Code)
		}
		get := h.do("GET", pA+"/workspaces/"+string(ws.ID), wt.Engineer.ID, nil, "")
		if get.Code != 200 || get.workspace(t).Id != string(ws.ID) || get.Header().Get("ETag") == "" {
			t.Fatalf("%d %s", get.Code, get.Body)
		}
	})

	t.Run("mutations need a current, well-formed If-Match", func(t *testing.T) {
		h := newHarness(t, wt.Config{})
		ws := h.e.Ready(wt.ScopeA, "wi_1")
		base := pA + "/workspaces/" + string(ws.ID)
		for _, op := range []string{"/archive", "/restore", "/sync"} {
			if r := h.do("POST", base+op, wt.Engineer.ID, nil, ""); r.Code != 428 || r.errorCode(t) != "version_required" {
				t.Fatalf("%s without If-Match: %d", op, r.Code)
			}
			for _, bad := range []string{`W/"1"`, `*`, `"1", "2"`, `"01"`, `1`} {
				if r := h.do("POST", base+op, wt.Engineer.ID, map[string]string{"If-Match": bad}, ""); r.Code != 400 {
					t.Fatalf("%s with If-Match %s: %d", op, bad, r.Code)
				}
			}
			if r := h.do("POST", base+op, wt.Engineer.ID, ifMatch(int64(ws.Version)+5), ""); r.Code != 412 || r.errorCode(t) != "version_conflict" {
				t.Fatalf("%s stale: %d", op, r.Code)
			}
		}
		// Sync with the current version is a no-op returning the same ETag.
		sync := h.do("POST", base+"/sync", wt.Engineer.ID, ifMatch(int64(ws.Version)), "")
		if sync.Code != 200 || sync.Header().Get("ETag") != fmt.Sprintf(`"%d"`, ws.Version) {
			t.Fatalf("%d %s", sync.Code, sync.Body)
		}
		// A human push is observed by sync and moves the ETag.
		human := h.e.Repo.HumanPush(t, ws.WorkBranch, "human", gitfx.File{Path: "h.txt", Content: "h"})
		sync = h.do("POST", base+"/sync", wt.Engineer.ID, ifMatch(int64(ws.Version)), "")
		w := sync.workspace(t)
		if sync.Code != 200 || *w.AcceptedCommit != human || w.Version <= int64(ws.Version) {
			t.Fatalf("%d %s", sync.Code, sync.Body)
		}
		// Archive then restore, each with the current tag.
		arch := h.do("POST", base+"/archive", wt.Engineer.ID, ifMatch(w.Version), "")
		aw := arch.workspace(t)
		if arch.Code != 200 || aw.State != wshttp.Archived {
			t.Fatalf("%d %s", arch.Code, arch.Body)
		}
		if r := h.do("POST", base+"/archive", wt.Engineer.ID, ifMatch(w.Version), ""); r.Code != 412 {
			t.Fatalf("replayed archive with the old tag: %d", r.Code)
		}
		rest := h.do("POST", base+"/restore", wt.Engineer.ID, ifMatch(aw.Version), "")
		if rest.Code != 200 || rest.workspace(t).State != wshttp.Available || rest.workspace(t).Id != w.Id {
			t.Fatalf("%d %s", rest.Code, rest.Body)
		}
		if err := h.api.validate(t, "workspace.json", "Workspace", rest.Body.Bytes()); err != nil {
			t.Fatal(err)
		}
		// Diverged history is reported, not adopted.
		h.e.Repo.SetRef(t, ws.WorkBranch, h.e.Repo.OrphanCommit(t, "rewrite"))
		cur := h.do("GET", base, wt.Engineer.ID, nil, "").workspace(t)
		div := h.do("POST", base+"/sync", wt.Engineer.ID, ifMatch(cur.Version), "")
		if div.Code != 409 || div.errorCode(t) != "branch_diverged" {
			t.Fatalf("%d %s", div.Code, div.Body)
		}
		if err := h.api.validate(t, "common.json", "ErrorEnvelope", div.Body.Bytes()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("archive uncertainty is a classified non-success that carries the new ETag", func(t *testing.T) {
		opts := wt.DefaultOptions()
		opts.ArchiveWait = 500 * time.Millisecond
		h := newHarness(t, wt.Config{Service: opts, Git: func(o *gitad.Options) { o.CommandTimeout = 3 * time.Second }})
		ws := h.e.Ready(wt.ScopeA, "wi_1")
		h.e.Writer(ws, "att_1")
		in := h.e.Proposal(ws, "att_1", h.e.Repo.Initial, "change", gitfx.File{Path: "a.txt", Content: "a"})
		h.e.Git.HoldReceivePack()
		if res, err := h.e.Svc.ProposeSource(context.Background(), in); err != nil || res.Status != workspace.ProposePending {
			t.Fatalf("%+v %v", res, err)
		}
		cur := h.do("GET", pA+"/workspaces/"+string(ws.ID), wt.Engineer.ID, nil, "").workspace(t)
		if cur.Publication == nil || cur.WriterAttemptId == nil || *cur.WriterAttemptId != "att_1" {
			t.Fatalf("%+v", cur)
		}
		r := h.do("POST", pA+"/workspaces/"+string(ws.ID)+"/archive", wt.Engineer.ID, ifMatch(cur.Version), "")
		if r.Code != 409 || r.errorCode(t) != "archive_pending" {
			t.Fatalf("%d %s", r.Code, r.Body)
		}
		if err := h.api.validate(t, "common.json", "ErrorEnvelope", r.Body.Bytes()); err != nil {
			t.Fatal(err)
		}
		var env httpapi.ErrorEnvelope
		_ = json.Unmarshal(r.Body.Bytes(), &env)
		if env.Error.Details["publicationPending"] != true || env.Error.Details["state"] != "available" {
			t.Fatalf("%v", env.Error.Details)
		}
		after := h.do("GET", pA+"/workspaces/"+string(ws.ID), wt.Engineer.ID, nil, "")
		if r.Header().Get("ETag") != after.Header().Get("ETag") || r.Header().Get("ETag") == fmt.Sprintf(`"%d"`, cur.Version) {
			t.Fatalf("the response must carry the new ETag: %q vs %q", r.Header().Get("ETag"), after.Header().Get("ETag"))
		}
		if aw := after.workspace(t); aw.ArchiveRequestedAt == nil || aw.State != wshttp.Available {
			t.Fatalf("%+v", aw)
		}
		// Restore is refused while the publication is unsettled.
		rest := h.do("POST", pA+"/workspaces/"+string(ws.ID)+"/restore", wt.Engineer.ID, ifMatch(after.workspace(t).Version), "")
		if rest.Code != 409 || rest.errorCode(t) != "publication_pending" {
			t.Fatalf("%d %s", rest.Code, rest.Body)
		}
	})

	t.Run("failures are safe: no credentials, URLs or driver text", func(t *testing.T) {
		h := newHarness(t, wt.Config{})
		h.e.Items.Add(wt.ScopeA, "wi_1", "story")
		h.e.Repos.Err = errors.New("dial postgres://admin:hunter2@db.internal:5432 refused; token=" + h.e.Git.WriteToken)
		r := h.do("POST", pA+"/work-items/wi_1/workspace", wt.Engineer.ID, nil, "")
		if r.Code != 503 || r.errorCode(t) != "unavailable" || r.Header().Get("Retry-After") == "" {
			t.Fatalf("%d %s", r.Code, r.Body)
		}
		for _, leak := range []string{"hunter2", "db.internal", h.e.Git.WriteToken, "postgres://"} {
			if strings.Contains(r.Body.String(), leak) {
				t.Fatalf("response leaks %q: %s", leak, r.Body)
			}
		}
		if err := h.api.validate(t, "common.json", "ErrorEnvelope", r.Body.Bytes()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("the OpenAPI fragment declares exactly the five contracted operations and valid examples", func(t *testing.T) {
		h := newHarness(t, wt.Config{})
		paths := h.api.raw["paths"].(map[string]any)
		want := map[string][]string{
			"/tenants/{tenantId}/projects/{projectId}/work-items/{id}/workspace": {"post"},
			"/tenants/{tenantId}/projects/{projectId}/workspaces/{id}":           {"get"},
			"/tenants/{tenantId}/projects/{projectId}/workspaces/{id}/archive":   {"post"},
			"/tenants/{tenantId}/projects/{projectId}/workspaces/{id}/restore":   {"post"},
			"/tenants/{tenantId}/projects/{projectId}/workspaces/{id}/sync":      {"post"},
		}
		if len(paths) != len(want) {
			t.Fatalf("%d paths, want %d (no publication or deletion route)", len(paths), len(want))
		}
		for path, methods := range want {
			item, ok := paths[path].(map[string]any)
			if !ok || len(item) != len(methods) {
				t.Fatalf("%s: %v", path, item)
			}
			if _, ok := item[methods[0]]; !ok {
				t.Fatalf("%s lacks %s", path, methods[0])
			}
		}
		// Every example validates against its schema.
		for name, ex := range map[string]struct {
			schema string
			res    string
			path   []string
		}{
			"available":           {"Workspace", "workspace.json", []string{"components", "responses", "Workspace", "content", "application/json", "examples", "available", "value"}},
			"provisioningPending": {"Workspace", "workspace.json", []string{"paths", "/tenants/{tenantId}/projects/{projectId}/work-items/{id}/workspace", "post", "responses", "202", "content", "application/json", "examples", "provisioningPending", "value"}},
			"diverged":            {"ErrorEnvelope", "common.json", []string{"components", "responses", "Conflict", "content", "application/json", "examples", "diverged", "value"}},
			"archivePending":      {"ErrorEnvelope", "common.json", []string{"components", "responses", "ArchiveConflict", "content", "application/json", "examples", "archivePending", "value"}},
		} {
			var n any = h.api.raw
			for _, p := range ex.path {
				m, ok := n.(map[string]any)
				if !ok {
					t.Fatalf("%s: broken path at %s", name, p)
				}
				n = m[p]
			}
			doc, err := json.Marshal(n)
			if err != nil || n == nil {
				t.Fatalf("%s: no example", name)
			}
			if err := h.api.validate(t, ex.res, ex.schema, doc); err != nil {
				t.Errorf("example %s does not validate: %v", name, err)
			}
		}
		// The schema rejects states other than the three, extra members and non-safe errors.
		for name, doc := range map[string]string{
			"unknown state": `{"id":"a","workItemId":"w","repositoryId":"r","branch":"refs/heads/x","acceptedCommit":null,"state":"failed","writerAttemptId":null,"publication":null,"archiveRequestedAt":null,"lastError":null,"version":1,"createdAt":"2026-10-19T10:00:00Z","updatedAt":"2026-10-19T10:00:00Z"}`,
			"extra member":  `{"id":"a","workItemId":"w","repositoryId":"r","branch":"refs/heads/x","acceptedCommit":null,"state":"available","writerAttemptId":null,"publication":null,"archiveRequestedAt":null,"lastError":null,"version":1,"createdAt":"2026-10-19T10:00:00Z","updatedAt":"2026-10-19T10:00:00Z","repositoryUrl":"https://x"}`,
			"free-text err": `{"id":"a","workItemId":"w","repositoryId":"r","branch":"refs/heads/x","acceptedCommit":null,"state":"available","writerAttemptId":null,"publication":null,"archiveRequestedAt":null,"lastError":"Connection refused to host","version":1,"createdAt":"2026-10-19T10:00:00Z","updatedAt":"2026-10-19T10:00:00Z"}`,
			"short commit":  `{"id":"a","workItemId":"w","repositoryId":"r","branch":"refs/heads/x","acceptedCommit":"abc","state":"available","writerAttemptId":null,"publication":null,"archiveRequestedAt":null,"lastError":null,"version":1,"createdAt":"2026-10-19T10:00:00Z","updatedAt":"2026-10-19T10:00:00Z"}`,
		} {
			if err := h.api.validate(t, "workspace.json", "Workspace", []byte(doc)); err == nil {
				t.Errorf("%s was accepted", name)
			}
		}
	})

	t.Run("construction and registration are explicit", func(t *testing.T) {
		if _, err := wshttp.NewHandler(nil); !errors.Is(err, workspace.ErrNotConfigured) {
			t.Fatalf("%v", err)
		}
		_ = domain.Scope{}
		_ = gitfx.Username
	})
}
