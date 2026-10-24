package integration_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	agentshttp "github.com/optimiware/kodama/internal/agents/http"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/httpapi"
)

// TestProfileRoutesOverPostgres exercises the profile routes through the real
// router, identity authority and PostgreSQL store: registration, duplicate
// convergence, role-based refusal, tenant isolation and scoped pagination.
func TestProfileRoutesOverPostgres(t *testing.T) {
	e := newEnv(t)
	rt, err := httpapi.NewRouter(httpapi.RouterConfig{
		Authenticator: tokens{"tok-engineer": executiontest.Engineer, "tok-viewer": executiontest.Viewer, "tok-stranger": executiontest.Stranger, "tok-other": executiontest.OtherEng},
		Authorizer:    allowAll{}, RateLimiter: httpapi.Unlimited(), Prefix: "/api/v1",
	})
	must(t, err)
	codec, err := httpapi.NewCursorCodec([]byte(strings.Repeat("k", 32)))
	must(t, err)
	h, err := agentshttp.NewHandler(e.Agents, codec)
	must(t, err)
	must(t, h.RegisterRoutes(rt))

	scope := executiontest.ScopeA
	path := "/api/v1/tenants/" + string(scope.TenantID) + "/projects/" + string(scope.ProjectID) + "/agent-profiles"
	do := func(token, method, p, body string) *httptest.ResponseRecorder {
		var rd *strings.Reader
		if body != "" {
			rd = strings.NewReader(body)
		} else {
			rd = strings.NewReader("")
		}
		req := httptest.NewRequest(method, p, rd)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, req)
		return rec
	}
	body := func(n int) string {
		raw, _ := json.Marshal(e.variant(n))
		return string(raw)
	}

	if rec := do("tok-engineer", http.MethodPost, path, body(0)); rec.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do("tok-engineer", http.MethodPost, path, body(0)); rec.Code != http.StatusOK {
		t.Fatalf("duplicate: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do("tok-engineer", http.MethodPost, path, body(1)); rec.Code != http.StatusCreated {
		t.Fatalf("second version: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do("tok-viewer", http.MethodPost, path, body(2)); rec.Code != http.StatusForbidden {
		t.Fatalf("a viewer must not register: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do("tok-stranger", http.MethodGet, path, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("a stranger learns nothing: %d", rec.Code)
	}
	if rec := do("tok-other", http.MethodGet, path, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("another tenant's engineer learns nothing: %d", rec.Code)
	}
	var page struct {
		Items []struct {
			Reference string `json:"reference"`
		} `json:"items"`
		NextCursor *string `json:"nextCursor"`
	}
	rec := do("tok-viewer", http.MethodGet, path+"?limit=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("a viewer may list: %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || len(page.Items) != 1 || page.NextCursor == nil {
		t.Fatalf("page: %v %s", err, rec.Body.String())
	}
	rec = do("tok-viewer", http.MethodGet, path+"?limit=1&cursor="+*page.NextCursor, "")
	var next struct {
		Items []struct {
			Reference string `json:"reference"`
		} `json:"items"`
		NextCursor *string `json:"nextCursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &next); err != nil || len(next.Items) != 1 || next.Items[0].Reference == page.Items[0].Reference || next.NextCursor != nil {
		t.Fatalf("second page: %v %s", err, rec.Body.String())
	}
	if n := e.Count(`SELECT count(*) FROM agent_profiles`); n != 2 {
		t.Fatalf("%d profiles", n)
	}
}
