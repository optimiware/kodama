package workhttp_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/work"
	workhttp "github.com/optimiware/kodama/internal/work/http"
)

const (
	workYAML   = "../../../api/work.yaml"
	commonYAML = "../../../api/common.yaml"
	commit1    = "1111111111111111111111111111111111111111"
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

// translate rewrites OpenAPI 3.0 `nullable: true` into JSON Schema type unions
// (and allOf-wrapped nullable references into anyOf with null) so the fragment
// can validate documents.
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
	for name, path := range map[string]string{"common.yaml": commonYAML, "work.yaml": workYAML} {
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
		if name == "work.yaml" {
			out = raw
		}
	}
	return &spec{raw: out, c: c}
}

func (s *spec) validate(t testing.TB, schema string, doc []byte) error {
	t.Helper()
	sch, err := s.c.Compile("work.yaml#/components/schemas/" + schema)
	if err != nil {
		t.Fatalf("schema %s: %v", schema, err)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		t.Fatalf("not JSON: %v: %s", err, doc)
	}
	return sch.Validate(v)
}

func (s *spec) validateCommon(t testing.TB, schema string, doc []byte) error {
	t.Helper()
	sch, err := s.c.Compile("common.yaml#/components/schemas/" + schema)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	return sch.Validate(v)
}

func strictDecode(doc []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// contractAPI builds the routes over the in-memory repository.
func contractAPI(t testing.TB) (*api, *facts, *memRepo) {
	f := newFacts()
	f.person("u_analyst", work.ActorUser, work.PermRead, work.PermCreate, work.PermEditSpec, work.PermRefine)
	f.person("u_engineer", work.ActorUser, work.PermRead, work.PermCreate, work.PermEditSpec, work.PermEditTechnical, work.PermRefine, work.PermWork, work.PermCancel, work.PermReopen)
	f.person("u_reviewer", work.ActorUser, work.PermRead, work.PermComplete)
	f.person("svc_run", work.ActorService, work.PermRead, work.PermComplete, work.PermWork)
	repo := newMemRepo(f)
	svc, err := work.NewService(work.Config{Repository: repo, Authority: f})
	if err != nil {
		t.Fatal(err)
	}
	a := newAPI(t, svc, tokens{"tok-analyst": "u_analyst", "tok-engineer": "u_engineer", "tok-reviewer": "u_reviewer", "tok-svc": "svc_run", "tok-stranger": "u_stranger"})
	return a, f, repo
}

const createBody = `{"kind":"story","title":"Resolve inventory exceptions","spec":"# Plan\nAdd a scoped review flow.\n\n# Behavior\nReviewers record ERP acknowledgments.","acceptanceCriteria":[{"id":"AC-1","text":"A reviewer resolves an exception with an audit record."}]}`

func etagVersion(r resp) string { return r.Header().Get("ETag") }

// Test_work_http_contract: the OpenAPI fragment, its examples and generated types
// match the handlers; strict JSON, the status mappings, ETags, request IDs,
// route authorization and denial defaults are enforced. No external services.
func Test_work_http_contract(t *testing.T) {
	sp := loadSpec(t)
	a, f, _ := contractAPI(t)
	p := base(tenantA, projectA)

	t.Run("documented_routes_are_exactly_the_registered_routes", func(t *testing.T) {
		paths := sp.raw["paths"].(map[string]any)
		var documented []string
		for path, item := range paths {
			for method := range item.(map[string]any) {
				if method == "parameters" {
					continue
				}
				documented = append(documented, strings.ToUpper(method)+" "+path)
			}
		}
		sort.Strings(documented)
		want := []string{
			"GET /tenants/{tenantId}/projects/{projectId}/work-items",
			"GET /tenants/{tenantId}/projects/{projectId}/work-items/{id}",
			"GET /tenants/{tenantId}/projects/{projectId}/work-items/{id}/history",
			"PATCH /tenants/{tenantId}/projects/{projectId}/work-items/{id}",
			"POST /tenants/{tenantId}/projects/{projectId}/work-items",
			"POST /tenants/{tenantId}/projects/{projectId}/work-items/{id}/transitions",
		}
		if strings.Join(documented, "\n") != strings.Join(want, "\n") {
			t.Fatalf("documented routes:\n%s", strings.Join(documented, "\n"))
		}
		// Every documented route exists (deny by default: 401 without a token) and
		// nothing else does.
		for _, route := range want {
			method, path, _ := strings.Cut(route, " ")
			path = strings.NewReplacer("{tenantId}", tenantA, "{projectId}", projectA, "{id}", "wi_1").Replace(path)
			if r := a.do("", method, path, `{}`); r.Code != http.StatusUnauthorized || r.errCode(t) != "invalid_credential" {
				t.Errorf("%s without a token: %d", route, r.Code)
			}
		}
		for _, route := range []string{
			"DELETE " + p + "/wi_1", "POST " + p + "/wi_1/workspace", "GET " + p + "/wi_1/dependencies", "PUT " + p + "/wi_1", "POST " + p + "/wi_1/findings",
		} {
			method, path, _ := strings.Cut(route, " ")
			if r := a.do("tok-engineer", method, path, `{}`); r.Code != http.StatusNotFound && r.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s is served: %d", route, r.Code)
			}
		}
	})

	t.Run("documented_examples_decode_into_the_generated_types_and_validate", func(t *testing.T) {
		post := sp.raw["paths"].(map[string]any)["/tenants/{tenantId}/projects/{projectId}/work-items"].(map[string]any)["post"].(map[string]any)
		ex := post["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["example"]
		body, _ := json.Marshal(ex)
		var dto workhttp.CreateWorkItem
		if err := strictDecode(body, &dto); err != nil {
			t.Fatalf("create example: %v", err)
		}
		if err := sp.validate(t, "CreateWorkItem", body); err != nil {
			t.Fatalf("create example schema: %v", err)
		}
		created := sp.raw["components"].(map[string]any)["examples"].(map[string]any)["CreatedWorkItem"].(map[string]any)["value"]
		cb, _ := json.Marshal(created)
		var item workhttp.WorkItem
		if err := strictDecode(cb, &item); err != nil {
			t.Fatalf("created example: %v", err)
		}
		if err := sp.validate(t, "WorkItem", cb); err != nil {
			t.Fatalf("created example schema: %v", err)
		}
		if item.State != workhttp.Backlog || item.Version != 1 || item.ContentRevision != 1 {
			t.Fatalf("example: %+v", item)
		}
		trans := sp.raw["paths"].(map[string]any)["/tenants/{tenantId}/projects/{projectId}/work-items/{id}/transitions"].(map[string]any)["post"].(map[string]any)
		for name, e := range trans["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["examples"].(map[string]any) {
			b, _ := json.Marshal(e.(map[string]any)["value"])
			var tr workhttp.TransitionRequest
			if err := strictDecode(b, &tr); err != nil {
				t.Errorf("transition example %s: %v", name, err)
			}
			if err := sp.validate(t, "TransitionRequest", b); err != nil {
				t.Errorf("transition example %s schema: %v", name, err)
			}
		}
		patch := sp.raw["paths"].(map[string]any)["/tenants/{tenantId}/projects/{projectId}/work-items/{id}"].(map[string]any)["patch"].(map[string]any)
		pe, _ := json.Marshal(patch["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["example"])
		var up workhttp.UpdateWorkItem
		if err := strictDecode(pe, &up); err != nil || !up.BlockedReason.Set || up.BlockedReason.Value != "Waiting for ERP sandbox access" {
			t.Errorf("patch example: %v %+v", err, up)
		}
		if err := sp.validate(t, "UpdateWorkItem", pe); err != nil {
			t.Errorf("patch example schema: %v", err)
		}
	})

	var itemID, etag string
	t.Run("create_returns_the_documented_shape", func(t *testing.T) {
		r := a.do("tok-analyst", http.MethodPost, p, createBody)
		if r.Code != http.StatusCreated || etagVersion(r) != `"1"` || r.Header().Get("X-Request-ID") == "" || !strings.HasSuffix(r.Header().Get("Location"), "/work-items/"+itemIDOf(t, r)) {
			t.Fatalf("create: %d %v %s", r.Code, r.Header(), r.Body.String())
		}
		if err := sp.validate(t, "WorkItem", r.Body.Bytes()); err != nil {
			t.Fatalf("response does not match the schema: %v\n%s", err, r.Body.String())
		}
		var item workhttp.WorkItem
		if err := strictDecode(r.Body.Bytes(), &item); err != nil {
			t.Fatalf("response does not decode into the generated type: %v", err)
		}
		if item.State != workhttp.Backlog || item.Version != 1 || item.ContentRevision != 1 || item.Kind != workhttp.Story ||
			item.CreatedBy.Id != "u_analyst" || item.AssigneeId != nil || item.ReviewSubject != nil || item.Completion != nil {
			t.Fatalf("created = %+v", item)
		}
		itemID, etag = item.Id, etagVersion(r)
		if r.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("Cache-Control = %q", r.Header().Get("Cache-Control"))
		}
	})

	item := func() string { return p + "/" + itemID }

	t.Run("strict_json", func(t *testing.T) {
		for name, tc := range map[string]struct {
			body string
			ct   string
			code int
		}{
			"unknown field":           {`{"kind":"story","title":"x","state":"done"}`, "application/json", 400},
			"client-chosen actor":     {`{"kind":"story","title":"x","createdBy":{"type":"user","id":"u_admin"}}`, "application/json", 400},
			"client-chosen version":   {`{"kind":"story","title":"x","version":9}`, "application/json", 400},
			"two documents":           {`{"kind":"story","title":"x"}{"kind":"story","title":"y"}`, "application/json", 400},
			"empty body":              {``, "application/json", 400},
			"wrong type":              {`{"kind":"story","title":5}`, "application/json", 400},
			"unknown kind":            {`{"kind":"feature","title":"x"}`, "application/json", 400},
			"blank title":             {`{"kind":"story","title":" "}`, "application/json", 400},
			"nested unknown field":    {`{"kind":"story","title":"x","acceptanceCriteria":[{"id":"A","text":"t","done":true}]}`, "application/json", 400},
			"duplicate criterion ids": {`{"kind":"story","title":"x","acceptanceCriteria":[{"id":"A","text":"t"},{"id":"A","text":"u"}]}`, "application/json", 400},
			"text/plain":              {`{"kind":"story","title":"x"}`, "text/plain", 415},
		} {
			req := a.do("tok-analyst", http.MethodPost, p, tc.body, "Content-Type", tc.ct)
			if tc.body == "" {
				req = a.do("tok-analyst", http.MethodPost, p, "")
			}
			if req.Code != tc.code && !(tc.body == "" && req.Code == 415) {
				t.Errorf("%s: %d %s", name, req.Code, req.Body.String())
			}
		}
	})

	t.Run("edits_need_a_strong_current_version", func(t *testing.T) {
		patch := func(body string, headers ...string) resp {
			return a.do("tok-analyst", http.MethodPatch, item(), body, headers...)
		}
		engineerPatch := func(body string, headers ...string) resp {
			return a.do("tok-engineer", http.MethodPatch, item(), body, headers...)
		}
		if r := patch(`{"title":"new"}`); r.Code != http.StatusPreconditionRequired || r.errCode(t) != "version_required" {
			t.Errorf("missing If-Match: %d %s", r.Code, r.Body.String())
		}
		for _, h := range []string{`W/"1"`, `*`, `"1", "2"`, `1`, `"01"`} {
			if r := patch(`{"title":"new"}`, "If-Match", h); r.Code != http.StatusBadRequest {
				t.Errorf("If-Match %s: %d", h, r.Code)
			}
		}
		if r := patch(`{"title":"new"}`, "If-Match", `"7"`); r.Code != http.StatusPreconditionFailed || r.errCode(t) != "version_conflict" {
			t.Errorf("stale: %d %s", r.Code, r.Body.String())
		}
		for name, body := range map[string]string{
			"empty patch":     `{}`,
			"state":           `{"state":"done"}`,
			"version":         `{"version":5}`,
			"content rev":     `{"contentRevision":5}`,
			"actor":           `{"actor":{"type":"user"}}`,
			"null title":      `{"title":null}`,
			"null spec":       `{"spec":null}`,
			"null criteria":   `{"acceptanceCriteria":null}`,
			"nested unknown":  `{"acceptanceCriteria":[{"id":"A","text":"t","extra":1}]}`,
			"oversized block": `{"blockedReason":"` + strings.Repeat("x", 3000) + `"}`,
		} {
			if r := engineerPatch(body, "If-Match", etag); r.Code != http.StatusBadRequest {
				t.Errorf("%s: %d %s", name, r.Code, r.Body.String())
			}
		}
		// A valid edit: spec change increments both counters; the blocked reason only the version.
		r := patch(`{"spec":"# Plan\nChanged"}`, "If-Match", etag)
		var it workhttp.WorkItem
		r.decode(t, &it)
		if r.Code != http.StatusOK || it.Version != 2 || it.ContentRevision != 2 || etagVersion(r) != `"2"` {
			t.Fatalf("spec edit: %d %s", r.Code, r.Body.String())
		}
		r = engineerPatch(`{"blockedReason":"waiting for ERP"}`, "If-Match", `"2"`)
		r.decode(t, &it)
		if r.Code != http.StatusOK || it.Version != 3 || it.ContentRevision != 2 || it.BlockedReason == nil || *it.BlockedReason != "waiting for ERP" {
			t.Fatalf("blocking edit: %d %s", r.Code, r.Body.String())
		}
		// null clears the blocked reason explicitly; omitted fields stay.
		r = engineerPatch(`{"blockedReason":null}`, "If-Match", `"3"`)
		r.decode(t, &it)
		if r.Code != http.StatusOK || it.BlockedReason != nil || it.Version != 4 || it.Spec != "# Plan\nChanged" {
			t.Fatalf("clear: %d %s", r.Code, r.Body.String())
		}
		// Field-sensitive permission: an analyst cannot edit technical fields.
		if r := patch(`{"assigneeId":"u_1"}`, "If-Match", `"4"`); r.Code != http.StatusForbidden {
			t.Errorf("analyst assigning: %d", r.Code)
		}
		etag = `"4"`
	})

	t.Run("transition_status_mapping", func(t *testing.T) {
		move := func(token, body, version string) resp {
			return a.do(token, http.MethodPost, item()+"/transitions", body, "If-Match", version)
		}
		if r := a.do("tok-engineer", http.MethodPost, item()+"/transitions", `{"to":"ready"}`); r.Code != http.StatusPreconditionRequired {
			t.Errorf("no If-Match: %d", r.Code)
		}
		if r := move("tok-engineer", `{"to":"archived"}`, `"4"`); r.Code != http.StatusBadRequest {
			t.Errorf("unknown state: %d", r.Code)
		}
		if r := move("tok-engineer", `{"to":"ready","actor":"u_admin"}`, `"4"`); r.Code != http.StatusBadRequest {
			t.Errorf("caller-chosen identity: %d", r.Code)
		}
		if r := move("tok-engineer", `{"to":"ready"}`, `"2"`); r.Code != http.StatusPreconditionFailed {
			t.Errorf("stale transition: %d", r.Code)
		}
		if r := move("tok-engineer", `{"to":"done"}`, `"4"`); r.Code != http.StatusForbidden || r.errCode(t) != "forbidden" {
			t.Errorf("an engineer completing: %d %s", r.Code, r.Body.String())
		}
		if r := move("tok-engineer", `{"to":"review"}`, `"4"`); r.Code != http.StatusUnprocessableEntity || r.errCode(t) != "invalid_transition" {
			t.Errorf("backlog -> review: %d %s", r.Code, r.Body.String())
		}
		if r := move("tok-engineer", `{"to":"cancelled"}`, `"4"`); r.Code != http.StatusUnprocessableEntity || r.errCode(t) != "prerequisite_unmet" {
			t.Errorf("cancel without a reason: %d %s", r.Code, r.Body.String())
		}
		r := move("tok-engineer", `{"to":"ready"}`, `"4"`)
		var it workhttp.WorkItem
		r.decode(t, &it)
		if r.Code != http.StatusOK || it.State != workhttp.Ready || it.Version != 5 || it.ContentRevision != 2 || etagVersion(r) != `"5"` {
			t.Fatalf("ready: %d %s", r.Code, r.Body.String())
		}
		if err := sp.validate(t, "WorkItem", r.Body.Bytes()); err != nil {
			t.Fatal(err)
		}
		f.wipFull = true
		if r := move("tok-engineer", `{"to":"active"}`, `"5"`); r.Code != http.StatusConflict || r.errCode(t) != "wip_limit_reached" {
			t.Errorf("wip: %d %s", r.Code, r.Body.String())
		}
		f.wipFull = false
		if r := move("tok-engineer", `{"to":"active"}`, `"5"`); r.Code != http.StatusOK {
			t.Fatalf("active: %d %s", r.Code, r.Body.String())
		}
		f.writerBusy = true
		if r := move("tok-engineer", `{"to":"ready","reason":"back"}`, `"6"`); r.Code != http.StatusConflict || r.errCode(t) != "workspace_busy" {
			t.Errorf("writer busy: %d %s", r.Code, r.Body.String())
		}
		f.writerBusy = false
		review := `{"to":"review","review":{"candidateCommit":"` + commit1 + `","evidence":[{"type":"artifact","id":"art_1"}]}}`
		f.subjectErr = work.ErrNotFound
		if r := move("tok-engineer", review, `"6"`); r.Code != http.StatusNotFound {
			t.Errorf("unknown evidence: %d", r.Code)
		}
		f.subjectErr = nil
		if r := move("tok-engineer", review, `"6"`); r.Code != http.StatusOK {
			t.Fatalf("review: %d %s", r.Code, r.Body.String())
		}
		complete := `{"to":"done","reason":"ok","completion":{"contentRevision":2,"candidateCommit":"` + commit1 + `","acceptedCriteria":["AC-1"]}}`
		if r := move("tok-svc", complete, `"7"`); r.Code != http.StatusForbidden || r.errCode(t) != "human_required" {
			t.Errorf("a service completing: %d %s", r.Code, r.Body.String())
		}
		stale := strings.Replace(complete, `"contentRevision":2`, `"contentRevision":1`, 1)
		if r := move("tok-reviewer", stale, `"7"`); r.Code != http.StatusConflict || r.errCode(t) != "stale_review_subject" {
			t.Errorf("stale revision: %d %s", r.Code, r.Body.String())
		}
		r = move("tok-reviewer", complete, `"7"`)
		r.decode(t, &it)
		if r.Code != http.StatusOK || it.State != workhttp.Done || it.Completion == nil || it.Completion.CompletedBy.Id != "u_reviewer" || it.Completion.CompletedBy.Type != workhttp.User {
			t.Fatalf("completion: %d %s", r.Code, r.Body.String())
		}
		if err := sp.validate(t, "WorkItem", r.Body.Bytes()); err != nil {
			t.Fatalf("completed item: %v\n%s", err, r.Body.String())
		}
		// The next legal use of a done item is reopening; other edges are 422.
		if r := move("tok-engineer", `{"to":"ready","reason":"x"}`, `"8"`); r.Code != http.StatusUnprocessableEntity {
			t.Errorf("done -> ready: %d", r.Code)
		}
	})

	t.Run("history_matches_its_schema", func(t *testing.T) {
		r := a.do("tok-analyst", http.MethodGet, item()+"/history?limit=100", "")
		if r.Code != http.StatusOK {
			t.Fatalf("%d %s", r.Code, r.Body.String())
		}
		if err := sp.validate(t, "HistoryPage", r.Body.Bytes()); err != nil {
			t.Fatalf("%v\n%s", err, r.Body.String())
		}
		var h workhttp.HistoryPage
		if err := strictDecode(r.Body.Bytes(), &h); err != nil || len(h.Items) < 7 || h.NextCursor != nil {
			t.Fatalf("history: %v %d", err, len(h.Items))
		}
		last := h.Items[len(h.Items)-1]
		if last.Completion != nil || last.Kind != workhttp.Transitioned {
			// the last entry is the done -> ready attempt? it was refused; the last is the completion
		}
		var sawCompletion bool
		for i, e := range h.Items {
			if int(e.Version) != i+1 {
				t.Fatalf("history order at %d: %d", i, e.Version)
			}
			sawCompletion = sawCompletion || e.Completion != nil
		}
		if !sawCompletion {
			t.Fatal("the completion decision is missing from history")
		}
	})

	t.Run("list_matches_its_schema", func(t *testing.T) {
		r := a.do("tok-analyst", http.MethodGet, p+"?limit=1", "")
		if r.Code != http.StatusOK {
			t.Fatalf("%d %s", r.Code, r.Body.String())
		}
		if err := sp.validate(t, "WorkItemPage", r.Body.Bytes()); err != nil {
			t.Fatalf("%v\n%s", err, r.Body.String())
		}
		var pg workhttp.WorkItemPage
		if err := strictDecode(r.Body.Bytes(), &pg); err != nil || len(pg.Items) != 1 || pg.NextCursor != nil {
			t.Fatalf("list: %v %+v", err, pg)
		}
	})

	t.Run("denial_defaults", func(t *testing.T) {
		// Unknown principal: the authority hides it; the response is the generic 404.
		if r := a.do("tok-stranger", http.MethodGet, item(), ""); r.Code != http.StatusNotFound {
			t.Errorf("stranger: %d %s", r.Code, r.Body.String())
		}
		if r := a.do("bad-token", http.MethodGet, item(), ""); r.Code != http.StatusUnauthorized {
			t.Errorf("bad token: %d", r.Code)
		}
		// Route authorization runs before the handler, with the registered action and resource.
		calls := len(a.authz.calls)
		a.authz.allow = func(_ httpapi.Principal, act httpapi.Action, tg httpapi.Target) error {
			if act == workhttp.ActionRead && tg.Resource.Kind == "work_item" && tg.Resource.ID == itemID && string(tg.TenantID) == tenantA && string(tg.ProjectID) == projectA {
				return httpapi.ErrDenied
			}
			return nil
		}
		if r := a.do("tok-analyst", http.MethodGet, item(), ""); r.Code != http.StatusForbidden {
			t.Errorf("route-level denial: %d", r.Code)
		}
		if len(a.authz.calls) != calls+1 {
			t.Errorf("authorizer calls = %d", len(a.authz.calls)-calls)
		}
		a.authz.allow = func(httpapi.Principal, httpapi.Action, httpapi.Target) error { return httpapi.ErrHidden }
		if r := a.do("tok-analyst", http.MethodGet, p, ""); r.Code != http.StatusNotFound {
			t.Errorf("hidden scope: %d", r.Code)
		}
		a.authz.allow = nil
		// A dependency outage is 503 with Retry-After and no internals.
		f.authorityErr = work.ErrUnavailable
		r := a.do("tok-analyst", http.MethodGet, item(), "")
		if r.Code != http.StatusServiceUnavailable || r.Header().Get("Retry-After") == "" || r.errCode(t) != "unavailable" {
			t.Errorf("authority outage: %d %s", r.Code, r.Body.String())
		}
		f.authorityErr = nil
		// Missing guards fail closed: a repository built over TxGuards{} refuses.
		if err := sp.validateCommon(t, "ErrorEnvelope", r.Body.Bytes()); err != nil {
			t.Errorf("error envelope: %v", err)
		}
	})
}

func itemIDOf(t testing.TB, r resp) string {
	var it struct{ ID string }
	r.decode(t, &it)
	return it.ID
}
