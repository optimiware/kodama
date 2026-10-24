package gateshttp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/gates/gatestest"
	gateshttp "github.com/optimiware/kodama/internal/gates/http"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

const (
	gatesYAML  = "../../../api/gates.yaml"
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
	for name, path := range map[string]string{"common.yaml": commonYAML, "gates.yaml": gatesYAML} {
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
		if name == "gates.yaml" {
			out = raw
		}
	}
	return &spec{raw: out, c: c}
}

func (s *spec) validate(t testing.TB, schema string, doc []byte) error {
	t.Helper()
	sch, err := s.c.Compile("gates.yaml#/components/schemas/" + schema)
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

// routeAuthorizer allows every route: the use cases enforce evidence:read and
// gate:decide themselves, and that is what these tests rely on.
type routeAuthorizer struct{}

func (routeAuthorizer) Authorize(context.Context, httpapi.Principal, httpapi.Action, httpapi.Target) error {
	return nil
}

type api struct {
	t      testing.TB
	e      *gatestest.Env
	router *httpapi.Router
}

func newAPI(t testing.TB) *api {
	t.Helper()
	e := gatestest.New(t)
	e.GrantAll()
	e.X.Grant(gatestest.ScopeA, "u034_revocable", identity.RoleReviewer)
	a := &api{t: t, e: e}
	rt, err := httpapi.NewRouter(httpapi.RouterConfig{
		Authenticator: tokens{"tok-reviewer": gatestest.Reviewer, "tok-author": gatestest.Author, "tok-admin": gatestest.Admin, "tok-engineer": gatestest.Engineer,
			"tok-viewer": gatestest.Viewer, "tok-stranger": "u034_stranger", "tok-other": "u034_reviewer_b", "tok-revocable": "u034_revocable"},
		Authorizer: routeAuthorizer{}, RateLimiter: httpapi.Unlimited(), Prefix: "/api/v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	codec, err := httpapi.NewCursorCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	h, err := gateshttp.NewHandler(gateshttp.Config{Service: e.Gates, Decider: e.Keyed, Cursors: codec, Clock: e.X.Clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.RegisterRoutes(rt); err != nil {
		t.Fatal(err)
	}
	a.router = rt
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

func (r resp) errEnvelope(t testing.TB) httpapi.Error {
	t.Helper()
	var e httpapi.ErrorEnvelope
	r.decode(t, &e)
	if e.Error.RequestId == "" || r.Header().Get("X-Request-ID") != e.Error.RequestId {
		t.Fatalf("error envelope without a matching request id: %s", r.Body.String())
	}
	return e.Error
}

func (r resp) errCode(t testing.TB) string { return r.errEnvelope(t).Code }

const readRun = "workspace: {access: read, from: run}"

// newGate runs a fresh run up to a requested gate and returns its API path.
func (a *api) newGate() (*gates.Gate, string) {
	a.t.Helper()
	scope := gatestest.ScopeA
	p := a.e.X.NewProject(scope)
	def := a.e.Define(scope,
		pipelinetest.Container("test", readRun, "outputs: {report: {type: artifact, path: /kodama/outputs/report.txt}}"),
		gatestest.Gate("review", []string{"test"}, "${{ steps.test.outputs.report }}"),
		pipelinetest.Container("deploy", "needs: [review]", readRun))
	run := a.e.MustAdmit(p, def, nil)
	a.t.Cleanup(func() {
		_, _ = a.e.Svc.Cancel(context.Background(), execution.Caller{PrincipalID: gatestest.Engineer}, run.Scope, run.ID)
	})
	a.e.Finish(a.e.MustClaim(scope.TenantID), "", gatestest.Output{Name: "report", Data: []byte("all tests passed\n")})
	if rep, err := a.e.Gates.Reconcile(context.Background(), scope.TenantID); err != nil || rep.Requested != 1 {
		a.t.Fatalf("reconcile: %+v %v", rep, err)
	}
	g, err := a.e.Store.ByStep(context.Background(), scope, run.ID, "review")
	if err != nil {
		a.t.Fatal(err)
	}
	return g, a.base() + "/" + string(g.ID)
}

func (a *api) base() string {
	return "/tenants/" + string(gatestest.ScopeA.TenantID) + "/projects/" + string(gatestest.ScopeA.ProjectID) + "/gates"
}

func decisionBody(g *gates.Gate, decision, reason string) string {
	return `{"decision":"` + decision + `","subjectDigest":"` + g.SubjectDigest() + `","reason":"` + reason + `"}`
}

func (a *api) decide(token, path, key, ifMatch, body string) resp {
	h := []string{}
	if key != "" {
		h = append(h, "Idempotency-Key", key)
	}
	if ifMatch != "" {
		h = append(h, "If-Match", ifMatch)
	}
	return a.do(token, http.MethodPost, path+"/decisions", body, h...)
}

// TestGateHTTPContract (DQ-03, IT-10, IT-11, DQ-14): the three routes over the
// public interface. OpenAPI examples and responses, strict decision bodies,
// ETags and preconditions, pagination, authorization, nondisclosing lookups,
// keyed replay and the error mappings match api/gates.yaml, against real
// PostgreSQL.
func TestGateHTTPContract(t *testing.T) {
	sp := loadSpec(t)

	t.Run("the OpenAPI examples are valid", func(t *testing.T) {
		if err := sp.validate(t, "Gate", sp.example(t, "PendingGate")); err != nil {
			t.Errorf("PendingGate: %v", err)
		}
		ok := `{"decision":"approve","subjectDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","reason":"Reviewed the identified source and test evidence."}`
		if err := sp.validate(t, "GateDecisionRequest", []byte(ok)); err != nil {
			t.Errorf("request example: %v", err)
		}
		for name, bad := range map[string]string{
			"actor in the body":   `{"decision":"approve","subjectDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","reason":"r","actor":"usr_x"}`,
			"subject in the body": `{"decision":"approve","subjectDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","reason":"r","subject":{}}`,
			"unknown decision":    `{"decision":"maybe","subjectDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","reason":"r"}`,
			"missing reason":      `{"decision":"approve","subjectDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
			"short digest":        `{"decision":"approve","subjectDigest":"sha256:aa","reason":"r"}`,
		} {
			if sp.validate(t, "GateDecisionRequest", []byte(bad)) == nil {
				t.Errorf("the schema accepts a request with %s", name)
			}
		}
	})

	t.Run("only the three specified routes exist", func(t *testing.T) {
		a := newAPI(t)
		_, path := a.newGate()
		for _, tc := range []struct{ method, path string }{
			{http.MethodPost, a.base()}, {http.MethodPut, path}, {http.MethodPatch, path}, {http.MethodDelete, path}, {http.MethodPost, path},
			{http.MethodGet, path + "/decisions"}, {http.MethodDelete, path + "/decisions"},
		} {
			if r := a.do("tok-reviewer", tc.method, tc.path, ""); r.Code != http.StatusMethodNotAllowed && r.Code != http.StatusNotFound {
				t.Errorf("%s %s: %d", tc.method, tc.path, r.Code)
			}
		}
		if r := a.do("", http.MethodGet, path, ""); r.Code != http.StatusUnauthorized || r.errCode(t) != "invalid_credential" {
			t.Errorf("unauthenticated read: %d", r.Code)
		}
	})

	t.Run("read, ETag and authorization", func(t *testing.T) {
		a := newAPI(t)
		g, path := a.newGate()
		r := a.do("tok-viewer", http.MethodGet, path, "")
		if r.Code != 200 || r.Header().Get("ETag") != `"1"` {
			t.Fatalf("read: %d etag %q: %s", r.Code, r.Header().Get("ETag"), r.Body.String())
		}
		if err := sp.validate(t, "Gate", r.Body.Bytes()); err != nil {
			t.Fatalf("the response violates its schema: %v\n%s", err, r.Body.String())
		}
		var dto gateshttp.Gate
		r.decode(t, &dto)
		if dto.Id != string(g.ID) || dto.Status != "pending" || dto.SubjectDigest != g.SubjectDigest() || dto.Subject.CandidateCommit != gatestest.CommitA ||
			len(dto.Subject.Evidence) != 1 || dto.Subject.DeploymentId != nil || dto.Applicability.Applicable || dto.Applicability.Expired || dto.Decision != nil {
			t.Fatalf("gate %+v", dto)
		}
		// A foreign tenant's reviewer, an unknown principal, an unknown gate and a
		// gate of another project are all the same 404.
		for name, req := range map[string]resp{
			"another tenant":  a.do("tok-other", http.MethodGet, path, ""),
			"a stranger":      a.do("tok-stranger", http.MethodGet, path, ""),
			"unknown gate":    a.do("tok-reviewer", http.MethodGet, a.base()+"/gate_unknown", ""),
			"another project": a.do("tok-reviewer", http.MethodGet, "/tenants/"+string(gatestest.ScopeA2.TenantID)+"/projects/"+string(gatestest.ScopeA2.ProjectID)+"/gates/"+string(g.ID), ""),
		} {
			if req.Code != 404 || req.errCode(t) != "not_found" {
				t.Errorf("%s: %d %s", name, req.Code, req.Body.String())
			}
		}
		if r := a.do("tok-reviewer", http.MethodGet, a.base()+"/bad%20id", ""); r.Code != 400 && r.Code != 404 {
			t.Errorf("a malformed identifier: %d", r.Code)
		}
	})

	t.Run("list pages, filters and scoped cursors", func(t *testing.T) {
		a := newAPI(t)
		var ids []string
		var firstRun string
		for i := 0; i < 3; i++ {
			g, _ := a.newGate()
			ids = append(ids, string(g.ID))
			if i == 0 {
				firstRun = string(g.RunID)
			}
		}
		page := func(q string) (gateshttp.GatePage, resp) {
			r := a.do("tok-viewer", http.MethodGet, a.base()+q, "")
			var p gateshttp.GatePage
			if r.Code == 200 {
				if err := sp.validate(t, "GatePage", r.Body.Bytes()); err != nil {
					t.Fatalf("page violates its schema: %v", err)
				}
				r.decode(t, &p)
			}
			return p, r
		}
		first, r := page("?limit=2")
		if r.Code != 200 || len(first.Items) != 2 || first.NextCursor == nil {
			t.Fatalf("first page: %d %s", r.Code, r.Body.String())
		}
		second, r := page("?limit=2&cursor=" + *first.NextCursor)
		if r.Code != 200 || len(second.Items) != 1 || second.NextCursor != nil {
			t.Fatalf("second page: %d %s", r.Code, r.Body.String())
		}
		seen := map[string]bool{}
		for _, g := range append(first.Items, second.Items...) {
			seen[g.Id] = true
		}
		for _, id := range ids {
			if !seen[id] {
				t.Fatalf("gate %s missing from the pages", id)
			}
		}
		if p, r := page("?runId=" + firstRun); r.Code != 200 || len(p.Items) != 1 || p.Items[0].RunId != firstRun {
			t.Fatalf("run filter: %d %s", r.Code, r.Body.String())
		}
		if p, r := page("?status=approved"); r.Code != 200 || len(p.Items) != 0 {
			t.Fatalf("status filter: %d %s", r.Code, r.Body.String())
		}
		// A cursor is bound to the filters and the endpoint that issued it.
		if r := a.do("tok-viewer", http.MethodGet, a.base()+"?limit=2&status=pending&cursor="+*first.NextCursor, ""); r.Code != 400 || r.errCode(t) != "invalid_cursor" {
			t.Errorf("a cursor reused under other filters: %d %s", r.Code, r.Body.String())
		}
		for name, q := range map[string]string{"unknown parameter": "?sort=desc", "bad status": "?status=expired", "limit 0": "?limit=0", "limit 201": "?limit=201", "bad run": "?runId=bad%20id"} {
			if r := a.do("tok-viewer", http.MethodGet, a.base()+q, ""); r.Code != 400 {
				t.Errorf("%s: %d", name, r.Code)
			}
		}
		if r := a.do("tok-stranger", http.MethodGet, a.base(), ""); r.Code != 404 {
			t.Errorf("a stranger listing: %d", r.Code)
		}
		if r := a.do("tok-other", http.MethodGet, a.base(), ""); r.Code != 404 {
			t.Errorf("another tenant's reviewer listing: %d", r.Code)
		}
	})

	t.Run("a decision needs its preconditions and a strict body", func(t *testing.T) {
		a := newAPI(t)
		g, path := a.newGate()
		body := decisionBody(g, "approve", "Reviewed the identified source and test evidence.")
		for name, tc := range map[string]struct {
			token, key, ifMatch, body string
			status                    int
			code                      string
		}{
			"missing If-Match":  {"tok-reviewer", "k1", "", body, 428, "version_required"},
			"weak If-Match":     {"tok-reviewer", "k1", `W/"1"`, body, 400, "invalid_request"},
			"wildcard If-Match": {"tok-reviewer", "k1", `*`, body, 400, "invalid_request"},
			"stale version":     {"tok-reviewer", "k1", `"7"`, body, 412, "version_conflict"},
			"missing key":       {"tok-reviewer", "", `"1"`, body, 400, "invalid_request"},
			"unknown field":     {"tok-reviewer", "k1", `"1"`, strings.Replace(body, "{", `{"actor":"usr_x",`, 1), 400, "invalid_request"},
			"duplicate keys":    {"tok-reviewer", "k1", `"1"`, strings.Replace(body, "{", `{"decision":"reject",`, 1), 400, "invalid_request"},
			"unknown decision":  {"tok-reviewer", "k1", `"1"`, strings.Replace(body, `"approve"`, `"maybe"`, 1), 400, "invalid_request"},
			"blank reason":      {"tok-reviewer", "k1", `"1"`, decisionBody(g, "approve", "  "), 400, "invalid_request"},
			"an empty body":     {"tok-reviewer", "k1", `"1"`, ``, 415, "invalid_request"},
			"wrong digest":      {"tok-reviewer", "k1", `"1"`, strings.Replace(body, g.SubjectDigest(), digestOfZero, 1), 409, "stale_gate_subject"},
			"an admin":          {"tok-admin", "k1", `"1"`, body, 403, "forbidden"},
			"an engineer":       {"tok-engineer", "k1", `"1"`, body, 403, "forbidden"},
			"a viewer":          {"tok-viewer", "k1", `"1"`, body, 403, "forbidden"},
			"a stranger":        {"tok-stranger", "k1", `"1"`, body, 404, "not_found"},
			"another tenant":    {"tok-other", "k1", `"1"`, body, 404, "not_found"},
			"no credential":     {"", "k1", `"1"`, body, 401, "invalid_credential"},
		} {
			r := a.decide(tc.token, path, tc.key, tc.ifMatch, tc.body)
			if r.Code != tc.status {
				t.Errorf("%s: %d %s", name, r.Code, r.Body.String())
				continue
			}
			if got := r.errCode(t); got != tc.code {
				t.Errorf("%s: code %s, want %s", name, got, tc.code)
			}
		}
		if r := a.decide("tok-reviewer", path, "k1", `"1"`, strings.Replace(body, g.SubjectDigest(), digestOfZero, 1)); r.errEnvelope(t).Details["reason"] != "subject_digest_mismatch" {
			t.Errorf("stale details: %s", r.Body.String())
		}
		cur, _ := a.e.Store.Get(context.Background(), g.Scope, g.ID)
		if cur.Status != gates.StatusPending || cur.Version != 1 {
			t.Fatalf("a refused request changed the gate: %+v", cur)
		}
	})

	t.Run("a decision, its replay and its immutability", func(t *testing.T) {
		a := newAPI(t)
		g, path := a.newGate()
		body := decisionBody(g, "approve", "Reviewed the identified source and test evidence.")
		first := a.decide("tok-author", path, "key-1", `"1"`, body)
		if first.Code != 200 || first.Header().Get("ETag") != `"2"` {
			t.Fatalf("decide: %d etag %q: %s", first.Code, first.Header().Get("ETag"), first.Body.String())
		}
		if err := sp.validate(t, "Gate", first.Body.Bytes()); err != nil {
			t.Fatalf("response violates its schema: %v\n%s", err, first.Body.String())
		}
		var dto gateshttp.Gate
		first.decode(t, &dto)
		if dto.Status != "approved" || dto.Decision == nil || dto.Decision.DecidedBy.Id != gatestest.Author || dto.Decision.DecidedBy.Type != "user" ||
			dto.Decision.Decision != "approve" || !dto.Applicability.Applicable || dto.Version != 2 {
			t.Fatalf("decided gate %+v %+v", dto, dto.Decision)
		}
		// A lost response is recovered with the same key and the same request.
		again := a.decide("tok-author", path, "key-1", `"1"`, body)
		if again.Code != 200 || again.Header().Get("Idempotent-Replayed") != "true" || again.Body.String() != first.Body.String() {
			t.Fatalf("replay: %d replayed=%q", again.Code, again.Header().Get("Idempotent-Replayed"))
		}
		if n := a.e.X.Count(`SELECT count(*) FROM audit WHERE action = 'gate.decided' AND resource_id = $1`, string(g.ID)); n != 1 {
			t.Fatalf("%d decision audit rows", n)
		}
		// A different body under the used key conflicts.
		if r := a.decide("tok-author", path, "key-1", `"1"`, decisionBody(g, "reject", "second thoughts")); r.Code != 409 || r.errCode(t) != "idempotency_conflict" {
			t.Fatalf("changed body: %d %s", r.Code, r.Body.String())
		}
		// Another reviewer with the new version cannot decide a decided gate.
		if r := a.decide("tok-reviewer", path, "key-2", `"2"`, decisionBody(g, "reject", "second thoughts")); r.Code != 422 || r.errCode(t) != "invalid_transition" {
			t.Fatalf("second decision: %d %s", r.Code, r.Body.String())
		}
		// The stale version of a concurrent decision loses.
		if r := a.decide("tok-reviewer", path, "key-3", `"1"`, decisionBody(g, "reject", "late")); r.Code != 412 || r.errCode(t) != "version_conflict" {
			t.Fatalf("stale concurrent decision: %d %s", r.Code, r.Body.String())
		}
		got := a.do("tok-viewer", http.MethodGet, path, "")
		var after gateshttp.Gate
		got.decode(t, &after)
		if after.Status != "approved" || after.Decision.DecidedBy.Id != gatestest.Author || got.Header().Get("ETag") != `"2"` {
			t.Fatalf("the decision changed: %s", got.Body.String())
		}
		// Replay reauthorizes with the current permission: a revoked reviewer
		// gets nothing back.
		g2, path2 := a.newGate()
		body2 := decisionBody(g2, "reject", "The evidence does not support the change.")
		if r := a.decide("tok-revocable", path2, "key-r", `"1"`, body2); r.Code != 200 {
			t.Fatalf("decide: %d %s", r.Code, r.Body.String())
		}
		a.e.X.Revoke(gatestest.ScopeA, "u034_revocable")
		if r := a.decide("tok-revocable", path2, "key-r", `"1"`, body2); r.Code != 404 && r.Code != 403 || strings.Contains(r.Body.String(), `"status"`) {
			t.Fatalf("replay after revocation: %d %s", r.Code, r.Body.String())
		}
	})

	t.Run("stale, expired and blocked subjects", func(t *testing.T) {
		a := newAPI(t)
		// Content changed since the gate was requested.
		g, path := a.newGate()
		item := g.Subject.WorkItemID()
		a.e.X.EditSpec(gatestest.ScopeA, item, "# Plan\nA changed requirement.")
		r := a.decide("tok-reviewer", path, "k-stale", `"1"`, decisionBody(g, "approve", "Looks fine."))
		if r.Code != 409 || r.errEnvelope(t).Code != "stale_gate_subject" || r.errEnvelope(t).Details["reason"] != "content_changed" {
			t.Fatalf("changed content: %d %s", r.Code, r.Body.String())
		}
		var dto gateshttp.Gate
		a.do("tok-viewer", http.MethodGet, path, "").decode(t, &dto)
		if dto.Invalidation == nil || dto.Invalidation.Reason != "content_changed" || dto.Status != "pending" || dto.Applicability.Applicable || dto.Applicability.Reason == nil {
			t.Fatalf("invalidation not visible: %+v", dto)
		}

		// Expiry is derived from the deadline, never a status.
		g2, path2 := a.newGate()
		a.e.X.Clock.Advance(2 * time.Hour)
		defer a.e.X.Clock.Advance(-2 * time.Hour)
		r = a.decide("tok-reviewer", path2, "k-exp", `"1"`, decisionBody(g2, "approve", "Too late."))
		if r.Code != 409 || r.errEnvelope(t).Details["reason"] != "expired" {
			t.Fatalf("expired gate: %d %s", r.Code, r.Body.String())
		}
		var expired gateshttp.Gate
		a.do("tok-viewer", http.MethodGet, path2, "").decode(t, &expired)
		if expired.Status != "pending" || !expired.Applicability.Expired || expired.Invalidation != nil {
			t.Fatalf("expired gate %+v", expired)
		}
	})

	t.Run("conformance denials map to 422 with their code", func(t *testing.T) {
		a := newAPI(t)
		scope := gatestest.ScopeA
		base := gatestest.NewBaseline(t, "module-boundaries")
		a.e.SetBaseline(&base)
		a.e.X.Sources.History[gatestest.CommitB] = true
		p := a.e.X.NewProject(scope)
		def := a.e.Define(scope, gatestest.Agent("implement"), gatestest.Gate("review", []string{"implement"}))
		run := a.e.MustAdmit(p, def, nil)
		a.t.Cleanup(func() {
			_, _ = a.e.Svc.Cancel(context.Background(), execution.Caller{PrincipalID: gatestest.Engineer}, run.Scope, run.ID)
		})
		a.e.Finish(a.e.MustClaim(scope.TenantID), gatestest.CommitB)
		if rep, err := a.e.Gates.Reconcile(context.Background(), scope.TenantID); err != nil || rep.Requested != 1 {
			t.Fatalf("reconcile: %+v %v", rep, err)
		}
		g, err := a.e.Store.ByStep(context.Background(), scope, run.ID, "review")
		if err != nil {
			t.Fatal(err)
		}
		path := a.base() + "/" + string(g.ID)
		var dto gateshttp.Gate
		a.do("tok-viewer", http.MethodGet, path, "").decode(t, &dto)
		if dto.Required.Conformance == nil || dto.Required.Conformance.Status != "blocked" || dto.Required.Conformance.Code == nil || *dto.Required.Conformance.Code != gates.CodeConformanceMissing {
			t.Fatalf("conformance snapshot %+v", dto.Required.Conformance)
		}
		r := a.decide("tok-reviewer", path, "k-conf", `"1"`, decisionBody(g, "approve", "The report is missing but I approve."))
		if r.Code != 422 || r.errCode(t) != gates.CodeConformanceMissing {
			t.Fatalf("approving blocked evidence: %d %s", r.Code, r.Body.String())
		}
		// Rejection is always possible and preserves the evidence.
		r = a.decide("tok-reviewer", path, "k-rej", `"1"`, decisionBody(g, "reject", "Rejected: no conformance evidence."))
		if r.Code != 200 {
			t.Fatalf("rejecting blocked evidence: %d %s", r.Code, r.Body.String())
		}
	})
}

const digestOfZero = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
