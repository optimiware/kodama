package chatopshttp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/integrations/chatops"
	chatopshttp "github.com/optimiware/kodama/internal/integrations/chatops/http"
)

const (
	chatopsYAML = "../../../../api/chatops.yaml"
	commonYAML  = "../../../../api/common.yaml"
)

var (
	t0     = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	scopeA = domain.Scope{TenantID: "t041_acme", ProjectID: "p041_inventory"}
)

// ---- the OpenAPI document as a validator ------------------------------------------

type spec struct {
	raw map[string]any
	c   *jsonschema.Compiler
}

func loadYAML(t testing.TB, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

// translate rewrites OpenAPI 3.0 nullability into JSON Schema.
func translate(n any) {
	switch v := n.(type) {
	case map[string]any:
		if nb, _ := v["nullable"].(bool); nb {
			if ty, ok := v["type"].(string); ok {
				v["type"] = []any{ty, "null"}
			}
		}
		delete(v, "nullable")
		for _, c := range v {
			translate(c)
		}
	case []any:
		for _, c := range v {
			translate(c)
		}
	}
}

func loadSpec(t testing.TB) *spec {
	t.Helper()
	c := jsonschema.NewCompiler()
	var out map[string]any
	for name, path := range map[string]string{"common.yaml": commonYAML, "chatops.yaml": chatopsYAML} {
		raw := loadYAML(t, path)
		translate(raw)
		js, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(js))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.AddResource(name, doc); err != nil {
			t.Fatal(err)
		}
		if name == "chatops.yaml" {
			out = raw
		}
	}
	return &spec{raw: out, c: c}
}

func (s *spec) schema(t testing.TB, name string) *jsonschema.Schema {
	t.Helper()
	sch, err := s.c.Compile("chatops.yaml#/components/schemas/" + name)
	if err != nil {
		t.Fatalf("schema %s: %v", name, err)
	}
	return sch
}

func (s *spec) validate(t testing.TB, schema string, doc []byte) error {
	t.Helper()
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		t.Fatalf("not JSON: %v: %s", err, doc)
	}
	return s.schema(t, schema).Validate(v)
}

func (s *spec) errorEnvelope(t testing.TB) *jsonschema.Schema {
	t.Helper()
	sch, err := s.c.Compile("common.yaml#/components/schemas/ErrorEnvelope")
	if err != nil {
		t.Fatal(err)
	}
	return sch
}

type operation struct {
	id, method, path, base string
	raw                    map[string]any
}

func (o operation) fullPath() string { return o.base + o.path }

// effectiveSecurity is the operation's own security requirement list, or the
// document's when it declares none (an explicit empty list means anonymous).
func (s *spec) effectiveSecurity(o operation) []map[string]any {
	raw, ok := o.raw["security"]
	if !ok {
		raw = s.raw["security"]
	}
	list, _ := raw.([]any)
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		m, _ := item.(map[string]any)
		out = append(out, m)
	}
	return out
}

func (s *spec) operations() []operation {
	servers := s.raw["servers"].([]any)
	base := servers[0].(map[string]any)["url"].(string)
	var out []operation
	for path, item := range s.raw["paths"].(map[string]any) {
		for method, op := range item.(map[string]any) {
			if !map[string]bool{"get": true, "post": true, "patch": true, "put": true, "delete": true}[method] {
				continue
			}
			m := op.(map[string]any)
			out = append(out, operation{id: m["operationId"].(string), method: strings.ToUpper(method), path: path, base: base, raw: m})
		}
	}
	return out
}

func (s *spec) example(t testing.TB, name string) []byte {
	t.Helper()
	ex := s.raw["components"].(map[string]any)["examples"].(map[string]any)[name].(map[string]any)["value"]
	b, err := json.Marshal(ex)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var wildcard = regexp.MustCompile(`\{[^}]+\}`)

// ---- a controlled double of the use-case port ----------------------------------------

// fakeService records every call and returns configured values. It proves the
// transport only: no business rule lives here.
type fakeService struct {
	mu    sync.Mutex
	calls []string
	err   error

	dest  chatops.Destination
	dests []chatops.Destination
	view  chatops.DeliveryView
	dels  []chatops.Delivery

	caller   chatops.Caller
	scope    domain.Scope
	expected int64
	reason   string
	input    chatops.DestinationInput
	edit     chatops.DestinationEdit
	after    chatops.DeliveryPosition
	afterD   chatops.DestinationID
	filter   chatops.DeliveryFilter
	limit    int
}

var _ chatopshttp.Service = (*fakeService)(nil)

func (f *fakeService) record(name string, c chatops.Caller, scope domain.Scope) {
	f.mu.Lock()
	f.calls = append(f.calls, name)
	f.caller, f.scope = c, scope
	f.mu.Unlock()
}

func (f *fakeService) CreateDestination(_ context.Context, c chatops.Caller, scope domain.Scope, in chatops.DestinationInput) (chatops.Destination, error) {
	f.record("create", c, scope)
	f.input = in
	return f.dest, f.err
}

func (f *fakeService) GetDestination(_ context.Context, c chatops.Caller, scope domain.Scope, _ chatops.DestinationID) (chatops.Destination, error) {
	f.record("get-destination", c, scope)
	return f.dest, f.err
}

func (f *fakeService) ListDestinations(_ context.Context, c chatops.Caller, scope domain.Scope, after chatops.DestinationID, limit int) ([]chatops.Destination, error) {
	f.record("list-destinations", c, scope)
	f.afterD, f.limit = after, limit
	out := f.dests
	for i, d := range out {
		if after != "" && d.ID == after {
			out = out[i+1:]
			break
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, f.err
}

func (f *fakeService) UpdateDestination(_ context.Context, c chatops.Caller, scope domain.Scope, _ chatops.DestinationID, expected int64, edit chatops.DestinationEdit) (chatops.Destination, error) {
	f.record("update", c, scope)
	f.expected, f.edit = expected, edit
	return f.dest, f.err
}

func (f *fakeService) ListDeliveries(_ context.Context, c chatops.Caller, scope domain.Scope, fl chatops.DeliveryFilter, after chatops.DeliveryPosition, limit int) ([]chatops.Delivery, error) {
	f.record("list-deliveries", c, scope)
	f.after, f.filter, f.limit = after, fl, limit
	out := f.dels
	if !after.IsZero() {
		for i, d := range out {
			if d.ID == after.ID {
				out = out[i+1:]
				break
			}
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, f.err
}

func (f *fakeService) GetDelivery(_ context.Context, c chatops.Caller, scope domain.Scope, _ chatops.DeliveryID) (chatops.DeliveryView, error) {
	f.record("get-delivery", c, scope)
	return f.view, f.err
}

func (f *fakeService) ReplayDelivery(_ context.Context, c chatops.Caller, scope domain.Scope, _ chatops.DeliveryID, expected int64, reason string) (chatops.Delivery, error) {
	f.record("replay", c, scope)
	f.expected, f.reason = expected, reason
	return f.view.Delivery, f.err
}

func (f *fakeService) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// ---- server ------------------------------------------------------------------------------

type bearerAuth struct{}

func (bearerAuth) Authenticate(_ context.Context, r *http.Request) (httpapi.Principal, error) {
	scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	return httpapi.Principal{ID: tok}, nil
}

// authorizer admits every principal except "mallory" (hidden) and "viewer"
// (denied for the settings actions).
type authorizer struct{ settings map[httpapi.Action]bool }

func (a authorizer) Authorize(_ context.Context, p httpapi.Principal, action httpapi.Action, _ httpapi.Target) error {
	switch {
	case p.ID == "mallory":
		return httpapi.ErrHidden
	case p.ID == "viewer" && a.settings[action]:
		return httpapi.ErrDenied
	}
	return nil
}

var cursorKey = []byte("t041-chatops-cursor-key-0000000000000000")

type server struct {
	*httptest.Server
	svc   *fakeService
	codec *httpapi.CursorCodec
}

func newServer(t *testing.T, svc *fakeService) *server {
	t.Helper()
	codec, err := httpapi.NewCursorCodec(cursorKey)
	if err != nil {
		t.Fatal(err)
	}
	settings := map[httpapi.Action]bool{}
	for _, a := range []httpapi.Action{chatopshttp.ActionDestinationList, chatopshttp.ActionDestinationCreate, chatopshttp.ActionDestinationRead,
		chatopshttp.ActionDestinationUpdate, chatopshttp.ActionDeliveryReplay} {
		settings[a] = true
	}
	rt, err := httpapi.NewRouter(httpapi.RouterConfig{Authenticator: bearerAuth{}, Authorizer: authorizer{settings: settings}, RateLimiter: httpapi.Unlimited(), Prefix: "/api/v1"})
	if err != nil {
		t.Fatal(err)
	}
	h, err := chatopshttp.NewHandler(svc, codec)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.RegisterRoutes(rt); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(rt)
	t.Cleanup(srv.Close)
	return &server{Server: srv, svc: svc, codec: codec}
}

type response struct {
	Status int
	Header http.Header
	Body   []byte
}

func (s *server) do(t *testing.T, method, path, who string, headers map[string]string, body string) response {
	t.Helper()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req, err := http.NewRequest(method, s.URL+"/api/v1"+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if who != "" {
		req.Header.Set("Authorization", "Bearer "+who)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return response{Status: resp.StatusCode, Header: resp.Header, Body: buf.Bytes()}
}

func projectPath(scope domain.Scope) string {
	return "/tenants/" + string(scope.TenantID) + "/projects/" + string(scope.ProjectID)
}

// ---- domain fixtures ----------------------------------------------------------------------

func destination() chatops.Destination {
	return chatops.Destination{Scope: scopeA, ID: "cdst_t041_1", Name: "review-channel", Endpoint: "https://hooks.t041.example/secret-path", Credential: "cred/ops",
		Events: []string{"gate.decided", "gate.requested"}, Enabled: true, BindingRevision: 2, Version: 3, CreatedAt: t0, UpdatedAt: t0.Add(time.Minute)}
}

func failedDelivery() chatops.Delivery {
	return chatops.Delivery{Scope: scopeA, ID: "cdl_t041_1", DestinationID: "cdst_t041_1", EventID: "evt_1", EventType: "gate.requested", BindingRevision: 2,
		Payload: []byte(`{"url":"https://api.t041.example/x","summary":"payload must not be exposed"}`), State: chatops.StateFailed, Attempts: 4, CycleAttempts: 4,
		LastClass: chatops.ClassReceiverError, LastStatus: 503, FailedReason: chatops.FailedExhausted, Version: 9, CreatedAt: t0, UpdatedAt: t0.Add(time.Hour)}
}

func detail() chatops.DeliveryView {
	d := failedDelivery()
	return chatops.DeliveryView{Delivery: d, DestinationName: "review-channel", Attempts: []chatops.Attempt{
		{Number: 1, ClaimToken: 1, StartedAt: t0, FinishedAt: t0.Add(time.Second), Result: chatops.ResultRetry, Class: chatops.ClassTimeout, Uncertain: true},
		{Number: 2, ClaimToken: 2, StartedAt: t0.Add(time.Minute), FinishedAt: t0.Add(time.Minute + time.Second), Result: chatops.ResultFailed, Class: chatops.ClassReceiverError, HTTPStatus: 503},
		{Number: 3, ClaimToken: 3, StartedAt: t0.Add(2 * time.Minute)},
	}}
}
