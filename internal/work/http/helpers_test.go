package workhttp_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/work"
	workhttp "github.com/optimiware/kodama/internal/work/http"
)

const (
	tenantA, projectA = "t_acme", "p_inventory"
	tenantB, projectB = "t_globex", "p_billing"
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

// routeAuthorizer allows the actions in its table and records the calls; a nil
// table allows everything (the use cases then enforce the fine-grained rules).
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
}

func newAPI(t testing.TB, svc *work.Service, toks tokens) *api {
	t.Helper()
	a := &api{t: t, authz: &routeAuthorizer{}}
	rt, err := httpapi.NewRouter(httpapi.RouterConfig{Authenticator: toks, Authorizer: a.authz, RateLimiter: httpapi.Unlimited(), Prefix: "/api/v1"})
	if err != nil {
		t.Fatal(err)
	}
	codec, err := httpapi.NewCursorCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	h, err := workhttp.NewHandler(svc, codec)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.RegisterRoutes(rt); err != nil {
		t.Fatal(err)
	}
	a.router = rt
	return a
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

func (r resp) errCode(t testing.TB) string {
	t.Helper()
	var e httpapi.ErrorEnvelope
	r.decode(t, &e)
	if e.Error.RequestId == "" || r.Header().Get("X-Request-ID") != e.Error.RequestId {
		t.Fatalf("error envelope without a matching request id: %s", r.Body.String())
	}
	return e.Error.Code
}

func base(tenant, project string) string {
	return "/tenants/" + tenant + "/projects/" + project + "/work-items"
}
