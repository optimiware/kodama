package gitpushhttp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitpush"
	"github.com/optimiware/kodama/internal/gitpush/gitpushtest"
	gitpushhttp "github.com/optimiware/kodama/internal/gitpush/http"
	"github.com/optimiware/kodama/internal/gitservice"
	"github.com/optimiware/kodama/internal/httpapi"
)

var listAll = gitpush.ListQuery{Limit: 100}

// verifier maps tokens to worker identities and applies gitv1.CheckWorkerCall,
// as the real verifier (T-136) does after the signature check.
type verifier map[string]gitv1.WorkerIdentity

func (v verifier) VerifyWorkerIdentity(_ context.Context, token string, pathTenant domain.TenantID, now time.Time) (gitv1.WorkerIdentity, error) {
	id, ok := v[token]
	if !ok {
		return gitv1.WorkerIdentity{}, errors.New("unknown token")
	}
	if err := gitv1.CheckWorkerCall(id, string(pathTenant), now); err != nil {
		return gitv1.WorkerIdentity{}, err
	}
	return id, nil
}

// lax is a verifier that skips the tenant comparison: the handler must still
// bind the identity to the path.
type lax struct{ id gitv1.WorkerIdentity }

func (l lax) VerifyWorkerIdentity(context.Context, string, domain.TenantID, time.Time) (gitv1.WorkerIdentity, error) {
	return l.id, nil
}

func identity(f *gitpushtest.Fixture, tenant domain.TenantID) gitv1.WorkerIdentity {
	now := f.Clock.Now()
	ts := func(t time.Time) gitv1.Timestamp { return gitv1.Timestamp(t.UTC().Format("2006-01-02T15:04:05Z")) }
	return gitv1.WorkerIdentity{Audience: gitv1.ControlAudience, TenantID: string(tenant), WorkerInstance: "worker-1", IssuedAt: ts(now), ExpiresAt: ts(now.Add(30 * time.Minute))}
}

type env struct {
	t   *testing.T
	f   *gitpushtest.Fixture
	srv *httptest.Server
	mux *http.ServeMux
}

func newEnv(t *testing.T) *env {
	f := gitpushtest.New(t, gitpushtest.OpenMem)
	h, err := gitpushhttp.NewWorkerHandler(f.Svc, verifier{
		"tok-a": identity(f, gitpushtest.TenantA), "tok-b": identity(f, gitpushtest.TenantB),
	}, f.Clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &env{t: t, f: f, srv: srv, mux: mux}
}

func (e *env) post(path, token, body string) (int, []byte) {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes()
}

func errCode(t *testing.T, body []byte) domain.ErrorCode {
	t.Helper()
	var e gitv1.ErrorResponse
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("not an error body: %v: %s", err, body)
	}
	if err := e.Validate(); err != nil {
		t.Fatalf("invalid error body: %v: %s", err, body)
	}
	return e.Error.Code
}

func path(tenant domain.TenantID, rest string) string {
	return strings.ReplaceAll(gitv1.ControlBasePath, "{tenantId}", string(tenant)) + rest
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The worker routes serve the whole receive over HTTP, with the answers the
// gitservice control client decodes.
func Test_unit_gitpush_http_worker_roundtrip(t *testing.T) {
	e := newEnv(t)
	f := e.f
	grant := f.Human(gitpushtest.Engineer, gitpushtest.RepoMain)
	req := gitpushtest.Request(gitpushtest.RepoMain, grant, "rcv_h1", false, gitpushtest.Advance(gitpushtest.Branch("feature"), gitpushtest.OID("1"), gitpushtest.OID("2")))

	client, err := gitservice.NewControlPlaneClient(e.srv.URL, gitpushtest.TenantA, func(context.Context) (string, error) { return "tok-a", nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	resp, err := client.Authorize(ctx, req)
	if err != nil || !resp.Allowed() || gitv1.CheckAuthorizeResponse(req, resp) != nil {
		t.Fatalf("authorize: %+v %v", resp, err)
	}
	if _, err := client.CommitIntent(ctx, resp.PushID, gitv1.CommitIntentRequest{ReceiveID: req.ReceiveID, Updates: req.Updates}); err != nil {
		t.Fatalf("commit intent: %v", err)
	}
	out, err := client.Outcome(ctx, resp.PushID, gitv1.OutcomeRequest{ReceiveID: req.ReceiveID, Applied: req.Updates})
	if err != nil || out.Status != gitv1.PushApplied {
		t.Fatalf("outcome: %+v %v", out, err)
	}
	f.Env.Events(t, gitpushtest.ScopeA(), gitpushtest.RepoMain)
	if n := len(f.Env.Events(t, gitpushtest.ScopeA(), gitpushtest.RepoMain)); n != 1 {
		t.Fatalf("%d events", n)
	}

	// Refusals keep their contract codes and statuses end to end.
	_, err = client.CommitIntent(ctx, resp.PushID, gitv1.CommitIntentRequest{ReceiveID: req.ReceiveID, Updates: req.Updates})
	var ce *gitv1.ContractError
	if !errors.As(err, &ce) || ce.Code != gitv1.CodePushSettled {
		t.Fatalf("a settled record: %v", err)
	}
	denied := gitpushtest.Request(gitpushtest.RepoMain, grant, "rcv_h2", false, gitpushtest.Advance(gitpushtest.Branch("main"), gitpushtest.OID("1"), gitpushtest.OID("2")))
	dresp, err := client.Authorize(ctx, denied)
	if err != nil || dresp.Allowed() || *dresp.Updates[0].Reason != gitv1.ReasonRefProtected {
		t.Fatalf("denied: %+v %v", dresp, err)
	}
	_, err = client.Authorize(ctx, gitv1.PushAuthorizeRequest{})
	if !errors.As(err, &ce) {
		t.Fatalf("an empty request: %v", err)
	}
}

func Test_unit_gitpush_http_worker_identity_is_tenant_bound(t *testing.T) {
	e := newEnv(t)
	f := e.f
	grant := f.Human(gitpushtest.Engineer, gitpushtest.RepoMain)
	body := mustJSON(t, gitpushtest.Request(gitpushtest.RepoMain, grant, "rcv_h3", false, gitpushtest.Create(gitpushtest.Branch("x"), gitpushtest.OID("1"))))
	p := path(gitpushtest.TenantA, "/pushes")

	for name, tc := range map[string]struct{ path, token string }{
		"no credential":            {p, ""},
		"unknown token":            {p, "tok-x"},
		"another tenant's worker":  {p, "tok-b"},
		"another tenant's path":    {path(gitpushtest.TenantB, "/pushes"), "tok-a"},
		"a credential of a person": {p, "gitc_abcdefghijklmnopqrstuvwxyz0123456789ABCD"},
	} {
		status, out := e.post(tc.path, tc.token, body)
		if status != http.StatusUnauthorized || errCode(t, out) != domain.CodeInvalidCredential {
			t.Errorf("%s: %d %s", name, status, out)
		}
	}
	// A verifier that forgets the tenant comparison cannot widen the identity.
	h, err := gitpushhttp.NewWorkerHandler(f.Svc, lax{id: identity(f, gitpushtest.TenantB)}, f.Clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	h.Register(mux)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, p, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer any")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("lax verifier: %d %s", rec.Code, rec.Body)
	}
	// Nothing was recorded by any refused call.
	page, err := f.Svc.List(context.Background(), "usr_reader", gitpushtest.ScopeA(), gitpushtest.RepoMain, listAll)
	if err != nil || len(page) != 0 {
		t.Fatalf("records after refused calls: %d %v", len(page), err)
	}
	// The authenticated tenant is the path's: another tenant's worker sees its own
	// (empty) namespace, never tenant A's records.
	status, out := e.post(path(gitpushtest.TenantB, "/pushes"), "tok-b", body)
	if status != http.StatusNotFound || errCode(t, out) != domain.CodeNotFound {
		t.Fatalf("a grant of tenant A in tenant B: %d %s", status, out)
	}
}

func Test_unit_gitpush_http_worker_requests_are_strict(t *testing.T) {
	e := newEnv(t)
	f := e.f
	grant := f.Human(gitpushtest.Engineer, gitpushtest.RepoMain)
	good := gitpushtest.Request(gitpushtest.RepoMain, grant, "rcv_h4", false, gitpushtest.Create(gitpushtest.Branch("x"), gitpushtest.OID("1")))
	p := path(gitpushtest.TenantA, "/pushes")
	for name, body := range map[string]string{
		"empty":           "",
		"not json":        "{",
		"unknown member":  strings.Replace(mustJSON(t, good), `"receiveId"`, `"extra":1,"receiveId"`, 1),
		"duplicate":       strings.Replace(mustJSON(t, good), `"receiveId":"rcv_h4"`, `"receiveId":"rcv_h4","receiveId":"rcv_h5"`, 1),
		"missing facts":   strings.Replace(mustJSON(t, good), `"gitFacts"`, `"gitFacts2"`, 1),
		"bad object id":   strings.Replace(mustJSON(t, good), gitpushtest.OID("1"), "xyz", 1),
		"too many (zero)": mustJSON(t, gitv1.PushAuthorizeRequest{RepositoryID: gitpushtest.RepoMain, GrantID: grant, ReceiveID: "r"}),
	} {
		status, out := e.post(p, "tok-a", body)
		if status != http.StatusBadRequest || errCode(t, out) != domain.CodeInvalidRequest {
			t.Errorf("%s: %d %s", name, status, out)
		}
	}
	if status, _ := e.post(p, "tok-a", mustJSON(t, good)); status != http.StatusOK {
		t.Fatalf("the valid request: %d", status)
	}
	// Another method, another path and a malformed push ID.
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	e.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path(gitpushtest.TenantA, "/repositories/r/initialized"), nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("another route: %d", rec.Code)
	}
	status, out := e.post(path(gitpushtest.TenantA, "/pushes/bad%20id/outcome"), "tok-a", `{"receiveId":"r","applied":[]}`)
	if status != http.StatusNotFound || errCode(t, out) != domain.CodeNotFound {
		t.Fatalf("a malformed push id: %d %s", status, out)
	}
	status, out = e.post(path(gitpushtest.TenantA, "/pushes/push_nope/outcome"), "tok-a", `{"receiveId":"r","applied":[]}`)
	if status != http.StatusNotFound {
		t.Fatalf("an unknown push: %d %s", status, out)
	}
	// An outage is a retryable 503.
	f.Repos.Fail = errors.New("repository module down")
	status, out = e.post(p, "tok-a", mustJSON(t, gitpushtest.Request(gitpushtest.RepoMain, grant, "rcv_h6", false, gitpushtest.Create(gitpushtest.Branch("y"), gitpushtest.OID("1")))))
	if status != http.StatusServiceUnavailable || errCode(t, out) != domain.CodeUnavailable {
		t.Fatalf("outage: %d %s", status, out)
	}
}

// ---- public reads ----

type authenticator map[string]string

func (a authenticator) Authenticate(_ context.Context, r *http.Request) (httpapi.Principal, error) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	id, ok := a[tok]
	if !ok {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	return httpapi.Principal{ID: id}, nil
}

type allowAll struct{}

func (allowAll) Authorize(context.Context, httpapi.Principal, httpapi.Action, httpapi.Target) error {
	return nil
}

func publicRouter(t *testing.T, f *gitpushtest.Fixture) *httpapi.Router {
	t.Helper()
	router, err := httpapi.NewRouter(httpapi.RouterConfig{
		Authenticator: authenticator{"tok-reader": "usr_reader", "tok-stranger": "usr_stranger"}, Authorizer: allowAll{},
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
	if err := h.RegisterRoutes(router); err != nil {
		t.Fatal(err)
	}
	return router
}

func get(router http.Handler, token, p string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1"+p, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func Test_unit_gitpush_http_public_reads(t *testing.T) {
	f := gitpushtest.New(t, gitpushtest.OpenMem)
	router := publicRouter(t, f)
	grant := f.Human(gitpushtest.Engineer, gitpushtest.RepoMain)
	var ids []string
	for i, b := range []string{"a", "b", "c"} {
		f.Clock.Advance(time.Second)
		resp := f.MustAuthorize(gitpushtest.Request(gitpushtest.RepoMain, grant, "rcv_p"+b, false, gitpushtest.Create(gitpushtest.Branch(b), gitpushtest.OID("1"))))
		ids = append(ids, resp.PushID)
		_ = i
	}
	base := "/tenants/" + string(gitpushtest.TenantA) + "/projects/" + string(gitpushtest.ProjectA) + "/repositories/" + gitpushtest.RepoMain + "/pushes"

	rec := get(router, "tok-reader", base+"/"+ids[0])
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rec.Code, rec.Body)
	}
	got, err := gitv1.DecodeResponse[gitv1.PushRecord](rec.Body.Bytes())
	if err != nil || got.PushID != ids[0] || got.Status != gitv1.PushAuthorized || got.Entries[0].Kind != gitv1.KindCreate {
		t.Fatalf("record: %+v %v", got, err)
	}

	rec = get(router, "tok-reader", base+"?limit=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	list, err := gitv1.DecodeResponse[gitv1.PushRecordList](rec.Body.Bytes())
	if err != nil || len(list.Items) != 2 || list.Items[0].PushID != ids[2] || list.NextCursor == nil {
		t.Fatalf("page 1: %+v %v", list, err)
	}
	rec = get(router, "tok-reader", base+"?limit=2&cursor="+*list.NextCursor)
	list2, err := gitv1.DecodeResponse[gitv1.PushRecordList](rec.Body.Bytes())
	if err != nil || len(list2.Items) != 1 || list2.Items[0].PushID != ids[0] || list2.NextCursor != nil {
		t.Fatalf("page 2: %+v %v: %s", list2, err, rec.Body)
	}
	// A cursor of another repository is refused.
	other := "/tenants/" + string(gitpushtest.TenantA) + "/projects/" + string(gitpushtest.ProjectA) + "/repositories/" + gitpushtest.RepoDirect + "/pushes"
	if rec = get(router, "tok-reader", other+"?limit=2&cursor="+*list.NextCursor); rec.Code != http.StatusBadRequest {
		t.Fatalf("a cursor of another collection: %d %s", rec.Code, rec.Body)
	}

	// Without the project's read authority, or for a foreign object: 403, 404.
	if rec = get(router, "tok-stranger", base); rec.Code != http.StatusForbidden {
		t.Fatalf("stranger: %d", rec.Code)
	}
	if rec = get(router, "", base); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", rec.Code)
	}
	if rec = get(router, "tok-reader", base+"/push_nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown push: %d", rec.Code)
	}
	if rec = get(router, "tok-reader", other+"/"+ids[0]); rec.Code != http.StatusNotFound {
		t.Fatalf("a push through another repository: %d", rec.Code)
	}
	foreign := "/tenants/" + string(gitpushtest.TenantB) + "/projects/" + string(gitpushtest.ProjectB) + "/repositories/" + gitpushtest.RepoMain + "/pushes"
	if rec = get(router, "tok-reader", foreign+"/"+ids[0]); rec.Code == http.StatusOK {
		t.Fatal("a push of another tenant was served")
	}
	// No write route exists.
	req := httptest.NewRequest(http.MethodPost, "/api/v1"+base, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer tok-reader")
	wrec := httptest.NewRecorder()
	router.ServeHTTP(wrec, req)
	if wrec.Code != http.StatusMethodNotAllowed && wrec.Code != http.StatusNotFound {
		t.Fatalf("POST: %d", wrec.Code)
	}
}
