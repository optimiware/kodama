package mcpproxyhttp_test

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

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/mcpproxy"
	mcpproxyhttp "github.com/optimiware/kodama/internal/mcpproxy/http"
	"github.com/optimiware/kodama/internal/mcpproxy/mcptest"
)

const (
	mcpYAML    = "../../../api/mcp.yaml"
	commonYAML = "../../../api/common.yaml"
)

// ---- the OpenAPI fragment as a validator ----

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
	for name, path := range map[string]string{"common.yaml": commonYAML, "mcp.yaml": mcpYAML} {
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
		if name == "mcp.yaml" {
			out = raw
		}
	}
	return &spec{raw: out, c: c}
}

func (s *spec) validate(t testing.TB, schema string, doc []byte) error {
	t.Helper()
	sch, err := s.c.Compile("mcp.yaml#/components/schemas/" + schema)
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

// ---- the router ----

type tokens map[string]string

func (t tokens) Authenticate(_ context.Context, r *http.Request) (httpapi.Principal, error) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	id, ok := t[tok]
	if !ok {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	return httpapi.Principal{ID: id}, nil
}

type recordingAuthorizer struct{ actions []httpapi.Action }

func (r *recordingAuthorizer) Authorize(_ context.Context, _ httpapi.Principal, a httpapi.Action, _ httpapi.Target) error {
	r.actions = append(r.actions, a)
	return nil
}

type api struct {
	t      testing.TB
	s      *mcptest.Stack
	router *httpapi.Router
	authz  *recordingAuthorizer
	scope  domain.Scope
	sec    mcpproxy.SecretRef
}

func newAPI(t testing.TB) *api {
	t.Helper()
	s := mcptest.New(t, mcptest.Config{})
	w := s.NewWorkload(mcptest.WorkloadSpec{})
	a := &api{t: t, s: s, authz: &recordingAuthorizer{}, scope: w.Scope}
	a.sec = s.Credential(w.Scope, "remote-token", s.Fixture.Origin(), s.Fixture.Token())
	s.Human.Grant("u_admin", w.Scope, mcpproxy.PermList, mcpproxy.PermMutate)
	s.Human.Grant("u_reader", w.Scope, mcpproxy.PermList)
	rt, err := httpapi.NewRouter(httpapi.RouterConfig{
		Authenticator: tokens{"tok-admin": "u_admin", "tok-reader": "u_reader", "tok-stranger": "u_stranger"},
		Authorizer:    a.authz, RateLimiter: httpapi.Unlimited(), Prefix: "/api/v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	codec, err := httpapi.NewCursorCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	h, err := mcpproxyhttp.NewHandler(s.Service, codec)
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

func (r resp) errCode(t testing.TB) string {
	t.Helper()
	var e httpapi.ErrorEnvelope
	if err := json.Unmarshal(r.Body.Bytes(), &e); err != nil || e.Error.RequestId == "" || r.Header().Get("X-Request-ID") != e.Error.RequestId {
		t.Fatalf("not an error envelope: %s", r.Body.String())
	}
	return e.Error.Code
}

func (a *api) base() string {
	return "/tenants/" + string(a.scope.TenantID) + "/projects/" + string(a.scope.ProjectID) + "/mcp-servers"
}

func (a *api) draftBody(tools ...string) string {
	b, _ := json.Marshal(map[string]any{
		"endpoint": a.s.Fixture.URL(), "credential": map[string]any{"name": a.sec.Name, "version": a.sec.Version}, "tools": tools,
	})
	return string(b)
}

// Test_http_contract: OpenAPI/DTO agreement, route audiences, management
// permissions, ETags, cursors, nondisclosure and bounded error mapping.
func Test_http_contract(t *testing.T) {
	sp := loadSpec(t)

	t.Run("the documented examples validate against the fragment's own schemas", func(t *testing.T) {
		if err := sp.validate(t, "McpServerDraft", sp.example(t, "McpServerDraft")); err != nil {
			t.Fatalf("draft: %v", err)
		}
		if err := sp.validate(t, "McpServer", sp.example(t, "McpServer")); err != nil {
			t.Fatalf("server: %v", err)
		}
	})

	a := newAPI(t)
	var created mcpproxyhttp.McpServer

	t.Run("create returns version 1 with an ETag and the response matches the schema", func(t *testing.T) {
		res := a.do("tok-admin", http.MethodPost, a.base(), a.draftBody("echo", "mutate"))
		if res.Code != http.StatusCreated {
			t.Fatalf("status %d: %s", res.Code, res.Body.String())
		}
		if err := sp.validate(t, "McpServer", res.Body.Bytes()); err != nil {
			t.Fatalf("schema: %v\n%s", err, res.Body.String())
		}
		if res.Header().Get("ETag") != `"1"` {
			t.Fatalf("ETag %q", res.Header().Get("ETag"))
		}
		if err := json.Unmarshal(res.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(res.Body.String(), a.s.Fixture.Token()) {
			t.Fatal("a credential value is in the response")
		}
	})

	t.Run("the router receives the documented actions", func(t *testing.T) {
		if len(a.authz.actions) == 0 || a.authz.actions[0] != mcpproxyhttp.ActionMutate {
			t.Fatalf("actions %v", a.authz.actions)
		}
	})

	t.Run("management permissions: a reader lists but cannot mutate; a stranger sees nothing", func(t *testing.T) {
		if res := a.do("tok-reader", http.MethodGet, a.base(), ""); res.Code != http.StatusOK {
			t.Fatalf("reader list: %d", res.Code)
		}
		res := a.do("tok-reader", http.MethodPost, a.base(), a.draftBody("echo"))
		if res.Code != http.StatusNotFound { // the test authority hides what it does not grant
			t.Fatalf("reader create: %d %s", res.Code, res.Body.String())
		}
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			res := a.do("tok-stranger", method, a.base(), a.draftBody("echo"))
			if res.Code != http.StatusNotFound || res.errCode(t) != "not_found" {
				t.Fatalf("stranger %s: %d %s", method, res.Code, res.Body.String())
			}
		}
		if res := a.do("", http.MethodGet, a.base(), ""); res.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous: %d", res.Code)
		}
		if res := a.do("tok-reader", http.MethodPatch, a.base()+"/"+string(created.Id), `{"tools":["echo"]}`, "If-Match", `"1"`); res.Code != http.StatusNotFound {
			t.Fatalf("reader patch: %d", res.Code)
		}
	})

	t.Run("strict decoding and validation precede any state", func(t *testing.T) {
		for name, body := range map[string]string{
			"unknown field": `{"endpoint":"https://x.example.com/","credential":{"name":"remote-token","version":1},"tools":[],"extra":1}`,
			"duplicate key": `{"endpoint":"https://x.example.com/","endpoint":"https://y.example.com/","credential":{"name":"remote-token","version":1},"tools":[]}`,
			"trailing data": a.draftBody("echo") + ` {}`,
			"empty":         ``,
		} {
			res := a.do("tok-admin", http.MethodPost, a.base(), body)
			want := http.StatusBadRequest
			if name == "empty" {
				want = http.StatusUnsupportedMediaType // no body, no content type
			}
			if res.Code != want {
				t.Errorf("%s: %d %s", name, res.Code, res.Body.String())
			}
		}
		for name, body := range map[string]string{
			"http endpoint":       `{"endpoint":"http://x.example.com/","credential":{"name":"remote-token","version":1},"tools":[]}`,
			"userinfo":            `{"endpoint":"https://u:p@x.example.com/","credential":{"name":"remote-token","version":1},"tools":[]}`,
			"unapproved host":     `{"endpoint":"https://unapproved.example.com/mcp","credential":{"name":"remote-token","version":1},"tools":[]}`,
			"missing credential":  `{"endpoint":"` + a.s.Fixture.URL() + `","credential":{"name":"nope","version":1},"tools":[]}`,
			"wildcard tool":       `{"endpoint":"` + a.s.Fixture.URL() + `","credential":{"name":"remote-token","version":1},"tools":["*"]}`,
			"qualified tool name": `{"endpoint":"` + a.s.Fixture.URL() + `","credential":{"name":"remote-token","version":1},"tools":["a/b"]}`,
		} {
			res := a.do("tok-admin", http.MethodPost, a.base(), body)
			if res.Code != http.StatusUnprocessableEntity || res.errCode(t) != "invalid_mcp_server" {
				t.Errorf("%s: %d %s", name, res.Code, res.Body.String())
			}
			if strings.Contains(res.Body.String(), "unapproved.example.com") {
				t.Errorf("%s: the error echoed the endpoint", name)
			}
		}
	})

	t.Run("patch needs If-Match, applies partial updates and bumps the ETag", func(t *testing.T) {
		path := a.base() + "/" + string(created.Id)
		if res := a.do("tok-admin", http.MethodPatch, path, `{"tools":["echo"]}`); res.Code != http.StatusPreconditionRequired {
			t.Fatalf("missing If-Match: %d", res.Code)
		}
		if res := a.do("tok-admin", http.MethodPatch, path, `{"tools":["echo"]}`, "If-Match", `W/"1"`); res.Code != http.StatusBadRequest {
			t.Fatalf("weak tag: %d", res.Code)
		}
		if res := a.do("tok-admin", http.MethodPatch, path, `{}`, "If-Match", `"1"`); res.Code != http.StatusBadRequest {
			t.Fatalf("empty patch: %d", res.Code)
		}
		res := a.do("tok-admin", http.MethodPatch, path, `{"tools":["echo"]}`, "If-Match", `"1"`)
		if res.Code != http.StatusOK || res.Header().Get("ETag") != `"2"` {
			t.Fatalf("patch: %d %s etag %q", res.Code, res.Body.String(), res.Header().Get("ETag"))
		}
		if err := sp.validate(t, "McpServer", res.Body.Bytes()); err != nil {
			t.Fatalf("schema: %v", err)
		}
		var got mcpproxyhttp.McpServer
		_ = json.Unmarshal(res.Body.Bytes(), &got)
		if got.Endpoint != created.Endpoint || len(got.Tools) != 1 || got.Credential != created.Credential {
			t.Fatalf("an omitted field changed: %+v", got)
		}
		if res := a.do("tok-admin", http.MethodPatch, path, `{"tools":["add"]}`, "If-Match", `"1"`); res.Code != http.StatusPreconditionFailed || res.errCode(t) != "version_conflict" {
			t.Fatalf("stale version: %d %s", res.Code, res.Body.String())
		}
	})

	t.Run("a foreign project's registration is not found", func(t *testing.T) {
		other := "/tenants/" + string(a.scope.TenantID) + "/projects/p033_other/mcp-servers/" + string(created.Id)
		a.s.Human.Grant("u_admin", domain.Scope{TenantID: a.scope.TenantID, ProjectID: "p033_other"}, mcpproxy.PermList, mcpproxy.PermMutate)
		if res := a.do("tok-admin", http.MethodPatch, other, `{"tools":["echo"]}`, "If-Match", `"2"`); res.Code != http.StatusNotFound {
			t.Fatalf("got %d: %s", res.Code, res.Body.String())
		}
	})

	t.Run("list pages with a bound cursor and never lists tombstones", func(t *testing.T) {
		for i := 0; i < 2; i++ {
			if res := a.do("tok-admin", http.MethodPost, a.base(), a.draftBody("echo")); res.Code != http.StatusCreated {
				t.Fatalf("create: %d", res.Code)
			}
		}
		res := a.do("tok-admin", http.MethodGet, a.base()+"?limit=2", "")
		if res.Code != http.StatusOK {
			t.Fatalf("list: %d %s", res.Code, res.Body.String())
		}
		var p mcpproxyhttp.McpServerPage
		_ = json.Unmarshal(res.Body.Bytes(), &p)
		if len(p.Items) != 2 || p.NextCursor == nil {
			t.Fatalf("page %+v", p)
		}
		if err := sp.validate(t, "McpServerPage", res.Body.Bytes()); err != nil {
			t.Fatalf("schema: %v", err)
		}
		next := a.do("tok-admin", http.MethodGet, a.base()+"?limit=2&cursor="+*p.NextCursor, "")
		if next.Code != http.StatusOK {
			t.Fatalf("next page: %d", next.Code)
		}
		// A cursor does not move to another project or endpoint.
		a.s.Human.Grant("u_admin", domain.Scope{TenantID: a.scope.TenantID, ProjectID: "p033_other"}, mcpproxy.PermList)
		cross := a.do("tok-admin", http.MethodGet, "/tenants/"+string(a.scope.TenantID)+"/projects/p033_other/mcp-servers?limit=2&cursor="+*p.NextCursor, "")
		if cross.Code != http.StatusBadRequest {
			t.Fatalf("a cursor crossed projects: %d", cross.Code)
		}
	})

	t.Run("delete tombstones: it disappears from lists and cannot be changed", func(t *testing.T) {
		path := a.base() + "/" + string(created.Id)
		if res := a.do("tok-admin", http.MethodDelete, path, "", "If-Match", `"1"`); res.Code != http.StatusPreconditionFailed {
			t.Fatalf("stale delete: %d", res.Code)
		}
		if res := a.do("tok-admin", http.MethodDelete, path, ""); res.Code != http.StatusPreconditionRequired {
			t.Fatalf("delete without If-Match: %d", res.Code)
		}
		if res := a.do("tok-admin", http.MethodDelete, path, "", "If-Match", `"2"`); res.Code != http.StatusNoContent {
			t.Fatalf("delete: %d %s", res.Code, res.Body.String())
		}
		if res := a.do("tok-admin", http.MethodDelete, path, "", "If-Match", `"3"`); res.Code != http.StatusNotFound {
			t.Fatalf("second delete: %d", res.Code)
		}
		if res := a.do("tok-admin", http.MethodPatch, path, `{"tools":["echo"]}`, "If-Match", `"3"`); res.Code != http.StatusNotFound {
			t.Fatalf("patch of a tombstone: %d", res.Code)
		}
		list := a.do("tok-admin", http.MethodGet, a.base(), "")
		if strings.Contains(list.Body.String(), string(created.Id)) {
			t.Fatal("a tombstone is listed")
		}
	})

	t.Run("the routes are exactly the documented ones", func(t *testing.T) {
		paths := sp.raw["paths"].(map[string]any)
		want := map[string]bool{"/tenants/{tenantId}/projects/{projectId}/mcp-servers": true, "/tenants/{tenantId}/projects/{projectId}/mcp-servers/{id}": true,
			"/internal/mcp/v1/attempts/{attemptId}/servers/{registrationId}": true}
		for p := range paths {
			if !want[p] {
				t.Errorf("undocumented path %s", p)
			}
		}
		// There is no GET-by-id and no PUT: a method the router does not serve is refused.
		path := a.base() + "/" + string(created.Id)
		for _, m := range []string{http.MethodGet, http.MethodPut} {
			if res := a.do("tok-admin", m, path, `{}`); res.Code == http.StatusOK {
				t.Errorf("%s %s answered 200", m, path)
			}
		}
	})
}
