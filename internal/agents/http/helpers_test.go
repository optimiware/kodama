package agentshttp_test

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

	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/agents/agentstest"
	agentshttp "github.com/optimiware/kodama/internal/agents/http"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
)

const (
	agentsYAML = "../../../api/agents.yaml"
	commonYAML = "../../../api/common.yaml"
)

const (
	tenantA, projectA = "t030_acme", "p030_inventory"
	tenantB, projectB = "t030_globex", "p030_billing"
)

var (
	scopeA = domain.Scope{TenantID: tenantA, ProjectID: projectA}
	scopeB = domain.Scope{TenantID: tenantB, ProjectID: projectB}
)

// tokens maps bearer tokens to principals; the router authenticates, never the
// request body.
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

// routeAuthorizer allows every route and records the actions; the use cases
// enforce the fine-grained permissions themselves.
type routeAuthorizer struct{ calls []httpapi.Action }

func (r *routeAuthorizer) Authorize(_ context.Context, _ httpapi.Principal, a httpapi.Action, _ httpapi.Target) error {
	r.calls = append(r.calls, a)
	return nil
}

type api struct {
	t         testing.TB
	router    *httpapi.Router
	authz     *routeAuthorizer
	auth      *agentstest.Authority
	repo      *agentstest.MemRepo
	approvals *agentstest.Approvals
	draft     agents.Draft
}

func newAPI(t testing.TB) *api {
	t.Helper()
	return newAPIWith(t, agentstest.Draft(t, "coding"), agentstest.NewApprovals())
}

// newAPIWith qualifies the tuple of draft in the catalogue and uses approvals.
func newAPIWith(t testing.TB, draft agents.Draft, approvals *agentstest.Approvals) *api {
	t.Helper()
	a := &api{t: t, authz: &routeAuthorizer{}, auth: agentstest.NewAuthority(), repo: &agentstest.MemRepo{}, approvals: approvals, draft: draft}
	n := 0
	svc, err := agents.NewService(agents.ServiceConfig{
		Repository: a.repo, Authority: a.auth, Compatibility: agentstest.Catalogue(t, a.draft), Approvals: a.approvals,
		Clock: func() time.Time { n++; return time.Date(2026, 10, 20, 9, 0, n, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	a.auth.Grant(scopeA, "u_engineer", agents.ActorUser, agents.PermList, agents.PermRegister)
	a.auth.Grant(scopeA, "u_reader", agents.ActorUser, agents.PermList)
	a.auth.Grant(scopeB, "u_engineer", agents.ActorUser, agents.PermList, agents.PermRegister)
	toks := tokens{"tok-engineer": "u_engineer", "tok-reader": "u_reader", "tok-stranger": "u_stranger"}
	rt, err := httpapi.NewRouter(httpapi.RouterConfig{Authenticator: toks, Authorizer: a.authz, RateLimiter: httpapi.Unlimited(), Prefix: "/api/v1"})
	if err != nil {
		t.Fatal(err)
	}
	codec, err := httpapi.NewCursorCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	h, err := agentshttp.NewHandler(svc, codec)
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
		t.Fatalf("response is not the expected JSON: %v: %s", err, r.Body.String())
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

func base(tenant, project string) string {
	return "/tenants/" + tenant + "/projects/" + project + "/agent-profiles"
}

func (a *api) body(mutate func(map[string]any)) string {
	a.t.Helper()
	raw, err := json.Marshal(a.draft)
	if err != nil {
		a.t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		a.t.Fatal(err)
	}
	if mutate != nil {
		mutate(m)
	}
	out, err := json.Marshal(m)
	if err != nil {
		a.t.Fatal(err)
	}
	return string(out)
}

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

// translate rewrites OpenAPI 3.0 nullability into JSON Schema so the fragment can
// validate documents.
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
	for name, path := range map[string]string{"common.yaml": commonYAML, "agents.yaml": agentsYAML} {
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
		if name == "agents.yaml" {
			out = raw
		}
	}
	return &spec{raw: out, c: c}
}

func (s *spec) validate(t testing.TB, schema string, doc []byte) error {
	t.Helper()
	sch, err := s.c.Compile("agents.yaml#/components/schemas/" + schema)
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
