package dependencieshttp_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/work"
	"github.com/optimiware/kodama/internal/work/dependencies/depstest"
	dependencieshttp "github.com/optimiware/kodama/internal/work/dependencies/http"
)

const (
	fragmentYAML = "../../../../api/work-dependencies.yaml"
	commonYAML   = "../../../../api/common.yaml"
	fragmentName = "work-dependencies.yaml"
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
// so the fragment can validate documents.
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
	for name, path := range map[string]string{"common.yaml": commonYAML, fragmentName: fragmentYAML} {
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
		if name == fragmentName {
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

func (s *spec) valid(t testing.TB, schema string, doc []byte) error {
	return s.validate(t, fragmentName, schema, doc)
}

func strictDecode(doc []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// operations returns "METHOD path" for every operation of the fragment.
func (s *spec) operations() []string {
	var out []string
	for path, item := range s.raw["paths"].(map[string]any) {
		for method := range item.(map[string]any) {
			if method == "parameters" {
				continue
			}
			out = append(out, strings.ToUpper(method)+" "+path)
		}
	}
	sort.Strings(out)
	return out
}

// Test_dependency_http_contract: generated schemas, strict requests,
// permissions, ETags, pagination, cursor ownership, duplicate semantics and the
// safe error and status mappings match the fragment. Real PostgreSQL for the
// adapter-backed cases; no NATS, S3 or Git. WM-05, WM-10, IT-02, IT-03, IT-A02.
func Test_dependency_http_contract(t *testing.T) {
	sp := loadSpec(t)
	st := depstest.NewStack(t)
	a := newAPI(t, st)
	dep := st.Create(scopeA, work.KindStory, "dependent")
	pre := st.Create(scopeA, work.KindStory, "prerequisite")
	list := depPath(tenantA, projectA, string(dep.ID))

	t.Run("the_fragment_declares_exactly_the_registered_routes", func(t *testing.T) {
		want := []string{
			"DELETE /tenants/{tenantId}/projects/{projectId}/work-items/{id}/dependencies/{dependencyId}",
			"GET /tenants/{tenantId}/projects/{projectId}/work-items/{id}/dependencies",
			"POST /tenants/{tenantId}/projects/{projectId}/work-items/{id}/dependencies",
		}
		if got := sp.operations(); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("operations = %v", got)
		}
		// Anything else on those paths is refused with the standard envelope.
		for _, tc := range []struct{ method, path string }{
			{http.MethodPatch, list}, {http.MethodPut, list}, {http.MethodDelete, list},
			{http.MethodGet, memberPath(tenantA, projectA, string(dep.ID), string(pre.ID))},
			{http.MethodPost, memberPath(tenantA, projectA, string(dep.ID), string(pre.ID))},
			{http.MethodPatch, memberPath(tenantA, projectA, string(dep.ID), string(pre.ID))},
		} {
			r := a.do("tok-engineer", tc.method, tc.path, "", "If-Match", `"1"`)
			if r.Code != http.StatusMethodNotAllowed || r.errCode(t) != "invalid_request" {
				t.Errorf("%s %s: %d %s", tc.method, tc.path, r.Code, r.Body.String())
			}
		}
		// Every route carries its action and names the dependent as the resource.
		a.authz.calls = nil
		a.do("tok-engineer", http.MethodGet, list, "")
		a.do("tok-engineer", http.MethodPost, list, addBody(string(pre.ID)), "If-Match", etag(int64(st.Get(dep).Version)))
		a.do("tok-engineer", http.MethodDelete, memberPath(tenantA, projectA, string(dep.ID), string(pre.ID)), "", "If-Match", etag(int64(st.Get(dep).Version)))
		wantActions := []httpapi.Action{dependencieshttp.ActionList, dependencieshttp.ActionAdd, dependencieshttp.ActionRemove}
		if len(a.authz.calls) != 3 {
			t.Fatalf("route authorizations: %d", len(a.authz.calls))
		}
		for i, c := range a.authz.calls {
			if c.Action != wantActions[i] || c.Target.Resource.Kind != "work_item" || c.Target.Resource.ID != string(dep.ID) ||
				string(c.Target.TenantID) != tenantA || string(c.Target.ProjectID) != projectA {
				t.Errorf("call %d: %+v", i, c)
			}
		}
	})

	t.Run("the_fragment_examples_match_their_schemas", func(t *testing.T) {
		paths := sp.raw["paths"].(map[string]any)
		collection := paths["/tenants/{tenantId}/projects/{projectId}/work-items/{id}/dependencies"].(map[string]any)
		get := collection["get"].(map[string]any)["responses"].(map[string]any)["200"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)
		ex, _ := json.Marshal(get["example"])
		if err := sp.valid(t, "DependencyPage", ex); err != nil {
			t.Errorf("list example: %v", err)
		}
		post := collection["post"].(map[string]any)
		body, _ := json.Marshal(post["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["example"])
		if err := sp.valid(t, "AddDependency", body); err != nil {
			t.Errorf("request example: %v", err)
		}
		for _, code := range []string{"200", "201"} {
			c := post["responses"].(map[string]any)[code].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["example"]
			b, _ := json.Marshal(c)
			if err := sp.valid(t, "DependencyChange", b); err != nil {
				t.Errorf("%s example: %v", code, err)
			}
		}
	})

	t.Run("add_and_list_match_the_generated_schemas", func(t *testing.T) {
		v := int64(st.Get(dep).Version)
		r := a.do("tok-engineer", http.MethodPost, list, addBody(string(pre.ID)), "If-Match", etag(v))
		if r.Code != http.StatusCreated {
			t.Fatalf("%d %s", r.Code, r.Body.String())
		}
		if err := sp.valid(t, "DependencyChange", r.Body.Bytes()); err != nil {
			t.Fatalf("%v\n%s", err, r.Body.String())
		}
		var ch dependencieshttp.DependencyChange
		if err := strictDecode(r.Body.Bytes(), &ch); err != nil || !ch.Changed || string(ch.DependentId) != string(dep.ID) || string(ch.DependencyId) != string(pre.ID) || int64(ch.Version) != v+1 {
			t.Fatalf("change: %+v %v", ch, err)
		}
		if r.etag(t) != v+1 {
			t.Fatalf("etag %q", r.Header().Get("ETag"))
		}

		l := a.do("tok-reader", http.MethodGet, list, "")
		if l.Code != http.StatusOK {
			t.Fatalf("%d %s", l.Code, l.Body.String())
		}
		if err := sp.valid(t, "DependencyPage", l.Body.Bytes()); err != nil {
			t.Fatalf("%v\n%s", err, l.Body.String())
		}
		var pg dependencieshttp.DependencyPage
		if err := strictDecode(l.Body.Bytes(), &pg); err != nil || len(pg.Items) != 1 || pg.NextCursor != nil {
			t.Fatalf("page: %+v %v", pg, err)
		}
		it := pg.Items[0]
		if string(it.DependentId) != string(dep.ID) || string(it.DependencyId) != string(pre.ID) || it.CreatedBy.Id != depstest.Engineer || it.CreatedBy.Type != dependencieshttp.User || it.CreatedAt.IsZero() {
			t.Fatalf("item: %+v", it)
		}
		// The list ETag is the dependent's version: the value to send as If-Match.
		if l.etag(t) != v+1 || l.etag(t) != int64(st.Get(dep).Version) {
			t.Fatalf("list etag %q", l.Header().Get("ETag"))
		}
		if l.Header().Get("Content-Type") != "application/json" || l.Header().Get("X-Request-ID") == "" {
			t.Fatalf("headers: %v", l.Header())
		}
		// An empty collection is an empty page with the version, never null.
		empty := st.Create(scopeA, work.KindStory, "no links")
		e := a.do("tok-reader", http.MethodGet, depPath(tenantA, projectA, string(empty.ID)), "")
		if e.Code != http.StatusOK || !strings.Contains(e.Body.String(), `"items":[]`) || !strings.Contains(e.Body.String(), `"nextCursor":null`) || e.etag(t) != int64(empty.Version) {
			t.Fatalf("empty page: %d %s %s", e.Code, e.Body.String(), e.Header().Get("ETag"))
		}
	})

	t.Run("duplicate_add_and_absent_delete_are_no_ops", func(t *testing.T) {
		d2, p2 := st.Create(scopeA, work.KindStory, "dup dependent"), st.Create(scopeA, work.KindStory, "dup prerequisite")
		path := depPath(tenantA, projectA, string(d2.ID))
		v := int64(d2.Version)
		first := a.do("tok-engineer", http.MethodPost, path, addBody(string(p2.ID)), "If-Match", etag(v))
		again := a.do("tok-engineer", http.MethodPost, path, addBody(string(p2.ID)), "If-Match", etag(v+1))
		if first.Code != http.StatusCreated || again.Code != http.StatusOK {
			t.Fatalf("statuses %d %d", first.Code, again.Code)
		}
		var ch dependencieshttp.DependencyChange
		again.decode(t, &ch)
		if ch.Changed || again.etag(t) != v+1 || int64(ch.Version) != v+1 {
			t.Fatalf("duplicate: %+v etag %s", ch, again.Header().Get("ETag"))
		}
		if err := sp.valid(t, "DependencyChange", again.Body.Bytes()); err != nil {
			t.Fatal(err)
		}
		before := snapshotOf(st, d2)
		del := memberPath(tenantA, projectA, string(d2.ID), string(p2.ID))
		r1 := a.do("tok-engineer", http.MethodDelete, del, "", "If-Match", etag(v+1))
		r2 := a.do("tok-engineer", http.MethodDelete, del, "", "If-Match", etag(v+2))
		if r1.Code != http.StatusNoContent || r2.Code != http.StatusNoContent || r1.Body.Len() != 0 || r1.etag(t) != v+2 || r2.etag(t) != v+2 {
			t.Fatalf("delete: %d %d %q %q", r1.Code, r2.Code, r1.Header().Get("ETag"), r2.Header().Get("ETag"))
		}
		after := snapshotOf(st, d2)
		if after.version != before.version+1 || after.history != before.history+1 {
			t.Fatalf("one removal, one version: %+v -> %+v", before, after)
		}
	})

	t.Run("if_match_is_required_strong_and_current", func(t *testing.T) {
		d, p := st.Create(scopeA, work.KindStory, "etag dependent"), st.Create(scopeA, work.KindStory, "etag prerequisite")
		path := depPath(tenantA, projectA, string(d.ID))
		v := int64(d.Version)
		for _, m := range []struct{ method, path, body string }{
			{http.MethodPost, path, addBody(string(p.ID))},
			{http.MethodDelete, memberPath(tenantA, projectA, string(d.ID), string(p.ID)), ""},
		} {
			if r := a.do("tok-engineer", m.method, m.path, m.body); r.Code != http.StatusPreconditionRequired || r.errCode(t) != "version_required" {
				t.Errorf("%s without If-Match: %d %s", m.method, r.Code, r.Body.String())
			}
			for name, bad := range map[string]string{"weak": `W/"1"`, "wildcard": `*`, "list": `"1", "2"`, "unquoted": `1`, "zero": `"0"`, "text": `"x"`} {
				if r := a.do("tok-engineer", m.method, m.path, m.body, "If-Match", bad); r.Code != http.StatusBadRequest {
					t.Errorf("%s with a %s If-Match: %d %s", m.method, name, r.Code, r.Body.String())
				}
			}
			if r := a.do("tok-engineer", m.method, m.path, m.body, "If-Match", etag(v+7)); r.Code != http.StatusPreconditionFailed || r.errCode(t) != "version_conflict" {
				t.Errorf("%s with a stale If-Match: %d %s", m.method, r.Code, r.Body.String())
			}
		}
		if got := st.Get(d); got.Version != d.Version {
			t.Fatalf("a refused request bumped the version: %d", got.Version)
		}
	})

	t.Run("requests_are_strict", func(t *testing.T) {
		d, p := st.Create(scopeA, work.KindStory, "strict dependent"), st.Create(scopeA, work.KindStory, "strict prerequisite")
		path := depPath(tenantA, projectA, string(d.ID))
		ifm := etag(int64(d.Version))
		for name, body := range map[string]string{
			"unknown member": `{"dependencyId":"` + string(p.ID) + `","extra":1}`,
			"missing member": `{}`,
			"null":           `{"dependencyId":null}`,
			"a number":       `{"dependencyId":7}`,
			"an array":       `[]`,
			"trailing data":  `{"dependencyId":"` + string(p.ID) + `"} {}`,
			"a malformed id": `{"dependencyId":"wi bad"}`,
			"an empty id":    `{"dependencyId":""}`,
			"truncated":      `{"dependencyId":`,
		} {
			r := a.do("tok-engineer", http.MethodPost, path, body, "If-Match", ifm)
			if r.Code != http.StatusBadRequest {
				t.Errorf("%s: %d %s", name, r.Code, r.Body.String())
				continue
			}
			if r.errCode(t) != "invalid_request" {
				t.Errorf("%s: %s", name, r.Body.String())
			}
		}
		if r := a.do("tok-engineer", http.MethodPost, path, ""); r.Code == http.StatusCreated {
			t.Errorf("an empty body created a link")
		}
		if r := a.do("tok-engineer", http.MethodPost, path, addBody(string(p.ID)), "If-Match", ifm, "Content-Type", "text/plain"); r.Code != http.StatusUnsupportedMediaType {
			t.Errorf("wrong content type: %d %s", r.Code, r.Body.String())
		}
		// The caller identity and actor type are never read from the body.
		if r := a.do("tok-engineer", http.MethodPost, path, `{"dependencyId":"`+string(p.ID)+`","createdBy":{"type":"service","id":"x"}}`, "If-Match", ifm); r.Code != http.StatusBadRequest {
			t.Errorf("caller identity in the body: %d", r.Code)
		}
		// A malformed identifier in the path is a request error.
		if r := a.do("tok-engineer", http.MethodDelete, memberPath(tenantA, projectA, string(d.ID), "wi%20bad"), "", "If-Match", ifm); r.Code != http.StatusBadRequest {
			t.Errorf("malformed dependencyId: %d %s", r.Code, r.Body.String())
		}
		if got := st.Get(d); got.Version != d.Version {
			t.Fatal("a malformed request changed state")
		}
		// Unknown query parameters on the list are refused.
		if r := a.do("tok-engineer", http.MethodGet, list+"?state=done", ""); r.Code != http.StatusBadRequest {
			t.Errorf("unknown query parameter: %d", r.Code)
		}
	})

	t.Run("permissions_and_denial_mappings", func(t *testing.T) {
		d, p := st.Create(scopeA, work.KindStory, "perm dependent"), st.Create(scopeA, work.KindStory, "perm prerequisite")
		path := depPath(tenantA, projectA, string(d.ID))
		ifm := etag(int64(d.Version))
		if r := a.do("tok-reader", http.MethodPost, path, addBody(string(p.ID)), "If-Match", ifm); r.Code != http.StatusForbidden || r.errCode(t) != "forbidden" {
			t.Errorf("reader add: %d %s", r.Code, r.Body.String())
		}
		if r := a.do("tok-reader", http.MethodDelete, memberPath(tenantA, projectA, string(d.ID), string(p.ID)), "", "If-Match", ifm); r.Code != http.StatusForbidden {
			t.Errorf("reader delete: %d", r.Code)
		}
		if r := a.do("tok-outsider", http.MethodGet, path, ""); r.Code != http.StatusForbidden {
			t.Errorf("outsider list: %d", r.Code)
		}
		if r := a.do("tok-stranger", http.MethodGet, path, ""); r.Code != http.StatusNotFound {
			t.Errorf("unknown principal: %d", r.Code)
		}
		if r := a.do("bad-token", http.MethodGet, path, ""); r.Code != http.StatusUnauthorized {
			t.Errorf("bad token: %d", r.Code)
		}
		if r := a.do("", http.MethodPost, path, addBody(string(p.ID)), "If-Match", ifm); r.Code != http.StatusUnauthorized {
			t.Errorf("anonymous: %d", r.Code)
		}
		// A route-level denial stops the request before the use case.
		a.authz.allow = func(_ httpapi.Principal, act httpapi.Action, _ httpapi.Target) error {
			if act == dependencieshttp.ActionAdd {
				return httpapi.ErrDenied
			}
			return nil
		}
		if r := a.do("tok-engineer", http.MethodPost, path, addBody(string(p.ID)), "If-Match", ifm); r.Code != http.StatusForbidden {
			t.Errorf("route-level denial: %d", r.Code)
		}
		a.authz.allow = func(httpapi.Principal, httpapi.Action, httpapi.Target) error { return httpapi.ErrHidden }
		if r := a.do("tok-engineer", http.MethodGet, path, ""); r.Code != http.StatusNotFound {
			t.Errorf("hidden scope: %d", r.Code)
		}
		a.authz.allow = nil
		// An identity outage is 503 with Retry-After and no internals.
		st.Authority.Outage.Store(true)
		r := a.do("tok-engineer", http.MethodGet, path, "")
		st.Authority.Outage.Store(false)
		if r.Code != http.StatusServiceUnavailable || r.Header().Get("Retry-After") == "" || r.errCode(t) != "unavailable" || strings.Contains(r.Body.String(), "unreachable") {
			t.Errorf("authority outage: %d %s", r.Code, r.Body.String())
		}
		if got := st.Get(d); got.Version != d.Version || st.Links(tenantA) > 1 {
			t.Fatal("a denied request changed state")
		}
	})

	t.Run("graph_terminal_and_missing_mappings", func(t *testing.T) {
		x, y := st.Create(scopeA, work.KindStory, "x"), st.Create(scopeA, work.KindStory, "y")
		st.MustLink(x, y)
		path := depPath(tenantA, projectA, string(y.ID))
		r := a.do("tok-engineer", http.MethodPost, path, addBody(string(x.ID)), "If-Match", etag(int64(y.Version)))
		if r.Code != http.StatusUnprocessableEntity {
			t.Fatalf("cycle: %d %s", r.Code, r.Body.String())
		}
		e := r.errBody(t)
		if e.Code != "dependency_cycle" || strings.Contains(e.Message, string(x.ID)) || strings.Contains(e.Message, string(y.ID)) || len(e.Details) != 0 {
			t.Fatalf("cycle error: %+v", e)
		}
		if err := sp.validate(t, "common.yaml", "ErrorEnvelope", r.Body.Bytes()); err != nil {
			t.Fatalf("error envelope: %v", err)
		}
		// A self link, before any lookup.
		if r := a.do("tok-engineer", http.MethodPost, path, addBody(string(y.ID)), "If-Match", etag(int64(y.Version))); r.Code != http.StatusUnprocessableEntity || r.errCode(t) != "dependency_cycle" {
			t.Errorf("self link: %d %s", r.Code, r.Body.String())
		}
		// A done dependent accepts no link changes; a done prerequisite is fine.
		done := st.Done(scopeA, "done dependent")
		if r := a.do("tok-engineer", http.MethodPost, depPath(tenantA, projectA, string(done.ID)), addBody(string(y.ID)), "If-Match", etag(int64(done.Version))); r.Code != http.StatusUnprocessableEntity || r.errCode(t) != "invalid_transition" {
			t.Errorf("done dependent: %d %s", r.Code, r.Body.String())
		}
		doneP := st.Get(done)
		if r := a.do("tok-engineer", http.MethodPost, depPath(tenantA, projectA, string(y.ID)), addBody(string(doneP.ID)), "If-Match", etag(int64(st.Get(y).Version))); r.Code != http.StatusCreated {
			t.Errorf("a done prerequisite: %d %s", r.Code, r.Body.String())
		}
		cancelled := st.MustMove(st.Create(scopeA, work.KindStory, "to cancel"), depstest.Engineer, work.StateCancelled)
		if r := a.do("tok-engineer", http.MethodDelete, memberPath(tenantA, projectA, string(cancelled.ID), string(y.ID)), "", "If-Match", etag(int64(cancelled.Version))); r.Code != http.StatusUnprocessableEntity || r.errCode(t) != "invalid_transition" {
			t.Errorf("cancelled dependent: %d %s", r.Code, r.Body.String())
		}
		// Missing work items.
		if r := a.do("tok-engineer", http.MethodGet, depPath(tenantA, projectA, "wi_missing"), ""); r.Code != http.StatusNotFound || r.errCode(t) != "not_found" {
			t.Errorf("missing dependent: %d", r.Code)
		}
	})

	t.Run("pagination_is_stable_bounded_and_scoped", func(t *testing.T) {
		d := st.Create(scopeA, work.KindStory, "paged dependent")
		var want []string
		for i := 0; i < 5; i++ {
			p := st.Create(scopeA, work.KindStory, fmt.Sprintf("paged %d", i))
			st.MustLink(d, p)
			want = append(want, string(p.ID))
		}
		sort.Strings(want)
		path := depPath(tenantA, projectA, string(d.ID))
		var got []string
		next := ""
		for pages := 0; ; pages++ {
			q := "?limit=2"
			if next != "" {
				q += "&cursor=" + url.QueryEscape(next)
			}
			r := a.do("tok-reader", http.MethodGet, path+q, "")
			if r.Code != http.StatusOK {
				t.Fatalf("%d %s", r.Code, r.Body.String())
			}
			if err := sp.valid(t, "DependencyPage", r.Body.Bytes()); err != nil {
				t.Fatal(err)
			}
			var pg dependencieshttp.DependencyPage
			r.decode(t, &pg)
			if len(pg.Items) > 2 {
				t.Fatalf("page of %d", len(pg.Items))
			}
			for _, it := range pg.Items {
				got = append(got, string(it.DependencyId))
			}
			if r.etag(t) != int64(st.Get(d).Version) {
				t.Fatalf("etag %s", r.Header().Get("ETag"))
			}
			if pg.NextCursor == nil {
				break
			}
			next = *pg.NextCursor
			if pages > 5 {
				t.Fatal("pagination does not terminate")
			}
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("paged %v, want %v", got, want)
		}
		// Limits outside 1..200 are rejected, never clamped; the default is 50.
		for _, bad := range []string{"0", "201", "-1", "abc", "1.5", ""} {
			if r := a.do("tok-reader", http.MethodGet, path+"?limit="+bad, ""); bad != "" && r.Code != http.StatusBadRequest {
				t.Errorf("limit=%s: %d", bad, r.Code)
			}
		}
		if r := a.do("tok-reader", http.MethodGet, path+"?limit=200", ""); r.Code != http.StatusOK {
			t.Errorf("limit=200: %d", r.Code)
		}
		// A continuation after a deletion still moves forward without repeating rows.
		first := a.do("tok-reader", http.MethodGet, path+"?limit=2", "")
		var pg dependencieshttp.DependencyPage
		first.decode(t, &pg)
		st.Unlink(d, work.Item{Scope: scopeA, ID: domain.WorkItemID(want[0])})
		r := a.do("tok-reader", http.MethodGet, path+"?limit=2&cursor="+url.QueryEscape(*pg.NextCursor), "")
		var pg2 dependencieshttp.DependencyPage
		r.decode(t, &pg2)
		if r.Code != http.StatusOK || len(pg2.Items) == 0 || string(pg2.Items[0].DependencyId) != want[2] {
			t.Fatalf("continuation after a deletion: %d %+v", r.Code, pg2)
		}
	})

	t.Run("error_envelopes_match_the_common_schema", func(t *testing.T) {
		r := a.do("tok-engineer", http.MethodPost, list, `{"nope":1}`, "If-Match", `"1"`)
		if err := sp.validate(t, "common.yaml", "ErrorEnvelope", r.Body.Bytes()); err != nil {
			t.Fatalf("%v\n%s", err, r.Body.String())
		}
		r = a.do("tok-engineer", http.MethodGet, depPath(tenantA, projectA, "wi_missing"), "")
		if err := sp.validate(t, "common.yaml", "ErrorEnvelope", r.Body.Bytes()); err != nil {
			t.Fatalf("%v\n%s", err, r.Body.String())
		}
	})

	t.Run("constructors_require_their_collaborators", func(t *testing.T) {
		if _, err := dependencieshttp.NewHandler(nil, nil); err == nil {
			t.Fatal("a handler without collaborators was constructed")
		}
	})
}
