package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
)

func operation(t *testing.T, id string) Operation {
	t.Helper()
	ops, err := BuildOperations(api.OpenAPI)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if op.ID == id {
			return op
		}
	}
	t.Fatalf("operation %s not found", id)
	return Operation{}
}

type staticToken string

func (s staticToken) Token(context.Context, Caller) (string, error) { return string(s), nil }

func testExecutor(t *testing.T, base string) *Executor {
	t.Helper()
	return testExecutorWithToken(t, base, "api-tok")
}

func testExecutorWithToken(t *testing.T, base, token string) *Executor {
	t.Helper()
	e, err := NewExecutor(base, 5*time.Second, 1<<20, nil, staticToken(token), "", "")
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestExecutorCallsPublicAPI(t *testing.T) {
	var got *http.Request
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-ID", "req_test")
		_, _ = w.Write([]byte(`{"id":"u_1","issuer":"https://id.example","subject":"user|1"}`))
	}))
	defer srv.Close()

	e := testExecutor(t, srv.URL)
	res, err := e.Call(withCaller(context.Background(), Caller{SubjectToken: "tok-1"}), operation(t, "getMe"), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(res.Text, `"u_1"`) {
		t.Fatalf("result = %+v", res)
	}
	if got.Method != http.MethodGet || got.URL.Path != "/me" {
		t.Fatalf("request = %s %s", got.Method, got.URL.Path)
	}
	if got.Header.Get("Authorization") != "Bearer api-tok" {
		t.Fatalf("authorization = %q, want the delegated token", got.Header.Get("Authorization"))
	}
	if res.Meta["kodama/requestId"] != "req_test" {
		t.Fatalf("meta = %+v", res.Meta)
	}
	if len(gotBody) != 0 {
		t.Fatalf("unexpected body %q", gotBody)
	}
}

func TestExecutorMapsPathQueryHeadersAndBody(t *testing.T) {
	var got *http.Request
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"wi_1"}`))
	}))
	defer srv.Close()

	e := testExecutor(t, srv.URL)
	op := operation(t, "createRun")
	res, err := e.Call(withCaller(context.Background(), Caller{SubjectToken: "tok"}), op, map[string]any{
		"tenant_id": "t_acme", "project_id": "p_inventory",
		"idempotency_key": "key-1",
		"body": map[string]any{
			"sourceCommit": "1111111111111111111111111111111111111111",
			"workItemId":   "wi_1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("result = %+v", res)
	}
	if got.Method != http.MethodPost || got.URL.Path != "/tenants/t_acme/projects/p_inventory/runs" {
		t.Fatalf("request = %s %s", got.Method, got.URL.Path)
	}
	if got.Header.Get("Idempotency-Key") != "key-1" || got.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers = %+v", got.Header)
	}
	if body["workItemId"] != "wi_1" {
		t.Fatalf("body = %+v", body)
	}
}

func TestExecutorMapsAPIErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"idempotency_conflict","message":"the key was reused","requestId":"req_1"}}`))
	}))
	defer srv.Close()

	e := testExecutor(t, srv.URL)
	res, err := e.Call(withCaller(context.Background(), Caller{SubjectToken: "tok"}), operation(t, "getMe"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || res.Meta["kodama/code"] != "idempotency_conflict" || res.Meta["kodama/requestId"] != "req_1" {
		t.Fatalf("result = %+v", res)
	}
}

func TestExecutorBoundsAndRejectsFollow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 2048)))
	}))
	defer srv.Close()

	e, err := NewExecutor(srv.URL, time.Second, 1024, nil, staticToken("api-tok"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Call(withCaller(context.Background(), Caller{SubjectToken: "tok"}), operation(t, "getMe"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || res.Meta["kodama/code"] != "response_too_large" {
		t.Fatalf("result = %+v", res)
	}

	if _, err := e.Call(withCaller(context.Background(), Caller{SubjectToken: "tok"}), operation(t, "getRunLogs"), map[string]any{
		"tenant_id": "t", "project_id": "p", "id": "run", "follow": true,
	}); err == nil {
		t.Fatal("follow was accepted")
	}
}

func TestExecutorInjectsDefaults(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	e, err := NewExecutor(srv.URL, time.Second, 1<<20, nil, staticToken("api-tok"), "t_default", "p_default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Call(withCaller(context.Background(), Caller{SubjectToken: "tok"}), operation(t, "listWorkItems"), map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if path != "/tenants/t_default/projects/p_default/work-items" {
		t.Fatalf("path = %s", path)
	}
}

func TestExecutorMapsVersionAndEventHeaders(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	e := testExecutor(t, srv.URL)

	if _, err := e.Call(withCaller(context.Background(), Caller{SubjectToken: "tok"}), operation(t, "updateWorkItem"), map[string]any{
		"tenant_id": "t", "project_id": "p", "id": "wi_1", "if_match": `"3"`,
		"body": map[string]any{"title": "new"},
	}); err != nil {
		t.Fatal(err)
	}
	if got.Header.Get("If-Match") != `"3"` {
		t.Fatalf("If-Match = %q", got.Header.Get("If-Match"))
	}

	if _, err := e.Call(withCaller(context.Background(), Caller{SubjectToken: "tok"}), operation(t, "observeProjectEvents"), map[string]any{
		"tenant_id": "t", "project_id": "p", "last_event_id": "cursor-1",
	}); err != nil {
		t.Fatal(err)
	}
	if got.Header.Get("Last-Event-ID") != "cursor-1" {
		t.Fatalf("Last-Event-ID = %q", got.Header.Get("Last-Event-ID"))
	}
	if got.Header.Get("Accept") != "application/json" {
		t.Fatalf("Accept = %q", got.Header.Get("Accept"))
	}
}
