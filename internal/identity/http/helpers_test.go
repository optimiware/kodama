package identityhttp_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
	identityhttp "github.com/optimiware/kodama/internal/identity/http"
	"github.com/optimiware/kodama/internal/identity/idtest"
)

const (
	tenantA, projectA, projectA2 = "t_acme", "p_inventory", "p_billing"
	tenantB, projectB            = "t_globex", "p_ledger"
	issuer                       = "https://issuer.test"
)

// verifier maps bearer tokens to verified principals; "down" simulates an
// issuer outage.
type verifier struct {
	mu     sync.Mutex
	tokens map[string]identity.Principal
	err    error
}

func (v *verifier) Verify(_ context.Context, token string) (identity.Principal, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.err != nil {
		return identity.Principal{}, v.err
	}
	p, ok := v.tokens[token]
	if !ok {
		return identity.Principal{}, identity.ErrUnauthenticated
	}
	return p, nil
}

// mapper registers users in the memory store like the first-login function.
type mapper struct{ mem *idtest.Mem }

func (m mapper) MapPrincipal(_ context.Context, p identity.Principal) (identity.UserID, error) {
	id := identity.UserID("u_" + strings.NewReplacer("|", "_", "-", "_").Replace(p.Subject))
	m.mem.AddUser(id, p.Issuer, p.Subject)
	return id, nil
}

type api struct {
	t      testing.TB
	mem    *idtest.Mem
	v      *verifier
	router *httpapi.Router
	codec  *httpapi.CursorCodec
	svc    *identity.Service
}

func userID(name string) string { return "u_" + name }

// newAPI builds the real handlers, authenticator, authorizer and service over the
// in-memory repository. Tokens are "tok-<name>"; the user ID is "u_<name>".
func newAPI(t testing.TB, names ...string) *api {
	t.Helper()
	a := &api{t: t, mem: idtest.NewMem(), v: &verifier{tokens: map[string]identity.Principal{}}}
	a.mem.AddTenant(tenantA, "acme")
	a.mem.AddTenant(tenantB, "globex")
	a.mem.AddProject(tenantA, projectA, "inventory")
	a.mem.AddProject(tenantA, projectA2, "billing")
	a.mem.AddProject(tenantB, projectB, "ledger")
	for _, n := range names {
		a.v.tokens["tok-"+n] = identity.Principal{Issuer: issuer, Subject: n}
	}
	authn, err := identityhttp.NewAuthenticator(a.v, mapper{a.mem})
	if err != nil {
		t.Fatal(err)
	}
	standalone, err := identity.NewStandaloneAuthority(a.mem)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := identity.SelectAuthority(identity.AuthorityConfig{Profile: identity.ProfileStandalone, Standalone: standalone})
	if err != nil {
		t.Fatal(err)
	}
	authz, err := identityhttp.NewAuthorizer(authority, identityhttp.DefaultRules())
	if err != nil {
		t.Fatal(err)
	}
	rt, err := httpapi.NewRouter(httpapi.RouterConfig{Authenticator: authn, Authorizer: authz, RateLimiter: httpapi.Unlimited(), Prefix: "/api/v1"})
	if err != nil {
		t.Fatal(err)
	}
	a.codec, err = httpapi.NewCursorCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	a.svc, err = identity.NewService(identity.Config{Repository: a.mem, Directory: a.mem})
	if err != nil {
		t.Fatal(err)
	}
	h, err := identityhttp.NewHandler(a.svc, a.codec)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.RegisterRoutes(rt); err != nil {
		t.Fatal(err)
	}
	a.router = rt
	// Users sign in once so they exist.
	for _, n := range names {
		a.mem.AddUser(identity.UserID(userID(n)), issuer, n)
	}
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

func (r resp) errCode(t testing.TB) string {
	t.Helper()
	var e httpapi.ErrorEnvelope
	r.decode(t, &e)
	if e.Error.RequestId == "" || r.Header().Get("X-Request-ID") != e.Error.RequestId {
		t.Fatalf("error envelope without a matching request id: %s", r.Body.String())
	}
	return e.Error.Code
}

func (a *api) bind(tenant domain.TenantID, id string, user string, project domain.ProjectID, role identity.Role) {
	a.mem.AddBinding(tenant, identity.BindingID(id), identity.UserID(userID(user)), project, role)
}

var errDown = errors.New("issuer down")

var _ = http.StatusOK
