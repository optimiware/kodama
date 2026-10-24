package identityhttp_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
	identityhttp "github.com/optimiware/kodama/internal/identity/http"
	secretshttp "github.com/optimiware/kodama/internal/secrets/http"
	workhttp "github.com/optimiware/kodama/internal/work/http"
)

const (
	identityYAML = "../../../api/identity.yaml"
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

// translate rewrites OpenAPI 3.0 `nullable: true` into JSON Schema type unions.
func translate(n any) {
	switch v := n.(type) {
	case map[string]any:
		if nb, _ := v["nullable"].(bool); nb {
			if ty, ok := v["type"].(string); ok {
				v["type"] = []any{ty, "null"}
			}
		}
		delete(v, "nullable")
		delete(v, "x-go-type")
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
	for name, path := range map[string]string{"common.yaml": commonYAML, "identity.yaml": identityYAML} {
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
		if name == "identity.yaml" {
			out = raw
		}
	}
	return &spec{raw: out, c: c}
}

func (s *spec) validate(t testing.TB, schema string, doc []byte) error {
	t.Helper()
	sch, err := s.c.Compile("identity.yaml#/components/schemas/" + schema)
	if err != nil {
		t.Fatalf("schema %s: %v", schema, err)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		t.Fatalf("not JSON: %v: %s", err, doc)
	}
	return sch.Validate(v)
}

func strictDecode(doc []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func etag(r resp) string { return r.Header().Get("ETag") }

func mustCode(t *testing.T, name string, r resp, code int, errCode string) {
	t.Helper()
	if r.Code != code {
		t.Errorf("%s: status %d, want %d: %s", name, r.Code, code, r.Body.String())
		return
	}
	if errCode != "" {
		if got := r.errCode(t); got != errCode {
			t.Errorf("%s: code %q, want %q", name, got, errCode)
		}
	}
}

// TestIdentityHTTPContract: the OpenAPI fragment, its examples and generated types
// match the handlers; strict JSON, the status mappings, ETags, request IDs,
// route authorization, principal-bound cursors and bounded pagination are
// enforced. No external services.
func TestIdentityHTTPContract(t *testing.T) {
	sp := loadSpec(t)
	a := newAPI(t, "admin", "padmin", "viewer", "reviewer", "outsider", "newbie")
	a.bind(tenantA, "mb_admin", "admin", "", identity.RoleAdmin)
	a.bind(tenantA, "mb_padmin", "padmin", projectA, identity.RoleAdmin)
	a.bind(tenantA, "mb_viewer", "viewer", "", identity.RoleViewer)
	a.bind(tenantA, "mb_reviewer", "reviewer", projectA2, identity.RoleReviewer)
	a.bind(tenantB, "mb_admin_b", "outsider", "", identity.RoleAdmin)

	t.Run("documented routes are exactly the registered routes", func(t *testing.T) {
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
			"DELETE /tenants/{tenantId}/memberships/{id}",
			"GET /me",
			"GET /tenants",
			"GET /tenants/{tenantId}/memberships",
			"GET /tenants/{tenantId}/projects",
			"GET /tenants/{tenantId}/projects/{projectId}",
			"PATCH /tenants/{tenantId}/memberships/{id}",
			"PATCH /tenants/{tenantId}/projects/{projectId}",
			"POST /tenants/{tenantId}/memberships",
			"POST /tenants/{tenantId}/projects",
		}
		if strings.Join(documented, "\n") != strings.Join(want, "\n") {
			t.Fatalf("documented routes:\n%s", strings.Join(documented, "\n"))
		}
		for _, route := range want {
			method, path, _ := strings.Cut(route, " ")
			path = strings.NewReplacer("{tenantId}", tenantA, "{projectId}", projectA, "{id}", "mb_admin").Replace(path)
			if r := a.do("", method, path, `{}`); r.Code != http.StatusUnauthorized || r.errCode(t) != "invalid_credential" {
				t.Errorf("%s without a token: %d", route, r.Code)
			}
		}
		for _, route := range []string{
			"DELETE /tenants/t_acme/projects/p_inventory", "POST /tenants", "PUT /me", "GET /tenants/t_acme/memberships/mb_admin",
			"POST /bootstrap", "POST /tenants/t_acme/bootstrap", "POST /token", "POST /tenants/t_acme/users", "GET /tenants/t_acme",
		} {
			method, path, _ := strings.Cut(route, " ")
			if r := a.do("tok-admin", method, path, `{}`); r.Code != http.StatusNotFound && r.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s is served: %d", route, r.Code)
			}
		}
	})

	t.Run("every action of the identity, work and configuration routes has a rule", func(t *testing.T) {
		rules := identityhttp.DefaultRules()
		secretsActions := secretshttp.DefaultActions()
		for _, act := range []httpapi.Action{
			identityhttp.ActionMe, identityhttp.ActionTenantList, identityhttp.ActionMembershipList, identityhttp.ActionMembershipCreate,
			identityhttp.ActionMembershipUpdate, identityhttp.ActionMembershipDelete, identityhttp.ActionProjectList, identityhttp.ActionProjectCreate,
			identityhttp.ActionProjectRead, identityhttp.ActionProjectUpdate,
			workhttp.ActionList, workhttp.ActionCreate, workhttp.ActionRead, workhttp.ActionUpdate, workhttp.ActionHistory, workhttp.ActionTransition,
			secretsActions.VariableList, secretsActions.VariableRead, secretsActions.VariableCreate, secretsActions.VariableUpdate, secretsActions.VariableDelete,
			secretsActions.SecretList, secretsActions.SecretRead, secretsActions.SecretCreate, secretsActions.SecretDelete,
			secretsActions.VersionList, secretsActions.VersionCreate, secretsActions.VersionRevoke,
		} {
			if _, ok := rules[act]; !ok {
				t.Errorf("action %q has no rule: its routes would be denied", act)
			}
		}
		for act, r := range rules {
			if r.Level != identityhttp.LevelPrincipal && len(r.Any) == 0 {
				t.Errorf("%q: no permission", act)
			}
			for _, p := range r.Any {
				if !p.Known() {
					t.Errorf("%q: unknown permission %q", act, p)
				}
			}
		}
	})

	t.Run("documented examples decode into the generated types and validate", func(t *testing.T) {
		paths := sp.raw["paths"].(map[string]any)
		example := func(path, method string) []byte {
			op := paths[path].(map[string]any)[method].(map[string]any)
			ex := op["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["example"]
			b, _ := json.Marshal(ex)
			return b
		}
		var cm identityhttp.CreateMembership
		b := example("/tenants/{tenantId}/memberships", "post")
		if err := strictDecode(b, &cm); err != nil || cm.Role != identityhttp.Reviewer {
			t.Errorf("create membership example: %v", err)
		}
		if err := sp.validate(t, "CreateMembership", b); err != nil {
			t.Errorf("create membership schema: %v", err)
		}
		var um identityhttp.UpdateMembership
		b = example("/tenants/{tenantId}/memberships/{id}", "patch")
		if err := strictDecode(b, &um); err != nil || sp.validate(t, "UpdateMembership", b) != nil {
			t.Errorf("update membership example: %v", err)
		}
		var cp identityhttp.CreateProject
		b = example("/tenants/{tenantId}/projects", "post")
		if err := strictDecode(b, &cp); err != nil || sp.validate(t, "CreateProject", b) != nil {
			t.Errorf("create project example: %v", err)
		}
		var up identityhttp.UpdateProject
		b = example("/tenants/{tenantId}/projects/{projectId}", "patch")
		if err := strictDecode(b, &up); err != nil || up.RoleBindings == nil || sp.validate(t, "UpdateProject", b) != nil {
			t.Errorf("update project example: %v", err)
		}
		me := paths["/me"].(map[string]any)["get"].(map[string]any)["responses"].(map[string]any)["200"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["example"]
		b, _ = json.Marshal(me)
		var dto identityhttp.Me
		if err := strictDecode(b, &dto); err != nil || sp.validate(t, "Me", b) != nil {
			t.Errorf("me example: %v", err)
		}
		tp := paths["/tenants"].(map[string]any)["get"].(map[string]any)["responses"].(map[string]any)["200"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["example"]
		b, _ = json.Marshal(tp)
		var page identityhttp.TenantPage
		if err := strictDecode(b, &page); err != nil || sp.validate(t, "TenantPage", b) != nil {
			t.Errorf("tenant page example: %v", err)
		}
	})

	t.Run("/me returns only the verified caller", func(t *testing.T) {
		r := a.do("tok-newbie", http.MethodGet, "/me", "")
		if r.Code != http.StatusOK || r.Header().Get("X-Request-ID") == "" || r.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%d %v", r.Code, r.Header())
		}
		if err := sp.validate(t, "Me", r.Body.Bytes()); err != nil {
			t.Fatalf("schema: %v\n%s", err, r.Body.String())
		}
		var me identityhttp.Me
		if err := strictDecode(r.Body.Bytes(), &me); err != nil || me.Id != userID("newbie") || me.Subject != "newbie" || me.Issuer != issuer {
			t.Fatalf("%+v %v", me, err)
		}
		// No caller-supplied selector widens or redirects it.
		for _, q := range []string{"?userId=u_admin", "?subject=admin", "?tenantId=t_acme", "?limit=5", "?cursor=a.b"} {
			if r := a.do("tok-newbie", http.MethodGet, "/me"+q, ""); r.Code != http.StatusBadRequest {
				t.Errorf("/me%s: %d", q, r.Code)
			}
		}
		for _, h := range [][2]string{{"X-User-Id", "u_admin"}, {"X-Principal", "u_admin"}, {"X-Tenant-Id", "t_acme"}} {
			r := a.do("tok-newbie", http.MethodGet, "/me", "", h[0], h[1])
			var got identityhttp.Me
			r.decode(t, &got)
			if r.Code != 200 || got.Id != userID("newbie") {
				t.Errorf("header %s changed the caller: %+v", h[0], got)
			}
		}
	})

	t.Run("an unbound user discovers an empty tenant list and nothing else", func(t *testing.T) {
		r := a.do("tok-newbie", http.MethodGet, "/tenants", "")
		if r.Code != http.StatusOK {
			t.Fatalf("%d %s", r.Code, r.Body.String())
		}
		var page identityhttp.TenantPage
		if err := strictDecode(r.Body.Bytes(), &page); err != nil || len(page.Items) != 0 || page.NextCursor != nil || !bytes.Contains(r.Body.Bytes(), []byte(`"items":[]`)) {
			t.Fatalf("%+v %v %s", page, err, r.Body.String())
		}
		for name, path := range map[string]string{
			"tenant projects":    "/tenants/t_acme/projects",
			"tenant memberships": "/tenants/t_acme/memberships",
			"project":            "/tenants/t_acme/projects/p_inventory",
			"foreign tenant":     "/tenants/t_globex/projects",
			"missing tenant":     "/tenants/t_nowhere/projects",
		} {
			if r := a.do("tok-newbie", http.MethodGet, path, ""); r.Code != http.StatusNotFound || r.errCode(t) != "not_found" {
				t.Errorf("%s: %d %s", name, r.Code, r.Body.String())
			}
		}
	})

	t.Run("a missing tenant and a hidden tenant are indistinguishable", func(t *testing.T) {
		hidden := a.do("tok-outsider", http.MethodGet, "/tenants/t_acme/projects", "")
		missing := a.do("tok-outsider", http.MethodGet, "/tenants/t_nowhere/projects", "")
		if hidden.Code != 404 || missing.Code != 404 {
			t.Fatalf("%d %d", hidden.Code, missing.Code)
		}
		var h, m httpapi.ErrorEnvelope
		hidden.decode(t, &h)
		missing.decode(t, &m)
		if h.Error.Code != m.Error.Code || h.Error.Message != m.Error.Message {
			t.Fatalf("%+v vs %+v", h.Error, m.Error)
		}
	})

	t.Run("tenant discovery lists only the caller's tenants", func(t *testing.T) {
		for token, want := range map[string]string{"tok-admin": "t_acme", "tok-padmin": "t_acme", "tok-outsider": "t_globex"} {
			r := a.do(token, http.MethodGet, "/tenants", "")
			var page identityhttp.TenantPage
			if err := strictDecode(r.Body.Bytes(), &page); err != nil || r.Code != 200 || len(page.Items) != 1 || page.Items[0].Id != want {
				t.Errorf("%s: %d %s", token, r.Code, r.Body.String())
			}
			if err := sp.validate(t, "TenantPage", r.Body.Bytes()); err != nil {
				t.Errorf("schema: %v", err)
			}
		}
		// A project-only member is discoverable with no tenant-wide roles.
		var page identityhttp.TenantPage
		a.do("tok-padmin", http.MethodGet, "/tenants", "").decode(t, &page)
		if len(page.Items[0].Roles) != 0 {
			t.Errorf("project-only roles: %+v", page.Items[0].Roles)
		}
		for _, q := range []string{"?userId=u_admin", "?tenantId=t_acme", "?limit=0", "?limit=201", "?limit=x", "?limit=1&limit=2", "?cursor=a.b", "?cursor=a&cursor=b"} {
			if r := a.do("tok-admin", http.MethodGet, "/tenants"+q, ""); r.Code != http.StatusBadRequest {
				t.Errorf("/tenants%s: %d", q, r.Code)
			}
		}
	})

	t.Run("authentication", func(t *testing.T) {
		for name, headers := range map[string][]string{
			"no header":        nil,
			"wrong scheme":     {"Authorization", "Basic dGVzdDp0ZXN0"},
			"empty token":      {"Authorization", "Bearer "},
			"unknown token":    {"Authorization", "Bearer nope"},
			"token with space": {"Authorization", "Bearer tok-admin extra"},
			"no scheme":        {"Authorization", "tok-admin"},
		} {
			req := a.do("", http.MethodGet, "/me", "", headers...)
			mustCode(t, name, req, 401, "invalid_credential")
		}
		// Credentials only travel in the Authorization header.
		if r := a.do("", http.MethodGet, "/me?access_token=tok-admin", ""); r.Code != 401 {
			t.Errorf("query token: %d", r.Code)
		}
		if r := a.do("", http.MethodGet, "/me", "", "Cookie", "id_token=tok-admin; access_token=tok-admin"); r.Code != 401 {
			t.Errorf("cookie token: %d", r.Code)
		}
		if r := a.do("tok-admin", http.MethodGet, "/me", "", "Authorization", "bearer tok-admin"); r.Code != 200 {
			t.Errorf("lowercase scheme: %d", r.Code)
		}
		// An issuer outage is 503, never 401 and never access.
		a.v.mu.Lock()
		a.v.err = errDown
		a.v.mu.Unlock()
		r := a.do("tok-admin", http.MethodGet, "/me", "")
		mustCode(t, "issuer down", r, 503, "unavailable")
		a.v.mu.Lock()
		a.v.err = nil
		a.v.mu.Unlock()
	})

	var bindingID string
	t.Run("membership create, update and revoke follow the documented contract", func(t *testing.T) {
		body := fmt.Sprintf(`{"userId":%q,"role":"engineer"}`, userID("newbie"))
		r := a.do("tok-admin", http.MethodPost, "/tenants/t_acme/memberships", body)
		if r.Code != 201 || etag(r) != `"1"` || !strings.HasSuffix(r.Header().Get("Location"), "/memberships/"+idOf(t, r)) || r.Header().Get("X-Request-ID") == "" {
			t.Fatalf("%d %v %s", r.Code, r.Header(), r.Body.String())
		}
		if err := sp.validate(t, "Membership", r.Body.Bytes()); err != nil {
			t.Fatalf("schema: %v\n%s", err, r.Body.String())
		}
		var m identityhttp.Membership
		if err := strictDecode(r.Body.Bytes(), &m); err != nil || m.UserId != userID("newbie") || m.Role != identityhttp.Engineer || m.ProjectId != nil || m.Version != 1 {
			t.Fatalf("%+v %v", m, err)
		}
		bindingID = m.Id

		r = a.do("tok-admin", http.MethodPost, "/tenants/t_acme/memberships", body)
		mustCode(t, "duplicate", r, 409, "already_exists")

		patch := "/tenants/t_acme/memberships/" + bindingID
		mustCode(t, "no If-Match", a.do("tok-admin", http.MethodPatch, patch, `{"role":"viewer"}`), 428, "version_required")
		mustCode(t, "stale", a.do("tok-admin", http.MethodPatch, patch, `{"role":"viewer"}`, "If-Match", `"9"`), 412, "version_conflict")
		for _, h := range []string{`W/"1"`, `*`, `1`, `"1", "2"`} {
			mustCode(t, "If-Match "+h, a.do("tok-admin", http.MethodPatch, patch, `{"role":"viewer"}`, "If-Match", h), 400, "invalid_request")
		}
		r = a.do("tok-admin", http.MethodPatch, patch, `{"role":"viewer"}`, "If-Match", `"1"`)
		if r.Code != 200 || etag(r) != `"2"` {
			t.Fatalf("patch: %d %s", r.Code, r.Body.String())
		}
		r.decode(t, &m)
		if m.Role != identityhttp.Viewer || m.Version != 2 {
			t.Fatalf("%+v", m)
		}
		mustCode(t, "delete without If-Match", a.do("tok-admin", http.MethodDelete, patch, ""), 428, "version_required")
		mustCode(t, "delete stale", a.do("tok-admin", http.MethodDelete, patch, "", "If-Match", `"1"`), 412, "version_conflict")
		d := a.do("tok-admin", http.MethodDelete, patch, "", "If-Match", `"2"`)
		if d.Code != 204 || d.Body.Len() != 0 {
			t.Fatalf("delete: %d %s", d.Code, d.Body.String())
		}
		mustCode(t, "delete again", a.do("tok-admin", http.MethodDelete, patch, "", "If-Match", `"2"`), 404, "not_found")
	})

	t.Run("strict JSON and field validation", func(t *testing.T) {
		post := func(body, ct string) resp {
			return a.do("tok-admin", http.MethodPost, "/tenants/t_acme/memberships", body, "Content-Type", ct)
		}
		u := userID("newbie")
		for name, tc := range map[string]struct {
			body, ct string
			code     int
		}{
			"unknown field":       {fmt.Sprintf(`{"userId":%q,"role":"viewer","email":"x@y"}`, u), "application/json", 400},
			"issuer smuggled":     {fmt.Sprintf(`{"userId":%q,"role":"viewer","issuer":"i","subject":"s"}`, u), "application/json", 400},
			"custom role":         {fmt.Sprintf(`{"userId":%q,"role":"owner"}`, u), "application/json", 400},
			"upper-case role":     {fmt.Sprintf(`{"userId":%q,"role":"Admin"}`, u), "application/json", 400},
			"missing role":        {fmt.Sprintf(`{"userId":%q}`, u), "application/json", 400},
			"missing user":        {`{"role":"viewer"}`, "application/json", 400},
			"bad user id":         {`{"userId":"a b","role":"viewer"}`, "application/json", 400},
			"unknown user":        {`{"userId":"u_nobody","role":"viewer"}`, "application/json", 400},
			"two documents":       {fmt.Sprintf(`{"userId":%q,"role":"viewer"}{"userId":%q,"role":"viewer"}`, u, u), "application/json", 400},
			"wrong type":          {fmt.Sprintf(`{"userId":%q,"role":5}`, u), "application/json", 400},
			"text/plain":          {fmt.Sprintf(`{"userId":%q,"role":"viewer"}`, u), "text/plain", 415},
			"unknown project":     {fmt.Sprintf(`{"userId":%q,"role":"viewer","projectId":"p_missing"}`, u), "application/json", 404},
			"another tenant's id": {fmt.Sprintf(`{"userId":%q,"role":"viewer","projectId":%q}`, u, projectB), "application/json", 404},
		} {
			if r := post(tc.body, tc.ct); r.Code != tc.code {
				t.Errorf("%s: %d %s", name, r.Code, r.Body.String())
			}
		}
		big := fmt.Sprintf(`{"userId":%q,"role":"viewer","pad":"%s"}`, u, strings.Repeat("x", 2<<20))
		if r := post(big, "application/json"); r.Code != 413 {
			t.Errorf("oversized: %d", r.Code)
		}
		if r := a.do("tok-admin", http.MethodPatch, "/tenants/t_acme/memberships/not%20valid", `{"role":"viewer"}`, "If-Match", `"1"`); r.Code != 400 {
			t.Errorf("malformed id: %d", r.Code)
		}
		if r := a.do("tok-admin", http.MethodPost, "/tenants/not%20valid/memberships", `{}`); r.Code != 400 {
			t.Errorf("malformed tenant: %d", r.Code)
		}
	})

	t.Run("denial classes: 403 for members, 404 for everyone else", func(t *testing.T) {
		body := fmt.Sprintf(`{"userId":%q,"role":"viewer"}`, userID("newbie"))
		mustCode(t, "viewer creates a binding", a.do("tok-viewer", http.MethodPost, "/tenants/t_acme/memberships", body), 403, "forbidden")
		mustCode(t, "viewer lists bindings", a.do("tok-viewer", http.MethodGet, "/tenants/t_acme/memberships", ""), 403, "forbidden")
		mustCode(t, "outsider creates a binding", a.do("tok-outsider", http.MethodPost, "/tenants/t_acme/memberships", body), 404, "not_found")
		mustCode(t, "project admin: tenant-wide grant", a.do("tok-padmin", http.MethodPost, "/tenants/t_acme/memberships", body), 403, "forbidden")
		mustCode(t, "project admin: other project", a.do("tok-padmin", http.MethodPost, "/tenants/t_acme/memberships",
			fmt.Sprintf(`{"userId":%q,"role":"viewer","projectId":%q}`, userID("newbie"), projectA2)), 404, "not_found")
		mustCode(t, "viewer creates a project", a.do("tok-viewer", http.MethodPost, "/tenants/t_acme/projects", `{"key":"x"}`), 403, "forbidden")
		mustCode(t, "project admin creates a project", a.do("tok-padmin", http.MethodPost, "/tenants/t_acme/projects", `{"key":"x"}`), 403, "forbidden")
		mustCode(t, "viewer edits a project", a.do("tok-viewer", http.MethodPatch, "/tenants/t_acme/projects/p_inventory", `{"displayName":"x"}`, "If-Match", `"1"`), 403, "forbidden")
		mustCode(t, "reviewer reads another project", a.do("tok-reviewer", http.MethodGet, "/tenants/t_acme/projects/p_inventory", ""), 404, "not_found")
		r := a.do("tok-reviewer", http.MethodGet, "/tenants/t_acme/projects/p_billing", "")
		if r.Code != 200 {
			t.Errorf("reviewer reads own project: %d", r.Code)
		}
	})

	t.Run("memberships list and cursors", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			a.mem.AddUser(identity.UserID(fmt.Sprintf("u_bulk%d", i)), issuer, fmt.Sprintf("bulk%d", i))
			a.mem.AddBinding(tenantA, identity.BindingID(fmt.Sprintf("mb_bulk%d", i)), identity.UserID(fmt.Sprintf("u_bulk%d", i)), projectA, identity.RoleViewer)
		}
		var seen []string
		cursor := ""
		pages := 0
		for {
			path := "/tenants/t_acme/memberships?limit=3"
			if cursor != "" {
				path += "&cursor=" + cursor
			}
			r := a.do("tok-admin", http.MethodGet, path, "")
			if r.Code != 200 {
				t.Fatalf("%d %s", r.Code, r.Body.String())
			}
			if err := sp.validate(t, "MembershipPage", r.Body.Bytes()); err != nil {
				t.Fatalf("schema: %v", err)
			}
			var page identityhttp.MembershipPage
			if err := strictDecode(r.Body.Bytes(), &page); err != nil || len(page.Items) > 3 {
				t.Fatalf("%v %s", err, r.Body.String())
			}
			for _, m := range page.Items {
				seen = append(seen, m.Id)
			}
			pages++
			if page.NextCursor == nil {
				break
			}
			cursor = *page.NextCursor
			if pages == 1 {
				// A copied cursor grants nothing: another principal cannot use it, and a different filter
				// or tenant invalidates it.
				mustCode(t, "foreign principal", a.do("tok-padmin", http.MethodGet, "/tenants/t_acme/memberships?limit=3&cursor="+cursor, ""), 400, "invalid_cursor")
				mustCode(t, "other filter", a.do("tok-admin", http.MethodGet, "/tenants/t_acme/memberships?limit=3&projectId=p_inventory&cursor="+cursor, ""), 400, "invalid_cursor")
				mustCode(t, "other limit sort", a.do("tok-admin", http.MethodGet, "/tenants/t_acme/projects?limit=3&cursor="+cursor, ""), 400, "invalid_cursor")
				mustCode(t, "tampered", a.do("tok-admin", http.MethodGet, "/tenants/t_acme/memberships?limit=3&cursor="+cursor+"A", ""), 400, "invalid_cursor")
			}
		}
		if len(seen) < 8 || !sort.StringsAreSorted(seen) {
			t.Fatalf("pages: %v", seen)
		}
		uniq := map[string]bool{}
		for _, id := range seen {
			if uniq[id] {
				t.Fatalf("duplicate %s across pages", id)
			}
			uniq[id] = true
		}
		// A project admin sees only its own project's bindings, with or without filters.
		for _, q := range []string{"", "?projectId=p_inventory", "?projectId=p_billing", "?userId=" + userID("admin")} {
			r := a.do("tok-padmin", http.MethodGet, "/tenants/t_acme/memberships"+q, "")
			var page identityhttp.MembershipPage
			r.decode(t, &page)
			for _, m := range page.Items {
				if m.ProjectId == nil || *m.ProjectId != projectA {
					t.Errorf("project admin sees %+v with filter %q", m, q)
				}
			}
		}
		for _, q := range []string{"?limit=0", "?limit=201", "?projectId=a%20b", "?projectId=p_a&projectId=p_b", "?role=admin", "?tenantId=t_globex"} {
			if r := a.do("tok-admin", http.MethodGet, "/tenants/t_acme/memberships"+q, ""); r.Code != 400 {
				t.Errorf("%s: %d", q, r.Code)
			}
		}
	})

	t.Run("projects", func(t *testing.T) {
		r := a.do("tok-admin", http.MethodPost, "/tenants/t_acme/projects", `{"key":"newapp","displayName":"New app","settings":{"region":"eu"},"activeWipLimit":3}`)
		if r.Code != 201 || etag(r) != `"1"` || !strings.HasSuffix(r.Header().Get("Location"), "/projects/"+idOf(t, r)) {
			t.Fatalf("%d %v %s", r.Code, r.Header(), r.Body.String())
		}
		if err := sp.validate(t, "Project", r.Body.Bytes()); err != nil {
			t.Fatalf("schema: %v\n%s", err, r.Body.String())
		}
		var p identityhttp.Project
		if err := strictDecode(r.Body.Bytes(), &p); err != nil || p.Key != "newapp" || p.ActiveWipLimit != 3 || p.Version != 1 || string(p.Settings) != `{"region":"eu"}` || len(p.CallerRoles) != 1 {
			t.Fatalf("%+v %v", p, err)
		}
		mustCode(t, "duplicate key", a.do("tok-admin", http.MethodPost, "/tenants/t_acme/projects", `{"key":"newapp"}`), 409, "already_exists")
		for name, body := range map[string]string{
			"bad key": `{"key":"a/b"}`, "no key": `{}`, "zero wip": `{"key":"zz","activeWipLimit":0}`, "huge wip": `{"key":"zz","activeWipLimit":100001}`,
			"array settings": `{"key":"zz","settings":[]}`, "unknown": `{"key":"zz","id":"p_x"}`, "bad member": `{"key":"zz","settings":{"1x":1}}`,
		} {
			mustCode(t, name, a.do("tok-admin", http.MethodPost, "/tenants/t_acme/projects", body), 400, "invalid_request")
		}

		path := "/tenants/t_acme/projects/" + p.Id
		mustCode(t, "no If-Match", a.do("tok-admin", http.MethodPatch, path, `{"displayName":"x"}`), 428, "version_required")
		mustCode(t, "stale", a.do("tok-admin", http.MethodPatch, path, `{"displayName":"x"}`, "If-Match", `"4"`), 412, "version_conflict")
		for name, body := range map[string]string{
			"key immutable": `{"key":"other"}`, "id": `{"id":"p_x"}`, "version": `{"version":5}`, "tenant": `{"tenantId":"t_globex"}`,
			"wip": `{"activeWipLimit":0}`, "bindings tenant-wide": `{"roleBindings":{"grant":[{"userId":"u_viewer","role":"viewer","projectId":""}]}}`,
			"bad role": `{"roleBindings":{"grant":[{"userId":"u_viewer","role":"owner"}]}}`,
		} {
			mustCode(t, name, a.do("tok-admin", http.MethodPatch, path, body, "If-Match", `"1"`), 400, "")
		}
		r = a.do("tok-admin", http.MethodPatch, path, fmt.Sprintf(`{"displayName":"Renamed","settings":{"a":1},"activeWipLimit":9,"roleBindings":{"grant":[{"userId":%q,"role":"analyst"}]}}`, userID("viewer")), "If-Match", `"1"`)
		if r.Code != 200 || etag(r) != `"2"` {
			t.Fatalf("patch: %d %s", r.Code, r.Body.String())
		}
		r.decode(t, &p)
		if p.DisplayName != "Renamed" || p.ActiveWipLimit != 9 || p.Version != 2 || string(p.Settings) != `{"a":1}` {
			t.Fatalf("%+v", p)
		}
		// An empty patch is a no-op that does not advance the version.
		r = a.do("tok-admin", http.MethodPatch, path, `{}`, "If-Match", `"2"`)
		if r.Code != 200 || etag(r) != `"2"` {
			t.Fatalf("empty patch: %d %s", r.Code, r.Body.String())
		}
		// The new binding went through the membership transition: one change per entry point.
		if got := a.do("tok-admin", http.MethodGet, "/tenants/t_acme/memberships?projectId="+p.Id, ""); !strings.Contains(got.Body.String(), `"analyst"`) {
			t.Fatalf("binding missing: %s", got.Body.String())
		}

		list := a.do("tok-reviewer", http.MethodGet, "/tenants/t_acme/projects", "")
		var page identityhttp.ProjectPage
		if err := strictDecode(list.Body.Bytes(), &page); err != nil || list.Code != 200 || len(page.Items) != 1 || page.Items[0].Id != projectA2 {
			t.Fatalf("%d %s", list.Code, list.Body.String())
		}
		if err := sp.validate(t, "ProjectPage", list.Body.Bytes()); err != nil {
			t.Fatalf("schema: %v", err)
		}
		all := a.do("tok-admin", http.MethodGet, "/tenants/t_acme/projects?limit=2", "")
		var first identityhttp.ProjectPage
		all.decode(t, &first)
		if len(first.Items) != 2 || first.NextCursor == nil {
			t.Fatalf("%s", all.Body.String())
		}
		mustCode(t, "project cursor copied to another principal", a.do("tok-reviewer", http.MethodGet, "/tenants/t_acme/projects?limit=2&cursor="+*first.NextCursor, ""), 400, "invalid_cursor")
		get := a.do("tok-admin", http.MethodGet, path, "")
		if get.Code != 200 || etag(get) != `"2"` {
			t.Fatalf("%d", get.Code)
		}
	})

	t.Run("an outage is 503, never empty discovery or access", func(t *testing.T) {
		a.mem.Down = errDown
		defer func() { a.mem.Down = nil }()
		for _, path := range []string{"/me", "/tenants", "/tenants/t_acme/projects", "/tenants/t_acme/memberships", "/tenants/t_acme/projects/p_inventory"} {
			r := a.do("tok-admin", http.MethodGet, path, "")
			mustCode(t, path, r, 503, "unavailable")
			if strings.Contains(r.Body.String(), errDown.Error()) {
				t.Errorf("%s leaks the cause: %s", path, r.Body.String())
			}
		}
		mustCode(t, "mutation", a.do("tok-admin", http.MethodPost, "/tenants/t_acme/memberships", fmt.Sprintf(`{"userId":%q,"role":"viewer"}`, userID("newbie"))), 503, "unavailable")
	})

	t.Run("a failed audit or event write fails the whole request", func(t *testing.T) {
		before := len(a.mem.Changes)
		a.mem.FailRecord = errors.New("event backlog full")
		r := a.do("tok-admin", http.MethodPost, "/tenants/t_acme/memberships", fmt.Sprintf(`{"userId":%q,"role":"analyst"}`, userID("newbie")))
		if r.Code != 500 && r.Code != 503 {
			t.Fatalf("%d %s", r.Code, r.Body.String())
		}
		if len(a.mem.Changes) != before {
			t.Fatal("a change was recorded for a failed request")
		}
		var got identityhttp.MembershipPage
		a.do("tok-admin", http.MethodGet, "/tenants/t_acme/memberships?userId="+userID("newbie"), "").decode(t, &got)
		if len(got.Items) != 0 {
			t.Fatalf("binding survived a failed record: %+v", got)
		}
	})

	t.Run("revocation takes effect for the next request with the same token", func(t *testing.T) {
		if r := a.do("tok-viewer", http.MethodGet, "/tenants/t_acme/projects/p_inventory", ""); r.Code != 200 {
			t.Fatalf("%d", r.Code)
		}
		r := a.do("tok-admin", http.MethodDelete, "/tenants/t_acme/memberships/mb_viewer", "", "If-Match", `"1"`)
		if r.Code != 204 {
			t.Fatalf("%d %s", r.Code, r.Body.String())
		}
		mustCode(t, "after revocation", a.do("tok-viewer", http.MethodGet, "/tenants/t_acme/projects/p_inventory", ""), 404, "not_found")
		if r := a.do("tok-viewer", http.MethodGet, "/tenants/t_acme/projects", ""); strings.Contains(r.Body.String(), `"p_inventory"`) {
			t.Fatalf("project still discoverable: %s", r.Body.String())
		}
	})
}

func idOf(t testing.TB, r resp) string {
	t.Helper()
	var v struct {
		Id string `json:"id"`
	}
	r.decode(t, &v)
	return v.Id
}
