package client_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api/client"
)

// The generated client is exercised against a plain HTTP server: the contract
// with the real composition is Test_core_assembly_* (test/integration/assembly-core).

func TestClientCarriesIdentityKeysAndParameters(t *testing.T) {
	var got *http.Request
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Idempotent-Replayed", "true")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"run_1","status":"queued","version":1,"purpose":"work","sourceRetention":"pending","definitionId":"pd_1","definitionDigest":"sha256:` + strings.Repeat("a", 64) + `","createdAt":"2026-01-01T00:00:00Z","reason":"admitted","requestedBy":{"id":"u_1","type":"user"},"inputSnapshot":{"digest":"sha256:` + strings.Repeat("b", 64) + `","inputs":[],"secrets":[]},"cancelRequestedAt":null,"contentRevision":1,"failureReason":null,"finishedAt":null,"sourceCommit":null,"startedAt":null,"workItemId":"wi_1"}`))
	}))
	defer srv.Close()

	c, err := client.NewClientWithResponses(srv.URL+"/api/v1", client.WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer test-token")
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.CreateRunWithResponse(context.Background(), "t_acme", "p_inv", &client.CreateRunParams{IdempotencyKey: "key-1"},
		client.CreateRunRequest{WorkItemId: "wi_1", DefinitionId: "pd_1", ContentRevision: 1, SourceCommit: strings.Repeat("1", 40)})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode() != 201 || resp.JSON201 == nil || resp.JSON201.Id != "run_1" || resp.HTTPResponse.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("%d %s", resp.StatusCode(), resp.Body)
	}
	if got.Method != "POST" || got.URL.Path != "/api/v1/tenants/t_acme/projects/p_inv/runs" || got.Header.Get("Idempotency-Key") != "key-1" ||
		got.Header.Get("Authorization") != "Bearer test-token" || got.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("request %s %s %v", got.Method, got.URL.Path, got.Header)
	}
	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil || sent["workItemId"] != "wi_1" || sent["contentRevision"] != float64(1) {
		t.Fatalf("body %s", body)
	}
}

func TestClientEncodesPathAndQueryParameters(t *testing.T) {
	var raw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw = r.URL.EscapedPath() + "?" + r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"nextCursor":null}`))
	}))
	defer srv.Close()
	c, err := client.NewClientWithResponses(srv.URL + "/api/v1")
	if err != nil {
		t.Fatal(err)
	}
	limit := 25
	cursor := "a b/c+d="
	states := []client.WorkState{"active", "ready"}
	if _, err := c.ListWorkItemsWithResponse(context.Background(), "t_acme", "p inv/x", &client.ListWorkItemsParams{Limit: &limit, Cursor: &cursor, State: &states}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/api/v1/tenants/t_acme/projects/p%20inv%2Fx/work-items?", "limit=25", "cursor=a+b%2Fc%2Bd%3D", "state=active", "state=ready"} {
		if !strings.Contains(raw, want) {
			t.Errorf("%q is missing from %s", want, raw)
		}
	}
}

func TestClientDecodesStableErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"idempotency_conflict","message":"a different request was made with this key","requestId":"req_1","details":{}}}`))
	}))
	defer srv.Close()
	c, err := client.NewClientWithResponses(srv.URL + "/api/v1")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.CreateRunWithResponse(context.Background(), "t_a", "p_a", &client.CreateRunParams{IdempotencyKey: "k"},
		client.CreateRunRequest{WorkItemId: "w", DefinitionId: "d", ContentRevision: 1, SourceCommit: strings.Repeat("1", 40)})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode() != 409 || resp.JSON409 == nil || resp.JSON409.Error.Code != "idempotency_conflict" || resp.JSON409.Error.RequestId != "req_1" {
		t.Fatalf("%d %s", resp.StatusCode(), resp.Body)
	}
}

// The public client depends on nothing of Kodama's implementation: no internal
// package, no contracts package, and no third-party module.
func TestClientImportsNothingInternal(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		if dep == "github.com/optimiware/kodama/api/client" || dep == "github.com/optimiware/kodama/api/client/internal/runtime" {
			continue
		}
		t.Errorf("the public client depends on %s", dep)
	}
}
