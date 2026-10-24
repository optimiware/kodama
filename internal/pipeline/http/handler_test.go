package pipelinehttp_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/pipeline"
	pipelinehttp "github.com/optimiware/kodama/internal/pipeline/http"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

const (
	pipelineYAML = "../../../api/pipeline.yaml"
	commonYAML   = "../../../api/common.yaml"
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

// translate rewrites OpenAPI 3.0 `nullable: true` into JSON Schema unions so
// the fragment can validate documents.
func translate(n any) {
	switch v := n.(type) {
	case map[string]any:
		if nb, _ := v["nullable"].(bool); nb {
			if ty, ok := v["type"].(string); ok {
				v["type"] = []any{ty, "null"}
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
	for name, path := range map[string]string{"common.yaml": commonYAML, "pipeline.yaml": pipelineYAML} {
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
		if name == "pipeline.yaml" {
			out = raw
		}
	}
	return &spec{raw: out, c: c}
}

func (s *spec) validate(t testing.TB, resource, schema string, doc []byte) error {
	t.Helper()
	sch, err := s.c.Compile(resource + "#/components/schemas/" + schema)
	if err != nil {
		t.Fatalf("schema %s: %v", schema, err)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		t.Fatalf("not JSON: %v: %s", err, doc)
	}
	return sch.Validate(v)
}

func register(a *api, token, tenant, project, body string) resp {
	return a.do(token, http.MethodPost, base(tenant, project), body)
}

// Test_pipeline_http_contract: 201/200 registration, ETag and Location, shared
// validator parity, denial after revocation, error envelopes, and absence of
// mutation and validation-only routes. PX-01, PX-02, PX-14, AC-12, AC-A09.
func Test_pipeline_http_contract(t *testing.T) {
	t.Run("new version is 201 and identical content is 200", func(t *testing.T) {
		a := newAPI(t)
		body := jsonDoc("unit-tests", jsonStep("test", ""))
		r1 := register(a, "tok-engineer", tenantA, projectA, body)
		if r1.Code != http.StatusCreated {
			t.Fatalf("first registration: %d %s", r1.Code, r1.Body.String())
		}
		var d1 definitionBody
		r1.decode(t, &d1)
		if d1.Version != 1 || d1.Name != "unit-tests" || !strings.HasPrefix(d1.Digest, "sha256:") || d1.CreatedBy.ID != "u_engineer" || d1.CreatedBy.Type != "user" {
			t.Fatalf("unexpected definition: %+v", d1)
		}
		if got, want := r1.Header().Get("ETag"), `"1"`; got != want {
			t.Fatalf("ETag = %q, want %q", got, want)
		}
		wantLoc := "/api/v1" + base(tenantA, projectA) + "/" + d1.ID
		if got := r1.Header().Get("Location"); got != wantLoc {
			t.Fatalf("Location = %q, want %q", got, wantLoc)
		}
		if got := pipeline.DigestOf(d1.Definition); string(got) != d1.Digest {
			t.Fatalf("the digest does not cover the returned definition: %s vs %s", got, d1.Digest)
		}

		// Reordered keys and whitespace are the same canonical content.
		reordered := `{ "spec": {"steps": [ ` + jsonStep("test", "") + ` ]}, "metadata": {"name": "unit-tests"}, "kind": "Pipeline", "apiVersion": "kodama/v1alpha1" }`
		r2 := register(a, "tok-engineer", tenantA, projectA, reordered)
		if r2.Code != http.StatusOK {
			t.Fatalf("identical registration: %d %s", r2.Code, r2.Body.String())
		}
		var d2 definitionBody
		r2.decode(t, &d2)
		if d2.ID != d1.ID || d2.Version != 1 || d2.Digest != d1.Digest {
			t.Fatalf("identical content must return the same definition: %+v vs %+v", d2, d1)
		}
		if r2.Header().Get("ETag") != `"1"` || r2.Header().Get("Location") != wantLoc {
			t.Fatalf("200 must carry the resource ETag and Location: %v", r2.Header())
		}

		// Changed content under the same name is a new immutable version.
		r3 := register(a, "tok-engineer", tenantA, projectA, jsonDoc("unit-tests", jsonStep("test", `,"args":["go test ./..."]`)))
		if r3.Code != http.StatusCreated {
			t.Fatalf("changed registration: %d %s", r3.Code, r3.Body.String())
		}
		var d3 definitionBody
		r3.decode(t, &d3)
		if d3.Version != 2 || d3.ID == d1.ID || r3.Header().Get("ETag") != `"2"` {
			t.Fatalf("changed content must be version 2: %+v", d3)
		}
		if n := len(a.repo.Definitions()); n != 2 {
			t.Fatalf("%d definitions stored, want 2", n)
		}

		// Both versions stay readable and unchanged.
		g := a.do("tok-engineer", http.MethodGet, base(tenantA, projectA)+"/"+d1.ID, "")
		if g.Code != http.StatusOK || g.Header().Get("ETag") != `"1"` || g.Header().Get("Location") != "" {
			t.Fatalf("read: %d %v", g.Code, g.Header())
		}
		var got definitionBody
		g.decode(t, &got)
		if got.Digest != d1.Digest || !bytes.Equal(got.Definition, d1.Definition) {
			t.Fatalf("a read must return the registered content")
		}
	})

	t.Run("neither If-Match nor Idempotency-Key is required", func(t *testing.T) {
		a := newAPI(t)
		r := register(a, "tok-engineer", tenantA, projectA, jsonDoc("p", jsonStep("a", "")))
		if r.Code != http.StatusCreated {
			t.Fatalf("%d %s", r.Code, r.Body.String())
		}
	})

	t.Run("responses and error examples match api/pipeline.yaml", func(t *testing.T) {
		s := loadSpec(t)
		a := newAPI(t)
		r := register(a, "tok-engineer", tenantA, projectA, jsonDoc("p", jsonStep("a", "")))
		if err := s.validate(t, "pipeline.yaml", "PipelineDefinition", r.Body.Bytes()); err != nil {
			t.Fatalf("registration response violates the schema: %v\n%s", err, r.Body.String())
		}
		l := a.do("tok-engineer", http.MethodGet, base(tenantA, projectA), "")
		if err := s.validate(t, "pipeline.yaml", "PipelineDefinitionPage", l.Body.Bytes()); err != nil {
			t.Fatalf("list response violates the schema: %v\n%s", err, l.Body.String())
		}
		bad := register(a, "tok-engineer", tenantA, projectA, jsonDoc("p", jsonStep("a", `,"needs":["a"]`)))
		if bad.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%d %s", bad.Code, bad.Body.String())
		}
		if err := s.validate(t, "common.yaml", "ErrorEnvelope", bad.Body.Bytes()); err != nil {
			t.Fatalf("error violates the envelope: %v", err)
		}
		// The published examples are themselves valid.
		comps := s.raw["components"].(map[string]any)
		ex := comps["examples"].(map[string]any)["RegisteredDefinition"].(map[string]any)["value"]
		raw, _ := json.Marshal(ex)
		if err := s.validate(t, "pipeline.yaml", "PipelineDefinition", raw); err != nil {
			t.Fatalf("the published example violates the schema: %v", err)
		}
		reqEx := s.raw["paths"].(map[string]any)["/tenants/{tenantId}/projects/{projectId}/pipeline-definitions"].(map[string]any)["post"].(map[string]any)["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["example"]
		raw, _ = json.Marshal(reqEx)
		if err := s.validate(t, "pipeline.yaml", "PipelineDocument", raw); err != nil {
			t.Fatalf("the published request example violates the schema: %v", err)
		}
		// ... and is accepted by the real validator.
		if r := register(newAPI(t), "tok-engineer", tenantA, projectA, string(raw)); r.Code != http.StatusCreated {
			t.Fatalf("the published request example is not registrable: %d %s", r.Code, r.Body.String())
		}
	})

	t.Run("shared validator parity: acceptance diagnostics", func(t *testing.T) {
		a := newAPI(t, pipeline.KindContainer, pipeline.KindAgent, pipeline.KindGate)
		m1 := newAPI(t)
		cases := map[string]struct {
			body string
			code string
		}{
			"cycle":            {jsonDoc("p", jsonStep("a", `,"needs":["b"]`), jsonStep("b", `,"needs":["a"]`)), pipeline.CodeCycle},
			"unknown_output":   {jsonDoc("p", jsonStep("a", ""), jsonStep("b", `,"needs":["a"],"env":{"X":"${{ steps.a.outputs.ghost }}"}`)), pipeline.CodeUnknownOutput},
			"parallel_writers": {jsonDoc("p", jsonStep("a", `,"workspace":{"access":"write","from":"run"}`), jsonStep("b", `,"workspace":{"access":"write","from":"run"}`)), pipeline.CodeParallelWriters},
			"mutable_image":    {strings.ReplaceAll(jsonDoc("p", jsonStep("a", "")), imageRef, "golang:1.24"), pipeline.CodeMutableImage},
			"conditional_gate": {jsonDoc("p", jsonStep("a", ""), `{"id":"g","kind":"gate","needs":["a"],"when":{"input":"x","equals":true}}`), pipeline.CodeConditionalGate},
			"disabled_kind":    {jsonDoc("p", jsonStep("a", ""), `{"id":"g","kind":"gate","needs":["a"]}`), pipeline.CodeKindDisabled},
		}
		for name, tc := range cases {
			t.Run(name, func(t *testing.T) {
				// Compare with the library directly: the HTTP result is the
				// library's verdict, not a second implementation.
				target, kinds := a, []pipeline.StepKind{pipeline.KindContainer, pipeline.KindAgent, pipeline.KindGate}
				if name == "disabled_kind" {
					target, kinds = m1, nil
				}
				_, libErr := pipelinetestValidate(t, tc.body, kinds...)
				if libErr == nil {
					t.Fatalf("the shared library accepted %s", name)
				}
				r := register(target, "tok-engineer", tenantA, projectA, tc.body)
				if r.Code != http.StatusUnprocessableEntity && r.Code != http.StatusBadRequest {
					t.Fatalf("status %d: %s", r.Code, r.Body.String())
				}
				e := r.errEnvelope(t)
				if len(e.Details["diagnostics"].([]any)) == 0 {
					t.Fatalf("no diagnostics: %s", r.Body.String())
				}
				found := false
				for _, d := range e.Details["diagnostics"].([]any) {
					if d.(map[string]any)["code"] == tc.code {
						found = true
					}
				}
				if !found {
					t.Fatalf("expected diagnostic %s in %s", tc.code, r.Body.String())
				}
			})
		}
		for _, x := range []*api{a, m1} {
			if n := len(x.repo.Definitions()); n != 0 || x.repo.Calls != 0 {
				t.Fatalf("rejected documents must not reach the repository: %d stored, %d calls", n, x.repo.Calls)
			}
		}
	})

	t.Run("syntax failures are 400 and semantic failures 422", func(t *testing.T) {
		a := newAPI(t)
		for name, tc := range map[string]struct {
			body   string
			status int
			code   string
		}{
			"malformed json":   {`{"apiVersion":`, http.StatusBadRequest, "invalid_request"},
			"unknown field":    {strings.Replace(jsonDoc("p", jsonStep("a", "")), `"kind":"Pipeline"`, `"kind":"Pipeline","extra":1`, 1), http.StatusBadRequest, "invalid_request"},
			"duplicate key":    {`{"apiVersion":"kodama/v1alpha1","apiVersion":"kodama/v1alpha1","kind":"Pipeline","metadata":{"name":"p"},"spec":{"steps":[` + jsonStep("a", "") + `]}}`, http.StatusBadRequest, "invalid_request"},
			"trailing content": {jsonDoc("p", jsonStep("a", "")) + ` {}`, http.StatusBadRequest, "invalid_request"},
			"empty body":       {"", http.StatusUnsupportedMediaType, "invalid_request"},
			"semantic":         {jsonDoc("p", jsonStep("a", `,"needs":["a"]`)), http.StatusUnprocessableEntity, "invalid_definition"},
		} {
			r := register(a, "tok-engineer", tenantA, projectA, tc.body)
			if r.Code != tc.status || r.errCode(t) != tc.code {
				t.Errorf("%s: %d %s", name, r.Code, r.Body.String())
			}
		}
		// A wrong media type is 415 with the common envelope.
		req := a.do("tok-engineer", http.MethodPost, base(tenantA, projectA), "apiVersion: x", "Content-Type", "application/yaml")
		if req.Code != http.StatusUnsupportedMediaType {
			t.Errorf("yaml body: %d", req.Code)
		}
	})

	t.Run("diagnostics never echo commands or environment values", func(t *testing.T) {
		a := newAPI(t)
		r := register(a, "tok-engineer", tenantA, projectA, jsonDoc("p", jsonStep("a", `,"env":{"TOKEN":"super-secret-value-${{ inputs.x }}"},"needs":["a"]`)))
		if r.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%d %s", r.Code, r.Body.String())
		}
		for _, leak := range []string{"super-secret-value", "/bin/sh"} {
			if strings.Contains(r.Body.String(), leak) {
				t.Fatalf("the error echoes document content %q: %s", leak, r.Body.String())
			}
		}
	})

	t.Run("denial after revocation", func(t *testing.T) {
		a := newAPI(t)
		body := jsonDoc("p", jsonStep("a", ""))
		r := register(a, "tok-engineer", tenantA, projectA, body)
		var d definitionBody
		r.decode(t, &d)
		// Revoking register keeps read but must not turn a duplicate into a
		// success: deduplication is not authorization.
		a.auth.Revoke(scopeA, "u_engineer", pipeline.PermRegister)
		dup := register(a, "tok-engineer", tenantA, projectA, body)
		if dup.Code != http.StatusForbidden || dup.errCode(t) != "forbidden" {
			t.Fatalf("duplicate by a revoked registrar: %d %s", dup.Code, dup.Body.String())
		}
		if strings.Contains(dup.Body.String(), d.ID) || strings.Contains(dup.Body.String(), d.Digest) {
			t.Fatalf("a denied duplicate must not disclose the existing definition: %s", dup.Body.String())
		}
		// A reader may read but not register.
		if g := a.do("tok-reader", http.MethodGet, base(tenantA, projectA)+"/"+d.ID, ""); g.Code != http.StatusOK {
			t.Fatalf("reader read: %d", g.Code)
		}
		if p := register(a, "tok-reader", tenantA, projectA, body); p.Code != http.StatusForbidden {
			t.Fatalf("reader register: %d", p.Code)
		}
		// Revoking read stops reads and lists.
		a.auth.Revoke(scopeA, "u_reader", pipeline.PermRead)
		a.auth.Revoke(scopeA, "u_reader", pipeline.PermList)
		if g := a.do("tok-reader", http.MethodGet, base(tenantA, projectA)+"/"+d.ID, ""); g.Code != http.StatusForbidden {
			t.Fatalf("revoked read: %d", g.Code)
		}
		if l := a.do("tok-reader", http.MethodGet, base(tenantA, projectA), ""); l.Code != http.StatusForbidden {
			t.Fatalf("revoked list: %d", l.Code)
		}
		// An unknown principal is hidden, never told whether the project exists.
		if s := a.do("tok-stranger", http.MethodGet, base(tenantA, projectA), ""); s.Code != http.StatusNotFound || s.errCode(t) != "not_found" {
			t.Fatalf("stranger: %d %s", s.Code, s.Body.String())
		}
		if a.repo.Calls != 1 {
			t.Fatalf("denied registrations reached the repository: %d calls", a.repo.Calls)
		}
	})

	t.Run("router denial runs before any handler", func(t *testing.T) {
		a := newAPI(t)
		a.authz.allow = func(httpapi.Principal, httpapi.Action, httpapi.Target) error { return httpapi.ErrDenied }
		r := register(a, "tok-engineer", tenantA, projectA, jsonDoc("p", jsonStep("a", "")))
		if r.Code != http.StatusForbidden || a.repo.Calls != 0 {
			t.Fatalf("%d, repository calls %d", r.Code, a.repo.Calls)
		}
		if got := a.authz.calls; len(got) != 1 || got[0] != pipelinehttp.ActionRegister {
			t.Fatalf("router actions %v", got)
		}
		if r := register(newAPI(t), "", tenantA, projectA, "{}"); r.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous: %d", r.Code)
		}
	})

	t.Run("cross-scope reads are hidden", func(t *testing.T) {
		a := newAPI(t)
		r := register(a, "tok-engineer", tenantA, projectA, jsonDoc("p", jsonStep("a", "")))
		var d definitionBody
		r.decode(t, &d)
		// The same principal holds rights in scope B but the ID belongs to A.
		g := a.do("tok-engineer", http.MethodGet, base(tenantB, projectB)+"/"+d.ID, "")
		if g.Code != http.StatusNotFound || g.errCode(t) != "not_found" {
			t.Fatalf("foreign read: %d %s", g.Code, g.Body.String())
		}
		missing := a.do("tok-engineer", http.MethodGet, base(tenantA, projectA)+"/pd_missing", "")
		if missing.Code != http.StatusNotFound || missing.Body.String() != g.Body.String() && missing.errCode(t) != g.errCode(t) {
			t.Fatalf("missing and foreign must be indistinguishable: %d", missing.Code)
		}
		// The identical document in scope B is a separate definition.
		rb := register(a, "tok-engineer", tenantB, projectB, jsonDoc("p", jsonStep("a", "")))
		var db definitionBody
		rb.decode(t, &db)
		if rb.Code != http.StatusCreated || db.ID == d.ID {
			t.Fatalf("deduplication must be scoped: %d %s", rb.Code, rb.Body.String())
		}
	})

	t.Run("dependency outages are 503 and never success", func(t *testing.T) {
		a := newAPI(t)
		a.repo.Err = fmt.Errorf("%w: %w", pipeline.ErrUnavailable, pipeline.ErrOutcomeUnknown)
		r := register(a, "tok-engineer", tenantA, projectA, jsonDoc("p", jsonStep("a", "")))
		if r.Code != http.StatusServiceUnavailable || r.errCode(t) != "unavailable" || r.Header().Get("Retry-After") == "" {
			t.Fatalf("unknown outcome: %d %s", r.Code, r.Body.String())
		}
		a.repo.Err = nil
		a.auth.SetErr(fmt.Errorf("authority down"))
		r = register(a, "tok-engineer", tenantA, projectA, jsonDoc("p", jsonStep("a", "")))
		if r.Code != http.StatusServiceUnavailable {
			t.Fatalf("authority outage: %d %s", r.Code, r.Body.String())
		}
		a.auth.SetErr(nil)
		// The retry after recovery succeeds exactly once.
		if r := register(a, "tok-engineer", tenantA, projectA, jsonDoc("p", jsonStep("a", ""))); r.Code != http.StatusCreated {
			t.Fatalf("retry: %d", r.Code)
		}
		if r := register(a, "tok-engineer", tenantA, projectA, jsonDoc("p", jsonStep("a", ""))); r.Code != http.StatusOK {
			t.Fatalf("second retry: %d", r.Code)
		}
	})

	t.Run("a repository conflict is 409 without internals", func(t *testing.T) {
		a := newAPI(t)
		a.repo.Err = fmt.Errorf("%w: digest collision with differing content", pipeline.ErrConflict)
		r := register(a, "tok-engineer", tenantA, projectA, jsonDoc("p", jsonStep("a", "")))
		if r.Code != http.StatusConflict || r.errCode(t) != "definition_conflict" || strings.Contains(r.Body.String(), "collision") {
			t.Fatalf("%d %s", r.Code, r.Body.String())
		}
	})

	t.Run("no mutation or validation-only routes", func(t *testing.T) {
		a := newAPI(t)
		r := register(a, "tok-engineer", tenantA, projectA, jsonDoc("p", jsonStep("a", "")))
		var d definitionBody
		r.decode(t, &d)
		one := base(tenantA, projectA) + "/" + d.ID
		for _, tc := range []struct{ method, path string }{
			{http.MethodPut, one}, {http.MethodPatch, one}, {http.MethodDelete, one},
			{http.MethodPost, one}, {http.MethodPut, base(tenantA, projectA)},
			{http.MethodPatch, base(tenantA, projectA)}, {http.MethodDelete, base(tenantA, projectA)},
		} {
			got := a.do("tok-engineer", tc.method, tc.path, `{}`)
			if got.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s = %d, want 405", tc.method, tc.path, got.Code)
			}
		}
		for _, p := range []string{"/validate", "/validations", "/latest", "/" + d.ID + "/validate", "/" + d.ID + "/versions"} {
			for _, m := range []string{http.MethodGet, http.MethodPost} {
				if p == "/latest" && m == http.MethodGet {
					// "latest" matches {id} and is simply an unknown definition.
					if got := a.do("tok-engineer", m, base(tenantA, projectA)+p, ""); got.Code != http.StatusNotFound {
						t.Errorf("GET latest = %d", got.Code)
					}
					continue
				}
				if p == "/validate" && m == http.MethodGet {
					continue // also matches {id}: an unknown definition
				}
				if got := a.do("tok-engineer", m, base(tenantA, projectA)+p, `{}`); got.Code != http.StatusNotFound && got.Code != http.StatusMethodNotAllowed {
					t.Errorf("%s %s = %d, want 404/405", m, p, got.Code)
				}
			}
		}
		if n := len(a.repo.Definitions()); n != 1 {
			t.Fatalf("%d definitions after rejected mutations", n)
		}
	})

	t.Run("pagination: ordered, scoped cursors", func(t *testing.T) {
		a := newAPI(t)
		var ids []string
		for i := 0; i < 5; i++ {
			r := register(a, "tok-engineer", tenantA, projectA, jsonDoc(fmt.Sprintf("p%d", i%2), jsonStep("a", fmt.Sprintf(`,"args":["%d"]`, i))))
			var d definitionBody
			r.decode(t, &d)
			ids = append(ids, d.ID)
		}
		type pg struct {
			Items []definitionBody `json:"items"`
			Next  *string          `json:"nextCursor"`
		}
		get := func(token, tenant, project, q string) (resp, pg) {
			r := a.do(token, http.MethodGet, base(tenant, project)+q, "")
			var p pg
			if r.Code == http.StatusOK {
				r.decode(t, &p)
			}
			return r, p
		}
		_, p1 := get("tok-engineer", tenantA, projectA, "?limit=2")
		if len(p1.Items) != 2 || p1.Next == nil {
			t.Fatalf("first page: %+v", p1)
		}
		_, p2 := get("tok-engineer", tenantA, projectA, "?limit=2&cursor="+url.QueryEscape(*p1.Next))
		_, p3 := get("tok-engineer", tenantA, projectA, "?limit=2&cursor="+url.QueryEscape(*p2.Next))
		if len(p2.Items) != 2 || len(p3.Items) != 1 || p3.Next != nil {
			t.Fatalf("pages: %d %d %d next=%v", len(p1.Items), len(p2.Items), len(p3.Items), p3.Next)
		}
		seen := map[string]bool{}
		for _, p := range []pg{p1, p2, p3} {
			for _, it := range p.Items {
				if seen[it.ID] {
					t.Fatalf("duplicate %s across pages", it.ID)
				}
				seen[it.ID] = true
			}
		}
		for _, id := range ids {
			if !seen[id] {
				t.Fatalf("%s missing from the pages", id)
			}
		}
		// Name filter.
		_, f := get("tok-engineer", tenantA, projectA, "?name=p1")
		if len(f.Items) != 2 {
			t.Fatalf("name filter: %+v", f)
		}
		// Copied cursors fail in another project, tenant, or under another filter.
		for name, got := range map[string]resp{
			"other tenant":  a.do("tok-engineer", http.MethodGet, base(tenantB, projectB)+"?limit=2&cursor="+url.QueryEscape(*p1.Next), ""),
			"other filter":  a.do("tok-engineer", http.MethodGet, base(tenantA, projectA)+"?limit=2&name=p1&cursor="+url.QueryEscape(*p1.Next), ""),
			"tampered":      a.do("tok-engineer", http.MethodGet, base(tenantA, projectA)+"?limit=2&cursor="+url.QueryEscape(*p1.Next+"x"), ""),
			"garbage":       a.do("tok-engineer", http.MethodGet, base(tenantA, projectA)+"?cursor=abc.def", ""),
			"limit too big": a.do("tok-engineer", http.MethodGet, base(tenantA, projectA)+"?limit=201", ""),
			"bad name":      a.do("tok-engineer", http.MethodGet, base(tenantA, projectA)+"?name=Not_A_Name", ""),
			"unknown query": a.do("tok-engineer", http.MethodGet, base(tenantA, projectA)+"?sort=asc", ""),
			"repeated name": a.do("tok-engineer", http.MethodGet, base(tenantA, projectA)+"?name=a&name=b", ""),
		} {
			if got.Code != http.StatusBadRequest {
				t.Errorf("%s: %d %s", name, got.Code, got.Body.String())
			}
		}
		// A copied cursor stays authorized per page: revocation stops it.
		a.auth.Revoke(scopeA, "u_engineer", pipeline.PermList)
		if r, _ := get("tok-engineer", tenantA, projectA, "?limit=2&cursor="+url.QueryEscape(*p1.Next)); r.Code != http.StatusForbidden {
			t.Fatalf("revoked cursor continuation: %d", r.Code)
		}
	})

	t.Run("routes carry the documented actions", func(t *testing.T) {
		a := newAPI(t)
		r := register(a, "tok-engineer", tenantA, projectA, jsonDoc("p", jsonStep("a", "")))
		var d definitionBody
		r.decode(t, &d)
		a.do("tok-engineer", http.MethodGet, base(tenantA, projectA), "")
		a.do("tok-engineer", http.MethodGet, base(tenantA, projectA)+"/"+d.ID, "")
		want := []httpapi.Action{"pipeline_definition.register", "pipeline_definition.list", "pipeline_definition.read"}
		if fmt.Sprint(a.authz.calls) != fmt.Sprint(want) {
			t.Fatalf("actions %v, want %v", a.authz.calls, want)
		}
	})

	t.Run("constructor and request body guards", func(t *testing.T) {
		if _, err := pipelinehttp.NewHandler(nil, nil); err == nil {
			t.Fatal("a handler without a service or codec must be refused")
		}
		a := newAPI(t)
		big := jsonDoc("p", jsonStep("a", `,"args":["`+strings.Repeat("x", 2<<20)+`"]`))
		if r := register(a, "tok-engineer", tenantA, projectA, big); r.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversized body: %d", r.Code)
		}
	})
}

func pipelinetestValidate(t testing.TB, body string, kinds ...pipeline.StepKind) (pipeline.Validated, error) {
	t.Helper()
	v := pipelinetest.NewValidator(t, kinds...)
	return v.ValidateJSON(t.Context(), scopeA, pipeline.Actor{ID: "u_engineer", Type: pipeline.ActorUser}, []byte(body))
}
