package gitpushhttp_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/gitpush/gitpushtest"
	gitpushhttp "github.com/optimiware/kodama/internal/gitpush/http"
	"github.com/optimiware/kodama/internal/httpapi"
)

func resolveRouter(t *testing.T, f *gitpushtest.Fixture, withResolver bool) *httpapi.Router {
	t.Helper()
	router, err := httpapi.NewRouter(httpapi.RouterConfig{
		Authenticator: authenticator{"tok-reader": "usr_reader", "tok-admin": "usr_admin"}, Authorizer: allowAll{},
		RateLimiter: httpapi.Unlimited(), Prefix: "/api/v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	codec, err := httpapi.NewCursorCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	h, err := gitpushhttp.NewHandler(f.Svc, codec)
	if err != nil {
		t.Fatal(err)
	}
	if withResolver {
		h.WithResolver(f.Svc)
	}
	if err := h.RegisterRoutes(router); err != nil {
		t.Fatal(err)
	}
	return router
}

func post(router http.Handler, token, p, ifMatch, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1"+p, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// The operator resolution route: If-Match on the record version, the admin
// role (the use case asks again), the recorded decision and the one event.
func Test_unit_gitpush_http_operator_resolution(t *testing.T) {
	f := gitpushtest.New(t, gitpushtest.OpenMem)
	grant := f.Human(gitpushtest.Engineer, gitpushtest.RepoMain)
	ref := gitpushtest.Branch("stuck")
	req := gitpushtest.Request(gitpushtest.RepoMain, grant, "rcv_ro", false, gitpushtest.Create(ref, gitpushtest.OID("1")))
	resp := f.MustAuthorize(req)
	if err := f.Intent(resp.PushID, req); err != nil {
		t.Fatal(err)
	}
	if err := f.Reconcile(gitpushtest.RepoMain); err != nil {
		t.Fatal(err)
	}
	base := "/tenants/" + string(gitpushtest.TenantA) + "/projects/" + string(gitpushtest.ProjectA) + "/repositories/" + gitpushtest.RepoMain + "/pushes/" + resp.PushID
	body := mustJSON(t, gitpushhttp.ResolveRequest{Ref: ref, Resolution: gitv1.EntryApplied, Reason: "the ref is at the pushed commit"})

	// Without the resolver the route does not exist.
	if rec := post(resolveRouter(t, f, false), "tok-admin", base+"/resolutions", `"1"`, body); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("an unmounted route: %d", rec.Code)
	}
	router := resolveRouter(t, f, true)
	rec := get(router, "tok-reader", base)
	etag := rec.Header().Get("ETag")
	if rec.Code != http.StatusOK || etag == "" {
		t.Fatalf("get: %d etag %q", rec.Code, etag)
	}
	path := base + "/resolutions"
	if rec = post(router, "tok-admin", path, "", body); rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("no If-Match: %d %s", rec.Code, rec.Body)
	}
	if rec = post(router, "tok-admin", path, `"99"`, body); rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale: %d %s", rec.Code, rec.Body)
	}
	if rec = post(router, "tok-reader", path, etag, body); rec.Code != http.StatusForbidden {
		t.Fatalf("not an administrator: %d %s", rec.Code, rec.Body)
	}
	for name, bad := range map[string]string{
		"no reason":       `{"ref":"` + ref + `","resolution":"applied","reason":""}`,
		"a bad decision":  `{"ref":"` + ref + `","resolution":"unknown","reason":"x"}`,
		"unknown members": `{"ref":"` + ref + `","resolution":"applied","reason":"x","extra":1}`,
	} {
		if rec = post(router, "tok-admin", path, etag, bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	f.Env.Events(t, gitpushtest.ScopeA(), gitpushtest.RepoMain)
	if n := len(f.Env.Events(t, gitpushtest.ScopeA(), gitpushtest.RepoMain)); n != 0 {
		t.Fatalf("%d events before the resolution", n)
	}

	rec = post(router, "tok-admin", path, etag, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", rec.Code, rec.Body)
	}
	got, err := gitv1.DecodeResponse[gitv1.PushRecord](rec.Body.Bytes())
	if err != nil || got.Status != gitv1.PushApplied || got.Entries[0].State != gitv1.EntryApplied {
		t.Fatalf("record: %+v %v", got, err)
	}
	if next := rec.Header().Get("ETag"); next == "" || next == etag {
		t.Fatalf("etag %q after %q", next, etag)
	}
	if n := len(f.Env.Events(t, gitpushtest.ScopeA(), gitpushtest.RepoMain)); n != 1 {
		t.Fatalf("%d events after the resolution", n)
	}
	// A repeat with the new tag is refused: the entry is no longer unknown.
	if rec = post(router, "tok-admin", path, rec.Header().Get("ETag"), body); rec.Code != http.StatusConflict {
		t.Fatalf("repeat: %d %s", rec.Code, rec.Body)
	}
	if n := len(f.Env.Events(t, gitpushtest.ScopeA(), gitpushtest.RepoMain)); n != 1 {
		t.Fatalf("%d events after the repeat", n)
	}
}
