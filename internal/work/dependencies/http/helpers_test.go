package dependencieshttp_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/work"
	"github.com/optimiware/kodama/internal/work/dependencies/depstest"
	dependencieshttp "github.com/optimiware/kodama/internal/work/dependencies/http"
)

const (
	tenantA, projectA = depstest.TenantA, depstest.ProjectA
	tenantB, projectB = depstest.TenantB, depstest.ProjectB
	projectA2         = depstest.ProjectA2
)

var (
	scopeA  = depstest.ScopeA
	scopeA2 = depstest.ScopeA2
	scopeB  = depstest.ScopeB
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

// routeAuthorizer records the route-level authorization calls; allow, when set,
// decides them (nil allows everything: the use cases then enforce the
// fine-grained permissions).
type routeAuthorizer struct {
	allow func(p httpapi.Principal, a httpapi.Action, t httpapi.Target) error
	calls []struct {
		Action httpapi.Action
		Target httpapi.Target
	}
}

func (r *routeAuthorizer) Authorize(_ context.Context, p httpapi.Principal, a httpapi.Action, t httpapi.Target) error {
	r.calls = append(r.calls, struct {
		Action httpapi.Action
		Target httpapi.Target
	}{a, t})
	if r.allow == nil {
		return nil
	}
	return r.allow(p, a, t)
}

type api struct {
	t      testing.TB
	st     *depstest.Stack
	router *httpapi.Router
	authz  *routeAuthorizer
}

var defaultTokens = tokens{
	"tok-engineer": depstest.Engineer, "tok-reader": depstest.Reader, "tok-reviewer": depstest.Reviewer,
	"tok-outsider": depstest.Outsider, "tok-stranger": "u_stranger", "tok-only-a": "u_only_a",
}

func newAPI(t testing.TB, st *depstest.Stack) *api {
	t.Helper()
	// u_only_a exists in tenant A only.
	st.Principal(depstest.TenantA, "u_only_a", work.ActorUser, depstest.EngineerPerms...)
	a := &api{t: t, st: st, authz: &routeAuthorizer{}}
	rt, err := httpapi.NewRouter(httpapi.RouterConfig{Authenticator: defaultTokens, Authorizer: a.authz, RateLimiter: httpapi.Unlimited(), Prefix: "/api/v1"})
	if err != nil {
		t.Fatal(err)
	}
	codec, err := httpapi.NewCursorCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	h, err := dependencieshttp.NewHandler(st.Deps, codec)
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
		if headers[i+1] == "" {
			req.Header.Del(headers[i])
			continue
		}
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

// errBody decodes the error envelope and requires a matching request id.
func (r resp) errBody(t testing.TB) httpapi.Error {
	t.Helper()
	var e httpapi.ErrorEnvelope
	r.decode(t, &e)
	if e.Error.RequestId == "" || r.Header().Get("X-Request-ID") != e.Error.RequestId {
		t.Fatalf("error envelope without a matching request id: %s", r.Body.String())
	}
	return e.Error
}

func (r resp) errCode(t testing.TB) string { return r.errBody(t).Code }

// etag returns the response ETag as a version, failing when it is absent.
func (r resp) etag(t testing.TB) int64 {
	t.Helper()
	tag := r.Header().Get("ETag")
	v, err := strconv.ParseInt(strings.Trim(tag, `"`), 10, 64)
	if err != nil || !strings.HasPrefix(tag, `"`) {
		t.Fatalf("response ETag %q (status %d): %s", tag, r.Code, r.Body.String())
	}
	return v
}

func etag(v int64) string { return `"` + strconv.FormatInt(v, 10) + `"` }

func depPath(tenant, project, id string) string {
	return "/tenants/" + tenant + "/projects/" + project + "/work-items/" + id + "/dependencies"
}

func memberPath(tenant, project, id, dep string) string {
	return depPath(tenant, project, id) + "/" + dep
}

func addBody(id string) string { return `{"dependencyId":"` + id + `"}` }

type snap struct {
	version                int64
	history, audit, outbox int
}

func snapshotOf(st *depstest.Stack, it work.Item) snap {
	h, a, o := st.Records(it)
	return snap{version: int64(st.Get(it).Version), history: h, audit: a, outbox: o}
}

func jsonString(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}
