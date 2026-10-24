package executionhttp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	executionhttp "github.com/optimiware/kodama/internal/execution/http"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

const (
	runsYAML   = "../../../api/runs.yaml"
	commonYAML = "../../../api/common.yaml"
)

type spec struct {
	raw map[string]any
	c   *jsonschema.Compiler
}

func loadYAML(t testing.TB, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

// translate rewrites OpenAPI 3.0 nullability into JSON Schema so the fragment can
// validate documents.
func translate(n any) {
	switch v := n.(type) {
	case map[string]any:
		if nb, _ := v["nullable"].(bool); nb {
			if ty, ok := v["type"].(string); ok {
				v["type"] = []any{ty, "null"}
			} else if all, ok := v["allOf"].([]any); ok {
				delete(v, "allOf")
				v["anyOf"] = []any{map[string]any{"allOf": all}, map[string]any{"type": "null"}}
			}
		}
		delete(v, "nullable")
		for _, c := range v {
			translate(c)
		}
	case []any:
		for _, c := range v {
			translate(c)
		}
	}
}

func loadSpec(t testing.TB) *spec {
	t.Helper()
	c := jsonschema.NewCompiler()
	var out map[string]any
	for name, path := range map[string]string{"common.yaml": commonYAML, "runs.yaml": runsYAML} {
		raw := loadYAML(t, path)
		translate(raw)
		js, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(js))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.AddResource(name, doc); err != nil {
			t.Fatal(err)
		}
		if name == "runs.yaml" {
			out = raw
		}
	}
	return &spec{raw: out, c: c}
}

func (s *spec) validate(t testing.TB, schema string, doc []byte) error {
	t.Helper()
	sch, err := s.c.Compile("runs.yaml#/components/schemas/" + schema)
	if err != nil {
		t.Fatalf("schema %s: %v", schema, err)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		t.Fatalf("not JSON: %v: %s", err, doc)
	}
	return sch.Validate(v)
}

func (s *spec) example(t testing.TB, name string) []byte {
	t.Helper()
	ex := s.raw["components"].(map[string]any)["examples"].(map[string]any)[name].(map[string]any)["value"]
	b, err := json.Marshal(ex)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type tokens map[string]string

func (tk tokens) Authenticate(_ context.Context, r *http.Request) (httpapi.Principal, error) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	id, ok := tk[tok]
	if !ok {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	return httpapi.Principal{ID: id}, nil
}

// routeAuthorizer allows every route: the use cases enforce run:read and
// run:manage themselves, and that is what these tests rely on.
type routeAuthorizer struct{}

func (routeAuthorizer) Authorize(context.Context, httpapi.Principal, httpapi.Action, httpapi.Target) error {
	return nil
}

type api struct {
	t      testing.TB
	e      *executiontest.Env
	router *httpapi.Router
	p      executiontest.Project
	def    pipeline.Definition
}

func newAPI(t testing.TB) *api {
	t.Helper()
	e := executiontest.New(t)
	e.GrantAll()
	a := &api{t: t, e: e}
	rt, err := httpapi.NewRouter(httpapi.RouterConfig{
		Authenticator: tokens{"tok-engineer": executiontest.Engineer, "tok-viewer": executiontest.Viewer, "tok-stranger": executiontest.Stranger, "tok-other": executiontest.OtherEng},
		Authorizer:    routeAuthorizer{}, RateLimiter: httpapi.Unlimited(), Prefix: "/api/v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	codec, err := httpapi.NewCursorCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	h, err := executionhttp.NewHandler(executionhttp.Config{Service: e.Svc, Creator: e.Keyed, Cursors: codec})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.RegisterRoutes(rt); err != nil {
		t.Fatal(err)
	}
	a.router = rt
	a.p = e.NewProject(executiontest.ScopeA)
	a.def = e.Define(a.p.Scope, pipelinetest.DocNamed("web", "{region: {type: string, default: eu}}",
		"{id: build, kind: container, image: "+pipelinetest.Image+", command: [/bin/sh, -ec], timeoutSeconds: 60, workspace: {access: read, from: run}, outputs: {label: {type: string}}}",
		"{id: test, kind: container, image: "+pipelinetest.Image+", command: [/bin/sh, -ec], timeoutSeconds: 60, needs: [build], workspace: {access: read, from: run}}"))
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

func (r resp) decode(t testing.TB, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Body.Bytes(), v); err != nil {
		t.Fatalf("not the expected JSON: %v: %s", err, r.Body.String())
	}
}

func (r resp) errCode(t testing.TB) string {
	t.Helper()
	var e httpapi.ErrorEnvelope
	r.decode(t, &e)
	if e.Error.RequestId == "" || r.Header().Get("X-Request-ID") != e.Error.RequestId {
		t.Fatalf("error envelope without a matching request id: %s", r.Body.String())
	}
	return e.Error.Code
}

func (a *api) base() string {
	return "/tenants/" + string(a.p.Scope.TenantID) + "/projects/" + string(a.p.Scope.ProjectID) + "/runs"
}

func (a *api) createBody(extra string) string {
	return `{"workItemId":"` + string(a.p.Item) + `","definitionId":"` + string(a.def.ID) + `","contentRevision":` + itoa(int64(a.e.ContentRevision(a.p))) +
		`,"sourceCommit":"` + executiontest.CommitA + `"` + extra + `}`
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func (a *api) create(key, body string) resp {
	return a.do("tok-engineer", http.MethodPost, a.base(), body, "Idempotency-Key", key)
}

func (a *api) mustCreate(key string) executionhttp.Run {
	a.t.Helper()
	r := a.create(key, a.createBody(""))
	if r.Code != http.StatusCreated {
		a.t.Fatalf("create %s: %d %s", key, r.Code, r.Body.String())
	}
	var run executionhttp.Run
	r.decode(a.t, &run)
	return run
}

// Test_runs_http_contract: PX-02, PX-08, PX-12 over the public interface. OpenAPI
// examples and responses, strict requests, pagination, ETags and preconditions,
// keyed creation, 202 cancellation and safe error mapping, against real PostgreSQL.
func Test_runs_http_contract(t *testing.T) {
	sp := loadSpec(t)

	t.Run("the OpenAPI examples are valid", func(t *testing.T) {
		for schema, example := range map[string]string{"Run": "CreatedRun", "Run ": "CancellingRun"} {
			if err := sp.validate(t, strings.TrimSpace(schema), sp.example(t, example)); err != nil {
				t.Errorf("%s: %v", example, err)
			}
		}
		req := `{"workItemId":"wi_1042","definitionId":"pd_01","contentRevision":2,"sourceCommit":"1111111111111111111111111111111111111111","inputs":{"region":"eu"}}`
		if err := sp.validate(t, "CreateRunRequest", []byte(req)); err != nil {
			t.Errorf("request example: %v", err)
		}
		for name, bad := range map[string]string{
			"unknown field":  `{"workItemId":"wi_1","definitionId":"pd_1","contentRevision":1,"sourceCommit":"1111111111111111111111111111111111111111","purpose":"cleanup"}`,
			"missing commit": `{"workItemId":"wi_1","definitionId":"pd_1","contentRevision":1}`,
			"float input":    `{"workItemId":"wi_1","definitionId":"pd_1","contentRevision":1,"sourceCommit":"1111111111111111111111111111111111111111","inputs":{"a":1.5}}`,
			"object input":   `{"workItemId":"wi_1","definitionId":"pd_1","contentRevision":1,"sourceCommit":"1111111111111111111111111111111111111111","inputs":{"a":{}}}`,
		} {
			if sp.validate(t, "CreateRunRequest", []byte(bad)) == nil {
				t.Errorf("the schema accepts a request with %s", name)
			}
		}
	})

	t.Run("keyed creation, replay and conflicts", func(t *testing.T) {
		a := newAPI(t)
		first := a.create("key-1", a.createBody(`,"inputs":{"region":"ap"}`))
		if first.Code != http.StatusCreated || first.Header().Get("ETag") != `"1"` || !strings.HasPrefix(first.Header().Get("Location"), "/api/v1"+a.base()+"/run_") {
			t.Fatalf("create: %d etag %q location %q: %s", first.Code, first.Header().Get("ETag"), first.Header().Get("Location"), first.Body.String())
		}
		if err := sp.validate(t, "Run", first.Body.Bytes()); err != nil {
			t.Fatalf("the response violates its schema: %v\n%s", err, first.Body.String())
		}
		var run executionhttp.Run
		first.decode(t, &run)
		if run.Status != "queued" || run.Purpose != "work" || run.InputSnapshot.Inputs[0].Source != "run_input" || run.InputSnapshot.Digest == "" {
			t.Fatalf("run %+v", run)
		}
		// A lost response is recovered with the same key: the stored response
		// is replayed and nothing new is created.
		again := a.create("key-1", a.createBody(`,"inputs":{"region":"ap"}`))
		if again.Code != http.StatusCreated || again.Header().Get("Idempotent-Replayed") != "true" || again.Body.String() != first.Body.String() {
			t.Fatalf("replay: %d replayed=%q", again.Code, again.Header().Get("Idempotent-Replayed"))
		}
		// The retained source reference was confirmed after the commit.
		if got := a.e.Get(&execution.Run{Scope: a.p.Scope, ID: domain.RunID(run.Id)}).Run; got.SourceRetention != execution.RetentionConfirmed {
			t.Fatalf("source retention %s", got.SourceRetention)
		}
		if a.e.Count(`SELECT count(*) FROM runs`) != 1 {
			t.Fatal("the replay created a run")
		}
		// A different body under the same key is a conflict.
		if r := a.create("key-1", a.createBody(`,"inputs":{"region":"us"}`)); r.Code != 409 || r.errCode(t) != "idempotency_conflict" {
			t.Fatalf("changed body: %d %s", r.Code, r.Body.String())
		}
		// A new key is a rerun with a new run ID; the first run's pins are untouched.
		rerun := a.mustCreate("key-2")
		if rerun.Id == run.Id || rerun.InputSnapshot.Digest == run.InputSnapshot.Digest {
			t.Fatal("rerun reused the run or its pins")
		}
		// A replay is authorized against current permissions.
		a.e.Revoke(a.p.Scope, executiontest.Engineer)
		if r := a.create("key-1", a.createBody(`,"inputs":{"region":"ap"}`)); r.Code != 404 && r.Code != 403 {
			t.Fatalf("replay after revocation: %d %s", r.Code, r.Body.String())
		}
	})

	t.Run("strict requests", func(t *testing.T) {
		a := newAPI(t)
		for name, tc := range map[string]struct {
			key, body string
			ct        string
			status    int
			code      string
		}{
			"unknown field":      {"k1", a.createBody(`,"unexpected":1`), "application/json", 400, "invalid_request"},
			"cleanup purpose":    {"k2", a.createBody(`,"purpose":"cleanup"`), "application/json", 400, "invalid_request"},
			"deployment id":      {"k3", a.createBody(`,"deploymentId":"dep_1"`), "application/json", 400, "invalid_request"},
			"duplicate key":      {"k4", strings.Replace(a.createBody(""), `"sourceCommit"`, `"sourceCommit":"1111111111111111111111111111111111111111","sourceCommit"`, 1), "application/json", 400, "invalid_request"},
			"float input":        {"k5", a.createBody(`,"inputs":{"region":1.5}`), "application/json", 400, "invalid_request"},
			"null input":         {"k6", a.createBody(`,"inputs":{"region":null}`), "application/json", 400, "invalid_request"},
			"trailing document":  {"k7", a.createBody("") + `{}`, "application/json", 400, "invalid_request"},
			"empty body":         {"k8", ``, "application/json", 400, "invalid_request"},
			"bad commit":         {"k9", strings.Replace(a.createBody(""), executiontest.CommitA, "main", 1), "application/json", 400, "invalid_request"},
			"zero revision":      {"k10", strings.Replace(a.createBody(""), `"contentRevision":1`, `"contentRevision":0`, 1), "application/json", 400, "invalid_request"},
			"wrong content type": {"k11", a.createBody(""), "text/plain", 415, "invalid_request"},
		} {
			req := httptest.NewRequest(http.MethodPost, "/api/v1"+a.base(), strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer tok-engineer")
			req.Header.Set("Content-Type", tc.ct)
			req.Header.Set("Idempotency-Key", tc.key)
			rec := httptest.NewRecorder()
			a.router.ServeHTTP(rec, req)
			if rec.Code != tc.status || (resp{rec}).errCode(t) != tc.code {
				t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
			}
		}
		// The idempotency key is required and validated.
		if r := a.do("tok-engineer", http.MethodPost, a.base(), a.createBody("")); r.Code != 400 {
			t.Errorf("no key: %d", r.Code)
		}
		if r := a.create("bad key!", a.createBody("")); r.Code != 400 {
			t.Errorf("bad key: %d", r.Code)
		}
		if a.e.Count(`SELECT count(*) FROM runs`) != 0 {
			t.Fatal("a rejected request created a run")
		}
		// No route mutates a run other than create and cancel.
		id := "run_000000000000000000000000"
		for _, m := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete} {
			if r := a.do("tok-engineer", m, a.base()+"/"+id, "{}"); r.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s run: %d", m, r.Code)
			}
		}
		if r := a.do("tok-engineer", http.MethodGet, a.base()+"/"+id+"/events", ""); r.Code != http.StatusNotFound {
			t.Errorf("events are not served here: %d", r.Code)
		}
		if r := a.do("", http.MethodGet, a.base(), ""); r.Code != 401 {
			t.Errorf("anonymous: %d", r.Code)
		}
	})

	t.Run("get, ETags and 202 cancellation", func(t *testing.T) {
		a := newAPI(t)
		run := a.mustCreate("k1")
		got := a.do("tok-viewer", http.MethodGet, a.base()+"/"+run.Id, "")
		var current executionhttp.Run
		got.decode(t, &current)
		if got.Code != 200 || got.Header().Get("ETag") != `"`+itoa(current.Version)+`"` || current.Version < run.Version {
			t.Fatalf("get: %d etag %q version %d", got.Code, got.Header().Get("ETag"), current.Version)
		}
		if err := sp.validate(t, "Run", got.Body.Bytes()); err != nil {
			t.Fatal(err)
		}
		// A viewer reads but cannot cancel.
		if r := a.do("tok-viewer", http.MethodPost, a.base()+"/"+run.Id+"/cancel", ""); r.Code != 403 || r.errCode(t) != "forbidden" {
			t.Fatalf("viewer cancel: %d %s", r.Code, r.Body.String())
		}
		c1 := a.do("tok-engineer", http.MethodPost, a.base()+"/"+run.Id+"/cancel", "")
		if c1.Code != http.StatusAccepted {
			t.Fatalf("cancel: %d %s", c1.Code, c1.Body.String())
		}
		if err := sp.validate(t, "Run", c1.Body.Bytes()); err != nil {
			t.Fatal(err)
		}
		var cancelled executionhttp.Run
		c1.decode(t, &cancelled)
		if cancelled.CancelRequestedAt == nil || cancelled.Status != "cancelled" || cancelled.Reason != "cancelled" {
			t.Fatalf("cancelled run %+v", cancelled)
		}
		if c1.Header().Get("ETag") == got.Header().Get("ETag") {
			t.Fatal("the ETag did not move with the run's version")
		}
		// Idempotent: the same request again is 202 with the same version.
		c2 := a.do("tok-engineer", http.MethodPost, a.base()+"/"+run.Id+"/cancel", "")
		if c2.Code != http.StatusAccepted || c2.Header().Get("ETag") != c1.Header().Get("ETag") || c2.Body.String() != c1.Body.String() {
			t.Fatalf("repeat cancel: %d", c2.Code)
		}
	})

	t.Run("steps, attempts and outputs", func(t *testing.T) {
		a := newAPI(t)
		run := a.mustCreate("k1")
		in := a.e.MustClaim(a.p.Scope.TenantID)
		a.e.Start(in)
		// Before success there are no outputs and the attempt is running.
		r := a.do("tok-viewer", http.MethodGet, a.base()+"/"+run.Id+"/steps", "")
		var page executionhttp.StepPage
		r.decode(t, &page)
		if r.Code != 200 || len(page.Items) != 2 || page.Items[0].Id != "build" || page.Items[0].Outputs != nil || len(page.Items[0].Attempts) != 1 {
			t.Fatalf("steps: %d %s", r.Code, r.Body.String())
		}
		if err := sp.validate(t, "StepPage", r.Body.Bytes()); err != nil {
			t.Fatal(err)
		}
		if _, err := a.e.Succeed(in, map[string]runtimev1.Value{"label": runtimev1.StringValue("v1")}, ""); err != nil {
			t.Fatal(err)
		}
		r = a.do("tok-viewer", http.MethodGet, a.base()+"/"+run.Id+"/steps", "")
		r.decode(t, &page)
		out := page.Items[0].Outputs
		if out == nil || (*out)["label"].String() != "v1" || page.Items[0].Attempts[0].Status != "succeeded" || !page.Items[0].Attempts[0].Settled {
			t.Fatalf("after success: %s", r.Body.String())
		}
		if err := sp.validate(t, "StepPage", r.Body.Bytes()); err != nil {
			t.Fatal(err)
		}
		// A failed attempt exposes no success outputs.
		in2 := a.e.MustClaim(a.p.Scope.TenantID)
		a.e.Start(in2)
		if _, err := a.e.Fail(in2); err != nil {
			t.Fatal(err)
		}
		r = a.do("tok-viewer", http.MethodGet, a.base()+"/"+run.Id+"/steps", "")
		r.decode(t, &page)
		if page.Items[1].Status == "succeeded" || page.Items[1].Outputs != nil || page.Items[1].Attempts[0].FailureClass == nil {
			t.Fatalf("failed step: %s", r.Body.String())
		}
		// Steps paginate with a cursor bound to the run.
		p1 := a.do("tok-viewer", http.MethodGet, a.base()+"/"+run.Id+"/steps?limit=1", "")
		var first executionhttp.StepPage
		p1.decode(t, &first)
		if len(first.Items) != 1 || first.NextCursor == nil {
			t.Fatalf("page 1: %s", p1.Body.String())
		}
		p2 := a.do("tok-viewer", http.MethodGet, a.base()+"/"+run.Id+"/steps?limit=1&cursor="+*first.NextCursor, "")
		var second executionhttp.StepPage
		p2.decode(t, &second)
		if len(second.Items) != 1 || second.Items[0].Id != "test" || second.NextCursor != nil {
			t.Fatalf("page 2: %s", p2.Body.String())
		}
	})

	t.Run("pagination, filters and cursor scope", func(t *testing.T) {
		a := newAPI(t)
		ids := map[string]bool{}
		for _, k := range []string{"a", "b", "c"} {
			ids[a.mustCreate("k-"+k).Id] = true
		}
		var seen []string
		cursor := ""
		for page := 0; page < 5; page++ {
			path := a.base() + "?limit=2"
			if cursor != "" {
				path += "&cursor=" + cursor
			}
			r := a.do("tok-viewer", http.MethodGet, path, "")
			var p executionhttp.RunPage
			r.decode(t, &p)
			if r.Code != 200 || len(p.Items) > 2 {
				t.Fatalf("page %d: %d %s", page, r.Code, r.Body.String())
			}
			if err := sp.validate(t, "RunPage", r.Body.Bytes()); err != nil {
				t.Fatal(err)
			}
			for _, it := range p.Items {
				seen = append(seen, it.Id)
			}
			if p.NextCursor == nil {
				break
			}
			cursor = *p.NextCursor
		}
		if len(seen) != 3 {
			t.Fatalf("saw %v", seen)
		}
		for _, id := range seen {
			if !ids[id] {
				t.Fatalf("unknown run %s", id)
			}
			delete(ids, id)
		}
		// A cursor does not carry over to another filter, and limits are not clamped.
		first := a.do("tok-viewer", http.MethodGet, a.base()+"?limit=1", "")
		var p executionhttp.RunPage
		first.decode(t, &p)
		for name, path := range map[string]string{
			"another filter": a.base() + "?status=running&cursor=" + *p.NextCursor,
			"limit zero":     a.base() + "?limit=0", "limit too large": a.base() + "?limit=201", "bad status": a.base() + "?status=paused",
			"unknown query": a.base() + "?color=red", "bad work item": a.base() + "?workItemId=%20",
		} {
			if r := a.do("tok-viewer", http.MethodGet, path, ""); r.Code != 400 {
				t.Errorf("%s: %d %s", name, r.Code, r.Body.String())
			}
		}
		if r := a.do("tok-viewer", http.MethodGet, a.base()+"?status=cancelled", ""); r.Code != 200 || !strings.Contains(r.Body.String(), `"items":[]`) {
			t.Errorf("empty filter: %s", r.Body.String())
		}
		if r := a.do("tok-viewer", http.MethodGet, a.base()+"?workItemId="+string(a.p.Item), ""); r.Code != 200 || strings.Contains(r.Body.String(), `"items":[]`) {
			t.Errorf("work item filter: %s", r.Body.String())
		}
	})

	t.Run("safe error mapping and hidden references", func(t *testing.T) {
		a := newAPI(t)
		run := a.mustCreate("k1")
		// Foreign tenants and strangers learn nothing: the same 404 as a missing run.
		missing := a.do("tok-engineer", http.MethodGet, a.base()+"/run_missing", "")
		for _, tok := range []string{"tok-stranger", "tok-other"} {
			r := a.do(tok, http.MethodGet, a.base()+"/"+run.Id, "")
			if r.Code != 404 || r.errCode(t) != "not_found" {
				t.Errorf("%s: %d %s", tok, r.Code, r.Body.String())
			}
		}
		if missing.Code != 404 || missing.errCode(t) != "not_found" {
			t.Errorf("missing: %d", missing.Code)
		}
		other := "/tenants/" + string(executiontest.ScopeB.TenantID) + "/projects/" + string(executiontest.ScopeB.ProjectID) + "/runs/" + run.Id
		if r := a.do("tok-other", http.MethodGet, other, ""); r.Code != 404 {
			t.Errorf("another tenant's scope: %d", r.Code)
		}
		// Admission refusals are 422 and never echo submitted values.
		bad := a.create("kbad", a.createBody(`,"inputs":{"region":5}`))
		if bad.Code != 422 || bad.errCode(t) != "admission_refused" {
			t.Errorf("type mismatch: %d %s", bad.Code, bad.Body.String())
		}
		unknown := a.create("kunk", a.createBody(`,"inputs":{"SECRETISH-NAME":"hunter2"}`))
		if unknown.Code != 422 || strings.Contains(unknown.Body.String(), "hunter2") || strings.Contains(unknown.Body.String(), "SECRETISH") {
			t.Errorf("unknown input: %d %s", unknown.Code, unknown.Body.String())
		}
		stale := a.create("kstale", strings.Replace(a.createBody(""), `"contentRevision":1`, `"contentRevision":9`, 1))
		if stale.Code != 409 || stale.errCode(t) != "run_conflict" {
			t.Errorf("stale revision: %d %s", stale.Code, stale.Body.String())
		}
		if r := a.create("knodef", strings.Replace(a.createBody(""), string(a.def.ID), "pd_missing", 1)); r.Code != 404 {
			t.Errorf("unknown definition: %d", r.Code)
		}
		commit := a.create("kcommit", strings.Replace(a.createBody(""), executiontest.CommitA, executiontest.CommitC, 1))
		if commit.Code != 422 {
			t.Errorf("a commit outside the history: %d %s", commit.Code, commit.Body.String())
		}
		a.e.CancelWork(a.p.Scope, a.p.Item)
		cancelled := a.create("kcancelled", a.createBody(""))
		if cancelled.Code != 409 || cancelled.errCode(t) != "work_item_not_runnable" {
			t.Errorf("cancelled work item: %d %s", cancelled.Code, cancelled.Body.String())
		}
		// A dependency outage is 503 with Retry-After and no detail.
		a.e.CancelWorkRestore(a.p.Scope, a.p.Item)
		a.e.Sources.VerifyErr = execution.Unavailablef(errors.New("dial git.internal:443 token=LEAKED"), "the remote history is unavailable")
		out := a.create("koutage", a.createBody(""))
		if out.Code != 503 || out.Header().Get("Retry-After") == "" || out.errCode(t) != "unavailable" ||
			strings.Contains(out.Body.String(), "LEAKED") || strings.Contains(out.Body.String(), "git.internal") {
			t.Errorf("outage: %d retry-after %q: %s", out.Code, out.Header().Get("Retry-After"), out.Body.String())
		}
	})
}
