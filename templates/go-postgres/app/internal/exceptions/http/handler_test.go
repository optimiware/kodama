package exhttp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"example.invalid/inventory/internal/exceptions"
	"example.invalid/inventory/internal/exceptions/exceptionstest"
	exhttp "example.invalid/inventory/internal/exceptions/http"
	"example.invalid/inventory/internal/identity"
	"example.invalid/inventory/internal/tenancy"
	"example.invalid/inventory/test/support/oidcfake"
)

const (
	aud     = "https://inventory.example.invalid/api"
	tenantA = "11111111-1111-4111-8111-111111111111"
	tenantB = "22222222-2222-4222-8222-222222222222"
)

type members struct {
	mu sync.Mutex
	m  map[string]tenancy.Role
}

func (m *members) Role(_ context.Context, t tenancy.TenantID, subject string) (tenancy.Role, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.m[string(t)+"/"+subject]
	return r, ok, nil
}

type fixture struct {
	srv  *httptest.Server
	idp  *oidcfake.Provider
	mem  *members
	down bool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	idp := oidcfake.New(t)
	ver, err := identity.NewOIDCVerifier(identity.OIDCConfig{Issuer: idp.Issuer(), Audience: aud})
	if err != nil {
		t.Fatal(err)
	}
	mem := &members{m: map[string]tenancy.Role{
		tenantA + "/rita": tenancy.RoleReviewer, tenantA + "/vic": tenancy.RoleViewer,
		tenantB + "/rita": tenancy.RoleViewer, tenantB + "/bea": tenancy.RoleAdmin,
	}}
	res, _ := tenancy.NewResolver(mem)
	svc, _ := exceptions.NewService(exceptions.Config{Store: exceptionstest.NewStore()})
	f := &fixture{idp: idp, mem: mem}
	h, err := exhttp.New(exhttp.Config{Auth: ver, Tenants: res, Service: svc, CursorKey: bytes.Repeat([]byte("k"), 32),
		Ready: func(ctx context.Context) error { return ver.Ready(ctx) }})
	if err != nil {
		t.Fatal(err)
	}
	f.srv = httptest.NewServer(h.Routes())
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fixture) do(t *testing.T, method, path, token string, body any, headers map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, f.srv.URL+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)
	return resp, out
}

func errCode(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func TestHandlerAuthorityComesFromVerifiedIdentityAndMembership(t *testing.T) {
	f := newFixture(t)
	path := "/api/v1/tenants/" + tenantA + "/exceptions"
	create := map[string]any{"erpRef": "WH1/SKU-9", "erpQuantity": 10, "countedQuantity": 7}
	rita := f.idp.Token("rita", aud)

	// No token, bad token, wrong audience.
	for name, tok := range map[string]string{"none": "", "garbage": "zzz", "wrong audience": f.idp.Token("rita", "https://other")} {
		if resp, body := f.do(t, "POST", path, tok, create, nil); resp.StatusCode != 401 || errCode(body) != "unauthenticated" {
			t.Errorf("%s: %d %v", name, resp.StatusCode, body)
		}
	}
	// A token full of claims and a tenant header grants nothing without membership.
	mallory := f.idp.Sign(oidcfake.Claims{Subject: "mallory", Audience: aud, Extra: map[string]any{"tenant_id": tenantA, "roles": []string{"admin"}}})
	if resp, body := f.do(t, "POST", path, mallory, create, map[string]string{"X-Tenant-ID": tenantA, "X-Role": "admin"}); resp.StatusCode != 404 || errCode(body) != "tenant_not_found" {
		t.Errorf("claims and headers must not grant access: %d %v", resp.StatusCode, body)
	}
	// A viewer may read but not create.
	vic := f.idp.Token("vic", aud)
	if resp, body := f.do(t, "POST", path, vic, create, nil); resp.StatusCode != 403 || errCode(body) != "forbidden" {
		t.Errorf("viewer create: %d %v", resp.StatusCode, body)
	}
	// Create, read.
	resp, body := f.do(t, "POST", path, rita, create, nil)
	if resp.StatusCode != 201 || resp.Header.Get("ETag") != `"1"` || body["state"] != "open" {
		t.Fatalf("create: %d %v", resp.StatusCode, body)
	}
	id := body["id"].(string)
	if resp, body := f.do(t, "GET", path+"/"+id, vic, nil, nil); resp.StatusCode != 200 || body["id"] != id {
		t.Fatalf("viewer read: %d %v", resp.StatusCode, body)
	}
	// The same subject has a different role in tenant B and cannot see tenant A's row there.
	pathB := "/api/v1/tenants/" + tenantB + "/exceptions"
	if resp, body := f.do(t, "GET", pathB+"/"+id, rita, nil, nil); resp.StatusCode != 404 || errCode(body) != "not_found" {
		t.Errorf("cross-tenant read: %d %v", resp.StatusCode, body)
	}
	if resp, body := f.do(t, "POST", pathB, rita, create, nil); resp.StatusCode != 403 {
		t.Errorf("rita is only a viewer in B: %d %v", resp.StatusCode, body)
	}
	// Membership is checked on every request: removal takes effect at once.
	f.mem.mu.Lock()
	delete(f.mem.m, tenantA+"/vic")
	f.mem.mu.Unlock()
	if resp, _ := f.do(t, "GET", path+"/"+id, vic, nil, nil); resp.StatusCode != 404 {
		t.Errorf("removed member still has access: %d", resp.StatusCode)
	}
	// Unknown tenants and malformed selectors are indistinguishable from forbidden ones.
	for _, tn := range []string{"33333333-3333-4333-8333-333333333333", "not-a-uuid"} {
		if resp, body := f.do(t, "GET", "/api/v1/tenants/"+tn+"/exceptions", rita, nil, nil); resp.StatusCode != 404 || errCode(body) != "tenant_not_found" {
			t.Errorf("%s: %d %v", tn, resp.StatusCode, body)
		}
	}
}

func TestHandlerResolutionContract(t *testing.T) {
	f := newFixture(t)
	path := "/api/v1/tenants/" + tenantA + "/exceptions"
	rita := f.idp.Token("rita", aud)
	_, body := f.do(t, "POST", path, rita, map[string]any{"erpRef": "WH1/SKU-9", "erpQuantity": 10, "countedQuantity": 7}, nil)
	id := body["id"].(string)
	url := path + "/" + id + "/resolution"
	req := map[string]any{"adjustedQuantity": 7, "reason": "recount"}
	hdr := func(key, version string) map[string]string {
		return map[string]string{"Idempotency-Key": key, "If-Match": version}
	}

	if resp, body := f.do(t, "POST", url, rita, req, hdr("", `"1"`)); resp.StatusCode != 400 || errCode(body) != "invalid_request" {
		t.Errorf("missing key: %d %v", resp.StatusCode, body)
	}
	if resp, body := f.do(t, "POST", url, rita, req, map[string]string{"Idempotency-Key": "key-00000001"}); resp.StatusCode != 428 || errCode(body) != "precondition_required" {
		t.Errorf("missing If-Match: %d %v", resp.StatusCode, body)
	}
	if resp, body := f.do(t, "POST", url, rita, map[string]any{"adjustedQuantity": 7, "reason": "x", "tenantId": tenantB}, hdr("key-00000001", `"1"`)); resp.StatusCode != 400 {
		t.Errorf("unknown fields are refused: %d %v", resp.StatusCode, body)
	}
	if resp, body := f.do(t, "POST", url, rita, req, hdr("key-00000001", `"9"`)); resp.StatusCode != 412 || errCode(body) != "version_conflict" {
		t.Errorf("stale version: %d %v", resp.StatusCode, body)
	}
	// Accepted work is 202/pending, never "resolved".
	resp, body := f.do(t, "POST", url, rita, req, hdr("key-00000001", `"1"`))
	if resp.StatusCode != 202 || body["state"] != "pending" || resp.Header.Get("ETag") != `"2"` {
		t.Fatalf("accept: %d %v", resp.StatusCode, body)
	}
	res := body["resolution"].(map[string]any)
	if res["requestedBy"] != "rita" {
		t.Errorf("the actor is the verified subject: %v", res)
	}
	// Replay returns the existing operation.
	resp, body = f.do(t, "POST", url, rita, req, hdr("key-00000001", `"1"`))
	if resp.StatusCode != 202 || resp.Header.Get("Idempotent-Replayed") != "true" || body["state"] != "pending" {
		t.Errorf("replay: %d %v", resp.StatusCode, body)
	}
	// Same key, changed payload.
	if resp, body := f.do(t, "POST", url, rita, map[string]any{"adjustedQuantity": 8, "reason": "recount"}, hdr("key-00000001", `"1"`)); resp.StatusCode != 409 || errCode(body) != "idempotency_conflict" {
		t.Errorf("changed payload: %d %v", resp.StatusCode, body)
	}
	// A different key cannot start a second resolution.
	if resp, body := f.do(t, "POST", url, rita, req, hdr("key-00000002", `"2"`)); resp.StatusCode != 409 || errCode(body) != "invalid_state" {
		t.Errorf("second resolution: %d %v", resp.StatusCode, body)
	}
	if _, got := f.do(t, "GET", path+"/"+id, rita, nil, nil); got["state"] != "pending" {
		t.Errorf("still pending until the ERP acknowledges: %v", got)
	}
	// History is attributable.
	if resp, h := f.do(t, "GET", path+"/"+id+"/history", rita, nil, nil); resp.StatusCode != 200 || len(h["items"].([]any)) != 2 {
		t.Errorf("history: %d %v", resp.StatusCode, h)
	}
}

func TestHandlerPaginationAndCursorBinding(t *testing.T) {
	f := newFixture(t)
	pathA := "/api/v1/tenants/" + tenantA + "/exceptions"
	rita := f.idp.Token("rita", aud)
	for i := 0; i < 5; i++ {
		f.do(t, "POST", pathA, rita, map[string]any{"erpRef": "WH1/SKU-" + string(rune('1'+i)), "erpQuantity": 10, "countedQuantity": 7}, nil)
	}
	seen, cursor := 0, ""
	for {
		q := pathA + "?limit=2"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		resp, body := f.do(t, "GET", q, rita, nil, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("%d %v", resp.StatusCode, body)
		}
		items := body["items"].([]any)
		if len(items) > 2 {
			t.Fatalf("page over limit")
		}
		seen += len(items)
		next, _ := body["nextCursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if seen != 5 {
		t.Fatalf("saw %d", seen)
	}
	// A cursor is bound to its tenant and filter and cannot be forged.
	_, first := f.do(t, "GET", pathA+"?limit=1", rita, nil, nil)
	c := first["nextCursor"].(string)
	pathB := "/api/v1/tenants/" + tenantB + "/exceptions"
	for name, u := range map[string]string{
		"other tenant": pathB + "?cursor=" + c,
		"other filter": pathA + "?state=open&cursor=" + c,
		"forged":       pathA + "?cursor=" + strings.Replace(c, ".", "x.", 1),
		"limit":        pathA + "?limit=101",
		"unknown":      pathA + "?foo=bar",
	} {
		if resp, _ := f.do(t, "GET", u, rita, nil, nil); resp.StatusCode != 400 {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
}

func TestHandlerHealthIndependentOfFactoryAndReadinessReportsDependencies(t *testing.T) {
	f := newFixture(t)
	if resp, _ := f.do(t, "GET", "/healthz", "", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("liveness: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "GET", "/readyz", "", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("readiness: %d", resp.StatusCode)
	}
	f.idp.SetDown(true)
	// A fresh verifier cannot reach discovery, so readiness fails while liveness holds.
	ver, _ := identity.NewOIDCVerifier(identity.OIDCConfig{Issuer: f.idp.Issuer(), Audience: aud})
	h, _ := exhttp.New(exhttp.Config{Auth: ver, Tenants: &failingTenants{}, Service: unusedService{}, CursorKey: bytes.Repeat([]byte("k"), 32), Ready: ver.Ready})
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()
	r1, _ := http.Get(srv.URL + "/readyz")
	r2, _ := http.Get(srv.URL + "/healthz")
	if r1.StatusCode != 503 || r2.StatusCode != 200 {
		t.Fatalf("ready=%d live=%d", r1.StatusCode, r2.StatusCode)
	}
}

type failingTenants struct{}

func (failingTenants) Resolve(context.Context, string, string) (tenancy.Access, error) {
	return tenancy.Access{}, tenancy.ErrNotMember
}

type unusedService struct{ exhttp.Service }
