package api_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/shell/api"
	"github.com/optimiware/kodama/internal/shell/store"
)

// The kernel server mounts the Git routes beside its own: the repository and
// credential routes behind the same authenticator, /git/ and the internal worker
// routes with their own authentication, and every earlier route unchanged.
func TestServerMountsTheGitRoutes(t *testing.T) {
	e := newEnv(t)
	srv, err := api.New(api.Config{Store: &store.Store{}, Auth: tokens, Git: e.git})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	do := func(method, path, token, body string) (int, string) {
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	if code, _ := do(http.MethodGet, "/healthz", "", ""); code != http.StatusOK {
		t.Fatalf("healthz = %d", code)
	}
	if code, body := do(http.MethodPost, "/api/v1/tenants/acme/projects/p_erp/repositories", "tok-eng", `{"name":"backend"}`); code != http.StatusCreated || !strings.Contains(body, `"projectId":"p_erp"`) {
		t.Fatalf("create through the kernel server = %d %s", code, body)
	}
	if code, _ := do(http.MethodPost, "/api/v1/tenants/acme/projects/p_erp/repositories", "", `{"name":"x"}`); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated create = %d", code)
	}
	if code, _ := do(http.MethodGet, "/git/acme/erp/backend.git/info/refs?service=git-upload-pack", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("/git/ without a credential = %d", code)
	}
	if code, _ := do(http.MethodGet, "/internal/git/v1/tenants/t_acme/assignments", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("an internal route without a worker identity = %d", code)
	}
	if code, body := do(http.MethodGet, "/api/v1/me", "tok-eng", ""); code != http.StatusOK || !strings.Contains(body, "u_eng") {
		t.Fatalf("an earlier route = %d %s", code, body)
	}
}
