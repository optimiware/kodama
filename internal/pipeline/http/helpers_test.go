package pipelinehttp_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/pipeline"
	pipelinehttp "github.com/optimiware/kodama/internal/pipeline/http"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

// Resource prefix of this task's test fixtures.
const prefix = "t014"

const (
	tenantA, projectA = "t_acme", "p_inventory"
	tenantB, projectB = "t_globex", "p_billing"
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

// routeAuthorizer records the router-level calls; a nil table allows
// everything and the use cases enforce the fine-grained permissions.
type routeAuthorizer struct {
	allow func(p httpapi.Principal, a httpapi.Action, t httpapi.Target) error
	calls []httpapi.Action
}

func (r *routeAuthorizer) Authorize(_ context.Context, p httpapi.Principal, a httpapi.Action, t httpapi.Target) error {
	r.calls = append(r.calls, a)
	if r.allow == nil {
		return nil
	}
	return r.allow(p, a, t)
}

type api struct {
	t      testing.TB
	router *httpapi.Router
	authz  *routeAuthorizer
	auth   *pipelinetest.Authority
	repo   *pipelinetest.MemRepo
}

func newAPI(t testing.TB, kinds ...pipeline.StepKind) *api {
	t.Helper()
	a := &api{t: t, authz: &routeAuthorizer{}, auth: pipelinetest.NewAuthority(), repo: &pipelinetest.MemRepo{}}
	svc, err := pipeline.NewService(pipeline.ServiceConfig{Repository: a.repo, Authority: a.auth, Validator: pipelinetest.NewValidator(t, kinds...)})
	if err != nil {
		t.Fatal(err)
	}
	a.auth.Grant(scopeA, "u_engineer", pipeline.ActorUser, pipeline.PermList, pipeline.PermRegister, pipeline.PermRead)
	a.auth.Grant(scopeA, "u_reader", pipeline.ActorUser, pipeline.PermList, pipeline.PermRead)
	a.auth.Grant(scopeB, "u_engineer", pipeline.ActorUser, pipeline.PermList, pipeline.PermRegister, pipeline.PermRead)
	a.router = a.build(svc, strings.Repeat("k", 32))
	return a
}

func (a *api) build(svc *pipeline.Service, key string) *httpapi.Router {
	a.t.Helper()
	toks := tokens{"tok-engineer": "u_engineer", "tok-reader": "u_reader", "tok-stranger": "u_stranger"}
	rt, err := httpapi.NewRouter(httpapi.RouterConfig{Authenticator: toks, Authorizer: a.authz, RateLimiter: httpapi.Unlimited(), Prefix: "/api/v1"})
	if err != nil {
		a.t.Fatal(err)
	}
	codec, err := httpapi.NewCursorCodec([]byte(key))
	if err != nil {
		a.t.Fatal(err)
	}
	h, err := pipelinehttp.NewHandler(svc, codec)
	if err != nil {
		a.t.Fatal(err)
	}
	if err := h.RegisterRoutes(rt); err != nil {
		a.t.Fatal(err)
	}
	return rt
}

type resp struct {
	*httptest.ResponseRecorder
}

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
	return "/tenants/" + tenant + "/projects/" + project + "/pipeline-definitions"
}

const imageRef = "registry.example.invalid/go-tools@sha256:1111111111111111111111111111111111111111111111111111111111111111"

// jsonDoc builds a JSON document named name from raw step objects.
func jsonDoc(name string, steps ...string) string {
	return `{"apiVersion":"kodama/v1alpha1","kind":"Pipeline","metadata":{"name":"` + name + `"},"spec":{"steps":[` + strings.Join(steps, ",") + `]}}`
}

// jsonStep builds one container step; extra is a raw `,"key":value` suffix.
func jsonStep(id, extra string) string {
	return `{"id":"` + id + `","kind":"container","image":"` + imageRef + `","command":["/bin/sh","-ec"],"timeoutSeconds":60` + extra + `}`
}

type definitionBody struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Version    int64           `json:"version"`
	Digest     string          `json:"digest"`
	Definition json.RawMessage `json:"definition"`
	CreatedBy  struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	} `json:"createdBy"`
	CreatedAt string `json:"createdAt"`
}
