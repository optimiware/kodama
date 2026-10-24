package gatewayhttp_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/gateway"
	"github.com/optimiware/kodama/internal/gateway/gatewaytest"
	gatewayhttp "github.com/optimiware/kodama/internal/gateway/http"
	"github.com/optimiware/kodama/internal/gateway/openai"
	"github.com/optimiware/kodama/internal/httpapi"
)

var (
	scopeA  = executiontest.ScopeA
	scopeA2 = executiontest.ScopeA2
	reqID   = regexp.MustCompile(`^req_[0-9a-f]{32}$`)
)

// users: the test authenticator accepts "Bearer user:<id>" only, so a runtime
// token is not a human credential.
type users struct{}

func (users) Authenticate(_ context.Context, r *http.Request) (httpapi.Principal, error) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer user:")
	if tok == "" || tok == r.Header.Get("Authorization") {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	return httpapi.Principal{ID: tok}, nil
}

type allowAll struct{}

func (allowAll) Authorize(context.Context, httpapi.Principal, httpapi.Action, httpapi.Target) error {
	return nil
}

type world struct {
	*gatewaytest.Env
	srv *httptest.Server
	h   *gatewayhttp.Handler
}

func newWorld(t *testing.T) *world {
	t.Helper()
	e := gatewaytest.New(t)
	h, err := gatewayhttp.NewHandler(gatewayhttp.Config{Service: e.Replica.Svc, WriteTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := httpapi.NewRouter(httpapi.RouterConfig{Authenticator: users{}, Authorizer: allowAll{}, RateLimiter: httpapi.Unlimited(), Prefix: "/api/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.RegisterRoutes(rt); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/api/v1/", rt)
	mux.Handle("/gateway/v1/", h.Workload())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &world{Env: e, srv: srv, h: h}
}

type reply struct {
	Status int
	Header http.Header
	Body   []byte
}

func (w *world) do(method, path string, body []byte, headers map[string]string) reply {
	w.T.Helper()
	req, err := http.NewRequest(method, w.srv.URL+path, bytes.NewReader(body))
	if err != nil {
		w.T.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.T.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return reply{Status: resp.StatusCode, Header: resp.Header, Body: raw}
}

func settingsPath(s domain.Scope) string {
	return "/api/v1/tenants/" + string(s.TenantID) + "/projects/" + string(s.ProjectID) + "/model-settings"
}

func user(id string) map[string]string {
	return map[string]string{"Authorization": "Bearer user:" + id, "Content-Type": "application/json"}
}

func putHeaders(id, ifMatch string) map[string]string {
	h := user(id)
	if ifMatch != "" {
		h["If-Match"] = ifMatch
	}
	return h
}

func (r reply) errorCode(t *testing.T) string {
	t.Helper()
	var env httpapi.ErrorEnvelope
	dec := json.NewDecoder(bytes.NewReader(r.Body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil {
		t.Fatalf("not the error envelope (%d): %s", r.Status, r.Body)
	}
	if !reqID.MatchString(env.Error.RequestId) || env.Error.Details == nil || env.Error.Message == "" {
		t.Fatalf("malformed envelope: %s", r.Body)
	}
	if h := r.Header.Get("X-Request-ID"); h != env.Error.RequestId {
		t.Fatalf("X-Request-ID %q != body %q", h, env.Error.RequestId)
	}
	return env.Error.Code
}

func (w *world) editJSON(mutate func(map[string]any)) []byte {
	ed := w.Edit()
	m := map[string]any{
		"enabled": ed.Enabled, "endpoint": ed.Endpoint,
		"credential": map[string]any{"ref": ed.Credential.Ref, "version": ed.Credential.Version},
		"models":     ed.Models,
		"limits": map[string]any{"maxRequests": ed.Limits.MaxRequests, "tokenAllowance": ed.Limits.TokenAllowance, "maxRequestBytes": ed.Limits.MaxRequestBytes,
			"maxInputTokens": ed.Limits.MaxInputTokens, "maxOutputTokens": ed.Limits.MaxOutputTokens},
	}
	if mutate != nil {
		mutate(m)
	}
	raw, _ := json.Marshal(m)
	return raw
}

// ---- OpenAPI document ---------------------------------------------------------

type doc = map[string]any

func loadYAML(t *testing.T) doc {
	t.Helper()
	raw, err := os.ReadFile("../../../api/gateway.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var d doc
	if err := yaml.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func get(v any, path ...string) any {
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

func keys(v any) []string {
	var out []string
	if m, ok := v.(map[string]any); ok {
		for k := range m {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func strs(v any) []string {
	var out []string
	if l, ok := v.([]any); ok {
		for _, x := range l {
			out = append(out, x.(string))
		}
	}
	sort.Strings(out)
	return out
}

func jsonKeys(t *testing.T, v any) []string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func jsonTags(v any) []string {
	var out []string
	rt := reflect.TypeOf(v)
	for i := 0; i < rt.NumField(); i++ {
		if tag := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]; tag != "" && tag != "-" {
			out = append(out, tag)
		}
	}
	sort.Strings(out)
	return out
}

// Test_gateway_http_contract: GM-01, GM-A01, GM-A08. The route table, the DTOs and
// the accepted request members agree with api/gateway.yaml; input is strict;
// management and workload authentication are separate; errors are safe envelopes.
func Test_gateway_http_contract(t *testing.T) {
	spec := loadYAML(t)

	t.Run("routes and operations agree with api/gateway.yaml", func(t *testing.T) {
		e := gatewaytest.New(t)
		h, err := gatewayhttp.NewHandler(gatewayhttp.Config{Service: e.Replica.Svc})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range h.Routes() {
			op := get(spec, "paths", r.Path, strings.ToLower(r.Method), "operationId")
			if op != r.Operation {
				t.Errorf("%s %s: operation %v, table %q", r.Method, r.Path, op, r.Operation)
			}
		}
		if len(h.Routes()) != 2 {
			t.Errorf("%d management routes, want exactly GET and PUT", len(h.Routes()))
		}
		if get(spec, "paths", "/gateway/v1/attempts/{attemptId}/chat/completions", "post", "operationId") != "createChatCompletion" {
			t.Error("the workload route is not documented")
		}
		if !strings.HasSuffix(gatewayhttp.WorkloadPattern, "/gateway/v1/attempts/{attemptId}/chat/completions") {
			t.Errorf("workload pattern %q", gatewayhttp.WorkloadPattern)
		}
		// Every other documented path/method has a handler: nothing is promised twice or twice-served.
		documented := 0
		for p, item := range get(spec, "paths").(map[string]any) {
			for m := range item.(map[string]any) {
				if m == "parameters" {
					continue
				}
				documented++
				_ = p
			}
		}
		if documented != 3 {
			t.Errorf("%d documented operations, want 3", documented)
		}
	})

	t.Run("DTOs and generated types agree with the schemas", func(t *testing.T) {
		props := keys(get(spec, "components", "schemas", "ModelSettings", "properties"))
		required := strs(get(spec, "components", "schemas", "ModelSettings", "required"))
		if !reflect.DeepEqual(props, jsonTags(gatewayhttp.ModelSettings{})) {
			t.Errorf("ModelSettings properties %v vs generated %v", props, jsonTags(gatewayhttp.ModelSettings{}))
		}
		w := newWorld(t)
		w.ModelSecret(scopeA)
		w.Configure(w.Replica, scopeA, w.Edit())
		for _, s := range []domain.Scope{scopeA, scopeA2} {
			r := w.do("GET", settingsPath(s), nil, user(gatewaytest.Viewer))
			if r.Status != 200 {
				t.Fatalf("GET %s: %d %s", s.ProjectID, r.Status, r.Body)
			}
			var got map[string]json.RawMessage
			if err := json.Unmarshal(r.Body, &got); err != nil {
				t.Fatal(err)
			}
			for _, k := range required {
				if _, ok := got[k]; !ok {
					t.Errorf("%s: required member %q missing", s.ProjectID, k)
				}
			}
			var typed gatewayhttp.ModelSettings
			dec := json.NewDecoder(bytes.NewReader(r.Body))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&typed); err != nil {
				t.Errorf("%s: %v", s.ProjectID, err)
			}
		}
		req := keys(get(spec, "components", "schemas", "ReplaceModelSettingsRequest", "properties"))
		if !reflect.DeepEqual(req, jsonTags(gatewayhttp.ReplaceModelSettingsRequest{})) {
			t.Errorf("request properties %v vs generated %v", req, jsonTags(gatewayhttp.ReplaceModelSettingsRequest{}))
		}
		if !reflect.DeepEqual(keys(get(spec, "components", "schemas", "ModelLimits", "properties")), jsonTags(gatewayhttp.ModelLimits{})) {
			t.Error("limits properties differ from the generated type")
		}
	})

	t.Run("the documented chat members are exactly the accepted ones", func(t *testing.T) {
		props := keys(get(spec, "components", "schemas", "ChatCompletionRequest", "properties"))
		if !reflect.DeepEqual(props, openai.AcceptedMembers()) {
			t.Errorf("schema members %v, decoder members %v", props, openai.AcceptedMembers())
		}
		if get(spec, "components", "schemas", "ChatCompletionRequest", "additionalProperties") != false {
			t.Error("the request schema must forbid additional members")
		}
		for _, name := range []string{"ChatMessage", "ChatTool", "ChatToolCall", "CredentialReference", "ModelLimits", "ReplaceModelSettingsRequest", "ModelSettings", "ChatCompletionChunk", "ChatStreamError"} {
			if get(spec, "components", "schemas", name, "additionalProperties") != false {
				t.Errorf("%s allows additional members", name)
			}
		}
		// Every fixture the protocol accepts uses only documented members.
		for _, f := range []string{"minimal.json", "full.json", "tool_loop.json"} {
			raw, err := os.ReadFile("../testdata/chat-completions/requests/" + f)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			for k := range m {
				if !reflect.ValueOf(props).IsValid() || !contains(props, k) {
					t.Errorf("%s uses the undocumented member %q", f, k)
				}
			}
		}
	})

	t.Run("management input is strict", func(t *testing.T) {
		w := newWorld(t)
		w.ModelSecret(scopeA)
		path := settingsPath(scopeA)
		ok := w.editJSON(nil)
		bad := map[string][]byte{
			"unknown member":    w.editJSON(func(m map[string]any) { m["extra"] = true }),
			"unknown nested":    w.editJSON(func(m map[string]any) { m["limits"].(map[string]any)["burst"] = 1 }),
			"wrong type":        w.editJSON(func(m map[string]any) { m["enabled"] = "yes" }),
			"missing endpoint":  w.editJSON(func(m map[string]any) { delete(m, "endpoint") }),
			"missing models":    w.editJSON(func(m map[string]any) { delete(m, "models") }),
			"not json":          []byte("{"),
			"trailing document": append(append([]byte{}, ok...), []byte(`{}`)...),
			"empty body":        nil,
		}
		for name, body := range bad {
			r := w.do("PUT", path, body, putHeaders(gatewaytest.Admin, `"1"`))
			if r.Status != 400 || r.errorCode(t) != "invalid_request" {
				t.Errorf("%s: %d %s", name, r.Status, r.Body)
			}
		}
		if r := w.do("PUT", path, ok, map[string]string{"Authorization": "Bearer user:" + gatewaytest.Admin, "Content-Type": "text/plain", "If-Match": `"1"`}); r.Status != 415 {
			t.Errorf("wrong content type: %d", r.Status)
		}
		for name, tag := range map[string]string{"weak": `W/"1"`, "wildcard": `*`, "list": `"1", "2"`, "zero": `"0"`, "leading zero": `"01"`, "unquoted": `1`} {
			if r := w.do("PUT", path, ok, putHeaders(gatewaytest.Admin, tag)); r.Status != 400 {
				t.Errorf("If-Match %s: %d", name, r.Status)
			}
		}
		if r := w.do("PUT", path, ok, putHeaders(gatewaytest.Admin, "")); r.Status != 428 || r.errorCode(t) != "version_required" {
			t.Errorf("missing If-Match: %d %s", r.Status, r.Body)
		}
		if r := w.do("PUT", path, ok, putHeaders(gatewaytest.Admin, `"9"`)); r.Status != 412 || r.errorCode(t) != "version_conflict" {
			t.Errorf("stale If-Match: %d %s", r.Status, r.Body)
		}
		// Settings validation is the use case's: bad values are 400 with a fixed message.
		for name, mutate := range map[string]func(map[string]any){
			"http endpoint": func(m map[string]any) { m["endpoint"] = "http://127.0.0.1:1/v1" },
			"zero limit":    func(m map[string]any) { m["limits"].(map[string]any)["maxRequests"] = 0 },
			"bad model":     func(m map[string]any) { m["models"] = []string{"bad model!"} },
		} {
			r := w.do("PUT", path, w.editJSON(mutate), putHeaders(gatewaytest.Admin, `"1"`))
			if r.Status != 400 || r.errorCode(t) != "invalid_request" {
				t.Errorf("%s: %d %s", name, r.Status, r.Body)
			}
		}
		var n int
		w.Scalar(&n, `SELECT count(*) FROM model_settings_revisions`)
		if n != 0 {
			t.Fatalf("%d revisions stored from rejected requests", n)
		}
	})

	t.Run("ETag, replace and settings authorization", func(t *testing.T) {
		w := newWorld(t)
		w.ModelSecret(scopeA)
		path := settingsPath(scopeA)

		// Virtual revision one of a project without settings.
		r := w.do("GET", path, nil, user(gatewaytest.Viewer))
		if r.Status != 200 || r.Header.Get("ETag") != `"1"` || !bytes.Contains(r.Body, []byte(`"stored":false`)) || !bytes.Contains(r.Body, []byte(`"endpoint":null`)) {
			t.Fatalf("virtual settings: %d %v %s", r.Status, r.Header, r.Body)
		}
		// A viewer reads but cannot replace.
		if r := w.do("PUT", path, w.editJSON(nil), putHeaders(gatewaytest.Viewer, `"1"`)); r.Status != 403 || r.errorCode(t) != "forbidden" {
			t.Fatalf("viewer PUT: %d %s", r.Status, r.Body)
		}
		// The administrator creates revision 2 with no wildcard.
		r = w.do("PUT", path, w.editJSON(nil), putHeaders(gatewaytest.Admin, `"1"`))
		if r.Status != 200 || r.Header.Get("ETag") != `"2"` {
			t.Fatalf("first PUT: %d %v %s", r.Status, r.Header, r.Body)
		}
		var typed gatewayhttp.ModelSettings
		if err := json.Unmarshal(r.Body, &typed); err != nil || !typed.Stored || typed.Version != 2 || typed.Credential == nil || typed.Credential.Version != 1 || typed.Recording {
			t.Fatalf("stored settings: %+v %v", typed, err)
		}
		if bytes.Contains(r.Body, []byte(gatewaytest.Bearer)) {
			t.Fatal("the response carries the provider key")
		}
		// An identical PUT is idempotent: same ETag, no new revision.
		r = w.do("PUT", path, w.editJSON(nil), putHeaders(gatewaytest.Admin, `"2"`))
		if r.Status != 200 || r.Header.Get("ETag") != `"2"` {
			t.Fatalf("idempotent PUT: %d %v", r.Status, r.Header)
		}
		// A change advances it, and the old tag is then stale.
		changed := w.editJSON(func(m map[string]any) { m["limits"].(map[string]any)["maxRequests"] = 9 })
		if r := w.do("PUT", path, changed, putHeaders(gatewaytest.Admin, `"2"`)); r.Status != 200 || r.Header.Get("ETag") != `"3"` {
			t.Fatalf("change PUT: %d %v %s", r.Status, r.Header, r.Body)
		}
		if r := w.do("PUT", path, changed, putHeaders(gatewaytest.Admin, `"2"`)); r.Status != 412 {
			t.Fatalf("stale PUT: %d", r.Status)
		}
		// Failures with module codes.
		for name, tc := range map[string]struct {
			mutate func(map[string]any)
			status int
			code   string
		}{
			"missing secret":        {func(m map[string]any) { m["credential"].(map[string]any)["ref"] = "secret://project/nope" }, 409, "credential_unusable"},
			"unapproved endpoint":   {func(m map[string]any) { m["endpoint"] = "https://api.example.invalid/v1" }, 409, "destination_not_approved"},
			"recording unavailable": {func(m map[string]any) { m["recording"] = true }, 409, "recording_unavailable"},
		} {
			r := w.do("PUT", path, w.editJSON(tc.mutate), putHeaders(gatewaytest.Admin, `"3"`))
			if r.Status != tc.status || r.errorCode(t) != tc.code {
				t.Errorf("%s: %d %s", name, r.Status, r.Body)
			}
		}
		// Authentication and tenancy.
		if r := w.do("GET", path, nil, nil); r.Status != 401 {
			t.Errorf("anonymous GET: %d", r.Status)
		}
		if r := w.do("GET", path, nil, user(executiontest.OtherEng)); r.Status != 404 {
			t.Errorf("another tenant's user: %d", r.Status)
		}
		// A runtime token is not a human credential.
		a := w.NewAttempt(scopeA)
		if r := w.do("GET", path, nil, map[string]string{"Authorization": "Bearer " + a.Token}); r.Status != 401 {
			t.Errorf("a runtime token on the management route: %d", r.Status)
		}
	})
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func workloadPath(a gatewaytest.Attempt) string {
	return "/gateway/v1/attempts/" + string(a.ID) + "/chat/completions"
}

func runtimeHeaders(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token, "Content-Type": "application/json"}
}

func sse(t *testing.T, raw []byte) (events []string, done bool) {
	t.Helper()
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			t.Fatalf("unexpected SSE line %q", line)
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			done = true
			continue
		}
		events = append(events, data)
	}
	return events, done
}

// Test_gateway_workload_route covers the workload route over a real server:
// streaming, safe errors, separate authentication, and partial-stream behavior.
func Test_gateway_workload_route(t *testing.T) {
	w := newWorld(t)
	w.ModelSecret(scopeA)
	w.Configure(w.Replica, scopeA, w.Edit())
	body := gatewaytest.Body("gpt-a", "hello", 64)

	t.Run("a stream of validated chunks ending with [DONE]", func(t *testing.T) {
		a := w.NewAttempt(scopeA)
		w.Provider.Script(gatewaytest.OK("gpt-a-2026", 10, 3, "Hel", "lo"))
		r := w.do("POST", workloadPath(a), body, runtimeHeaders(a.Token))
		if r.Status != 200 || r.Header.Get("Content-Type") != "text/event-stream" || r.Header.Get("Cache-Control") != "no-store" || !reqID.MatchString(r.Header.Get("X-Request-ID")) {
			t.Fatalf("%d %v", r.Status, r.Header)
		}
		events, done := sse(t, r.Body)
		if !done || len(events) < 3 {
			t.Fatalf("done=%v events=%d: %s", done, len(events), r.Body)
		}
		for _, ev := range events {
			var c gatewayhttp.ChatCompletionChunk
			dec := json.NewDecoder(strings.NewReader(ev))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&c); err != nil || c.Object != "chat.completion.chunk" {
				t.Fatalf("event %q is not a documented chunk: %v", ev, err)
			}
			if strings.Contains(ev, "fp_dropped") {
				t.Fatalf("an unvalidated provider member was forwarded: %s", ev)
			}
		}
	})

	t.Run("errors are safe envelopes with the documented codes", func(t *testing.T) {
		a := w.NewAttempt(scopeA)
		cases := []struct {
			name    string
			method  string
			path    string
			body    []byte
			headers map[string]string
			status  int
			code    string
		}{
			{"no credential", "POST", workloadPath(a), body, map[string]string{"Content-Type": "application/json"}, 401, "invalid_credential"},
			{"human token", "POST", workloadPath(a), body, runtimeHeaders("user:" + gatewaytest.Admin), 401, "invalid_credential"},
			{"two headers", "POST", workloadPath(a), body, map[string]string{"Authorization": "Bearer a b", "Content-Type": "application/json"}, 401, "invalid_credential"},
			{"wrong content type", "POST", workloadPath(a), body, map[string]string{"Authorization": "Bearer " + a.Token, "Content-Type": "text/plain"}, 415, "invalid_request"},
			{"unsupported member", "POST", workloadPath(a), []byte(strings.Replace(string(body), `"stream":true`, `"stream":true,"logprobs":true`, 1)), runtimeHeaders(a.Token), 400, "invalid_request"},
			{"not streaming", "POST", workloadPath(a), []byte(strings.Replace(string(body), `"stream":true`, `"stream":false`, 1)), runtimeHeaders(a.Token), 400, "invalid_request"},
			{"empty body", "POST", workloadPath(a), nil, runtimeHeaders(a.Token), 400, "invalid_request"},
			{"model off the list", "POST", workloadPath(a), gatewaytest.Body("gpt-z", "hi", 8), runtimeHeaders(a.Token), 403, "model_not_allowed"},
			{"output over limit", "POST", workloadPath(a), gatewaytest.Body("gpt-a", "hi", 100000), runtimeHeaders(a.Token), 403, "limit_exceeded"},
			{"bad attempt id", "POST", "/gateway/v1/attempts/bad%20id/chat/completions", body, runtimeHeaders(a.Token), 400, "invalid_request"},
			{"another attempt's route", "POST", "/gateway/v1/attempts/att_other/chat/completions", body, runtimeHeaders(a.Token), 401, "invalid_credential"},
			{"unknown path", "POST", "/gateway/v1/attempts/" + string(a.ID) + "/embeddings", body, runtimeHeaders(a.Token), 404, "not_found"},
			{"wrong method", "GET", workloadPath(a), nil, runtimeHeaders(a.Token), 404, "not_found"},
		}
		for _, c := range cases {
			r := w.do(c.method, c.path, c.body, c.headers)
			if r.Status != c.status || r.errorCode(t) != c.code {
				t.Errorf("%s: %d %s", c.name, r.Status, r.Body)
			}
			if c.status == 401 && !strings.HasPrefix(r.Header.Get("WWW-Authenticate"), "Bearer") {
				t.Errorf("%s: no challenge", c.name)
			}
		}
		if n := len(w.Provider.Calls()); n != 1 {
			t.Fatalf("provider calls %d: a refused request reached the provider", n)
		}
	})

	t.Run("project and run states", func(t *testing.T) {
		unconfigured := w.NewAttempt(scopeA2)
		if r := w.do("POST", workloadPath(unconfigured), body, runtimeHeaders(unconfigured.Token)); r.Status != 409 || r.errorCode(t) != "model_not_configured" {
			t.Errorf("unconfigured project: %d %s", r.Status, r.Body)
		}
		a := w.NewAttempt(scopeA)
		started := make(chan struct{}, 1)
		release := make(chan struct{})
		w.Provider.Script(gatewaytest.Gate(started, release, gatewaytest.OK("gpt-a-2026", 10, 3, "x")))
		first := make(chan reply, 1)
		go func() { first <- w.do("POST", workloadPath(a), body, runtimeHeaders(a.Token)) }()
		<-started
		r := w.do("POST", workloadPath(a), body, runtimeHeaders(a.Token))
		if r.Status != 409 || r.errorCode(t) != "run_busy" || r.Header.Get("Retry-After") != "1" {
			t.Errorf("busy run: %d %v %s", r.Status, r.Header, r.Body)
		}
		close(release)
		if f := <-first; f.Status != 200 {
			t.Errorf("first request: %d", f.Status)
		}
		// A provider failure before any byte is a plain error that quotes nothing.
		w.Provider.Script(gatewaytest.Status(500))
		r = w.do("POST", workloadPath(a), gatewaytest.Body("gpt-a", "PROMPT-SENTINEL", 8), runtimeHeaders(a.Token))
		if r.Status != 502 || r.errorCode(t) != "provider_error" || bytes.Contains(r.Body, []byte(gatewaytest.Bearer)) || bytes.Contains(r.Body, []byte("PROMPT-SENTINEL")) {
			t.Errorf("provider failure: %d %s", r.Status, r.Body)
		}
		// Revoked sessions are refused.
		if err := w.RT.Revoke(context.Background(), a.Scope, a.ID); err != nil {
			t.Fatal(err)
		}
		if r := w.do("POST", workloadPath(a), body, runtimeHeaders(a.Token)); r.Status != 403 || r.errorCode(t) != "forbidden" {
			t.Errorf("revoked: %d %s", r.Status, r.Body)
		}
	})

	t.Run("a truncated stream shows its failure and never a terminator", func(t *testing.T) {
		a := w.NewAttempt(scopeA)
		w.Provider.Script(gatewaytest.Truncated("gpt-a-2026", "par", "tial"))
		req, _ := http.NewRequest("POST", w.srv.URL+workloadPath(a), bytes.NewReader(body))
		for k, v := range runtimeHeaders(a.Token) {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, readErr := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		if readErr == nil {
			t.Fatal("the response ended like a complete one: the transport must show an incomplete body")
		}
		events, done := sse(t, bytes.TrimSpace(raw))
		if done || len(events) < 3 {
			t.Fatalf("done=%v events=%d\n%s", done, len(events), raw)
		}
		var fail gatewayhttp.ChatStreamError
		dec := json.NewDecoder(strings.NewReader(events[len(events)-1]))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&fail); err != nil || fail.Error.Code != "stream_truncated" || fail.Error.Type != "kodama_gateway" {
			t.Fatalf("last event %q: %v", events[len(events)-1], err)
		}
	})

	t.Run("a client that goes away cancels the upstream request", func(t *testing.T) {
		a := w.NewAttempt(scopeA)
		before := w.Provider.Cancelled()
		started := make(chan struct{})
		w.Provider.Script(w.Provider.Hang("gpt-a-2026", started, nil, "one"))
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, "POST", w.srv.URL+workloadPath(a), bytes.NewReader(body))
		for k, v := range runtimeHeaders(a.Token) {
			req.Header.Set(k, v)
		}
		go func() {
			if resp, err := http.DefaultClient.Do(req); err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
		<-started
		time.Sleep(200 * time.Millisecond)
		cancel()
		if !gatewaytest.Wait(10*time.Second, func() bool { return w.Provider.Cancelled() == before+1 }) {
			t.Fatal("the upstream request was not cancelled after the client left")
		}
		if !gatewaytest.Wait(10*time.Second, func() bool {
			return w.Count(`SELECT count(*) FROM model_usage WHERE run_id = $1 AND outcome_code = 'client_cancelled'`, string(a.RunID)) == 1
		}) {
			t.Fatal("the cancelled request left no usage record")
		}
	})
}

var _ = errors.Is
var _ gateway.Sink = nil
