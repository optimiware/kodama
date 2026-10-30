package state

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	statev1 "github.com/optimiware/kodama/contracts/state/v1"
	"github.com/optimiware/kodama/internal/domain"
)

type testResolver struct {
	scope Scope
	write bool
}

func (r testResolver) ResolveBinding(_ context.Context, _ *http.Request, attempt, alias string) (Scope, Binding, error) {
	if attempt != "att_test" || alias != "release" {
		return Scope{}, Binding{}, ErrDenied
	}
	return r.scope, Binding{Namespace: "deploy", Prefix: "website/", Read: true, Write: r.write}, nil
}

func testScope() Scope {
	return Scope{TenantID: domain.TenantID("t_test"), ProjectID: domain.ProjectID("p_test")}
}

func TestHTTPConditionalLifecycle(t *testing.T) {
	mem := NewMemory(nil)
	h, err := NewHandler(mem, testResolver{scope: testScope(), write: true})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	base := srv.URL + "/runtime/v3/attempts/att_test/state/release/entries?key=releases/v1"
	body := `{"value":{"release":"v1"},"artifactReferences":[]}`
	req, _ := http.NewRequest(http.MethodPut, base, strings.NewReader(body))
	req.Header.Set("If-None-Match", "*")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("create: %s: %s", resp.Status, data)
	}
	etag := strings.Trim(resp.Header.Get("ETag"), `"`)
	if etag == "" {
		t.Fatal("missing ETag")
	}
	resp.Body.Close()

	req, _ = http.NewRequest(http.MethodGet, base, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: %s", resp.Status)
	}
	var entry statev1.Entry
	if err := json.NewDecoder(resp.Body).Decode(&entry); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if entry.Generation != 1 || entry.Revision != 1 {
		t.Fatalf("unexpected entry: %+v", entry)
	}

	req, _ = http.NewRequest(http.MethodPut, base, strings.NewReader(`{"value":{"release":"v2"}}`))
	req.Header.Set("If-Match", etag)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update: %s", resp.Status)
	}
	resp.Body.Close()

	req, _ = http.NewRequest(http.MethodPut, base, strings.NewReader(`{"value":{"release":"v3"}}`))
	req.Header.Set("If-Match", etag)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("stale update: got %s, want 412", resp.Status)
	}
	resp.Body.Close()

	req, _ = http.NewRequest(http.MethodDelete, base, nil)
	req.Header.Set("If-Match", etag)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("stale delete: got %s, want 412", resp.Status)
	}
	resp.Body.Close()
}

func TestHTTPBindingAndPrefixAreEnforced(t *testing.T) {
	mem := NewMemory(nil)
	h, _ := NewHandler(mem, testResolver{scope: testScope(), write: true})
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet,
		srv.URL+"/runtime/v3/attempts/att_test/state/release/entries?key=../escape", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("escape key: got %s", resp.Status)
	}
	resp.Body.Close()

	req, _ = http.NewRequest(http.MethodGet,
		srv.URL+"/runtime/v3/attempts/att_test/state/release/entries?prefix=other/", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("relative list prefix: got %s", resp.Status)
	}
	resp.Body.Close()

	body := `{"value":{"release":"v1"}}`
	req, _ = http.NewRequest(http.MethodPut,
		srv.URL+"/runtime/v3/attempts/att_test/state/release/entries?key=releases/v1", strings.NewReader(body))
	req.Header.Set("If-None-Match", "*")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create under binding: %s", resp.Status)
	}
	resp.Body.Close()
	if _, err := mem.Get(context.Background(), testScope(), "deploy", "website/releases/v1"); err != nil {
		t.Fatalf("binding prefix was not applied: %v", err)
	}
}

func TestMemoryListAndRecreateGeneration(t *testing.T) {
	mem := NewMemory(nil)
	ctx := context.Background()
	scope := testScope()
	for _, key := range []string{"a/one", "a/two", "b/three"} {
		if _, err := mem.Create(ctx, scope, "deploy", key, "att_test", statev1.WriteRequest{Value: json.RawMessage(`{"ok":true}`)}); err != nil {
			t.Fatal(err)
		}
	}
	items, next, err := mem.List(ctx, scope, "deploy", "a/", "", 1)
	if err != nil || len(items) != 1 || next != "a/two" || items[0].Key != "a/one" {
		t.Fatalf("list: %+v next=%q err=%v", items, next, err)
	}
	second, next2, err := mem.List(ctx, scope, "deploy", "a/", next, 1)
	if err != nil || len(second) != 1 || second[0].Key != "a/two" || next2 != "" {
		t.Fatalf("second page: %+v next=%q err=%v", second, next2, err)
	}
	first, _ := mem.Get(ctx, scope, "deploy", "a/one")
	if err := mem.Delete(ctx, scope, "deploy", "a/one", first.ETag()); err != nil {
		t.Fatal(err)
	}
	recreated, err := mem.Create(ctx, scope, "deploy", "a/one", "att_test", statev1.WriteRequest{Value: json.RawMessage(`{"ok":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	if recreated.ETag() == first.ETag() {
		t.Fatal("recreated key reused an old ETag")
	}
}

type testVerifier struct{ err error }

func (v testVerifier) VerifyArtifact(context.Context, Scope, domain.ArtifactID, string) error {
	return v.err
}

func TestHTTPVerifyArtifactReferencesBeforeWrite(t *testing.T) {
	mem := NewMemory(nil)
	h, _ := NewHandler(mem, testResolver{scope: testScope(), write: true})
	h.Artifacts = testVerifier{err: nil}
	srv := httptest.NewServer(h)
	defer srv.Close()
	body := `{"value":{"release":"v1"},"artifactReferences":[{"artifactId":"art_1","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/runtime/v3/attempts/att_test/state/release/entries?key=releases/v1", strings.NewReader(body))
	req.Header.Set("If-None-Match", "*")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("verified references: %s", resp.Status)
	}
	resp.Body.Close()

	req, _ = http.NewRequest(http.MethodPut, srv.URL+"/runtime/v3/attempts/att_test/state/release/entries?key=releases/v2", strings.NewReader(body))
	req.Header.Set("If-None-Match", "*")
	h.Artifacts = testVerifier{err: ErrDenied}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("denied reference: %s", resp.Status)
	}
	resp.Body.Close()
}
