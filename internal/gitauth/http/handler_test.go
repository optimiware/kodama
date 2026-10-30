package gitauthhttp_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/gitauth"
	"github.com/optimiware/kodama/internal/gitauth/gitauthtest"
	gitauthhttp "github.com/optimiware/kodama/internal/gitauth/http"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
	identityhttp "github.com/optimiware/kodama/internal/identity/http"
)

// bearerIsUser is the authenticator stand-in: the bearer value is the user ID.
type bearerIsUser struct{}

func (bearerIsUser) Authenticate(_ context.Context, r *http.Request) (httpapi.Principal, error) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || scheme != "Bearer" || token == "" {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	return httpapi.Principal{ID: token}, nil
}

type rig struct {
	f      *gitauthtest.Fixture
	router *httpapi.Router
}

func newRig(t *testing.T) *rig {
	t.Helper()
	f := gitauthtest.New(t, gitauthtest.OpenMem)
	authority, err := identity.NewStandaloneAuthority(f.Roles)
	if err != nil {
		t.Fatal(err)
	}
	authorizer, err := identityhttp.NewAuthorizer(authority, map[httpapi.Action]identityhttp.Rule{gitauthhttp.ActionExchange: gitauthhttp.Rule()})
	if err != nil {
		t.Fatal(err)
	}
	router, err := httpapi.NewRouter(httpapi.RouterConfig{Authenticator: bearerIsUser{}, Authorizer: authorizer, RateLimiter: httpapi.Unlimited(), Prefix: "/api/v1"})
	if err != nil {
		t.Fatal(err)
	}
	h, err := gitauthhttp.NewHandler(f.Svc)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.RegisterRoutes(router); err != nil {
		t.Fatal(err)
	}
	return &rig{f: f, router: router}
}

func (r *rig) post(t *testing.T, tenant, user, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tenants/"+tenant+"/git-credentials", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req.Header.Set("Authorization", "Bearer "+user)
	}
	rec := httptest.NewRecorder()
	r.router.ServeHTTP(rec, req)
	return rec
}

func request(repo string, seed bool) string {
	b, _ := json.Marshal(gitv1.CredentialRequest{ProjectID: string(gitauthtest.ProjectA), RepositoryID: repo, Seed: seed})
	return string(b)
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Error.Code == "" {
		t.Fatalf("not an error envelope (%d): %s", rec.Code, rec.Body.String())
	}
	return env.Error.Code
}

func TestExchangeIssuesHumanAndSeedCredentials(t *testing.T) {
	r := newRig(t)
	tenant := string(gitauthtest.TenantA)

	rec := r.post(t, tenant, string(gitauthtest.Engineer), request(gitauthtest.RepoReady, false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") || rec.Header().Get("X-Request-ID") == "" {
		t.Fatalf("headers = %v", rec.Header())
	}
	resp, err := gitv1.DecodeResponse[gitv1.CredentialResponse](rec.Body.Bytes())
	if err != nil {
		t.Fatalf("response: %v", err)
	}
	var generated gitauthhttp.GitCredential
	if err := json.Unmarshal(rec.Body.Bytes(), &generated); err != nil || generated.Password != resp.Password || generated.GrantId != resp.GrantID || generated.Username != "kodama" {
		t.Fatalf("the generated type does not read the response: %+v %v", generated, err)
	}
	claims, err := r.f.Svc.Verify(context.Background(), gitauthtest.TenantA, resp.Password)
	if err != nil || claims.Kind != gitv1.GrantHuman || claims.Actor.ID != string(gitauthtest.Engineer) {
		t.Fatalf("issued credential = %+v %v", claims, err)
	}

	rec = r.post(t, tenant, string(gitauthtest.Engineer), request(gitauthtest.RepoEmpty, true))
	if rec.Code != http.StatusOK {
		t.Fatalf("seed status %d: %s", rec.Code, rec.Body.String())
	}
	seed, err := gitv1.DecodeResponse[gitv1.CredentialResponse](rec.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if c, err := r.f.Svc.Verify(context.Background(), gitauthtest.TenantA, seed.Password); err != nil || c.Kind != gitv1.GrantSeed {
		t.Fatalf("seed credential = %+v %v", c, err)
	}

	// The request builds from the generated type, too.
	body, _ := json.Marshal(gitauthhttp.GitCredentialRequest{ProjectId: string(gitauthtest.ProjectA), RepositoryId: gitauthtest.RepoReady, Seed: false})
	if rec := r.post(t, tenant, string(gitauthtest.Viewer), string(body)); rec.Code != http.StatusOK {
		t.Fatalf("viewer: %d %s", rec.Code, rec.Body.String())
	}
}

func TestExchangeErrorMapping(t *testing.T) {
	r := newRig(t)
	tenant := string(gitauthtest.TenantA)
	engineer, viewer, admin, outsider := string(gitauthtest.Engineer), string(gitauthtest.Viewer), string(gitauthtest.Admin), string(gitauthtest.Outsider)
	r.f.Pushes.SetUnsettled(gitauthtest.ScopeA(), "r136_pending", true)
	r.f.Repos.Put(gitauthtest.ScopeA(), gitauthtest.NewRepository("r136_pending", func(x *gitv1.Repository) { x.Empty = true }))

	for _, c := range []struct {
		name   string
		tenant string
		user   string
		body   string
		status int
		code   string
	}{
		{"repository not empty", tenant, engineer, request(gitauthtest.RepoReady, true), 409, "repository_not_empty"},
		{"seed pending", tenant, engineer, request("r136_pending", true), 409, "seed_pending"},
		{"not ready", tenant, engineer, request(gitauthtest.RepoBooting, false), 409, "repository_not_ready"},
		{"archived", tenant, engineer, request(gitauthtest.RepoArchived, true), 409, "repository_archived"},
		{"external", tenant, engineer, request(gitauthtest.RepoExternal, false), 422, "hosting_mismatch"},
		{"seed by a viewer", tenant, viewer, request(gitauthtest.RepoEmpty, true), 403, "forbidden"},
		{"admin has no source access", tenant, admin, request(gitauthtest.RepoReady, false), 403, "forbidden"},
		{"foreign repository", tenant, engineer, request(gitauthtest.RepoOther, false), 404, "not_found"},
		{"unknown repository", tenant, engineer, request("r136_unknown", false), 404, "not_found"},
		{"a stranger learns nothing", tenant, outsider, request(gitauthtest.RepoReady, false), 404, "not_found"},
		{"another tenant's path", string(gitauthtest.TenantB), engineer, request(gitauthtest.RepoReady, false), 404, "not_found"},
		{"unknown tenant", "t136_nobody", engineer, request(gitauthtest.RepoReady, false), 404, "not_found"},
		{"no credential", tenant, "", request(gitauthtest.RepoReady, false), 401, "invalid_credential"},
	} {
		rec := r.post(t, c.tenant, c.user, c.body)
		if rec.Code != c.status || errorCode(t, rec) != c.code {
			t.Errorf("%s: %d %s, want %d %s", c.name, rec.Code, rec.Body.String(), c.status, c.code)
		}
		if strings.Contains(rec.Body.String(), "gitc_") {
			t.Errorf("%s: a credential in an error", c.name)
		}
	}

	// A dependency outage is a retryable 503 and no detail.
	r.f.Repos.Fail = errors.New("repository module: password=hunter2")
	rec := r.post(t, tenant, engineer, request(gitauthtest.RepoReady, false))
	if rec.Code != http.StatusServiceUnavailable || errorCode(t, rec) != "unavailable" || rec.Header().Get("Retry-After") == "" || strings.Contains(rec.Body.String(), "hunter2") {
		t.Fatalf("outage: %d %s", rec.Code, rec.Body.String())
	}
	r.f.Repos.Fail = nil

	// Every error code of the route is a known contract code with the documented status.
	for _, op := range gitv1.Operations {
		if op.ID != "exchangeCredential" {
			continue
		}
		for _, code := range op.Errors {
			if _, ok := gitv1.Status(code); !ok {
				t.Errorf("code %s has no status", code)
			}
		}
	}
}

func TestExchangeBodyIsStrict(t *testing.T) {
	r := newRig(t)
	tenant, user := string(gitauthtest.TenantA), string(gitauthtest.Engineer)
	for name, body := range map[string]string{
		"empty":              "",
		"not json":           "seed me",
		"seed omitted":       `{"projectId":"p136_inventory","repositoryId":"r136_backend"}`,
		"seed null":          `{"projectId":"p136_inventory","repositoryId":"r136_backend","seed":null}`,
		"seed string":        `{"projectId":"p136_inventory","repositoryId":"r136_backend","seed":"yes"}`,
		"project omitted":    `{"repositoryId":"r136_backend","seed":false}`,
		"unknown member":     `{"projectId":"p136_inventory","repositoryId":"r136_backend","seed":false,"extra":1}`,
		"a user in the body": `{"projectId":"p136_inventory","repositoryId":"r136_backend","seed":false,"userId":"u136_admin"}`,
		"a tenant in body":   `{"projectId":"p136_inventory","repositoryId":"r136_backend","seed":false,"tenantId":"t136_globex"}`,
		"duplicate member":   `{"projectId":"p136_inventory","repositoryId":"r136_backend","seed":false,"seed":true}`,
		"miscased member":    `{"projectId":"p136_inventory","repositoryId":"r136_backend","Seed":false}`,
		"two documents":      `{"projectId":"p136_inventory","repositoryId":"r136_backend","seed":false}{}`,
		"bad id":             `{"projectId":"not an id","repositoryId":"r136_backend","seed":false}`,
		"array":              `[]`,
	} {
		rec := r.post(t, tenant, user, body)
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_request" {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
		if body != "" && strings.Contains(rec.Body.String(), "hunter") {
			t.Errorf("%s: the body is echoed", name)
		}
	}
	// The media type is checked before the body is read.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tenants/"+tenant+"/git-credentials", strings.NewReader(request(gitauthtest.RepoReady, false)))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Authorization", "Bearer "+user)
	rec := httptest.NewRecorder()
	r.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("media type: %d", rec.Code)
	}
	// Only POST is routed.
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(m, "/api/v1/tenants/"+tenant+"/git-credentials", nil)
		req.Header.Set("Authorization", "Bearer "+user)
		rec := httptest.NewRecorder()
		r.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d", m, rec.Code)
		}
	}
	// The body never selects the caller: the principal is the router's.
	if n := r.f.Env.GrantCount(t, gitauthtest.TenantA); n != 0 {
		t.Fatalf("refused requests stored %d grants", n)
	}
}

// TestFragmentMatchesTheRoute keeps api/git-credentials.yaml and the handler in
// step: the same path and verb, the required members of the contract, and the
// error statuses the route returns.
func TestFragmentMatchesTheRoute(t *testing.T) {
	raw, err := os.ReadFile("../../../api/git-credentials.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths      map[string]map[string]any `yaml:"paths"`
		Components struct {
			Schemas map[string]struct {
				Required   []string                  `yaml:"required"`
				Properties map[string]map[string]any `yaml:"properties"`
				Additional any                       `yaml:"additionalProperties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	op, ok := doc.Paths[gitauthhttp.Path]["post"].(map[string]any)
	if !ok || op["operationId"] != "exchangeGitCredential" || len(doc.Paths) != 1 {
		t.Fatalf("paths = %+v", doc.Paths)
	}
	responses, _ := op["responses"].(map[string]any)
	var route *gitv1.Operation
	for i := range gitv1.Operations {
		if gitv1.Operations[i].ID == "exchangeCredential" {
			route = &gitv1.Operations[i]
		}
	}
	if route == nil || route.Path != gitauthhttp.Path || route.Method != http.MethodPost {
		t.Fatalf("the contract's route = %+v", route)
	}
	statuses := map[string]bool{}
	for code := range responses {
		statuses[code] = true
	}
	for _, code := range route.Errors {
		status, _ := gitv1.Status(code)
		// 401 and 429 and 503 are the shared responses; every other status must be documented.
		if !statuses[strconv.Itoa(status)] {
			t.Errorf("status %d of %s is not documented", status, code)
		}
	}
	for _, s := range []string{"200", "400", "401", "403", "404", "409", "422", "429", "503"} {
		if !statuses[s] {
			t.Errorf("response %s is not documented", s)
		}
	}
	req := doc.Components.Schemas["GitCredentialRequest"]
	if strings.Join(req.Required, ",") != "projectId,repositoryId,seed" || req.Additional != false {
		t.Fatalf("request schema = %+v", req)
	}
	resp := doc.Components.Schemas["GitCredential"]
	if strings.Join(resp.Required, ",") != "username,password,grantId,expiresAt" {
		t.Fatalf("response schema = %+v", resp)
	}
	for field, pattern := range map[string]string{"username": gitv1.Patterns["Username"], "password": gitv1.Patterns["Credential"]} {
		if got := resp.Properties[field]["pattern"]; got != pattern {
			t.Errorf("%s pattern = %v, want the contract's %s", field, got, pattern)
		}
	}
}

func TestRegistrationNeedsAServiceAndARoute(t *testing.T) {
	if _, err := gitauthhttp.NewHandler(nil); err == nil {
		t.Fatal("a handler needs the use case")
	}
	if got := gitauthhttp.Rule(); got.Level != identityhttp.LevelAnyScope || len(got.Any) != 1 || got.Any[0] != gitauth.PermRead {
		t.Fatalf("rule = %+v", got)
	}
}
